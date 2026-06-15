package handlers

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
)

// recordingBridge records the method names it was asked to forward, so we
// can assert when the local path served vs fell back.
type recordingBridge struct {
	fakeBridge
	methods []string
	reply   map[string]json.RawMessage
}

func (r *recordingBridge) RawJSONRPC(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	r.methods = append(r.methods, method)
	if v, ok := r.reply[method]; ok {
		return v, nil
	}
	// default: a plausible hex quantity
	return json.RawMessage(`"0x2a"`), nil
}

// --- Stage 1: eth_getTransactionCount -------------------------------------

// With no accessor wired, the local path can't serve and we must fall
// back to the bridge (proves the fallback contract during rollout).
func TestEthGetTransactionCount_FallsBackWithoutAccessor(t *testing.T) {
	c := newCAPI() // no Accessor
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_getTransactionCount": json.RawMessage(`"0x7"`),
	}}
	c.Bridge = rb

	out, err := c.EthGetTransactionCount(context.Background(),
		"0x09a0fdc2723fad1a7b8e3e00ee5df73841df55a0", "latest")
	if err != nil {
		t.Fatalf("EthGetTransactionCount: %v", err)
	}
	if out != "0x7" {
		t.Fatalf("expected bridge value 0x7, got %s", out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_getTransactionCount" {
		t.Fatalf("expected one bridge forward, got %v", rb.methods)
	}
}

// A malformed address can't be parsed locally; with no bridge we must
// return errBridgeUnconfigured (not panic, not a bogus value).
func TestEthGetTransactionCount_NoBridgeNoLocal(t *testing.T) {
	c := newCAPI() // no Accessor, no Bridge
	_, err := c.EthGetTransactionCount(context.Background(), "0xnot-hex", "latest")
	if err == nil {
		t.Fatal("expected error with no local path and no bridge")
	}
}

// blockParamWantsPending classification.
func TestBlockParamWantsPending(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{"pending", true},
		{"latest", false},
		{"0x10", false},
		{nil, false},
		{42, false},
	}
	for _, tc := range cases {
		if got := blockParamWantsPending(tc.in); got != tc.want {
			t.Fatalf("blockParamWantsPending(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- Stage 2: eth_estimateGas ---------------------------------------------

// ethCallToMessage must always produce a DEFINED From (else
// Message.ChainLength() panics during estimation). Even with no `from`
// in the call object, From defaults to the target.
func TestEthCallToMessage_FromAlwaysDefined(t *testing.T) {
	c := newCAPI()
	msg, ok := c.ethCallToMessage(ethCallObject{
		To:   "0x09a0fdc2723fad1a7b8e3e00ee5df73841df55a0",
		Data: "0xabcdef",
	})
	if !ok {
		t.Fatal("expected translation to succeed")
	}
	if msg.From.Empty() {
		t.Fatal("From must be defined to avoid ChainLength panic")
	}
	if msg.To.Empty() {
		t.Fatal("To must be defined")
	}
	// Must not panic — this is the real guard we care about.
	_ = msg.ChainLength()
	// Contract-invoke method so the estimator picks the high ceiling.
	if msg.Method == 0 {
		t.Fatal("expected a contract-invoke method, got 0 (would under-estimate)")
	}
}

// A bad To address can't translate; estimate must fall back to bridge.
func TestEthEstimateGas_FallsBackOnBadCall(t *testing.T) {
	c := newCAPI()
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_estimateGas": json.RawMessage(`"0x5208"`),
	}}
	c.Bridge = rb
	// callObj with no `to` -> can't build message -> bridge.
	out, err := c.EthEstimateGas(context.Background(), map[string]any{"data": "0x00"})
	if err != nil {
		t.Fatalf("EthEstimateGas: %v", err)
	}
	if out != "0x5208" {
		t.Fatalf("expected bridge value, got %s", out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_estimateGas" {
		t.Fatalf("expected bridge fallback, got %v", rb.methods)
	}
}

// When the local estimator serves, the result must be a conservative
// (large, contract-invoke ceiling) over-estimate, never a tiny value
// that would starve a real SP tx. We assert it's well above a bare Send.
func TestEthEstimateGas_LocalIsConservative(t *testing.T) {
	c := newCAPI()
	// No bridge: if local doesn't serve, this errors and the test fails,
	// which is the signal we want (local MUST serve a well-formed call).
	out, err := c.EthEstimateGas(context.Background(), map[string]any{
		"to":   "0x09a0fdc2723fad1a7b8e3e00ee5df73841df55a0",
		"from": "0xb3042734b608a1b16e9e86b374a3f3e389b4cdf0",
		"data": "0x1234",
	})
	if err != nil {
		t.Fatalf("local estimate should serve a well-formed call: %v", err)
	}
	got := new(big.Int)
	if _, ok := got.SetString(out[2:], 16); !ok {
		t.Fatalf("estimate not hex: %s", out)
	}
	// Contract invoke -> 75M base ceiling + margins. Must be >> a 1M send.
	if got.Cmp(big.NewInt(10_000_000)) < 0 {
		t.Fatalf("estimate %s suspiciously low for a contract call (would risk under-gas)", got)
	}
}
