// FEVM contract state prefetcher (lantern#44).
//
// On every chain head advance the prefetcher walks a configured set of
// EVM contract addresses and pulls their bytecode + storage-trie nodes
// into the local blockstore cache. The goal is "block availability for
// local eth_call": when curio-core (or filcensus) reads a contract
// shortly after a head advance, the read should hit the cache rather
// than fall back to Bitswap (or, with --vm-bridge-rpc-disable, fail).
//
// This is a best-effort warming step. It must NEVER block the
// head-advance path, NEVER affect the proof loop, and NEVER replace the
// authoritative read path (kamt.Get still does the cryptographic
// descent + verification on every eth_call).
package prefetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/builtin"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/state/kamt"
)

var log = logging.Logger("lantern/prefetch")

// Config controls a Prefetcher.
type Config struct {
	// Addrs is the list of 20-byte hex eth addresses to prefetch.
	// Strings are accepted in 0x-prefixed or bare form. Anything that
	// fails to parse is logged once and skipped.
	Addrs []string

	// MaxBlocksPerAddr caps the BFS node-count per address per run.
	// Default 256 (covers PDPVerifier + similar proxy-impl contracts
	// at their current depth with margin). 0 -> use default.
	MaxBlocksPerAddr int

	// PerAddrTimeout bounds one address's walk. Default 20s.
	PerAddrTimeout time.Duration

	// MinInterval coalesces rapid head advances: prefetch runs at most
	// once per MinInterval per address. Default 60s. 0 disables
	// coalescing (run every head advance).
	MinInterval time.Duration
}

// Pinner pins CIDs in the persistent block cache so they are never
// LRU-evicted. Satisfied by *state/cache.Store. Optional: nil on the
// memory-cached light node.
type Pinner interface {
	Pin(cid.Cid) error
}

// Prefetcher walks contract state subtrees through a BlockGetter on
// every Trigger() call.
type Prefetcher struct {
	cfg     Config
	bg      hamt.BlockGetter
	reg     *actors.Registry
	chainID uint64

	// pinner, when set (PDP tier), pins the walked nodes of STATIC
	// (configured) contracts so the warm PDP/payments/registry/USDFC
	// subtrees survive LRU eviction across restart. Dynamic (client-
	// learned) addresses are deliberately NOT pinned - that set is
	// attacker-influenceable and must stay evictable.
	pinner Pinner

	mu       sync.Mutex
	lastRun  map[string]time.Time // keyed by canonical eth-address (lowercase hex)
	inflight map[string]bool      // keyed by canonical eth-address; prevents overlap

	// dynAddrs holds addresses learned at runtime via AddAddr (lantern#44
	// adaptive warming): contracts an eth_call locally missed and fell
	// back to the bridge for. They're merged with cfg.Addrs on every
	// Trigger. Capped at maxDynAddrs to bound memory/walk cost.
	dynAddrs map[string]struct{} // canonical lowercase key -> present

	// stats
	runs           atomic.Uint64
	walks          atomic.Uint64
	skippedCoolDn  atomic.Uint64
	skippedInfligh atomic.Uint64
	blocksFetched  atomic.Uint64
	bytesFetched   atomic.Uint64
	errors         atomic.Uint64
}

// New constructs a Prefetcher. bg must be a cache-fronted BlockGetter
// (typically the combined fetcher) so walks actually populate the cache.
func New(cfg Config, bg hamt.BlockGetter) *Prefetcher {
	if cfg.MaxBlocksPerAddr <= 0 {
		cfg.MaxBlocksPerAddr = 256
	}
	if cfg.PerAddrTimeout <= 0 {
		cfg.PerAddrTimeout = 20 * time.Second
	}
	if cfg.MinInterval < 0 {
		cfg.MinInterval = 0
	} else if cfg.MinInterval == 0 {
		cfg.MinInterval = 60 * time.Second
	}
	return &Prefetcher{
		cfg:      cfg,
		bg:       bg,
		reg:      actors.DefaultRegistry(),
		lastRun:  make(map[string]time.Time),
		inflight: make(map[string]bool),
	}
}

// SetPinner attaches a persistent-cache Pinner (PDP tier). When set, the
// walked nodes of STATIC configured contracts are pinned so the warm set
// survives restart un-evicted. Call once at wiring time before Trigger.
func (p *Prefetcher) SetPinner(pn Pinner) {
	p.mu.Lock()
	p.pinner = pn
	p.mu.Unlock()
}

// Trigger runs a prefetch pass against the given header. Returns
// immediately; walks run on internal goroutines (so head-advance is
// never blocked). Safe to call from a Store.OnHeadChange callback.
// maxDynAddrs caps the runtime-learned address set so a pathological
// caller (or hostile client spraying eth_call to random addresses)
// can't grow the per-head walk unboundedly.
const maxDynAddrs = 64

