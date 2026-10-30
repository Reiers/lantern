// Package backfill resolves tipsets that sit below the header store's
// coverage by walking hash links backwards from a trusted tipset.
//
// A freshly initialised Lantern node only holds headers from its boot
// anchor forward, so a Lotus-style ChainGetTipSetByHeight for a height
// older than the node's runtime used to fail with "header/store: not
// found". Curio's StorageGCMark asks for head - (WinningPoStSectorSetLookback
// + EpochsInDay + 1), about 24h back, so a node younger than a day logged
// that error on every run.
//
// The Resolver closes the gap without adding trust: it starts from a
// tipset that is already trusted (the store head, or a tipset the caller
// named that resolves to stored headers), and fetches each missing parent
// tipset by the CIDs its child references. Every fetched header must hash
// to one of those CIDs, the block set must match the child's Parents
// exactly, and heights must strictly decrease, so the result is the same
// hash-linked chain the trusted tipset commits to. Fetched headers are
// persisted in the header store, and canonical pointers are recorded below
// finality, so the next lookup is a local read.
//
// Fetch sources, in order: a ChainExchange client (verified header runs of
// up to ChainBatch tipsets per request from Filecoin peers) and the
// content-addressed block getter (bitswap / gateway / RPC fallback, one
// tipset per hop, blocks of a tipset fetched concurrently).
//
// Cost is bounded: MaxLookback caps how far below the starting tipset a
// lookup may reach, each call waits at most CallTimeout, and the walk
// itself runs as a single shared background job (JobTimeout) so a caller
// that times out gets a retryable error while the walk keeps making
// persisted progress for the next call.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/header/backfill")

const (
	// EpochsInDay matches Filecoin's 30s epoch.
	EpochsInDay abi.ChainEpoch = 2880
	// DefaultMaxLookback is two days of epochs plus slack. Curio's
	// StorageGCMark needs about one day (WinningPoStSectorSetLookback +
	// EpochsInDay + 1); two days leaves room for other lookback readers.
	DefaultMaxLookback = 2*EpochsInDay + 120
	// DefaultCallTimeout bounds one RPC lookup, well under typical client
	// timeouts (Curio / go-jsonrpc callers commonly allow 30s or more).
	DefaultCallTimeout = 20 * time.Second
	// DefaultJobTimeout bounds one background walk.
	DefaultJobTimeout = 10 * time.Minute
	// DefaultConcurrency bounds parallel block fetches within a tipset.
	DefaultConcurrency = 4
	// DefaultChainBatch is the ChainExchange request length (protocol max
	// is 900).
	DefaultChainBatch = 500
	// DefaultFinality is the depth below which backfilled tipsets get
	// canonical pointers (Filecoin's 900-epoch finality). Above it the
	// head-sync path owns the pointers.
	DefaultFinality abi.ChainEpoch = 900
	// DefaultStartupDepth is how far the optional startup prefetch walks
	// (one day plus slack, covering Curio's StorageGCMark lookback).
	DefaultStartupDepth = EpochsInDay + 200

	// maxNullScan bounds the canonical-pointer scan when looking for the
	// tipset at or below a height. Long null runs fall through to the walk.
	maxNullScan abi.ChainEpoch = 900
	// xchgBackoff is how long ChainExchange is skipped after a failure
	// within one job, so a peerless node does not pay its timeout per hop.
	xchgBackoff = 30 * time.Second
	// xchgRequestTimeout bounds one ChainExchange request.
	xchgRequestTimeout = 60 * time.Second
)

// ErrBeyondLookback is returned when the requested height is further below
// the starting tipset than MaxLookback allows.
var ErrBeyondLookback = errors.New("chain: requested height is beyond the header backfill lookback")

// ErrFutureHeight is returned when the requested height is above the
// starting tipset.
var ErrFutureHeight = errors.New("chain: looking for tipset with height greater than start point")

