package anchor

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedMainnetLoads(t *testing.T) {
	a, err := Embedded("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	if a.Network != "mainnet" {
		t.Fatalf("expected mainnet, got %q", a.Network)
	}
	if a.Instance == 0 {
		t.Fatal("instance is 0; expected captured GPBFT instance")
	}
	if len(a.Entries) < 100 {
		t.Fatalf("expected >=100 entries, got %d", len(a.Entries))
	}
	pt, err := a.PowerTable()
	if err != nil {
		t.Fatalf("materialise power table: %v", err)
	}
	if got := len(pt.Entries); got != len(a.Entries) {
		t.Fatalf("power table entry count mismatch: anchor=%d table=%d", len(a.Entries), got)
	}
}

func TestUnknownNetwork(t *testing.T) {
	if _, err := Embedded("nonsense"); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

func TestRoundtripJSON(t *testing.T) {
	a, err := Embedded("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var a2 Anchor
	if err := json.Unmarshal(b, &a2); err != nil {
		t.Fatal(err)
	}
	if !a.Equal(&a2) {
		t.Fatal("roundtrip mismatch")
	}
}
