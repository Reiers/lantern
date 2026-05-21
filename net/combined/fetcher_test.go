package combined

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/hsync"
	"github.com/Reiers/lantern/state/hamt"
)

// TestFetcher_FallbackOrder confirms: cache hit short-circuits; otherwise
// bitswap stub fails fast, HTTP gateway serves the block, cache is
// populated, and stats record the source.
func TestFetcher_FallbackOrder(t *testing.T) {
	// 1. Generate a fixed test CID + bytes.
	raw := []byte{0x82, 0x41, 0x01, 0x41, 0x02} // valid CBOR: [b'\x01', b'\x02']
	c, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)

	// 2. Spin up a fake gateway that serves /block/{cid}.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block/"+c.String() {
			w.Header().Set("Content-Type", "application/vnd.ipld.raw")
			w.Write(raw)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cache := hamt.NewMemBlockStore()
	httpClient := hsync.NewClient([]string{srv.URL}, 3*time.Second)

	f := New(cache,
		Source{Name: "bitswap", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond},
		Source{Name: "http", Getter: httpClient, Timeout: 3 * time.Second},
	)

	// 3. First fetch: cache miss, bitswap stub fails, HTTP succeeds.
	got, err := f.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("first Get value mismatch")
	}
	stats := f.Stats()
	if stats["http"] != 1 {
		t.Fatalf("expected http=1 hit, got stats=%+v", stats)
	}
	if stats["cache"] != 0 {
		t.Fatalf("expected cache=0 on first miss, got %+v", stats)
	}

	// 4. Second fetch: cache hit, no upstream calls.
	got2, err := f.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if !bytes.Equal(got2, raw) {
		t.Fatalf("second Get value mismatch")
	}
	stats = f.Stats()
	if stats["cache"] != 1 {
		t.Fatalf("expected cache=1 on second fetch, got %+v", stats)
	}
	if stats["http"] != 1 {
		t.Fatalf("http count should remain at 1, got %+v", stats)
	}
}

func TestFetcher_AllSourcesFail(t *testing.T) {
	cache := hamt.NewMemBlockStore()
	f := New(cache,
		Source{Name: "bitswap", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond},
	)
	raw := []byte{0x01}
	c, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)
	_, err := f.Get(context.Background(), c)
	if err == nil {
		t.Fatalf("expected fetch error, got nil")
	}
	stats := f.Stats()
	if stats["misses"] != 1 {
		t.Fatalf("expected misses=1, got %+v", stats)
	}
}

func TestFetcher_RejectsCIDMismatch(t *testing.T) {
	// Gateway returns bytes that don't hash to the requested CID; the
	// fetcher must reject them and not poison the cache.
	raw := []byte{0x82, 0x41, 0x01, 0x41, 0x02}
	good, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)
	// Construct a different CID over different bytes, but the server returns
	// the original raw for *both* paths.
	other := []byte{0x82, 0x41, 0xff, 0x41, 0xff}
	bad, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(other)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(raw) // always serve the same bytes (which only match `good`'s CID).
	}))
	defer srv.Close()

	cache := hamt.NewMemBlockStore()
	f := New(cache, Source{
		Name:    "http",
		Getter:  hsync.NewClient([]string{srv.URL}, time.Second),
		Timeout: time.Second,
	})

	// Ask for the BAD CID — server returns the good bytes; both layers (the
	// client and the combined fetcher) should reject.
	_, err := f.Get(context.Background(), bad)
	if err == nil {
		t.Fatalf("expected CID-mismatch rejection, got nil")
	}
	if cache.Has(bad) {
		t.Fatalf("cache poisoned with bad CID!")
	}
	if cache.Has(good) {
		t.Fatalf("cache should not contain `good` CID either — fetcher never requested it")
	}
}
