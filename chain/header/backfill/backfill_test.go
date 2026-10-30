package backfill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// ---- fake chain ----

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkBlock(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, tag string) *types.BlockHeader {
	t.Helper()
	a, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	return &types.BlockHeader{
		Miner:                 a,
		Ticket:                &types.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &types.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		Parents:               parents,
		ParentWeight:          types.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkCID(t, "state-"+tag),
		ParentMessageReceipts: mkCID(t, "receipts-"+tag),
		Messages:              mkCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         types.NewInt(100),
	}
}

type fakeChain struct {
	byH  map[abi.ChainEpoch]*types.TipSet
	raw  map[cid.Cid][]byte
	head *types.TipSet
}

// buildChain builds tipsets 0..top, skipping heights in nulls, with
// multi[h] blocks at height h (default 1). Tags carry prefix so forks get
// distinct CIDs.
func buildChain(t *testing.T, prefix string, base *types.TipSet, from, top abi.ChainEpoch, nulls map[abi.ChainEpoch]bool, multi map[abi.ChainEpoch]int, fc *fakeChain) *fakeChain {
	t.Helper()
	if fc == nil {
		fc = &fakeChain{byH: map[abi.ChainEpoch]*types.TipSet{}, raw: map[cid.Cid][]byte{}}
	}
	var parents []cid.Cid
	if base != nil {
		parents = base.Cids()
	}
	for h := from; h <= top; h++ {
		if nulls[h] {
			continue
		}
		n := multi[h]
		if n == 0 {
			n = 1
		}
		var blks []*types.BlockHeader
		for i := 0; i < n; i++ {
			b := mkBlock(t, h, parents, fmt.Sprintf("%s-%d-%d", prefix, h, i))
			raw, err := b.Serialize()
			require.NoError(t, err)
			fc.raw[b.Cid()] = raw
			blks = append(blks, b)
		}
		ts, err := types.NewTipSet(blks)
		require.NoError(t, err)
		fc.byH[h] = ts
		fc.head = ts
		parents = ts.Cids()
	}
	return fc
}

// seedStore installs tipsets [lo..hi] of fc as the canonical segment of
// a fresh node (what a node holds after booting at lo).
func seedStore(t *testing.T, st *hstore.Store, fc *fakeChain, lo, hi abi.ChainEpoch) {
	t.Helper()
	for h := lo; h <= hi; h++ {
		ts, ok := fc.byH[h]
		if !ok {
			continue
		}
		require.NoError(t, st.SetHead(context.Background(), ts))
	}
}

type fakeFetcher struct {
	fc      *fakeChain
	calls   atomic.Int64
	tamper  map[cid.Cid][]byte
	block   chan struct{} // when non-nil, Get waits for it (or ctx)
	mu      sync.Mutex
	failAll bool
}

func (f *fakeFetcher) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	fail := f.failAll
	f.mu.Unlock()
	if fail {
		return nil, errors.New("fetch failed")
	}
	if b, ok := f.tamper[c]; ok {
		return b, nil
	}
	b, ok := f.fc.raw[c]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

// fakeXchg serves ChainExchange runs from a fake chain (optionally a
// different, forged chain).
type fakeXchg struct {
	fc    *fakeChain
	calls atomic.Int64
}

func (x *fakeXchg) FetchTipsetChain(_ context.Context, head []cid.Cid, length int) ([][]*types.BlockHeader, error) {
	x.calls.Add(1)
	var cur *types.TipSet
	want := types.NewTipSetKey(head...)
	for _, ts := range x.fc.byH {
		if ts.Key() == want {
			cur = ts
		}
	}
	if cur == nil {
		return nil, errors.New("NotFound")
	}
	var out [][]*types.BlockHeader
	for len(out) < length && cur != nil {
		out = append(out, cur.Blocks())
		if cur.Parents().IsEmpty() {
			break
		}
		var next *types.TipSet
		for _, ts := range x.fc.byH {
			if ts.Key() == cur.Parents() {
				next = ts
			}
		}
		cur = next
	}
	return out, nil
}

func newResolver(t *testing.T, st *hstore.Store, cfg Config, bf BlockFetcher) *Resolver {
	t.Helper()
	r := New(st, cfg)
	t.Cleanup(r.Close)
	if bf != nil {
		r.SetBlockFetcher(func() BlockFetcher { return bf })
	}
	return r
}

