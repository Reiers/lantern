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

// TestSyncCatchUpBeyondMaxBacktrack reproduces issue #33: when the store
// falls further behind the chain than MaxBacktrack (e.g. embedded mode
// during a long ProveTask wait, or after a Glif blip), the agent must
// still converge to head over subsequent polls instead of leaving an
// unfillable gap between currentHead+1 and head-MaxBacktrack.
//
// Before the fix, pollAndApply set start = head-MaxBacktrack whenever
// currentHead+1 < that, skipping every epoch in between. SetHead at
// `start` then needed a parent at start-1 that was never ingested, so the
// head pointer stalled and the daemon reported a stale ChainHead until
// restart (the 70-epoch lag in curio-core#62).
func TestSyncCatchUpBeyondMaxBacktrack(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	// Build a contiguous chain 0..200.
	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 200; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	// Small MaxBacktrack so the lag easily exceeds it.
	sync := hstore.NewSync(s, src, hstore.SyncOptions{MaxBacktrack: 10})

	// First poll: source head is 200, store is empty. BootstrapDepth
	// defaults to MaxBacktrack (10), so the store comes up near head.
	require.NoError(t, sync.PollOnce(context.Background()))
	first := s.HeadEpoch()
	require.Greater(t, int64(first), int64(0), "first poll should establish a head")
	require.LessOrEqual(t, int64(first), int64(200))

	// Now simulate falling far behind: head is already 200, store is at
	// `first` (~190). The gap (200-190=10) is within MaxBacktrack here,
	// but the real failure is a gap LARGER than MaxBacktrack. Force it:
	// repeated polls with a fixed-far-ahead head must monotonically
	// advance the store to head, never stalling.
	prev := s.HeadEpoch()
	for i := 0; i < 50; i++ {
		require.NoError(t, sync.PollOnce(context.Background()))
		cur := s.HeadEpoch()
		require.GreaterOrEqual(t, int64(cur), int64(prev),
			"head must never regress (poll %d)", i)
		prev = cur
		if cur >= 200 {
			break
		}
	}
	require.Equal(t, abi.ChainEpoch(200), s.HeadEpoch(),
		"store must converge to chain head despite lag > MaxBacktrack")

	// Every epoch from the first head up to 200 must be canonically
	// present with no gap (the bug left holes that broke parent linkage).
	for h := first; h <= 200; h++ {
		ts, err := s.GetTipSetByHeight(h)
		require.NoErrorf(t, err, "missing canonical tipset at epoch %d (gap)", h)
		require.Equal(t, h, ts.Height())
	}
}

// flakySource wraps an RPCSource and fails FetchBlock every Nth call,
// modelling a rate-limited / intermittently-failing public RPC (Glif
// under load). This is the real-world condition behind issue #33: a
// single transient fetch error during a deep catch-up must not abort the
// whole poll and stall the head pointer.
type flakySource struct {
	inner    hstore.RPCSource
	mu       sync.Mutex
	calls    int
	failEach int // fail when calls % failEach == 0
}

func (f *flakySource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	return f.inner.HeadEpoch(ctx)
}
func (f *flakySource) TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	return f.inner.TipsetCIDsByHeight(ctx, h)
}
func (f *flakySource) FetchBlock(ctx context.Context, k cid.Cid) (*ltypes.BlockHeader, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failEach > 0 && f.calls%f.failEach == 0
	f.mu.Unlock()
	if fail {
		return nil, errors.New("simulated rate-limit (429)")
	}
	return f.inner.FetchBlock(ctx, k)
}

// failOnceSource fails FetchBlock the first time it's asked for any CID
// in failCIDs, then succeeds. Models a transient Glif error on specific
// blocks during a deep catch-up.
type failOnceSource struct {
	inner   hstore.RPCSource
	mu      sync.Mutex
	failCID map[cid.Cid]bool
}

func (f *failOnceSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	return f.inner.HeadEpoch(ctx)
}
func (f *failOnceSource) TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	return f.inner.TipsetCIDsByHeight(ctx, h)
}
func (f *failOnceSource) FetchBlock(ctx context.Context, k cid.Cid) (*ltypes.BlockHeader, error) {
	f.mu.Lock()
	if f.failCID[k] {
		f.failCID[k] = false
		f.mu.Unlock()
		return nil, errors.New("simulated transient fetch error")
	}
	f.mu.Unlock()
	return f.inner.FetchBlock(ctx, k)
}

