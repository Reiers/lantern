package cache

import (
	"context"
	"fmt"
	"testing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/net/combined"
)

// compile-time: the persistent cache is a drop-in combined.Cache. Kept in
// the test package so production state/cache has no import cycle back to
// net/combined.
var _ combined.Cache = (*Store)(nil)

func mkCID(t *testing.T, data []byte) cid.Cid {
	t.Helper()
	h, err := mh.Sum(data, mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, h)
}

func TestPutGetHas(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	data := []byte("hello lantern")
	c := mkCID(t, data)
	s.Put(c, data)

	if !s.Has(c) {
		t.Fatal("Has=false after Put")
	}
	got, err := s.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get=%q want %q", got, data)
	}
	if _, err := s.Get(context.Background(), mkCID(t, []byte("absent"))); err == nil {
		t.Fatal("expected miss for absent CID")
	}
}

func TestPutVerifyRejectsMismatch(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	data := []byte("real bytes")
	c := mkCID(t, data)
	// Correct bytes accepted.
	if err := s.PutVerify(c, data); err != nil {
		t.Fatalf("PutVerify(correct): %v", err)
	}
	// Wrong bytes under the same CID rejected (the no-trust guarantee).
	if err := s.PutVerify(c, []byte("tampered")); err == nil {
		t.Fatal("PutVerify accepted CID/bytes mismatch")
	}
}

// TestPersistsAcrossReopen is the PDP-tier guarantee: the warm set survives
// a restart (this is why the persistent cache exists vs MemBlockStore).
func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	data := []byte("warm contract subtree node")
	c := mkCID(t, data)

	s1, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s1.Put(c, data)
	if err := s1.Pin(c); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: block must still be present, and live-bytes recomputed > 0.
	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !s2.Has(c) {
		t.Fatal("block did NOT survive reopen (persistence broken)")
	}
	got, err := s2.Get(context.Background(), c)
	if err != nil || string(got) != string(data) {
		t.Fatalf("post-reopen Get=%q err=%v", got, err)
	}
	if s2.Stats().LiveBytes <= 0 {
		t.Fatal("live-bytes not recovered on reopen")
	}
}

// TestEvictionRespectsSoftCapAndPins: over-cap inserts trigger LRU
// eviction of unpinned blocks; pinned blocks survive.
func TestEvictionRespectsSoftCapAndPins(t *testing.T) {
	// Tiny cap so a handful of blocks trips eviction deterministically.
	s, err := Open(t.TempDir(), Options{SoftCapBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	block := make([]byte, 512)

	// One pinned block we must never lose.
	pinData := append([]byte("PIN"), block...)
	pinCID := mkCID(t, pinData)
	s.Put(pinCID, pinData)
	if err := s.Pin(pinCID); err != nil {
		t.Fatal(err)
	}

	// Insert well past the cap with distinct unpinned blocks (synchronous
	// eviction via direct call to avoid racing the background goroutine).
	var cids []cid.Cid
	for i := 0; i < 40; i++ {
		d := append([]byte(fmt.Sprintf("blk-%d-", i)), block...)
		c := mkCID(t, d)
		s.Put(c, d)
		cids = append(cids, c)
	}
	s.evictNow() // blocking, deterministic pass

	// Pinned block must survive.
	if !s.Has(pinCID) {
		t.Fatal("pinned block was evicted (pin not honored)")
	}
	// Live bytes should be at/under ~cap (allow the pinned floor).
	if lb := s.Stats().LiveBytes; lb > s.softCap {
		t.Fatalf("live bytes %d exceeds soft cap %d after eviction", lb, s.softCap)
	}
	if s.Stats().Evictions == 0 {
		t.Fatal("expected at least one eviction over cap")
	}
}
