package winningpost

import "fmt"

var ErrNotImplemented = notImplementedError{}

type notImplementedError struct{}

func (notImplementedError) Error() string {
	return "pure-go winningpost verify: Stage B WIP"
}

// Verify is the pure-Go scaffold entrypoint.
//
// TODO:
//   - derive the winning post challenge from randomness + prover + tipset context
//   - assemble public inputs from challenged sectors and proof metadata
//   - load the correct verifying key for the registered PoSt proof type
//   - run the Groth16 pairing check over BLS12-381
//   - validate Poseidon-based commitments / transcript inputs as required
//
// This stage intentionally does not verify anything yet.
func Verify(info WinningPoStVerifyInfo) (bool, error) {
	_ = info
	return false, fmt.Errorf("%w", ErrNotImplemented)
}
