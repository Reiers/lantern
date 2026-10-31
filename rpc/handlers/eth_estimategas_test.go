package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/holiman/uint256"
	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/vm/evm"
)

// estimateFixture is recorded state for eth_estimateGas regression tests:
// every code / storage slot / balance the local EVM touched while running
// the cases against Glif at a pinned calibration block, plus Glif's own
// eth_estimateGas answer for each case at that block.
type estimateFixture struct {
	Comment  string                       `json:"_comment"`
	Block    string                       `json:"block"`
	Code     map[string]string            `json:"code"`
	Storage  map[string]map[string]string `json:"storage"`
	Balances map[string]string            `json:"balances"`
	Kinds    map[string]string            `json:"kinds"`
	ActorIDs map[string]string            `json:"actorIDs"`
	Cases    []estimateFixtureCase        `json:"cases"`
}

type estimateFixtureCase struct {
	Name string        `json:"name"`
	Call ethCallObject `json:"call"`
	Glif struct {
		Result string `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error,omitempty"`
	} `json:"glif"`
}

const estimateFixturePath = "testdata/estimategas_calib.json"

func addrKey(a evm.Address) string { return "0x" + hex.EncodeToString(a[:]) }

func parseWord(t testing.TB, s string) uint256.Int {
	t.Helper()
	var w uint256.Int
	b, err := decodeHexData(s)
	if err != nil {
		t.Fatalf("bad word %q: %v", s, err)
	}
	w.SetBytes(b)
	return w
}

// fixtureBackend serves an estimateFixture as an estimateBackend. A read
// the fixture does not cover fails the test (the recording is incomplete).
type fixtureBackend struct {
	t  testing.TB
	fx *estimateFixture
}

func (b *fixtureBackend) GetCode(a evm.Address) ([]byte, error) {
	c, ok := b.fx.Code[addrKey(a)]
	if !ok {
		return nil, nil
	}
	return decodeHexData(c)
}

func (b *fixtureBackend) GetStorage(a evm.Address, k uint256.Int) (uint256.Int, error) {
	kb := k.Bytes32()
	v, ok := b.fx.Storage[addrKey(a)]["0x"+hex.EncodeToString(kb[:])]
	if !ok {
		return uint256.Int{}, fmt.Errorf("fixture: storage %s slot %x not recorded", addrKey(a), kb)
	}
	return parseWord(b.t, v), nil
}

func (b *fixtureBackend) GetBalance(a evm.Address) (uint256.Int, error) {
	v, ok := b.fx.Balances[addrKey(a)]
	if !ok {
		return uint256.Int{}, fmt.Errorf("fixture: balance %s not recorded", addrKey(a))
	}
	return parseWord(b.t, v), nil
}

func (b *fixtureBackend) ActorKind(a evm.Address) (evm.ActorKind, error) {
	switch b.fx.Kinds[addrKey(a)] {
	case "evm":
		return evm.ActorEVM, nil
	case "account":
		return evm.ActorAccount, nil
	case "native":
		return evm.ActorNative, nil
	case "":
		return 0, fmt.Errorf("fixture: kind of %s not recorded", addrKey(a))
	}
	return evm.ActorNotFound, nil
}

func (b *fixtureBackend) BlockNumber() uint64 { return 0 }
func (b *fixtureBackend) Timestamp() uint64   { return 0 }
func (b *fixtureBackend) ChainID() uint64     { return 314159 }

func (b *fixtureBackend) actorName(a evm.Address) string { return b.fx.ActorIDs[addrKey(a)] }

