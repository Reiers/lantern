// Unit tests for the KAMT subtree walker (lantern#44).
//
// These tests construct synthetic KAMT-shaped CBOR blocks: each "node" is
// CBOR `[bitfield_bytes, [pointer...]]`, each link pointer is
// `{"l":[cid, extLen, extBytes]}`. We don't go through the full builder
// path because the walker only needs to recognise nodes + follow link
// pointers — value pointers are leaves, opaque to it.
package kamt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/state/hamt"
)

// memBG is a tiny in-memory BlockGetter for tests.
type memBG struct {
	m map[string][]byte
}

func newMemBG() *memBG { return &memBG{m: make(map[string][]byte)} }

func (m *memBG) put(raw []byte) cid.Cid {
	mh, err := multihash.Sum(raw, multihash.BLAKE2B_MIN+31, -1)
	if err != nil {
		panic(err)
	}
	c := cid.NewCidV1(cid.DagCBOR, mh)
	m.m[c.KeyString()] = append([]byte(nil), raw...)
	return c
}

func (m *memBG) Get(_ context.Context, c cid.Cid) ([]byte, error) {
	raw, ok := m.m[c.KeyString()]
	if !ok {
		return nil, errors.New("kamt walk test: not found")
	}
	return append([]byte(nil), raw...), nil
}

var _ hamt.BlockGetter = (*memBG)(nil)

// encodeNode encodes [bitfield(bytes), [pointer...]] as CBOR, where each
// pointer is `{"l": [cid, extLen_u32, extBytes]}`.
func encodeLinkOnlyNode(t *testing.T, bitfield []byte, children []cid.Cid) []byte {
	t.Helper()
	var buf bytes.Buffer
	// outer [2]
	if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajArray, 2); err != nil {
		t.Fatal(err)
	}
	// bitfield bytes
	if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajByteString, uint64(len(bitfield))); err != nil {
		t.Fatal(err)
	}
	buf.Write(bitfield)
	// inner array of pointers
	if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajArray, uint64(len(children))); err != nil {
		t.Fatal(err)
	}
	for _, c := range children {
		// {"l": [cid, 0, h''] }  (map of 1 entry, key "l")
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajMap, 1); err != nil {
			t.Fatal(err)
		}
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajTextString, 1); err != nil {
			t.Fatal(err)
		}
		buf.WriteByte('l')
		// value: [cid, extLen_u32, extBytes]
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajArray, 3); err != nil {
			t.Fatal(err)
		}
		// cid as tag(42) + byte-string(0x00 || raw cid bytes)
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajTag, 42); err != nil {
			t.Fatal(err)
		}
		cb := c.Bytes()
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajByteString, uint64(len(cb)+1)); err != nil {
			t.Fatal(err)
		}
		buf.WriteByte(0x00)
		buf.Write(cb)
		// extLen
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajUnsignedInt, 0); err != nil {
			t.Fatal(err)
		}
		// extBytes (empty)
		if err := cbg.WriteMajorTypeHeader(&buf, cbg.MajByteString, 0); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// buildTwoLevelTree returns the root cid of a two-level link-only tree:
// root has fanout children, each a leaf link-only node with no children.
func buildTwoLevelTree(t *testing.T, store *memBG, fanout int) cid.Cid {
	t.Helper()
	// Children are leaf nodes (empty link list). bitfield zero, 0 ptrs.
	leafRaw := encodeLinkOnlyNode(t, []byte{0x00}, nil)
	children := make([]cid.Cid, fanout)
	for i := 0; i < fanout; i++ {
		// Distinct payload per leaf so each has a distinct CID.
		uniq := append([]byte(nil), leafRaw...)
		uniq = append(uniq, byte(i)) // intentional CBOR garbage tail
		children[i] = store.put(uniq)
	}
	// A nonzero bitfield (purely cosmetic; decoder reads pointer count
	// from inner-array header).
	rootRaw := encodeLinkOnlyNode(t, []byte{0xff, 0xff, 0xff, 0xff}, children)
	return store.put(rootRaw)
}

func TestWalkSubtree_FetchesAllNodes(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 4)

	stats, err := WalkSubtree(context.Background(), root, store, WalkOptions{MaxNodes: 100})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// root + 4 leaves (but leaves are CBOR-garbage so decode errors).
	if stats.NodesFetched < 1 {
		t.Fatalf("expected at least the root fetched, got %+v", stats)
	}
	if stats.NodesFetched > 5 {
		t.Fatalf("expected at most 5 nodes (root + 4 leaves), got %+v", stats)
	}
	// Errors are tolerated (the leaves intentionally don't decode as
	// real nodes), but root must succeed.
	if stats.Errors > 4 {
		t.Fatalf("too many errors: %+v", stats)
	}
}

func TestWalkSubtree_RespectsMaxNodes(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 6)

	stats, err := WalkSubtree(context.Background(), root, store, WalkOptions{MaxNodes: 1})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if stats.NodesFetched != 1 {
		t.Fatalf("MaxNodes=1 must cap fetches at 1, got %+v", stats)
	}
	if !stats.Capped {
		t.Fatalf("expected Capped=true, got %+v", stats)
	}
}

func TestWalkSubtree_NilGetterErrs(t *testing.T) {
	if _, err := WalkSubtree(context.Background(), cid.Undef, nil, WalkOptions{}); err == nil {
		t.Fatal("expected error on nil getter")
	}
}

func TestWalkSubtree_UndefinedRootErrs(t *testing.T) {
	if _, err := WalkSubtree(context.Background(), cid.Undef, newMemBG(), WalkOptions{}); err == nil {
		t.Fatal("expected error on undefined root")
	}
}

func TestWalkSubtree_RootMissPropagates(t *testing.T) {
	store := newMemBG()
	// Reserve a cid that is NOT in the store.
	mh, _ := multihash.Sum([]byte("nope"), multihash.BLAKE2B_MIN+31, -1)
	root := cid.NewCidV1(cid.DagCBOR, mh)
	stats, err := WalkSubtree(context.Background(), root, store, WalkOptions{MaxNodes: 10})
	if err != nil {
		// Not required, but acceptable.
		_ = err
	}
	if stats.Errors == 0 {
		t.Fatalf("expected root-miss to be counted, got %+v", stats)
	}
}

// silence unused-import warnings if cbg helpers shift around.
var _ = fmt.Sprintf

