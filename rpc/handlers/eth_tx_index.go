package handlers

// Persistent, bounded eth tx index wiring (F12: Curio watcher receipts
// after a restart).
//
// Before: a receipt / tx-by-hash lookup for a hash not in the in-memory
// "published locally" map walked the chain backward from head by parent
// links, and for EVERY tipset fetched its message metadata, its receipts
// root and the body of every secp message (to recompute eth hashes), all
// serially through the retrying network block getter. The in-memory map
// was lost on restart, so a tx sent before a restart and included ~150
// epochs back cost thousands of serial network block fetches (minutes),
// and a hash not on chain (Curio polls pending txs every few seconds)
// walked the whole 2000-epoch lookback every poll.
//
// After: a head-following indexer records, for every tipset adopted as
// head, ethTxHash -> (inclusion height, receipt index, msg CID, logIndex
// base, compact receipt) in the header store's BadgerDB (chain/ethtxindex),
// for the last Window epochs, pruning older heights. A background
// backfill fills the window (newest first, bounded concurrency) and the
// gap left by downtime. Lookups are one KV read plus the few blocks needed
// to render the response; a miss inside a fully indexed window returns
// null immediately (Lotus semantics), or goes to the VM bridge when one is
// configured.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/ethtxindex"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/amt"
	"github.com/Reiers/lantern/state/hamt"
)

const (
	// ethIdxFollowMax bounds one head-follow walk; ethIdxFollowIndex bounds
	// how many of the newest unindexed tipsets the follower indexes itself
	// per head. Anything deeper is left to the (concurrent) backfill loop,
	// so a long catch-up never delays indexing of the next head.
	ethIdxFollowMax   = 256
	ethIdxFollowIndex = 16
	// ethIdxBackfillEvery is the backfill / prune cadence once the window
	// is covered.
	ethIdxBackfillEvery = 2 * time.Minute
	// ethIdxFetchConc bounds concurrent block fetches inside one tipset.
	ethIdxFetchConc = 8
	// ethIdxBackfillWorkers bounds tipsets indexed concurrently by backfill.
	ethIdxBackfillWorkers = 4
)

// EthTxIndexWindowFromEnv returns LANTERN_ETH_TX_INDEX_WINDOW when set to
// an integer (0 or negative disables the index), else def.
func EthTxIndexWindowFromEnv(def abi.ChainEpoch) abi.ChainEpoch {
	if v := strings.TrimSpace(os.Getenv("LANTERN_ETH_TX_INDEX_WINDOW")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return abi.ChainEpoch(n)
		}
	}
	return def
}

// EthTxIndexOptions configures StartEthTxIndex.
type EthTxIndexOptions struct {
	// Window is the retention in epochs (<= 0: ethtxindex.DefaultWindow).
	Window abi.ChainEpoch
	// CacheBlocks lets the indexer write fetched blocks into the node's
	// block cache. Set it only when that cache is bounded (the persistent
	// PDP / Full tier cache); on the light tier's unbounded in-memory
	// cache the indexer reads around the cache instead.
	CacheBlocks bool
	// Workers bounds tipsets indexed concurrently by the backfill
	// (<= 0: default).
	Workers int
	// NoBackfill disables the startup / periodic backfill (tests).
	NoBackfill bool
}

// ethTxIndexer owns the persistent index and its follow/backfill loops.
type ethTxIndexer struct {
	c    *ChainAPI
	ix   *ethtxindex.Index
	opts EthTxIndexOptions
	bg   hamt.BlockGetter // indexer reads (maybe cache-bypassing, retrying)
	// hdrBG reads block headers the header store lacks (cached + retrying;
	// headers are small and persisted into the header store anyway).
	hdrBG hamt.BlockGetter

	headCh chan struct{}
	backCh chan struct{}

	failMu sync.Mutex
	fails  map[abi.ChainEpoch]ethIdxFail

	indexed   atomic.Uint64
	failures  atomic.Uint64
	follows   atomic.Uint64 // completed head-follow passes
	lastWarn  atomic.Int64  // unix nanos of the last failure WARN (rate limit)
	lastErr   atomic.Value  // string
	lastIdxMs atomic.Int64
}

