// eth_estimateGas that executes the call (stock Curio relies on it).
//
// Lotus' EthEstimateGas runs the message (GasEstimateMessageGas, then
// applyMessage for the revert reason) and fails with JSON-RPC code 3
// "message execution failed (exit=[33], revert reason=[...], vm error=[...])"
// when it would revert. Curio (go-ethereum ethclient) treats that error as
// "don't broadcast". Lantern used to hand back a heuristic gas limit without
// executing anything, so Curio broadcast transactions that then reverted on
// chain, each burning gas at the heuristic limit.
//
// Here the call is executed with the local EVM against verified state at
// the live head (the same state eth_call "latest" uses), including from /
// value / data:
//
//   - revert (or insufficient funds for the value): Lotus-shaped code-3
//     error, data = revert bytes;
//   - success: a gas estimate (the bridge's exact one when a bridge is
//     configured, else the conservative heuristic; Lantern does not meter
//     FEVM gas);
//   - not executable locally (native actor target, unsupported precompile
//     or opcode, value to a native actor, missing state): bridge when
//     configured, else the heuristic plus a rate-limited WARN that the
//     estimate was not executed.
package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/holiman/uint256"

	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/vm/evm"
)

// eExecutionReverted is Lotus' api.EExecutionReverted (jsonrpc
// FirstUserCode + 1 = 3), the code go-ethereum clients read as a revert.
const eExecutionReverted = 3

// execRevertedError is Lotus' api.ErrExecutionReverted: code 3, the
// "message execution failed (...)" message, and the revert bytes as a hex
// string in error.data.
type execRevertedError struct {
	Message string
	Data    string
}

func (e *execRevertedError) Error() string { return e.Message }

// ToJSONRPCError implements jsonrpc.RPCErrorCodec.
func (e *execRevertedError) ToJSONRPCError() (jsonrpc.JSONRPCError, error) {
	return jsonrpc.JSONRPCError{Code: eExecutionReverted, Message: e.Message, Data: e.Data}, nil
}

// FromJSONRPCError implements jsonrpc.RPCErrorCodec.
func (e *execRevertedError) FromJSONRPCError(jerr jsonrpc.JSONRPCError) error {
	if jerr.Code != eExecutionReverted || jerr.Message == "" {
		return errors.New("invalid execution reverted error")
	}
	e.Message = jerr.Message
	if s, ok := jerr.Data.(string); ok {
		e.Data = s
	}
	return nil
}

var _ jsonrpc.RPCErrorCodec = (*execRevertedError)(nil)

// ToJSONRPCError makes a local eth_call revert go out the way Lotus sends
// it too (code 3 + revert data) instead of a generic code-1 error.
func (e *revertError) ToJSONRPCError() (jsonrpc.JSONRPCError, error) {
	return jsonrpc.JSONRPCError{
		Code: eExecutionReverted,
		Message: fmt.Sprintf("message execution failed (exit=[%s], revert reason=[%s], vm error=[contract reverted (RetCode=%d)])",
			exitcode.ExitCode(evm.ExitContractReverted), parseEthRevert(e.data), evm.ExitContractReverted),
		Data: "0x" + hex.EncodeToString(e.data),
	}, nil
}

// FromJSONRPCError implements jsonrpc.RPCErrorCodec.
func (e *revertError) FromJSONRPCError(jerr jsonrpc.JSONRPCError) error {
	s, _ := jerr.Data.(string)
	b, err := decodeHexData(s)
	if err != nil {
		return err
	}
	e.data = b
	return nil
}

var _ jsonrpc.RPCErrorCodec = (*revertError)(nil)

// Lotus ethtypes selectors / panic codes (chain/types/ethtypes).
const (
	errorFunctionSelector = "\x08\xc3\x79\xa0" // Error(string)
	panicFunctionSelector = "\x4e\x48\x7b\x71" // Panic(uint256)
)

