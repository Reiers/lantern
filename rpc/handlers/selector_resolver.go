package handlers

// Production selector.Resolver backed by ChainAPI (#99).
//
// Wires the tipset-selector resolver to Lantern's real chain state: the
// header store for head + height lookups, and the FRC-0089 EC-finality
// calculator for the ec-finalized tipset. F3-finalized currently returns
// (nil, nil) because Lantern V1 doesn't run an in-process F3 subscriber
// (the daemon probes F3 RPC only during boot, for the trusted-root anchor
// -- see attachF3Latest); the selector then falls back to EC finality,
// which is the same behavior Lotus takes when F3 is unavailable.
//
// When a future daemon runs an in-process F3 subscriber, plug it in by
// setting ChainAPI.F3LatestFinalizedTipSet (a package-level func hook).

import (
	"context"
	"errors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/ecfinality"
	"github.com/Reiers/lantern/chain/selector"
	"github.com/Reiers/lantern/chain/types"
)

// SelectorResolver returns a selector.Resolver backed by this ChainAPI.
// The optional ecFin is the FRC-0089 EC-finality calculator (#96); pass
// nil to use only the static head - ChainFinality fallback.
func (c *ChainAPI) SelectorResolver(ecFin *ecfinality.Cache) selector.Resolver {
	return &chainAPISelectorResolver{c: c, ecFin: ecFin}
}

type chainAPISelectorResolver struct {
	c     *ChainAPI
	ecFin *ecfinality.Cache
}

func (r *chainAPISelectorResolver) HeadTipSet(ctx context.Context) (*types.TipSet, error) {
	return r.c.ChainHead(ctx)
}

// ECFinalizedTipSet queries the FRC-0089 calculator. Returns (nil, nil)
// when the calculator is below its observation threshold; the selector
// then falls back to head - ChainFinality via TipSetByHeight.
func (r *chainAPISelectorResolver) ECFinalizedTipSet(ctx context.Context) (*types.TipSet, error) {
	if r.ecFin == nil {
		return nil, nil
	}
	st, err := r.ecFin.Status()
	if err != nil {
		// Errors from the calculator are treated as "below threshold" to
		// keep the selector honest under startup / degraded conditions.
		return nil, nil
	}
	if st == nil || st.ThresholdDepth < 0 || st.FinalizedEpoch < 0 {
		return nil, nil
	}
	head, err := r.HeadTipSet(ctx)
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, errors.New("chainapi selector resolver: nil head")
	}
	return r.TipSetByHeight(ctx, st.FinalizedEpoch, head, true)
}

// F3FinalizedTipSet is a stub for Lantern V1: no in-process F3 subscriber
// runs, so we return (nil, nil) and let finalized fall through to EC.
// A future subscriber wires here.
func (r *chainAPISelectorResolver) F3FinalizedTipSet(ctx context.Context) (*types.TipSet, error) {
	return nil, nil
}

func (r *chainAPISelectorResolver) TipSetByHeight(ctx context.Context, h abi.ChainEpoch, _ *types.TipSet, _ bool) (*types.TipSet, error) {
	// ChainAPI.ChainGetTipSetByHeight already implements "previous non-null
	// parent" semantics via the header store when present. The `from`
	// anchor is ignored: Lantern's header store has a single canonical
	// heaviest chain (no reorg-aware anchor walk), so height lookups are
	// unambiguous.
	return r.c.ChainGetTipSetByHeight(ctx, h, types.EmptyTSK)
}

var _ selector.Resolver = (*chainAPISelectorResolver)(nil)
