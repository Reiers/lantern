// Phase 5 Part E: Sector / replica queries.

package handlers

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/types"
)

// StateSectorPreCommitInfo loads a pre-commit record from the miner's
// PreCommittedSectors HAMT. Tier 1 (#36).
func (c *ChainAPI) StateSectorPreCommitInfo(ctx context.Context, m address.Address, sector abi.SectorNumber, _ types.TipSetKey) (*api.SectorPreCommitOnChainInfo, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("StateSectorPreCommitInfo(%s, %d): %w", m, sector, err)
	}
	pi, found, err := ms.GetPrecommit(ctx, sector)
	if err != nil {
		return nil, fmt.Errorf("StateSectorPreCommitInfo(%s, %d) lookup: %w", m, sector, err)
	}
	if !found {
		return nil, nil
	}
	return pi, nil
}

// StateSectorGetInfo loads a sector from the miner's Sectors AMT.
// Tier 1 (#37).
func (c *ChainAPI) StateSectorGetInfo(ctx context.Context, m address.Address, sector abi.SectorNumber, _ types.TipSetKey) (*api.SectorOnChainInfo, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("StateSectorGetInfo(%s, %d): %w", m, sector, err)
	}
	si, found, err := ms.GetSector(ctx, sector)
	if err != nil {
		return nil, fmt.Errorf("StateSectorGetInfo(%s, %d) lookup: %w", m, sector, err)
	}
	if !found {
		return nil, nil
	}
	return si, nil
}

// StateSectorPartition returns (deadline, partition) for a sector. Tier 1 (#55).
func (c *ChainAPI) StateSectorPartition(ctx context.Context, m address.Address, sector abi.SectorNumber, _ types.TipSetKey) (*api.SectorLocation, error) {
	ms, _, err := c.Accessor.LoadMiner(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("StateSectorPartition(%s, %d): %w", m, sector, err)
	}
	dl, part, err := ms.FindSector(ctx, sector)
	if err != nil {
		return nil, fmt.Errorf("StateSectorPartition(%s, %d) find: %w", m, sector, err)
	}
	return &api.SectorLocation{Deadline: dl, Partition: part}, nil
}
