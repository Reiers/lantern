// Package blockingest consumes Filecoin block announcements from
// gossipsub (/fil/blocks/<network>) and installs them into the header
// store as new heads, giving Lantern 0-1 epoch head-tracking latency
// without polling an upstream RPC.
//
// This logic previously lived in cmd/lantern (package main) and could
// not be reused by pkg/daemon (the embedded path used by curio-core).
// It is extracted here unchanged so both the standalone daemon
// (cmd/lantern) and the embedded daemon (pkg/daemon) mount the same
// gossipsub head-tracker. See lantern#40.
//
// Trust model: validation here is intentionally narrow. blockpub already
// did the CBOR shape + signature-presence check and the gossipsub topic
// validator; we add a CID re-derive (defense in depth), dedupe, a height
// fence, and parent-linkage. We do NOT do full Lotus-style block
// validation (BLS sig / election proof / beacon). Lantern is an
// F3-anchored light client: the trust path is F3 anchor -> SetHead ->
// content-addressed state queries, not full block re-execution. The
// polling Sync agent (chain/header/store) remains the catch-up fallback
// for anything gossipsub misses.
//
// Concurrency: blockpub's OnBlock callback fires from its own read
// goroutine. We funnel into a single processor goroutine via a bounded
// channel so head installs are serialized cleanly against the polling
// Sync and a runaway peer can't blow our memory.
package blockingest

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/Reiers/lantern/chain/header"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/blockpub"
)

var log = logging.Logger("lantern/blockingest")

// DefaultBackfillCap bounds RPC-based inline backfill depth (in epochs)
// per gossipsub event before deferring to the polling Sync agent. The RPC
// path (glif) does one round-trip per epoch, so this stays small to avoid
// hammering the upstream on a single stale gossip arrival.
const DefaultBackfillCap abi.ChainEpoch = 3

// DefaultParentWalkCap bounds parent-walk (bridge-off, #76) inline
// backfill depth in epochs. In bridge-off mode there is NO polling Sync
// catch-up (the polling src is a no-op without an upstream RPC), so this
// cap is the only path that stitches a cold-booted head from the seeded
// anchor to the live tip. It has to accommodate the natural anchor→live
// gap that accrues during init + daemon startup + peer discovery (easily
// 30-120 epochs on calibration; more if the anchor sat on disk overnight).
// The chainxchg path handles this efficiently (one iterative request per
// 900-tipset chunk, capped internally at maxBackfillTipsets); the
// per-CID bitswap fallback pays a linear FetchBlock cost, still bounded.
const DefaultParentWalkCap abi.ChainEpoch = 2880

// Corroboration retry tuning (#80 part 2). Gossipsub delivers a block to
// the subscriber on FIRST receipt; the duplicate copies that serve as
// corroboration votes arrive over the following ~1s from the rest of the
// mesh. Three retries 1.5s apart give the mesh ~4.5s to corroborate
// before the block is dropped (an epoch is 30s, so a corroborated block
// still adopts with seconds to spare; an uncorroborated one is left to
// the polling Sync safety net).
const (
	corroMaxRetries = 3
	corroRetryDelay = 1500 * time.Millisecond
)

// BackfillSource is the minimal RPC surface the ingestor uses for inline
// backfill when a gossipsub block arrives at head+N with N>1. The
// Lantern glif client, gateway client, and combined fetcher all satisfy
// it.
type BackfillSource interface {
	TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error)
	FetchBlock(ctx context.Context, k cid.Cid) (*ltypes.BlockHeader, error)
}

