// Regression tests for #155: backfill paths must not move the store head
// (or canonical pointers) before the gossip tip passes fork choice, the
// divergence gate and corroboration. Backfill persists ancestors only; the
// single guarded SetHead on the adopted tip rewires them.

package blockingest

import (
	"context"
	"fmt"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// requireCanonicalAt asserts the canonical tipset at or below epoch h is
// the one at `want` (i.e. nothing above `want` has been made canonical).
func requireCanonicalAt(t *testing.T, s *hstore.Store, h, want abi.ChainEpoch) {
	t.Helper()
	ts, err := s.GetTipSetByHeight(h)
	require.NoError(t, err)
	require.Equal(t, want, ts.Height(), "canonical tipset at/below %d", h)
}

// Inline (RPC) backfill of a lighter branch: ancestors are fetched and
// persisted, but the tip loses fork choice, so head and canonical
// pointers must be exactly where they were.
func TestBackfillGuard_InlineLighterBranchDoesNotMoveHead(t *testing.T) {
	s, head := withStore(t, 10) // head weight 10
	src := newFakeBackfillSource()

	a11 := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 2000, "atk11")
	a12 := mkBlock(t, 12, []cid.Cid{a11.Cid()}, 2000, "atk12")
	tip := mkBlock(t, 13, []cid.Cid{a12.Cid()}, 2000, "atk13")
	tip.ParentWeight = ltypes.NewInt(5) // lighter than head
	src.register(a11)
	src.register(a12)

	ing := New(s, src)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: tip})

	require.Equal(t, uint64(1), ing.backfilled.Load(), "backfill ran")
	require.Equal(t, uint64(1), ing.Stats().RejectedLighter, "tip rejected by fork choice")
	require.Equal(t, uint64(0), ing.installed.Load())
	require.Equal(t, head.Height(), s.HeadEpoch(), "#155: backfill must not move head")
	requireCanonicalAt(t, s, 12, head.Height())

	// Ancestors are still persisted (state stays warm).
	_, err := s.Get(a12.Cid())
	require.NoError(t, err)
}

// Divergence gate closed: an honest heavier gap is backfilled but head is
// held, including for the backfilled ancestors.
func TestBackfillGuard_GateClosedHoldsBackfilledAncestors(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()

	e11 := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "e11")
	e12 := mkBlock(t, 12, []cid.Cid{e11.Cid()}, 1000, "e12")
	e13 := mkBlock(t, 13, []cid.Cid{e12.Cid()}, 1000, "e13")
	src.register(e11)
	src.register(e12)

	ing := New(s, src)
	ing.SetHeadAdoptionGate(func() bool { return false })
	ing.process(context.Background(), &ltypes.BlockMsg{Header: e13})

	require.Equal(t, uint64(1), ing.Stats().HeldDiverged)
	require.Equal(t, head.Height(), s.HeadEpoch(), "#155: gate closed, head must not move via backfill")
	requireCanonicalAt(t, s, 12, head.Height())
}

// Honest heavier gap with an open gate: the tip is adopted and its
// SetHead rewires the backfilled ancestors to canonical.
func TestBackfillGuard_AdoptedTipRewiresAncestors(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()

	e11 := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "e11")
	e12 := mkBlock(t, 12, []cid.Cid{e11.Cid()}, 1000, "e12")
	e13 := mkBlock(t, 13, []cid.Cid{e12.Cid()}, 1000, "e13")
	src.register(e11)
	src.register(e12)

	ing := New(s, src)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: e13})

	require.Equal(t, abi.ChainEpoch(13), s.HeadEpoch())
	for _, b := range []*ltypes.BlockHeader{e11, e12} {
		ts, err := s.Tipset(b.Height)
		require.NoError(t, err)
		require.Equal(t, b.Cid(), ts.Blocks()[0].Cid(), "ancestor %d canonical after adoption", b.Height)
	}
}

// Bridge-off ChainExchange backfill of a lighter branch.
func TestBackfillGuard_ChainExchangeLighterBranchDoesNotMoveHead(t *testing.T) {
	s, head := withStore(t, 10)

	parents := []cid.Cid{head.Blocks()[0].Cid()}
	var oldestFirst []*ltypes.BlockHeader
	for h := abi.ChainEpoch(11); h <= 19; h++ {
		b := mkBlock(t, h, parents, 2000, fmt.Sprintf("cx-%d", h))
		oldestFirst = append(oldestFirst, b)
		parents = []cid.Cid{b.Cid()}
	}
	tip := mkBlock(t, 20, parents, 2000, "cx-20")
	tip.ParentWeight = ltypes.NewInt(3)

	fetcher := newFakeChainFetcher()
	var newestFirst [][]*ltypes.BlockHeader
	for i := len(oldestFirst) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, []*ltypes.BlockHeader{oldestFirst[i]})
	}
	fetcher.byHead[tip.Parents[0]] = newestFirst

	// process() skips missing-parent blocks when src == nil, so a (unused)
	// per-CID source is required to reach the chainxchg path, same as
	// TestIngestor_ParentWalkAcceptsGapAboveRPCCap.
	ing := New(s, newFakeBackfillSource())
	ing.SetParentWalkBackfill(true)
	ing.SetChainFetcher(fetcher)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: tip})

	require.Equal(t, uint64(1), ing.backfilled.Load())
	require.Equal(t, uint64(1), ing.Stats().RejectedLighter)
	require.Equal(t, head.Height(), s.HeadEpoch(), "#155: chainxchg backfill must not move head")
	requireCanonicalAt(t, s, 19, head.Height())
}

// Bridge-off per-CID (bitswap) parent-walk backfill of a lighter branch.
func TestBackfillGuard_ParentWalkLighterBranchDoesNotMoveHead(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()

	a11 := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 2000, "pw11")
	a12 := mkBlock(t, 12, []cid.Cid{a11.Cid()}, 2000, "pw12")
	tip := mkBlock(t, 13, []cid.Cid{a12.Cid()}, 2000, "pw13")
	tip.ParentWeight = ltypes.NewInt(4)
	src.register(a11)
	src.register(a12)

	ing := New(s, src)
	ing.SetParentWalkBackfill(true) // no chain fetcher: per-CID walk
	ing.process(context.Background(), &ltypes.BlockMsg{Header: tip})

	require.Equal(t, uint64(1), ing.backfilled.Load())
	require.Equal(t, uint64(1), ing.Stats().RejectedLighter)
	require.Equal(t, head.Height(), s.HeadEpoch(), "#155: parent-walk backfill must not move head")
	requireCanonicalAt(t, s, 12, head.Height())
}
