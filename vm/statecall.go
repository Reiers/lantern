// StateCall implementation.
//
// `StateCall(msg, tsk)` is "apply this message dry-run, return the
// receipt + trace". Lotus uses it for `eth_call`, gas estimation, and
// the lotus-shed VM-replay tool. Curio relies on it for:
//   - storage-market PSD verification (`lib/proofsvc`)
//   - eth_call against the EVM actor (sptool evm.go)
//   - generic dry-runs on the toolbox CLI
//
// Lantern's StateCall implementation:
//
//   1. Resolve `msg.From` to an ID address via the trusted accessor.
//   2. Compute the message CID for ExecutionTrace.MsgCid.
//   3. Run vm.Apply with the parent state root from `tsk`.
//   4. Wrap the result in an InvocResult shape compatible with Lotus.
//
// Limitations (documented in PHASE7-BLOCKERS.md):
//   - We don't execute actor logic, so ExitCode is always 0 or system
//     error. We cannot detect application-level reverts (e.g. "deal not
//     proposed").
//   - Return bytes are empty for builtin methods (we synthesise no
//     payload). Curio relies on this for eth_call; that path will need
//     a Phase 8 EVM port.

package vm

import (
	"context"
	"errors"
	"time"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
)

// CallResult mirrors the parts of api.InvocResult that callers care
// about.
type CallResult struct {
	Receipt    types.MessageReceipt
	MethodInfo *MethodInfo
	GasCost    GasCost
	Duration   time.Duration
	Error      string
}

// StateCall runs `msg` in dry-run mode against `acc`'s state.
//
// `acc` must already be bound to the desired tipset's parent state root.
// Phase 7 does not support specifying an arbitrary tipset key — callers
// must build the right accessor up-front.
func StateCall(ctx context.Context, acc *accessor.Accessor, msg *types.Message, opts ApplyOptions) (*CallResult, error) {
	if msg == nil {
		return nil, errors.New("StateCall: nil message")
	}
	t0 := time.Now()
	opts.DryRun = true
	r, err := Apply(ctx, acc, msg, opts)
	if err != nil {
		return nil, err
	}
	return &CallResult{
		Receipt:    r.Receipt,
		MethodInfo: r.MethodInfo,
		GasCost:    r.GasCost,
		Duration:   time.Since(t0),
		Error:      r.Error,
	}, nil
}
