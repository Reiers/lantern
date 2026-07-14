package fullvalidate

// Pure-Go WinningPoSt block-level verification (lantern#87 wiring for #88).
//
// This file completes the Full-node consensus pipeline by verifying a
// block's WinningPoSt SNARK against the parent state's eligible-sector set.
// It is the last check that Lotus needs filecoin-ffi for; the underlying
// Groth16 verify and public-input assembly live in the proofs/ package
// (issue #88, shipped in PR #135, vector-matched against filecoin-ffi).
//
// VerifyBlockWinningPoSt is OPTIONAL: it requires a MinerSectorSetView
// (which reads the miner's active-sector list) and a verifying-key cache
// directory. ValidateBlockConsensus intentionally does not depend on either,
// so nodes that don't opt in stay F3-trusted for the SNARK and pay no cost.
//
// Reference (Lotus, for cross-verification):
//   - chain/consensus/filcns/filecoin.go: winning-post randomness derivation
//   - chain/stmgr/actors.go::GetSectorsForWinningPoSt: sector-set selection
//
// The pure-Go side of this pipeline (public-input assembly, Groth16 verify,
// vector-matched sector-challenge derivation) is in proofs/.

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/proofs"
)

// MinerSectorRef is the minimum sector metadata WinningPoSt verification
// needs from parent-tipset state: the sector number, its sealed-sector CID
// (whose multihash digest is the CommR), and its seal proof type (which
// implies the WinningPoSt proof type and sector size).
type MinerSectorRef struct {
	SectorNumber abi.SectorNumber
	SealedCID    cid.Cid
	SealProof    abi.RegisteredSealProof
}

// MinerSectorSetView is the extra read surface WinningPoSt verify needs on
// top of StateView. A Full node exposes it separately so that the basic
// consensus checks in ValidateBlockConsensus don't require it (a deployment
// can F3-trust WinningPoSt while still running signature/VRF/win-count
// checks pure-Go).
//
// The returned slice MUST be the miner's ACTIVE, non-faulty sectors at the
// parent tipset, sorted by SectorNumber ascending. This mirrors Lotus's
// GetSectorsForWinningPoSt which iterates the proving-sectors bitfield in
// bit-index order (i.e., sector-number order).
type MinerSectorSetView interface {
	MinerActiveSectors(ctx context.Context, miner address.Address) ([]MinerSectorRef, error)
}

// VerifyBlockWinningPoSt runs pure-Go WinningPoSt SNARK verification against
// a block's proof, deriving the challenge randomness from the parent-epoch
// beacon per Lotus filcns. Returns nil on success.
//
// Steps:
//  1. Derive WinningPoSt randomness (see ComputeWinningPoStRandomness).
//  2. Fetch the miner's active-sector set at the parent tipset.
//  3. Derive the challenged sector index (proofs.GenerateWinningPoStSectorChallenge).
//  4. Extract the challenged sector's CommR + WinningPoSt proof type.
//  5. Load the verifying key from vkCacheDir and run
//     proofs.VerifyWinningPoStByType (Groth16 pairing check over BLS12-381).
//
// The block header's WinPoStProof slice is expected to carry exactly one
// PoSt proof for a WinningPoSt (single-partition, unlike WindowPoSt).
func VerifyBlockWinningPoSt(
	ctx context.Context,
	bh *types.BlockHeader,
	prevBeacon *types.BeaconEntry,
	sv StateView,
	msv MinerSectorSetView,
	vkCacheDir string,
) error {
	if bh == nil {
		return errors.New("fullvalidate/winningpost: nil block header")
	}
	rand, err := ComputeWinningPoStRandomness(bh, prevBeacon)
	if err != nil {
		return err
	}
	return VerifyBlockWinningPoStWithChallengeRandomness(ctx, bh, sv, msv, rand, vkCacheDir)
}

