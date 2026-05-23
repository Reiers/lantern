// Phase 8 Part B: verify the bridge integration plumbing on the
// handler side. The bridge package itself is unit-tested in
// vm/bridge/bridge_test.go; this file proves the wiring inside
// StateCall (and indirectly the routing condition).

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	vmbridge "github.com/Reiers/lantern/vm/bridge"
)

type fakeBridge struct {
	calls int
	last  []*types.Message
}

func (f *fakeBridge) Provenance() string { return "fake" }
func (f *fakeBridge) RawJSONRPC(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("fakeBridge: RawJSONRPC not used in this test")
}
func (f *fakeBridge) ComputeStateRoot(_ context.Context, _ cid.Cid, _ int64, msgs []*types.Message) (cid.Cid, []*types.MessageReceipt, error) {
	f.calls++
	f.last = msgs
	out := make([]*types.MessageReceipt, len(msgs))
	for i := range msgs {
		out[i] = &types.MessageReceipt{
			ExitCode: exitcode.Ok,
			Return:   []byte("bridged-return"),
			GasUsed:  777,
		}
	}
	root, _ := cid.Parse("bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")
	return root, out, nil
}

func msg(method uint64) *types.Message {
	from, _ := addr.NewIDAddress(1)
	to, _ := addr.NewIDAddress(2)
	return &types.Message{
		Version:    0,
		From:       from,
		To:         to,
		Value:      big.Zero(),
		Method:     abi.MethodNum(method),
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(10),
	}
}

func newCAPI() *ChainAPI {
	stateRoot, _ := cid.Parse("bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	tr := &trustedroot.TrustedRoot{
		Epoch:        6_000_000,
		StateRoot:    stateRoot,
		TipSetKey:    types.NewTipSetKey(stateRoot),
		ParentWeight: big.NewInt(123),
	}
	c := &ChainAPI{
		Trusted:     tr,
		NetworkName: "mainnet",
	}
	return c
}

func TestStateCall_BridgeRoutesNonSend(t *testing.T) {
	c := newCAPI()
	b := &fakeBridge{}
	c.Bridge = b

	r, err := c.StateCall(context.Background(), msg(7), types.TipSetKey{})
	if err != nil {
		t.Fatalf("StateCall: %v", err)
	}
	if b.calls != 1 {
		t.Fatalf("bridge calls = %d, want 1", b.calls)
	}
	if r.MsgRct == nil || string(r.MsgRct.Return) != "bridged-return" {
		t.Fatalf("expected bridged Return bytes, got %#v", r.MsgRct)
	}
	if r.MsgRct.GasUsed != 777 {
		t.Fatalf("expected gas 777 from bridge, got %d", r.MsgRct.GasUsed)
	}
}

func TestStateCall_BridgeBypassedForSend(t *testing.T) {
	c := newCAPI()
	b := &fakeBridge{}
	c.Bridge = b

	// Send (method 0) goes through the native vm shell, not the bridge.
	_, err := c.StateCall(context.Background(), msg(0), types.TipSetKey{})
	if err != nil {
		t.Fatalf("StateCall: %v", err)
	}
	if b.calls != 0 {
		t.Fatalf("bridge should NOT be called for Send: calls=%d", b.calls)
	}
}

func TestStateCall_NoBridge_NonSendStillReturns(t *testing.T) {
	// Without a bridge, non-Send returns the native vm shell receipt
	// (which is SysErrInvalidReceiver per Phase 7 B1 documentation).
	c := newCAPI()
	r, err := c.StateCall(context.Background(), msg(7), types.TipSetKey{})
	if err != nil {
		t.Fatalf("StateCall: %v", err)
	}
	if r.MsgRct == nil {
		t.Fatal("nil receipt")
	}
	// We don't assert on the exact exit code here; the contract is
	// "doesn't crash and returns a structured result." That's enough
	// to prove the no-bridge path still works.
}

// Compile-time check: the *fakeBridge satisfies the vmbridge.Bridge
// interface declared in vm/bridge.
var _ vmbridge.Bridge = (*fakeBridge)(nil)
