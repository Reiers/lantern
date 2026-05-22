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
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/internal/buildinfo"
	lbitswap "github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/chainxchg"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/hello"
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

	// Issue #14: action handlers need to know the data directory (to
	// write the refreshed bootstrap-anchor.json) and the default gateway
	// (passed to runBootstrapQuorum). They're also coordinated by
	// actionsMu so concurrent button presses can't run two quorum probes
	// at once.
	dataDirPath string
	gatewayURL  string
	actionsMu   sync.Mutex

	// Issue #16: Hello service activity (received / sent / rejected).
	hello *hello.Service
	// Issue #17: ChainExchange responder activity (received / rejected).
	xchg *chainxchg.Service
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

	// Issue #14: operator action endpoints. These mutate node state and
	// must therefore be POST-only with same-origin guard. The dashboard
	// JS sets X-Lantern-Origin to 'dashboard' on every action POST.
	mux.HandleFunc("/api/dashboard/actions/find-peers", func(w http.ResponseWriter, r *http.Request) {
		if !actionPreflight(w, r) {
			return
		}
		writeJSON(w, deps.actionFindPeers(r.Context()))
	})
	mux.HandleFunc("/api/dashboard/actions/renew-anchor", func(w http.ResponseWriter, r *http.Request) {
		if !actionPreflight(w, r) {
			return
		}
		writeJSON(w, deps.actionRenewAnchor(r.Context()))
	})
}

// actionPreflight enforces POST + same-origin header on every dashboard
// action endpoint. Returns true when the request may proceed.
//
// Same-origin guard: we require the X-Lantern-Origin header to equal
// 'dashboard'. Browsers refuse to set custom headers via simple form
// submission or <img>/<script> CSRF vectors; only XHR/fetch from a page
// served from this same daemon can set it. This is the same CSRF
// defense lots of small services use. It does NOT protect against an
// attacker with local-shell access (they can curl), but the dashboard
// listener is 127.0.0.1-bound; if you have local-shell access you
// already own the node.
func actionPreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return false
	}
	if r.Header.Get("X-Lantern-Origin") != "dashboard" {
		http.Error(w, "missing X-Lantern-Origin", http.StatusForbidden)
		return false
	}
	return true
}

// actionResult is the wire shape every action handler returns.
type actionResult struct {
	Status  string         `json:"status"` // "ok" or "error"
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// actionFindPeers exposes Host.TriggerKeepalive as a click-to-fire button.
// Returns peer count before/after and the keepalive counters so the UI can
// show the operator what just happened.
func (d *dashboardDeps) actionFindPeers(ctx context.Context) actionResult {
	d.actionsMu.Lock()
	defer d.actionsMu.Unlock()
	if d.host == nil {
		return actionResult{Status: "error", Message: "libp2p host not running"}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	before, after, err := d.host.TriggerKeepalive(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard action find-peers: %v\n", err)
		return actionResult{Status: "error", Message: err.Error()}
	}
	ks := d.host.KeepaliveStats()
	fmt.Fprintf(os.Stderr, "dashboard action find-peers: peers %d -> %d (dialed %d this cycle, %d cumulative routing dials)\n",
		before, after, after-before, ks.RoutingDial)
	msg := fmt.Sprintf("%d peers → %d peers", before, after)
	if after > before {
		msg = fmt.Sprintf("+%d peer connections (%d → %d)", after-before, before, after)
	} else if after == before {
		msg = fmt.Sprintf("no change (%d peers; dials attempted, none stuck this cycle)", after)
	}
	return actionResult{
		Status:  "ok",
		Message: msg,
		Detail: map[string]any{
			"peers_before": before,
			"peers_after":  after,
			"keepalive":    map[string]any{"cycles": ks.Cycles, "triggered": ks.Triggered, "routing_dials": ks.RoutingDial, "bootstrap_dials": ks.BootstrapDial, "stuck": ks.Stuck},
		},
	}
}

// actionRenewAnchor re-runs the bootstrap quorum probe and overwrites
// ~/.lantern/bootstrap-anchor.json on success. Same flow as
// 'lantern repair'. Refuses to overwrite when quorum isn't reached.
func (d *dashboardDeps) actionRenewAnchor(ctx context.Context) actionResult {
	d.actionsMu.Lock()
	defer d.actionsMu.Unlock()
	if d.dataDirPath == "" {
		return actionResult{Status: "error", Message: "data directory not configured"}
	}
	gw := d.gatewayURL
	if gw == "" {
		gw = defaultGateway
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	fmt.Fprintln(os.Stderr, "dashboard action renew-anchor: running 5-of-N bootstrap quorum probe...")
	var progressLog []string
	fin, err := runBootstrapQuorum(ctx, bootstrapParams{
		Quorum:       5,
		Timeout:      60 * time.Second,
		Gateway:      gw,
		CountGateway: false,
		NoLibp2p:     false,
		Libp2pSettle: 8 * time.Second,
		NetworkName:  "filecoin",
		Progress: func(sr bootstrap.SourceResult) {
			status := "ok"
			if sr.Error != nil {
				status = sr.Error.Error()
			} else if !sr.OK() {
				status = "no finality"
			}
			progressLog = append(progressLog, fmt.Sprintf("%s: %s", sr.Name, status))
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard action renew-anchor: quorum failed: %v\n", err)
		return actionResult{
			Status:  "error",
			Message: "quorum probe failed: " + err.Error(),
			Detail:  map[string]any{"sources": progressLog},
		}
	}
	if err := writeBootstrapAnchor(d.dataDirPath, fin); err != nil {
		fmt.Fprintf(os.Stderr, "dashboard action renew-anchor: anchor write failed: %v\n", err)
		return actionResult{Status: "error", Message: "failed to write anchor: " + err.Error()}
	}
	// Best-effort: refresh in-memory TrustedRoot.AcceptedAt so the UI's
	// anchor-age starts counting from the renewal moment. Full re-load
	// would require recomputing AncestorRoots; defer that to next daemon
	// restart.
	if d.tr != nil {
		d.tr.AcceptedAt = time.Now().UTC()
		d.tr.F3Instance = fin.Instance
	}
	fmt.Fprintf(os.Stderr, "dashboard action renew-anchor: epoch=%d state_root=%s\n", fin.Epoch, fin.StateRoot)
	return actionResult{
		Status:  "ok",
		Message: fmt.Sprintf("trust anchor refreshed at epoch %d", fin.Epoch),
		Detail: map[string]any{
			"epoch":      fin.Epoch,
			"state_root": fin.StateRoot.String(),
			"instance":   fin.Instance,
			"sources":    progressLog,
		},
	}
}

// silence the linter when build is unused (it's used through bootstrapParams).
var _ = build.MainnetNetworkName

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
			"stuck":           ks.Stuck,
			"closest_walks":   ks.ClosestWalks,
			"last_peer_count": ks.LastPeerCount,
		}
	}
	if d.hello != nil {
		hs := d.hello.Stats()
		out["hello"] = map[string]any{
			"received": hs.Received,
			"sent":     hs.Sent,
			"rejected": hs.Rejected,
		}
	}
	if d.xchg != nil {
		xs := d.xchg.Stats()
		out["chainxchg"] = map[string]any{
			"received": xs.Received,
			"rejected": xs.Rejected,
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
