package fvmkernel

// Proof-verify interface tests (lantern#130 Tier 2).

import "testing"

func TestRejectAllProofVerifierRejects(t *testing.T) {
	v := RejectAllProofVerifier{}
	if err := v.VerifyProof(0, []byte{0x01}, nil); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestAcceptAllProofVerifierAccepts(t *testing.T) {
	v := AcceptAllProofVerifier{}
	if err := v.VerifyProof(0, []byte{0x01}, nil); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}
