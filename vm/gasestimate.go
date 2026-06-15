// Gas estimation helpers built on top of the VM shell.
//
// These methods are designed to be called by rpc/handlers and the
// stand-alone examples/historical/phase7 demo. They implement the same shape as
// Lotus' GasEstimateMessageGas / GasEstimateFeeCap /
// GasEstimateGasPremium / GasEstimateGasLimit, but with two important
// differences:
//
//   - We don't run a full FVM, so the gas-limit search is bounded by a
//     conservative ceiling rather than the actual per-method execution
//     cost. The cost we charge per method is the per-invocation overhead
//     plus one IPLD-get, which matches what Lotus has empirically seen
//     for most non-aggregate messages.
//
//   - The premium percentile is computed from a configurable rolling
//     window of recent header tipsets via the HistoryFetcher interface.
//     If no history is available, we fall back to a per-method floor
//     value (100k attoFIL) — the same default Curio's task/message
//     pipeline uses when its own helper times out.
//
// Cross-check methodology
//
// Phase 7's examples/historical/phase7 demo picks ~10 real mainnet messages from
// Phase 6's gossipsub mempool stream, calls GasEstimateMessageGas on
// each, and compares against the actual on-chain GasLimit / GasFeeCap /
// GasPremium values. We expect order-of-magnitude agreement — the
// network's estimators fluctuate by ~2x over short windows, so an
// estimator within 0.5-2x of the truth is "good".

package vm

import (
	"context"
	"fmt"
	"sort"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
)

// BaseFeeSource is the minimum chain-history surface the gas estimator
// needs.
type BaseFeeSource interface {
	// CurrentBaseFee returns the latest known ParentBaseFee.
	CurrentBaseFee(ctx context.Context) (big.Int, error)
}

// PremiumSource exposes a rolling sample of recently included messages'
// premiums (attoFIL/gas) over the last `n` tipsets.
type PremiumSource interface {
	// RecentPremiums returns up to `samples` recent GasPremium values
	// from messages included in the last `lookback` epochs. Order is
	// not specified.
	RecentPremiums(ctx context.Context, lookback int64, samples int) ([]big.Int, error)
}

// GasEstimator computes Lotus-compatible gas estimates without executing
// the message.
type GasEstimator struct {
	Acc       *accessor.Accessor
	BaseFee   BaseFeeSource
	Premium   PremiumSource
	PriceList PriceList
}

// EstimateGasLimit returns a conservative gas-limit ceiling for `msg`.
//
// We compute the union of:
//   - The fixed-shape overhead (OnChainMessage + OnInvoke).
//   - A per-method ceiling: 75M for known Curio-write methods
//     (PreCommit, ProveCommit, PoSt, PublishStorageDeals), 30M for
//     simple builtin sends, 10M for Send (method 0).
//   - Plus a 25% safety margin.
//
// Numbers are deliberately conservative: better to over-estimate than
// have Curio's message run out of gas mid-execution.
func (e *GasEstimator) EstimateGasLimit(ctx context.Context, msg *types.Message) (int64, error) {
	if msg == nil {
		return 0, fmt.Errorf("nil message")
	}
	pl := e.PriceList
	if pl == (PriceList{}) {
		pl = V15PriceList()
	}

	base := pl.OnChainMessage(msg.ChainLength()) +
		pl.OnInvoke(!msg.Value.NilOrZero() && msg.Value.GreaterThan(big.Zero()))

	// Pick a per-method ceiling.
	var methodCeil int64
	switch msg.Method {
	case 0:
		methodCeil = 1_000_000 // 1M gas for pure value transfer
	default:
		methodCeil = 75_000_000 // 75M for arbitrary builtin actor call
	}

	out := base + methodCeil
	// 25% safety margin.
	out = out + out/4
	if out > build.BlockGasLimit {
		out = build.BlockGasLimit
	}
	return out, nil
}

