// Read-burst performance layer for local eth_call (Curio-PDP).
//
// A stock Curio-PDP node fires its whole read set at the chain node the
// moment a new head lands: for every proving data set an eth_call to
// PDPVerifier (getNextChallengeEpoch, getDataSetLeafCount,
// getChallengeRange, ...) plus FWSS views, receipt polls and block reads.
// With ~30 data sets that is ~100 concurrent eth_calls against the SAME
// freshly-advanced state root. Before this layer every one of them
// independently re-walked the state tree from the root: Init-actor
// AddressMap HAMT, state-tree HAMT, EVM actor head, the full bytecode blob
// (CID + keccak verified), and one KAMT descent per SLOAD, each block read
// going through the persistent block cache (a Badger read plus an
// asynchronous Badger write to bump LRU access time). Cold blocks for the
// new state root were fetched once per caller instead of once per node.
//
// Everything cached here is keyed by content address or by the execution
// state identity, so nothing can go stale across tipsets:
//
//   - blocks:    CID -> verified bytes (content addressed, valid forever)
//   - actors:    (state root, address) -> actor (immutable for that root)
//   - evmStates: actor head CID -> decoded EVM state
//   - code:      bytecode CID -> verified bytecode
//   - slots:     (storage root, slot) -> value
//   - results:   (execution tipset, state root, from, to, data) -> result,
//     bounded to the two most recent execution states
//
// Concurrent identical work is collapsed with singleflight: one block
// fetch per CID, one actor resolution per (root, address), and one
// interpreter run per identical eth_call. Shared work runs on a context
// detached from the first caller (bounded by its own timeout) so one
// client giving up never fails the others, and the result still lands in
// the cache for the next poll.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-address"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/holiman/uint256"
	"github.com/ipfs/go-cid"
	"golang.org/x/sync/singleflight"

	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/hamt"
)

const (
	perfBlockEntries    = 32768            // ~ tens of MiB of HAMT/KAMT nodes
	perfBlockMaxSize    = 256 << 10        // don't pin huge blocks in memory
	perfActorEntries    = 4096             // (root, addr) pairs; a few heads' worth
	perfEVMStateEntries = 4096             // decoded EVM actor heads
	perfCodeEntries     = 256              // distinct contract bytecodes
	perfSlotEntries     = 65536            // (storage root, slot) values
	perfF4IDEntries     = 4096             // delegated (f410) address -> actor ID
	perfResultGens      = 2                // execution states kept in the result cache
	perfResultsPerGen   = 16384            // results kept per execution state
	perfSharedTimeout   = 30 * time.Second // bound on detached shared work
)

// ethCallPerf holds the shared caches for one ChainAPI.
type ethCallPerf struct {
	blocks    atomic.Pointer[sharedBlockGetter] // swapped on BlockGetter rebind
	actors    *lru.Cache[actorCacheKey, actorCacheEntry]
	evmStates *lru.Cache[evmStateKey, actors.EVMState]
	code      *lru.Cache[cid.Cid, []byte]
	slots     *lru.Cache[slotCacheKey, uint256.Int]
	results   *ethResultCache

	// f4ids remembers which actor ID a delegated (f410) address resolved
	// to. It lets a call skip the Init actor AddressMap walk (state tree
	// path to the Init actor, its state, then the AddressMap HAMT: about
	// a third of the dependent block fetches of a cold PDPVerifier call)
	// on every new state root. An entry is only a hint: it is used only
	// when the actor at that ID in the CURRENT state carries exactly this
	// delegated address, which identifies the same actor the Init actor
	// lookup would return. Any mismatch or miss falls back to the full
	// lookup, so a stale hint can cost time but never correctness.
	f4ids *lru.Cache[string, address.Address]

	actorFlight singleflight.Group
	callFlight  singleflight.Group

	warm        *headWarmer // ethcall_warm.go
	warmPasses  atomic.Uint64
	warmedSlots atomic.Uint64

	resultHits   atomic.Uint64
	resultMisses atomic.Uint64
	callShared   atomic.Uint64 // callers that received a result shared with other callers
	executions   atomic.Uint64 // interpreter runs on the live-head path
}

type actorCacheKey struct {
	root cid.Cid
	addr string // address.Address bytes
}

type actorCacheEntry struct {
	actor    *accessor.Actor
	notFound bool
}

// evmStateKey identifies a decoded EVM actor state: the code CID selects
// the actors version used to decode the head block.
type evmStateKey struct {
	code, head cid.Cid
}

type slotCacheKey struct {
	root cid.Cid
	slot [32]byte
}

