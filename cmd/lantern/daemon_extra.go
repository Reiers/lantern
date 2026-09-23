// Phase 10 daemon helpers: preferred-peer parsing, BlockGetter rebinding,
// and the optional Prometheus /metrics endpoint.

package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/headcheck"
	"github.com/Reiers/lantern/internal/buildinfo"
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
func serveMetrics(ctx context.Context, addr, token string, f *combined.Fetcher, bs *lbitswap.Client, host *llibp2p.Host, dash *dashboardDeps, hc *headcheck.Monitor, ing *gossipBlockIngestor) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP lantern_build_info Build identity (#156).")
		fmt.Fprintln(w, "# TYPE lantern_build_info gauge")
		fmt.Fprintf(w, "lantern_build_info{version=%q,commit=%q,network=%q} 1\n",
			buildinfo.BuildVersion(), buildinfo.Commit(), buildinfo.Network())
		writeHeadcheckMetrics(w, hc)
		if ing != nil {
			st := ing.Stats()
			fmt.Fprintln(w, "# HELP lantern_head_rejected_total Gossip head candidates rejected, by reason (#79/#156).")
			fmt.Fprintln(w, "# TYPE lantern_head_rejected_total counter")
			fmt.Fprintf(w, "lantern_head_rejected_total{reason=\"lighter\"} %d\n", st.RejectedLighter)
			fmt.Fprintf(w, "lantern_head_rejected_total{reason=\"non_monotonic_weight\"} %d\n", st.RejectedNonMonotonic)
			fmt.Fprintf(w, "lantern_head_rejected_total{reason=\"held_diverged\"} %d\n", st.HeldDiverged)
		}
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

// writeHeadcheckMetrics exports the running-head quorum state (#156) so
// production evidence for the head quorum comes from /metrics, not logs.
func writeHeadcheckMetrics(w io.Writer, hc *headcheck.Monitor) {
	if hc == nil {
		fmt.Fprintln(w, "# HELP lantern_headcheck_enabled 1 when the running-head divergence monitor is active.")
		fmt.Fprintln(w, "# TYPE lantern_headcheck_enabled gauge")
		fmt.Fprintln(w, "lantern_headcheck_enabled 0")
		return
	}
	r := hc.Last()
	rounds, diverged := hc.Stats()
	fmt.Fprintln(w, "# HELP lantern_headcheck_enabled 1 when the running-head divergence monitor is active.")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_enabled gauge")
	fmt.Fprintln(w, "lantern_headcheck_enabled 1")
	fmt.Fprintln(w, "# HELP lantern_headcheck_status Last round status (1 for the current status label).")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_status gauge")
	for _, st := range []headcheck.Status{headcheck.StatusAgree, headcheck.StatusDiverge, headcheck.StatusBehind, headcheck.StatusInsufficient} {
		v := 0
		if !r.At.IsZero() && r.Status == st {
			v = 1
		}
		fmt.Fprintf(w, "lantern_headcheck_status{status=%q} %d\n", st.String(), v)
	}
	fmt.Fprintln(w, "# HELP lantern_headcheck_voters Independent voters in the last round, by outcome.")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_voters gauge")
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"agree\"} %d\n", r.Agreeing)
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"disagree\"} %d\n", r.Disagreeing)
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"lagging\"} %d\n", r.Lagging)
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"forked\"} %d\n", r.ForkedKinds)
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"key_checked\"} %d\n", r.KeyChecked)
	fmt.Fprintf(w, "lantern_headcheck_voters{outcome=\"peer_groups\"} %d\n", r.PeerVoters)
	fmt.Fprintln(w, "# HELP lantern_headcheck_head_epoch Local vs median external head epoch in the last round.")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_head_epoch gauge")
	fmt.Fprintf(w, "lantern_headcheck_head_epoch{side=\"local\"} %d\n", r.LocalHead)
	fmt.Fprintf(w, "lantern_headcheck_head_epoch{side=\"external_median\"} %d\n", r.MedianExtHead)
	fmt.Fprintln(w, "# HELP lantern_headcheck_rounds_total Monitor rounds completed.")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_rounds_total counter")
	fmt.Fprintf(w, "lantern_headcheck_rounds_total %d\n", rounds)
	fmt.Fprintln(w, "# HELP lantern_headcheck_diverged_total Rounds that reported divergence.")
	fmt.Fprintln(w, "# TYPE lantern_headcheck_diverged_total counter")
	fmt.Fprintf(w, "lantern_headcheck_diverged_total %d\n", diverged)
}
