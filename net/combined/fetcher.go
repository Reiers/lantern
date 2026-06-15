// Combined cache + bitswap + HTTP fallback BlockGetter.

package combined

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// Cache is the local-cache half of a combined fetcher. It must support
// CID-keyed reads and writes. The state/cache package satisfies this in
// production; tests use hamt.MemBlockStore.
type Cache interface {
	hamt.BlockGetter
	Put(c cid.Cid, raw []byte) cid.Cid
}

// Source is any networked BlockGetter (Bitswap, HTTP gateway).
//
// Race=true sources are fired in parallel; the first successful response
// wins and the rest are abandoned. Race=false sources are tried
// sequentially AFTER all Race sources have failed.
//
// Use Race=true for fast, low-cost alternative paths to the same blocks
// (e.g. our HTTP gateway and Bitswap both serve cold IPLD blocks).
// Use Race=false for last-resort fallbacks with different semantics or
// cost (e.g. Glif RPC, which is a public service and has a fee structure).
type Source struct {
	Name    string
	Getter  hamt.BlockGetter
	Timeout time.Duration
	Race    bool
}

// Fetcher wires Cache + N Sources into the standard cache-first fallback
// chain. Sources are tried in order; the first successful CID-matching
// response wins. Successful responses are written into Cache.
type Fetcher struct {
	cache   Cache
	sources []Source

	cacheHits   atomic.Uint64
	sourceHits  map[string]*atomic.Uint64
	totalMisses atomic.Uint64

	mu sync.Mutex // protects sourceHits map writes + sources slice mutation
}

// AddSource appends a block source at runtime (thread-safe). Used by the
// embedded daemon to mount the libp2p Bitswap source once the host is up
// (lantern#50): on calibration the gateway+glif sources both point at
// Glif, so without a p2p source a bridge-off daemon has no non-Glif way to
// fetch message/receipt blocks. prepend=true puts it ahead of existing
// sources (so p2p is tried before the HTTP fallbacks). Idempotent on Name.
func (f *Fetcher) AddSource(s Source, prepend bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ex := range f.sources {
		if ex.Name == s.Name {
			return // already present
		}
	}
	next := make([]Source, 0, len(f.sources)+1)
	if prepend {
		next = append(next, s)
		next = append(next, f.sources...)
	} else {
		next = append(next, f.sources...)
		next = append(next, s)
	}
	f.sources = next
	if _, ok := f.sourceHits[s.Name]; !ok {
		var c atomic.Uint64
		f.sourceHits[s.Name] = &c
	}
}

// snapshotSources returns a stable copy of the sources slice for a single
// Get, so a concurrent AddSource can't race the iteration.
func (f *Fetcher) snapshotSources() []Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sources
}

// New constructs a Fetcher.
func New(cache Cache, sources ...Source) *Fetcher {
	f := &Fetcher{
		cache:      cache,
		sources:    sources,
		sourceHits: make(map[string]*atomic.Uint64),
	}
	for _, s := range sources {
		var c atomic.Uint64
		f.sourceHits[s.Name] = &c
	}
	return f
}

// Get implements hamt.BlockGetter. Cache-first; then race-tier sources
// in parallel; then sequential fallback sources. The first CID-matching
// response wins, is cached, and the rest are abandoned via ctx cancel.
//
// Issue #3: racing the gateway against Bitswap cuts cold-block latency
// from "5s Bitswap timeout + gateway fetch" to "gateway fetch (~100ms)
// OR Bitswap fast deadline", whichever fires first. State-tree walks
// that previously took 30s+ now complete in low single seconds.
func (f *Fetcher) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	// 1. Cache.
	if f.cache != nil {
		if raw, err := f.cache.Get(ctx, c); err == nil {
			f.cacheHits.Add(1)
			return raw, nil
		}
	}

	// 2. Race tier: fire every Race=true source concurrently. First
	//    successful CID-verified response wins, others are cancelled.
	srcs := f.snapshotSources()
	race := make([]Source, 0, len(srcs))
	fallback := make([]Source, 0, len(srcs))
	for _, s := range srcs {
		if s.Race {
			race = append(race, s)
		} else {
			fallback = append(fallback, s)
		}
	}

	var firstErr error
	if len(race) > 0 {
		raw, name, err := f.raceFetch(ctx, c, race)
		if err == nil {
			f.recordHit(c, raw, name)
			return raw, nil
		}
		firstErr = err
	}

	// 3. Sequential fallback tier.
	for _, s := range fallback {
		raw, err := f.fetchOne(ctx, c, s)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		f.recordHit(c, raw, s.Name)
		return raw, nil
	}

	f.totalMisses.Add(1)
	if firstErr == nil {
		firstErr = errors.New("no source returned the block")
	}
	return nil, firstErr
}

// raceFetch fires `sources` concurrently with their per-source timeouts.
// Returns the first successful CID-verified response, with the name of
// the source that served it. On total failure returns the first error
// observed (best-effort; the goroutines may have produced different errors).
func (f *Fetcher) raceFetch(ctx context.Context, c cid.Cid, sources []Source) ([]byte, string, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		raw  []byte
		name string
		err  error
	}
	out := make(chan result, len(sources))
	for _, s := range sources {
		go func(s Source) {
			sctx := rctx
			if s.Timeout > 0 {
				var tcancel context.CancelFunc
				sctx, tcancel = context.WithTimeout(rctx, s.Timeout)
				defer tcancel()
			}
			raw, err := s.Getter.Get(sctx, c)
			if err != nil {
				out <- result{nil, s.Name, err}
				return
			}
			if vErr := hamt.VerifyBlockCID(c, raw); vErr != nil {
				out <- result{nil, s.Name, vErr}
				return
			}
			out <- result{raw, s.Name, nil}
		}(s)
	}

	var firstErr error
	for i := 0; i < len(sources); i++ {
		r := <-out
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		// Got one. Cancel the rest (defer cancel does this).
		return r.raw, r.name, nil
	}
	if firstErr == nil {
		firstErr = errors.New("race: no source returned the block")
	}
	return nil, "", firstErr
}

// fetchOne does a single sequential fetch with per-source timeout + CID verify.
func (f *Fetcher) fetchOne(ctx context.Context, c cid.Cid, s Source) ([]byte, error) {
	sctx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	raw, err := s.Getter.Get(sctx, c)
	if err != nil {
		return nil, err
	}
	if vErr := hamt.VerifyBlockCID(c, raw); vErr != nil {
		return nil, vErr
	}
	return raw, nil
}

// recordHit caches the block (if cache present) and bumps the source-hit
// counter for `name`.
func (f *Fetcher) recordHit(c cid.Cid, raw []byte, name string) {
	if f.cache != nil {
		f.cache.Put(c, raw)
	}
	f.mu.Lock()
	if counter, ok := f.sourceHits[name]; ok {
		counter.Add(1)
	}
	f.mu.Unlock()
}

// Stats returns a snapshot of fetch counters by source.
func (f *Fetcher) Stats() map[string]uint64 {
	out := map[string]uint64{
		"cache":  f.cacheHits.Load(),
		"misses": f.totalMisses.Load(),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, c := range f.sourceHits {
		out[name] = c.Load()
	}
	return out
}
