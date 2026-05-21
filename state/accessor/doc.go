// Package accessor is Lantern's query layer over a trusted state root.
//
// Every public method takes a TrustedRoot (or its StateRoot CID) and an
// address (or other key), returns the requested actor state plus a proof
// path that lets a third party re-verify the result.
//
// The accessor is the only layer in Lantern that knows how to translate
// "what's f099's balance?" into a sequence of HAMT/AMT lookups. It does so
// using `go-state-types` actor codecs and Lantern's path-recording HAMT/AMT
// walkers in state/hamt and state/amt.
//
// Provenance: structurally inspired by Lotus' chain/state/statetree.go and
// chain/actors/builtin/init/* loaders, but every method is reimplemented to
// use Lantern's BlockGetter abstraction (no IpldStore dependency). Lifted
// constants and the StateRoot tuple definition are tagged in-place.
package accessor
