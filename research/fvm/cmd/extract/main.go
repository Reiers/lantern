// research/fvm/extract.go: Step 1 of the lantern#89 spike.
//
// Read the builtin-actors CAR, decode the manifest, and extract each
// actor's WASM code block by CID. Writes one .wasm file per actor into
// ./wasm/<name>.wasm plus a manifest.json summary.
//
// Manifest shape (from builtin-actors@v17):
//
//	Top CID -> [version, manifestDataCID]                (CBOR array)
//	manifestDataCID -> [[actor_name, code_cid], ...]     (CBOR array of pairs)
//
// This matches ref-fvm's ManifestData layout so future spikes can
// re-load the same manifest without a custom parser.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/ipfs/go-cid"
	blockstore "github.com/ipld/go-car/v2/blockstore"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: extract <bundle.car>")
		os.Exit(2)
	}
	path := os.Args[1]
	if err := run(path); err != nil {
		fmt.Fprintf(os.Stderr, "err: %v\n", err)
		os.Exit(1)
	}
}

func run(path string) error {
	ctx := context.Background()

	bs, err := blockstore.OpenReadOnly(path)
	if err != nil {
		return fmt.Errorf("open car: %w", err)
	}
	defer bs.Close()

	roots, err := bs.Roots()
	if err != nil {
		return fmt.Errorf("roots: %w", err)
	}
	if len(roots) == 0 {
		return fmt.Errorf("no roots in car")
	}
	fmt.Printf("car roots: %d\n", len(roots))
	for i, r := range roots {
		fmt.Printf("  root[%d]: %s\n", i, r)
	}

	// Every block in the CAR — collect stats
	total := 0
	var codeCIDs []cid.Cid
	byCID := map[string][]byte{}

	ch, err := bs.AllKeysChan(ctx)
	if err != nil {
		return fmt.Errorf("all keys: %w", err)
	}
	for c := range ch {
		blk, err := bs.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("get %s: %w", c, err)
		}
		total++
		byCID[c.String()] = blk.RawData()
		// Heuristic: WASM has a well-known 4-byte magic 0x00 0x61 0x73 0x6d ("\0asm").
		if len(blk.RawData()) >= 4 && bytes.Equal(blk.RawData()[:4], []byte{0x00, 0x61, 0x73, 0x6d}) {
			codeCIDs = append(codeCIDs, c)
		}
	}
	fmt.Printf("total blocks: %d\n", total)
	fmt.Printf("wasm blocks: %d\n", len(codeCIDs))
	// Debug: list every block CID + size so we can find the manifest.
	var allCIDs []string
	for s := range byCID {
		allCIDs = append(allCIDs, s)
	}
	sort.Strings(allCIDs)
	fmt.Println("all block CIDs in CAR:")
	for _, s := range allCIDs {
		fmt.Printf("  %s  len=%d\n", s, len(byCID[s]))
	}

	// The manifest root is either the CAR root directly, or points to it.
	// For builtin-actors@v17 the root is [version, dataCID] where dataCID
	// is a plain CBOR list of [name, cid] pairs.
	if len(roots) != 1 {
		return fmt.Errorf("expected exactly 1 root, got %d", len(roots))
	}
	// Look up by multihash: the CAR may store blocks under the raw codec
	// (bafk) while the root reference uses dag-cbor (bafy) — same hash,
	// different codec byte.
	manifestBlk, ok := byHash(byCID, roots[0])
	if !ok {
		return fmt.Errorf("root cid %s (hash) not present as a block", roots[0])
	}
	// Try to decode as [version, dataCID] first (v3 manifest).
	var top []any
	if err := simpleCborDecode(manifestBlk, &top); err != nil {
		return fmt.Errorf("decode manifest top: %w", err)
	}
	if len(top) < 2 {
		return fmt.Errorf("manifest top: expected 2 fields, got %d", len(top))
	}
	fmt.Printf("manifest version: %v\n", top[0])

	dataCIDBytes, ok := top[1].(cidTag)
	if !ok {
		return fmt.Errorf("manifest[1] not a CID (got %T)", top[1])
	}
	dataCID, err := cid.Cast(dataCIDBytes.bytes)
	if err != nil {
		return fmt.Errorf("cast dataCID: %w", err)
	}
	fmt.Printf("manifest data CID: %s\n", dataCID)

	dataBlk, ok := byHash(byCID, dataCID)
	if !ok {
		return fmt.Errorf("manifest data block not present in CAR")
	}
	var entries [][]any
	if err := simpleCborDecode(dataBlk, &entries); err != nil {
		return fmt.Errorf("decode manifest data: %w", err)
	}

	// Extract each entry [name, cid] and write the WASM out.
	if err := os.MkdirAll("wasm", 0o755); err != nil {
		return err
	}
	type entryOut struct {
		Name    string `json:"name"`
		CodeCID string `json:"code_cid"`
		Size    int    `json:"size"`
		WASM    string `json:"wasm"`
	}
	var out []entryOut
	for _, e := range entries {
		if len(e) != 2 {
			return fmt.Errorf("manifest entry has %d fields, want 2: %v", len(e), e)
		}
		name, ok := e[0].(string)
		if !ok {
			return fmt.Errorf("manifest entry [0] not string: %T", e[0])
		}
		ctag, ok := e[1].(cidTag)
		if !ok {
			return fmt.Errorf("manifest entry [1] not CID: %T", e[1])
		}
		c, err := cid.Cast(ctag.bytes)
		if err != nil {
			return fmt.Errorf("cast entry cid: %w", err)
		}
		blk, ok := byHash(byCID, c)
		if !ok {
			return fmt.Errorf("actor %q code block missing: %s", name, c)
		}
		if len(blk) < 4 || !bytes.Equal(blk[:4], []byte{0x00, 0x61, 0x73, 0x6d}) {
			return fmt.Errorf("actor %q block does not start with wasm magic (first 8: %s)",
				name, hex.EncodeToString(blk[:min(8, len(blk))]))
		}
		outPath := fmt.Sprintf("wasm/%s.wasm", name)
		if err := os.WriteFile(outPath, blk, 0o644); err != nil {
			return err
		}
		out = append(out, entryOut{Name: name, CodeCID: c.String(), Size: len(blk), WASM: outPath})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size < out[j].Size })

	f, err := os.Create("manifest.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	f.Close()

	fmt.Println("\nActors sorted by WASM size (smallest first):")
	for _, e := range out {
		fmt.Printf("  %-16s  %6d bytes  %s\n", e.Name, e.Size, e.CodeCID)
	}
	fmt.Printf("\nWrote %d WASM files to ./wasm/ + manifest.json\n", len(out))
	_ = io.Copy
	return nil
}

