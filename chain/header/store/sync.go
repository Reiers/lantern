// Background head-sync agent for the persistent header store.
//
// The Sync agent talks to a Lotus-compatible JSON-RPC source. It polls
// ChainHead at a configurable interval (default 6s, half of Filecoin's 30s
// block time), and on every new head epoch:
//
//  1. Walks back from the new head to the most-recent epoch we already
//     have canonical for (or HeadEpoch - MaxBacktrack, whichever is closer)
//     fetching block headers along the way.
//  2. For each fetched header, verifies the block CID and parent linkage.
//  3. Calls Store.SetHead(newHead), which rewrites canonical pointers and
//     fires OnHeadChange listeners. SetHead itself detects parent-CID
//     mismatch against the prior canonical chain and acts as the reorg
//     trigger.
//
// We do not use a gossipsub or libp2p ChainNotify channel here — that's the
// job of net/libp2p. The Sync agent is the simple "poll for ChainHead"
// fallback that works against any Lotus-compatible RPC.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/header"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// RPCSource is the minimal RPC surface required by Sync. The Lantern glif
// client and gateway client both satisfy this interface.
type RPCSource interface {
	// HeadEpoch returns the current chain head epoch.
	HeadEpoch(ctx context.Context) (abi.ChainEpoch, error)
	// TipsetCIDsByHeight returns the block CIDs that form the canonical
	// tipset at the given epoch.
	TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error)
	// FetchBlock returns a single BlockHeader, CID-verified.
	FetchBlock(ctx context.Context, k cid.Cid) (*ltypes.BlockHeader, error)
}