type ethIdxFail struct {
	n    int
	next time.Time
}

// noCacheGetter is implemented by net/combined.Fetcher.
type noCacheGetter interface {
	GetNoCache(ctx context.Context, c cid.Cid) ([]byte, error)
}

type noCacheAdapter struct{ g noCacheGetter }

func (a noCacheAdapter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	return a.g.GetNoCache(ctx, c)
}

// EthTxIndex returns the persistent eth tx index (nil when not started).
func (c *ChainAPI) EthTxIndex() *ethtxindex.Index {
	if ei := c.ethIdx(); ei != nil {
		return ei.ix
	}
	return nil
}

func (c *ChainAPI) ethIdx() *ethTxIndexer {
	c.ethIdxMu.RLock()
	defer c.ethIdxMu.RUnlock()
	return c.ethIdxr
}

// StartEthTxIndex opens the persistent eth tx index in the header store
// and starts the head-following indexer and the backfill loop (both stop
// when ctx is cancelled). Requires HeaderStore and BlockGetter.
func (c *ChainAPI) StartEthTxIndex(ctx context.Context, opts EthTxIndexOptions) error {
	if c.HeaderStore == nil || c.BlockGetter == nil {
		return errors.New("eth tx index: header store / block getter not configured")
	}
	if opts.Window <= 0 {
		opts.Window = ethtxindex.DefaultWindow
	}
	if opts.Workers <= 0 {
		opts.Workers = ethIdxBackfillWorkers
	}
	ix, err := ethtxindex.Open(c.HeaderStore, opts.Window)
	if err != nil {
		return err
	}
	var inner hamt.BlockGetter = c.BlockGetter
	if nc, ok := c.BlockGetter.(noCacheGetter); ok && !opts.CacheBlocks {
		inner = noCacheAdapter{nc}
	}
	ei := &ethTxIndexer{
		c:      c,
		ix:     ix,
		opts:   opts,
		bg:     newRetryingBlockGetter(inner, 2, 30*time.Second),
		hdrBG:  newRetryingBlockGetter(c.BlockGetter, 2, 30*time.Second),
		headCh: make(chan struct{}, 1),
		backCh: make(chan struct{}, 1),
		fails:  map[abi.ChainEpoch]ethIdxFail{},
	}
	c.ethIdxMu.Lock()
	c.ethIdxr = ei
	c.ethIdxMu.Unlock()
	c.HeaderStore.OnHeadChange(func(*types.TipSet) { ei.kickHead() })
	lo, hi, _ := ix.Range()
	log.Infow("eth tx index opened", "window", opts.Window, "heights", ix.Count(), "lo", lo, "hi", hi, "cacheBlocks", opts.CacheBlocks)
	go ei.followLoop(ctx)
	if !opts.NoBackfill {
		go ei.backfillLoop(ctx)
	}
	ei.kickHead() // catch up on the downtime gap without waiting for a head
	return nil
}

func (ei *ethTxIndexer) kickHead() {
	select {
	case ei.headCh <- struct{}{}:
	default:
	}
}

func (ei *ethTxIndexer) kickBackfill() {
	select {
	case ei.backCh <- struct{}{}:
	default:
	}
}

// EthTxIndexStats is a diagnostics snapshot.
type EthTxIndexStats struct {
	Window      abi.ChainEpoch
	Heights     int
	Lo, Hi      abi.ChainEpoch
	Covered     bool
	Indexed     uint64
	Failures    uint64
	LastError   string
	LastIndexMs int64
}

