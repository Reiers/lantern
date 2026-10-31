package evm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/holiman/uint256"
)

// ---- tiny assembler ----

// asm assembles test bytecode. OpCode -> the byte; int / uint64 /
// uint256.Int -> minimal PUSH (PUSH0 for zero); Address -> PUSH20;
// []byte -> raw bytes.
func asm(items ...any) []byte {
	var out []byte
	pushWord := func(v uint256.Int) {
		if v.IsZero() {
			out = append(out, byte(PUSH0))
			return
		}
		b := v.Bytes()
		out = append(out, byte(PUSH1)+byte(len(b)-1))
		out = append(out, b...)
	}
	for _, it := range items {
		switch v := it.(type) {
		case OpCode:
			out = append(out, byte(v))
		case int:
			pushWord(*uint256.NewInt(uint64(v)))
		case uint64:
			pushWord(*uint256.NewInt(v))
		case uint256.Int:
			pushWord(v)
		case Address:
			out = append(out, byte(PUSH1)+19)
			out = append(out, v[:]...)
		case []byte:
			out = append(out, v...)
		default:
			panic("asm: bad item")
		}
	}
	return out
}

// retTop returns the top stack word as 32 bytes.
func retTop() []any { return []any{PUSH0, MSTORE, 32, PUSH0, RETURN} }

func prog(items ...any) []byte {
	var flat []any
	for _, it := range items {
		if s, ok := it.([]any); ok {
			flat = append(flat, s...)
		} else {
			flat = append(flat, it)
		}
	}
	return asm(flat...)
}

func word(t *testing.T, res *Result) uint256.Int {
	t.Helper()
	if res.Reverted {
		t.Fatalf("unexpected revert: %x", res.Return)
	}
	if len(res.Return) != 32 {
		t.Fatalf("return len %d, want 32 (%x)", len(res.Return), res.Return)
	}
	var v uint256.Int
	v.SetBytes(res.Return)
	return v
}

