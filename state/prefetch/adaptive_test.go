package prefetch

import (
	"fmt"
	"testing"
)

// TestAddAddr_AdaptiveWarming covers lantern#44 adaptive warming: addresses
// learned from eth_call misses are merged into the walk set, deduped,
// canonicalized, and capped.
func TestAddAddr_AdaptiveWarming(t *testing.T) {
	p := New(Config{Addrs: []string{"0x1111111111111111111111111111111111111111"}}, nil)

	// Learn one address; snapshot should contain static + dynamic.
	p.AddAddr("0x09a0Fdc2723faD1a7b8e3E00ee5DF73841df55A0") // mixed case
	snap := p.addrSnapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (1 static + 1 dynamic): %v", len(snap), snap)
	}

	// Idempotent: re-adding (any case) doesn't grow the set.
	p.AddAddr("0x09a0fdc2723fad1a7b8e3e00ee5df73841df55a0") // lowercase
	p.AddAddr("0x09A0FDC2723FAD1A7B8E3E00EE5DF73841DF55A0") // uppercase
	if got := len(p.addrSnapshot()); got != 2 {
		t.Fatalf("after dup adds, snapshot len = %d, want 2", got)
	}

	// Unparseable input is ignored.
	p.AddAddr("not-an-address")
	p.AddAddr("")
	if got := len(p.addrSnapshot()); got != 2 {
		t.Fatalf("after junk adds, snapshot len = %d, want 2", got)
	}

	// Cap: adding beyond maxDynAddrs distinct addresses stops growing.
	for i := 0; i < maxDynAddrs+10; i++ {
		p.AddAddr(fmt.Sprintf("0x%040x", i+0x1000))
	}
	dyn := len(p.addrSnapshot()) - 1 // minus the 1 static
	if dyn > maxDynAddrs {
		t.Fatalf("dynamic set grew past cap: %d > %d", dyn, maxDynAddrs)
	}
}

// TestAddAddr_NilSafe: AddAddr on a nil prefetcher is a no-op (mirrors
// the nil-safety of Trigger; curio-core may wire OnLocalMiss before the
// prefetcher exists in degenerate configs).
func TestAddAddr_NilSafe(t *testing.T) {
	var p *Prefetcher
	p.AddAddr("0x1111111111111111111111111111111111111111") // must not panic
}
