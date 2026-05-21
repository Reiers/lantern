// Phase 10: end-to-end test that two libp2p hosts connected to each other
// expose their connectedness through the handlers.NetInfo adapter, and that
// the BandwidthCounter records bytes after at least one stream.

package libp2p_test

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/rpc/handlers"
)

func TestNetInfo_LivePeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h1, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		MaxPeers:    8,
	})
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	defer h1.Close()
	h2, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		MaxPeers:    8,
	})
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	defer h2.Close()

	// Connect h1 → h2.
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	if err := h1.H.Connect(cctx, peer.AddrInfo{
		ID:    h2.H.ID(),
		Addrs: h2.H.Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Allow identify/security handshake to populate peerstore.
	time.Sleep(200 * time.Millisecond)

	ni := h1.NetInfo()
	if _, ok := ni.(handlers.NetInfo); !ok {
		t.Fatalf("net info doesn't satisfy handlers.NetInfo")
	}

	peers := ni.Peers()
	if len(peers) == 0 {
		t.Fatalf("expected at least 1 peer, got 0")
	}
	found := false
	for _, p := range peers {
		if p.ID == h2.H.ID().String() {
			found = true
			if len(p.Addrs) == 0 {
				t.Errorf("peer %s has zero addrs", p.ID)
			}
		}
	}
	if !found {
		t.Errorf("h2 not present in h1.Peers(); got %+v", peers)
	}

	// Connectedness should be Connected (1).
	if c := ni.Connectedness(h2.H.ID().String()); c != 1 {
		t.Errorf("expected Connectedness=1 (Connected), got %d", c)
	}

	// Listening: host has bound at least one addr.
	if !ni.Listening() {
		t.Errorf("expected Listening()=true")
	}

	// Bandwidth counter is wired and increases after identify exchange.
	// Wait briefly for identify bytes to flush through the counter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := ni.BandwidthTotals()
		if s.TotalIn > 0 || s.TotalOut > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := ni.BandwidthTotals()
	if s.TotalIn == 0 && s.TotalOut == 0 {
		t.Errorf("expected non-zero bandwidth totals after identify; got %+v", s)
	}

	// AutoNAT defaults to Unknown immediately; just sanity-check the call.
	nat := ni.AutoNatStatus()
	if nat.Reachability < 0 || nat.Reachability > 3 {
		t.Errorf("unexpected reachability value %d", nat.Reachability)
	}
}
