package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
)

// TestTipsetForRandomness_FutureEpochFailsFast covers #82: an epoch far
// above the head (no header store wired, so liveHead == frozen anchor) is
// genuinely in the future and must error promptly without entering the
// bounded wait-for-head loop (which only applies within a small window
// above the live head and only when a header store is present).
func TestTipsetForRandomness_FutureEpochFailsFast(t *testing.T) {
	c := newCAPI() // Trusted.Epoch = 6_000_000, no HeaderStore

	start := time.Now()
	_, err := c.tipsetForRandomness(context.Background(), abi.ChainEpoch(6_000_500))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected future-epoch error, got nil")
	}
	if !strings.Contains(err.Error(), "future epoch") {
		t.Fatalf("expected 'future epoch' error, got: %v", err)
	}
	// Must not have spun in the wait loop: no header store => immediate
	// error, well under the 20s wait budget.
	if elapsed > 2*time.Second {
		t.Fatalf("future-epoch error took %s, expected near-immediate (no wait without header store)", elapsed)
	}
}

// TestTipsetForRandomness_NegativeEpoch guards the basic input validation.
func TestTipsetForRandomness_NegativeEpoch(t *testing.T) {
	c := newCAPI()
	if _, err := c.tipsetForRandomness(context.Background(), abi.ChainEpoch(-1)); err == nil {
		t.Fatal("expected error for negative randomness epoch")
	}
}
