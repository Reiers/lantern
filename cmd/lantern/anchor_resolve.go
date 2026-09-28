// Stale-F3 anchor fallback (#167).
//
// The bootstrap quorum anchors on the latest F3 finality certificate. When
// F3 stops producing certificates (mainnet on 2026-09-28: every public
// source serves instance 466453 at epoch 5824156, ~203 days behind the
// wall-clock head), every source still *agrees*, so the quorum succeeds
// and hands back a finality far behind the live chain. `lantern init`
// refused it (fresh installs broke); `repair`, the dashboard renew action
// and the bridge-off auto-stale-reset wrote it to disk unchecked.
//
// resolveAnchor is the single gate every anchor writer now goes through:
//
//  1. F3 winner within maxAnchorLagEpochs of the wall-clock head -> use it.
//  2. Stale and --allow-stale-anchor set -> use it with a warning.
//  3. Stale otherwise -> fall back to the #54 EC multi-source head
//     agreement (independent operators, #153) at the live head. The EC
//     head must be newer than the F3-finalized epoch and itself fresh;
//     anything else is refused.
//
// Chain state only; keys are never touched.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/anchorverify"
	"github.com/Reiers/lantern/chain/bootstrap"
)

// maxAnchorLagEpochs is the widest gap between an anchor and the
// wall-clock expected head that we accept without complaint (24h).
const maxAnchorLagEpochs = 2880

const (
	anchorSourceF3 = "f3-quorum"
	anchorSourceEC = "ec-multi-source"
)

// ErrStaleAnchor means the F3 winner was stale and no fresh EC anchor could
// replace it.
var ErrStaleAnchor = errors.New("stale anchor")

type anchorChoice struct {
	Fin    bootstrap.Finality
	Source string // anchorSourceF3 | anchorSourceEC
}

type anchorResolveOpts struct {
	network    build.Network
	allowStale bool
	now        func() time.Time
	// ecHead returns the #54-verified live head. Injectable for tests.
	ecHead func(ctx context.Context) (anchorverify.Result, error)
	logf   func(format string, args ...any)
}

// defaultAnchorResolveOpts wires the production EC head fetch (gateway +
// Glif + an independent operator, per-operator agreement).
func defaultAnchorResolveOpts(network build.Network, gw string, allowStale bool) anchorResolveOpts {
	return anchorResolveOpts{
		network:    network,
		allowStale: allowStale,
		ecHead: func(ctx context.Context) (anchorverify.Result, error) {
			return verifiedECHead(ctx, gw, network, false)
		},
	}
}

func epochLag(network build.Network, now time.Time, epoch int64) (lag, expected int64, ok bool) {
	exp := network.ExpectedHeadEpoch(now.Unix())
	if exp <= 0 {
		return 0, exp, false
	}
	lag = exp - epoch
	if lag < 0 {
		lag = -lag
	}
	return lag, exp, true
}

// resolveAnchor decides which finality gets persisted as the trust anchor.
func resolveAnchor(ctx context.Context, fin bootstrap.Finality, o anchorResolveOpts) (anchorChoice, error) {
	logf := o.logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Printf(format, args...) }
	}
	now := time.Now
	if o.now != nil {
		now = o.now
	}

	lag, exp, known := epochLag(o.network, now(), fin.Epoch)
	if !known || lag <= maxAnchorLagEpochs {
		// Unknown genesis (unconfigured devnet) or fresh F3 finality.
		return anchorChoice{Fin: fin, Source: anchorSourceF3}, nil
	}
	if o.allowStale {
		logf("  ⚠ anchor is %d epochs (~%.1f days) from wall-clock expected head %d, accepted due to --allow-stale-anchor\n",
			lag, float64(lag)/2880.0, exp)
		return anchorChoice{Fin: fin, Source: anchorSourceF3}, nil
	}

	logf("  ⚠ F3 finality is stale: instance %d epoch %d is %d epochs (~%.1f days) behind wall-clock head %d (#167)\n",
		fin.Instance, fin.Epoch, lag, float64(lag)/2880.0, exp)
	logf("    falling back to EC multi-source head agreement (independent operators, #54/#153)\n")

	if o.ecHead == nil {
		return anchorChoice{}, fmt.Errorf("%w: F3 finality epoch %d is ~%.1f days old and no EC head source is configured", ErrStaleAnchor, fin.Epoch, float64(lag)/2880.0)
	}
	res, err := o.ecHead(ctx)
	if err != nil {
		return anchorChoice{}, fmt.Errorf("%w: F3 finality epoch %d is ~%.1f days old and EC fallback failed: %v (override with --allow-stale-anchor)",
			ErrStaleAnchor, fin.Epoch, float64(lag)/2880.0, err)
	}
	ecEpoch := int64(res.Chosen.Epoch)
	if ecEpoch <= fin.Epoch {
		return anchorChoice{}, fmt.Errorf("%w: EC head epoch %d is not newer than F3 finalized epoch %d", ErrStaleAnchor, ecEpoch, fin.Epoch)
	}
	if ecLag, _, _ := epochLag(o.network, now(), ecEpoch); ecLag > maxAnchorLagEpochs {
		return anchorChoice{}, fmt.Errorf("%w: EC head epoch %d is itself %d epochs from wall-clock head %d", ErrStaleAnchor, ecEpoch, ecLag, exp)
	}
	if !res.Chosen.StateRoot.Defined() || len(res.Chosen.TipSetKey.Cids()) == 0 {
		return anchorChoice{}, fmt.Errorf("%w: EC head is missing state root or tipset key", ErrStaleAnchor)
	}

	out := bootstrap.Finality{
		// Keep the latest F3 instance we saw: it is what the daemon shows
		// as F3Instance, and it is still the newest certificate available.
		Instance:  fin.Instance,
		TipSetKey: res.Chosen.TipSetKey.Cids(),
		StateRoot: res.Chosen.StateRoot,
		Epoch:     ecEpoch,
	}
	logf("  ✓ EC anchor: epoch %d via %s (%d independent operators agree, f3-checked=%t)\n",
		ecEpoch, res.Method, res.AgreeingSources, res.F3Checked)
	return anchorChoice{Fin: out, Source: anchorSourceEC}, nil
}