func openStore(t *testing.T) *hstore.Store {
	t.Helper()
	st, err := hstore.Open("", hstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ---- tests ----

// A node that booted at 80 must still answer a lookup for 30 by walking
// verified parent links back, and the second lookup must be local.
func TestGapBackfillByCID(t *testing.T) {
	nulls := map[abi.ChainEpoch]bool{40: true, 41: true}
	multi := map[abi.ChainEpoch]int{30: 3, 55: 2, 81: 2}
	fc := buildChain(t, "main", nil, 0, 100, nulls, multi, nil)
	st := openStore(t)
	seedStore(t, st, fc, 80, 100)

	// Before: the store alone cannot serve it.
	_, err := st.GetTipSetByHeight(30)
	require.Error(t, err)

	f := &fakeFetcher{fc: fc}
	r := newResolver(t, st, Config{Finality: 10}, f)
	head := st.Head()
	got, err := r.TipSetByHeight(context.Background(), 30, head)
	require.NoError(t, err)
	require.Equal(t, fc.byH[30].Key(), got.Key())
	require.Len(t, got.Blocks(), 3)
	calls := f.calls.Load()
	require.Positive(t, calls)

	// Persisted: second call is served without any fetch, from the fast
	// path (canonical pointers recorded below finality).
	got2, err := r.TipSetByHeight(context.Background(), 30, head)
	require.NoError(t, err)
	require.Equal(t, got.Key(), got2.Key())
	require.Equal(t, calls, f.calls.Load())
	require.Equal(t, uint64(1), r.Stats().FastHits)

	// And the plain store lookup now works too.
	sts, err := st.GetTipSetByHeight(30)
	require.NoError(t, err)
	require.Equal(t, fc.byH[30].Key(), sts.Key())

	// Every intermediate tipset is persisted.
	for h := abi.ChainEpoch(30); h < 80; h++ {
		if ts, ok := fc.byH[h]; ok {
			_, err := st.GetTipSet(ts.Key())
			require.NoError(t, err, "height %d", h)
		}
	}
}

// Null rounds: ByHeight returns the first non-null tipset below, AfterHeight
// the first non-null tipset above (Lotus semantics).
func TestNullRoundSemantics(t *testing.T) {
	nulls := map[abi.ChainEpoch]bool{40: true, 41: true}
	fc := buildChain(t, "main", nil, 0, 100, nulls, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 80, 100)
	r := newResolver(t, st, Config{}, &fakeFetcher{fc: fc})
	head := st.Head()

	got, err := r.TipSetByHeight(context.Background(), 41, head)
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(39), got.Height())
	require.Equal(t, fc.byH[39].Key(), got.Key())

	after, err := r.TipSetAfterHeight(context.Background(), 40, head)
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(42), after.Height())

	exact, err := r.TipSetAfterHeight(context.Background(), 39, head)
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(39), exact.Height())

	// h == start and h > start.
	same, err := r.TipSetByHeight(context.Background(), head.Height(), head)
	require.NoError(t, err)
	require.Equal(t, head.Key(), same.Key())
	_, err = r.TipSetByHeight(context.Background(), head.Height()+1, head)
	require.ErrorIs(t, err, ErrFutureHeight)
}

// A fetched header that does not hash to the CID its child references is
// rejected and nothing is persisted for it.
func TestHashMismatchRejected(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 60, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 50, 60)

	// Parent of 50 is 49: serve a different (valid) header's bytes for it.
	victim := fc.byH[49].Cids()[0]
	forged := mkBlock(t, 49, fc.byH[48].Cids(), "forged")
	forgedRaw, err := forged.Serialize()
	require.NoError(t, err)
	f := &fakeFetcher{fc: fc, tamper: map[cid.Cid][]byte{victim: forgedRaw}}
	r := newResolver(t, st, Config{}, f)

	_, err = r.TipSetByHeight(context.Background(), 45, st.Head())
	require.Error(t, err)
	require.Contains(t, err.Error(), "do not hash to the requested CID")
	require.Positive(t, r.Stats().Rejected)
	_, gerr := st.Get(victim)
	require.Error(t, gerr)
	_, gerr = st.Get(forged.Cid())
	require.Error(t, gerr)
}