// RetryableError means the backfill to Height is not finished yet (or a
// fetch failed transiently). Fetched headers are persisted, so retrying
// resumes from Reached instead of starting over.
type RetryableError struct {
	Height  abi.ChainEpoch // requested height
	Reached abi.ChainEpoch // lowest height the local chain reaches so far
	Cause   error
}

func (e *RetryableError) Error() string {
	return fmt.Sprintf("chain: header backfill to height %d still in progress (local chain reaches %d), retry shortly: %v",
		e.Height, e.Reached, e.Cause)
}

func (e *RetryableError) Unwrap() error { return e.Cause }

// IsRetryable reports whether err is a RetryableError.
func IsRetryable(err error) bool {
	var re *RetryableError
	return errors.As(err, &re)
}

// BlockFetcher is a content-addressed block source (hamt.BlockGetter /
// combined.Fetcher). Bytes are re-verified against the CID here.
type BlockFetcher interface {
	Get(ctx context.Context, c cid.Cid) ([]byte, error)
}

// ChainFetcher fetches a verified run of header tipsets walking
// parent-ward from head (net/chainxchg.Client). Index 0 must be the tipset
// whose block CIDs are head. Results are re-verified here.
type ChainFetcher interface {
	FetchTipsetChain(ctx context.Context, head []cid.Cid, length int) ([][]*types.BlockHeader, error)
}

// Config tunes the Resolver. Zero values take the defaults above.
type Config struct {
	MaxLookback abi.ChainEpoch
	CallTimeout time.Duration
	JobTimeout  time.Duration
	Concurrency int
	ChainBatch  int
	Finality    abi.ChainEpoch
}

func (c Config) withDefaults() Config {
	if c.MaxLookback <= 0 {
		c.MaxLookback = DefaultMaxLookback
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = DefaultJobTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.ChainBatch <= 0 || c.ChainBatch > 900 {
		c.ChainBatch = DefaultChainBatch
	}
	if c.Finality <= 0 {
		c.Finality = DefaultFinality
	}
	return c
}

// Stats are observable counters.
type Stats struct {
	Lookups        uint64
	FastHits       uint64
	LocalWalks     uint64
	Jobs           uint64
	JobFailures    uint64
	TipsetsFetched uint64
	XchgTipsets    uint64
	Rejected       uint64
	Timeouts       uint64
}

// Resolver serves tipset lookups below the header store's coverage.
type Resolver struct {
	store *hstore.Store
	cfg   Config

	fetchMu sync.RWMutex
	blocks  func() BlockFetcher
	chain   ChainFetcher

	jobMu sync.Mutex
	job   *job

	baseCtx context.Context
	cancel  context.CancelFunc

	lookups, fastHits, localWalks, jobs, jobFailures atomic.Uint64
	fetched, xchgTipsets, rejected, timeouts         atomic.Uint64
}

type job struct {
	done    chan struct{}
	target  abi.ChainEpoch
	reached atomic.Int64
	err     error
}

// New builds a Resolver over store. Call SetBlockFetcher and optionally
// SetChainFetcher to enable remote fetches; without them lookups are
// served from the store only.
func New(store *hstore.Store, cfg Config) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{store: store, cfg: cfg.withDefaults(), baseCtx: ctx, cancel: cancel}
}

// Close stops any background walk.
func (r *Resolver) Close() { r.cancel() }

// Config returns the effective configuration.
func (r *Resolver) Config() Config { return r.cfg }

// SetBlockFetcher wires the content-addressed fetcher. The func is called
// on every use so a fetcher that is rebuilt later (bitswap coming up after
// the RPC server) is always the current one.
func (r *Resolver) SetBlockFetcher(f func() BlockFetcher) {
	r.fetchMu.Lock()
	r.blocks = f
	r.fetchMu.Unlock()
}

// SetChainFetcher wires the ChainExchange client (optional).
func (r *Resolver) SetChainFetcher(f ChainFetcher) {
	r.fetchMu.Lock()
	r.chain = f
	r.fetchMu.Unlock()
}

