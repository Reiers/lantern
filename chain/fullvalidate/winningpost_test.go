package fullvalidate

// End-to-end WinningPoSt block-verification test.
//
// Drives VerifyBlockWinningPoStWithChallengeRandomness with the shipped
// 2KiB WinningPoSt proof fixture (proofs/testdata/winningpost_2kib_vector.json
// + winning_2kib.vk) reconstructed at the block-header layer. This exercises
// the full wiring — MinerSectorSetView -> challenge derivation -> CommR
// extraction from a real sealed CID -> WinningPoSt proof-type dispatch ->
// pure-Go Groth16 verify — against a proof produced by filecoin-ffi.
//
// Note: the test uses VerifyBlockWinningPoStWithChallengeRandomness so it
// can pass the vector's known challenge randomness (2a07...) directly
// rather than reconstruct a synthetic block-header/beacon that
// ComputeWinningPoStRandomness would have to reproduce byte-for-byte. That
// derivation is covered separately by TestComputeWinningPoStRandomness_Deterministic.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	proofsig "github.com/filecoin-project/go-state-types/proof"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
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

func loadShippedVector(t *testing.T) (winningVector, [32]byte, []byte, cid.Cid) {
	t.Helper()
	// The fixture lives in proofs/testdata/; tests here run from
	// chain/fullvalidate/ so we walk up.
	path := filepath.Join("..", "..", "proofs", "testdata", "winningpost_2kib_vector.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vector %s: %v", path, err)
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
	sealedCID, err := cid.Decode(v.SealedCID)
	if err != nil {
		t.Fatalf("decode sealed cid: %v", err)
	}
	return v, randomness, proof, sealedCID
}

// staticSectorSetView returns a fixed active-sector list for any miner.
type staticSectorSetView struct {
	sectors []MinerSectorRef
}

func (v staticSectorSetView) MinerActiveSectors(_ context.Context, _ address.Address) ([]MinerSectorRef, error) {
	return v.sectors, nil
}

// vkCacheDirForFixture stages the shipped winning_2kib.vk under the
// CID-named filename LoadWinningPoStVK expects, in a t.TempDir, and returns
// the directory path. Production nodes populate this directory with the
// full Filecoin proof-parameters set; here we only need the WinningPoSt
// 2KiB VK.
func vkCacheDirForFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "proofs", "testdata", "winning_2kib.vk")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read shipped vk: %v", err)
	}
	dir := t.TempDir()
	const vkName = "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-0-0-3ea05428c9d11689f23529cde32fd30aabd50f7d2c93657c1d3650bca3e8ea9e.vk"
	if err := os.WriteFile(filepath.Join(dir, vkName), data, 0o600); err != nil {
		t.Fatalf("stage vk: %v", err)
	}
	return dir
}

func TestVerifyBlockWinningPoStWithChallengeRandomness_ShippedFixture(t *testing.T) {
	vec, rand, proofBytes, sealedCID := loadShippedVector(t)

	minerAddr, err := address.NewIDAddress(vec.MinerID)
	if err != nil {
		t.Fatalf("miner id addr: %v", err)
	}

	// The fixture's active-sector set has exactly one sector (challenge
	// derivation returns index 0 for eligibleCount=1).
	sv := staticSectorSetView{sectors: []MinerSectorRef{{
		SectorNumber: abi.SectorNumber(vec.SectorNumber),
		SealedCID:    sealedCID,
		SealProof:    abi.RegisteredSealProof(vec.SealProofType),
	}}}

	bh := &types.BlockHeader{
		Miner: minerAddr,
		WinPoStProof: []proofsig.PoStProof{{
			PoStProof:  abi.RegisteredPoStProof(vec.PostProofType),
			ProofBytes: proofBytes,
		}},
	}

	vkCacheDir := vkCacheDirForFixture(t)
	if err := VerifyBlockWinningPoStWithChallengeRandomness(
		context.Background(), bh, nil /*sv unused by this path*/, sv, rand, vkCacheDir,
	); err != nil {
		t.Fatalf("verify shipped WinningPoSt fixture: %v", err)
	}
}

