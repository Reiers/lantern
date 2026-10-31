// Package evm is a minimal, read-only EVM interpreter for executing
// view/pure contract calls against locally-verified Filecoin FEVM state
// (lantern#43 Part B, Stage 3).
//
// Scope: local `eth_call`. It implements the instruction set of the
// Filecoin EVM actor (builtin-actors actors/evm, v16..v19, i.e. NV24..NV29)
// with FEVM semantics where they differ from Ethereum: arithmetic,
// comparison, bitwise (incl. CLZ from NV28), KECCAK256, environment /
// calldata, memory (incl. MCOPY), storage (SLOAD/SSTORE on an ephemeral
// overlay), transient storage (TLOAD/TSTORE, EIP-1153), control flow, LOGs
// and CALL/STATICCALL/DELEGATECALL for cross-contract reads.
//
// Call value is modeled for eth_call / eth_estimateGas semantics
// (CallWithValue): CALLVALUE, the sender balance check, and balance moves
// on the ephemeral overlay for CALL{value} into EVM contracts and
// account-type actors. Nothing is ever transferred on chain.
//
// Anything Lantern cannot reproduce exactly (an opcode FEVM does not
// define, an environment value the backend can't supply, contract
// creation, value sent to a native actor or a precompile, an
// unimplemented precompile) faults with an
// error wrapping ErrUnsupported. The eth_call handler treats every fault as
// "not served locally" and falls back to the bridge (or errors when the
// bridge is off); it never turns a fault into a result.
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
	// observes: FEVM's NUMBER is the epoch of the tipset the call executes
	// in (Lotus eth_call "latest" = head's parent), TIMESTAMP is that
	// tipset's min timestamp, CHAINID is 314 / 314159.
	BlockNumber() uint64
	Timestamp() uint64
	ChainID() uint64
}

// ActorKind classifies an address the way the FEVM's get_contract_type
// does (builtin-actors actors/evm/src/interpreter/instructions/ext.rs).
type ActorKind int

const (
	// ActorNotFound: no actor at this address.
	ActorNotFound ActorKind = iota
	// ActorAccount: account, placeholder or ethaccount actor.
	ActorAccount
	// ActorEVM: an EVM contract actor.
	ActorEVM
	// ActorNative: any other builtin actor (miner, multisig, ...).
	ActorNative
)

// KindBackend is optionally implemented by a Backend that can classify
// addresses. It is needed for FEVM-exact EXTCODESIZE / EXTCODEHASH /
// EXTCODECOPY and CALL semantics on non-EVM actors. Backends without it get
// the legacy "empty code" behaviour (used by offline test harnesses).
type KindBackend interface {
	ActorKind(addr Address) (ActorKind, error)
}

// EnvBackend is optionally implemented by a Backend that can supply the
// rest of the block / message environment FEVM exposes. When the backend
// doesn't implement it (or a method returns an error) the opcode faults
// with ErrUnsupported instead of guessing a value.
type EnvBackend interface {
	// BaseFee is the base fee of the execution tipset (FEVM BASEFEE; also
	// GASPRICE, since eth_call messages carry a zero gas premium).
	BaseFee() (uint256.Int, error)
	// TipsetCIDDigest returns the multihash digest of the tipset-key CID
	// at epoch (null rounds resolve to the nearest earlier tipset, as the
	// FVM's tipset_cid does). Used by BLOCKHASH.
	TipsetCIDDigest(epoch uint64) ([]byte, error)
	// PrevRandao returns the FEVM PREVRANDAO word: beacon randomness for
	// the execution epoch drawn with DST EvmPrevRandao and entropy
	// "prevrandao".
	PrevRandao() (uint256.Int, error)
	// NetworkVersion returns the network version of the execution epoch
	// and whether it is known. Used to gate instructions added by actor
	// upgrades (CLZ, NV28 / actors v18).
	NetworkVersion() (uint64, bool)
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