var panicErrorCodes = map[uint64]string{
	0x00: "Panic()",
	0x01: "Assert()",
	0x11: "ArithmeticOverflow()",
	0x12: "DivideByZero()",
	0x21: "InvalidEnumVariant()",
	0x22: "InvalidStorageArray()",
	0x31: "PopEmptyArray()",
	0x32: "ArrayIndexOutOfBounds()",
	0x41: "OutOfMemory()",
	0x51: "CalledUninitializedFunction()",
}

// word64 is Lotus' EthUint64FromBytes: a 32-byte word that must fit in 64
// bits.
func word64(b []byte) (uint64, bool) {
	if len(b) != 32 {
		return 0, false
	}
	for _, x := range b[:24] {
		if x != 0 {
			return 0, false
		}
	}
	var v uint64
	for _, x := range b[24:] {
		v = v<<8 | uint64(x)
	}
	return v, true
}

// parseEthRevert is Lotus' revert-reason decoding (ethtypes.ParseEthRevert
// plus the "none" for empty data from NewErrExecutionRevertedFromResult):
// Error(string) -> "Error(msg)", Panic(uint256) -> the Solidity panic name,
// anything else (custom errors) -> the raw bytes as 0x-hex.
func parseEthRevert(ret []byte) string {
	if len(ret) == 0 {
		return "none"
	}
	if len(ret) < 4+32 {
		return "0x" + hex.EncodeToString(ret)
	}
	switch string(ret[:4]) {
	case panicFunctionSelector:
		w := ret[4 : 4+32]
		code, ok := word64(w)
		if !ok {
			return fmt.Sprintf("Panic(0x%x)", new(big.Int).SetBytes(w))
		}
		if s, ok := panicErrorCodes[code]; ok {
			return s
		}
		return fmt.Sprintf("Panic(0x%x)", code)
	case errorFunctionSelector:
		body := ret[4:]
		n := uint64(len(body))
		offset, ok := word64(body[:32])
		if !ok || n < offset || n-offset < 32 {
			break
		}
		start := offset + 32
		length, ok := word64(body[offset : offset+32])
		if !ok || n-start < length {
			break
		}
		return fmt.Sprintf("Error(%s)", body[start:start+length])
	}
	return "0x" + hex.EncodeToString(ret)
}

// FVM method numbers in a backtrace: an EVM frame entered by a message or
// CALL runs InvokeContract (FRC-42 hash 3844450837); a DELEGATECALL frame
// is InvokeContractDelegate (6) sent to self.
var (
	methodInvokeContract         = uint64(builtin.MethodsEVM.InvokeContract)
	methodInvokeContractDelegate = uint64(builtin.MethodsEVM.InvokeContractDelegate)
)

// revertedToError renders a reverted local execution as Lotus does:
// api.NewErrExecutionReverted(exit, backtrace, ParseEthRevert(ret), ret).
// actorName maps a frame address to the actor id string the FVM prints
// ("f0176630"); it may return "" to fall back to the eth address.
func revertedToError(res *evm.Result, actorName func(evm.Address) string) *execRevertedError {
	exit := res.ExitCode()
	var bt strings.Builder
	bt.WriteString("message failed with backtrace:\n")
	for i, f := range res.Trace {
		name := ""
		if actorName != nil {
			name = actorName(f.Addr)
		}
		if name == "" {
			name = "0x" + hex.EncodeToString(f.Addr[:])
		}
		method := methodInvokeContract
		if f.Delegate {
			method = methodInvokeContractDelegate
		}
		what := "contract reverted"
		if f.Exit == evm.ExitInvalidInstruction {
			what = "invalid instruction"
		}
		fmt.Fprintf(&bt, "%02d: %s (method %d) -- %s at %d (%d)\n", i, name, method, what, f.PC, f.Exit)
	}
	fmt.Fprintf(&bt, " (RetCode=%d)", exit)
	return &execRevertedError{
		Message: fmt.Sprintf("message execution failed (exit=[%s], revert reason=[%s], vm error=[%s])",
			exitcode.ExitCode(exit), parseEthRevert(res.Return), bt.String()),
		Data: "0x" + hex.EncodeToString(res.Return),
	}
}

