// Package bls implements Filecoin BLS12-381 signature verification using
// github.com/consensys/gnark-crypto/ecc/bls12-381. The Filecoin BLS Domain
// Separation Tag is
//
//	BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_
//
// Filecoin uses the "minimum-pubkey-size" BLS variant: public keys live on
// G1, signatures on G2. Messages are hashed to G2 using the SSWU map with the
// DST above; verification computes the pairing equality
//
//	e(g1, sig) == e(pk, H(msg))
//
// This package implements Verify and AggregateVerify with that scheme.
package bls
