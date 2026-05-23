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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdbig "math/big"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

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

// EthSyncing returns false because Lantern is a light client — we're
// always anchored to our trust root, so there's no 'syncing' state
// the way a full node has. Returns `false` (not an object) to match
// the typical Ethereum convention when a node is fully synced.
//
// Note: returns `any` because the JSON-RPC convention is for this
// method to return EITHER `false` (when synced) OR a SyncStatus
// object. Always-returning-false is the simpler shape and matches
// what most viem clients expect.
func (c *ChainAPI) EthSyncing(_ context.Context) (any, error) {
	return false, nil
}

// EthGetBalance returns the balance of an Ethereum-shaped address as
// a 0x-prefixed hex attoFIL amount.
//
// Address resolution:
//   - 0x-prefixed 20-byte ETH address
//   - check if it's a masked-ID address (0xff prefix in first 12 bytes)
//     → decode as f0 (Filecoin ID address)
//   - otherwise → wrap as f4-namespaced delegated address under EAM
//     (ActorID 10)
//
// Block parameter is accepted for API compatibility but ignored:
// Lantern reads state at the trusted-root tipset only.
//
// Closes part of lantern#29.
func (c *ChainAPI) EthGetBalance(ctx context.Context, addrHex string, _ any) (string, error) {
	if c.Accessor == nil {
		return "", xerrors.New("state accessor not configured")
	}

	// Strip 0x prefix.
	h := addrHex
	if len(h) >= 2 && (h[:2] == "0x" || h[:2] == "0X") {
		h = h[2:]
	}
	if len(h) != 40 {
		return "", xerrors.Errorf("eth address must be 20 bytes (40 hex chars), got %d chars", len(h))
	}
	raw := make([]byte, 20)
	if _, err := hex.Decode(raw, []byte(h)); err != nil {
		return "", xerrors.Errorf("decode eth address: %w", err)
	}

	var filAddr address.Address
	var err error

	// Detect masked-ID address: first byte 0xff, next 11 bytes 0x00.
	// FEVM convention is 0xff || 11 zero bytes || big-endian uint64 actor ID.
	maskedID := raw[0] == 0xff
	for i := 1; i < 12 && maskedID; i++ {
		if raw[i] != 0x00 {
			maskedID = false
		}
	}
	if maskedID {
		// Last 8 bytes are big-endian uint64 actor ID.
		actorID := uint64(0)
		for i := 12; i < 20; i++ {
			actorID = (actorID << 8) | uint64(raw[i])
		}
		filAddr, err = address.NewIDAddress(actorID)
		if err != nil {
			return "", xerrors.Errorf("build id address: %w", err)
		}
	} else {
		filAddr, err = address.NewDelegatedAddress(builtin.EthereumAddressManagerActorID, raw)
		if err != nil {
			return "", xerrors.Errorf("build f4 address: %w", err)
		}
	}

	actor, _, err := c.Accessor.GetActor(ctx, filAddr)
	if err != nil {
		// Unknown actor returns 0 balance, matching Ethereum convention
		// for never-used addresses.
		return "0x0", nil
	}
	return "0x" + actor.Balance.Int.Text(16), nil
}

