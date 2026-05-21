// Versioned market-actor loader.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	market17 "github.com/filecoin-project/go-state-types/builtin/v17/market"
	adt17 "github.com/filecoin-project/go-state-types/builtin/v17/util/adt"
	market18 "github.com/filecoin-project/go-state-types/builtin/v18/market"
	adt18 "github.com/filecoin-project/go-state-types/builtin/v18/util/adt"

	"github.com/Reiers/lantern/state/hamt"
)

// DealProposal is a network-version-agnostic view of market.DealProposal.
type DealProposal struct {
	PieceCID             cid.Cid
	PieceSize            abi.PaddedPieceSize
	VerifiedDeal         bool
	Client               address.Address
	Provider             address.Address
	Label                string // best-effort decode; empty if not a string label
	StartEpoch           abi.ChainEpoch
	EndEpoch             abi.ChainEpoch
	StoragePricePerEpoch abi.TokenAmount
	ProviderCollateral   abi.TokenAmount
	ClientCollateral     abi.TokenAmount
}

// DealState is the post-activation state of a deal.
type DealState struct {
	SectorNumber     abi.SectorNumber
	SectorStartEpoch abi.ChainEpoch
	LastUpdatedEpoch abi.ChainEpoch
	SlashEpoch       abi.ChainEpoch
}

// MarketBalance carries the escrow + locked balance for one address.
type MarketBalance struct {
	Escrow abi.TokenAmount
	Locked abi.TokenAmount
}

// MarketState is the version-agnostic interface for the market actor.
type MarketState interface {
	Version() int
	GetDealProposal(ctx context.Context, dealID abi.DealID) (*DealProposal, bool, error)
	GetDealState(ctx context.Context, dealID abi.DealID) (*DealState, bool, error)
	BalanceOf(ctx context.Context, a address.Address) (MarketBalance, error)
	AllBalances(ctx context.Context) (map[address.Address]MarketBalance, error)
	AllDeals(ctx context.Context, max int) (map[abi.DealID]*DealEntry, error)
}

// DealEntry combines proposal + state for a single deal.
type DealEntry struct {
	Proposal DealProposal
	State    DealState
}

// LoadMarket fetches and decodes the market actor's state.
func LoadMarket(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (MarketState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindMarket {
		return nil, fmt.Errorf("LoadMarket: code %s is %s, not storagemarket", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch market head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("market head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s market17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding market v17: %w", err)
		}
		return &marketV17{s: &s, store: adt17.WrapStore(ctx, store), ctx: ctx}, nil
	case 18:
		var s market18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding market v18: %w", err)
		}
		return &marketV18{s: &s, store: adt18.WrapStore(ctx, store), ctx: ctx}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindMarket, Version: info.Version}
}

// ----- v18 -----

type marketV18 struct {
	s     *market18.State
	store adt18.Store
	ctx   context.Context
}

func (m *marketV18) Version() int { return 18 }

func (m *marketV18) GetDealProposal(ctx context.Context, dealID abi.DealID) (*DealProposal, bool, error) {
	props, err := adt18.AsArray(m.store, m.s.Proposals, market18.ProposalsAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load proposals v18: %w", err)
	}
	var dp market18.DealProposal
	found, err := props.Get(uint64(dealID), &dp)
	if err != nil || !found {
		return nil, found, err
	}
	return dealProposalV18(&dp), true, nil
}

func (m *marketV18) GetDealState(ctx context.Context, dealID abi.DealID) (*DealState, bool, error) {
	states, err := adt18.AsArray(m.store, m.s.States, market18.StatesAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load states v18: %w", err)
	}
	var ds market18.DealState
	found, err := states.Get(uint64(dealID), &ds)
	if err != nil || !found {
		return nil, found, err
	}
	return &DealState{
		SectorNumber:     ds.SectorNumber,
		SectorStartEpoch: ds.SectorStartEpoch,
		LastUpdatedEpoch: ds.LastUpdatedEpoch,
		SlashEpoch:       ds.SlashEpoch,
	}, true, nil
}

