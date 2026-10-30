package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
)

func hbfCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

// Fresh node: header store covers [900, 1000] only. Curio's StorageGCMark
// asks ChainGetTipSetByHeight(head - N, head.Key()) for a height below
// that. It must be served (backfilled through the block getter) instead of
// "header/store: not found".
func TestChainGetTipSetByHeight_BackfillsBelowCoverage(t *testing.T) {
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	miner, _ := address.NewIDAddress(1000)
	getter := mapGetter{}
	byH := map[abi.ChainEpoch]*types.TipSet{}
	var parents []cid.Cid
	for h := abi.ChainEpoch(0); h <= 1000; h++ {
		if h == 700 || h == 701 {
			continue // null rounds
		}
		tag := fmt.Sprint(h)
		b := &types.BlockHeader{
			Miner:                 miner,
			Ticket:                &types.Ticket{VRFProof: []byte("t" + tag)},
			ElectionProof:         &types.ElectionProof{WinCount: 1, VRFProof: []byte("e" + tag)},
			Parents:               parents,
			ParentWeight:          types.NewInt(uint64(h)),
			Height:                h,
			ParentStateRoot:       hbfCID(t, "s"+tag),
			ParentMessageReceipts: hbfCID(t, "r"+tag),
			Messages:              hbfCID(t, "m"+tag),
			Timestamp:             uint64(h) * 30,
			ParentBaseFee:         types.NewInt(100),
		}
		raw, err := b.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		getter[b.Cid().String()] = raw
		ts, err := types.NewTipSet([]*types.BlockHeader{b})
		if err != nil {
			t.Fatal(err)
		}
		byH[h] = ts
		parents = ts.Cids()
		if h >= 900 {
			if err := hs.SetHead(context.Background(), ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := &ChainAPI{
		Trusted:     &trustedroot.TrustedRoot{Epoch: 900, StateRoot: hbfCID(t, "anchor-state")},
		HeaderStore: hs,
		BlockGetter: getter,
	}
	ctx := context.Background()
	head, err := c.ChainHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Plain store lookup is the bug.
	if _, err := hs.GetTipSetByHeight(500); err == nil {
		t.Fatal("precondition: store should not cover 500")
	}
	got, err := c.ChainGetTipSetByHeight(ctx, 500, head.Key())
	if err != nil {
		t.Fatalf("ChainGetTipSetByHeight(500, head): %v", err)
	}
	if got.Key() != byH[500].Key() {
		t.Fatalf("wrong tipset at 500")
	}
	// Empty key = head.
	got, err = c.ChainGetTipSetByHeight(ctx, 600, types.EmptyTSK)
	if err != nil || got.Key() != byH[600].Key() {
		t.Fatalf("ChainGetTipSetByHeight(600, empty): %v", err)
	}
	// Null rounds: prev for ByHeight, next for AfterHeight.
	got, err = c.ChainGetTipSetByHeight(ctx, 701, head.Key())
	if err != nil || got.Height() != 699 {
		t.Fatalf("null round ByHeight: %v %v", got, err)
	}
	got, err = c.ChainGetTipSetAfterHeight(ctx, 700, head.Key())
	if err != nil || got.Height() != 702 {
		t.Fatalf("null round AfterHeight: %v %v", got, err)
	}
	// ChainGetTipSet / ChainGetBlock for a key the store never saw.
	ts, err := c.ChainGetTipSet(ctx, byH[100].Key())
	if err != nil || ts.Key() != byH[100].Key() {
		t.Fatalf("ChainGetTipSet(100): %v", err)
	}
	bh, err := c.ChainGetBlock(ctx, byH[50].Cids()[0])
	if err != nil || bh.Height != 50 {
		t.Fatalf("ChainGetBlock(50): %v", err)
	}
	// Above head stays an error.
	if _, err := c.ChainGetTipSetByHeight(ctx, 1001, head.Key()); err == nil {
		t.Fatal("expected error above start tipset")
	}
}
