// Package amt is Lantern's proof-recording AMT walker.
//
// Filecoin uses Array-Mapped Tries (AMTs) for indexed arrays in actor state:
// message receipts, miner sectors, partition expirations, market deal
// proposals, etc. The on-disk encoding is documented in
// github.com/filecoin-project/go-amt-ipld/v4.
//
// We delegate the actual node decoding and traversal to that library (pure
// Go, no CGo) and wrap it with a BlockGetter abstraction + proof-path
// recorder, mirroring the state/hamt design.
package amt