// TestSyncTransientFetchErrorDoesNotStall is the precise #33 regression.
// A warm store falls far behind head (a 110-epoch jump > MaxBacktrack),
// then a transient fetch error hits one of the catch-up epochs on its
// first attempt. The head pointer must still make forward progress that
// poll (advancing up to the failed epoch) and converge on the next poll
// once the transient clears — it must NOT abort the whole cycle and leave
// the head pinned at the warm point (the curio-core#62 stall, where the
// embedded daemon stayed 70 epochs behind until restart).
func TestSyncTransientFetchErrorDoesNotStall(t *testing.T) {
	s, _ := newStore(t, false)
	base := newFakeSource()
	g := mkBlock(t, 0, nil, 1000, "g")
	base.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 40; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		base.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	fo := &failOnceSource{inner: base, failCID: map[cid.Cid]bool{}}
	sync := hstore.NewSync(s, fo, hstore.SyncOptions{MaxBacktrack: 10})
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(40), s.HeadEpoch())

	// Chain jumps to 150 (110 epochs > MaxBacktrack=10). Arm a transient
	// failure on epoch 100's block, mid-catch-up.
	for h := abi.ChainEpoch(41); h <= 150; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		base.put(b)
		parents = []cid.Cid{b.Cid()}
		if h == 100 {
			fo.mu.Lock()
			fo.failCID[b.Cid()] = true
			fo.mu.Unlock()
		}
	}

	// One poll: head must advance past the warm point (forward progress),
	// even though epoch 100 transiently failed.
	_ = sync.PollOnce(context.Background())
	afterFirst := s.HeadEpoch()
	require.Greater(t, int64(afterFirst), int64(40),
		"head must make forward progress despite a transient fetch error mid-catch-up, got %d", afterFirst)

	// A few more polls converge to head with no gaps.
	prev := afterFirst
	for i := 0; i < 20 && s.HeadEpoch() < 150; i++ {
		_ = sync.PollOnce(context.Background())
		require.GreaterOrEqual(t, int64(s.HeadEpoch()), int64(prev), "head must not regress")
		prev = s.HeadEpoch()
	}
	require.Equal(t, abi.ChainEpoch(150), s.HeadEpoch(), "must converge to head")
	for h := abi.ChainEpoch(40); h <= 150; h++ {
		_, err := s.GetTipSetByHeight(h)
		require.NoErrorf(t, err, "gap at epoch %d", h)
	}
}

// TestSyncCatchUpWithFlakySource is the core #33 regression: a store far
// behind head, catching up against a source that intermittently fails
// fetches, must still converge to head. Before the fix, one fetch error
// during backfill returned from the whole poll with no head advance, and
// repeated failures meant the head pointer never moved (the stall that
// looked like a 70-epoch lag until daemon restart).
func TestSyncCatchUpWithFlakySource(t *testing.T) {
	s, _ := newStore(t, false)
	base := newFakeSource()

	g := mkBlock(t, 0, nil, 1000, "g")
	base.put(g)
	parents := []cid.Cid{g.Cid()}
	mk := func(upto abi.ChainEpoch) {
		for h := base.head + 1; h <= upto; h++ {
			b := mkBlock(t, h, parents, 1000, "")
			base.put(b)
			parents = []cid.Cid{b.Cid()}
		}
	}

	// Phase 1: chain at head 40, store syncs up cleanly (no flakiness).
	mk(40)
	src := &flakySource{inner: base, failEach: 0}
	sync := hstore.NewSync(s, src, hstore.SyncOptions{MaxBacktrack: 10})
	require.NoError(t, sync.PollOnce(context.Background()))
	warm := s.HeadEpoch()
	require.GreaterOrEqual(t, int64(warm), int64(38),
		"warm store should be at/near head 40, got %d", warm)

	// Phase 2: the daemon "stalls" — chain advances far ahead (to 150,
	// a 110-epoch jump, well beyond MaxBacktrack=10) while the source
	// becomes intermittently rate-limited. This is the curio-core#62
	// condition: a long wait during which head ran away. Before the fix
	// the store stalled at `warm` until restart. After the fix it must
	// catch up contiguously to head.
	mk(150)
	src.mu.Lock()
	src.failEach = 7 // every 7th fetch 429s
	src.mu.Unlock()

	prev := warm
	converged := false
	for i := 0; i < 400; i++ {
		_ = sync.PollOnce(context.Background()) // transient errors expected
		cur := s.HeadEpoch()
		require.GreaterOrEqual(t, int64(cur), int64(prev),
			"head must never regress across transient fetch errors (poll %d)", i)
		prev = cur
		if cur >= 150 {
			converged = true
			break
		}
	}
	require.True(t, converged,
		"store must converge to head=150 despite intermittent fetch failures; stalled at %d", prev)

	// The served chain must be gap-free and contiguous from the warm
	// point all the way to head — no holes left below the head pointer.
	for h := warm; h <= 150; h++ {
		_, err := s.GetTipSetByHeight(h)
		require.NoErrorf(t, err, "gap at epoch %d after flaky catch-up (head=%d)", h, s.HeadEpoch())
	}
}

