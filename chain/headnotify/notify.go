// Package headnotify implements Lantern's chain-head change distributor.
//
// It's the bridge between a Store's OnHeadChange callback (which fires on
// every new head) and the per-WebSocket subscriber channels that
// Filecoin.ChainNotify hands out.
//
// Semantics match Lotus:
//
//   - When a subscriber connects, the first message is always
//     {Type:"current", Val: <current head>}.
//   - For every subsequent canonical head, fan out {Type:"apply", Val:
//     <new tipset>}. On reorg, fan out a sequence of {Type:"revert"}
//     events for the orphaned tipsets followed by {Type:"apply"} events
//     for the new chain.
//   - Per-subscriber buffer is bounded (default 64 events). When a
//     subscriber's channel is full we drop the oldest events (matching
//     Lotus' slow-subscriber behaviour) and emit a log warning.
//
// Written for Lantern. Not a direct lift of Lotus' notifee plumbing,
// but the head-change event shape (HeadChange{Type,Val}) is the same
// wire format Curio expects.
package headnotify

import (
	"context"
	"sync"
	"sync/atomic"

	abi "github.com/filecoin-project/go-state-types/abi"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/api"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/headnotify")

// DefaultBufferSize is the per-subscriber bounded buffer depth.
//
// Lotus' default is similar; we pick 64 to absorb ~30s of mainnet
// epochs without dropping under normal load.
const DefaultBufferSize = 64

// Distributor fans out head-change events from a header store to many
// subscribers (one per active WebSocket ChainNotify session).
type Distributor struct {
	store      *hstore.Store
	bufferSize int

	mu          sync.Mutex
	subs        map[uint64]*subscriber
	nextSubID   uint64
	lastHead    *ltypes.TipSet
	startedOnce sync.Once
}

type subscriber struct {
	id uint64
	ch chan []api.HeadChange
	// sendMu serializes deliver() sends against unsubscribe()'s close, so
	// we can never send on a closed channel (a TOCTOU between the atomic
	// closed-check and the send otherwise races close vs chansend; the
	// race detector flagged it and it could panic in production).
	sendMu  sync.Mutex
	closed  atomic.Bool
	dropped atomic.Uint64
}

// New returns a Distributor backed by the given header store. The
// distributor does NOT start listening on the store until Start() is
// called.
func New(store *hstore.Store, bufferSize int) *Distributor {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	return &Distributor{
		store:      store,
		bufferSize: bufferSize,
		subs:       map[uint64]*subscriber{},
	}
}

// Start hooks the distributor into the header store. Idempotent.
func (d *Distributor) Start() {
	d.startedOnce.Do(func() {
		if d.store == nil {
			return
		}
		// Seed lastHead so the first subscriber gets the right "current".
		d.mu.Lock()
		d.lastHead = d.store.Head()
		d.mu.Unlock()
		d.store.OnHeadChange(d.onStoreHead)
		log.Infow("started head-change distributor", "bufferSize", d.bufferSize)
	})
}

// CurrentHead returns the most recent head the distributor has observed,
// or nil if no head has been published yet.
func (d *Distributor) CurrentHead() *ltypes.TipSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastHead
}

// Subscribe returns a new channel that receives head-change events. The
// first event is always {Type:"current"} with the current head (or empty
// if no head is available yet). Cancelling ctx unsubscribes.
//
// The returned channel is closed by the distributor when ctx is done.
func (d *Distributor) Subscribe(ctx context.Context) <-chan []api.HeadChange {
	d.mu.Lock()
	id := d.nextSubID
	d.nextSubID++
	sub := &subscriber{
		id: id,
		ch: make(chan []api.HeadChange, d.bufferSize),
	}
	d.subs[id] = sub
	current := d.lastHead
	d.mu.Unlock()

	// Send "current" synchronously so the client always sees it as the
	// first message after subscribe (matches Lotus). When the store
	// hasn't observed a head yet, send a "current" event with nil Val
	// rather than an empty batch — downstream consumers that pattern-
	// match on the first event's Type (notably curio's chainsched at
	// lib/chainsched/chain_sched.go:162) reject an empty batch with a
	// 'first notification must be current' error and tear down the
	// subscription.
	sub.sendMu.Lock()
	if !sub.closed.Load() {
		sub.ch <- []api.HeadChange{{Type: "current", Val: current}}
	}
	sub.sendMu.Unlock()

	// Detach on ctx cancellation.
	go func() {
		<-ctx.Done()
		d.unsubscribe(id)
	}()
	return sub.ch
}

