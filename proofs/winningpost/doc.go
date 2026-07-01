// Package winningpost is the Stage B scaffold for a pure-Go WinningPoSt verifier.
//
// Trust model:
//   - removes the filecoin-ffi dependency from verification path #1
//   - keeps all verification logic in Go
//   - does not attempt proving, parameter generation, or sector challenges yet
//
// Scope:
//   - verify only
//   - proving remains out of scope
//   - this package is a placeholder for the future Groth16/BLS12-381 path
package winningpost
