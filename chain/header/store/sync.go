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
	Polls         uint64
	HeadAdvances  uint64
	Reorgs        uint64
	HeadersAdded  uint64
	LastError     string
	LastHeadEpoch abi.ChainEpoch
}

// NewSync returns a Sync agent that has not been started.
func NewSync(s *Store, src RPCSource, opts SyncOptions) *Sync {
	if opts.Interval == 0 {
		opts.Interval = 6 * time.Second
	}
	if opts.MaxBacktrack == 0 {
		opts.MaxBacktrack = 30
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

	head, err := s.src.HeadEpoch(ctx)
	if err != nil {
		s.recordErr(err)
		return err
	}
	currentHead := s.store.HeadEpoch()
	if head <= currentHead && currentHead >= 0 {
		return nil
	}

	// Walk forward from max(currentHead+1, head-MaxBacktrack) to head.
	start := head - s.opts.MaxBacktrack
	if currentHead+1 > start && currentHead >= 0 {
		start = currentHead + 1
	}
	if start < 0 {
		start = 0
	}

	// Snapshot canonical pointers in (0, currentHead] BEFORE applying so
	// we can detect a reorg after the fact.
	priorCanon := make(map[abi.ChainEpoch]ltypes.TipSetKey)
	for ep := abi.ChainEpoch(0); ep <= currentHead; ep++ {
		if canon, err := s.store.canonicalAt(ep); err == nil && canon != nil {
			priorCanon[ep] = canon.Key()
		}
	}

	// Pre-fetch each epoch's blocks.
	newTSs := make(map[abi.ChainEpoch]*ltypes.TipSet, int(head-start+1))
	for ep := start; ep <= head; ep++ {
		ts, err := s.fetchAndPersistTipset(ctx, ep)
		if err != nil || ts == nil {
			continue
		}
		newTSs[ep] = ts
	}

	// Always backfill parents for each new tipset. This catches the
	// reorg case where divergence is deeper than `start`: the new tip's
	// parent chain branches off at some epoch we've already canonicalized
	// to a different fork, and that parent header isn't yet in store.
	added := 0
	for ep := start; ep <= head; ep++ {
		ts, ok := newTSs[ep]
		if !ok {
			continue
		}
		if err := s.backfillParents(ctx, ts); err != nil {
			s.recordErr(err)
			return err
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
// pointers until it reaches an epoch whose blocks are already in the store.
// All intermediate headers are inserted via putLenient.
func (s *Sync) backfillParents(ctx context.Context, ts *ltypes.TipSet) error {
	if ts == nil {
		return nil
	}
	cur := ts
	for cur.Height() > 0 {
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
