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
	"strings"
	"sync/atomic"

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
	case "devnet":
		// Devnet genesis is generated per-boot; read the CID from
		// the runtime config seeded by `lantern devnet-init`.
		// Fixes lantern#123.
		cfg := build.GetDevnetConfig()
		if cfg == nil || cfg.GenesisCID == "" {
			return nil, ErrNotImpl("ChainGetGenesis",
				"devnet config not loaded; re-run `lantern devnet-init --lotus-rpc <URL>`")
		}
		gc, err := cid.Parse(cfg.GenesisCID)
		if err != nil {
			return nil, fmt.Errorf("parse devnet genesis cid %q: %w", cfg.GenesisCID, err)
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
//
//	mainnet     → 314    (0x13a)
//	calibration → 314159 (0x4cb2f)
//
// These are the published EIP-155 chain IDs; viem and synapse-sdk use
// them to scope signatures + reject cross-chain replays.
func (c *ChainAPI) EthChainId(_ context.Context) (string, error) {
	switch c.NetworkName {
	case "calibration":
		return "0x4cb2f", nil // 314159
	case "devnet":
		// Devnet chainId is per-config (curio-fork docker devnet
		// defaults to 31415926, custom setups may differ). Read
		// from the runtime config seeded by `lantern devnet-init`.
		// Missing/zero chainId means an older devnet-config predates
		// lantern#123; ask the operator to re-init.
		cfg := build.GetDevnetConfig()
		if cfg == nil || cfg.EthChainID == 0 {
			return "", fmt.Errorf("devnet ethChainID missing from config; re-run `lantern devnet-init --force`")
		}
		return fmt.Sprintf("0x%x", cfg.EthChainID), nil
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
// Now wired to the live head base fee (via EthBaseFee) so a tx builder
// that prices off gasPrice during a base-fee spike doesn't underprice and
// stall. Falls back to MinimumBaseFee only when no live head is available.
func (c *ChainAPI) EthGasPrice(ctx context.Context) (string, error) {
	if bf, err := c.EthBaseFee(ctx); err == nil && bf != "" && bf != "0x0" {
		return bf, nil
	}
	return fmt.Sprintf("0x%x", build.MinimumBaseFee), nil
}

// EthBaseFee returns the base fee for the next block as a hex-quantity string
// (lotus #13615, eth_baseFee). On Filecoin the base fee is consensus-determined
// per tipset, so the head tipset's ParentBaseFee is the correct next-block base
// fee. Lantern serves this from its own locally-validated head (no bridge round
// trip); it falls back to the VMBridge, then to MinimumBaseFee, so the method
// always returns a usable quantity for tx builders.
func (c *ChainAPI) EthBaseFee(ctx context.Context) (string, error) {
	if c.HeaderStore != nil {
		if ts, err := c.HeaderStore.GetTipSetByHeight(abi.ChainEpoch(c.HeaderStore.HeadEpoch())); err == nil && ts != nil {
			if blocks := ts.Blocks(); len(blocks) > 0 && blocks[0].ParentBaseFee.Int != nil {
				return "0x" + blocks[0].ParentBaseFee.Int.Text(16), nil
			}
		}
	}
	if c.Bridge != nil {
		if raw, err := c.Bridge.RawJSONRPC(ctx, "eth_baseFee", []byte("[]")); err == nil {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s, nil
			}
		}
	}
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

	// Resolve against the LIVE head's state root, not the boot TrustedRoot.
	// The boot anchor is frozen at daemon-start state, so any account
	// created after boot (e.g. a freshly faucet-funded address) is invisible
	// to it and reads as 0 even though it has a balance on chain. Fall back
	// to the boot accessor only when no live head is available.
	acc := c.Accessor
	if live, ok := c.liveAccessor(); ok {
		acc = live
	}
	actor, _, err := acc.GetActor(ctx, filAddr)
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
func (c *ChainAPI) EthGetBlockByNumber(ctx context.Context, blockParam string, fullTx bool) (any, error) {
	if c.HeaderStore == nil {
		// Embedded mode without header store mounted (pkg/daemon's
		// extraction is incomplete — see TODO(daemon-extraction)).
		// Forward to VMBridge if available so curio-core's tx-builder
		// path (go-ethereum HeaderByNumber for baseFee) keeps working.
		if c.Bridge != nil {
			params, err := json.Marshal([]any{blockParam, fullTx})
			if err != nil {
				return nil, xerrors.Errorf("marshal eth_getBlockByNumber params: %w", err)
			}
			raw, err := c.Bridge.RawJSONRPC(ctx, "eth_getBlockByNumber", params)
			if err != nil {
				return nil, xerrors.Errorf("bridge eth_getBlockByNumber: %w", err)
			}
			var out any
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, xerrors.Errorf("decode eth_getBlockByNumber result: %w", err)
			}
			return out, nil
		}
		return nil, xerrors.New("header store not configured (and no VM bridge available for fallback)")
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
	return tipsetToEthBlock(ts), nil
}

// tipsetToEthBlock converts a Lantern TipSet to the ETH-shaped block
// dict used by eth_getBlockByNumber + eth_subscribe(newHeads). Extracted
// from EthGetBlockByNumber so both call sites share the exact same
// reshape.
//
// All address + hash fields go through the ETH-shape helpers so strict
// ETH parsers (e.g. go-ethereum types.Header) can JSON-unmarshal the
// result. Filecoin CIDs become 32-byte hex hashes (via EthHashFromCid).
// The miner f0-actor becomes 0xff||be64(id). The original CIDs are
// surfaced in filecoinTipsetCids for callers that know about Filecoin.
//
// baseFeePerGas is the parent's base fee (same as the current's on
// Filecoin since base fee is consensus-determined per tipset).
func tipsetToEthBlock(ts *types.TipSet) map[string]any {
	blocks := ts.Blocks()
	if len(blocks) == 0 {
		return nil
	}
	b := blocks[0]

	cidStrs := make([]string, len(blocks))
	for i, blk := range blocks {
		cidStrs[i] = blk.Cid().String()
	}

	baseFeeHex := "0x0"
	if b.ParentBaseFee.Int != nil {
		baseFeeHex = "0x" + b.ParentBaseFee.Int.Text(16)
	}
	return map[string]any{
		"number":             fmt.Sprintf("0x%x", int64(b.Height)),
		"hash":               EthHashFromCid(b.Cid()),
		"parentHash":         firstCidHash(b.Parents),
		"nonce":              "0x0000000000000000",
		"sha3Uncles":         "0x0000000000000000000000000000000000000000000000000000000000000000",
		"logsBloom":          "0x" + zeroPad(512),
		"transactionsRoot":   "0x0000000000000000000000000000000000000000000000000000000000000000",
		"stateRoot":          EthHashFromCid(b.ParentStateRoot),
		"receiptsRoot":       EthHashFromCid(b.ParentMessageReceipts),
		"miner":              EthAddressFromFilecoinIDActor(b.Miner),
		"difficulty":         "0x0",
		"totalDifficulty":    "0x0",
		"extraData":          "0x",
		"size":               "0x0",
		"gasLimit":           fmt.Sprintf("0x%x", build.BlockGasLimit),
		"gasUsed":            "0x0",
		"baseFeePerGas":      baseFeeHex,
		"timestamp":          fmt.Sprintf("0x%x", b.Timestamp),
		"transactions":       []string{},
		"uncles":             []string{},
		"filecoinTipsetCids": cidStrs,
	}
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
	// Devnet bridge-first (lantern#123 finding 7): on a single-node docker
	// devnet the local hamt walk always burns its full retry budget (no
	// bitswap peers to fetch cold storage-trie blocks from), then falls
	// through to the bridge anyway. The devnet lotus IS the source of
	// truth for the devnet chain, so short-circuiting straight to it is
	// both faster and semantically identical. Guarded on Bridge != nil so
	// we still attempt local if the operator dropped the auto-wired bridge.
	if c.NetworkName == "devnet" && c.Bridge != nil {
		return c.bridgeEthCall(ctx, callObj, blockParam)
	}
	// Local FEVM execution first (lantern#43 Part B): run the call against
	// our own verified state tree with the pure-Go EVM. A clean local
	// result (including a definitive revert) is returned directly; a local
	// miss (unconfigured, unsupported opcode, malformed input) falls
	// through to the VMBridge so we degrade to the upstream rather than
	// fail. Disable with LocalFEVMDisabled to force bridge-only.
	if !c.LocalFEVMDisabled {
		var call ethCallObject
		if b, err := json.Marshal(callObj); err == nil {
			_ = json.Unmarshal(b, &call)
		}
		if res, served, err := c.localEthCall(ctx, call); served {
			// served==true is a definitive answer (success or revert).
			atomic.AddUint64(&c.localEthCallServed, 1)
			if err != nil {
				log.Debugw("eth_call: served locally (revert)", "to", call.To)
				return "", err // revertError -> RPC maps to code 3
			}
			log.Debugw("eth_call: served locally", "to", call.To)
			return res, nil
		}
		atomic.AddUint64(&c.localEthCallBridgeFallback, 1)
		log.Debugw("eth_call: local miss, falling back to bridge", "to", call.To)
		// Adaptive warming (lantern#44): feed the missed contract back to
		// the prefetcher so it's warm next head advance. Non-blocking;
		// curio-core's hook dedups + caps internally.
		if c.OnLocalMiss != nil && call.To != "" {
			c.OnLocalMiss(strings.ToLower(call.To))
		}
	}

	return c.bridgeEthCall(ctx, callObj, blockParam)
}

// bridgeEthCall forwards eth_call to the auto-wired VM bridge. Extracted
// so the devnet bridge-first path and the mainnet/calibration local-miss
// fallback share the same code.
func (c *ChainAPI) bridgeEthCall(ctx context.Context, callObj any, blockParam any) (string, error) {
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
// EthEstimateGas + EthGetTransactionCount moved to extra_writepath.go
// (lantern#45 Stages 1-2: local-first with bridge fallback).

// EthGetTransactionReceipt returns the receipt for a previously-broadcast
// transaction. Forwarded to the upstream VM bridge: receipts require
// indexed message lookups + execution result reconstruction which Lantern
// doesn't run today. Returns nil when the tx isn't found (caller will
// retry; the standard go-ethereum receipt poll loop expects this shape).
// EthGetTransactionReceipt moved to extra_writepath_tx.go (lantern#45
// Stage 5: local resolve for txs we originated, bridge fallback otherwise).

// EthFeeHistory returns historical gas fee data used by tx builders to
// suggest EIP-1559 priority fees. Local-first (lantern#76): Filecoin's
// base fee is consensus-determined per tipset and lives in each header's
// ParentBaseFee, which Lantern already tracks in the header store. So we
// can serve the base-fee history straight from local headers with NO
// bridge - closing the last read method on a writing SP's path that
// hard-failed bridge-off. Priority fees (rewards) don't have a native
// Filecoin equivalent; we report a zero-premium reward vector (clients
// price premium off eth_gasPrice / eth_maxPriorityFeePerGas separately),
// which matches how a min-base-fee chain behaves. Bridge remains the
// fallback for ranges outside the local header window.
func (c *ChainAPI) EthFeeHistory(ctx context.Context, blockCount string, newestBlock string, rewardPercentiles []float64) (any, error) {
	if out, served, err := c.localEthFeeHistory(ctx, blockCount, newestBlock, rewardPercentiles); served {
		return out, err
	}
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

// parseEthUintDefault parses an Ethereum QUANTITY (0x-hex) or a plain
// decimal string into a uint64, returning def on any parse failure or
// empty input. Used for eth_feeHistory's blockCount / newestBlock which
// clients send as either form.
func parseEthUintDefault(s string, def uint64) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	base := 10
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
		base = 16
	}
	v, ok := new(stdbig.Int).SetString(s, base)
	if !ok || v.Sign() < 0 || !v.IsUint64() {
		return def
	}
	return v.Uint64()
}

// localEthFeeHistory serves eth_feeHistory from the local header store's
// per-tipset ParentBaseFee. Returns (result, served, err); served==false
// means fall back to the bridge (no header store, or the requested range
// lies outside what we have locally).
//
// Result shape matches Ethereum's eth_feeHistory: oldestBlock,
// baseFeePerGas (len n+1: one per block plus the next-block projection),
// gasUsedRatio (len n), and reward (len n, each an array sized to
// rewardPercentiles) when percentiles were requested. On Filecoin the
// premium market has no on-chain per-percentile history, so reward
// entries are reported as zero - callers derive priority fee from
// eth_gasPrice / gas estimation, not from this vector.
func (c *ChainAPI) localEthFeeHistory(ctx context.Context, blockCount string, newestBlock string, rewardPercentiles []float64) (any, bool, error) {
	if c.HeaderStore == nil {
		return nil, false, nil
	}

	// Parse blockCount (hex quantity or small decimal). Clamp to a sane
	// window so a huge count can't walk the whole store.
	n := parseEthUintDefault(blockCount, 0)
	if n == 0 {
		return nil, false, nil
	}
	const maxFeeHistoryBlocks = uint64(1024)
	if n > maxFeeHistoryBlocks {
		n = maxFeeHistoryBlocks
	}

	// Resolve newestBlock: "latest"/"pending"/"" => head; else a hex/dec height.
	head := abi.ChainEpoch(c.HeaderStore.HeadEpoch())
	newest := head
	switch nb := strings.ToLower(strings.TrimSpace(newestBlock)); nb {
	case "", "latest", "pending", "safe", "finalized":
		newest = head
	default:
		newest = abi.ChainEpoch(parseEthUintDefault(newestBlock, uint64(head)))
	}
	if newest > head {
		newest = head
	}
	if newest < 0 {
		return nil, false, nil
	}

	oldest := newest - abi.ChainEpoch(n) + 1
	if oldest < 0 {
		oldest = 0
	}

	// Walk oldest..newest from local headers, collecting ParentBaseFee.
	// A single missing tipset in the window => fall back to the bridge
	// (don't return a hole-y history that misleads a fee estimator).
	baseFees := make([]string, 0, int(newest-oldest)+2)
	gasUsedRatio := make([]float64, 0, int(newest-oldest)+1)
	var lastBaseFee string
	for ep := oldest; ep <= newest; ep++ {
		ts, err := c.HeaderStore.GetTipSetByHeight(ep)
		if err != nil || ts == nil {
			// Null round or gap. Null rounds are legitimate; carry the
			// prior base fee forward. A true gap (err) below the window
			// start means we don't have local coverage -> bridge.
			if lastBaseFee == "" {
				return nil, false, nil
			}
			baseFees = append(baseFees, lastBaseFee)
			gasUsedRatio = append(gasUsedRatio, 0)
			continue
		}
		blocks := ts.Blocks()
		if len(blocks) == 0 || blocks[0].ParentBaseFee.Int == nil {
			if lastBaseFee == "" {
				return nil, false, nil
			}
			baseFees = append(baseFees, lastBaseFee)
			gasUsedRatio = append(gasUsedRatio, 0)
			continue
		}
		bf := "0x" + blocks[0].ParentBaseFee.Int.Text(16)
		baseFees = append(baseFees, bf)
		lastBaseFee = bf
		// We don't track per-block gas used locally without message
		// re-execution; report 0.0 (a min-base-fee chain sits near-empty).
		gasUsedRatio = append(gasUsedRatio, 0)
	}
	if len(baseFees) == 0 {
		return nil, false, nil
	}

	// eth_feeHistory's baseFeePerGas is len n+1: append the next-block
	// projection (Filecoin: the head tipset's ParentBaseFee is already the
	// next-block base fee, so reuse the last observed value).
	baseFees = append(baseFees, lastBaseFee)

	out := map[string]any{
		"oldestBlock":   "0x" + abiEpochHex(oldest), // abiEpochHex returns minimal hex w/o 0x ("0" for zero)
		"baseFeePerGas": baseFees,
		"gasUsedRatio":  gasUsedRatio,
	}
	// reward is only present when percentiles were requested; each entry is
	// a per-percentile array. No native premium history on Filecoin => zero.
	if len(rewardPercentiles) > 0 {
		reward := make([][]string, len(gasUsedRatio))
		for i := range reward {
			row := make([]string, len(rewardPercentiles))
			for j := range row {
				row[j] = "0x0"
			}
			reward[i] = row
		}
		out["reward"] = reward
	}
	return out, true, nil
}

// EthSendRawTransaction forwards a signed raw transaction to the
// upstream VM bridge for mempool admission. The transaction's hash
// is returned verbatim from the upstream.
//
// Note: clients sign locally; Lantern never touches their private
// keys. We just relay the wire bytes.
// EthSendRawTransaction moved to extra_writepath_tx.go (lantern#45 Stage
// 4: local decode + MpoolPush, bridge fallback only on decode failure).

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

// ─── lite-node eth tx + state methods, all VMBridge-forwarded ────────
// Lantern's local state tree doesn't include FEVM execution + indexed
// message lookups today (see lantern#3 and the storage backfill work).
// Forward these to the configured upstream so curio-core's tx-builder
// path + retrieval-side clients keep working. Migrate to local state
// once the backfill catches up.
//
// All errors propagated verbatim; clients see the same status they
// would talking to the upstream directly.

// EthGetTransactionByHash returns a previously-broadcast transaction
// by its hash, or null if not found.
//
// Local-first for txs we originated: the curio-core #81 eth watcher polls
// this to learn a sent tx's pending/mined status. We resolve it from the
// send-time index + StateSearchMsg (same path as the receipt), so the
// write-confirm loop runs with the bridge disabled. Falls back to the
// bridge for hashes we didn't originate (external lookups during rollout).
func (c *ChainAPI) EthGetTransactionByHash(ctx context.Context, txHash string) (any, error) {
	if out, served, err := c.localEthGetTransactionByHash(ctx, txHash); served {
		return out, err
	}
	return c.forwardEth(ctx, "eth_getTransactionByHash", []any{txHash})
}

// EthGetTransactionByBlockNumberAndIndex returns a transaction in a
// specific block by position.
func (c *ChainAPI) EthGetTransactionByBlockNumberAndIndex(ctx context.Context, blockParam string, index string) (any, error) {
	return c.forwardEth(ctx, "eth_getTransactionByBlockNumberAndIndex", []any{blockParam, index})
}

// EthGetCode returns the deployed contract bytecode at an address.
// Used by every ethclient.CodeAt call (e.g. wallet detection, contract
// presence checks before calling).
//
// Local-first (lantern#74): resolve the bytecode from local state so a
// bridge-off node (stock Curio / maxboom) serves contract-presence checks
// without a VMBridge. Falls back to the bridge only when the address
// can't be resolved locally (cold blocks during rollout).
func (c *ChainAPI) EthGetCode(ctx context.Context, addr string, blockParam any) (string, error) {
	// Devnet bridge-first (lantern#123 finding 7): see EthCall.
	if c.NetworkName == "devnet" && c.Bridge != nil {
		raw, err := c.forwardEth(ctx, "eth_getCode", []any{addr, blockParam})
		if err != nil {
			return "", err
		}
		s, ok := raw.(string)
		if !ok {
			return "", xerrors.Errorf("eth_getCode: unexpected result type %T", raw)
		}
		return s, nil
	}
	if out, served, err := c.localEthGetCode(ctx, addr); served {
		return out, err
	}
	raw, err := c.forwardEth(ctx, "eth_getCode", []any{addr, blockParam})
	if err != nil {
		return "", err
	}
	s, ok := raw.(string)
	if !ok {
		return "", xerrors.Errorf("eth_getCode: unexpected result type %T", raw)
	}
	return s, nil
}

// EthGetStorageAt returns the raw 32-byte storage slot value at the
// given key on the given contract.
func (c *ChainAPI) EthGetStorageAt(ctx context.Context, addr string, key string, blockParam any) (string, error) {
	// Devnet bridge-first (lantern#123 finding 7): see EthCall.
	if c.NetworkName == "devnet" && c.Bridge != nil {
		raw, err := c.forwardEth(ctx, "eth_getStorageAt", []any{addr, key, blockParam})
		if err != nil {
			return "", err
		}
		s, ok := raw.(string)
		if !ok {
			return "", xerrors.Errorf("eth_getStorageAt: unexpected result type %T", raw)
		}
		return s, nil
	}
	// Local-first (lantern#75): read the storage slot from local state so a
	// bridge-off node serves it without a VMBridge; fall back otherwise.
	if out, served, err := c.localEthGetStorageAt(ctx, addr, key); served {
		return out, err
	}
	raw, err := c.forwardEth(ctx, "eth_getStorageAt", []any{addr, key, blockParam})
	if err != nil {
		return "", err
	}
	s, ok := raw.(string)
	if !ok {
		return "", xerrors.Errorf("eth_getStorageAt: unexpected result type %T", raw)
	}
	return s, nil
}

// EthGetBlockByHash returns the ETH-shaped block for the given hash.
// Mirrors EthGetBlockByNumber's behaviour: prefer local header store
// if present, otherwise forward to the bridge.
func (c *ChainAPI) EthGetBlockByHash(ctx context.Context, blockHash string, fullTx bool) (any, error) {
	// Local-first (lantern#75): resolve recent blocks from the header store
	// (scanned by hash over a bounded window) so a bridge-off node serves
	// receipt/tx-context block lookups without a VMBridge. Falls back for
	// hashes older than the window or when no header store is mounted.
	if out, served, err := c.localEthGetBlockByHash(ctx, blockHash, fullTx); served {
		return out, err
	}
	return c.forwardEth(ctx, "eth_getBlockByHash", []any{blockHash, fullTx})
}

// EthGetLogs returns logs matching the provided filter. Heavily used
// by client-side payment rail watchers (FilecoinPay rail event
// indexing). Lantern doesn't run an FEVM log index of its own; the
// upstream's index is the source of truth.
func (c *ChainAPI) EthGetLogs(ctx context.Context, filter any) (any, error) {
	// Local-first (lantern#73): decode logs from per-receipt event AMTs so
	// a bridge-off node (stock Curio / maxboom) serves PDP settlement +
	// FilecoinPay rail watchers without a VMBridge. Falls back to the
	// bridge for ranges/blocks outside the local window or on decode gaps.
	if out, served, err := c.localEthGetLogs(ctx, filter); served {
		return out, err
	}
	return c.forwardEth(ctx, "eth_getLogs", []any{filter})
}

// forwardEth is the common shape: marshal params, post to bridge,
// return the result blob untouched. Callers may type-assert if they
// need a specific shape.
func (c *ChainAPI) forwardEth(ctx context.Context, method string, params []any) (any, error) {
	if c.Bridge == nil {
		return nil, errBridgeUnconfigured
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, xerrors.Errorf("marshal %s params: %w", method, err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, method, payload)
	if err != nil {
		return nil, xerrors.Errorf("bridge %s: %w", method, err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, xerrors.Errorf("decode %s result: %w", method, err)
	}
	return out, nil
}