// --- minimal CBOR helpers ---------------------------------------------------
//
// The manifest uses a tiny subset of CBOR: unsigned ints, byte strings, text
// strings, arrays, and CBOR tag 42 for CIDs (RFC 8949 / dag-cbor). Enough of
// a decoder for this one use.

type cidTag struct{ bytes []byte }

func simpleCborDecode(data []byte, out any) error {
	r := &cborReader{buf: data}
	v, err := r.value()
	if err != nil {
		return err
	}
	switch dst := out.(type) {
	case *[]any:
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("expected top-level array, got %T", v)
		}
		*dst = arr
		return nil
	case *[][]any:
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("expected top-level array, got %T", v)
		}
		res := make([][]any, len(arr))
		for i, x := range arr {
			inner, ok := x.([]any)
			if !ok {
				return fmt.Errorf("element %d not array: %T", i, x)
			}
			res[i] = inner
		}
		*dst = res
		return nil
	default:
		return fmt.Errorf("unsupported target type %T", out)
	}
}

type cborReader struct {
	buf []byte
	pos int
}

func (r *cborReader) value() (any, error) {
	if r.pos >= len(r.buf) {
		return nil, fmt.Errorf("cbor eof at %d", r.pos)
	}
	ib := r.buf[r.pos]
	major := ib >> 5
	info := ib & 0x1f
	r.pos++
	arg, err := r.arg(info)
	if err != nil {
		return nil, err
	}
	switch major {
	case 0: // uint
		return uint64(arg), nil
	case 1: // negative
		return -1 - int64(arg), nil
	case 2: // byte string
		if r.pos+int(arg) > len(r.buf) {
			return nil, fmt.Errorf("bytes short at %d", r.pos)
		}
		out := r.buf[r.pos : r.pos+int(arg)]
		r.pos += int(arg)
		return append([]byte(nil), out...), nil
	case 3: // text string
		if r.pos+int(arg) > len(r.buf) {
			return nil, fmt.Errorf("text short at %d", r.pos)
		}
		s := string(r.buf[r.pos : r.pos+int(arg)])
		r.pos += int(arg)
		return s, nil
	case 4: // array
		out := make([]any, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case 5: // map (unused here)
		return nil, fmt.Errorf("cbor map unsupported")
	case 6: // tag
		inner, err := r.value()
		if err != nil {
			return nil, err
		}
		if arg == 42 { // dag-cbor CID tag
			// inner should be a byte string; the first byte is a null-byte
			// multibase prefix per the dag-cbor spec.
			b, ok := inner.([]byte)
			if !ok {
				return nil, fmt.Errorf("cbor tag 42: inner not bytes: %T", inner)
			}
			if len(b) > 0 && b[0] == 0 {
				b = b[1:]
			}
			return cidTag{bytes: b}, nil
		}
		return inner, nil
	default:
		return nil, fmt.Errorf("cbor major %d not supported", major)
	}
}

func (r *cborReader) arg(info byte) (uint64, error) {
	switch info {
	case 24:
		if r.pos >= len(r.buf) {
			return 0, fmt.Errorf("arg8 eof")
		}
		v := uint64(r.buf[r.pos])
		r.pos++
		return v, nil
	case 25:
		if r.pos+2 > len(r.buf) {
			return 0, fmt.Errorf("arg16 eof")
		}
		v := uint64(r.buf[r.pos])<<8 | uint64(r.buf[r.pos+1])
		r.pos += 2
		return v, nil
	case 26:
		if r.pos+4 > len(r.buf) {
			return 0, fmt.Errorf("arg32 eof")
		}
		v := uint64(r.buf[r.pos])<<24 | uint64(r.buf[r.pos+1])<<16 |
			uint64(r.buf[r.pos+2])<<8 | uint64(r.buf[r.pos+3])
		r.pos += 4
		return v, nil
	case 27:
		if r.pos+8 > len(r.buf) {
			return 0, fmt.Errorf("arg64 eof")
		}
		var v uint64
		for i := 0; i < 8; i++ {
			v = v<<8 | uint64(r.buf[r.pos+i])
		}
		r.pos += 8
		return v, nil
	default:
		if info < 24 {
			return uint64(info), nil
		}
		return 0, fmt.Errorf("cbor arg info %d not supported", info)
	}
}

// byHash looks up a block ignoring the CID's codec byte, matching only
// on the multihash. The CAR frequently stores blocks under the raw codec
// (0x55) while references use dag-cbor (0x71) — same content, different
// CID prefix.
func byHash(m map[string][]byte, want cid.Cid) ([]byte, bool) {
	wh := want.Hash().String()
	for cs, blk := range m {
		c, err := cid.Parse(cs)
		if err != nil {
			continue
		}
		if c.Hash().String() == wh {
			return blk, true
		}
	}
	return nil, false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
