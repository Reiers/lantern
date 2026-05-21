// Phase 5 Part B: StateMiner* family.
//
// All these methods load the requested miner actor (resolving via Init if
// `addr` is non-ID), pick the right go-state-types/builtin/vN/miner
// decoder via state/actors.Registry, then walk the relevant sub-state.

package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	miner "github.com/filecoin-project/go-state-types/builtin/v18/miner"
	"github.com/filecoin-project/go-state-types/dline"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/types"
)

var _ = abi.ChainEpoch(0) // keep abi import

// StateMinerInfo decodes the miner actor's Info sub-block. Tier 1 (#8).
func (c *ChainAPI) StateMinerInfo(ctx context.Context, a address.Address, _ types.TipSetKey) (api.MinerInfo, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return api.MinerInfo{}, fmt.Errorf("StateMinerInfo(%s): %w", a, err)
	}
	info, err := ms.Info(ctx)
	if err != nil {
		return api.MinerInfo{}, fmt.Errorf("StateMinerInfo(%s) Info: %w", a, err)
	}
	out := api.MinerInfo{
		Owner:                      info.Owner,
		Worker:                     info.Worker,
		NewWorker:                  info.NewWorker,
		ControlAddresses:           info.ControlAddresses,
		WorkerChangeEpoch:          info.WorkerChangeEpoch,
		PeerId:                     info.PeerId,
		Multiaddrs:                 info.Multiaddrs,
		WindowPoStProofType:        info.WindowPoStProofType,
		SectorSize:                 info.SectorSize,
		WindowPoStPartitionSectors: info.WindowPoStPartitionSectors,
		ConsensusFaultElapsed:      info.ConsensusFaultElapsed,
		Beneficiary:                info.Beneficiary,
		BeneficiaryTerm: &miner.BeneficiaryTerm{
			Quota:      info.BeneficiaryQuota,
			UsedQuota:  info.BeneficiaryUsedQuota,
			Expiration: info.BeneficiaryExpiration,
		},
	}
	if info.PendingBeneficiary != nil {
		out.PendingBeneficiaryTerm = &miner.PendingBeneficiaryChange{
			NewBeneficiary:        info.PendingBeneficiary.NewBeneficiary,
			NewQuota:              info.PendingBeneficiary.NewQuota,
			NewExpiration:         info.PendingBeneficiary.NewExpiration,
			ApprovedByBeneficiary: info.PendingBeneficiary.ApprovedByBeneficiary,
			ApprovedByNominee:     info.PendingBeneficiary.ApprovedByNominee,
		}
	}
	return out, nil
}

// StateMinerPower reads this miner's claim from the power actor + returns
// network totals. Tier 1 (#40).
func (c *ChainAPI) StateMinerPower(ctx context.Context, a address.Address, _ types.TipSetKey) (*api.MinerPower, error) {
	idAddr, _, err := c.Accessor.LookupID(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerPower(%s) resolve: %w", a, err)
	}
	ps, _, err := c.Accessor.LoadPower(ctx)
	if err != nil {
		return nil, fmt.Errorf("StateMinerPower load power: %w", err)
	}
	tot := ps.Totals()
	claim, found, err := ps.MinerPower(ctx, idAddr)
	if err != nil {
		return nil, fmt.Errorf("StateMinerPower(%s) lookup claim: %w", a, err)
	}
	out := &api.MinerPower{
		TotalPower: api.Claim{
			RawBytePower:    tot.RawBytePower,
			QualityAdjPower: tot.QualityAdjPower,
		},
	}
	if found {
		out.MinerPower = api.Claim{
			RawBytePower:    claim.RawBytePower,
			QualityAdjPower: claim.QualityAdjPower,
		}
		// HasMinPower: copy Lotus heuristic — non-zero raw byte power +
		// MinerAboveMinPowerCount semantics. We approximate by checking
		// whether RawBytePower is non-zero. Curio doesn't act on
		// HasMinPower beyond UI display.
		out.HasMinPower = !claim.RawBytePower.IsZero()
	} else {
		out.MinerPower = api.Claim{RawBytePower: big.Zero(), QualityAdjPower: big.Zero()}
	}
	return out, nil
}

