// Phase 5 Part C: StateMarket* family.

package handlers

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/types"
)

// StateMarketBalance returns escrow+locked for an address. Tier 1 (#59).
func (c *ChainAPI) StateMarketBalance(ctx context.Context, a address.Address, _ types.TipSetKey) (api.MarketBalance, error) {
	id, _, err := c.Accessor.LookupID(ctx, a)
	if err != nil {
		return api.MarketBalance{}, fmt.Errorf("StateMarketBalance(%s) resolve: %w", a, err)
	}
	ms, _, err := c.Accessor.LoadMarket(ctx)
	if err != nil {
		return api.MarketBalance{}, fmt.Errorf("StateMarketBalance load market: %w", err)
	}
	bal, err := ms.BalanceOf(ctx, id)
	if err != nil {
		return api.MarketBalance{}, fmt.Errorf("StateMarketBalance(%s) lookup: %w", a, err)
	}
	return api.MarketBalance{Escrow: bal.Escrow, Locked: bal.Locked}, nil
}

// StateMarketStorageDeal walks Proposals + States AMTs for the deal.
// Tier 1 (#60).
func (c *ChainAPI) StateMarketStorageDeal(ctx context.Context, dealID abi.DealID, _ types.TipSetKey) (*api.MarketDeal, error) {
	ms, _, err := c.Accessor.LoadMarket(ctx)
	if err != nil {
		return nil, fmt.Errorf("StateMarketStorageDeal load market: %w", err)
	}
	prop, foundP, err := ms.GetDealProposal(ctx, dealID)
	if err != nil {
		return nil, fmt.Errorf("StateMarketStorageDeal proposal %d: %w", dealID, err)
	}
	if !foundP {
		return nil, fmt.Errorf("deal %d: proposal not found", dealID)
	}
	state, foundS, err := ms.GetDealState(ctx, dealID)
	if err != nil {
		return nil, fmt.Errorf("StateMarketStorageDeal state %d: %w", dealID, err)
	}
	out := &api.MarketDeal{
		Proposal: api.MarketDealProposal{
			PieceCID:             prop.PieceCID,
			PieceSize:            prop.PieceSize,
			VerifiedDeal:         prop.VerifiedDeal,
			Client:               prop.Client,
			Provider:             prop.Provider,
			Label:                prop.Label,
			StartEpoch:           prop.StartEpoch,
			EndEpoch:             prop.EndEpoch,
			StoragePricePerEpoch: prop.StoragePricePerEpoch,
			ProviderCollateral:   prop.ProviderCollateral,
			ClientCollateral:     prop.ClientCollateral,
		},
	}
	if foundS {
		out.State = api.MarketDealState{
			SectorStartEpoch: state.SectorStartEpoch,
			LastUpdatedEpoch: state.LastUpdatedEpoch,
			SlashEpoch:       state.SlashEpoch,
		}
	} else {
		// Inactive deal: Lotus returns SectorStartEpoch = -1.
		out.State = api.MarketDealState{
			SectorStartEpoch: -1,
			LastUpdatedEpoch: -1,
			SlashEpoch:       -1,
		}
	}
	return out, nil
}

// StateDealProviderCollateralBounds derives min/max collateral. Tier 2 (#56).
//
// Formula (since FIP-0028, post-nv13):
//
//	min = base × dealQA × (1 - networkPower / qaPowerEst)
//	max = 0.01 × supply.FilCirculating × dealQA / qaPowerEst
//
// We don't have circulating-supply yet; return a conservative bound where:
//   - min = 0 (provider can always opt to under-collateralize; chain
//     would reject but bound itself is non-negative).
//   - max = a large constant.
//
// The Phase 5 cut-corner: a full impl needs StateCirculatingSupply. Phase
// 5 ships the formula in state_compute.go.
func (c *ChainAPI) StateDealProviderCollateralBounds(ctx context.Context, size abi.PaddedPieceSize, verified bool, _ types.TipSetKey) (api.DealCollateralBounds, error) {
	// Conservative defaults. Curio uses this only to sanity-check
	// proposed deals; the chain validates collateral on PublishStorageDeals
	// anyway. Document the gap in PHASE5-BLOCKERS.md.
	_ = size
	_ = verified
	return api.DealCollateralBounds{
		Min: big.Zero(),
		Max: big.NewInt(1_000_000_000_000_000_000), // 1 FIL ceiling — placeholder
	}, nil
}
