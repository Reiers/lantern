package fvmkernel

// Machine end-to-end tests (lantern#129, Stage C3 Milestone 1).
//
// These exercise the shared StateTree + nested Frame machinery against
// the real v17 account actor. What they prove:
//
//   1. Address resolution: a robust f1 address gets assigned an id via
//      the StateTree address map (init.Exec semantics), and repeat calls
//      to the same address resolve to the SAME id.
//   2. Frame semantics: a fresh Kernel is built per invocation, executes
//      under wazero, and its state-root delta is committed back to the
//      state tree on exit 0.
//   3. Value transfer: send with value moves TokenAmount from sender to
//      recipient atomically with method success.
//   4. Rollback: a non-zero exit causes both state root AND value
//      transfer to roll back to the pre-call snapshot.
//   5. Read-only: a read-only send with value>0 is forbidden.

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/ipfs/go-cid"
)

// buildRobustAddr returns a deterministic f1 (secp256k1) robust address.
func buildRobustAddr(seed byte) Address {
	payload := make([]byte, 20)
	for i := range payload {
		payload[i] = seed + byte(i)
	}
	return Address{Protocol: 1, Payload: payload}
}

// makeAccountCodeCID returns a synthetic-but-stable "account" code CID
// used to tag account actors in this test harness. The real network
// uses the bundle-provided CID; the runtime doesn't care which CID it
// is, only that it maps to a builtin type + WASM.
func makeAccountCodeCID(t *testing.T) cid.Cid {
	t.Helper()
	c, err := cidOfBlock(codecRaw, []byte("test:account-code-v17"))
	if err != nil {
		t.Fatalf("code cid: %v", err)
	}
	return c
}

// setupMachine builds a StateTree with:
//   - the account WASM registered under a synthetic code CID (type=Account)
//   - a system actor at id 0 (also account-code so it can invoke Constructor)
//   - an empty-CBOR-array block for use as the initial state root
//
// It returns the Machine, the account code CID, and the empty-array CID.
func setupMachine(t *testing.T) (*Machine, cid.Cid, cid.Cid) {
	t.Helper()
	bs := NewMemBlockstore()
	tree := NewStateTree(bs)

	accCode := makeAccountCodeCID(t)
	tree.RegisterBuiltin(accCode, TypeAccount, accountWasm)

	// system actor at id 0 (needed to be a valid caller for method 1 =
	// Constructor, which restrict_internal_api gates to non-EVM builtins).
	sysCode := SyntheticCode(TypeSystem)
	// Register a WASM for the system code too, otherwise a call TO the
	// system actor would fail; but here we only need it as a *caller*,
	// so an empty entry is fine. Use accountWasm as a placeholder; it
	// never runs, since we only Send FROM id 0.
	tree.RegisterBuiltin(sysCode, TypeSystem, accountWasm)

	// Put the empty CBOR array (used as fresh-actor state root).
	emptyArrCID, err := putBlockToStore(bs, []byte{0x80})
	if err != nil {
		t.Fatalf("put empty arr: %v", err)
	}

	tree.SetActor(0, ActorState{
		CodeCID:   sysCode,
		StateRoot: emptyArrCID,
		Balance:   TokenAmount{Lo: 1_000_000},
	})

	nc := NetworkContext{Epoch: 100, NetworkVersion: 27, ChainID: 314}
	return NewMachine(tree, nc), accCode, emptyArrCID
}

// putBlockToStore puts a raw block under its dag-cbor CID.
func putBlockToStore(bs Blockstore, data []byte) (cid.Cid, error) {
	c, err := cidOfBlock(codecDagCBOR, data)
	if err != nil {
		return cid.Undef, err
	}
	bs.Put(c, data)
	return c, nil
}

