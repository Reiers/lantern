package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// testCID returns a distinct, defined CID for test n.
func testCID(t *testing.T, n byte) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte{n, n, n, n}, mh.SHA2_256, -1)
	if err != nil {
		t.Fatalf("multihash: %v", err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

// newTestWarmer builds a warmer with fast timings around an injected search.
func newTestWarmer(ctx context.Context, search searchFn) *sendWarmer {
	w := newSendWarmerWithSearch(ctx, search)
	w.pollInterval = 1 * time.Millisecond
	w.maxDuration = 2 * time.Second
	return w
}

// TestWarm_StopsOnFound: the loop ends as soon as the search reports found,
// and does not keep polling afterward.
func TestWarm_StopsOnFound(t *testing.T) {
	var calls int32
	foundAfter := int32(3)
	done := make(chan struct{})

	w := newTestWarmer(context.Background(), func(_ context.Context, _ cid.Cid) (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n >= foundAfter {
			close(done)
			return true, nil
		}
		return false, nil // not yet on-chain
	})

	w.Warm(testCID(t, 1))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("warm loop never reported found")
	}

	// Give the goroutine a beat to exit, then confirm it stopped polling.
	time.Sleep(20 * time.Millisecond)
	stable := atomic.LoadInt32(&calls)
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != stable {
		t.Fatalf("warmer kept polling after found: %d -> %d", stable, got)
	}
	if w.activeCount() != 0 {
		t.Fatalf("active count not drained: %d", w.activeCount())
	}
}

// TestWarm_Dedup: concurrent Warm calls for the SAME cid run a single loop.
func TestWarm_Dedup(t *testing.T) {
	release := make(chan struct{})
	var distinct sync.Map
	var running int32

	w := newTestWarmer(context.Background(), func(_ context.Context, c cid.Cid) (bool, error) {
		distinct.Store(c, true)
		atomic.AddInt32(&running, 1)
		<-release // hold the loop open so dedup is observable
		return true, nil
	})

	c := testCID(t, 2)
	for i := 0; i < 8; i++ {
		w.Warm(c)
	}
	// Let the single goroutine enter search.
	time.Sleep(30 * time.Millisecond)

	if w.activeCount() != 1 {
		t.Fatalf("expected exactly 1 active warm for a deduped cid, got %d", w.activeCount())
	}
	if got := atomic.LoadInt32(&running); got != 1 {
		t.Fatalf("expected 1 concurrent search, got %d", got)
	}
	close(release)
}

// TestWarm_ConcurrencyCap: at most maxConcurrent loops run at once; extra
// sends are dropped (no goroutine), preserving correctness without unbounded
// fan-out.
func TestWarm_ConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	w := newTestWarmer(context.Background(), func(_ context.Context, _ cid.Cid) (bool, error) {
		<-release
		return true, nil
	})
	w.maxConcurrent = 4

	for i := 0; i < 10; i++ {
		w.Warm(testCID(t, byte(100+i)))
	}
	time.Sleep(30 * time.Millisecond)

	if got := w.activeCount(); got != 4 {
		t.Fatalf("expected active capped at 4, got %d", got)
	}
	close(release)
}

// TestWarm_ContextCancelStops: cancelling the root context ends the loop even
// if the search never reports found.
func TestWarm_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})

	w := newSendWarmerWithSearch(ctx, func(sctx context.Context, _ cid.Cid) (bool, error) {
		return false, nil // never lands
	})
	w.pollInterval = 1 * time.Millisecond
	w.maxDuration = time.Hour // would otherwise run a long time

	// Wrap run completion detection via a tiny shim cid.
	go func() {
		w.Warm(testCID(t, 9))
		// Warm spawns its own goroutine; poll activeCount to detect exit.
		for {
			if w.activeCount() == 0 {
				close(exited)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("warm loop did not stop on context cancel")
	}
}

// TestWarm_NilSafe: a nil warmer and a nil-search warmer are both no-ops.
func TestWarm_NilSafe(t *testing.T) {
	var nilW *sendWarmer
	nilW.Warm(testCID(t, 1)) // must not panic

	inert := newSendWarmer(context.Background(), nil) // nil chain -> inert
	inert.Warm(testCID(t, 1))
	if inert.activeCount() != 0 {
		t.Fatalf("inert warmer started a loop: active=%d", inert.activeCount())
	}

	// Undefined CID is ignored.
	w := newTestWarmer(context.Background(), func(_ context.Context, _ cid.Cid) (bool, error) {
		t.Fatal("search called for undefined cid")
		return false, nil
	})
	w.Warm(cid.Undef)
}

// activeCount is a test helper exposing the live warm count.
func (w *sendWarmer) activeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}
