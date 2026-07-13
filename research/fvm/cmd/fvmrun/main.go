// cmd/fvmrun: end-to-end demonstration of the pure-Go FVM kernel
// (lantern#89 Stage C1). Runs the real v17 `account` actor's
// PubkeyAddress method (method 2) against a hand-constructed state tree,
// entirely in pure-Go wazero, and verifies the stored address round-trips
// back through the return value.
//
// Usage: fvmrun <account.wasm>
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"

	fvm "github.com/Reiers/lantern/research/fvm/fvmkernel"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fvmrun <account.wasm>")
		os.Exit(2)
	}
	wasm, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read wasm: %v\n", err)
		os.Exit(1)
	}
	if err := runPubkeyAddress(wasm); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func runPubkeyAddress(wasm []byte) error {
	ctx := context.Background()

	// --- 1. Construct a secp256k1 address (protocol 0x01 + 20-byte payload). ---
	payload := make([]byte, 20)
	for i := range payload {
		payload[i] = byte(0xA0 + i)
	}
	addrRaw := append([]byte{0x01}, payload...) // 21 bytes: [protocol][payload]

	// --- 2. Build the account State block: Serialize_tuple{address}. ---
	//   CBOR: array(1) [ byte-string(21) ]
	//   0x81 (array,1) | 0x55 (bytes,len=21) | <21 bytes>
	stateCBOR := buildStateCBOR(addrRaw)
	fmt.Printf("state block (%d bytes): %s\n", len(stateCBOR), hex.EncodeToString(stateCBOR))

	// --- 3. Put the state block in the blockstore, set it as the root. ---
	bs := fvm.NewMemBlockstore()
	k := fvm.NewKernel(bs)
	stateCID, err := k.PutStateBlock(stateCBOR)
	if err != nil {
		return fmt.Errorf("put state: %w", err)
	}
	fmt.Printf("state CID: %s\n", stateCID)
	k.SetStateRoot(stateCID)

	// --- 4. Message context: method 2 (PubkeyAddress), any caller. ---
	k.SetMessageContext(fvm.MessageContext{
		Origin:       100,
		Caller:       100, // accept_any: value irrelevant
		Receiver:     101,
		MethodNumber: 2, // PubkeyAddress
	})
	k.SetNetworkContext(fvm.NetworkContext{Epoch: 100, NetworkVersion: 27, ChainID: 314})
	k.SetDebug(false)

	// Method 2 (PubkeyAddress) is below FIRST_EXPORTED_METHOD_NUMBER, so
	// the actor_dispatch! macro runs restrict_internal_api: it looks up
	// the caller's code CID + builtin type and rejects unknown/EVM callers.
	// Register the caller (id 100) as a builtin System actor so the check
	// passes. In Lantern this comes from the real state tree.
	k.SetActor(100, fvm.SyntheticCode(fvm.TypeSystem), fvm.TypeSystem)

	// --- 5. Execute (no params: method 2 takes none). ---
	res, err := fvm.Execute(ctx, wasm, k, 0, nil)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	fmt.Printf("\n=== invocation result ===\n")
	fmt.Printf("exit code:    %d\n", res.ExitCode)
	fmt.Printf("return codec: 0x%x\n", res.ReturnCodec)
	fmt.Printf("return data (%d bytes): %s\n", len(res.ReturnData), hex.EncodeToString(res.ReturnData))
	if res.TrapErr != nil {
		fmt.Printf("trap: %v\n", res.TrapErr)
	}
	fmt.Printf("syscalls: %v\n", res.Syscalls)
	if len(res.Logs) > 0 {
		fmt.Printf("actor logs: %v\n", res.Logs)
	}

	// --- 6. Verify. ---
	if res.TrapErr != nil {
		return fmt.Errorf("actor trapped without clean exit: %w", res.TrapErr)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("actor exited with non-zero code %d (data=%s)", res.ExitCode, hex.EncodeToString(res.ReturnData))
	}
	// The PubkeyAddressReturn embeds the same 21-byte address. Verify it
	// round-tripped (lenient on the CBOR wrapper: transparent tuple may or
	// may not add the array header).
	if !bytes.Contains(res.ReturnData, addrRaw) {
		return fmt.Errorf("returned data does not contain the stored address %s", hex.EncodeToString(addrRaw))
	}
	fmt.Printf("\n✅ PASS: account.PubkeyAddress returned the stored address %s\n", hex.EncodeToString(addrRaw))
	fmt.Printf("   Pure-Go wazero FVM executed a real Filecoin v17 builtin actor end-to-end.\n")
	return nil
}

// buildStateCBOR emits the account State as fvm_ipld_encoding tuple CBOR:
// array(1)[ bytes(addrRaw) ].
func buildStateCBOR(addrRaw []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(0x81) // array of 1
	writeCBORBytes(&b, addrRaw)
	return b.Bytes()
}

// writeCBORBytes writes a CBOR byte string (major type 2).
func writeCBORBytes(b *bytes.Buffer, data []byte) {
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
