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

// TestFetcher_RaceTier_FastestWins: when two race sources are configured
// and one responds quickly while the other stalls, the fast one wins and
// the slow goroutine is cancelled. Total latency is bounded by the fast
// source, not by the slow source's timeout. This is the issue #3 fix.
func TestFetcher_RaceTier_FastestWins(t *testing.T) {
	raw := []byte{0x82, 0x41, 0x01, 0x41, 0x02}
	c, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)

	// Slow gateway: takes 2 seconds to respond.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.Write(raw)
		case <-r.Context().Done():
			return
		}
	}))
	defer slow.Close()

	// Fast gateway: responds immediately.
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(raw)
	}))
	defer fast.Close()

	cache := hamt.NewMemBlockStore()
	f := New(cache,
		Source{Name: "slow", Getter: hsync.NewClient([]string{slow.URL}, 5*time.Second), Timeout: 5 * time.Second, Race: true},
		Source{Name: "fast", Getter: hsync.NewClient([]string{fast.URL}, 5*time.Second), Timeout: 5 * time.Second, Race: true},
	)

	start := time.Now()
	got, err := f.Get(context.Background(), c)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("value mismatch")
	}
	// Must complete in well under the slow source's response time.
	if elapsed >= time.Second {
		t.Fatalf("race fetch took %v; expected < 1s (fast source should have won)", elapsed)
	}
	stats := f.Stats()
	if stats["fast"] != 1 {
		t.Fatalf("expected fast=1, got %+v", stats)
	}
}

// TestFetcher_RaceTier_FallsBackOnRaceFail: when all race sources fail, the
// fetcher must fall through to the non-Race fallback tier.
func TestFetcher_RaceTier_FallsBackOnRaceFail(t *testing.T) {
	raw := []byte{0x82, 0x41, 0x01, 0x41, 0x02}
	c, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)

	// All race sources 404.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer dead.Close()

	// Fallback gateway: serves the block.
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block/"+c.String() {
			w.Write(raw)
			return
		}
		http.NotFound(w, r)
	}))
	defer fb.Close()

	cache := hamt.NewMemBlockStore()
	f := New(cache,
		Source{Name: "dead-race-1", Getter: hsync.NewClient([]string{dead.URL}, time.Second), Timeout: time.Second, Race: true},
		Source{Name: "dead-race-2", Getter: hsync.NewClient([]string{dead.URL}, time.Second), Timeout: time.Second, Race: true},
		Source{Name: "fallback", Getter: hsync.NewClient([]string{fb.URL}, time.Second), Timeout: time.Second},
	)

	got, err := f.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("value mismatch")
	}
	stats := f.Stats()
	if stats["fallback"] != 1 {
		t.Fatalf("expected fallback=1, got %+v", stats)
	}
}

// TestFetcher_RaceTier_AllFail: when every source (race + fallback) fails,
// the fetcher returns an error and records a miss.
func TestFetcher_RaceTier_AllFail(t *testing.T) {
	raw := []byte{0x01}
	c, _ := cid.V1Builder{Codec: 0x71, MhType: 0x12, MhLength: 32}.Sum(raw)

	cache := hamt.NewMemBlockStore()
	f := New(cache,
		Source{Name: "race-bitswap", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond, Race: true},
		Source{Name: "race-gw", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond, Race: true},
		Source{Name: "fallback", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond},
	)
	_, err := f.Get(context.Background(), c)
	if err == nil {
		t.Fatalf("expected error when all sources fail")
	}
	stats := f.Stats()
	if stats["misses"] != 1 {
		t.Fatalf("expected misses=1, got %+v", stats)
	}
}
