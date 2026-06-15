package handlers

// Local FEVM write-path methods (lantern#45, Stages 1-2).
//
// These replace the bridge-only nonce + gas-estimate forwarders with
// local implementations that use Lantern's already-present primitives
// (accessor nonce, pending-mpool adjust, vm.GasEstimator). Each method
// keeps a bridge fallback: if the local path can't serve (no accessor,
// resolve failure, estimator error), it degrades to the VMBridge rather
// than failing, so a translation bug can't break a writing SP during
// rollout. When the bridge is nil AND local can't serve, they return the
// usual errBridgeUnconfigured.
//
// Stage 1: eth_getTransactionCount (nonce)   — read-only, lowest risk.
// Stage 2: eth_estimateGas                   — read-only (no broadcast).
//
// Not yet local (still bridge-only, see #45 Stages 3-6):
// sendRawTransaction, getTransactionReceipt, feeHistory, getLogs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
)

// liveActorNonce reads addr's actor nonce anchored at the LIVE chain head
// rather than the boot TrustedRoot. Nonce is collision-sensitive: a stale
// read (boot anchor can be thousands of epochs behind on a long-running
// daemon) would hand a writing SP a reused nonce and the tx would be
// rejected. We therefore only serve locally when a header store is wired
// AND has a defined live head; otherwise we report served=false so the
// caller falls back to the bridge. Returns (nonce, served, errFatal).
func (c *ChainAPI) liveActorNonce(ctx context.Context, a address.Address) (uint64, bool, error) {
	if c.Accessor == nil || c.BlockGetter == nil || c.HeaderStore == nil {
		return 0, false, nil
	}
	head := c.HeaderStore.Head()
	if head == nil {
		return 0, false, nil
	}
	liveRoot := head.ParentState()
	if !liveRoot.Defined() {
		return 0, false, nil
	}
	liveTR := &trustedroot.TrustedRoot{Epoch: head.Height(), StateRoot: liveRoot}
	acc := accessor.New(liveTR, c.BlockGetter)
	act, _, err := acc.GetActor(ctx, a)
	if err != nil {
		if isAddressNotFound(err) {
			return 0, true, nil // not-yet-deployed account -> nonce 0
		}
		return 0, false, nil // unexpected -> degrade to bridge
	}
	return act.Nonce, true, nil
}

// isAddressNotFound reports whether err is the accessor's
// "address not in Init actor" sentinel (a not-yet-deployed account,
// which has nonce 0).
func isAddressNotFound(err error) bool {
	return errors.Is(err, accessor.ErrAddressNotFound)
}

// ethCallToMessage builds the Filecoin message shape for an eth call,
// for the purpose of gas estimation only. The gas estimator
// (vm.GasEstimator.EstimateGasLimit) is heuristic/method-based, not
// execution-based, so we only need From + the right Method: a non-zero
// (contract-invoke) method makes the estimator pick the conservative
// 75M-gas ceiling, which is the safe over-estimate a writing SP wants.
// Returns (msg, false) if From/To can't be resolved.
func (c *ChainAPI) ethCallToMessage(call ethCallObject) (*types.Message, bool) {
	toRaw, err := decodeEthAddr(call.To)
	if err != nil {
		return nil, false
	}
	to, err := ethAddrToFilecoin(toRaw)
	if err != nil {
		return nil, false
	}
	msg := &types.Message{
		Version:  0,
		To:       to,
		Value:    big.Zero(),
		Method:   builtin.MethodsEVM.InvokeContract,
		GasLimit: 0,
	}
	// From must be a defined address: Message.ChainLength() serializes
	// the message and panics on an undefined From. For a heuristic
	// (method-based) gas ceiling the exact sender is irrelevant, so we
	// fall back to the burnt-funds/zero ID address when the call omits
	// `from`. Default to that, then override with the real sender if
	// present and resolvable.
	msg.From = to // safe non-undef default (the target itself serializes fine)
	if call.From != "" {
		if fromRaw, err := decodeEthAddr(call.From); err == nil {
			if from, err := ethAddrToFilecoin(fromRaw); err == nil {
				msg.From = from
			}
		}
	}
	dataHex := call.Data
	if dataHex == "" {
		dataHex = call.Input
	}
	if input, err := decodeHexData(dataHex); err == nil {
		msg.Params = input
	}
	return msg, true
}

// blockParamWantsPending reports whether an eth block param string asks
// for the pending state (nonce/gas should include unsubmitted mempool
// messages). go-ethereum sends "pending" as a string; everything else
// ("latest", "0x...", a number, or nil) is treated as on-chain.
func blockParamWantsPending(blockParam any) bool {
	s, ok := blockParam.(string)
	if !ok {
		return false
	}
	return s == "pending"
}

