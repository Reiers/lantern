package proofs

// Vector-matched WinningPoSt test (lantern#88).
//
// testdata/winningpost_2kib_vector.json is a REAL WinningPoSt proof
// generated + verified by filecoin-ffi v1.36.0 (the Rust reference) over
// a freshly-sealed 2KiB sector. testdata/winning_2kib.vk is the official
// Filecoin v28 winning-post 2KiB verifying key (from the proof params,
// CID QmSTCXF2ipGA3f6muVo6kHc2URSx6PzZxGUqu7uykaH5KU).
//
// If our pure-Go verifier accepts this proof (and rejects tampered
// variants), the challenge derivation + public-input layout + VK parsing
// + Groth16 pairing check all match the reference bit-for-bit.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/ipfs/go-cid"
)

type winningVector struct {
	SealProofType  int      `json:"seal_proof_type"`
	PostProofType  int      `json:"post_proof_type"`
	MinerID        uint64   `json:"miner_id"`
	SectorNumber   uint64   `json:"sector_number"`
	Randomness     string   `json:"randomness_hex"`
	SealedCID      string   `json:"sealed_cid"`
	SealedCIDBytes string   `json:"sealed_cid_bytes_hex"`
	ProofBytes     string   `json:"proof_bytes_hex"`
	ChallengeIdx   []uint64 `json:"challenged_indices"`
}

func loadWinningVector(t *testing.T) (winningVector, [32]byte, []byte, [32]byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/winningpost_2kib_vector.json")
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var v winningVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	rnd, err := hex.DecodeString(v.Randomness)
	if err != nil || len(rnd) != 32 {
		t.Fatalf("randomness hex: %v (len %d)", err, len(rnd))
	}
	var randomness [32]byte
	copy(randomness[:], rnd)

	proof, err := hex.DecodeString(v.ProofBytes)
	if err != nil {
		t.Fatalf("proof hex: %v", err)
	}

	// comm_r = the 32-byte digest of the sealed CID (little-endian Fr).
	c, err := cid.Decode(v.SealedCID)
	if err != nil {
		t.Fatalf("decode sealed cid: %v", err)
	}
	digest := c.Hash()
	// multihash: <code><len><digest>; take the trailing 32 bytes.
	var commR [32]byte
	copy(commR[:], digest[len(digest)-32:])

	return v, randomness, proof, commR
}

func loadWinningVK(t *testing.T) *Groth16VerifyingKey {
	t.Helper()
	f, err := os.Open("testdata/winning_2kib.vk")
	if err != nil {
		t.Fatalf("open vk: %v", err)
	}
	defer f.Close()
	vk, err := ParseGroth16VerifyingKey(f)
	if err != nil {
		t.Fatalf("parse vk: %v", err)
	}
	return vk
}

// TestWinningVKParses: the real Filecoin winning-2KiB VK must parse, and
// its IC length must be 133 (132 public inputs = 66 x [comm_r, leaf]).
func TestWinningVKParses(t *testing.T) {
	vk := loadWinningVK(t)
	if len(vk.IC) != 133 {
		t.Fatalf("vk IC length %d, want 133", len(vk.IC))
	}
	t.Logf("winning-2KiB VK parsed: IC length %d (132 public inputs)", len(vk.IC))
}

// TestVerifyWinningPoStRealVector: THE test. Our pure-Go verifier must
// accept the real filecoin-ffi-generated proof.
func TestVerifyWinningPoStRealVector(t *testing.T) {
	v, randomness, proof, commR := loadWinningVector(t)
	vk := loadWinningVK(t)

	// StackedDrgWinning2KiBV1 (post type 0) is API version 1.1.0.
	info := WinningPoStVerifyInfo{
		Randomness: randomness,
		Proof:      proof,
		Sectors: []WinningPoStSector{{
			SectorNumber: v.SectorNumber,
			CommR:        commR,
		}},
		SectorSize: 2048,
		APIVersion: APIVersion1_1_0,
	}

	err := VerifyWinningPoSt(vk, info)
	if err != nil {
		t.Fatalf("pure-Go VerifyWinningPoSt REJECTED a valid reference proof: %v", err)
	}
	t.Log("pure-Go VerifyWinningPoSt ACCEPTED the real filecoin-ffi proof ✓")
}

// TestVerifyWinningPoStRejectsTamperedProof: flip a byte in the proof;
// must reject.
func TestVerifyWinningPoStRejectsTamperedProof(t *testing.T) {
	v, randomness, proof, commR := loadWinningVector(t)
	vk := loadWinningVK(t)

	bad := append([]byte(nil), proof...)
	bad[0] ^= 0x01 // flip a bit in proof.A

	info := WinningPoStVerifyInfo{
		Randomness: randomness,
		Proof:      bad,
		Sectors:    []WinningPoStSector{{SectorNumber: v.SectorNumber, CommR: commR}},
		SectorSize: 2048,
		APIVersion: APIVersion1_1_0,
	}
	if err := VerifyWinningPoSt(vk, info); err == nil {
		t.Fatal("tampered proof was ACCEPTED (should reject)")
	}
}

// TestVerifyWinningPoStRejectsWrongRandomness: change randomness; the
// challenges shift, so the proof must no longer verify.
func TestVerifyWinningPoStRejectsWrongRandomness(t *testing.T) {
	v, randomness, proof, commR := loadWinningVector(t)
	vk := loadWinningVK(t)

	randomness[0] ^= 0x01

	info := WinningPoStVerifyInfo{
		Randomness: randomness,
		Proof:      proof,
		Sectors:    []WinningPoStSector{{SectorNumber: v.SectorNumber, CommR: commR}},
		SectorSize: 2048,
		APIVersion: APIVersion1_1_0,
	}
	if err := VerifyWinningPoSt(vk, info); err == nil {
		t.Fatal("proof verified under wrong randomness (should reject)")
	}
}

// TestVerifyWinningPoStRejectsWrongCommR: change comm_r; must reject.
func TestVerifyWinningPoStRejectsWrongCommR(t *testing.T) {
	v, randomness, proof, commR := loadWinningVector(t)
	vk := loadWinningVK(t)

	commR[0] ^= 0x01

	info := WinningPoStVerifyInfo{
		Randomness: randomness,
		Proof:      proof,
		Sectors:    []WinningPoStSector{{SectorNumber: v.SectorNumber, CommR: commR}},
		SectorSize: 2048,
		APIVersion: APIVersion1_1_0,
	}
	if err := VerifyWinningPoSt(vk, info); err == nil {
		t.Fatal("proof verified under wrong comm_r (should reject)")
	}
}
