// ForestBridge: a Bridge backed by a Forest (or Lotus) Filecoin.StateCompute
// RPC endpoint.
//
// Forest exposes the standard Lotus-compatible JSON-RPC at /rpc/v1. Both
// Forest and Lotus implement Filecoin.StateCompute(epoch, msgs, tsk) ->
// {Root: CID, Trace: [InvocResult]} where Root is the post-execution
// state root and Trace[i].MsgRct is the receipt for msgs[i].
//
// We use that single method for both StateCall (when the native vm shell
// declines) and MinerCreateBlock post-execution state root computation.

package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// ForestBridge talks JSON-RPC to a Forest (or Lotus) full node.
type ForestBridge struct {
	URL    string
	Token  string // optional Bearer token (empty for unauthenticated)
	Client *http.Client
	tag    string
}

// NewForestBridge constructs a ForestBridge.
//
//	url    — base RPC URL, e.g. "http://127.0.0.1:2345/rpc/v1"
//	token  — optional Bearer token (empty string for unauthenticated)
//	timeout — per-request timeout; 30s is a reasonable default
func NewForestBridge(url, token string, timeout time.Duration) *ForestBridge {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	tag := "forest"
	if i := strings.Index(url, "://"); i >= 0 {
		tag = "forest@" + url[i+3:]
	}
	return &ForestBridge{
		URL:    url,
		Token:  token,
		Client: &http.Client{Timeout: timeout},
		tag:    tag,
	}
}

// Provenance returns "forest@<host>" or similar tag.
func (f *ForestBridge) Provenance() string { return f.tag }

// ComputeStateRoot calls Filecoin.StateCompute on the upstream node.
func (f *ForestBridge) ComputeStateRoot(ctx context.Context, base cid.Cid, epoch int64, msgs []*types.Message) (cid.Cid, []*types.MessageReceipt, error) {
	if !base.Defined() {
		return cid.Undef, nil, errors.New("ForestBridge: undefined base state root")
	}
	// Lotus's StateCompute takes (epoch, []*Message, tsk). The "tsk"
	// argument is the tipset key to pin against; passing null tells the
	// upstream to use its current head, which matches "base = head
	// stateRoot" semantics for Lantern's most common call sites.
	//
	// For the case where Lantern's caller has a specific historical
	// tipset key, the bridge currently passes nil; the upstream
	// computes against its head. Future Phase 8.x: encode the tsk
	// directly when Lantern's accessor knows it.

	// Encode each message as the JSON shape Lotus expects.
	jsonMsgs := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return cid.Undef, nil, fmt.Errorf("ForestBridge: marshal msg[%d]: %w", i, err)
		}
		jsonMsgs[i] = b
	}

	params, err := json.Marshal([]interface{}{epoch, jsonMsgs, nil})
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: marshal params: %w", err)
	}

	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "Filecoin.StateCompute",
		"params":  json.RawMessage(params),
		"id":      1,
	})
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: marshal rpc body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", f.URL, bytes.NewReader(body))
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: rpc call: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 256))
	}

	var envelope struct {
		Result *stateComputeResult `json:"result"`
		Error  *jsonRPCError       `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: decode response: %w (body=%s)", err, truncate(string(respBody), 256))
	}
	if envelope.Error != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: upstream %s error %d: %s",
			f.Provenance(), envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result == nil {
		return cid.Undef, nil, errors.New("ForestBridge: empty result")
	}
	root, err := envelope.Result.Root.parse()
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("ForestBridge: parse root cid %q: %w", envelope.Result.Root.Slash, err)
	}
	recs := make([]*types.MessageReceipt, 0, len(envelope.Result.Trace))
	for i, t := range envelope.Result.Trace {
		recs = append(recs, t.MsgRct.toMessageReceipt())
		_ = i
	}
	// Forest may return fewer/different traces than messages; pad with
	// zero-receipts on mismatch so callers don't index out of bounds.
	for len(recs) < len(msgs) {
		recs = append(recs, &types.MessageReceipt{ExitCode: 0})
	}
	return root, recs[:len(msgs)], nil
}

// --- wire types ---

// stateComputeResult mirrors the Lotus StateCompute response shape.
//
// Critical wire-format note: Lotus encodes the Root CID as a JSON object
// `{"/":"<cid>"}` (the standard IPLD-link JSON shape), NOT as a bare
// string. Same for every other CID in the response. Decoding into a
// `string` field silently leaves Root empty.
type stateComputeResult struct {
	Root  cidLink                 `json:"Root"`
	Trace []stateComputeTraceItem `json:"Trace"`
}

// cidLink is the IPLD-link JSON shape: {"/":"<cid>"}.
type cidLink struct {
	Slash string `json:"/"`
}

func (l cidLink) parse() (cid.Cid, error) {
	if l.Slash == "" {
		return cid.Undef, errors.New("cidLink: empty")
	}
	return cid.Parse(l.Slash)
}

type stateComputeTraceItem struct {
	MsgRct apiMessageReceipt `json:"MsgRct"`
}

// apiMessageReceipt mirrors the Lotus JSON shape:
//
//	{"ExitCode": 0, "Return": "<base64>", "GasUsed": 0}
//
// types.MessageReceipt's MarshalJSON renders Return as base64 already
// so we mirror that here for decoding.
type apiMessageReceipt struct {
	ExitCode int64  `json:"ExitCode"`
	Return   string `json:"Return"`
	GasUsed  int64  `json:"GasUsed"`
}

func (r *apiMessageReceipt) toMessageReceipt() *types.MessageReceipt {
	var ret []byte
	if r.Return != "" {
		if dec, err := base64.StdEncoding.DecodeString(r.Return); err == nil {
			ret = dec
		}
	}
	return &types.MessageReceipt{
		ExitCode: exitcode.ExitCode(r.ExitCode),
		Return:   ret,
		GasUsed:  r.GasUsed,
	}
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// RawJSONRPC forwards an arbitrary JSON-RPC method call to the upstream
// Forest/Lotus node. Used by handlers for FEVM-shaped methods
// (eth_call, eth_estimateGas, eth_sendRawTransaction) that Lantern's
// native Send-only VM can't execute.
//
// `params` is already-marshaled JSON. The upstream's `result` is
// returned verbatim as json.RawMessage so the caller can decode it
// with the shape it expects.
func (f *ForestBridge) RawJSONRPC(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if params == nil {
		params = json.RawMessage("[]")
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	if err != nil {
		return nil, fmt.Errorf("ForestBridge: marshal rpc body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", f.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ForestBridge: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ForestBridge: %s rpc call: %w", method, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ForestBridge: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ForestBridge: HTTP %d on %s: %s", resp.StatusCode, method, truncate(string(respBody), 256))
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *jsonRPCError   `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("ForestBridge: decode response: %w (body=%s)", err, truncate(string(respBody), 256))
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("ForestBridge: upstream %s on %s: code=%d %s",
			f.Provenance(), method, envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result, nil
}
