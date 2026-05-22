// Versioned loaders for the remaining actors:
//   - Account     (singleton-but-many) — exposes pubkey address
//   - Init        (f01)                — address resolution + network name
//   - Verifreg    (f06)                — allocations + claims + verifier datacap
//   - Datacap     (f07)                — per-client datacap (post-nv17)
//   - Reward      (f02)                — network pledge/epoch reward
//   - Multisig    (variable)           — vested funds + signer set
//   - System      (f00)                — manifest CID
//
// Phase 5 scope: v17 + v18.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	builtin "github.com/filecoin-project/go-state-types/builtin"
	account17 "github.com/filecoin-project/go-state-types/builtin/v17/account"
	datacap17 "github.com/filecoin-project/go-state-types/builtin/v17/datacap"
	init17 "github.com/filecoin-project/go-state-types/builtin/v17/init"
	multisig17 "github.com/filecoin-project/go-state-types/builtin/v17/multisig"
	reward17 "github.com/filecoin-project/go-state-types/builtin/v17/reward"
	system17 "github.com/filecoin-project/go-state-types/builtin/v17/system"
	adt17 "github.com/filecoin-project/go-state-types/builtin/v17/util/adt"
	verifreg17 "github.com/filecoin-project/go-state-types/builtin/v17/verifreg"
	account18 "github.com/filecoin-project/go-state-types/builtin/v18/account"
	datacap18 "github.com/filecoin-project/go-state-types/builtin/v18/datacap"
	init18 "github.com/filecoin-project/go-state-types/builtin/v18/init"
	multisig18 "github.com/filecoin-project/go-state-types/builtin/v18/multisig"
	reward18 "github.com/filecoin-project/go-state-types/builtin/v18/reward"
	system18 "github.com/filecoin-project/go-state-types/builtin/v18/system"
	adt18 "github.com/filecoin-project/go-state-types/builtin/v18/util/adt"
	verifreg18 "github.com/filecoin-project/go-state-types/builtin/v18/verifreg"

	"github.com/Reiers/lantern/state/hamt"
)

// ----- Account -----

// AccountState exposes the pubkey-typed address of an account actor.
type AccountState interface {
	Version() int
	PubkeyAddress() address.Address
}

// LoadAccount fetches and decodes an account actor.
func LoadAccount(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (AccountState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindAccount {
		return nil, fmt.Errorf("LoadAccount: code %s is %s, not account", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch account head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("account head: %w", err)
	}
	switch info.Version {
	case 17:
		var s account17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding account v17: %w", err)
		}
		return &accountV17{s: &s}, nil
	case 18:
		var s account18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding account v18: %w", err)
		}
		return &accountV18{s: &s}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindAccount, Version: info.Version}
}

type accountV17 struct{ s *account17.State }

func (a *accountV17) Version() int                   { return 17 }
func (a *accountV17) PubkeyAddress() address.Address { return a.s.Address }

type accountV18 struct{ s *account18.State }

func (a *accountV18) Version() int                   { return 18 }
func (a *accountV18) PubkeyAddress() address.Address { return a.s.Address }

// ----- Init -----

// InitState exposes init-actor lookups: address resolution and network name.
type InitState interface {
	Version() int
	NetworkName() string
	NextID() abi.ActorID
	ResolveAddress(ctx context.Context, a address.Address) (address.Address, bool, error)
}

// LoadInit fetches and decodes the init actor's state.
func LoadInit(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (InitState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindInit {
		return nil, fmt.Errorf("LoadInit: code %s is %s, not init", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch init head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("init head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s init17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding init v17: %w", err)
		}
		return &initV17{s: &s, store: adt17.WrapStore(ctx, store)}, nil
	case 18:
		var s init18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding init v18: %w", err)
		}
		return &initV18{s: &s, store: adt18.WrapStore(ctx, store)}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindInit, Version: info.Version}
}

type initV17 struct {
	s     *init17.State
	store adt17.Store
}

func (i *initV17) Version() int        { return 17 }
func (i *initV17) NetworkName() string { return i.s.NetworkName }
func (i *initV17) NextID() abi.ActorID { return i.s.NextID }
func (i *initV17) ResolveAddress(ctx context.Context, a address.Address) (address.Address, bool, error) {
	return i.s.ResolveAddress(i.store, a)
}

type initV18 struct {
	s     *init18.State
	store adt18.Store
}

func (i *initV18) Version() int        { return 18 }
func (i *initV18) NetworkName() string { return i.s.NetworkName }
func (i *initV18) NextID() abi.ActorID { return i.s.NextID }
func (i *initV18) ResolveAddress(ctx context.Context, a address.Address) (address.Address, bool, error) {
	return i.s.ResolveAddress(i.store, a)
}

