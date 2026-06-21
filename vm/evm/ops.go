package evm

import (
	"errors"
	"fmt"

	"github.com/holiman/uint256"
)

// analyzeJumpdests precomputes the set of valid JUMPDEST positions,
// skipping PUSH immediates (which are data, not code).
func analyzeJumpdests(code []byte) map[uint64]bool {
	dests := make(map[uint64]bool)
	for pc := uint64(0); pc < uint64(len(code)); pc++ {
		op := OpCode(code[pc])
		if op == JUMPDEST {
			dests[pc] = true
		} else if op >= PUSH1 && op <= PUSH32 {
			pc += uint64(op - PUSH1 + 1)
		}
	}
	return dests
}

func (ip *interpreter) binaryOp(op OpCode) error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	b, err := ip.stack.pop()
	if err != nil {
		return err
	}
	var r uint256.Int
	switch op {
	case ADD:
		r.Add(&a, &b)
	case MUL:
		r.Mul(&a, &b)
	case SUB:
		r.Sub(&a, &b)
	case DIV:
		r.Div(&a, &b)
	case SDIV:
		r.SDiv(&a, &b)
	case MOD:
		r.Mod(&a, &b)
	case SMOD:
		r.SMod(&a, &b)
	case EXP:
		r.Exp(&a, &b)
	case SIGNEXTEND:
		r.ExtendSign(&b, &a)
	case LT:
		r = boolWord(a.Lt(&b))
	case GT:
		r = boolWord(a.Gt(&b))
	case SLT:
		r = boolWord(a.Slt(&b))
	case SGT:
		r = boolWord(a.Sgt(&b))
	case EQ:
		r = boolWord(a.Eq(&b))
	case AND:
		r.And(&a, &b)
	case OR:
		r.Or(&a, &b)
	case XOR:
		r.Xor(&a, &b)
	case BYTE:
		r.Byte(&b) // a = index (top), b = value; uint256.Byte uses receiver as value
		// uint256's Byte: z.Byte(n) treats z as the value and n as index.
		// We popped a=index, b=value, so recompute correctly:
		r.Set(&b)
		r.Byte(&a)
	case SHL:
		// a = shift, b = value
		r.Lsh(&b, uint(min64(a.Uint64(), 256)))
		if !a.IsUint64() || a.Uint64() >= 256 {
			r.Clear()
		}
	case SHR:
		r.Rsh(&b, uint(min64(a.Uint64(), 256)))
		if !a.IsUint64() || a.Uint64() >= 256 {
			r.Clear()
		}
	case SAR:
		r.SRsh(&b, uint(min64safeShift(&a)))
	case ADDMOD:
		c, err := ip.stack.pop()
		if err != nil {
			return err
		}
		r.AddMod(&a, &b, &c)
	case MULMOD:
		c, err := ip.stack.pop()
		if err != nil {
			return err
		}
		r.MulMod(&a, &b, &c)
	default:
		return fmt.Errorf("evm: binaryOp on non-binary op %s", op)
	}
	return ip.stack.push(r)
}

func (ip *interpreter) unaryOp(op OpCode) error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	var r uint256.Int
	switch op {
	case ISZERO:
		r = boolWord(a.IsZero())
	case NOT:
		r.Not(&a)
	default:
		return fmt.Errorf("evm: unaryOp on non-unary op %s", op)
	}
	return ip.stack.push(r)
}

func (ip *interpreter) opSha3() error {
	offset, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	data := ip.mem.get(offset.Uint64(), size.Uint64())
	h := keccak256(data)
	var r uint256.Int
	r.SetBytes(h[:])
	return ip.stack.push(r)
}

func (ip *interpreter) opCalldataload() error {
	off, err := ip.stack.pop()
	if err != nil {
		return err
	}
	var buf [32]byte
	o := off.Uint64()
	for i := uint64(0); i < 32; i++ {
		if off.IsUint64() && o+i < uint64(len(ip.input)) {
			buf[i] = ip.input[o+i]
		}
	}
	var r uint256.Int
	r.SetBytes(buf[:])
	return ip.stack.push(r)
}

