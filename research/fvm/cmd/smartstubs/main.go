// research/fvm/smart_stubs.go: Step 3 of lantern#89 spike.
//
// Push past the naive-stub "unreachable" trap by returning plausible
// hostcall values: FVM_OK=0 for status-return hostcalls, small non-zero
// handles for block_open/block_create so actors that inspect them
// don't panic. Also unblocks the `debug.log` sink so we can see actor
// log lines routed via the log ABI.
//
// Goal: show that we can run the actor path far enough to see it emit
// real debug output or hit a specific hostcall it depends on, proving
// that the wazero execution loop + host-import surface is fully wired.
// This isn't semantically correct FVM execution — that requires the
// full port — but it proves the wazero half is not the wall.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// FVM syscall status codes we mirror in our stubs.
const (
	fvmOK              uint32 = 0
	fvmIllegalArgument uint32 = 1
	fvmNotFound        uint32 = 3
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smart_stubs <actor-name>")
		fmt.Fprintln(os.Stderr, "  (actor-name matches wasm/<name>.wasm)")
		os.Exit(2)
	}
	target := os.Args[1]

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	wasmBytes, err := os.ReadFile(filepath.Join("wasm", target+".wasm"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read wasm: %v\n", err)
		os.Exit(1)
	}
	cm, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: %v\n", err)
		os.Exit(1)
	}
	defer cm.Close(ctx)

	// Collect this actor's imports and register smart stubs for them.
	type key struct{ mod, name string }
	imports := map[key]struct {
		params  []api.ValueType
		results []api.ValueType
	}{}
	for _, imp := range cm.ImportedFunctions() {
		mod, name, _ := imp.Import()
		imports[key{mod, name}] = struct {
			params  []api.ValueType
			results []api.ValueType
		}{
			imp.ParamTypes(), imp.ResultTypes(),
		}
	}

	// Track stub call statistics
	stats := map[string]int{}
	// A simple in-memory "block handle -> bytes" registry so
	// block_create + block_read + block_stat can talk to each other.
	blockHandles := map[uint32][]byte{}
	nextHandle := uint32(1)

	byMod := map[string][]key{}
	for k := range imports {
		byMod[k.mod] = append(byMod[k.mod], k)
	}

	for mod, keys := range byMod {
		hb := rt.NewHostModuleBuilder(mod)
		for _, k := range keys {
			sig := imports[k]
			modName, fnName := k.mod, k.name
			resultLen := len(sig.results)

			// Special-case a handful of hostcalls with smart behavior.
			handler := api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
				stats[modName+"."+fnName]++

				switch modName + "." + fnName {
				case "debug.enabled":
					// Return 1 to enable debug logging.
					if resultLen > 0 {
						stack[len(stack)-1] = 1
					}
					return
				case "debug.log":
					// Params: (ptr:i32, len:i32) into linear memory.
					if len(stack) >= 2 {
						ptr := uint32(stack[0])
						sz := uint32(stack[1])
						if mem := m.Memory(); mem != nil {
							if b, ok := mem.Read(ptr, sz); ok {
								fmt.Printf("[actor:%s log] %s\n", target, string(b))
							}
						}
					}
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(fvmOK)
					}
					return
				case "vm.message_context":
					// Fill the caller's out-buffer at ptr with a plausible
					// message context struct. We don't know the exact
					// layout without ref-fvm sources; leave zeros for now
					// and return OK.
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(fvmOK)
					}
					return
				case "gas.available":
					// Return a huge gas budget so nothing gets OOG-killed.
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(1 << 60)
					}
					return
				case "ipld.block_create":
					// (codec:i64, data_off:i32, data_len:i32, ret_id:i32) -> i32
					// Read the payload into our fake blockstore, return handle.
					if len(stack) >= 4 {
						dataOff := uint32(stack[1])
						dataLen := uint32(stack[2])
						retIDPtr := uint32(stack[3])
						buf := make([]byte, dataLen)
						if mem := m.Memory(); mem != nil {
							if b, ok := mem.Read(dataOff, dataLen); ok {
								copy(buf, b)
							}
						}
						h := nextHandle
						nextHandle++
						blockHandles[h] = buf
						if mem := m.Memory(); mem != nil {
							var out [4]byte
							binary.LittleEndian.PutUint32(out[:], h)
							_ = mem.Write(retIDPtr, out[:])
						}
					}
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(fvmOK)
					}
					return
				case "vm.exit":
					// Actor is telling us to exit. Params: (exit_code:i32, ...).
					if len(stack) >= 1 {
						fmt.Printf("[actor:%s exit] exit_code=%d\n", target, uint32(stack[0]))
					}
					// vm.exit is expected to be a trap in the actor. We
					// can't actually trap here so we just return; the
					// actor will pop back and (usually) hit unreachable
					// anyway. That's fine for the spike.
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(fvmOK)
					}
					return
				default:
					// Fallback: return OK.
					if resultLen > 0 {
						stack[len(stack)-1] = uint64(fvmOK)
					}
				}
			})

			hb = hb.NewFunctionBuilder().
				WithGoModuleFunction(handler, sig.params, sig.results).
				Export(fnName)
		}
		if _, err := hb.Instantiate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "stub module %s: %v\n", mod, err)
			os.Exit(1)
		}
	}

	mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(target))
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate: %v\n", err)
		os.Exit(1)
	}
	defer mod.Close(ctx)

	invoke := mod.ExportedFunction("invoke")
	if invoke == nil {
		fmt.Fprintf(os.Stderr, "no invoke export\n")
		os.Exit(1)
	}
	out, err := invoke.Call(ctx, 0) // method 0 = Constructor for most actors
	if err != nil {
		fmt.Printf("[actor:%s invoke(0) ERR] %v\n", target, err)
	} else {
		fmt.Printf("[actor:%s invoke(0) RESULT] %v\n", target, out)
	}

	// Dump stats
	fmt.Printf("\nHostcalls made by invoke(0) on %s:\n", target)
	var lines []string
	for k, v := range stats {
		lines = append(lines, fmt.Sprintf("  %-40s  %d", k, v))
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
	_ = strings.Repeat // keep import
}