// SyncOptions configures a Sync agent.
type SyncOptions struct {
	// Interval between ChainHead polls. Default 6s.
	Interval time.Duration
	// MaxBacktrack caps how far back the agent walks on each poll
	// (defends against unbounded catch-up cost). Default 30.
	MaxBacktrack abi.ChainEpoch
	// BootstrapDepth is how many tipsets to ingest on the very first
	// poll when the store is empty. Defaults to MaxBacktrack. Operators
	// running against rate-limited public RPC providers (Glif) should
	// set this small (1–3) so startup is quick; subsequent polls catch
	// up incrementally.
	BootstrapDepth abi.ChainEpoch
	// CatchUpChunk caps how many epochs the warm-store catch-up advances
	// in a single poll. When the store falls behind by more than this,
	// each poll ingests a contiguous chunk of this size starting from
	// currentHead+1, so lag decreases monotonically without skipping
	// epochs (issue #33). 0 means "no cap" (advance straight to head).
	// Default 200 — large enough that normal operation (0–2 epochs
	// behind) is a single chunk, bounded enough that a deep backlog
	// against rate-limited RPC is paced over several polls.
	CatchUpChunk abi.ChainEpoch
	// GossipFresh, when set, reports whether an external head source
	// (the gossipsub block ingestor) has advanced the store's head
	// recently — i.e. within roughly the last poll Interval. When it
	// returns true the Sync skips its own RPCSource.HeadEpoch() poll for
	// that tick, because gossip is already supplying the live head with
	// no upstream-RPC dependency. The Sync then acts purely as a
	// catch-up fallback: it resumes polling the RPCSource only when
	// gossip goes quiet (stale store head). This is what keeps a node
	// with healthy gossipsub from hammering a rate-limited public RPC
	// (Glif 429) every Interval (#71). Nil disables the optimization
	// (the Sync polls every tick, the pre-#71 behavior).
	GossipFresh func() bool

	// GossipObservedHead, when set, reports the highest chain epoch the
	// gossip ingestor has observed/installed - i.e. the gossip layer's
	// view of the live tip (>= the canonical store head). It makes the
	// #71 GossipFresh skip LAG-AWARE (#83): GossipFresh alone only tells
	// us the head moved recently, not that it reached the tip. A node
	// whose gossip is fresh-but-lagging (installing some blocks while
	// skipping head+N>1 blocks it can't backfill) would otherwise have
	// Sync skip its catch-up poll forever, wedging the head ~10-20 epochs
	// behind the tip. With this set, Sync skips only when the store head
	// is within SkipLagTolerance of the observed tip; when further behind
	// it runs the catch-up poll regardless of GossipFresh. Returns a
	// negative epoch when no observation is available (treated as "can't
	// confirm we're at the tip" -> don't skip). Nil keeps the pre-#83
	// behavior (skip purely on GossipFresh).
	GossipObservedHead func() abi.ChainEpoch

	// SkipLagTolerance is how many epochs behind the gossip-observed tip
	// the store head may be and still skip the catch-up poll (#83). Only
	// consulted when GossipObservedHead is set. Default 2 (allows the
	// normal 0-1 epoch gossip latency without forcing an RPC poll).
	SkipLagTolerance abi.ChainEpoch

	// StaleResetThreshold is the lag (live head − store head, in epochs)
	// beyond which a warm store is considered hopelessly stale and is
	// re-bootstrapped near the live head instead of attempting a
	// contiguous backfill.
	//
	// This is the #51 "down for a week" fix. The contiguous catch-up
	// (issue #33) walks from currentHead+1 and relies on backfillParents
	// connecting each new tipset to the store's existing chain within
	// MaxBacktrack epochs. After a multi-day outage the gap is tens of
	// thousands of epochs — far beyond MaxBacktrack — so backfill never
	// connects, SetHead never fires, and the head pointer freezes at the
	// stale epoch (exactly the symptom reported 2026-06-18). Past this
	// threshold we stop trying to bridge the gap and re-anchor near live
	// head, the same way a cold start does. No keys, wallets, or other
	// secrets are touched — only rebuildable chain state.
	//
	// 0 disables the behaviour (pure contiguous catch-up, legacy).
	// Default 2880 (~1 day at 30s epochs): a node behind by more than a
	// day re-anchors instead of grinding.
	StaleResetThreshold abi.ChainEpoch
	// OnStaleReset, when set, is fired once when a stale warm store is
	// re-anchored, with (storeHead, liveHead). Used for logging/metrics.
	OnStaleReset func(storeHead, liveHead abi.ChainEpoch)
	// OnReorg is fired (after Store.SetHead) when the new head's parent
	// chain replaced canonical pointers at one or more epochs. The
	// argument is the divergence epoch (the deepest epoch whose
	// canonical pointer changed).
	OnReorg func(divergence abi.ChainEpoch)
}

// Sync polls an RPCSource and feeds new heads into a Store.
type Sync struct {
	store  *Store
	src    RPCSource
	opts   SyncOptions
	cancel context.CancelFunc

	mu      sync.Mutex
	running bool
	stats   SyncStats
}

// SyncStats reports observable Sync activity.
type SyncStats struct {
	Polls              uint64
	SkippedGossipFresh uint64 // #71: ticks where the Glif poll was skipped because gossip was fresh
	HeadAdvances       uint64
	Reorgs             uint64
	StaleResets        uint64
	HeadersAdded       uint64
	LastError          string
	LastHeadEpoch      abi.ChainEpoch
}

// NewSync returns a Sync agent that has not been started.
func NewSync(s *Store, src RPCSource, opts SyncOptions) *Sync {
	if opts.Interval == 0 {
		opts.Interval = 6 * time.Second
	}
	if opts.MaxBacktrack == 0 {
		opts.MaxBacktrack = 30
	}
	if opts.BootstrapDepth == 0 {
		opts.BootstrapDepth = opts.MaxBacktrack
	}
	if opts.CatchUpChunk == 0 {
		opts.CatchUpChunk = 200
	}
	if opts.StaleResetThreshold == 0 {
		opts.StaleResetThreshold = 2880 // ~1 day at 30s epochs
	}
	return &Sync{store: s, src: src, opts: opts}
}

