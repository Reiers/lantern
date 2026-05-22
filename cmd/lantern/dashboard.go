// Embedded operator dashboard for the Lantern daemon.
//
// Issue #5: operators need a visual dashboard. Without one, "is my Lantern
// healthy" requires shelling in and reading log lines or hitting the
// JSON-RPC. The dashboard lives on the same listener as /metrics and
// /healthz (configured via --metrics), serves a single static HTML/JS page
// from an embed.FS at /dashboard/, and exposes a small set of JSON
// endpoints under /api/dashboard/* for the page to poll every 5 seconds.
//
// Design choices:
//   - One vanilla HTML/JS page, no React/Vue/build step. Stays maintainable
//     when no-one's touched the file in 6 months.
//   - JSON endpoints are read-only and scoped to operator-facing observability.
//     They don't expose the wallet, the JWT secret, or any sign-scope path.
//   - The dashboard listener binds to 127.0.0.1 by default. Operators who
//     want remote access can put it behind a reverse proxy with their own
//     auth.

package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/internal/buildinfo"
	lbitswap "github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/combined"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
)

//go:embed dashboard/index.html dashboard/lantern-mark.svg dashboard/lantern-favicon.svg
var dashboardAssets embed.FS

// dashboardDeps bundles the data sources the dashboard endpoints need.
// Constructed in main once everything is wired up, then handed to
// registerDashboard. Any field may be nil; the handlers must tolerate it.
type dashboardDeps struct {
	tr           *trustedroot.TrustedRoot
	store        *store.Store
	sync         *store.Sync
	host         *llibp2p.Host
	bsClient     *lbitswap.Client
	fetcher      *combined.Fetcher
	ingestor     *gossipBlockIngestor
	vmBridgeTag  string // empty when no bridge configured
	allowSubmit  bool
	network      string
	rpcAddr      string
	startedAt    time.Time
	headDelaySec uint64
}

// registerDashboard attaches /dashboard and /api/dashboard/* to the mux.
// Safe to call once per daemon. mux is the same one serveMetrics uses.
func registerDashboard(mux *http.ServeMux, deps *dashboardDeps) {
	if deps == nil {
		return
	}

	// Serve the static UI from the embed.FS, rooted at "dashboard/" so
	// URLs like /dashboard/index.html map onto the file at
	// dashboard/index.html in the embed.FS.
	sub, err := fs.Sub(dashboardAssets, "dashboard")
	if err != nil {
		// Can't happen unless someone deletes the embedded directory at
		// build time; embed.FS errors are programmer errors.
		panic("dashboard sub fs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", fileServer))
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
	})

	// JSON endpoints. Each one is read-only, no auth (operator-bound
	// listener), and ~constant-time to compute.
	mux.HandleFunc("/api/dashboard/overview", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, deps.overview())
	})
	mux.HandleFunc("/api/dashboard/peers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, deps.peers())
	})
	mux.HandleFunc("/api/dashboard/sync", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, deps.syncSnapshot())
	})
	mux.HandleFunc("/api/dashboard/fetcher", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, deps.fetcherSnapshot())
	})
}

// ---- payload builders ----

