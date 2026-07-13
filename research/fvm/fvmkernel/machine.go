package fvmkernel

// Machine: pure-Go FVM call-manager (lantern#129, Stage C3).
//
// The Machine owns the shared StateTree and drives message execution.
// Every Send call creates a new invocation Frame (a Kernel) for the
// recipient actor, wires its send-syscall back to Machine.Send for
// nested recursion, and commits or rolls back the frame's state
// mutations based on the actor's exit code.
//
// This is the piece the #89 spike scaffolded but did not implement:
// real address resolution (init HAMT semantics via StateTree.AssignID),
// nested Execute re-entry, value transfer atomic with method success,
// and transactional rollback on non-zero exit.

import (
	"context"

	"github.com/ipfs/go-cid"
)

// SendResult mirrors the receive side of an invocation, plus per-frame
// observability (syscall counts, logs) so tests can inspect nested runs.
type SendResult struct {
	ExitCode    uint32
	ReturnCodec uint64
	ReturnData  []byte
	NewRoot     cid.Cid // recipient's new state root (== old root on abort)
	Syscalls    map[string]int
	Logs        []string
	// TrapErr is set when the WASM trapped for a non-exit reason (real
	// fault). The frame rolls back and the parent gets ExitCode = 0xFFFFFFFF.
	TrapErr error
}

// Machine drives message execution over a shared StateTree.
type Machine struct {
	tree   *StateTree
	netCtx NetworkContext
	// Depth tracks nested-call depth for a rough re-entry guard. Real
	// ref-fvm has MAX_CALL_DEPTH = 4096; we use the same cap.
	depth    int
	maxDepth int
}

// NewMachine builds a Machine bound to a state tree + network context.
func NewMachine(tree *StateTree, nc NetworkContext) *Machine {
	return &Machine{tree: tree, netCtx: nc, maxDepth: 4096}
}

// State returns the underlying state tree (for harness / test access).
func (m *Machine) State() *StateTree { return m.tree }

// SendOpts controls per-call behaviour.
type SendOpts struct {
	// ReadOnly forbids value transfer and state mutation in this call
	// tree (Filecoin's read-only send flag).
	ReadOnly bool
	// Nonce is set on the top-level message; ignored for nested sends.
	Nonce uint64
}

// Send executes a message from `from` to `toAddr` with method+params+value.
// It is the entry point for both top-level messages and nested sends.
//
// Success (ExitCode=0): the recipient's state root is committed to the
// state tree, and value is transferred.
//
// Non-zero exit or trap: the state tree is rolled back to its pre-call
// snapshot; value transfer is undone.
func (m *Machine) Send(ctx context.Context, from uint64, toAddr Address, method uint64, paramsCodec uint64, params []byte, value TokenAmount, opts SendOpts) (SendResult, error) {
	if m.depth >= m.maxDepth {
		return SendResult{ExitCode: errLimitExceeded}, nil
	}
	m.depth++
	defer func() { m.depth-- }()

	// 1. Resolve recipient. A robust address that hasn't been seen gets
	// a fresh id (init.Exec semantics). At this prototype level we do
	// NOT auto-create the actor state — the harness has to have placed
	// the recipient in the tree beforehand (that's what init.Exec would
	// do in a full implementation). This mirrors "the target must exist
	// or we're calling init.Exec".
	toID, ok := m.tree.LookupID(toAddr)
	if !ok {
		toID = m.tree.AssignID(toAddr)
	}

	// 2. Snapshot before any state changes so we can roll back cleanly.
	snap := m.tree.Snapshot()

	// 3. Value transfer (atomic with method success). Read-only sends
	// cannot transfer value.
	if !value.IsZero() {
		if opts.ReadOnly {
			m.tree.Restore(snap)
			return SendResult{ExitCode: errForbidden}, nil
		}
		if !m.tree.Transfer(from, toID, value) {
			m.tree.Restore(snap)
			return SendResult{ExitCode: errInsufficientFund}, nil
		}
	}

	// 4. Method 0 = bare value transfer, no actor code executed. This
	// matches Filecoin's protocol semantics.
	if method == 0 {
		return SendResult{ExitCode: 0}, nil
	}

	// 5. Load recipient's actor state + WASM.
	toActor, ok := m.tree.GetActor(toID)
	if !ok {
		m.tree.Restore(snap)
		return SendResult{ExitCode: errNotFound}, nil
	}
	wasm, ok := m.tree.ActorWASM(toID)
	if !ok {
		m.tree.Restore(snap)
		return SendResult{ExitCode: errNotFound}, nil
	}

	// 6. Build the recipient's frame (a fresh Kernel with per-invocation
	// blocks, gas, and exit state, but sharing the StateTree's block-
	// store + actor registry via SetActor).
	frame := NewKernel(m.tree.bs)
	frame.SetStateRoot(toActor.StateRoot)
	frame.SetMessageContext(MessageContext{
		Origin:          from, // (top-level would carry the real origin; nested inherit)
		Caller:          from,
		Receiver:        toID,
		MethodNumber:    method,
		ValueReceivedHi: value.Hi,
		ValueReceivedLo: value.Lo,
		Nonce:           opts.Nonce,
	})
	frame.SetNetworkContext(m.netCtx)

	// Populate the actor registry the frame needs for
	// restrict_internal_api caller-type checks.
	for id, as := range m.tree.actors {
		bt := m.tree.builtinTypes[as.CodeCID.KeyString()]
		frame.SetActor(id, as.CodeCID, bt)
	}

	// Wire the frame's send-syscall back into this Machine so a nested
	// call from within the actor re-enters Send. Depth is tracked here
	// on Machine, not on the frame.
	frame.SetSendFn(func(toBytes []byte, nMethod uint64, nParams []byte, nValue TokenAmount) (uint32, uint64, []byte, error) {
		nAddr, err := ParseAddress(toBytes)
		if err != nil {
			return 0, 0, nil, err
		}
		// Preserve read-only across nested calls: once a frame is
		// read-only, everything it sends must be read-only too.
		nRes, nErr := m.Send(ctx, toID, nAddr, nMethod, 0x51, nParams, nValue, SendOpts{ReadOnly: opts.ReadOnly})
		if nErr != nil {
			return 0, 0, nil, nErr
		}
		return nRes.ExitCode, nRes.ReturnCodec, nRes.ReturnData, nil
	})

	// 7. Execute. The recipient's `invoke(paramsID)` runs to completion.
	res, err := Execute(ctx, wasm, frame, paramsCodec, params)
	if err != nil {
		m.tree.Restore(snap)
		return SendResult{TrapErr: err}, err
	}
	if res.TrapErr != nil {
		m.tree.Restore(snap)
		return SendResult{TrapErr: res.TrapErr, Syscalls: res.Syscalls, Logs: res.Logs}, nil
	}
	if res.ExitCode != 0 {
		// Abort: roll back state + value transfer.
		m.tree.Restore(snap)
		return SendResult{
			ExitCode:    res.ExitCode,
			ReturnCodec: res.ReturnCodec,
			ReturnData:  res.ReturnData,
			NewRoot:     toActor.StateRoot, // unchanged
			Syscalls:    res.Syscalls,
			Logs:        res.Logs,
		}, nil
	}

	// 8. Commit: persist the frame's new state root back to the actor.
	m.tree.SetStateRoot(toID, res.NewStateRoot)

	return SendResult{
		ExitCode:    0,
		ReturnCodec: res.ReturnCodec,
		ReturnData:  res.ReturnData,
		NewRoot:     res.NewStateRoot,
		Syscalls:    res.Syscalls,
		Logs:        res.Logs,
	}, nil
}
