// Package bitswap is Lantern's Bitswap client wrapper.
//
// Currently a stub: see PHASE2-BLOCKERS.md item B8 for why. Filecoin's
// public bootstrap peers don't reliably serve historical state CIDs over
// Bitswap, so the production demo uses HTTP gateway fallback as the hot
// path. This package retains a BlockGetter interface implementation so
// net/combined can include it in a fallback chain when a real Bitswap
// peer set becomes available (Phase 6, gateway infrastructure).
//
// The intended production wiring is to lift `github.com/ipfs/boxo/bitswap`
// (pure Go) on top of a libp2p host configured for the Filecoin mainnet
// bootstrap peer list (lifted from `lotus/build/bootstrap/mainnet.pi`).
package bitswap
