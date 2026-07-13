package fvmkernel

// StateTree: pure-Go actor / balance / address-resolution store shared
// across nested invocation frames (lantern#129, Stage C3).
//
// Not a real HAMT: this prototype keeps actors + address map in Go maps.
// The transactional semantics (Snapshot/Restore) are what matter — a
// nested frame's mutations only reach the parent tree if it exits 0,
// otherwise they roll back. That is the semantics ref-fvm implements
// with its call-manager transactions.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/ipfs/go-cid"
)

// TokenAmount is a 128-bit unsigned integer stored as two u64s, matching
// ref-fvm's u128-in-syscalls convention (see `sys::TokenAmount`).
type TokenAmount struct {
	Hi uint64
	Lo uint64
}

func (t TokenAmount) IsZero() bool { return t.Hi == 0 && t.Lo == 0 }

func (t TokenAmount) Add(u TokenAmount) TokenAmount {
	lo := t.Lo + u.Lo
	hi := t.Hi + u.Hi
	if lo < t.Lo { // carry
		hi++
	}
	return TokenAmount{Hi: hi, Lo: lo}
}

// Sub returns (t - u, ok). ok=false on underflow, in which case the
// result is undefined and MUST NOT be applied.
func (t TokenAmount) Sub(u TokenAmount) (TokenAmount, bool) {
	if t.Hi < u.Hi || (t.Hi == u.Hi && t.Lo < u.Lo) {
		return TokenAmount{}, false
	}
	borrow := uint64(0)
	if t.Lo < u.Lo {
		borrow = 1
	}
	return TokenAmount{Hi: t.Hi - u.Hi - borrow, Lo: t.Lo - u.Lo}, true
}

func (t TokenAmount) String() string { return fmt.Sprintf("{hi:%d lo:%d}", t.Hi, t.Lo) }

// TokenFromU64 constructs a TokenAmount from a plain u64 (Lo only).
func TokenFromU64(v uint64) TokenAmount { return TokenAmount{Lo: v} }

// Address matches Filecoin's on-wire address format: 1 protocol byte +
// payload. Protocols:
//
//	0 = ID (payload = uvarint id)
//	1 = Secp256k1 (payload = 20-byte hash)
//	2 = Actor     (payload = 20-byte hash)
//	3 = BLS       (payload = 48-byte pubkey)
//	4 = Delegated (f4: namespace + subaddress)
type Address struct {
	Protocol byte
	Payload  []byte
}

func (a Address) IsID() bool { return a.Protocol == 0 }

// IDValue extracts the id from a protocol-0 (id) address.
func (a Address) IDValue() (uint64, bool) {
	if !a.IsID() {
		return 0, false
	}
	v, n := binary.Uvarint(a.Payload)
	if n <= 0 {
		return 0, false
	}
	return v, true
}

// String returns a stable key form for map lookups; NOT the canonical
// Filecoin address string ("f0123" etc), just a deterministic hex.
func (a Address) String() string {
	return fmt.Sprintf("f%d/%s", a.Protocol, hex.EncodeToString(a.Payload))
}

// Bytes returns the wire encoding (protocol byte + payload).
func (a Address) Bytes() []byte {
	b := make([]byte, 1+len(a.Payload))
	b[0] = a.Protocol
	copy(b[1:], a.Payload)
	return b
}

// IDAddress builds a protocol-0 id-address for the given actor id.
func IDAddress(id uint64) Address {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, id)
	return Address{Protocol: 0, Payload: append([]byte(nil), buf[:n]...)}
}

// ParseAddress reads a wire-encoded address (protocol byte + payload).
func ParseAddress(raw []byte) (Address, error) {
	if len(raw) < 1 {
		return Address{}, fmt.Errorf("address too short")
	}
	return Address{Protocol: raw[0], Payload: append([]byte(nil), raw[1:]...)}, nil
}

// ActorState is one entry in the state tree.
type ActorState struct {
	CodeCID   cid.Cid     // which builtin/user actor code
	StateRoot cid.Cid     // root of the actor's per-actor state
	Balance   TokenAmount // FIL balance
	Nonce     uint64      // for messages (top-level only)
}

// StateTree is the shared world state across nested frames.
//
// It is transactional via Snapshot/Restore: a nested frame gets a deep
// copy of the mutable maps + counters. On exit 0 the caller keeps the
// child's snapshot (commit). On any non-zero exit or trap, the caller
// restores from its own pre-child snapshot (rollback). The blockstore
// is content-addressed / append-only so it is NEVER rolled back — that
// is a real property of Filecoin's IPLD store.
type StateTree struct {
	bs Blockstore

	// actor id -> state
	actors map[uint64]ActorState

	// init actor's robust-address map: Address.String() -> id
	addrMap map[string]uint64

	// next id to assign for a new (robust-address) actor
	nextActorID uint64

	// code CID keystring -> builtin type id (immutable, shared)
	builtinTypes map[string]int32

	// code CID keystring -> WASM bytecode (immutable, shared)
	actorWasm map[string][]byte
}