// StateMinerSectors returns the requested sectors. If `filter` is nil the
// full Sectors AMT is walked. Tier 2 (#23).
func (c *ChainAPI) StateMinerSectors(ctx context.Context, a address.Address, filter *bitfield.BitField, _ types.TipSetKey) ([]*api.SectorOnChainInfo, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerSectors(%s): %w", a, err)
	}
	var out []*api.SectorOnChainInfo
	err = ms.AllSectors(ctx, filter, func(si *api.SectorOnChainInfo) error {
		cp := *si
		out = append(out, &cp)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("StateMinerSectors(%s) walk: %w", a, err)
	}
	return out, nil
}

// StateMinerActiveSectors returns sectors that are live and not faulty.
// Tier 1 (#54).
func (c *ChainAPI) StateMinerActiveSectors(ctx context.Context, a address.Address, _ types.TipSetKey) ([]*api.SectorOnChainInfo, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerActiveSectors(%s): %w", a, err)
	}
	// Union of ActiveSectors across all partitions / deadlines.
	var active []bitfield.BitField
	for dl := uint64(0); dl < ms.DeadlineCount(); dl++ {
		parts, err := ms.Partitions(ctx, dl)
		if err != nil {
			return nil, fmt.Errorf("StateMinerActiveSectors load dl %d: %w", dl, err)
		}
		for _, p := range parts {
			active = append(active, p.ActiveSectors)
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	merged, err := bitfield.MultiMerge(active...)
	if err != nil {
		return nil, fmt.Errorf("StateMinerActiveSectors merge: %w", err)
	}
	return c.StateMinerSectors(ctx, a, &merged, types.TipSetKey{})
}

// StateMinerProvingDeadline computes the current proving deadline. Tier 1 (#19).
func (c *ChainAPI) StateMinerProvingDeadline(ctx context.Context, a address.Address, _ types.TipSetKey) (*dline.Info, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerProvingDeadline(%s): %w", a, err)
	}
	if c.Trusted == nil {
		return nil, errors.New("trusted root not initialised")
	}
	return ms.ProvingDeadlineInfo(c.Trusted.Epoch), nil
}

// StateMinerDeadlines returns the 48 deadline metadata structs. Tier 1 (#41).
func (c *ChainAPI) StateMinerDeadlines(ctx context.Context, a address.Address, _ types.TipSetKey) ([]api.MinerDeadline, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerDeadlines(%s): %w", a, err)
	}
	count := ms.DeadlineCount()
	out := make([]api.MinerDeadline, 0, count)
	for i := uint64(0); i < count; i++ {
		dl, err := ms.Deadline(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("StateMinerDeadlines(%s) dl %d: %w", a, i, err)
		}
		out = append(out, api.MinerDeadline{
			PostSubmissions:      dl.PostSubmissions,
			DisputableProofCount: dl.DisputableProofCount,
		})
	}
	return out, nil
}

// StateMinerPartitions returns the per-partition state for a deadline.
// Tier 1 (#21).
func (c *ChainAPI) StateMinerPartitions(ctx context.Context, a address.Address, dlIdx uint64, _ types.TipSetKey) ([]api.Partition, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerPartitions(%s, %d): %w", a, dlIdx, err)
	}
	parts, err := ms.Partitions(ctx, dlIdx)
	if err != nil {
		return nil, fmt.Errorf("StateMinerPartitions(%s, %d) load: %w", a, dlIdx, err)
	}
	out := make([]api.Partition, 0, len(parts))
	for _, p := range parts {
		out = append(out, api.Partition{
			AllSectors:        p.AllSectors,
			FaultySectors:     p.FaultySectors,
			RecoveringSectors: p.RecoveringSectors,
			LiveSectors:       p.LiveSectors,
			ActiveSectors:     p.ActiveSectors,
		})
	}
	return out, nil
}

// StateMinerFaults returns the union of fault bitfields. Tier 2 (#61).
func (c *ChainAPI) StateMinerFaults(ctx context.Context, a address.Address, _ types.TipSetKey) (bitfield.BitField, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("StateMinerFaults(%s): %w", a, err)
	}
	return ms.Faults(ctx)
}