// A ChainExchange peer serving a different chain is rejected; the walk
// falls back to the per-CID path and still returns the real tipset.
func TestForgedChainExchangeRejected(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 60, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 50, 60)

	// The forged peer answers the right head CIDs (it has to, or the
	// client would drop it) but then splices a fake ancestry below.
	forged := &fakeChain{byH: map[abi.ChainEpoch]*types.TipSet{}, raw: map[cid.Cid][]byte{}}
	forged.byH[49] = fc.byH[49]
	fake := buildChain(t, "fake", nil, 0, 47, nil, nil, nil)
	for h, ts := range fake.byH {
		forged.byH[h] = ts
	}
	// Make 49's parent lookup resolve to the fake 47.
	forged.byH[48] = fake.byH[47]
	x := &forgingXchg{real: fc, fake: fake}
	f := &fakeFetcher{fc: fc}
	r := newResolver(t, st, Config{}, f)
	r.SetChainFetcher(x)

	got, err := r.TipSetByHeight(context.Background(), 40, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[40].Key(), got.Key())
	require.Positive(t, r.Stats().Rejected)
	require.Positive(t, f.calls.Load())
	// The verified prefix (49) came from chainxchg; the forged levels never
	// reached the store.
	for _, ts := range fake.byH {
		_, err := st.GetTipSet(ts.Key())
		require.Error(t, err)
	}
}

// forgingXchg returns the real first level and then a fake ancestry.
type forgingXchg struct{ real, fake *fakeChain }

func (x *forgingXchg) FetchTipsetChain(_ context.Context, head []cid.Cid, length int) ([][]*types.BlockHeader, error) {
	want := types.NewTipSetKey(head...)
	var first *types.TipSet
	for _, ts := range x.real.byH {
		if ts.Key() == want {
			first = ts
		}
	}
	if first == nil {
		return nil, errors.New("NotFound")
	}
	out := [][]*types.BlockHeader{first.Blocks()}
	for h := first.Height() - 1; h >= 0 && len(out) < length; h-- {
		if ts, ok := x.fake.byH[h]; ok {
			out = append(out, ts.Blocks())
		}
	}
	return out, nil
}

// ChainExchange is preferred when wired: a 50-tipset gap is one or two
// requests, with no per-CID fetches.
func TestChainExchangePath(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 200, map[abi.ChainEpoch]bool{120: true}, map[abi.ChainEpoch]int{130: 2}, nil)
	st := openStore(t)
	seedStore(t, st, fc, 180, 200)
	f := &fakeFetcher{fc: fc}
	x := &fakeXchg{fc: fc}
	r := newResolver(t, st, Config{ChainBatch: 40}, f)
	r.SetChainFetcher(x)

	got, err := r.TipSetByHeight(context.Background(), 100, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[100].Key(), got.Key())
	require.Zero(t, f.calls.Load())
	require.Positive(t, x.calls.Load())
	require.LessOrEqual(t, x.calls.Load(), int64(3))
	require.Positive(t, r.Stats().XchgTipsets)
}

// When the walk does not finish inside the per-call budget the RPC gets a
// retryable error (not "not found"), the walk keeps going in the
// background, and a later call succeeds.
func TestTimeoutIsRetryableAndResumes(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 100, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 80, 100)
	f := &fakeFetcher{fc: fc, block: make(chan struct{})}
	r := newResolver(t, st, Config{CallTimeout: 150 * time.Millisecond}, f)

	start := time.Now()
	_, err := r.TipSetByHeight(context.Background(), 50, st.Head())
	require.Error(t, err)
	require.True(t, IsRetryable(err), "got %v", err)
	require.NotContains(t, err.Error(), "not found")
	require.Contains(t, err.Error(), "retry")
	require.Less(t, time.Since(start), 2*time.Second)

	// Unblock the source; the background job finishes on its own.
	close(f.block)
	require.Eventually(t, func() bool {
		_, err := st.GetTipSet(fc.byH[50].Key())
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	got, err := r.TipSetByHeight(context.Background(), 50, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[50].Key(), got.Key())
}

// A source that fails outright yields a retryable error too.
func TestFetchFailureIsRetryable(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 100, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 80, 100)
	f := &fakeFetcher{fc: fc, failAll: true}
	r := newResolver(t, st, Config{}, f)
	_, err := r.TipSetByHeight(context.Background(), 50, st.Head())
	require.Error(t, err)
	require.True(t, IsRetryable(err), "got %v", err)
}

