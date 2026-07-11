// Unit tests for maybeAutoRefreshAnchor (#118).
//
// The helper is deliberately factored so tests can drive it without a
// live libp2p host, network, or quorum probe. We inject a fake probe
// function that returns a canned bootstrap.Finality (or an error), a
// fake writer that records what it was called with, and a controlled
// `now` clock. That way every branch of the helper is exercised in
// isolation.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
)

// stampedAnchor writes a placeholder bootstrap-anchor.json in dir and
// sets its mtime to `mtime`. Content is intentionally irrelevant — the
// helper only cares about the file's mtime (fresh vs stale), not its
// contents.
func stampedAnchor(t *testing.T, dir string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, "bootstrap-anchor.json")
	if err := os.WriteFile(p, []byte(`{"epoch":1,"tipsetKey":[],"stateRoot":"bafy","instance":1,"capturedAt":"2026-01-01T00:00:00Z","network":"mainnet"}`), 0o600); err != nil {
		t.Fatalf("write anchor placeholder: %v", err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return p
}

// TestAutoStaleReset_TriggeredByMtime: anchor stamped 24h ago +
// maxAge=12h → probe called, write called, returns (true, nil).
func TestAutoStaleReset_TriggeredByMtime(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-24*time.Hour))

	probeCalled := 0
	writeCalled := 0
	var writtenNetwork build.Network
	fakeFin := bootstrap.Finality{Instance: 42, Epoch: 5555555}

	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(ctx context.Context) (bootstrap.Finality, error) {
			probeCalled++
			return fakeFin, nil
		},
		write: func(d string, fin bootstrap.Finality, net build.Network) error {
			writeCalled++
			writtenNetwork = net
			if fin.Instance != 42 || fin.Epoch != 5555555 {
				t.Errorf("write: got fin=%v, want the fake finality", fin)
			}
			if d != dir {
				t.Errorf("write: got dir=%q, want %q", d, dir)
			}
			return nil
		},
		logf: func(string, ...interface{}) {}, // quiet in tests
	})
	if err != nil {
		t.Fatalf("maybeAutoRefreshAnchor: err=%v, want nil", err)
	}
	if !refreshed {
		t.Fatalf("refreshed=false, want true for a 24h-old anchor with 12h max-age")
	}
	if probeCalled != 1 {
		t.Errorf("probe called %d times, want 1", probeCalled)
	}
	if writeCalled != 1 {
		t.Errorf("write called %d times, want 1", writeCalled)
	}
	if writtenNetwork != build.Mainnet {
		t.Errorf("write network=%v, want mainnet", writtenNetwork)
	}
}

// TestAutoStaleReset_SkippedWhenFresh: anchor stamped 30 min ago +
// maxAge=12h → no probe, no write, returns (false, nil).
func TestAutoStaleReset_SkippedWhenFresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-30*time.Minute))

	probeCalled := 0
	writeCalled := 0

	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(ctx context.Context) (bootstrap.Finality, error) {
			probeCalled++
			return bootstrap.Finality{}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error {
			writeCalled++
			return nil
		},
		logf: func(string, ...interface{}) {},
	})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if refreshed {
		t.Errorf("refreshed=true, want false for fresh anchor")
	}
	if probeCalled != 0 {
		t.Errorf("probe called %d times, want 0 (fresh anchor)", probeCalled)
	}
	if writeCalled != 0 {
		t.Errorf("write called %d times, want 0 (fresh anchor)", writeCalled)
	}
}

// TestAutoStaleReset_QuorumFailureDoesNotOverwrite: probe returns
// ErrQuorumNotReached → no write, returns (false, nil) with warn log.
// This is the fail-warn contract: a probe blip must never brick the
// daemon.
func TestAutoStaleReset_QuorumFailureDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	anchorPath := stampedAnchor(t, dir, now.Add(-24*time.Hour))
	statBefore, err := os.Stat(anchorPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	writeCalled := 0
	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(ctx context.Context) (bootstrap.Finality, error) {
			return bootstrap.Finality{}, bootstrap.ErrQuorumNotReached
		},
		write: func(string, bootstrap.Finality, build.Network) error {
			writeCalled++
			return nil
		},
		logf: func(string, ...interface{}) {},
	})
	if err != nil {
		t.Fatalf("err=%v, want nil (fail-warn contract)", err)
	}
	if refreshed {
		t.Errorf("refreshed=true, want false when probe failed")
	}
	if writeCalled != 0 {
		t.Errorf("write called %d times after quorum failure, want 0", writeCalled)
	}
	// Anchor file must be untouched (same mtime, same size).
	statAfter, err := os.Stat(anchorPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Errorf("anchor mtime changed after probe failure: before=%v after=%v",
			statBefore.ModTime(), statAfter.ModTime())
	}
}

