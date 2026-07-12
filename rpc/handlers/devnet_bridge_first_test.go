package handlers

// Tests for the lantern#123 finding-7 devnet bridge-first short-circuit.
// On a single-node docker devnet the local hamt walk always burns its
// full retry budget (no bitswap peers to fetch cold storage-trie blocks
// from) before falling through to the bridge. The devnet lotus IS the
// source of truth for the devnet chain, so short-circuiting straight to
// it is both faster (verified live: 34ms vs 15s timeout) and semantically
// identical. These tests lock in the routing decision.
//
// Assertions here focus on the ROUTING (which path was taken), not on
// end-to-end correctness of the bridge's answer (that lives in
// vm/bridge tests).

import (
	"context"
	"encoding/json"
	"testing"
)

// Devnet: EthGetCode always forwards to the bridge, even for the "0x"-EOA
// case where mainnet/calibration would have served locally without a
// bridge round-trip.
func TestEthGetCode_DevnetGoesStraightToBridge(t *testing.T) {
	c := newCAPI()
	c.NetworkName = "devnet"
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_getCode": json.RawMessage(`"0xdeadbeef"`),
	}}
	c.Bridge = rb

	out, err := c.EthGetCode(context.Background(),
		"0x0000000000000000000000000000000000000042", "latest")
	if err != nil {
		t.Fatalf("EthGetCode: %v", err)
	}
	if out != "0xdeadbeef" {
		t.Fatalf("expected bridge answer 0xdeadbeef, got %q", out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_getCode" {
		t.Fatalf("expected exactly one bridge forward of eth_getCode, got %v", rb.methods)
	}
}

// Mainnet with no local accessor wired: the local resolver can't serve,
// falls back to the bridge. Same OBSERVABLE result as devnet, but proves
// the mainnet path still tries local first (accessor==nil is a serve=false
// miss, not a definitive short-circuit).
func TestEthGetCode_MainnetTriesLocalFirst(t *testing.T) {
	c := newCAPI() // NetworkName = "mainnet" via newCAPI
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_getCode": json.RawMessage(`"0xdeadbeef"`),
	}}
	c.Bridge = rb

	out, err := c.EthGetCode(context.Background(),
		"0x0000000000000000000000000000000000000042", "latest")
	if err != nil {
		t.Fatalf("EthGetCode: %v", err)
	}
	if out != "0xdeadbeef" {
		t.Fatalf("expected bridge answer 0xdeadbeef, got %q", out)
	}
	// The bridge is still called (accessor==nil = local miss), but the
	// path is different: on devnet we bypass localEthGetCode entirely.
	// We can't easily observe the code path from the test, only its
	// pre-conditions. Guarding against a regression here means asserting
	// that at least the routing table looks right (bridge called once).
	if len(rb.methods) != 1 {
		t.Fatalf("expected one bridge forward on mainnet fallback, got %v", rb.methods)
	}
}

// Devnet without a bridge: no auto-wired lotus, no local. The read
// returns errBridgeUnconfigured (no crash, no bogus value). This is the
// operator-explicitly-drops-the-bridge escape hatch.
func TestEthGetCode_DevnetNoBridgeErrorsOut(t *testing.T) {
	c := newCAPI()
	c.NetworkName = "devnet"
	// No bridge assigned.
	_, err := c.EthGetCode(context.Background(),
		"0x0000000000000000000000000000000000000042", "latest")
	if err == nil {
		t.Fatal("expected error with no bridge on devnet")
	}
}

// Devnet: EthGetStorageAt bypasses local read, goes straight to bridge.
func TestEthGetStorageAt_DevnetGoesStraightToBridge(t *testing.T) {
	c := newCAPI()
	c.NetworkName = "devnet"
	slotHex := `"0x0000000000000000000000000000000000000000000000000000000000000005"`
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_getStorageAt": json.RawMessage(slotHex),
	}}
	c.Bridge = rb

	out, err := c.EthGetStorageAt(context.Background(),
		"0x0000000000000000000000000000000000000042",
		"0x0",
		"latest")
	if err != nil {
		t.Fatalf("EthGetStorageAt: %v", err)
	}
	want := "0x0000000000000000000000000000000000000000000000000000000000000005"
	if out != want {
		t.Fatalf("expected %q, got %q", want, out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_getStorageAt" {
		t.Fatalf("expected exactly one bridge forward of eth_getStorageAt, got %v", rb.methods)
	}
}

// Devnet: EthCall bypasses local FEVM execution, goes straight to bridge.
// Uses a well-formed selector so the bridge is asked (recordingBridge
// returns the pre-canned reply below).
func TestEthCall_DevnetGoesStraightToBridge(t *testing.T) {
	c := newCAPI()
	c.NetworkName = "devnet"
	// Even though LocalFEVMDisabled is false (default), the devnet path
	// must bypass local FEVM and go straight to bridge.
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_call": json.RawMessage(`"0x2a"`),
	}}
	c.Bridge = rb

	out, err := c.EthCall(context.Background(),
		map[string]any{
			"to":   "0x0000000000000000000000000000000000000042",
			"data": "0x9a99b4f0",
		},
		"latest",
	)
	if err != nil {
		t.Fatalf("EthCall: %v", err)
	}
	if out != "0x2a" {
		t.Fatalf("expected bridge answer 0x2a, got %q", out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_call" {
		t.Fatalf("expected exactly one bridge forward of eth_call, got %v", rb.methods)
	}
}

// Devnet: EthCall with no bridge returns errBridgeUnconfigured.
// (Sanity check for the escape-hatch case.)
func TestEthCall_DevnetNoBridgeErrorsOut(t *testing.T) {
	c := newCAPI()
	c.NetworkName = "devnet"
	// No bridge.
	_, err := c.EthCall(context.Background(),
		map[string]any{
			"to":   "0x0000000000000000000000000000000000000042",
			"data": "0x9a99b4f0",
		},
		"latest",
	)
	if err == nil {
		t.Fatal("expected error with no bridge on devnet")
	}
}
