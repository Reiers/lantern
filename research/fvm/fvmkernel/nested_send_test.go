package fvmkernel

// Nested-from-WASM send test (lantern#129, Stage C3 final proof).
//
// The other Machine tests drive Send from Go. This test proves the full
// loop: an ACTOR running under wazero calls the `send.send` syscall,
// hits our Kernel's sendSend, forwards through the Machine's callback,
// spawns a nested frame that runs another actor under wazero, and the
// target's return data flows back through the send-out struct into the
// caller's WASM, which returns it to Machine.Send.
//
// The caller actor is a hand-emitted synthetic WASM (see
// testactor_caller.go); the target is the real v17 account actor.

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/tetratelabs/wazero"
)

// TestCallerWASMValidates: the emitter's output must instantiate cleanly
// under wazero. Isolates the WASM builder from the rest of the test.
func TestCallerWASMValidates(t *testing.T) {
	target := buildRobustAddr(0xAA)
	wasm := BuildCallerWASM(target.Bytes(), 2, TokenAmount{})
	if len(wasm) < 32 {
		t.Fatalf("emitted wasm too short: %d bytes", len(wasm))
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	if _, err := rt.CompileModule(ctx, wasm); err != nil {
		t.Fatalf("compile: %v (bytes=%s)", err, hex.EncodeToString(wasm))
	}
	t.Logf("caller WASM ok (%d bytes)", len(wasm))
}

// TestNestedSendFromWASM: put a real account actor at a resolvable
// address, seed its state with a known address, then drive the caller
// actor which will internally invoke `send.send` with method=2
// (PubkeyAddress). Verify the parent Send() receives the account's
// return data (the stored address).
func TestNestedSendFromWASM(t *testing.T) {
	ctx := context.Background()

	// State tree setup.
	bs := NewMemBlockstore()
	tree := NewStateTree(bs)

	// (a) The target: a real account actor with a pre-populated state.
	accCode := makeAccountCodeCID(t)
	tree.RegisterBuiltin(accCode, TypeAccount, accountWasm)

	targetRobust := buildRobustAddr(0xE1)
	targetAddrRaw := targetRobust.Bytes()
	stateCBOR := accountStateCBOR(targetAddrRaw)
	targetStateCID, err := cidOfBlock(codecDagCBOR, stateCBOR)
	if err != nil {
		t.Fatalf("target state cid: %v", err)
	}
	bs.Put(targetStateCID, stateCBOR)
	targetID := tree.AssignID(targetRobust)
	tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: targetStateCID,
	})

	// (b) The caller: our hand-emitted WASM. Type = Multisig so
	// restrict_internal_api in the target account allows the call
	// (any non-EVM builtin type passes; multisig is the canonical
	// real-world example of an actor that calls send).
	callerWasm := BuildCallerWASM(targetAddrRaw, 2 /* PubkeyAddress */, TokenAmount{})
	callerCode, err := cidOfBlock(codecRaw, []byte("test:caller-actor-v1"))
	if err != nil {
		t.Fatalf("caller code cid: %v", err)
	}
	tree.RegisterBuiltin(callerCode, TypeMultisig, callerWasm)

	callerRobust := buildRobustAddr(0xE0)
	callerID := tree.AssignID(callerRobust)
	// Caller needs a state root (empty CBOR array works — the caller
	// doesn't touch state).
	emptyArrCID, err := cidOfBlock(codecDagCBOR, []byte{0x80})
	if err != nil {
		t.Fatalf("empty arr cid: %v", err)
	}
	bs.Put(emptyArrCID, []byte{0x80})
	tree.SetActor(callerID, ActorState{
		CodeCID:   callerCode,
		StateRoot: emptyArrCID,
	})

	// (c) The origin: system actor at id 0.
	sysCode := SyntheticCode(TypeSystem)
	tree.RegisterBuiltin(sysCode, TypeSystem, accountWasm /* unused */)
	tree.SetActor(0, ActorState{
		CodeCID:   sysCode,
		StateRoot: emptyArrCID,
		Balance:   TokenAmount{Lo: 1_000_000},
	})

	// Machine.
	m := NewMachine(tree, NetworkContext{Epoch: 100, NetworkVersion: 27, ChainID: 314})

	// Drive: system -> caller. The caller ignores the method number
	// (its invoke just calls send unconditionally). Value = 0.
	res, err := m.Send(ctx, 0, callerRobust, 42 /* dummy method */, 0, nil, TokenAmount{}, SendOpts{})
	if err != nil {
		t.Fatalf("top-level Send: %v", err)
	}
	if res.TrapErr != nil {
		t.Fatalf("caller trapped: %v (syscalls=%v)", res.TrapErr, res.Syscalls)
	}
	if res.ExitCode != 0 {
		t.Fatalf("caller exit %d (data=%s)", res.ExitCode, hex.EncodeToString(res.ReturnData))
	}

	// The caller returned the target's return_id; that block id was
	// registered in the caller's block registry with the target's
	// return data. Execute() resolves that block and puts it in
	// res.ReturnData. It must contain the target's stored address.
	if !bytes.Contains(res.ReturnData, targetAddrRaw) {
		t.Fatalf("top-level return %s does not contain target address %s (nested send did not propagate)",
			hex.EncodeToString(res.ReturnData), hex.EncodeToString(targetAddrRaw))
	}

	// And send.send must have fired in the caller's syscall counts.
	if res.Syscalls["send.send"] != 1 {
		t.Fatalf("expected send.send to fire exactly once, got %v", res.Syscalls)
	}

	t.Logf("Nested send OK: caller %d -> target %d, propagated %d bytes (%s)",
		callerID, targetID, len(res.ReturnData), hex.EncodeToString(res.ReturnData))
}
