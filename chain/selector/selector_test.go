package selector

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/types"
)

// mockResolver drives the selector without touching real state. Each field
// controls exactly one code path; nil-valued *TipSet returns cover the
// "F3 unavailable" / "EC calculator below threshold" branches.
type mockResolver struct {
	head        *types.TipSet
	headErr     error
	ecFin       *types.TipSet
	ecFinErr    error
	f3Fin       *types.TipSet
	f3FinErr    error
	byHeight    func(h abi.ChainEpoch, from *types.TipSet, prev bool) (*types.TipSet, error)
	byHeightErr error
}

func (m *mockResolver) HeadTipSet(_ context.Context) (*types.TipSet, error) {
	return m.head, m.headErr
}
func (m *mockResolver) ECFinalizedTipSet(_ context.Context) (*types.TipSet, error) {
	return m.ecFin, m.ecFinErr
}
func (m *mockResolver) F3FinalizedTipSet(_ context.Context) (*types.TipSet, error) {
	return m.f3Fin, m.f3FinErr
}
func (m *mockResolver) TipSetByHeight(_ context.Context, h abi.ChainEpoch, from *types.TipSet, prev bool) (*types.TipSet, error) {
	if m.byHeightErr != nil {
		return nil, m.byHeightErr
	}
	if m.byHeight != nil {
		return m.byHeight(h, from, prev)
	}
	// Default: produce a synthetic tipset at exactly the requested height.
	return syntheticTipSet(h), nil
}

// syntheticTipSet builds the smallest valid *types.TipSet at height h.
// Real BlockHeader fields don't matter for selector logic; only Height()
// and Key() participate in the comparisons under test.
func syntheticTipSet(h abi.ChainEpoch) *types.TipSet {
	// A distinct-per-height Miner CID keeps tipset keys deterministic
	// so a test can compare tipsets by height alone.
	pref := cid.NewPrefixV1(cid.DagCBOR, mh.SHA2_256)
	minerCID, _ := pref.Sum([]byte{byte(h), byte(h >> 8)})
	bh := &types.BlockHeader{
		Miner:                 mustAddress(),
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

// mustAddress returns a tiny stable address; the value does not affect
// selector semantics but a valid one is required by NewTipSet.
func mustAddress() (addr addressLike) {
	// The concrete type is chain/types's own miner address; import indirection
	// avoided by using the address package via a tiny local helper file below.
	return newIDAddr(1000)
}

func TestResolveTag_Latest(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	got, err := ResolveTag(ctx, &mockResolver{head: head}, TagLatest)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got.Height() != head.Height() {
		t.Fatalf("latest height = %d, want %d", got.Height(), head.Height())
	}
}

func TestResolveTag_FinalizedPrefersECWhenF3Unavailable(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	ecFin := syntheticTipSet(999_970)
	got, err := ResolveTag(ctx, &mockResolver{head: head, ecFin: ecFin}, TagFinalized)
	if err != nil {
		t.Fatalf("finalized w/o F3: %v", err)
	}
	if got.Height() != ecFin.Height() {
		t.Fatalf("finalized = %d, want %d (ec)", got.Height(), ecFin.Height())
	}
}

func TestResolveTag_FinalizedPrefersF3WhenAhead(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_700)
	f3 := syntheticTipSet(999_950) // F3 ahead of EC
	got, err := ResolveTag(ctx, &mockResolver{head: head, ecFin: ec, f3Fin: f3}, TagFinalized)
	if err != nil {
		t.Fatalf("finalized w/ F3 ahead: %v", err)
	}
	if got.Height() != f3.Height() {
		t.Fatalf("finalized = %d, want %d (f3 ahead)", got.Height(), f3.Height())
	}
}

func TestResolveTag_FinalizedFallsBackToECWhenF3BehindOrEqual(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_950)
	f3 := syntheticTipSet(999_800) // F3 behind EC
	got, err := ResolveTag(ctx, &mockResolver{head: head, ecFin: ec, f3Fin: f3}, TagFinalized)
	if err != nil {
		t.Fatalf("finalized w/ F3 behind: %v", err)
	}
	if got.Height() != ec.Height() {
		t.Fatalf("finalized = %d, want %d (F3 behind -> EC)", got.Height(), ec.Height())
	}
}

