// Retrying BlockGetter wrapper for eth_call (lantern#44, "fetch-on-miss
// with retry").
//
// The eth_call backend walks the contract bytecode block + KAMT storage
// trie at the live head. When the embedded Bitswap blockstore hasn't yet
// fetched a storage-trie node at this head, a single Get failure today
// causes the whole eth_call to fall back to the VMBridge. That's correct
// but unnecessarily eager: the underlying combined fetcher has a 1.5s
// "fast" deadline + 5s "full" deadline, so a cold block under load can
// miss the first attempt while still being available a second or two
// later (or after a concurrent prefetch fills the cache).
//
// retryingBlockGetter wraps any BlockGetter and retries up to N times
// with a bounded total timeout, re-checking the underlying source each
// attempt. It is used ONLY for the eth_call backend (see evmexec.go),
// not the global accessor path — we don't want to inflate latency on
// the hot proof-loop reads.
package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// retryingBlockGetter wraps an inner BlockGetter with bounded retries.
type retryingBlockGetter struct {
	inner   hamt.BlockGetter
	retries int           // additional attempts AFTER the first try
	total   time.Duration // total budget across all attempts
	backoff time.Duration // sleep between attempts (capped at total/retries)

	// stats (lantern#44 counter A: KAMT/state miss bridge-fallback driver)
	attempts atomic.Uint64
	retried  atomic.Uint64
	succeeds atomic.Uint64
	fails    atomic.Uint64
}

// newRetryingBlockGetter constructs a retry wrapper. retries=0 or
// total<=0 disables retries (returns inner unchanged via Get's fast
// path).
func newRetryingBlockGetter(inner hamt.BlockGetter, retries int, total time.Duration) *retryingBlockGetter {
	bo := 250 * time.Millisecond
	if retries > 0 && total > 0 {
		if cap := total / time.Duration(retries+1); cap < bo {
			bo = cap
		}
	}
	return &retryingBlockGetter{
		inner:   inner,
		retries: retries,
		total:   total,
		backoff: bo,
	}
}

// Get fetches the block, retrying up to r.retries+1 attempts within
// r.total. The per-attempt context inherits the parent's deadline if
// it's tighter than the total budget.
func (r *retryingBlockGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if r.inner == nil {
		return nil, errors.New("retryingBlockGetter: nil inner")
	}
	// Fast path: no retry configured, transparent passthrough.
	if r.retries <= 0 || r.total <= 0 {
		r.attempts.Add(1)
		raw, err := r.inner.Get(ctx, c)
		if err != nil {
			r.fails.Add(1)
			return nil, err
		}
		r.succeeds.Add(1)
		return raw, nil
	}

	totalCtx, cancel := context.WithTimeout(ctx, r.total)
	defer cancel()

	var lastErr error
	maxAttempts := r.retries + 1
	for i := 0; i < maxAttempts; i++ {
		// Abort early if total budget already exhausted.
		if err := totalCtx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		r.attempts.Add(1)
		if i > 0 {
			r.retried.Add(1)
		}
		raw, err := r.inner.Get(totalCtx, c)
		if err == nil {
			r.succeeds.Add(1)
			return raw, nil
		}
		lastErr = err
		// Don't sleep after the last attempt.
		if i+1 < maxAttempts {
			select {
			case <-totalCtx.Done():
				// exit on next loop iteration check
			case <-time.After(r.backoff):
			}
		}
	}
	r.fails.Add(1)
	return nil, lastErr
}

// fetchStats is a snapshot of retry-wrapper counters.
type fetchStats struct {
	Attempts uint64
	Retried  uint64
	Succeeds uint64
	Fails    uint64
}

func (r *retryingBlockGetter) stats() fetchStats {
	return fetchStats{
		Attempts: r.attempts.Load(),
		Retried:  r.retried.Load(),
		Succeeds: r.succeeds.Load(),
		Fails:    r.fails.Load(),
	}
}
