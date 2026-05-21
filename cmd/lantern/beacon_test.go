// Phase 10 Part C — integration test for the beacon flow.
//
// Sets up:
//   - A beacon-shaped libp2p host with a badgerBlockstore preloaded with
//     one block, serving Bitswap.
//   - A client libp2p host running Lantern's net/bitswap.Client pointed at
//     the beacon as a preferred peer.
//   - Verifies the client successfully fetches the block via Bitswap.

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	boxobs "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"

	lbitswap "github.com/Reiers/lantern/net/bitswap"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
)

type noProviders struct{}

func (noProviders) FindProvidersAsync(_ context.Context, _ cid.Cid, _ int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo)
	close(ch)
	return ch
}

var _ routing.ContentDiscovery = noProviders{}

func TestBeacon_E2E_FetchViaBeacon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Beacon-side: a libp2p host + badger blockstore with one block + boxo Bitswap.
	beaconHost, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatalf("beacon host: %v", err)
	}
	defer beaconHost.Close()

	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "blocks")).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()
	bs := newBadgerBlockstore(db)

	blk := blocks.NewBlock([]byte("beacon-served-block"))
	if err := bs.Put(ctx, blk); err != nil {
		t.Fatalf("put: %v", err)
	}

	beaconBS := boxobs.New(ctx, bsnet.NewFromIpfsHost(beaconHost.H), noProviders{}, bs)
	defer beaconBS.Close()

	// Client-side: a libp2p host using Lantern's net/bitswap.Client, with
	// the beacon listed as a preferred peer.
	clientHost, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatalf("client host: %v", err)
	}
	defer clientHost.Close()

	preferred := []peer.AddrInfo{{
		ID:    beaconHost.H.ID(),
		Addrs: beaconHost.H.Addrs(),
	}}

	cli, err := lbitswap.New(ctx, lbitswap.Config{
		Host:           clientHost.H,
		PreferredPeers: preferred,
		FastDeadline:   3 * time.Second,
		FullDeadline:   8 * time.Second,
	})
	if err != nil {
		t.Fatalf("bitswap.New: %v", err)
	}
	defer cli.Close()

	// Give the client a moment to dial the beacon.
	time.Sleep(500 * time.Millisecond)

	gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
	defer gcancel()
	got, err := cli.Get(gctx, blk.Cid())
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	if string(got) != "beacon-served-block" {
		t.Fatalf("payload mismatch: %q", got)
	}

	st := cli.Stats()
	if st.GotBlocks == 0 {
		t.Errorf("expected GotBlocks > 0, got %d", st.GotBlocks)
	}
	if st.BytesIn == 0 {
		t.Errorf("expected BytesIn > 0, got %d", st.BytesIn)
	}

	// Beacon-side stats should show the block stored locally.
	nb, nbytes := bs.Stats()
	if nb == 0 || nbytes == 0 {
		t.Errorf("beacon blockstore stats: blocks=%d bytes=%d", nb, nbytes)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1024":   1024,
		"1KiB":   1024,
		"1k":     1024,
		"500MB":  500_000_000,
		"5GiB":   5 * (1 << 30),
		"1.5GiB": int64(1.5 * float64(1<<30)),
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