// EthTxIndexStats returns index diagnostics (zero value when not started).
func (c *ChainAPI) EthTxIndexStats() EthTxIndexStats {
	ei := c.ethIdx()
	if ei == nil {
		return EthTxIndexStats{}
	}
	st := EthTxIndexStats{Window: ei.opts.Window, Heights: ei.ix.Count(), Indexed: ei.indexed.Load(), Failures: ei.failures.Load(), LastIndexMs: ei.lastIdxMs.Load()}
	st.Lo, st.Hi, _ = ei.ix.Range()
	if s, ok := ei.lastErr.Load().(string); ok {
		st.LastError = s
	}
	if head := c.HeaderStore.Head(); head != nil {
		st.Covered = ei.ix.Covered(ei.low(head), head.Height()-1)
	}
	return st
}

func (ei *ethTxIndexer) low(head *types.TipSet) abi.ChainEpoch {
	lo := head.Height() - ei.opts.Window
	if lo < 0 {
		lo = 0
	}
	return lo
}

// tsPair is an inclusion tipset and the child that executed it.
type tsPair struct {
	exec, incl *types.TipSet
}

func (p tsPair) nulls() []abi.ChainEpoch {
	var out []abi.ChainEpoch
	for h := p.incl.Height() + 1; h < p.exec.Height(); h++ {
		out = append(out, h)
	}
	return out
}

func (ei *ethTxIndexer) pairIndexed(p tsPair) bool {
	if !ei.ix.Has(p.incl.Height(), p.incl.Key()) {
		return false
	}
	for _, n := range p.nulls() {
		if !ei.ix.Has(n, types.TipSetKey{}) {
			return false
		}
	}
	return true
}

// walk visits canonical (exec, incl) pairs from head down to the window
// floor, newest first, calling visit for each pair that is not indexed yet.
// stopAtIndexed ends the walk at the first pair that is already indexed
// (head-follow mode); otherwise indexed pairs are skipped and the walk
// continues (backfill mode). max bounds the number of pairs visited (<= 0:
// no bound). visit returning false stops the walk.
func (ei *ethTxIndexer) walk(ctx context.Context, stopAtIndexed bool, max int, visit func(tsPair) bool) (truncated bool, err error) {
	head := ei.c.HeaderStore.Head()
	if head == nil {
		return false, nil
	}
	low := ei.low(head)
	exec := head
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if max > 0 && n >= max {
			return true, nil
		}
		pk := exec.Parents()
		if pk.IsEmpty() {
			return false, nil
		}
		incl, err := ei.loadTipSet(ctx, pk)
		if err != nil {
			return false, fmt.Errorf("load parent tipset of %d: %w", exec.Height(), err)
		}
		if incl.Height() < low {
			return false, nil
		}
		p := tsPair{exec: exec, incl: incl}
		if ei.pairIndexed(p) {
			if stopAtIndexed {
				return false, nil
			}
		} else if !visit(p) {
			return false, nil
		}
		exec = incl
	}
}

// loadTipSet rebuilds a tipset from its key: header store first, missing
// headers fetched concurrently by CID (verified) and persisted into the
// header store, so later walks over the same range stay local (a fresh
// node's store starts at its boot anchor, below which every backfill walk
// would otherwise re-fetch every header).
func (ei *ethTxIndexer) loadTipSet(ctx context.Context, tsk types.TipSetKey) (*types.TipSet, error) {
	hs := ei.c.HeaderStore
	if ts, err := hs.GetTipSet(tsk); err == nil && ts != nil {
		return ts, nil
	}
	cids := tsk.Cids()
	if len(cids) == 0 {
		return nil, errors.New("empty tipset key")
	}
	blks := make([]*types.BlockHeader, len(cids))
	err := forEachConc(ctx, len(cids), len(cids), func(i int) error {
		bh, err := ei.c.loadHeader(ctx, ei.hdrBG, cids[i])
		if err != nil {
			return err
		}
		blks[i] = bh
		return nil
	})
	if err != nil {
		return nil, err
	}
	ts, err := types.NewTipSet(blks)
	if err != nil {
		return nil, err
	}
	if err := hs.PutTipSet(ts); err != nil {
		log.Debugw("eth tx index: persist walked tipset", "height", ts.Height(), "err", err)
	}
	return ts, nil
}