func mustRun(t *testing.T, be Backend, code []byte) *Result {
	t.Helper()
	res, err := runRaw(be, code)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func wantUnsupported(t *testing.T, be Backend, code []byte) {
	t.Helper()
	_, err := runRaw(be, code)
	if err == nil {
		t.Fatal("expected a fault, got a result")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("fault should wrap ErrUnsupported, got %v", err)
	}
}

// ---- multi-contract test backend ----

type testBackend struct {
	code     map[Address][]byte
	storage  map[Address]map[uint256.Int]uint256.Int
	balances map[Address]uint256.Int
	number   uint64
	tstamp   uint64
}

func newTestBackend() *testBackend {
	return &testBackend{
		code:     map[Address][]byte{},
		storage:  map[Address]map[uint256.Int]uint256.Int{},
		balances: map[Address]uint256.Int{},
		number:   1000,
		tstamp:   1_700_000_000,
	}
}

func (b *testBackend) GetCode(a Address) ([]byte, error) { return b.code[a], nil }
func (b *testBackend) GetStorage(a Address, k uint256.Int) (uint256.Int, error) {
	return b.storage[a][k], nil
}
func (b *testBackend) GetBalance(a Address) (uint256.Int, error) { return b.balances[a], nil }
func (b *testBackend) BlockNumber() uint64                       { return b.number }
func (b *testBackend) Timestamp() uint64                         { return b.tstamp }
func (b *testBackend) ChainID() uint64                           { return 314159 }

// kindBackend adds ActorKind (FEVM get_contract_type).
type kindBackend struct {
	*testBackend
	kinds map[Address]ActorKind
}

func (b *kindBackend) ActorKind(a Address) (ActorKind, error) {
	if k, ok := b.kinds[a]; ok {
		return k, nil
	}
	if len(b.code[a]) > 0 {
		return ActorEVM, nil
	}
	return ActorNotFound, nil
}

// envBackend adds the execution environment.
type envBackend struct {
	*testBackend
	baseFee uint256.Int
	rand    uint256.Int
	nv      uint64
	digests map[uint64][]byte
}

func (b *envBackend) BaseFee() (uint256.Int, error)    { return b.baseFee, nil }
func (b *envBackend) PrevRandao() (uint256.Int, error) { return b.rand, nil }
func (b *envBackend) NetworkVersion() (uint64, bool)   { return b.nv, b.nv != 0 }
func (b *envBackend) TipsetCIDDigest(e uint64) ([]byte, error) {
	d, ok := b.digests[e]
	if !ok {
		return nil, errors.New("no tipset")
	}
	return d, nil
}

func addr(n byte) Address { return Address{0: 0xaa, 19: n} }

// ---- PUSH0 ----

func TestOp_PUSH0(t *testing.T) {
	// PUSH1 7, PUSH0, ADD -> 7 (PUSH0 pushed a zero word, stack depth 2).
	res := mustRun(t, &nullBackend{}, prog(7, PUSH0, ADD, retTop()))
	if v := word(t, res); v.Uint64() != 7 {
		t.Fatalf("got %d", v.Uint64())
	}
}

// ---- MCOPY (EIP-5656): FEVM edge semantics beyond the #177 basics ----

func TestOp_MCOPY_OverlapForwardAndBackward(t *testing.T) {
	// EIP-5656 test vectors: memory 0x0001..1f (32 bytes, byte i = i).
	base := make([]byte, 32)
	for i := range base {
		base[i] = byte(i)
	}
	w := new(uint256.Int).SetBytes(base)
	// Forward overlap: MCOPY(dest=1, src=0, size=8)
	code := prog(*w, PUSH0, MSTORE, 8, PUSH0, 1, MCOPY, 32, PUSH0, RETURN)
	res := mustRun(t, &nullBackend{}, code)
	want := append([]byte{0, 0, 1, 2, 3, 4, 5, 6, 7}, base[9:]...)
	if !bytes.Equal(res.Return, want) {
		t.Fatalf("forward overlap: got %x want %x", res.Return, want)
	}
	// Backward overlap: MCOPY(dest=0, src=1, size=8)
	code = prog(*w, PUSH0, MSTORE, 8, 1, PUSH0, MCOPY, 32, PUSH0, RETURN)
	res = mustRun(t, &nullBackend{}, code)
	want = append([]byte{1, 2, 3, 4, 5, 6, 7, 8}, base[8:]...)
	if !bytes.Equal(res.Return, want) {
		t.Fatalf("backward overlap: got %x want %x", res.Return, want)
	}
}

func TestOp_MCOPY_ZeroSizeNoExpansion(t *testing.T) {
	// MCOPY(dest=0x1000, src=0x2000, size=0) then MSIZE -> 0. A zero-size
	// copy ignores offsets entirely (even > u32).
	huge := new(uint256.Int).Lsh(uint256.NewInt(1), 200)
	code := prog(PUSH0, *huge, *huge, MCOPY, PUSH0, 0x2000, 0x1000, MCOPY, MSIZE, retTop())
	res := mustRun(t, &nullBackend{}, code)
	// retTop's MSTORE grows memory, but MSIZE was sampled first.
	if v := word(t, res); !v.IsZero() {
		t.Fatalf("MSIZE after zero-size MCOPY = %d, want 0", v.Uint64())
	}
}

func TestOp_MCOPY_ExpandsToMaxOfSrcAndDest(t *testing.T) {
	// MCOPY(dest=0, src=100, size=1): memory grows to cover src+size=101
	// -> 128 (word aligned), copied byte is zero.
	code := prog(1, 100, PUSH0, MCOPY, MSIZE, retTop())
	if v := word(t, mustRun(t, &nullBackend{}, code)); v.Uint64() != 128 {
		t.Fatalf("MSIZE = %d, want 128", v.Uint64())
	}
	// MCOPY(dest=200, src=0, size=1) -> 224
	code = prog(1, PUSH0, 200, MCOPY, MSIZE, retTop())
	if v := word(t, mustRun(t, &nullBackend{}, code)); v.Uint64() != 224 {
		t.Fatalf("MSIZE = %d, want 224", v.Uint64())
	}
}

func TestOp_MCOPY_IllegalOffsetFaults(t *testing.T) {
	huge := new(uint256.Int).Lsh(uint256.NewInt(1), 40) // > u32
	code := prog(1, *huge, PUSH0, MCOPY, STOP)
	if _, err := runRaw(&nullBackend{}, code); err == nil {
		t.Fatal("MCOPY with src > u32 must fault, not succeed")
	}
}

// ---- TLOAD / TSTORE (EIP-1153): scoping, rollback, static context ----

// callAndReturnWord: STATICCALL/CALL target with no input, return the
// callee's 32-byte output.
func callTarget(op OpCode, target Address) []any {
	items := []any{32, PUSH0, PUSH0, PUSH0}
	if op == CALL {
		items = append(items, PUSH0) // value
	}
	return append(items, target, GAS, op)
}

func TestOp_Transient_SharedAcrossFramesAndScopedPerCall(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	// B: TSTORE(slot 1, 99 + TLOAD(1)) then return TLOAD(1)
	be.code[b] = prog(1, TLOAD, 99, ADD, 1, TSTORE, 1, TLOAD, retTop())
	// A: CALL B twice (output of the second call is returned). Transient
	// state of B persists across the two frames of the same message.
	be.code[a] = prog(callTarget(CALL, b), POP, callTarget(CALL, b), POP, 32, PUSH0, RETURN)
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := word(t, res); v.Uint64() != 198 {
		t.Fatalf("second frame saw transient %d, want 198 (99+99)", v.Uint64())
	}
	// A fresh eth_call starts with empty transient storage.
	res, err = Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := word(t, res); v.Uint64() != 198 {
		t.Fatalf("transient leaked across calls: %d", v.Uint64())
	}
}

func TestOp_Transient_RevertedSubcallRollsBack(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	// B: TSTORE(1, 5); SSTORE(2, 6); REVERT
	be.code[b] = prog(5, 1, TSTORE, 6, 2, SSTORE, PUSH0, PUSH0, REVERT)
	// B2 reads B's state through A? Simpler: A delegatecalls B so writes
	// land in A's context, then reads them back.
	// A: DELEGATECALL B (reverts) ; return TLOAD(1) + SLOAD(2)
	be.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, b, GAS, DELEGATECALL, POP,
		1, TLOAD, 2, SLOAD, ADD, retTop())
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := word(t, res); !v.IsZero() {
		t.Fatalf("reverted sub-call writes survived: %d", v.Uint64())
	}
}

