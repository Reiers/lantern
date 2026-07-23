// ChainAPI is the concrete api.FullNode implementation.
//
// Construction wires:
//   - state.Accessor — bound to the current TrustedRoot
//   - trustedroot.Producer — for ChainHead, ChainNotify
//   - wallet.Wallet — for WalletSign, WalletNew, ...
//   - mpool publisher — for MpoolPush (gossipsub on /fil/msgs/<network>)
//
// Mpool + ChainNotify are wired conditionally; Lantern's V1 default
// daemon does not require an active libp2p host for the basic
// `lotus chain head` and `lotus wallet balance` cross-tests.

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	verifreg "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/proof"

	"github.com/filecoin-project/go-state-types/network"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/build"
	lbeacon "github.com/Reiers/lantern/chain/beacon"
	hstore "github.com/Reiers/lantern/chain/header/store"
	headnotify "github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/msgsearch"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/internal/buildinfo"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/vm"
	"github.com/Reiers/lantern/vm/bridge"
	"github.com/Reiers/lantern/wallet"
)

// ChainAPI bundles the dependencies the handlers need.
type ChainAPI struct {
	Trusted     *trustedroot.TrustedRoot
	BlockGetter hamt.BlockGetter
	Accessor    *accessor.Accessor
	Wallet      *wallet.Wallet
	Mpool       MpoolPublisher
	NetworkName string

	// HeaderStore is the optional persistent header store. When wired,
	// methods like ChainGetTipSetByHeight, StateGetRandomnessFromTickets,
	// StateGetRandomnessFromBeacon, StateSearchMsg and StateWaitMsg are
	// available; otherwise they return ErrNotImpl.
	HeaderStore *hstore.Store

	// LocalFEVMDisabled forces eth_call to skip local FEVM execution and
	// forward to the VMBridge unconditionally (lantern#43 Part B). Default
	// false: local-exec-first with bridge fallback.
	LocalFEVMDisabled bool

	// LocalFEVMFetchRetries and LocalFEVMFetchTimeout configure the
	// retry-on-miss wrapper used for bytecode + KAMT storage block reads
	// inside eth_call (lantern#44). Zero values fall back to sensible
	// defaults inside localEthCall (2 retries, 8s total budget). Set
	// either to a negative value to disable retries entirely.
	LocalFEVMFetchRetries int
	LocalFEVMFetchTimeout time.Duration

	// localEthCall counters (lantern#44 observability). Loaded via
	// LocalEthCallStats(); written only by EthCall and localEthCall.
	localEthCallTotal          uint64
	localEthCallServed         uint64
	localEthCallBridgeFallback uint64

	// sentTxIdx maps eth tx hashes we broadcast (eth_sendRawTransaction,
	// lantern#45 Stage 4) to their Filecoin message CIDs, so the receipt
	// lookup (Stage 5) can resolve them locally. Lazily initialised via
	// sentTx().
	sentTxOnce sync.Once
	sentTxIdx  *sentTxIndex

	// OnLocalMiss, when set, is invoked with the lowercase 0x-prefixed
	// `to` address every time an eth_call can't be served locally and
	// falls back to the bridge (lantern#44 adaptive warming). curio-core
	// wires this to the state prefetcher so the missed contract's state
	// subtree is warmed on the next head advance, after which subsequent
	// reads of that contract serve locally. This turns the prefetch
	// warm-list from a static guess into a self-expanding set and is the
	// mechanism that closes the last read-path bridge dependency (linked
	// contracts like FilecoinPay that aren't in the seed list). Must be
	// cheap + non-blocking; the handler calls it on the hot path.
	OnLocalMiss func(addr string)

	// OnSentTx, when set, is invoked (non-blocking, on the send hot path)
	// with the Filecoin message CID every time eth_sendRawTransaction
	// publishes a tx locally (lantern#50 prefetch-on-send). curio-core /
	// the daemon wires this to a background warmer that polls
	// StateSearchMsg for the message until it lands, which drives the
	// embedded Bitswap source to pull the freshly-produced message + AMT +
	// receipt blocks into cache. By the time the client's own receipt poll
	// runs, those blocks are warm, so the receipt resolves locally instead
	// of racing (and losing to) a cold cross-peer Bitswap fetch inside the
	// poll window — the residual that kept #50 open. Must be cheap +
	// non-blocking; the handler fires it in a goroutine-friendly way.
	OnSentTx func(msgCID cid.Cid)

	// BeaconParams is the drand-round mapping for the active network.
	// Defaults to mainnet quicknet if zero-value.
	BeaconParams lbeacon.QuicknetParams

	// AuthIssuer satisfies AuthNew / AuthVerify by delegating to the RPC
	// server's Auth state.
	AuthIssuer AuthIssuer

	// VMShell is the optional pure-Go VM shell used for StateCall and
	// gas estimation. When nil, those methods fall back to the Phase 4
	// heuristics (fixed default gas).
	VMShell *vm.GasEstimator

	// AllowBlockSubmit gates Phase 7's SyncSubmitBlock publish path. The
	// default (false) makes SyncSubmitBlock a no-op that returns
	// ErrNotImpl. Operators set this true only when they explicitly want
	// their daemon to publish blocks to the gossipsub /fil/blocks topic.
	AllowBlockSubmit bool

	// Bridge is the optional VM bridge (Phase 8 Part B). When wired, the
	// handler routes StateCall for non-Send messages and the
	// post-execution stateRoot for MinerCreateBlock through this Bridge
	// rather than the native vm shell. See vm/bridge/doc.go and
	// TRUST-MODEL.md for the trust implications.
	Bridge bridge.Bridge

	// optional: shutdown hook
	OnShutdown func() error

	mu          sync.Mutex
	sessionUUID string
	notifySubs  []chan []api.HeadChange
	// pushLocks serializes MpoolPushMessage per sender so two concurrent
	// pushes from the same From can't read the same nonce and produce two
	// messages that collide on-chain (lantern#146; mirrors lotus
	// PushLocks). Guarded by mu; the per-sender mutex is held across the
	// whole estimate+sign+publish sequence.
	pushLocks map[address.Address]*sync.Mutex
	// blockPub is the /fil/blocks publisher for SyncSubmitBlock (PDP/backup
	// tier). Wired via SetBlockPublisher; guarded by mu.
	blockPub BlockPublisher

	// HeadNotify, when non-nil, takes over from the legacy notifySubs
	// slice. ChainNotify subscribers route through the Distributor
	// which honours bounded per-subscriber buffers and drop-slow
	// semantics. The daemon wires this in Phase 9.
	HeadNotify *headnotify.Distributor

	// NetInfoSource is the live libp2p host adapter used by NetPeers,
	// NetBandwidthStats, NetAutoNatStatus and friends. Nil-able: when
	// nil (e.g. wallet-only CLI invocations) those methods return safe
	// zero values, matching the Phase 9 stub behaviour.
	NetInfoSource NetInfo

	// eth_subscribe state. ethSubs maps each active subscription ID to
	// its cancel func. Lazy-initialised on first EthSubscribe call so
	// cold ChainAPI instances (probe mode, tests) don't carry the
	// allocation. See rpc/handlers/eth_subscribe.go for the wire flow.
	ethSubMu sync.Mutex
	ethSubs  map[EthSubscriptionID]*subscription
}

// AuthIssuer abstracts the rpc/server Auth type.
type AuthIssuer interface {
	AuthNew(perms []auth.Permission) ([]byte, error)
	AuthVerify(token string) ([]auth.Permission, error)
}

// MpoolPublisher abstracts the libp2p gossipsub publisher in net/mpool.
// Nil-able: when nil, MpoolPush returns ErrMpoolNotWired.
type MpoolPublisher interface {
	Publish(ctx context.Context, sm *types.SignedMessage) (cid.Cid, error)
}

// MpoolPendingLister is an optional capability of MpoolPublisher: query
// locally pending messages for nonce derivation and `MpoolPending`. Lantern's
// net/mpool.Pool satisfies this; tests can implement only MpoolPublisher.
type MpoolPendingLister interface {
	Pending() []*types.SignedMessage
}