// unsubscribe removes a subscriber and closes its channel. Safe to call
// multiple times.
func (d *Distributor) unsubscribe(id uint64) {
	d.mu.Lock()
	sub, ok := d.subs[id]
	if !ok {
		d.mu.Unlock()
		return
	}
	delete(d.subs, id)
	d.mu.Unlock()
	// Take the per-subscriber send lock so we don't close the channel
	// while deliver() is mid-send. deliver() re-checks closed under the
	// same lock, so once we set closed=true here no further send happens.
	sub.sendMu.Lock()
	closedNow := sub.closed.CompareAndSwap(false, true)
	if closedNow {
		close(sub.ch)
	}
	sub.sendMu.Unlock()
	if dropped := sub.dropped.Load(); dropped > 0 {
		log.Warnw("subscriber removed with drops", "id", id, "dropped", dropped)
	}
}

// SubscriberCount returns the number of active subscribers.
func (d *Distributor) SubscriberCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.subs)
}

// onStoreHead is the OnHeadChange callback hooked into the header store.
// It computes the apply/revert event sequence against the previously
// observed head and fans it out to every subscriber.
func (d *Distributor) onStoreHead(ts *ltypes.TipSet) {
	if ts == nil {
		return
	}
	d.mu.Lock()
	prev := d.lastHead
	d.lastHead = ts
	subs := make([]*subscriber, 0, len(d.subs))
	for _, s := range d.subs {
		subs = append(subs, s)
	}
	d.mu.Unlock()

	events := buildEvents(prev, ts, d.store)
	if len(events) == 0 {
		return
	}
	for _, sub := range subs {
		d.deliver(sub, events)
	}
}

// PublishCustom lets the daemon push an arbitrary head-change sequence
// (e.g. a synthetic "current" reset). Used during startup if the daemon
// wants to advertise the trusted-root head before the header store has
// caught up.
func (d *Distributor) PublishCustom(events []api.HeadChange) {
	if len(events) == 0 {
		return
	}
	d.mu.Lock()
	subs := make([]*subscriber, 0, len(d.subs))
	for _, s := range d.subs {
		subs = append(subs, s)
	}
	d.mu.Unlock()
	for _, sub := range subs {
		d.deliver(sub, events)
	}
}

// deliver attempts a non-blocking send. If the channel is full, drop the
// oldest event, increment the drop counter, and try again. This matches
// Lotus' slow-subscriber semantics (drop the slow consumer's history
// rather than back-pressure the chain syncer).
func (d *Distributor) deliver(sub *subscriber, events []api.HeadChange) {
	// Hold the per-subscriber send lock for the whole send so unsubscribe()
	// can't close the channel underneath us. Re-check closed under the lock
	// (it may have been closed before we acquired it).
	sub.sendMu.Lock()
	defer sub.sendMu.Unlock()
	if sub.closed.Load() {
		return
	}
	for {
		select {
		case sub.ch <- events:
			return
		default:
			// Drop oldest.
			select {
			case <-sub.ch:
				sub.dropped.Add(1)
				if sub.dropped.Load()%16 == 1 {
					log.Warnw("slow subscriber, dropping oldest event",
						"id", sub.id, "dropped", sub.dropped.Load())
				}
			default:
				// Channel was emptied by the consumer between our two
				// selects. Loop and try again.
			}
		}
	}
}

// buildEvents computes the apply/revert sequence from prev → next.
//
// If prev is nil → emit a single apply for next.
//
// If prev's tipset is the parent of next, or prev is at lower or equal
// height and next.Parents == prev.Key → emit one apply event.
//
// Otherwise compute the revert+apply path via the header store (walking
// parents of both to find the divergence epoch). Reverts come first
// (oldest-first revert order matches Lotus).
//
// If the header store can't reconstruct the path (e.g. prev is no
// longer canonical), we conservatively emit a single apply for next.
// Subscribers can resync via ChainGetTipSetByHeight if they care.
func buildEvents(prev, next *ltypes.TipSet, store *hstore.Store) []api.HeadChange {
	if next == nil {
		return nil
	}
	if prev == nil {
		return []api.HeadChange{{Type: "apply", Val: next}}
	}
	// Fast path: same tipset, skip.
	if prev.Key() == next.Key() {
		return nil
	}
	// Fast path: linear extension (prev is the direct parent tipset of next).
	if next.Height() == prev.Height()+1 {
		if next.Parents() == prev.Key() {
			return []api.HeadChange{{Type: "apply", Val: next}}
		}
	}
	// Multi-epoch linear extension: walk back from next to prev's height
	// and confirm prev is on next's chain.
	if next.Height() > prev.Height() {
		if applies, ok := linearWalk(prev, next, store); ok {
			return applies
		}
	}
	// Reorg path: find divergence.
	return reorgPath(prev, next, store)
}

