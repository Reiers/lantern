// Phase 10 daemon helpers: preferred-peer parsing, BlockGetter rebinding,
// and the optional Prometheus /metrics endpoint.

package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/api"
	lbitswap "github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/combined"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/hamt"
)

// parsePreferredPeers turns a comma-separated multiaddr list into AddrInfos.
// Empty input is valid (returns nil, nil).
func parsePreferredPeers(s string) ([]peer.AddrInfo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]peer.AddrInfo, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ma, err := multiaddr.NewMultiaddr(p)
		if err != nil {
			return nil, fmt.Errorf("bad multiaddr %q: %w", p, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			return nil, fmt.Errorf("not a /p2p multiaddr %q: %w", p, err)
		}
		out = append(out, *ai)
	}
	return out, nil
}

// rebindBlockGetter swaps the ChainAPI's BlockGetter and rebinds the
// state.Accessor on top of it. Used after Bitswap is wired in so existing
// handlers immediately route through the new chain.
//
// It rebinds IN PLACE rather than rebuilding the accessor: a fresh
// accessor.New would drop the head-state provider wired by
// ChainAPI.FollowHeadState (#87), silently re-pinning actor-state reads to
// the boot trusted-root. If for any reason the accessor isn't there yet,
// fall back to building one and re-apply head-following.
func rebindBlockGetter(c *handlers.ChainAPI, bg hamt.BlockGetter) {
	c.BlockGetter = bg
	if c.Accessor != nil {
		c.Accessor.Rebind(bg)
	} else {
		c.Accessor = accessor.New(c.Trusted, bg)
	}
	// Re-apply head-following in case the accessor was just (re)built or the
	// header store was wired after the initial FollowHeadState call. No-op
	// when no header store is attached.
	c.FollowHeadState()
}

// serveMetrics exposes per-source fetch hit counts + bitswap stats + libp2p
// peer count on a Prometheus-style /metrics endpoint. Format is text
// exposition (no client_golang dependency).
//
// When `dash` is non-nil (issue #5) the same listener also serves the
// operator dashboard at /dashboard/ + JSON endpoints under
// /api/dashboard/*. Pass nil to disable the dashboard.
func serveMetrics(ctx context.Context, addr, token string, f *combined.Fetcher, bs *lbitswap.Client, host *llibp2p.Host, dash *dashboardDeps) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if f != nil {
			fmt.Fprintln(w, "# HELP lantern_fetch_total Number of block fetches served by each layer.")
			fmt.Fprintln(w, "# TYPE lantern_fetch_total counter")
			for k, v := range f.Stats() {
				fmt.Fprintf(w, "lantern_fetch_total{source=%q} %d\n", k, v)
			}
		}
		if bs != nil {
			s := bs.Stats()
			fmt.Fprintln(w, "# HELP lantern_bitswap_blocks_total Bitswap blocks successfully fetched.")
			fmt.Fprintln(w, "# TYPE lantern_bitswap_blocks_total counter")
			fmt.Fprintf(w, "lantern_bitswap_blocks_total %d\n", s.GotBlocks)
			fmt.Fprintln(w, "# HELP lantern_bitswap_errors_total Bitswap fetch failures.")
			fmt.Fprintln(w, "# TYPE lantern_bitswap_errors_total counter")
			fmt.Fprintf(w, "lantern_bitswap_errors_total %d\n", s.Errors)
			fmt.Fprintln(w, "# HELP lantern_bitswap_bytes_in_total Cumulative bytes received via Bitswap.")
			fmt.Fprintln(w, "# TYPE lantern_bitswap_bytes_in_total counter")
			fmt.Fprintf(w, "lantern_bitswap_bytes_in_total %d\n", s.BytesIn)
		}
		if host != nil {
			ni := host.NetInfo()
			peers := ni.Peers()
			bw := ni.BandwidthTotals()
			fmt.Fprintln(w, "# HELP lantern_libp2p_peers Number of currently-connected libp2p peers.")
			fmt.Fprintln(w, "# TYPE lantern_libp2p_peers gauge")
			fmt.Fprintf(w, "lantern_libp2p_peers %d\n", len(peers))
			fmt.Fprintln(w, "# HELP lantern_libp2p_bw_bytes Cumulative libp2p bandwidth (bytes).")
			fmt.Fprintln(w, "# TYPE lantern_libp2p_bw_bytes counter")
			fmt.Fprintf(w, "lantern_libp2p_bw_bytes{direction=\"in\"} %d\n", bw.TotalIn)
			fmt.Fprintf(w, "lantern_libp2p_bw_bytes{direction=\"out\"} %d\n", bw.TotalOut)
			_ = api.NetBandwidthStats{} // keep api import non-trivial for future
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Issue #5: operator dashboard, opt-in by passing dash != nil.
	registerDashboard(mux, dash)

	// Bare-root convenience: if someone hits http://addr/ they probably
	// want the dashboard, not a 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && dash != nil {
			http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("  metrics listener failed: %v\n", err)
		return
	}
	var handler http.Handler = mux
	if token != "" {
		handler = dashboardTokenAuth(token, mux)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}

// dashboardTokenAuth wraps the dashboard/metrics mux with a Bearer-token
// gate (#57). Used when the listener is bound to a non-loopback address.
// /healthz stays open so external probes/load-balancers still work; every
// other path requires `Authorization: Bearer <token>`. The comparison is
// constant-time to avoid leaking the token via timing.
func dashboardTokenAuth(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized (#57: dashboard requires LANTERN_DASHBOARD_TOKEN bearer)", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
