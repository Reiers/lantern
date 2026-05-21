package hamt

import (
	"bytes"
	"context"
	"testing"

	hamtipld "github.com/filecoin-project/go-hamt-ipld/v3"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// TestSyntheticHAMT_RoundTrip builds a small HAMT in-memory with Filecoin
// parameters (bitWidth=5, sha256), records its root CID, then walks it via
// Lookup and confirms the proof path is non-empty and the value matches.
func TestSyntheticHAMT_RoundTrip(t *testing.T) {
	ctx := context.Background()
	mem := NewMemBlockStore()
	store := CborStoreFromMem(mem)

	node, err := hamtipld.NewNode(store,
		hamtipld.UseTreeBitWidth(FilBitWidth),
		hamtipld.UseHashFunction(hamtipld.HashFunc(FilHash)),
	)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	// Insert ~100 keys so we definitely get a multi-node tree.
	keys := make(map[string][]byte, 100)
	for i := 0; i < 100; i++ {
		k := []byte{byte(i & 0xff), byte((i >> 8) & 0xff), 0xab, 0xcd}
		// CBOR-valid value: 2-element array of [byte-string{i}, byte-string{0x2a}]
		v := &cbg.Deferred{Raw: []byte{0x82, 0x41, byte(i), 0x41, 0x2a}}
		if err := node.Set(ctx, string(k), v); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
		keys[string(k)] = v.Raw
	}
	if err := node.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rootCID, err := store.Put(ctx, node)
	if err != nil {
		t.Fatalf("Put root: %v", err)
	}
	t.Logf("synthetic HAMT root: %s (%d blocks)", rootCID, mem.Len())

	// Walk via our Lookup. Confirm value matches and proof path is non-empty.
	hits := 0
	for k, want := range keys {
		got, proof, err := Lookup(ctx, rootCID, []byte(k), mem, nil)
		if err != nil {
			t.Fatalf("Lookup(%x): %v", k, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Lookup(%x) value mismatch: got %x want %x", k, got, want)
		}
		if len(proof) == 0 {
			t.Fatalf("Lookup(%x) empty proof", k)
		}
		hits++
	}
	if hits != len(keys) {
		t.Fatalf("expected %d hits, got %d", len(keys), hits)
	}
	t.Logf("walked %d keys, proof depths bounded by tree shape", hits)
}

func TestSyntheticHAMT_NotFound(t *testing.T) {
	ctx := context.Background()
	mem := NewMemBlockStore()
	store := CborStoreFromMem(mem)

	node, err := hamtipld.NewNode(store,
		hamtipld.UseTreeBitWidth(FilBitWidth),
		hamtipld.UseHashFunction(hamtipld.HashFunc(FilHash)),
	)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	v := &cbg.Deferred{Raw: []byte{0x18, 0x42}} // CBOR uint 0x42
	_ = node.Set(ctx, "present", v)
	_ = node.Flush(ctx)
	rootCID, _ := store.Put(ctx, node)

	_, proof, err := Lookup(ctx, rootCID, []byte("absent"), mem, nil)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if len(proof) == 0 {
		t.Fatalf("absence proof should still record the root visit")
	}
}

func TestSyntheticHAMT_VerifyProof(t *testing.T) {
	ctx := context.Background()
	mem := NewMemBlockStore()
	store := CborStoreFromMem(mem)

	node, _ := hamtipld.NewNode(store,
		hamtipld.UseTreeBitWidth(FilBitWidth),
		hamtipld.UseHashFunction(hamtipld.HashFunc(FilHash)),
	)
	for i := 0; i < 50; i++ {
		k := []byte{byte(i), 0xab}
		// CBOR-valid: byte-string of length 1 carrying byte i.
		_ = node.Set(ctx, string(k), &cbg.Deferred{Raw: []byte{0x41, byte(i)}})
	}
	_ = node.Flush(ctx)
	rootCID, _ := store.Put(ctx, node)

	key := []byte{0x05, 0xab}
	val, proof, err := Lookup(ctx, rootCID, key, mem, nil)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// Build a fresh BlockGetter that ONLY contains the proof blocks. The
	// verifier must succeed using just these.
	thin := NewMemBlockStore()
	for _, c := range proof {
		raw, _ := mem.Get(ctx, c)
		thin.Put(c, raw)
	}

	if err := VerifyProof(ctx, rootCID, key, val, proof, thin, nil); err != nil {
		t.Fatalf("VerifyProof failed: %v", err)
	}

	// Negative: lie about the value.
	if err := VerifyProof(ctx, rootCID, key, []byte{0xff}, proof, thin, nil); err == nil {
		t.Fatalf("VerifyProof should have detected a wrong value")
	}
}

func TestVerifyBlockCID_DetectsTampering(t *testing.T) {
	mem := NewMemBlockStore()
	store := CborStoreFromMem(mem)
	ctx := context.Background()

	node, _ := hamtipld.NewNode(store,
		hamtipld.UseTreeBitWidth(FilBitWidth),
		hamtipld.UseHashFunction(hamtipld.HashFunc(FilHash)),
	)
	_ = node.Set(ctx, "k", &cbg.Deferred{Raw: []byte{0x01}}) // CBOR uint 1
	_ = node.Flush(ctx)
	rootCID, _ := store.Put(ctx, node)

	raw, _ := mem.Get(ctx, rootCID)
	if err := VerifyBlockCID(rootCID, raw); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	tampered := append([]byte(nil), raw...)
	tampered[0] ^= 0xff
	if err := VerifyBlockCID(rootCID, tampered); err == nil {
		t.Fatalf("VerifyBlockCID accepted tampered bytes")
	}
}
