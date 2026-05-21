// Phase 10 daemon helpers: preferred-peer parsing, BlockGetter rebinding,
// and the optional Prometheus /metrics endpoint.

package main

import (
	"context"
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

// rebindBlockGetter swaps the ChainAPI's BlockGetter and rebuilds the
// state.Accessor on top of it. Used after Bitswap is wired in so existing
// handlers immediately route through the new chain.
func rebindBlockGetter(c *handlers.ChainAPI, bg hamt.BlockGetter) {
	c.BlockGetter = bg
	c.Accessor = accessor.New(c.Trusted, bg)
}

// serveMetrics exposes per-source fetch hit counts + bitswap stats + libp2p
// peer count on a Prometheus-style /metrics endpoint. Format is text
// exposition (no client_golang dependency).
func serveMetrics(ctx context.Context, addr string, f *combined.Fetcher, bs *lbitswap.Client, host *llibp2p.Host) {
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

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("  metrics listener failed: %v\n", err)
		return
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}
