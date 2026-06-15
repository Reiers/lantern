package evm

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// Result is the outcome of a read-only EVM execution.
type Result struct {
	// Return is the RETURN data (for a successful call) or the REVERT
	// data (when Reverted is true).
	Return []byte
	// Reverted is true if the call ended in REVERT or a fault that maps to
	// a revert (e.g. an unsupported state-mutation opcode on a read call).
	Reverted bool
	// GasUsed is a coarse gas counter (read calls are not gas-billed on
	// chain; this is informational / for an estimate ceiling).
	GasUsed uint64
}

// ErrExecutionReverted signals the call reverted; Result.Return carries
// the revert payload (ABI-encoded Error(string) etc.). Maps to eth JSON-RPC
// error code 3.
var ErrExecutionReverted = errors.New("execution reverted")

// callMaxSteps bounds interpreter steps so a malformed view call can't spin
// forever. View functions are short; this is generous.
const callMaxSteps = 10_000_000

// Call executes `input` against the code at `to` in read-only mode and
// returns the result. `caller` is the msg.sender observed by the code.
func Call(b Backend, caller, to Address, input []byte) (*Result, error) {
	code, err := b.GetCode(to)
	if err != nil {
		return nil, fmt.Errorf("evm: get code %x: %w", to, err)
	}
	if len(code) == 0 {
		// Calling an account with no code returns empty success.
		return &Result{Return: nil}, nil
	}
	ip := &interpreter{
		b:        b,
		caller:   caller,
		self:     to,
		code:     code,
		input:    input,
		stack:    newStack(),
		mem:      &memory{},
		jumpdest: analyzeJumpdests(code),
	}
	return ip.run()
}

type interpreter struct {
	b        Backend
	caller   Address
	self     Address
	code     []byte
	input    []byte
	stack    *stack
	mem      *memory
	pc       uint64
	ret      []byte
	jumpdest map[uint64]bool
	gas      uint64
}

func (ip *interpreter) run() (*Result, error) {
	steps := 0
	for ip.pc < uint64(len(ip.code)) {
		if steps++; steps > callMaxSteps {
			return nil, errors.New("evm: step limit exceeded")
		}
		op := OpCode(ip.code[ip.pc])
		ip.gas += op.baseGas()

		switch {
		case op >= PUSH1 && op <= PUSH32:
			n := int(op - PUSH1 + 1)
			ip.pushImmediate(n)
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
// it, advancing pc past the immediate.
func (ip *interpreter) pushImmediate(n int) {
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
	ip.stack.push(v)
	ip.pc = end
}

// exec runs a single non-PUSH/DUP/SWAP opcode. Returns done=true with a
// Result when the call terminates (STOP/RETURN/REVERT/invalid).
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

	case SHA3:
		return false, nil, ip.opSha3()

	// ---- environment / calldata ----
	case ADDRESS:
		ip.stack.push(addrToWord(ip.self))
	case CALLER:
		ip.stack.push(addrToWord(ip.caller))
	case ORIGIN:
		ip.stack.push(addrToWord(ip.caller))
	case CALLVALUE:
		ip.stack.push(uint256.Int{})
	case CALLDATASIZE:
		ip.stack.push(*uint256.NewInt(uint64(len(ip.input))))
	case CALLDATALOAD:
		return false, nil, ip.opCalldataload()
	case CALLDATACOPY:
		return false, nil, ip.opCalldatacopy()
	case CODESIZE:
		ip.stack.push(*uint256.NewInt(uint64(len(ip.code))))
	case CODECOPY:
		return false, nil, ip.opCodecopy()
	case RETURNDATASIZE:
		ip.stack.push(*uint256.NewInt(uint64(len(ip.ret))))
	case RETURNDATACOPY:
		return false, nil, ip.opReturndatacopy()
	case GAS:
		ip.stack.push(*uint256.NewInt(0xffffffff))
	case CHAINID:
		ip.stack.push(*uint256.NewInt(ip.b.ChainID()))
	case NUMBER:
		ip.stack.push(*uint256.NewInt(ip.b.BlockNumber()))
	case TIMESTAMP:
		ip.stack.push(*uint256.NewInt(ip.b.Timestamp()))
	case SELFBALANCE:
		bal, err := ip.b.GetBalance(ip.self)
		if err != nil {
			return false, nil, err
		}
		ip.stack.push(bal)
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
		ip.stack.push(*uint256.NewInt(uint64(ip.mem.len())))

	// ---- storage (read-only) ----
	case SLOAD:
		return false, nil, ip.opSload()

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
		ip.stack.push(*uint256.NewInt(ip.pc))
	case PUSH0:
		ip.stack.push(uint256.Int{})

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
		return true, &Result{Return: data, Reverted: true, GasUsed: ip.gas}, nil

	// ---- state mutation: unreachable on a read call ----
	case SSTORE, CREATE, CREATE2, SELFDESTRUCT,
		LOG0, LOG1, LOG2, LOG3, LOG4:
		return true, &Result{Reverted: true, GasUsed: ip.gas}, nil

	case INVALID:
		return true, &Result{Reverted: true, GasUsed: ip.gas}, nil

	default:
		return false, nil, fmt.Errorf("evm: unsupported opcode 0x%02x (%s) at pc %d", byte(op), op.String(), ip.pc)
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