func (d *dashboardDeps) overview() map[string]any {
	out := map[string]any{
		"now":                time.Now().Unix(),
		"version":            buildinfo.BuildVersion() + " " + d.network,
		"network":            d.network,
		"block_delay":        d.headDelaySec,
		"rpc_addr":           d.rpcAddr,
		"vm_bridge":          d.vmBridgeTag,
		"allow_block_submit": d.allowSubmit,
		"uptime_seconds":     int64(time.Since(d.startedAt).Seconds()),
	}

	if d.store != nil {
		if ts := d.store.Head(); ts != nil && len(ts.Blocks()) > 0 {
			out["head_epoch"] = int64(ts.Height())
			out["tipset_blocks"] = len(ts.Blocks())
			out["head_timestamp"] = int64(ts.Blocks()[0].Timestamp)
			cids := ts.Cids()
			if len(cids) > 0 {
				out["tipset_key"] = cids[0].String()
			}
		}
	}
	// F3 height / instance from the trusted root, when available.
	if d.tr != nil {
		out["f3_height"] = int64(d.tr.Epoch)
		out["f3_instance"] = d.tr.F3Instance
		out["state_root"] = d.tr.StateRoot.String()
		out["epoch"] = int64(d.tr.Epoch)
		if !d.tr.AcceptedAt.IsZero() {
			out["anchor_age_seconds"] = int64(time.Since(d.tr.AcceptedAt).Seconds())
		}
	}

	if d.host != nil {
		ni := d.host.NetInfo()
		peers := ni.Peers()
		bw := ni.BandwidthTotals()
		out["peers"] = len(peers)
		out["peers_min"] = d.host.MinPeers()
		out["peers_max"] = d.host.MaxPeers()
		out["bw_in"] = bw.TotalIn
		out["bw_out"] = bw.TotalOut
		out["bw_in_rate"] = int64(bw.RateIn)
		out["bw_out_rate"] = int64(bw.RateOut)
		out["reachability"] = reachabilityLabel(ni.AutoNatStatus().Reachability)
	}

	return out
}

func (d *dashboardDeps) peers() map[string]any {
	out := map[string]any{"peers": []any{}}
	if d.host == nil {
		return out
	}
	ni := d.host.NetInfo()
	plist := ni.Peers()
	dst := make([]map[string]any, 0, len(plist))
	for _, p := range plist {
		dst = append(dst, map[string]any{
			"id":    p.ID,
			"addrs": p.Addrs,
		})
	}
	out["peers"] = dst
	return out
}

func (d *dashboardDeps) syncSnapshot() map[string]any {
	out := map[string]any{}
	if d.sync != nil {
		s := d.sync.Stats()
		out["polls"] = s.Polls
		out["head_advances"] = s.HeadAdvances
		out["reorgs"] = s.Reorgs
		out["headers_added"] = s.HeadersAdded
		out["last_head_epoch"] = int64(s.LastHeadEpoch)
		out["last_error"] = s.LastError
	}
	if d.ingestor != nil {
		s := d.ingestor.Stats()
		out["gossip"] = map[string]any{
			"received":           s.Received,
			"installed":          s.Installed,
			"dedup":              s.Dedup,
			"skipped":            s.Skipped,
			"rejected":           s.Rejected,
			"backfilled":         s.Backfilled,
			"backfill_failed":    s.BackfillFailed,
			"last_install_epoch": int64(s.LastInstallEpoch),
		}
	}
	if d.host != nil {
		ks := d.host.KeepaliveStats()
		out["keepalive"] = map[string]any{
			"cycles":          ks.Cycles,
			"triggered":       ks.Triggered,
			"bootstrap_dials": ks.BootstrapDial,
			"routing_dials":   ks.RoutingDial,
			"last_peer_count": ks.LastPeerCount,
		}
	}
	return out
}

func (d *dashboardDeps) fetcherSnapshot() map[string]any {
	out := map[string]any{"sources": map[string]uint64{}}
	if d.fetcher != nil {
		out["sources"] = d.fetcher.Stats()
	}
	if d.bsClient != nil {
		s := d.bsClient.Stats()
		out["bitswap"] = map[string]any{
			"blocks":   s.GotBlocks,
			"errors":   s.Errors,
			"bytes_in": s.BytesIn,
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if err := enc.Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// reachabilityLabel maps libp2p network.Reachability ints to human strings
// (0=Unknown, 1=Public, 2=Private). Keeps the JSON readable without
// pulling libp2p types into the dashboard wire surface.
func reachabilityLabel(r int) string {
	switch r {
	case 1:
		return "Public"
	case 2:
		return "Private"
	default:
		return "Unknown"
	}
}
