package blockingest

import (
	"context"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// #156: a candidate heavier than our head (passes #79) but NOT heavier than
// its own parent is malformed/forged and must be rejected.
func TestWeightGuard_NonMonotonicChildRejected(t *testing.T) {
	s, head := withStore(t, 10) // head ParentWeight = 10
	p := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "side11")
	p.ParentWeight = ltypes.NewInt(100)
	require.NoError(t, s.Put(p))

	c := mkBlock(t, 12, []cid.Cid{p.Cid()}, 1000, "side12")
	c.ParentWeight = ltypes.NewInt(50) // > head (10), < parent (100)

	ing := New(s, nil)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: c})

	st := ing.Stats()
	require.Equal(t, uint64(1), st.RejectedNonMonotonic)
	require.Equal(t, uint64(0), st.RejectedLighter, "must be caught by the weight guard, not #79")
	require.Equal(t, head.Height(), s.HeadEpoch())
}

// Equal weight to the parent is also non-monotonic.
func TestWeightGuard_EqualWeightRejected(t *testing.T) {
	s, head := withStore(t, 10)
	p := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "eq11")
	p.ParentWeight = ltypes.NewInt(40)
	require.NoError(t, s.Put(p))
	c := mkBlock(t, 12, []cid.Cid{p.Cid()}, 1000, "eq12")
	c.ParentWeight = ltypes.NewInt(40)

	ing := New(s, nil)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: c})
	require.Equal(t, uint64(1), ing.Stats().RejectedNonMonotonic)
	require.Equal(t, head.Height(), s.HeadEpoch())
}

// An honest child (weight > parent > head) is adopted.
func TestWeightGuard_HonestChildAdopted(t *testing.T) {
	s, head := withStore(t, 10)
	c := mkBlock(t, 11, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "ok11")
	ing := New(s, nil)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: c})
	require.Equal(t, uint64(0), ing.Stats().RejectedNonMonotonic)
	require.Equal(t, int64(11), int64(s.HeadEpoch()))
}