func loadEstimateFixture(t testing.TB) *estimateFixture {
	t.Helper()
	raw, err := os.ReadFile(estimateFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fx estimateFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return &fx
}

func fixtureCase(t testing.TB, fx *estimateFixture, name string) estimateFixtureCase {
	t.Helper()
	for _, c := range fx.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("fixture case %q missing", name)
	return estimateFixtureCase{}
}

func runFixtureCase(t *testing.T, fx *estimateFixture, name string) (estimateFixtureCase, estimateOutcome) {
	t.Helper()
	c := fixtureCase(t, fx, name)
	ec, reason, ok := decodeEstimateCall(c.Call)
	if !ok {
		t.Fatalf("%s: decode: %s", name, reason)
	}
	be := &fixtureBackend{t: t, fx: fx}
	return c, runEstimate(be, ec, be.actorName)
}

// TestEstimateGas_Fixture replays recorded calibration state through the
// local estimate path and requires Glif's verdict: the same code-3 error
// (message and revert data byte-for-byte, backtrace PCs included) for a
// reverting call, success where Glif returns an estimate.
func TestEstimateGas_Fixture(t *testing.T) {
	fx := loadEstimateFixture(t)
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, out := runFixtureCase(t, fx, c.Name)
			if !out.executed {
				t.Fatalf("not executed locally: %s", out.reason)
			}
			if c.Glif.Error == nil {
				if out.err != nil {
					t.Fatalf("Glif estimates %s, local says: %v", c.Glif.Result, out.err)
				}
				return
			}
			if out.err == nil {
				t.Fatalf("Glif: %s; local: success", c.Glif.Error.Message)
			}
			rerr, ok := out.err.(*execRevertedError)
			if !ok {
				t.Fatalf("error type %T, want *execRevertedError", out.err)
			}
			j, _ := rerr.ToJSONRPCError()
			if int(j.Code) != c.Glif.Error.Code {
				t.Errorf("code %d, Glif %d", j.Code, c.Glif.Error.Code)
			}
			if j.Message != c.Glif.Error.Message {
				t.Errorf("message mismatch\n local: %q\n  glif: %q", j.Message, c.Glif.Error.Message)
			}
			if j.Data != c.Glif.Error.Data {
				t.Errorf("data mismatch\n local: %v\n  glif: %s", j.Data, c.Glif.Error.Data)
			}
		})
	}
}

// TestEstimateGas_ProvePossessionNonProvider is the bug report: Lantern
// returned 0x55b13d93 where Lotus fails with "Only the storage provider can
// prove possession".
func TestEstimateGas_ProvePossessionNonProvider(t *testing.T) {
	fx := loadEstimateFixture(t)
	_, out := runFixtureCase(t, fx, "provePossession_nonProvider")
	if !out.executed || out.err == nil {
		t.Fatalf("want a definitive revert, got %+v", out)
	}
	if !strings.Contains(out.err.Error(), "revert reason=[Error(Only the storage provider can prove possession)]") {
		t.Fatalf("reason: %v", out.err)
	}
}

// TestEstimateGas_PayableFeeMismatch: a payable call whose value the
// contract rejects ("no fee on add to existing dataset" for value=1)
// executes with msg.value set and reverts, where the old EVM refused any
// value and the estimate was never executed.
func TestEstimateGas_PayableFeeMismatch(t *testing.T) {
	fx := loadEstimateFixture(t)
	_, out := runFixtureCase(t, fx, "addPieces_existing_value1")
	if !out.executed || out.err == nil {
		t.Fatalf("want a definitive revert, got %+v", out)
	}
	if !strings.Contains(out.err.Error(), "Error(no fee on add to existing dataset)") {
		t.Fatalf("reason: %v", out.err)
	}
	// Same call without value gets past the fee check.
	_, out = runFixtureCase(t, fx, "addPieces_existing_value0")
	if out.err == nil || !strings.Contains(out.err.Error(), "Error(Only the storage provider can add pieces)") {
		t.Fatalf("value=0: %+v", out)
	}
}

// ---- end-to-end handler flow (hooked executor) ----

type stubBridge struct {
	calls int
	res   string
	err   error
}

func (s *stubBridge) Provenance() string { return "stub" }

func (s *stubBridge) ComputeStateRoot(context.Context, cid.Cid, int64, []*ltypes.Message) (cid.Cid, []*ltypes.MessageReceipt, error) {
	return cid.Undef, nil, errors.New("stubBridge: not used")
}

func (s *stubBridge) RawJSONRPC(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(s.res)
}

func withEstimateHook(t *testing.T, out estimateOutcome) {
	t.Helper()
	estimateExecHook = func(context.Context, *ChainAPI, ethCallObject) estimateOutcome { return out }
	t.Cleanup(func() { estimateExecHook = nil })
}

var testCallObj = map[string]any{
	"from": "0x000000000000000000000000000000000000dEaD",
	"to":   "0x85e366Cf9DD2c0aE37E963d9556F5f4718d6417C",
	"data": "0xf58f952b",
}

