// Unit tests for resolveAnchor (#167): stale F3 finality must never be
// persisted silently; it falls back to a fresh EC multi-source head or is
// refused.
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/anchorverify"
	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/types"
)

func arCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatalf("mh.Sum: %v", err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

// arNow returns a wall-clock time whose expected mainnet head is `epoch`.
func arNow(epoch int64) time.Time {
	return time.Unix(build.Mainnet.GenesisUnix()+epoch*int64(build.BlockDelaySecs), 0)
}

func arFin(t *testing.T, epoch int64) bootstrap.Finality {
	return bootstrap.Finality{
		Instance:  466453,
		Epoch:     epoch,
		TipSetKey: []cid.Cid{arCID(t, "f3-tsk")},
		StateRoot: arCID(t, "f3-state"),
	}
}

func arEC(t *testing.T, epoch int64) func(context.Context) (anchorverify.Result, error) {
	return func(context.Context) (anchorverify.Result, error) {
		return anchorverify.Result{
			Chosen: anchorverify.Candidate{
				Source:    "glif",
				Epoch:     abi.ChainEpoch(epoch),
				StateRoot: arCID(t, "ec-state"),
				TipSetKey: types.NewTipSetKey(arCID(t, "ec-a"), arCID(t, "ec-b")),
			},
			Method:          "multi-source-agreement",
			AgreeingSources: 2,
		}, nil
	}
}

func quiet(string, ...any) {}

const liveHead = 6_410_000

func TestResolveAnchor_FreshF3IsKept(t *testing.T) {
	fin := arFin(t, liveHead-10)
	called := false
	got, err := resolveAnchor(context.Background(), fin, anchorResolveOpts{
		network: build.Mainnet,
		now:     func() time.Time { return arNow(liveHead) },
		ecHead: func(context.Context) (anchorverify.Result, error) {
			called = true
			return anchorverify.Result{}, nil
		},
		logf: quiet,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("EC fallback must not run for fresh F3 finality")
	}
	if got.Source != anchorSourceF3 || got.Fin.Epoch != fin.Epoch {
		t.Fatalf("got %+v, want F3 finality kept", got)
	}
}

func TestResolveAnchor_StaleF3FallsBackToEC(t *testing.T) {
	stale := arFin(t, liveHead-586_000) // the 2026-09-28 mainnet shape
	got, err := resolveAnchor(context.Background(), stale, anchorResolveOpts{
		network: build.Mainnet,
		now:     func() time.Time { return arNow(liveHead) },
		ecHead:  arEC(t, liveHead-2),
		logf:    quiet,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != anchorSourceEC {
		t.Fatalf("source = %q, want %q", got.Source, anchorSourceEC)
	}
	if got.Fin.Epoch != liveHead-2 {
		t.Fatalf("epoch = %d, want EC head %d", got.Fin.Epoch, liveHead-2)
	}
	if !got.Fin.StateRoot.Equals(arCID(t, "ec-state")) || len(got.Fin.TipSetKey) != 2 {
		t.Fatalf("EC state root / tipset not carried over: %+v", got.Fin)
	}
	if got.Fin.Instance != stale.Instance {
		t.Fatalf("instance = %d, want latest F3 instance %d kept", got.Fin.Instance, stale.Instance)
	}
}

func TestResolveAnchor_StaleF3AllowStaleKeepsF3(t *testing.T) {
	stale := arFin(t, liveHead-586_000)
	got, err := resolveAnchor(context.Background(), stale, anchorResolveOpts{
		network:    build.Mainnet,
		allowStale: true,
		now:        func() time.Time { return arNow(liveHead) },
		ecHead: func(context.Context) (anchorverify.Result, error) {
			t.Fatal("EC fallback must not run with --allow-stale-anchor")
			return anchorverify.Result{}, nil
		},
		logf: quiet,
	})
	if err != nil || got.Source != anchorSourceF3 || got.Fin.Epoch != stale.Epoch {
		t.Fatalf("got %+v err %v, want stale F3 accepted", got, err)
	}
}

func TestResolveAnchor_StaleRefusedCases(t *testing.T) {
	stale := arFin(t, liveHead-586_000)
	cases := map[string]func(context.Context) (anchorverify.Result, error){
		"no EC source": nil,
		"EC fails": func(context.Context) (anchorverify.Result, error) {
			return anchorverify.Result{}, errors.New("sources disagree")
		},
		"EC not newer than F3": arEC(t, stale.Epoch),
		"EC itself stale":      arEC(t, liveHead-10_000),
		"EC missing state root": func(context.Context) (anchorverify.Result, error) {
			return anchorverify.Result{Chosen: anchorverify.Candidate{
				Epoch:     abi.ChainEpoch(liveHead - 1),
				TipSetKey: types.NewTipSetKey(arCID(t, "x")),
			}}, nil
		},
	}
	for name, ec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolveAnchor(context.Background(), stale, anchorResolveOpts{
				network: build.Mainnet,
				now:     func() time.Time { return arNow(liveHead) },
				ecHead:  ec,
				logf:    quiet,
			})
			if !errors.Is(err, ErrStaleAnchor) {
				t.Fatalf("err = %v, want ErrStaleAnchor", err)
			}
		})
	}
}

func TestResolveAnchor_UnknownGenesisPassesThrough(t *testing.T) {
	// Unconfigured devnet: no genesis, so no wall-clock gate.
	fin := arFin(t, 42)
	got, err := resolveAnchor(context.Background(), fin, anchorResolveOpts{network: build.Devnet, logf: quiet})
	if err != nil || got.Source != anchorSourceF3 || got.Fin.Epoch != 42 {
		t.Fatalf("got %+v err %v, want passthrough", got, err)
	}
}
