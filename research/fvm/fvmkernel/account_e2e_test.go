package fvmkernel

// End-to-end tests: run the real v17 `account` builtin actor under the
// pure-Go wazero FVM kernel (lantern#89 Stage C1). These exercise the
// full read path (PubkeyAddress) and write path (Constructor) against a
// real IPLD blockstore, with byte-exact syscall ABI.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/hex"
	"testing"
)

//go:embed testdata/account.wasm
var accountWasm []byte

// buildAddr returns a deterministic secp256k1 address (protocol 0x01 + 20 bytes).
func buildAddr() []byte {
	payload := make([]byte, 20)
	for i := range payload {
		payload[i] = byte(0xA0 + i)
	}
	return append([]byte{0x01}, payload...)
}

// account State CBOR: Serialize_tuple{address} = array(1)[ bytes(addr) ].
func accountStateCBOR(addrRaw []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(0x81)
	writeCBORByteString(&b, addrRaw)
	return b.Bytes()
}

// ConstructorParams / PubkeyAddressReturn are #[serde(transparent)]
// single-field tuples: they serialize as a bare CBOR byte string.
func transparentAddrCBOR(addrRaw []byte) []byte {
	var b bytes.Buffer
	writeCBORByteString(&b, addrRaw)
	return b.Bytes()
}

func writeCBORByteString(b *bytes.Buffer, data []byte) {
	n := len(data)
	switch {
	case n < 24:
		b.WriteByte(byte(0x40 | n))
	case n < 256:
		b.WriteByte(0x58)
		b.WriteByte(byte(n))
	default:
		b.WriteByte(0x59)
		b.WriteByte(byte(n >> 8))
		b.WriteByte(byte(n))
	}
	b.Write(data)
}

// TestAccountPubkeyAddress: read path. Pre-seed the account state, invoke
// method 2, verify the stored address comes back.
func TestAccountPubkeyAddress(t *testing.T) {
	ctx := context.Background()
	addrRaw := buildAddr()

	bs := NewMemBlockstore()
	k := NewKernel(bs)
	stateCID, err := k.PutStateBlock(accountStateCBOR(addrRaw))
	if err != nil {
		t.Fatalf("put state: %v", err)
	}
	k.SetStateRoot(stateCID)
	k.SetMessageContext(MessageContext{Origin: 100, Caller: 100, Receiver: 101, MethodNumber: 2})
	k.SetNetworkContext(NetworkContext{Epoch: 100, NetworkVersion: 27, ChainID: 314})
	// Method 2 < FIRST_EXPORTED: restrict_internal_api checks caller type.
	k.SetActor(100, SyntheticCode(TypeSystem), TypeSystem)

	res, err := Execute(ctx, accountWasm, k, 0, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.TrapErr != nil {
		t.Fatalf("actor trapped: %v", res.TrapErr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0", res.ExitCode)
	}
	if !bytes.Contains(res.ReturnData, addrRaw) {
		t.Fatalf("return %s does not contain address %s",
			hex.EncodeToString(res.ReturnData), hex.EncodeToString(addrRaw))
	}
	// Verify the exact expected syscalls fired.
	for _, want := range []string{"vm.message_context", "self.root", "ipld.block_open", "ipld.block_read", "ipld.block_create", "actor.get_actor_code_cid", "actor.get_builtin_actor_type"} {
		if res.Syscalls[want] == 0 {
			t.Errorf("expected syscall %q to fire, counts=%v", want, res.Syscalls)
		}
	}
	t.Logf("PubkeyAddress OK: returned %s via syscalls %v",
		hex.EncodeToString(res.ReturnData), res.Syscalls)
}

// TestAccountConstructor: write path. Start with an empty actor
// (state root = EMPTY_ARR_CID), invoke Constructor(method 1) with an
// address param, verify the new state root equals the hand-computed
// account State CID (proves set_root + put_cbor produced byte-identical
// state).
func TestAccountConstructor(t *testing.T) {
	ctx := context.Background()
	addrRaw := buildAddr()

	bs := NewMemBlockstore()
	k := NewKernel(bs)

	// Empty actor: root = CID of empty CBOR array (0x80). rt.create()
	// requires this exact sentinel before it will write.
	emptyArrCID, err := k.PutStateBlock([]byte{0x80})
	if err != nil {
		t.Fatalf("put empty arr: %v", err)
	}
	k.SetStateRoot(emptyArrCID)

	// Constructor (method 1) must be called by the system actor (id 0).
	k.SetMessageContext(MessageContext{Origin: 0, Caller: 0, Receiver: 101, MethodNumber: 1})
	k.SetNetworkContext(NetworkContext{Epoch: 100, NetworkVersion: 27, ChainID: 314})
	k.SetActor(0, SyntheticCode(TypeSystem), TypeSystem)

	// ConstructorParams{address} is transparent -> bare CBOR byte string.
	params := transparentAddrCBOR(addrRaw)

	res, err := Execute(ctx, accountWasm, k, 0x51, params)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.TrapErr != nil {
		t.Fatalf("actor trapped: %v", res.TrapErr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0 (data=%s)", res.ExitCode, hex.EncodeToString(res.ReturnData))
	}

	// The new state root must equal the CID of the account State CBOR.
	wantState := accountStateCBOR(addrRaw)
	wantCID, err := cidOfBlock(codecDagCBOR, wantState)
	if err != nil {
		t.Fatalf("cid of state: %v", err)
	}
	if !res.NewStateRoot.Equals(wantCID) {
		t.Fatalf("new state root %s, want %s (state write mismatch)", res.NewStateRoot, wantCID)
	}
	// And the actor must have written that block into the store.
	if _, ok := bs.Get(wantCID); !ok {
		t.Fatalf("state block %s not persisted to blockstore", wantCID)
	}
	if res.Syscalls["self.set_root"] == 0 {
		t.Fatalf("expected self.set_root to fire, counts=%v", res.Syscalls)
	}
	t.Logf("Constructor OK: wrote state %s via syscalls %v", res.NewStateRoot, res.Syscalls)
}