func TestEthEstimateGas_RevertIsNeverSuccess(t *testing.T) {
	rerr := &execRevertedError{Message: "message execution failed (exit=[33], revert reason=[Error(x)], vm error=[...])", Data: "0x08c379a0"}
	withEstimateHook(t, estimateOutcome{executed: true, err: rerr})
	for _, withBridge := range []bool{false, true} {
		c := &ChainAPI{}
		sb := &stubBridge{res: "0x5208"}
		if withBridge {
			c.Bridge = sb
		}
		out, err := c.EthEstimateGas(context.Background(), testCallObj)
		if err == nil {
			t.Fatalf("bridge=%v: reverting call returned estimate %s", withBridge, out)
		}
		if err != error(rerr) {
			t.Fatalf("bridge=%v: error %v, want the revert error", withBridge, err)
		}
		if sb.calls != 0 {
			t.Fatalf("bridge consulted for a local revert")
		}
	}
}

func TestEthEstimateGas_SuccessReturnsEstimate(t *testing.T) {
	withEstimateHook(t, estimateOutcome{executed: true})
	// Bridge off: heuristic estimate.
	out, err := (&ChainAPI{}).EthEstimateGas(context.Background(), testCallObj)
	if err != nil || !strings.HasPrefix(out, "0x") || out == "0x0" {
		t.Fatalf("heuristic estimate: %q %v", out, err)
	}
	// Bridge on: the bridge's exact estimate.
	sb := &stubBridge{res: "0xec6d07"}
	out, err = (&ChainAPI{Bridge: sb}).EthEstimateGas(context.Background(), testCallObj)
	if err != nil || out != "0xec6d07" {
		t.Fatalf("bridge estimate: %q %v", out, err)
	}
	// Bridge relays an upstream revert: code 3, not the heuristic.
	sb = &stubBridge{err: fmt.Errorf("ForestBridge: upstream glif on eth_estimateGas: code=3 message execution failed (exit=[33], revert reason=[Error(y)], vm error=[z])")}
	_, err = (&ChainAPI{Bridge: sb}).EthEstimateGas(context.Background(), testCallObj)
	rerr, ok := err.(*execRevertedError)
	if !ok || !strings.HasPrefix(rerr.Message, "message execution failed") {
		t.Fatalf("bridge revert: %T %v", err, err)
	}
	// Bridge transport failure: fall back to the heuristic.
	sb = &stubBridge{err: fmt.Errorf("dial tcp: timeout")}
	out, err = (&ChainAPI{Bridge: sb}).EthEstimateGas(context.Background(), testCallObj)
	if err != nil || !strings.HasPrefix(out, "0x") {
		t.Fatalf("bridge down: %q %v", out, err)
	}
}

func TestEthEstimateGas_NotExecutedFallsBack(t *testing.T) {
	withEstimateHook(t, estimateOutcome{reason: "target is a native (non-EVM) actor"})
	sb := &stubBridge{res: "0x1234"}
	out, err := (&ChainAPI{Bridge: sb}).EthEstimateGas(context.Background(), testCallObj)
	if err != nil || out != "0x1234" || sb.calls != 1 {
		t.Fatalf("bridge fallback: %q %v calls=%d", out, err, sb.calls)
	}

	var mu sync.Mutex
	warned := 0
	orig := estimateNotExecutedLog.emit
	estimateNotExecutedLog.emit = func(string, string, string, string, bool, int) { mu.Lock(); warned++; mu.Unlock() }
	estimateNotExecutedLog.last = map[string]time.Time{}
	t.Cleanup(func() { estimateNotExecutedLog.emit = orig })
	for i := 0; i < 5; i++ {
		out, err = (&ChainAPI{}).EthEstimateGas(context.Background(), testCallObj)
		if err != nil || !strings.HasPrefix(out, "0x") {
			t.Fatalf("heuristic fallback: %q %v", out, err)
		}
	}
	if warned != 1 {
		t.Fatalf("WARN emitted %d times for 5 identical misses, want 1 (rate-limited)", warned)
	}
}

