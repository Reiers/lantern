// Tests for the local eth_feeHistory path (lantern#76): base-fee history
// served from the header store, with bridge fallback when no store or the
// range is out of local coverage.

package handlers

import (
	"context"
	"testing"
)

func TestParseEthUintDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  uint64
		want uint64
	}{
		{"", 7, 7},        // empty -> default
		{"  ", 7, 7},      // whitespace -> default
		{"0x10", 0, 16},   // hex quantity
		{"0X1f4", 0, 500}, // upper 0X hex
		{"500", 0, 500},   // plain decimal
		{"0x0", 9, 0},     // explicit zero hex
		{"garbage", 3, 3}, // unparseable -> default
		{"-5", 4, 4},      // negative -> default (QUANTITY is unsigned)
		{"0xzz", 2, 2},    // bad hex -> default
	}
	for _, tc := range cases {
		if got := parseEthUintDefault(tc.in, tc.def); got != tc.want {
			t.Errorf("parseEthUintDefault(%q, %d) = %d, want %d", tc.in, tc.def, got, tc.want)
		}
	}
}

// TestLocalEthFeeHistory_NoStoreFallsBack: with no header store wired, the
// local path must decline (served==false) so EthFeeHistory falls through
// to the bridge. This is the boundary that keeps a store-less light node
// working via the bridge instead of returning an empty history.
func TestLocalEthFeeHistory_NoStoreFallsBack(t *testing.T) {
	c := newCAPI() // no HeaderStore
	out, served, err := c.localEthFeeHistory(context.Background(), "0x5", "latest", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if served {
		t.Fatalf("expected served=false with no header store, got served=true (out=%v)", out)
	}
}

// TestLocalEthFeeHistory_ZeroCountFallsBack: a zero blockCount is not a
// serveable request; decline so the bridge (or caller) handles it.
func TestLocalEthFeeHistory_ZeroCountFallsBack(t *testing.T) {
	c := newCAPI()
	_, served, err := c.localEthFeeHistory(context.Background(), "0x0", "latest", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if served {
		t.Fatal("expected served=false for zero blockCount")
	}
}