// VerifyBlockWinningPoStWithChallengeRandomness is the lower half of
// VerifyBlockWinningPoSt: given an already-drawn 32-byte challenge
// randomness, it runs steps (2)-(5). Tests use it to drive the verifier
// with the hardcoded randomness of a shipped proof fixture, without
// reconstructing a synthetic block-header/beacon whose DrawRandomnessFromBase
// would have to reproduce that exact 32-byte value.
func VerifyBlockWinningPoStWithChallengeRandomness(
	ctx context.Context,
	bh *types.BlockHeader,
	sv StateView,
	msv MinerSectorSetView,
	rand [32]byte,
	vkCacheDir string,
) error {
	if bh == nil {
		return errors.New("fullvalidate/winningpost: nil block header")
	}
	if msv == nil {
		return errors.New("fullvalidate/winningpost: nil sector-set view")
	}
	if len(bh.WinPoStProof) != 1 {
		return fmt.Errorf("fullvalidate/winningpost: expected 1 WinPoStProof, got %d",
			len(bh.WinPoStProof))
	}

	// (2) Load active sector set.
	sectors, err := msv.MinerActiveSectors(ctx, bh.Miner)
	if err != nil {
		return fmt.Errorf("fullvalidate/winningpost: load active sectors: %w", err)
	}
	if len(sectors) == 0 {
		return errors.New("fullvalidate/winningpost: miner has no active sectors")
	}

	// (3) Sector-challenge selection (SHA256, mirrors filecoin-ffi byte-for-byte).
	mid, err := address.IDFromAddress(bh.Miner)
	if err != nil {
		return fmt.Errorf("fullvalidate/winningpost: id-from-address(%s): %w", bh.Miner, err)
	}
	ids, err := proofs.GenerateWinningPoStSectorChallenge(
		abi.ActorID(mid), rand, uint64(len(sectors)))
	if err != nil {
		return fmt.Errorf("fullvalidate/winningpost: challenge: %w", err)
	}
	if len(ids) != 1 {
		return fmt.Errorf("fullvalidate/winningpost: expected 1 challenge, got %d", len(ids))
	}
	if ids[0] >= uint64(len(sectors)) {
		return fmt.Errorf("fullvalidate/winningpost: challenge index %d out of range (len=%d)",
			ids[0], len(sectors))
	}
	challenged := sectors[ids[0]]

	// (4) Extract CommR + WinningPoSt proof type.
	commR, err := proofs.SealedCIDToCommR(challenged.SealedCID.Hash())
	if err != nil {
		return fmt.Errorf("fullvalidate/winningpost: sealed cid -> commR: %w", err)
	}
	postType, err := challenged.SealProof.RegisteredWinningPoStProof()
	if err != nil {
		return fmt.Errorf("fullvalidate/winningpost: registered winning post proof: %w", err)
	}

	// (5) Verify.
	post := bh.WinPoStProof[0]
	if abi.RegisteredPoStProof(post.PoStProof) != postType {
		return fmt.Errorf(
			"fullvalidate/winningpost: block WinPoStProof type %d != expected %d",
			int64(post.PoStProof), int64(postType))
	}
	if err := proofs.VerifyWinningPoStByType(
		vkCacheDir,
		proofs.RegisteredPoStProof(int64(postType)),
		rand,
		post.ProofBytes,
		[]proofs.WinningPoStSector{{
			SectorNumber: uint64(challenged.SectorNumber),
			CommR:        commR,
		}},
	); err != nil {
		return fmt.Errorf("fullvalidate/winningpost: verify: %w", err)
	}
	return nil
}

// ComputeWinningPoStRandomness derives the 32-byte challenge randomness that
// filecoin-ffi's VerifyWinningPoSt (and now proofs.VerifyWinningPoStByType)
// takes as input. Matches Lotus filcns.
//
// Formula:
//
//	rbase = bh.BeaconEntries[last] if any, else prevBeacon
//	rand  = DrawRandomnessFromBase(
//	          rbase.Data,
//	          DomainSeparationTag_WinningPoStChallengeSeed,
//	          bh.Height,
//	          CBOR(bh.Miner),
//	        )
//
// Exposed as a top-level helper so callers writing test vectors can drive
// the verifier without reconstructing the whole block-validation path.
func ComputeWinningPoStRandomness(
	bh *types.BlockHeader,
	prevBeacon *types.BeaconEntry,
) ([32]byte, error) {
	var out [32]byte
	rBeaconData, err := randBeaconData(bh, prevBeacon)
	if err != nil {
		return out, fmt.Errorf("fullvalidate/winningpost: %w", err)
	}
	minerEntropy, err := minerCBOR(bh.Miner)
	if err != nil {
		return out, err
	}
	base, err := beacon.DrawRandomnessFromBase(
		rBeaconData,
		gscrypto.DomainSeparationTag_WinningPoStChallengeSeed,
		bh.Height,
		minerEntropy,
	)
	if err != nil {
		return out, fmt.Errorf("fullvalidate/winningpost: draw randomness: %w", err)
	}
	copy(out[:], base)
	return out, nil
}