// TestExecRevertedError_WireShape checks what a client actually receives
// through go-jsonrpc: code 3, Lotus' message, data = revert hex string
// (go-ethereum's ethclient exposes it via rpc.DataError).
func TestExecRevertedError_WireShape(t *testing.T) {
	withEstimateHook(t, estimateOutcome{executed: true, err: &execRevertedError{Message: "message execution failed (exit=[33], revert reason=[Error(x)], vm error=[y])", Data: "0x08c379a0"}})
	rs := jsonrpc.NewServer()
	rs.Register("eth", &wireEth{c: &ChainAPI{}})
	hs := httptest.NewServer(rs)
	defer hs.Close()
	srv := hs.URL
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth.EthEstimateGas","params":[{"from":"0x000000000000000000000000000000000000dEaD","to":"0x85e366Cf9DD2c0aE37E963d9556F5f4718d6417C","data":"0xf58f952b"}]}`)
	resp, err := http.Post(srv, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != 3 || out.Error.Data != "0x08c379a0" || !strings.HasPrefix(out.Error.Message, "message execution failed (exit=[33]") {
		t.Fatalf("wire error: %+v", out.Error)
	}
}

// wireEth exposes EthEstimateGas on a bare go-jsonrpc server.
type wireEth struct{ c *ChainAPI }

func (w *wireEth) EthEstimateGas(ctx context.Context, callObj any) (string, error) {
	return w.c.EthEstimateGas(ctx, callObj)
}

func TestParseEthRevert(t *testing.T) {
	errStr := func(s string) []byte {
		b := []byte(errorFunctionSelector)
		w := make([]byte, 32)
		w[31] = 0x20
		b = append(b, w...)
		l := make([]byte, 32)
		l[31] = byte(len(s))
		b = append(b, l...)
		p := make([]byte, (len(s)+31)/32*32)
		copy(p, s)
		return append(b, p...)
	}
	panicCode := func(c byte) []byte {
		b := []byte(panicFunctionSelector)
		w := make([]byte, 32)
		w[31] = c
		return append(b, w...)
	}
	for _, tc := range []struct {
		in   []byte
		want string
	}{
		{nil, "none"},
		{errStr("Incorrect fee amount"), "Error(Incorrect fee amount)"},
		{panicCode(0x11), "ArithmeticOverflow()"},
		{panicCode(0x99), "Panic(0x99)"},
		{[]byte{0xde, 0xad, 0xbe, 0xef}, "0xdeadbeef"},
	} {
		if got := parseEthRevert(tc.in); got != tc.want {
			t.Errorf("parseEthRevert(%x) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInsufficientFundsMessageMatchesLotus(t *testing.T) {
	var bal, val uint256.Int
	_ = bal.SetFromDecimal("37605216685032464004")
	_ = val.SetFromHex("0xffffffffffffffffffffffffffff")
	got := insufficientFundsToError(&evm.InsufficientFundsError{Balance: bal, Value: val}).Message
	want := "message execution failed (exit=[SysErrInsufficientFunds(6)], vm error=[message failed with backtrace:\n--> caused by: send::send -- sender does not have funds to transfer (balance 37.605216685032464004, transfer 5192296858534827.628530496329220095) (5: insufficient funds)\n (RetCode=6)])"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// ---- fixture recorder (manual) ----

// glifRecorder is an estimateBackend over a Lotus RPC at a pinned block
// that records every read, to (re)generate testdata/estimategas_calib.json:
//
//	LANTERN_RECORD_ESTGAS_FIXTURE=https://api.calibration.node.glif.io/rpc/v1 //	  go test ./rpc/handlers -run TestRecordEstimateGasFixture
type glifRecorder struct {
	t     testing.TB
	url   string
	block string
	hc    *http.Client
	fx    *estimateFixture
}

func (g *glifRecorder) call(method string, params ...any) (json.RawMessage, *json.RawMessage) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := g.hc.Post(g.url, "application/json", bytes.NewReader(body))
	if err != nil {
		g.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage  `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		g.t.Fatalf("%s: %v", method, err)
	}
	return out.Result, out.Error
}

func (g *glifRecorder) str(method string, params ...any) string {
	res, e := g.call(method, params...)
	if e != nil {
		g.t.Fatalf("%s: %s", method, *e)
	}
	var s string
	_ = json.Unmarshal(res, &s)
	return s
}

func (g *glifRecorder) GetCode(a evm.Address) ([]byte, error) {
	k := addrKey(a)
	if c, ok := g.fx.Code[k]; ok {
		return decodeHexData(c)
	}
	c := g.str("eth_getCode", k, g.block)
	g.fx.Code[k] = c
	return decodeHexData(c)
}

func (g *glifRecorder) GetStorage(a evm.Address, key uint256.Int) (uint256.Int, error) {
	k := addrKey(a)
	kb := key.Bytes32()
	slot := "0x" + hex.EncodeToString(kb[:])
	v := g.str("eth_getStorageAt", k, slot, g.block)
	if g.fx.Storage[k] == nil {
		g.fx.Storage[k] = map[string]string{}
	}
	g.fx.Storage[k][slot] = v
	return parseWord(g.t, v), nil
}

func (g *glifRecorder) GetBalance(a evm.Address) (uint256.Int, error) {
	k := addrKey(a)
	v := g.str("eth_getBalance", k, g.block)
	g.fx.Balances[k] = v
	return parseWord(g.t, v), nil
}

func (g *glifRecorder) ActorKind(a evm.Address) (evm.ActorKind, error) {
	k := addrKey(a)
	code, _ := g.GetCode(a)
	kind := "account"
	if len(code) > 0 {
		kind = "evm"
	}
	g.fx.Kinds[k] = kind
	if kind == "evm" {
		g.actorName(a)
		return evm.ActorEVM, nil
	}
	return evm.ActorAccount, nil
}

func (g *glifRecorder) actorName(a evm.Address) string {
	k := addrKey(a)
	if n, ok := g.fx.ActorIDs[k]; ok {
		return n
	}
	f4 := g.str("Filecoin.EthAddressToFilecoinAddress", k)
	id := g.str("Filecoin.StateLookupID", f4, nil)
	n := "f0" + strings.TrimPrefix(strings.TrimPrefix(id, "f0"), "t0")
	g.fx.ActorIDs[k] = n
	return n
}

func (g *glifRecorder) BlockNumber() uint64 { return 0 }
func (g *glifRecorder) Timestamp() uint64   { return 0 }
func (g *glifRecorder) ChainID() uint64     { return 314159 }

func TestRecordEstimateGasFixture(t *testing.T) {
	url := os.Getenv("LANTERN_RECORD_ESTGAS_FIXTURE")
	if url == "" {
		t.Skip("set LANTERN_RECORD_ESTGAS_FIXTURE=<lotus rpc url> to re-record " + estimateFixturePath)
	}
	old := loadEstimateFixture(t)
	g := &glifRecorder{t: t, url: url, hc: &http.Client{Timeout: 60 * time.Second}}
	g.block = g.str("eth_blockNumber")
	g.fx = &estimateFixture{
		Comment:  old.Comment,
		Block:    g.block,
		Code:     map[string]string{},
		Storage:  map[string]map[string]string{},
		Balances: map[string]string{},
		Kinds:    map[string]string{},
		ActorIDs: map[string]string{},
	}
	for _, c := range old.Cases {
		ec, reason, ok := decodeEstimateCall(c.Call)
		if !ok {
			t.Fatalf("%s: %s", c.Name, reason)
		}
		out := runEstimate(g, ec, g.actorName)
		nc := estimateFixtureCase{Name: c.Name, Call: c.Call}
		obj := map[string]string{"from": c.Call.From, "to": c.Call.To}
		if c.Call.Data != "" {
			obj["data"] = c.Call.Data
		}
		if c.Call.Value != "" {
			obj["value"] = c.Call.Value
		}
		res, e := g.call("eth_estimateGas", obj, g.block)
		if e != nil {
			_ = json.Unmarshal(*e, &nc.Glif.Error)
		} else {
			_ = json.Unmarshal(res, &nc.Glif.Result)
		}
		t.Logf("%s: local executed=%v err=%v | glif result=%s err=%v", c.Name, out.executed, out.err, nc.Glif.Result, nc.Glif.Error)
		g.fx.Cases = append(g.fx.Cases, nc)
	}
	raw, _ := json.MarshalIndent(g.fx, "", " ")
	if err := os.WriteFile(estimateFixturePath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