// TestMachineSendConstructor: system (id 0) sends Constructor to a fresh
// account address. Proves address resolution + frame execution + state
// commit on the shared tree.
func TestMachineSendConstructor(t *testing.T) {
	ctx := context.Background()
	m, accCode, emptyArr := setupMachine(t)

	// Pre-place the target account as an existing (empty-state) actor.
	// Real Filecoin does this via init.Exec; here the harness plays init.
	targetRobust := buildRobustAddr(0xA0)
	targetID := m.tree.AssignID(targetRobust)
	m.tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: emptyArr,
	})

	// Constructor param = the address the account will store (secp256k1).
	addrRaw := targetRobust.Bytes()
	params := transparentAddrCBOR(addrRaw)

	res, err := m.Send(ctx, 0, targetRobust, 1 /* Constructor */, 0x51, params, TokenAmount{}, SendOpts{})
	if err != nil {
		t.Fatalf("machine.Send: %v", err)
	}
	if res.TrapErr != nil {
		t.Fatalf("frame trapped: %v", res.TrapErr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0 (data=%s)", res.ExitCode, hex.EncodeToString(res.ReturnData))
	}

	// The new state root must equal the canonical account State CID.
	wantState := accountStateCBOR(addrRaw)
	wantCID, err := cidOfBlock(codecDagCBOR, wantState)
	if err != nil {
		t.Fatalf("want cid: %v", err)
	}
	if !res.NewRoot.Equals(wantCID) {
		t.Fatalf("frame NewRoot %s, want %s", res.NewRoot, wantCID)
	}
	// And the StateTree's actor entry must be updated (commit path).
	got, ok := m.tree.GetActor(targetID)
	if !ok {
		t.Fatalf("actor %d missing after Send", targetID)
	}
	if !got.StateRoot.Equals(wantCID) {
		t.Fatalf("state tree actor %d root %s, want %s", targetID, got.StateRoot, wantCID)
	}

	// Repeat address resolution: same robust address -> same id.
	againID := m.tree.AssignID(targetRobust)
	if againID != targetID {
		t.Fatalf("re-resolve gave id %d, want %d (address map instability)", againID, targetID)
	}

	t.Logf("Constructor via Machine.Send OK: id=%d root=%s syscalls=%v",
		targetID, res.NewRoot, res.Syscalls)
}

// TestMachineSendPubkeyAddressRoundTrip: after Constructor commits, a
// separate PubkeyAddress send must be able to read the stored address.
// This proves: (a) state tree carries state across successive sends,
// and (b) frame reads observe the committed root.
func TestMachineSendPubkeyAddressRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, accCode, emptyArr := setupMachine(t)

	targetRobust := buildRobustAddr(0xB0)
	targetID := m.tree.AssignID(targetRobust)
	m.tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: emptyArr,
	})

	addrRaw := targetRobust.Bytes()

	// Step 1: Constructor.
	_, err := m.Send(ctx, 0, targetRobust, 1, 0x51, transparentAddrCBOR(addrRaw), TokenAmount{}, SendOpts{})
	if err != nil {
		t.Fatalf("constructor send: %v", err)
	}

	// Step 2: PubkeyAddress. Read-only would be fine here since it
	// mutates nothing; we run it as a normal send with value=0.
	res, err := m.Send(ctx, 0, targetRobust, 2 /* PubkeyAddress */, 0, nil, TokenAmount{}, SendOpts{})
	if err != nil {
		t.Fatalf("pubkey send: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("pubkey exit %d, data=%s", res.ExitCode, hex.EncodeToString(res.ReturnData))
	}
	if !bytes.Contains(res.ReturnData, addrRaw) {
		t.Fatalf("return %s does not contain address %s",
			hex.EncodeToString(res.ReturnData), hex.EncodeToString(addrRaw))
	}

	t.Logf("Round-trip OK: Constructor + PubkeyAddress via Machine.Send, id=%d", targetID)
}