func (m *marketV18) BalanceOf(ctx context.Context, a address.Address) (MarketBalance, error) {
	esc, err := adt18.AsBalanceTable(m.store, m.s.EscrowTable)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("load escrow v18: %w", err)
	}
	e, err := esc.Get(a)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("get escrow %s v18: %w", a, err)
	}
	lck, err := adt18.AsBalanceTable(m.store, m.s.LockedTable)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("load locked v18: %w", err)
	}
	l, err := lck.Get(a)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("get locked %s v18: %w", a, err)
	}
	return MarketBalance{Escrow: e, Locked: l}, nil
}

func (m *marketV18) AllBalances(ctx context.Context) (map[address.Address]MarketBalance, error) {
	// EscrowTable and LockedTable are HAMTs of (Address → BigInt). We walk
	// both and merge.
	out := make(map[address.Address]MarketBalance)
	esc, err := adt18.AsMap(m.store, m.s.EscrowTable, adt18.BalanceTableBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load escrow map v18: %w", err)
	}
	var v abi.TokenAmount
	err = esc.ForEach(&v, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		b := out[a]
		b.Escrow = v
		out[a] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	lck, err := adt18.AsMap(m.store, m.s.LockedTable, adt18.BalanceTableBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load locked map v18: %w", err)
	}
	err = lck.ForEach(&v, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		b := out[a]
		b.Locked = v
		out[a] = b
		return nil
	})
	return out, err
}

func (m *marketV18) AllDeals(ctx context.Context, max int) (map[abi.DealID]*DealEntry, error) {
	out := make(map[abi.DealID]*DealEntry)
	props, err := adt18.AsArray(m.store, m.s.Proposals, market18.ProposalsAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load proposals v18: %w", err)
	}
	var dp market18.DealProposal
	count := 0
	err = props.ForEach(&dp, func(i int64) error {
		if max > 0 && count >= max {
			return fmt.Errorf("stop")
		}
		dpc := dp
		dpv := dealProposalV18(&dpc)
		out[abi.DealID(i)] = &DealEntry{Proposal: *dpv}
		count++
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, err
	}
	states, err := adt18.AsArray(m.store, m.s.States, market18.StatesAmtBitwidth)
	if err != nil {
		return out, fmt.Errorf("load states v18: %w", err)
	}
	var ds market18.DealState
	err = states.ForEach(&ds, func(i int64) error {
		if e, ok := out[abi.DealID(i)]; ok {
			e.State = DealState{
				SectorNumber:     ds.SectorNumber,
				SectorStartEpoch: ds.SectorStartEpoch,
				LastUpdatedEpoch: ds.LastUpdatedEpoch,
				SlashEpoch:       ds.SlashEpoch,
			}
		}
		return nil
	})
	return out, err
}

func dealProposalV18(in *market18.DealProposal) *DealProposal {
	return &DealProposal{
		PieceCID:             in.PieceCID,
		PieceSize:            in.PieceSize,
		VerifiedDeal:         in.VerifiedDeal,
		Client:               in.Client,
		Provider:             in.Provider,
		Label:                labelString(in.Label),
		StartEpoch:           in.StartEpoch,
		EndEpoch:             in.EndEpoch,
		StoragePricePerEpoch: in.StoragePricePerEpoch,
		ProviderCollateral:   in.ProviderCollateral,
		ClientCollateral:     in.ClientCollateral,
	}
}

// labelString extracts a string label from a market.DealLabel; returns "" if
// the label is a byte string (rare) or unset.
func labelString(label market18.DealLabel) string {
	if label.IsString() {
		s, _ := label.ToString()
		return s
	}
	return ""
}

// ----- v17 -----

type marketV17 struct {
	s     *market17.State
	store adt17.Store
	ctx   context.Context
}

func (m *marketV17) Version() int { return 17 }

func (m *marketV17) GetDealProposal(ctx context.Context, dealID abi.DealID) (*DealProposal, bool, error) {
	props, err := adt17.AsArray(m.store, m.s.Proposals, market17.ProposalsAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load proposals v17: %w", err)
	}
	var dp market17.DealProposal
	found, err := props.Get(uint64(dealID), &dp)
	if err != nil || !found {
		return nil, found, err
	}
	return dealProposalV17(&dp), true, nil
}

func (m *marketV17) GetDealState(ctx context.Context, dealID abi.DealID) (*DealState, bool, error) {
	states, err := adt17.AsArray(m.store, m.s.States, market17.StatesAmtBitwidth)
	if err != nil {
		return nil, false, fmt.Errorf("load states v17: %w", err)
	}
	var ds market17.DealState
	found, err := states.Get(uint64(dealID), &ds)
	if err != nil || !found {
		return nil, found, err
	}
	return &DealState{
		SectorNumber:     ds.SectorNumber,
		SectorStartEpoch: ds.SectorStartEpoch,
		LastUpdatedEpoch: ds.LastUpdatedEpoch,
		SlashEpoch:       ds.SlashEpoch,
	}, true, nil
}

func (m *marketV17) BalanceOf(ctx context.Context, a address.Address) (MarketBalance, error) {
	esc, err := adt17.AsBalanceTable(m.store, m.s.EscrowTable)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("load escrow v17: %w", err)
	}
	e, err := esc.Get(a)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("get escrow %s v17: %w", a, err)
	}
	lck, err := adt17.AsBalanceTable(m.store, m.s.LockedTable)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("load locked v17: %w", err)
	}
	l, err := lck.Get(a)
	if err != nil {
		return MarketBalance{}, fmt.Errorf("get locked %s v17: %w", a, err)
	}
	return MarketBalance{Escrow: e, Locked: l}, nil
}

