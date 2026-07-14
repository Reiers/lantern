// Accessor provides high-level actor lookups bound to a TrustedRoot.
//
// Lookup methods return both the decoded value and the proof path (list of
// node CIDs traversed in fetch order). Callers can hand the path to
// VerifyProof to independently re-prove the claim.

package accessor

import (
	"bytes"
	"context"
	"fmt"

	addr "github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/state/hamt"
)

// Accessor is the public surface used by every RPC handler. It is goroutine-
// safe: each method captures the trusted-root pointer at entry and reads
// only via the supplied BlockGetter.
type Accessor struct {
	tr *trustedroot.TrustedRoot
	bg hamt.BlockGetter

	// headState, when set, returns the state root of the current verified
	// header-store head (typically head.ParentStateRoot). State reads use
	// it instead of the frozen boot trusted-root state root, so actor
	// lookups follow the live chain instead of aging out at the boot epoch
	// (lantern#87: without this the accessor is pinned to the boot anchor
	// and StateMinerPower/StateMinerInfo fail once upstreams prune the
	// stale boot state root). The (cid, ok) shape lets the provider signal
	// "no live head yet" so the accessor falls back to the boot root during
	// the first moments after start. The provider must be goroutine-safe;
	// the header store's Head() already is.
	headState func() (cid.Cid, bool)
}

// New returns an Accessor bound to a TrustedRoot and a BlockGetter.
func New(tr *trustedroot.TrustedRoot, bg hamt.BlockGetter) *Accessor {
	return &Accessor{tr: tr, bg: bg}
}

// TrustedRoot returns the bound root.
func (a *Accessor) TrustedRoot() *trustedroot.TrustedRoot { return a.tr }

// SetHeadStateProvider wires an optional live-head state-root provider.
// When set, state reads resolve against the provider's state root instead
// of the frozen boot trusted-root, so the accessor follows the chain. Pass
// nil to revert to boot-anchored reads. Safe to call once at wiring time.
func (a *Accessor) SetHeadStateProvider(fn func() (cid.Cid, bool)) {
	a.headState = fn
}

// effectiveStateRoot returns the state root state reads should resolve
// against: the live head's state root when a provider is wired and has a
// head, else the frozen boot trusted-root state root.
func (a *Accessor) effectiveStateRoot() cid.Cid {
	if a.headState != nil {
		if sr, ok := a.headState(); ok && sr.Defined() {
			return sr
		}
	}
	return a.tr.StateRoot
}

// loadStateRoot fetches the [version, actorsRoot, infoRoot] tuple and
// returns the actors-tree HAMT CID plus the proof for that step.
func (a *Accessor) loadStateRoot(ctx context.Context) (*StateRoot, []cid.Cid, error) {
	stateRoot := a.effectiveStateRoot()
	raw, err := a.bg.Get(ctx, stateRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch state root %s: %w", stateRoot, err)
	}
	if err := hamt.VerifyBlockCID(stateRoot, raw); err != nil {
		return nil, nil, fmt.Errorf("state root: %w", err)
	}
	sr, err := DecodeStateRoot(raw)
	if err != nil {
		return nil, nil, err
	}
	return sr, []cid.Cid{stateRoot}, nil
}

// GetActor resolves `target` (any-protocol address) to its ID address via
// the Init actor, then walks the state-tree HAMT to the actor's state.
// Returns the Actor + the full proof path (state-root block + Init-actor
// HAMT path + state-tree HAMT path).
func (a *Accessor) GetActor(ctx context.Context, target addr.Address) (*Actor, []cid.Cid, error) {
	idAddr, idProof, err := a.LookupID(ctx, target)
	if err != nil {
		return nil, idProof, fmt.Errorf("resolving %s to ID: %w", target, err)
	}
	actor, treeProof, err := a.getActorByID(ctx, idAddr)
	if err != nil {
		return nil, append(idProof, treeProof...), err
	}
	full := make([]cid.Cid, 0, len(idProof)+len(treeProof))
	full = append(full, idProof...)
	full = append(full, treeProof...)
	return actor, full, nil
}

