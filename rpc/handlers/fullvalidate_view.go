package handlers

import (
	"context"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	gsbig "github.com/filecoin-project/go-state-types/big"

	"github.com/Reiers/lantern/chain/fullvalidate"
	"github.com/Reiers/lantern/chain/types"
)

// FullValidateView returns a fullvalidate.StateView backed by this ChainAPI's
// state accessor. A Full node uses it to re-verify block signature / VRF /
// win-count against resident F3-anchored state (issue #90). Light/PDP tiers
// don't call this; the Full-tier ingest path does.
func (c *ChainAPI) FullValidateView() fullvalidate.StateView {
	return chainAPIStateView{c}
}

// WinningPoStSectorView returns the fullvalidate.MinerSectorSetView backed by
// this ChainAPI so a Full node can additionally run pure-Go WinningPoSt
// SNARK verification (#87 + #88). Same underlying accessor as FullValidateView;
// separate return type keeps the WinningPoSt wiring opt-in.
func (c *ChainAPI) WinningPoStSectorView() fullvalidate.MinerSectorSetView {
	return chainAPIStateView{c}
}

type chainAPIStateView struct{ c *ChainAPI }

// WorkerKey resolves miner -> current worker -> pubkey address, matching
// StateMinerInfo(...).Worker then StateAccountKey.
func (v chainAPIStateView) WorkerKey(ctx context.Context, miner address.Address) (address.Address, error) {
	info, err := v.c.StateMinerInfo(ctx, miner, types.EmptyTSK)
	if err != nil {
		return address.Undef, err
	}
	// info.Worker is an ID address; resolve to its BLS/secp pubkey addr so
	// sigs.Verify can check the block/VRF signatures.
	return v.c.StateAccountKey(ctx, info.Worker, types.EmptyTSK)
}

// MinerQAPower returns (miner QA power, network total QA power) from the power
// actor, matching StateMinerPower semantics.
func (v chainAPIStateView) MinerQAPower(ctx context.Context, miner address.Address) (abi.StoragePower, abi.StoragePower, error) {
	mp, err := v.c.StateMinerPower(ctx, miner, types.EmptyTSK)
	if err != nil {
		return abi.NewStoragePower(0), abi.NewStoragePower(0), err
	}
	return mp.MinerPower.QualityAdjPower, mp.TotalPower.QualityAdjPower, nil
}

// MinerEligible mirrors Lotus stmgr.MinerEligibleToMine (post-v3 actors):
// non-empty QA-power claim, no fee debt, no active consensus fault. Reads are
// against the current-head accessor (the parent state for an ingested block).
func (v chainAPIStateView) MinerEligible(ctx context.Context, miner address.Address) (bool, error) {
	// (1) Non-empty QA-power claim.
	mp, err := v.c.StateMinerPower(ctx, miner, types.EmptyTSK)
	if err != nil {
		return false, err
	}
	if mp.MinerPower.QualityAdjPower.LessThanEqual(gsbig.Zero()) {
		return false, nil
	}

	// (2)+(3) Fee debt + consensus fault: load miner state directly.
	ms, _, err := v.c.accForReads().LoadMiner(ctx, miner)
	if err != nil {
		return false, err
	}
	if debt := ms.FeeDebt(); !debt.IsZero() {
		return false, nil
	}
	info, err := ms.Info(ctx)
	if err != nil {
		return false, err
	}
	head, err := v.c.ChainHead(ctx)
	if err != nil {
		return false, err
	}
	if head.Height() <= info.ConsensusFaultElapsed {
		return false, nil
	}
	return true, nil
}

// MinerActiveSectors returns the miner's active (non-faulty) sectors at the
// current head state, sorted by SectorNumber ascending. Implements
// fullvalidate.MinerSectorSetView so a Full node can run pure-Go WinningPoSt
// SNARK verification (#87 + #88). Sort order matches the proving-sectors
// bitfield's bit-index iteration in Lotus's GetSectorsForWinningPoSt, so a
// pure-Go challenge index resolves to the same sector.
func (v chainAPIStateView) MinerActiveSectors(ctx context.Context, miner address.Address) ([]fullvalidate.MinerSectorRef, error) {
	infos, err := v.c.StateMinerActiveSectors(ctx, miner, types.EmptyTSK)
	if err != nil {
		return nil, err
	}
	out := make([]fullvalidate.MinerSectorRef, 0, len(infos))
	for _, si := range infos {
		if si == nil {
			continue
		}
		out = append(out, fullvalidate.MinerSectorRef{
			SectorNumber: si.SectorNumber,
			SealedCID:    si.SealedCID,
			SealProof:    si.SealProof,
		})
	}
	return out, nil
}

var (
	_ fullvalidate.StateView          = chainAPIStateView{}
	_ fullvalidate.MinerSectorSetView = chainAPIStateView{}
)
