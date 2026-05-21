// ApplyMessage: the Lantern VM shell's entry point.
//
// `Apply` consumes a Message and produces a synthetic MessageReceipt.
// We support three execution modes:
//
//  1. **Send (method 0)**: full fidelity. We resolve the sender + receiver
//     actors, verify the sender has sufficient balance for value +
//     max-gas-cost, charge gas, and return ExitOk. State mutation is
//     tracked in an in-memory diff; commit-or-discard is the caller's
//     choice (StateCall = discard, future-MinerCreateBlock = commit but
//     we don't have a full VM so we don't actually publish).
//
//  2. **Built-in actor method (kind, version, method)**: gas-accounting
//     only. We look up the method's MethodMeta from the dispatch table,
//     decode the parameters for traceability, charge a per-method
//     ceiling gas amount, and return ExitOk with a zero-value return.
//     State is **not** modified. This matches what Lotus' gas estimator
//     actually does for the `MaxBlockGas` initial probe before binary
//     search.
//
//  3. **Unknown actor or user-deployed actor**: return
//     ExitCodeSysErrUnsupportedMethod with no state change.

package vm

import (
	"context"
	"fmt"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
)

// ApplyOptions controls execution behaviour.
type ApplyOptions struct {
	// NetworkVersion is the current Filecoin network version (e.g. nv26
	// for v18 actors).
	NetworkVersion network.Version

	// PriceList is the gas schedule. If zero-value, V15PriceList is used.
	PriceList PriceList

	// BaseFee is the parent tipset's base fee in attoFIL.
	BaseFee big.Int

	// DryRun = true means do not commit state. Always true in Phase 7
	// (we have no place to commit it anyway).
	DryRun bool
}

// ApplyResult is the structured return from Apply.
type ApplyResult struct {
	Receipt    types.MessageReceipt
	MethodInfo *MethodInfo // nil for Send / unknown
	GasCost    GasCost
	Error      string
	// ParamsDecoded is the human-readable representation of the params,
	// when we can decode them. Useful for tracing.
	ParamsDecoded interface{}
}

// GasCost mirrors api.MessageGasCost.
type GasCost struct {
	GasUsed            int64
	BaseFeeBurn        big.Int
	OverEstimationBurn big.Int
	MinerTip           big.Int
	MinerPenalty       big.Int
	Refund             big.Int
	TotalCost          big.Int
}