// Ingestor consumes block announcements from gossipsub and installs them
// into the header store as new heads. One ingestor per daemon; owns the
// dedup state. Build with New, then call Run once and feed it via
// Enqueue (which is the blockpub OnBlock callback).
type Ingestor struct {
	store *hstore.Store

	// src is the optional inline-backfill RPC source. With src set,
	// head+N>1 blocks land immediately via a bounded backfill burst
	// (capped by backfillCap) instead of waiting for the polling Sync's
	// cycle. May be nil.
	src           BackfillSource
	backfillCap   abi.ChainEpoch
	parentWalkCap abi.ChainEpoch

	// parentWalk selects the bridge-off backfill strategy (#76): resolve a
	// gap by walking the gossip block's own Parents chain via CID-addressed
	// FetchBlock (bitswap), instead of the RPC-shaped height->CID lookup.
	// Enabled by the daemon only when there is no upstream RPC source.
	parentWalk bool

	// chainFetcher, when set, is the preferred gap-resolution path in
	// parent-walk mode (#91): ONE ChainExchange request pulls the whole
	// verified header chain from a libp2p peer, instead of per-CID
	// bitswap fetches. Falls back to the per-CID walk when nil or on
	// error.
	chainFetcher ChainFetcher

	incoming chan *ltypes.BlockMsg

	seen      map[cid.Cid]struct{}
	seenOrder []cid.Cid
	seenCap   int

	// headAdoptionGate, when set, must return true for the ingestor to
	// adopt a new gossip head. The daemon wires this to the headcheck
	// divergence monitor: while the running head DIVERGES from the
	// independent-source quorum (possible eclipse/fork), the gate returns
	// false and the ingestor holds - it keeps receiving/backfilling blocks
	// but does not move head onto an uncorroborated tip. nil = always open
	// (Light/PDP, or no corroborating sources). (#79 item 2)
	headAdoptionGate atomic.Pointer[func() bool]

	// headCorroboration, when set, is consulted with the candidate head's
	// CID before adoption (#80 part 2). It returns true once the block has
	// been forwarded by enough distinct scored peers (or a trusted floor
	// peer). While false the ingestor holds head and retries briefly:
	// corroborating duplicates typically arrive within a second of first
	// delivery, so a held head is usually adopted on the first retry.
	// nil = always corroborated (Light/PDP default).
	headCorroboration atomic.Pointer[func(cid.Cid) bool]

	// Retry bookkeeping for corroboration holds. Touched ONLY from the
	// processor goroutine (Run loop); AfterFunc callbacks merely re-send
	// on the incoming channel.
	corroRetries map[cid.Cid]int
	corroPending map[cid.Cid]struct{}

	received        atomic.Uint64
	dedup           atomic.Uint64
	installed       atomic.Uint64
	skipped         atomic.Uint64
	rejected        atomic.Uint64
	rejectedLighter atomic.Uint64 // #79: candidates rejected by heaviest-weight fork choice
	// #156: rejected because ParentWeight didn't exceed the parent's.
	rejectedNonMonotonic atomic.Uint64
	heldDiverged         atomic.Uint64 // #79: head adoptions held while the divergence gate was closed
	heldUncorrob         atomic.Uint64 // #80: head adoptions held awaiting multi-peer corroboration
	backfilled           atomic.Uint64
	siblingsStored       atomic.Uint64 // blocks at the head height stored so the next epoch finds its parents
	siblingsMerged       atomic.Uint64 // of those, merged into the head tipset (head grew)
	parentsFetched       atomic.Uint64 // head+1 blocks whose missing parent headers were fetched directly
	backfillFailed       atomic.Uint64
	lastInstallEpoch     atomic.Int64
	lastInstallNanos     atomic.Int64 // wall-clock UnixNano of the last successful install (#71)
}

// SetHeadAdoptionGate wires a predicate consulted before each head
// adoption. While it returns false (e.g. headcheck reports the running head
// DIVERGES from the independent-source quorum), the ingestor holds head
// where it is: incoming blocks are still received and backfilled, but head
// is not advanced onto an uncorroborated tip. Passing nil re-opens the gate
// (always adopt). Safe to call before or after Run. (#79 item 2)
func (g *Ingestor) SetHeadAdoptionGate(fn func() bool) {
	if fn == nil {
		g.headAdoptionGate.Store(nil)
		return
	}
	g.headAdoptionGate.Store(&fn)
}

// SetHeadCorroboration wires the per-candidate corroboration predicate
// (#80 part 2): before adopting a block as the new head, the ingestor
// asks whether the block has been forwarded by enough distinct peers
// (see blockpub.CorroborationGate). While the predicate returns false
// the ingestor holds head where it is and schedules bounded retries
// (corroborating duplicates arrive within ~1s of first delivery on a
// healthy mesh). After the retries are exhausted the block is dropped;
// the polling Sync remains the safety net for head progress, so an
// uncorroboratable block can delay adoption but never wedge the node.
// Passing nil disables the check. Safe to call before or after Run.
func (g *Ingestor) SetHeadCorroboration(fn func(cid.Cid) bool) {
	if fn == nil {
		g.headCorroboration.Store(nil)
		return
	}
	g.headCorroboration.Store(&fn)
}

// New builds an ingestor wired to the header store. src may be nil; when
// nil, blocks at head+N>1 are skipped and the polling Sync's backfill
// path handles them on its next cycle.
func New(store *hstore.Store, src BackfillSource) *Ingestor {
	return &Ingestor{
		store:         store,
		src:           src,
		backfillCap:   DefaultBackfillCap,
		parentWalkCap: DefaultParentWalkCap,
		incoming:      make(chan *ltypes.BlockMsg, 64),
		seen:          make(map[cid.Cid]struct{}, 256),
		seenCap:       512,
		corroRetries:  make(map[cid.Cid]int, 8),
		corroPending:  make(map[cid.Cid]struct{}, 8),
	}
}

// Enqueue is the OnBlock callback handed to blockpub. Non-blocking:
// drops when the processor is behind so the gossipsub read loop never
// stalls (the polling Sync picks up any dropped head within its cycle).
func (g *Ingestor) Enqueue(blk *ltypes.BlockMsg) {
	if blk == nil || blk.Header == nil {
		return
	}
	g.received.Add(1)
	select {
	case g.incoming <- blk:
	default:
		g.skipped.Add(1)
	}
}