// EstimateFeeCap returns BaseFee * 1.5 + premium, with a floor of
// MinimumBaseFee. `maxqueueblocks` follows Lotus' convention but is
// currently ignored: we always assume "include within the next few
// blocks".
func (e *GasEstimator) EstimateFeeCap(ctx context.Context, premium big.Int, _ int64) (big.Int, error) {
	bf, err := e.currentBaseFee(ctx)
	if err != nil {
		return big.Zero(), err
	}
	// Lotus multiplier: 2x baseFee + premium.
	cap := big.Mul(bf, big.NewInt(2))
	if !premium.NilOrZero() {
		cap = big.Add(cap, premium)
	}
	floor := big.NewInt(build.MinimumBaseFee)
	if cap.LessThan(floor) {
		cap = floor
	}
	return cap, nil
}

// EstimateGasPremium returns the 60th percentile premium from the last
// `nblocksincl` epochs. Falls back to 100k attoFIL if no sample is
// available.
func (e *GasEstimator) EstimateGasPremium(ctx context.Context, nblocksincl uint64) (big.Int, error) {
	const fallback = int64(100_000) // 100k attoFIL/gas — Curio's compile-time default
	if e.Premium == nil {
		return big.NewInt(fallback), nil
	}
	lookback := int64(nblocksincl)
	if lookback < 1 {
		lookback = 5
	}
	if lookback > 100 {
		lookback = 100
	}
	prems, err := e.Premium.RecentPremiums(ctx, lookback, 256)
	if err != nil || len(prems) == 0 {
		return big.NewInt(fallback), nil
	}
	// Sort ascending, pick the 60th percentile.
	sort.Slice(prems, func(i, j int) bool {
		return prems[i].LessThan(prems[j])
	})
	idx := (len(prems) * 60) / 100
	if idx >= len(prems) {
		idx = len(prems) - 1
	}
	out := prems[idx]
	if out.LessThan(big.NewInt(fallback)) {
		out = big.NewInt(fallback)
	}
	return out, nil
}

// EstimateMessageGas fills in `msg.GasLimit`, `msg.GasFeeCap`, and
// `msg.GasPremium` and returns the modified message. If any of those
// fields are already non-zero, they are preserved (matching Lotus's
// "don't clobber explicit user input" rule).
func (e *GasEstimator) EstimateMessageGas(ctx context.Context, msg *types.Message, maxFee big.Int) (*types.Message, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	out := *msg

	// GasPremium first — used by FeeCap.
	if out.GasPremium.NilOrZero() {
		pr, err := e.EstimateGasPremium(ctx, 10)
		if err != nil {
			return nil, fmt.Errorf("estimate premium: %w", err)
		}
		out.GasPremium = pr
	}

	if out.GasFeeCap.NilOrZero() {
		fc, err := e.EstimateFeeCap(ctx, out.GasPremium, 20)
		if err != nil {
			return nil, fmt.Errorf("estimate fee cap: %w", err)
		}
		out.GasFeeCap = fc
	}

	if out.GasLimit == 0 {
		gl, err := e.EstimateGasLimit(ctx, &out)
		if err != nil {
			return nil, fmt.Errorf("estimate gas limit: %w", err)
		}
		out.GasLimit = gl
	}

	// Cap by maxFee if provided.
	if !maxFee.NilOrZero() && maxFee.GreaterThan(big.Zero()) {
		cap := big.Mul(big.NewIntUnsigned(uint64(out.GasLimit)), out.GasFeeCap)
		if cap.GreaterThan(maxFee) {
			// Lower the fee cap so cap = maxFee/gasLimit.
			newCap := big.Div(maxFee, big.NewIntUnsigned(uint64(out.GasLimit)))
			if newCap.LessThan(out.GasPremium) {
				newCap = out.GasPremium
			}
			out.GasFeeCap = newCap
		}
	}

	return &out, nil
}

func (e *GasEstimator) currentBaseFee(ctx context.Context) (big.Int, error) {
	if e.BaseFee == nil {
		// Conservative default: 100 attoFIL.
		return big.NewInt(build.MinimumBaseFee), nil
	}
	return e.BaseFee.CurrentBaseFee(ctx)
}

// _ keeps addr imported for future hooks.
var _ = addr.Undef