// getActorByID walks just the state-tree HAMT (no Init actor lookup).
// Caller must pass an ID-address.
func (a *Accessor) getActorByID(ctx context.Context, idAddr addr.Address) (*Actor, []cid.Cid, error) {
	if idAddr.Protocol() != addr.ID {
		return nil, nil, fmt.Errorf("getActorByID requires an ID address, got %s", idAddr)
	}

	sr, srProof, err := a.loadStateRoot(ctx)
	if err != nil {
		return nil, srProof, err
	}

	// State-tree HAMT key is the raw address bytes (abi.AddrKey).
	key := idAddr.Bytes()

	raw, hamtProof, err := hamt.Lookup(ctx, sr.Actors, key, a.bg, nil)
	full := append(srProof, hamtProof...)
	if err != nil {
		return nil, full, fmt.Errorf("state-tree HAMT lookup for %s: %w", idAddr, err)
	}
	actor, err := DecodeActor(raw)
	if err != nil {
		return nil, full, fmt.Errorf("decoding actor %s: %w", idAddr, err)
	}
	return actor, full, nil
}

// GetActorByID is the ID-only fast path. Use it when you already have an
// ID address and want to skip the Init-actor resolve step.
func (a *Accessor) GetActorByID(ctx context.Context, idAddr addr.Address) (*Actor, []cid.Cid, error) {
	return a.getActorByID(ctx, idAddr)
}

// LookupID resolves an address to its ID-address. ID addresses pass through
// unchanged (with no proof needed beyond the bound TrustedRoot).
func (a *Accessor) LookupID(ctx context.Context, target addr.Address) (addr.Address, []cid.Cid, error) {
	if target.Protocol() == addr.ID {
		return target, nil, nil
	}

	// 1. Load the Init actor (singleton at f01).
	initIDAddr, _ := addr.NewIDAddress(1) // builtin.InitActorAddr = f01

	sr, srProof, err := a.loadStateRoot(ctx)
	if err != nil {
		return addr.Undef, srProof, err
	}
	initActorBytes, treeProof, err := hamt.Lookup(ctx, sr.Actors, initIDAddr.Bytes(), a.bg, nil)
	proof := append(srProof, treeProof...)
	if err != nil {
		return addr.Undef, proof, fmt.Errorf("looking up init actor in state tree: %w", err)
	}
	initActor, err := DecodeActor(initActorBytes)
	if err != nil {
		return addr.Undef, proof, fmt.Errorf("decoding init actor: %w", err)
	}

	// 2. Load the Init actor's state and pull AddressMap CID.
	initState, err := a.bg.Get(ctx, initActor.Head)
	if err != nil {
		return addr.Undef, proof, fmt.Errorf("fetch init state %s: %w", initActor.Head, err)
	}
	if err := hamt.VerifyBlockCID(initActor.Head, initState); err != nil {
		return addr.Undef, proof, fmt.Errorf("init state: %w", err)
	}
	proof = append(proof, initActor.Head)

	addrMapCID, err := decodeInitAddressMapCID(initState)
	if err != nil {
		return addr.Undef, proof, fmt.Errorf("decoding init state: %w", err)
	}

	// 3. Walk the Init actor's AddressMap HAMT.
	idBytes, mapProof, err := hamt.Lookup(ctx, addrMapCID, target.Bytes(), a.bg, nil)
	proof = append(proof, mapProof...)
	if err != nil {
		if err == hamt.ErrNotFound {
			return addr.Undef, proof, ErrAddressNotFound
		}
		return addr.Undef, proof, fmt.Errorf("walking init AddressMap for %s: %w", target, err)
	}

	// 4. Decode actor ID (CBOR cbg.CborInt, i.e. a varint).
	var actorID cbg.CborInt
	if err := actorID.UnmarshalCBOR(bytes.NewReader(idBytes)); err != nil {
		return addr.Undef, proof, fmt.Errorf("decoding actor ID: %w", err)
	}
	resolved, err := addr.NewIDAddress(uint64(actorID))
	if err != nil {
		return addr.Undef, proof, fmt.Errorf("constructing ID address: %w", err)
	}
	return resolved, proof, nil
}

// ErrAddressNotFound is returned when an address is not in the Init actor
// AddressMap. This is normal for never-funded addresses on chain.
var ErrAddressNotFound = fmt.Errorf("address not found in init actor AddressMap")

// decodeInitAddressMapCID parses the first field of the Init actor's state
// CBOR struct. The Init.State layout (since v8): [AddressMap cid, NextID
// uint, NetworkName string]. We only need the first field.
func decodeInitAddressMapCID(raw []byte) (cid.Cid, error) {
	br := bytes.NewReader(raw)
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return cid.Undef, err
	}
	if maj != cbg.MajArray {
		return cid.Undef, fmt.Errorf("init state not a CBOR array")
	}
	if extra < 3 {
		return cid.Undef, fmt.Errorf("init state array length %d, want >=3", extra)
	}
	return readCidLink(br)
}