// Run is the processor loop. Blocks until ctx is cancelled.
func (g *Ingestor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case blk := <-g.incoming:
			g.process(ctx, blk)
		}
	}
}

// process turns a gossiped block into a header-store head. See the
// package doc for the validation rationale.
func (g *Ingestor) process(ctx context.Context, blk *ltypes.BlockMsg) {
	bh := blk.Header
	headerCID := bh.Cid()

	if _, ok := g.seen[headerCID]; ok {
		// A corroboration retry legitimately re-enters process for a
		// block we've already marked seen; everything else is a dup.
		if _, retry := g.corroPending[headerCID]; !retry {
			g.dedup.Add(1)
			return
		}
		delete(g.corroPending, headerCID)
	}
	g.markSeen(headerCID)

	// Defense in depth: re-derive the CID and verify against the header.
	if err := header.VerifyBlockHeaderCID(bh, headerCID); err != nil {
		g.rejected.Add(1)
		return
	}

	// Height fence: only act on blocks that advance our head, or that
	// complete the head tipset (a sibling at the head height).
	curHead := g.store.HeadEpoch()
	if bh.Height < curHead {
		return
	}
	if bh.Height == curHead {
		g.absorbSibling(ctx, bh, headerCID)
		return
	}

	// Parent linkage: inline-backfill (bounded) when parents are
	// missing; skip and defer to the polling Sync when src is nil.
	parents := bh.Parents
	if !g.allParentsPresent(parents) {
		if g.src == nil {
			g.skipped.Add(1)
			return
		}
		if err := g.inlineBackfill(ctx, bh); err != nil {
			// A stalled sync stems from these failures; log so operators
			// don't have to enable debug on every subsystem to see why
			// backfillFailed is incrementing (#116).
			log.Warnw("gossip inline-backfill failed",
				"err", err,
				"block_epoch", bh.Height,
				"cur_head", curHead,
				"parent_walk", g.parentWalk,
				"chain_fetcher", g.chainFetcher != nil)
			g.backfillFailed.Add(1)
			g.skipped.Add(1)
			return
		}
		if !g.allParentsPresent(parents) {
			// The gap fill above only covers epochs ABOVE the store head.
			// A block at head+1 whose parent tipset includes a block we
			// never received (a sibling of our head block that gossip
			// delivered late or not at all) lands here; without this it
			// was skipped and head waited for the next epoch.
			if err := g.fetchMissingParents(ctx, bh); err != nil {
				log.Warnw("gossip missing-parent fetch failed",
					"err", err,
					"block_epoch", bh.Height,
					"cur_head", curHead)
				g.backfillFailed.Add(1)
				g.skipped.Add(1)
				return
			}
		}
		g.backfilled.Add(1)
		if !g.allParentsPresent(parents) {
			g.skipped.Add(1)
			return
		}
	}

	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	if err != nil {
		g.rejected.Add(1)
		return
	}

	// #79: heaviest-ParentWeight fork choice on the running head.
	//
	// The height fence above only guarantees the candidate is *higher*
	// than our current head - it does NOT guarantee it's on the canonical
	// (heaviest) chain. An attacker who eclipses the peer table can feed
	// parent-linked, height-advancing blocks on a valid-but-lighter fork
	// and walk us onto it; content addressing doesn't catch this because
	// the attacker's blocks hash fine, they're just not canonical.
	//
	// Filecoin's fork-choice rule is heaviest ParentWeight. A real
	// descendant of our current head always has strictly greater
	// ParentWeight; a competing lighter fork at a higher height has lower
	// or equal weight. So we adopt the candidate as head only when its
	// ParentWeight strictly exceeds the current head's. This is pure
	// header arithmetic (no proof verification, no ffi) and raises the
	// eclipse cost from "spin up N sybil peers" to "out-weight the real
	// chain" (i.e. control real storage power).
	if cur := g.store.Head(); cur != nil {
		cw := cur.ParentWeight()
		nw := ts.ParentWeight()
		if !cw.Nil() && !nw.Nil() && nw.LessThanEqual(cw) {
			g.rejectedLighter.Add(1)
			return
		}
	}

	// #156: weight-monotonic guard. ParentWeight is taken from the header
	// (full recompute needs power state, out of scope for a light node),
	// but every honest child has strictly more weight than its parent
	// tipset. A header whose ParentWeight doesn't exceed its parent's is
	// malformed or forged; never let it win fork choice. Only checked
	// when the parent is in store (backfill above makes that the norm).
	// Runs after #79 so lighter-than-head candidates keep counting as
	// RejectedLighter; this catches the ones that beat our head but not
	// their own parent.
	if len(bh.Parents) > 0 {
		if pb, perr := g.store.Get(bh.Parents[0]); perr == nil && pb != nil {
			pw, cw := pb.ParentWeight, bh.ParentWeight
			if !pw.Nil() && !cw.Nil() && cw.LessThanEqual(pw) {
				g.rejectedNonMonotonic.Add(1)
				return
			}
		}
	}

	// #79 item 2: divergence gate. If the headcheck monitor reports the
	// running head is uncorroborated by the independent-source quorum, do
	// NOT adopt this new tip - hold head where it is until corroboration
	// returns. The block is still seen + backfilled (state stays warm); we
	// just refuse to walk head onto a possibly-eclipsed fork. A heavier,
	// corroborated head will be adopted on a later round once the gate
	// reopens.
	if gp := g.headAdoptionGate.Load(); gp != nil {
		if open := (*gp)(); !open {
			g.heldDiverged.Add(1)
			return
		}
	}

	// #80 part 2: head-source corroboration. Gossipsub dedups messages,
	// so first delivery may reach us before other mesh peers' copies have
	// been counted. If the block is not yet corroborated by enough
	// distinct sources, hold head and retry shortly: the duplicates that
	// serve as corroboration votes typically land within a second. Give
	// up after corroMaxRetries - the polling Sync is the safety net, so
	// this can delay head adoption but never wedge it.
	if cp := g.headCorroboration.Load(); cp != nil {
		if !(*cp)(headerCID) {
			g.heldUncorrob.Add(1)
			if g.corroRetries[headerCID] < corroMaxRetries {
				g.corroRetries[headerCID]++
				g.corroPending[headerCID] = struct{}{}
				time.AfterFunc(corroRetryDelay, func() {
					select {
					case g.incoming <- blk:
					default:
					}
				})
			} else {
				delete(g.corroRetries, headerCID)
			}
			return
		}
		delete(g.corroRetries, headerCID)
		delete(g.corroPending, headerCID)
	}

	if err := g.store.SetHead(ctx, ts); err != nil {
		g.rejected.Add(1)
		return
	}
	g.installed.Add(1)
	g.lastInstallEpoch.Store(int64(bh.Height))
	g.lastInstallNanos.Store(time.Now().UnixNano())
}