func (ip *interpreter) opCalldatacopy() error {
	memOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	dataOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	data := copyPadded(ip.input, dataOff.Uint64(), size.Uint64())
	ip.mem.set(memOff.Uint64(), data)
	return nil
}

func (ip *interpreter) opCodecopy() error {
	memOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	codeOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	data := copyPadded(ip.code, codeOff.Uint64(), size.Uint64())
	ip.mem.set(memOff.Uint64(), data)
	return nil
}

func (ip *interpreter) opReturndatacopy() error {
	memOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	dataOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	ip.mem.set(memOff.Uint64(), copyPadded(ip.ret, dataOff.Uint64(), size.Uint64()))
	return nil
}

func (ip *interpreter) opMload() error {
	off, err := ip.stack.pop()
	if err != nil {
		return err
	}
	b := ip.mem.get(off.Uint64(), 32)
	var r uint256.Int
	r.SetBytes(b)
	return ip.stack.push(r)
}

func (ip *interpreter) opMstore() error {
	off, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	ip.mem.set32(off.Uint64(), val.Bytes32())
	return nil
}

func (ip *interpreter) opMstore8() error {
	off, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	ip.mem.set8(off.Uint64(), byte(val.Uint64()))
	return nil
}

func (ip *interpreter) opSload() error {
	key, err := ip.stack.pop()
	if err != nil {
		return err
	}
	// Overlay (in-call SSTORE writes) shadows verified backend state.
	if v, ok := ip.ov.get(ip.self, key); ok {
		return ip.stack.push(v)
	}
	v, err := ip.b.GetStorage(ip.self, key)
	if err != nil {
		return fmt.Errorf("evm: SLOAD: %w", err)
	}
	return ip.stack.push(v)
}

// opSstore records a storage write in the ephemeral overlay. Discarded
// when the top-level eth_call returns; chain state is never mutated.
func (ip *interpreter) opSstore() error {
	key, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	ip.ov.set(ip.self, key, val)
	return nil
}

// opLog consumes a LOGn instruction's operands (mem offset, size, and n
// topics) and discards them. Emitted events don't affect a call's return
// value, so for eth_call purposes this is a well-formed no-op that keeps
// the stack balanced.
func (ip *interpreter) opLog(n int) error {
	if _, err := ip.stack.pop(); err != nil { // mem offset
		return err
	}
	if _, err := ip.stack.pop(); err != nil { // size
		return err
	}
	for i := 0; i < n; i++ {
		if _, err := ip.stack.pop(); err != nil { // topic
			return err
		}
	}
	return nil
}

func (ip *interpreter) opBalance() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	bal, err := ip.b.GetBalance(wordToAddress(a))
	if err != nil {
		return err
	}
	return ip.stack.push(bal)
}

func (ip *interpreter) opJump() error {
	dest, err := ip.stack.pop()
	if err != nil {
		return err
	}
	d := dest.Uint64()
	if !dest.IsUint64() || !ip.jumpdest[d] {
		return errors.New("evm: invalid jump destination")
	}
	ip.pc = d - 1 // run loop will ++ after exec
	return nil
}

func (ip *interpreter) opJumpi() error {
	dest, err := ip.stack.pop()
	if err != nil {
		return err
	}
	cond, err := ip.stack.pop()
	if err != nil {
		return err
	}
	if cond.IsZero() {
		return nil // fall through
	}
	d := dest.Uint64()
	if !dest.IsUint64() || !ip.jumpdest[d] {
		return errors.New("evm: invalid jumpi destination")
	}
	ip.pc = d - 1
	return nil
}

func (ip *interpreter) opExtcodesize() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	code, err := ip.b.GetCode(wordToAddress(a))
	if err != nil {
		return err
	}
	return ip.stack.push(*uint256.NewInt(uint64(len(code))))
}