func (m *marketV17) AllBalances(ctx context.Context) (map[address.Address]MarketBalance, error) {
	out := make(map[address.Address]MarketBalance)
	esc, err := adt17.AsMap(m.store, m.s.EscrowTable, adt17.BalanceTableBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load escrow map v17: %w", err)
	}
	var v abi.TokenAmount
	err = esc.ForEach(&v, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		b := out[a]
		b.Escrow = v
		out[a] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	lck, err := adt17.AsMap(m.store, m.s.LockedTable, adt17.BalanceTableBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load locked map v17: %w", err)
	}
	err = lck.ForEach(&v, func(k string) error {
		a, err := address.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		b := out[a]
		b.Locked = v
		out[a] = b
		return nil
	})
	return out, err
}

func (m *marketV17) AllDeals(ctx context.Context, max int) (map[abi.DealID]*DealEntry, error) {
	out := make(map[abi.DealID]*DealEntry)
	props, err := adt17.AsArray(m.store, m.s.Proposals, market17.ProposalsAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load proposals v17: %w", err)
	}
	var dp market17.DealProposal
	count := 0
	err = props.ForEach(&dp, func(i int64) error {
		if max > 0 && count >= max {
			return fmt.Errorf("stop")
		}
		dpc := dp
		dpv := dealProposalV17(&dpc)
		out[abi.DealID(i)] = &DealEntry{Proposal: *dpv}
		count++
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, err
	}
	states, err := adt17.AsArray(m.store, m.s.States, market17.StatesAmtBitwidth)
	if err != nil {
		return out, fmt.Errorf("load states v17: %w", err)
	}
	var ds market17.DealState
	err = states.ForEach(&ds, func(i int64) error {
		if e, ok := out[abi.DealID(i)]; ok {
			e.State = DealState{
				SectorNumber:     ds.SectorNumber,
				SectorStartEpoch: ds.SectorStartEpoch,
				LastUpdatedEpoch: ds.LastUpdatedEpoch,
				SlashEpoch:       ds.SlashEpoch,
			}
		}
		return nil
	})
	return out, err
}

func dealProposalV17(in *market17.DealProposal) *DealProposal {
	return &DealProposal{
		PieceCID:             in.PieceCID,
		PieceSize:            in.PieceSize,
		VerifiedDeal:         in.VerifiedDeal,
		Client:               in.Client,
		Provider:             in.Provider,
		Label:                labelV17(in.Label),
		StartEpoch:           in.StartEpoch,
		EndEpoch:             in.EndEpoch,
		StoragePricePerEpoch: in.StoragePricePerEpoch,
		ProviderCollateral:   in.ProviderCollateral,
		ClientCollateral:     in.ClientCollateral,
	}
}

func labelV17(label market17.DealLabel) string {
	if label.IsString() {
		s, _ := label.ToString()
		return s
	}
	return ""
}

// silence unused-import linters when this file gets edited.
var _ = big.Zero