// Stats returns a snapshot of activity counters.
func (s *Sync) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Start launches the polling loop. Returns an error if already started.
func (s *Sync) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("sync: already running")
	}
	s.running = true
	cctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	go s.loop(cctx)
	return nil
}

// SetGossipObservedHead wires the gossip-observed-tip callback after
// construction (#83). Pair with SetGossipFresh to make the #71 skip
// lag-aware. tol is the SkipLagTolerance in epochs; <=0 uses the default.
func (s *Sync) SetGossipObservedHead(fn func() abi.ChainEpoch, tol abi.ChainEpoch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.GossipObservedHead = fn
	if tol > 0 {
		s.opts.SkipLagTolerance = tol
	}
}

// SetGossipFresh wires the gossip-freshness callback after construction.
// The gossipsub ingestor is created after the Sync in both daemon paths,
// so the callback can't be passed to NewSync; this lets the caller wire
// it once the ingestor exists (#71). Safe to call before or after
// Start. Passing nil disables the gossip-aware poll skip.
func (s *Sync) SetGossipFresh(fn func() bool) {
	s.mu.Lock()
	s.opts.GossipFresh = fn
	s.mu.Unlock()
}

// Stop halts the polling loop.
func (s *Sync) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.running = false
}

// PollOnce runs one synchronization cycle. Useful for tests + the Phase 6
// demo where we want a deterministic single-shot sync.
func (s *Sync) PollOnce(ctx context.Context) error {
	return s.pollAndApply(ctx)
}

func (s *Sync) loop(ctx context.Context) {
	t := time.NewTicker(s.opts.Interval)
	defer t.Stop()
	// First poll immediately.
	if err := s.pollAndApply(ctx); err != nil {
		log.Warnf("sync poll: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.pollAndApply(ctx); err != nil {
				log.Warnf("sync poll: %v", err)
			}
		}
	}
}

