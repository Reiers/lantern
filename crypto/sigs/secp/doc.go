// Package secp implements Filecoin secp256k1 signature verification. The
// signing scheme is BlakeKeccak as used by Lotus: messages are hashed with
// BLAKE2b-256, then signed/verified via the standard secp256k1 ECDSA recover
// scheme using github.com/filecoin-project/go-crypto.
//
// Code in this package is copied from
// github.com/filecoin-project/lotus/lib/sigs/secp at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE-LOTUS.
package secp
