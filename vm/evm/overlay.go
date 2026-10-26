package evm

import "github.com/holiman/uint256"

// overlay is an ephemeral, in-memory state diff used during an eth_call.
//
// Real nodes (go-ethereum, Lotus) execute eth_call against a throwaway
// copy of state: SSTORE/LOG and value transfers are applied to that copy
// and discarded when the call returns. Lantern's read-only interpreter
// previously reverted the instant it hit SSTORE/LOG, which made any call
// that exercises the WRITE path (ERC-20 transferFrom inside a DEX swap,
// router reentrancy, etc.) falsely revert locally even though it succeeds
// on chain. That broke the zero-Glif promise for simulate/estimateGas on
// write-shaped calls.
//
// overlay closes that gap: SSTORE writes here, SLOAD reads here first then
// falls back to the verified backend, and LOG* is a no-op (logs don't
// affect return data). The whole frame -- including nested CALLs into other
// contracts -- shares one overlay so a swap's transferFrom sees its own
// balance writes. The overlay is created per top-level Call and never
// persisted, so chain state is untouched.
//
// It also holds EIP-1153 transient storage (TLOAD/TSTORE). In the FEVM
// transient slots live in the contract actor's state tagged with a
// (origin, nonce) lifespan, so they are visible across every frame of one
// message and reset for the next one. One top-level eth_call is one
// message, so transient storage is scoped to the overlay: shared by all
// nested frames, starting empty for every call.
//
// Both maps are journaled: a sub-call that reverts (or aborts) rolls back
// its own storage and transient writes, exactly as the FVM discards the
// callee actor's state changes on a failed send.
//
// CREATE/CREATE2/SELFDESTRUCT remain unsupported (fault with
// ErrUnsupported): they need contract-creation machinery out of scope here.
type overlay struct {
	// storage maps contract address -> slot -> value for SSTORE writes.
	storage map[Address]map[uint256.Int]uint256.Int
	// transient maps contract address -> slot -> value for TSTORE writes.
	transient map[Address]map[uint256.Int]uint256.Int
	journal   []journalEntry
}

type journalEntry struct {
	transient bool
	addr      Address
	key       uint256.Int
	prev      uint256.Int
	existed   bool
}

func newOverlay() *overlay {
	return &overlay{
		storage:   make(map[Address]map[uint256.Int]uint256.Int),
		transient: make(map[Address]map[uint256.Int]uint256.Int),
	}
}

// get returns (value, true) if slot was written in this overlay.
func (o *overlay) get(addr Address, key uint256.Int) (uint256.Int, bool) {
	if o == nil {
		return uint256.Int{}, false
	}
	return lookup(o.storage, addr, key)
}

// set records an SSTORE write in the overlay.
func (o *overlay) set(addr Address, key, val uint256.Int) {
	if o == nil {
		return
	}
	o.write(false, addr, key, val)
}

// tget returns the transient slot value (zero when never written).
func (o *overlay) tget(addr Address, key uint256.Int) uint256.Int {
	if o == nil {
		return uint256.Int{}
	}
	v, _ := lookup(o.transient, addr, key)
	return v
}

// tset records a TSTORE write.
func (o *overlay) tset(addr Address, key, val uint256.Int) {
	if o == nil {
		return
	}
	o.write(true, addr, key, val)
}

func (o *overlay) write(transient bool, addr Address, key, val uint256.Int) {
	maps := o.storage
	if transient {
		maps = o.transient
	}
	m, ok := maps[addr]
	if !ok {
		m = make(map[uint256.Int]uint256.Int)
		maps[addr] = m
	}
	prev, existed := m[key]
	o.journal = append(o.journal, journalEntry{transient: transient, addr: addr, key: key, prev: prev, existed: existed})
	m[key] = val
}

// snapshot returns a journal mark to roll back to with revert.
func (o *overlay) snapshot() int {
	if o == nil {
		return 0
	}
	return len(o.journal)
}

// revert undoes every write made after the snapshot mark.
func (o *overlay) revert(mark int) {
	if o == nil {
		return
	}
	for i := len(o.journal) - 1; i >= mark; i-- {
		e := o.journal[i]
		maps := o.storage
		if e.transient {
			maps = o.transient
		}
		m := maps[e.addr]
		if e.existed {
			m[e.key] = e.prev
		} else {
			delete(m, e.key)
		}
	}
	o.journal = o.journal[:mark]
}

func lookup(maps map[Address]map[uint256.Int]uint256.Int, addr Address, key uint256.Int) (uint256.Int, bool) {
	m, ok := maps[addr]
	if !ok {
		return uint256.Int{}, false
	}
	v, ok := m[key]
	return v, ok
}