func newEthCallPerf(inner hamt.BlockGetter) *ethCallPerf {
	p := &ethCallPerf{
		results: newEthResultCache(perfResultGens, perfResultsPerGen),
		warm:    newHeadWarmer(),
	}
	p.blocks.Store(newSharedBlockGetter(inner, perfBlockEntries))
	p.actors, _ = lru.New[actorCacheKey, actorCacheEntry](perfActorEntries)
	p.evmStates, _ = lru.New[evmStateKey, actors.EVMState](perfEVMStateEntries)
	p.code, _ = lru.New[cid.Cid, []byte](perfCodeEntries)
	p.slots, _ = lru.New[slotCacheKey, uint256.Int](perfSlotEntries)
	p.f4ids, _ = lru.New[string, address.Address](perfF4IDEntries)
	return p
}

// ethPerf returns the ChainAPI's shared eth_call caches, building them on
// first use over the current BlockGetter. If the BlockGetter is later
// swapped (rebindBlockGetter after Bitswap comes up), the block layer is
// rebuilt over the new source; the content-addressed caches stay valid.
func (c *ChainAPI) ethPerf() *ethCallPerf {
	c.ethPerfMu.Lock()
	defer c.ethPerfMu.Unlock()
	if c.ethPerfState == nil {
		p := newEthCallPerf(c.BlockGetter)
		c.ethPerfState = p
		if c.HeaderStore != nil {
			c.HeaderStore.OnHeadChange(func(ts *ltypes.TipSet) { c.warmOnHead(p, ts) })
		}
	} else if cur := c.ethPerfState.blocks.Load(); !sameBlockGetter(cur.inner, c.BlockGetter) {
		// Keep the (content-addressed) cached blocks; swap the source.
		c.ethPerfState.blocks.Store(&sharedBlockGetter{inner: c.BlockGetter, cache: cur.cache})
	}
	return c.ethPerfState
}

// blockGetter returns the shared block layer.
func (p *ethCallPerf) blockGetter() *sharedBlockGetter { return p.blocks.Load() }

// sameBlockGetter compares two BlockGetters by identity without panicking
// on non-comparable dynamic types (treated as different).
func sameBlockGetter(a, b hamt.BlockGetter) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// EthCallCacheStats is a snapshot of the eth_call burst caches.
type EthCallCacheStats struct {
	ResultHits, ResultMisses, CallsShared, Executions uint64
	BlockHits, BlockMisses, BlocksShared              uint64
	WarmPasses, WarmedSlots                           uint64
}

// EthCallCacheStats reports the eth_call cache counters (zero before first use).
func (c *ChainAPI) EthCallCacheStats() EthCallCacheStats {
	c.ethPerfMu.Lock()
	p := c.ethPerfState
	c.ethPerfMu.Unlock()
	if p == nil {
		return EthCallCacheStats{}
	}
	return EthCallCacheStats{
		ResultHits:   p.resultHits.Load(),
		ResultMisses: p.resultMisses.Load(),
		CallsShared:  p.callShared.Load(),
		Executions:   p.executions.Load(),
		BlockHits:    p.blockGetter().hits.Load(),
		BlockMisses:  p.blockGetter().misses.Load(),
		BlocksShared: p.blockGetter().shared.Load(),
		WarmPasses:   p.warmPasses.Load(),
		WarmedSlots:  p.warmedSlots.Load(),
	}
}

// sharedDo runs fn once per key across concurrent callers. fn gets a
// context detached from any single caller (bounded by perfSharedTimeout);
// each caller waits for the shared result or its own ctx, whichever comes
// first. shared reports whether this caller joined an in-flight call.
func sharedDo[T any](ctx context.Context, g *singleflight.Group, key string, fn func(context.Context) (T, error)) (v T, shared bool, err error) {
	ch := g.DoChan(key, func() (any, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), perfSharedTimeout)
		defer cancel()
		return fn(sctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return v, r.Shared, r.Err
		}
		return r.Val.(T), r.Shared, nil
	case <-ctx.Done():
		return v, false, ctx.Err()
	}
}

// ---- block layer ----------------------------------------------------------

// sharedBlockGetter is a content-addressed in-memory block cache with
// per-CID singleflight in front of the daemon's BlockGetter. Bytes are
// CID-verified before they are cached, so a hit is exactly the block the
// CID names.
type sharedBlockGetter struct {
	inner  hamt.BlockGetter
	cache  *lru.Cache[cid.Cid, []byte]
	flight singleflight.Group

	hits, misses, shared atomic.Uint64
}

func newSharedBlockGetter(inner hamt.BlockGetter, entries int) *sharedBlockGetter {
	c, _ := lru.New[cid.Cid, []byte](entries)
	return &sharedBlockGetter{inner: inner, cache: c}
}

// Get implements hamt.BlockGetter.
func (s *sharedBlockGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if raw, ok := s.cache.Get(c); ok {
		s.hits.Add(1)
		return raw, nil
	}
	s.misses.Add(1)
	if s.inner == nil {
		return nil, errors.New("sharedBlockGetter: nil inner")
	}
	raw, shared, err := sharedDo(ctx, &s.flight, c.KeyString(), func(fctx context.Context) ([]byte, error) {
		// Re-check: a flight for c may have completed between our cache
		// miss and joining the group.
		if raw, ok := s.cache.Get(c); ok {
			return raw, nil
		}
		raw, err := s.inner.Get(fctx, c)
		if err != nil {
			return nil, err
		}
		if err := hamt.VerifyBlockCID(c, raw); err != nil {
			return nil, err
		}
		if len(raw) <= perfBlockMaxSize {
			s.cache.Add(c, raw)
		}
		return raw, nil
	})
	if shared {
		s.shared.Add(1)
	}
	return raw, err
}