// EthGetBlockByNumber returns a tipset reshaped to look like an
// Ethereum block. fullTx is accepted but ignored — Lantern's header
// store doesn't carry full message bodies, only block headers and
// chain structure.
//
// blockParam accepts: 0x-hex epoch, "latest", "earliest", "pending",
// "safe", "finalized". All non-numeric values resolve to head.
//
// Closes part of lantern#29.
func (c *ChainAPI) EthGetBlockByNumber(ctx context.Context, blockParam string, _ bool) (any, error) {
	if c.HeaderStore == nil {
		return nil, xerrors.New("header store not configured")
	}

	var epoch int64
	switch blockParam {
	case "earliest":
		epoch = 0
	case "latest", "pending", "safe", "finalized", "":
		epoch = int64(c.HeaderStore.HeadEpoch())
	default:
		// Parse 0x-hex.
		h := blockParam
		if len(h) >= 2 && (h[:2] == "0x" || h[:2] == "0X") {
			h = h[2:]
		}
		parsed, ok := new(stdbig.Int).SetString(h, 16)
		if !ok {
			return nil, xerrors.Errorf("bad block number %q", blockParam)
		}
		epoch = parsed.Int64()
	}

	ts, err := c.HeaderStore.GetTipSetByHeight(abi.ChainEpoch(epoch))
	if err != nil || ts == nil {
		return nil, nil // matches Ethereum convention: unknown block returns null
	}

	// Use the first block in the tipset as the canonical block id.
	blocks := ts.Blocks()
	if len(blocks) == 0 {
		return nil, nil
	}
	b := blocks[0]

	cidStrs := make([]string, len(blocks))
	for i, blk := range blocks {
		cidStrs[i] = blk.Cid().String()
	}

	// ETH-shaped block fields. Many Filecoin concepts have no ETH
	// equivalent; we return zero/empty for those and trust the client
	// to only consult the fields it cares about (typical viem use is
	// number + timestamp + hash).
	return map[string]any{
		"number":           fmt.Sprintf("0x%x", int64(b.Height)),
		"hash":             b.Cid().String(),
		"parentHash":       firstCidOrEmpty(b.Parents),
		"nonce":            "0x0000000000000000",
		"sha3Uncles":       "0x0000000000000000000000000000000000000000000000000000000000000000",
		"logsBloom":        "0x" + zeroPad(512),
		"transactionsRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
		"stateRoot":        b.ParentStateRoot.String(),
		"receiptsRoot":     b.ParentMessageReceipts.String(),
		"miner":            b.Miner.String(),
		"difficulty":       "0x0",
		"totalDifficulty":  "0x0",
		"extraData":        "0x",
		"size":             "0x0",
		"gasLimit":         fmt.Sprintf("0x%x", build.BlockGasLimit),
		"gasUsed":          "0x0", // we don't track per-block gas use
		"timestamp":        fmt.Sprintf("0x%x", b.Timestamp),
		"transactions":     []string{}, // empty when fullTx=false (we ignore fullTx for now)
		"uncles":           []string{},
		// Filecoin extension: surface the full tipset CIDs so clients
		// that know about Filecoin can disambiguate sibling blocks.
		"filecoinTipsetCids": cidStrs,
	}, nil
}

// --- FEVM bridge-forwarding handlers (lantern#30) -----------------------
//
// Lantern's native VM is a gas-accurate Send-only shell; it can't
// execute FEVM bytecode. The three eth_* methods below forward to an
// upstream Forest/Lotus via the existing VMBridge. When --vm-bridge-rpc
// isn't configured, they return ErrNotImpl with a pointer at the flag.
//
// Architectural note: this preserves the 'no Glif sidecar' promise.
// 'No Glif sidecar' means 'no DEPENDENCY on Glif.' Operators with
// their own Lotus/Forest use that as the FEVM oracle via VMBridge.
// Glif is just one possible VMBridge target.
// ------------------------------------------------------------------------

// errBridgeUnconfigured is returned when an FEVM-requiring method is
// called on a daemon without --vm-bridge-rpc. The message points at
// the flag so operators can self-diagnose.
var errBridgeUnconfigured = xerrors.New("FEVM method requires --vm-bridge-rpc pointing at a Forest/Lotus node")

