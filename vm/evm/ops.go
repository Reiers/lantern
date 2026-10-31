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
		// a = index (top), b = value; uint256's z.Byte(n) treats z as the
		// value and n as the index.
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

func (ip *interpreter) opClz() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	// leading zeros of the 256-bit word; CLZ(0) == 256.
	return ip.stack.push(*uint256.NewInt(uint64(256 - a.BitLen())))
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
	off, sz, err := ip.mem.region(offset, size)
	if err != nil {
		return err
	}
	h := keccak256(ip.mem.slice(off, sz))
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
		if off.IsUint64() && o+i < uint64(len(ip.input)) && o+i >= o {
			buf[i] = ip.input[o+i]
		}
	}
	var r uint256.Int
	r.SetBytes(buf[:])
	return ip.stack.push(r)
}

// opCopyToMemory implements CALLDATACOPY / CODECOPY: pop (memOff, dataOff,
// size), copy src[dataOff:dataOff+size] into memory, zero-filling past the
// end of src (FEVM copy_to_memory with zero_fill=true).
func (ip *interpreter) opCopyToMemory(src []byte) error {
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
	return ip.copyToMemory(memOff, size, dataOff, src, true)
}

// copyToMemory mirrors builtin-actors instructions/memory.rs copy_to_memory.
func (ip *interpreter) copyToMemory(destOff, destSize, dataOff uint256.Int, data []byte, zeroFill bool) error {
	off, sz, err := ip.mem.region(destOff, destSize)
	if err != nil {
		return err
	}
	if sz == 0 {
		return nil
	}
	dst := ip.mem.slice(off, sz)
	start := uint64(len(data))
	if dataOff.IsUint64() && dataOff.Uint64() < start {
		start = dataOff.Uint64()
	}
	n := copy(dst, data[start:])
	if zeroFill {
		for i := n; i < len(dst); i++ {
			dst[i] = 0
		}
	}
	return nil
}

// opReturndatacopy faults (FEVM EVM_CONTRACT_ILLEGAL_MEMORY_ACCESS) when
// the source range exceeds the return data, per EIP-211.
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
	off, sz, err := ip.mem.region(memOff, size)
	if err != nil {
		return err
	}
	if !dataOff.IsUint64() || dataOff.Uint64() > uint64(len(ip.ret)) {
		return fmt.Errorf("evm: returndatacopy start exceeds return-data length %d", len(ip.ret))
	}
	src := dataOff.Uint64()
	if src+sz > uint64(len(ip.ret)) {
		return fmt.Errorf("evm: returndatacopy end %d exceeds return-data length %d", src+sz, len(ip.ret))
	}
	copy(ip.mem.slice(off, sz), ip.ret[src:src+sz])
	return nil
}

func (ip *interpreter) opMload() error {
	idx, err := ip.stack.pop()
	if err != nil {
		return err
	}
	off, _, err := ip.mem.region(idx, *uint256.NewInt(32))
	if err != nil {
		return err
	}
	var r uint256.Int
	r.SetBytes(ip.mem.slice(off, 32))
	return ip.stack.push(r)
}

func (ip *interpreter) opMstore() error {
	idx, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	off, _, err := ip.mem.region(idx, *uint256.NewInt(32))
	if err != nil {
		return err
	}
	b := val.Bytes32()
	copy(ip.mem.slice(off, 32), b[:])
	return nil
}

func (ip *interpreter) opMstore8() error {
	idx, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	off, _, err := ip.mem.region(idx, *uint256.NewInt(1))
	if err != nil {
		return err
	}
	ip.mem.store[off] = byte(val.Uint64() & 0xff)
	return nil
}

