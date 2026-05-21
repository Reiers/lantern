package hsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// makeCID returns a deterministic raw-codec CID for some bytes.
func makeCID(t *testing.T, data []byte) cid.Cid {
	t.Helper()
	h, err := mh.Sum(data, mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, h)
}

func TestClient_RetriesOn5xxThenSucceeds(t *testing.T) {
	payload := []byte("hello world")
	c := makeCID(t, payload)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			http.Error(w, "transient", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.raw")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	cl := NewClient([]string{srv.URL}, 5*time.Second)
	cl.SetRetries(3)
	got, err := cl.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("expected success after retries, got %v (hits=%d)", err, hits.Load())
	}
	if string(got) != "hello world" {
		t.Fatalf("payload mismatch: %q", got)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", hits.Load())
	}
}

func TestClient_4xxDoesNotRetry(t *testing.T) {
	c := makeCID(t, []byte("x"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	cl := NewClient([]string{srv.URL}, 2*time.Second)
	cl.SetRetries(5)
	_, err := cl.Get(context.Background(), c)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 attempt for 4xx, got %d", hits.Load())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 in error, got %v", err)
	}
}

func TestClient_FallsThroughEndpoints(t *testing.T) {
	payload := []byte("from-second")
	c := makeCID(t, payload)

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv2.Close()

	cl := NewClient([]string{srv1.URL, srv2.URL}, 2*time.Second)
	cl.SetRetries(1)
	got, err := cl.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("expected success via fallback, got %v", err)
	}
	if string(got) != "from-second" {
		t.Fatalf("expected fallback payload, got %q", got)
	}
}