func TestResolveTag_FinalizedTolerantOfF3Error(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	ec := syntheticTipSet(999_100)
	// An F3 error must NOT propagate up; it's treated as unavailable and
	// EC finality is used. This mirrors Lotus semantics.
	m := &mockResolver{head: head, ecFin: ec, f3FinErr: errors.New("boom")}
	got, err := ResolveTag(ctx, m, TagFinalized)
	if err != nil {
		t.Fatalf("finalized w/ F3 error: %v", err)
	}
	if got.Height() != ec.Height() {
		t.Fatalf("finalized fallback ignored F3 error; got %d, want %d", got.Height(), ec.Height())
	}
}

func TestResolveTag_FinalizedFallsBackToStaticWhenECCalcBelowThreshold(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	// ecFin == nil simulates the FRC-0089 calculator below-threshold branch.
	// The resolver then falls back to head - ChainFinality via TipSetByHeight.
	m := &mockResolver{head: head, ecFin: nil}
	got, err := ResolveTag(ctx, m, TagFinalized)
	if err != nil {
		t.Fatalf("finalized w/ ec below threshold: %v", err)
	}
	want := head.Height() - ChainFinality
	if got.Height() != want {
		t.Fatalf("finalized static fallback height = %d, want %d", got.Height(), want)
	}
}

func TestResolveTag_SafeClampsToHeadMinusDistance(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	// finalized is way behind safeHeight -> safe = head - SafeHeightDistance
	ec := syntheticTipSet(950_000)
	m := &mockResolver{head: head, ecFin: ec}
	got, err := ResolveTag(ctx, m, TagSafe)
	if err != nil {
		t.Fatalf("safe: %v", err)
	}
	want := head.Height() - SafeHeightDistance
	if got.Height() != want {
		t.Fatalf("safe height = %d, want %d (head - SafeHeightDistance)", got.Height(), want)
	}
}

func TestResolveTag_SafePrefersFinalizedWhenRecent(t *testing.T) {
	ctx := context.Background()
	head := syntheticTipSet(1_000_000)
	// finalized is INSIDE the SafeHeightDistance window -> safe returns finalized.
	ec := syntheticTipSet(999_900)
	m := &mockResolver{head: head, ecFin: ec}
	got, err := ResolveTag(ctx, m, TagSafe)
	if err != nil {
		t.Fatalf("safe: %v", err)
	}
	if got.Height() != ec.Height() {
		t.Fatalf("safe height = %d, want %d (finalized is recent enough)", got.Height(), ec.Height())
	}
}

func TestResolveTag_SafeClampsAtZero(t *testing.T) {
	ctx := context.Background()
	// Very low head -> safeHeight < 0 must clamp to 0.
	head := syntheticTipSet(50)
	ec := syntheticTipSet(10)
	m := &mockResolver{head: head, ecFin: ec}
	got, err := ResolveTag(ctx, m, TagSafe)
	if err != nil {
		t.Fatalf("safe near-genesis: %v", err)
	}
	// head - 200 clamps to 0. finalized (ec, height 10) is >= 0, so it wins.
	if got.Height() != ec.Height() {
		t.Fatalf("safe near-genesis = %d, want %d", got.Height(), ec.Height())
	}
}

func TestResolveTag_UnknownTagRejected(t *testing.T) {
	ctx := context.Background()
	if _, err := ResolveTag(ctx, &mockResolver{head: syntheticTipSet(1)}, Tag("stable")); err == nil {
		t.Fatal("expected error for unknown tag")
	}
}

func TestResolveTag_NilResolverRejected(t *testing.T) {
	if _, err := ResolveTag(context.Background(), nil, TagLatest); err == nil {
		t.Fatal("expected error for nil resolver")
	}
}

func TestKnownTag(t *testing.T) {
	for _, tag := range []string{"latest", "finalized", "safe"} {
		if !KnownTag(tag) {
			t.Errorf("KnownTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range []string{"", "stable", "LATEST", "finalised"} {
		if KnownTag(tag) {
			t.Errorf("KnownTag(%q) = true, want false", tag)
		}
	}
}
