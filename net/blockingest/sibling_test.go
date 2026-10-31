package blockingest

import (
	"context"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// siblingOf returns a block of the same tipset as b: identical height and
// parent-derived fields, different miner and ticket.
func siblingOf(t *testing.T, b *ltypes.BlockHeader, miner uint64, tag string) *ltypes.BlockHeader {
	t.Helper()
	s := *b
	s.Miner = mkAddr(t, miner)
	s.Ticket = &ltypes.Ticket{VRFProof: []byte("t-" + tag)}
	s.ElectionProof = &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)}
	s.Messages = mkCID(t, "msgs-"+tag)
	return &s
}

// Regression for the ~1 epoch head lag (Curio "slow (55s behind)"): a
// second block of the head tipset used to be dropped at the height fence,
// so every block of the next epoch (whose Parents list both) was skipped.
func TestIngestor_SiblingCompletesHeadAndNextEpochAdoptsImmediately(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil) // no backfill source: the next epoch must not need one
	ctx := context.Background()

	b1 := mkBlock(t, 11, head.Cids(), 1000, "h11")
	ing.process(ctx, &ltypes.BlockMsg{Header: b1})
	require.Equal(t, uint64(1), ing.installed.Load())

	b2 := siblingOf(t, b1, 2000, "h11-sib")
	ing.process(ctx, &ltypes.BlockMsg{Header: b2})
	require.Equal(t, uint64(1), ing.siblingsStored.Load())
	require.Equal(t, uint64(1), ing.siblingsMerged.Load())
	cur := s.Head()
	require.Len(t, cur.Blocks(), 2, "head tipset grows to include the sibling (Lotus semantics)")
	require.True(t, cur.Contains(b1.Cid()) && cur.Contains(b2.Cid()))

	// Next epoch: parents are the full two-block tipset.
	c1 := mkBlock(t, 12, cur.Cids(), 1000, "h12")
	ing.process(ctx, &ltypes.BlockMsg{Header: c1})
	require.Equal(t, uint64(0), ing.skipped.Load(), "next epoch must not be skipped")
	require.Equal(t, uint64(2), ing.installed.Load())
	require.EqualValues(t, 12, s.HeadEpoch())
	parent, err := s.GetTipSet(s.Head().Parents())
	require.NoError(t, err)
	require.Len(t, parent.Blocks(), 2, "canonical parent is the full tipset")
}

func TestIngestor_CompetingBlockAtHeadHeightIgnored(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)
	ctx := context.Background()

	b1 := mkBlock(t, 11, head.Cids(), 1000, "h11")
	ing.process(ctx, &ltypes.BlockMsg{Header: b1})

	// Same height, different parent state: another fork, not a sibling.
	other := siblingOf(t, b1, 3000, "fork")
	other.ParentStateRoot = mkCID(t, "other-state")
	ing.process(ctx, &ltypes.BlockMsg{Header: other})
	require.Equal(t, uint64(0), ing.siblingsStored.Load())
	require.Len(t, s.Head().Blocks(), 1)
	_, err := s.Get(other.Cid())
	require.Error(t, err, "a non-sibling must not be persisted")

	// Different ParentWeight: also not a sibling.
	heavier := siblingOf(t, b1, 3001, "fork2")
	heavier.ParentWeight = ltypes.NewInt(999)
	ing.process(ctx, &ltypes.BlockMsg{Header: heavier})
	require.Equal(t, uint64(0), ing.siblingsStored.Load())
}

func TestIngestor_SiblingStoredButHeadHeldWhenGateClosed(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)
	ctx := context.Background()
	b1 := mkBlock(t, 11, head.Cids(), 1000, "h11")
	ing.process(ctx, &ltypes.BlockMsg{Header: b1})

	ing.SetHeadAdoptionGate(func() bool { return false })
	b2 := siblingOf(t, b1, 2000, "h11-sib")
	ing.process(ctx, &ltypes.BlockMsg{Header: b2})
	require.Equal(t, uint64(1), ing.siblingsStored.Load())
	require.Equal(t, uint64(0), ing.siblingsMerged.Load())
	require.Len(t, s.Head().Blocks(), 1, "closed divergence gate: head does not grow")

	ing.SetHeadCorroboration(func(cid.Cid) bool { return false })
	ing.SetHeadAdoptionGate(nil)
	b3 := siblingOf(t, b1, 2001, "h11-sib2")
	ing.process(ctx, &ltypes.BlockMsg{Header: b3})
	require.Equal(t, uint64(2), ing.siblingsStored.Load())
	require.Len(t, s.Head().Blocks(), 1, "uncorroborated sibling: head does not grow")
}

// A head+1 block whose parent tipset includes a sibling we never received
// (late or missed gossip) is fetched directly instead of being skipped.
func TestIngestor_FetchesMissingSiblingParent(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()
	ing := New(s, src)
	ctx := context.Background()

	b1 := mkBlock(t, 11, head.Cids(), 1000, "h11")
	ing.process(ctx, &ltypes.BlockMsg{Header: b1})
	b2 := siblingOf(t, b1, 2000, "h11-missed")
	src.register(b2) // reachable over the network, never gossiped to us

	full, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b1, b2})
	require.NoError(t, err)
	c1 := mkBlock(t, 12, full.Cids(), 1000, "h12")
	ing.process(ctx, &ltypes.BlockMsg{Header: c1})
	require.Equal(t, uint64(1), ing.parentsFetched.Load())
	require.EqualValues(t, 12, s.HeadEpoch(), "head adopts the block in the same epoch")
}

func TestIngestor_MissingParentFetchRejectsWrongTipset(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()
	ing := New(s, src)
	ctx := context.Background()

	b1 := mkBlock(t, 11, head.Cids(), 1000, "h11")
	ing.process(ctx, &ltypes.BlockMsg{Header: b1})
	// The missing parent claims a different parent state than b1: the
	// two cannot form one tipset, so nothing is adopted.
	bad := siblingOf(t, b1, 2000, "h11-bad")
	bad.ParentStateRoot = mkCID(t, "forged")
	src.register(bad)
	full, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b1, bad})
	require.NoError(t, err)
	c1 := mkBlock(t, 12, full.Cids(), 1000, "h12")
	ing.process(ctx, &ltypes.BlockMsg{Header: c1})
	require.Equal(t, uint64(0), ing.parentsFetched.Load())
	require.EqualValues(t, 11, s.HeadEpoch())
	require.Equal(t, uint64(1), ing.backfillFailed.Load())
}