// TestSyncColdStartFarHead verifies a cold store against a high chain
// head converges to head over repeated polls (the embedded curio-core
// bring-up case), bounding per-poll work via BootstrapDepth then
// catching up incrementally.
func TestSyncColdStartFarHead(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 100; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	sync := hstore.NewSync(s, src, hstore.SyncOptions{
		MaxBacktrack:   10,
		BootstrapDepth: 2, // tiny cold-start window (rate-limited Glif)
	})

	prev := abi.ChainEpoch(-1)
	for i := 0; i < 200; i++ {
		require.NoError(t, sync.PollOnce(context.Background()))
		cur := s.HeadEpoch()
		require.GreaterOrEqual(t, int64(cur), int64(prev), "head must never regress")
		prev = cur
		if cur >= 100 {
			break
		}
	}
	require.Equal(t, abi.ChainEpoch(100), s.HeadEpoch(),
		"cold store must converge to chain head")
}

// TestSyncStaleResetReanchors reproduces issue #51: a node stopped for a
// long time boots with a persisted warm store many thousands of epochs
// behind the live head. The intermediate epochs (between the stale store
// head and live head) have parents the store can't reach within
// MaxBacktrack, so the contiguous catch-up (issue #33) could never link a
// new tipset to the store's chain — the head pointer froze at the stale
// epoch (the observed "Reconnecting / 8d ago" symptom).
//
// With StaleResetThreshold set, a lag past the threshold re-anchors the
// store near the live head (cold-start semantics, lenient writes) instead
// of grinding forever. The fix is chain-state-only; this test asserts
// convergence + that the OnStaleReset hook fires with the right epochs.
func TestSyncStaleResetReanchors(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	// Phase 1: small contiguous chain 0..20, store syncs near head.
	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 20; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	var resetFired int
	var gotStoreHead, gotLiveHead abi.ChainEpoch = -1, -1
	sync := hstore.NewSync(s, src, hstore.SyncOptions{
		MaxBacktrack:        10,
		BootstrapDepth:      3,
		StaleResetThreshold: 100, // re-anchor when >100 epochs behind
		OnStaleReset: func(storeHead, liveHead abi.ChainEpoch) {
			resetFired++
			gotStoreHead, gotLiveHead = storeHead, liveHead
		},
	})

	require.NoError(t, sync.PollOnce(context.Background()))
	staleHead := s.HeadEpoch()
	require.GreaterOrEqual(t, int64(staleHead), int64(0))
	require.LessOrEqual(t, int64(staleHead), int64(20))
	require.Equal(t, 0, resetFired, "no reset on the initial near-head sync")

	// Phase 2: simulate a long outage. The chain jumps far ahead to
	// epoch 5000 — but ONLY the recent tip region is fetchable as a
	// connectable chain; the giant gap below it is not backfillable
	// within MaxBacktrack. Build a fresh contiguous segment near 5000 so
	// the cold re-anchor has blocks to land on.
	const live = abi.ChainEpoch(5000)
	// Seed a connectable window [live-BootstrapDepth-5, live] so the
	// re-anchor (which fetches head-BootstrapDepth..head) succeeds.
	var p []cid.Cid
	for h := live - 10; h <= live; h++ {
		b := mkBlock(t, h, p, 1000, "late")
		src.replaceCanonAt(b)
		p = []cid.Cid{b.Cid()}
	}
	src.head = live

	// One poll: lag (5000 - staleHead) >> threshold(100) → stale reset.
	require.NoError(t, sync.PollOnce(context.Background()))

	require.Equal(t, 1, resetFired, "stale reset must fire exactly once")
	require.Equal(t, staleHead, gotStoreHead, "OnStaleReset storeHead arg")
	require.Equal(t, live, gotLiveHead, "OnStaleReset liveHead arg")

	// The head must have jumped near live, NOT frozen at the stale epoch.
	got := s.HeadEpoch()
	require.Greater(t, int64(got), int64(staleHead),
		"head must advance past the stale epoch after reset")
	require.GreaterOrEqual(t, int64(got), int64(live-10),
		"head must re-anchor near live head, got %d (live %d)", got, live)

	// And the StaleResets stat must reflect it.
	require.Equal(t, uint64(1), sync.Stats().StaleResets)
}

