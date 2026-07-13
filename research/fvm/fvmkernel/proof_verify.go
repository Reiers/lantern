package fvmkernel

// Proof-verify interface for the FVM kernel (lantern#130 Tier 2).
//
// The FVM's crypto.verify_post / verify_seal / verify_aggregate_seals /
// verify_replica_update / batch_verify_seals syscalls all reduce to
// Groth16 verification over BLS12-381. The kernel delegates to a
// ProofVerifier interface so the concrete implementation (gnark-crypto
// pairing check) lives outside the kernel's module boundary.
//
// Default: RejectAllProofVerifier (safe posture — no proof passes until
// a real verifier is plugged in). Wiring the main module's
// proofs/groth16.go verifier is the integrator's job (when the FVM is
// promoted from research to the shipping binary).
//
// The syscall handlers read the CBOR-encoded proof + inputs from WASM
// memory, call ProofVerifier.VerifyProof, and write the boolean result.
// The CBOR layout varies per syscall; the handler is responsible for
// parsing the right shape and packing the arguments.

import (
	"fmt"
)

// ProofVerifier is the interface the FVM kernel calls for all Groth16
// proof-verify syscalls. Implementations receive the raw proof bytes +
// the assembled public-input scalars (as 32-byte big-endian field
// elements) and return nil on success.
type ProofVerifier interface {
	// VerifyProof checks a Groth16 proof against a verifying key
	// identified by proofType and the public inputs.
	VerifyProof(proofType int64, proofBytes []byte, publicInputs [][]byte) error
}

// RejectAllProofVerifier rejects every proof. Safe default so the
// prototype cannot accept a proof without an explicit real verifier.
type RejectAllProofVerifier struct{}

func (RejectAllProofVerifier) VerifyProof(int64, []byte, [][]byte) error {
	return fmt.Errorf("proof verification not wired (requires proofs/groth16 from the main module)")
}

// AcceptAllProofVerifier accepts every proof. TEST-ONLY — lets tests
// drive the syscall ABI + CBOR parsing in isolation from the crypto.
type AcceptAllProofVerifier struct{}

func (AcceptAllProofVerifier) VerifyProof(int64, []byte, [][]byte) error { return nil }
