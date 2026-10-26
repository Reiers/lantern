// Command genbundles regenerates state/actors/bundles.go from a Lotus
// release's build/builtin_actors_gen.go.
//
// Usage (from the repo root):
//
//	go run ./state/actors/internal/genbundles -lotus v1.38.3
//
// By default the Lotus source file is downloaded from
// raw.githubusercontent.com at the given tag. Pass -in to read a local copy
// instead (offline / air-gapped regeneration). Only the mainnet and
// calibrationnet entries are kept. The output is gofmt-formatted and
// deterministic, so re-running against the same tag is a no-op.
//
// Bump procedure for a new network upgrade:
//
//  1. go run ./state/actors/internal/genbundles -lotus <new tag>
//  2. add the new vN cases to the versioned loaders in state/actors,
//     vm/methods.go and rpc/handlers/state_readstate_decode.go
//  3. add the new upgrade heights to rpc/handlers/network_version.go
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

type bundle struct {
	Network  string
	Version  int
	Manifest string
	Actors   map[string]string
}

var keepNetworks = map[string]bool{"mainnet": true, "calibrationnet": true}

func main() {
	lotus := flag.String("lotus", "", "Lotus release tag the source file comes from (e.g. v1.38.3); required")
	in := flag.String("in", "", "local path to builtin_actors_gen.go (default: download from GitHub at -lotus)")
	out := flag.String("out", "state/actors/bundles.go", "output path")
	minVersion := flag.Int("min-version", 8, "lowest actors version to keep")
	flag.Parse()
	if *lotus == "" {
		fatalf("-lotus is required")
	}

	src, err := readSource(*in, *lotus)
	if err != nil {
		fatalf("read source: %v", err)
	}
	bundles, err := parse(src)
	if err != nil {
		fatalf("parse: %v", err)
	}
	var kept []bundle
	for _, b := range bundles {
		if keepNetworks[b.Network] && b.Version >= *minVersion {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		fatalf("no mainnet/calibrationnet bundles found")
	}
	code, err := render(*lotus, kept)
	if err != nil {
		fatalf("render: %v", err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		fatalf("write %s: %v", *out, err)
	}
	fmt.Fprintf(os.Stderr, "genbundles: wrote %d bundles from lotus@%s to %s\n", len(kept), *lotus, *out)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "genbundles: "+f+"\n", a...)
	os.Exit(1)
}

func readSource(path, tag string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	url := "https://raw.githubusercontent.com/filecoin-project/lotus/" + tag + "/build/builtin_actors_gen.go"
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// parse walks the EmbeddedBuiltinActorsMetadata composite literal.
func parse(src []byte) ([]bundle, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "builtin_actors_gen.go", src, 0)
	if err != nil {
		return nil, err
	}
	var list *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "EmbeddedBuiltinActorsMetadata" || len(vs.Values) != 1 {
			return true
		}
		list, _ = vs.Values[0].(*ast.CompositeLit)
		return false
	})
	if list == nil {
		return nil, fmt.Errorf("EmbeddedBuiltinActorsMetadata not found")
	}
	var out []bundle
	for i, elt := range list.Elts {
		if u, ok := elt.(*ast.UnaryExpr); ok {
			elt = u.X
		}
		cl, ok := elt.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("element %d: not a composite literal", i)
		}
		b := bundle{Actors: map[string]string{}}
		for _, fe := range cl.Elts {
			kv, ok := fe.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, _ := kv.Key.(*ast.Ident)
			if key == nil {
				continue
			}
			switch key.Name {
			case "Network":
				b.Network, err = strLit(kv.Value)
			case "Version":
				var s string
				s, err = basicLit(kv.Value, token.INT)
				if err == nil {
					b.Version, err = strconv.Atoi(s)
				}
			case "ManifestCid":
				b.Manifest, err = cidCall(kv.Value)
			case "Actors":
				m, ok := kv.Value.(*ast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("element %d: Actors is not a map literal", i)
				}
				for _, ae := range m.Elts {
					akv := ae.(*ast.KeyValueExpr)
					name, err := strLit(akv.Key)
					if err != nil {
						return nil, err
					}
					c, err := cidCall(akv.Value)
					if err != nil {
						return nil, err
					}
					b.Actors[name] = c
				}
			}
			if err != nil {
				return nil, fmt.Errorf("element %d field %s: %w", i, key.Name, err)
			}
		}
		if b.Network == "" || b.Version == 0 || b.Manifest == "" || len(b.Actors) == 0 {
			return nil, fmt.Errorf("element %d: incomplete entry %+v", i, b)
		}
		out = append(out, b)
	}
	return out, nil
}

