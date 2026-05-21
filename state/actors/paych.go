// Versioned payment-channel actor loader.
//
// Scope: Phase 7 needs a read-only view of paych state for
// PaychAvailableFunds + PaychVoucherCheckValid + PaychVoucherList. We
// don't need state mutation (no PaychVoucherCreate signs an on-chain
// message via the paych actor — Curio uses the lane stored in the
// channel actor state purely for off-chain redemption math).
//
// As with the other loaders in this package, we cover v17 (nv25) and
// v18 (nv26) — the active mainnet actor versions.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"

	paych17 "github.com/filecoin-project/go-state-types/builtin/v17/paych"
	adt17 "github.com/filecoin-project/go-state-types/builtin/v17/util/adt"
	paych18 "github.com/filecoin-project/go-state-types/builtin/v18/paych"
	adt18 "github.com/filecoin-project/go-state-types/builtin/v18/util/adt"

	"github.com/Reiers/lantern/state/hamt"
)

// PaychLaneInfo is the network-version-agnostic view of one lane.
type PaychLaneInfo struct {
	ID       uint64
	Nonce    uint64
	Redeemed big.Int
}

// PaychChannelInfo is the network-version-agnostic view of a paych
// channel actor's relevant state fields.
type PaychChannelInfo struct {
	From            address.Address
	To              address.Address
	ToSend          big.Int
	SettlingAt      abi.ChainEpoch
	MinSettleHeight abi.ChainEpoch
	Lanes           []PaychLaneInfo
}

// PaychState exposes paych actor state in a version-agnostic way.
type PaychState interface {
	Version() int
	Info(ctx context.Context) (*PaychChannelInfo, error)
	GetLane(ctx context.Context, laneID uint64) (*PaychLaneInfo, bool, error)
}

// LoadPaych dispatches on Code CID -> versioned state loader.
func LoadPaych(ctx context.Context, code, head cid.Cid, bg hamt.BlockGetter, reg *Registry) (PaychState, error) {
	info, ok := reg.Lookup(code)
	if !ok {
		return nil, fmt.Errorf("unknown actor code %s", code)
	}
	if info.Kind != KindPaych {
		return nil, fmt.Errorf("code %s is not paych (got %s)", code, info.Kind)
	}
	raw, err := bg.Get(ctx, head)
	if err != nil {
		return nil, fmt.Errorf("fetch paych head %s: %w", head, err)
	}
	if err := hamt.VerifyBlockCID(head, raw); err != nil {
		return nil, fmt.Errorf("verify paych head: %w", err)
	}
	cs := cbornode.NewCborStore(&bgBlockstore{bg: bg})
	switch info.Version {
	case 17:
		var s paych17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decode paych v17 state: %w", err)
		}
		return &paychV17{s: s, store: adt17.WrapStore(ctx, cs)}, nil
	case 18:
		var s paych18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decode paych v18 state: %w", err)
		}
		return &paychV18{s: s, store: adt18.WrapStore(ctx, cs)}, nil
	}
	return nil, fmt.Errorf("paych version %d not supported in Phase 7", info.Version)
}

// --- v18 ---

type paychV18 struct {
	s     paych18.State
	store adt18.Store
}

func (p *paychV18) Version() int { return 18 }

func (p *paychV18) Info(ctx context.Context) (*PaychChannelInfo, error) {
	out := &PaychChannelInfo{
		From:            p.s.From,
		To:              p.s.To,
		ToSend:          p.s.ToSend,
		SettlingAt:      p.s.SettlingAt,
		MinSettleHeight: p.s.MinSettleHeight,
	}
	if !p.s.LaneStates.Defined() {
		return out, nil
	}
	arr, err := adt18.AsArray(p.store, p.s.LaneStates, paych18.LaneStatesAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load lane states: %w", err)
	}
	var lane paych18.LaneState
	if err := arr.ForEach(&lane, func(i int64) error {
		out.Lanes = append(out.Lanes, PaychLaneInfo{
			ID:       uint64(i),
			Nonce:    lane.Nonce,
			Redeemed: lane.Redeemed,
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk lanes: %w", err)
	}
	return out, nil
}

func (p *paychV18) GetLane(ctx context.Context, laneID uint64) (*PaychLaneInfo, bool, error) {
	if !p.s.LaneStates.Defined() {
		return nil, false, nil
	}
	arr, err := adt18.AsArray(p.store, p.s.LaneStates, paych18.LaneStatesAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load lane states: %w", err)
	}
	var lane paych18.LaneState
	found, err := arr.Get(laneID, &lane)
	if err != nil {
		return nil, false, fmt.Errorf("get lane %d: %w", laneID, err)
	}
	if !found {
		return nil, false, nil
	}
	return &PaychLaneInfo{
		ID:       laneID,
		Nonce:    lane.Nonce,
		Redeemed: lane.Redeemed,
	}, true, nil
}

// --- v17 ---

type paychV17 struct {
	s     paych17.State
	store adt17.Store
}

func (p *paychV17) Version() int { return 17 }

func (p *paychV17) Info(ctx context.Context) (*PaychChannelInfo, error) {
	out := &PaychChannelInfo{
		From:            p.s.From,
		To:              p.s.To,
		ToSend:          p.s.ToSend,
		SettlingAt:      p.s.SettlingAt,
		MinSettleHeight: p.s.MinSettleHeight,
	}
	if !p.s.LaneStates.Defined() {
		return out, nil
	}
	arr, err := adt17.AsArray(p.store, p.s.LaneStates, paych17.LaneStatesAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load lane states: %w", err)
	}
	var lane paych17.LaneState
	if err := arr.ForEach(&lane, func(i int64) error {
		out.Lanes = append(out.Lanes, PaychLaneInfo{
			ID:       uint64(i),
			Nonce:    lane.Nonce,
			Redeemed: lane.Redeemed,
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk lanes: %w", err)
	}
	return out, nil
}

func (p *paychV17) GetLane(ctx context.Context, laneID uint64) (*PaychLaneInfo, bool, error) {
	if !p.s.LaneStates.Defined() {
		return nil, false, nil
	}
	arr, err := adt17.AsArray(p.store, p.s.LaneStates, paych17.LaneStatesAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load lane states: %w", err)
	}
	var lane paych17.LaneState
	found, err := arr.Get(laneID, &lane)
	if err != nil {
		return nil, false, fmt.Errorf("get lane %d: %w", laneID, err)
	}
	if !found {
		return nil, false, nil
	}
	return &PaychLaneInfo{
		ID:       laneID,
		Nonce:    lane.Nonce,
		Redeemed: lane.Redeemed,
	}, true, nil
}