func (r *Resolver) blockFetcher() BlockFetcher {
	r.fetchMu.RLock()
	f := r.blocks
	r.fetchMu.RUnlock()
	if f == nil {
		return nil
	}
	return f()
}

func (r *Resolver) chainFetcher() ChainFetcher {
	r.fetchMu.RLock()
	defer r.fetchMu.RUnlock()
	return r.chain
}

// Stats returns a snapshot of the counters.
func (r *Resolver) Stats() Stats {
	return Stats{
		Lookups:        r.lookups.Load(),
		FastHits:       r.fastHits.Load(),
		LocalWalks:     r.localWalks.Load(),
		Jobs:           r.jobs.Load(),
		JobFailures:    r.jobFailures.Load(),
		TipsetsFetched: r.fetched.Load(),
		XchgTipsets:    r.xchgTipsets.Load(),
		Rejected:       r.rejected.Load(),
		Timeouts:       r.timeouts.Load(),
	}
}

// TipSetByHeight returns the tipset at height h on the chain that ends at
// from, with Lotus ChainGetTipSetByHeight semantics: when h is a null
// round the first non-null tipset BELOW h is returned.
func (r *Resolver) TipSetByHeight(ctx context.Context, h abi.ChainEpoch, from *types.TipSet) (*types.TipSet, error) {
	at, _, err := r.resolve(ctx, h, from)
	return at, err
}

// TipSetAfterHeight is TipSetByHeight with Lotus ChainGetTipSetAfterHeight
// semantics: when h is a null round the first non-null tipset ABOVE h is
// returned.
func (r *Resolver) TipSetAfterHeight(ctx context.Context, h abi.ChainEpoch, from *types.TipSet) (*types.TipSet, error) {
	at, child, err := r.resolve(ctx, h, from)
	if err != nil {
		return nil, err
	}
	if at.Height() == h || child == nil {
		return at, nil
	}
	return child, nil
}

