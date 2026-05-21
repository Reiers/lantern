package vm

import (
	"context"
	"testing"

	"github.com/filecoin-project/go-state-types/big"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/types"
)

// fakeBaseFee is a static BaseFeeSource for tests.
type fakeBaseFee struct{ v big.Int }

func (f fakeBaseFee) CurrentBaseFee(_ context.Context) (big.Int, error) { return f.v, nil }

// fakePremium is a static PremiumSource for tests.
type fakePremium struct{ samples []big.Int }

func (f fakePremium) RecentPremiums(_ context.Context, _ int64, _ int) ([]big.Int, error) {
	return f.samples, nil
}

func TestEstimateGasLimit_Send(t *testing.T) {
	e := &GasEstimator{}
	msg := &types.Message{
		From:       mustIDAddr(100),
		To:         mustIDAddr(101),
		Method:     0,
		Value:      big.NewInt(1),
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(1),
	}
	gl, err := e.EstimateGasLimit(context.Background(), msg)
	if err != nil {
		t.Fatalf("EstimateGasLimit: %v", err)
	}
	if gl <= 0 || gl > build.BlockGasLimit {
		t.Errorf("Send gas limit out of range: got %d", gl)
	}
}

func TestEstimateGasLimit_BuiltinCall(t *testing.T) {
	e := &GasEstimator{}
	msg := &types.Message{
		From:       mustIDAddr(100),
		To:         mustIDAddr(1000),
		Method:     6, // miner.PreCommitSector
		Value:      big.Zero(),
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(1),
	}
	gl, err := e.EstimateGasLimit(context.Background(), msg)
	if err != nil {
		t.Fatalf("EstimateGasLimit: %v", err)
	}
	// Should be > 75M (per-method ceiling).
	if gl < 75_000_000 {
		t.Errorf("Builtin call gas limit too low: got %d, want >= 75M", gl)
	}
}

func TestEstimateFeeCap(t *testing.T) {
	e := &GasEstimator{
		BaseFee: fakeBaseFee{v: big.NewInt(1000)},
	}
	fc, err := e.EstimateFeeCap(context.Background(), big.NewInt(100), 10)
	if err != nil {
		t.Fatalf("EstimateFeeCap: %v", err)
	}
	// Expect 2x baseFee + premium = 2100.
	if !fc.Equals(big.NewInt(2100)) {
		t.Errorf("FeeCap: want 2100, got %s", fc)
	}
}

func TestEstimateFeeCap_FloorBaseFee(t *testing.T) {
	e := &GasEstimator{} // no BaseFee source
	fc, err := e.EstimateFeeCap(context.Background(), big.Zero(), 10)
	if err != nil {
		t.Fatalf("EstimateFeeCap: %v", err)
	}
	if fc.LessThan(big.NewInt(build.MinimumBaseFee)) {
		t.Errorf("FeeCap should be >= MinimumBaseFee, got %s", fc)
	}
}

func TestEstimatePremium_FromSamples(t *testing.T) {
	e := &GasEstimator{
		Premium: fakePremium{samples: []big.Int{
			big.NewInt(100_000), big.NewInt(200_000), big.NewInt(300_000),
			big.NewInt(400_000), big.NewInt(500_000),
		}},
	}
	pr, err := e.EstimateGasPremium(context.Background(), 10)
	if err != nil {
		t.Fatalf("EstimateGasPremium: %v", err)
	}
	// 60th percentile of 5 samples => index 3 => 400000.
	if !pr.Equals(big.NewInt(400_000)) {
		t.Errorf("Premium: want 400000, got %s", pr)
	}
}

func TestEstimatePremium_FloorIsFallback(t *testing.T) {
	e := &GasEstimator{
		Premium: fakePremium{samples: []big.Int{
			big.NewInt(100), big.NewInt(200), big.NewInt(300),
		}},
	}
	pr, err := e.EstimateGasPremium(context.Background(), 10)
	if err != nil {
		t.Fatalf("EstimateGasPremium: %v", err)
	}
	// All samples below fallback floor => return fallback.
	if !pr.Equals(big.NewInt(100_000)) {
		t.Errorf("Premium floor: want 100000, got %s", pr)
	}
}

func TestEstimatePremium_Fallback(t *testing.T) {
	e := &GasEstimator{}
	pr, err := e.EstimateGasPremium(context.Background(), 10)
	if err != nil {
		t.Fatalf("EstimateGasPremium: %v", err)
	}
	// Fallback = 100k.
	if !pr.Equals(big.NewInt(100_000)) {
		t.Errorf("Fallback premium: want 100000, got %s", pr)
	}
}

func TestEstimateMessageGas_FillsAll(t *testing.T) {
	e := &GasEstimator{
		BaseFee: fakeBaseFee{v: big.NewInt(1000)},
	}
	msg := &types.Message{
		From:   mustIDAddr(100),
		To:     mustIDAddr(101),
		Method: 0,
		Value:  big.NewInt(1),
	}
	out, err := e.EstimateMessageGas(context.Background(), msg, big.Zero())
	if err != nil {
		t.Fatalf("EstimateMessageGas: %v", err)
	}
	if out.GasLimit == 0 || out.GasFeeCap.NilOrZero() || out.GasPremium.NilOrZero() {
		t.Errorf("EstimateMessageGas didn't fill all fields: %+v", out)
	}
}

func TestEstimateMessageGas_PreservesUserInput(t *testing.T) {
	e := &GasEstimator{
		BaseFee: fakeBaseFee{v: big.NewInt(1000)},
	}
	msg := &types.Message{
		From:       mustIDAddr(100),
		To:         mustIDAddr(101),
		Method:     0,
		Value:      big.NewInt(1),
		GasLimit:   42, // user explicitly set
		GasFeeCap:  big.NewInt(99),
		GasPremium: big.NewInt(7),
	}
	out, err := e.EstimateMessageGas(context.Background(), msg, big.Zero())
	if err != nil {
		t.Fatalf("EstimateMessageGas: %v", err)
	}
	if out.GasLimit != 42 || !out.GasFeeCap.Equals(big.NewInt(99)) || !out.GasPremium.Equals(big.NewInt(7)) {
		t.Errorf("User input clobbered: %+v", out)
	}
}

func TestStateCall_NilAccessor(t *testing.T) {
	msg := &types.Message{
		From:       mustIDAddr(100),
		To:         mustIDAddr(101),
		Method:     0,
		Value:      big.Zero(),
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(1),
	}
	r, err := StateCall(context.Background(), nil, msg, ApplyOptions{})
	if err != nil {
		t.Fatalf("StateCall: %v", err)
	}
	if r.Receipt.ExitCode != 0 {
		t.Errorf("StateCall Send ExitCode: want 0, got %d", r.Receipt.ExitCode)
	}
	if r.Duration <= 0 {
		t.Errorf("StateCall Duration should be > 0")
	}
}
