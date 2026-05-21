// Phase 8 — small RPC surface gaps Part A's live Curio test exposed.
// (Bucket 1 in docs/phase8-part-a-results.md.)
//
// Each method here is intentionally small and corresponds to a one-liner
// in Lotus. They were left out of Phase 4-7 because they didn't appear in
// the priority Curio matrix; Part A proved real Curio installs probe
// them on startup.

package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/actors"
)

// mainnetGenesisCID is the canonical Filecoin mainnet genesis tipset's
// single-block CID. Cross-verified against Filecoin.ChainGetGenesis on
// api.node.glif.io on 2026-05-21:
//
//	bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2
const mainnetGenesisCID = "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2"

// ChainGetGenesis returns the genesis tipset for this network.
//
// Lantern V1 carries only the genesis CID, not the full block bytes. We
// synthesise a single-block TipSet whose CID matches the canonical
// mainnet genesis. Curio + lotus CLI both consume only .Cids[0] on the
// result of this method (Curio uses it as a network identity probe).
func (c *ChainAPI) ChainGetGenesis(_ context.Context) (*types.TipSet, error) {
	switch c.NetworkName {
	case "mainnet", "":
		gc, err := cid.Parse(mainnetGenesisCID)
		if err != nil {
			return nil, fmt.Errorf("parse mainnet genesis cid: %w", err)
		}
		return chainGetGenesisStub(gc), nil
	default:
		return nil, ErrNotImpl("ChainGetGenesis",
			"genesis CID for network "+c.NetworkName+" not embedded in build/")
	}
}

// chainGetGenesisStub fabricates the smallest TipSet that satisfies
// JSON consumers checking .Cids[0]. We don't have the genesis block
// bytes locally, so .Blocks is empty; the .Cids slice carries the
// canonical genesis CID which is the only thing callers compare.
func chainGetGenesisStub(c cid.Cid) *types.TipSet {
	return types.NewStubTipSet([]cid.Cid{c}, 0)
}

// MpoolBatchPush submits a slice of signed messages via the existing
// MpoolPush path, returning the resulting CIDs in order. Curio's
// retrieval-task harness uses this when several signed messages are
// ready to publish at once.
func (c *ChainAPI) MpoolBatchPush(ctx context.Context, sms []*types.SignedMessage) ([]cid.Cid, error) {
	out := make([]cid.Cid, 0, len(sms))
	for i, sm := range sms {
		cidOut, err := c.MpoolPush(ctx, sm)
		if err != nil {
			return out, fmt.Errorf("MpoolBatchPush[%d]: %w", i, err)
		}
		out = append(out, cidOut)
	}
	return out, nil
}

// MpoolBatchPushUntrusted is the no-validation cousin Lotus exposes
// alongside MpoolBatchPush. We route both through the same path: the
// MpoolPublisher already validates serialization + signature before
// publishing on gossipsub.
func (c *ChainAPI) MpoolBatchPushUntrusted(ctx context.Context, sms []*types.SignedMessage) ([]cid.Cid, error) {
	return c.MpoolBatchPush(ctx, sms)
}

// GasEstimateGasLimit returns just the GasLimit field of the full gas
// estimate. Lotus exposes both this and GasEstimateMessageGas; some
// Curio call sites prefer the narrow form.
func (c *ChainAPI) GasEstimateGasLimit(ctx context.Context, msg *types.Message, _ types.TipSetKey) (int64, error) {
	est := c.gasEstimator()
	if est == nil {
		return 0, errors.New("GasEstimateGasLimit: no VM shell configured")
	}
	r, err := est.EstimateMessageGas(ctx, msg, big.Zero())
	if err != nil {
		return 0, fmt.Errorf("GasEstimateGasLimit: %w", err)
	}
	return r.GasLimit, nil
}

// StateActorCodeCIDs returns the kind→codeCID manifest for the
// specified network version. Lotus maps nv → actor-version internally;
// we use the canonical table embedded in state/actors/bundles.go.
//
// The mapping nv → actor-version follows Filecoin's release history:
//
//	nv25 → actor v17
//	nv26 → actor v18
//
// Earlier mappings are present in Bundles but rarely queried.
func (c *ChainAPI) StateActorCodeCIDs(_ context.Context, nv network.Version) (map[string]cid.Cid, error) {
	av := networkVersionToActorVersion(nv)
	if av == 0 {
		return nil, fmt.Errorf("StateActorCodeCIDs: unsupported network version %d", nv)
	}
	want := c.NetworkName
	if want == "" {
		want = "mainnet"
	}
	for _, b := range actors.Bundles {
		if b.Version == av && b.Network == want {
			out := make(map[string]cid.Cid, len(b.Actors))
			for k, v := range b.Actors {
				out[k] = v
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("StateActorCodeCIDs: no manifest for actor version %d on %s", av, want)
}

// networkVersionToActorVersion mirrors the mapping in
// lotus@v1.36.0/chain/actors/policy/policy.go. Only the recent versions
// are wired; older ones fall back to 0 (caller surfaces an error).
func networkVersionToActorVersion(nv network.Version) int {
	switch {
	case nv >= 27:
		// nv27+ keeps actor v18 in the v1.36 release window; new actor
		// versions land at later upgrades.
		return 18
	case nv == 26:
		return 18
	case nv == 25:
		return 17
	case nv == 24:
		return 16
	case nv == 23:
		return 15
	default:
		return 0
	}
}
