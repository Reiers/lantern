// Unit tests for ForestBridge.
//
// We spin up an httptest.Server that mimics a Lotus/Forest
// Filecoin.StateCompute response and verify the bridge:
//   - issues a well-formed JSON-RPC request
//   - parses the response root + receipts correctly
//   - propagates upstream errors
//   - respects ctx cancellation
//   - sends the Bearer token when one is configured

package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/types"
)

func mkMsg(t *testing.T, from, to uint64, val uint64) *types.Message {
	t.Helper()
	fa, err := address.NewIDAddress(from)
	if err != nil {
		t.Fatalf("mkMsg: from: %v", err)
	}
	ta, err := address.NewIDAddress(to)
	if err != nil {
		t.Fatalf("mkMsg: to: %v", err)
	}
	return &types.Message{
		Version:    0,
		To:         ta,
		From:       fa,
		Nonce:      1,
		Value:      big.NewInt(int64(val)),
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(10),
		Method:     0,
		Params:     nil,
	}
}

func mkRootCID(t *testing.T, tag string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(tag), mh.SHA2_256, -1)
	if err != nil {
		t.Fatalf("mkRootCID: %v", err)
	}
	return cid.NewCidV1(cid.DagCBOR, hash)
}

// readRPCReq decodes the JSON-RPC request the bridge sent.
type rpcReq struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func decodeRPC(t *testing.T, r *http.Request) rpcReq {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode rpc req: %v (body=%s)", err, body)
	}
	return req
}

func TestForestBridge_HappyPath(t *testing.T) {
	wantRoot := mkRootCID(t, "happy-path-root")

	var seenMethod string
	var seenParamsLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRPC(t, r)
		seenMethod = req.Method
		seenParamsLen = len(req.Params)
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"Root": map[string]string{"/": wantRoot.String()},
				"Trace": []map[string]interface{}{
					{"MsgRct": map[string]interface{}{
						"ExitCode": 0,
						"Return":   base64.StdEncoding.EncodeToString([]byte("hello")),
						"GasUsed":  12345,
					}},
				},
			},
		}
		// The bridge expects Root as a JSON string. Adjust to the wire
		// shape Forest actually sends: a bare CID string under "Root".
		respPayload := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"Root": wantRoot.String(),
				"Trace": []map[string]interface{}{
					{"MsgRct": map[string]interface{}{
						"ExitCode": 0,
						"Return":   base64.StdEncoding.EncodeToString([]byte("hello")),
						"GasUsed":  12345,
					}},
				},
			},
		}
		_ = resp
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respPayload)
	}))
	defer srv.Close()

	b := NewForestBridge(srv.URL, "", 5*time.Second)
	base := mkRootCID(t, "parent-state")
	msgs := []*types.Message{mkMsg(t, 1000, 1001, 42)}

	gotRoot, gotRecs, err := b.ComputeStateRoot(context.Background(), base, 1234, msgs)
	if err != nil {
		t.Fatalf("ComputeStateRoot: %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("root mismatch: got %s want %s", gotRoot, wantRoot)
	}
	if len(gotRecs) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(gotRecs))
	}
	if gotRecs[0].ExitCode != exitcode.Ok {
		t.Fatalf("receipt exit code = %d, want 0", gotRecs[0].ExitCode)
	}
	if gotRecs[0].GasUsed != 12345 {
		t.Fatalf("receipt gas used = %d, want 12345", gotRecs[0].GasUsed)
	}
	if string(gotRecs[0].Return) != "hello" {
		t.Fatalf("receipt return = %q, want %q", gotRecs[0].Return, "hello")
	}
	if seenMethod != "Filecoin.StateCompute" {
		t.Fatalf("upstream method = %q, want Filecoin.StateCompute", seenMethod)
	}
	if seenParamsLen != 3 {
		t.Fatalf("want 3 params (epoch, msgs, tsk), got %d", seenParamsLen)
	}
}

func TestForestBridge_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32602,
				"message": "invalid epoch",
			},
		})
	}))
	defer srv.Close()

	b := NewForestBridge(srv.URL, "", 5*time.Second)
	base := mkRootCID(t, "x")
	_, _, err := b.ComputeStateRoot(context.Background(), base, 1, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	wantSubstr := "invalid epoch"
	if !contains(err.Error(), wantSubstr) {
		t.Fatalf("error missing %q: %v", wantSubstr, err)
	}
}

func TestForestBridge_HTTPNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("backend offline"))
	}))
	defer srv.Close()

	b := NewForestBridge(srv.URL, "", 5*time.Second)
	_, _, err := b.ComputeStateRoot(context.Background(), mkRootCID(t, "x"), 1, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !contains(err.Error(), "503") {
		t.Fatalf("error missing HTTP 503: %v", err)
	}
}

func TestForestBridge_BearerToken(t *testing.T) {
	wantToken := "secret-jwt-blob"
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"Root":  mkRootCID(t, "tok").String(),
				"Trace": []map[string]interface{}{},
			},
		})
	}))
	defer srv.Close()

	b := NewForestBridge(srv.URL, wantToken, 5*time.Second)
	_, _, err := b.ComputeStateRoot(context.Background(), mkRootCID(t, "x"), 1, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawAuth != "Bearer "+wantToken {
		t.Fatalf("auth header = %q, want %q", sawAuth, "Bearer "+wantToken)
	}
}

func TestForestBridge_ContextCancellation(t *testing.T) {
	// Server that never responds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	b := NewForestBridge(srv.URL, "", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, _, err := b.ComputeStateRoot(ctx, mkRootCID(t, "x"), 1, nil)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error after cancel, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ComputeStateRoot did not return after ctx cancel")
	}
}

func TestForestBridge_UndefinedBase(t *testing.T) {
	b := NewForestBridge("http://does.not.matter", "", time.Second)
	_, _, err := b.ComputeStateRoot(context.Background(), cid.Undef, 1, nil)
	if err == nil {
		t.Fatal("want error for undefined base")
	}
	if !contains(err.Error(), "undefined base") {
		t.Fatalf("error missing 'undefined base': %v", err)
	}
}

func TestForestBridge_Provenance(t *testing.T) {
	b := NewForestBridge("http://my-lotus.example:1234/rpc/v1", "", 0)
	got := b.Provenance()
	want := "forest@my-lotus.example:1234/rpc/v1"
	if got != want {
		t.Fatalf("provenance = %q, want %q", got, want)
	}
}

// contains is a tiny helper so we don't pull in strings just for this.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Compile-time guard that the bridge satisfies the Bridge interface.
var _ Bridge = (*ForestBridge)(nil)

// Defensive: if anyone ever changes the ForestBridge signature we want
// the tests to fail loudly rather than silently.
func TestForestBridge_InterfaceCompliance(t *testing.T) {
	var _ Bridge = (*ForestBridge)(nil)
	_ = fmt.Sprintf("%T", &ForestBridge{}) // suppress import-unused if shape changes
	_ = errors.New                         // same
}