// insufficientFundsToError renders a value the sender can't cover the way
// Lotus reports it (the send fails before any code runs, exit 6, no data).
func insufficientFundsToError(e *evm.InsufficientFundsError) *execRevertedError {
	return &execRevertedError{
		Message: fmt.Sprintf("message execution failed (exit=[%s], vm error=[message failed with backtrace:\n--> caused by: send::send -- sender does not have funds to transfer (balance %s, transfer %s) (5: insufficient funds)\n (RetCode=%d)])",
			exitcode.SysErrInsufficientFunds, filUnitless(e.Balance.ToBig()), filUnitless(e.Value.ToBig()), int(exitcode.SysErrInsufficientFunds)),
		Data: "0x",
	}
}

// filUnitless formats attoFIL as Lotus' types.FIL.Unitless().
func filUnitless(atto *big.Int) string {
	r := new(big.Rat).SetFrac(atto, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	if r.Sign() == 0 {
		return "0"
	}
	return strings.TrimRight(strings.TrimRight(r.FloatString(18), "0"), ".")
}

// estimateBackend is what local estimation needs from the state backend.
type estimateBackend interface {
	evm.Backend
	evm.KindBackend
}

// estimateCall is a decoded eth_estimateGas call object.
type estimateCall struct {
	from  evm.Address
	to    evm.Address
	input []byte
	value uint256.Int
}

// estimateOutcome is the result of trying to execute an estimate locally.
// executed=false means "no definitive local answer" (reason says why);
// executed=true with err=nil is a successful execution, with err != nil a
// definitive failure (*execRevertedError) to return to the caller.
type estimateOutcome struct {
	executed bool
	err      error
	reason   string
}

// decodeEstimateCall validates and decodes the call object. ok=false (with
// a reason) when it can't be executed locally at all.
func decodeEstimateCall(call ethCallObject) (estimateCall, string, bool) {
	var out estimateCall
	toRaw, err := decodeEthAddr(call.To)
	if err != nil {
		return out, "malformed or missing 'to' (contract creation / bad address)", false
	}
	out.to = evm.BytesToAddress(toRaw)
	dataHex := call.Data
	if dataHex == "" {
		dataHex = call.Input
	}
	if out.input, err = decodeHexData(dataHex); err != nil {
		return out, "malformed call data hex", false
	}
	if call.From != "" {
		fb, err := decodeEthAddr(call.From)
		if err != nil {
			return out, "malformed 'from'", false
		}
		out.from = evm.BytesToAddress(fb)
	}
	if v := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(call.Value), "0x"), "0X"); v != "" {
		n, ok := new(big.Int).SetString(v, 16)
		if !ok || n.Sign() < 0 || n.BitLen() > 256 {
			return out, "malformed 'value'", false
		}
		out.value.SetFromBig(n)
		if !out.value.IsZero() && call.From == "" {
			return out, "value without 'from'", false
		}
	}
	return out, "", true
}