// ----- Verifreg -----

// Allocation is a network-version-agnostic verifreg.Allocation.
type Allocation struct {
	Client     abi.ActorID
	Provider   abi.ActorID
	Data       cid.Cid
	Size       abi.PaddedPieceSize
	TermMin    abi.ChainEpoch
	TermMax    abi.ChainEpoch
	Expiration abi.ChainEpoch
}

// Claim is a network-version-agnostic verifreg.Claim.
type Claim struct {
	Provider  abi.ActorID
	Client    abi.ActorID
	Data      cid.Cid
	Size      abi.PaddedPieceSize
	TermMin   abi.ChainEpoch
	TermMax   abi.ChainEpoch
	TermStart abi.ChainEpoch
	Sector    abi.SectorNumber
}

// VerifregState exposes the verified-registry actor.
type VerifregState interface {
	Version() int
	RootKey() address.Address
	FindAllocation(ctx context.Context, clientID address.Address, allocID uint64) (*Allocation, bool, error)
	AllocationsByClient(ctx context.Context, clientID address.Address) (map[uint64]Allocation, error)
	FindClaim(ctx context.Context, providerID address.Address, claimID uint64) (*Claim, bool, error)
	ClaimsByProvider(ctx context.Context, providerID address.Address) (map[uint64]Claim, error)
	VerifierStatus(ctx context.Context, verifier address.Address) (abi.StoragePower, bool, error)
}

// LoadVerifreg fetches and decodes the verified-registry actor's state.
func LoadVerifreg(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (VerifregState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindVerifreg {
		return nil, fmt.Errorf("LoadVerifreg: code %s is %s, not verifiedregistry", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch verifreg head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("verifreg head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s verifreg17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding verifreg v17: %w", err)
		}
		return &verifregV17{s: &s, store: adt17.WrapStore(ctx, store)}, nil
	case 18:
		var s verifreg18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding verifreg v18: %w", err)
		}
		return &verifregV18{s: &s, store: adt18.WrapStore(ctx, store)}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindVerifreg, Version: info.Version}
}

type verifregV18 struct {
	s     *verifreg18.State
	store adt18.Store
}

func (v *verifregV18) Version() int             { return 18 }
func (v *verifregV18) RootKey() address.Address { return v.s.RootKey }

func (v *verifregV18) FindAllocation(ctx context.Context, clientID address.Address, allocID uint64) (*Allocation, bool, error) {
	a, ok, err := v.s.FindAllocation(v.store, clientID, verifreg18.AllocationId(allocID))
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Allocation{
		Client: a.Client, Provider: a.Provider, Data: a.Data,
		Size: a.Size, TermMin: a.TermMin, TermMax: a.TermMax,
		Expiration: a.Expiration,
	}, true, nil
}

func (v *verifregV18) AllocationsByClient(ctx context.Context, clientID address.Address) (map[uint64]Allocation, error) {
	m, err := v.s.LoadAllocationsToMap(v.store, clientID)
	if err != nil {
		return nil, fmt.Errorf("load allocations v18: %w", err)
	}
	out := make(map[uint64]Allocation, len(m))
	for id, a := range m {
		out[uint64(id)] = Allocation{
			Client: a.Client, Provider: a.Provider, Data: a.Data,
			Size: a.Size, TermMin: a.TermMin, TermMax: a.TermMax,
			Expiration: a.Expiration,
		}
	}
	return out, nil
}

func (v *verifregV18) FindClaim(ctx context.Context, providerID address.Address, claimID uint64) (*Claim, bool, error) {
	c, ok, err := v.s.FindClaim(v.store, providerID, verifreg18.ClaimId(claimID))
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Claim{
		Provider: c.Provider, Client: c.Client, Data: c.Data,
		Size: c.Size, TermMin: c.TermMin, TermMax: c.TermMax,
		TermStart: c.TermStart, Sector: c.Sector,
	}, true, nil
}

func (v *verifregV18) ClaimsByProvider(ctx context.Context, providerID address.Address) (map[uint64]Claim, error) {
	m, err := v.s.LoadClaimsToMap(v.store, providerID)
	if err != nil {
		return nil, fmt.Errorf("load claims v18: %w", err)
	}
	out := make(map[uint64]Claim, len(m))
	for id, c := range m {
		out[uint64(id)] = Claim{
			Provider: c.Provider, Client: c.Client, Data: c.Data,
			Size: c.Size, TermMin: c.TermMin, TermMax: c.TermMax,
			TermStart: c.TermStart, Sector: c.Sector,
		}
	}
	return out, nil
}