func TestOp_Transient_DelegatecallUsesCallerContext(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	be.code[b] = prog(77, 3, TSTORE, STOP)
	be.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, b, GAS, DELEGATECALL, POP, 3, TLOAD, retTop())
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := word(t, res); v.Uint64() != 77 {
		t.Fatalf("got %d, want 77", v.Uint64())
	}
}

func TestOp_TSTORE_InStaticContextFaults(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	be.code[b] = prog(1, 1, TSTORE, STOP)
	be.code[a] = prog(callTarget(STATICCALL, b), retTop())
	_, err := Call(be, Address{}, a, nil)
	if err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("TSTORE under STATICCALL must fault, got %v", err)
	}
	// TLOAD under STATICCALL is fine.
	be.code[b] = prog(1, TLOAD, retTop())
	if _, err := Call(be, Address{}, a, nil); err != nil {
		t.Fatalf("TLOAD under STATICCALL: %v", err)
	}
}

func TestOp_SSTORE_InStaticContextFaults(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	be.code[b] = prog(1, 1, SSTORE, STOP)
	be.code[a] = prog(callTarget(STATICCALL, b), retTop())
	if _, err := Call(be, Address{}, a, nil); err == nil {
		t.Fatal("SSTORE under STATICCALL must fault")
	}
}

// ---- CLZ (EIP-7939, FEVM v18 / NV28) ----

