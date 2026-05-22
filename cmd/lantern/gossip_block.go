// Gossipsub block ingestor.
//
// Issue #1: the daemon learns about new chain heads by polling an upstream
// Lotus-compatible JSON-RPC source every 6 seconds (chain/header/store/sync.go).
// Mainnet block time is 30 seconds, so even at our most aggressive poll
// cadence we sit a stable 1 epoch behind Lotus on the same box. The lag is
// not a peer-propagation problem; it's a poll-cadence problem.
//
// The fix is to subscribe to /fil/blocks/testnetnet on gossipsub via the
// existing net/blockpub package. When a block arrives over gossipsub at
// the head+1 epoch, install it into the header store immediately, rather
// than waiting for the next 6-second poll.
//
// The polling Sync agent stays in place as a catch-up fallback for any
// block that gossipsub missed (connectivity blips, late-joining the mesh,
// etc.). When gossipsub is active we lift the poll interval to 30s so we
// don't hammer the upstream during normal operation.
//
// Concurrency model: blockpub's OnBlock callback fires from its own read
// goroutine. We funnel into a single processor goroutine via a channel
// so head installs are serialized cleanly against the polling Sync.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/Reiers/lantern/chain/header"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/blockpub"
)

// gossipBlockIngestor consumes block announcements from gossipsub and
// installs them into the header store as new heads.
//
// One ingestor per daemon. Owns the deduplication state.
type gossipBlockIngestor struct {
	store *hstore.Store

	// incoming carries decoded blocks from blockpub's read goroutine to
	// the single processor goroutine. Bounded so a runaway peer can't
	// blow our memory.
	incoming chan *ltypes.BlockMsg

	// seen tracks header CIDs we've already processed in this run, so a
	// duplicate gossipsub announcement (libp2p replays during dial
	// churn) is a no-op. Bounded by simple LRU-by-insertion-order via
	// the seenOrder slice.
	seen      map[cid.Cid]struct{}
	seenOrder []cid.Cid
	seenCap   int

	// Stats are observable via lantern info / metrics endpoint.
	received         atomic.Uint64
	dedup            atomic.Uint64
	installed        atomic.Uint64
	skipped          atomic.Uint64
	rejected         atomic.Uint64
	lastInstallEpoch atomic.Int64
}

// newGossipBlockIngestor builds an ingestor wired to the header store.
//
// Caller is responsible for calling Run once and Close on shutdown.
func newGossipBlockIngestor(store *hstore.Store) *gossipBlockIngestor {
	return &gossipBlockIngestor{
		store:    store,
		incoming: make(chan *ltypes.BlockMsg, 64),
		seen:     make(map[cid.Cid]struct{}, 256),
		seenCap:  512,
	}
}

// Enqueue is the OnBlock callback handed to blockpub. Non-blocking: drops
// when the processor is behind so the gossipsub read loop never stalls.
func (g *gossipBlockIngestor) Enqueue(blk *ltypes.BlockMsg) {
	if blk == nil || blk.Header == nil {
		return
	}
	g.received.Add(1)
	select {
	case g.incoming <- blk:
	default:
		// Processor is behind. Dropping is safe because the polling
		// Sync agent will pick the head up within 30s and we'll just
		// have missed the latency improvement for this one epoch.
		g.skipped.Add(1)
	}
}

// Run is the processor loop. Blocks until ctx is cancelled.
func (g *gossipBlockIngestor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case blk := <-g.incoming:
			g.process(ctx, blk)
		}
	}
}