// localEthGetTransactionCount resolves the nonce locally. Returns
// (hexCount, served, err). served==false means "fall back to bridge".
func (c *ChainAPI) localEthGetTransactionCount(ctx context.Context, addr string, blockParam any) (string, bool, error) {
	if c.Accessor == nil {
		return "", false, nil
	}
	raw, err := decodeEthAddr(addr)
	if err != nil {
		return "", false, nil // malformed -> let bridge try
	}
	filAddr, err := ethAddrToFilecoin(raw)
	if err != nil {
		return "", false, nil
	}

	// On-chain nonce anchored at the LIVE head (not the boot root).
	onChain, served, err := c.liveActorNonce(ctx, filAddr)
	if !served {
		return "", false, err // degrade to bridge
	}
	nonce := onChain
	if blockParamWantsPending(blockParam) {
		// Pending: add this sender's locally-queued (unsubmitted) messages
		// on top of the live on-chain nonce.
		if pl, ok := c.Mpool.(MpoolPendingLister); ok && pl != nil {
			next := onChain
			for _, sm := range pl.Pending() {
				if sm.Message.From == filAddr && sm.Message.Nonce >= next {
					next = sm.Message.Nonce + 1
				}
			}
			nonce = next
		}
	}
	return "0x" + bigFromUint64(nonce).Text(16), true, nil
}

// EthGetTransactionCount returns the transaction count (nonce) for an
// Ethereum address. lantern#45 Stage 1: served locally from the accessor
// (+ pending-mpool adjust for "pending"), with bridge fallback retained
// for safety during rollout.
func (c *ChainAPI) EthGetTransactionCount(ctx context.Context, addr string, blockParam any) (string, error) {
	if out, served, err := c.localEthGetTransactionCount(ctx, addr, blockParam); served {
		return out, err
	}
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{addr, blockParam})
	if err != nil {
		return "", fmt.Errorf("marshal eth_getTransactionCount params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_getTransactionCount", params)
	if err != nil {
		return "", fmt.Errorf("bridge eth_getTransactionCount: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode eth_getTransactionCount result: %w", err)
	}
	return out, nil
}

// localEthEstimateGas estimates gas locally via the VM gas estimator.
// Returns (hexGas, served, err). served==false -> fall back to bridge.
//
// We map the eth call-object to a Filecoin message and ask the estimator
// for a gas-limit ceiling. The estimator (vm.GasEstimator) already errs
// conservative (matches Lotus' GasEstimateGasLimit overestimate), which
// is exactly what a writing SP needs: an under-estimate would make the
// real tx fail on-chain and stall the proof loop, so we never want to
// return less than the bridge would. As an extra guard we apply the same
// safety margin upstream uses.
func (c *ChainAPI) localEthEstimateGas(ctx context.Context, callObj any) (string, bool, error) {
	// Note: gas estimation here is heuristic/method-based
	// (vm.GasEstimator.EstimateGasLimit), not execution-based, so it does
	// NOT require a wired accessor or full state coverage. It needs only
	// to translate the call to a message shape and pick the conservative
	// per-method ceiling. The estimator falls back to default base-fee
	// numbers when no header store is present.
	var call ethCallObject
	b, err := json.Marshal(callObj)
	if err != nil {
		return "", false, nil
	}
	if err := json.Unmarshal(b, &call); err != nil {
		return "", false, nil
	}

	// Build the Filecoin message shape for the call. We reuse the same
	// To/From/Data marshalling the local eth_call path uses.
	msg, ok := c.ethCallToMessage(call)
	if !ok {
		return "", false, nil // can't translate -> bridge
	}

	estim, err := c.GasEstimateMessageGas(ctx, msg, nil, types.TipSetKey{})
	if err != nil {
		return "", false, nil // degrade to bridge on estimator error
	}
	if estim.GasLimit <= 0 {
		return "", false, nil
	}
	// Safety margin: never hand back a tighter estimate than upstream.
	// The estimator already overestimates; add a further 15% ceiling so
	// the proof loop has headroom (an over-estimate only costs the unused
	// gas as refund; an under-estimate fails the tx).
	gas := estim.GasLimit
	gas += gas * 15 / 100
	return "0x" + bigFromInt64(gas).Text(16), true, nil
}

// EthEstimateGas estimates gas for a call. lantern#45 Stage 2: served
// locally via vm.GasEstimator with a conservative margin, bridge fallback
// retained.
func (c *ChainAPI) EthEstimateGas(ctx context.Context, callObj any) (string, error) {
	if out, served, err := c.localEthEstimateGas(ctx, callObj); served {
		return out, err
	}
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{callObj})
	if err != nil {
		return "", fmt.Errorf("marshal eth_estimateGas params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_estimateGas", params)
	if err != nil {
		return "", fmt.Errorf("bridge eth_estimateGas: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode eth_estimateGas result: %w", err)
	}
	return out, nil
}

// bigFromUint64 / bigFromInt64 are tiny helpers to format hex quantities
// without pulling math/big into every call site.
func bigFromUint64(v uint64) big.Int { return big.NewIntUnsigned(v) }
func bigFromInt64(v int64) big.Int   { return big.NewInt(v) }