// TestMachineValueTransfer: a send with value>0 debits the sender and
// credits the recipient atomically with method success.
func TestMachineValueTransfer(t *testing.T) {
	ctx := context.Background()
	m, accCode, emptyArr := setupMachine(t)

	// Recipient starts empty (id assigned, balance 0).
	targetRobust := buildRobustAddr(0xC0)
	targetID := m.tree.AssignID(targetRobust)
	m.tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: emptyArr,
	})

	// Bare value transfer (method 0). Sender = id 0 (balance 1_000_000).
	amount := TokenAmount{Lo: 250_000}
	res, err := m.Send(ctx, 0, targetRobust, 0 /* value only */, 0, nil, amount, SendOpts{})
	if err != nil {
		t.Fatalf("transfer send: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("transfer exit %d", res.ExitCode)
	}

	src, _ := m.tree.GetActor(0)
	dst, _ := m.tree.GetActor(targetID)
	if src.Balance.Lo != 750_000 {
		t.Fatalf("sender balance %v, want 750000", src.Balance)
	}
	if dst.Balance.Lo != 250_000 {
		t.Fatalf("recipient balance %v, want 250000", dst.Balance)
	}
	t.Logf("Value transfer OK: sender=%v recipient=%v", src.Balance, dst.Balance)
}

// TestMachineRollbackOnFailedMethod: if the actor exits non-zero, both
// state root AND value transfer must roll back to pre-call snapshot.
func TestMachineRollbackOnFailedMethod(t *testing.T) {
	ctx := context.Background()
	m, accCode, emptyArr := setupMachine(t)

	targetRobust := buildRobustAddr(0xD0)
	targetID := m.tree.AssignID(targetRobust)
	m.tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: emptyArr,
	})

	// Call Constructor with BAD params (empty). The actor decodes the
	// address param and will abort on serialization error -> non-zero
	// exit -> our Machine must roll back.
	amount := TokenAmount{Lo: 100_000}
	srcBefore, _ := m.tree.GetActor(0)
	dstBefore, _ := m.tree.GetActor(targetID)

	res, err := m.Send(ctx, 0, targetRobust, 1, 0x51, []byte{0x40 /* zero-length CBOR bytes */}, amount, SendOpts{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Any of: non-zero exit, or a trap. Either way state must be rolled back.
	if res.ExitCode == 0 && res.TrapErr == nil {
		t.Fatalf("expected abort, got clean exit 0 (data=%s)", hex.EncodeToString(res.ReturnData))
	}

	srcAfter, _ := m.tree.GetActor(0)
	dstAfter, _ := m.tree.GetActor(targetID)
	if srcAfter.Balance != srcBefore.Balance {
		t.Fatalf("sender balance changed after abort: %v -> %v", srcBefore.Balance, srcAfter.Balance)
	}
	if dstAfter.Balance != dstBefore.Balance {
		t.Fatalf("recipient balance changed after abort: %v -> %v", dstBefore.Balance, dstAfter.Balance)
	}
	if !dstAfter.StateRoot.Equals(dstBefore.StateRoot) {
		t.Fatalf("recipient state root changed after abort: %s -> %s", dstBefore.StateRoot, dstAfter.StateRoot)
	}
	t.Logf("Rollback OK: exit=%d trap=%v, balances + state root preserved", res.ExitCode, res.TrapErr)
}

// TestMachineReadOnlyForbidsValue: a read-only send with value>0 must
// be rejected outright without touching the state tree.
func TestMachineReadOnlyForbidsValue(t *testing.T) {
	ctx := context.Background()
	m, accCode, emptyArr := setupMachine(t)

	targetRobust := buildRobustAddr(0xE0)
	targetID := m.tree.AssignID(targetRobust)
	m.tree.SetActor(targetID, ActorState{
		CodeCID:   accCode,
		StateRoot: emptyArr,
	})

	srcBefore, _ := m.tree.GetActor(0)

	res, err := m.Send(ctx, 0, targetRobust, 0, 0, nil, TokenAmount{Lo: 1}, SendOpts{ReadOnly: true})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.ExitCode != errForbidden {
		t.Fatalf("expected errForbidden (%d), got %d", errForbidden, res.ExitCode)
	}
	srcAfter, _ := m.tree.GetActor(0)
	if srcAfter.Balance != srcBefore.Balance {
		t.Fatalf("balance moved during read-only value send: %v -> %v", srcBefore.Balance, srcAfter.Balance)
	}
	t.Logf("Read-only + value>0 correctly rejected (exit=%d)", res.ExitCode)
}