func (ei *ethTxIndexer) followLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ei.headCh:
		}
		var todo []tsPair
		truncated, err := ei.walk(ctx, true, ethIdxFollowMax, func(p tsPair) bool {
			todo = append(todo, p)
			return true
		})
		if err != nil && ctx.Err() == nil {
			ei.noteErr(err)
		}
		if len(todo) > ethIdxFollowIndex {
			todo = todo[:ethIdxFollowIndex] // newest first; backfill takes the rest
			truncated = true
		}
		if truncated || err != nil {
			ei.kickBackfill()
		}
		for _, p := range todo {
			if ctx.Err() != nil {
				return
			}
			_ = ei.indexPairLogged(ctx, p)
		}
		ei.prune(false)
		ei.follows.Add(1)
	}
}

func (ei *ethTxIndexer) backfillLoop(ctx context.Context) {
	t := time.NewTicker(ethIdxBackfillEvery)
	defer t.Stop()
	for {
		ei.backfillOnce(ctx)
		ei.prune(true)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-ei.backCh:
		}
	}
}

func (ei *ethTxIndexer) backfillOnce(ctx context.Context) {
	head := ei.c.HeaderStore.Head()
	if head == nil {
		return
	}
	if ei.ix.Covered(ei.low(head), head.Height()-1) {
		return
	}
	start := time.Now()
	// Pipeline: the walker streams unindexed pairs (newest first) to a
	// bounded worker pool, so indexing starts with the first missing
	// tipset instead of after a full-window walk.
	jobs := make(chan tsPair, ei.opts.Workers)
	var wg sync.WaitGroup
	var ok, bad, skipped atomic.Int64
	for w := 0; w < ei.opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ei.pairIndexed(p) { // the follower got there first
					continue
				}
				if err := ei.indexPairLogged(ctx, p); err != nil {
					bad.Add(1)
				} else if n := ok.Add(1); n%50 == 0 {
					log.Infow("eth tx index backfill progress", "indexed", n, "height", p.incl.Height(), "heights", ei.ix.Count(), "elapsed", time.Since(start).Round(time.Second))
				}
			}
		}()
	}
	_, err := ei.walk(ctx, false, 0, func(p tsPair) bool {
		ei.failMu.Lock()
		f, failed := ei.fails[p.incl.Height()]
		ei.failMu.Unlock()
		if failed && time.Now().Before(f.next) {
			skipped.Add(1)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case jobs <- p:
			return true
		}
	})
	close(jobs)
	wg.Wait()
	if err != nil && ctx.Err() == nil {
		ei.noteErr(fmt.Errorf("backfill walk: %w", err))
	}
	if ok.Load()+bad.Load() > 0 || err != nil {
		h := ei.c.HeaderStore.Head()
		covered := h != nil && ei.ix.Covered(ei.low(h), h.Height()-1)
		log.Infow("eth tx index backfill pass", "indexed", ok.Load(), "failed", bad.Load(), "backoff", skipped.Load(), "took", time.Since(start).Round(time.Millisecond), "heights", ei.ix.Count(), "covered", covered, "walkErr", err)
	}
}

// prune drops heights below the window (and, with sent=true, stale
// locally-published tx records).
func (ei *ethTxIndexer) prune(sent bool) {
	head := ei.c.HeaderStore.Head()
	if head == nil {
		return
	}
	low := ei.low(head)
	if n, err := ei.ix.Prune(low); err != nil {
		ei.noteErr(fmt.Errorf("prune: %w", err))
	} else if n > 0 {
		log.Debugw("eth tx index pruned", "heights", n, "below", low)
	}
	if sent {
		if _, err := ei.ix.PruneSent(low-sentTxSearchSlack, maxSentTxTracked); err != nil {
			ei.noteErr(fmt.Errorf("prune sent: %w", err))
		}
	}
	ei.failMu.Lock()
	for h := range ei.fails {
		if h < low {
			delete(ei.fails, h)
		}
	}
	ei.failMu.Unlock()
}