func (v *verifregV18) VerifierStatus(ctx context.Context, verifier address.Address) (abi.StoragePower, bool, error) {
	verifiers, err := adt18.AsMap(v.store, v.s.Verifiers, builtin.DefaultHamtBitwidth)
	if err != nil {
		return big.Zero(), false, fmt.Errorf("load verifiers v18: %w", err)
	}
	var dc abi.StoragePower
	found, err := verifiers.Get(abi.AddrKey(verifier), &dc)
	if err != nil {
		return big.Zero(), false, err
	}
	if !found {
		return big.Zero(), false, nil
	}
	return dc, true, nil
}

type verifregV17 struct {
	s     *verifreg17.State
	store adt17.Store
}

func (v *verifregV17) Version() int             { return 17 }
func (v *verifregV17) RootKey() address.Address { return v.s.RootKey }

func (v *verifregV17) FindAllocation(ctx context.Context, clientID address.Address, allocID uint64) (*Allocation, bool, error) {
	a, ok, err := v.s.FindAllocation(v.store, clientID, verifreg17.AllocationId(allocID))
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Allocation{
		Client: a.Client, Provider: a.Provider, Data: a.Data,
		Size: a.Size, TermMin: a.TermMin, TermMax: a.TermMax,
		Expiration: a.Expiration,
	}, true, nil
}

func (v *verifregV17) AllocationsByClient(ctx context.Context, clientID address.Address) (map[uint64]Allocation, error) {
	m, err := v.s.LoadAllocationsToMap(v.store, clientID)
	if err != nil {
		return nil, fmt.Errorf("load allocations v17: %w", err)
	}
	out := make(map[uint64]Allocation, len(m))
	for id, a := range m {
		out[uint64(id)] = Allocation{
			Client: a.Client, Provider: a.Provider, Data: a.Data,
			Size: a.Size, TermMin: a.TermMin, TermMax: a.TermMax,
			Expiration: a.Expiration,
		}
	}
	return out, nil
}

func (v *verifregV17) FindClaim(ctx context.Context, providerID address.Address, claimID uint64) (*Claim, bool, error) {
	c, ok, err := v.s.FindClaim(v.store, providerID, verifreg17.ClaimId(claimID))
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Claim{
		Provider: c.Provider, Client: c.Client, Data: c.Data,
		Size: c.Size, TermMin: c.TermMin, TermMax: c.TermMax,
		TermStart: c.TermStart, Sector: c.Sector,
	}, true, nil
}

func (v *verifregV17) ClaimsByProvider(ctx context.Context, providerID address.Address) (map[uint64]Claim, error) {
	m, err := v.s.LoadClaimsToMap(v.store, providerID)
	if err != nil {
		return nil, fmt.Errorf("load claims v17: %w", err)
	}
	out := make(map[uint64]Claim, len(m))
	for id, c := range m {
		out[uint64(id)] = Claim{
			Provider: c.Provider, Client: c.Client, Data: c.Data,
			Size: c.Size, TermMin: c.TermMin, TermMax: c.TermMax,
			TermStart: c.TermStart, Sector: c.Sector,
		}
	}
	return out, nil
}

func (v *verifregV17) VerifierStatus(ctx context.Context, verifier address.Address) (abi.StoragePower, bool, error) {
	verifiers, err := adt17.AsMap(v.store, v.s.Verifiers, builtin.DefaultHamtBitwidth)
	if err != nil {
		return big.Zero(), false, fmt.Errorf("load verifiers v17: %w", err)
	}
	var dc abi.StoragePower
	found, err := verifiers.Get(abi.AddrKey(verifier), &dc)
	if err != nil {
		return big.Zero(), false, err
	}
	if !found {
		return big.Zero(), false, nil
	}
	return dc, true, nil
}

// ----- Datacap -----

// DatacapState exposes the per-client datacap balance from the datacap
// actor (post-nv17).
type DatacapState interface {
	Version() int
	Governor() address.Address
	Balance(ctx context.Context, client address.Address) (abi.StoragePower, error)
}

// LoadDatacap fetches and decodes the datacap actor's state.
func LoadDatacap(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (DatacapState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindDatacap {
		return nil, fmt.Errorf("LoadDatacap: code %s is %s, not datacap", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch datacap head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("datacap head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s datacap17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding datacap v17: %w", err)
		}
		return &datacapV17{s: &s, store: adt17.WrapStore(ctx, store)}, nil
	case 18:
		var s datacap18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding datacap v18: %w", err)
		}
		return &datacapV18{s: &s, store: adt18.WrapStore(ctx, store)}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindDatacap, Version: info.Version}
}

