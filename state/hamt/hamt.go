// Lookup + VerifyProof against a Filecoin-flavoured HAMT.
//
// This file is the proof-recording layer over go-hamt-ipld/v3. Decoding,
// hashing, traversal and bucket-search are all delegated to that library
// (~1.4k LOC, pure Go, no CGo). We capture the fetched CIDs and provide an
// independent re-verification entry point.
//
// Filecoin parameters (from go-state-types/builtin/v15/init and the
// hash-map spec appendix): bitWidth = 5, hash = sha256 (truncated to 32 bytes),
// bucketSize = 3. The Filecoin Init actor's address-resolution table and the
// top-level state tree both use these parameters.

package hamt

import (
	"context"
	"crypto/sha256"
	"fmt"

	cbornode "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	hamtipld "github.com/filecoin-project/go-hamt-ipld/v3"
	"github.com/ipfs/go-cid"
)

// Defaults for the canonical Filecoin state-tree HAMT.
const (
	FilBitWidth   = 5
	FilBucketSize = 3
)

// FilHash implements the go-hamt-ipld HashFunc using sha256, matching what
// Filecoin's state tree uses. Returns the full 32-byte digest.
func FilHash(k []byte) []byte {
	h := sha256.Sum256(k)
	return h[:]
}

// Options on a single Lookup call.
type LookupOptions struct {
	// BitWidth selects the HAMT bit-width. Default FilBitWidth (5).
	BitWidth int
	// HashAlgo selects the hashing function. Default FilHash (sha256).
	HashAlgo func([]byte) []byte
}

func (o *LookupOptions) hamtOpts() []hamtipld.Option {
	bw := o.BitWidth
	if bw == 0 {
		bw = FilBitWidth
	}
	h := o.HashAlgo
	if h == nil {
		h = FilHash
	}
	return []hamtipld.Option{
		hamtipld.UseTreeBitWidth(bw),
		hamtipld.UseHashFunction(hamtipld.HashFunc(h)),
	}
}

// Lookup walks the HAMT rooted at `root`, fetching nodes via `bg` (which
// must serve canonical IPLD-DAG-CBOR encoded HAMT nodes for every CID it
// returns). It returns the raw value bytes for the leaf, the *path of node
// CIDs traversed* (the inclusion proof), and an error.
//
// If the key isn't present, Lookup returns ErrNotFound and the proof path
// captured up to the negative-result node (the absence proof).
func Lookup(ctx context.Context, root cid.Cid, key []byte, bg BlockGetter, opts *LookupOptions) ([]byte, []cid.Cid, error) {
	if opts == nil {
		opts = &LookupOptions{}
	}
	prs := newPathRecordingStore(bg)
	cstore := cbornode.NewCborStore(prs)

	node, err := hamtipld.LoadNode(ctx, cstore, root, opts.hamtOpts()...)
	if err != nil {
		return nil, prs.Path(), fmt.Errorf("loading HAMT root %s: %w", root, err)
	}

	found, raw, err := node.FindRaw(ctx, string(key))
	if err != nil {
		return nil, prs.Path(), fmt.Errorf("HAMT walk: %w", err)
	}
	if !found {
		return nil, prs.Path(), ErrNotFound
	}
	return raw, prs.Path(), nil
}

// LookupCBOR is a convenience wrapper around Lookup that unmarshals the
// leaf bytes into out via cbor-gen's UnmarshalCBOR interface.
func LookupCBOR(ctx context.Context, root cid.Cid, key []byte, bg BlockGetter, out cbg.CBORUnmarshaler, opts *LookupOptions) ([]cid.Cid, error) {
	raw, path, err := Lookup(ctx, root, key, bg, opts)
	if err != nil {
		return path, err
	}
	if err := out.UnmarshalCBOR(bytesReader(raw)); err != nil {
		return path, fmt.Errorf("decoding leaf: %w", err)
	}
	return path, nil
}

// VerifyProof independently re-verifies an inclusion-proof: given a root
// CID, a key, an expected value, and a proof path (list of node CIDs in
// fetch order), it loads each node from blockGet, walks the HAMT, and
// confirms the key resolves to value. Returns nil on success.
//
// Crucially, blockGet here is typically a *fresh* MemBlockStore seeded
// only with the proof-path bytes — that's how a third party can audit a
// claim "Lantern claimed addr X had balance Y at state root R" using
// nothing but the proof bytes Lantern published.
func VerifyProof(ctx context.Context, root cid.Cid, key []byte, value []byte, proof []cid.Cid, blockGet BlockGetter, opts *LookupOptions) error {
	if opts == nil {
		opts = &LookupOptions{}
	}
	// Re-hash each block in the proof so we know they're canonical.
	for _, c := range proof {
		raw, err := blockGet.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("missing proof node %s: %w", c, err)
		}
		if err := VerifyBlockCID(c, raw); err != nil {
			return fmt.Errorf("proof node %s: %w", c, err)
		}
	}

	// Re-run the walk.
	got, _, err := Lookup(ctx, root, key, blockGet, opts)
	if err != nil {
		return fmt.Errorf("re-walking HAMT for proof verify: %w", err)
	}
	if !bytesEqual(got, value) {
		return fmt.Errorf("proof verifies, but value bytes don't match: got %x want %x", got, value)
	}
	return nil
}

// ErrNotFound is returned by Lookup when the key is not in the HAMT.
var ErrNotFound = fmt.Errorf("key not found in HAMT")

// bytesReader is a tiny adapter to read raw bytes via the io.Reader interface.
func bytesReader(b []byte) *bReader { return &bReader{b: b} }

type bReader struct {
	b []byte
	o int
}

func (r *bReader) Read(p []byte) (int, error) {
	if r.o >= len(r.b) {
		return 0, errEOF
	}
	n := copy(p, r.b[r.o:])
	r.o += n
	return n, nil
}
func (r *bReader) ReadByte() (byte, error) {
	if r.o >= len(r.b) {
		return 0, errEOF
	}
	c := r.b[r.o]
	r.o++
	return c, nil
}

var errEOF = fmt.Errorf("eof")

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
