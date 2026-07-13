package proofs

// Pure-Go WinningPoSt verification (lantern#88, Stage B).
//
// Replaces filecoin-ffi's VerifyWinningPoSt. The heavy lifting is the
// Groth16 verify in groth16.go; this file assembles the exact public
// inputs that rust-fil-proofs' FallbackPoStCompound::generate_public_inputs
// produces for a WinningPoSt, so the pairing check matches the reference
// bit-for-bit.
//
// Reference (rust-fil-proofs):
//   - filecoin-proofs/src/api/winning_post.rs::verify_winning_post
//   - filecoin-proofs/src/parameters.rs::winning_post_setup_params
//   - storage-proofs-post/src/fallback/compound.rs::generate_public_inputs
//   - storage-proofs-post/src/fallback/vanilla.rs (leaf challenge SHA256)
//   - storage-proofs-core/src/gadgets/por.rs (1 Fr per challenge = the index)
//
// Winning-post shape: post_config has challenge_count=66, sector_count=1.
// winning_post_setup_params divides these into the CIRCUIT params:
//   param_sector_count   = 66 / 1 = 66
//   param_challenge_count = 66 / 66 = 1
// verify_winning_post replicates the single challenged sector 66 times.
// generate_public_inputs then emits, for i in 0..66:
//   [ comm_r , Fr(leaf_i) ]
// where leaf_i is the SHA256-derived leaf challenge for virtual sector
// index i. Total = 132 public inputs (VK IC length 133).

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
)

const (
	winningPostChallengeCount = 66 // WINNING_POST_CHALLENGE_COUNT
	winningPostSectorCount    = 1  // WINNING_POST_SECTOR_COUNT
	nodeSize                  = 32 // NODE_SIZE
)

// APIVersion selects the challenge-index derivation rule, matching
// storage_proofs_core::api_version::ApiVersion.
type APIVersion int

const (
	APIVersion1_0_0 APIVersion = iota
	APIVersion1_1_0
	APIVersion1_2_0
)

// getChallengeIndex mirrors storage-proofs-post fallback::utils::get_challenge_index.
func getChallengeIndex(api APIVersion, sectorIndex, challengeCountPerSector, challengeIndex int) uint64 {
	switch api {
	case APIVersion1_2_0:
		return uint64(challengeIndex)
	default: // V1_0_0 | V1_1_0
		return uint64(sectorIndex*challengeCountPerSector + challengeIndex)
	}
}

// generateLeafChallenge computes one leaf challenge:
//
//	leaf = LE_u64( SHA256(randomness || sectorID_le || challengeIndex_le)[0:8] ) % numLeaves
//
// The hasher is seeded with randomness (32B) and sector id (u64 LE), then
// finalized after appending the challenge index (u64 LE). Matches
// storage-proofs-post fallback::vanilla::generate_leaf_challenge_inner.
func generateLeafChallenge(randomness [32]byte, sectorID uint64, challengeIndex, numLeaves uint64) uint64 {
	h := sha256.New()
	h.Write(randomness[:])
	var sidLE [8]byte
	binary.LittleEndian.PutUint64(sidLE[:], sectorID)
	h.Write(sidLE[:])
	var ciLE [8]byte
	binary.LittleEndian.PutUint64(ciLE[:], challengeIndex)
	h.Write(ciLE[:])
	sum := h.Sum(nil)
	leaf := binary.LittleEndian.Uint64(sum[:8])
	return leaf % numLeaves
}

// WinningPoStSector is one challenged sector's public data.
type WinningPoStSector struct {
	SectorNumber uint64
	// CommR is the 32-byte sealed-sector commitment in LITTLE-endian
	// field-element representation (as stored in the sealed CID digest).
	CommR [32]byte
}

// WinningPoStVerifyInfo is the pure-Go equivalent of
// proof.WinningPoStVerifyInfo (the inputs to VerifyWinningPoSt).
type WinningPoStVerifyInfo struct {
	Randomness [32]byte
	Proof      []byte // the Groth16 proof bytes (compressed, 192B)
	Sectors    []WinningPoStSector
	SectorSize uint64
	APIVersion APIVersion
}

// commRToFr converts a 32-byte little-endian field element (the sealed
// commitment) to an fr-compatible big.Int for the public input vector.
func commRToFr(commR [32]byte) *big.Int {
	// The bytes are little-endian; big.Int wants big-endian, so reverse.
	be := make([]byte, 32)
	for i := 0; i < 32; i++ {
		be[i] = commR[31-i]
	}
	return new(big.Int).SetBytes(be)
}

// BuildWinningPoStPublicInputs assembles the Groth16 public-input vector
// for a WinningPoSt, exactly as rust-fil-proofs generate_public_inputs.
//
// param_sector_count = challenge_count / sector_count (= 66 for winning),
// and the single challenged sector is replicated that many times, each
// virtual copy i getting leaf challenge for sector_index = i.
func BuildWinningPoStPublicInputs(info WinningPoStVerifyInfo) ([]*big.Int, error) {
	if len(info.Sectors) != winningPostSectorCount {
		return nil, fmt.Errorf("winning post expects %d challenged sector, got %d",
			winningPostSectorCount, len(info.Sectors))
	}
	if info.SectorSize == 0 || info.SectorSize%nodeSize != 0 {
		return nil, fmt.Errorf("bad sector size %d", info.SectorSize)
	}
	numLeaves := info.SectorSize / nodeSize

	paramSectorCount := winningPostChallengeCount / winningPostSectorCount // 66
	paramChallengeCount := winningPostChallengeCount / paramSectorCount    // 1

	sector := info.Sectors[0]
	commRFr := commRToFr(sector.CommR)

	inputs := make([]*big.Int, 0, paramSectorCount*(1+paramChallengeCount))
	for i := 0; i < paramSectorCount; i++ {
		// 1. comm_r for this (virtual) sector.
		inputs = append(inputs, new(big.Int).Set(commRFr))
		// 2. one leaf challenge.
		for n := 0; n < paramChallengeCount; n++ {
			ci := getChallengeIndex(info.APIVersion, i, paramChallengeCount, n)
			leaf := generateLeafChallenge(info.Randomness, sector.SectorNumber, ci, numLeaves)
			inputs = append(inputs, new(big.Int).SetUint64(leaf))
		}
	}
	return inputs, nil
}

// VerifyWinningPoSt verifies a WinningPoSt proof in pure Go against the
// given verifying key. Returns nil on success.
func VerifyWinningPoSt(vk *Groth16VerifyingKey, info WinningPoStVerifyInfo) error {
	proof, err := ParseGroth16Proof(info.Proof)
	if err != nil {
		return fmt.Errorf("parse proof: %w", err)
	}
	inputs, err := BuildWinningPoStPublicInputs(info)
	if err != nil {
		return fmt.Errorf("build public inputs: %w", err)
	}
	if len(inputs)+1 != len(vk.IC) {
		return fmt.Errorf("public input count %d + 1 != vk IC length %d", len(inputs), len(vk.IC))
	}
	return Groth16Verify(vk, proof, inputs)
}
