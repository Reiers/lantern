package evm

import (
	"encoding/hex"
	"testing"

	"github.com/holiman/uint256"
)

// nullBackend is an offline backend with no code/storage, for testing the
// interpreter's pure-computation opcodes.
type nullBackend struct{ storage map[string]uint256.Int }

func (n *nullBackend) GetCode(Address) ([]byte, error) { return nil, nil }
func (n *nullBackend) GetStorage(_ Address, k uint256.Int) (uint256.Int, error) {
	if n.storage == nil {
		return uint256.Int{}, nil
	}
	return n.storage[k.Hex()], nil
}
func (n *nullBackend) GetBalance(Address) (uint256.Int, error) { return uint256.Int{}, nil }
func (n *nullBackend) BlockNumber() uint64                     { return 100 }
func (n *nullBackend) Timestamp() uint64                       { return 200 }
func (n *nullBackend) ChainID() uint64                         { return 314159 }

// runCode runs raw bytecode with empty input and returns the result.
func runCode(t *testing.T, codeHex string, be Backend) *Result {
	t.Helper()
	code, err := hex.DecodeString(codeHex)
	if err != nil {
		t.Fatalf("bad code hex: %v", err)
	}
	ip := &interpreter{
		b: be, ov: newOverlay(), code: code, stack: newStack(), mem: &memory{},
		jumpdest: analyzeJumpdests(code),
	}
	res, err := ip.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestInterp_AddReturn(t *testing.T) {
	// PUSH1 0x02, PUSH1 0x03, ADD, PUSH1 0x00, MSTORE, PUSH1 0x20, PUSH1 0x00, RETURN
	// => returns 32-byte word == 5.
	code := "6002600301" + "600052" + "60206000f3"
	res := runCode(t, code, &nullBackend{})
	if res.Reverted {
		t.Fatal("unexpected revert")
	}
	want := make([]byte, 32)
	want[31] = 5
	if hex.EncodeToString(res.Return) != hex.EncodeToString(want) {
		t.Errorf("ADD: got %x, want %x", res.Return, want)
	}
}

func TestInterp_Revert(t *testing.T) {
	// PUSH1 0x00, PUSH1 0x00, REVERT
	res := runCode(t, "60006000fd", &nullBackend{})
	if !res.Reverted {
		t.Error("expected revert")
	}
}

func TestInterp_SloadReturn(t *testing.T) {
	// PUSH1 0x00, SLOAD, PUSH1 0x00, MSTORE, PUSH1 0x20, PUSH1 0x00, RETURN
	// storage[0] = 0x2a (42)
	be := &nullBackend{storage: map[string]uint256.Int{}}
	be.storage[uint256.NewInt(0).Hex()] = *uint256.NewInt(0x2a)
	res := runCode(t, "600054"+"600052"+"60206000f3", be)
	if res.Reverted {
		t.Fatal("unexpected revert")
	}
	got := new(uint256.Int).SetBytes(res.Return)
	if got.Uint64() != 0x2a {
		t.Errorf("SLOAD: got %d, want 42", got.Uint64())
	}
}

func TestInterp_Sha3(t *testing.T) {
	// keccak256 of zero-length data == c5d2460186f7233c927e7db2dcc703c0
	// e500b653ca82273b7bfad8045d85a470.
	// PUSH1 0x00, PUSH1 0x00, SHA3, PUSH1 0x00, MSTORE, PUSH1 0x20, PUSH1 0x00, RETURN
	res := runCode(t, "6000600020"+"600052"+"60206000f3", &nullBackend{})
	if res.Reverted {
		t.Fatal("unexpected revert")
	}
	want := "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	if hex.EncodeToString(res.Return) != want {
		t.Errorf("SHA3(empty): got %x, want %s", res.Return, want)
	}
}

func TestInterp_SstoreThenSloadOverlay(t *testing.T) {
	// Write 42 to slot 0, then read it back and return it. Previously this
	// reverted the instant SSTORE was hit; now the overlay holds the write
	// and SLOAD sees it. This is the core of the DEX-swap transferFrom fix.
	// PUSH1 0x2a, PUSH1 0x00, SSTORE  (store 42 at slot 0)
	// PUSH1 0x00, SLOAD               (load slot 0)
	// PUSH1 0x00, MSTORE, PUSH1 0x20, PUSH1 0x00, RETURN
	code := "602a600055" + "600054" + "600052" + "60206000f3"
	res := runCode(t, code, &nullBackend{})
	if res.Reverted {
		t.Fatal("SSTORE/SLOAD round-trip reverted; overlay not applied")
	}
	got := new(uint256.Int).SetBytes(res.Return)
	if got.Uint64() != 0x2a {
		t.Errorf("overlay SLOAD: got %d, want 42", got.Uint64())
	}
}

func TestInterp_OverlayShadowsBackend(t *testing.T) {
	// Backend says slot 0 = 7; an in-call SSTORE overwrites it to 99.
	// SLOAD must observe the overlay value (99), not the backend (7).
	be := &nullBackend{storage: map[string]uint256.Int{}}
	be.storage[uint256.NewInt(0).Hex()] = *uint256.NewInt(7)
	// PUSH1 0x63 (99), PUSH1 0x00, SSTORE, PUSH1 0x00, SLOAD, store+return
	code := "6063600055" + "600054" + "600052" + "60206000f3"
	res := runCode(t, code, be)
	if res.Reverted {
		t.Fatal("unexpected revert")
	}
	got := new(uint256.Int).SetBytes(res.Return)
	if got.Uint64() != 99 {
		t.Errorf("overlay should shadow backend: got %d, want 99", got.Uint64())
	}
}

func TestInterp_LogIsNoOp(t *testing.T) {
	// LOG1: PUSH topic, PUSH size, PUSH offset, LOG1, then return 1.
	// Must not revert and must keep the stack balanced.
	// PUSH1 0x00 (topic), PUSH1 0x00 (size), PUSH1 0x00 (offset), LOG1
	// PUSH1 0x01, PUSH1 0x00, MSTORE, PUSH1 0x20, PUSH1 0x00, RETURN
	code := "600060006000a1" + "6001600052" + "60206000f3"
	res := runCode(t, code, &nullBackend{})
	if res.Reverted {
		t.Fatal("LOG1 should be a no-op, not a revert")
	}
	got := new(uint256.Int).SetBytes(res.Return)
	if got.Uint64() != 1 {
		t.Errorf("post-LOG return: got %d, want 1", got.Uint64())
	}
}

func TestInterp_UnsupportedOpcodeErrors(t *testing.T) {
	// 0x0c is undefined -> error (not a silent success).
	code, _ := hex.DecodeString("0c")
	ip := &interpreter{b: &nullBackend{}, code: code, stack: newStack(), mem: &memory{}, jumpdest: map[uint64]bool{}}
	if _, err := ip.run(); err == nil {
		t.Error("expected error on undefined opcode")
	}
}
