// Path-recording IPLD blockstore + IpldStore wrappers used by the HAMT and
// AMT walkers.

package hamt

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multihash"
)

// BlockGetter is the minimal interface Lantern's state layer needs from a
// block source. It is satisfied by:
//
//   - state/cache (local Badger blockstore)
//   - net/bitswap (Bitswap client)
//   - net/hsync (HTTP gateway client)
//   - net/combined (the cache+bitswap+http fallback chain)
//   - testdata fixtures (MemBlockStore in this package)
//
// Get returns the raw IPLD block bytes for the given CID, or an error. The
// caller is responsible for verifying the CID against the returned bytes; for
// safety, all Lantern-internal BlockGetter wrappers re-hash on the way out.
type BlockGetter interface {
	Get(ctx context.Context, c cid.Cid) ([]byte, error)
}

// MemBlockStore is an in-memory BlockGetter used for tests and fixture
// replay. It also implements the go-ipld-cbor IpldBlockstore interface so
// it can be plugged into go-hamt-ipld and go-amt-ipld directly.
type MemBlockStore struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemBlockStore returns an empty MemBlockStore.
func NewMemBlockStore() *MemBlockStore {
	return &MemBlockStore{m: make(map[string][]byte)}
}

// Put stores raw bytes under their CID. Returns the CID for chaining.
// It does not re-validate that c.Hash matches the bytes; callers wanting
// to import untrusted data should use PutVerify.
func (m *MemBlockStore) Put(c cid.Cid, raw []byte) cid.Cid {
	m.mu.Lock()
	m.m[c.KeyString()] = append([]byte(nil), raw...)
	m.mu.Unlock()
	return c
}

// PutVerify recomputes the CID's hash over raw and inserts only if it
// matches; otherwise returns an error.
func (m *MemBlockStore) PutVerify(c cid.Cid, raw []byte) error {
	got, err := c.Prefix().Sum(raw)
	if err != nil {
		return fmt.Errorf("hashing block for CID verify: %w", err)
	}
	if !got.Equals(c) {
		return fmt.Errorf("CID mismatch: have %s, computed %s", c, got)
	}
	m.Put(c, raw)
	return nil
}

// Get implements BlockGetter.
func (m *MemBlockStore) Get(_ context.Context, c cid.Cid) ([]byte, error) {
	m.mu.RLock()
	raw, ok := m.m[c.KeyString()]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("block not found: %s", c)
	}
	return append([]byte(nil), raw...), nil
}

// GetBlock implements the go-ipld-cbor IpldBlockstore interface.
func (m *MemBlockStore) GetBlock(ctx context.Context, c cid.Cid) (block.Block, error) {
	raw, err := m.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	return block.NewBlockWithCid(raw, c)
}

// PutBlock implements the go-ipld-cbor IpldBlockstore interface.
func (m *MemBlockStore) PutBlock(_ context.Context, b block.Block) error {
	m.Put(b.Cid(), b.RawData())
	return nil
}

// Has reports whether a block is present.
func (m *MemBlockStore) Has(c cid.Cid) bool {
	m.mu.RLock()
	_, ok := m.m[c.KeyString()]
	m.mu.RUnlock()
	return ok
}

// Len returns the number of stored blocks.
func (m *MemBlockStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.m)
}

// CIDs returns the list of stored CIDs (unordered).
func (m *MemBlockStore) CIDs() []cid.Cid {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]cid.Cid, 0, len(m.m))
	for k := range m.m {
		c, _ := cid.Cast([]byte(k))
		out = append(out, c)
	}
	return out
}

// adapt-shape: implement boxo/ipld-blockstore-like interface for go-hamt-ipld.

type basicBlockstoreAdapter struct{ inner *MemBlockStore }

func (a *basicBlockstoreAdapter) Get(ctx context.Context, c cid.Cid) (block.Block, error) {
	return a.inner.GetBlock(ctx, c)
}
func (a *basicBlockstoreAdapter) Put(ctx context.Context, b block.Block) error {
	return a.inner.PutBlock(ctx, b)
}

// CborStoreFromMem returns a cbornode.IpldStore backed by an in-memory
// MemBlockStore. Tests use it.
func CborStoreFromMem(m *MemBlockStore) *cbornode.BasicIpldStore {
	return cbornode.NewCborStore(&basicBlockstoreAdapter{inner: m})
}

// --------------------------------------------------------------------
// pathRecordingStore wraps any BlockGetter and an underlying go-ipld-cbor
// IpldStore, recording every CID it fetches in fetch order. This is what
// gives us inclusion-proof paths from a vanilla go-hamt-ipld Find call.

type pathRecordingStore struct {
	inner BlockGetter

	mu   sync.Mutex
	path []cid.Cid
	seen map[string]bool
}

func newPathRecordingStore(inner BlockGetter) *pathRecordingStore {
	return &pathRecordingStore{inner: inner, seen: make(map[string]bool)}
}

// Get implements cbornode.IpldBlockstore.
func (p *pathRecordingStore) Get(ctx context.Context, c cid.Cid) (block.Block, error) {
	raw, err := p.inner.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	// Re-verify CID. This is a *defensive* check: any honest BlockGetter
	// should already do this, but the HAMT walker is the last line of
	// defense for the "no peer trust" property.
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

// Put implements cbornode.IpldBlockstore. We never write through this
// adapter; it'd indicate a logic bug if invoked, so we return an error.
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

// --------------------------------------------------------------------

// VerifyBlockCID recomputes the CID hash over raw and returns nil iff it
// matches c. Exported because both state/hamt and state/amt callers want it.
func VerifyBlockCID(c cid.Cid, raw []byte) error {
	got, err := c.Prefix().Sum(raw)
	if err != nil {
		return fmt.Errorf("hashing block: %w", err)
	}
	if !got.Equals(c) {
		return fmt.Errorf("CID mismatch: have %s, computed %s", c, got)
	}
	return nil
}

// computeCIDForBytes hashes raw under the same multihash as `c.Prefix()`.
// Useful when building a CID from-scratch with a specific codec.
func computeCIDForBytes(codec uint64, mhType uint64, mhLen int, raw []byte) (cid.Cid, error) {
	pref := cid.Prefix{Version: 1, Codec: codec, MhType: mhType, MhLength: mhLen}
	return pref.Sum(raw)
}

// silence unused-import lint when the bytes package isn't otherwise used.
var _ = bytes.Compare
var _ = multihash.SHA2_256