// ErrMpoolNotWired is returned by MpoolPush when no publisher is configured.
var ErrMpoolNotWired = errors.New("lantern: mpool publisher not configured (no libp2p host) — see PHASE4-BLOCKERS.md")

// ErrNotImpl is the canonical "not implemented yet" error.
func ErrNotImpl(method, reason string) error {
	return xerrors.Errorf("not implemented in Lantern V1 — %s (method=%s)", reason, method)
}

// New returns a ChainAPI ready to register on a go-jsonrpc server.
func New(tr *trustedroot.TrustedRoot, bg hamt.BlockGetter, w *wallet.Wallet, mp MpoolPublisher, netName string) *ChainAPI {
	return &ChainAPI{
		Trusted:      tr,
		BlockGetter:  bg,
		Accessor:     accessor.New(tr, bg),
		Wallet:       w,
		Mpool:        mp,
		NetworkName:  netName,
		BeaconParams: beaconParamsForNetwork(netName),
		sessionUUID:  uuid.New().String(),
	}
}

// beaconParamsForNetwork picks the drand-quicknet timing parameters that
// match the given Filecoin network. Calibration and mainnet share the
// drand quicknet chain, but their FilecoinGenesisTime differs by ~2 years,
// so hardcoding mainnet on calibration produces beacon rounds ~23 million
// too high (StateGetBeaconEntry then fails with "not found in walked
// tipsets", which cascades into every miner-side read that draws
// randomness — including Curio's mining loop).
//
// Unknown networks fall back to mainnet params so the existing behaviour
// is preserved.
func beaconParamsForNetwork(netName string) lbeacon.QuicknetParams {
	switch netName {
	case "calibrationnet", "calibration", "calibnet":
		return lbeacon.CalibnetQuicknetParams()
	default:
		return lbeacon.MainnetQuicknetParams()
	}
}

// WithHeaderStore returns c with the header store attached. The store
// unlocks ChainGetTipSetByHeight, randomness queries, and StateSearchMsg.
func (c *ChainAPI) WithHeaderStore(s *hstore.Store) *ChainAPI {
	c.HeaderStore = s
	return c
}

// FollowHeadState wires the accessor to resolve state reads against the
// live header-store head (head.ParentStateRoot) instead of the frozen boot
// trusted-root. Without this, actor-state reads (StateMinerPower,
// StateMinerInfo, GetActor, ...) are pinned to the boot anchor epoch and
// fail once upstreams prune the aging boot state root (lantern#87). Call
// after HeaderStore is set. No-op if the accessor or header store is nil.
//
// Semantics: uses the head tipset's ParentStateRoot, matching Lantern's
// existing trusted-root convention (FromF3State) and the state a light
// client can serve without executing the head tipset. This is at most one
// epoch behind the true head state, which is correct for slowly-changing
// reads like miner power/info and, critically, is recent enough that
// bitswap/Glif can still serve it.
func (c *ChainAPI) FollowHeadState() {
	if c.Accessor == nil || c.HeaderStore == nil {
		return
	}
	store := c.HeaderStore
	c.Accessor.SetHeadStateProvider(func() (cid.Cid, bool) {
		ts := store.Head()
		if ts == nil {
			return cid.Undef, false
		}
		blks := ts.Blocks()
		if len(blks) == 0 {
			return cid.Undef, false
		}
		psr := blks[0].ParentStateRoot
		if !psr.Defined() {
			return cid.Undef, false
		}
		return psr, true
	})
}

// WithBridge attaches a VM bridge to the handler. See vm/bridge for the
// trust model.
func (c *ChainAPI) WithBridge(b bridge.Bridge) *ChainAPI {
	c.Bridge = b
	return c
}

// LocalEthCallStats returns a snapshot of the local-eth_call counters
// (lantern#44). Total = both served + bridge_fallback paths; a healthy
// embedded daemon with state-block availability should approach
// Served/Total = 1.0.
//
// Not a JSON-RPC method (lower-case start; suppressed in the registered
// surface). Embedded callers reach it via the ChainAPI handle.
func (c *ChainAPI) localEthCallStatsSnapshot() (total, served, bridgeFallback uint64) {
	total = atomic.LoadUint64(&c.localEthCallTotal)
	served = atomic.LoadUint64(&c.localEthCallServed)
	bridgeFallback = atomic.LoadUint64(&c.localEthCallBridgeFallback)
	return
}

// LocalEthCallStats is the public single-struct view of the counters,
// safe to expose through the daemon facade and through later RPC
// surfaces that want it. Lower-case method names on ChainAPI are
// skipped by the JSON-RPC registrar, but to keep the JSON-RPC handler
// surface clean we expose the struct via a Daemon-level accessor
// (see pkg/daemon.Daemon.LocalEthCallStats); the snapshot fn is the
// shared implementation.
type LocalEthCallStatsView struct {
	Total          uint64
	Served         uint64
	BridgeFallback uint64
}

// LocalEthCallStatsView is the in-process getter used by
// pkg/daemon.Daemon.LocalEthCallStats.
func (c *ChainAPI) LocalEthCallStatsView() LocalEthCallStatsView {
	t, s, b := c.localEthCallStatsSnapshot()
	return LocalEthCallStatsView{Total: t, Served: s, BridgeFallback: b}
}

// ----------------- Node admin (N) -----------------

// AuthVerify validates a JWT and returns its perms. Tier 1 (#1).
func (c *ChainAPI) AuthVerify(_ context.Context, token string) ([]auth.Permission, error) {
	if c.AuthIssuer == nil {
		return nil, errors.New("auth issuer not wired")
	}
	return c.AuthIssuer.AuthVerify(token)
}

// AuthNew mints a new JWT. Tier 1 (#2).
func (c *ChainAPI) AuthNew(_ context.Context, perms []auth.Permission) ([]byte, error) {
	if c.AuthIssuer == nil {
		return nil, errors.New("auth issuer not wired")
	}
	return c.AuthIssuer.AuthNew(perms)
}

// Version returns Lantern's identification. Tier 1 (#3).
//
// The version string is built from internal/buildinfo so it tracks the
// ldflags-injected versionTag at release time. Format:
//
//	"<versionTag> Lantern+<network>"   e.g. "v1.2.1 Lantern+mainnet"
//
// Untagged dev builds report "dev Lantern+mainnet". No "lotus-compat"
// suffix: Curio and Lotus CLIs only gate on APIVersion's major/minor.
func (c *ChainAPI) Version(_ context.Context) (api.Version, error) {
	// BlockDelay is 30 on mainnet/calibration, 4 on the curio-fork docker
	// devnet (`//go:build 2k`). Devnet consumers like curio's scheduler
	// compute epoch-time from this, so use the runtime devnet config
	// when we're on devnet. Fixes lantern#123.
	blockDelay := uint64(30)
	if c.NetworkName == "devnet" {
		if cfg := build.GetDevnetConfig(); cfg != nil && cfg.BlockDelaySecs > 0 {
			blockDelay = cfg.BlockDelaySecs
		}
	}
	return api.Version{
		Version: buildinfo.BuildVersion() + " Lantern+" + buildinfo.Network(),
		// Lotus FullAPIVersion1 = newVer(2,3,0) = (2<<16)|(3<<8)|0 = 0x020300.
		// Curio / Lotus CLI EqMajorMinor checks the high 16 bits, so the
		// patch byte is free for us to bump as Lantern's RPC surface
		// evolves; the 2.3.x major.minor is what's gated.
		APIVersion: 0x00020300,
		BlockDelay: blockDelay,
	}, nil
}

// Shutdown signals the daemon to exit. Tier 1 (#4).
func (c *ChainAPI) Shutdown(_ context.Context) error {
	if c.OnShutdown != nil {
		return c.OnShutdown()
	}
	return nil
}

// Session returns the per-process session UUID. Tier 1 (#5).
func (c *ChainAPI) Session(_ context.Context) (string, error) {
	return c.sessionUUID, nil
}