// runEstimate executes c on be and classifies the outcome. actorName is
// used to render backtrace frames (may be nil).
func runEstimate(be estimateBackend, c estimateCall, actorName func(evm.Address) string) estimateOutcome {
	kind, err := be.ActorKind(c.to)
	if err != nil {
		return estimateOutcome{reason: "classify target: " + err.Error()}
	}
	switch kind {
	case evm.ActorEVM:
		res, err := evm.CallWithValue(be, c.from, c.to, c.input, c.value)
		if err != nil {
			var ife *evm.InsufficientFundsError
			if errors.As(err, &ife) {
				return estimateOutcome{executed: true, err: insufficientFundsToError(ife)}
			}
			return estimateOutcome{reason: "local exec fault: " + err.Error()}
		}
		if res.Reverted {
			return estimateOutcome{executed: true, err: revertedToError(res, actorName)}
		}
		return estimateOutcome{executed: true}
	case evm.ActorAccount, evm.ActorNotFound:
		// A send to an EOA / placeholder (or a new f4 address, which
		// becomes a placeholder): succeeds iff the sender covers the value.
		// An unknown masked-ID address would fail on chain in a way we
		// don't reproduce: not executed.
		if kind == evm.ActorNotFound && isMaskedID(c.to) {
			return estimateOutcome{reason: "target is an unknown ID address"}
		}
		if !c.value.IsZero() {
			bal, err := be.GetBalance(c.from)
			if err != nil {
				return estimateOutcome{reason: "sender balance: " + err.Error()}
			}
			if bal.Lt(&c.value) {
				return estimateOutcome{executed: true, err: insufficientFundsToError(&evm.InsufficientFundsError{Balance: bal, Value: c.value})}
			}
		}
		return estimateOutcome{executed: true}
	default:
		return estimateOutcome{reason: "target is a native (non-EVM) actor"}
	}
}