// ---- state-object caches used by evmBackend --------------------------------

// getActorCached resolves filAddr at stateRoot through the actor cache,
// collapsing concurrent resolutions of the same (root, address). Only
// definitive answers are cached: a found actor or "address not found".
// Fetch/decode faults are returned uncached so the next call retries.
func (p *ethCallPerf) getActorCached(ctx context.Context, acc *accessor.Accessor, stateRoot cid.Cid, filAddr address.Address) (*accessor.Actor, error) {
	key := actorCacheKey{root: stateRoot, addr: string(filAddr.Bytes())}
	if e, ok := p.actors.Get(key); ok {
		if e.notFound {
			return nil, accessor.ErrAddressNotFound
		}
		return e.actor, nil
	}
	e, _, err := sharedDo(ctx, &p.actorFlight, stateRoot.KeyString()+key.addr, func(fctx context.Context) (actorCacheEntry, error) {
		if e, ok := p.actors.Get(key); ok {
			return e, nil
		}
		actor, err := p.resolveActor(fctx, acc, filAddr)
		if err != nil {
			if isActorNotFound(err) {
				e := actorCacheEntry{notFound: true}
				p.actors.Add(key, e)
				return e, nil
			}
			return actorCacheEntry{}, err
		}
		e := actorCacheEntry{actor: actor}
		p.actors.Add(key, e)
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	if e.notFound {
		return nil, accessor.ErrAddressNotFound
	}
	return e.actor, nil
}

// resolveActor is acc.GetActor with the verified f410 -> ID shortcut.
func (p *ethCallPerf) resolveActor(ctx context.Context, acc *accessor.Accessor, filAddr address.Address) (*accessor.Actor, error) {
	if filAddr.Protocol() != address.Delegated {
		actor, _, err := acc.GetActor(ctx, filAddr)
		return actor, err
	}
	k := string(filAddr.Bytes())
	if id, ok := p.f4ids.Get(k); ok {
		actor, _, err := acc.GetActorByID(ctx, id)
		if err == nil && actor.DelegatedAddress != nil && *actor.DelegatedAddress == filAddr {
			return actor, nil
		}
		p.f4ids.Remove(k)
		// Stale or unverifiable hint: fall through to the full lookup.
	}
	id, _, err := acc.LookupID(ctx, filAddr)
	if err != nil {
		return nil, err
	}
	actor, _, err := acc.GetActorByID(ctx, id)
	if err != nil {
		return nil, err
	}
	p.f4ids.Add(k, id)
	return actor, nil
}

// ---- per-execution-state result cache --------------------------------------

// ethCallOutcome is a definitive local eth_call answer: a return value or
// a revert. Faults (served == false) are never cached.
type ethCallOutcome struct {
	served bool
	ret    string
	reason string
	err    error // *revertError for a revert, nil otherwise
}

// ethResultCache keeps eth_call outcomes for the most recent execution
// states. A generation is the identity of the state a call executes on
// (execution tipset key + state root); a call can only hit an entry from
// the exact same generation, so a new head can never be served a result
// computed on an older state.
type ethResultCache struct {
	mu     sync.Mutex
	maxGen int
	maxPer int
	gens   []*resultGen // oldest first
}

type resultGen struct {
	id string
	m  map[string]ethCallOutcome
}

func newEthResultCache(maxGen, maxPer int) *ethResultCache {
	return &ethResultCache{maxGen: maxGen, maxPer: maxPer}
}

func (r *ethResultCache) get(gen, key string) (ethCallOutcome, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.gens {
		if g.id == gen {
			o, ok := g.m[key]
			return o, ok
		}
	}
	return ethCallOutcome{}, false
}

func (r *ethResultCache) put(gen, key string, o ethCallOutcome) {
	if !o.served {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var g *resultGen
	for _, x := range r.gens {
		if x.id == gen {
			g = x
			break
		}
	}
	if g == nil {
		g = &resultGen{id: gen, m: make(map[string]ethCallOutcome)}
		r.gens = append(r.gens, g)
		if len(r.gens) > r.maxGen {
			r.gens = r.gens[len(r.gens)-r.maxGen:]
		}
	}
	if len(g.m) >= r.maxPer {
		return
	}
	g.m[key] = o
}

// generations reports the cached generation ids, oldest first (tests).
func (r *ethResultCache) generations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.gens))
	for i, g := range r.gens {
		out[i] = g.id
	}
	return out
}

func ethCallGen(execKey string, stateRoot cid.Cid) string {
	return fmt.Sprintf("%s|%s", execKey, stateRoot)
}
