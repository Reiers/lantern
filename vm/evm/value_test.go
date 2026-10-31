package evm

import (
	"bytes"
	"errors"
	"testing"

	"github.com/holiman/uint256"
)

// revertWith returns code that reverts with Error(msg) (ABI-encoded,
// msg <= 32 bytes).
func revertWith(msg string) []any {
	var sel uint256.Int
	sel.SetBytes([]byte{0x08, 0xc3, 0x79, 0xa0})
	sel.Lsh(&sel, 224)
	var m uint256.Int
	pad := make([]byte, 32)
	copy(pad, msg)
	m.SetBytes(pad)
	return []any{
		sel, PUSH0, MSTORE, // selector at 0..4
		0x20, 4, MSTORE, // offset
		len(msg), 36, MSTORE, // length
		m, 68, MSTORE, // data
		100, PUSH0, REVERT,
	}
}

// feeContract is a payable fee check: CALLVALUE < fee reverts with
// "Incorrect fee amount"; otherwise it burns fee to the burnt funds actor,
// refunds the excess to msg.sender, and returns SELFBALANCE.
func feeContract(fee int, burn Address) []byte {
	// Layout: [check] JUMPI ok ; revert ; ok: JUMPDEST burn refund return
	revert := asm(revertWith("Incorrect fee amount")...)
	head := asm(fee, CALLVALUE, LT) // callvalue < fee
	// PUSH2 <revertDest> JUMPI ; fallthrough = ok path
	body := asm(
		// burn: CALL(gas, burn, fee, 0,0,0,0)
		PUSH0, PUSH0, PUSH0, PUSH0, fee, burn, GAS, CALL, POP,
		// refund: CALL(gas, caller, callvalue-fee, 0,0,0,0)
		PUSH0, PUSH0, PUSH0, PUSH0, fee, CALLVALUE, SUB, CALLER, GAS, CALL, POP,
		SELFBALANCE,
	)
	body = append(body, asm(retTop()...)...)
	revertDest := len(head) + 4 + len(body)
	code := append([]byte{}, head...)
	code = append(code, byte(PUSH1)+1, byte(revertDest>>8), byte(revertDest), byte(JUMPI))
	code = append(code, body...)
	code = append(code, byte(JUMPDEST))
	code = append(code, revert...)
	return code
}

func TestCallWithValue_PayableFee(t *testing.T) {
	sender, c, burn := addr(1), addr(2), Address{0: 0xff, 19: 99} // f099
	tb := newTestBackend()
	be := &kindBackend{testBackend: tb, kinds: map[Address]ActorKind{sender: ActorAccount, burn: ActorAccount}}
	tb.code[c] = feeContract(1000, burn)
	tb.balances[sender] = *uint256.NewInt(5000)
	tb.balances[c] = *uint256.NewInt(7)

	// Insufficient fee: definitive revert with the reason.
	res, err := CallWithValue(be, sender, c, nil, *uint256.NewInt(999))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reverted || !bytes.Contains(res.Return, []byte("Incorrect fee amount")) {
		t.Fatalf("want revert Incorrect fee amount, got %+v", res)
	}
	if len(res.Trace) != 1 || res.Trace[0].Addr != c || res.Trace[0].Exit != ExitContractReverted || res.ExitCode() != 33 {
		t.Fatalf("trace %+v", res.Trace)
	}

	// No value at all: CALLVALUE = 0 -> revert.
	if res, _ = CallWithValue(be, sender, c, nil, uint256.Int{}); !res.Reverted {
		t.Fatal("zero value should revert")
	}

	// Enough fee: burn + refund modeled; contract keeps its own balance.
	res, err = CallWithValue(be, sender, c, nil, *uint256.NewInt(1500))
	if err != nil {
		t.Fatal(err)
	}
	if got := word(t, res); got.Uint64() != 7 {
		t.Fatalf("SELFBALANCE after burn+refund = %d, want 7", got.Uint64())
	}

	// The sender can't cover the value: insufficient funds, nothing runs.
	_, err = CallWithValue(be, sender, c, nil, *uint256.NewInt(6000))
	var ife *InsufficientFundsError
	if !errors.As(err, &ife) || ife.Balance.Uint64() != 5000 || ife.Value.Uint64() != 6000 {
		t.Fatalf("want InsufficientFundsError, got %v", err)
	}
}

func TestCallWithValue_BalanceVisible(t *testing.T) {
	sender, c := addr(1), addr(2)
	tb := newTestBackend()
	tb.balances[sender] = *uint256.NewInt(100)
	tb.balances[c] = *uint256.NewInt(5)
	// Return BALANCE(sender) + SELFBALANCE*2^128.
	tb.code[c] = prog(SELFBALANCE, 128, SHL, sender, BALANCE, ADD, retTop())
	res, err := CallWithValue(tb, sender, c, nil, *uint256.NewInt(40))
	if err != nil {
		t.Fatal(err)
	}
	w := word(t, res)
	var lo, hi uint256.Int
	lo.And(&w, new(uint256.Int).SubUint64(new(uint256.Int).Lsh(uint256.NewInt(1), 128), 1))
	hi.Rsh(&w, 128)
	if lo.Uint64() != 60 || hi.Uint64() != 45 {
		t.Fatalf("sender %d (want 60), self %d (want 45)", lo.Uint64(), hi.Uint64())
	}
}

