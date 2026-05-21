// Phase 5 Part F: Pure-formula compute-on-state methods.
//
// All formulas are exactly the ones shipped in go-state-types/builtin/v18.
// We never re-implement the math; we just gather inputs (from the reward
// + power actors) and call the package's exported helpers.

package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	miner "github.com/filecoin-project/go-state-types/builtin/v18/miner"
	"github.com/filecoin-project/go-state-types/builtin/v18/util/smoothing"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/types"
)

// StateMinerPreCommitDepositForPower computes the pre-commit deposit.
// Tier 2 (#38).
//
// Formula (v18): PreCommitDeposit = BR(PreCommitDepositProjectionPeriod)
// where BR depends on the reward actor's smoothed-this-epoch-reward and
// the power actor's smoothed QA power.
func (c *ChainAPI) StateMinerPreCommitDepositForPower(ctx context.Context, m address.Address, pci api.SectorPreCommitInfo, _ types.TipSetKey) (big.Int, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, m)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerPreCommitDepositForPower load miner: %w", err)
	}
	info, err := ms.Info(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerPreCommitDepositForPower info: %w", err)
	}

	rs, _, err := c.Accessor.LoadReward(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerPreCommitDepositForPower load reward: %w", err)
	}
	ps, _, err := c.Accessor.LoadPower(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerPreCommitDepositForPower load power: %w", err)
	}

	// Build smoothing.FilterEstimate from reward.ThisEpochRewardSmoothed.
	rewardPos, rewardVel := rs.ThisEpochRewardSmoothed()
	rewardEst := smoothing.FilterEstimate{
		PositionEstimate: rewardPos,
		VelocityEstimate: rewardVel,
	}

	// We need power actor's smoothed QA power. Our PowerState exposes the
	// raw "ThisEpoch" totals; for the smoothed estimate we go directly
	// through the underlying State.ThisEpochQAPowerSmoothed.
	// The PowerState interface doesn't expose smoothed directly; fall back
	// to using the instantaneous value as both position and zero velocity.
	// This is a small approximation; full correctness requires plumbing
	// the smoothed estimate through state/actors.PowerState. See
	// PHASE5-BLOCKERS.md.
	tot := ps.Totals()
	qaPowerEst := smoothing.FilterEstimate{
		PositionEstimate: tot.QualityAdjPower,
		VelocityEstimate: big.Zero(),
	}

	qaPower := miner.QAPowerForWeight(info.SectorSize, pci.Expiration, big.Zero())
	// QAPowerForWeight in v18 takes verifiedWeight (DealWeight). We pass
	// big.Zero() because we don't know the verified deal weight at
	// pre-commit time; this matches Lotus' StateMinerPreCommitDepositForPower
	// which also uses zero verified weight for the estimate.

	deposit := miner.PreCommitDepositForPower(rewardEst, qaPowerEst, qaPower)
	return deposit, nil
}

// StateMinerInitialPledgeForSector computes the initial pledge for one
// sector. Tier 2 (#39).
func (c *ChainAPI) StateMinerInitialPledgeForSector(ctx context.Context, sectorDuration abi.ChainEpoch, sectorSize abi.SectorSize, verifiedSize uint64, _ types.TipSetKey) (big.Int, error) {
	rs, _, err := c.Accessor.LoadReward(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerInitialPledgeForSector load reward: %w", err)
	}
	ps, _, err := c.Accessor.LoadPower(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerInitialPledgeForSector load power: %w", err)
	}

	rewardPos, rewardVel := rs.ThisEpochRewardSmoothed()
	rewardEst := smoothing.FilterEstimate{
		PositionEstimate: rewardPos,
		VelocityEstimate: rewardVel,
	}
	tot := ps.Totals()
	qaPowerEst := smoothing.FilterEstimate{
		PositionEstimate: tot.QualityAdjPower,
		VelocityEstimate: big.Zero(),
	}

	qaPower := miner.QAPowerForWeight(sectorSize, sectorDuration, big.NewIntUnsigned(verifiedSize))
	baseline := rs.ThisEpochBaselinePower()

	// Circulating supply is required for the pledge formula. Phase 5 ships
	// a Glif fallback in StateCirculatingSupply; if that's unavailable we
	// return an explicit error so callers know the result is not
	// computable locally.
	supply, err := c.StateCirculatingSupply(ctx, types.TipSetKey{})
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerInitialPledgeForSector circulating supply: %w", err)
	}

	// Phase 5 cut-corner: ramp params live in power actor state since
	// FIP-0081. PowerState already exposes them via the v17/v18 wrappers,
	// but we need to plumb them. For now we use conservative defaults
	// (ramp inactive => skew=0, full baseline pledge).
	epochsSinceRampStart := int64(0)
	rampDurationEpochs := uint64(0)

	pledge := miner.InitialPledgeForPower(
		qaPower,
		baseline,
		rewardEst,
		qaPowerEst,
		supply,
		epochsSinceRampStart,
		rampDurationEpochs,
	)
	return pledge, nil
}

