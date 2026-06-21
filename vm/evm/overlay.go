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
// CREATE/CREATE2/SELFDESTRUCT remain unsupported (still revert): they are
// rare on the swap/payment paths we care about and need contract-creation
// machinery out of scope here.
type overlay struct {
	// storage maps contract address -> slot -> value for SSTORE writes.
	storage map[Address]map[uint256.Int]uint256.Int
}

func newOverlay() *overlay {
	return &overlay{storage: make(map[Address]map[uint256.Int]uint256.Int)}
}

// get returns (value, true) if slot was written in this overlay.
func (o *overlay) get(addr Address, key uint256.Int) (uint256.Int, bool) {
	if o == nil {
		return uint256.Int{}, false
	}
	m, ok := o.storage[addr]
	if !ok {
		return uint256.Int{}, false
	}
	v, ok := m[key]
	return v, ok
}

// set records an SSTORE write in the overlay.
func (o *overlay) set(addr Address, key, val uint256.Int) {
	if o == nil {
		return
	}
	m, ok := o.storage[addr]
	if !ok {
		m = make(map[uint256.Int]uint256.Int)
		o.storage[addr] = m
	}
	m[key] = val
}
