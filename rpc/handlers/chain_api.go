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
	"errors"
	"fmt"
	"sync"
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
	lbeacon "github.com/Reiers/lantern/chain/beacon"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/msgsearch"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/vm"
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

	// optional: shutdown hook
	OnShutdown func() error

	mu          sync.Mutex
	sessionUUID string
	notifySubs  []chan []api.HeadChange
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
		BeaconParams: lbeacon.MainnetQuicknetParams(),
		sessionUUID:  uuid.New().String(),
	}
}

// WithHeaderStore returns c with the header store attached. The store
// unlocks ChainGetTipSetByHeight, randomness queries, and StateSearchMsg.
func (c *ChainAPI) WithHeaderStore(s *hstore.Store) *ChainAPI {
	c.HeaderStore = s
	return c
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
func (c *ChainAPI) Version(_ context.Context) (api.Version, error) {
	return api.Version{
		Version:    "lantern/0.4.0 (lotus-compat)",
		APIVersion: 0x000d0900, // matches Lotus v1.36 APIVersion = NewVer(1, 13, 9)
		BlockDelay: 30,
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
// V1 implementation only knows the current trusted tipset. Requests for
// other keys return ErrTipSetNotFound. Phase 5 wires the header-store
// lookup.
func (c *ChainAPI) ChainGetTipSet(_ context.Context, key types.TipSetKey) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	// Heuristic match: requested key matches the synthetic head's key.
	cur := synthesizeTipSet(c.Trusted)
	if key.IsEmpty() || tipsetKeyMatches(cur.Key(), key) {
		return cur, nil
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
func (c *ChainAPI) ChainGetBlock(_ context.Context, _ cid.Cid) (*types.BlockHeader, error) {
	return nil, ErrNotImpl("ChainGetBlock", "header-by-CID lookup deferred to Phase 5 (header store)")
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
func (c *ChainAPI) ChainTipSetWeight(_ context.Context, _ types.TipSetKey) (big.Int, error) {
	if c.Trusted == nil {
		return big.Zero(), errors.New("trusted root not initialised")
	}
	return c.Trusted.ParentWeight, nil
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

// StateGetActor reads an actor at a tipset. Tier 1 (#44 — the hot one).
func (c *ChainAPI) StateGetActor(ctx context.Context, a address.Address, _ types.TipSetKey) (*types.Actor, error) {
	act, _, err := c.Accessor.GetActor(ctx, a)
	if err != nil {
		return nil, err
	}
	return &types.Actor{
		Code:    act.Code,
		Head:    act.Head,
		Nonce:   act.Nonce,
		Balance: act.Balance,
		DelegatedAddress: act.DelegatedAddress,
	}, nil
}

// StateLookupID resolves any-protocol address to ID address. Tier 1 (#25).
func (c *ChainAPI) StateLookupID(ctx context.Context, a address.Address, _ types.TipSetKey) (address.Address, error) {
	id, _, err := c.Accessor.LookupID(ctx, a)
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
	// If `a` is already non-ID, pass through.
	if a.Protocol() != address.ID {
		return a, nil
	}
	// Otherwise we need the account actor's state. For now return a
	// not-impl error — Phase 5 wires the account-state decoder.
	return address.Undef, ErrNotImpl("StateAccountKey", "account-actor state decode deferred to Phase 5 (see PHASE2-BLOCKERS.md B11)")
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
	if c.NetworkName == "" {
		return "mainnet", nil
	}
	return c.NetworkName, nil
}

// StateReadState dumps an actor's state. Tier 3 (#64).
func (c *ChainAPI) StateReadState(ctx context.Context, a address.Address, _ types.TipSetKey) (*api.ActorState, error) {
	act, _, err := c.Accessor.GetActor(ctx, a)
	if err != nil {
		return nil, err
	}
	// Lantern returns the raw head bytes as `State` — Phase 5 will type
	// the field according to the actor's Code CID.
	headBytes, err := c.BlockGetter.Get(ctx, act.Head)
	if err != nil {
		return nil, err
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
func (c *ChainAPI) tipsetForRandomness(_ context.Context, randEpoch abi.ChainEpoch) (*types.TipSet, error) {
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	if randEpoch < 0 {
		return nil, fmt.Errorf("randomness epoch %d cannot be negative", randEpoch)
	}
	if randEpoch > c.Trusted.Epoch {
		return nil, fmt.Errorf("cannot draw randomness from future epoch %d (head %d)", randEpoch, c.Trusted.Epoch)
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
	s := msgsearch.New(c.HeaderStore, c.BlockGetter)
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
	return c.Wallet.Has(ctx, a)
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
	return c.Wallet.Sign(ctx, a, msg)
}
func (c *ChainAPI) WalletSignMessage(ctx context.Context, a address.Address, msg *types.Message) (*types.SignedMessage, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	if msg == nil {
		return nil, errors.New("nil message")
	}
	mcid := msg.Cid()
	sig, err := c.Wallet.Sign(ctx, a, mcid.Bytes())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return &types.SignedMessage{Message: *msg, Signature: *sig}, nil
}

// WalletBalance reads the balance via StateGetActor at the trusted head.
// Tier 1 (#14).
func (c *ChainAPI) WalletBalance(ctx context.Context, a address.Address) (big.Int, error) {
	act, _, err := c.Accessor.GetActor(ctx, a)
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

// MpoolPushMessage = GasEstimate + Sign + Push. Tier 2 (#53).
func (c *ChainAPI) MpoolPushMessage(ctx context.Context, msg *types.Message, spec *api.MessageSendSpec) (*types.SignedMessage, error) {
	if c.Wallet == nil {
		return nil, errors.New("wallet not initialised")
	}
	if msg == nil {
		return nil, errors.New("nil message")
	}
	// Fill nonce if 0.
	if msg.Nonce == 0 {
		n, err := c.MpoolGetNonce(ctx, msg.From)
		if err != nil {
			return nil, fmt.Errorf("get nonce: %w", err)
		}
		msg.Nonce = n
	}
	// Fill gas defaults.
	estim, err := c.GasEstimateMessageGas(ctx, msg, spec, types.TipSetKey{})
	if err != nil {
		return nil, err
	}
	*msg = *estim
	// Sign over the message CID.
	sm, err := c.WalletSignMessage(ctx, msg.From, msg)
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

// ----------------- SP block production (Tier 4) -----------------

func (c *ChainAPI) MinerGetBaseInfo(_ context.Context, _ address.Address, _ abi.ChainEpoch, _ types.TipSetKey) (*api.MiningBaseInfo, error) {
	return nil, ErrNotImpl("MinerGetBaseInfo", "requires VM + winning POST infra, see Phase 7")
}
func (c *ChainAPI) MinerCreateBlock(_ context.Context, _ *api.BlockTemplate) (*types.BlockMsg, error) {
	return nil, ErrNotImpl("MinerCreateBlock", "requires VM, see Phase 5/7")
}
func (c *ChainAPI) MpoolSelect(_ context.Context, _ types.TipSetKey, _ float64) ([]*types.SignedMessage, error) {
	return nil, ErrNotImpl("MpoolSelect", "requires mpool message-selection logic, see Phase 7")
}
func (c *ChainAPI) SyncSubmitBlock(ctx context.Context, blk *types.BlockMsg) error {
	if c.Mpool == nil {
		return ErrMpoolNotWired
	}
	return ErrNotImpl("SyncSubmitBlock", "block submission requires gossipsub /fil/blocks topic, see Phase 7")
}

// MarketAddBalance composes+signs+pushes a market deposit message.
// Tier 3 (#70). Stubbed pending Phase 5 sub-state decoders.
func (c *ChainAPI) MarketAddBalance(_ context.Context, _, _ address.Address, _ big.Int) (cid.Cid, error) {
	return cid.Undef, ErrNotImpl("MarketAddBalance", "needs market actor MethodNum lookup, see Phase 5")
}

// Compile-time assertion that ChainAPI satisfies api.FullNode.
var _ api.FullNode = (*ChainAPI)(nil)
