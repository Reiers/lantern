package fvmkernel

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ipfs/go-cid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func le32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

// Result is the outcome of a single actor invocation.
type Result struct {
	ExitCode     uint32
	ReturnCodec  uint64
	ReturnData   []byte
	NewStateRoot cid.Cid
	Syscalls     map[string]int
	Logs         []string
	// TrapErr is set when the WASM trapped for a reason OTHER than a
	// clean vm.exit (i.e. a real fault: unreachable without exit, OOB
	// memory, bad hostcall return). Nil on success or clean exit.
	TrapErr error
}

// Execute runs one method invocation of an actor. The kernel must already
// have its stateRoot + msgCtx (+ optional netCtx, sendFn) configured.
//
// paramsCodec/params is the invocation parameter block; pass nil params
// for a no-arg method (the FVM passes NO_DATA_BLOCK_ID=0 to invoke).
func Execute(ctx context.Context, actorWasm []byte, k *Kernel, paramsCodec uint64, params []byte) (Result, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	if err := k.Register(ctx, rt); err != nil {
		return Result{}, fmt.Errorf("register host modules: %w", err)
	}

	cm, err := rt.CompileModule(ctx, actorWasm)
	if err != nil {
		return Result{}, fmt.Errorf("compile actor: %w", err)
	}
	mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName("__actor_under_test").WithStartFunctions())
	if err != nil {
		return Result{}, fmt.Errorf("instantiate actor: %w", err)
	}
	defer mod.Close(ctx)

	// Load params as a block (id passed to invoke), or 0 if none.
	var paramsID uint32 = noDataBlockID
	if len(params) > 0 {
		paramsID = k.putBlock(paramsCodec, params)
	}

	invoke := mod.ExportedFunction("invoke")
	if invoke == nil {
		return Result{}, fmt.Errorf("actor has no exported invoke function")
	}

	res := Result{Syscalls: k.syscalls}
	out, callErr := invoke.Call(ctx, uint64(paramsID))
	// Snapshot log + state-root regardless of outcome.
	res.Logs = k.logs
	res.NewStateRoot = k.stateRoot

	if callErr != nil {
		// A clean vm.exit records k.exited and then the actor's Rust
		// marks unreachable (exit is `-> !`), which surfaces here as a
		// trap. Treat that as the actor's real exit outcome.
		if k.exited {
			res.ExitCode = k.exitCode
			res.ReturnData = k.exitData
			return res, nil
		}
		res.TrapErr = callErr
		return res, nil
	}

	// Normal return: invoke returned the return block id (0 = no data).
	if len(out) == 1 {
		retID := api.DecodeU32(out[0])
		if retID != noDataBlockID {
			if be, ok := k.blocks[retID]; ok {
				res.ReturnCodec = be.codec
				res.ReturnData = be.data
			}
		}
	}
	res.ExitCode = 0
	return res, nil
}

// Configure helpers -----------------------------------------------------

func (k *Kernel) SetStateRoot(c cid.Cid)              { k.stateRoot = c }
func (k *Kernel) SetMessageContext(mc MessageContext) { k.msgCtx = mc }
func (k *Kernel) SetNetworkContext(nc NetworkContext) { k.netCtx = nc }
func (k *Kernel) SetDebug(on bool)                    { k.debugEnabled = on }
func (k *Kernel) SetSendFn(fn SendFunc)               { k.sendFn = fn }

// PutStateBlock is a convenience: store a raw state block in the
// blockstore and return its (dag-cbor, blake2b-256) CID.
func (k *Kernel) PutStateBlock(data []byte) (cid.Cid, error) {
	c, err := cidOfBlock(codecDagCBOR, data)
	if err != nil {
		return cid.Undef, err
	}
	k.bs.Put(c, data)
	return c, nil
}
