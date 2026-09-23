package headcheck_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/headcheck"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/blockingest"
)

type fixedSrc struct {
	name string
	kind bootstrap.Kind
	ep   abi.ChainEpoch
}

func (f fixedSrc) Name() string                                      { return f.name }
func (f fixedSrc) Kind() bootstrap.Kind                              { return f.kind }
func (f fixedSrc) HeadEpoch(context.Context) (abi.ChainEpoch, error) { return f.ep, nil }

func blk(t *testing.T, h abi.ChainEpoch, parents []cid.Cid) *ltypes.BlockHeader {
	t.Helper()
	sum := func(s string) cid.Cid {
		m, _ := mh.Sum([]byte(s), mh.SHA2_256, -1)
		return cid.NewCidV1(cid.DagCBOR, m)
	}
	a, _ := address.NewIDAddress(1000)
	tag := string(rune('a' + int(h)))
	return &ltypes.BlockHeader{
		Miner: a, Ticket: &ltypes.Ticket{VRFProof: []byte("t" + tag)},
		ElectionProof: &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e" + tag)},
		Parents:       parents, ParentWeight: ltypes.NewInt(uint64(h)), Height: h,
		ParentStateRoot: sum("s" + tag), ParentMessageReceipts: sum("r" + tag), Messages: sum("m" + tag),
		Timestamp: 1_700_000_000 + uint64(h)*30, ParentBaseFee: ltypes.NewInt(100),
	}
}

// Fresh daemon: store head seeded at the anchor (10), nothing installed by
// gossip yet, two independent voters at the anchor. The next honest gossip
// block (11) MUST be adopted. Pre-fix, Local=ing.ObservedHead()=-1 made
// every round DIVERGE, closing the gate forever (deadlock).
func TestGate_FreshNodeAdoptsFirstGossipHead(t *testing.T) {
	s, err := hstore.Open(filepath.Join(t.TempDir(), "hs"), hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var parents []cid.Cid
	var last *ltypes.BlockHeader
	for h := abi.ChainEpoch(0); h <= 10; h++ {
		b := blk(t, h, parents)
		ts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		if err := s.SetHead(ctx, ts); err != nil {
			t.Fatal(err)
		}
		parents, last = []cid.Cid{b.Cid()}, b
	}
	ing := blockingest.New(s, nil)
	go ing.Run(ctx)

	rounds := make(chan headcheck.Result, 4)
	mon := headcheck.StartGated(ctx, ing, s, []headcheck.HeadSource{
		fixedSrc{"a", bootstrap.KindForest, 10},
		fixedSrc{"b", bootstrap.KindUser, 11},
	}, func(r headcheck.Result) { rounds <- r })
	defer mon.Stop()
	select {
	case <-rounds:
	case <-time.After(5 * time.Second):
		t.Fatal("no round")
	}

	ing.Enqueue(&ltypes.BlockMsg{Header: blk(t, 11, []cid.Cid{last.Cid()})})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s.HeadEpoch() != 11 {
		time.Sleep(20 * time.Millisecond)
	}
	if s.HeadEpoch() != 11 {
		t.Fatalf("honest head must be adopted; head=%d last=%s held=%d", s.HeadEpoch(), mon.Last().Status, ing.Stats().HeldDiverged)
	}
}
