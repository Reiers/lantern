package proofs

// Registered WinningPoSt proof types -> circuit parameters, and VK
// loading from a Filecoin proof-parameter cache directory (lantern#88).

import (
	"fmt"
	"os"
	"path/filepath"
)

// RegisteredPoStProof mirrors abi.RegisteredPoStProof for the WinningPoSt
// variants (the only ones this package verifies).
type RegisteredPoStProof int64

const (
	StackedDrgWinning2KiBV1   RegisteredPoStProof = 0
	StackedDrgWinning8MiBV1   RegisteredPoStProof = 1
	StackedDrgWinning512MiBV1 RegisteredPoStProof = 2
	StackedDrgWinning32GiBV1  RegisteredPoStProof = 3
	StackedDrgWinning64GiBV1  RegisteredPoStProof = 4
)

// winningPoStParams describes the circuit parameters for a WinningPoSt
// proof type: the sector size, the API version (which selects the
// challenge-index rule), and the Filecoin parameter file identifier of
// the verifying key (the CID-suffixed name under the param cache).
type winningPoStParams struct {
	SectorSize uint64
	APIVersion APIVersion
	// VKFile is the filename of the verifying key in the param cache.
	VKFile string
}

// winningPoStParamTable maps each WinningPoSt proof type to its params.
// The VK filenames are the canonical v28 Filecoin parameter names.
var winningPoStParamTable = map[RegisteredPoStProof]winningPoStParams{
	StackedDrgWinning2KiBV1: {
		SectorSize: 1 << 11,
		APIVersion: APIVersion1_1_0,
		VKFile:     "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-0-0-3ea05428c9d11689f23529cde32fd30aabd50f7d2c93657c1d3650bca3e8ea9e.vk",
	},
	StackedDrgWinning8MiBV1: {
		SectorSize: 1 << 23,
		APIVersion: APIVersion1_1_0,
		VKFile:     "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-0-0-0170db1f394b35d995252228ee359194b13199d259380541dc529fb0099096b0.vk",
	},
	StackedDrgWinning512MiBV1: {
		SectorSize: 1 << 29,
		APIVersion: APIVersion1_1_0,
		VKFile:     "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-0-0-0cfb4f178bbb71cf2ecfcd42accce558b27199ab4fb59cb78f2483fe21ef36d9.vk",
	},
	StackedDrgWinning32GiBV1: {
		SectorSize: 1 << 35,
		APIVersion: APIVersion1_1_0,
		VKFile:     "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-8-0-559e581f022bb4e4ec6e719e563bf0e026ad6de42e56c18714a2c692b1b88d7e.vk",
	},
	StackedDrgWinning64GiBV1: {
		SectorSize: 1 << 36,
		APIVersion: APIVersion1_1_0,
		VKFile:     "v28-proof-of-spacetime-fallback-merkletree-poseidon_hasher-8-8-2-b62098629d07946e9028127e70295ed996fe3ed25b0f9f88eb610a0ab4385a3c.vk",
	},
}

// WinningPoStParams returns the circuit parameters for a proof type.
func WinningPoStParams(pt RegisteredPoStProof) (winningPoStParams, error) {
	p, ok := winningPoStParamTable[pt]
	if !ok {
		return winningPoStParams{}, fmt.Errorf("unsupported winning post proof type %d", pt)
	}
	return p, nil
}

// LoadWinningPoStVK loads the verifying key for a WinningPoSt proof type
// from the given Filecoin parameter cache directory (the same directory
// filecoin-ffi uses, e.g. $FIL_PROOFS_PARAMETER_CACHE).
func LoadWinningPoStVK(cacheDir string, pt RegisteredPoStProof) (*Groth16VerifyingKey, error) {
	p, err := WinningPoStParams(pt)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(cacheDir, p.VKFile))
	if err != nil {
		return nil, fmt.Errorf("open winning post vk: %w", err)
	}
	defer f.Close()
	return ParseGroth16VerifyingKey(f)
}

// SealedCIDToCommR extracts the 32-byte little-endian comm_r field element
// from a sealed sector CID's multihash digest.
func SealedCIDToCommR(mhDigest []byte) ([32]byte, error) {
	var commR [32]byte
	if len(mhDigest) < 32 {
		return commR, fmt.Errorf("sealed cid digest %d bytes, need >= 32", len(mhDigest))
	}
	copy(commR[:], mhDigest[len(mhDigest)-32:])
	return commR, nil
}

// VerifyWinningPoStByType is the high-level entry point closest to
// filecoin-ffi's VerifyWinningPoSt: given the proof type, the param cache
// dir, and the challenged sector(s), it loads the VK and verifies.
func VerifyWinningPoStByType(cacheDir string, pt RegisteredPoStProof, randomness [32]byte, proof []byte, sectors []WinningPoStSector) error {
	p, err := WinningPoStParams(pt)
	if err != nil {
		return err
	}
	vk, err := LoadWinningPoStVK(cacheDir, pt)
	if err != nil {
		return err
	}
	return VerifyWinningPoSt(vk, WinningPoStVerifyInfo{
		Randomness: randomness,
		Proof:      proof,
		Sectors:    sectors,
		SectorSize: p.SectorSize,
		APIVersion: p.APIVersion,
	})
}