// AddAddr registers a contract address discovered at runtime (typically
// from an eth_call local miss) so the prefetcher warms its state subtree
// on the next head advance. Idempotent, thread-safe, cheap, and bounded
// by maxDynAddrs. Unparseable input is ignored. This is the seam
// curio-core wires to ChainAPI.OnLocalMiss for self-expanding read-path
// coverage (lantern#44).
func (p *Prefetcher) AddAddr(raw string) {
	if p == nil {
		return
	}
	addr, ok := parseEthAddr(raw)
	if !ok {
		return
	}
	key := canonicalKey(addr)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dynAddrs == nil {
		p.dynAddrs = make(map[string]struct{}, 8)
	}
	if _, exists := p.dynAddrs[key]; exists {
		return
	}
	if len(p.dynAddrs) >= maxDynAddrs {
		return
	}
	p.dynAddrs[key] = struct{}{}
	log.Debugw("prefetch: learned address from eth_call miss", "addr", "0x"+key, "dyn_total", len(p.dynAddrs))
}

// isDynamic reports whether key was learned at runtime via AddAddr.
func (p *Prefetcher) isDynamic(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.dynAddrs[key]
	return ok
}

// addrSnapshot returns the merged static + dynamic address list to walk.
func (p *Prefetcher) addrSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.cfg.Addrs)+len(p.dynAddrs))
	out = append(out, p.cfg.Addrs...)
	for k := range p.dynAddrs {
		out = append(out, k)
	}
	return out
}

func (p *Prefetcher) Trigger(ctx context.Context, head *ltypes.TipSet) {
	if p == nil || head == nil {
		return
	}
	addrs := p.addrSnapshot()
	if len(addrs) == 0 {
		return
	}
	stateRoot := head.ParentState()
	if !stateRoot.Defined() {
		return
	}
	epoch := head.Height()

	p.runs.Add(1)

	// Build a fresh accessor anchored at this head's parent state, so
	// actor lookups verify against the live state root. We construct
	// one accessor per Trigger; cheap (no walks happen until GetActor
	// is called).
	tr := &trustedroot.TrustedRoot{Epoch: epoch, StateRoot: stateRoot}
	acc := accessor.New(tr, p.bg)

	for _, raw := range addrs {
		addr, ok := parseEthAddr(raw)
		if !ok {
			log.Debugw("prefetch: skipping unparseable address", "addr", raw)
			continue
		}
		key := canonicalKey(addr)

		p.mu.Lock()
		if p.cfg.MinInterval > 0 {
			if last, ok := p.lastRun[key]; ok && time.Since(last) < p.cfg.MinInterval {
				p.mu.Unlock()
				p.skippedCoolDn.Add(1)
				continue
			}
		}
		if p.inflight[key] {
			p.mu.Unlock()
			p.skippedInfligh.Add(1)
			continue
		}
		p.inflight[key] = true
		p.mu.Unlock()

		go p.walkOne(ctx, acc, key, addr, epoch)
	}
}

// walkOne resolves addr at the anchored state and pulls its bytecode +
// storage-trie nodes into the cache. Runs in its own goroutine.
func (p *Prefetcher) walkOne(ctx context.Context, acc *accessor.Accessor, key string, ethAddr [20]byte, epoch abi.ChainEpoch) {
	defer func() {
		p.mu.Lock()
		delete(p.inflight, key)
		p.lastRun[key] = time.Now()
		p.mu.Unlock()
	}()

	// Dynamic (learned) contracts walk 4x more nodes with retries; give
	// them proportionally more wall-clock so the deeper walk completes.
	perAddr := p.cfg.PerAddrTimeout
	if p.isDynamic(key) {
		perAddr *= 3
	}
	wctx, cancel := context.WithTimeout(ctx, perAddr)
	defer cancel()

	filAddr, err := ethToFilecoin(ethAddr)
	if err != nil {
		log.Debugw("prefetch: addr resolve failed", "addr", "0x"+key, "err", err)
		p.errors.Add(1)
		return
	}
	actor, _, err := acc.GetActor(wctx, filAddr)
	if err != nil {
		// Common case: contract not deployed (yet) at this head. Soft.
		log.Debugw("prefetch: GetActor miss", "addr", "0x"+key, "err", err)
		return
	}
	st, err := actors.LoadEVM(wctx, actor.Code, actor.Head, p.bg, p.reg)
	if err != nil {
		// Not an EVM actor (e.g. account placeholder). Nothing to walk.
		log.Debugw("prefetch: not an EVM actor", "addr", "0x"+key, "code", actor.Code.String())
		return
	}
	// Bytecode: one extra fetch, populates cache.
	if _, err := actors.FetchBytecode(wctx, st, p.bg); err != nil {
		log.Debugw("prefetch: bytecode fetch", "addr", "0x"+key, "err", err)
		p.errors.Add(1)
		// keep going: storage walk is independent
	}
	storageRoot := st.StorageRoot()
	if !storageRoot.Defined() {
		log.Debugw("prefetch: empty storage root", "addr", "0x"+key)
		p.walks.Add(1)
		return
	}
	// Learned (dynamic) contracts are the deep ones the static seed
	// list missed (e.g. FilecoinPay); give them a higher node budget so
	// their full storage trie lands. Retries close transient-miss holes
	// that would otherwise leave a permanent gap a later SLOAD trips on.
	maxNodes := p.cfg.MaxBlocksPerAddr
	if p.isDynamic(key) && maxNodes > 0 {
		maxNodes *= 4
	}
	// PDP tier: pin the walked storage-trie nodes of STATIC contracts so
	// the warm set survives LRU eviction / restart. Dynamic (learned)
	// addresses are never pinned (attacker-influenceable set). Also pin the
	// storage root itself so a re-walk always has its anchor.
	var onNode func(cid.Cid)
	p.mu.Lock()
	pinner := p.pinner
	p.mu.Unlock()
	if pinner != nil && !p.isDynamic(key) {
		onNode = func(c cid.Cid) { _ = pinner.Pin(c) }
		_ = pinner.Pin(storageRoot)
		if bc := st.BytecodeCID(); bc.Defined() {
			_ = pinner.Pin(bc)
		}
	}
	stats, err := kamt.WalkSubtree(wctx, storageRoot, p.bg, kamt.WalkOptions{
		MaxNodes:     maxNodes,
		FetchRetries: 2,
		OnNode:       onNode,
	})
	if err != nil {
		log.Debugw("prefetch: walk failed", "addr", "0x"+key, "err", err)
		p.errors.Add(1)
		return
	}
	p.walks.Add(1)
	p.blocksFetched.Add(uint64(stats.NodesFetched))
	p.bytesFetched.Add(uint64(stats.BytesFetched))
	log.Debugw("prefetch: walked",
		"addr", "0x"+key,
		"epoch", epoch,
		"nodes", stats.NodesFetched,
		"bytes", stats.BytesFetched,
		"errors", stats.Errors,
		"capped", stats.Capped,
	)
}

