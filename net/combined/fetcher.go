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
type Source struct {
	Name    string
	Getter  hamt.BlockGetter
	Timeout time.Duration
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

	mu sync.Mutex // protects sourceHits map writes
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

// Get implements hamt.BlockGetter. It tries the cache, then each Source in
// order, returning the first CID-matching response and caching it on the
// way back.
func (f *Fetcher) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	// 1. Cache.
	if f.cache != nil {
		if raw, err := f.cache.Get(ctx, c); err == nil {
			f.cacheHits.Add(1)
			return raw, nil
		}
	}
	// 2. Each source in order.
	var firstErr error
	for _, s := range f.sources {
		sctx := ctx
		if s.Timeout > 0 {
			var cancel context.CancelFunc
			sctx, cancel = context.WithTimeout(ctx, s.Timeout)
			defer cancel()
		}
		raw, err := s.Getter.Get(sctx, c)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// CID verify.
		if vErr := hamt.VerifyBlockCID(c, raw); vErr != nil {
			if firstErr == nil {
				firstErr = vErr
			}
			continue
		}
		// Cache it.
		if f.cache != nil {
			f.cache.Put(c, raw)
		}
		f.mu.Lock()
		f.sourceHits[s.Name].Add(1)
		f.mu.Unlock()
		return raw, nil
	}
	f.totalMisses.Add(1)
	if firstErr == nil {
		firstErr = errors.New("no source returned the block")
	}
	return nil, firstErr
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
