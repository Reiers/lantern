// KAMT subtree walker (lantern#44).
//
// WalkSubtree performs a bounded breadth-first traversal of a KAMT
// rooted at `root`, fetching every node CID it visits through the
// supplied BlockGetter. Its purpose is **block availability**: when
// `bg` is a cache-fronted fetcher (combined.Fetcher), walking the
// subtree pulls the storage-trie nodes into the local cache so that a
// later eth_call's KAMT lookup hits the cache rather than Bitswap.
//
// The walker is intentionally narrow: it does NOT decode values, it
// does NOT verify keys, it does NOT enforce extensions beyond reading
// past them. All it does is "fetch every reachable IPLD node, up to a
// bound, and return how many we walked". The eth_call read path
// (kamt.Get) still does the real cryptographic descent + verification.
package kamt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// WalkStats is the per-walk summary returned by WalkSubtree.
type WalkStats struct {
	NodesFetched int   // total IPLD nodes successfully fetched
	BytesFetched int64 // total raw block bytes
	Errors       int   // node-fetch / decode errors encountered (BFS continues past these)
	Capped       bool  // true if MaxNodes was hit before the walk finished
}

// WalkOptions controls a WalkSubtree run.
type WalkOptions struct {
	// MaxNodes caps the total number of nodes visited. Zero or negative
	// means "no cap" — caller is responsible for bounding work via ctx.
	MaxNodes int

	// FetchRetries is the number of additional attempts made for a node
	// whose first fetch fails, before the walker gives up on it (and the
	// subtree below it). A transient Bitswap/gateway miss during a walk
	// otherwise leaves a permanent hole in cache coverage: the failed
	// node's children are never queued, so a later eth_call SLOAD into
	// that subtree misses and falls back to the bridge. Retrying closes
	// that gap for contracts with large/deep storage tries (e.g.
	// FilecoinPay) where the read path is fine but the bulk walk races
	// the network. Zero means no retry (legacy best-effort behaviour).
	FetchRetries int

	// RetryBackoff is the base delay between fetch retries (linear:
	// attempt n waits n*RetryBackoff). Zero defaults to 150ms when
	// FetchRetries > 0.
	RetryBackoff time.Duration

	// OnNode, if set, is called once for each successfully fetched and
	// CID-verified node in the subtree (root included). The PDP tier uses
	// this to PIN the walked contract-state nodes in the persistent block
	// cache, so the warm set the node proves/settles against is never
	// LRU-evicted. Must be cheap and non-blocking; it runs inline on the
	// walk goroutine. Nil disables (light-node behaviour).
	OnNode func(cid.Cid)
}

// WalkSubtree walks the KAMT rooted at `root` breadth-first through bg,
// visiting at most opts.MaxNodes nodes. It returns when the BFS frontier
// is empty, the cap is hit, or ctx is done.
//
// Errors from individual node fetches are counted but do not abort the
// walk: the goal is best-effort cache-warming, not exhaustiveness.
// Returns an error only if root itself is undefined or ctx fails before
// any node is visited.
func WalkSubtree(ctx context.Context, root cid.Cid, bg hamt.BlockGetter, opts WalkOptions) (WalkStats, error) {
	var stats WalkStats
	if bg == nil {
		return stats, errors.New("kamt.WalkSubtree: nil BlockGetter")
	}
	if !root.Defined() {
		return stats, errors.New("kamt.WalkSubtree: undefined root")
	}

	// BFS queue + visited set (KAMT subtrees can in principle share
	// nodes; in practice they don't, but the dedup is cheap and keeps
	// the walker correct under future encoding changes).
	queue := []cid.Cid{root}
	visited := make(map[string]struct{}, 64)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			if stats.NodesFetched == 0 {
				return stats, err
			}
			return stats, nil
		}
		if opts.MaxNodes > 0 && stats.NodesFetched >= opts.MaxNodes {
			stats.Capped = true
			return stats, nil
		}

		cur := queue[0]
		queue = queue[1:]
		key := cur.KeyString()
		if _, seen := visited[key]; seen {
			continue
		}
		visited[key] = struct{}{}

		raw, err := getWithRetry(ctx, bg, cur, opts)
		if err != nil {
			stats.Errors++
			continue
		}
		// Defensive CID check (the inner BlockGetter should already do
		// this; doing it again is cheap on the prefetch path and means
		// a misbehaving source can't poison our cache via the walker).
		if err := hamt.VerifyBlockCID(cur, raw); err != nil {
			stats.Errors++
			continue
		}
		stats.NodesFetched++
		stats.BytesFetched += int64(len(raw))
		if opts.OnNode != nil {
			opts.OnNode(cur)
		}

		n, err := decodeNode(raw)
		if err != nil {
			stats.Errors++
			continue
		}
		for _, p := range n.pointers {
			if p.isValues {
				continue // leaves: no children
			}
			if !p.link.Defined() {
				continue
			}
			queue = append(queue, p.link)
		}
	}
	return stats, nil
}

// getWithRetry fetches a single node, retrying transient failures up to
// opts.FetchRetries times with linear backoff. The combined fetcher
// already races cache+Bitswap+gateway per attempt, so a retry mostly
// buys time for a cold block to land. Honours ctx cancellation between
// attempts.
func getWithRetry(ctx context.Context, bg hamt.BlockGetter, c cid.Cid, opts WalkOptions) ([]byte, error) {
	backoff := opts.RetryBackoff
	if backoff <= 0 {
		backoff = 150 * time.Millisecond
	}
	attempts := opts.FetchRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i) * backoff):
			}
		}
		raw, err := bg.Get(ctx, c)
		if err == nil {
			return raw, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// debugFormatStats is here so callers (and tests) can print a one-line
// summary without a custom Stringer.
func (s WalkStats) String() string {
	return fmt.Sprintf("nodes=%d bytes=%d errors=%d capped=%v",
		s.NodesFetched, s.BytesFetched, s.Errors, s.Capped)
}