// absorbSibling handles a gossiped block at the head's height.
//
// A Filecoin tipset is every block at one height that shares the same
// parents; its blocks arrive one by one over gossip, and Lotus grows its
// head tipset as they land (the bigger tipset is the heavier one). Lantern
// used to drop these blocks at the height fence. That left the head as a
// one-block tipset and, worse, the NEXT epoch's blocks, whose Parents list
// the full tipset, then found a parent missing from the store and were
// skipped: on any epoch with more than one block the head stalled until a
// later block arrived and the gap was backfilled, about one epoch (30 s)
// behind the network. Curio's Chain page shows that as "slow (55s behind)".
//
// The sibling is persisted whenever it provably belongs to the head
// tipset: same height and identical parent-derived header fields
// (Parents, ParentStateRoot, ParentMessageReceipts, ParentWeight), which
// every honest block of one tipset shares. That is header arithmetic only,
// the same trust level as the rest of this file. The head itself then
// grows to include it, through the same divergence and corroboration
// gates as any other head adoption.
func (g *Ingestor) absorbSibling(ctx context.Context, bh *ltypes.BlockHeader, c cid.Cid) {
	cur := g.store.Head()
	if cur == nil || cur.Height() != bh.Height || cur.Contains(c) || len(cur.Blocks()) == 0 {
		return
	}
	if !sameTipsetParent(cur.Blocks()[0], bh) {
		// A competing block on another fork at the head height. Fork
		// choice happens on its children (heaviest ParentWeight).
		return
	}
	single, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	if err != nil {
		g.rejected.Add(1)
		return
	}
	if err := g.store.PutTipSet(single); err != nil {
		g.rejected.Add(1)
		return
	}
	g.siblingsStored.Add(1)

	if gp := g.headAdoptionGate.Load(); gp != nil && !(*gp)() {
		g.heldDiverged.Add(1)
		return
	}
	if cp := g.headCorroboration.Load(); cp != nil && !(*cp)(c) {
		// Stored for the next epoch's parent check; the head simply does
		// not grow onto an uncorroborated block.
		g.heldUncorrob.Add(1)
		return
	}
	blocks := append(append([]*ltypes.BlockHeader(nil), cur.Blocks()...), bh)
	grown, err := ltypes.NewTipSet(blocks)
	if err != nil {
		g.rejected.Add(1)
		return
	}
	// The head may have moved while we worked (polling Sync); only grow
	// the tipset we started from.
	if now := g.store.Head(); now == nil || now.Key() != cur.Key() {
		return
	}
	if err := g.store.SetHead(ctx, grown); err != nil {
		g.rejected.Add(1)
		return
	}
	g.siblingsMerged.Add(1)
	g.lastInstallNanos.Store(time.Now().UnixNano())
}

