package kamt

import (
	"context"
	"testing"

	"github.com/ipfs/go-cid"
)

// TestWalkSubtree_OnNodeVisitsEveryFetchedNode proves the PDP-tier pinning
// hook: OnNode fires exactly once per successfully fetched+verified node
// (root included), so the prefetcher can pin the whole walked warm set.
func TestWalkSubtree_OnNodeVisitsEveryFetchedNode(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 4)

	seen := map[string]int{}
	stats, err := WalkSubtree(context.Background(), root, store, WalkOptions{
		MaxNodes: 100,
		OnNode:   func(c cid.Cid) { seen[c.KeyString()]++ },
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if stats.NodesFetched == 0 {
		t.Fatal("walk fetched 0 nodes")
	}
	if len(seen) != stats.NodesFetched {
		t.Fatalf("OnNode fired for %d distinct nodes, NodesFetched=%d", len(seen), stats.NodesFetched)
	}
	// Root must be among the pinned/visited nodes.
	if seen[root.KeyString()] == 0 {
		t.Fatal("OnNode never fired for the root node")
	}
	// No node visited more than once (visited-set dedup holds).
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("node %s visited %d times, want 1", k, n)
		}
	}
}

// TestWalkSubtree_OnNodeNilIsNoop: a nil OnNode must not panic (light-node
// path).
func TestWalkSubtree_OnNodeNilIsNoop(t *testing.T) {
	store := newMemBG()
	root := buildTwoLevelTree(t, store, 4)
	if _, err := WalkSubtree(context.Background(), root, store, WalkOptions{MaxNodes: 100, OnNode: nil}); err != nil {
		t.Fatalf("walk with nil OnNode: %v", err)
	}
}
