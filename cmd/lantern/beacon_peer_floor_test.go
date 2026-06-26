package main

import (
	"fmt"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/build"
)

func mkPeer(t *testing.T, n int) peer.AddrInfo {
	t.Helper()
	// Deterministic fake peer ids via multihash-encoded identity.
	ma, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", 3000+n))
	if err != nil {
		t.Fatal(err)
	}
	// Build a peer.ID from a fake key-ish string.
	pid, err := peer.Decode("12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt")
	if err != nil {
		t.Fatal(err)
	}
	return peer.AddrInfo{ID: pid, Addrs: []multiaddr.Multiaddr{ma}}
}

// #59: the dynamic (DHT) pool must be capped so a rendezvous flood can't
// grow the rotation unboundedly.
func TestSetDynamic_CapsPool(t *testing.T) {
	prov := &dhtBeaconProvider{}
	flood := make([]peer.AddrInfo, build.MaxDynamicBeaconPeers+50)
	for i := range flood {
		flood[i] = mkPeer(t, i)
	}
	prov.setDynamic(flood)
	prov.mu.RLock()
	got := len(prov.dyn)
	prov.mu.RUnlock()
	if got > build.MaxDynamicBeaconPeers {
		t.Fatalf("dynamic pool = %d, want <= %d", got, build.MaxDynamicBeaconPeers)
	}
}

// #59: pinned (trusted floor + operator pins) always precede dynamic peers
// in Peers() and are never evicted by a dynamic flood.
func TestPeers_PinnedPrecedeDynamic(t *testing.T) {
	pinned := mkPeer(t, 1)
	prov := &dhtBeaconProvider{pinned: []peer.AddrInfo{pinned}}
	prov.setDynamic([]peer.AddrInfo{mkPeer(t, 2), mkPeer(t, 3)})
	out := prov.Peers()
	if len(out) == 0 {
		t.Fatal("expected peers")
	}
	if out[0].ID != pinned.ID {
		t.Fatalf("pinned peer must be first, got %s", out[0].ID)
	}
}

// MaxDynamicBeaconPeers is a sane positive bound.
func TestMaxDynamicBeaconPeers_Sane(t *testing.T) {
	if build.MaxDynamicBeaconPeers <= 0 || build.MaxDynamicBeaconPeers > 256 {
		t.Fatalf("MaxDynamicBeaconPeers = %d, want a small positive cap", build.MaxDynamicBeaconPeers)
	}
}
