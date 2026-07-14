package retention

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/selector"
	"github.com/Reiers/lantern/chain/types"
)

// mockResolver is a minimal selector.Resolver drop-in for retention tests.
// It only tracks the three finality inputs Depth cares about (head +
// ecFin + f3Fin) plus a synthetic TipSetByHeight for the static fallback.
type mockResolver struct {
	head     *types.TipSet
	ecFin    *types.TipSet
	f3Fin    *types.TipSet
	f3Err    error
	headErr  error
	ecErr    error
	byHeight func(h abi.ChainEpoch) *types.TipSet
}

func (m *mockResolver) HeadTipSet(_ context.Context) (*types.TipSet, error) {
	return m.head, m.headErr
}
func (m *mockResolver) ECFinalizedTipSet(_ context.Context) (*types.TipSet, error) {
	return m.ecFin, m.ecErr
}
func (m *mockResolver) F3FinalizedTipSet(_ context.Context) (*types.TipSet, error) {
	return m.f3Fin, m.f3Err
}
func (m *mockResolver) TipSetByHeight(_ context.Context, h abi.ChainEpoch, _ *types.TipSet, _ bool) (*types.TipSet, error) {
	if m.byHeight != nil {
		return m.byHeight(h), nil
	}
	return syntheticTipSet(h), nil
}

func syntheticTipSet(h abi.ChainEpoch) *types.TipSet {
	pref := cid.NewPrefixV1(cid.DagCBOR, mh.SHA2_256)
	minerCID, _ := pref.Sum([]byte{byte(h), byte(h >> 8), byte(h >> 16)})
	addr, err := address.NewIDAddress(1000)
	if err != nil {
		panic(err)
	}
	bh := &types.BlockHeader{
		Miner:                 addr,
		Height:                h,
		Timestamp:             uint64(h) * 30,
		ParentStateRoot:       minerCID,
		ParentMessageReceipts: minerCID,
		Messages:              minerCID,
		Parents:               []cid.Cid{minerCID},
	}
	ts, err := types.NewTipSet([]*types.BlockHeader{bh})
	if err != nil {
		panic(err)
	}
	return ts
}

func TestDepth_ECOnly_ZeroSafety(t *testing.T) {
	// Only EC finality; safetyFinalities = 0 means "prune below finality".
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	m := &mockResolver{head: head, ecFin: ec}
	got, margin, err := Depth(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if got != ec.Height() {
		t.Fatalf("retentionEpoch = %d, want %d (ec height)", got, ec.Height())
	}
	if margin != 0 {
		t.Fatalf("margin = %d, want 0", margin)
	}
}

func TestDepth_ECOnly_WithSafetyFinalities(t *testing.T) {
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	// safetyFinalities=1 => extra 900 epochs below finality.
	m := &mockResolver{head: head, ecFin: ec}
	got, margin, err := Depth(context.Background(), m, 1)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	want := ec.Height() - selector.ChainFinality
	if got != want {
		t.Fatalf("retentionEpoch = %d, want %d", got, want)
	}
	if margin != selector.ChainFinality {
		t.Fatalf("margin = %d, want %d", margin, selector.ChainFinality)
	}
}

func TestDepth_F3ChoosesLowerFloor(t *testing.T) {
	// F3 behind EC => finality floor = F3 (min of the two).
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_500)
	f3 := syntheticTipSet(999_200) // behind EC
	m := &mockResolver{head: head, ecFin: ec, f3Fin: f3}
	got, _, err := Depth(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if got != f3.Height() {
		t.Fatalf("retentionEpoch = %d, want %d (min of ec/f3)", got, f3.Height())
	}
}

func TestDepth_F3AheadIgnored(t *testing.T) {
	// F3 ahead of EC => finality floor = EC (min of the two).
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	f3 := syntheticTipSet(999_500) // ahead of EC
	m := &mockResolver{head: head, ecFin: ec, f3Fin: f3}
	got, _, err := Depth(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if got != ec.Height() {
		t.Fatalf("retentionEpoch = %d, want %d (min)", got, ec.Height())
	}
}

func TestDepth_F3ErrorTolerated(t *testing.T) {
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	m := &mockResolver{head: head, ecFin: ec, f3Err: errors.New("f3 boom")}
	got, _, err := Depth(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("Depth (F3 error should be tolerated): %v", err)
	}
	if got != ec.Height() {
		t.Fatalf("retentionEpoch = %d, want %d (f3 error -> EC only)", got, ec.Height())
	}
}

func TestDepth_ECBelowThresholdStaticFallback(t *testing.T) {
	// FRC-0089 calculator below threshold => static fallback:
	// finalityFloor = head - ChainFinality.
	head := syntheticTipSet(1_000_000)
	m := &mockResolver{head: head, ecFin: nil}
	got, _, err := Depth(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("Depth (static fallback): %v", err)
	}
	want := head.Height() - selector.ChainFinality
	if got != want {
		t.Fatalf("retentionEpoch = %d, want %d (head - ChainFinality)", got, want)
	}
}

func TestDepth_ClampAtZero(t *testing.T) {
	// Near-genesis: finality floor - safety < 0 must clamp to 0.
	head := syntheticTipSet(500)
	ec := syntheticTipSet(100)
	m := &mockResolver{head: head, ecFin: ec}
	// safetyFinalities=1 => margin=900, ec=100, ec-900=-800, clamp to 0.
	got, _, err := Depth(context.Background(), m, 1)
	if err != nil {
		t.Fatalf("Depth (near genesis): %v", err)
	}
	if got != 0 {
		t.Fatalf("retentionEpoch = %d, want 0 (clamp)", got)
	}
}

func TestDepth_NilResolverRejected(t *testing.T) {
	if _, _, err := Depth(context.Background(), nil, 0); err == nil {
		t.Fatal("expected error for nil resolver")
	}
}

func TestDepth_NegativeSafetyRejected(t *testing.T) {
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	m := &mockResolver{head: head, ecFin: ec}
	if _, _, err := Depth(context.Background(), m, -1); err == nil {
		t.Fatal("expected error for negative safetyFinalities")
	}
}

func TestDefaultSafetyFinalitiesValue(t *testing.T) {
	// Sanity: the default is 1 finality (~900 epochs, ~7.5h at 30s blocks).
	if DefaultSafetyFinalities != 1 {
		t.Errorf("DefaultSafetyFinalities = %d, want 1", DefaultSafetyFinalities)
	}
}