func isMaskedID(a evm.Address) bool {
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

// estimateExecHook replaces execForEstimate in tests.
var estimateExecHook func(ctx context.Context, c *ChainAPI, call ethCallObject) estimateOutcome

// execForEstimate executes call locally for eth_estimateGas against the
// live head state (head.ParentState(), the state eth_call "latest" runs
// on; Lantern does not execute tipsets, so this is one epoch older than
// the post-head state Lotus estimates on, and pending mpool messages of
// the sender are not applied).
func (c *ChainAPI) execForEstimate(ctx context.Context, call ethCallObject) estimateOutcome {
	if estimateExecHook != nil {
		return estimateExecHook(ctx, c, call)
	}
	if c.LocalFEVMDisabled {
		return estimateOutcome{reason: "local FEVM disabled"}
	}
	if c.Accessor == nil || c.BlockGetter == nil {
		return estimateOutcome{reason: "local state not wired (no accessor/block getter)"}
	}
	ec, reason, ok := decodeEstimateCall(call)
	if !ok {
		return estimateOutcome{reason: reason}
	}

	retries := c.LocalFEVMFetchRetries
	total := c.LocalFEVMFetchTimeout
	if retries == 0 && total == 0 {
		retries = 2
		total = 8 * time.Second
	}
	bg := newRetryingBlockGetter(c.BlockGetter, retries, total)

	acc := c.Accessor
	blockNo := uint64(0)
	tstamp := uint64(0)
	if c.Trusted != nil {
		blockNo = uint64(c.Trusted.Epoch)
	}
	var execTS *ltypes.TipSet
	if c.HeaderStore != nil {
		if head := c.HeaderStore.Head(); head != nil && head.ParentState().Defined() {
			parent, err := c.HeaderStore.GetTipSet(head.Parents())
			if err != nil || parent == nil {
				return estimateOutcome{reason: fmt.Sprintf("execution tipset (head %d parent) not in header store", head.Height())}
			}
			acc = accessor.New(&trustedroot.TrustedRoot{Epoch: head.Height(), StateRoot: head.ParentState()}, bg)
			execTS = parent
			blockNo = uint64(parent.Height())
			tstamp = parent.MinTimestamp()
		}
	}
	be := &evmBackend{
		ctx:     ctx,
		c:       c,
		acc:     acc,
		reg:     acc.Registry(),
		chainID: chainIDForNetwork(c.NetworkName),
		blockNo: blockNo,
		tstamp:  tstamp,
		bg:      bg,
		execTS:  execTS,
	}
	actorName := func(a evm.Address) string {
		fa, err := ethAddrToFilecoin(a.Bytes())
		if err != nil {
			return ""
		}
		id, _, err := acc.LookupID(ctx, fa)
		if err != nil {
			return ""
		}
		n, err := address.IDFromAddress(id)
		if err != nil {
			return ""
		}
		// The FVM prints mainnet-prefixed ids on every network.
		return fmt.Sprintf("f0%d", n)
	}
	return runEstimate(be, ec, actorName)
}

// estimateNotExecutedLog rate-limits the WARN for estimates returned
// without executing the call (bridge off).
var estimateNotExecutedLog = &ethMissLogger{
	last:       map[string]time.Time{},
	suppressed: map[string]int{},
	now:        time.Now,
	emit: func(_ string, to, selector, reason string, _ bool, suppressed int) {
		log.Warnw("eth_estimateGas: call was NOT executed locally and no bridge is configured; returning a heuristic estimate (a reverting transaction will not be caught)",
			"to", to, "selector", selector, "reason", reason, "suppressedSinceLast", suppressed)
	},
}

// EthEstimateGas estimates gas for a call. The call is executed first
// (see the file comment): a revert is returned as Lotus' code-3 error so
// clients like Curio do not broadcast a doomed transaction.
func (c *ChainAPI) EthEstimateGas(ctx context.Context, callObj any) (string, error) {
	// Devnet: the devnet Lotus is the source of truth (as for eth_call).
	if c.NetworkName == "devnet" && c.Bridge != nil {
		return c.bridgeEthEstimateGas(ctx, callObj)
	}
	var call ethCallObject
	if b, err := json.Marshal(callObj); err == nil {
		_ = json.Unmarshal(b, &call)
	}
	data := call.Data
	if data == "" {
		data = call.Input
	}

	out := c.execForEstimate(ctx, call)
	if out.executed {
		if out.err != nil {
			log.Debugw("eth_estimateGas: call reverts", "to", call.To, "selector", ethSelector(data))
			return "", out.err
		}
		// Executed and succeeded: an exact estimate from the bridge when
		// configured (Lantern does not meter FEVM gas), else the heuristic.
		if c.Bridge != nil {
			res, err := c.bridgeEthEstimateGas(ctx, callObj)
			if err == nil {
				return res, nil
			}
			var rerr *execRevertedError
			if errors.As(err, &rerr) {
				// The upstream (newer state) says it reverts: trust it.
				return "", rerr
			}
			log.Debugw("eth_estimateGas: bridge estimate failed, using heuristic", "err", err)
		}
		if res, ok, _ := c.localEthEstimateGas(ctx, callObj); ok {
			return res, nil
		}
		return "", errors.New("eth_estimateGas: call executed but no gas estimate is available")
	}

	// Not executable locally.
	if c.Bridge != nil {
		ethLocalMiss("eth_estimateGas", call.To, ethSelector(data), out.reason, true)
		return c.bridgeEthEstimateGas(ctx, callObj)
	}
	estimateNotExecutedLog.record("eth_estimateGas", call.To, ethSelector(data), out.reason, false)
	if res, ok, err := c.localEthEstimateGas(ctx, callObj); ok {
		return res, err
	}
	return "", errBridgeUnconfigured
}

// bridgeRevertError turns an upstream code-3 error relayed by the bridge
// into a code-3 error again (the bridge flattens upstream errors to text,
// so the revert data is not available: data is "0x").
func bridgeRevertError(err error) *execRevertedError {
	s := err.Error()
	i := strings.Index(s, "code=3 ")
	if i < 0 {
		return nil
	}
	return &execRevertedError{Message: s[i+len("code=3 "):], Data: "0x"}
}

// bridgeEthEstimateGas forwards eth_estimateGas to the VM bridge.
func (c *ChainAPI) bridgeEthEstimateGas(ctx context.Context, callObj any) (string, error) {
	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{callObj})
	if err != nil {
		return "", fmt.Errorf("marshal eth_estimateGas params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_estimateGas", params)
	if err != nil {
		if rerr := bridgeRevertError(err); rerr != nil {
			return "", rerr
		}
		return "", fmt.Errorf("bridge eth_estimateGas: %w", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode eth_estimateGas result: %w", err)
	}
	return out, nil
}