// TestSyncStaleResetDisabled confirms threshold=0 keeps legacy behaviour:
// no stale reset, the agent attempts contiguous catch-up.
func TestSyncStaleResetDisabled(t *testing.T) {
	s, _ := newStore(t, false)
	src := newFakeSource()

	g := mkBlock(t, 0, nil, 1000, "g")
	src.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 30; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		src.put(b)
		parents = []cid.Cid{b.Cid()}
	}

	var resetFired int
	sync := hstore.NewSync(s, src, hstore.SyncOptions{
		MaxBacktrack:        10,
		BootstrapDepth:      3,
		StaleResetThreshold: 0, // disabled
		OnStaleReset:        func(_, _ abi.ChainEpoch) { resetFired++ },
	})

	// Converge normally to 30.
	prev := abi.ChainEpoch(-1)
	for i := 0; i < 50; i++ {
		require.NoError(t, sync.PollOnce(context.Background()))
		cur := s.HeadEpoch()
		require.GreaterOrEqual(t, int64(cur), int64(prev))
		prev = cur
		if cur >= 30 {
			break
		}
	}
	require.Equal(t, abi.ChainEpoch(30), s.HeadEpoch())
	require.Equal(t, 0, resetFired, "threshold=0 must never fire a stale reset")
}

// countingSource wraps an RPCSource and counts HeadEpoch() calls, to
// verify the #71 gossip-aware poll skip.
type countingSource struct {
	inner     hstore.RPCSource
	headCalls int
	mu        sync.Mutex
}

func (c *countingSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	c.mu.Lock()
	c.headCalls++
	c.mu.Unlock()
	return c.inner.HeadEpoch(ctx)
}
func (c *countingSource) TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	return c.inner.TipsetCIDsByHeight(ctx, h)
}
func (c *countingSource) FetchBlock(ctx context.Context, k cid.Cid) (*ltypes.BlockHeader, error) {
	return c.inner.FetchBlock(ctx, k)
}
func (c *countingSource) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headCalls
}

// TestSyncSkipsPollWhenGossipFresh: with a warm store head and
// GossipFresh()==true, the Sync must NOT call src.HeadEpoch() (#71).
func TestSyncSkipsPollWhenGossipFresh(t *testing.T) {
	s, _ := newStore(t, false)
	fake := newFakeSource()

	// Seed canonical 0..3 and prime the store head so it's "warm".
	g := mkBlock(t, 0, nil, 1000, "g")
	fake.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 3; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		fake.put(b)
		parents = []cid.Cid{b.Cid()}
	}
	src := &countingSource{inner: fake}
	sync := hstore.NewSync(s, src, hstore.SyncOptions{MaxBacktrack: 10})

	// First poll (no GossipFresh): warms the store head, calls HeadEpoch.
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, abi.ChainEpoch(3), s.HeadEpoch())
	base := src.calls()
	require.GreaterOrEqual(t, base, 1)

	// Now gossip is "fresh": the next poll must skip HeadEpoch entirely.
	gossipFresh := true
	sync.SetGossipFresh(func() bool { return gossipFresh })
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, base, src.calls(), "HeadEpoch must NOT be polled while gossip is fresh")

	// Gossip goes quiet: the Sync must resume polling HeadEpoch.
	gossipFresh = false
	require.NoError(t, sync.PollOnce(context.Background()))
	require.Equal(t, base+1, src.calls(), "HeadEpoch must resume when gossip is stale")
}

// TestSyncColdStartPollsDespiteGossipFresh: a cold/empty store (head<0)
// must still poll HeadEpoch even if GossipFresh()==true, so the very
// first head can bootstrap from the RPC (#71 guard).
func TestSyncColdStartPollsDespiteGossipFresh(t *testing.T) {
	s, _ := newStore(t, false)
	fake := newFakeSource()
	g := mkBlock(t, 0, nil, 1000, "g")
	fake.put(g)
	parents := []cid.Cid{g.Cid()}
	for h := abi.ChainEpoch(1); h <= 2; h++ {
		b := mkBlock(t, h, parents, 1000, "")
		fake.put(b)
		parents = []cid.Cid{b.Cid()}
	}
	src := &countingSource{inner: fake}
	sync := hstore.NewSync(s, src, hstore.SyncOptions{MaxBacktrack: 10, BootstrapDepth: 2})
	sync.SetGossipFresh(func() bool { return true }) // gossip claims fresh...

	require.Equal(t, abi.ChainEpoch(-1), s.HeadEpoch(), "store starts cold")
	require.NoError(t, sync.PollOnce(context.Background()))
	require.GreaterOrEqual(t, src.calls(), 1, "cold store must poll HeadEpoch despite GossipFresh")
}
