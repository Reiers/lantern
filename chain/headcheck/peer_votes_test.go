// Tests for #154: libp2p peer-group votes give bridge-off (--no-fallback-rpc)
// a running head quorum with zero RPC sources.

package headcheck_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/headcheck"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/blockingest"
	"github.com/Reiers/lantern/net/peerheads"
)

// chainTo builds a single-block canonical chain 0..head and returns the
// store plus the block CIDs by height.
func chainTo(t *testing.T, head abi.ChainEpoch) (*hstore.Store, map[abi.ChainEpoch]cid.Cid) {
	t.Helper()
	s, err := hstore.Open(filepath.Join(t.TempDir(), "hs"), hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cids := map[abi.ChainEpoch]cid.Cid{}
	var parents []cid.Cid
	for h := abi.ChainEpoch(0); h <= head; h++ {
		b := blk(t, h, parents)
		ts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		if err := s.SetHead(context.Background(), ts); err != nil {
			t.Fatal(err)
		}
		cids[h] = b.Cid()
		parents = []cid.Cid{b.Cid()}
	}
	return s, cids
}

func otherCID(t *testing.T, tag string) cid.Cid {
	m, _ := mh.Sum([]byte("other-"+tag), mh.SHA2_256, -1)
	return cid.NewCidV1(cid.DagCBOR, m)
}

// book maps peer IDs to public IPs in distinct /16s (one voter each).
func book(t *testing.T, ips map[peer.ID]string) *peerheads.Book {
	return peerheads.New(func(p peer.ID) multiaddr.Multiaddr {
		ip, ok := ips[p]
		if !ok {
			return nil
		}
		m, err := multiaddr.NewMultiaddr("/ip4/" + ip + "/tcp/1234")
		if err != nil {
			t.Fatal(err)
		}
		return m
	})
}

func TestPeerVotesFrom_Classification(t *testing.T) {
	s, cids := chainTo(t, 10)
	b := book(t, map[peer.ID]string{"on": "8.8.4.4", "fork": "9.9.9.9", "hello": "1.1.1.1", "ahead": "4.4.4.4"})
	// Gossip block at 11 whose parent is our block at 10 => on chain.
	b.ObserveBlock("on", otherCID(t, "on11"), 11, []cid.Cid{cids[10]})
	// Gossip block at 11 whose parent is NOT our block at 10 => forked.
	b.ObserveBlock("fork", otherCID(t, "fork11"), 11, []cid.Cid{otherCID(t, "fork10")})
	// Hello announcing our own head tipset => on chain.
	b.Observe("hello", []cid.Cid{cids[10]}, 10)
	// Gossip block far ahead of our store: can't tell => height-only.
	b.ObserveBlock("ahead", otherCID(t, "a50"), 50, []cid.Cid{otherCID(t, "a49")})

	got := map[string]int{}
	for _, v := range headcheck.PeerVotesFrom(b, s, time.Minute)(10) {
		got[v.Voter] = v.OnChain
	}
	want := map[string]int{"ip4:8.8.0.0/16": 1, "ip4:9.9.0.0/16": -1, "ip4:1.1.0.0/16": 1, "ip4:4.4.0.0/16": 0}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("voter %s: OnChain=%d want %d (all=%v)", k, got[k], w, got)
		}
	}
}

func startBridgeOff(t *testing.T, s *hstore.Store, b *peerheads.Book) (*headcheck.Monitor, chan headcheck.Result) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ing := blockingest.New(s, nil)
	rounds := make(chan headcheck.Result, 8)
	// No RPC sources at all: bridge-off.
	mon := headcheck.StartGated(ctx, ing, s, nil, func(r headcheck.Result) { rounds <- r },
		headcheck.WithPeerVotes(headcheck.PeerVotesFrom(b, s, time.Minute)))
	if mon == nil {
		t.Fatal("bridge-off with peer votes must start a monitor")
	}
	t.Cleanup(mon.Stop)
	return mon, rounds
}

func firstRound(t *testing.T, rounds chan headcheck.Result) headcheck.Result {
	t.Helper()
	select {
	case r := <-rounds:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no round")
	}
	return headcheck.Result{}
}

func TestBridgeOff_PeerGroupsAgree(t *testing.T) {
	s, cids := chainTo(t, 10)
	b := book(t, map[peer.ID]string{"a": "8.8.4.4", "b": "9.9.9.9"})
	b.ObserveBlock("a", otherCID(t, "a11"), 11, []cid.Cid{cids[10]})
	b.ObserveBlock("b", otherCID(t, "b11"), 11, []cid.Cid{cids[10]})
	_, rounds := startBridgeOff(t, s, b)
	r := firstRound(t, rounds)
	if r.Status != headcheck.StatusAgree || r.PeerVoters != 2 || r.Total != 0 {
		t.Fatalf("two on-chain peer groups, zero RPC => agree; got %s peers=%d rpc=%d", r.Status, r.PeerVoters, r.Total)
	}
}

func TestBridgeOff_ForkedPeerGroupsDiverge(t *testing.T) {
	s, _ := chainTo(t, 10)
	b := book(t, map[peer.ID]string{"a": "8.8.4.4", "b": "9.9.9.9"})
	b.ObserveBlock("a", otherCID(t, "a11"), 11, []cid.Cid{otherCID(t, "real10")})
	b.ObserveBlock("b", otherCID(t, "b11"), 11, []cid.Cid{otherCID(t, "real10")})
	_, rounds := startBridgeOff(t, s, b)
	if r := firstRound(t, rounds); r.Status != headcheck.StatusDiverge || r.ForkedKinds != 2 {
		t.Fatalf("two peer groups on another chain => diverge; got %s forked=%d", r.Status, r.ForkedKinds)
	}
}

func TestBridgeOff_SybilsInOneGroupAreOneVoter(t *testing.T) {
	s, cids := chainTo(t, 10)
	ips := map[peer.ID]string{}
	b := book(t, ips)
	for i := 0; i < 20; i++ {
		p := peer.ID(rune('a' + i))
		ips[p] = "8.8." + string(rune('0'+i%10)) + ".1" // all in 8.8.0.0/16
		b.ObserveBlock(p, otherCID(t, string(p)), 11, []cid.Cid{cids[10]})
	}
	_, rounds := startBridgeOff(t, s, b)
	if r := firstRound(t, rounds); r.Status == headcheck.StatusAgree || r.PeerVoters != 1 {
		t.Fatalf("20 peers in one /16 must be ONE voter (no quorum); got %s peers=%d", r.Status, r.PeerVoters)
	}
}

func TestBridgeOff_BehindPeersIgnored(t *testing.T) {
	s, cids := chainTo(t, 10)
	b := book(t, map[peer.ID]string{"a": "8.8.4.4", "b": "9.9.9.9"})
	// Stale hello heads far below us (on our chain) are not evidence.
	b.Observe("a", []cid.Cid{cids[2]}, 2)
	b.Observe("b", []cid.Cid{cids[3]}, 3)
	_, rounds := startBridgeOff(t, s, b)
	if r := firstRound(t, rounds); r.Status == headcheck.StatusDiverge || r.PeerVoters != 0 {
		t.Fatalf("stale behind peers must be skipped, got %s peers=%d", r.Status, r.PeerVoters)
	}
}

func TestStartGated_NoSourcesNoPeersIsNil(t *testing.T) {
	s, _ := chainTo(t, 3)
	if headcheck.StartGated(context.Background(), blockingest.New(s, nil), s, nil, nil) != nil {
		t.Fatal("no sources and no peer votes => nil monitor")
	}
}
