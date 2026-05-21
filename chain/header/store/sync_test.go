package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// fakeSource is an in-memory RPCSource for sync tests. It models a single
// canonical chain at any moment, and supports atomic "reorg replacement"
// by overwriting the canonical map at a given epoch range.
type fakeSource struct {
	mu     sync.Mutex
	canon  map[abi.ChainEpoch]*ltypes.BlockHeader // epoch → canonical block
	blocks map[cid.Cid]*ltypes.BlockHeader        // CID → header (all forks)
	head   abi.ChainEpoch
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		canon:  map[abi.ChainEpoch]*ltypes.BlockHeader{},
		blocks: map[cid.Cid]*ltypes.BlockHeader{},
	}
}

func (f *fakeSource) put(b *ltypes.BlockHeader) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canon[b.Height] = b
	f.blocks[b.Cid()] = b
	if b.Height > f.head {
		f.head = b.Height
	}
}

func (f *fakeSource) replaceCanonAt(b *ltypes.BlockHeader) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canon[b.Height] = b
	f.blocks[b.Cid()] = b
}

func (f *fakeSource) HeadEpoch(_ context.Context) (abi.ChainEpoch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *fakeSource) TipsetCIDsByHeight(_ context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.canon[h]
	if !ok {
		return nil, errors.New("no tipset at height")
	}
	return []cid.Cid{b.Cid()}, nil
}

func (f *fakeSource) FetchBlock(_ context.Context, k cid.Cid) (*ltypes.BlockHeader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blocks[k]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

func TestSyncLinearAdvance(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	// Seed: epoch 0..4 in canonical chain.
	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 4; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	sync := hstore.NewSync(s, src, hstore.SyncOptions{MaxBacktrack: 10})
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(4), s.HeadEpoch())

	for h := abi.ChainEpoch(0); h <= 4; h++ {
		ts, err := s.GetTipSetByHeight(h)
		require.NoError(t, err)
		require.Equal(t, h, ts.Height())
	}

	// Advance the source by 2 epochs.
	parents = []cid.Cid{src.canon[4].Cid()}
	for h := abi.ChainEpoch(5); h <= 6; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(6), s.HeadEpoch())
}

func TestSyncReorgDetected(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	// Initial chain: epoch 0..5 (tag "A").
	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	var aChain []*ltypes.BlockHeader
	aChain = append(aChain, g)
	for h := abi.ChainEpoch(1); h <= 5; h++ {
		b := mkBlock(t, h, parents, 1000, "A")
		src.put(b)
		aChain = append(aChain, b)
		parents = []cid.Cid{b.Cid()}
	}

	sync := hstore.NewSync(s, src, hstore.SyncOptions{
		MaxBacktrack: 30,
	})
	var reorgFired int
	var reorgEp abi.ChainEpoch = -1
	sync = hstore.NewSync(s, src, hstore.SyncOptions{
		MaxBacktrack: 30,
		OnReorg: func(d abi.ChainEpoch) {
			reorgFired++
			reorgEp = d
		},
	})

	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(5), s.HeadEpoch())

	// Now reorg: replace epochs 3..6 with a new fork ("B"), making head 6.
	parents = []cid.Cid{src.canon[2].Cid()}
	for h := abi.ChainEpoch(3); h <= 6; h++ {
		b := mkBlock(t, h, parents, 1001, "B")
		src.replaceCanonAt(b)
		parents = []cid.Cid{b.Cid()}
	}
	src.head = 6

	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(6), s.HeadEpoch())
	require.Equal(t, 1, reorgFired, "reorg listener should fire exactly once")
	require.Equal(t, abi.ChainEpoch(3), reorgEp, "divergence should be at epoch 3")

	// Canonical at epoch 3..5 should now be the B fork.
	ts3, err := s.GetTipSetByHeight(3)
	require.NoError(t, err)
	require.Equal(t, src.canon[3].Cid(), ts3.Cids()[0])
}
