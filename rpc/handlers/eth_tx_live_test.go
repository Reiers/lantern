package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// Live-path regression for 0x468f...01d4 (CreateDataSet sent by stock
// Curio-PDP through lantern-m2 on calibration, included at 4189932 in a
// 2-block tipset, executed at 4189933, 10 logs). Curio stored a receipt
// for it with logs [] and blockHash = digest of the first block CID; that
// came from the pre-#173 binary (ecf21fa), but the scenarios below pin the
// live shapes the #173 path must handle:
//
//   - first poll before the child tipset exists: null, never logs [];
//   - first poll after execution, tx in the send index, head tipset only
//     partially in the header store: Lotus-identical receipt;
//   - same tx after a restart (send index empty): Lotus-identical receipt;
//   - unavailable blocks / exhausted time budget: an error, never null
//     and never logs [].
//
// Re-record: LANTERN_RECORD_ETH_RECEIPT_FIXTURE=1 go test -run TestEthTxLive ./rpc/handlers
const (
	liveFixture   = "testdata/eth_receipt_calib_4189932.json.gz"
	liveTx        = "0x468fbdb14caafd192ac7977a591e591a08757104bd74a8306e0e870eb61d01d4"
	liveMsgCID    = "bafy2bzacecyxef34a7y4u4irk7ilazkfs6umt7ebsku7dcnt2z2fke7v3zsnq"
	liveInclBlkA  = "bafy2bzaceasxktzo6v6vhlvpn74ykdyi7xmdrohzhpr2jiiff7npclfmpjnmq" // 1 of 2 @4189932
	liveChildBlk  = "bafy2bzacec3qjrhimbfgfth6tjfxbmfangzppz7kvmxvmi77gn6d7et2sskra" // 1 of 4 @4189933
	liveInclEpoch = 4189932
)

type liveEnv struct {
	getter interface {
		Get(context.Context, cid.Cid) ([]byte, error)
	}
	glif map[string]json.RawMessage
	rec  *recordingGetter
}

func loadLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	if os.Getenv("LANTERN_RECORD_ETH_RECEIPT_FIXTURE") == "1" {
		g := &hbGlif{url: "https://api.calibration.node.glif.io/rpc/v1", hc: &http.Client{Timeout: 60 * time.Second}}
		rg := &recordingGetter{inner: g, got: map[string][]byte{}}
		glif := map[string]json.RawMessage{}
		for k, m := range map[string]string{"receipt": "eth_getTransactionReceipt", "tx": "eth_getTransactionByHash"} {
			res, err := g.rpc(context.Background(), m, []any{liveTx})
			if err != nil {
				t.Fatalf("glif %s: %v", k, err)
			}
			glif[k] = res
		}
		return &liveEnv{getter: rg, glif: glif, rec: rg}
	}
	raw, err := os.ReadFile(liveFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var f ethRcptFixtureFile
	if err := json.NewDecoder(zr).Decode(&f); err != nil {
		t.Fatal(err)
	}
	mg := mapGetter{}
	for k, v := range f.Blocks {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		mg[k] = b
	}
	return &liveEnv{getter: mg, glif: f.Glif}
}

