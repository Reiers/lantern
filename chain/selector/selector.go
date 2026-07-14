// Package selector resolves Lotus /rpc/v2 tipset-selector tags (latest,
// finalized, safe) against Lantern's chain state (issue #99).
//
// The three tags carry Lotus semantics byte-for-byte:
//
//   - latest    = heaviest tipset (current head).
//   - finalized = max(EC-finalized, F3-finalized). If F3 is unavailable or
//     behind EC, falls back to EC finality (from the FRC-0089 calculator
//     shipped in #96, else static head - ChainFinality).
//   - safe      = finalized clamped to at least (head - SafeHeightDistance).
//     If the tipset at safe-height is null, the first non-null parent is
//     returned (behavior of TipSetByHeight with previous=true).
//
// Lantern serves these more honestly than a gateway because "finalized" and
// "safe" both derive from cryptographically verified state: F3 certs (BLS
// aggregate over >= 2/3 of committee power) and observed EC finality on top
// of a header store that verifies every header's BLS block signature.
//
// This package is deliberately a pure resolver: it does no I/O beyond the
// Resolver interface it takes. That keeps the /rpc/v2 mount trivially
// testable with a mock Resolver, and lets non-RPC callers (finality-aware
// message-search, retention pruning, …) reuse the same logic.
package selector

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/types"
)

// Filecoin network constants, mirrored from Lotus buildconstants. These are
// mainnet + calibration values; devnet uses the same distances (they are
// derived from ChainFinality/epoch-length ratios, not the per-network
// block-time).
const (
	// SafeHeightDistance is head - safeHeight. Lotus mainnet value.
	SafeHeightDistance abi.ChainEpoch = 200

	// ChainFinality is the static-fallback EC finality window used when the
	// FRC-0089 calculator is not yet at threshold. Lotus mainnet value.
	ChainFinality abi.ChainEpoch = 900
)

// Tag names the well-known selector tags.
type Tag string

const (
	TagLatest    Tag = "latest"
	TagFinalized Tag = "finalized"
	TagSafe      Tag = "safe"
)

// KnownTag reports whether s is a supported selector tag.
func KnownTag(s string) bool {
	switch Tag(s) {
	case TagLatest, TagFinalized, TagSafe:
		return true
	}
	return false
}

// Resolver is the state-lookup surface a tipset selector needs. Callers
// wiring /rpc/v2 typically satisfy this with an adapter over the chain
// api + ecfinality.Cache + f3 accessor. Tests use a mock.
type Resolver interface {
	// HeadTipSet returns the heaviest known tipset.
	HeadTipSet(ctx context.Context) (*types.TipSet, error)

	// ECFinalizedTipSet returns the FRC-0089 EC-finalized tipset. Returns
	// (nil, nil) if the calculator has not yet reached threshold; the
	// resolver then falls back to (head - ChainFinality).
	ECFinalizedTipSet(ctx context.Context) (*types.TipSet, error)

	// F3FinalizedTipSet returns the tipset finalized by the latest F3
	// cert, or (nil, nil) if F3 is unavailable / not ready. A non-nil
	// error is treated as unavailable (a healthy node must not fail-open
	// on selector queries when F3 has a transient issue; Lotus does the
	// same).
	F3FinalizedTipSet(ctx context.Context) (*types.TipSet, error)

	// TipSetByHeight returns the tipset at `height` reached by walking
	// back from `from`. When the tipset at height is null and
	// previous=true, returns the first non-null parent (matches Lotus's
	// GetTipsetByHeight semantics with prev=true).
	TipSetByHeight(ctx context.Context, height abi.ChainEpoch, from *types.TipSet, previous bool) (*types.TipSet, error)
}

// ResolveTag returns the tipset for a well-known selector tag.
func ResolveTag(ctx context.Context, r Resolver, tag Tag) (*types.TipSet, error) {
	if r == nil {
		return nil, errors.New("selector: nil resolver")
	}
	switch tag {
	case TagLatest:
		return r.HeadTipSet(ctx)
	case TagFinalized:
		return resolveFinalized(ctx, r)
	case TagSafe:
		return resolveSafe(ctx, r)
	default:
		return nil, fmt.Errorf("selector: unknown tag %q", string(tag))
	}
}

// resolveFinalized returns max(EC-finalized, F3-finalized). Falls back to
// EC finality if F3 is unavailable / behind EC / errors.
func resolveFinalized(ctx context.Context, r Resolver) (*types.TipSet, error) {
	ec, err := ecFinalizedOrFallback(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("selector: ec finalized: %w", err)
	}
	// F3 optional.
	f3, err := r.F3FinalizedTipSet(ctx)
	if err != nil {
		return ec, nil
	}
	if f3 == nil {
		return ec, nil
	}
	if f3.Height() <= ec.Height() {
		return ec, nil
	}
	return f3, nil
}

// resolveSafe returns finalized clamped to at least (head - SafeHeightDistance).
// If the tipset at safe-height is null, the first non-null parent is returned.
func resolveSafe(ctx context.Context, r Resolver) (*types.TipSet, error) {
	finalized, err := resolveFinalized(ctx, r)
	if err != nil {
		return nil, err
	}
	head, err := r.HeadTipSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("selector: head: %w", err)
	}
	if head == nil {
		return nil, errors.New("selector: nil head")
	}
	safeHeight := head.Height() - SafeHeightDistance
	if safeHeight < 0 {
		safeHeight = 0
	}
	if finalized != nil && finalized.Height() >= safeHeight {
		return finalized, nil
	}
	return r.TipSetByHeight(ctx, safeHeight, head, true)
}

// ecFinalizedOrFallback returns EC-finalized from the FRC-0089 calculator
// when available, else head - ChainFinality. Never returns nil on success.
func ecFinalizedOrFallback(ctx context.Context, r Resolver) (*types.TipSet, error) {
	if ec, err := r.ECFinalizedTipSet(ctx); err == nil && ec != nil {
		return ec, nil
	}
	head, err := r.HeadTipSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}
	if head == nil {
		return nil, errors.New("nil head")
	}
	target := head.Height() - ChainFinality
	if target < 0 {
		target = 0
	}
	return r.TipSetByHeight(ctx, target, head, true)
}