// The datacap actor stores per-client balance in TokenState.Balances. The
// key is the actor ID (varint-encoded) — keyer interface conversion is
// done via cbor varint encoding.

type datacapV17 struct {
	s     *datacap17.State
	store adt17.Store
}

func (d *datacapV17) Version() int              { return 17 }
func (d *datacapV17) Governor() address.Address { return d.s.Governor }

func (d *datacapV17) Balance(ctx context.Context, client address.Address) (abi.StoragePower, error) {
	if client.Protocol() != address.ID {
		return big.Zero(), fmt.Errorf("datacap balance requires ID address, got %s", client)
	}
	id, err := address.IDFromAddress(client)
	if err != nil {
		return big.Zero(), err
	}
	balances, err := adt17.AsMap(d.store, d.s.Token.Balances, int(d.s.Token.HamtBitWidth))
	if err != nil {
		return big.Zero(), fmt.Errorf("load datacap balances v17: %w", err)
	}
	var amt abi.TokenAmount
	found, err := balances.Get(abi.UIntKey(id), &amt)
	if err != nil {
		return big.Zero(), err
	}
	if !found {
		return big.Zero(), nil
	}
	// datacap.TokenAmount is in "FIL units" but represents datacap bytes
	// scaled by 1e18. Caller divides as needed; we return raw to match
	// Lotus' StateVerifiedClientStatus, which itself uses the raw value.
	return amt, nil
}

type datacapV18 struct {
	s     *datacap18.State
	store adt18.Store
}

func (d *datacapV18) Version() int              { return 18 }
func (d *datacapV18) Governor() address.Address { return d.s.Governor }

func (d *datacapV18) Balance(ctx context.Context, client address.Address) (abi.StoragePower, error) {
	if client.Protocol() != address.ID {
		return big.Zero(), fmt.Errorf("datacap balance requires ID address, got %s", client)
	}
	id, err := address.IDFromAddress(client)
	if err != nil {
		return big.Zero(), err
	}
	balances, err := adt18.AsMap(d.store, d.s.Token.Balances, int(d.s.Token.HamtBitWidth))
	if err != nil {
		return big.Zero(), fmt.Errorf("load datacap balances v18: %w", err)
	}
	var amt abi.TokenAmount
	found, err := balances.Get(abi.UIntKey(id), &amt)
	if err != nil {
		return big.Zero(), err
	}
	if !found {
		return big.Zero(), nil
	}
	return amt, nil
}

// ----- Reward -----

// RewardState exposes the reward actor (f02) sub-state used by pledge
// formulas. Phase 5: we expose just the fields needed.
type RewardState interface {
	Version() int
	ThisEpochBaselinePower() abi.StoragePower
	ThisEpochReward() abi.TokenAmount
	ThisEpochRewardSmoothed() (position, velocity big.Int)
	CumsumRealized() big.Int
	CumsumBaseline() big.Int
	EffectiveNetworkTime() abi.ChainEpoch
}

// LoadReward fetches and decodes the reward actor's state.
func LoadReward(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (RewardState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindReward {
		return nil, fmt.Errorf("LoadReward: code %s is %s, not reward", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch reward head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("reward head: %w", err)
	}
	switch info.Version {
	case 17:
		var s reward17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding reward v17: %w", err)
		}
		return &rewardV17{s: &s}, nil
	case 18:
		var s reward18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding reward v18: %w", err)
		}
		return &rewardV18{s: &s}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindReward, Version: info.Version}
}

type rewardV17 struct{ s *reward17.State }

func (r *rewardV17) Version() int                             { return 17 }
func (r *rewardV17) ThisEpochBaselinePower() abi.StoragePower { return r.s.ThisEpochBaselinePower }
func (r *rewardV17) ThisEpochReward() abi.TokenAmount         { return r.s.ThisEpochReward }
func (r *rewardV17) ThisEpochRewardSmoothed() (big.Int, big.Int) {
	return r.s.ThisEpochRewardSmoothed.PositionEstimate, r.s.ThisEpochRewardSmoothed.VelocityEstimate
}
func (r *rewardV17) CumsumRealized() big.Int              { return r.s.CumsumRealized }
func (r *rewardV17) CumsumBaseline() big.Int              { return r.s.CumsumBaseline }
func (r *rewardV17) EffectiveNetworkTime() abi.ChainEpoch { return r.s.EffectiveNetworkTime }

type rewardV18 struct{ s *reward18.State }

