// Package f3 is a thin wrapper around github.com/filecoin-project/go-f3 that
// follows the F3 (Filecoin Finality Fast-forward) finality-certificate stream.
//
// Given an initial PowerTable (from the network manifest) and a sequence of
// FinalityCertificates, this package verifies the aggregate BLS signature of
// each certificate against >=2/3 of voting power and walks the power table
// forward by applying each cert's power diff. It exposes the resulting
// terminal power table and the chain of finalized TipSetKeys to chain/header
// and chain/trustedroot.
//
// The package itself contains no BLS code: it relies on
// go-f3/blssig.VerifierWithKeyOnG1 (pure-Go gnark + kyber).
package f3
