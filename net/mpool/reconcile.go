package mpool

// #47: pending -> confirm -> retry reconcile loop.
//
// Lantern's mpool was fire-and-forget: Publish pushed once and tracked the
// CID, but nothing ever re-published or checked inclusion. A message that
// was gossiped but never mined (e.g. transient mesh issue) would stall
// silently, and because the sender's nonce is consumed in their view,
// every later send from that account stalls behind it.
//
// Reconcile, driven by the head-change tick the embedded daemon already
// has, closes that gap:
//   - confirmed on chain  -> drop from pending (done),
//   - unseen, age < window -> wait (confidence window),
//   - unseen, age >= window -> REBROADCAST identical signed bytes (same
//     nonce, same CID; idempotent, exactly what Lotus does),
//   - max retries exhausted -> mark failed + fire OnFailed (never silently
//     stuck).
//
// SAFETY: retry re-gossips the SAME signed message. We do NOT re-sign and
// we do NOT bump the fee on a timer (RBF is a separate, gated, opt-in path,
// out of scope here). A nonce executes at most once on chain, so
// re-gossiping identical bytes can never double-spend.

import (
	"context"

	"github.com/ipfs/go-cid"
)

// SearchResult reports whether a message was found on chain.
type SearchResult int

const (
	// SearchUnknown: not found (yet) — keep waiting / retry per policy.
	SearchUnknown SearchResult = iota
	// SearchFound: confirmed on chain — drop from pending.
	SearchFound
)

// SearchFunc resolves whether a published message CID has landed on chain.
// The daemon wires this to ChainAPI.StateSearchMsg (already local + zero
// Glif). It must be safe to call without holding the pool lock.
type SearchFunc func(ctx context.Context, msgCID cid.Cid) (SearchResult, error)

// Reconcile runs one pass of the confirm/retry state machine at the given
// chain head epoch. Intended to be called once per new head. It never
// holds the pool lock across the SearchFunc call or a rebroadcast.
func (p *Pool) Reconcile(ctx context.Context, headEpoch int64, search SearchFunc) {
	if p == nil || search == nil {
		return
	}

	// Snapshot the pending set (cid + a copy of the bookkeeping) so we
	// don't hold the lock during network / search I/O.
	type item struct {
		cid cid.Cid
		raw []byte
		pm  pendingMsg
	}
	p.mu.Lock()
	items := make([]item, 0, len(p.pending))
	for c, pm := range p.pending {
		// First time we see it, anchor publishedAt to the current head so
		// the confidence window is measured from when we started watching.
		if pm.publishedAt == 0 {
			pm.publishedAt = headEpoch
			pm.lastActivity = headEpoch
		}
		items = append(items, item{cid: c, raw: pm.raw, pm: *pm})
	}
	p.mu.Unlock()

	for _, it := range items {
		res, err := search(ctx, it.cid)
		if err != nil {
			// Search failed (transient availability) — leave pending,
			// retry next head. Persist the anchored publishedAt.
			p.persistAnchor(it.cid, it.pm.publishedAt)
			continue
		}
		if res == SearchFound {
			p.mu.Lock()
			if _, ok := p.pending[it.cid]; ok {
				delete(p.pending, it.cid)
				p.confirmd++
			}
			p.mu.Unlock()
			log.Debugw("mpool: message confirmed on chain", "cid", it.cid)
			continue
		}

		// Not found. Within the confidence window? Wait.
		age := headEpoch - it.pm.publishedAt
		if age < p.cfg.ConfirmAfterEpochs {
			p.persistAnchor(it.cid, it.pm.publishedAt)
			continue
		}

		// Window elapsed. Max retries exhausted -> fail loudly.
		if p.cfg.MaxRetries >= 0 && it.pm.retries >= p.cfg.MaxRetries {
			p.mu.Lock()
			pm, ok := p.pending[it.cid]
			if ok {
				delete(p.pending, it.cid)
				p.failed++
			}
			p.mu.Unlock()
			if ok {
				log.Warnw("mpool: message gave up (max retries) — surfacing as failed",
					"cid", it.cid, "retries", pm.retries, "nonce", pm.sm.Message.Nonce)
				if p.cfg.OnFailed != nil {
					p.cfg.OnFailed(pm.sm, "max retries exhausted without on-chain inclusion")
				}
			}
			continue
		}

		// Rebroadcast the IDENTICAL bytes (same nonce, same CID). Not in
		// DryRun.
		if !p.cfg.DryRun && it.raw != nil {
			if err := p.topic.Publish(ctx, it.raw); err != nil {
				log.Warnw("mpool: rebroadcast publish failed; will retry next head", "cid", it.cid, "err", err)
				p.persistAnchor(it.cid, it.pm.publishedAt)
				continue
			}
		}
		p.mu.Lock()
		if pm, ok := p.pending[it.cid]; ok {
			pm.retries++
			pm.lastActivity = headEpoch
			pm.publishedAt = it.pm.publishedAt // keep original anchor; window resets via lastActivity if desired
			p.rebroad++
		}
		p.mu.Unlock()
		log.Debugw("mpool: rebroadcast pending message (identical bytes)", "cid", it.cid, "retries", it.pm.retries+1)
	}
}

// persistAnchor writes back the anchored publishedAt for a pending message
// if it's still present (it may have been confirmed/forgotten concurrently).
func (p *Pool) persistAnchor(c cid.Cid, anchor int64) {
	p.mu.Lock()
	if pm, ok := p.pending[c]; ok && pm.publishedAt == 0 {
		pm.publishedAt = anchor
		if pm.lastActivity == 0 {
			pm.lastActivity = anchor
		}
	}
	p.mu.Unlock()
}
