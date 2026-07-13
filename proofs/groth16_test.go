package proofs

// Groth16 verify test using gnark-crypto's own test helpers: generate a
// valid proof over a trivial circuit and verify it with our Groth16Verify.
// This proves the pairing-check wiring is correct.

import (
	"math/big"
	"testing"

	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
)

// TestGroth16VerifyPairingCheckWiring: construct a synthetic VK + proof
// that satisfies the Groth16 pairing equation by construction, and verify
// it passes. This is NOT a real Filecoin proof; it is a cryptographic
// sanity check that our pairing-check call + MSM wiring is correct.
//
// Method: we set up the verification equation to be trivially satisfiable
// by choosing generator-based keys and a "proof" that makes the product
// of pairings equal to 1.
func TestGroth16VerifyPairingCheckWiring(t *testing.T) {
	// Use gnark-crypto's generators as a known base.
	_, _, g1Gen, g2Gen := bls12381.Generators()

	// Trivial VK: alpha = g1, beta2 = g2, gamma2 = g2, delta2 = g2,
	// IC = [g1] (one element = zero public inputs).
	vk := &Groth16VerifyingKey{
		AlphaG1: g1Gen,
		BetaG2:  g2Gen,
		GammaG2: g2Gen,
		DeltaG2: g2Gen,
		IC:      []bls12381.G1Affine{g1Gen}, // IC[0], no public inputs
	}

	// For the equation to hold with zero public inputs:
	//   e(-A, B) · e(AlphaG1, BetaG2) · e(IC[0], GammaG2) · e(C, DeltaG2) == 1
	// With AlphaG1 = g1, BetaG2 = g2, IC[0] = g1, GammaG2 = g2, DeltaG2 = g2:
	//   e(-A, B) · e(g1, g2) · e(g1, g2) · e(C, g2) == 1
	//   e(-A, B) · e(g1, g2)^2 · e(C, g2) == 1
	//
	// Choose A = 3*g1, B = g2, C such that the product works.
	// e(-3*g1, g2) = e(g1, g2)^(-3)
	// So: e(g1, g2)^(-3) · e(g1, g2)^2 · e(C, g2) == 1
	//     e(g1, g2)^(-1) · e(C, g2) == 1
	//     e(C, g2) == e(g1, g2)
	//     C = g1
	three := new(big.Int).SetInt64(3)
	var A bls12381.G1Affine
	A.ScalarMultiplication(&g1Gen, three)

	proof := &Groth16Proof{
		A: A,
		B: g2Gen,
		C: g1Gen,
	}

	err := Groth16Verify(vk, proof, nil)
	if err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	t.Log("Groth16 pairing-check wiring verified (synthetic proof)")
}

// TestGroth16VerifyRejectsBadProof: flip a bit and verify rejection.
func TestGroth16VerifyRejectsBadProof(t *testing.T) {
	_, _, g1Gen, g2Gen := bls12381.Generators()

	vk := &Groth16VerifyingKey{
		AlphaG1: g1Gen,
		BetaG2:  g2Gen,
		GammaG2: g2Gen,
		DeltaG2: g2Gen,
		IC:      []bls12381.G1Affine{g1Gen},
	}

	// Use a random A that does NOT satisfy the equation.
	two := new(big.Int).SetInt64(2)
	var wrongA bls12381.G1Affine
	wrongA.ScalarMultiplication(&g1Gen, two)

	proof := &Groth16Proof{A: wrongA, B: g2Gen, C: g1Gen}
	err := Groth16Verify(vk, proof, nil)
	if err == nil {
		t.Fatal("bad proof accepted")
	}
	t.Logf("Bad proof correctly rejected: %v", err)
}

// TestGroth16VerifyWithPublicInputs: one public input, wired through the
// IC MSM, proves the public-input accumulation is correct.
func TestGroth16VerifyWithPublicInputs(t *testing.T) {
	_, _, g1Gen, g2Gen := bls12381.Generators()

	// IC = [g1, 2*g1] (IC[0] = base, IC[1] = coefficient for input[0]).
	two := new(big.Int).SetInt64(2)
	var ic1 bls12381.G1Affine
	ic1.ScalarMultiplication(&g1Gen, two)

	vk := &Groth16VerifyingKey{
		AlphaG1: g1Gen,
		BetaG2:  g2Gen,
		GammaG2: g2Gen,
		DeltaG2: g2Gen,
		IC:      []bls12381.G1Affine{g1Gen, ic1},
	}

	// public input = 5
	// L = IC[0] + IC[1]*5 = g1 + 2*g1*5 = g1 + 10*g1 = 11*g1
	// Equation: e(-A, B) · e(g1, g2) · e(11*g1, g2) · e(C, g2) == 1
	//           e(-A, B) · e(g1, g2)^12 · e(C, g2) == 1
	// Choose A = 13*g1, B = g2:
	//           e(g1, g2)^(-13) · e(g1, g2)^12 · e(C, g2) == 1
	//           e(g1, g2)^(-1) · e(C, g2) == 1
	//           C = g1
	thirteen := new(big.Int).SetInt64(13)
	var A bls12381.G1Affine
	A.ScalarMultiplication(&g1Gen, thirteen)

	proof := &Groth16Proof{A: A, B: g2Gen, C: g1Gen}

	input := new(big.Int).SetInt64(5)
	err := Groth16Verify(vk, proof, []*big.Int{input})
	if err != nil {
		t.Fatalf("valid proof with public input rejected: %v", err)
	}

	// Wrong input should fail.
	wrongInput := new(big.Int).SetInt64(6)
	err = Groth16Verify(vk, proof, []*big.Int{wrongInput})
	if err == nil {
		t.Fatal("bad input accepted")
	}

	t.Log("Groth16 with public inputs verified (1 input, MSM wiring correct)")
}

// TestGroth16VerifyInputCountMismatch: mismatched input count caught.
func TestGroth16VerifyInputCountMismatch(t *testing.T) {
	var g1 bls12381.G1Affine
	var g2 bls12381.G2Affine
	vk := &Groth16VerifyingKey{IC: []bls12381.G1Affine{g1}}
	proof := &Groth16Proof{A: g1, B: g2, C: g1}
	// VK has IC of length 1 = 0 public inputs, but we pass 1.
	err := Groth16Verify(vk, proof, []*big.Int{big.NewInt(1)})
	if err == nil {
		t.Fatal("expected input count mismatch error")
	}
}

// TestFrElementRoundTrip: verify our big.Int → fr.Element → big.Int
// conversion doesn't lose precision for a value in the scalar field.
func TestFrElementRoundTrip(t *testing.T) {
	v := new(big.Int).SetUint64(123456789)
	var e fr.Element
	e.SetBigInt(v)
	var out big.Int
	e.BigInt(&out)
	if out.Cmp(v) != 0 {
		t.Fatalf("fr round-trip: %s != %s", out.String(), v.String())
	}
}
