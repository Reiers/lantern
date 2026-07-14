package proofs

// Pure-Go WinningPoSt sector-challenge derivation (lantern#87 / #88 wiring).
//
// This is the pure-Go replacement for filecoin-ffi's
// GenerateWinningPoStSectorChallenge. Given a miner's eligible-sector set
// count and a chain-derived randomness, it deterministically returns which
// sector indices (into the eligible set) must be challenged for a WinningPoSt
// at that epoch. Consensus depends on every node deriving the same set.
//
// Reference (rust-fil-proofs):
//   - filecoin-proofs/src/api/winning_post.rs::generate_winning_post_sector_challenge
//   - storage-proofs-post/src/fallback/vanilla.rs::generate_sector_challenge
//   - filecoin-ffi/proofs.go::GenerateWinningPoStSectorChallenge (Go binding)
//
// Algorithm (per challenge index n = 0..WinningPoStSectorCount):
//
//	proverID[32] = pad(address.NewIDAddress(minerID).Payload(), 32, 'r')
//	h            = SHA256(proverID || randomness || uint64_LE(n))
//	sectorIndex  = LittleEndian.Uint64(h[0:8]) % eligibleSectorCount
//
// For WinningPoSt, WinningPoStSectorCount = 1 so exactly one sector is
// selected. The value comes from rust-fil-proofs' PoStConfig.sector_count
// for the Winning variant; it is a network constant across Filecoin.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
)

// WinningPoStSectorCount is the number of distinct sector indices returned by
// GenerateWinningPoStSectorChallenge for a WinningPoSt.
// (rust-fil-proofs: WINNING_POST_SECTOR_COUNT = 1)
const WinningPoStSectorCount = 1

// GenerateWinningPoStSectorChallenge selects the indices, into a miner's
// eligible-sector set, whose sectors must be challenged for a WinningPoSt at
// the given randomness. Byte-for-byte compatible with filecoin-ffi's
// GenerateWinningPoStSectorChallenge (Go binding calls into rust-fil-proofs;
// this function has no ffi/cgo dependency).
//
// The returned slice has length WinningPoStSectorCount (=1). Any caller that
// wants byte-for-byte parity with filecoin-ffi must feed this output back
// into the miner's eligible-sector list (typically ordered by SectorNumber
// ascending) to resolve the concrete sector.
func GenerateWinningPoStSectorChallenge(
	minerID abi.ActorID,
	randomness [32]byte,
	eligibleSectorCount uint64,
) ([]uint64, error) {
	if eligibleSectorCount == 0 {
		return nil, fmt.Errorf("winningpost: eligible sector count is 0")
	}
	proverID, err := toProverID(minerID)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, WinningPoStSectorCount)
	for n := 0; n < WinningPoStSectorCount; n++ {
		var nLE [8]byte
		binary.LittleEndian.PutUint64(nLE[:], uint64(n))
		h := sha256.New()
		h.Write(proverID[:])
		h.Write(randomness[:])
		h.Write(nLE[:])
		sum := h.Sum(nil)
		sectorChallenge := binary.LittleEndian.Uint64(sum[:8])
		out[n] = sectorChallenge % eligibleSectorCount
	}
	return out, nil
}

// toProverID mirrors filecoin-ffi's toProverID: converts an actor ID into
// the 32-byte prover-id representation used by rust-fil-proofs. The bytes
// are the varint-encoded ID-address payload, right-padded to 32 bytes.
func toProverID(minerID abi.ActorID) ([32]byte, error) {
	var out [32]byte
	maddr, err := address.NewIDAddress(uint64(minerID))
	if err != nil {
		return out, fmt.Errorf("winningpost: build id-address for actor %d: %w", uint64(minerID), err)
	}
	payload := maddr.Payload()
	if len(payload) > 32 {
		return out, fmt.Errorf("winningpost: id-address payload %d > 32 bytes for actor %d",
			len(payload), uint64(minerID))
	}
	copy(out[:], payload)
	return out, nil
}
