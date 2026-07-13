package proofs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyWinningPoStByType exercises the high-level, filecoin-ffi-style
// entry point: proof type -> load VK from a param cache dir -> verify.
// It stages the committed testdata VK under its canonical 2KiB filename
// in a temp "param cache" and verifies the real reference proof.
func TestVerifyWinningPoStByType(t *testing.T) {
	v, randomness, proof, commR := loadWinningVector(t)

	p, err := WinningPoStParams(StackedDrgWinning2KiBV1)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.SectorSize != 2048 {
		t.Fatalf("2KiB sector size %d, want 2048", p.SectorSize)
	}

	// Stage the VK under its canonical name in a temp param cache.
	cache := t.TempDir()
	src, err := os.ReadFile("testdata/winning_2kib.vk")
	if err != nil {
		t.Fatalf("read testdata vk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cache, p.VKFile), src, 0o644); err != nil {
		t.Fatalf("stage vk: %v", err)
	}

	err = VerifyWinningPoStByType(cache, StackedDrgWinning2KiBV1, randomness, proof,
		[]WinningPoStSector{{SectorNumber: v.SectorNumber, CommR: commR}})
	if err != nil {
		t.Fatalf("VerifyWinningPoStByType rejected the real proof: %v", err)
	}
	t.Log("VerifyWinningPoStByType (load VK from cache + verify) ACCEPTED the real proof ✓")
}

// TestWinningPoStParamTableComplete: all 5 winning proof types are mapped
// with plausible sector sizes.
func TestWinningPoStParamTableComplete(t *testing.T) {
	want := map[RegisteredPoStProof]uint64{
		StackedDrgWinning2KiBV1:   1 << 11,
		StackedDrgWinning8MiBV1:   1 << 23,
		StackedDrgWinning512MiBV1: 1 << 29,
		StackedDrgWinning32GiBV1:  1 << 35,
		StackedDrgWinning64GiBV1:  1 << 36,
	}
	for pt, sz := range want {
		p, err := WinningPoStParams(pt)
		if err != nil {
			t.Errorf("proof type %d: %v", pt, err)
			continue
		}
		if p.SectorSize != sz {
			t.Errorf("proof type %d sector size %d, want %d", pt, p.SectorSize, sz)
		}
		if p.VKFile == "" {
			t.Errorf("proof type %d has no VK file", pt)
		}
	}
}