// sameTipsetParent reports whether a and b can be blocks of one tipset:
// same height and the same parent-derived fields.
func sameTipsetParent(a, b *ltypes.BlockHeader) bool {
	if a.Height != b.Height || len(a.Parents) != len(b.Parents) {
		return false
	}
	for i := range a.Parents {
		if a.Parents[i] != b.Parents[i] {
			return false
		}
	}
	if a.ParentStateRoot != b.ParentStateRoot || a.ParentMessageReceipts != b.ParentMessageReceipts {
		return false
	}
	if a.ParentWeight.Nil() || b.ParentWeight.Nil() {
		return a.ParentWeight.Nil() == b.ParentWeight.Nil()
	}
	return a.ParentWeight.Equals(b.ParentWeight)
}

// fetchMissingParents fetches exactly the parent headers of bh that are not
// in the store, verifies them (CID re-derived from the bytes, one tipset at
// a lower height), and persists that parent tipset. Persist only: head
// adoption stays the single guarded decision in process().
func (g *Ingestor) fetchMissingParents(ctx context.Context, bh *ltypes.BlockHeader) error {
	if len(bh.Parents) == 0 {
		return fmt.Errorf("block @ %d has no parents", bh.Height)
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ts *ltypes.TipSet
	if g.chainFetcher != nil {
		if chain, err := g.chainFetcher.FetchTipsetChain(fctx, bh.Parents, 1); err == nil && len(chain) > 0 {
			if cand, terr := ltypes.NewTipSet(dedupBlocksByCID(chain[0])); terr == nil && sameCIDSet(cand.Cids(), bh.Parents) {
				ts = cand
			}
		}
	}
	if ts == nil {
		if g.src == nil {
			return fmt.Errorf("no block source for missing parents")
		}
		blocks := make([]*ltypes.BlockHeader, 0, len(bh.Parents))
		for _, pc := range bh.Parents {
			if b, err := g.store.Get(pc); err == nil {
				blocks = append(blocks, b)
				continue
			}
			b, err := g.src.FetchBlock(fctx, pc)
			if err != nil {
				return fmt.Errorf("fetch parent %s: %w", pc, err)
			}
			blocks = append(blocks, b)
		}
		cand, err := ltypes.NewTipSet(blocks)
		if err != nil {
			return fmt.Errorf("parent tipset: %w", err)
		}
		ts = cand
	}
	// ts.Cids() are re-derived from the header bytes, so matching them
	// against bh.Parents is the content-address check: a peer can only
	// refuse, never substitute a header.
	if !sameCIDSet(ts.Cids(), bh.Parents) {
		return fmt.Errorf("parent tipset does not match the block's Parents")
	}
	for _, b := range ts.Blocks()[1:] {
		if !sameTipsetParent(ts.Blocks()[0], b) {
			return fmt.Errorf("parent tipset blocks disagree on parent-derived fields")
		}
	}
	if ts.Height() >= bh.Height {
		return fmt.Errorf("parent tipset height %d not below block height %d", ts.Height(), bh.Height)
	}
	if err := g.store.PutTipSet(ts); err != nil {
		return err
	}
	g.parentsFetched.Add(1)
	return nil
}

// sameCIDSet reports whether a and b hold the same CIDs (order-insensitive).
func sameCIDSet(a, b []cid.Cid) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[cid.Cid]int, len(a))
	for _, c := range a {
		m[c]++
	}
	for _, c := range b {
		if m[c] == 0 {
			return false
		}
		m[c]--
	}
	return true
}

// ObservedHead returns the highest block height the ingestor has
// successfully installed into the store. This is the gossip layer's view
// of the chain tip: it tracks the live head (>= the canonical store head,
// since individual high-epoch blocks can be installed before the canonical
// head advances contiguously to them). Returns -1 if nothing installed yet.
//
// The polling Sync uses this to make its #71 gossip-fresh skip lag-aware
// (#83): gossip being "fresh" only means head moved recently, not that
// head is at the tip. Comparing the store head against ObservedHead lets
// Sync skip the catch-up poll only when actually at the tip, and run it
// when gossip is fresh-but-lagging - without paying an upstream HeadEpoch
// RPC call.
func (g *Ingestor) ObservedHead() abi.ChainEpoch {
	v := g.lastInstallEpoch.Load()
	if v == 0 {
		return -1
	}
	return abi.ChainEpoch(v)
}

// Fresh reports whether the ingestor installed a block within the last
// `within` duration. Used by the polling Sync (#71) to decide whether
// gossip is currently keeping the store head live, in which case the
// Sync skips its upstream-RPC HeadEpoch() poll for that tick. Returns
// false before the first install (zero timestamp).
func (g *Ingestor) Fresh(within time.Duration) bool {
	ns := g.lastInstallNanos.Load()
	if ns == 0 {
		return false
	}
	return time.Since(time.Unix(0, ns)) <= within
}

// allParentsPresent returns true if every parent CID is already in the
// header store. Cheap point-check, no fetch.
func (g *Ingestor) allParentsPresent(parents []cid.Cid) bool {
	for _, pc := range parents {
		if _, err := g.store.Get(pc); err != nil {
			return false
		}
	}
	return true
}

