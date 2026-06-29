package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkAddr(t *testing.T, id uint64) address.Address {
	t.Helper()
	a, err := address.NewIDAddress(id)
	require.NoError(t, err)
	return a
}

// mkBlock builds a deterministic test block. Tag distinguishes competing
// blocks at the same height (=> different CIDs).
func mkBlock(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, miner uint64, tag string) *ltypes.BlockHeader {
	return &ltypes.BlockHeader{
		Miner:                 mkAddr(t, miner),
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		BeaconEntries:         nil,
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkCID(t, "state-"+tag),
		ParentMessageReceipts: mkCID(t, "receipts-"+tag),
		Messages:              mkCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

func newStore(t *testing.T, persistent bool) (*store.Store, string) {
	t.Helper()
	path := ""
	if persistent {
		path = filepath.Join(t.TempDir(), "hdrs")
	}
	s, err := store.Open(path, store.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// TestPutGetRoundtrip checks that we can Put a header and Get it back.
func TestPutGetRoundtrip(t *testing.T) {
	s, _ := newStore(t, false)
	bh := mkBlock(t, 0, nil, 1000, "genesis")
	require.NoError(t, s.Put(bh))
	got, err := s.Get(bh.Cid())
	require.NoError(t, err)
	require.Equal(t, bh.Cid(), got.Cid())
	require.Equal(t, bh.Height, got.Height)
}

// TestSetHeadAndTipsetByHeight verifies that a linear chain → SetHead →
// GetTipSetByHeight returns the canonical block at each epoch.
func TestSetHeadAndTipsetByHeight(t *testing.T) {
	s, _ := newStore(t, false)

	// Genesis tipset (height 0, no parents).
	g := mkBlock(t, 0, nil, 1000, "g")
	require.NoError(t, s.Put(g))
	gts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{g})
	require.NoError(t, err)
	require.NoError(t, s.SetHead(context.Background(), gts))

	// Build a linear chain of 5 epochs.
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 5; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		require.NoError(t, s.Put(b))
		ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		require.NoError(t, err)
		require.NoError(t, s.SetHead(context.Background(), ts))
		parents = []cid.Cid{b.Cid()}
	}

	require.Equal(t, abi.ChainEpoch(5), s.HeadEpoch())

	// Walk back via GetTipSetByHeight.
	for h := abi.ChainEpoch(0); h <= 5; h++ {
		ts, err := s.GetTipSetByHeight(h)
		require.NoError(t, err)
		require.Equal(t, h, ts.Height(), "epoch %d", h)
	}
}

// TestPersistenceRoundTrip checks that closing + reopening the store
// recovers the head and lets us look up the prior canonical chain.
func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hdrs")
	s, err := store.Open(path, store.Options{})
	require.NoError(t, err)

	// Genesis + 3 epochs.
	g := mkBlock(t, 0, nil, 1000, "g")
	require.NoError(t, s.Put(g))
	gts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{g})
	require.NoError(t, s.SetHead(context.Background(), gts))

	parents := []cid.Cid{g.Cid()}
	var heads []*ltypes.BlockHeader
	for h := abi.ChainEpoch(1); h <= 3; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		require.NoError(t, s.Put(b))
		ts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		require.NoError(t, s.SetHead(context.Background(), ts))
		parents = []cid.Cid{b.Cid()}
		heads = append(heads, b)
	}
	require.NoError(t, s.Close())

	// Reopen.
	s2, err := store.Open(path, store.Options{})
	require.NoError(t, err)
	defer s2.Close()

	require.Equal(t, abi.ChainEpoch(3), s2.HeadEpoch())
	ts3, err := s2.GetTipSetByHeight(3)
	require.NoError(t, err)
	require.Equal(t, heads[2].Cid(), ts3.Cids()[0])
}

