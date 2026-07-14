// Package retention resolves how far below head a Full node MAY prune
// cached IPLD state (issue #92, part of the #87 epic).
//
// The Full-node design (see docs/design/) keeps a small hot working set
// of state + messages + receipts near the head and drops anything older
// than a finality-driven cutoff. Anything pruned stays CID-verifiably
// re-fetchable via bitswap/gateway, so pruning is a cache policy, not
// data loss.
//
// The cutoff comes from:
//
//	retentionEpoch = min(EC-finalized, F3-finalized) - safetyFinalities * ChainFinality
//
// where EC-finalized comes from the FRC-0089 calculator (#96), F3-finalized
// comes from the latest F3 cert, and safetyFinalities is the number of
// ChainFinality-sized windows of extra safety we keep below finality.
// A safetyFinalities of 0 means "prune everything below finality"; a value
// of 2 means "keep an extra 2*900=1800 epochs of history for the archive
// role" (mainnet). Full nodes default to safetyFinalities=1.
//
// The resolver reports a THRESHOLD, not a schedule: callers decide when
// to run the actual drop (a periodic tick, an idle sweep, a manual op).
// The resolver never returns a threshold above finality: below-finality
// pruning is only ever allowed for the "static-fallback" flavor of EC
// finality (head - ChainFinality); F3 and FRC-0089 are trusted to move
// forward safely on a healthy chain.
package retention

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/selector"
	"github.com/Reiers/lantern/chain/types"
)

// DefaultSafetyFinalities is the extra safety margin applied to the
// finality-derived retention epoch. 1 => keep an extra ChainFinality
// (~900 epochs, ~7.5h at 30s blocks) of history below finality. A Full
// node can lower this to 0 to prune more aggressively, or raise it for
// an archive role.
const DefaultSafetyFinalities = 1

// Depth reports how far below head a Full node MAY prune. Returns:
//   - retentionEpoch: the SHALLOWEST epoch that must be retained. Anything
//     STRICTLY below this epoch is a candidate for pruning.
//   - safetyMarginEpochs: how many epochs below finality this policy keeps
//     (safetyFinalities * ChainFinality).
//
// When the FRC-0089 calculator has not yet reached threshold AND F3 is
// unavailable, retentionEpoch falls back to (head - ChainFinality -
// safetyMargin). This preserves the static-fallback semantics of
// finalized in chain/selector.
//
// The returned retentionEpoch is always >= 0.
func Depth(
	ctx context.Context,
	r selector.Resolver,
	safetyFinalities int,
) (retentionEpoch, safetyMarginEpochs abi.ChainEpoch, err error) {
	if r == nil {
		return 0, 0, errors.New("retention: nil resolver")
	}
	if safetyFinalities < 0 {
		return 0, 0, fmt.Errorf("retention: safetyFinalities < 0: %d", safetyFinalities)
	}
	head, err := r.HeadTipSet(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("retention: head: %w", err)
	}
	if head == nil {
		return 0, 0, errors.New("retention: nil head")
	}
	safety := abi.ChainEpoch(safetyFinalities) * selector.ChainFinality

	finalized, err := finalityFloor(ctx, r, head)
	if err != nil {
		return 0, 0, fmt.Errorf("retention: finality floor: %w", err)
	}
	depth := finalized - safety
	if depth < 0 {
		depth = 0
	}
	return depth, safety, nil
}

// finalityFloor returns the epoch to base the retention cutoff on:
// min(EC-finalized, F3-finalized). Both are queried through the same
// selector.Resolver interface used for /rpc/v2 (#99), so semantics stay
// consistent across the code base.
func finalityFloor(ctx context.Context, r selector.Resolver, head *types.TipSet) (abi.ChainEpoch, error) {
	// EC first (never nil: falls back to head - ChainFinality below).
	ec, err := ecFinalizedEpoch(ctx, r, head)
	if err != nil {
		return 0, err
	}
	// F3 optional.
	f3, err := r.F3FinalizedTipSet(ctx)
	if err != nil || f3 == nil {
		return ec, nil
	}
	if f3.Height() < ec {
		return f3.Height(), nil
	}
	return ec, nil
}

// ecFinalizedEpoch returns the FRC-0089 EC-finalized epoch, or the
// static (head - ChainFinality) fallback when the calculator is below
// threshold. Never returns a negative value.
func ecFinalizedEpoch(ctx context.Context, r selector.Resolver, head *types.TipSet) (abi.ChainEpoch, error) {
	if ec, err := r.ECFinalizedTipSet(ctx); err == nil && ec != nil {
		return ec.Height(), nil
	}
	target := head.Height() - selector.ChainFinality
	if target < 0 {
		target = 0
	}
	return target, nil
}