func TestVerifyBlockWinningPoStWithChallengeRandomness_TamperedProof(t *testing.T) {
	vec, rand, proofBytes, sealedCID := loadShippedVector(t)

	// Flip a bit in the middle of the proof; the Groth16 pairing check
	// must reject.
	proofBytes[len(proofBytes)/2] ^= 0x01

	minerAddr, _ := address.NewIDAddress(vec.MinerID)
	sv := staticSectorSetView{sectors: []MinerSectorRef{{
		SectorNumber: abi.SectorNumber(vec.SectorNumber),
		SealedCID:    sealedCID,
		SealProof:    abi.RegisteredSealProof(vec.SealProofType),
	}}}
	bh := &types.BlockHeader{
		Miner: minerAddr,
		WinPoStProof: []proofsig.PoStProof{{
			PoStProof:  abi.RegisteredPoStProof(vec.PostProofType),
			ProofBytes: proofBytes,
		}},
	}
	vkCacheDir := vkCacheDirForFixture(t)

	if err := VerifyBlockWinningPoStWithChallengeRandomness(
		context.Background(), bh, nil, sv, rand, vkCacheDir,
	); err == nil {
		t.Fatal("expected verify to reject tampered proof, got nil error")
	}
}

func TestVerifyBlockWinningPoStWithChallengeRandomness_WrongPoStProofType(t *testing.T) {
	vec, rand, proofBytes, sealedCID := loadShippedVector(t)

	minerAddr, _ := address.NewIDAddress(vec.MinerID)
	sv := staticSectorSetView{sectors: []MinerSectorRef{{
		SectorNumber: abi.SectorNumber(vec.SectorNumber),
		SealedCID:    sealedCID,
		SealProof:    abi.RegisteredSealProof(vec.SealProofType),
	}}}
	// Block claims a different PoSt proof type than the sector's seal
	// proof implies. Must reject before running the SNARK.
	bh := &types.BlockHeader{
		Miner: minerAddr,
		WinPoStProof: []proofsig.PoStProof{{
			PoStProof:  abi.RegisteredPoStProof(99),
			ProofBytes: proofBytes,
		}},
	}
	vkCacheDir := vkCacheDirForFixture(t)

	if err := VerifyBlockWinningPoStWithChallengeRandomness(
		context.Background(), bh, nil, sv, rand, vkCacheDir,
	); err == nil {
		t.Fatal("expected verify to reject mismatched PoSt proof type")
	}
}

func TestVerifyBlockWinningPoStWithChallengeRandomness_EmptyActiveSet(t *testing.T) {
	vec, rand, proofBytes, _ := loadShippedVector(t)
	minerAddr, _ := address.NewIDAddress(vec.MinerID)
	sv := staticSectorSetView{sectors: nil}
	bh := &types.BlockHeader{
		Miner: minerAddr,
		WinPoStProof: []proofsig.PoStProof{{
			PoStProof:  abi.RegisteredPoStProof(vec.PostProofType),
			ProofBytes: proofBytes,
		}},
	}
	if err := VerifyBlockWinningPoStWithChallengeRandomness(
		context.Background(), bh, nil, sv, rand, "",
	); err == nil {
		t.Fatal("expected reject on empty active sector set")
	}
}

// TestComputeWinningPoStRandomness_UsesLastBeaconEntry pins the beacon-source
// selection: block's own last entry preferred over prev-epoch beacon.
func TestComputeWinningPoStRandomness_UsesLastBeaconEntry(t *testing.T) {
	minerAddr, _ := address.NewIDAddress(1000)
	prev := &types.BeaconEntry{Round: 100, Data: []byte{0xaa, 0xaa}}
	own := types.BeaconEntry{Round: 200, Data: []byte{0xbb, 0xbb}}
	bh := &types.BlockHeader{Miner: minerAddr, Height: abi.ChainEpoch(1234), BeaconEntries: []types.BeaconEntry{own}}

	r1, err := ComputeWinningPoStRandomness(bh, prev)
	if err != nil {
		t.Fatalf("with own entry: %v", err)
	}

	// Now drop own entries, keep prev — must produce a DIFFERENT randomness.
	bh2 := &types.BlockHeader{Miner: minerAddr, Height: abi.ChainEpoch(1234)}
	r2, err := ComputeWinningPoStRandomness(bh2, prev)
	if err != nil {
		t.Fatalf("with prev entry only: %v", err)
	}
	if r1 == r2 {
		t.Fatalf("randomness identical whether beacon came from own entries or prev; source not honored")
	}

	// With no beacon anywhere, must error (matches Lotus).
	if _, err := ComputeWinningPoStRandomness(bh2, nil); err == nil {
		t.Fatal("expected error when no beacon available")
	}
}