func (s *Sync) pollAndApply(ctx context.Context) error {
	s.mu.Lock()
	s.stats.Polls++
	s.mu.Unlock()

	// #71: when the gossipsub ingestor is keeping the store head fresh,
	// skip the upstream-RPC HeadEpoch() poll entirely. Gossip installs
	// blocks into this same store via SetHead with 0-1 epoch latency and
	// no Glif call, so polling Glif on every tick is pure redundant load
	// that gets a node rate-limited (429). The Sync stays a catch-up
	// fallback: as soon as gossip goes quiet, GossipFresh() returns false
	// and we resume polling. A cold/empty store (currentHead < 0) always
	// polls so the very first head can be bootstrapped from the RPC.
	s.mu.Lock()
	gossipFresh := s.opts.GossipFresh
	gossipObservedHead := s.opts.GossipObservedHead
	skipLagTol := s.opts.SkipLagTolerance
	s.mu.Unlock()
	if skipLagTol <= 0 {
		skipLagTol = 2
	}
	if gossipFresh != nil && s.store.HeadEpoch() >= 0 && gossipFresh() {
		// #83: the gossip-fresh skip must be lag-aware. GossipFresh only
		// reports that the head moved recently, not that it reached the
		// tip. A node whose gossip installs some blocks while skipping
		// head+N>1 blocks it can't backfill stays fresh-but-lagging; if
		// we skip the catch-up poll here the head wedges ~10-20 epochs
		// behind the tip forever (Beck, v1.8.4-m). Only skip when we can
		// confirm the store head is within SkipLagTolerance of the
		// gossip-observed tip. When GossipObservedHead is unset or can't
		// confirm (negative), fall back to the pre-#83 behavior of
		// trusting GossipFresh, since without a tip estimate the only
		// alternative is paying the RPC HeadEpoch poll #71 exists to
		// avoid.
		atTip := true
		if gossipObservedHead != nil {
			if tip := gossipObservedHead(); tip >= 0 {
				atTip = s.store.HeadEpoch() >= tip-skipLagTol
			}
		}
		if atTip {
			s.mu.Lock()
			s.stats.SkippedGossipFresh++
			s.mu.Unlock()
			return nil
		}
	}

	// #50 part 3: a nil source means no upstream RPC was wired (bridge-off
	// NoFallbackRPC). Gossipsub is the sole head driver in that mode, so
	// the polling Sync has nothing to do - it stays a no-op rather than
	// nil-panicking on HeadEpoch. The store head still advances via the
	// gossip ingestor's SetHead calls.
	if s.src == nil {
		return nil
	}

	head, err := s.src.HeadEpoch(ctx)
	if err != nil {
		s.recordErr(err)
		return err
	}
	currentHead := s.store.HeadEpoch()
	if head <= currentHead && currentHead >= 0 {
		return nil
	}

	// Decide the [start, target] window to ingest this poll.
	//
	// Warm store (currentHead >= 0): always resume CONTIGUOUSLY from
	// currentHead+1. The old code set start = head-MaxBacktrack whenever
	// the store was further behind than MaxBacktrack, which skipped every
	// epoch in between and left an unfillable gap — the head pointer then
	// stalled (issue #33 / curio-core#62, the 70-epoch lag). To bound
	// per-poll cost during a deep catch-up we instead cap how far FORWARD
	// we advance in one poll (CatchUpChunk); subsequent polls continue
	// from where this one stopped, so lag strictly decreases without ever
	// creating a hole.
	//
	// Cold start (currentHead < 0): begin near head using BootstrapDepth
	// so the first poll completes quickly against rate-limited public RPC;
	// later polls catch up incrementally.
	//
	// Stale warm store (#51): if the store is so far behind that a
	// contiguous backfill can't connect (lag > StaleResetThreshold), the
	// per-poll backfillParents walk would hit its MaxBacktrack cap on
	// every new tipset, never link to the store's chain, and the head
	// would freeze (the "down for a week" symptom). Treat it as a cold
	// start: re-anchor near live head with lenient writes. This discards
	// the forward chain-state pointers below the new anchor, which are
	// rebuildable; it never touches keys/wallets/tokens (separate files).
	staleReset := false
	if currentHead >= 0 && s.opts.StaleResetThreshold > 0 &&
		head-currentHead > s.opts.StaleResetThreshold {
		staleReset = true
		if s.opts.OnStaleReset != nil {
			s.opts.OnStaleReset(currentHead, head)
		}
		log.Warnw("sync: warm store too stale to backfill contiguously, re-anchoring near live head",
			"storeHead", currentHead, "liveHead", head, "lag", head-currentHead,
			"threshold", s.opts.StaleResetThreshold)
		s.mu.Lock()
		s.stats.StaleResets++
		s.mu.Unlock()
	}

	var start, target abi.ChainEpoch
	if currentHead < 0 || staleReset {
		start = head - s.opts.BootstrapDepth
		target = head
	} else {
		start = currentHead + 1
		target = head
		if chunk := s.opts.CatchUpChunk; chunk > 0 && target-start+1 > chunk {
			target = start + chunk - 1
		}
	}
	if start < 0 {
		start = 0
	}

	// Snapshot canonical pointers in (0, currentHead] BEFORE applying so
	// we can detect a reorg after the fact. Skipped on a stale reset:
	// there's no meaningful reorg to detect when we're discarding the old
	// chain wholesale, and iterating millions of epochs here would stall
	// the re-anchor poll.
	priorCanon := make(map[abi.ChainEpoch]ltypes.TipSetKey)
	if !staleReset {
		for ep := abi.ChainEpoch(0); ep <= currentHead; ep++ {
			if canon, err := s.store.canonicalAt(ep); err == nil && canon != nil {
				priorCanon[ep] = canon.Key()
			}
		}
	}

	// Pre-fetch each epoch's blocks. A fetch ERROR (transient Glif 5xx /
	// 429) is distinct from a null round (legitimately empty epoch): the
	// former must block head-advance past that epoch so we retry it next
	// poll; the latter is skipped over normally. We track failed epochs
	// explicitly so the apply loop can stop at the first hole instead of
	// advancing the head pointer past missing data (issue #33).
	newTSs := make(map[abi.ChainEpoch]*ltypes.TipSet, int(target-start+1))
	fetchFailed := make(map[abi.ChainEpoch]bool)
	for ep := start; ep <= target; ep++ {
		ts, err := s.fetchAndPersistTipset(ctx, ep)
		if err != nil {
			s.recordErr(fmt.Errorf("fetch epoch %d: %w", ep, err))
			log.Debugw("sync: fetch epoch failed", "epoch", ep, "err", err)
			fetchFailed[ep] = true
			continue
		}
		if ts == nil {
			continue // null round
		}
		newTSs[ep] = ts
	}

	// Apply tipsets in strict epoch order. backfillParents handles the
	// reorg case where divergence is deeper than `start` (the new tip's
	// parent chain branches off at an epoch already canonicalized to a
	// different fork). On a cold start (currentHead < 0) backfill is
	// skipped — the only alternative is walking back to genesis, which
	// would block startup for hours against rate-limited public RPC;
	// subsequent polls catch up incrementally.
	//
	// We advance the head pointer
	// only as far as the chain is CONTIGUOUS. The moment we hit an epoch
	// that failed to fetch, we stop advancing the head and leave the rest
	// for the next poll. This is the core #33 fix: previously the loop
	// skipped failed/missing epochs but kept calling SetHead for later
	// ones, advancing the head pointer past holes (a head that points at
	// data with missing ancestors). Now lag decreases monotonically and
	// the served head is always backed by a gap-free chain.
	//
	// A single backfill error is likewise non-fatal: we record it, stop
	// advancing at that epoch, and retry next poll, instead of aborting
	// the whole cycle (which under sustained rate-limiting meant the head
	// never moved at all).
	added := 0
	skipBackfill := currentHead < 0 || staleReset
	for ep := start; ep <= target; ep++ {
		if fetchFailed[ep] {
			// Hole: stop here. Next poll resumes from this epoch.
			log.Debugw("sync: stopping head-advance at fetch hole", "epoch", ep)
			break
		}
		ts, ok := newTSs[ep]
		if !ok {
			continue // null round: no tipset, but not a hole
		}
		if !skipBackfill {
			if err := s.backfillParents(ctx, ts); err != nil {
				s.recordErr(err)
				log.Debugw("sync: backfill failed, stopping head-advance", "epoch", ep, "err", err)
				break
			}
		}
		if err := s.store.SetHead(ctx, ts); err != nil {
			s.recordErr(err)
			return fmt.Errorf("set head at %d: %w", ep, err)
		}
		added++
	}

	// Compute divergence: deepest epoch where canonical-after differs
	// from canonical-before.
	reorgDivergence := abi.ChainEpoch(-1)
	for ep, prior := range priorCanon {
		now, err := s.store.canonicalAt(ep)
		if err != nil || now == nil {
			continue
		}
		if now.Key() != prior {
			if reorgDivergence < 0 || ep < reorgDivergence {
				reorgDivergence = ep
			}
		}
	}

	s.mu.Lock()
	s.stats.HeadAdvances++
	s.stats.HeadersAdded += uint64(added)
	s.stats.LastHeadEpoch = head
	if reorgDivergence >= 0 {
		s.stats.Reorgs++
	}
	s.mu.Unlock()

	if reorgDivergence >= 0 && s.opts.OnReorg != nil {
		s.opts.OnReorg(reorgDivergence)
	}
	return nil
}