func (r *rewardV18) Version() int                             { return 18 }
func (r *rewardV18) ThisEpochBaselinePower() abi.StoragePower { return r.s.ThisEpochBaselinePower }
func (r *rewardV18) ThisEpochReward() abi.TokenAmount         { return r.s.ThisEpochReward }
func (r *rewardV18) ThisEpochRewardSmoothed() (big.Int, big.Int) {
	return r.s.ThisEpochRewardSmoothed.PositionEstimate, r.s.ThisEpochRewardSmoothed.VelocityEstimate
}
func (r *rewardV18) CumsumRealized() big.Int              { return r.s.CumsumRealized }
func (r *rewardV18) CumsumBaseline() big.Int              { return r.s.CumsumBaseline }
func (r *rewardV18) EffectiveNetworkTime() abi.ChainEpoch { return r.s.EffectiveNetworkTime }

// ----- Multisig -----

// MultisigState exposes the parts of the multisig actor needed by
// MsigGetAvailableBalance + MsigGetVested.
type MultisigState interface {
	Version() int
	Signers() []address.Address
	Threshold() uint64
	UnlockDuration() abi.ChainEpoch
	StartEpoch() abi.ChainEpoch
	InitialBalance() abi.TokenAmount
	// LockedBalance returns the amount still vesting at currEpoch.
	LockedBalance(currEpoch abi.ChainEpoch) abi.TokenAmount
}

// LoadMultisig fetches and decodes the multisig actor's state.
func LoadMultisig(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (MultisigState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindMultisig {
		return nil, fmt.Errorf("LoadMultisig: code %s is %s, not multisig", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch multisig head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("multisig head: %w", err)
	}
	switch info.Version {
	case 17:
		var s multisig17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding multisig v17: %w", err)
		}
		return &multisigV17{s: &s}, nil
	case 18:
		var s multisig18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding multisig v18: %w", err)
		}
		return &multisigV18{s: &s}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindMultisig, Version: info.Version}
}

type multisigV17 struct{ s *multisig17.State }

func (m *multisigV17) Version() int                    { return 17 }
func (m *multisigV17) Signers() []address.Address      { return m.s.Signers }
func (m *multisigV17) Threshold() uint64               { return m.s.NumApprovalsThreshold }
func (m *multisigV17) UnlockDuration() abi.ChainEpoch  { return m.s.UnlockDuration }
func (m *multisigV17) StartEpoch() abi.ChainEpoch      { return m.s.StartEpoch }
func (m *multisigV17) InitialBalance() abi.TokenAmount { return m.s.InitialBalance }
func (m *multisigV17) LockedBalance(curr abi.ChainEpoch) abi.TokenAmount {
	return m.s.AmountLocked(curr - m.s.StartEpoch)
}

type multisigV18 struct{ s *multisig18.State }

func (m *multisigV18) Version() int                    { return 18 }
func (m *multisigV18) Signers() []address.Address      { return m.s.Signers }
func (m *multisigV18) Threshold() uint64               { return m.s.NumApprovalsThreshold }
func (m *multisigV18) UnlockDuration() abi.ChainEpoch  { return m.s.UnlockDuration }
func (m *multisigV18) StartEpoch() abi.ChainEpoch      { return m.s.StartEpoch }
func (m *multisigV18) InitialBalance() abi.TokenAmount { return m.s.InitialBalance }
func (m *multisigV18) LockedBalance(curr abi.ChainEpoch) abi.TokenAmount {
	return m.s.AmountLocked(curr - m.s.StartEpoch)
}

// ----- System -----

// SystemState exposes the manifest CID from the system actor (f00).
type SystemState interface {
	Version() int
	BuiltinActors() cid.Cid
}

// LoadSystem fetches and decodes the system actor's state.
func LoadSystem(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (SystemState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindSystem {
		return nil, fmt.Errorf("LoadSystem: code %s is %s, not system", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch system head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("system head: %w", err)
	}
	switch info.Version {
	case 17:
		var s system17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding system v17: %w", err)
		}
		return &systemV17{s: &s}, nil
	case 18:
		var s system18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding system v18: %w", err)
		}
		return &systemV18{s: &s}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindSystem, Version: info.Version}
}

type systemV17 struct{ s *system17.State }

func (s *systemV17) Version() int           { return 17 }
func (s *systemV17) BuiltinActors() cid.Cid { return s.s.BuiltinActors }

type systemV18 struct{ s *system18.State }

func (s *systemV18) Version() int           { return 18 }
func (s *systemV18) BuiltinActors() cid.Cid { return s.s.BuiltinActors }
