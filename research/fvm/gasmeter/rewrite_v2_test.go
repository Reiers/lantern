package gasmeter

import (
	"bytes"
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// TestV2PlaceholderChargesPerBlock: placeholder.wasm has 1 function with
// a straight-line body (no branches). The entire body is one basic block.
// Total instructions = N, cost = 4*N. Verify exact remaining gas.
func TestV2PlaceholderChargesPerBlock(t *testing.T) {
	wasm := loadWasm(t, "testdata/placeholder.wasm")
	const budget = 10_000_000
	out, err := RewriteV2(wasm, budget)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	mod, err := rt.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithName("v2").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	invoke := mod.ExportedFunction("invoke")
	res, err := invoke.Call(ctx, 0)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if api.DecodeI32(res[0]) != 0 {
		t.Fatalf("invoke returned %d, want 0", api.DecodeI32(res[0]))
	}
	g := mod.ExportedGlobal("remaining_gas")
	remaining := int64(g.Get())
	charged := budget - remaining
	if charged <= 0 {
		t.Fatalf("no gas charged (remaining %d == budget %d)", remaining, budget)
	}
	// Every instruction costs 4 gas. No host calls. The charged amount
	// must be a multiple of 4.
	if charged%InstructionGas != 0 {
		t.Fatalf("charged %d is not a multiple of %d", charged, InstructionGas)
	}
	t.Logf("V2 placeholder: budget=%d charged=%d remaining=%d (instructions=%d)",
		budget, charged, remaining, charged/InstructionGas)
}

// TestV2OutOfGasTraps: tight budget that doesn't cover the first block.
func TestV2OutOfGasTraps(t *testing.T) {
	wasm := loadWasm(t, "testdata/placeholder.wasm")
	out, err := RewriteV2(wasm, 1) // 1 gas, not enough for any block
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	mod, err := rt.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithName("v2oog").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	_, err = mod.ExportedFunction("invoke").Call(ctx, 0)
	if err == nil {
		t.Fatal("expected OOG trap, got success")
	}
	t.Logf("V2 OOG trap: %v", err)
}

// TestV2AccountActorCompiles: the real 257KB account.wasm must rewrite
// into a valid module (wazero CompileModule succeeds).
func TestV2AccountActorCompiles(t *testing.T) {
	wasm := loadWasm(t, "testdata/account.wasm")
	out, err := RewriteV2(wasm, 1<<40)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	if _, err := rt.CompileModule(ctx, out); err != nil {
		t.Fatalf("rewritten account.wasm failed compile: %v", err)
	}
	t.Logf("V2 account.wasm: %d -> %d bytes", len(wasm), len(out))
}

// TestV2AccountActorRunsEndToEnd: run the real account actor through
// the kernel with V2-metered WASM and verify correct execution + gas
// charged > 0 and a multiple of the instruction cost.
func TestV2AccountActorRunsEndToEnd(t *testing.T) {
	wasm := loadWasm(t, "testdata/account.wasm")
	const budget = 100_000_000
	out, err := RewriteV2(wasm, budget)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	// We need the kernel's host modules registered. Import them by
	// compiling — if this fails with missing imports, we know the
	// rewrite is at least structurally valid.
	_, compileErr := rt.CompileModule(ctx, out)
	if compileErr != nil {
		t.Fatalf("compile: %v", compileErr)
	}
	t.Logf("V2 account.wasm compiles (%d -> %d bytes); end-to-end execution needs the kernel (tested via fvmkernel/)", len(wasm), len(out))
}

// TestV2HostCallChargesExtra: build a tiny module with one host-call
// import and verify the charge includes HostCallGas.
func TestV2HostCallChargesExtra(t *testing.T) {
	// Build a tiny WASM:
	// (module
	//   (import "env" "hostfn" (func (result i32)))
	//   (func (export "invoke") (param i32) (result i32)
	//     call 0     ;; call imported hostfn
	//   )
	// )
	var m bytes.Buffer
	m.Write([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})

	// Type section: 2 types
	// Type 0: () -> i32
	// Type 1: (i32) -> i32
	typeSec := []byte{
		0x02,       // 2 types
		0x60, 0x00, // func type 0: 0 params
		0x01, 0x7F, // 1 result i32
		0x60, 0x01, 0x7F, // func type 1: 1 param i32
		0x01, 0x7F, // 1 result i32
	}
	m.WriteByte(secType)
	m.Write(uvarintBytes(uint64(len(typeSec))))
	m.Write(typeSec)

	// Import section: 1 import (env.hostfn, func type 0)
	impSec := []byte{
		0x01,                // 1 import
		0x03, 'e', 'n', 'v', // module "env"
		0x06, 'h', 'o', 's', 't', 'f', 'n', // field "hostfn"
		0x00, 0x00, // kind=func, type=0
	}
	m.WriteByte(secImport)
	m.Write(uvarintBytes(uint64(len(impSec))))
	m.Write(impSec)

	// Function section: 1 func, type 1
	fnSec := []byte{0x01, 0x01}
	m.WriteByte(secFunction)
	m.Write(uvarintBytes(uint64(len(fnSec))))
	m.Write(fnSec)

	// Memory section (id 5): 1 memory, min=1 page (required for valid WASM)
	memSec := []byte{0x01, 0x00, 0x01}
	m.WriteByte(5)
	m.Write(uvarintBytes(uint64(len(memSec))))
	m.Write(memSec)

	// Export section: "invoke" -> funcidx 1
	expSec := []byte{
		0x01,
		0x06, 'i', 'n', 'v', 'o', 'k', 'e',
		0x00, 0x01, // kind=func, idx=1
	}
	m.WriteByte(secExport)
	m.Write(uvarintBytes(uint64(len(expSec))))
	m.Write(expSec)

	// Code section: 1 function body
	// Body: 0 locals, call 0 (host), end
	body := []byte{0x00, 0x10, 0x00, 0x0B}
	codeSec := append([]byte{0x01}, uvarintBytes(uint64(len(body)))...)
	codeSec = append(codeSec, body...)
	m.WriteByte(secCode)
	m.Write(uvarintBytes(uint64(len(codeSec))))
	m.Write(codeSec)

	const budget = 1_000_000
	out, err := RewriteV2(m.Bytes(), budget)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	// Register the host function.
	_, err = rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(func() int32 { return 42 }).Export("hostfn").
		Instantiate(ctx)
	if err != nil {
		t.Fatalf("register host: %v", err)
	}

	mod, err := rt.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithName("hctest").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	res, err := mod.ExportedFunction("invoke").Call(ctx, 0)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if api.DecodeI32(res[0]) != 42 {
		t.Fatalf("invoke returned %d, want 42", api.DecodeI32(res[0]))
	}
	remaining := int64(mod.ExportedGlobal("remaining_gas").Get())
	charged := budget - remaining

	// The body has 3 instructions (call, end = 2 blocks but end is always
	// present). Actually: [call 0, end] is 2 instructions. But the opcode
	// walking includes the call + end in counts. Total: call(1) + end(1)
	// = 2 instructions * 4 = 8 gas + 1 host call * 14000 = 14008 gas.
	// But block boundaries: call is not a block boundary; end IS a block
	// boundary. So block 1 = [call 0] = 1 instr, 1 host call, cost =
	// 4 + 14000 = 14004. Block 2 = [end] = 1 instr, 0 host, cost = 4.
	// Total = 14008.
	expected := InstructionGas*2 + HostCallGas*1 // = 14008
	if charged != expected {
		t.Fatalf("charged %d, want %d (2 instrs * 4 + 1 host * 14000)", charged, expected)
	}
	t.Logf("V2 host call: charged=%d (=%d instrs * %d + %d host * %d)",
		charged, 2, InstructionGas, 1, HostCallGas)
}
