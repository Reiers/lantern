// StateReadState actor-state decoding (lantern#3, Part A: CBOR system actors).
//
// Lotus's StateReadState returns the actor's state decoded into a JSON object
// whose fields match the actor's Go struct (e.g. f04 -> TotalRawBytePower,
// TotalQualityAdjPower, TotalPledgeCollateral, ...). Lantern historically
// returned the raw CBOR head bytes here, because the light client didn't ship
// actor-state decoders for this path.
//
// We already vendor go-state-types and verify the actor head block against its
// CID in the accessor. The go-state-types State structs JSON-marshal with the
// exact field names Lotus exposes, so decoding the head CBOR into the
// versioned struct and returning that struct gives a Lotus-compatible
// StateReadState for the system actors — with zero new trust, no network
// dependency, and no FEVM. This closes the Glif dependency for filcensus's
// network-truth headline numbers (pledge, deal count, datacap) and for any
// curio-core read path that uses StateReadState on the system actors.
//
// EVM contract actors (eth_call against a deployed contract) are NOT handled
// here — that needs local FEVM execution and is tracked as lantern#3 Part B
// (see docs/issues/0003-local-fevm-eth-call.md). Unknown / unsupported actors
// fall back to the historical raw-bytes behaviour, so this change is strictly
// additive and backward compatible.

package handlers

import (
	"bytes"
	"context"
	"io"

	"github.com/ipfs/go-cid"

	power17 "github.com/filecoin-project/go-state-types/builtin/v17/power"
	power18 "github.com/filecoin-project/go-state-types/builtin/v18/power"

	market17 "github.com/filecoin-project/go-state-types/builtin/v17/market"
	market18 "github.com/filecoin-project/go-state-types/builtin/v18/market"

	verifreg17 "github.com/filecoin-project/go-state-types/builtin/v17/verifreg"
	verifreg18 "github.com/filecoin-project/go-state-types/builtin/v18/verifreg"

	"github.com/Reiers/lantern/state/actors"
)

// cborState is anything that decodes itself from CBOR. The vendored
// go-state-types State structs all satisfy this.
type cborState interface {
	UnmarshalCBOR(r io.Reader) error
}

// decodeSystemActorState attempts to decode the head CBOR of a known system
// actor (power / market / verifreg) into its versioned go-state-types struct.
// The returned value JSON-marshals with Lotus-compatible field names.
//
// Returns (decoded, true, nil) on success, (nil, false, nil) when the actor
// is not a system actor we decode (caller should fall back to raw bytes), and
// (nil, false, err) when the actor IS one we handle but its CBOR failed to
// decode (a real error worth surfacing).
func decodeSystemActorState(reg *actors.Registry, code cid.Cid, head []byte) (any, bool, error) {
	info, ok := reg.Lookup(code)
	if !ok {
		return nil, false, nil
	}

	var target cborState
	switch info.Kind {
	case actors.KindPower:
		switch info.Version {
		case 18:
			target = &power18.State{}
		case 17:
			target = &power17.State{}
		}
	case actors.KindMarket:
		switch info.Version {
		case 18:
			target = &market18.State{}
		case 17:
			target = &market17.State{}
		}
	case actors.KindVerifreg:
		switch info.Version {
		case 18:
			target = &verifreg18.State{}
		case 17:
			target = &verifreg17.State{}
		}
	}
	if target == nil {
		// Either not a system actor we decode, or a version we don't yet
		// support. Fall back to raw bytes rather than erroring.
		return nil, false, nil
	}

	if err := target.UnmarshalCBOR(bytes.NewReader(head)); err != nil {
		return nil, false, err
	}
	return target, true, nil
}

// systemActorRegistry is a tiny indirection so the handler can reach the
// accessor's registry without importing accessor internals.
func (c *ChainAPI) systemActorRegistry(_ context.Context) *actors.Registry {
	return c.Accessor.Registry()
}