func TestOp_CLZ(t *testing.T) {
	max := new(uint256.Int).SetAllOne()
	top := new(uint256.Int).Lsh(uint256.NewInt(1), 255)
	cases := []struct {
		in   uint256.Int
		want uint64
	}{
		{uint256.Int{}, 256},
		{*uint256.NewInt(1), 255},
		{*uint256.NewInt(0xff), 248},
		{*top, 0},
		{*max, 0},
		{*new(uint256.Int).Lsh(uint256.NewInt(1), 128), 127},
	}
	for _, c := range cases {
		v := word(t, mustRun(t, &nullBackend{}, prog(c.in, CLZ, retTop())))
		if v.Uint64() != c.want {
			t.Errorf("CLZ(%s) = %d, want %d", c.in.Hex(), v.Uint64(), c.want)
		}
	}
}

func TestOp_CLZ_UndefinedBeforeNV28(t *testing.T) {
	be := &envBackend{testBackend: newTestBackend(), nv: 27}
	wantUnsupported(t, be, prog(1, CLZ, retTop()))
	be.nv = 28
	mustRun(t, be, prog(1, CLZ, retTop()))
}

// ---- environment ----

func TestOp_EnvironmentFEVM(t *testing.T) {
	be := &envBackend{testBackend: newTestBackend(), nv: 29}
	be.baseFee = *uint256.NewInt(100)
	be.rand = *new(uint256.Int).SetBytes(bytes.Repeat([]byte{0xab}, 32))
	be.balances[Address{}] = *uint256.NewInt(5)

	check := func(name string, items []any, want uint256.Int) {
		t.Helper()
		v := word(t, mustRun(t, be, prog(append(items, retTop()...)...)))
		if !v.Eq(&want) {
			t.Errorf("%s = %s, want %s", name, v.Hex(), want.Hex())
		}
	}
	check("BASEFEE", []any{BASEFEE}, *uint256.NewInt(100))
	check("GASPRICE", []any{GASPRICE}, *uint256.NewInt(100)) // base fee + zero premium
	check("PREVRANDAO", []any{PREVRANDAO}, be.rand)
	check("COINBASE", []any{COINBASE}, uint256.Int{})
	check("GASLIMIT", []any{GASLIMIT}, *uint256.NewInt(10_000_000_000))
	check("CHAINID", []any{CHAINID}, *uint256.NewInt(314159))
	check("NUMBER", []any{NUMBER}, *uint256.NewInt(1000))
	check("TIMESTAMP", []any{TIMESTAMP}, *uint256.NewInt(1_700_000_000))
	check("SELFBALANCE", []any{SELFBALANCE}, *uint256.NewInt(5))
	check("CALLVALUE", []any{CALLVALUE}, uint256.Int{})
}

func TestOp_EnvironmentWithoutEnvBackendFaults(t *testing.T) {
	for _, op := range []OpCode{BASEFEE, GASPRICE, PREVRANDAO} {
		wantUnsupported(t, &nullBackend{}, prog(op, retTop()))
	}
}

func TestOp_BLOCKHASH(t *testing.T) {
	be := &envBackend{testBackend: newTestBackend(), nv: 29}
	d := bytes.Repeat([]byte{0x5a}, 32)
	be.digests = map[uint64][]byte{999: d, 744: d}
	check := func(n uint64, want []byte) {
		t.Helper()
		res := mustRun(t, be, prog(n, BLOCKHASH, retTop()))
		if !bytes.Equal(res.Return, want) {
			t.Errorf("BLOCKHASH(%d) = %x, want %x", n, res.Return, want)
		}
	}
	zero := make([]byte, 32)
	check(999, d)     // NUMBER-1
	check(744, d)     // NUMBER-256
	check(743, zero)  // NUMBER-257: out of window
	check(1000, zero) // current
	check(5000, zero) // future
	// In-window height the backend can't resolve -> fault, never zero.
	wantUnsupported(t, be, prog(900, BLOCKHASH, retTop()))
	// Out-of-window heights don't need the env at all.
	mustRun(t, &nullBackend{}, prog(5000, BLOCKHASH, retTop()))
}

