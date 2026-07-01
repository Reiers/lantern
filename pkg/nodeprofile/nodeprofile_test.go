package nodeprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsLight(t *testing.T) {
	p, err := Load(t.TempDir(), "mainnet")
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if p.Tier != TierLight {
		t.Fatalf("missing profile => tier %q, want light", p.Tier)
	}
	if p.UsesPersistentCache() {
		t.Fatal("light tier must not use persistent cache")
	}
	if p.CacheBytes() != 0 {
		t.Fatalf("light CacheBytes=%d, want 0", p.CacheBytes())
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	want := Profile{Tier: TierPDP, AllowBlockSubmit: true}
	if err := Save(home, "mainnet", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(home, "mainnet")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Tier != TierPDP {
		t.Fatalf("tier=%q want pdp", got.Tier)
	}
	if !got.UsesPersistentCache() {
		t.Fatal("pdp must use persistent cache")
	}
	if got.CacheBytes() != DefaultPDPCacheBytes {
		t.Fatalf("pdp CacheBytes=%d want default %d", got.CacheBytes(), DefaultPDPCacheBytes)
	}
	if !got.AllowBlockSubmit {
		t.Fatal("AllowBlockSubmit lost across round trip")
	}
}

func TestExplicitBudgetHonored(t *testing.T) {
	home := t.TempDir()
	const budget = 5 << 30
	if err := Save(home, "calibration", Profile{Tier: TierPDP, PersistentCacheBytes: budget}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home, "calibration")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheBytes() != budget {
		t.Fatalf("CacheBytes=%d want %d", got.CacheBytes(), budget)
	}
}

func TestUnknownTierNormalizesToLight(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "mainnet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{"tier":"banana"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home, "mainnet")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Tier != TierLight {
		t.Fatalf("unknown tier => %q, want light", got.Tier)
	}
}

func TestMalformedIsError(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "mainnet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, "mainnet"); err == nil {
		t.Fatal("expected error on malformed profile (must not silently downgrade)")
	}
}