func (e *liveEnv) save(t *testing.T) {
	if e.rec == nil {
		return
	}
	f := &ethRcptFixtureFile{Blocks: map[string]string{}, Glif: e.glif}
	e.rec.mu.Lock()
	for k, v := range e.rec.got {
		f.Blocks[k] = base64.StdEncoding.EncodeToString(v)
	}
	e.rec.mu.Unlock()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(f); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()
	if err := os.WriteFile(liveFixture, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d blocks (%d bytes gz)", len(f.Blocks), buf.Len())
}

// apiWithHead builds a ChainAPI whose header store holds only the given
// head block(s), like a gossip-fed node that missed sibling blocks.
func (e *liveEnv) apiWithHead(t *testing.T, getter interface {
	Get(context.Context, cid.Cid) ([]byte, error)
}, headBlocks ...string) *ChainAPI {
	t.Helper()
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	var blks []*types.BlockHeader
	for _, hc := range headBlocks {
		raw, err := getter.Get(context.Background(), cid.MustParse(hc))
		if err != nil {
			t.Fatalf("head block %s: %v", hc, err)
		}
		bh, err := types.DecodeBlock(raw)
		if err != nil {
			t.Fatal(err)
		}
		blks = append(blks, bh)
	}
	head, err := types.NewTipSet(blks)
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.PutTipSet(head); err != nil {
		t.Fatal(err)
	}
	if err := hs.SetHead(context.Background(), head); err != nil {
		t.Fatal(err)
	}
	return &ChainAPI{NetworkName: "calibration", HeaderStore: hs, BlockGetter: getter, EthTxLookback: 3}
}

func (e *liveEnv) wantJSON(t *testing.T, k string) any {
	var v any
	if err := json.Unmarshal(e.glif[k], &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEthTxLive_468f(t *testing.T) {
	e := loadLiveEnv(t)
	ctx := context.Background()
	defer e.save(t)

	// 1. First poll right after inclusion, before any child tipset: the tx
	// is in the send index but not executed. Lotus answers null.
	{
		c := e.apiWithHead(t, e.getter, liveInclBlkA)
		c.sentTx().put(liveTx, sentTxRecord{msgCID: cid.MustParse(liveMsgCID), sentEpoch: liveInclEpoch - 1})
		got, err := c.EthGetTransactionReceipt(ctx, liveTx)
		if err != nil || got != nil {
			t.Fatalf("before execution: want (nil, nil), got (%v, %v)", got, err)
		}
	}

	// 2. First poll after execution, in the send index, head tipset partial.
	{
		c := e.apiWithHead(t, e.getter, liveChildBlk)
		c.sentTx().put(liveTx, sentTxRecord{msgCID: cid.MustParse(liveMsgCID), sentEpoch: liveInclEpoch - 1})
		got, err := c.EthGetTransactionReceipt(ctx, liveTx)
		if err != nil || got == nil {
			t.Fatalf("after execution (sent): got (%v, %v)", got, err)
		}
		if d := diffEthJSON("receipt", e.wantJSON(t, "receipt"), normJSON(t, got)); len(d) > 0 {
			t.Fatalf("sent-index receipt: %d diffs vs Lotus:\n  %s", len(d), strings.Join(d, "\n  "))
		}
		wantB, _ := json.Marshal(e.wantJSON(t, "receipt"))
		gotB, _ := json.Marshal(normJSON(t, got))
		if !bytes.Equal(wantB, gotB) {
			t.Fatalf("receipt not byte-equal (canonical JSON):\nwant %s\ngot  %s", wantB, gotB)
		}
	}

	// 3. After a restart: send index empty, match by recomputed eth hash.
	{
		c := e.apiWithHead(t, e.getter, liveChildBlk)
		got, err := c.EthGetTransactionReceipt(ctx, liveTx)
		if err != nil || got == nil {
			t.Fatalf("after restart: got (%v, %v)", got, err)
		}
		if d := diffEthJSON("receipt", e.wantJSON(t, "receipt"), normJSON(t, got)); len(d) > 0 {
			t.Fatalf("restart receipt: %d diffs vs Lotus:\n  %s", len(d), strings.Join(d, "\n  "))
		}
		tx, err := c.EthGetTransactionByHash(ctx, liveTx)
		if err != nil || tx == nil {
			t.Fatalf("tx by hash: got (%v, %v)", tx, err)
		}
		// accessList is null or [] depending on the Lotus version behind
		// Glif (both mean "empty" to go-ethereum); compare the rest.
		wantTx := e.wantJSON(t, "tx").(map[string]any)
		gotTx := normJSON(t, tx).(map[string]any)
		if wantTx["accessList"] == nil {
			if al, ok := gotTx["accessList"].([]any); ok && len(al) == 0 {
				gotTx["accessList"] = nil
			}
		}
		if d := diffEthJSON("tx", wantTx, gotTx); len(d) > 0 {
			t.Fatalf("tx by hash: %d diffs vs Lotus:\n  %s", len(d), strings.Join(d, "\n  "))
		}
	}
}

// liveFaultGetter fails for a chosen set of CIDs and can add latency.
type liveFaultGetter struct {
	inner interface {
		Get(context.Context, cid.Cid) ([]byte, error)
	}
	fail  func(cid.Cid, []byte) bool
	delay time.Duration
}

func (f *liveFaultGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b, err := f.inner.Get(ctx, c)
	if err == nil && f.fail != nil && f.fail(c, b) {
		return nil, errors.New("block unavailable (test)")
	}
	return b, err
}

func TestEthTxLive_UnavailableBlocksAreAnErrorNotNull(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_RECEIPT_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	e := loadLiveEnv(t)
	// Every message body unavailable: the eth-hash scan cannot decide.
	isMsgBody := func(_ cid.Cid, b []byte) bool {
		if _, err := types.DecodeSignedMessage(b); err == nil {
			return true
		}
		return false
	}
	fg := &liveFaultGetter{inner: e.getter, fail: isMsgBody}
	c := e.apiWithHead(t, e.getter, liveChildBlk)
	c.BlockGetter = fg
	got, err := c.EthGetTransactionReceipt(context.Background(), liveTx)
	if got != nil {
		t.Fatalf("want no receipt, got %v", got)
	}
	var inc *errEthTxIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("want errEthTxIncomplete, got %v", err)
	}
}

func TestEthTxLive_TimeBudgetNeverHangs(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_RECEIPT_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	e := loadLiveEnv(t)
	fg := &liveFaultGetter{inner: e.getter, delay: 50 * time.Millisecond}
	c := e.apiWithHead(t, e.getter, liveChildBlk)
	c.BlockGetter = fg
	c.EthTxLookupTimeout = 300 * time.Millisecond
	start := time.Now()
	got, err := c.EthGetTransactionReceipt(context.Background(),
		"0x3333333333333333333333333333333333333333333333333333333333333333")
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("lookup took %s, budget 300ms", el)
	}
	if got != nil || err == nil {
		t.Fatalf("want (nil, incomplete error), got (%v, %v)", got, err)
	}
}
