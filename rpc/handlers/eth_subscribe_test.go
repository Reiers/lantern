package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/headnotify"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// mkTestCID returns a deterministic CID for the given seed string.
func mkTestCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, h)
}

// mkTestTipset builds a minimal single-block tipset at the given height,
// enough for tipsetToEthBlock to produce a non-nil ETH block.
func mkTestTipset(t *testing.T, h abi.ChainEpoch) *ltypes.TipSet {
	t.Helper()
	miner, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	bh := &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t")},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e")},
		Parents:               nil,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkTestCID(t, "state"),
		ParentMessageReceipts: mkTestCID(t, "receipts"),
		Messages:              mkTestCID(t, "msgs"),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	require.NoError(t, err)
	return ts
}

func hcBatch(t *testing.T, typ string, ts *ltypes.TipSet) []api.HeadChange {
	t.Helper()
	return []api.HeadChange{{Type: typ, Val: ts}}
}

// captureSubscriber is a mock EthSubscriberMethods reverse client that
// records every eth_subscription notification pushed to it. failAfter
// makes EthSubscription start returning an error (simulating a dead WS
// connection) after N successful pushes.
type captureSubscriber struct {
	mu        sync.Mutex
	got       []EthSubscriptionResponse
	failAfter int // 0 = never fail
	calls     int
	signalCh  chan struct{}
}

func (cs *captureSubscriber) push(_ context.Context, p jsonrpc.RawParams) error {
	cs.mu.Lock()
	cs.calls++
	if cs.failAfter > 0 && cs.calls > cs.failAfter {
		cs.mu.Unlock()
		return errors.New("simulated WS write failure")
	}
	var resp EthSubscriptionResponse
	_ = json.Unmarshal(p, &resp)
	cs.got = append(cs.got, resp)
	ch := cs.signalCh
	cs.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func (cs *captureSubscriber) methods() EthSubscriberMethods {
	return EthSubscriberMethods{EthSubscription: cs.push}
}

func (cs *captureSubscriber) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.got)
}

func (cs *captureSubscriber) responses() []EthSubscriptionResponse {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]EthSubscriptionResponse, len(cs.got))
	copy(out, cs.got)
	return out
}

// newSubTestCAPI returns a ChainAPI with a head-change distributor wired
// (no backing store needed: we drive events via PublishCustom).
func newSubTestCAPI() (*ChainAPI, *headnotify.Distributor) {
	c := newCAPI()
	dist := headnotify.New(nil, 16)
	c.HeadNotify = dist
	return c, dist
}

func TestEthSubscribeNewHeads_PushesApplyEvents(t *testing.T) {
	c, dist := newSubTestCAPI()
	cs := &captureSubscriber{signalCh: make(chan struct{}, 8)}

	id, err := c.subscribeNewHeads(context.Background(), cs.methods())
	require.NoError(t, err)
	require.Regexp(t, `^0x[0-9a-f]{64}$`, id, "sub id must be 0x + 32-byte hex")

	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 100)))
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 101)))

	waitFor(t, cs, 2)

	resps := cs.responses()
	require.Len(t, resps, 2)
	for _, r := range resps {
		require.Equal(t, id, r.SubscriptionID, "each notif carries the sub id")
		blk, ok := r.Result.(map[string]any)
		require.True(t, ok, "result is an eth-shaped block")
		require.Contains(t, blk, "number")
		require.Contains(t, blk, "hash")
	}
}

func TestEthSubscribeNewHeads_SkipsCurrentAndRevert(t *testing.T) {
	c, dist := newSubTestCAPI()
	cs := &captureSubscriber{signalCh: make(chan struct{}, 8)}

	_, err := c.subscribeNewHeads(context.Background(), cs.methods())
	require.NoError(t, err)

	dist.PublishCustom(hcBatch(t, "current", mkTestTipset(t, 100)))
	dist.PublishCustom(hcBatch(t, "revert", mkTestTipset(t, 100)))
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 101)))

	waitFor(t, cs, 1)
	time.Sleep(50 * time.Millisecond) // allow any erroneous extra pushes

	require.Equal(t, 1, cs.count(), "only the apply event should be pushed")
}

func TestEthUnsubscribe_StopsPushesAndCleansUp(t *testing.T) {
	c, dist := newSubTestCAPI()
	cs := &captureSubscriber{signalCh: make(chan struct{}, 8)}

	id, err := c.subscribeNewHeads(context.Background(), cs.methods())
	require.NoError(t, err)

	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 100)))
	waitFor(t, cs, 1)

	c.ethSubMu.Lock()
	_, present := c.ethSubs[id]
	c.ethSubMu.Unlock()
	require.True(t, present, "subscription should be tracked before unsubscribe")

	ok, err := c.EthUnsubscribe(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	c.ethSubMu.Lock()
	_, present = c.ethSubs[id]
	c.ethSubMu.Unlock()
	require.False(t, present, "subscription should be removed after unsubscribe")

	// EthUnsubscribe has returned, which now deterministically means the
	// push goroutine has fully stopped (it waits on sub.done). So a head
	// change published after this point can never reach the subscriber,
	// with no timing assumption needed.
	before := cs.count()
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 101)))
	require.Equal(t, before, cs.count(), "no pushes after unsubscribe")

	ok, err = c.EthUnsubscribe(context.Background(), "0xdeadbeef")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestEthSubscribeNewHeads_SelfCleansOnClientWriteFailure(t *testing.T) {
	c, dist := newSubTestCAPI()
	cs := &captureSubscriber{signalCh: make(chan struct{}, 8), failAfter: 1}

	id, err := c.subscribeNewHeads(context.Background(), cs.methods())
	require.NoError(t, err)

	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 100)))
	waitFor(t, cs, 1)
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 101)))

	require.Eventually(t, func() bool {
		c.ethSubMu.Lock()
		defer c.ethSubMu.Unlock()
		_, present := c.ethSubs[id]
		return !present
	}, 2*time.Second, 10*time.Millisecond, "sub should self-clean after client write failure")
}

