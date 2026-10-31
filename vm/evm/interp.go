package evm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// Result is the outcome of a read-only EVM execution.
type Result struct {
	// Return is the RETURN data (for a successful call) or the REVERT
	// data (when Reverted is true).
	Return []byte
	// Reverted is true if the call ended in REVERT or INVALID.
	Reverted bool
	// GasUsed is a coarse gas counter (read calls are not gas-billed on
	// chain; this is informational / for an estimate ceiling).
	GasUsed uint64
	// Trace is the failure backtrace of a reverted call, outermost frame
	// first: the frame that ended the call, followed by the chain of
	// reverted sub-calls it bubbled up from (the FVM backtrace Lotus
	// prints in "vm error=[...]"). Empty for a successful call.
	Trace []TraceFrame
}

// Exit codes of the FEVM EVM actor (builtin-actors actors/evm).
const (
	// ExitContractReverted is EVM_CONTRACT_REVERTED (REVERT).
	ExitContractReverted = 33
	// ExitInvalidInstruction is EVM_CONTRACT_INVALID_INSTRUCTION (INVALID).
	ExitInvalidInstruction = 34
)

// TraceFrame is one frame of a revert backtrace.
type TraceFrame struct {
	// Addr is the actor the frame ran in (for a DELEGATECALL frame: the
	// calling contract, as the FVM sends InvokeContractDelegate to self).
	Addr Address
	// Delegate is true for a DELEGATECALL frame (FVM method 6).
	Delegate bool
	// PC is the program counter of the REVERT / INVALID that ended it.
	PC uint64
	// Exit is the actor exit code (ExitContractReverted, ...).
	Exit uint32
}

// ExitCode returns the exit code of a reverted result (33 when unknown).
func (r *Result) ExitCode() uint32 {
	if r != nil && len(r.Trace) > 0 && r.Trace[0].Exit != 0 {
		return r.Trace[0].Exit
	}
	return ExitContractReverted
}

// InsufficientFundsError is returned by CallWithValue when the caller's
// balance does not cover the call value. On chain the message fails
// before any code runs (SysErrInsufficientFunds, exit 6).
type InsufficientFundsError struct {
	Balance uint256.Int
	Value   uint256.Int
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("sender does not have funds to transfer (balance %s, transfer %s)", e.Balance.Dec(), e.Value.Dec())
}

// ErrExecutionReverted signals the call reverted; Result.Return carries
// the revert payload (ABI-encoded Error(string) etc.). Maps to eth JSON-RPC
// error code 3.
var ErrExecutionReverted = errors.New("execution reverted")

// ErrUnsupported is wrapped by every fault where Lantern cannot reproduce
// the FEVM result locally: an opcode the FEVM does not define (BLOBHASH,
// BLOBBASEFEE, CALLCODE, ...), an environment value the backend can't
// supply, contract creation, value transfers, unimplemented precompiles,
// resource caps. Callers must never turn such a fault into a result;
// eth_call falls back to the bridge or returns an error.
var ErrUnsupported = errors.New("evm: unsupported")

// callMaxSteps bounds interpreter steps (across all frames of one call) so
// a malformed view call can't spin forever. View functions are short;
// this is generous.
const callMaxSteps = 10_000_000

// maxCallDepth mirrors the FVM call-depth limit. Deeper nesting is not
// served locally.
const maxCallDepth = 1024

// blockGasLimit is the FEVM GASLIMIT constant (builtin-actors context.rs).
const blockGasLimit = 10_000_000_000

// clzMinNetworkVersion is the first network version whose EVM actor
// (v18) defines CLZ.
const clzMinNetworkVersion = 28

// Call executes `input` against the code at `to` and returns the result.
//
// eth_call semantics: state mutations (SSTORE / TSTORE) and LOGs that
// happen during the call are applied to an ephemeral overlay that is
// discarded when the call returns -- chain state is never touched. This is
// what lets write-shaped calls (ERC-20 transferFrom inside a DEX swap,
// router reentrancy) execute correctly instead of falsely reverting.
// ORIGIN is the eth_call sender for every frame.
func Call(b Backend, caller, to Address, input []byte) (*Result, error) {
	return CallWithValue(b, caller, to, input, uint256.Int{})
}