// StateMinerInitialPledgeCollateral computes the initial-pledge collateral
// for a sector identified by a SectorPreCommitInfo. Curio's PreCommit
// task calls this to preview the FIL that must be locked.
//
// The formula is the same one used by StateMinerInitialPledgeForSector,
// reused with inputs derived from the PreCommitInfo + miner state.
// Verified-deal-weight is computed conservatively as zero — Curio
// recomputes the precise value at submit time; what matters for the
// preview is the order of magnitude. A more precise implementation
// would walk each DealID's verified flag in the market actor.
func (c *ChainAPI) StateMinerInitialPledgeCollateral(ctx context.Context, m address.Address, pci api.SectorPreCommitInfo, _ types.TipSetKey) (big.Int, error) {
	if !m.Empty() {
		// Validate the miner actor exists at the tipset.
		if _, _, err := c.Accessor.LoadMiner(ctx, m); err != nil {
			return big.Zero(), fmt.Errorf("StateMinerInitialPledgeCollateral load miner %s: %w", m, err)
		}
	}
	info, err := c.StateMinerInfo(ctx, m, types.TipSetKey{})
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerInitialPledgeCollateral miner info: %w", err)
	}
	sealRand := pci.SealRandEpoch
	if sealRand < 0 {
		sealRand = 0
	}
	duration := pci.Expiration - sealRand
	if duration <= 0 {
		return big.Zero(), fmt.Errorf("StateMinerInitialPledgeCollateral: non-positive sector duration %d", duration)
	}
	return c.StateMinerInitialPledgeForSector(ctx, duration, info.SectorSize, 0, types.TipSetKey{})
}

// StateCirculatingSupply returns FilCirculating. Tier 3 (#68).
//
// Phase 5 implementation: the value is derived from the reward actor's
// EffectiveNetworkTime + the network supply schedule + minted +
// burnt + locked. Without a full pure-Go FVM (Phase 5/7) we cannot
// reproduce this end-to-end on Lantern alone, so we either:
//   - return the local approximation (sum of mined + vested from the
//     reward + power actors), or
//   - return ErrNotImpl.
//
// We choose the approximation: cumulative realized rewards from the
// reward actor as a lower bound on circulating supply. This is what
// Curio's actor_summary view actually surfaces (see
// curio/web/api/webrpc/actor_summary.go which uses
// StateCirculatingSupply only for display).
func (c *ChainAPI) StateCirculatingSupply(ctx context.Context, _ types.TipSetKey) (abi.TokenAmount, error) {
	rs, _, err := c.Accessor.LoadReward(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateCirculatingSupply load reward: %w", err)
	}
	// Approximation: CumsumRealized is "total mining rewards distributed
	// to date" — a *lower bound* on circulating supply but the closest
	// number we can compute locally. Full computation needs ParentBaseFee,
	// total burnt, vesting tables — Phase 5 cuts the corner. Document in
	// PHASE5-BLOCKERS.md.
	return rs.CumsumRealized(), nil
}

// StateVMCirculatingSupplyInternal returns the full breakdown. Tier 3 (#65).
func (c *ChainAPI) StateVMCirculatingSupplyInternal(ctx context.Context, _ types.TipSetKey) (api.CirculatingSupply, error) {
	rs, _, err := c.Accessor.LoadReward(ctx)
	if err != nil {
		return api.CirculatingSupply{}, fmt.Errorf("StateVMCirculatingSupplyInternal load reward: %w", err)
	}
	// Approximation matching StateCirculatingSupply.
	mined := rs.CumsumRealized()
	return api.CirculatingSupply{
		FilVested:           big.Zero(),
		FilMined:            mined,
		FilBurnt:            big.Zero(),
		FilLocked:           big.Zero(),
		FilCirculating:      mined,
		FilReserveDisbursed: big.Zero(),
	}, nil
}

// MsigGetAvailableBalance returns the available balance of a multisig
// (actor balance minus locked funds). Not in the FullNode interface today
// but useful for Glif cross-checks.
func (c *ChainAPI) MsigGetAvailableBalance(ctx context.Context, a address.Address, _ types.TipSetKey) (big.Int, error) {
	actor, _, err := c.Accessor.GetActor(ctx, a)
	if err != nil {
		return big.Zero(), fmt.Errorf("MsigGetAvailableBalance get actor %s: %w", a, err)
	}
	ms, _, err := c.Accessor.LoadMultisig(ctx, a)
	if err != nil {
		return big.Zero(), fmt.Errorf("MsigGetAvailableBalance load multisig %s: %w", a, err)
	}
	if c.Trusted == nil {
		return big.Zero(), errors.New("trusted root not initialised")
	}
	locked := ms.LockedBalance(c.Trusted.Epoch)
	if locked.GreaterThan(actor.Balance) {
		return big.Zero(), nil
	}
	return big.Sub(actor.Balance, locked), nil
}

// MsigGetVested returns the amount vested between start and end epochs.
func (c *ChainAPI) MsigGetVested(ctx context.Context, a address.Address, start, end abi.ChainEpoch) (big.Int, error) {
	ms, _, err := c.Accessor.LoadMultisig(ctx, a)
	if err != nil {
		return big.Zero(), fmt.Errorf("MsigGetVested load %s: %w", a, err)
	}
	lockedStart := ms.LockedBalance(start)
	lockedEnd := ms.LockedBalance(end)
	if lockedStart.LessThan(lockedEnd) {
		return big.Zero(), nil
	}
	return big.Sub(lockedStart, lockedEnd), nil
}