// StateMinerRecoveries returns the union of recovery bitfields. Tier 2 (#62).
func (c *ChainAPI) StateMinerRecoveries(ctx context.Context, a address.Address, _ types.TipSetKey) (bitfield.BitField, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("StateMinerRecoveries(%s): %w", a, err)
	}
	return ms.Recoveries(ctx)
}

// StateMinerAvailableBalance computes the miner's withdrawable balance.
// Tier 1 (#10).
func (c *ChainAPI) StateMinerAvailableBalance(ctx context.Context, a address.Address, _ types.TipSetKey) (big.Int, error) {
	actor, _, err := c.Accessor.GetActor(ctx, a)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerAvailableBalance(%s) get actor: %w", a, err)
	}
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return big.Zero(), fmt.Errorf("StateMinerAvailableBalance(%s) load: %w", a, err)
	}
	return ms.AvailableBalance(actor.Balance)
}

// StateMinerAllocated returns the bitfield of allocated sector IDs.
// Tier 2 (#48).
func (c *ChainAPI) StateMinerAllocated(ctx context.Context, a address.Address, _ types.TipSetKey) (*bitfield.BitField, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateMinerAllocated(%s): %w", a, err)
	}
	root := ms.AllocatedSectorsRoot()
	raw, err := c.BlockGetter.Get(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("StateMinerAllocated fetch bitfield %s: %w", root, err)
	}
	bf, err := bitfield.NewFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("StateMinerAllocated decode bitfield: %w", err)
	}
	return &bf, nil
}

// StateMinerSectorAllocated reports whether a specific sector number has
// been allocated for this miner. Curio's pre-flight sector allocator
// uses this to pick the next sector number.
func (c *ChainAPI) StateMinerSectorAllocated(ctx context.Context, a address.Address, s abi.SectorNumber, _ types.TipSetKey) (bool, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return false, fmt.Errorf("StateMinerSectorAllocated(%s,%d): %w", a, s, err)
	}
	root := ms.AllocatedSectorsRoot()
	raw, err := c.BlockGetter.Get(ctx, root)
	if err != nil {
		return false, fmt.Errorf("StateMinerSectorAllocated fetch bitfield %s: %w", root, err)
	}
	bf, err := bitfield.NewFromBytes(raw)
	if err != nil {
		return false, fmt.Errorf("StateMinerSectorAllocated decode bitfield: %w", err)
	}
	isSet, err := bf.IsSet(uint64(s))
	if err != nil {
		return false, fmt.Errorf("StateMinerSectorAllocated isSet(%d): %w", s, err)
	}
	return isSet, nil
}

// StateMinerSectorCount returns counts of active/faulty/live sectors.
// Tier 3 (#67).
func (c *ChainAPI) StateMinerSectorCount(ctx context.Context, a address.Address, _ types.TipSetKey) (api.MinerSectors, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, a)
	if err != nil {
		return api.MinerSectors{}, fmt.Errorf("StateMinerSectorCount(%s): %w", a, err)
	}
	var liveBfs, activeBfs, faultyBfs []bitfield.BitField
	for dl := uint64(0); dl < ms.DeadlineCount(); dl++ {
		parts, err := ms.Partitions(ctx, dl)
		if err != nil {
			return api.MinerSectors{}, fmt.Errorf("StateMinerSectorCount partitions dl %d: %w", dl, err)
		}
		for _, p := range parts {
			liveBfs = append(liveBfs, p.LiveSectors)
			activeBfs = append(activeBfs, p.ActiveSectors)
			faultyBfs = append(faultyBfs, p.FaultySectors)
		}
	}
	live, _ := bitfield.MultiMerge(liveBfs...)
	active, _ := bitfield.MultiMerge(activeBfs...)
	faulty, _ := bitfield.MultiMerge(faultyBfs...)
	liveCount, _ := live.Count()
	activeCount, _ := active.Count()
	faultyCount, _ := faulty.Count()
	return api.MinerSectors{
		Live:    liveCount,
		Active:  activeCount,
		Faulty:  faultyCount,
	}, nil
}
