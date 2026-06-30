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

// Issue #9: KeepaliveStats should exist and be readable as a snapshot
// after EnableDHT, even when no bootstrap peers respond. Doesn't assert
// non-zero cycle counts because the keepalive loop has a 15s startup
// delay; we just verify the surface is wired and returns zero values.
func TestKeepaliveStats_Surface(t *testing.T) {
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

	err = h.EnableDHT(ctx, llibp2p.DHTOptions{})
	require.NoError(t, err)

	s := h.KeepaliveStats()
	// All counters are zero immediately after EnableDHT (the keepalive
	// loop has a 15s startup delay before its first tick).
	require.Equal(t, uint64(0), s.Cycles)
	require.Equal(t, uint64(0), s.Triggered)
	require.Equal(t, uint64(0), s.BootstrapDial)
	require.Equal(t, uint64(0), s.RoutingDial)
	require.Equal(t, 0, s.LastPeerCount)
}

// TestProtectPeers_UnevictableFloor covers #80 part 1: peers passed to
// ProtectPeers are marked connmgr-protected so the trim path can never
// evict them (the trusted-floor anti-eclipse property).
func TestProtectPeers_UnevictableFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode (constructs real libp2p hosts)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Host under test.
	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	// A second host to borrow a real peer identity + dialable multiaddr.
	other, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer other.Close()

	// Build a /p2p/ multiaddr for the other host.
	addrs := other.ListenAddrs()
	require.NotEmpty(t, addrs)
	full := addrs[0] + "/p2p/" + other.ID().String()

	n := h.ProtectPeers([]string{full, "garbage-not-a-multiaddr"}, "test-floor")
	require.Equal(t, 1, n, "exactly one valid peer should be protected (garbage skipped)")

	// The connmgr must now report the other peer as protected under the tag.
	cm := h.H.ConnManager()
	require.NotNil(t, cm)
	require.True(t, cm.IsProtected(other.ID(), "test-floor"),
		"protected peer must be reported as protected (un-evictable floor)")
}
