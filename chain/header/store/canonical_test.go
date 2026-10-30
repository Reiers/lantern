package store_test

import (
	"context"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// SetCanonicalSegment writes pointers for a linked run below maxHeight,
// deletes stale pointers at null epochs, and rejects unlinked runs.
func TestSetCanonicalSegment(t *testing.T) {
	s, _ := newStore(t, false)
	var chain []*ltypes.TipSet // oldest first while building
	var parents []cid.Cid
	for h := abi.ChainEpoch(0); h <= 10; h++ {
		if h == 5 {
			continue // null round
		}
		b := mkBlock(t, h, parents, 1000, "")
		ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		require.NoError(t, err)
		require.NoError(t, s.PutTipSet(ts))
		chain = append(chain, ts)
		parents = ts.Cids()
	}
	// A stale pointer at the null epoch 5 (e.g. from an old fork).
	stale := mkBlock(t, 5, chain[4].Cids(), 1000, "stale")
	sts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{stale})
	require.NoError(t, err)
	require.NoError(t, s.SetHead(context.Background(), sts))
	got, err := s.Tipset(5)
	require.NoError(t, err)
	require.Equal(t, sts.Key(), got.Key())

	newestFirst := make([]*ltypes.TipSet, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, chain[i])
	}
	n, err := s.SetCanonicalSegment(newestFirst, 8)
	require.NoError(t, err)
	require.Positive(t, n)
	for _, ts := range chain {
		got, err := s.Tipset(ts.Height())
		if ts.Height() <= 8 {
			require.NoError(t, err)
			require.Equal(t, ts.Key(), got.Key())
		} else {
			require.Error(t, err) // above maxHeight: untouched
		}
	}
	_, err = s.Tipset(5)
	require.Error(t, err, "stale pointer at null epoch must be deleted")
	// Head untouched.
	require.Equal(t, sts.Key(), s.Head().Key())

	// Idempotent.
	n, err = s.SetCanonicalSegment(newestFirst, 8)
	require.NoError(t, err)
	require.Zero(t, n)

	// Unlinked run rejected.
	_, err = s.SetCanonicalSegment([]*ltypes.TipSet{chain[5], chain[2]}, 100)
	require.Error(t, err)

	key, par, ok, err := s.CanonicalLink(4)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, chain[4].Key(), key)
	require.Equal(t, chain[3].Key(), par)
}