func (ei *ethTxIndexer) noteErr(err error) {
	ei.lastErr.Store(err.Error())
	log.Debugw("eth tx index", "err", err)
}

func (ei *ethTxIndexer) indexPairLogged(ctx context.Context, p tsPair) error {
	start := time.Now()
	err := ei.indexPair(ctx, p)
	h := p.incl.Height()
	ei.failMu.Lock()
	if err != nil {
		f := ei.fails[h]
		f.n++
		backoff := time.Duration(f.n*f.n) * 15 * time.Second
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		f.next = time.Now().Add(backoff)
		ei.fails[h] = f
	} else {
		delete(ei.fails, h)
	}
	ei.failMu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			n := ei.failures.Add(1)
			ei.noteErr(fmt.Errorf("index tipset %d: %w", h, err))
			if last := ei.lastWarn.Load(); time.Since(time.Unix(0, last)) > time.Minute && ei.lastWarn.CompareAndSwap(last, time.Now().UnixNano()) {
				log.Warnw("eth tx index: tipset indexing failed (will retry)", "height", h, "failuresTotal", n, "err", err)
			}
		}
		return err
	}
	ei.indexed.Add(1)
	ei.lastIdxMs.Store(time.Since(start).Milliseconds())
	return nil
}

// indexPair indexes every message of p.incl against p.exec's receipts.
func (ei *ethTxIndexer) indexPair(ctx context.Context, p tsPair) error {
	c := ei.c
	bg := ei.bg
	ix, err := c.buildTipsetMsgIndex(ctx, bg, p.incl, p.exec)
	if err != nil {
		return err
	}
	// Prefetch secp bodies concurrently; ensureEthHashes then decodes
	// them in order. With a cache-bypassing getter the bodies are handed
	// over through a local map instead of the node cache.
	var secp []cid.Cid
	for i, mc := range ix.msgs {
		if ix.isSecp[i] {
			secp = append(secp, mc)
		}
	}
	local, err := fetchAll(ctx, bg, secp, ethIdxFetchConc)
	if err != nil {
		return err
	}
	lbg := &overlayGetter{m: local, inner: bg}
	if left := c.ensureEthHashes(ctx, lbg, ix); left > 0 {
		return fmt.Errorf("tipset %d: %d eth tx hashes unresolved", p.incl.Height(), left)
	}

	// Receipts: compact copies + per-receipt event counts (logIndex base).
	recRoot := p.exec.Blocks()[0].ParentMessageReceipts
	receipts := make([]types.MessageReceipt, 0, len(ix.msgs))
	if err := amt.ForEachV2Raw(ctx, recRoot, bg, func(i uint64, raw []byte) error {
		var r types.MessageReceipt
		if err := r.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return fmt.Errorf("decode receipt %d: %w", i, err)
		}
		receipts = append(receipts, r)
		return nil
	}); err != nil {
		return fmt.Errorf("receipts: %w", err)
	}
	if len(receipts) != len(ix.msgs) {
		return fmt.Errorf("tipset %d: %d receipts vs %d messages", p.incl.Height(), len(receipts), len(ix.msgs))
	}
	counts := make([]uint64, len(receipts))
	emitters := map[abi.ActorID]struct{}{}
	var emMu sync.Mutex
	if err := forEachConc(ctx, len(receipts), ethIdxFetchConc, func(i int) error {
		er := receipts[i].EventsRoot
		if er == nil || !er.Defined() {
			return nil
		}
		var n uint64
		_, err := amt.ForEachRaw(ctx, *er, bg, &amt.LookupOptions{BitWidth: types.EventAMTBitwidth}, func(_ uint64, val []byte) error {
			n++
			var ev types.Event
			if err := ev.UnmarshalCBOR(bytes.NewReader(val)); err == nil {
				emMu.Lock()
				emitters[ev.Emitter] = struct{}{}
				emMu.Unlock()
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("events of receipt %d: %w", i, err)
		}
		counts[i] = n
		return nil
	}); err != nil {
		return err
	}
	bases := make([]uint64, len(receipts))
	var run uint64
	for i, n := range counts {
		bases[i] = run
		run += n
	}

	locs := make(map[[32]byte]ethtxindex.Loc, len(ix.ethPos))
	for h, i := range ix.ethPos {
		r := receipts[i]
		l := ethtxindex.Loc{
			Height:  p.incl.Height(),
			Idx:     uint32(i),
			IsSecp:  ix.isSecp[i],
			MsgCID:  ix.msgs[i],
			Receipt: ethtxindex.CompactReceipt(int64(r.ExitCode), r.GasUsed, r.Return, r.EventsRoot),
		}
		if bases[i] <= uint64(^uint32(0)) {
			l.EvBase, l.HasEvBase = uint32(bases[i]), true
		}
		locs[h] = l
	}
	rec := ethtxindex.HeightRecord{Height: p.incl.Height(), Incl: p.incl.Key(), Exec: p.exec.Key()}
	if err := ei.ix.Apply(rec, locs, p.nulls()); err != nil {
		return fmt.Errorf("persist: %w", err)
	}

	// Resolve event emitters to eth addresses now (state reads at the exec
	// tipset), so the first receipt render after a restart needs none.
	if len(emitters) > 0 {
		acc := c.execAccessor(p.exec, bg)
		ids := make([]abi.ActorID, 0, len(emitters))
		for id := range emitters {
			if _, ok := ei.ix.Emitter(id); !ok {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if _, err := c.emitterEthAddr(ctx, acc, id); err != nil {
				log.Debugw("eth tx index: emitter resolve", "actor", id, "err", err)
			}
		}
	}
	return nil
}

// overlayGetter serves blocks from m first, then inner.
type overlayGetter struct {
	m     map[cid.Cid][]byte
	inner hamt.BlockGetter
}

func (o *overlayGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if b, ok := o.m[c]; ok {
		return b, nil
	}
	return o.inner.Get(ctx, c)
}

// fetchAll fetches cids with bounded concurrency.
func fetchAll(ctx context.Context, bg hamt.BlockGetter, cids []cid.Cid, conc int) (map[cid.Cid][]byte, error) {
	out := make(map[cid.Cid][]byte, len(cids))
	var mu sync.Mutex
	err := forEachConc(ctx, len(cids), conc, func(i int) error {
		b, err := bg.Get(ctx, cids[i])
		if err != nil {
			return fmt.Errorf("fetch %s: %w", cids[i], err)
		}
		mu.Lock()
		out[cids[i]] = b
		mu.Unlock()
		return nil
	})
	return out, err
}

// forEachConc runs fn(0..n-1) with at most conc in flight; first error wins.
func forEachConc(ctx context.Context, n, conc int, fn func(i int) error) error {
	if n == 0 {
		return nil
	}
	if conc <= 0 {
		conc = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	sem := make(chan struct{}, conc)
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				once.Do(func() { firstErr = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// --- lookup ---------------------------------------------------------------

// errEthIdxMiss: the index answered "not here" (caller decides null vs
// bridge from coverage).
var errEthIdxMiss = errors.New("eth tx index miss")

// indexedFindEthTx resolves hash through the persistent index. Returns
// (hit, true, nil) on a hit, (nil, true, errEthTxNotFound) when the index
// authoritatively covers the search range and the hash is not in it,
// (nil, true, errEthIdxMiss) when it is not in the index and coverage is
// incomplete, and (nil, false, nil) when the index is not running.
func (c *ChainAPI) indexedFindEthTx(ctx context.Context, bg hamt.BlockGetter, hash [32]byte, minEpoch abi.ChainEpoch) (*ethTxHit, bool, error) {
	ei := c.ethIdx()
	if ei == nil {
		return nil, false, nil
	}
	loc, rec, ok, err := ei.ix.Lookup(hash)
	if err != nil {
		return nil, true, fmt.Errorf("eth tx index read: %w", err)
	}
	if !ok {
		head := c.HeaderStore.Head()
		if head == nil {
			return nil, true, errEthIdxMiss
		}
		lo := ei.low(head)
		if minEpoch > lo {
			lo = minEpoch
		}
		if ei.ix.Covered(lo, head.Height()-1) {
			return nil, true, errEthTxNotFound
		}
		return nil, true, errEthIdxMiss
	}
	// Render inputs: start the block reads we know we will need while the
	// tipsets load (warms the node cache, so the render's own reads overlap
	// instead of running back to back; errors surface on the real read).
	prefetch := func(k cid.Cid) {
		go func() { _, _ = bg.Get(ctx, k) }()
	}
	prefetch(loc.MsgCID)
	if loc.Receipt != nil && loc.Receipt.EventsRoot.Defined() {
		prefetch(loc.Receipt.EventsRoot)
	}

	incl, err := c.loadFullTipSet(ctx, bg, rec.Incl)
	if err != nil {
		return nil, true, fmt.Errorf("load inclusion tipset %d: %w", rec.Height, err)
	}
	exec, err := c.loadFullTipSet(ctx, bg, rec.Exec)
	if err != nil {
		return nil, true, fmt.Errorf("load exec tipset of %d: %w", rec.Height, err)
	}
	hit := &ethTxHit{incl: incl, exec: exec, idx: int(loc.Idx), msgCID: loc.MsgCID, isSecp: loc.IsSecp}
	if loc.HasEvBase {
		b := uint64(loc.EvBase)
		hit.evBase = &b
	}
	if r := loc.Receipt; r != nil {
		hit.receipt = types.MessageReceipt{ExitCode: exitCodeOf(r.ExitCode), Return: r.Return, GasUsed: r.GasUsed}
		if r.EventsRoot.Defined() {
			er := r.EventsRoot
			hit.receipt.EventsRoot = &er
		}
	} else {
		rraw, _, err := amt.LookupV2(ctx, exec.Blocks()[0].ParentMessageReceipts, uint64(loc.Idx), bg)
		if err != nil {
			return nil, true, fmt.Errorf("receipt %d at %d: %w", loc.Idx, exec.Height(), err)
		}
		if err := hit.receipt.UnmarshalCBOR(bytes.NewReader(rraw)); err != nil {
			return nil, true, fmt.Errorf("decode receipt: %w", err)
		}
	}
	return hit, true, nil
}

// persistSentTx records a locally published tx in the persistent index.
func (c *ChainAPI) persistSentTx(hashHex string, rec sentTxRecord, raw []byte) {
	ei := c.ethIdx()
	if ei == nil {
		return
	}
	h, ok := parseEthHash(hashHex)
	if !ok {
		return
	}
	if err := ei.ix.PutSent(h, ethtxindex.Sent{MsgCID: rec.msgCID, From: rec.from, SentEpoch: rec.sentEpoch, Raw: raw}); err != nil {
		log.Warnw("eth tx index: persist sent tx", "ethHash", hashHex, "err", err)
	}
}

// loadSentTx restores a locally published tx record persisted before a
// restart.
func (c *ChainAPI) loadSentTx(hashHex string) (sentTxRecord, bool) {
	ei := c.ethIdx()
	if ei == nil {
		return sentTxRecord{}, false
	}
	h, ok := parseEthHash(hashHex)
	if !ok {
		return sentTxRecord{}, false
	}
	s, ok, err := ei.ix.GetSent(h)
	if err != nil || !ok {
		return sentTxRecord{}, false
	}
	rec := sentTxRecord{msgCID: s.MsgCID, from: s.From, sentEpoch: s.SentEpoch}
	if len(s.Raw) > 0 {
		if tx, err := parseSignedEthTx(s.Raw); err == nil {
			rec.tx = tx
		}
	}
	return rec, true
}

func exitCodeOf(v int64) exitcode.ExitCode { return exitcode.ExitCode(v) }
