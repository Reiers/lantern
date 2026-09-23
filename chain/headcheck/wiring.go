// Shared daemon wiring for the running-head divergence monitor (#160).
//
// Before #160 the source-set construction and the adoption-gate feedback
// lived only in pkg/daemon (embedded mode). The standalone `lantern daemon`
// (cmd/lantern) wired gossip + #79 fork choice + #80 corroboration but never
// started a Monitor, so production had no continuous head quorum. Both
// paths now call DefaultSources + StartGated so they cannot drift again.

package headcheck

import (
	"context"
	"strings"
	"sync/atomic"

	abi "github.com/filecoin-project/go-state-types/abi"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
	ltypes "github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/headcheck")

// SourceOptions describes the operator's configured endpoints.
type SourceOptions struct {
	Network build.Network
	// ExtraRPCs are operator-supplied Lotus-compatible RPC URLs
	// (--head-check-rpc / HeadCheckRPCs).
	ExtraRPCs []string
	// Gateway is the Lantern gateway base URL ("" = none).
	Gateway string
	// FallbackRPC overrides the Glif fallback URL ("" = network default).
	FallbackRPC string
	// NoFallbackRPC is bridge-off (#76): no upstream RPC sources at all.
	NoFallbackRPC bool
}

// GlifURL returns the public Glif RPC for a network.
func GlifURL(n build.Network) string {
	if n == build.Calibration {
		return "https://api.calibration.node.glif.io/rpc/v1"
	}
	return "https://api.node.glif.io/rpc/v1"
}

// IndependentRPCURL returns a public Lotus-compatible RPC run by a
// different operator than Glif (#153), or "" when none is known.
func IndependentRPCURL(n build.Network) string {
	switch n {
	case build.Mainnet:
		return "https://api.chain.love/rpc/v1"
	case build.Calibration:
		return "https://calibration.filfox.info/rpc/v1"
	}
	return ""
}

// DefaultSources builds the operator-diverse source set (#80, #153):
// operator RPCs, the gateway (votes as its upstream), the fallback RPC and
// a differently-operated public RPC. Bridge-off keeps only operator RPCs
// and the gateway; peer-backed sources are added by the caller (#154).
func DefaultSources(o SourceOptions) []HeadSource {
	var out []HeadSource
	for _, u := range o.ExtraRPCs {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, NewRPCHeadSource("", bootstrap.KindForest, u, "", 0))
		}
	}
	if o.Gateway != "" {
		out = append(out, NewGatewayHeadSource(o.Gateway, 0))
	}
	if !o.NoFallbackRPC && o.Network != build.Devnet {
		fb := strings.TrimSpace(o.FallbackRPC)
		if fb == "" {
			fb = GlifURL(o.Network)
		}
		out = append(out, NewRPCHeadSource("fallback-rpc", bootstrap.KindForest, fb, "", 0))
		if indep := IndependentRPCURL(o.Network); indep != "" && bootstrap.OperatorOf(indep) != bootstrap.OperatorOf(fb) {
			out = append(out, NewRPCHeadSource("independent-rpc", bootstrap.KindForest, indep, "", 0))
		}
	}
	return out
}

// GatedIngestor is the slice of blockingest.Ingestor the monitor needs.
type GatedIngestor interface {
	ObservedHead() abi.ChainEpoch
	SetHeadAdoptionGate(func() bool)
}

// TipSetStore is the slice of the header store the monitor needs.
type TipSetStore interface {
	GetTipSetByHeight(abi.ChainEpoch) (*ltypes.TipSet, error)
	HeadEpoch() abi.ChainEpoch
}

// StartGated starts a Monitor over sources and feeds its verdict back to
// the ingestor as the head-adoption gate (#79 item 2): DIVERGE closes the
// gate, AGREE reopens it, INSUFFICIENT keeps it open (a lightly-sourced
// node must not freeze). Returns nil when there are no sources. onResult,
// if non-nil, is called after the gate is updated (metrics/dashboard).
func StartGated(ctx context.Context, ing GatedIngestor, store TipSetStore, sources []HeadSource, onResult func(Result)) *Monitor {
	if len(sources) == 0 || ing == nil {
		return nil
	}
	var diverged atomic.Bool
	ing.SetHeadAdoptionGate(func() bool { return !diverged.Load() })
	// #162: our head is the store head (advanced by polling Sync AND
	// gossip), not just what gossip installed. ObservedHead alone is -1 on
	// a fresh node and frozen while the gate is closed, which deadlocked
	// the gate.
	local := ing.ObservedHead
	if store != nil {
		local = func() abi.ChainEpoch {
			h := store.HeadEpoch()
			if o := ing.ObservedHead(); o > h {
				h = o
			}
			return h
		}
	}
	cfg := Config{
		Local:   local,
		Sources: sources,
		OnResult: func(r Result) {
			switch r.Status {
			case StatusDiverge:
				diverged.Store(true)
				log.Warnw("running head DIVERGES from independent sources (possible eclipse/fork); HOLDING head adoption",
					"localHead", r.LocalHead, "medianExtHead", r.MedianExtHead,
					"agreeing", r.Agreeing, "disagreeing", r.Disagreeing, "reachable", r.Reachable,
					"checkpoint", r.CheckpointEpoch, "forkedVoters", r.ForkedKinds)
			case StatusAgree:
				if diverged.Swap(false) {
					log.Infow("running head re-corroborated; resuming head adoption",
						"localHead", r.LocalHead, "agreeing", r.Agreeing)
				}
				log.Debugw("head corroborated", "localHead", r.LocalHead, "agreeing", r.Agreeing, "keyChecked", r.KeyChecked)
			case StatusBehind:
				// #162: lag, not eclipse. Keep adopting so we catch up.
				diverged.Store(false)
				log.Infow("running head behind independent voters on the same chain; catching up",
					"localHead", r.LocalHead, "medianExtHead", r.MedianExtHead, "lagging", r.Lagging)
			case StatusInsufficient:
				diverged.Store(false)
				log.Debugw("head uncorroborated (too few reachable independent voters)",
					"localHead", r.LocalHead, "reachable", r.Reachable)
			}
			if onResult != nil {
				onResult(r)
			}
		},
	}
	if store != nil {
		// #152: tipset-key agreement at head - lookback.
		cfg.LocalTipSetAt = func(ep abi.ChainEpoch) (TipSetRef, bool) {
			ts, err := store.GetTipSetByHeight(ep)
			if err != nil || ts == nil {
				return TipSetRef{}, false
			}
			return TipSetRef{Epoch: ts.Height(), Key: ts.Key(), ParentWeight: ts.ParentWeight()}, true
		}
	}
	mon := New(cfg)
	mon.Start(ctx)
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Name())
	}
	log.Infow("running-head divergence monitor started", "sources", names, "lookback", mon.cfg.Lookback, "minAgree", mon.cfg.MinAgree)
	return mon
}
