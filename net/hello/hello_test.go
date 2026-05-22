// Tests for the Hello protocol (issue #16).
//
// Two ends of a mocknet libp2p pair exchange Hello messages. Verifies:
//   - matching-genesis Hello is received, counted, and tags the peer
//   - mismatching-genesis Hello closes the stream and increments rejected
//   - SayHello on the wire round-trips correctly

package hello_test

import (
	"context"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Reiers/lantern/net/hello"
)

const testGenesisStr = "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2"

func mustGenesis(t *testing.T) cid.Cid {
	t.Helper()
	c, err := cid.Parse(testGenesisStr)
	if err != nil {
		t.Fatalf("parse genesis: %v", err)
	}
	return c
}

// TestHelloStats_Counters exercises the Stats() snapshot accessor with
// no other infrastructure. Sanity check on the public surface.
func TestHelloStats_Counters(t *testing.T) {
	gen := mustGenesis(t)
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	defer h.Close()

	svc := hello.NewService(h, gen, func() ([]cid.Cid, int64, string) {
		return []cid.Cid{gen}, 1, big.NewInt(42).String()
	})
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	stats := svc.Stats()
	if stats.Received != 0 || stats.Sent != 0 || stats.Rejected != 0 {
		t.Errorf("fresh service stats nonzero: %+v", stats)
	}
}

// TestHelloService_RegisterIdempotent verifies Register doesn't panic
// when called twice (the libp2p host overwrites the stream handler).
func TestHelloService_RegisterIdempotent(t *testing.T) {
	gen := mustGenesis(t)
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	defer h.Close()

	svc := hello.NewService(h, gen, nil)
	svc.Register()
	svc.Register() // must not panic
}

// TestHelloService_SayHelloRoundtrip runs A -> B with matching genesis
// and asserts B's received counter increments and A's sent counter
// increments. Uses real libp2p hosts wired locally.
func TestHelloService_SayHelloRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	gen := mustGenesis(t)

	hA, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("hA: %v", err)
	}
	defer hA.Close()
	hB, err := libp2p.New()
	if err != nil {
		t.Fatalf("hB: %v", err)
	}
	defer hB.Close()

	headA := func() ([]cid.Cid, int64, string) {
		return []cid.Cid{gen}, 100, big.NewInt(1000).String()
	}
	svcA := hello.NewService(hA, gen, headA)
	svcA.Register()

	svcB := hello.NewService(hB, gen, headA)
	svcB.Register()

	// Connect A -> B.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hA.Peerstore().AddAddrs(hB.ID(), hB.Addrs(), time.Hour)
	if err := hA.Connect(ctx, peer.AddrInfo{ID: hB.ID(), Addrs: hB.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// A says hello to B.
	if err := svcA.SayHello(ctx, hB.ID()); err != nil {
		t.Fatalf("SayHello: %v", err)
	}

	// Give B's stream handler a moment to run.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svcB.Stats().Received >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if svcA.Stats().Sent != 1 {
		t.Errorf("A.Sent = %d, want 1", svcA.Stats().Sent)
	}
	if svcB.Stats().Received != 1 {
		t.Errorf("B.Received = %d, want 1", svcB.Stats().Received)
	}
	if svcB.Stats().Rejected != 0 {
		t.Errorf("B.Rejected = %d, want 0", svcB.Stats().Rejected)
	}

	// B should have tagged A as "fcpeer" in its ConnManager.
	tags := hB.ConnManager().GetTagInfo(hA.ID())
	if tags == nil {
		t.Fatal("no tag info for A in B.ConnManager")
	}
	if _, ok := tags.Tags[hello.PeerTag]; !ok {
		t.Errorf("expected %q tag on A; tags=%v", hello.PeerTag, tags.Tags)
	}
}

// TestHelloService_GenesisMismatchRejects exercises the failure path:
// A says hello to B with a DIFFERENT genesis; B must close the stream
// and bump its Rejected counter without tagging A.
func TestHelloService_GenesisMismatchRejects(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	genA := mustGenesis(t)
	// Distinct genesis for B (just a different CID).
	genB, err := cid.Parse("bafy2bzaceakcaubevwt7yxhqxcwttvxjs2bcprc2yzpdfm26ufpdyqxqofmqi")
	if err != nil {
		t.Fatalf("parse alt genesis: %v", err)
	}

	hA, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("hA: %v", err)
	}
	defer hA.Close()
	hB, err := libp2p.New()
	if err != nil {
		t.Fatalf("hB: %v", err)
	}
	defer hB.Close()

	headA := func() ([]cid.Cid, int64, string) {
		return []cid.Cid{genA}, 100, big.NewInt(1000).String()
	}
	svcA := hello.NewService(hA, genA, headA)
	svcA.Register()
	svcB := hello.NewService(hB, genB, headA) // B has a DIFFERENT genesis
	svcB.Register()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hA.Peerstore().AddAddrs(hB.ID(), hB.Addrs(), time.Hour)
	if err := hA.Connect(ctx, peer.AddrInfo{ID: hB.ID(), Addrs: hB.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// A says hello with its (mainnet) genesis to B (which expects a
	// different genesis).
	if err := svcA.SayHello(ctx, hB.ID()); err != nil {
		t.Fatalf("SayHello: %v", err)
	}
	// Wait for B to process.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svcB.Stats().Rejected >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if svcB.Stats().Rejected != 1 {
		t.Errorf("B.Rejected = %d, want 1", svcB.Stats().Rejected)
	}
	if svcB.Stats().Received != 0 {
		t.Errorf("B.Received = %d, want 0 (mismatch should NOT count as received)", svcB.Stats().Received)
	}

	// B must NOT have tagged A.
	if tags := hB.ConnManager().GetTagInfo(hA.ID()); tags != nil {
		if _, ok := tags.Tags[hello.PeerTag]; ok {
			t.Errorf("B incorrectly tagged A despite genesis mismatch")
		}
	}
}