// Stats returns a snapshot of prefetcher counters.
func (p *Prefetcher) Stats() Stats {
	return Stats{
		Runs:             p.runs.Load(),
		Walks:            p.walks.Load(),
		SkippedCooldown:  p.skippedCoolDn.Load(),
		SkippedInflight:  p.skippedInfligh.Load(),
		BlocksFetched:    p.blocksFetched.Load(),
		BytesFetched:     p.bytesFetched.Load(),
		Errors:           p.errors.Load(),
		ConfiguredAddrs:  len(p.cfg.Addrs),
		MaxBlocksPerAddr: p.cfg.MaxBlocksPerAddr,
	}
}

// Stats is the public counter snapshot.
type Stats struct {
	Runs             uint64
	Walks            uint64
	SkippedCooldown  uint64
	SkippedInflight  uint64
	BlocksFetched    uint64
	BytesFetched     uint64
	Errors           uint64
	ConfiguredAddrs  int
	MaxBlocksPerAddr int
}

// String renders Stats one line.
func (s Stats) String() string {
	return fmt.Sprintf("runs=%d walks=%d cooldown=%d inflight=%d blocks=%d bytes=%d errors=%d addrs=%d cap=%d",
		s.Runs, s.Walks, s.SkippedCooldown, s.SkippedInflight,
		s.BlocksFetched, s.BytesFetched, s.Errors,
		s.ConfiguredAddrs, s.MaxBlocksPerAddr,
	)
}

// ---- address helpers ----

// parseEthAddr accepts 0x-prefixed or bare hex strings and returns the
// 20-byte address.
func parseEthAddr(s string) ([20]byte, bool) {
	var out [20]byte
	// Trim 0x.
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	if len(s) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		b, ok := hexByte(s[2*i], s[2*i+1])
		if !ok {
			return out, false
		}
		out[i] = b
	}
	return out, true
}

func hexByte(a, b byte) (byte, bool) {
	hi, ok := hexNibble(a)
	if !ok {
		return 0, false
	}
	lo, ok := hexNibble(b)
	if !ok {
		return 0, false
	}
	return (hi << 4) | lo, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// canonicalKey returns the lowercase-hex address (no 0x).
func canonicalKey(b [20]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 40)
	for i := 0; i < 20; i++ {
		out[2*i] = hex[b[i]>>4]
		out[2*i+1] = hex[b[i]&0x0f]
	}
	return string(out)
}

// ethToFilecoin mirrors the recipe in rpc/handlers/evmexec.go.
func ethToFilecoin(raw [20]byte) (address.Address, error) {
	maskedID := raw[0] == 0xff
	for i := 1; i < 12 && maskedID; i++ {
		if raw[i] != 0x00 {
			maskedID = false
		}
	}
	if maskedID {
		actorID := uint64(0)
		for i := 12; i < 20; i++ {
			actorID = (actorID << 8) | uint64(raw[i])
		}
		return address.NewIDAddress(actorID)
	}
	return address.NewDelegatedAddress(builtin.EthereumAddressManagerActorID, raw[:])
}

// guardrails: ensure exported types stay non-nil-receiver-safe.
var (
	_ = errors.New
)