// inlineBackfill walks from curHead+1 up to (but not including)
// bh.Height, fetching missing tipsets via the RPC source and installing
// each. Bounded by backfillCap (epoch-depth).
func (g *Ingestor) inlineBackfill(ctx context.Context, bh *ltypes.BlockHeader) error {
	if g.parentWalk {
		return g.parentWalkBackfill(ctx, bh)
	}
	curHead := g.store.HeadEpoch()
	needFrom := curHead + 1
	needTo := bh.Height - 1
	if needFrom > needTo {
		return nil
	}
	gap := needTo - needFrom + 1
	if gap > g.backfillCap {
		return fmt.Errorf("backfill gap %d > cap %d", gap, g.backfillCap)
	}

	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for ep := needFrom; ep <= needTo; ep++ {
		cids, err := g.src.TipsetCIDsByHeight(bctx, ep)
		if err != nil {
			return fmt.Errorf("backfill cids @ %d: %w", ep, err)
		}
		if len(cids) == 0 {
			continue // null-round epoch
		}
		blocks := make([]*ltypes.BlockHeader, 0, len(cids))
		for _, c := range cids {
			if b, gerr := g.store.Get(c); gerr == nil {
				blocks = append(blocks, b)
				continue
			}
			b, ferr := g.src.FetchBlock(bctx, c)
			if ferr != nil {
				return fmt.Errorf("backfill fetch %s: %w", c, ferr)
			}
			if verr := header.VerifyBlockHeaderCID(b, c); verr != nil {
				return fmt.Errorf("backfill cid verify @ %d: %w", ep, verr)
			}
			blocks = append(blocks, b)
		}
		ts, err := ltypes.NewTipSet(blocks)
		if err != nil {
			return fmt.Errorf("backfill tipset @ %d: %w", ep, err)
		}
		// #155: persist only. Head adoption (fork choice + divergence
		// gate + corroboration) happens once, on the gossip tip, in
		// process(); the tip's SetHead rewires canonical pointers for
		// these ancestors if and only if the tip is adopted.
		if err := g.store.PutTipSet(ts); err != nil {
			return fmt.Errorf("backfill put @ %d: %w", ep, err)
		}
	}
	return nil
}

// markSeen inserts the CID into the dedupe set with simple LRU eviction.
func (g *Ingestor) markSeen(c cid.Cid) {
	g.markSeenImpl(c)
}

// SetParentWalkBackfill enables the #76 bridge-off backfill strategy, where
// a gap is filled by CID-walking the gossip block's Parents via bitswap
// FetchBlock instead of the RPC-shaped height->CID lookup. Call once at
// construction time before Run.
func (g *Ingestor) SetParentWalkBackfill(on bool) { g.parentWalk = on }

// ChainFetcher pulls a verified header chain (newest->oldest, level 0 =
// the tipset whose block CIDs are head) from the p2p swarm. Implemented
// by net/chainxchg.Client (#91).
type ChainFetcher interface {
	FetchTipsetChain(ctx context.Context, head []cid.Cid, length int) ([][]*ltypes.BlockHeader, error)
}

// SetChainFetcher wires the ChainExchange client as the preferred
// parent-walk gap resolver (#91). Call before Run.
func (g *Ingestor) SetChainFetcher(f ChainFetcher) { g.chainFetcher = f }

