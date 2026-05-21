// Package sigs dispatches Filecoin signature verification to the appropriate
// backend by signature type: BLS (BLS12-381), secp256k1, or delegated
// (Ethereum f4 addresses).
//
// The interface mirrors Lotus' lib/sigs/sigs.go so wallet and chain
// validation code can be ported with minimal churn. Each backend lives in a
// subpackage (bls, secp, delegated) that registers itself via init() with the
// dispatcher.
//
// Unlike Lotus, the BLS backend here is pure Go (gnark-crypto), so the entire
// crypto/sigs tree builds with CGO_ENABLED=0.
package sigs
