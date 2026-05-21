// Lookup + VerifyProof against a Filecoin-flavoured AMT (Array-Mapped Trie).
//
// Filecoin's default AMT parameters: bitWidth = 3, width = 8 per node. The
// actor state structs (e.g. miner.ParentMessageReceipts, market.Proposals)
// configure these on a per-context basis. We expose BitWidth as a lookup
// option.
//
// Like state/hamt, this file is a path-recording layer over the underlying
// go-amt-ipld/v4 library. The proof path is the list of node CIDs fetched in
// fetch order during a Lookup, sufficient to re-verify the result with no
// other inputs.

package amt

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	amtipld "github.com/filecoin-project/go-amt-ipld/v4"
	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/state/hamt"
)

// FilBitWidth is the default Filecoin AMT bit-width. Most actor sub-AMTs
// use width = 3 (8 entries per node); see go-amt-ipld/v4 docs.
const FilBitWidth = 3

// LookupOptions on a single Lookup call.
type LookupOptions struct {
	BitWidth uint // default FilBitWidth (3)
}

func (o *LookupOptions) amtOpts() []amtipld.Option {
	bw := o.BitWidth
	if bw == 0 {
		bw = FilBitWidth
	}
	return []amtipld.Option{amtipld.UseTreeBitWidth(bw)}
}

// pathRecordingStore mirrors the hamt-package one. We re-implement it here
// instead of exporting it to keep the two packages independent and avoid an
// inverted import dependency (state/hamt depends on nothing in state/amt and
// vice-versa).
type pathRecordingStore struct {
	inner hamt.BlockGetter

	mu   sync.Mutex
	path []cid.Cid
	seen map[string]bool
}

func newPathRecordingStore(inner hamt.BlockGetter) *pathRecordingStore {
	return &pathRecordingStore{inner: inner, seen: make(map[string]bool)}
}

// Get implements cbornode.IpldBlockstore.
func (p *pathRecordingStore) Get(ctx context.Context, c cid.Cid) (block.Block, error) {
	raw, err := p.inner.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	want, err := c.Prefix().Sum(raw)
	if err != nil {
		return nil, fmt.Errorf("recomputing CID: %w", err)
	}
	if !want.Equals(c) {
		return nil, fmt.Errorf("CID mismatch from BlockGetter: requested %s, computed %s", c, want)
	}
	p.mu.Lock()
	if !p.seen[c.KeyString()] {
		p.seen[c.KeyString()] = true
		p.path = append(p.path, c)
	}
	p.mu.Unlock()
	return block.NewBlockWithCid(raw, c)
}

func (p *pathRecordingStore) Put(_ context.Context, _ block.Block) error {
	return fmt.Errorf("pathRecordingStore is read-only")
}

func (p *pathRecordingStore) Path() []cid.Cid {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]cid.Cid, len(p.path))
	copy(out, p.path)
	return out
}

// Lookup walks the AMT rooted at `root`, fetching nodes via `bg`. It returns
// the raw value bytes at index `i`, the proof path (list of node CIDs in
// fetch order), and an error.
func Lookup(ctx context.Context, root cid.Cid, i uint64, bg hamt.BlockGetter, opts *LookupOptions) ([]byte, []cid.Cid, error) {
	if opts == nil {
		opts = &LookupOptions{}
	}
	prs := newPathRecordingStore(bg)
	cstore := cbornode.NewCborStore(prs)

	r, err := amtipld.LoadAMT(ctx, cstore, root, opts.amtOpts()...)
	if err != nil {
		return nil, prs.Path(), fmt.Errorf("loading AMT root %s: %w", root, err)
	}

	var d cbg.Deferred
	found, err := r.Get(ctx, i, &d)
	if err != nil {
		return nil, prs.Path(), fmt.Errorf("AMT walk: %w", err)
	}
	if !found {
		return nil, prs.Path(), ErrNotFound
	}
	return d.Raw, prs.Path(), nil
}

// LookupCBOR is a convenience wrapper that decodes the leaf bytes via
// cbor-gen into `out`.
func LookupCBOR(ctx context.Context, root cid.Cid, i uint64, bg hamt.BlockGetter, out cbg.CBORUnmarshaler, opts *LookupOptions) ([]cid.Cid, error) {
	raw, path, err := Lookup(ctx, root, i, bg, opts)
	if err != nil {
		return path, err
	}
	if err := out.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
		return path, fmt.Errorf("decoding AMT leaf %d: %w", i, err)
	}
	return path, nil
}

// VerifyProof independently re-verifies an AMT lookup proof. See
// state/hamt.VerifyProof for the design rationale.
func VerifyProof(ctx context.Context, root cid.Cid, i uint64, value []byte, proof []cid.Cid, bg hamt.BlockGetter, opts *LookupOptions) error {
	for _, c := range proof {
		raw, err := bg.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("missing proof node %s: %w", c, err)
		}
		want, err := c.Prefix().Sum(raw)
		if err != nil {
			return fmt.Errorf("hashing proof node %s: %w", c, err)
		}
		if !want.Equals(c) {
			return fmt.Errorf("proof node CID mismatch: have %s, computed %s", c, want)
		}
	}
	got, _, err := Lookup(ctx, root, i, bg, opts)
	if err != nil {
		return fmt.Errorf("re-walking AMT for proof verify: %w", err)
	}
	if !bytes.Equal(got, value) {
		return fmt.Errorf("proof verifies, but value bytes don't match")
	}
	return nil
}

// ErrNotFound is returned by Lookup when the index is not in the AMT.
var ErrNotFound = fmt.Errorf("index not found in AMT")