// parentWalkBackfill resolves a head+N (N>1) gap without any RPC by walking
// the target block's Parents chain via CID-addressed FetchBlock (bitswap),
// descending until every CID at a level is already in the store (the seeded
// anchor / current head), then installing the collected tipsets oldest-first
// so the head ends at the highest backfilled tipset. Bounded by backfillCap.
func (g *Ingestor) parentWalkBackfill(ctx context.Context, bh *ltypes.BlockHeader) error {
	curHead := g.store.HeadEpoch()
	needTo := bh.Height - 1
	if curHead >= needTo {
		return nil
	}
	if gap := needTo - curHead; gap > g.parentWalkCap {
		return fmt.Errorf("bridge-off backfill gap %d > cap %d", gap, g.parentWalkCap)
	}
	if len(bh.Parents) == 0 {
		return fmt.Errorf("bridge-off backfill: gossip block @ %d has no parents", bh.Height)
	}

	// Preferred path (#91): ONE ChainExchange request for the whole gap.
	// The client CID-verifies the chain against bh.Parents, so a bad peer
	// can only refuse, not splice. Fall back to the per-CID bitswap walk
	// on any error.
	if g.chainFetcher != nil {
		if err := g.chainExchangeBackfill(ctx, bh); err == nil {
			return nil
		} else if g.src == nil {
			// No per-CID fallback source either: surface the real error.
			return fmt.Errorf("bridge-off chainxchg backfill: %w", err)
		} else {
			log.Infow("chainxchg backfill failed; falling back to per-CID walk",
				"err", err,
				"block_epoch", bh.Height,
				"cur_head", curHead)
		}
	}

	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	collected := map[abi.ChainEpoch][]*ltypes.BlockHeader{}
	seen := map[cid.Cid]struct{}{}
	frontier := append([]cid.Cid(nil), bh.Parents...)
	for len(frontier) > 0 {
		if abi.ChainEpoch(len(collected)) > g.parentWalkCap {
			return fmt.Errorf("bridge-off backfill exceeded cap %d", g.parentWalkCap)
		}
		var next []cid.Cid
		levelAllPresent := true
		for _, c := range frontier {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			if _, err := g.store.Get(c); err == nil {
				continue // already present: reached the store head / seeded anchor
			}
			levelAllPresent = false
			b, ferr := g.src.FetchBlock(bctx, c)
			if ferr != nil {
				return fmt.Errorf("bridge-off backfill fetch %s: %w", c, ferr)
			}
			if verr := header.VerifyBlockHeaderCID(b, c); verr != nil {
				return fmt.Errorf("bridge-off backfill cid verify %s: %w", c, verr)
			}
			if b.Height <= curHead {
				continue // walked below the store head on this branch; stop it
			}
			collected[b.Height] = append(collected[b.Height], b)
			next = append(next, b.Parents...)
		}
		if levelAllPresent {
			break
		}
		frontier = next
	}
	if len(collected) == 0 {
		return nil
	}

	heights := make([]abi.ChainEpoch, 0, len(collected))
	for h := range collected {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	for _, h := range heights {
		ts, err := ltypes.NewTipSet(dedupBlocksByCID(collected[h]))
		if err != nil {
			return fmt.Errorf("bridge-off backfill tipset @ %d: %w", h, err)
		}
		// #155: persist only; see inlineBackfill.
		if err := g.store.PutTipSet(ts); err != nil {
			return fmt.Errorf("bridge-off backfill put @ %d: %w", h, err)
		}
	}
	return nil
}

// maxBackfillTipsets bounds a single catch-up so a stale anchor (or a
// misbehaving peer) can't drive unbounded memory. ~20k tipsets of
// headers is a few hundred MiB worst case and covers ~7 days of
// calibration epochs - well beyond any healthy quorum-anchor age.
const maxBackfillTipsets = 20000

// chainExchangeBackfill fills [storeHead+1, bh.Height-1] via ChainExchange
// (#91), rooted at bh.Parents. A single request is capped at
// MaxRequestLength (900) tipsets, but a snapshot-free boot from a stale
// quorum anchor can be thousands of epochs behind live head - so we LOOP,
// rooting each successive request at the previous batch's oldest tipset's
// parents, until we reach the store head. Each returned chain is
// newest->oldest and already CID-verified (a peer can only refuse, never
// splice); we install the whole gap oldest-first so the store head walks
// contiguously up to the gossip block.
func (g *Ingestor) chainExchangeBackfill(ctx context.Context, bh *ltypes.BlockHeader) error {
	curHead := g.store.HeadEpoch()
	bctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	var full [][]*ltypes.BlockHeader
	nextHead := bh.Parents
	oldest := bh.Height
	for oldest > curHead+1 {
		if len(full) >= maxBackfillTipsets {
			return fmt.Errorf("chainxchg backfill: gap exceeds %d tipsets (store head %d, target %d) - anchor too stale", maxBackfillTipsets, curHead, bh.Height)
		}
		gap := int(oldest - 1 - curHead)
		if gap < 1 {
			gap = 1
		}
		chain, err := g.chainFetcher.FetchTipsetChain(bctx, nextHead, gap)
		if err != nil {
			return err
		}
		if len(chain) == 0 {
			return fmt.Errorf("chainxchg backfill: peer returned empty chain at height %d", oldest)
		}
		full = append(full, chain...)
		last := chain[len(chain)-1]
		newOldest := last[0].Height
		if newOldest >= oldest {
			// No downward progress: guard against a peer that echoes the
			// same level forever.
			return fmt.Errorf("chainxchg backfill: no progress at height %d", oldest)
		}
		oldest = newOldest
		nextHead = last[0].Parents
	}

	// Install oldest-first, skipping tipsets at/below the store head.
	for i := len(full) - 1; i >= 0; i-- {
		blocks := full[i]
		if blocks[0].Height <= curHead {
			continue
		}
		ts, terr := ltypes.NewTipSet(blocks)
		if terr != nil {
			return fmt.Errorf("chainxchg backfill tipset @ %d: %w", blocks[0].Height, terr)
		}
		// #155: persist only; see inlineBackfill.
		if serr := g.store.PutTipSet(ts); serr != nil {
			return fmt.Errorf("chainxchg backfill put @ %d: %w", blocks[0].Height, serr)
		}
	}
	return nil
}

// dedupBlocksByCID removes duplicate block headers (same CID reached via
// multiple parent paths) while preserving order.
func dedupBlocksByCID(in []*ltypes.BlockHeader) []*ltypes.BlockHeader {
	if len(in) < 2 {
		return in
	}
	seen := make(map[cid.Cid]struct{}, len(in))
	out := in[:0:0]
	for _, b := range in {
		c := b.Cid()
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, b)
	}
	return out
}

