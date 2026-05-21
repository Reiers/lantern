// Phase 5 Part D: StateVerifiedClient* + DataCap + verifreg allocations/claims.

package handlers

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	verifreg "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"

	"github.com/Reiers/lantern/chain/types"
)

// StateVerifiedClientStatus returns a client's remaining DataCap. Tier 1 (#66).
//
// Post-nv17 the source-of-truth lives in the dedicated Datacap actor (f07).
// Lotus' StateVerifiedClientStatus reads from there.
func (c *ChainAPI) StateVerifiedClientStatus(ctx context.Context, a address.Address, _ types.TipSetKey) (*big.Int, error) {
	id, _, err := c.Accessor.LookupID(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("StateVerifiedClientStatus(%s) resolve: %w", a, err)
	}
	ds, _, err := c.Accessor.LoadDatacap(ctx)
	if err != nil {
		return nil, fmt.Errorf("StateVerifiedClientStatus load datacap: %w", err)
	}
	dc, err := ds.Balance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("StateVerifiedClientStatus(%s) balance: %w", a, err)
	}
	if dc.IsZero() {
		// Lotus returns nil when not a verified client.
		return nil, nil
	}
	out := dc
	return &out, nil
}

// StateGetAllocation looks up a single allocation by (client, id). Tier 1 (#42).
func (c *ChainAPI) StateGetAllocation(ctx context.Context, client address.Address, allocID verifreg.AllocationId, _ types.TipSetKey) (*verifreg.Allocation, error) {
	id, _, err := c.Accessor.LookupID(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("StateGetAllocation resolve client %s: %w", client, err)
	}
	vs, _, err := c.Accessor.LoadVerifreg(ctx)
	if err != nil {
		return nil, fmt.Errorf("StateGetAllocation load verifreg: %w", err)
	}
	a, found, err := vs.FindAllocation(ctx, id, uint64(allocID))
	if err != nil {
		return nil, fmt.Errorf("StateGetAllocation lookup: %w", err)
	}
	if !found {
		return nil, nil
	}
	return &verifreg.Allocation{
		Client:     a.Client,
		Provider:   a.Provider,
		Data:       a.Data,
		Size:       a.Size,
		TermMin:    a.TermMin,
		TermMax:    a.TermMax,
		Expiration: a.Expiration,
	}, nil
}

// StateGetAllocations returns all allocations for one client.
//
// Lotus signature: StateGetAllocations(client, tsk) (map[AllocationId]Allocation, error).
// Our FullNode interface doesn't list this method yet, but we expose it via
// state_verifreg.go for downstream wiring + the demo. Adding the interface
// method is a follow-up.
func (c *ChainAPI) GetAllocationsForClient(ctx context.Context, client address.Address) (map[uint64]verifreg.Allocation, error) {
	id, _, err := c.Accessor.LookupID(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("resolve client %s: %w", client, err)
	}
	vs, _, err := c.Accessor.LoadVerifreg(ctx)
	if err != nil {
		return nil, fmt.Errorf("load verifreg: %w", err)
	}
	allocs, err := vs.AllocationsByClient(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]verifreg.Allocation, len(allocs))
	for k, a := range allocs {
		out[k] = verifreg.Allocation{
			Client:     a.Client,
			Provider:   a.Provider,
			Data:       a.Data,
			Size:       a.Size,
			TermMin:    a.TermMin,
			TermMax:    a.TermMax,
			Expiration: a.Expiration,
		}
	}
	return out, nil
}

// GetClaimsForProvider lists all claims for a given provider miner.
func (c *ChainAPI) GetClaimsForProvider(ctx context.Context, provider address.Address) (map[uint64]verifreg.Claim, error) {
	id, _, err := c.Accessor.LookupID(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %s: %w", provider, err)
	}
	vs, _, err := c.Accessor.LoadVerifreg(ctx)
	if err != nil {
		return nil, fmt.Errorf("load verifreg: %w", err)
	}
	claims, err := vs.ClaimsByProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]verifreg.Claim, len(claims))
	for k, cm := range claims {
		out[k] = verifreg.Claim{
			Provider:  cm.Provider,
			Client:    cm.Client,
			Data:      cm.Data,
			Size:      cm.Size,
			TermMin:   cm.TermMin,
			TermMax:   cm.TermMax,
			TermStart: cm.TermStart,
			Sector:    cm.Sector,
		}
	}
	return out, nil
}

// StateListMiners walks the Power actor's Claims HAMT keys. Tier 1 (#58).
func (c *ChainAPI) StateListMiners(ctx context.Context, _ types.TipSetKey) ([]address.Address, error) {
	ps, _, err := c.Accessor.LoadPower(ctx)
	if err != nil {
		return nil, fmt.Errorf("StateListMiners load power: %w", err)
	}
	return ps.ListMiners(ctx)
}

// VerifierStatus returns datacap left for a notary (pre-allocation verifier).
// Lotus' StateVerifierStatus signature; not in the FullNode interface yet
// but useful for cross-tests + the demo.
func (c *ChainAPI) VerifierStatus(ctx context.Context, verifier address.Address) (abi.StoragePower, error) {
	id, _, err := c.Accessor.LookupID(ctx, verifier)
	if err != nil {
		return big.Zero(), fmt.Errorf("VerifierStatus resolve %s: %w", verifier, err)
	}
	vs, _, err := c.Accessor.LoadVerifreg(ctx)
	if err != nil {
		return big.Zero(), fmt.Errorf("VerifierStatus load verifreg: %w", err)
	}
	dc, found, err := vs.VerifierStatus(ctx, id)
	if err != nil || !found {
		return big.Zero(), err
	}
	return dc, nil
}

// VerifiedRegistryRootKey returns the root verifier address.
func (c *ChainAPI) VerifiedRegistryRootKey(ctx context.Context) (address.Address, error) {
	vs, _, err := c.Accessor.LoadVerifreg(ctx)
	if err != nil {
		return address.Undef, fmt.Errorf("VerifiedRegistryRootKey load verifreg: %w", err)
	}
	return vs.RootKey(), nil
}