// ----------------- Chain reads (R) -----------------

// ChainHead returns the current trusted tipset. Tier 1 (#6).
//
// Lantern's TrustedRoot stores the (epoch, tipsetKey, stateRoot) tuple but
// not the full TipSet block list. We reconstruct a minimal TipSet that
// passes the JSON shape Curio expects (Cids + Height + ParentState +
// ParentMessageReceipts + ParentWeight). Blocks slice is reconstructed
// from the persisted header store when available; otherwise we emit a
// single synthetic header that carries enough metadata for downstream
// readers.
func (c *ChainAPI) ChainHead(_ context.Context) (*types.TipSet, error) {
	// Phase 9: prefer the persistent header store's head when wired —
	// it carries real blocks (not the synthetic placeholder) and
	// advances as the sync agent observes new tipsets.
	if c.HeaderStore != nil {
		if ts := c.HeaderStore.Head(); ts != nil {
			return ts, nil
		}
	}
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	return synthesizeTipSet(c.Trusted), nil
}

// synthesizeTipSet builds a TipSet from a TrustedRoot.
//
// We synthesise a single BlockHeader with the persisted (height,
// parentState, parentMessageReceipts, parentWeight) so JSON consumers
// (Lotus, Curio) read the right fields. The header's CID becomes the
// TipSet key.
func synthesizeTipSet(tr *trustedroot.TrustedRoot) *types.TipSet {
	pmr := tr.ParentMessageReceipts
	if !pmr.Defined() {
		pmr = tr.StateRoot // placeholder if not set
	}
	bh := &types.BlockHeader{
		Miner:                 mustZeroIDAddr(),
		Ticket:                &types.Ticket{VRFProof: []byte("lantern-synth")},
		ElectionProof:         &types.ElectionProof{WinCount: 1},
		BeaconEntries:         []types.BeaconEntry{},
		WinPoStProof:          []proof.PoStProof{},
		Parents:               []cid.Cid{},
		ParentWeight:          tr.ParentWeight,
		Height:                tr.Epoch,
		ParentStateRoot:       tr.StateRoot,
		ParentMessageReceipts: pmr,
		Messages:              tr.StateRoot, // placeholder
		BLSAggregate:          &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
		Timestamp:             uint64(time.Now().Unix()),
		BlockSig:              &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
	}
	ts, err := types.NewTipSet([]*types.BlockHeader{bh})
	if err != nil {
		// Last-resort fallback. Should never happen — return a TipSet
		// directly so callers can inspect Height etc.
		return &types.TipSet{}
	}
	return ts
}

func mustZeroIDAddr() address.Address {
	a, _ := address.NewIDAddress(0)
	return a
}

// ChainGetTipSet returns a tipset by key. Tier 1 (#18).
//
// Resolution order:
//  1. The synthetic current trusted head (matches empty key or head key).
//  2. The persistent header store, by key (#68) — serves any historical
//     tipset whose headers we've persisted. Curio's message/watch.go and
//     apiinfo.go ask for specific recent (non-head) tipset keys; before
//     this fell straight through to ErrTipSetNotFound, surfacing as
//     "tipset not in local store (only current head is cached in V1)" and
//     stalling Curio's chain watcher.
func (c *ChainAPI) ChainGetTipSet(_ context.Context, key types.TipSetKey) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	// Fast path: requested key matches (or omits) the synthetic head's key.
	cur := synthesizeTipSet(c.Trusted)
	if key.IsEmpty() || tipsetKeyMatches(cur.Key(), key) {
		return cur, nil
	}
	// Fall through to the header store for historical / non-head tipsets.
	if c.HeaderStore != nil {
		if ts, err := c.HeaderStore.GetTipSet(key); err == nil {
			return ts, nil
		}
	}
	return nil, ErrTipSetNotFound
}

func tipsetKeyMatches(a, b types.TipSetKey) bool {
	if a.IsEmpty() || b.IsEmpty() {
		return false
	}
	return a.String() == b.String()
}

// ChainGetTipSetByHeight walks back to the tipset at h. Tier 1 (#45).
//
// Phase 6: served from the persistent header store when configured.
func (c *ChainAPI) ChainGetTipSetByHeight(_ context.Context, h abi.ChainEpoch, _ types.TipSetKey) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	if h == c.Trusted.Epoch {
		return synthesizeTipSet(c.Trusted), nil
	}
	if c.HeaderStore == nil {
		return nil, ErrNotImpl("ChainGetTipSetByHeight", "header store not configured")
	}
	return c.HeaderStore.GetTipSetByHeight(h)
}

// ChainGetTipSetAfterHeight returns the first tipset at or after h.
// Tier 2 (#20).
func (c *ChainAPI) ChainGetTipSetAfterHeight(ctx context.Context, h abi.ChainEpoch, key types.TipSetKey) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	if h <= c.Trusted.Epoch {
		return synthesizeTipSet(c.Trusted), nil
	}
	return nil, ErrNotImpl("ChainGetTipSetAfterHeight", "requested height ahead of current trusted head")
}

// ChainGetBlock returns the header for a CID. Tier 1 (#4 in priority).
//
// V1: we don't persist block headers other than the synthetic current
// head. Phase 5 wires the header store lookup.
func (c *ChainAPI) ChainGetBlock(_ context.Context, k cid.Cid) (*types.BlockHeader, error) {
	if c.HeaderStore != nil {
		if bh, err := c.HeaderStore.Get(k); err == nil {
			return bh, nil
		}
	}
	return nil, ErrNotImpl("ChainGetBlock", "header-by-CID not in header store; configure a sync source or backfill")
}

// ChainGetMessage decodes an on-chain message. Tier 1 (#47).
func (c *ChainAPI) ChainGetMessage(ctx context.Context, k cid.Cid) (*types.Message, error) {
	raw, err := c.BlockGetter.Get(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("fetch message %s: %w", k, err)
	}
	if err := hamt.VerifyBlockCID(k, raw); err != nil {
		return nil, err
	}
	return types.DecodeMessage(raw)
}

// ChainGetMessagesInTipset walks the AMT of messages for a tipset.
// Tier 2 (#6 in priority).
func (c *ChainAPI) ChainGetMessagesInTipset(ctx context.Context, key types.TipSetKey) ([]api.ApiMsg, error) {
	return nil, ErrNotImpl("ChainGetMessagesInTipset", "block-meta AMT decode deferred to Phase 5 — needs full BlockHeader.Messages CID for each block")
}

