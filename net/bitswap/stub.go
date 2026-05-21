// Bitswap stub: always returns "block not found, bitswap stub". When
// upgraded to a real boxo/bitswap client this file should be replaced with
// the full wiring (libp2p host setup, bootstrap dial, bsnet.NewFromIpfsHost
// with prefix "/chain", etc.). See doc.go.

package bitswap

import (
	"context"
	"errors"

	"github.com/ipfs/go-cid"
)

// Stub satisfies state/hamt.BlockGetter. Always returns an error so the
// combined fetcher falls through to the next source.
type Stub struct{}

// Get always returns ErrStub. This is intentional: see package doc.
func (Stub) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	return nil, ErrStub
}

// ErrStub is the canonical "Bitswap path not yet wired" error.
var ErrStub = errors.New("net/bitswap: stub (no Filecoin Bitswap peers wired yet, see B8)")
