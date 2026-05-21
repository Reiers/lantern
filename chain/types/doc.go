// Package types contains Filecoin chain primitive types: BlockHeader, TipSet,
// Message, SignedMessage, BeaconEntry, ElectionProof, BigInt, FIL, and the
// associated CBOR codecs.
//
// The files in this package are copied verbatim from
// github.com/filecoin-project/lotus/chain/types at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253 unless a header note indicates
// otherwise. See LICENSE-LOTUS in the repository root.
//
// Lantern maintains its own copy so the module can avoid importing the
// Lotus module (and its transitive filecoin-ffi dependency). Code here must
// remain pure Go; any change that introduces CGo or depends on
// filecoin-ffi is a regression.
package types