// ChainReadObj returns the raw bytes for an IPLD block. Tier 1 (#50).
func (c *ChainAPI) ChainReadObj(ctx context.Context, k cid.Cid) ([]byte, error) {
	raw, err := c.BlockGetter.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if err := hamt.VerifyBlockCID(k, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ChainHasObj reports whether we have the block locally. Tier 1 (#51).
//
// "Locally" in Lantern means: reachable from any configured source
// (cache + gateway). We do a Get and discard the bytes.
func (c *ChainAPI) ChainHasObj(ctx context.Context, k cid.Cid) (bool, error) {
	_, err := c.BlockGetter.Get(ctx, k)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// ChainPutObj inserts a block. Tier 3 (#52).
func (c *ChainAPI) ChainPutObj(ctx context.Context, raw []byte) (cid.Cid, error) {
	return cid.Undef, ErrNotImpl("ChainPutObj", "no admin-side block insertion in V1; gateway is the source of truth")
}

// ChainTipSetWeight returns parent weight of a tipset. Tier 2 (#29).
func (c *ChainAPI) ChainTipSetWeight(ctx context.Context, key types.TipSetKey) (big.Int, error) {
	// Resolve the *requested* tipset (not just the head) and return the
	// ParentWeight carried by its block headers. All blocks in a tipset
	// share ParentWeight; it is a monotone proxy for the tipset's own weight
	// and is sufficient for fork-choice comparisons such as Curio's
	// winning-block base-vs-head weight check.
	//
	// Two invariants this fixes vs the previous implementation:
	//   (a) honour the key—a caller comparing two tipsets (e.g. head vs base)
	//       must get distinct weights, not the head's weight for both; and
	//   (b) never return a nil-inner BigInt. c.Trusted.ParentWeight can be
	//       unset, and a nil big.Int JSON-marshals as "<nil>", which breaks
	//       clients (Curio's mining loop dies with
	//       "failed to parse big string: <nil>").
	if ts, err := c.ChainGetTipSet(ctx, key); err == nil && ts != nil && len(ts.Blocks()) > 0 {
		if w := ts.Blocks()[0].ParentWeight; w.Int != nil {
			return w, nil
		}
	}
	if c.Trusted != nil && c.Trusted.ParentWeight.Int != nil {
		return c.Trusted.ParentWeight, nil
	}
	return big.Zero(), nil
}

// ChainNotify streams head changes. Tier 2 (#7).
//
// V1 implementation: returns a channel that immediately fires `current`
// with the trusted head, then stays open for future updates. The
// background producer (a goroutine wired by the daemon) calls
// `Broadcast()` to push updates.
func (c *ChainAPI) ChainNotify(ctx context.Context) (<-chan []api.HeadChange, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	// Phase 9: route through the head-change distributor when wired.
	// Falls back to the legacy single-event channel when the daemon
	// hasn't initialised a distributor (e.g. in unit tests that do not
	// configure a header store).
	if c.HeadNotify != nil {
		return c.HeadNotify.Subscribe(ctx), nil
	}
	ch := make(chan []api.HeadChange, 16)
	c.mu.Lock()
	c.notifySubs = append(c.notifySubs, ch)
	c.mu.Unlock()
	// Initial value.
	ch <- []api.HeadChange{{Type: "current", Val: synthesizeTipSet(c.Trusted)}}
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		for i, sub := range c.notifySubs {
			if sub == ch {
				c.notifySubs = append(c.notifySubs[:i], c.notifySubs[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// BroadcastHead pushes a HeadChange to all ChainNotify subscribers. The
// daemon's trustedroot follower calls this on every accepted tipset.
func (c *ChainAPI) BroadcastHead(hc []api.HeadChange) {
	c.mu.Lock()
	subs := append([]chan []api.HeadChange(nil), c.notifySubs...)
	c.mu.Unlock()
	for _, s := range subs {
		select {
		case s <- hc:
		default:
		}
	}
}

// Errors.
var ErrTipSetNotFound = errors.New("lantern: tipset not in local store (only current head is cached in V1)")

// ----------------- State reads (R) -----------------

// accForReads returns the accessor to use for current-state reads. It
// prefers an accessor anchored at the LIVE head (so accounts/actors
// created after daemon boot are visible) and falls back to the boot
// TrustedRoot accessor when no live head is available. See #48: the boot
// anchor is frozen at start-of-day state, making post-boot accounts read
// as empty/zero even though they exist on chain.
func (c *ChainAPI) accForReads() *accessor.Accessor {
	if live, ok := c.liveAccessor(); ok {
		return live
	}
	return c.Accessor
}

// StateGetActor reads an actor at a tipset. Tier 1 (#44 — the hot one).
func (c *ChainAPI) StateGetActor(ctx context.Context, a address.Address, _ types.TipSetKey) (*types.Actor, error) {
	act, _, err := c.accForReads().GetActor(ctx, a)
	if err != nil {
		return nil, err
	}
	return &types.Actor{
		Code:             act.Code,
		Head:             act.Head,
		Nonce:            act.Nonce,
		Balance:          act.Balance,
		DelegatedAddress: act.DelegatedAddress,
	}, nil
}

// StateLookupID resolves any-protocol address to ID address. Tier 1 (#25).
func (c *ChainAPI) StateLookupID(ctx context.Context, a address.Address, _ types.TipSetKey) (address.Address, error) {
	id, _, err := c.accForReads().LookupID(ctx, a)
	return id, err
}

// StateAccountKey reverse-resolves ID to BLS/secp pubkey address.
// Tier 1 (#12).
//
// V1: the Account actor's state is a single field (PubkeyAddress). We read
// it via accessor.GetActor then decode the head block. Deferred for now
// because the decode helper isn't in Phase 2's accessor (per B11). Phase 5
// adds the proper sub-state decoder.
func (c *ChainAPI) StateAccountKey(ctx context.Context, a address.Address, _ types.TipSetKey) (address.Address, error) {
	// If `a` is already a pubkey-typed (BLS / secp256k1 / delegated)
	// address, return it unchanged — Lotus does the same.
	if a.Protocol() != address.ID {
		return a, nil
	}
	// Otherwise load the account actor's state and read PubkeyAddress.
	as, _, err := c.accForReads().LoadAccount(ctx, a)
	if err != nil {
		return address.Undef, fmt.Errorf("StateAccountKey load account %s: %w", a, err)
	}
	return as.PubkeyAddress(), nil
}

// StateNetworkVersion returns the network version at a tipset. Tier 1 (#11).
//
// V1: hardcoded to the current mainnet version (Version27, GoldenWeek as
// of mid-2026). Phase 5 wires the upgrade schedule.
func (c *ChainAPI) StateNetworkVersion(_ context.Context, _ types.TipSetKey) (network.Version, error) {
	return network.Version27, nil
}

// StateNetworkName returns the network's well-known name. Tier 2 (#63).
func (c *ChainAPI) StateNetworkName(_ context.Context) (string, error) {
	switch c.NetworkName {
	case "calibration":
		return "calibrationnet", nil
	case "", "mainnet":
		return "mainnet", nil
	case "devnet":
		// The devnet's wire-name is the localnet-uuid that was
		// generated when its genesis was created. Read from the
		// runtime config seeded by `lantern devnet-init`, not the
		// literal string "devnet", so gossipsub topics + DHT
		// protocol prefix line up with lotus. Fixes lantern#123.
		if cfg := build.GetDevnetConfig(); cfg != nil && cfg.NetworkName != "" {
			return cfg.NetworkName, nil
		}
		return "devnet", nil
	default:
		return c.NetworkName, nil
	}
}

// StateReadState dumps an actor's state. Tier 3 (#64).
func (c *ChainAPI) StateReadState(ctx context.Context, a address.Address, _ types.TipSetKey) (*api.ActorState, error) {
	act, _, err := c.accForReads().GetActor(ctx, a)
	if err != nil {
		return nil, err
	}
	headBytes, err := c.BlockGetter.Get(ctx, act.Head)
	if err != nil {
		return nil, err
	}
	// lantern#3 Part A: for known system actors (power/market/verifreg) decode
	// the head CBOR into the versioned go-state-types struct so `State`
	// JSON-marshals with Lotus-compatible field names. Unknown actors and EVM
	// contracts fall back to the historical raw-bytes behaviour (backward
	// compatible; EVM eth_call is lantern#3 Part B).
	if decoded, ok, derr := decodeSystemActorState(c.systemActorRegistry(ctx), act.Code, headBytes); derr == nil && ok {
		return &api.ActorState{
			Balance: act.Balance,
			Code:    act.Code,
			State:   decoded,
		}, nil
	}
	return &api.ActorState{
		Balance: act.Balance,
		Code:    act.Code,
		State:   headBytes,
	}, nil
}

// StateGetRandomnessFromBeacon implements drand-derived randomness for the
// requested filecoin epoch and personalisation tag. Walks back from the
// reference tipset to find the canonical tipset for `randEpoch`, picks the
// beacon entry whose drand round matches MaxBeaconRoundForEpoch, then
// applies the Lotus DrawRandomnessFromDigest formula.
func (c *ChainAPI) StateGetRandomnessFromBeacon(ctx context.Context, pers gscrypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte, _ types.TipSetKey) (abi.Randomness, error) {
	entry, err := c.beaconEntryForEpoch(ctx, randEpoch)
	if err != nil {
		return nil, err
	}
	out, err := lbeacon.DrawBeaconRandomness(*entry, pers, randEpoch, entropy)
	if err != nil {
		return nil, err
	}
	return abi.Randomness(out), nil
}

// StateGetRandomnessDigestFromBeacon returns the blake2b-256 of the
// canonical beacon entry's Data bytes at randEpoch. Matches Lotus'
// StateManager.GetRandomnessDigestFromBeacon -> stateRand.GetBeaconRandomness
// (chain/rand/rand.go). The Digest variant has no personalisation /
// entropy mixing - callers that need that should use
// StateGetRandomnessFromBeacon. PDP ProveTask is one of the callers
// that wants only the raw digest (see curio/tasks/pdpv0/task_prove.go).
//
// Lookup strategy:
//
//  1. Try the local header store (beaconEntryForEpoch). Fastest +
//     stays on-chain-verified.
//  2. If the requested epoch is beyond the local head OR the beacon
//     entry for that epoch isn't in the locally-walked tipsets,
//     fall back to the VMBridge. Mirrors the EthGetBlockByNumber
//     fallback pattern (extra.go).
//
// Why the fallback matters: PDP ProveTask asks for randomness at the
// dataset's challenge epoch the moment the proving window opens. If
// the local header store is even one epoch behind the chain head
// (normal during sync catch-up), the local path errors with
// 'cannot draw randomness from future epoch'. Without fallback the
// prove task burns its MaxFailures budget waiting for sync to catch
// up - on a slow link or after a header-store cold start, that can
// exceed the proving window itself.
func (c *ChainAPI) StateGetRandomnessDigestFromBeacon(ctx context.Context, randEpoch abi.ChainEpoch, tsk types.TipSetKey) (abi.Randomness, error) {
	entry, err := c.beaconEntryForEpoch(ctx, randEpoch)
	if err == nil {
		d := lbeacon.BeaconDigest(*entry)
		return abi.Randomness(d[:]), nil
	}

	localErr := err

	// Local lookup failed. If we have a bridge, try forwarding to the
	// upstream node. The upstream returns the same blake2b-256 digest;
	// we don't lose verifiability here because the digest is itself a
	// commitment to the beacon entry that the upstream is also
	// computing from the same DRAND quicknet round.
	if c.Bridge != nil {
		params, perr := json.Marshal([]any{int64(randEpoch), tsk})
		if perr != nil {
			return nil, xerrors.Errorf("local lookup failed (%w) and bridge marshal failed: %v", localErr, perr)
		}
		raw, brErr := c.Bridge.RawJSONRPC(ctx, "Filecoin.StateGetRandomnessDigestFromBeacon", params)
		if brErr != nil {
			return nil, xerrors.Errorf("local lookup failed (%w) and bridge fallback failed: %v", localErr, brErr)
		}
		var out abi.Randomness
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			return nil, xerrors.Errorf("local lookup failed (%w) and bridge decode failed: %v", localErr, uerr)
		}
		log.Infow("StateGetRandomnessDigestFromBeacon: served via bridge fallback",
			"epoch", randEpoch, "local_err", localErr.Error())
		return out, nil
	}
	return nil, localErr
}

// StateGetRandomnessFromTickets returns randomness derived from the chain
// ticket at the requested epoch. Matches Lotus' getChainRandomness +
// DrawRandomnessFromDigest path for nv >= 13 (no lookback flag; we use the
// exact canonical tipset at randEpoch, walking back through null rounds).
func (c *ChainAPI) StateGetRandomnessFromTickets(ctx context.Context, pers gscrypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte, _ types.TipSetKey) (abi.Randomness, error) {
	ts, err := c.tipsetForRandomness(ctx, randEpoch)
	if err != nil {
		return nil, err
	}
	out, err := lbeacon.DrawChainRandomness(ts, pers, randEpoch, entropy)
	if err != nil {
		return nil, err
	}
	return abi.Randomness(out), nil
}

// StateGetBeaconEntry returns the beacon entry whose drand round matches
// the canonical max-round for the given filecoin epoch.
func (c *ChainAPI) StateGetBeaconEntry(ctx context.Context, epoch abi.ChainEpoch) (*types.BeaconEntry, error) {
	return c.beaconEntryForEpoch(ctx, epoch)
}

// tipsetForRandomness returns the canonical tipset at randEpoch, walking
// back through null rounds. Uses the header store when configured;
// otherwise returns the synthesized current-head tipset if randEpoch is
// the current head, else an error.
func (c *ChainAPI) tipsetForRandomness(ctx context.Context, randEpoch abi.ChainEpoch) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	if randEpoch < 0 {
		return nil, fmt.Errorf("randomness epoch %d cannot be negative", randEpoch)
	}

	// #82: the ceiling for "is this a future epoch?" must be the LIVE head
	// (HeaderStore.Head()), not the frozen boot anchor (c.Trusted.Epoch).
	// c.Trusted is set once at construction and never advances (see #48),
	// so comparing randEpoch against it incorrectly reports epochs the node
	// has actually reached as "future". A PDP prove task asks for randomness
	// at its challenge epoch the instant the window opens; on a node whose
	// header sync is a few epochs behind the chain tip, that epoch can be
	// just above the current head. Bridge-off there is no upstream to fall
	// to, so instead of hard-failing we wait briefly for the header sync to
	// reach randEpoch, then draw locally. The wait is bounded so a genuinely
	// future epoch still errors promptly.
	liveHead := c.Trusted.Epoch
	if c.HeaderStore != nil {
		if ts := c.HeaderStore.Head(); ts != nil {
			liveHead = ts.Height()
		}
	}

	if randEpoch > liveHead {
		// Bounded wait-for-head: only worth waiting when randEpoch is
		// within a small window above the live head (the normal
		// sync-catch-up case). Anything far ahead is a genuinely future
		// epoch and errors immediately.
		const (
			randWaitWindow abi.ChainEpoch = 10
			randWaitTotal                 = 20 * time.Second
			randWaitPoll                  = 500 * time.Millisecond
		)
		if c.HeaderStore == nil || randEpoch > liveHead+randWaitWindow {
			return nil, fmt.Errorf("cannot draw randomness from future epoch %d (head %d)", randEpoch, liveHead)
		}
		deadline := time.Now().Add(randWaitTotal)
		for randEpoch > liveHead {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("cannot draw randomness from future epoch %d (head %d after waiting %s for sync)", randEpoch, liveHead, randWaitTotal)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(randWaitPoll):
			}
			if ts := c.HeaderStore.Head(); ts != nil {
				liveHead = ts.Height()
			}
		}
	}

	if c.HeaderStore != nil {
		ts, err := c.HeaderStore.GetTipSetByHeight(randEpoch)
		if err == nil {
			return ts, nil
		}
		// fall through: maybe header store doesn't have it yet but the
		// requested epoch is exactly the current head.
	}
	if randEpoch == c.Trusted.Epoch {
		return synthesizeTipSet(c.Trusted), nil
	}
	return nil, ErrNotImpl("randomness", fmt.Sprintf("tipset at epoch %d not in header store", randEpoch))
}

// beaconEntryForEpoch finds the BeaconEntry whose drand round matches
// MaxBeaconRoundForEpoch(epoch). Walks back up to 20 tipsets if the
// expected entry isn't on the first candidate.
func (c *ChainAPI) beaconEntryForEpoch(ctx context.Context, epoch abi.ChainEpoch) (*types.BeaconEntry, error) {
	ts, err := c.tipsetForRandomness(ctx, epoch)
	if err != nil {
		return nil, err
	}
	wantRound := c.BeaconParams.MaxBeaconRoundForEpoch(epoch)
	for i := 0; i < 20; i++ {
		for _, b := range ts.Blocks() {
			for _, e := range b.BeaconEntries {
				if e.Round == wantRound {
					copy := e
					return &copy, nil
				}
			}
		}
		if ts.Height() <= 0 || c.HeaderStore == nil {
			break
		}
		prev, perr := c.HeaderStore.GetTipSetByHeight(ts.Height() - 1)
		if perr != nil {
			break
		}
		ts = prev
	}
	return nil, fmt.Errorf("beacon entry for round %d (epoch %d) not found in walked tipsets", wantRound, epoch)
}

// ----------------- Miner reads -----------------
//
// All StateMiner* / StateSector* handlers live in state_miner.go and
// state_sector.go (Phase 5). Pure-formula compute-on-state methods (e.g.
// StateMinerPreCommitDepositForPower) live in state_compute.go.

func (c *ChainAPI) StateMinerCreationDeposit(_ context.Context, _ types.TipSetKey) (big.Int, error) {
	return big.Zero(), ErrNotImpl("StateMinerCreationDeposit", "needs reward+power decode")
}

// StateMinerPreCommitDepositForPower / StateMinerInitialPledgeForSector
// live in state_compute.go (Phase 5 Part F).

// Sector / replica queries live in state_sector.go (Phase 5 Part E).

// ----------------- Market / verifreg -----------------
//
// Real impls live in state_market.go (Part C) and state_verifreg.go
// (Part D). The handlers below are typed stubs for methods not yet
// implemented.

func (c *ChainAPI) StateGetAllocationIdForPendingDeal(_ context.Context, _ abi.DealID, _ types.TipSetKey) (verifreg.AllocationId, error) {
	return 0, ErrNotImpl("StateGetAllocationIdForPendingDeal", "market HAMT decode deferred")
}
func (c *ChainAPI) StateGetAllocationForPendingDeal(_ context.Context, _ abi.DealID, _ types.TipSetKey) (*verifreg.Allocation, error) {
	return nil, ErrNotImpl("StateGetAllocationForPendingDeal", "combo lookup deferred to Phase 5")
}
func (c *ChainAPI) StateListMessages(_ context.Context, _ *api.MessageMatch, _ types.TipSetKey, _ abi.ChainEpoch) ([]cid.Cid, error) {
	return nil, ErrNotImpl("StateListMessages", "heavy scan deferred to Phase 5")
}

// StateCirculatingSupply / StateVMCirculatingSupplyInternal live in
// state_compute.go (Phase 5 Part F).

// ----------------- Wait / search / call -----------------

// StateSearchMsg walks backward from `from` (or current head if empty) up
// to `lookbackLimit` epochs, looking for `msgCID` in any block's message
// AMTs. Returns the receipt + tipset of inclusion, or nil when not found.
// Tier 1 (#46), Phase 6 Part E.
func (c *ChainAPI) StateSearchMsg(ctx context.Context, from types.TipSetKey, msgCID cid.Cid, lookbackLimit abi.ChainEpoch, _ bool) (*api.MsgLookup, error) {
	if c.HeaderStore == nil {
		return nil, ErrNotImpl("StateSearchMsg", "header store not configured")
	}
	if lookbackLimit <= 0 {
		lookbackLimit = 7200 // mirror lotus default ~30h
	}
	fromEpoch := abi.ChainEpoch(-1)
	if !from.IsEmpty() {
		// Best-effort: walk the header store for from's height by
		// loading any of its block CIDs. Cheap fallback: ignore and
		// start at head.
		for _, k := range from.Cids() {
			if bh, err := c.HeaderStore.Get(k); err == nil {
				fromEpoch = bh.Height
				break
			}
		}
	}
	// lantern#50: message/receipt blocks for a tipset may not be in the
	// embedded Bitswap cache the instant StateSearchMsg runs (the SP just
	// learned the tipset via gossipsub headers; the message bodies arrive
	// over Bitswap a beat later). A single Get miss here would error the
	// whole search and force a bridge fallback. Bridge-off, that fallback
	// is a hard failure. Wrap the getter so a momentarily-uncached block
	// resolves on a short retry window instead, mirroring the eth_call
	// fetch-on-miss policy (#44).
	//
	// Budget sizing: the underlying bitswap source needs up to its full
	// deadline (~5s) to broadcast a WANT and receive a cold block from a
	// peer. A too-tight per-attempt slice cancels bitswap mid-broadcast
	// (observed as "net/bitswap: context canceled"). So give a generous
	// total budget (3 attempts x ~6s = 18s) that comfortably contains a
	// full bitswap round per attempt.
	bg := newRetryingBlockGetter(c.BlockGetter, 2, 18*time.Second)
	s := msgsearch.New(c.HeaderStore, bg)
	res, err := s.Find(ctx, fromEpoch, msgCID, lookbackLimit)
	if err != nil {
		if errors.Is(err, msgsearch.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &api.MsgLookup{
		Message: msgCID,
		Receipt: res.Receipt,
		TipSet:  res.TipSet.Key(),
		Height:  res.Height,
	}, nil
}

// StateWaitMsg behaves like StateSearchMsg but blocks waiting for inclusion
// and `confidence` additional epochs. The wait loop is bounded by
// `lookbackLimit` epochs (default 7200 ~ 30h).
// Tier 1 (#9), Phase 6 Part E.
func (c *ChainAPI) StateWaitMsg(ctx context.Context, msgCID cid.Cid, confidence uint64, lookbackLimit abi.ChainEpoch, allowReplaced bool) (*api.MsgLookup, error) {
	if c.HeaderStore == nil {
		return nil, ErrNotImpl("StateWaitMsg", "header store not configured")
	}
	deadline := time.Now().Add(30 * time.Hour)
	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	for {
		lookup, err := c.StateSearchMsg(ctx, types.TipSetKey{}, msgCID, lookbackLimit, allowReplaced)
		if err != nil {
			return nil, err
		}
		if lookup != nil {
			// Wait for the chain to reach lookup.Height + confidence.
			for c.HeaderStore.HeadEpoch() < lookup.Height+abi.ChainEpoch(confidence) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-poll.C:
				}
				if time.Now().After(deadline) {
					return nil, errors.New("StateWaitMsg: confidence timeout")
				}
			}
			return lookup, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}
		if time.Now().After(deadline) {
			return nil, errors.New("StateWaitMsg: lookback exhausted")
		}
	}
}

// StateCall runs `msg` in dry-run mode against the trusted tipset's
// state. Tier 4 (#69). Phase 7 implementation: pure-Go VM shell from
// `vm.StateCall`.
func (c *ChainAPI) StateCall(ctx context.Context, msg *types.Message, _ types.TipSetKey) (*api.InvocResult, error) {
	if msg == nil {
		return nil, errors.New("StateCall: nil message")
	}
	// Bridge routing: for non-Send messages, when a bridge is wired,
	// delegate to the bridge to get a real receipt with proper Return
	// bytes. The native vm shell otherwise returns SysErrInvalidReceiver
	// for builtin actor methods (PHASE7-BLOCKERS.md B1).
	if c.Bridge != nil && msg.Method != 0 && c.Trusted != nil {
		root, recs, err := c.Bridge.ComputeStateRoot(ctx, c.Trusted.StateRoot, int64(c.Trusted.Epoch), []*types.Message{msg})
		if err == nil && len(recs) >= 1 && recs[0] != nil {
			mcid := msg.Cid()
			_ = root
			return &api.InvocResult{
				MsgCid:   mcid,
				Msg:      msg,
				MsgRct:   recs[0],
				Duration: 0,
				GasCost: api.MessageGasCost{
					Message: mcid,
					GasUsed: big.NewIntUnsigned(uint64(recs[0].GasUsed)),
				},
				ExecutionTrace: api.ExecutionTrace{
					Msg:    msg,
					MsgRct: recs[0],
					Error:  "",
				},
			}, nil
		}
		// On bridge error, fall through to the native vm shell. The
		// shell will surface SysErrInvalidReceiver for non-Send, which
		// is the documented behaviour without a bridge — but we log the
		// upstream error so operators can see why the bridge declined.
		if err != nil {
			fmt.Printf("lantern: StateCall bridge route failed (%s): %v — falling back to native vm shell\n", c.Bridge.Provenance(), err)
		}
	}
	r, err := vm.StateCall(ctx, c.Accessor, msg, vm.ApplyOptions{})
	if err != nil {
		return nil, err
	}
	mcid := msg.Cid()
	inv := &api.InvocResult{
		MsgCid:   mcid,
		Msg:      msg,
		MsgRct:   &r.Receipt,
		Duration: r.Duration.Nanoseconds(),
		Error:    r.Error,
		GasCost: api.MessageGasCost{
			Message: mcid,
			GasUsed: big.NewIntUnsigned(uint64(r.GasCost.GasUsed)),
		},
		ExecutionTrace: api.ExecutionTrace{
			Msg:    msg,
			MsgRct: &r.Receipt,
			Error:  r.Error,
		},
	}
	return inv, nil
}

// ----------------- Gas -----------------

// GasEstimateMessageGas fills in nonce, premium, fee cap, and gas limit
// based on the chain's recent base fee and mempool premium distribution.
// Tier 2 (#13). Phase 7 implementation: vm.GasEstimator.
func (c *ChainAPI) GasEstimateMessageGas(ctx context.Context, msg *types.Message, spec *api.MessageSendSpec, _ types.TipSetKey) (*types.Message, error) {
	if msg == nil {
		return nil, errors.New("nil message")
	}
	e := c.gasEstimator()
	maxFee := big.Zero()
	if spec != nil {
		maxFee = spec.MaxFee
	}
	out, err := e.EstimateMessageGas(ctx, msg, maxFee)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ChainAPI) GasEstimateFeeCap(ctx context.Context, msg *types.Message, maxqueueblocks int64, _ types.TipSetKey) (abi.TokenAmount, error) {
	e := c.gasEstimator()
	var prem big.Int
	if msg != nil {
		prem = msg.GasPremium
	}
	return e.EstimateFeeCap(ctx, prem, maxqueueblocks)
}

func (c *ChainAPI) GasEstimateGasPremium(ctx context.Context, nblocksincl uint64, _ address.Address, _ int64, _ types.TipSetKey) (abi.TokenAmount, error) {
	e := c.gasEstimator()
	return e.EstimateGasPremium(ctx, nblocksincl)
}

// gasEstimator returns the configured VM gas estimator, building a
// best-effort default if the field is nil. The default wires whatever
// header store and mempool are already attached to the ChainAPI; if
// neither is present, the estimator returns Lotus-compatible fallback
// numbers (100 attoFIL base fee, 100k attoFIL premium).
func (c *ChainAPI) gasEstimator() *vm.GasEstimator {
	if c.VMShell != nil {
		return c.VMShell
	}
	e := &vm.GasEstimator{Acc: c.Accessor}
	if c.HeaderStore != nil {
		e.BaseFee = &vm.HeaderStoreFeeSource{Store: c.HeaderStore}
	}
	if pl, ok := c.Mpool.(MpoolPendingLister); ok && pl != nil {
		e.Premium = &vm.MempoolPremiumSource{Pending: pl.Pending}
	}
	return e
}

// ----------------- Wallet -----------------

func (c *ChainAPI) WalletNew(ctx context.Context, kt api.KeyType) (address.Address, error) {
	if c.Wallet == nil {
		return address.Undef, errors.New("wallet not initialised")
	}
	return c.Wallet.NewAddress(ctx, wallet.KeyType(kt))
}
func (c *ChainAPI) WalletList(ctx context.Context) ([]address.Address, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	return c.Wallet.List(ctx)
}
func (c *ChainAPI) WalletHas(ctx context.Context, a address.Address) (bool, error) {
	if c.Wallet == nil {
		return false, nil
	}
	resolved := c.resolveToKeyAddress(ctx, a)
	return c.Wallet.Has(ctx, resolved)
}

// resolveToKeyAddress maps an ID-typed address (e.g. f0137632) to its
// BLS/secp pubkey-address by reading the on-chain Account actor, matching
// what lotus does inside its wallet-facing calls. Wallets store keys under
// the pubkey-address, but Filecoin miner data (MinerInfo.Worker,
// MinerGetBaseInfo.WorkerKey) surfaces the ID-address; without this
// resolution WalletSign/WalletHas fail with "key not found" for the very
// address a caller would look up from a MinerInfo response.
//
// If `a` is already a pubkey address, it is returned unchanged. If
// resolution fails, the original address is returned so the caller sees
// the underlying keystore error rather than a resolver error swallowing
// it.
func (c *ChainAPI) resolveToKeyAddress(ctx context.Context, a address.Address) address.Address {
	if a.Protocol() != address.ID {
		return a
	}
	resolved, err := c.StateAccountKey(ctx, a, types.EmptyTSK)
	if err != nil || resolved == address.Undef {
		return a
	}
	return resolved
}
func (c *ChainAPI) WalletDelete(ctx context.Context, a address.Address) error {
	if c.Wallet == nil {
		return errors.New("wallet not initialised")
	}
	return c.Wallet.Delete(ctx, a)
}
func (c *ChainAPI) WalletExport(ctx context.Context, a address.Address) (*api.KeyInfo, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	ki, err := c.Wallet.Export(ctx, a)
	if err != nil {
		return nil, err
	}
	return &api.KeyInfo{Type: api.KeyType(ki.Type), PrivateKey: ki.PrivateKey}, nil
}
func (c *ChainAPI) WalletImport(ctx context.Context, ki *api.KeyInfo) (address.Address, error) {
	if c.Wallet == nil {
		return address.Undef, errors.New("wallet not initialised")
	}
	return c.Wallet.Import(ctx, &wallet.KeyInfo{Type: string(ki.Type), PrivateKey: ki.PrivateKey})
}
func (c *ChainAPI) WalletSetDefault(ctx context.Context, a address.Address) error {
	if c.Wallet == nil {
		return errors.New("wallet not initialised")
	}
	return c.Wallet.SetDefault(ctx, a)
}
func (c *ChainAPI) WalletDefaultAddress(ctx context.Context) (address.Address, error) {
	if c.Wallet == nil {
		return address.Undef, errors.New("wallet not initialised")
	}
	return c.Wallet.Default(ctx)
}
func (c *ChainAPI) WalletSign(ctx context.Context, a address.Address, msg []byte) (*gscrypto.Signature, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	return c.Wallet.Sign(ctx, c.resolveToKeyAddress(ctx, a), msg)
}
func (c *ChainAPI) WalletSignMessage(ctx context.Context, a address.Address, msg *types.Message) (*types.SignedMessage, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	if msg == nil {
		return nil, errors.New("nil message")
	}
	mcid := msg.Cid()
	sig, err := c.Wallet.Sign(ctx, c.resolveToKeyAddress(ctx, a), mcid.Bytes())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return &types.SignedMessage{Message: *msg, Signature: *sig}, nil
}

// WalletBalance reads the balance via StateGetActor at the trusted head.
// Tier 1 (#14).
func (c *ChainAPI) WalletBalance(ctx context.Context, a address.Address) (big.Int, error) {
	act, _, err := c.accForReads().GetActor(ctx, a)
	if err != nil {
		// Treat "not found" as zero balance — matches Lotus behaviour
		// for never-funded addresses.
		if errors.Is(err, accessor.ErrAddressNotFound) {
			return big.Zero(), nil
		}
		return big.Zero(), err
	}
	return act.Balance, nil
}

// ----------------- Mpool -----------------

func (c *ChainAPI) MpoolPush(ctx context.Context, sm *types.SignedMessage) (cid.Cid, error) {
	if c.Mpool == nil {
		return cid.Undef, ErrMpoolNotWired
	}
	return c.Mpool.Publish(ctx, sm)
}

// pushLockFor returns the per-sender mutex used to serialize the whole
// estimate+sign+publish sequence in MpoolPushMessage (lantern#146).
func (c *ChainAPI) pushLockFor(a address.Address) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pushLocks == nil {
		c.pushLocks = make(map[address.Address]*sync.Mutex)
	}
	lk, ok := c.pushLocks[a]
	if !ok {
		lk = &sync.Mutex{}
		c.pushLocks[a] = lk
	}
	return lk
}

// MpoolPushMessage = GasEstimate + Sign + Push. Tier 2 (#53).
//
// lantern#146 hardening (mirrors lotus node/impl/full/mpool.go):
//   - never mutates the caller's message (works on a copy)
//   - serializes per sender, so concurrent pushes from one From can't
//     read the same nonce and produce colliding messages
//   - rewrites an ID-typed From (f0...) to the deterministic pubkey
//     address before signing — ID addresses are not reorg-stable
//   - rejects when balance < Value + RequiredFunds (a message that can
//     never land would otherwise loop in the #47 rebroadcaster)
//   - rejects GasPremium > GasFeeCap after estimation
func (c *ChainAPI) MpoolPushMessage(ctx context.Context, msgIn *types.Message, spec *api.MessageSendSpec) (*types.SignedMessage, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	if msgIn == nil {
		return nil, errors.New("nil message")
	}
	// Work on a copy: the caller's struct must stay untouched.
	msg := *msgIn

	// ID-typed From (f0...) is not reorg-stable; resolve to the pubkey
	// address (lotus does the same, with a warning).
	if msg.From.Protocol() == address.ID {
		resolved := c.resolveToKeyAddress(ctx, msg.From)
		if resolved != msg.From && resolved != address.Undef {
			msg.From = resolved
		}
	}

	// Serialize estimate+sign+publish per sender.
	lk := c.pushLockFor(msg.From)
	lk.Lock()
	defer lk.Unlock()

	// Fill nonce if 0.
	if msg.Nonce == 0 {
		n, err := c.MpoolGetNonce(ctx, msg.From)
		if err != nil {
			return nil, fmt.Errorf("get nonce: %w", err)
		}
		msg.Nonce = n
	}
	// Fill gas defaults.
	estim, err := c.GasEstimateMessageGas(ctx, &msg, spec, types.TipSetKey{})
	if err != nil {
		return nil, err
	}
	msg = *estim
	if msg.GasPremium.GreaterThan(msg.GasFeeCap) {
		return nil, fmt.Errorf("after estimation, GasPremium %s > GasFeeCap %s", msg.GasPremium, msg.GasFeeCap)
	}

	// Balance gate: a message the sender can't fund would be published,
	// never land, and loop in the #47 rebroadcaster until max-retries.
	// Fail it here instead. Balance-read errors don't block the push
	// (fresh nodes may not have the sender's state warm yet).
	if c.accForReads() != nil {
		if bal, berr := c.WalletBalance(ctx, msg.From); berr == nil {
			required := big.Add(msg.Value, msg.RequiredFunds())
			if bal.LessThan(required) {
				return nil, fmt.Errorf("mpool push: not enough funds: %s < %s (value + max gas fees)", bal, required)
			}
		}
	}

	// Sign over the message CID.
	sm, err := c.WalletSignMessage(ctx, msg.From, &msg)
	if err != nil {
		return nil, err
	}
	if c.Mpool == nil {
		return sm, ErrMpoolNotWired
	}
	if _, err := c.Mpool.Publish(ctx, sm); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return sm, nil
}

// MpoolGetNonce returns the current actor nonce, accounting for any
// locally pending messages from the same sender (unsubmitted nonces stack
// on top of the on-chain nonce). Tier 2 (#15).
func (c *ChainAPI) MpoolGetNonce(ctx context.Context, a address.Address) (uint64, error) {
	act, err := c.StateGetActor(ctx, a, types.TipSetKey{})
	var onChain uint64
	if err != nil {
		if !errors.Is(err, accessor.ErrAddressNotFound) {
			return 0, err
		}
		onChain = 0
	} else {
		onChain = act.Nonce
	}
	if pl, ok := c.Mpool.(MpoolPendingLister); ok && pl != nil {
		next := onChain
		for _, sm := range pl.Pending() {
			if sm.Message.From == a && sm.Message.Nonce >= next {
				next = sm.Message.Nonce + 1
			}
		}
		return next, nil
	}
	return onChain, nil
}

// MpoolPending returns the locally-tracked pending signed messages. We do
// not maintain a full mempool view; Lantern relies on the rest of the
// network's full nodes for inclusion. Tier 2 (Phase 6 Part D).
func (c *ChainAPI) MpoolPending(_ context.Context, _ []types.TipSetKey) ([]*types.SignedMessage, error) {
	if pl, ok := c.Mpool.(MpoolPendingLister); ok && pl != nil {
		return pl.Pending(), nil
	}
	return nil, nil
}

// MpoolGetConfig returns a static MpoolConfig snapshot. Lantern doesn't
// maintain a full lotus-style mempool with priority-addr admission /
// RBF / size-based pruning; we only track locally-published messages
// for the #47 rebroadcast + #119 persist paths. But some consumers
// (curio at boot) call MpoolGetConfig to size their own mempool tracking
// caches. Returning sensible lotus-default values keeps those consumers
// happy without pretending Lantern implements the full admission policy.
// Fixes lantern#123 finding 6.
func (c *ChainAPI) MpoolGetConfig(_ context.Context) (*types.MpoolConfig, error) {
	return &types.MpoolConfig{
		// nil slice marshals to JSON null, matching lotus's PriorityAddrs
		// wire shape byte-for-byte. An empty slice would marshal as [] and
		// break consumers that check == null (curio boot does this).
		PriorityAddrs:          nil,
		SizeLimitHigh:          30000,
		SizeLimitLow:           20000,
		ReplaceByFeeRatio:      types.Percent(125),
		PruneCooldown:          time.Minute,
		GasLimitOverestimation: 1.25,
	}, nil
}

// ----------------- SP block production (Tier 4) -----------------
//
// MinerGetBaseInfo, MinerCreateBlock, MpoolSelect live in miner_block.go.
// SyncSubmitBlock is gated below.

// SyncSubmitBlock publishes a block to the gossipsub /fil/blocks/<network>
// topic. Phase 7 implementation: requires AllowBlockSubmit=true to
// actually publish. Otherwise returns an explicit "dry-run" error so
// Curio's tests fail loudly instead of silently dropping blocks.
func (c *ChainAPI) SyncSubmitBlock(ctx context.Context, blk *types.BlockMsg) error {
	if blk == nil || blk.Header == nil {
		return errors.New("SyncSubmitBlock: nil block")
	}
	if !c.AllowBlockSubmit {
		return ErrNotImpl("SyncSubmitBlock",
			"block submission requires ChainAPI.AllowBlockSubmit=true (operator opt-in)")
	}
	// Prefer an explicitly-wired block publisher (net/blockpub on the
	// /fil/blocks topic - what the embedded PDP/backup daemon wires). Fall
	// back to the mpool if it happens to also publish blocks. The message
	// mpool (net/mpool) publishes on /fil/msgs and is NOT a block publisher,
	// so without the explicit one, block submit is correctly unavailable.
	c.mu.Lock()
	bp := c.blockPub
	c.mu.Unlock()
	if bp == nil {
		if mbp, ok := c.Mpool.(BlockPublisher); ok && mbp != nil {
			bp = mbp
		}
	}
	if bp == nil {
		return errors.New("SyncSubmitBlock: no block publisher wired (need libp2p /fil/blocks publisher)")
	}
	return bp.PublishBlock(ctx, blk)
}

// BlockPublisher is the /fil/blocks gossipsub publish capability. Satisfied
// by *net/blockpub.Publisher.
type BlockPublisher interface {
	PublishBlock(ctx context.Context, blk *types.BlockMsg) error
}

// SetBlockPublisher wires the /fil/blocks publisher used by SyncSubmitBlock
// (PDP/backup tier). Safe to call after construction; nil clears it.
func (c *ChainAPI) SetBlockPublisher(bp BlockPublisher) {
	c.mu.Lock()
	c.blockPub = bp
	c.mu.Unlock()
}

// MarketAddBalance composes+signs+pushes a market deposit message.
// Tier 3 (#70). Stubbed pending Phase 5 sub-state decoders.
func (c *ChainAPI) MarketAddBalance(_ context.Context, _, _ address.Address, _ big.Int) (cid.Cid, error) {
	return cid.Undef, ErrNotImpl("MarketAddBalance", "needs market actor MethodNum lookup, see Phase 5")
}

// Compile-time assertion that ChainAPI satisfies api.FullNode.
var _ api.FullNode = (*ChainAPI)(nil)
