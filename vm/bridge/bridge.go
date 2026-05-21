// Bridge interface + caching wrapper.

package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// Bridge is the operator-pluggable VM bridge. Implementations call out
// to an upstream Forest/Lotus (or a private FVM service) to compute the
// post-execution state root + receipts for a base + message list.
//
// Implementations MUST be safe for concurrent use.
type Bridge interface {
	// ComputeStateRoot applies `msgs` against `base` (the parent
	// tipset's stateRoot) at the given epoch and returns the resulting
	// state root + per-message receipts.
	//
	// The returned receipts MUST be in the same order as `msgs`.
	//
	// `epoch` lets the upstream node pick the right network version /
	// actor version table. Callers should pass the epoch of the parent
	// tipset (i.e. the epoch at which messages execute).
	//
	// `ctx` cancellation propagates to the upstream RPC call.
	ComputeStateRoot(ctx context.Context, base cid.Cid, epoch int64, msgs []*types.Message) (cid.Cid, []*types.MessageReceipt, error)

	// Provenance returns a short, opaque identifier for the upstream
	// node serving this bridge — typically "forest@<host>" or
	// "lotus@<host>". Used for trace logging only; not security-bearing.
	Provenance() string
}

// CachingBridge wraps a Bridge with a small LRU keyed by
// (base, epoch, msgs digest). Repeated StateCall against the same
// base + message list short-circuits to the cached result.
type CachingBridge struct {
	inner Bridge
	mu    sync.Mutex
	cache map[string]cacheEntry
	max   int
	order []string // simple FIFO; LRU would be nicer but overkill at <1k entries
}

type cacheEntry struct {
	root     cid.Cid
	receipts []*types.MessageReceipt
}

// NewCachingBridge returns a CachingBridge with the given upper bound on
// cache entries. 1024 is a generous default for V1.
func NewCachingBridge(b Bridge, maxEntries int) *CachingBridge {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &CachingBridge{
		inner: b,
		max:   maxEntries,
		cache: make(map[string]cacheEntry, maxEntries),
	}
}

// Provenance proxies to the underlying bridge.
func (c *CachingBridge) Provenance() string {
	return c.inner.Provenance() + "+cache"
}

// ComputeStateRoot is the cached variant.
func (c *CachingBridge) ComputeStateRoot(ctx context.Context, base cid.Cid, epoch int64, msgs []*types.Message) (cid.Cid, []*types.MessageReceipt, error) {
	k := cacheKey(base, epoch, msgs)

	c.mu.Lock()
	if e, ok := c.cache[k]; ok {
		c.mu.Unlock()
		// Return a deep copy of the receipts slice so callers can
		// mutate without poisoning the cache.
		out := make([]*types.MessageReceipt, len(e.receipts))
		for i, r := range e.receipts {
			cp := *r
			out[i] = &cp
		}
		return e.root, out, nil
	}
	c.mu.Unlock()

	root, recs, err := c.inner.ComputeStateRoot(ctx, base, epoch, msgs)
	if err != nil {
		return cid.Undef, nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= c.max && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.cache, oldest)
	}
	// Store a defensive copy.
	stored := make([]*types.MessageReceipt, len(recs))
	for i, r := range recs {
		cp := *r
		stored[i] = &cp
	}
	c.cache[k] = cacheEntry{root: root, receipts: stored}
	c.order = append(c.order, k)
	return root, recs, nil
}

// cacheKey is a stable hash over (base, epoch, msgs) suitable for an
// in-memory cache. We use a SHA-256 of the canonical CBOR-encoded
// message slice plus the base CID bytes.
func cacheKey(base cid.Cid, epoch int64, msgs []*types.Message) string {
	h := sha256.New()
	h.Write(base.Bytes())
	// Mix epoch in little-endian.
	var ebuf [8]byte
	for i := 0; i < 8; i++ {
		ebuf[i] = byte(epoch >> (i * 8))
	}
	h.Write(ebuf[:])
	// Sort by CID for deterministic ordering on identical sets.
	cids := make([]cid.Cid, 0, len(msgs))
	cidToMsg := make(map[cid.Cid]*types.Message, len(msgs))
	for _, m := range msgs {
		c := m.Cid()
		cids = append(cids, c)
		cidToMsg[c] = m
	}
	sort.Slice(cids, func(i, j int) bool { return cids[i].KeyString() < cids[j].KeyString() })
	for _, c := range cids {
		h.Write(c.Bytes())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ErrNoBridge is returned from helper code paths when a bridge was
// expected but not configured.
var ErrNoBridge = errors.New("vm/bridge: no bridge configured (StateCall beyond Send + post-execution state root require a bridge — see TRUST-MODEL.md)")