// markSeenImpl inserts the CID into the dedupe set with simple LRU eviction.
func (g *Ingestor) markSeenImpl(c cid.Cid) {
	g.seen[c] = struct{}{}
	g.seenOrder = append(g.seenOrder, c)
	if len(g.seenOrder) > g.seenCap {
		evict := g.seenOrder[0]
		g.seenOrder = g.seenOrder[1:]
		delete(g.seen, evict)
	}
}

// Stats is a snapshot of ingestor counters for observability.
type Stats struct {
	Received        uint64
	Dedup           uint64
	Installed       uint64
	Skipped         uint64
	Rejected        uint64
	RejectedLighter uint64 // #79: rejected by heaviest-ParentWeight fork choice
	// #156: rejected because ParentWeight <= parent tipset's ParentWeight.
	RejectedNonMonotonic uint64
	HeldDiverged         uint64 // #79: head adoptions held while divergence gate closed
	HeldUncorrob         uint64 // #80: head adoptions held awaiting multi-peer corroboration
	Backfilled           uint64
	BackfillFailed       uint64
	SiblingsStored       uint64 // blocks at the head height persisted for the next epoch's parent check
	SiblingsMerged       uint64 // of those, merged into the head tipset
	ParentsFetched       uint64 // head+1 blocks whose missing parent headers were fetched directly
	LastInstallEpoch     abi.ChainEpoch
}

// Stats returns a snapshot of counters.
func (g *Ingestor) Stats() Stats {
	return Stats{
		Received:             g.received.Load(),
		Dedup:                g.dedup.Load(),
		Installed:            g.installed.Load(),
		Skipped:              g.skipped.Load(),
		Rejected:             g.rejected.Load(),
		RejectedLighter:      g.rejectedLighter.Load(),
		RejectedNonMonotonic: g.rejectedNonMonotonic.Load(),
		HeldDiverged:         g.heldDiverged.Load(),
		HeldUncorrob:         g.heldUncorrob.Load(),
		Backfilled:           g.backfilled.Load(),
		BackfillFailed:       g.backfillFailed.Load(),
		SiblingsStored:       g.siblingsStored.Load(),
		SiblingsMerged:       g.siblingsMerged.Load(),
		ParentsFetched:       g.parentsFetched.Load(),
		LastInstallEpoch:     abi.ChainEpoch(g.lastInstallEpoch.Load()),
	}
}

// Start brings up the blockpub subscription + ingestor goroutine on the
// given gossipsub topic. Returns the ingestor (for stats/shutdown) and
// the blockpub publisher, or an error.
//
// Side effects:
//   - Joins /fil/blocks/<network> on gossipsub
//   - Starts the ingestor goroutine
//
// The caller owns ctx cancellation for shutdown and may start its own
// stats-logging loop using Ingestor.Stats() + Publisher.Stats().
func Start(ctx context.Context, ps *pubsub.PubSub, store *hstore.Store, src BackfillSource, topic string) (*Ingestor, *blockpub.Publisher, error) {
	if ps == nil || store == nil {
		return nil, nil, fmt.Errorf("blockingest.Start: ps and store are required")
	}
	ing := New(store, src)
	pub, err := blockpub.New(ctx, ps, blockpub.Config{
		OnBlock: ing.Enqueue,
		Topic:   topic,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("blockpub.New: %w", err)
	}
	go ing.Run(ctx)
	log.Infow("gossipsub block ingestor started", "topic", topic, "inline_backfill", src != nil)
	return ing, pub, nil
}

// StatsLogger runs a periodic one-line stats summary every interval until
// ctx is cancelled. Optional; standalone cmd/lantern uses it so operators
// can confirm gossipsub is carrying the load. logf is the sink (e.g.
// fmt.Fprintf to stderr or a logger).
func StatsLogger(ctx context.Context, ing *Ingestor, pub *blockpub.Publisher, interval time.Duration, logf func(format string, args ...any)) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := ing.Stats()
			published, received, rejected := pub.Stats()
			logf("  [gossip-block] sub-rcv=%d sub-rej=%d ing-rcv=%d installed=%d dedup=%d skipped=%d rejected=%d backfilled=%d backfillFail=%d siblings=%d/%d parentsFetched=%d lastEpoch=%d published=%d\n",
				received, rejected,
				s.Received, s.Installed, s.Dedup, s.Skipped, s.Rejected,
				s.Backfilled, s.BackfillFailed,
				s.SiblingsMerged, s.SiblingsStored, s.ParentsFetched,
				s.LastInstallEpoch, published,
			)
		}
	}
}
