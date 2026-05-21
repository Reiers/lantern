// Package hamt is Lantern's proof-recording HAMT walker.
//
// The Filecoin state tree maps `Address -> Actor` via a HAMT (hash-array-mapped
// trie) of bit-width 5, using sha256 as the hash function. Actor sub-state
// (e.g. the Init actor's address-resolution table) uses HAMTs too. See
// https://github.com/ipld/specs/blob/master/data-structures/hashmap.md
// for the IPLD HashMap spec; Filecoin's variant fixes `bucketSize = 3`.
//
// We delegate the actual node decoding and walking to
// github.com/filecoin-project/go-hamt-ipld/v3 (pure Go, no CGo). Our value-add
// is two things:
//
//  1. A BlockGetter abstraction the caller controls (cache + Bitswap + HTTP).
//  2. A path-recording wrapper that captures every node CID fetched during
//     a Lookup. That CID list is the inclusion (or exclusion) proof and is
//     what Lantern's "verified by us" guarantee is built on: a third party
//     can re-run VerifyProof against the same root and prove the value was
//     present in the canonical state at the time of lookup.
//
// Strictly local: this package never talks to the network. Network adapters
// live in net/bitswap and net/hsync; they implement BlockGetter.
package hamt
