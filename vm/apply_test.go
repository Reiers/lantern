package vm

import (
	"context"
	"testing"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"

	"github.com/Reiers/lantern/chain/types"
)

func mustIDAddr(id uint64) addr.Address {
	a, err := addr.NewIDAddress(id)
	if err != nil {
		panic(err)
	}
	return a
}

// TestApply_SendNilAccessor exercises the "no accessor available" path:
// we should still compute a plausible receipt for a Send.
func TestApply_SendNilAccessor(t *testing.T) {
	msg := &types.Message{
		Version:    0,
		From:       mustIDAddr(100),
		To:         mustIDAddr(101),
		Nonce:      0,
		Value:      big.NewInt(1_000_000),
		GasLimit:   2_000_000,
		GasFeeCap:  big.NewInt(100_000_000),
		GasPremium: big.NewInt(100_000),
		Method:     abi.MethodNum(0),
	}
	r, err := Apply(context.Background(), nil, msg, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.Receipt.ExitCode != 0 {
		t.Errorf("ExitCode: want 0, got %d", r.Receipt.ExitCode)
	}
	if r.Receipt.GasUsed <= 0 {
		t.Errorf("GasUsed should be > 0, got %d", r.Receipt.GasUsed)
	}
	// Encoded message length is small but non-zero.
	if r.GasCost.GasUsed != r.Receipt.GasUsed {
		t.Errorf("GasCost.GasUsed should equal Receipt.GasUsed, got %d vs %d",
			r.GasCost.GasUsed, r.Receipt.GasUsed)
	}
}

// TestApply_NilMessage rejects nil messages.
func TestApply_NilMessage(t *testing.T) {
	_, err := Apply(context.Background(), nil, nil, ApplyOptions{})
	if err == nil {
		t.Fatal("expected error for nil message")
	}
}

// TestApply_ValueXferIncreasesGas: sending value should cost more than
// a zero-value Send.
func TestApply_ValueXferIncreasesGas(t *testing.T) {
	mk := func(value big.Int) *types.Message {
		return &types.Message{
			From:       mustIDAddr(100),
			To:         mustIDAddr(101),
			Value:      value,
			GasLimit:   1_000_000,
			GasFeeCap:  big.NewInt(100),
			GasPremium: big.NewInt(1),
			Method:     0,
		}
	}
	a, err := Apply(context.Background(), nil, mk(big.Zero()), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply zero-value: %v", err)
	}
	b, err := Apply(context.Background(), nil, mk(big.NewInt(1_000_000)), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply with value: %v", err)
	}
	pl := V15PriceList()
	// b should be larger than a by at least OnMethodInvocationValue
	// (the value-xfer surcharge) plus a tiny bit of CBOR overhead from
	// the bigger Value field. We allow up to 200 extra bytes of slack.
	delta := b.Receipt.GasUsed - a.Receipt.GasUsed
	if delta < pl.OnMethodInvocationValue || delta > pl.OnMethodInvocationValue+200*pl.OnChainMessageStoragePerByte {
		t.Errorf("value-xfer gas delta out of range: want ~%d, got %d",
			pl.OnMethodInvocationValue, delta)
	}
}

// TestMaxGasCost sanity-checks the helper.
func TestMaxGasCost(t *testing.T) {
	msg := &types.Message{
		GasLimit:  1000,
		GasFeeCap: big.NewInt(50),
	}
	got := MaxGasCost(msg)
	want := big.NewInt(50_000)
	if !got.Equals(want) {
		t.Errorf("MaxGasCost: want %s, got %s", want, got)
	}
	// Nil-or-zero handling.
	z := MaxGasCost(nil)
	if !z.Equals(big.Zero()) {
		t.Errorf("MaxGasCost(nil) should be zero, got %s", z)
	}
	z = MaxGasCost(&types.Message{})
	if !z.Equals(big.Zero()) {
		t.Errorf("MaxGasCost(empty) should be zero, got %s", z)
	}
}

// TestApplyResult_TypePropagation ensures we surface MethodInfo for
// builtin actor calls (when we can resolve them).
func TestApplyResult_Send_NoMethodInfo(t *testing.T) {
	msg := &types.Message{
		From:       mustIDAddr(100),
		To:         mustIDAddr(101),
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(1),
		Method:     0, // Send
		Value:      big.Zero(),
	}
	r, _ := Apply(context.Background(), nil, msg, ApplyOptions{})
	if r.MethodInfo != nil {
		t.Errorf("Send should not have MethodInfo, got %+v", r.MethodInfo)
	}
	if exitcode.ExitCode(r.Receipt.ExitCode) != 0 {
		t.Errorf("Send should succeed, got exit %d", r.Receipt.ExitCode)
	}
}
