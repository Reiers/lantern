package ecfinality

import (
	"fmt"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

func tCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func tHeader(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, tag string) *ltypes.BlockHeader {
	t.Helper()
	miner, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	return &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       tCID(t, "state-"+tag),
		ParentMessageReceipts: tCID(t, "receipts-"+tag),
		Messages:              tCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

// fakeSource is an in-memory HeaderSource over a linked chain of tipsets.
type fakeSource struct {
	head *ltypes.TipSet
	byK  map[string]*ltypes.TipSet
}

func (f *fakeSource) Head() *ltypes.TipSet { return f.head }
func (f *fakeSource) GetTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error) {
	ts, ok := f.byK[tsk.String()]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ts, nil
}

// buildChain builds a linked single-block-per-epoch chain of n epochs
// starting at startEpoch, optionally skipping heights in skip (null
// rounds). Blocks-per-epoch is 1 for simplicity; blockCount overrides via
// widthAt (height -> number of blocks in the tipset).
func buildChain(t *testing.T, startEpoch abi.ChainEpoch, n int, widthAt func(abi.ChainEpoch) int, skip map[abi.ChainEpoch]bool) *fakeSource {
	t.Helper()
	src := &fakeSource{byK: make(map[string]*ltypes.TipSet)}
	var prev []cid.Cid
	for i := 0; i < n; i++ {
		h := startEpoch + abi.ChainEpoch(i)
		if skip != nil && skip[h] {
			continue // null round: no tipset at this height
		}
		width := 1
		if widthAt != nil {
			width = widthAt(h)
		}
		var blks []*ltypes.BlockHeader
		for w := 0; w < width; w++ {
			blks = append(blks, tHeader(t, h, prev, fmt.Sprintf("h%d-w%d", h, w)))
		}
		ts, err := ltypes.NewTipSet(blks)
		require.NoError(t, err)
		src.byK[ts.Key().String()] = ts
		src.head = ts
		prev = ts.Cids()
	}
	return src
}

// TestCache_HealthyWindowFindsThreshold: a healthy 5-wide chain with 120
// epochs of history reports a threshold around the ~30-epoch mark.
func TestCache_HealthyWindowFindsThreshold(t *testing.T) {
	src := buildChain(t, 100, 120, func(abi.ChainEpoch) int { return 5 }, nil)
	c := NewCache(src, 900)

	st, err := c.Status()
	require.NoError(t, err)
	require.Equal(t, 120, st.WindowEpochs, "walk must cover the full available window")
	require.Greater(t, st.ThresholdDepth, 0, "healthy chain must meet the guarantee")
	require.Less(t, st.ThresholdDepth, 35, "healthy chain finalizes shallow")
	require.Equal(t, st.HeadEpoch-abi.ChainEpoch(st.ThresholdDepth), st.FinalizedEpoch)
}

// TestCache_ShallowWindowNotComputable: a node with only a few epochs of
// observed history (fresh boot from anchor) must report -1, not an
// over-confident threshold.
func TestCache_ShallowWindowNotComputable(t *testing.T) {
	src := buildChain(t, 100, MinWindow-5, func(abi.ChainEpoch) int { return 5 }, nil)
	c := NewCache(src, 900)

	st, err := c.Status()
	require.NoError(t, err)
	require.Equal(t, MinWindow-5, st.WindowEpochs)
	require.Equal(t, -1, st.ThresholdDepth, "shallow window must not report a threshold")
	require.Equal(t, abi.ChainEpoch(-1), st.FinalizedEpoch)
}

// TestCache_NullRoundsCounted: a gap in heights (null rounds) appears as
// 0-entries in the walked window, so WindowEpochs counts epochs, not
// tipsets.
func TestCache_NullRoundsCounted(t *testing.T) {
	skip := map[abi.ChainEpoch]bool{150: true, 151: true}
	src := buildChain(t, 100, 100, func(abi.ChainEpoch) int { return 5 }, skip)
	c := NewCache(src, 900)

	st, err := c.Status()
	require.NoError(t, err)
	require.Equal(t, 100, st.WindowEpochs, "null rounds count as epochs (0 blocks)")
	require.Greater(t, st.ThresholdDepth, 0)
}

// TestCache_RecomputesOnlyOnHeadChange: repeated Status calls on the same
// head are cache hits; a head advance triggers exactly one recompute.
func TestCache_RecomputesOnlyOnHeadChange(t *testing.T) {
	src := buildChain(t, 100, 120, func(abi.ChainEpoch) int { return 5 }, nil)
	c := NewCache(src, 900)

	_, err := c.Status()
	require.NoError(t, err)
	_, err = c.Status()
	require.NoError(t, err)
	calls, comps := c.Stats()
	require.Equal(t, uint64(2), calls)
	require.Equal(t, uint64(1), comps, "same head must be a cache hit")

	// Advance head by one epoch.
	newHead := tHeader(t, src.head.Height()+1, src.head.Cids(), "advance")
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{newHead})
	require.NoError(t, err)
	src.byK[ts.Key().String()] = ts
	src.head = ts

	_, err = c.Status()
	require.NoError(t, err)
	_, comps = c.Stats()
	require.Equal(t, uint64(2), comps, "head advance must recompute once")
}

// TestCache_NoHead: an empty source errors cleanly.
func TestCache_NoHead(t *testing.T) {
	c := NewCache(&fakeSource{byK: map[string]*ltypes.TipSet{}}, 900)
	_, err := c.Status()
	require.Error(t, err)
}
