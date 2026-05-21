// Phase 10: integration test — two Bitswap-equipped hosts, one serves a
// block, the other fetches it through Lantern's Client.

package bitswap_test

import (
	"context"
	"testing"
	"time"

	boxobs "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bstore "github.com/ipfs/boxo/blockstore"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"

	lbitswap "github.com/Reiers/lantern/net/bitswap"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestBitswap_FetchFromPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Server host: has Bitswap with one known block in its blockstore.
	server, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	defer server.Close()

	serverBS := bstore.NewBlockstore(dssync.MutexWrap(ds.NewMapDatastore()))
	blk := blocks.NewBlock([]byte("hello-bitswap-from-lantern"))
	if err := serverBS.Put(ctx, blk); err != nil {
		t.Fatalf("server put: %v", err)
	}
	sBs := boxobs.New(ctx, bsnet.NewFromIpfsHost(server.H), noopRouter{}, serverBS)
	defer sBs.Close()

	// Client host: uses Lantern's Bitswap wrapper.
	client, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatalf("client host: %v", err)
	}
	defer client.Close()

	bs, err := lbitswap.New(ctx, lbitswap.Config{
		Host:         client.H,
		FullDeadline: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("bitswap.New: %v", err)
	}
	defer bs.Close()

	// Connect client → server AFTER bitswap on both sides is wired so the
	// connect-event manager sees the connection.
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	if err := client.H.Connect(cctx, peer.AddrInfo{
		ID:    server.H.ID(),
		Addrs: server.H.Addrs(),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Give Bitswap's connect-event manager a moment to register the peer
	// and the multistream-select to settle on the bitswap protocol.
	time.Sleep(500 * time.Millisecond)

	// Fetch the block via our Client.
	gctx, gcancel := context.WithTimeout(ctx, 12*time.Second)
	defer gcancel()
	got, err := bs.Get(gctx, blk.Cid())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello-bitswap-from-lantern" {
		t.Fatalf("unexpected payload: %q", got)
	}

	st := bs.Stats()
	if st.GotBlocks == 0 {
		t.Errorf("expected GotBlocks > 0, got %d", st.GotBlocks)
	}
	if st.BytesIn == 0 {
		t.Errorf("expected BytesIn > 0, got %d", st.BytesIn)
	}
}

// noopRouter is the server-side test stand-in. Server doesn't need a
// router; it only answers WANT-HAVE/WANT-BLOCK requests for CIDs in its
// blockstore.
type noopRouter struct{}

func (noopRouter) FindProvidersAsync(_ context.Context, _ cid.Cid, _ int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo)
	close(ch)
	return ch
}
