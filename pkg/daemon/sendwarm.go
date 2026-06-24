package daemon

// lantern#50 prefetch-on-send.
//
// When eth_sendRawTransaction publishes a tx locally (#45 Stage 4), the
// receipt poll that follows (eth_getTransactionReceipt -> StateSearchMsg)
// has to fetch the freshly-produced message block, its message AMTs, and
// the receipt block over Bitswap. Bridge-off, those blocks live only on the
// peer set, and a just-produced block isn't reliably served by random
// gossip peers inside the client's poll window. The result was the residual
// "net/bitswap: context canceled" miss documented in #50.
//
// The warmer closes that gap proactively: the moment we publish a tx, we
// start polling StateSearchMsg for its message CID in the BACKGROUND, on a
// generous standalone context that isn't tied to any client RPC deadline.
// That background search drives Bitswap to pull the message + AMT + receipt
// blocks into the local blockstore as soon as the tx lands. By the time the
// client's own (short-deadline) receipt poll runs, the blocks are warm and
// the search resolves from cache instead of racing a cold cross-peer fetch.
//
// This is purely additive and best-effort:
//   - It never affects the send result (fired non-blocking from the handler).
//   - It only READS (StateSearchMsg); it never publishes or mutates state.
//   - A nil warmer (hook unset) means the old behavior, unchanged.
//   - It self-bounds: each warm goroutine stops on success, on the message
//     becoming un-findable past its lookback, or after maxWarmDuration.

import (
	"context"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/rpc/handlers"
)

const (
	// warmPollInterval is how often the background warmer re-runs
	// StateSearchMsg for an in-flight tx. Matches the chain's ~30s block
	// cadence loosely; a tx typically lands within 1-3 polls.
	warmPollInterval = 5 * time.Second

	// maxWarmDuration bounds a single tx's warming loop. A calibration/
	// mainnet tx that hasn't landed in this window is either dropped or
	// stuck (an #47 concern), not a Bitswap-availability problem, so we
	// stop warming and let the normal poll/bridge path handle it.
	maxWarmDuration = 10 * time.Minute

	// maxConcurrentWarms caps in-flight warm goroutines so a burst of
	// sends can't spawn unbounded work. The SP write->confirm loop only
	// has a handful of txs in flight at once; well past that we simply
	// skip warming (the receipt path still works, just without the warm
	// assist).
	maxConcurrentWarms = 64
)

// searchFn resolves a message CID, returning (found, err). It is the seam
// the warmer drives in a loop; production wires it to ChainAPI.StateSearchMsg
// (which pulls the needed blocks over Bitswap as a side effect). Tests inject
// a deterministic stand-in.
type searchFn func(ctx context.Context, msgCID cid.Cid) (found bool, err error)

// sendWarmer runs background StateSearchMsg polls for recently-sent txs to
// pre-warm their message/receipt blocks into the Bitswap cache (#50).
type sendWarmer struct {
	ctx    context.Context
	search searchFn

	// pollInterval / maxDuration / maxConcurrent are fields (not the bare
	// consts) so tests can drive the loop fast. Production uses the const
	// defaults via newSendWarmer.
	pollInterval  time.Duration
	maxDuration   time.Duration
	maxConcurrent int

	mu       sync.Mutex
	inFlight map[cid.Cid]struct{}
	active   int
}

// newSendWarmer builds a warmer bound to the daemon's root context. The
// chainAPI must be the same handler the RPC server uses (so StateSearchMsg
// reads through the embedded Bitswap-backed BlockGetter).
func newSendWarmer(ctx context.Context, chain *handlers.ChainAPI) *sendWarmer {
	w := newSendWarmerWithSearch(ctx, func(sctx context.Context, msgCID cid.Cid) (bool, error) {
		lookup, err := chain.StateSearchMsg(sctx, types.TipSetKey{}, msgCID, 0, false)
		return lookup != nil, err
	})
	// A nil chain means "no real handler"; mark the warmer inert so Warm
	// is a no-op (mirrors the old `w.chain == nil` guard).
	if chain == nil {
		w.search = nil
	}
	return w
}

// newSendWarmerWithSearch is the testable constructor: it takes the search
// seam directly.
func newSendWarmerWithSearch(ctx context.Context, search searchFn) *sendWarmer {
	return &sendWarmer{
		ctx:           ctx,
		search:        search,
		pollInterval:  warmPollInterval,
		maxDuration:   maxWarmDuration,
		maxConcurrent: maxConcurrentWarms,
		inFlight:      make(map[cid.Cid]struct{}),
	}
}

// Warm starts a background warming loop for msgCID. Safe to call from the
// send hot path: it returns immediately and dedups concurrent calls for the
// same message. A no-op if the warmer or its chain handle is nil.
func (w *sendWarmer) Warm(msgCID cid.Cid) {
	if w == nil || w.search == nil || !msgCID.Defined() {
		return
	}

	w.mu.Lock()
	if _, dup := w.inFlight[msgCID]; dup {
		w.mu.Unlock()
		return
	}
	if w.active >= w.maxConcurrent {
		// Over budget: skip warming. The receipt path still resolves the
		// tx (just without the warm assist), so this only loses the
		// optimization under heavy burst, never correctness.
		w.mu.Unlock()
		return
	}
	w.inFlight[msgCID] = struct{}{}
	w.active++
	w.mu.Unlock()

	go w.run(msgCID)
}

func (w *sendWarmer) run(msgCID cid.Cid) {
	defer func() {
		w.mu.Lock()
		delete(w.inFlight, msgCID)
		w.active--
		w.mu.Unlock()
	}()

	deadline := time.Now().Add(w.maxDuration)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		// Each search gets a bounded child context so a single hung
		// Bitswap round can't pin the goroutine; the standalone budget
		// (well above the StateSearchMsg internal 18s retry window) is
		// what makes this a real warm rather than a client-deadline race.
		sctx, cancel := context.WithTimeout(w.ctx, warmSearchBudget)
		found, err := w.search(sctx, msgCID)
		cancel()

		switch {
		case err != nil:
			// Transient (e.g. a block still uncached). Keep polling; the
			// whole point is to drive Bitswap to fetch it.
		case found:
			// Landed and resolved locally: blocks are now warm, the
			// client's receipt poll will hit cache. Done.
			return
		}

		if time.Now().After(deadline) {
			return
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// warmSearchBudget is the per-attempt ceiling for a background warm search.
// It must comfortably exceed StateSearchMsg's internal retry window (18s)
// so a warm attempt contains a full set of Bitswap rounds rather than being
// cut short the way a client's short receipt-poll deadline would be.
const warmSearchBudget = 25 * time.Second
