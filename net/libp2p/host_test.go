package libp2p_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llibp2p "github.com/Reiers/lantern/net/libp2p"
)

func TestHostStartsAndListens(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	require.NotEmpty(t, h.ID().String())
	require.NotEmpty(t, h.ListenAddrs())
	require.NotNil(t, h.PubSub)
}

// Phase 8 Part D: smoke test that EnableDHT runs without panicking
// even when no bootstrap peers respond. The DHT itself bootstraps
// async; we only verify the synchronous setup path doesn't fail.
func TestEnableDHT_NoPeers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	err = h.EnableDHT(ctx, llibp2p.DHTOptions{
		BootstrapPeers:  nil,
		RefreshInterval: 1 * time.Second, // fast for test
		TargetPeers:     30,
	})
	require.NoError(t, err)

	// Calling EnableDHT a second time must error (single-init).
	err = h.EnableDHT(ctx, llibp2p.DHTOptions{})
	require.Error(t, err)
}