func TestOp_ORIGIN_IsTopLevelSenderInNestedFrames(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	sender := Address{19: 0x77}
	be.code[b] = prog(ORIGIN, retTop())
	be.code[a] = prog(callTarget(STATICCALL, b), 32, PUSH0, RETURN)
	res, err := Call(be, sender, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := wordToAddress(word(t, res)); got != sender {
		t.Fatalf("ORIGIN in nested frame = %x, want %x", got, sender)
	}
}

// ---- opcodes the FEVM leaves undefined ----

func TestOp_FEVMUndefinedOpcodesFault(t *testing.T) {
	for _, op := range []OpCode{BLOBHASH, BLOBBASEFEE, CALLCODE, 0x0c, 0x21, 0xef} {
		var code []byte
		if op == BLOBHASH {
			code = prog(PUSH0, op, retTop())
		} else {
			code = []byte{byte(op)}
		}
		_, err := runRaw(&nullBackend{}, code)
		if err == nil || !errors.Is(err, ErrUnsupported) {
			t.Errorf("opcode 0x%02x: want ErrUnsupported fault, got %v", byte(op), err)
		}
	}
}

func TestOp_CreateAndSelfdestructAreNotServedLocally(t *testing.T) {
	for _, op := range []OpCode{CREATE, CREATE2, SELFDESTRUCT} {
		wantUnsupported(t, &nullBackend{}, prog(PUSH0, PUSH0, PUSH0, PUSH0, op, STOP))
	}
}

// A CALL{value} the contract can't cover would fail on chain in a way the
// local EVM does not reproduce: not served (value_test.go covers the
// modeled transfers).
func TestOp_CallWithValueBeyondBalanceNotServedLocally(t *testing.T) {
	be := newTestBackend()
	a, b := addr(1), addr(2)
	be.code[b] = prog(CALLVALUE, retTop())
	be.code[a] = prog(32, PUSH0, PUSH0, PUSH0, 1, b, GAS, CALL, STOP)
	if _, err := Call(be, Address{}, a, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("CALL with value: want ErrUnsupported, got %v", err)
	}
}

// ---- memory safety ----

func TestMemory_HugeOffsetFaultsNotWraps(t *testing.T) {
	// MLOAD at 2^64 + 0 used to truncate to offset 0 (wrong result).
	off := new(uint256.Int).Lsh(uint256.NewInt(1), 64)
	if _, err := runRaw(&nullBackend{}, prog(*off, MLOAD, retTop())); err == nil {
		t.Fatal("MLOAD past u32 must fault")
	}
	// A memory expansion past the local cap is not served locally.
	wantUnsupported(t, &nullBackend{}, prog(1, 0x7fffffff, MSTORE8, STOP))
}

func TestReturndatacopy_OutOfBoundsFaults(t *testing.T) {
	// No call made: return data is empty; copying 1 byte must fault.
	if _, err := runRaw(&nullBackend{}, prog(1, PUSH0, PUSH0, RETURNDATACOPY, STOP)); err == nil {
		t.Fatal("RETURNDATACOPY past return data must fault (EIP-211)")
	}
}

func TestCalldatacopy_HugeDataOffsetZeroFills(t *testing.T) {
	// dataOffset > 2^64 must read as past-the-end (zeros), not wrap.
	off := new(uint256.Int).Lsh(uint256.NewInt(1), 64)
	be := newTestBackend()
	a := addr(1)
	be.code[a] = prog(32, *off, PUSH0, CALLDATACOPY, 32, PUSH0, RETURN)
	res, err := Call(be, Address{}, a, bytes.Repeat([]byte{0xff}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Return, make([]byte, 32)) {
		t.Fatalf("got %x, want zeros", res.Return)
	}
}

// ---- precompiles ----

func TestPrecompiles(t *testing.T) {
	ip := &interpreter{}
	abc := []byte("abc")
	cases := []struct {
		a    byte
		in   []byte
		want string
	}{
		{0x02, abc, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{0x03, abc, "0000000000000000000000008eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
		{0x04, abc, "616263"},
	}
	for _, c := range cases {
		out, ok, err := ip.callPrecompile(Address{19: c.a}, c.in)
		if err != nil || !ok {
			t.Fatalf("precompile %d: ok=%v err=%v", c.a, ok, err)
		}
		if hex.EncodeToString(out) != c.want {
			t.Errorf("precompile %d: got %x want %s", c.a, out, c.want)
		}
	}
	// ecrecover (go-ethereum precompile test vector).
	in, _ := hex.DecodeString("38d18acb67d25c8bb9942764b62f18e17054f66a817bd4295423adf9ed98873e000000000000000000000000000000000000000000000000000000000000001b38d18acb67d25c8bb9942764b62f18e17054f66a817bd4295423adf9ed98873e789d1dd423d25f0772d2748d60f7e4b81bb14d086eba8e8e8efb6dcff8a4ae02")
	out, ok, err := ip.callPrecompile(Address{19: 0x01}, in)
	if err != nil || !ok {
		t.Fatalf("ecrecover: ok=%v err=%v", ok, err)
	}
	if got := hex.EncodeToString(out); got != "000000000000000000000000ceaccac640adf55b2028469bd36ba501f28b699d" {
		t.Errorf("ecrecover: got %s", got)
	}
	// Bad v -> empty output, still success (FEVM ec_recover never fails).
	bad := append([]byte(nil), in...)
	bad[63] = 29
	out, ok, err = ip.callPrecompile(Address{19: 0x01}, bad)
	if err != nil || !ok || len(out) != 0 {
		t.Errorf("ecrecover bad v: out=%x ok=%v err=%v", out, ok, err)
	}
	// Not implemented locally -> fault (never the empty success an
	// absent actor would give).
	for _, a := range []Address{{19: 0x05}, {19: 0x08}, {0: 0xfe, 19: 0x01}, p256VerifyAddress} {
		if _, _, err := ip.callPrecompile(a, nil); !errors.Is(err, ErrUnsupported) {
			t.Errorf("precompile %x: want ErrUnsupported, got %v", a, err)
		}
	}
}

func TestPrecompile_IdentityViaStaticcall(t *testing.T) {
	be := newTestBackend()
	a := addr(1)
	// mem[0:32] = word; STATICCALL(identity, in=0..32, out=32..64); return mem[32:64]
	w := new(uint256.Int).SetBytes(bytes.Repeat([]byte{0x42}, 32))
	be.code[a] = prog(*w, PUSH0, MSTORE, 32, 32, 32, PUSH0, Address{19: 4}, GAS, STATICCALL, POP, 32, 32, RETURN)
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Return, bytes.Repeat([]byte{0x42}, 32)) {
		t.Fatalf("got %x", res.Return)
	}
}

// ---- FEVM EXTCODE* / CALL semantics on non-EVM actors ----

func TestExtcode_FEVMKinds(t *testing.T) {
	tb := newTestBackend()
	evmA, acct, native, missing := addr(1), addr(2), addr(3), addr(4)
	tb.code[evmA] = []byte{0x60, 0x00}
	be := &kindBackend{testBackend: tb, kinds: map[Address]ActorKind{
		acct: ActorAccount, native: ActorNative, missing: ActorNotFound,
	}}
	size := func(a Address) uint64 {
		v := word(t, mustRun(t, be, prog(a, EXTCODESIZE, retTop())))
		return v.Uint64()
	}
	hash := func(a Address) []byte { return mustRun(t, be, prog(a, EXTCODEHASH, retTop())).Return }

	if s := size(evmA); s != 2 {
		t.Errorf("EXTCODESIZE(evm) = %d", s)
	}
	if s := size(native); s != 1 {
		t.Errorf("EXTCODESIZE(native) = %d, want 1", s)
	}
	if s := size(acct); s != 0 {
		t.Errorf("EXTCODESIZE(account) = %d", s)
	}
	if s := size(Address{19: 1}); s != 0 {
		t.Errorf("EXTCODESIZE(precompile) = %d", s)
	}
	eh := keccak256(nil)
	nh := keccak256([]byte{0xfe})
	ch := keccak256([]byte{0x60, 0x00})
	if !bytes.Equal(hash(acct), eh[:]) {
		t.Errorf("EXTCODEHASH(account) = %x, want keccak(empty)", hash(acct))
	}
	if !bytes.Equal(hash(native), nh[:]) {
		t.Errorf("EXTCODEHASH(native) = %x", hash(native))
	}
	if !bytes.Equal(hash(missing), make([]byte, 32)) {
		t.Errorf("EXTCODEHASH(missing) = %x, want 0", hash(missing))
	}
	if !bytes.Equal(hash(Address{19: 2}), eh[:]) {
		t.Errorf("EXTCODEHASH(precompile) = %x, want keccak(empty)", hash(Address{19: 2}))
	}
	if !bytes.Equal(hash(evmA), ch[:]) {
		t.Errorf("EXTCODEHASH(evm) = %x", hash(evmA))
	}
	// EXTCODECOPY of a native actor is the single byte 0xfe.
	res := mustRun(t, be, prog(32, PUSH0, PUSH0, native, EXTCODECOPY, 32, PUSH0, RETURN))
	if res.Return[0] != 0xfe || !bytes.Equal(res.Return[1:], make([]byte, 31)) {
		t.Errorf("EXTCODECOPY(native) = %x", res.Return)
	}
	// CALL into a native actor is not modeled; DELEGATECALL fails (0).
	wantUnsupported(t, be, prog(callTarget(STATICCALL, native), STOP))
	v := word(t, mustRun(t, be, prog(PUSH0, PUSH0, PUSH0, PUSH0, native, GAS, DELEGATECALL, retTop())))
	if !v.IsZero() {
		t.Errorf("DELEGATECALL(native) pushed %d, want 0", v.Uint64())
	}
	// CALL into an account succeeds with empty return data.
	v = word(t, mustRun(t, be, prog(callTarget(STATICCALL, acct), POP, RETURNDATASIZE, 1, ADD, retTop())))
	if v.Uint64() != 1 {
		t.Errorf("CALL(account): returndatasize+1 = %d, want 1", v.Uint64())
	}
}

// ---- regression: PDPVerifier VERSION() on calibration (MCOPY) ----

type pdpFixture struct {
	Proxy          string `json:"proxy"`
	Implementation string `json:"implementation"`
	ImplSlot       string `json:"implSlot"`
	ProxyCode      string `json:"proxyCode"`
	ImplCode       string `json:"implCode"`
	Call           struct {
		Data string `json:"data"`
	} `json:"call"`
	GlifResult string `json:"glifResult"`
}

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRegression_PDPVerifierVersion_MCOPY replays stock Curio-PDP's
// recurring eth_call PDPVerifier.VERSION() (0xffa1ad74) against the
// calibration proxy + implementation bytecode recorded from Glif. Before
// MCOPY support this faulted with "unsupported opcode 0x5e at pc 11975".
func TestRegression_PDPVerifierVersion_MCOPY(t *testing.T) {
	raw, err := os.ReadFile("testdata/pdpverifier_version_calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx pdpFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	proxy := BytesToAddress(hexBytes(t, fx.Proxy))
	impl := BytesToAddress(hexBytes(t, fx.Implementation))
	implCode := hexBytes(t, fx.ImplCode)

	// The recorded bytecode really exercises MCOPY.
	if implCode[11975] != byte(MCOPY) {
		t.Fatalf("fixture: impl pc 11975 = 0x%02x, want MCOPY", implCode[11975])
	}

	be := newTestBackend()
	be.code[proxy] = hexBytes(t, fx.ProxyCode)
	be.code[impl] = implCode
	var slot, implWord uint256.Int
	slot.SetBytes(hexBytes(t, fx.ImplSlot))
	implWord.SetBytes(impl[:])
	be.storage[proxy] = map[uint256.Int]uint256.Int{slot: implWord}

	res, err := Call(be, Address{}, proxy, hexBytes(t, fx.Call.Data))
	if err != nil {
		t.Fatalf("local exec fault: %v", err)
	}
	if res.Reverted {
		t.Fatalf("reverted: %x", res.Return)
	}
	if got, want := "0x"+hex.EncodeToString(res.Return), fx.GlifResult; got != want {
		t.Fatalf("VERSION() mismatch\n got %s\nwant %s (Glif)", got, want)
	}
	// ABI string "3.5.0".
	if !bytes.Contains(res.Return, []byte("3.5.0")) {
		t.Errorf("expected VERSION string 3.5.0 in %x", res.Return)
	}
}

// ---- coverage against the builtin-actors EVM jump table ----

// fevmV19Opcodes is the instruction table of the Filecoin EVM actor,
// builtin-actors v19.0.1 (NV29) actors/evm/src/interpreter/execution.rs
// def_opcodes!. v16/v17 (NV24..NV27) are identical minus CLZ (0x1e), which
// v18 (NV28) added. Every byte not listed is UNDEFINED (abort
// EVM_CONTRACT_UNDEFINED_INSTRUCTION), e.g. BLOBHASH 0x49, BLOBBASEFEE
// 0x4a, CALLCODE 0xf2.
var fevmV19Opcodes = func() map[byte]bool {
	m := map[byte]bool{}
	add := func(lo, hi byte) {
		for b := int(lo); b <= int(hi); b++ {
			m[byte(b)] = true
		}
	}
	add(0x00, 0x0b) // STOP..SIGNEXTEND
	add(0x10, 0x1e) // LT..SAR, CLZ
	add(0x20, 0x20) // KECCAK256
	add(0x30, 0x3f) // ADDRESS..EXTCODEHASH
	add(0x40, 0x48) // BLOCKHASH..BASEFEE
	add(0x50, 0x5f) // POP..PUSH0 (incl. TLOAD, TSTORE, MCOPY)
	add(0x60, 0x7f) // PUSH1..PUSH32
	add(0x80, 0x8f) // DUP1..DUP16
	add(0x90, 0x9f) // SWAP1..SWAP16
	add(0xa0, 0xa4) // LOG0..LOG4
	for _, b := range []byte{0xf0, 0xf1, 0xf3, 0xf4, 0xf5, 0xfa, 0xfd, 0xfe, 0xff} {
		m[b] = true // CREATE CALL RETURN DELEGATECALL CREATE2 STATICCALL REVERT INVALID SELFDESTRUCT
	}
	return m
}()

// TestOpcodeCoverage_MatchesFEVMTable executes every byte 0x00..0xff with
// a pre-filled zero stack. Every opcode the FEVM defines must be
// implemented (it may still decline with a specific "not modeled"
// ErrUnsupported, e.g. CREATE or a value-transferring CALL, but must never
// hit the generic unknown-opcode path); every opcode the FEVM leaves
// undefined must fault with ErrUnsupported, never produce a result.
func TestOpcodeCoverage_MatchesFEVMTable(t *testing.T) {
	tb := newTestBackend()
	be := &envBackend{testBackend: tb, nv: 29, digests: map[uint64][]byte{}}
	notModeled := map[byte]bool{0xf0: true, 0xf5: true, 0xff: true} // CREATE, CREATE2, SELFDESTRUCT
	for b := 0; b < 256; b++ {
		op := byte(b)
		code := []byte{op, byte(STOP)}
		cs := &callState{b: be, ov: newOverlay()}
		ip := newInterpreter(cs, frame{}, code)
		for i := 0; i < 20; i++ {
			_ = ip.stack.push(uint256.Int{})
		}
		_, err := ip.run()
		if fevmV19Opcodes[op] {
			if err != nil && strings.Contains(err.Error(), "unsupported opcode") {
				t.Errorf("0x%02x (%s): defined by the FEVM but not implemented: %v", op, OpCode(op), err)
			}
			if err != nil && strings.Contains(err.Error(), "undefined in the FEVM") {
				t.Errorf("0x%02x (%s): defined by the FEVM but reported undefined", op, OpCode(op))
			}
			if notModeled[op] && !errors.Is(err, ErrUnsupported) {
				t.Errorf("0x%02x (%s): expected a not-modeled fault, got %v", op, OpCode(op), err)
			}
			continue
		}
		if err == nil {
			t.Errorf("0x%02x: undefined in the FEVM but produced a result", op)
		} else if !errors.Is(err, ErrUnsupported) {
			t.Errorf("0x%02x: undefined opcode fault should wrap ErrUnsupported, got %v", op, err)
		}
	}
}
