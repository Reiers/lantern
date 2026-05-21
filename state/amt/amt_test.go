package amt

import (
	"bytes"
	"context"
	"testing"

	amtipld "github.com/filecoin-project/go-amt-ipld/v4"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/state/hamt"
)

// build an AMT in-memory, then walk it via our path-recording Lookup.
func TestSyntheticAMT_RoundTrip(t *testing.T) {
	ctx := context.Background()
	mem := hamt.NewMemBlockStore()
	store := hamt.CborStoreFromMem(mem)

	amtRoot, err := amtipld.NewAMT(store, amtipld.UseTreeBitWidth(FilBitWidth))
	if err != nil {
		t.Fatalf("NewAMT: %v", err)
	}

	// Insert 200 sparse indices so we definitely span multiple nodes.
	values := make(map[uint64][]byte)
	for i := uint64(0); i < 200; i++ {
		idx := i * 7
		val := &cbg.Deferred{Raw: []byte{0x41, byte(i)}} // CBOR byte-string of one byte = i.
		if err := amtRoot.Set(ctx, idx, val); err != nil {
			t.Fatalf("Set %d: %v", idx, err)
		}
		values[idx] = val.Raw
	}
	rootCID, err := amtRoot.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	t.Logf("synthetic AMT root: %s (%d blocks)", rootCID, mem.Len())

	for idx, want := range values {
		got, proof, err := Lookup(ctx, rootCID, idx, mem, nil)
		if err != nil {
			t.Fatalf("Lookup(%d): %v", idx, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Lookup(%d) value mismatch: got %x want %x", idx, got, want)
		}
		if len(proof) == 0 {
			t.Fatalf("Lookup(%d) empty proof", idx)
		}
	}
}

func TestSyntheticAMT_NotFound(t *testing.T) {
	ctx := context.Background()
	mem := hamt.NewMemBlockStore()
	store := hamt.CborStoreFromMem(mem)

	amtRoot, _ := amtipld.NewAMT(store, amtipld.UseTreeBitWidth(FilBitWidth))
	_ = amtRoot.Set(ctx, 0, &cbg.Deferred{Raw: []byte{0x01}})
	rootCID, _ := amtRoot.Flush(ctx)

	_, _, err := Lookup(ctx, rootCID, 999999, mem, nil)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSyntheticAMT_VerifyProof(t *testing.T) {
	ctx := context.Background()
	mem := hamt.NewMemBlockStore()
	store := hamt.CborStoreFromMem(mem)

	amtRoot, _ := amtipld.NewAMT(store, amtipld.UseTreeBitWidth(FilBitWidth))
	for i := uint64(0); i < 100; i++ {
		_ = amtRoot.Set(ctx, i*3, &cbg.Deferred{Raw: []byte{0x41, byte(i)}})
	}
	rootCID, _ := amtRoot.Flush(ctx)

	idx := uint64(42 * 3)
	val, proof, err := Lookup(ctx, rootCID, idx, mem, nil)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// Build a fresh BlockGetter that ONLY contains the proof blocks.
	thin := hamt.NewMemBlockStore()
	for _, c := range proof {
		raw, _ := mem.Get(ctx, c)
		thin.Put(c, raw)
	}
	if err := VerifyProof(ctx, rootCID, idx, val, proof, thin, nil); err != nil {
		t.Fatalf("VerifyProof failed: %v", err)
	}
	if err := VerifyProof(ctx, rootCID, idx, []byte{0xff}, proof, thin, nil); err == nil {
		t.Fatalf("VerifyProof should detect wrong value")
	}
}
