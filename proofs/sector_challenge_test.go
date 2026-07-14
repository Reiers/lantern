package proofs

import (
	"encoding/hex"
	"testing"
)

// TestGenerateWinningPoStSectorChallenge_ReferenceVectors pins the byte-for-byte
// SHA256 output of the sector-challenge algorithm at three eligible-set sizes.
// All three values were computed against the reference algorithm in
// rust-fil-proofs' storage-proofs-post::fallback::vanilla::generate_sector_challenge:
//
//	h = SHA256(proverID(minerID=1000) || randomness("2a07...") || u64_LE(0))
//	  = 4163488eb8f40d2e...
//	LE_u64(h[0:8]) = 3318577573940192065
//
// mod 1 = 0, mod 10 = 5, mod 100 = 65. Any change to the algorithm (order of
// inputs, hash function, endianness, or challenge-count) breaks these tests
// on purpose.
func TestGenerateWinningPoStSectorChallenge_ReferenceVectors(t *testing.T) {
	const minerID = 1000
	var randomness [32]byte
	rnd, err := hex.DecodeString("2a07000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("decode randomness: %v", err)
	}
	copy(randomness[:], rnd)

	cases := []struct {
		name             string
		eligibleCount    uint64
		expectedChallenge uint64
	}{
		{name: "eligibleSet=1 (matches shipped 2KiB proof vector)", eligibleCount: 1, expectedChallenge: 0},
		{name: "eligibleSet=10", eligibleCount: 10, expectedChallenge: 5},
		{name: "eligibleSet=100", eligibleCount: 100, expectedChallenge: 65},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateWinningPoStSectorChallenge(minerID, randomness, tc.eligibleCount)
			if err != nil {
				t.Fatalf("GenerateWinningPoStSectorChallenge: %v", err)
			}
			if len(got) != WinningPoStSectorCount {
				t.Fatalf("got %d challenges, want %d", len(got), WinningPoStSectorCount)
			}
			if got[0] != tc.expectedChallenge {
				t.Errorf("challenge index = %d, want %d", got[0], tc.expectedChallenge)
			}
		})
	}
}

func TestGenerateWinningPoStSectorChallenge_ProverIDPadding(t *testing.T) {
	// toProverID must produce right-padded varint bytes for the ID-address
	// payload. Miner 1000 encodes as 0xe8 0x07 (varint) → 32-byte prefix.
	p, err := toProverID(1000)
	if err != nil {
		t.Fatalf("toProverID: %v", err)
	}
	want, _ := hex.DecodeString("e807000000000000000000000000000000000000000000000000000000000000")
	if hex.EncodeToString(p[:]) != hex.EncodeToString(want) {
		t.Errorf("prover id = %x, want %x", p[:], want)
	}
}

func TestGenerateWinningPoStSectorChallenge_ZeroEligibleRejected(t *testing.T) {
	var r [32]byte
	if _, err := GenerateWinningPoStSectorChallenge(1000, r, 0); err == nil {
		t.Fatal("expected error for eligible=0, got nil")
	}
}

// TestGenerateWinningPoStSectorChallenge_MatchesShippedProofVector pins that
// the shipped WinningPoSt 2KiB proof's challenge derivation (which had exactly
// one eligible sector, sector 7) resolves to index 0 for the same
// (minerID, randomness) pair. This ties the sector-challenge code to the
// existing groth16-verified proof fixture.
func TestGenerateWinningPoStSectorChallenge_MatchesShippedProofVector(t *testing.T) {
	// From proofs/testdata/winningpost_2kib_vector.json:
	//   miner_id: 1000, randomness: 2a07..., challenged_indices: [0]
	const minerID = 1000
	var randomness [32]byte
	rnd, _ := hex.DecodeString("2a07000000000000000000000000000000000000000000000000000000000000")
	copy(randomness[:], rnd)

	got, err := GenerateWinningPoStSectorChallenge(minerID, randomness, 1)
	if err != nil {
		t.Fatalf("GenerateWinningPoStSectorChallenge: %v", err)
	}
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("got %v, want [0] (matches shipped fixture challenged_indices)", got)
	}
}
