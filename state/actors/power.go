// Versioned power-actor loader.
//
// Phase 5: v17 + v18 only. Returns a PowerState that exposes:
//  - Total network power (raw + quality-adjusted)
//  - Per-miner Claim lookup (raw + QA byte power)
//  - List of registered miners (used by StateListMiners)
//
// The power actor lives at the singleton ID address f04.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	builtin "github.com/filecoin-project/go-state-types/builtin"
	power17 "github.com/filecoin-project/go-state-types/builtin/v17/power"
	adt17 "github.com/filecoin-project/go-state-types/builtin/v17/util/adt"
	power18 "github.com/filecoin-project/go-state-types/builtin/v18/power"
	adt18 "github.com/filecoin-project/go-state-types/builtin/v18/util/adt"

	"github.com/Reiers/lantern/state/hamt"
)

// PowerClaim is a network-version-agnostic view of power.Claim.
type PowerClaim struct {
	RawBytePower    abi.StoragePower
	QualityAdjPower abi.StoragePower
}

// PowerTotals is a snapshot of the network's aggregate power.
type PowerTotals struct {
	RawBytePower         abi.StoragePower
	QualityAdjPower      abi.StoragePower
	MinerCount           int64
	MinerAboveMinCount   int64
	ThisEpochRawByte     abi.StoragePower
	ThisEpochQualityAdj  abi.StoragePower
	ThisEpochPledge      abi.TokenAmount
}

// PowerState is the version-agnostic interface for the power actor.
type PowerState interface {
	Version() int
	Totals() PowerTotals
	MinerPower(ctx context.Context, miner address.Address) (*PowerClaim, bool, error)
	ListMiners(ctx context.Context) ([]address.Address, error)
}

// PowerActorAddress is the singleton ID address of the power actor (f04).
var PowerActorAddress = mustIDAddr(4)

// MarketActorAddress is the singleton ID address of the market actor (f05).
var MarketActorAddress = mustIDAddr(5)

// VerifregActorAddress is the singleton ID address of the verifreg actor (f06).
var VerifregActorAddress = mustIDAddr(6)

// DatacapActorAddress is the singleton ID address of the datacap actor (f07).
var DatacapActorAddress = mustIDAddr(7)

// RewardActorAddress is the singleton ID address of the reward actor (f02).
var RewardActorAddress = mustIDAddr(2)

// CronActorAddress is the singleton ID address of the cron actor (f03).
var CronActorAddress = mustIDAddr(3)

// SystemActorAddress is the singleton ID address of the system actor (f00).
var SystemActorAddress = mustIDAddr(0)

// InitActorAddress is the singleton ID address of the init actor (f01).
var InitActorAddress = mustIDAddr(1)

func mustIDAddr(i uint64) address.Address {
	a, err := address.NewIDAddress(i)
	if err != nil {
		panic(err)
	}
	return a
}

// LoadPower fetches and decodes the power actor's state.
func LoadPower(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (PowerState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindPower {
		return nil, fmt.Errorf("LoadPower: code %s is %s, not storagepower", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch power head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("power head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s power17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding power v17: %w", err)
		}
		return &powerV17{s: &s, store: adt17.WrapStore(ctx, store), ctx: ctx}, nil
	case 18:
		var s power18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding power v18: %w", err)
		}
		return &powerV18{s: &s, store: adt18.WrapStore(ctx, store), ctx: ctx}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindPower, Version: info.Version}
}

// ----- v18 -----

type powerV18 struct {
	s     *power18.State
	store adt18.Store
	ctx   context.Context
}

func (p *powerV18) Version() int { return 18 }

func (p *powerV18) Totals() PowerTotals {
	return PowerTotals{
		RawBytePower:        p.s.TotalRawBytePower,
		QualityAdjPower:     p.s.TotalQualityAdjPower,
		MinerCount:          p.s.MinerCount,
		MinerAboveMinCount:  p.s.MinerAboveMinPowerCount,
		ThisEpochRawByte:    p.s.ThisEpochRawBytePower,
		ThisEpochQualityAdj: p.s.ThisEpochQualityAdjPower,
		ThisEpochPledge:     p.s.ThisEpochPledgeCollateral,
	}
}

func (p *powerV18) MinerPower(ctx context.Context, miner address.Address) (*PowerClaim, bool, error) {
	claims, err := adt18.AsMap(p.store, p.s.Claims, builtin.DefaultHamtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load claims v18: %w", err)
	}
	var c power18.Claim
	found, err := claims.Get(abi.AddrKey(miner), &c)
	if err != nil {
		return nil, false, fmt.Errorf("HAMT lookup %s v18: %w", miner, err)
	}
	if !found {
		return nil, false, nil
	}
	return &PowerClaim{RawBytePower: c.RawBytePower, QualityAdjPower: c.QualityAdjPower}, true, nil
}

func (p *powerV18) ListMiners(ctx context.Context) ([]address.Address, error) {
	claims, err := adt18.AsMap(p.store, p.s.Claims, builtin.DefaultHamtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load claims v18: %w", err)
	}
	var out []address.Address
	var c power18.Claim
	err = claims.ForEach(&c, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

// ----- v17 -----

type powerV17 struct {
	s     *power17.State
	store adt17.Store
	ctx   context.Context
}

func (p *powerV17) Version() int { return 17 }

func (p *powerV17) Totals() PowerTotals {
	return PowerTotals{
		RawBytePower:        p.s.TotalRawBytePower,
		QualityAdjPower:     p.s.TotalQualityAdjPower,
		MinerCount:          p.s.MinerCount,
		MinerAboveMinCount:  p.s.MinerAboveMinPowerCount,
		ThisEpochRawByte:    p.s.ThisEpochRawBytePower,
		ThisEpochQualityAdj: p.s.ThisEpochQualityAdjPower,
		ThisEpochPledge:     p.s.ThisEpochPledgeCollateral,
	}
}

func (p *powerV17) MinerPower(ctx context.Context, miner address.Address) (*PowerClaim, bool, error) {
	claims, err := adt17.AsMap(p.store, p.s.Claims, builtin.DefaultHamtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load claims v17: %w", err)
	}
	var c power17.Claim
	found, err := claims.Get(abi.AddrKey(miner), &c)
	if err != nil {
		return nil, false, fmt.Errorf("HAMT lookup %s v17: %w", miner, err)
	}
	if !found {
		return nil, false, nil
	}
	return &PowerClaim{RawBytePower: c.RawBytePower, QualityAdjPower: c.QualityAdjPower}, true, nil
}

func (p *powerV17) ListMiners(ctx context.Context) ([]address.Address, error) {
	claims, err := adt17.AsMap(p.store, p.s.Claims, builtin.DefaultHamtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load claims v17: %w", err)
	}
	var out []address.Address
	var c power17.Claim
	err = claims.ForEach(&c, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}
