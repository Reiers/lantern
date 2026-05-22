// Tests for the embeddable daemon API surface.

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Reiers/lantern/wallet"
)

func TestConfig_ValidationRequiresDataDir(t *testing.T) {
	w, _ := wallet.New(context.Background(), t.TempDir(), "")
	_, err := New(Config{Wallet: w})
	if err == nil {
		t.Fatal("expected error for missing DataDir")
	}
}

func TestConfig_ValidationRequiresWallet(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for missing Wallet")
	}
}

func TestConfig_ValidationAllowBlockSubmitRequiresBridge(t *testing.T) {
	w, _ := wallet.New(context.Background(), t.TempDir(), "")
	_, err := New(Config{
		DataDir:          t.TempDir(),
		Wallet:           w,
		AllowBlockSubmit: true,
	})
	if err == nil {
		t.Fatal("expected error for AllowBlockSubmit without VMBridgeRPC")
	}
}

func TestConfig_DefaultsApplied(t *testing.T) {
	w, _ := wallet.New(context.Background(), t.TempDir(), "")
	d, err := New(Config{
		DataDir: t.TempDir(),
		Wallet:  w,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := d.Config()
	if c.Gateway != "https://gateway.lantern.reiers.io" {
		t.Errorf("Gateway default = %q, want lantern gateway", c.Gateway)
	}
	if c.RPCListen != "127.0.0.1:1234" {
		t.Errorf("RPCListen default = %q, want 127.0.0.1:1234", c.RPCListen)
	}
	if c.SyncInterval != 6*time.Second {
		t.Errorf("SyncInterval default = %v, want 6s", c.SyncInterval)
	}
	if c.NotifyBufSize != 64 {
		t.Errorf("NotifyBufSize default = %d, want 64", c.NotifyBufSize)
	}
	if c.BitswapFastDeadline != 1500*time.Millisecond {
		t.Errorf("BitswapFastDeadline default = %v", c.BitswapFastDeadline)
	}
	if c.BitswapFullDeadline != 5*time.Second {
		t.Errorf("BitswapFullDeadline default = %v", c.BitswapFullDeadline)
	}
	if c.VMBridgeTimeout != 30*time.Second {
		t.Errorf("VMBridgeTimeout default = %v", c.VMBridgeTimeout)
	}
	if c.Network != "mainnet" {
		t.Errorf("Network default = %q, want mainnet", c.Network)
	}
}

// TestStartAnchorsAgainstNetwork is a smoke test that exercises the
// load-bearing first step (TrustedRoot capture) against the real Lantern
// gateway. Skipped in -short mode because it makes a network call.
func TestStartAnchorsAgainstNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	w, _ := wallet.New(context.Background(), t.TempDir(), "")
	d, err := New(Config{
		DataDir:      t.TempDir(),
		Wallet:       w,
		EmbeddedMode: true,
		NoLibp2p:     true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Start(ctx) }()

	// Wait for the daemon to anchor + report Started.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if d.Started() {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("Start returned before Started became true: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !d.Started() {
		t.Fatal("daemon never reached Started state within 20s")
	}
	tr := d.TrustedRoot()
	if tr == nil {
		t.Fatal("TrustedRoot is nil after Started")
	}
	if tr.Epoch == 0 {
		t.Errorf("TrustedRoot epoch is zero, expected > 0")
	}
	if d.HeadEpoch() != tr.Epoch {
		t.Errorf("HeadEpoch() = %d, TrustedRoot().Epoch = %d (must match)", d.HeadEpoch(), tr.Epoch)
	}

	// Clean shutdown.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := d.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}

	// Start should have returned cleanly after Stop.
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Start did not return within 2s of Stop")
	}
}
