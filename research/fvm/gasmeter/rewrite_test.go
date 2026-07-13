package gasmeter

import (
	"context"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// loadWasm reads a wasm file, skipping the test if it's absent.
func loadWasm(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("wasm %s not present: %v", path, err)
	}
	return b
}

// TestRewriteRunsAndCharges: instrument placeholder.wasm (1 function),
// run invoke, confirm it still returns 0 AND that the remaining_gas
// global dropped by exactly the per-function cost.
func TestRewriteRunsAndCharges(t *testing.T) {
	wasm := loadWasm(t, "testdata/placeholder.wasm")
	const budget = 1_000_000
	const cost = 1000
	out, err := Rewrite(wasm, budget, cost)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	mod, err := rt.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithName("gt").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate rewritten module: %v", err)
	}
	invoke := mod.ExportedFunction("invoke")
	if invoke == nil {
		t.Fatal("no invoke export after rewrite")
	}
	res, err := invoke.Call(ctx, 0)
	if err != nil {
		t.Fatalf("invoke after rewrite: %v", err)
	}
	if len(res) != 1 || api.DecodeI32(res[0]) != 0 {
		t.Fatalf("invoke returned %v, want [0]", res)
	}
	g := mod.ExportedGlobal("remaining_gas")
	if g == nil {
		t.Fatal("remaining_gas global not exported")
	}
	remaining := int64(g.Get())
	if remaining != budget-cost {
		t.Fatalf("remaining gas = %d, want %d (budget %d - cost %d)", remaining, budget-cost, budget, cost)
	}
	t.Logf("gas metering works: budget %d, charged %d, remaining %d", budget, cost, remaining)
}

// TestRewriteOutOfGasTraps: with a budget smaller than the per-function
// cost, invoke must trap (unreachable = out of gas).
func TestRewriteOutOfGasTraps(t *testing.T) {
	wasm := loadWasm(t, "testdata/placeholder.wasm")
	const budget = 100
	const cost = 1000 // > budget => OOG on first charge
	out, err := Rewrite(wasm, budget, cost)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	mod, err := rt.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithName("gt").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	invoke := mod.ExportedFunction("invoke")
	_, err = invoke.Call(ctx, 0)
	if err == nil {
		t.Fatal("expected out-of-gas trap, got success")
	}
	t.Logf("out-of-gas correctly trapped: %v", err)
}

// TestRewriteBigActorInstantiates: the rewriter must produce a still-valid
// module for a large real actor (storageminer, 28 imports). We can't run
// its invoke without the full kernel, but instantiation exercises the
// binary validity of every rewritten function body.
func TestRewriteBigActorInstantiates(t *testing.T) {
	wasm := loadWasm(t, "testdata/account.wasm")
	out, err := Rewrite(wasm, 1<<40, 100)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)
	// Compile is enough to validate the rewritten binary (all bodies,
	// sizes, global/export sections). Instantiation needs the host
	// imports, which we don't wire here.
	if _, err := rt.CompileModule(ctx, out); err != nil {
		t.Fatalf("rewritten account.wasm failed to compile (invalid binary): %v", err)
	}
	t.Logf("rewritten account.wasm (%d bytes -> %d) compiles clean", len(wasm), len(out))
}