// opMcopy implements EIP-5656 MCOPY as the FEVM does (instructions/
// memory.rs mcopy): pop (dest, src, size); for size > 0 expand memory to
// cover both src+size and dest+size, then copy as if through an
// intermediate buffer (overlap-safe). Size 0 is a no-op with no expansion.
func (ip *interpreter) opMcopy() error {
	dest, err := ip.stack.pop()
	if err != nil {
		return err
	}
	src, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	if size.IsZero() {
		return nil
	}
	so, sz, err := ip.mem.region(src, size)
	if err != nil {
		return err
	}
	do, _, err := ip.mem.region(dest, size)
	if err != nil {
		return err
	}
	// Go's copy has memmove semantics for overlapping slices.
	copy(ip.mem.store[do:do+sz], ip.mem.store[so:so+sz])
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

// errReadOnly is the FEVM's USR_READ_ONLY abort (state change inside a
// STATICCALL context). Surfaced as a fault so the call is not served
// locally rather than modeling the callee abort.
func (ip *interpreter) errReadOnly(op OpCode) error {
	return fmt.Errorf("%w: %s called while read-only (static context) at pc %d", ErrUnsupported, op, ip.pc)
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
	if ip.readonly {
		return ip.errReadOnly(SSTORE)
	}
	ip.ov.set(ip.self, key, val)
	return nil
}

// opTload reads EIP-1153 transient storage of the current storage context
// (self; the caller's context under DELEGATECALL, as in the FEVM where the
// delegated code runs inside the calling actor).
func (ip *interpreter) opTload() error {
	key, err := ip.stack.pop()
	if err != nil {
		return err
	}
	return ip.stack.push(ip.ov.tget(ip.self, key))
}

// opTstore writes transient storage; read-only contexts abort (FEVM
// storage.rs tstore).
func (ip *interpreter) opTstore() error {
	key, err := ip.stack.pop()
	if err != nil {
		return err
	}
	val, err := ip.stack.pop()
	if err != nil {
		return err
	}
	if ip.readonly {
		return ip.errReadOnly(TSTORE)
	}
	ip.ov.tset(ip.self, key, val)
	return nil
}

// opLog consumes a LOGn instruction's operands (mem offset, size, and n
// topics) and discards them. Emitted events don't affect a call's return
// value, so for eth_call purposes this is a well-formed no-op that keeps
// the stack balanced. The memory region is still validated/expanded and a
// LOG in a read-only context aborts, as in the FEVM.
func (ip *interpreter) opLog(n int) error {
	off, err := ip.stack.pop()
	if err != nil {
		return err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if _, err := ip.stack.pop(); err != nil { // topic
			return err
		}
	}
	if ip.readonly {
		return ip.errReadOnly(LOG0 + OpCode(n))
	}
	_, _, err = ip.mem.region(off, size)
	return err
}

func (ip *interpreter) opBalance() error {
	a, err := ip.stack.pop()
	if err != nil {
		return err
	}
	bal, err := ip.cs.balanceOf(wordToAddress(a))
	if err != nil {
		return err
	}
	return ip.stack.push(bal)
}

// opBlockhash mirrors builtin-actors context.rs blockhash: heights in
// [NUMBER-256, NUMBER) yield the tipset-key CID digest (first 32 bytes),
// anything else is zero.
func (ip *interpreter) opBlockhash() error {
	bn, err := ip.stack.pop()
	if err != nil {
		return err
	}
	curr := ip.b.BlockNumber()
	lo := uint64(0)
	if curr > 256 {
		lo = curr - 256
	}
	if !bn.IsUint64() || bn.Uint64() >= curr || bn.Uint64() < lo || bn.Uint64() > 1<<63-1 {
		return ip.stack.push(uint256.Int{})
	}
	e, err := ip.env(BLOCKHASH)
	if err != nil {
		return err
	}
	digest, err := e.TipsetCIDDigest(bn.Uint64())
	if err != nil {
		return fmt.Errorf("%w: BLOCKHASH(%d): %v", ErrUnsupported, bn.Uint64(), err)
	}
	if len(digest) > 32 {
		digest = digest[:32]
	}
	var r uint256.Int
	r.SetBytes(digest)
	return ip.stack.push(r)
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

// actorKind classifies addr. Without a KindBackend it reports ActorEVM so
// callers fall through to the legacy "code may be empty" path.
func (ip *interpreter) actorKind(a Address) (ActorKind, bool, error) {
	kb, ok := ip.b.(KindBackend)
	if !ok {
		return ActorEVM, false, nil
	}
	k, err := kb.ActorKind(a)
	if err != nil {
		return 0, true, fmt.Errorf("evm: classify %x: %w", a, err)
	}
	return k, true, nil
}

var (
	emptyCodeHash  = keccak256(nil)
	nativeCodeHash = keccak256([]byte{0xfe}) // builtin-actors BytecodeHash::NATIVE_ACTOR
)

func (ip *interpreter) opExtcodesize() error {
	w, err := ip.stack.pop()
	if err != nil {
		return err
	}
	a := wordToAddress(w)
	if isFEVMPrecompile(a) {
		return ip.stack.push(uint256.Int{})
	}
	kind, _, err := ip.actorKind(a)
	if err != nil {
		return err
	}
	switch kind {
	case ActorNative:
		return ip.stack.push(*uint256.NewInt(1)) // FEVM: native actors report size 1
	case ActorAccount, ActorNotFound:
		return ip.stack.push(uint256.Int{})
	}
	code, err := ip.b.GetCode(a)
	if err != nil {
		return err
	}
	return ip.stack.push(*uint256.NewInt(uint64(len(code))))
}

func (ip *interpreter) opExtcodehash() error {
	w, err := ip.stack.pop()
	if err != nil {
		return err
	}
	a := wordToAddress(w)
	var r uint256.Int
	if isFEVMPrecompile(a) {
		r.SetBytes(emptyCodeHash[:])
		return ip.stack.push(r)
	}
	kind, classified, err := ip.actorKind(a)
	if err != nil {
		return err
	}
	switch kind {
	case ActorNative:
		r.SetBytes(nativeCodeHash[:])
		return ip.stack.push(r)
	case ActorAccount:
		r.SetBytes(emptyCodeHash[:])
		return ip.stack.push(r)
	case ActorNotFound:
		return ip.stack.push(r)
	}
	code, err := ip.b.GetCode(a)
	if err != nil {
		return err
	}
	if len(code) == 0 && !classified {
		return ip.stack.push(r) // legacy: empty -> 0 (no-account convention)
	}
	h := keccak256(code)
	r.SetBytes(h[:])
	return ip.stack.push(r)
}

func (ip *interpreter) opExtcodecopy() error {
	w, err := ip.stack.pop()
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
	a := wordToAddress(w)
	var code []byte
	if !isFEVMPrecompile(a) {
		kind, _, err := ip.actorKind(a)
		if err != nil {
			return err
		}
		switch kind {
		case ActorNative:
			code = []byte{0xfe}
		case ActorEVM:
			if code, err = ip.b.GetCode(a); err != nil {
				return err
			}
		}
	}
	return ip.copyToMemory(memOff, size, codeOff, code, true)
}

func (ip *interpreter) opReturnData() ([]byte, error) {
	offset, err := ip.stack.pop()
	if err != nil {
		return nil, err
	}
	size, err := ip.stack.pop()
	if err != nil {
		return nil, err
	}
	off, sz, err := ip.mem.region(offset, size)
	if err != nil {
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	return append([]byte(nil), ip.mem.slice(off, sz)...), nil
}

// opCall handles STATICCALL / CALL / DELEGATECALL following builtin-actors
// instructions/call.rs call_generic:
//
//   - precompile addresses run the precompile (implemented subset; others
//     fault with ErrUnsupported);
//   - EVM contracts execute recursively against the shared overlay; a
//     reverted sub-call rolls back its overlay writes and pushes 0;
//   - account / placeholder / ethaccount / missing actors succeed with
//     empty return data;
//   - DELEGATECALL into a native actor fails (pushes 0); CALL into a
//     native actor is not modeled (fault);
//   - CALL with a non-zero value moves the value on the call overlay when
//     the target is an EVM contract or an account-type actor (EOA,
//     placeholder, burnt funds actor, a new f4 address); value to a native
//     actor, a precompile or an unknown ID address, or more value than the
//     contract holds, is not modeled (fault).
func (ip *interpreter) opCall(op OpCode) error {
	if _, err := ip.stack.pop(); err != nil { // gas (not metered locally)
		return err
	}
	toW, err := ip.stack.pop()
	if err != nil {
		return err
	}
	var value uint256.Int
	if op == CALL {
		if value, err = ip.stack.pop(); err != nil {
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
	if !value.IsZero() && ip.readonly {
		return ip.errReadOnly(op)
	}

	io, is, err := ip.mem.region(inOff, inSize)
	if err != nil {
		return err
	}
	callInput := append([]byte(nil), ip.mem.slice(io, is)...)
	to := wordToAddress(toW)

	var (
		success bool
		ret     []byte
	)
	if isReservedPrecompileAddress(to) {
		if !value.IsZero() {
			return fmt.Errorf("%w: CALL with value to precompile %x at pc %d is not modeled locally", ErrUnsupported, to, ip.pc)
		}
		ret, success, err = ip.callPrecompile(to, callInput)
		if err != nil {
			return err
		}
		ip.subTrace = nil
	} else {
		kind, _, err := ip.actorKind(to)
		if err != nil {
			return err
		}
		if !value.IsZero() {
			switch {
			case kind == ActorNative:
				return fmt.Errorf("%w: CALL with value into native actor %x at pc %d is not modeled locally", ErrUnsupported, to, ip.pc)
			case kind == ActorNotFound && isMaskedIDAddress(to):
				return fmt.Errorf("%w: CALL with value to unknown ID address %x at pc %d is not modeled locally", ErrUnsupported, to, ip.pc)
			}
			bal, err := ip.cs.balanceOf(ip.self)
			if err != nil {
				return err
			}
			if bal.Lt(&value) {
				return fmt.Errorf("%w: CALL value %s exceeds contract balance %s at pc %d (failed send not modeled locally)", ErrUnsupported, value.Dec(), bal.Dec(), ip.pc)
			}
		}
		switch kind {
		case ActorEVM:
			mark := ip.ov.snapshot()
			var f frame
			if op == DELEGATECALL {
				// Run the target's code in our own context: same caller,
				// value, storage and read-only flag.
				f = frame{caller: ip.caller, self: ip.self, codeAddr: to, input: callInput,
					value: ip.value, readonly: ip.readonly, delegate: true, depth: ip.depth + 1}
			} else {
				if err := ip.cs.transfer(ip.self, to, value); err != nil {
					return fmt.Errorf("%w: CALL value transfer at pc %d: %v", ErrUnsupported, ip.pc, err)
				}
				f = frame{caller: ip.self, self: to, codeAddr: to, input: callInput, value: value,
					readonly: ip.readonly || op == STATICCALL, depth: ip.depth + 1}
			}
			sub, err := ip.cs.run(f)
			if err != nil {
				return err
			}
			if sub.Reverted {
				ip.ov.revert(mark)
				ip.subTrace = sub.Trace
			} else {
				ip.subTrace = nil
			}
			success, ret = !sub.Reverted, sub.Return
		case ActorAccount, ActorNotFound:
			// FEVM sends METHOD_SEND to account-type / absent actors: the
			// value moves (an absent f4 address becomes a placeholder).
			if err := ip.cs.transfer(ip.self, to, value); err != nil {
				return fmt.Errorf("%w: CALL value transfer at pc %d: %v", ErrUnsupported, ip.pc, err)
			}
			success = true
			ip.subTrace = nil
		case ActorNative:
			ip.subTrace = nil
			if op != DELEGATECALL {
				return fmt.Errorf("%w: %s into native actor %x is not modeled locally", ErrUnsupported, op, to)
			}
			success = false
		}
	}

	ip.ret = ret
	// FEVM: copy_to_memory(output_offset, output_size, 0, return_data,
	// zero_fill=false): grow to the full output region, copy what fits.
	if err := ip.copyToMemory(retOff, retSize, uint256.Int{}, ret, false); err != nil {
		return err
	}
	ok := uint256.NewInt(0)
	if success {
		ok.SetOne()
	}
	return ip.stack.push(*ok)
}

// ---- helpers ----

// isMaskedIDAddress reports whether a is an ID address in eth form
// (0xff || 11 zero bytes || be64 actor id).
func isMaskedIDAddress(a Address) bool {
	if a[0] != 0xff {
		return false
	}
	for _, b := range a[1:12] {
		if b != 0 {
			return false
		}
	}
	return true
}

func boolWord(b bool) uint256.Int {
	var v uint256.Int
	if b {
		v.SetOne()
	}
	return v
}

func min64safeShift(a *uint256.Int) uint64 {
	if !a.IsUint64() || a.Uint64() >= 256 {
		return 256
	}
	return a.Uint64()
}
