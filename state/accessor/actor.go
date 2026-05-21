// Actor is the value type stored at each leaf of the state-tree HAMT.
// V5+ adds an optional DelegatedAddress for f4 actors.

package accessor

import (
	"bytes"
	"fmt"
	"io"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// Actor is the runtime representation of an on-chain actor. It corresponds
// to ActorV5 in go-state-types/builtin/actor_tree.go.
type Actor struct {
	Code             cid.Cid
	Head             cid.Cid
	Nonce            uint64
	Balance          big.Int
	DelegatedAddress *addr.Address // nil for non-EVM (non-f4) actors
}

// DecodeActor parses a HAMT-leaf bytes blob into an Actor.
func DecodeActor(raw []byte) (*Actor, error) {
	br := bytes.NewReader(raw)
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("reading actor header: %w", err)
	}
	if maj != cbg.MajArray {
		return nil, fmt.Errorf("actor not a CBOR array (got major %d)", maj)
	}
	if extra != 4 && extra != 5 {
		return nil, fmt.Errorf("actor array length %d, want 4 or 5", extra)
	}
	hasDelegated := extra == 5

	code, err := readCidLink(br)
	if err != nil {
		return nil, fmt.Errorf("Code: %w", err)
	}
	head, err := readCidLink(br)
	if err != nil {
		return nil, fmt.Errorf("Head: %w", err)
	}

	maj, nonce, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("Nonce: %w", err)
	}
	if maj != cbg.MajUnsignedInt {
		return nil, fmt.Errorf("Nonce not uint (got major %d)", maj)
	}

	var bal big.Int
	if err := bal.UnmarshalCBOR(br); err != nil {
		return nil, fmt.Errorf("Balance: %w", err)
	}

	var dAddr *addr.Address
	if hasDelegated {
		dAddr, err = readOptionalAddr(br)
		if err != nil {
			return nil, fmt.Errorf("DelegatedAddress: %w", err)
		}
	}

	return &Actor{
		Code:             code,
		Head:             head,
		Nonce:            nonce,
		Balance:          bal,
		DelegatedAddress: dAddr,
	}, nil
}

func readOptionalAddr(br io.Reader) (*addr.Address, error) {
	// Probe first byte.
	one := make([]byte, 1)
	if _, err := io.ReadFull(br, one); err != nil {
		return nil, err
	}
	if one[0] == 0xf6 { // CBOR null
		return nil, nil
	}
	// Push back and read as byte-string header.
	combined := io.MultiReader(bytes.NewReader(one), br)
	maj, l, err := cbg.CborReadHeader(combined)
	if err != nil {
		return nil, err
	}
	if maj != cbg.MajByteString {
		return nil, fmt.Errorf("DelegatedAddress not byte-string (got major %d)", maj)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(combined, buf); err != nil {
		return nil, err
	}
	a, err := addr.NewFromBytes(buf)
	if err != nil {
		return nil, fmt.Errorf("parsing address bytes: %w", err)
	}
	return &a, nil
}
