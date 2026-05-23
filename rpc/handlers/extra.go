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

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/build"
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

// --- Net* and Eth* health probes (Phase 10: live libp2p host wiring) ----
//
// Curio's webui consumes these for the "Chain Node Network" panel. When
// NetInfoSource is wired (daemon path) the methods read live state off the
// libp2p host's peerstore + bandwidth counter + AutoNAT subsystem. When it
// is nil (one-shot CLI invocations like `lantern wallet balance`) the
// methods return zero-value answers so typed clients don't error.

// NetPeers returns the currently-connected peer set with each peer's known
// multiaddrs. Reads off host.Network().Peers() + host.Peerstore().Addrs().
func (c *ChainAPI) NetPeers(_ context.Context) ([]struct {
	ID    string
	Addrs []string
}, error) {
	if c.NetInfoSource == nil {
		return []struct {
			ID    string
			Addrs []string
		}{}, nil
	}
	ps := c.NetInfoSource.Peers()
	out := make([]struct {
		ID    string
		Addrs []string
	}, 0, len(ps))
	for _, p := range ps {
		out = append(out, struct {
			ID    string
			Addrs []string
		}{ID: p.ID, Addrs: p.Addrs})
	}
	return out, nil
}

// NetAgentVersion returns the libp2p agent string of a remote peer as
// recorded in the peerstore. Returns "unknown" when the peerstore has no
// record for that peer (matches Lotus's behaviour for unseen peers).
func (c *ChainAPI) NetAgentVersion(_ context.Context, peerID string) (string, error) {
	if c.NetInfoSource == nil {
		return "lantern/unknown", nil
	}
	av := c.NetInfoSource.AgentVersion(peerID)
	if av == "" {
		return "unknown", nil
	}
	return av, nil
}

// NetConnectedness returns the libp2p network.Connectedness value for
// peerID. Returns 0 (NotConnected) when NetInfoSource is unwired or the
// peer is unknown.
func (c *ChainAPI) NetConnectedness(_ context.Context, peerID string) (int, error) {
	if c.NetInfoSource == nil {
		return 0, nil
	}
	return c.NetInfoSource.Connectedness(peerID), nil
}

// NetListening returns whether the host has any listen addresses. Lantern's
// daemon ALWAYS opens a libp2p listener (TCP+QUIC) on Phase 6+ so this is
// true in steady state; the false branch matters for early-boot probes.
func (c *ChainAPI) NetListening(_ context.Context) (bool, error) {
	if c.NetInfoSource == nil {
		return true, nil // CLI-only path: report true to keep Curio quiet.
	}
	return c.NetInfoSource.Listening(), nil
}

// EthBlockNumber returns the current head epoch as a 0x-prefixed hex
// string. Lantern doesn't run an FEVM block-number index, so this just
// mirrors the Filecoin chain epoch — which is what Curio's webui uses for
// the "Eth block height" display when no separate index is available.
func (c *ChainAPI) EthBlockNumber(_ context.Context) (string, error) {
	var epoch int64
	if c.HeaderStore != nil {
		epoch = int64(c.HeaderStore.HeadEpoch())
	} else if c.Trusted != nil {
		epoch = int64(c.Trusted.Epoch)
	}
	if epoch < 0 {
		epoch = 0
	}
	return fmt.Sprintf("0x%x", epoch), nil
}

// EthChainId returns the Ethereum-style chain identifier for the active
// Filecoin network. Filecoin's mapping:
//   mainnet     → 314    (0x13a)
//   calibration → 314159 (0x4cb2f)
//
// These are the published EIP-155 chain IDs; viem and synapse-sdk use
// them to scope signatures + reject cross-chain replays.
func (c *ChainAPI) EthChainId(_ context.Context) (string, error) {
	switch c.NetworkName {
	case "calibration":
		return "0x4cb2f", nil // 314159
	default:
		return "0x13a", nil // 314
	}
}

// EthAccounts returns an empty array. Lantern does not expose its
// wallet via eth_accounts because the keystore is administrator-only
// and clients sign locally with their own keys. Matches Glif's
// behaviour: returns [] on every call.
func (c *ChainAPI) EthAccounts(_ context.Context) ([]string, error) {
	return []string{}, nil
}

// EthMaxPriorityFeePerGas returns "0x0". Filecoin's fee market has no
// tip / priority-fee concept: gas is paid out of (baseFee + premium)
// where the premium is set per-message. EIP-1559-style tipping is not
// applicable. viem clients use this method during transaction
// preparation; returning 0 lets the call succeed without setting a tip.
func (c *ChainAPI) EthMaxPriorityFeePerGas(_ context.Context) (string, error) {
	return "0x0", nil
}

// EthGasPrice returns the chain's current floor base fee in attoFIL,
// hex-encoded. This is the EIP-1559 'gasPrice' compatibility shim;
// strictly speaking Filecoin uses base-fee + premium per message, but
// reporting MinimumBaseFee gives viem clients a workable estimate
// when they call gasPrice during transaction preparation.
//
// TODO(#26): when we wire chain-head base-fee tracking, return the
// actual head base-fee instead of the protocol minimum. For now,
// MinimumBaseFee is the safe floor.
func (c *ChainAPI) EthGasPrice(_ context.Context) (string, error) {
	return fmt.Sprintf("0x%x", build.MinimumBaseFee), nil
}

// NetBandwidthStats returns the live libp2p BandwidthCounter totals. The
// counter is installed at Host construction via libp2p.BandwidthReporter,
// so the values reflect stream-level bytes since the host started.
func (c *ChainAPI) NetBandwidthStats(_ context.Context) (api.NetBandwidthStats, error) {
	if c.NetInfoSource == nil {
		return api.NetBandwidthStats{}, nil
	}
	return c.NetInfoSource.BandwidthTotals(), nil
}

// NetAutoNatStatus returns the latest AutoNAT-discovered reachability +
// the host's public addresses. Light clients behind NAT report
// ReachabilityPrivate after ~30s; beacon nodes on public IPs report
// ReachabilityPublic with the dial-back addrs filled in.
func (c *ChainAPI) NetAutoNatStatus(_ context.Context) (api.NatInfo, error) {
	if c.NetInfoSource == nil {
		return api.NatInfo{Reachability: 0, PublicAddrs: nil}, nil
	}
	return c.NetInfoSource.AutoNatStatus(), nil
}