// resolve returns the tipset at or below h on from's chain plus the tipset
// directly above it on that chain (nil when at == from).
func (r *Resolver) resolve(ctx context.Context, h abi.ChainEpoch, from *types.TipSet) (*types.TipSet, *types.TipSet, error) {
	r.lookups.Add(1)
	if from == nil {
		return nil, nil, errors.New("chain: no starting tipset (local head not synced yet)")
	}
	if h < 0 {
		return nil, nil, fmt.Errorf("chain: negative height %d", h)
	}
	if h > from.Height() {
		return nil, nil, fmt.Errorf("%w (%d > %d)", ErrFutureHeight, h, from.Height())
	}
	if h == from.Height() {
		return from, nil, nil
	}
	if depth := from.Height() - h; depth > r.cfg.MaxLookback {
		return nil, nil, fmt.Errorf("%w: height %d is %d epochs below %d, lookback limit is %d", ErrBeyondLookback, h, depth, from.Height(), r.cfg.MaxLookback)
	}

	canonFrom := r.isCanonical(from)
	if canonFrom {
		if at, child, ok := r.fastPath(h, from); ok {
			r.fastHits.Add(1)
			return at, child, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	reached := from.Height()
	for attempt := 0; attempt < 4; attempt++ {
		r.localWalks.Add(1)
		at, child, low, path, err := r.walkLocal(ctx, h, from)
		if err != nil {
			if ctx.Err() != nil {
				r.timeouts.Add(1)
				return nil, nil, &RetryableError{Height: h, Reached: reached, Cause: ctx.Err()}
			}
			return nil, nil, err
		}
		if at != nil {
			if canonFrom {
				r.markCanonical(path, from)
			}
			return at, child, nil
		}
		reached = low.Height()
		if err := r.runJob(ctx, low, h); err != nil {
			if ctx.Err() != nil {
				r.timeouts.Add(1)
				return nil, nil, &RetryableError{Height: h, Reached: r.reachedNow(reached), Cause: fmt.Errorf("per-call budget %s exhausted, backfill continues in the background", r.cfg.CallTimeout)}
			}
			return nil, nil, &RetryableError{Height: h, Reached: r.reachedNow(reached), Cause: err}
		}
	}
	return nil, nil, &RetryableError{Height: h, Reached: reached, Cause: errors.New("walk did not converge")}
}

func (r *Resolver) reachedNow(fallback abi.ChainEpoch) abi.ChainEpoch {
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	if r.job != nil {
		if v := abi.ChainEpoch(r.job.reached.Load()); v > 0 && v < fallback {
			return v
		}
	}
	return fallback
}

func (r *Resolver) isCanonical(ts *types.TipSet) bool {
	c, err := r.store.Tipset(ts.Height())
	return err == nil && c != nil && c.Key() == ts.Key()
}

// fastPath answers from canonical pointers alone. The highest canonical
// tipset X at or below h is accepted only if the canonical pointers from X
// up to from form an unbroken parent-linked run ending exactly at from.
// That rejects islands left behind by a stale-reset and stale pointers at
// epochs that later became null rounds. The check decodes one header per
// epoch, which is a few tens of milliseconds for a day of chain.
func (r *Resolver) fastPath(h abi.ChainEpoch, from *types.TipSet) (*types.TipSet, *types.TipSet, bool) {
	var at *types.TipSet
	for ep := h; ep >= 0 && h-ep <= maxNullScan; ep-- {
		if ts, err := r.store.Tipset(ep); err == nil && ts != nil {
			at = ts
			break
		}
	}
	if at == nil {
		return nil, nil, false
	}
	var child *types.TipSet
	expect := at.Key()
	for ep := at.Height() + 1; ep <= from.Height(); ep++ {
		key, parents, ok, err := r.store.CanonicalLink(ep)
		if err != nil {
			return nil, nil, false
		}
		if !ok {
			continue
		}
		if parents != expect {
			return nil, nil, false
		}
		if child == nil {
			c, err := r.store.GetTipSet(key)
			if err != nil {
				return nil, nil, false
			}
			child = c
		}
		expect = key
	}
	if expect != from.Key() || child == nil {
		return nil, nil, false
	}
	return at, child, true
}

// walkLocal follows parent links from from through the store. It returns
// the tipset at or below h (and its child on the path) when the store
// holds the whole run, otherwise the lowest stored tipset reached (low).
// path is the visited run, newest first.
func (r *Resolver) walkLocal(ctx context.Context, h abi.ChainEpoch, from *types.TipSet) (at, child, low *types.TipSet, path []*types.TipSet, err error) {
	cur := from
	path = []*types.TipSet{from}
	for i := 0; ; i++ {
		if cur.Height() <= h {
			return cur, child, nil, path, nil
		}
		if i&255 == 255 {
			if cerr := ctx.Err(); cerr != nil {
				return nil, nil, nil, nil, cerr
			}
		}
		pk := cur.Parents()
		if pk.IsEmpty() {
			return nil, nil, nil, nil, fmt.Errorf("chain: tipset at %d has no parent links; cannot walk to %d", cur.Height(), h)
		}
		pts, gerr := r.store.GetTipSet(pk)
		if gerr != nil {
			return nil, nil, cur, path, nil
		}
		if pts.Height() >= cur.Height() {
			return nil, nil, nil, nil, fmt.Errorf("chain: stored parent of %d has height %d", cur.Height(), pts.Height())
		}
		child, cur = cur, pts
		path = append(path, cur)
	}
}

// markCanonical records canonical pointers for a verified path that hangs
// off the canonical tipset from, below finality only.
func (r *Resolver) markCanonical(path []*types.TipSet, from *types.TipSet) {
	if len(path) < 2 {
		return
	}
	head := from.Height()
	if sh := r.store.HeadEpoch(); sh > head {
		head = sh
	}
	if n, err := r.store.SetCanonicalSegment(path, head-r.cfg.Finality); err != nil {
		log.Warnw("header backfill: recording canonical pointers failed", "err", err)
	} else if n > 0 {
		log.Debugw("header backfill: canonical pointers recorded", "count", n)
	}
}

// runJob makes sure a background walk from low down to target runs, and
// waits for it (or ctx). If another walk is already running the caller
// waits for that one instead; the caller then re-walks locally, which
// picks up whatever the finished job persisted.
func (r *Resolver) runJob(ctx context.Context, low *types.TipSet, target abi.ChainEpoch) error {
	r.jobMu.Lock()
	if j := r.job; j != nil {
		r.jobMu.Unlock()
		select {
		case <-j.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	j := &job{done: make(chan struct{}), target: target}
	j.reached.Store(int64(low.Height()))
	r.job = j
	r.jobMu.Unlock()
	r.jobs.Add(1)

	go func() {
		jctx, cancel := context.WithTimeout(r.baseCtx, r.cfg.JobTimeout)
		defer cancel()
		start := time.Now()
		err := r.walkRemote(jctx, low, target, j)
		if err != nil {
			r.jobFailures.Add(1)
			log.Warnw("header backfill: walk stopped", "from", low.Height(), "target", target,
				"reached", j.reached.Load(), "took", time.Since(start).String(), "err", err)
		} else {
			log.Infow("header backfill: walk complete", "from", low.Height(), "target", target,
				"reached", j.reached.Load(), "took", time.Since(start).String())
		}
		r.jobMu.Lock()
		j.err = err
		r.job = nil
		r.jobMu.Unlock()
		close(j.done)
	}()

	select {
	case <-j.done:
		return j.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// walkRemote fetches and persists parent tipsets from low down to target.
func (r *Resolver) walkRemote(ctx context.Context, low *types.TipSet, target abi.ChainEpoch, j *job) error {
	cur := low
	var xchgSkipUntil time.Time
	for cur.Height() > target {
		if err := ctx.Err(); err != nil {
			return err
		}
		pk := cur.Parents()
		if pk.IsEmpty() {
			return nil // genesis
		}
		if pts, err := r.store.GetTipSet(pk); err == nil {
			cur = pts
			j.reached.Store(int64(cur.Height()))
			continue
		}

		if cf := r.chainFetcher(); cf != nil && time.Now().After(xchgSkipUntil) {
			n := int(cur.Height() - target)
			if n > r.cfg.ChainBatch {
				n = r.cfg.ChainBatch
			}
			if n < 1 {
				n = 1
			}
			rctx, rcancel := context.WithTimeout(ctx, xchgRequestTimeout)
			levels, err := cf.FetchTipsetChain(rctx, pk.Cids(), n)
			rcancel()
			if err == nil {
				tss, verr := verifyLevels(cur, levels)
				if verr != nil {
					r.rejected.Add(1)
					err = verr
				}
				// Persist whatever verified prefix we got.
				for _, ts := range tss {
					if perr := r.store.PutTipSet(ts); perr != nil {
						return fmt.Errorf("persist tipset %d: %w", ts.Height(), perr)
					}
					cur = ts
					j.reached.Store(int64(cur.Height()))
				}
				r.fetched.Add(uint64(len(tss)))
				r.xchgTipsets.Add(uint64(len(tss)))
				if len(tss) > 0 && verr == nil {
					continue
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Debugw("header backfill: chainxchg unavailable, using per-CID fetch", "height", cur.Height(), "err", err)
			xchgSkipUntil = time.Now().Add(xchgBackoff)
			if cur.Height() <= target {
				break
			}
		}

		pts, err := r.fetchParent(ctx, cur)
		if err != nil {
			return err
		}
		if err := r.store.PutTipSet(pts); err != nil {
			return fmt.Errorf("persist tipset %d: %w", pts.Height(), err)
		}
		r.fetched.Add(1)
		cur = pts
		j.reached.Store(int64(cur.Height()))
	}
	return nil
}

// fetchParent fetches the parent tipset of child block by block (bounded
// concurrency), verifying every header against the CIDs child references.
func (r *Resolver) fetchParent(ctx context.Context, child *types.TipSet) (*types.TipSet, error) {
	want := child.Parents().Cids()
	blocks, err := r.fetchBlocks(ctx, want)
	if err != nil {
		return nil, err
	}
	ts, err := verifyLevel(child.Parents(), child.Height(), blocks)
	if err != nil {
		r.rejected.Add(1)
		return nil, err
	}
	return ts, nil
}

// fetchBlocks fetches each CID (store first, then the block fetcher),
// verifying each header hashes to its CID.
func (r *Resolver) fetchBlocks(ctx context.Context, want []cid.Cid) ([]*types.BlockHeader, error) {
	out := make([]*types.BlockHeader, len(want))
	var missing []int
	for i, c := range want {
		if bh, err := r.store.Get(c); err == nil {
			out[i] = bh
			continue
		}
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return out, nil
	}
	bf := r.blockFetcher()
	if bf == nil {
		return nil, errors.New("no block fetcher configured for header backfill")
	}
	sem := make(chan struct{}, r.cfg.Concurrency)
	errs := make([]error, len(want))
	var wg sync.WaitGroup
	for _, i := range missing {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var bh *types.BlockHeader
			var err error
			for try := 0; try < 2; try++ {
				bh, err = fetchHeader(ctx, bf, want[i])
				if err == nil || ctx.Err() != nil || errors.Is(err, errCIDMismatch) {
					break
				}
			}
			out[i], errs[i] = bh, err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			if errors.Is(err, errCIDMismatch) {
				r.rejected.Add(1)
			}
			return nil, fmt.Errorf("fetch header %s: %w", want[i], err)
		}
	}
	return out, nil
}

var errCIDMismatch = errors.New("header bytes do not hash to the requested CID")

// fetchHeader fetches one header and verifies it against c twice over:
// the raw bytes must hash to c, and the decoded header must re-encode to c.
func fetchHeader(ctx context.Context, bf BlockFetcher, c cid.Cid) (*types.BlockHeader, error) {
	raw, err := bf.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	got, err := c.Prefix().Sum(raw)
	if err != nil {
		return nil, err
	}
	if !got.Equals(c) {
		return nil, fmt.Errorf("%w: requested %s, got %s", errCIDMismatch, c, got)
	}
	bh, err := types.DecodeBlock(raw)
	if err != nil {
		return nil, fmt.Errorf("decode header %s: %w", c, err)
	}
	if bh.Cid() != c {
		return nil, fmt.Errorf("%w: header re-encodes to %s, requested %s", errCIDMismatch, bh.Cid(), c)
	}
	return bh, nil
}

// verifyLevel checks that blocks form exactly the tipset named by want and
// sit strictly below childHeight.
func verifyLevel(want types.TipSetKey, childHeight abi.ChainEpoch, blocks []*types.BlockHeader) (*types.TipSet, error) {
	if len(blocks) == 0 {
		return nil, errors.New("empty tipset")
	}
	wantSet := make(map[cid.Cid]bool)
	for _, c := range want.Cids() {
		wantSet[c] = true
	}
	got := make(map[cid.Cid]bool, len(blocks))
	for _, b := range blocks {
		if b == nil {
			return nil, errors.New("nil header in tipset")
		}
		c := b.Cid()
		if !wantSet[c] {
			return nil, fmt.Errorf("%w: header %s at height %d is not referenced by its child", errCIDMismatch, c, b.Height)
		}
		got[c] = true
	}
	if len(got) != len(wantSet) {
		return nil, fmt.Errorf("tipset has %d of %d referenced blocks", len(got), len(wantSet))
	}
	ts, err := types.NewTipSet(append([]*types.BlockHeader(nil), blocks...))
	if err != nil {
		return nil, err
	}
	if ts.Height() >= childHeight {
		return nil, fmt.Errorf("parent height %d not below child height %d", ts.Height(), childHeight)
	}
	return ts, nil
}

// verifyLevels verifies a ChainExchange run below child and returns the
// longest verified prefix as tipsets (newest first). A non-nil error means
// the run broke at some level; the returned prefix is still sound.
func verifyLevels(child *types.TipSet, levels [][]*types.BlockHeader) ([]*types.TipSet, error) {
	out := make([]*types.TipSet, 0, len(levels))
	want, h := child.Parents(), child.Height()
	for i, blocks := range levels {
		ts, err := verifyLevel(want, h, blocks)
		if err != nil {
			return out, fmt.Errorf("chainxchg level %d: %w", i, err)
		}
		out = append(out, ts)
		if ts.Parents().IsEmpty() {
			break
		}
		want, h = ts.Parents(), ts.Height()
	}
	return out, nil
}

// LoadTipSet returns the tipset with key tsk, fetching and persisting any
// blocks the store lacks. The key itself is the trust anchor: every block
// must hash to one of its CIDs.
func (r *Resolver) LoadTipSet(ctx context.Context, tsk types.TipSetKey) (*types.TipSet, error) {
	if tsk.IsEmpty() {
		return nil, errors.New("empty tipset key")
	}
	if ts, err := r.store.GetTipSet(tsk); err == nil {
		return ts, nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	blocks, err := r.fetchBlocks(ctx, tsk.Cids())
	if err != nil {
		return nil, err
	}
	ts, err := types.NewTipSet(blocks)
	if err != nil {
		return nil, err
	}
	if err := r.store.PutTipSet(ts); err != nil {
		return nil, err
	}
	return ts, nil
}

// LoadBlock returns the header with CID c, fetching (verified) and
// persisting it when the store lacks it.
func (r *Resolver) LoadBlock(ctx context.Context, c cid.Cid) (*types.BlockHeader, error) {
	if bh, err := r.store.Get(c); err == nil {
		return bh, nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	blocks, err := r.fetchBlocks(ctx, []cid.Cid{c})
	if err != nil {
		return nil, err
	}
	if ts, terr := types.NewTipSet([]*types.BlockHeader{blocks[0]}); terr == nil {
		_ = r.store.PutTipSet(ts)
	}
	return blocks[0], nil
}

// Prefetch backfills headers from the store head down to head-depth in
// the background (newest first), so the first lookback reader after boot
// is served locally. It waits startDelay plus for a store head, retries
// with backoff on failure, and returns when done or ctx is cancelled. Run
// it in its own goroutine.
func (r *Resolver) Prefetch(ctx context.Context, depth abi.ChainEpoch, startDelay time.Duration) error {
	if depth <= 0 {
		return nil
	}
	if depth > r.cfg.MaxLookback {
		depth = r.cfg.MaxLookback
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(startDelay):
	}
	backoff := 15 * time.Second
	for {
		head := r.store.Head()
		if head != nil && head.Height() > depth && !head.Parents().IsEmpty() {
			target := head.Height() - depth
			at, _, low, path, err := r.walkLocal(ctx, target, head)
			if err == nil && at != nil {
				if r.isCanonical(head) {
					r.markCanonical(path, head)
				}
				log.Infow("header backfill: startup prefetch complete", "head", head.Height(), "down_to", at.Height())
				return nil
			}
			if err == nil && low != nil {
				log.Infow("header backfill: startup prefetch running", "head", head.Height(), "target", target, "local_low", low.Height())
				jerr := r.runJob(ctx, low, target)
				if jerr == nil {
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Warnw("header backfill: startup prefetch retrying", "err", jerr, "backoff", backoff.String())
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}