// linearWalk attempts to walk parents of `next` back to `prev`. If
// successful, returns the apply events (oldest first) between prev (exclusive)
// and next (inclusive). If we can't reconstruct the path (missing parents
// in store) returns ok=false.
func linearWalk(prev, next *ltypes.TipSet, store *hstore.Store) ([]api.HeadChange, bool) {
	if store == nil {
		return nil, false
	}
	chain := []*ltypes.TipSet{next}
	cur := next
	for cur.Height() > prev.Height() {
		parentKey := cur.Parents()
		// Walk to the canonical tipset at the parent height by loading
		// the first parent CID's height.
		parentCIDs := cur.Blocks()[0].Parents
		if len(parentCIDs) == 0 {
			return nil, false
		}
		// Use the store's GetTipSetByHeight at parent height (we
		// expect the canonical to match parentKey for linear walks).
		pts, err := store.GetTipSetByHeight(abi.ChainEpoch(cur.Height() - 1))
		if err != nil || pts == nil {
			return nil, false
		}
		_ = parentKey
		if pts.Height() <= prev.Height() {
			if pts.Key() == prev.Key() {
				// Linear; we're done.
				break
			}
			// Divergent parent below prev's height — not a linear walk.
			return nil, false
		}
		chain = append(chain, pts)
		cur = pts
	}
	// Reverse chain so it's oldest-first (apply order).
	out := make([]api.HeadChange, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		out = append(out, api.HeadChange{Type: "apply", Val: chain[i]})
	}
	return out, true
}

// reorgPath emits revert events for prev's chain back to divergence,
// then apply events for next's chain forward from divergence.
//
// If we can't reconstruct (prev's chain is no longer canonical in the
// store), fall back to a single apply for next. That's defensible
// because the subscriber sees a head movement and can re-fetch state if
// they care about the in-between epochs.
func reorgPath(prev, next *ltypes.TipSet, store *hstore.Store) []api.HeadChange {
	if store == nil {
		return []api.HeadChange{{Type: "apply", Val: next}}
	}
	// Climb prev's chain back epoch-by-epoch until we find a tipset
	// whose canonical-at-that-epoch matches what's on next's path.
	// Practical heuristic: walk both back to common ancestor by epoch,
	// emitting revert events for prev's path along the way.

	// Build prev path backwards.
	prevPath := []*ltypes.TipSet{prev}
	// Build next path backwards as a map by height for O(1) lookup.
	nextByHeight := map[abi.ChainEpoch]*ltypes.TipSet{next.Height(): next}

	curPrev := prev
	curNext := next
	for {
		// Equalise heights — bring the higher one down.
		if curNext.Height() > curPrev.Height() {
			pts, err := store.GetTipSetByHeight(curNext.Height() - 1)
			if err != nil || pts == nil {
				return []api.HeadChange{{Type: "apply", Val: next}}
			}
			curNext = pts
			nextByHeight[curNext.Height()] = curNext
			continue
		}
		if curPrev.Height() > curNext.Height() {
			// Walk prev back via canonical store. (Prev was canonical
			// before; the store may have rewritten it during reorg, in
			// which case this fails.)
			pts, err := store.GetTipSetByHeight(curPrev.Height() - 1)
			if err != nil || pts == nil {
				return []api.HeadChange{{Type: "apply", Val: next}}
			}
			curPrev = pts
			prevPath = append(prevPath, curPrev)
			continue
		}
		// Same height. Common ancestor?
		if curPrev.Key() == curNext.Key() {
			break
		}
		// Otherwise step both back one epoch.
		if curPrev.Height() == 0 {
			break
		}
		pnext, err := store.GetTipSetByHeight(curNext.Height() - 1)
		if err != nil || pnext == nil {
			return []api.HeadChange{{Type: "apply", Val: next}}
		}
		curNext = pnext
		nextByHeight[curNext.Height()] = curNext
		pprev, err := store.GetTipSetByHeight(curPrev.Height() - 1)
		if err != nil || pprev == nil {
			return []api.HeadChange{{Type: "apply", Val: next}}
		}
		curPrev = pprev
		prevPath = append(prevPath, curPrev)
	}

	// Common ancestor is curPrev (==curNext by key).
	ancestor := curPrev

	// Reverts: prev → ancestor (already in prevPath, ordered newest-first).
	// We want to emit reverts of orphaned tipsets only — exclude the
	// ancestor itself.
	events := make([]api.HeadChange, 0, len(prevPath)+8)
	for _, t := range prevPath {
		if t.Key() == ancestor.Key() {
			continue
		}
		events = append(events, api.HeadChange{Type: "revert", Val: t})
	}

	// Applies: walk forward from ancestor+1 to next via the store's
	// canonical pointers (post-SetHead, those reflect next's chain).
	for ep := ancestor.Height() + 1; ep <= next.Height(); ep++ {
		ts, err := store.GetTipSetByHeight(ep)
		if err != nil || ts == nil {
			continue
		}
		events = append(events, api.HeadChange{Type: "apply", Val: ts})
	}
	return events
}