// backfillParents walks back from the given tipset through its parent
// pointers until it reaches an epoch whose blocks are already in the store,
// OR until the depth cap is hit. All intermediate headers are inserted via
// putLenient.
//
// The depth cap (default MaxBacktrack) is critical: without it, the very
// first sync against a fresh store would walk back to genesis (millions
// of epochs). Subsequent polls' backfill is a no-op because the store has
// the relevant parents already.
func (s *Sync) backfillParents(ctx context.Context, ts *ltypes.TipSet) error {
	if ts == nil {
		return nil
	}
	cap := s.opts.MaxBacktrack
	if cap <= 0 {
		cap = 30
	}
	cur := ts
	for depth := abi.ChainEpoch(0); cur.Height() > 0 && depth < cap; depth++ {
		parents := cur.Blocks()[0].Parents
		allPresent := true
		for _, pc := range parents {
			if _, err := s.store.Get(pc); err != nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			return nil
		}
		// Fetch each parent block.
		parentBlocks := make([]*ltypes.BlockHeader, 0, len(parents))
		for _, pc := range parents {
			bh, err := s.src.FetchBlock(ctx, pc)
			if err != nil {
				return fmt.Errorf("backfill parent %s: %w", pc, err)
			}
			if err := s.putLenient(bh); err != nil {
				return err
			}
			parentBlocks = append(parentBlocks, bh)
		}
		pts, err := ltypes.NewTipSet(parentBlocks)
		if err != nil {
			return err
		}
		cur = pts
	}
	return nil
}

