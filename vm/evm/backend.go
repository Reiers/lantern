// Package evm is a minimal, read-only EVM interpreter for executing
// view/pure contract calls against locally-verified Filecoin FEVM state
// (lantern#43 Part B, Stage 3).
//
// Scope: read-only `eth_call`. It implements the opcode subset that
// Solidity view/pure functions emit (arithmetic, comparison, bitwise,
// SHA3, environment/calldata, memory, storage SLOAD, control flow, and
// CALL/STATICCALL to other contracts for cross-contract reads). State
// mutation opcodes (SSTORE, CREATE/CREATE2, SELFDESTRUCT, LOG*) are not
// reachable on a read-only call and cause a revert if hit, exactly as a
// staticcall would.
//
// It deliberately avoids vendoring go-ethereum's full core/vm (which drags
// in core/types, params, and the CGo secp256k1 crypto) to keep Lantern
// pure-Go and lean. Word math is holiman/uint256; hashing is keccak from
// golang.org/x/crypto/sha3.
package evm

import (
	"github.com/holiman/uint256"
)

// Backend supplies the contract/world state the interpreter reads. All
// methods are read-only. Implementations back these with Lantern's
// verified state accessor (Stage 1 bytecode loader + Stage 2 KAMT storage
// reader).
type Backend interface {
	// GetCode returns the EVM bytecode for the given 20-byte address.
	// Empty (len 0) for accounts with no code.
	GetCode(addr Address) ([]byte, error)

	// GetStorage returns the 32-byte storage value at slot `key` for the
	// contract at `addr`. Absent slots return the zero word.
	GetStorage(addr Address, key uint256.Int) (uint256.Int, error)

	// GetBalance returns the attoFIL/wei balance of addr as a 256-bit word.
	GetBalance(addr Address) (uint256.Int, error)

	// BlockNumber / Timestamp / ChainID expose the environment a view call
	// observes. These can be the head tipset's epoch/time and the network
	// chain id (314 / 314159).
	BlockNumber() uint64
	Timestamp() uint64
	ChainID() uint64
}

// Address is a 20-byte EVM address.
type Address [20]byte

// BytesToAddress packs the rightmost 20 bytes of b into an Address.
func BytesToAddress(b []byte) Address {
	var a Address
	if len(b) > 20 {
		b = b[len(b)-20:]
	}
	copy(a[20-len(b):], b)
	return a
}

// Bytes returns the 20-byte slice.
func (a Address) Bytes() []byte { return a[:] }