func TestEthSubscribe_RequiresWebSocket(t *testing.T) {
	c, _ := newSubTestCAPI()
	// No reverse client in ctx == plain HTTP request.
	_, err := c.EthSubscribe(context.Background(), mustJSON(t, []any{"newHeads"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "WebSocket")
}

// logsBridge is a fakeBridge whose eth_getLogs returns a canned set of
// logs per call, and records the filter it was asked for.
type logsBridge struct {
	fakeBridge
	mu        sync.Mutex
	logs      []map[string]any // returned for every eth_getLogs call
	filters   []map[string]any // recorded filters seen
	failFirst bool             // first call returns an error
	calls     int
}

func (b *logsBridge) RawJSONRPC(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if method != "eth_getLogs" {
		return nil, errors.New("logsBridge: unexpected method " + method)
	}
	b.mu.Lock()
	b.calls++
	failNow := b.failFirst && b.calls == 1
	// params is [filter]; record the filter object.
	var arr []map[string]any
	_ = json.Unmarshal(params, &arr)
	if len(arr) > 0 {
		b.filters = append(b.filters, arr[0])
	}
	logs := b.logs
	b.mu.Unlock()
	if failNow {
		return nil, errors.New("simulated transient bridge error")
	}
	return json.Marshal(logs)
}

func (b *logsBridge) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *logsBridge) seenFilters() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]any, len(b.filters))
	copy(out, b.filters)
	return out
}

func TestEthSubscribeLogs_PushesMatchingLogsPerHead(t *testing.T) {
	c, dist := newSubTestCAPI()
	br := &logsBridge{logs: []map[string]any{
		{"address": "0xabc", "data": "0x1"},
		{"address": "0xabc", "data": "0x2"},
	}}
	c.Bridge = br
	cs := &captureSubscriber{signalCh: make(chan struct{}, 16)}

	filter := map[string]json.RawMessage{"address": mustRaw(t, "0xabc")}
	id, err := c.subscribeLogs(context.Background(), cs.methods(), filter)
	require.NoError(t, err)
	require.Regexp(t, `^0x[0-9a-f]{64}$`, id)

	// One apply head -> one eth_getLogs call -> 2 logs pushed.
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 500)))
	waitFor(t, cs, 2)

	resps := cs.responses()
	require.Len(t, resps, 2)
	for _, r := range resps {
		require.Equal(t, id, r.SubscriptionID)
	}

	// The bridge filter should carry the caller's address and the
	// per-head fromBlock=toBlock=0x1f4 (500).
	filters := br.seenFilters()
	require.Len(t, filters, 1)
	require.Equal(t, "0xabc", filters[0]["address"])
	require.Equal(t, "0x1f4", filters[0]["fromBlock"])
	require.Equal(t, "0x1f4", filters[0]["toBlock"])
}

func TestEthSubscribeLogs_TransientBridgeErrorKeepsSubAlive(t *testing.T) {
	c, dist := newSubTestCAPI()
	br := &logsBridge{
		logs:      []map[string]any{{"address": "0xabc", "data": "0x9"}},
		failFirst: true,
	}
	c.Bridge = br
	cs := &captureSubscriber{signalCh: make(chan struct{}, 16)}

	id, err := c.subscribeLogs(context.Background(), cs.methods(), nil)
	require.NoError(t, err)

	// First head: bridge errors -> no push, sub stays alive.
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 10)))
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 0, cs.count(), "no push on transient bridge error")

	c.ethSubMu.Lock()
	_, present := c.ethSubs[id]
	c.ethSubMu.Unlock()
	require.True(t, present, "sub must survive a transient bridge error")

	// Second head: bridge recovers -> log pushed.
	dist.PublishCustom(hcBatch(t, "apply", mkTestTipset(t, 11)))
	waitFor(t, cs, 1)
	require.GreaterOrEqual(t, br.callCount(), 2)
}

// TestEthSubscribeLogs_RequiresLogSource: with neither a header store
// (local getLogs, lantern#73) nor a VM bridge, log subscriptions have no
// source and must be refused. (Previously this required a bridge outright;
// lantern#76 made EthGetLogs local-first, so a header store alone now
// suffices — see the bridge-off positive test below.)
func TestEthSubscribeLogs_RequiresLogSource(t *testing.T) {
	c, _ := newSubTestCAPI()
	c.Bridge = nil
	c.HeaderStore = nil
	_, err := c.subscribeLogs(context.Background(), (&captureSubscriber{}).methods(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "log source")
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return json.RawMessage(b)
}

// --- helpers ---

func waitFor(t *testing.T, cs *captureSubscriber, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return cs.count() >= n },
		2*time.Second, 5*time.Millisecond, "expected at least %d pushes", n)
}

func mustJSON(t *testing.T, v any) jsonrpc.RawParams {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return jsonrpc.RawParams(b)
}