// fetchAndPersistTipset fetches every block at epoch ep, verifies each
// header (CID + parent linkage), inserts them into the store, and returns
// the assembled tipset. Returns (nil, nil) for null-round epochs.
func (s *Sync) fetchAndPersistTipset(ctx context.Context, ep abi.ChainEpoch) (*ltypes.TipSet, error) {
	cids, err := s.src.TipsetCIDsByHeight(ctx, ep)
	if err != nil {
		return nil, err
	}
	if len(cids) == 0 {
		return nil, nil
	}
	blocks := make([]*ltypes.BlockHeader, 0, len(cids))
	for _, c := range cids {
		// Use cache first.
		if bh, err := s.store.Get(c); err == nil {
			blocks = append(blocks, bh)
			continue
		}
		bh, err := s.src.FetchBlock(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("fetch block %s: %w", c, err)
		}
		// CID-verify (the RPC source already does this, but defense in
		// depth).
		if err := header.VerifyBlockHeaderCID(bh, c); err != nil {
			return nil, err
		}
		blocks = append(blocks, bh)
	}
	// Phase 1 tipset-shape check (identical Parents, height, etc.)
	if _, err := header.ValidateTipsetShape(blocks); err != nil {
		return nil, err
	}
	// Persist each block. Note: Put requires parents to be present; for
	// blocks far in the past during gap-fill we might be missing
	// grandparents. We tolerate that here because the chain validator
	// upstream already cross-checks via the F3 anchor.
	for _, b := range blocks {
		if err := s.putLenient(b); err != nil {
			return nil, err
		}
	}
	return ltypes.NewTipSet(blocks)
}

// putLenient writes the header bypassing Put's parent-linkage check. We use
// this during gap-fill where parents are intentionally not yet ingested.
func (s *Sync) putLenient(bh *ltypes.BlockHeader) error {
	// Reuse Put when parent linkage is satisfied; otherwise write the raw
	// CID → bytes mapping directly.
	if err := s.store.Put(bh); err == nil {
		return nil
	}
	raw, err := bh.Serialize()
	if err != nil {
		return err
	}
	return s.store.db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(cidKey(bh.Cid()), raw); err != nil {
			return err
		}
		ek := epochKey(prefixEpoch, bh.Height)
		cur, err := getOrEmpty(txn, ek)
		if err != nil {
			return err
		}
		merged, changed := appendCIDIfMissing(cur, bh.Cid())
		if changed {
			if err := txn.Set(ek, merged); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Sync) recordErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.LastError = err.Error()
}
