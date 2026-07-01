// Package fullvalidate implements the pure-Go, full-node block-validation
// pipeline for Lantern's Full node tier (issue #90, part of the #87 full-node
// epic).
//
// # What this is
//
// A Lantern Full node follows and serves the whole chain. On top of the Phase-1
// structural/beacon checks in chain/header.ValidateHeader, a Full node also has
// the chain STATE resident (via the F3-anchored, CID-verified cache), so it can
// look up a block's worker key and miner power and run the remaining consensus
// checks that a light client cannot. This package is that second pass.
//
// It lifts the Lotus filcns block-validation check set MINUS the two filecoin-ffi
// (Rust) calls:
//
//   - VerifyWinningPoSt  (Groth16/BLS12-381 SNARK)   -> deferred to proofs/winningpost (#88)
//   - TipSetState        (FVM message re-execution)  -> deferred to Stage C wazero (#89)
//
// Everything this package DOES run is pure-Go BLS / arithmetic that Lantern
// already ships (crypto/sigs, chain/beacon, chain/types.ElectionProof):
//
//   - block signature over the worker key           (crypto/sigs.CheckBlockSignature)
//   - election-proof VRF                             (VerifyVRF over drand-derived base)
//   - ticket VRF                                     (VerifyVRF)
//   - win-count vs miner/total quality-adjusted power (ElectionProof.ComputeWinCount)
//   - claimed-winner sanity (WinCount >= 1)
//
// # Trust model
//
// With this package a Full node independently re-verifies the VRF/signature/
// win-count consensus of every ingested block. It still TRUSTS F3 finality for
// the WinningPoSt SNARK and the FVM state transition it does not natively run.
// That boot/finality trust is a multi-source, BLS-verified, 2/3-power quorum -
// strictly stronger than a single-source snapshot import. #88 and #89 close the
// two remaining ffi gaps to make the node fully trustless.
package fullvalidate
