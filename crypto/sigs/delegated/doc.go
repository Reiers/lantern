// Package delegated implements verification for Filecoin "delegated" (f4)
// signatures: Ethereum-style secp256k1 over Keccak-256 of an RLP-encoded
// payload. Used by the EVM-actor pathway introduced in nv18.
//
// Code in this package is copied from
// github.com/filecoin-project/lotus/lib/sigs/delegated at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE-LOTUS.
package delegated