// EthCall forwards an eth_call to the upstream VM bridge.
// Synapse-SDK / viem `readContract` / `simulateContract` calls hit
// this method.
//
// We pass the request through verbatim — the upstream is responsible
// for executing the FEVM bytecode. Lantern's contribution is its
// trust-anchored chain head (we don't speak the FEVM ourselves, but
// we know which block to ask about).
func (c *ChainAPI) EthCall(ctx context.Context, callObj any, blockParam any) (string, error) {
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{callObj, blockParam})
	if err != nil {
		return "", xerrors.Errorf("marshal eth_call params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_call", params)
	if err != nil {
		return "", xerrors.Errorf("bridge eth_call: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", xerrors.Errorf("decode eth_call result: %w", err)
	}
	return out, nil
}

// EthEstimateGas forwards an eth_estimateGas to the upstream VM bridge.
// viem `writeContract` calls this during transaction preparation to
// size the gas limit.
func (c *ChainAPI) EthEstimateGas(ctx context.Context, callObj any) (string, error) {
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{callObj})
	if err != nil {
		return "", xerrors.Errorf("marshal eth_estimateGas params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_estimateGas", params)
	if err != nil {
		return "", xerrors.Errorf("bridge eth_estimateGas: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", xerrors.Errorf("decode eth_estimateGas result: %w", err)
	}
	return out, nil
}

// EthGetTransactionCount returns the transaction count (nonce) for an
// Ethereum address. Forwarded to the upstream VM bridge because Lantern's
// own state tree doesn't currently include f4 account nonces — it would
// require either a full FEVM state import or a custom EthAccountActor
// reader. Cheap to forward; cheap to migrate to a local implementation
// later when state-tree backfill catches up (lantern#3).
func (c *ChainAPI) EthGetTransactionCount(ctx context.Context, addr string, blockParam any) (string, error) {
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{addr, blockParam})
	if err != nil {
		return "", xerrors.Errorf("marshal eth_getTransactionCount params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_getTransactionCount", params)
	if err != nil {
		return "", xerrors.Errorf("bridge eth_getTransactionCount: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", xerrors.Errorf("decode eth_getTransactionCount result: %w", err)
	}
	return out, nil
}

// EthGetTransactionReceipt returns the receipt for a previously-broadcast
// transaction. Forwarded to the upstream VM bridge: receipts require
// indexed message lookups + execution result reconstruction which Lantern
// doesn't run today. Returns nil when the tx isn't found (caller will
// retry; the standard go-ethereum receipt poll loop expects this shape).
func (c *ChainAPI) EthGetTransactionReceipt(ctx context.Context, txHash string) (any, error) {
	if c.Bridge == nil {
		return nil, errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{txHash})
	if err != nil {
		return nil, xerrors.Errorf("marshal eth_getTransactionReceipt params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_getTransactionReceipt", params)
	if err != nil {
		return nil, xerrors.Errorf("bridge eth_getTransactionReceipt: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, xerrors.Errorf("decode eth_getTransactionReceipt result: %w", err)
	}
	return out, nil
}

// EthFeeHistory returns historical gas fee data used by tx builders to
// suggest EIP-1559 priority fees. Forwarded to the upstream VM bridge
// since Filecoin's fee market shape doesn't map 1:1 to Ethereum's
// baseFee+tip model and the upstream already does the right shimming.
func (c *ChainAPI) EthFeeHistory(ctx context.Context, blockCount string, newestBlock string, rewardPercentiles []float64) (any, error) {
	if c.Bridge == nil {
		return nil, errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{blockCount, newestBlock, rewardPercentiles})
	if err != nil {
		return nil, xerrors.Errorf("marshal eth_feeHistory params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_feeHistory", params)
	if err != nil {
		return nil, xerrors.Errorf("bridge eth_feeHistory: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, xerrors.Errorf("decode eth_feeHistory result: %w", err)
	}
	return out, nil
}

// EthSendRawTransaction forwards a signed raw transaction to the
// upstream VM bridge for mempool admission. The transaction's hash
// is returned verbatim from the upstream.
//
// Note: clients sign locally; Lantern never touches their private
// keys. We just relay the wire bytes.
func (c *ChainAPI) EthSendRawTransaction(ctx context.Context, signedTxHex string) (string, error) {
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{signedTxHex})
	if err != nil {
		return "", xerrors.Errorf("marshal eth_sendRawTransaction params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_sendRawTransaction", params)
	if err != nil {
		return "", xerrors.Errorf("bridge eth_sendRawTransaction: %w", err)
	}
	var txHash string
	if err := json.Unmarshal(raw, &txHash); err != nil {
		return "", xerrors.Errorf("decode eth_sendRawTransaction result: %w", err)
	}
	return txHash, nil
}

// firstCidOrEmpty returns the first CID's string or a 32-byte zero hex.
func firstCidOrEmpty(cs []cid.Cid) string {
	if len(cs) == 0 {
		return "0x0000000000000000000000000000000000000000000000000000000000000000"
	}
	return cs[0].String()
}

// zeroPad returns a string of `n` '0' characters. For logsBloom etc.
func zeroPad(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
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
