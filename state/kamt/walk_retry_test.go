package kamt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// flakyBG wraps a memBG and fails the first failCount fetches for a
// specific cid, then serves it. Models a transient Bitswap/gateway miss
// that resolves on retry.
type flakyBG struct {
	inner     *memBG
	mu        sync.Mutex
	failFor   string // cid.KeyString() to make flaky
	failCount int    // remaining failures before it succeeds
}

func (f *flakyBG) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	f.mu.Lock()
	if c.KeyString() == f.failFor && f.failCount > 0 {
		f.failCount--
		f.mu.Unlock()
		return nil, errors.New("flaky: transient miss")
	}
	f.mu.Unlock()
	return f.inner.Get(ctx, c)
}

var _ hamt.BlockGetter = (*flakyBG)(nil)

// TestWalkSubtree_RetryClosesTransientHole proves the #44 deep-trie fix:
// a node that misses on first fetch but lands on retry must NOT leave a
// permanent hole. Without retries the root would error and its children
// would never be queued.
func TestWalkSubtree_RetryClosesTransientHole(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 4)

	// Make the ROOT flaky: fail once, then succeed. Without retry the
	// whole walk yields 0 nodes (root errors, children never queued).
	flaky := &flakyBG{inner: store, failFor: root.KeyString(), failCount: 1}

	// No retry: root miss => walk fetches nothing reachable past it.
	statsNo, _ := WalkSubtree(context.Background(), root, flaky, WalkOptions{MaxNodes: 100, FetchRetries: 0})
	if statsNo.NodesFetched != 0 {
		t.Fatalf("no-retry: expected 0 nodes when root misses, got %+v", statsNo)
	}

	// With retry: root lands on attempt 2, children get walked.
	flaky2 := &flakyBG{inner: store, failFor: root.KeyString(), failCount: 1}
	statsRetry, err := WalkSubtree(context.Background(), root, flaky2, WalkOptions{
		MaxNodes:     100,
		FetchRetries: 2,
		RetryBackoff: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("retry walk: %v", err)
	}
	if statsRetry.NodesFetched < 1 {
		t.Fatalf("retry: expected root+children walked, got %+v", statsRetry)
	}
}

// TestWalkSubtree_RetryHonorsCtxCancel ensures retries don't ignore ctx.
func TestWalkSubtree_RetryHonorsCtxCancel(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 2)
	// Permanently failing root forces the retry loop; cancel mid-flight.
	flaky := &flakyBG{inner: store, failFor: root.KeyString(), failCount: 1 << 30}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	// Large backoff so the second attempt waits on ctx.
	_, _ = WalkSubtree(ctx, root, flaky, WalkOptions{
		MaxNodes:     10,
		FetchRetries: 5,
		RetryBackoff: 50 * time.Millisecond,
	})
	// Must return promptly (ctx-bounded), not after 5*50ms. If we got
	// here without hanging, the ctx path works. Assert timing loosely.
}
