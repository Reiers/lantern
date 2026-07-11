// Auto-stale-reset (#118): bridge-off boot recovery from long outages.
//
// #51 gave RPC-mode Lantern the "restart after a week and it just catches
// up" property via the polling Sync's StaleResetThreshold path. In
// bridge-off mode (--no-fallback-rpc) that path is a no-op: there's no
// RPC head-source to report a live head that the polling Sync could
// compare against. So a bridge-off node that was stopped past the
// parentWalkCap (default 2880 epochs / ~24h) boots wedged — the persisted
// anchor + header store are old, and every parent-walk from the fresh
// gossip head exceeds the cap.
//
// This file is the parallel code for bridge-off: on daemon boot, if the
// persisted bootstrap-anchor.json is older than --anchor-max-age (default
// 12h), re-run the same multi-source quorum probe `lantern repair` uses
// and overwrite the anchor with a fresh finality. The subsequent boot
// steps then treat this as a normal fresh-anchor start; the
// ChainExchange seed (main.go:1326) re-seats the header store from the
// fresh anchor tipset, and #117's parentWalkCap covers whatever gap
// remains between the fresh anchor and the live head.
//
// Chain state only. Keys, JWT, tokens are never touched (same allow-list
// discipline as `lantern reset --chain-state`, enforced by never
// touching anything outside `bootstrap-anchor.json`). Quorum probe
// failure is fail-warn (not fail-fatal): we continue boot with the
// stale anchor and let the existing boot paths do their best. A probe
// blip should not take the daemon down.
//
// The helper is factored out of cmdDaemon so it can be unit-tested with
// a fake probe + fake writer (no live network needed).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
)

// autoStaleResetOpts bundles the arguments maybeAutoRefreshAnchor needs.
// The probe + write function pointers are injectable so tests can drive
// the helper without a live libp2p host or filesystem quorum probe.
type autoStaleResetOpts struct {
	// dir is the per-network data directory (e.g. ~/.lantern/mainnet).
	dir string
	// network is the resolved build.Network.
	network build.Network
	// enabled is the master switch (defaults to true in bridge-off mode).
	enabled bool
	// bridgeOff mirrors *noFallbackRPC; the helper is a no-op outside
	// bridge-off since the polling Sync already covers the RPC path.
	bridgeOff bool
	// maxAge is the anchor mtime threshold. Zero disables the mtime check.
	maxAge time.Duration
	// now is injectable so tests can control the mtime comparison.
	now func() time.Time
	// probe re-runs the multi-source quorum. Wraps runBootstrapQuorum in
	// production; tests inject a fake.
	probe func(ctx context.Context) (bootstrap.Finality, error)
	// write overwrites bootstrap-anchor.json with a fresh finality.
	// Wraps writeBootstrapAnchor in production; tests inject a fake.
	write func(dir string, fin bootstrap.Finality, net build.Network) error
	// logf is the human-readable log sink ("auto-stale-reset: ...").
	// Defaults to fmt.Printf when nil.
	logf func(format string, args ...interface{})
}

// maybeAutoRefreshAnchor inspects the persisted bootstrap-anchor.json
// and, if it is older than opts.maxAge, re-runs the multi-source quorum
// probe and overwrites the anchor. Returns (refreshed, error).
//
// Guarantees:
//   - No-op when opts.enabled is false, opts.bridgeOff is false, or
//     opts.maxAge is zero.
//   - No-op when the anchor file doesn't exist yet (cold install).
//   - Never touches anything other than the anchor file itself. Keys,
//     JWT, tokens, header store, secrets/, backups/ are never referenced.
//   - Quorum probe failure returns (false, nil) with a warn log — the
//     boot continues with the stale anchor.
//   - Only successful probe → successful write returns (true, nil).
//   - Any error from write() is returned (that path already refuses to
//     overwrite on partial data; a filesystem failure at write time is
//     fatal because the anchor may be half-written).
func maybeAutoRefreshAnchor(ctx context.Context, opts autoStaleResetOpts) (bool, error) {
	logf := opts.logf
	if logf == nil {
		logf = func(format string, args ...interface{}) {
			fmt.Printf(format, args...)
		}
	}
	if !opts.enabled {
		return false, nil
	}
	if !opts.bridgeOff {
		// RPC mode: polling Sync's StaleResetThreshold path (#51) already
		// handles this. Skip so the two paths don't race.
		return false, nil
	}
	if opts.maxAge <= 0 {
		return false, nil
	}

	anchorPath := filepath.Join(opts.dir, "bootstrap-anchor.json")
	fi, statErr := os.Stat(anchorPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			// Cold install; nothing to refresh. `lantern init` writes the
			// first anchor and boot then proceeds normally.
			return false, nil
		}
		return false, fmt.Errorf("auto-stale-reset: stat %s: %w", anchorPath, statErr)
	}

	now := opts.now
	if now == nil {
		now = time.Now
	}
	age := now().Sub(fi.ModTime())
	if age < opts.maxAge {
		// Anchor is fresh enough; leave it.
		return false, nil
	}

	logf("  auto-stale-reset: bootstrap-anchor is %s old (max-age %s); re-running multi-source quorum probe before boot — chain state only, keys untouched (#118)\n",
		roundDur(age), opts.maxAge)

	if opts.probe == nil {
		return false, fmt.Errorf("auto-stale-reset: nil probe (internal error)")
	}
	fin, err := opts.probe(ctx)
	if err != nil {
		// Fail-warn, not fail-fatal: a probe blip must not brick the
		// daemon. Boot continues with the stale anchor; the existing
		// #117 parent-walk cap + gossip-based recovery handle whatever
		// they can. If the outage was too long they surface as a
		// wedged head, which is at least observable.
		logf("  auto-stale-reset: quorum probe failed (%v); continuing boot with existing anchor\n", err)
		return false, nil
	}

	if opts.write == nil {
		return false, fmt.Errorf("auto-stale-reset: nil write (internal error)")
	}
	if werr := opts.write(opts.dir, fin, opts.network); werr != nil {
		return false, fmt.Errorf("auto-stale-reset: write fresh anchor: %w", werr)
	}
	logf("  auto-stale-reset: fresh anchor written @ epoch %d (was %s stale; #118)\n",
		fin.Epoch, roundDur(age))
	return true, nil
}

// roundDur is a tiny formatter that trims noise from time.Duration
// (e.g. `12h34m56s` instead of `12h34m56.123456789s`).
func roundDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d
	}
	return d.Round(time.Second)
}