// TestReorgRewritesCanonical builds chain A, sets it as head, then ingests
// a heavier chain B that diverges 2 epochs back, and verifies the canonical
// pointers in the divergence range are rewritten to B.
func TestReorgRewritesCanonical(t *testing.T) {
	s, _ := newStore(t, false)
	ctx := context.Background()

	// Common ancestor at epoch 0.
	g := mkBlock(t, 0, nil, 1000, "g")
	require.NoError(t, s.Put(g))
	gts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{g})
	require.NoError(t, s.SetHead(ctx, gts))

	// Chain A: 0 → A1 → A2.
	a1 := mkBlock(t, 1, []cid.Cid{g.Cid()}, 1000, "A1")
	a2 := mkBlock(t, 2, []cid.Cid{a1.Cid()}, 1000, "A2")
	require.NoError(t, s.Put(a1))
	require.NoError(t, s.Put(a2))
	a2ts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{a2})
	require.NoError(t, s.SetHead(ctx, a2ts))

	tsA1, err := s.GetTipSetByHeight(1)
	require.NoError(t, err)
	require.Equal(t, a1.Cid(), tsA1.Cids()[0])
	tsA2, err := s.GetTipSetByHeight(2)
	require.NoError(t, err)
	require.Equal(t, a2.Cid(), tsA2.Cids()[0])

	// Chain B: 0 → B1 → B2 → B3 (different blocks, different CIDs).
	b1 := mkBlock(t, 1, []cid.Cid{g.Cid()}, 1001, "B1")
	b2 := mkBlock(t, 2, []cid.Cid{b1.Cid()}, 1001, "B2")
	b3 := mkBlock(t, 3, []cid.Cid{b2.Cid()}, 1001, "B3")
	require.NoError(t, s.Put(b1))
	require.NoError(t, s.Put(b2))
	require.NoError(t, s.Put(b3))
	b3ts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{b3})
	require.NoError(t, s.SetHead(ctx, b3ts))

	require.Equal(t, abi.ChainEpoch(3), s.HeadEpoch())

	// Canonical at 1 and 2 should now be B*.
	ts1, err := s.GetTipSetByHeight(1)
	require.NoError(t, err)
	require.Equal(t, b1.Cid(), ts1.Cids()[0], "epoch 1 canonical should be B1 after reorg")

	ts2, err := s.GetTipSetByHeight(2)
	require.NoError(t, err)
	require.Equal(t, b2.Cid(), ts2.Cids()[0], "epoch 2 canonical should be B2 after reorg")

	// AllHeadersAtEpoch should still surface both forks (we don't garbage
	// collect orphaned headers).
	all1, err := s.AllHeadersAtEpoch(1)
	require.NoError(t, err)
	require.Len(t, all1, 2, "both A1 and B1 should remain stored at epoch 1")
}

// TestOnHeadChange fires the callback on SetHead.
func TestOnHeadChange(t *testing.T) {
	s, _ := newStore(t, false)
	fired := make(chan *ltypes.TipSet, 1)
	s.OnHeadChange(func(ts *ltypes.TipSet) { fired <- ts })

	g := mkBlock(t, 0, nil, 1000, "g")
	require.NoError(t, s.Put(g))
	gts, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{g})
	require.NoError(t, s.SetHead(context.Background(), gts))

	select {
	case ts := <-fired:
		require.Equal(t, abi.ChainEpoch(0), ts.Height())
	default:
		t.Fatal("OnHeadChange callback did not fire")
	}
}

// TestGetTipSetByKey verifies GetTipSet(key) resolves a tipset directly
// from persisted headers, including a NON-head historical tipset (the
// Curio chain-watcher scenario from #68), and returns ErrNotFound when a
// constituent block is missing or the key is empty.
func TestGetTipSetByKey(t *testing.T) {
	s, _ := newStore(t, false)

	// Build a short linear chain: genesis -> h1 -> h2 (h2 is head).
	g := mkBlock(t, 0, nil, 1000, "g")
	require.NoError(t, s.Put(g))
	gts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{g})
	require.NoError(t, err)
	require.NoError(t, s.SetHead(context.Background(), gts))

	b1 := mkBlock(t, 1, []cid.Cid{g.Cid()}, 1000, "1")
	require.NoError(t, s.Put(b1))
	ts1, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b1})
	require.NoError(t, err)
	require.NoError(t, s.SetHead(context.Background(), ts1))

	b2 := mkBlock(t, 2, []cid.Cid{b1.Cid()}, 1000, "2")
	require.NoError(t, s.Put(b2))
	ts2, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b2})
	require.NoError(t, err)
	require.NoError(t, s.SetHead(context.Background(), ts2))

	// Resolve a NON-head historical tipset (epoch 1) directly by key.
	got, err := s.GetTipSet(ts1.Key())
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(1), got.Height())
	require.Equal(t, ts1.Key().String(), got.Key().String())

	// Resolve the head tipset by key too.
	gotHead, err := s.GetTipSet(ts2.Key())
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(2), gotHead.Height())

	// Empty key -> ErrNotFound.
	_, err = s.GetTipSet(ltypes.EmptyTSK)
	require.ErrorIs(t, err, store.ErrNotFound)

	// Key referencing a block we never persisted -> ErrNotFound.
	missing := mkBlock(t, 3, []cid.Cid{b2.Cid()}, 1000, "missing")
	missingTS, err := ltypes.NewTipSet([]*ltypes.BlockHeader{missing})
	require.NoError(t, err)
	_, err = s.GetTipSet(missingTS.Key())
	require.True(t, errors.Is(err, store.ErrNotFound), "missing block should be ErrNotFound, got %v", err)
}