func TestCallWithValue_RevertedSubCallRollsBackValue(t *testing.T) {
	a, b := addr(1), addr(2)
	tb := newTestBackend()
	be := &kindBackend{testBackend: tb, kinds: map[Address]ActorKind{}}
	tb.balances[a] = *uint256.NewInt(50)
	tb.code[b] = prog(PUSH0, PUSH0, REVERT)
	// a: CALL b with value 30 (b reverts), then return SELFBALANCE.
	tb.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, 30, b, GAS, CALL, POP, SELFBALANCE, retTop())
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := word(t, res); got.Uint64() != 50 {
		t.Fatalf("SELFBALANCE after reverted value CALL = %d, want 50", got.Uint64())
	}
	// And a successful one moves it.
	tb.code[b] = prog(STOP)
	res, _ = Call(be, Address{}, a, nil)
	if got := word(t, res); got.Uint64() != 20 {
		t.Fatalf("SELFBALANCE after value CALL = %d, want 20", got.Uint64())
	}
}

func TestCallWithValue_UnmodeledTargetsFault(t *testing.T) {
	a, native := addr(1), addr(9)
	tb := newTestBackend()
	be := &kindBackend{testBackend: tb, kinds: map[Address]ActorKind{native: ActorNative}}
	tb.balances[a] = *uint256.NewInt(50)
	unknownID := Address{0: 0xff, 19: 0x42}
	for name, to := range map[string]Address{
		"native actor":        native,
		"precompile (pay)":    {0: 0xfe, 19: 0x03},
		"unknown ID":          unknownID,
		"ethereum precompile": {19: 0x02},
	} {
		tb.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, 1, to, GAS, CALL, STOP)
		if _, err := Call(be, Address{}, a, nil); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: value CALL want ErrUnsupported, got %v", name, err)
		}
	}
	// Value into a new f4 address (placeholder creation) is modeled.
	tb.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, 1, addr(77), GAS, CALL, retTop())
	res, err := Call(be, Address{}, a, nil)
	if err != nil {
		t.Fatalf("value to new f4 address: %v", err)
	}
	if w := word(t, res); w.Uint64() != 1 {
		t.Fatalf("value to new f4 address: CALL returned %d", w.Uint64())
	}
}

func TestCallWithValue_StaticValueStillReadOnlyFault(t *testing.T) {
	a, b := addr(1), addr(2)
	tb := newTestBackend()
	tb.balances[b] = *uint256.NewInt(10)
	tb.code[b] = prog(PUSH0, PUSH0, PUSH0, PUSH0, 1, addr(3), GAS, CALL, STOP)
	tb.code[a] = prog(PUSH0, PUSH0, PUSH0, PUSH0, b, GAS, STATICCALL, retTop())
	res, err := Call(tb, Address{}, a, nil)
	if err == nil {
		if w := word(t, res); w.Uint64() == 1 {
			t.Fatal("value CALL inside STATICCALL must not succeed")
		}
	}
}

// TestRevertTrace_DelegateProxy: an ERC1967-style proxy DELEGATECALLs the
// implementation, which reverts; the proxy bubbles it. The backtrace is
// outermost first, like Lotus: proxy frame, then the delegate frame (run
// in the proxy's context), each with the pc of its REVERT.
func TestRevertTrace_DelegateProxy(t *testing.T) {
	proxy, impl := addr(1), addr(2)
	tb := newTestBackend()
	tb.code[impl] = prog(revertWith("nope"))
	// proxy: DELEGATECALL impl, copy returndata, REVERT it.
	tb.code[proxy] = prog(PUSH0, PUSH0, PUSH0, PUSH0, impl, GAS, DELEGATECALL, POP,
		RETURNDATASIZE, PUSH0, PUSH0, RETURNDATACOPY, RETURNDATASIZE, PUSH0, REVERT)
	res, err := Call(tb, Address{}, proxy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reverted || len(res.Trace) != 2 {
		t.Fatalf("want reverted with 2 frames, got %+v", res)
	}
	f0, f1 := res.Trace[0], res.Trace[1]
	if f0.Addr != proxy || f0.Delegate || f1.Addr != proxy || !f1.Delegate {
		t.Fatalf("frames %+v", res.Trace)
	}
	if int(f0.PC) != len(tb.code[proxy])-1 || int(f1.PC) != len(tb.code[impl])-1 {
		t.Fatalf("pcs %d/%d, want %d/%d", f0.PC, f1.PC, len(tb.code[proxy])-1, len(tb.code[impl])-1)
	}
	// A caught sub-call revert followed by a successful call does not
	// leak into a later revert's trace.
	tb.code[proxy] = prog(PUSH0, PUSH0, PUSH0, PUSH0, impl, GAS, DELEGATECALL, POP,
		PUSH0, PUSH0, PUSH0, PUSH0, addr(5), GAS, STATICCALL, POP, PUSH0, PUSH0, REVERT)
	res, _ = Call(tb, Address{}, proxy, nil)
	if len(res.Trace) != 1 {
		t.Fatalf("trace after recovered sub-call: %+v", res.Trace)
	}
}