// CallWithValue is Call with a message value (eth_call / eth_estimateGas
// "value"): the caller must hold at least value (else
// *InsufficientFundsError, nothing runs), the value is moved from caller
// to the target on the ephemeral overlay (so BALANCE / SELFBALANCE see
// it), and CALLVALUE reads it. Nothing is transferred on chain.
//
// Value moved onwards by the contract (CALL{value}) is modeled for EVM
// contracts and account-type actors (EOAs, placeholders, the burnt funds
// actor); value to a native actor or a precompile, or a CALL{value} the
// contract can't cover, faults with ErrUnsupported instead of guessing.
func CallWithValue(b Backend, caller, to Address, input []byte, value uint256.Int) (*Result, error) {
	cs := &callState{b: b, ov: newOverlay(), origin: caller}
	if !value.IsZero() {
		if err := cs.transfer(caller, to, value); err != nil {
			var ife *InsufficientFundsError
			if errors.As(err, &ife) {
				return nil, ife
			}
			return nil, err
		}
	}
	return cs.run(frame{caller: caller, self: to, codeAddr: to, input: input, value: value})
}

// balanceOf returns addr's balance: the verified backend balance plus the
// value moved in / out during this call.
func (cs *callState) balanceOf(a Address) (uint256.Int, error) {
	base, err := cs.b.GetBalance(a)
	if err != nil {
		return uint256.Int{}, err
	}
	d := cs.ov.balanceDelta(a)
	if d.Sign() == 0 {
		return base, nil
	}
	eff := new(big.Int).Add(base.ToBig(), d)
	if eff.Sign() < 0 || eff.BitLen() > 256 {
		return uint256.Int{}, fmt.Errorf("%w: balance of %x out of range after value transfers", ErrUnsupported, a)
	}
	var out uint256.Int
	out.SetFromBig(eff)
	return out, nil
}

// transfer moves value from -> to on the overlay. An insufficient balance
// is reported as *InsufficientFundsError.
func (cs *callState) transfer(from, to Address, value uint256.Int) error {
	if value.IsZero() {
		return nil
	}
	bal, err := cs.balanceOf(from)
	if err != nil {
		return fmt.Errorf("evm: balance %x: %w", from, err)
	}
	if bal.Lt(&value) {
		return &InsufficientFundsError{Balance: bal, Value: value}
	}
	v := value.ToBig()
	cs.ov.addBalance(from, new(big.Int).Neg(v))
	cs.ov.addBalance(to, v)
	return nil
}

// callState is shared by every frame of one top-level call.
type callState struct {
	b      Backend
	ov     *overlay
	origin Address
	steps  int
}

// frame describes one execution context.
type frame struct {
	caller   Address
	self     Address // storage / ADDRESS context
	codeAddr Address // whose bytecode runs (differs from self for DELEGATECALL)
	input    []byte
	value    uint256.Int
	readonly bool // STATICCALL context (sticky for nested frames)
	delegate bool // DELEGATECALL frame (FVM InvokeContractDelegate)
	depth    int
}

func (cs *callState) run(f frame) (*Result, error) {
	if f.depth > maxCallDepth {
		return nil, fmt.Errorf("%w: call depth limit (%d) exceeded", ErrUnsupported, maxCallDepth)
	}
	code, err := cs.b.GetCode(f.codeAddr)
	if err != nil {
		return nil, fmt.Errorf("evm: get code %x: %w", f.codeAddr, err)
	}
	if len(code) == 0 {
		// Calling an account with no code returns empty success.
		return &Result{Return: nil}, nil
	}
	ip := newInterpreter(cs, f, code)
	return ip.run()
}