// NewStateTree builds an empty state tree bound to a blockstore.
func NewStateTree(bs Blockstore) *StateTree {
	return &StateTree{
		bs:           bs,
		actors:       make(map[uint64]ActorState),
		addrMap:      make(map[string]uint64),
		nextActorID:  100, // start above the small-id reserved range (0=system, 1=init, ...)
		builtinTypes: make(map[string]int32),
		actorWasm:    make(map[string][]byte),
	}
}

// Blockstore exposes the shared content-addressed store.
func (t *StateTree) Blockstore() Blockstore { return t.bs }

// RegisterBuiltin binds a code CID to a builtin type + WASM bytecode.
// The registry is shared across all frames (immutable at runtime).
func (t *StateTree) RegisterBuiltin(codeCID cid.Cid, typeID int32, wasm []byte) {
	t.builtinTypes[codeCID.KeyString()] = typeID
	t.actorWasm[codeCID.KeyString()] = wasm
}

// SetActor places an actor at a specific id (harness setup).
func (t *StateTree) SetActor(id uint64, as ActorState) {
	t.actors[id] = as
	if id >= t.nextActorID {
		t.nextActorID = id + 1
	}
}

// GetActor returns the actor at id and whether it exists.
func (t *StateTree) GetActor(id uint64) (ActorState, bool) {
	as, ok := t.actors[id]
	return as, ok
}

// SetAddrMapping binds a robust address to an id (harness setup or
// init.Exec output).
func (t *StateTree) SetAddrMapping(a Address, id uint64) {
	if a.IsID() {
		return
	}
	t.addrMap[a.String()] = id
}

// LookupID resolves an address to an id. For id addresses it decodes
// the payload; for robust addresses it consults the address map.
func (t *StateTree) LookupID(a Address) (uint64, bool) {
	if a.IsID() {
		return a.IDValue()
	}
	id, ok := t.addrMap[a.String()]
	return id, ok
}

// AssignID assigns a fresh id-address to a robust address, mirroring
// init.Exec's on-demand id allocation. Returns the existing id if
// already resolved.
func (t *StateTree) AssignID(a Address) uint64 {
	if id, ok := t.LookupID(a); ok {
		return id
	}
	id := t.nextActorID
	t.nextActorID++
	t.addrMap[a.String()] = id
	return id
}

// Transfer moves value from -> to. Returns false on underflow; caller
// MUST NOT apply anything else on false.
func (t *StateTree) Transfer(from, to uint64, amount TokenAmount) bool {
	if amount.IsZero() {
		return true
	}
	src, ok := t.actors[from]
	if !ok {
		return false
	}
	newSrc, ok := src.Balance.Sub(amount)
	if !ok {
		return false
	}
	dst := t.actors[to] // zero if absent
	src.Balance = newSrc
	dst.Balance = dst.Balance.Add(amount)
	t.actors[from] = src
	t.actors[to] = dst
	return true
}

// Snapshot returns a deep copy of the mutable state (actors, addrMap,
// nextActorID). Blockstore and builtin registry are content-addressed
// / immutable and are shared.
func (t *StateTree) Snapshot() *StateTree {
	c := &StateTree{
		bs:           t.bs,
		actors:       make(map[uint64]ActorState, len(t.actors)),
		addrMap:      make(map[string]uint64, len(t.addrMap)),
		nextActorID:  t.nextActorID,
		builtinTypes: t.builtinTypes,
		actorWasm:    t.actorWasm,
	}
	for k, v := range t.actors {
		c.actors[k] = v
	}
	for k, v := range t.addrMap {
		c.addrMap[k] = v
	}
	return c
}

// Restore rolls this state tree's mutable fields back to a snapshot.
func (t *StateTree) Restore(snap *StateTree) {
	t.actors = snap.actors
	t.addrMap = snap.addrMap
	t.nextActorID = snap.nextActorID
}

// ActorType returns the builtin type id of the actor at id, or 0.
func (t *StateTree) ActorType(id uint64) int32 {
	as, ok := t.actors[id]
	if !ok {
		return 0
	}
	return t.builtinTypes[as.CodeCID.KeyString()]
}

// ActorWASM returns the bytecode for the actor at id.
func (t *StateTree) ActorWASM(id uint64) ([]byte, bool) {
	as, ok := t.actors[id]
	if !ok {
		return nil, false
	}
	w, ok := t.actorWasm[as.CodeCID.KeyString()]
	return w, ok
}

// SetStateRoot updates an existing actor's state root (frame commit).
func (t *StateTree) SetStateRoot(id uint64, root cid.Cid) {
	as, ok := t.actors[id]
	if !ok {
		return
	}
	as.StateRoot = root
	t.actors[id] = as
}