// process is the single point where a gossiped block becomes a header-store
// head. Validation is intentionally narrow: blockpub already did the CBOR
// shape + signature-presence check. We add:
//
//   - CID re-derive (defense in depth against a malicious peer that crafts
//     a block whose declared CID lies; superficiallyValid doesn't check this)
//   - Dedupe against the seen set
//   - Height fence: refuse blocks at epoch <= our current head
//   - Parent fetch: if we don't have the parents in the store, skip and let
//     the polling Sync's backfill path handle it
//
// We do NOT do full ValidateHeader here; the polling Sync's backfill of
// the parent chain triggers ValidateTipsetShape via SetHead, and the
// upstream gateway re-verifies every IPLD block on fetch. The trust path
// remains: F3 anchor -> SetHead -> content-addressed state queries.
func (g *gossipBlockIngestor) process(ctx context.Context, blk *ltypes.BlockMsg) {
	bh := blk.Header
	headerCID := bh.Cid()

	// Dedupe.
	if _, ok := g.seen[headerCID]; ok {
		g.dedup.Add(1)
		return
	}
	g.markSeen(headerCID)

	// Defense in depth: re-derive the CID and verify against the header.
	// Cheap to do, cheap to forget; if a malicious peer manages to slip
	// a malformed block past blockpub's shape check we catch it here.
	if err := header.VerifyBlockHeaderCID(bh, headerCID); err != nil {
		g.rejected.Add(1)
		return
	}

	// Height fence: only act on blocks that advance our head.
	curHead := g.store.HeadEpoch()
	if bh.Height <= curHead {
		// Block at or behind our head. Either a sibling tipset (handled
		// by the polling Sync's reorg detection) or a stale rebroadcast.
		// Either way, no work to do here.
		return
	}

	// Parent linkage: if the parent tipset isn't in the store, the
	// polling Sync's backfill path is the right tool. We don't want to
	// trigger backfills from the gossipsub goroutine.
	parents := bh.Parents
	allParentsKnown := true
	for _, pc := range parents {
		if _, err := g.store.Get(pc); err != nil {
			allParentsKnown = false
			break
		}
	}
	if !allParentsKnown {
		g.skipped.Add(1)
		return
	}

	// Multi-block tipsets at the same height: Filecoin allows up to
	// BlocksPerEpoch (typically 5) winning miners per epoch. Each
	// announces its own block separately. We install each as it
	// arrives; SetHead's reorg / tipset assembly logic takes care of
	// merging by epoch. For epochs with a single block in this run, the
	// install is straightforward.
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	if err != nil {
		g.rejected.Add(1)
		return
	}

	if err := g.store.SetHead(ctx, ts); err != nil {
		g.rejected.Add(1)
		return
	}
	g.installed.Add(1)
	g.lastInstallEpoch.Store(int64(bh.Height))
}

// markSeen inserts the CID into the dedupe set with simple LRU eviction.
func (g *gossipBlockIngestor) markSeen(c cid.Cid) {
	g.seen[c] = struct{}{}
	g.seenOrder = append(g.seenOrder, c)
	if len(g.seenOrder) > g.seenCap {
		evict := g.seenOrder[0]
		g.seenOrder = g.seenOrder[1:]
		delete(g.seen, evict)
	}
}

// Stats returns a snapshot of counters for observability.
type gossipBlockStats struct {
	Received         uint64
	Dedup            uint64
	Installed        uint64
	Skipped          uint64
	Rejected         uint64
	LastInstallEpoch abi.ChainEpoch
}

// Stats returns a snapshot of counters.
func (g *gossipBlockIngestor) Stats() gossipBlockStats {
	return gossipBlockStats{
		Received:         g.received.Load(),
		Dedup:            g.dedup.Load(),
		Installed:        g.installed.Load(),
		Skipped:          g.skipped.Load(),
		Rejected:         g.rejected.Load(),
		LastInstallEpoch: abi.ChainEpoch(g.lastInstallEpoch.Load()),
	}
}

// startGossipBlocks brings up the blockpub subscription + ingestor + log
// loop. Returns the ingestor (for stats / shutdown) or nil + error.
//
// Side effects:
//   - Joins /fil/blocks/<network> on gossipsub
//   - Starts the ingestor goroutine
//   - Starts a periodic stats log every 60s (matches the existing [sync]
//     stat cadence so operators see both side by side)
func startGossipBlocks(ctx context.Context, ps *pubsub.PubSub, store *hstore.Store) (*gossipBlockIngestor, *blockpub.Publisher, error) {
	if ps == nil || store == nil {
		return nil, nil, fmt.Errorf("startGossipBlocks: ps and store are required")
	}
	ing := newGossipBlockIngestor(store)
	pub, err := blockpub.New(ctx, ps, blockpub.Config{
		OnBlock: ing.Enqueue,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("blockpub.New: %w", err)
	}
	go ing.Run(ctx)
	go logGossipBlockStats(ctx, ing, pub)
	return ing, pub, nil
}

// logGossipBlockStats prints a one-line stats summary every 60s so the
// operator can confirm gossipsub is carrying the load.
func logGossipBlockStats(ctx context.Context, ing *gossipBlockIngestor, pub *blockpub.Publisher) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := ing.Stats()
			published, received, rejected := pub.Stats()
			fmt.Fprintf(os.Stderr,
				"  [gossip-block] sub-received=%d sub-rejected=%d ingest-received=%d installed=%d dedup=%d skipped=%d rejected=%d lastEpoch=%d published=%d\n",
				received, rejected,
				s.Received, s.Installed, s.Dedup, s.Skipped, s.Rejected,
				s.LastInstallEpoch, published,
			)
		}
	}
}

// _ keeps the bytes import alive even when the rest of the file is
// reshuffled later. Block-decode in process() does not currently use it.
var _ = bytes.NewReader