// TestAutoStaleReset_DisabledByFlag: enabled=false → no probe, no write,
// even with a stale anchor.
func TestAutoStaleReset_DisabledByFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-72*time.Hour))

	probeCalled := 0
	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   false, // master switch off
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(context.Context) (bootstrap.Finality, error) {
			probeCalled++
			return bootstrap.Finality{}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error { return nil },
		logf:  func(string, ...interface{}) {},
	})
	if err != nil || refreshed || probeCalled != 0 {
		t.Fatalf("disabled=false: got err=%v refreshed=%v probe=%d, want (nil, false, 0)", err, refreshed, probeCalled)
	}
}

// TestAutoStaleReset_SkippedInRPCMode: bridgeOff=false → no-op. RPC mode
// is handled by the polling Sync's StaleResetThreshold (#51); we must not
// double-fire.
func TestAutoStaleReset_SkippedInRPCMode(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-72*time.Hour))

	probeCalled := 0
	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: false, // RPC mode
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(context.Context) (bootstrap.Finality, error) {
			probeCalled++
			return bootstrap.Finality{}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error { return nil },
		logf:  func(string, ...interface{}) {},
	})
	if err != nil || refreshed || probeCalled != 0 {
		t.Fatalf("RPC mode: got err=%v refreshed=%v probe=%d, want (nil, false, 0)", err, refreshed, probeCalled)
	}
}

// TestAutoStaleReset_MaxAgeZeroDisables: maxAge=0 → no-op, matching the
// flag doc contract ("0 disables").
func TestAutoStaleReset_MaxAgeZeroDisables(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-72*time.Hour))

	probeCalled := 0
	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    0, // disabled
		now:       func() time.Time { return now },
		probe: func(context.Context) (bootstrap.Finality, error) {
			probeCalled++
			return bootstrap.Finality{}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error { return nil },
		logf:  func(string, ...interface{}) {},
	})
	if err != nil || refreshed || probeCalled != 0 {
		t.Fatalf("maxAge=0: got err=%v refreshed=%v probe=%d, want (nil, false, 0)", err, refreshed, probeCalled)
	}
}

// TestAutoStaleReset_NoAnchorFile: fresh install with no
// bootstrap-anchor.json → no-op, no error. `lantern init` handles cold
// installs; the auto-reset path has nothing to do.
func TestAutoStaleReset_NoAnchorFile(t *testing.T) {
	dir := t.TempDir() // empty

	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       time.Now,
		probe: func(context.Context) (bootstrap.Finality, error) {
			t.Errorf("probe called for missing anchor file")
			return bootstrap.Finality{}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error {
			t.Errorf("write called for missing anchor file")
			return nil
		},
		logf: func(string, ...interface{}) {},
	})
	if err != nil {
		t.Fatalf("err=%v, want nil for missing anchor file", err)
	}
	if refreshed {
		t.Fatalf("refreshed=true, want false when anchor file absent")
	}
}

// TestAutoStaleReset_WriteFailureReturnsError: write() returning an error
// is fatal (unlike probe failure, which is fail-warn). A partially-written
// anchor is worse than a stale one, so surface it loud.
func TestAutoStaleReset_WriteFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stampedAnchor(t, dir, now.Add(-24*time.Hour))
	sentinel := errors.New("disk full")

	refreshed, err := maybeAutoRefreshAnchor(context.Background(), autoStaleResetOpts{
		dir:       dir,
		network:   build.Mainnet,
		enabled:   true,
		bridgeOff: true,
		maxAge:    12 * time.Hour,
		now:       func() time.Time { return now },
		probe: func(context.Context) (bootstrap.Finality, error) {
			return bootstrap.Finality{Instance: 1, Epoch: 100}, nil
		},
		write: func(string, bootstrap.Finality, build.Network) error {
			return sentinel
		},
		logf: func(string, ...interface{}) {},
	})
	if err == nil {
		t.Fatalf("err=nil, want a wrapped disk-full error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err=%v, want wrap of %v", err, sentinel)
	}
	if refreshed {
		t.Errorf("refreshed=true, want false on write failure")
	}
}
