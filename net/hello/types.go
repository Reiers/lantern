// Type aliases + small helpers used by hello.go. Kept separate so hello.go
// reads like protocol logic and this file holds the cross-package glue.

package hello

import (
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multiaddr"
)

// HelloMessage is the Filecoin /fil/hello/1.0.0 message. Wire format
// matches Lotus byte-for-byte; the CBOR codec is in cbor_gen.go.
//
// HeaviestTipSet is the sender's current head tipset (one or more block
// CIDs). HeaviestTipSetHeight is the head epoch. HeaviestTipSetWeight is
// the cumulative parent weight at that head. GenesisHash is the CID of
// block 0 (network identity); receivers close the connection on mismatch.
type HelloMessage struct {
	HeaviestTipSet       []cid.Cid
	HeaviestTipSetHeight abi.ChainEpoch
	HeaviestTipSetWeight big.Int
	GenesisHash          cid.Cid
}

// LatencyMessage is the optional reply sent by Lotus peers carrying their
// arrival + sent timestamps for clock-skew measurement. Lantern reads it
// to drain the stream cleanly but doesn't act on it.
type LatencyMessage struct {
	TArrival int64
	TSent    int64
}

// Re-exports of the underlying upstream types, named with a "T" suffix so
// they read as type aliases inside hello.go's signatures.
type (
	chainEpochT = abi.ChainEpoch
	bigT        = big.Int
	multiaddrT  = multiaddr.Multiaddr
)

// parseBigImpl parses a Lantern ParentWeight string ("1234567890") into a
// big.Int. Returns big.Zero() on parse failure so callers can use the
// result unconditionally; the Hello protocol tolerates a wrong weight
// (peers only fail-close on genesis mismatch).
func parseBigImpl(s string) (big.Int, error) {
	b, err := big.FromString(s)
	if err != nil {
		return big.Zero(), err
	}
	return b, nil
}