func newInterpreter(cs *callState, f frame, code []byte) *interpreter {
	return &interpreter{
		b:        cs.b,
		cs:       cs,
		ov:       cs.ov,
		caller:   f.caller,
		self:     f.self,
		value:    f.value,
		readonly: f.readonly,
		delegate: f.delegate,
		depth:    f.depth,
		code:     code,
		input:    f.input,
		stack:    newStack(),
		mem:      &memory{},
		jumpdest: analyzeJumpdests(code),
	}
}

type interpreter struct {
	b        Backend
	cs       *callState
	ov       *overlay
	caller   Address
	self     Address
	value    uint256.Int
	readonly bool
	delegate bool
	depth    int
	code     []byte
	input    []byte
	stack    *stack
	mem      *memory
	pc       uint64
	ret      []byte
	jumpdest map[uint64]bool
	gas      uint64
	// subTrace is the backtrace of the most recent sub-call when it
	// failed (nil once a later sub-call succeeds); a REVERT in this frame
	// appends it below its own frame.
	subTrace []TraceFrame
}

// revertTrace builds the backtrace for a frame ending at the current pc.
func (ip *interpreter) revertTrace(exit uint32) []TraceFrame {
	tr := make([]TraceFrame, 0, 1+len(ip.subTrace))
	tr = append(tr, TraceFrame{Addr: ip.self, Delegate: ip.delegate, PC: ip.pc, Exit: exit})
	return append(tr, ip.subTrace...)
}

func (ip *interpreter) run() (*Result, error) {
	for ip.pc < uint64(len(ip.code)) {
		ip.cs.steps++
		if ip.cs.steps > callMaxSteps {
			return nil, fmt.Errorf("%w: step limit exceeded", ErrUnsupported)
		}
		op := OpCode(ip.code[ip.pc])
		ip.gas += op.baseGas()

		switch {
		case op >= PUSH1 && op <= PUSH32:
			n := int(op - PUSH1 + 1)
			if err := ip.pushImmediate(n); err != nil {
				return nil, err
			}
			continue
		case op >= DUP1 && op <= DUP16:
			if err := ip.stack.dup(int(op - DUP1 + 1)); err != nil {
				return nil, err
			}
		case op >= SWAP1 && op <= SWAP16:
			if err := ip.stack.swap(int(op - SWAP1 + 1)); err != nil {
				return nil, err
			}
		default:
			done, res, err := ip.exec(op)
			if err != nil {
				return nil, err
			}
			if done {
				return res, nil
			}
		}
		ip.pc++
	}
	// Falling off the end is an implicit STOP -> empty success.
	return &Result{Return: nil, GasUsed: ip.gas}, nil
}

// pushImmediate reads the next n code bytes as a big-endian word and pushes
// it, advancing pc past the immediate. Bytes past the end of code read as
// zero (FEVM pads truncated immediates the same way).
func (ip *interpreter) pushImmediate(n int) error {
	start := ip.pc + 1
	end := start + uint64(n)
	var buf [32]byte
	for i := uint64(0); i < uint64(n); i++ {
		if start+i < uint64(len(ip.code)) {
			buf[32-uint64(n)+i] = ip.code[start+i]
		}
	}
	var v uint256.Int
	v.SetBytes(buf[:])
	ip.pc = end
	return ip.stack.push(v)
}

// errUndefined is the FEVM's EVM_CONTRACT_UNDEFINED_INSTRUCTION abort.
func (ip *interpreter) errUndefined(op OpCode) error {
	return fmt.Errorf("%w: opcode 0x%02x (%s) is undefined in the FEVM at pc %d", ErrUnsupported, byte(op), op.String(), ip.pc)
}

// env returns the optional environment backend or an ErrUnsupported fault.
func (ip *interpreter) env(op OpCode) (EnvBackend, error) {
	e, ok := ip.b.(EnvBackend)
	if !ok {
		return nil, fmt.Errorf("%w: %s needs execution environment the backend does not provide", ErrUnsupported, op)
	}
	return e, nil
}

