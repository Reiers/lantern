// Package beacon verifies DRAND beacon entries referenced from Filecoin block
// headers. Filecoin uses a chained-DRAND setup: blocks reference one or more
// beacon entries from the configured drand networks (mainnet uses the original
// league-of-entropy chain plus the newer quicknet chain after a coordinated
// upgrade).
//
// Lantern's verifier is pure Go: it uses github.com/drand/drand/v2/crypto and
// github.com/drand/kyber-bls12381 for BLS verification on the drand chain.
//
// The verifier is intentionally narrow: it answers "does this BeaconEntry sit
// in the configured DRAND chain at the declared round, and is its signature
// valid?" Higher-level rules (e.g. "block at epoch E references the correct
// drand round given the network schedule") live in chain/header.
package beacon
