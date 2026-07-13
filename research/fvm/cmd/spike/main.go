// research/fvm/wazero_spike.go: Step 2 of the lantern#89 spike (v2).
//
// Load each extracted builtin-actor WASM under wazero, list the module's
// imports + exports, and attempt to instantiate it with stub hostcalls.
//
// v2 changes over v1:
//   - Build a UNION host-module set once (all imports across all actors),
//     using each import's ACTUAL i32/i64 signature (v1 hardcoded i64 which
//     mismatched every actor).
//   - One shared runtime, so registered stub modules aren't re-registered.
//   - Report each actor's instantiation + invoke(0) outcome.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

type importSpec struct {
	Module    string          `json:"module"`
	Name      string          `json:"name"`
	Params    []api.ValueType `json:"-"`
	Results   []api.ValueType `json:"-"`
	ParamStr  string          `json:"params"`
	ResultStr string          `json:"results"`
}

func (i importSpec) key() string { return i.Module + "." + i.Name }

type actorReport struct {
	Actor          string       `json:"actor"`
	Size           int          `json:"size"`
	ImportCount    int          `json:"import_count"`
	ImportModules  []string     `json:"import_modules"`
	Imports        []importSpec `json:"imports"`
	ExportCount    int          `json:"export_count"`
	Exports        []string     `json:"exports"`
	Instantiated   bool         `json:"instantiated"`
	InstantiateErr string       `json:"instantiate_err,omitempty"`
	InvokeResult   string       `json:"invoke_result,omitempty"`
	InvokeErr      string       `json:"invoke_err,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "err: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	wasmDir := "wasm"
	entries, err := os.ReadDir(wasmDir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", wasmDir, err)
	}
	// Sort by size ascending so the trivial actors go first.
	type entryInfo struct {
		name string
		size int64
	}
	var files []entryInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wasm") {
			continue
		}
		fi, _ := e.Info()
		files = append(files, entryInfo{name: e.Name(), size: fi.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size < files[j].size })

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer rt.Close(ctx)

	// Pass 1: compile every module, collect the union of imports (module, name, signature).
	type key struct{ mod, name string }
	unionImports := map[key]importSpec{}

	compiled := map[string]wazero.CompiledModule{}
	reports := map[string]*actorReport{}

	for _, f := range files {
		name := strings.TrimSuffix(f.name, ".wasm")
		wasmBytes, err := os.ReadFile(filepath.Join(wasmDir, f.name))
		if err != nil {
			return err
		}
		cm, err := rt.CompileModule(ctx, wasmBytes)
		if err != nil {
			reports[name] = &actorReport{Actor: name, Size: int(f.size), InstantiateErr: fmt.Sprintf("compile: %v", err)}
			continue
		}
		compiled[name] = cm
		r := &actorReport{Actor: name, Size: int(f.size)}
		seenMods := map[string]bool{}
		for _, imp := range cm.ImportedFunctions() {
			modName, impName, _ := imp.Import()
			spec := importSpec{
				Module:    modName,
				Name:      impName,
				Params:    imp.ParamTypes(),
				Results:   imp.ResultTypes(),
				ParamStr:  valueTypesToStr(imp.ParamTypes()),
				ResultStr: valueTypesToStr(imp.ResultTypes()),
			}
			r.Imports = append(r.Imports, spec)
			k := key{modName, impName}
			if existing, ok := unionImports[k]; ok {
				if !sameSig(existing, spec) {
					r.InstantiateErr = fmt.Sprintf("import %s.%s: signature mismatch with prior actor (this actor: %s->%s, previously: %s->%s)",
						modName, impName, spec.ParamStr, spec.ResultStr,
						existing.ParamStr, existing.ResultStr)
				}
			} else {
				unionImports[k] = spec
			}
			seenMods[modName] = true
		}
		for m := range seenMods {
			r.ImportModules = append(r.ImportModules, m)
		}
		sort.Strings(r.ImportModules)
		r.ImportCount = len(r.Imports)
		for _, exp := range cm.ExportedFunctions() {
			r.Exports = append(r.Exports, exp.Name())
		}
		sort.Strings(r.Exports)
		r.ExportCount = len(r.Exports)
		reports[name] = r
	}

	// Pass 2: build one host module per unique module name, with all
	// its imports as stubs returning zero. Correct signatures this time.
	byMod := map[string][]importSpec{}
	for _, spec := range unionImports {
		byMod[spec.Module] = append(byMod[spec.Module], spec)
	}
	for modName, funcs := range byMod {
		hb := rt.NewHostModuleBuilder(modName)
		for _, fn := range funcs {
			// Capture signature by value so the closure sees the right one.
			resultLen := len(fn.Results)
			hb = hb.NewFunctionBuilder().
				WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
					// Zero the return slots (last resultLen entries in stack).
					if resultLen > 0 {
						for i := len(stack) - resultLen; i < len(stack); i++ {
							stack[i] = 0
						}
					}
				}), fn.Params, fn.Results).
				Export(fn.Name)
		}
		if _, err := hb.Instantiate(ctx); err != nil {
			return fmt.Errorf("build stub host module %s: %w", modName, err)
		}
	}

	// Pass 3: instantiate each actor + try invoke(0).
	for _, f := range files {
		name := strings.TrimSuffix(f.name, ".wasm")
		cm, ok := compiled[name]
		if !ok {
			continue
		}
		r := reports[name]
		if r.InstantiateErr != "" {
			continue
		}
		mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(name))
		if err != nil {
			r.InstantiateErr = fmt.Sprintf("instantiate: %v", err)
			continue
		}
		r.Instantiated = true
		invoke := mod.ExportedFunction("invoke")
		if invoke == nil {
			r.InvokeErr = "no exported invoke"
			mod.Close(ctx)
			continue
		}
		// invoke(method uint64) returning uint32.
		out, err := invoke.Call(ctx, 0)
		if err != nil {
			r.InvokeErr = fmt.Sprintf("%v", err)
		} else {
			r.InvokeResult = fmt.Sprintf("%v", out)
		}
		mod.Close(ctx)
	}

	// Print summary
	fmt.Printf("\n%-16s  %-7s  %-8s  %-8s  %-12s  %s\n",
		"ACTOR", "SIZE", "IMPORTS", "EXPORTS", "INSTANTIATED", "invoke(0)")
	fmt.Printf("%s\n", strings.Repeat("-", 100))
	var orderedNames []string
	for _, f := range files {
		orderedNames = append(orderedNames, strings.TrimSuffix(f.name, ".wasm"))
	}
	for _, name := range orderedNames {
		r, ok := reports[name]
		if !ok {
			continue
		}
		inst := "no"
		if r.Instantiated {
			inst = "YES"
		}
		inv := r.InvokeResult
		if inv == "" {
			inv = fmt.Sprintf("ERR: %s", truncErr(r.InvokeErr, 60))
		}
		if !r.Instantiated {
			inv = fmt.Sprintf("(not inst) %s", truncErr(r.InstantiateErr, 60))
		}
		fmt.Printf("%-16s  %7d  %-8d  %-8d  %-12s  %s\n",
			r.Actor, r.Size, r.ImportCount, r.ExportCount, inst, inv)
	}

	// Global hostcall surface
	fmt.Println("\nUnion hostcall surface (module.name -> signature, ports needed):")
	var keys []key
	for k := range unionImports {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].mod != keys[j].mod {
			return keys[i].mod < keys[j].mod
		}
		return keys[i].name < keys[j].name
	})
	curMod := ""
	for _, k := range keys {
		if k.mod != curMod {
			curMod = k.mod
			fmt.Printf("  module %q\n", curMod)
		}
		spec := unionImports[k]
		fmt.Printf("    %-40s  (%s) -> (%s)\n", spec.Name, spec.ParamStr, spec.ResultStr)
	}
	fmt.Printf("\nTotal unique hostcalls to port: %d (across %d modules)\n",
		len(unionImports), len(byMod))

	if f, err := os.Create("report.json"); err == nil {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		var out []actorReport
		for _, name := range orderedNames {
			if r, ok := reports[name]; ok {
				out = append(out, *r)
			}
		}
		_ = enc.Encode(out)
		f.Close()
	}
	return nil
}

func valueTypesToStr(v []api.ValueType) string {
	if len(v) == 0 {
		return ""
	}
	parts := make([]string, len(v))
	for i, t := range v {
		switch t {
		case api.ValueTypeI32:
			parts[i] = "i32"
		case api.ValueTypeI64:
			parts[i] = "i64"
		case api.ValueTypeF32:
			parts[i] = "f32"
		case api.ValueTypeF64:
			parts[i] = "f64"
		default:
			parts[i] = fmt.Sprintf("t%d", t)
		}
	}
	return strings.Join(parts, ",")
}

func sameSig(a, b importSpec) bool {
	if len(a.Params) != len(b.Params) || len(a.Results) != len(b.Results) {
		return false
	}
	for i := range a.Params {
		if a.Params[i] != b.Params[i] {
			return false
		}
	}
	for i := range a.Results {
		if a.Results[i] != b.Results[i] {
			return false
		}
	}
	return true
}

func truncErr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
