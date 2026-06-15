package amt

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/state/hamt"
)

// realV2RootBytes is the actual on-chain BLS message AMT root captured from
// calibration block bafy2bzaceacm7zhissw at height 3807124:
//
//	CID bafy2bzacedswlcz5ddgqnyo3sak3jmhmkxashisnlpq6ujgyhe4mlobzpnhs6
//	raw 8300008341008080
//
// Decoded: 0x83 = array of 3 = [Height=0, Count=0, Node=[bitmap, vals, links]].
// This is the legacy v2 AMT root shape (3 fields, no stored BitWidth). The v4
// loader rejects it with "cbor input had wrong number of fields"; the v2 loader
// must accept it. This is the exact regression behind #49.
const realV2RootHex = "8300008341008080"

// TestV2RootDecodes proves the captured legacy 3-field AMT root loads via the
// v2 path (and that the v4 path rejects it, documenting the original bug).
func TestV2RootDecodes(t *testing.T) {
	ctx := context.Background()

	raw, err := hex.DecodeString(realV2RootHex)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}

	// Build the canonical CID for these bytes (dag-cbor, blake2b-256) and
	// stash them in an in-memory blockstore.
	mem := hamt.NewMemBlockStore()
	pref := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: mh.BLAKE2B_MIN + 31 /* blake2b-256 */, MhLength: 32}
	c, err := pref.Sum(raw)
	if err != nil {
		t.Fatalf("cid sum: %v", err)
	}
	mem.Put(c, raw)

	// v4 loader must FAIL (documents the original #49 bug).
	if _, _, err := Lookup(ctx, c, 0, mem, nil); err == nil {
		t.Fatalf("v4 Lookup unexpectedly succeeded on a v2 root; the regression guard is meaningless")
	}

	// v2 loader must LOAD the root (this empty AMT has no element at idx 0,
	// so we expect ErrNotFound, NOT a decode error).
	_, _, err = LookupV2(ctx, c, 0, mem)
	if err != ErrNotFound {
		t.Fatalf("v2 LookupV2 on empty root: want ErrNotFound, got %v", err)
	}

	// ForEachV2CIDs on an empty AMT returns no CIDs and no error.
	cids, err := ForEachV2CIDs(ctx, c, mem)
	if err != nil {
		t.Fatalf("ForEachV2CIDs: %v", err)
	}
	if len(cids) != 0 {
		t.Fatalf("ForEachV2CIDs on empty AMT: want 0 cids, got %d", len(cids))
	}
}