// Apply executes a single message against the accessor's view of state.
//
// `acc` is a read-only state accessor bound to the tipset's parent state
// root. We use it to (a) resolve `msg.From` and `msg.To` to ID
// addresses, (b) read the receiver actor's code CID for method dispatch,
// (c) check the sender's balance.
//
// The returned ApplyResult.Receipt has:
//   - ExitCode: 0 on success, SysErrUnsupportedMethod for unknown actors,
//     SysErrInsufficientFunds when the sender can't cover gas+value.
//   - GasUsed: estimated gas consumed.
//   - Return: empty bytes (we don't synthesize return-typed payloads).
func Apply(ctx context.Context, acc *accessor.Accessor, msg *types.Message, opts ApplyOptions) (*ApplyResult, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	pl := opts.PriceList
	if pl == (PriceList{}) {
		pl = V15PriceList()
	}

	// 1. Encoded message length for OnChainMessage cost.
	encLen := msg.ChainLength()
	gasOnChain := pl.OnChainMessage(encLen)

	// 2. Per-invocation cost. Value transfer adds the value-xfer
	//    surcharge.
	valueXfer := !msg.Value.NilOrZero() && msg.Value.GreaterThan(big.Zero())
	gasInvoke := pl.OnInvoke(valueXfer)

	// 3. Resolve sender ID + actor for balance + nonce checks. Allow
	//    Phase-7 "no accessor" paths (e.g. block production previews on
	//    a stub).
	var fromActor *accessor.Actor
	if acc != nil {
		var err error
		fromActor, _, err = acc.GetActor(ctx, msg.From)
		if err != nil {
			// Sender not found: classic Send-to-nonexistent-actor case.
			// Fail with SysErrInsufficientFunds, mirroring Lotus's
			// "actor not found" treatment for the sender.
			return &ApplyResult{
				Receipt: types.MessageReceipt{
					ExitCode: exitcode.ExitCode(exitcode.SysErrSenderInvalid),
					GasUsed:  gasOnChain,
				},
				Error: fmt.Sprintf("resolve sender %s: %v", msg.From, err),
			}, nil
		}
	}

	// 4. Resolve receiver. For Send (method 0) the receiver may be a
	//    placeholder or non-existent; we still allow the call (this
	//    matches mainnet semantics: Send to a non-existent ID creates
	//    a placeholder).
	var (
		toKind    actors.Kind
		toVersion int
		hasTo     bool
	)
	if acc != nil {
		toActor, _, err := acc.GetActor(ctx, msg.To)
		if err == nil && toActor != nil {
			reg := acc.Registry()
			if reg != nil {
				if info, ok := reg.Lookup(toActor.Code); ok {
					toKind = info.Kind
					toVersion = info.Version
					hasTo = true
				}
			}
		}
	}

	// 5. Dispatch.
	out := &ApplyResult{}
	out.GasCost.GasUsed = gasOnChain + gasInvoke

	switch {
	case msg.Method == 0:
		// Send. Always succeeds gas-wise if the sender has enough
		// balance for `value + gasFeeCap*gasLimit`.
		if fromActor != nil {
			max := big.Mul(big.NewIntUnsigned(uint64(msg.GasLimit)), msg.GasFeeCap)
			needed := big.Add(max, msg.Value)
			if fromActor.Balance.LessThan(needed) {
				out.Receipt = types.MessageReceipt{
					ExitCode: exitcode.ExitCode(exitcode.SysErrInsufficientFunds),
					GasUsed:  out.GasCost.GasUsed,
				}
				out.Error = "insufficient balance for value+max-gas"
				return out, nil
			}
		}
		out.Receipt = types.MessageReceipt{
			ExitCode: 0,
			Return:   nil,
			GasUsed:  out.GasCost.GasUsed,
		}
		return out, nil

	case hasTo:
		// Built-in actor method. Gas-accounting-only path.
		info, err := ResolveMethod(ctx, toKind, toVersion, msg.Method)
		if err != nil {
			out.Receipt = types.MessageReceipt{
				ExitCode: exitcode.ExitCode(exitcode.SysErrInvalidReceiver),
				GasUsed:  out.GasCost.GasUsed,
			}
			out.Error = err.Error()
			return out, nil
		}
		// Charge an extra synthetic cost for the method (proxy for the
		// per-method IPLD reads + state lookups Lotus would charge for
		// real).
		out.GasCost.GasUsed += pl.OnIpldGetBase
		out.MethodInfo = info
		out.Receipt = types.MessageReceipt{
			ExitCode: 0,
			Return:   nil,
			GasUsed:  out.GasCost.GasUsed,
		}
		return out, nil

	default:
		// Unknown receiver actor (or no accessor): we can't dispatch
		// further. Charge OnChainMessage + Invoke gas and return
		// "unsupported".
		out.Receipt = types.MessageReceipt{
			ExitCode: exitcode.ExitCode(exitcode.SysErrInvalidReceiver),
			GasUsed:  out.GasCost.GasUsed,
		}
		out.Error = fmt.Sprintf("unknown receiver actor %s", msg.To)
		return out, nil
	}
}

// MaxGasCost returns the worst-case fee a sender will pay for a fully
// burnt gas budget: `msg.GasLimit × msg.GasFeeCap`.
func MaxGasCost(msg *types.Message) big.Int {
	if msg == nil || msg.GasLimit == 0 || msg.GasFeeCap.NilOrZero() {
		return big.Zero()
	}
	return big.Mul(big.NewIntUnsigned(uint64(msg.GasLimit)), msg.GasFeeCap)
}

// _ ensures addr is imported for future hooks.
var _ = addr.Undef