func basicLit(e ast.Expr, kind token.Token) (string, error) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != kind {
		return "", fmt.Errorf("expected %s literal", kind)
	}
	return bl.Value, nil
}

func strLit(e ast.Expr) (string, error) {
	s, err := basicLit(e, token.STRING)
	if err != nil {
		return "", err
	}
	return strconv.Unquote(s)
}

// cidCall extracts the string argument of cid.MustParse("...").
func cidCall(e ast.Expr) (string, error) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", fmt.Errorf("expected cid.MustParse(\"...\")")
	}
	return strLit(call.Args[0])
}

func render(tag string, bundles []bundle) ([]byte, error) {
	minV, maxV := bundles[0].Version, bundles[0].Version
	for _, b := range bundles {
		if b.Version < minV {
			minV = b.Version
		}
		if b.Version > maxV {
			maxV = b.Version
		}
	}
	var w bytes.Buffer
	fmt.Fprintf(&w, "// Code generated by state/actors/internal/genbundles from filecoin-project/lotus@%s/build/builtin_actors_gen.go. DO NOT EDIT.\n", tag)
	fmt.Fprintf(&w, "// Regenerate: go run ./state/actors/internal/genbundles -lotus <tag>\n")
	fmt.Fprintf(&w, "// License: Apache-2.0 / MIT (Lotus dual-licensed; see LICENSE{,-APACHE,-MIT}).\n\n")
	fmt.Fprintf(&w, "package actors\n\nimport \"github.com/ipfs/go-cid\"\n\n")
	fmt.Fprintf(&w, "// BundlesLotusVersion is the Lotus release the Bundles table was generated from.\n")
	fmt.Fprintf(&w, "const BundlesLotusVersion = %q\n\n", tag)
	fmt.Fprintf(&w, "// BundlesMaxActorsVersion is the newest actors version present in Bundles.\n")
	fmt.Fprintf(&w, "const BundlesMaxActorsVersion = %d\n\n", maxV)
	fmt.Fprintf(&w, "// BundleEntry lists the actor code CIDs published as part of one\n// builtin-actors release, for one network.\n")
	fmt.Fprintf(&w, "type BundleEntry struct {\n\tNetwork string\n\tVersion int // actors version (%d..%d)\n\tManifestCID cid.Cid\n\tActors map[string]cid.Cid // key from manifest.go (account/cron/...)\n}\n\n", minV, maxV)
	fmt.Fprintf(&w, "// Bundles is the canonical Lotus-shipped list, filtered to mainnet +\n// calibrationnet. Lifted verbatim from lotus@%s build/builtin_actors_gen.go.\n", tag)
	fmt.Fprintf(&w, "var Bundles = []BundleEntry{\n")
	for _, b := range bundles {
		fmt.Fprintf(&w, "{\nNetwork: %q,\nVersion: %d,\nManifestCID: cid.MustParse(%q),\nActors: map[string]cid.Cid{\n", b.Network, b.Version, b.Manifest)
		names := make([]string, 0, len(b.Actors))
		for n := range b.Actors {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&w, "%q: cid.MustParse(%q),\n", n, b.Actors[n])
		}
		fmt.Fprintf(&w, "},\n},\n")
	}
	fmt.Fprintf(&w, "}\n")
	return format.Source(w.Bytes())
}