func (ip *interpreter) opExtcodehash() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	code, err := ip.b.GetCode(wordToAddress(a))
	if err != nil {
		return err
	}
	var r uint256.Int
	if len(code) == 0 {
		return ip.stack.push(r) // empty -> 0 (no-account convention)
	}
	h := keccak256(code)
	r.SetBytes(h[:])
	return ip.stack.push(r)
}

func (ip *interpreter) opExtcodecopy() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	memOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	codeOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	code, err := ip.b.GetCode(wordToAddress(a))
	if err != nil {
		return err
	}
	ip.mem.set(memOff.Uint64(), copyPadded(code, codeOff.Uint64(), size.Uint64()))
	return nil
}

func (ip *interpreter) opReturnData() ([]byte, error) {
	off, err := ip.stack.pop()
	if err != nil {
		return nil, err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return nil, err
	}
	return ip.mem.get(off.Uint64(), size.Uint64()), nil
}

// opCall handles STATICCALL / CALL / DELEGATECALL in read-only mode. A
// nested read-only call into another contract is executed recursively
// against the same backend. CALL/DELEGATECALL with value are treated as
// read-only (value transfer is a no-op on a view call).
func (ip *interpreter) opCall(op OpCode) error {
	// Stack layout differs slightly between CALL (has value) and
	// STATICCALL/DELEGATECALL (no value).
	if _, err := ip.stack.pop(); err != nil { // gas
		return err
	}
	toW, err := ip.stack.pop()
	if err != nil {
		return err
	}
	if op == CALL {
		if _, err := ip.stack.pop(); err != nil { // value (ignored, read-only)
			return err
		}
	}
	inOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	inSize, err := ip.stack.pop()
	if err != nil {
		return err
	}
	retOff, err := ip.stack.pop()
	if err != nil {
		return err
	}
	retSize, err := ip.stack.pop()
	if err != nil {
		return err
	}

	callInput := ip.mem.get(inOff.Uint64(), inSize.Uint64())
	to := wordToAddress(toW)

	// DELEGATECALL keeps self as the storage context; for read-only
	// purposes we still execute the target's code but report self/caller
	// per delegatecall semantics. Cross-contract STATICCALL/CALL execute
	// the target with self=target.
	var sub *Result
	var subErr error
	if op == DELEGATECALL {
		sub, subErr = callWithContext(ip.b, ip.ov, ip.caller, ip.self, ip.self, callInput, to)
	} else {
		sub, subErr = callWithState(ip.b, ip.ov, ip.self, to, callInput)
	}
	if subErr != nil {
		return subErr
	}

	// Write return data into memory (truncated to retSize) and push
	// success (1) / failure (0).
	if sub != nil {
		ip.ret = sub.Return
		out := sub.Return
		if uint64(len(out)) > retSize.Uint64() {
			out = out[:retSize.Uint64()]
		}
		ip.mem.set(retOff.Uint64(), out)
	}
	ok := uint256.NewInt(1)
	if sub == nil || sub.Reverted {
		ok = uint256.NewInt(0)
	}
	return ip.stack.push(*ok)
}

// callWithContext is the DELEGATECALL execution variant: run `to`'s code
// but with storage/self context preserved as `storageCtx`.
func callWithContext(b Backend, ov *overlay, caller, self, storageCtx Address, input []byte, codeAddr Address) (*Result, error) {
	code, err := b.GetCode(codeAddr)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return &Result{}, nil
	}
	ip := &interpreter{
		b: b, ov: ov, caller: caller, self: storageCtx, code: code, input: input,
		stack: newStack(), mem: &memory{}, jumpdest: analyzeJumpdests(code),
	}
	return ip.run()
}

// ---- helpers ----

func boolWord(b bool) uint256.Int {
	var v uint256.Int
	if b {
		v.SetOne()
	}
	return v
}

func copyPadded(src []byte, off, size uint64) []byte {
	out := make([]byte, size)
	if off < uint64(len(src)) {
		copy(out, src[off:min64(off+size, uint64(len(src)))])
	}
	return out
}

func min64safeShift(a *uint256.Int) uint64 {
	if !a.IsUint64() || a.Uint64() >= 256 {
		return 256
	}
	return a.Uint64()
}
