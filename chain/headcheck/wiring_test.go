package headcheck

import (
	"context"
	"sync"
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func operators(srcs []HeadSource) map[string]int {
	out := map[string]int{}
	for _, s := range srcs {
		out[voterKey(s)]++
	}
	return out
}

func TestDefaultSources_MainnetIsOperatorDiverse(t *testing.T) {
	srcs := DefaultSources(SourceOptions{Network: build.Mainnet, Gateway: "https://gateway.lantern.reiers.io"})
	ops := operators(srcs)
	// gateway (glif.io) + glif (glif.io) + chain.love => 2 distinct voters.
	if len(ops) != 2 || ops["op:glif.io"] != 2 || ops["op:chain.love"] != 1 {
		t.Fatalf("want glif.io x2 + chain.love, got %v", ops)
	}
}

func TestDefaultSources_FallbackOverrideSkipsDuplicateIndependent(t *testing.T) {
	srcs := DefaultSources(SourceOptions{Network: build.Mainnet, FallbackRPC: "https://api.chain.love/rpc/v1"})
	if ops := operators(srcs); len(srcs) != 1 || ops["op:chain.love"] != 1 {
		t.Fatalf("fallback=chain.love must not add chain.love twice, got %v", ops)
	}
}

func TestDefaultSources_CalibrationUsesFilfox(t *testing.T) {
	ops := operators(DefaultSources(SourceOptions{Network: build.Calibration}))
	if ops["op:glif.io"] != 1 || ops["op:filfox.info"] != 1 {
		t.Fatalf("calibration: want glif + filfox, got %v", ops)
	}
}

func TestDefaultSources_BridgeOffHasNoRPC(t *testing.T) {
	srcs := DefaultSources(SourceOptions{Network: build.Mainnet, NoFallbackRPC: true})
	if len(srcs) != 0 {
		t.Fatalf("bridge-off must add no RPC sources, got %d", len(srcs))
	}
	srcs = DefaultSources(SourceOptions{Network: build.Mainnet, NoFallbackRPC: true, ExtraRPCs: []string{" https://my.node/rpc/v1 ", ""}})
	if len(srcs) != 1 {
		t.Fatalf("operator RPCs are kept in bridge-off, got %d", len(srcs))
	}
}

// fakeIng records the gate the monitor installs.
type fakeIng struct {
	mu   sync.Mutex
	head abi.ChainEpoch
	gate func() bool
}

func (f *fakeIng) ObservedHead() abi.ChainEpoch { f.mu.Lock(); defer f.mu.Unlock(); return f.head }
func (f *fakeIng) SetHeadAdoptionGate(g func() bool) {
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
}
func (f *fakeIng) open() bool { f.mu.Lock(); g := f.gate; f.mu.Unlock(); return g == nil || g() }

// switchSource lets a test flip what a source reports between rounds.
type switchSource struct {
	name  string
	kind  bootstrap.Kind
	mu    *sync.Mutex
	epoch *abi.ChainEpoch
}

func (s switchSource) Name() string         { return s.name }
func (s switchSource) Kind() bootstrap.Kind { return s.kind }
func (s switchSource) HeadEpoch(context.Context) (abi.ChainEpoch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.epoch, nil
}

type nilStore struct{}

func (nilStore) GetTipSetByHeight(abi.ChainEpoch) (*ltypes.TipSet, error) { return nil, nil }

func TestStartGated_DivergeClosesAgreeReopens(t *testing.T) {
	ing := &fakeIng{head: 100}
	var mu sync.Mutex
	ext := abi.ChainEpoch(200) // far ahead => diverge
	srcs := []HeadSource{
		switchSource{"a", bootstrap.KindForest, &mu, &ext},
		switchSource{"b", bootstrap.KindUser, &mu, &ext},
	}
	results := make(chan Result, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon := StartGated(ctx, ing, nilStore{}, srcs, func(r Result) { results <- r })
	if mon == nil {
		t.Fatal("monitor must start with sources")
	}
	defer mon.Stop()
	select {
	case r := <-results:
		if r.Status != StatusDiverge || ing.open() {
			t.Fatalf("first round: want diverge + gate closed, got %s open=%v", r.Status, ing.open())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first round")
	}

	mu.Lock()
	ext = 100
	mu.Unlock()
	if r := mon.CheckOnce(ctx); r.Status != StatusAgree || !ing.open() {
		t.Fatalf("after agreement: want agree + gate open, got %s open=%v", r.Status, ing.open())
	}
}

func TestStartGated_NoSourcesIsNil(t *testing.T) {
	ing := &fakeIng{head: 100}
	if StartGated(context.Background(), ing, nil, nil, nil) != nil {
		t.Fatal("no sources => nil monitor")
	}
	if ing.gate != nil {
		t.Fatal("no sources => no gate installed")
	}
}