func (ip *interpreter) envWord(op OpCode, get func(EnvBackend) (uint256.Int, error)) error {
	e, err := ip.env(op)
	if err != nil {
		return err
	}
	v, err := get(e)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupported, op, err)
	}
	return ip.stack.push(v)
}

// exec runs a single non-PUSH/DUP/SWAP opcode. Returns done=true with a
// Result when the call terminates (STOP/RETURN/REVERT/INVALID).
func (ip *interpreter) exec(op OpCode) (bool, *Result, error) {
	switch op {
	case STOP:
		return true, &Result{Return: nil, GasUsed: ip.gas}, nil

	// ---- arithmetic ----
	case ADD, MUL, SUB, DIV, SDIV, MOD, SMOD, EXP, SIGNEXTEND,
		LT, GT, SLT, SGT, EQ, AND, OR, XOR, BYTE, SHL, SHR, SAR,
		ADDMOD, MULMOD:
		return false, nil, ip.binaryOp(op)
	case ISZERO, NOT:
		return false, nil, ip.unaryOp(op)
	case CLZ:
		// EIP-7939, defined by the EVM actor from v18 (NV28).
		if e, ok := ip.b.(EnvBackend); ok {
			if nv, known := e.NetworkVersion(); known && nv < clzMinNetworkVersion {
				return false, nil, ip.errUndefined(op)
			}
		}
		return false, nil, ip.opClz()

	case SHA3:
		return false, nil, ip.opSha3()

	// ---- environment / calldata ----
	case ADDRESS:
		return false, nil, ip.stack.push(addrToWord(ip.self))
	case CALLER:
		return false, nil, ip.stack.push(addrToWord(ip.caller))
	case ORIGIN:
		// FEVM: the eth address of the message origin, identical in every
		// frame (not the immediate caller).
		return false, nil, ip.stack.push(addrToWord(ip.cs.origin))
	case CALLVALUE:
		return false, nil, ip.stack.push(ip.value)
	case CALLDATASIZE:
		return false, nil, ip.stack.push(*uint256.NewInt(uint64(len(ip.input))))
	case CALLDATALOAD:
		return false, nil, ip.opCalldataload()
	case CALLDATACOPY:
		return false, nil, ip.opCopyToMemory(ip.input)
	case CODESIZE:
		return false, nil, ip.stack.push(*uint256.NewInt(uint64(len(ip.code))))
	case CODECOPY:
		return false, nil, ip.opCopyToMemory(ip.code)
	case RETURNDATASIZE:
		return false, nil, ip.stack.push(*uint256.NewInt(uint64(len(ip.ret))))
	case RETURNDATACOPY:
		return false, nil, ip.opReturndatacopy()
	case GAS:
		// Not gas-metered locally; a large constant keeps gasleft() checks
		// passing. (FEVM returns the real remaining gas.)
		return false, nil, ip.stack.push(*uint256.NewInt(0xffffffff))
	case GASPRICE:
		// FEVM: base_fee + message gas premium. eth_call messages carry a
		// zero premium (Lotus EthCall.ToFilecoinMessage), so this is the
		// base fee.
		return false, nil, ip.envWord(op, func(e EnvBackend) (uint256.Int, error) { return e.BaseFee() })
	case CHAINID:
		return false, nil, ip.stack.push(*uint256.NewInt(ip.b.ChainID()))
	case NUMBER:
		return false, nil, ip.stack.push(*uint256.NewInt(ip.b.BlockNumber()))
	case TIMESTAMP:
		return false, nil, ip.stack.push(*uint256.NewInt(ip.b.Timestamp()))
	case COINBASE:
		// FEVM: always the zero address (multiple winners per tipset).
		return false, nil, ip.stack.push(uint256.Int{})
	case GASLIMIT:
		// FEVM: the constant block gas limit.
		return false, nil, ip.stack.push(*uint256.NewInt(blockGasLimit))
	case BASEFEE:
		return false, nil, ip.envWord(op, func(e EnvBackend) (uint256.Int, error) { return e.BaseFee() })
	case PREVRANDAO:
		return false, nil, ip.envWord(op, func(e EnvBackend) (uint256.Int, error) { return e.PrevRandao() })
	case BLOCKHASH:
		return false, nil, ip.opBlockhash()
	case SELFBALANCE:
		bal, err := ip.cs.balanceOf(ip.self)
		if err != nil {
			return false, nil, err
		}
		return false, nil, ip.stack.push(bal)
	case BALANCE:
		return false, nil, ip.opBalance()

	// ---- memory ----
	case MLOAD:
		return false, nil, ip.opMload()
	case MSTORE:
		return false, nil, ip.opMstore()
	case MSTORE8:
		return false, nil, ip.opMstore8()
	case MSIZE:
		return false, nil, ip.stack.push(*uint256.NewInt(uint64(ip.mem.len())))
	case MCOPY:
		return false, nil, ip.opMcopy()

	// ---- storage ----
	case SLOAD:
		return false, nil, ip.opSload()
	case SSTORE:
		return false, nil, ip.opSstore()
	case TLOAD:
		return false, nil, ip.opTload()
	case TSTORE:
		return false, nil, ip.opTstore()

	// ---- logs: observable side effects, no effect on return data ----
	case LOG0, LOG1, LOG2, LOG3, LOG4:
		return false, nil, ip.opLog(int(op - LOG0))

	// ---- control flow ----
	case POP:
		_, err := ip.stack.pop()
		return false, nil, err
	case JUMP:
		return false, nil, ip.opJump()
	case JUMPI:
		return false, nil, ip.opJumpi()
	case JUMPDEST:
		// no-op marker
	case PC:
		return false, nil, ip.stack.push(*uint256.NewInt(ip.pc))
	case PUSH0:
		return false, nil, ip.stack.push(uint256.Int{})

	// ---- external code reads (for cross-contract view CALLs) ----
	case EXTCODESIZE:
		return false, nil, ip.opExtcodesize()
	case EXTCODEHASH:
		return false, nil, ip.opExtcodehash()
	case EXTCODECOPY:
		return false, nil, ip.opExtcodecopy()

	// ---- calls (read-only sub-calls) ----
	case STATICCALL, CALL, DELEGATECALL:
		return false, nil, ip.opCall(op)

	// ---- terminators ----
	case RETURN:
		data, err := ip.opReturnData()
		if err != nil {
			return false, nil, err
		}
		return true, &Result{Return: data, GasUsed: ip.gas}, nil
	case REVERT:
		data, err := ip.opReturnData()
		if err != nil {
			return false, nil, err
		}
		return true, &Result{Return: data, Reverted: true, GasUsed: ip.gas, Trace: ip.revertTrace(ExitContractReverted)}, nil

	// ---- contract creation / destruction: not modeled locally ----
	case CREATE, CREATE2, SELFDESTRUCT:
		return false, nil, fmt.Errorf("%w: %s at pc %d (contract creation/destruction is not modeled locally)", ErrUnsupported, op, ip.pc)

	case INVALID:
		return true, &Result{Reverted: true, GasUsed: ip.gas, Trace: ip.revertTrace(ExitInvalidInstruction)}, nil

	// ---- defined in Ethereum (Cancun) but not by the FEVM ----
	case BLOBHASH, BLOBBASEFEE, CALLCODE:
		return false, nil, ip.errUndefined(op)

	default:
		return false, nil, fmt.Errorf("%w: unsupported opcode 0x%02x (%s) at pc %d", ErrUnsupported, byte(op), op.String(), ip.pc)
	}
	return false, nil, nil
}

// keccak256 returns the Keccak-256 of b.
func keccak256(b []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func addrToWord(a Address) uint256.Int {
	var v uint256.Int
	v.SetBytes(a[:])
	return v
}

func wordToAddress(v uint256.Int) Address {
	b := v.Bytes32()
	return BytesToAddress(b[:])
}
