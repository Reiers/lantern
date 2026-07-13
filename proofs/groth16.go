// Package proofs implements pure-Go Filecoin proof verification.
//
// Stage B of #87: WinningPoSt verify (remove filecoin-ffi dep #1).
// The crypto core is Groth16 verification over BLS12-381, using
// gnark-crypto's pairing engine. This same core is shared with the
// FVM's proof-verify syscalls (#130 Tier 2).
//
// Hard safety line: one wrong field ordering or public-input layout =
// every proof fails or, worse, a bad proof passes. This MUST be
// vector-matched against filecoin-ffi reference vectors before wiring
// into block validation.
package proofs

import (
	"fmt"
	"io"
	"math/big"

	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
)

// Groth16VerifyingKey holds a parsed BLS12-381 Groth16 verifying key
// in the format Filecoin uses (bellperson-compatible).
//
// Layout mirrors bellperson::groth16::VerifyingKey<Bls12>:
//
//	alpha_g1: G1Affine
//	beta_g1:  G1Affine (not used in verification, but present in the serialized VK)
//	beta_g2:  G2Affine
//	gamma_g2: G2Affine
//	delta_g1: G1Affine (not used in verification)
//	delta_g2: G2Affine
//	ic:       []G1Affine (public-input commitment bases; len = #public_inputs + 1)
type Groth16VerifyingKey struct {
	AlphaG1 bls12381.G1Affine
	BetaG1  bls12381.G1Affine // present in VK but not used for verify
	BetaG2  bls12381.G2Affine
	GammaG2 bls12381.G2Affine
	DeltaG1 bls12381.G1Affine // present in VK but not used for verify
	DeltaG2 bls12381.G2Affine
	IC      []bls12381.G1Affine
}

// Groth16Proof holds a parsed BLS12-381 Groth16 proof.
//
//	a: G1Affine
//	b: G2Affine
//	c: G1Affine
type Groth16Proof struct {
	A bls12381.G1Affine
	B bls12381.G2Affine
	C bls12381.G1Affine
}

// Groth16Verify checks a Groth16 proof against a verifying key and
// public inputs. Returns nil on success, an error on failure.
//
// The verification equation is the pairing check:
//
//	e(A, B) == e(AlphaG1, BetaG2) · e(L, GammaG2) · e(C, DeltaG2)
//
// where L = IC[0] + ∑(IC[i+1] * input[i]).
//
// Rearranged as PairingCheck (product == 1):
//
//	e(-A, B) · e(AlphaG1, BetaG2) · e(L, GammaG2) · e(C, DeltaG2) == 1
func Groth16Verify(vk *Groth16VerifyingKey, proof *Groth16Proof, publicInputs []*big.Int) error {
	if len(publicInputs)+1 != len(vk.IC) {
		return fmt.Errorf("public inputs length %d != IC length %d - 1", len(publicInputs), len(vk.IC))
	}

	// L = IC[0] + ∑(IC[i+1] * input[i])
	var L bls12381.G1Affine
	L.Set(&vk.IC[0])
	for i, input := range publicInputs {
		var s fr.Element
		s.SetBigInt(input)
		var sb big.Int
		s.BigInt(&sb)
		var term bls12381.G1Affine
		term.ScalarMultiplication(&vk.IC[i+1], &sb)
		L.Add(&L, &term)
	}

	// Negate A for the pairing check form.
	var negA bls12381.G1Affine
	negA.Neg(&proof.A)

	// PairingCheck: e(-A, B) · e(AlphaG1, BetaG2) · e(L, GammaG2) · e(C, DeltaG2) == 1
	ok, err := bls12381.PairingCheck(
		[]bls12381.G1Affine{negA, vk.AlphaG1, L, proof.C},
		[]bls12381.G2Affine{proof.B, vk.BetaG2, vk.GammaG2, vk.DeltaG2},
	)
	if err != nil {
		return fmt.Errorf("pairing check: %w", err)
	}
	if !ok {
		return fmt.Errorf("proof verification failed")
	}
	return nil
}

// ParseG1Uncompressed reads a bellperson-serialized G1 point (96 bytes,
// big-endian X || Y, uncompressed, with the infinity flag in the top bit
// of the first byte). Returns the point and the number of bytes consumed.
func ParseG1Uncompressed(data []byte) (bls12381.G1Affine, error) {
	if len(data) < 96 {
		return bls12381.G1Affine{}, fmt.Errorf("G1: need 96 bytes, got %d", len(data))
	}
	var p bls12381.G1Affine
	// gnark-crypto's SetBytes expects big-endian uncompressed format
	// (96 bytes for G1Affine). The bellperson format is the same as the
	// ZCash serialization: 96 bytes, X first, then Y, with flags in the
	// top 3 bits of the first byte.
	_, err := p.SetBytes(data[:96])
	if err != nil {
		return bls12381.G1Affine{}, fmt.Errorf("G1 parse: %w", err)
	}
	return p, nil
}