func TestBeyondLookback(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 100, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 80, 100)
	f := &fakeFetcher{fc: fc}
	r := newResolver(t, st, Config{MaxLookback: 30}, f)
	_, err := r.TipSetByHeight(context.Background(), 50, st.Head())
	require.ErrorIs(t, err, ErrBeyondLookback)
	require.Zero(t, f.calls.Load())
	got, err := r.TipSetByHeight(context.Background(), 75, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[75].Key(), got.Key())
}

// The start tipset names the chain to walk: from a fork tip, heights above
// the fork point resolve to the fork, below it to the shared ancestry, and
// canonical pointers are never written from a non-canonical walk.
func TestHonorsStartTipsetOnFork(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 100, nil, nil, nil)
	fork := buildChain(t, "fork", fc.byH[90], 91, 95, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 85, 100)
	for h := abi.ChainEpoch(91); h <= 95; h++ {
		require.NoError(t, st.PutTipSet(fork.byH[h]))
	}
	all := &fakeChain{byH: fc.byH, raw: map[cid.Cid][]byte{}}
	for k, v := range fc.raw {
		all.raw[k] = v
	}
	for k, v := range fork.raw {
		all.raw[k] = v
	}
	r := newResolver(t, st, Config{Finality: 1}, &fakeFetcher{fc: all})
	tip := fork.byH[95]

	got, err := r.TipSetByHeight(context.Background(), 92, tip)
	require.NoError(t, err)
	require.Equal(t, fork.byH[92].Key(), got.Key())

	got, err = r.TipSetByHeight(context.Background(), 60, tip)
	require.NoError(t, err)
	require.Equal(t, fc.byH[60].Key(), got.Key())
	// Walked from a non-canonical tip: no canonical pointer at 60 yet.
	_, err = st.Tipset(60)
	require.Error(t, err)

	// From the canonical head the main chain wins at 92.
	got, err = r.TipSetByHeight(context.Background(), 92, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[92].Key(), got.Key())
}

// Stale canonical pointers left by an earlier run (an island that does not
// link to the current chain) must not be served.
func TestIgnoresDisconnectedIsland(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 100, nil, nil, nil)
	island := buildChain(t, "island", nil, 0, 20, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, island, 10, 20)
	seedStore(t, st, fc, 80, 100)
	// The island answers 15 from the plain store (that's the hazard).
	sts, err := st.GetTipSetByHeight(15)
	require.NoError(t, err)
	require.Equal(t, island.byH[15].Key(), sts.Key())

	r := newResolver(t, st, Config{MaxLookback: 200, Finality: 10}, &fakeFetcher{fc: fc})
	got, err := r.TipSetByHeight(context.Background(), 15, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[15].Key(), got.Key())
	// Canonical pointers were repaired.
	sts, err = st.GetTipSetByHeight(15)
	require.NoError(t, err)
	require.Equal(t, fc.byH[15].Key(), sts.Key())
}

// Concurrent callers share one background walk.
func TestConcurrentCallersShareJob(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 300, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 280, 300)
	f := &fakeFetcher{fc: fc}
	r := newResolver(t, st, Config{}, f)
	head := st.Head()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.TipSetByHeight(context.Background(), 100, head)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	// One fetch per missing tipset (180 of them), not one per caller.
	require.LessOrEqual(t, f.calls.Load(), int64(200))
}

// Startup prefetch fills the last N epochs so the first lookup is local.
func TestPrefetch(t *testing.T) {
	fc := buildChain(t, "main", nil, 0, 300, nil, nil, nil)
	st := openStore(t)
	seedStore(t, st, fc, 290, 300)
	f := &fakeFetcher{fc: fc}
	r := newResolver(t, st, Config{Finality: 10}, f)
	require.NoError(t, r.Prefetch(context.Background(), 150, 0))
	calls := f.calls.Load()
	got, err := r.TipSetByHeight(context.Background(), 160, st.Head())
	require.NoError(t, err)
	require.Equal(t, fc.byH[160].Key(), got.Key())
	require.Equal(t, calls, f.calls.Load())
}

func TestRetryableErrorText(t *testing.T) {
	err := &RetryableError{Height: 10, Reached: 50, Cause: context.DeadlineExceeded}
	require.True(t, strings.Contains(err.Error(), "retry shortly"))
	require.True(t, errors.Is(err, context.DeadlineExceeded))
}