// ParseG2Uncompressed reads a bellperson-serialized G2 point (192 bytes).
func ParseG2Uncompressed(data []byte) (bls12381.G2Affine, error) {
	if len(data) < 192 {
		return bls12381.G2Affine{}, fmt.Errorf("G2: need 192 bytes, got %d", len(data))
	}
	var p bls12381.G2Affine
	_, err := p.SetBytes(data[:192])
	if err != nil {
		return bls12381.G2Affine{}, fmt.Errorf("G2 parse: %w", err)
	}
	return p, nil
}

// ParseGroth16Proof parses a 192-byte bellperson proof (A: G1, B: G2, C: G1).
func ParseGroth16Proof(data []byte) (*Groth16Proof, error) {
	if len(data) < 192 {
		return nil, fmt.Errorf("proof too short: %d bytes, need 192", len(data))
	}
	a, err := ParseG1Uncompressed(data[0:96])
	if err != nil {
		return nil, fmt.Errorf("proof.A: %w", err)
	}
	b, err := ParseG2Uncompressed(data[96:288])
	if err != nil {
		return nil, fmt.Errorf("proof.B: %w", err)
	}
	c, err := ParseG1Uncompressed(data[288:384])
	if err != nil {
		return nil, fmt.Errorf("proof.C: %w", err)
	}
	return &Groth16Proof{A: a, B: b, C: c}, nil
}

// ParseGroth16VerifyingKey reads a bellperson verifying key from a reader.
// Format: AlphaG1(96) + BetaG1(96) + BetaG2(192) + GammaG2(192) +
// DeltaG1(96) + DeltaG2(192) + u32le(ic_count) + IC[0..ic_count](96 each).
func ParseGroth16VerifyingKey(r io.Reader) (*Groth16VerifyingKey, error) {
	buf := make([]byte, 96+96+192+192+96+192)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read VK header: %w", err)
	}
	off := 0
	read := func(size int) []byte {
		b := buf[off : off+size]
		off += size
		return b
	}
	alphaG1, err := ParseG1Uncompressed(read(96))
	if err != nil {
		return nil, fmt.Errorf("AlphaG1: %w", err)
	}
	betaG1, err := ParseG1Uncompressed(read(96))
	if err != nil {
		return nil, fmt.Errorf("BetaG1: %w", err)
	}
	betaG2, err := ParseG2Uncompressed(read(192))
	if err != nil {
		return nil, fmt.Errorf("BetaG2: %w", err)
	}
	gammaG2, err := ParseG2Uncompressed(read(192))
	if err != nil {
		return nil, fmt.Errorf("GammaG2: %w", err)
	}
	deltaG1, err := ParseG1Uncompressed(read(96))
	if err != nil {
		return nil, fmt.Errorf("DeltaG1: %w", err)
	}
	deltaG2, err := ParseG2Uncompressed(read(192))
	if err != nil {
		return nil, fmt.Errorf("DeltaG2: %w", err)
	}

	// IC count: u32 little-endian.
	var countBuf [4]byte
	if _, err := io.ReadFull(r, countBuf[:]); err != nil {
		return nil, fmt.Errorf("read IC count: %w", err)
	}
	icCount := uint32(countBuf[0]) | uint32(countBuf[1])<<8 | uint32(countBuf[2])<<16 | uint32(countBuf[3])<<24
	if icCount == 0 || icCount > 100 {
		return nil, fmt.Errorf("IC count %d out of range", icCount)
	}

	ic := make([]bls12381.G1Affine, icCount)
	for i := uint32(0); i < icCount; i++ {
		var pb [96]byte
		if _, err := io.ReadFull(r, pb[:]); err != nil {
			return nil, fmt.Errorf("IC[%d]: %w", i, err)
		}
		ic[i], err = ParseG1Uncompressed(pb[:])
		if err != nil {
			return nil, fmt.Errorf("IC[%d]: %w", i, err)
		}
	}

	return &Groth16VerifyingKey{
		AlphaG1: alphaG1,
		BetaG1:  betaG1,
		BetaG2:  betaG2,
		GammaG2: gammaG2,
		DeltaG1: deltaG1,
		DeltaG2: deltaG2,
		IC:      ic,
	}, nil
}
