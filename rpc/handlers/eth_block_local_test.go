package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// Real calibration fixture for the bridge-off eth block path (Curio's
// reorg check walks eth blocks and looks for its txs in them).
//
//	0x3feed0 (4189904)  3-block tipset, 42 eth txs, incl. tx1 (createDataSet)
//	0x3feec7 (4189895)  50 txs incl. tx2 (registerProvider) and 1 legacy
//	                    (EIP-155) tx
//	0x3feeb0 (4189872)  51 txs incl. 1 native secp message
//	                    (SubmitWindowedPoSt, rendered by Lotus' native
//	                    conversion)
//
// As in the receipt fixture, the header store only holds a partial head
// tipset at 4189905 (1 of its 4 blocks): every block must be rebuilt
// from parent links, and "latest" (head's parent) must be the FULL
// 3-block tipset at 4189904.
//
// Expected values are Glif responses captured at record time. Glif
// load-balances Lotus 1.38.3 and Forest; see asLotusShape for the one
// field they render differently.
//
// Re-record (network): LANTERN_RECORD_ETH_BLOCK_FIXTURE=1 go test -run TestEthBlockLocal ./rpc/handlers
const (
	ethBlkFixture = "testdata/eth_block_calib_4189904.json.gz"
	ethBlkLatest  = "0x3feed0"
)

var ethBlkHeights = []string{"0x3feed0", "0x3feec7", "0x3feeb0"}

// asLotusShape rewrites recorded Glif tx objects into the exact Lotus
// v1.38.3 rendering. The only field Forest and Lotus render differently
// is accessList on eth (delegated) txs: Lotus emits null (EthTx.AccessList
// is never set for them), Forest [] (and omits it on legacy txs). Native
// messages (r = s = 0) are [] on both.
func asLotusShape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		_, hasV := x["v"]
		_, hasInput := x["input"]
		if hasV && hasInput && !(x["r"] == "0x0" && x["s"] == "0x0") {
			x["accessList"] = nil
		}
		for k, e := range x {
			x[k] = asLotusShape(e)
		}
	case []any:
		for i := range x {
			x[i] = asLotusShape(x[i])
		}
	}
	return v
}

func isFullBloom(raw json.RawMessage) bool {
	var b struct {
		LogsBloom string `json:"logsBloom"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b.LogsBloom == "0x"+strings.Repeat("ff", 256)
}

// glifBlockRPC fetches a block, retrying while Glif answers with the
// "not indexed yet" full bloom (a Lotus backend whose chain indexer had
// not caught up); an indexed Lotus and Forest agree on the real bloom.
func glifBlockRPC(ctx context.Context, t *testing.T, g *hbGlif, method string, params []any) json.RawMessage {
	t.Helper()
	var res json.RawMessage
	var err error
	for i := 0; i < 16; i++ {
		// Vary the request id: Glif's balancer is sticky per request,
		// so a different id can reach an indexed backend.
		res, err = glifRPCWithID(ctx, g, method, params, i+1)
		if err != nil {
			t.Fatalf("glif %s %v: %v", method, params, err)
		}
		if !isFullBloom(res) {
			return res
		}
	}
	return res // still the unindexed answer; patched by patchBloom
}

// patchBloom replaces an unindexed (full) logsBloom in a recorded block
// with the bloom Lotus' chain indexer stores for the tipset: the OR of
// the receipts' blooms (same addr + topics of EVM-shaped events), taken
// from Glif's eth_getBlockReceipts. Used only when every Glif retry hit
// a Lotus backend that had cached the block before indexing it.
func patchBloom(ctx context.Context, t *testing.T, g *hbGlif, raw json.RawMessage, blockHash string, glif map[string]json.RawMessage, h string) json.RawMessage {
	t.Helper()
	if !isFullBloom(raw) {
		return raw
	}
	bloomKey := "receiptsbloom_" + h
	if _, ok := glif[bloomKey]; !ok {
		res, err := g.rpc(ctx, "eth_getBlockReceipts", []any{blockHash})
		if err != nil {
			t.Fatalf("glif eth_getBlockReceipts: %v", err)
		}
		var rcpts []struct {
			LogsBloom string `json:"logsBloom"`
		}
		if err := json.Unmarshal(res, &rcpts); err != nil {
			t.Fatal(err)
		}
		acc := make([]byte, 256)
		for _, r := range rcpts {
			b, err := hex.DecodeString(strings.TrimPrefix(r.LogsBloom, "0x"))
			if err != nil || len(b) != 256 {
				t.Fatalf("receipt bloom %q", r.LogsBloom)
			}
			for i := range acc {
				acc[i] |= b[i]
			}
		}
		glif[bloomKey] = json.RawMessage(strconv.Quote("0x" + hex.EncodeToString(acc)))
	}
	var bloom string
	_ = json.Unmarshal(glif[bloomKey], &bloom)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["logsBloom"] = bloom
	out, _ := json.Marshal(m)
	return out
}

func glifRPCWithID(ctx context.Context, g *hbGlif, method string, params []any, id int) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, "POST", g.url, bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if id%2 == 0 {
		req.Header.Set("user-agent", "curl/8.7.1")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, errors.New(method + ": " + out.Error.Message)
	}
	return out.Result, nil
}

// progressGetter reports record-mode fetch progress on stderr.
type progressGetter struct {
	inner interface {
		Get(context.Context, cid.Cid) ([]byte, error)
	}
	n atomic.Int64
}

func (p *progressGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if n := p.n.Add(1); n%100 == 0 {
		fmt.Fprintf(os.Stderr, "record: %d objects fetched\n", n)
	}
	return p.inner.Get(ctx, c)
}

func setupEthBlocks(t *testing.T) (*ChainAPI, map[string]json.RawMessage, func() *ethRcptFixtureFile) {
	t.Helper()
	record := os.Getenv("LANTERN_RECORD_ETH_BLOCK_FIXTURE") == "1"
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })

	var getter interface {
		Get(context.Context, cid.Cid) ([]byte, error)
	}
	var glif map[string]json.RawMessage
	var dump func() *ethRcptFixtureFile
	headCIDs := []string{ethRcptHeadBlock}

	if record {
		g := &hbGlif{url: "https://api.calibration.node.glif.io/rpc/v1", hc: &http.Client{Timeout: 60 * time.Second}}
		rg := &recordingGetter{inner: g, got: map[string][]byte{}}
		getter = &progressGetter{inner: rg}
		glif = map[string]json.RawMessage{}
		ctx := context.Background()
		for _, h := range ethBlkHeights {
			for _, full := range []bool{false, true} {
				glif["num_"+h+"_"+strconv.FormatBool(full)] = glifBlockRPC(ctx, t, g, "eth_getBlockByNumber", []any{h, full})
			}
			var blk struct {
				Hash         string            `json:"hash"`
				Transactions []json.RawMessage `json:"transactions"`
			}
			if err := json.Unmarshal(glif["num_"+h+"_true"], &blk); err != nil {
				t.Fatal(err)
			}
			glif["hash_"+h+"_true"] = glifBlockRPC(ctx, t, g, "eth_getBlockByHash", []any{blk.Hash, true})
			glif["hash_"+h+"_false"] = glifBlockRPC(ctx, t, g, "eth_getBlockByHash", []any{blk.Hash, false})
			for _, k := range []string{"num_" + h + "_false", "num_" + h + "_true", "hash_" + h + "_true", "hash_" + h + "_false"} {
				glif[k] = patchBloom(ctx, t, g, glif[k], blk.Hash, glif, h)
			}
			res, err := g.rpc(ctx, "eth_getBlockTransactionCountByNumber", []any{h})
			if err != nil {
				t.Fatal(err)
			}
			glif["count_"+h] = res
			if res, err = g.rpc(ctx, "eth_getBlockTransactionCountByHash", []any{blk.Hash}); err != nil {
				t.Fatal(err)
			}
			glif["counthash_"+h] = res
			// One tx by index per block: the first legacy or native tx
			// when present (the interesting conversions), else index 0.
			idx := 0
			for i, raw := range blk.Transactions {
				var tx map[string]any
				_ = json.Unmarshal(raw, &tx)
				if tx["type"] == "0x0" || tx["r"] == "0x0" {
					idx = i
					break
				}
			}
			ih := "0x" + strconv.FormatInt(int64(idx), 16)
			if res, err = g.rpc(ctx, "eth_getTransactionByBlockNumberAndIndex", []any{h, ih}); err != nil {
				t.Fatal(err)
			}
			glif["txidx_"+h] = res
			if res, err = g.rpc(ctx, "eth_getTransactionByBlockHashAndIndex", []any{blk.Hash, ih}); err != nil {
				t.Fatal(err)
			}
			glif["txhashidx_"+h] = res
			glif["txidxpos_"+h] = json.RawMessage(strconv.Quote(ih))
		}
		dump = func() *ethRcptFixtureFile {
			f := &ethRcptFixtureFile{Head: headCIDs, Blocks: map[string]string{}, Glif: glif}
			rg.mu.Lock()
			for k, v := range rg.got {
				f.Blocks[k] = base64.StdEncoding.EncodeToString(v)
			}
			rg.mu.Unlock()
			return f
		}
	} else {
		raw, err := os.ReadFile(ethBlkFixture)
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
		getter = mg
		glif = f.Glif
		headCIDs = f.Head
	}

	var blks []*types.BlockHeader
	for _, hc := range headCIDs {
		c, _ := cid.Parse(hc)
		raw, err := getter.Get(context.Background(), c)
		if err != nil {
			t.Fatalf("head block: %v", err)
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
	c := &ChainAPI{NetworkName: "calibration", HeaderStore: hs, BlockGetter: getter, EthTxLookback: 40}
	return c, glif, dump
}

func writeEthBlockFixture(t *testing.T, dump func() *ethRcptFixtureFile) {
	t.Helper()
	if dump == nil {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(dump()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ethBlkFixture, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", ethBlkFixture, buf.Len())
}

func wantJSON(t *testing.T, glif map[string]json.RawMessage, key string) any {
	t.Helper()
	raw, ok := glif[key]
	if !ok {
		t.Fatalf("fixture has no %q", key)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return asLotusShape(v)
}

// TestEthBlockLocal_MatchesLotus compares every bridge-off block / tx /
// count response field by field with Lotus (Glif) for the three fixture
// blocks: 0 diffs required.
func TestEthBlockLocal_MatchesLotus(t *testing.T) {
	c, glif, dump := setupEthBlocks(t)
	ctx := context.Background()

	check := func(name, key string, got any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := diffEthJSON(key, wantJSON(t, glif, key), normJSON(t, got)); len(d) > 0 {
			if len(d) > 12 {
				d = append(d[:12], "...")
			}
			t.Errorf("%s: field diffs vs Lotus:\n  %s", name, strings.Join(d, "\n  "))
		}
	}

	for _, h := range ethBlkHeights {
		for _, full := range []bool{false, true} {
			fs := strconv.FormatBool(full)
			got, err := c.EthGetBlockByNumber(ctx, h, full)
			check("eth_getBlockByNumber("+h+","+fs+")", "num_"+h+"_"+fs, got, err)

			hash := wantJSON(t, glif, "num_"+h+"_true").(map[string]any)["hash"].(string)
			got, err = c.EthGetBlockByHash(ctx, hash, full)
			check("eth_getBlockByHash("+h+","+fs+")", "hash_"+h+"_"+fs, got, err)
		}
		hash := wantJSON(t, glif, "num_"+h+"_true").(map[string]any)["hash"].(string)
		cnt, err := c.EthGetBlockTransactionCountByNumber(ctx, h)
		check("eth_getBlockTransactionCountByNumber("+h+")", "count_"+h, cnt, err)
		cnt, err = c.EthGetBlockTransactionCountByHash(ctx, hash)
		check("eth_getBlockTransactionCountByHash("+h+")", "counthash_"+h, cnt, err)

		var ih string
		if err := json.Unmarshal(glif["txidxpos_"+h], &ih); err != nil {
			t.Fatal(err)
		}
		tx, err := c.EthGetTransactionByBlockNumberAndIndex(ctx, h, ih)
		check("eth_getTransactionByBlockNumberAndIndex("+h+","+ih+")", "txidx_"+h, tx, err)
		tx, err = c.EthGetTransactionByBlockHashAndIndex(ctx, hash, ih)
		check("eth_getTransactionByBlockHashAndIndex("+h+","+ih+")", "txhashidx_"+h, tx, err)
	}

	// "latest" is the head's parent: the full 3-block tipset at 4189904.
	got, err := c.EthGetBlockByNumber(ctx, "latest", true)
	check("eth_getBlockByNumber(latest,true)", "num_"+ethBlkLatest+"_true", got, err)
	if bn, err := c.EthBlockNumber(ctx); err != nil || bn != ethBlkLatest {
		t.Errorf("eth_blockNumber = %q, %v; want %s (head's parent, like Lotus)", bn, err, ethBlkLatest)
	}

	// The native message renders exactly like Lotus' native conversion
	// (also covered by the field diff above; asserted here so a fixture
	// change can't silently drop the case).
	nat := 0
	blk, _ := c.EthGetBlockByNumber(ctx, "0x3feeb0", true)
	for _, tx := range normJSON(t, blk).(map[string]any)["transactions"].([]any) {
		m := tx.(map[string]any)
		if m["r"] == "0x0" && m["s"] == "0x0" {
			nat++
			if al, ok := m["accessList"].([]any); !ok || len(al) != 0 {
				t.Errorf("native tx accessList = %v, want []", m["accessList"])
			}
		}
	}
	if nat == 0 {
		t.Errorf("0x3feeb0: no native message found; fixture should contain one")
	}

	// Stock Curio's reorg walk over the same node (shares its caches, as a
	// running Lantern would).
	runCurioReorgWalk(t, c)

	writeEthBlockFixture(t, dump)
}

// runCurioReorgWalk replays stock Curio's reorg check
// (tasks/pdpv0/task_reorg_check.go via go-ethereum ethclient): latest
// block with full txs, then BlockByHash(parentHash) down the chain,
// collecting tx hashes. Both Curio txs must be found in their canonical
// blocks, and every block must pass go-ethereum's header/body checks.
func runCurioReorgWalk(t *testing.T, c *ChainAPI) {
	t.Helper()
	ctx := context.Background()

	raw, err := c.EthGetBlockByNumber(ctx, "latest", true)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]uint64{}
	minHeight := uint64(0x3feec7)
	for steps := 0; ; steps++ {
		if steps > 64 {
			t.Fatal("walk did not terminate")
		}
		blk := normJSON(t, raw).(map[string]any)
		num, _ := parseEthQuantity(blk["number"].(string))
		txs := blk["transactions"].([]any)
		// go-ethereum ethclient getBlock checks.
		root := blk["transactionsRoot"].(string)
		if root == ethEmptyRootHash && len(txs) > 0 {
			t.Fatalf("block %d: non-empty tx list but header says no transactions", num)
		}
		if root != ethEmptyRootHash && len(txs) == 0 {
			t.Fatalf("block %d: empty tx list but header indicates transactions", num)
		}
		for i, tx := range txs {
			m := tx.(map[string]any)
			if m["blockHash"] != blk["hash"] || m["blockNumber"] != blk["number"] {
				t.Fatalf("block %d tx %d: block fields %v/%v", num, i, m["blockHash"], m["blockNumber"])
			}
			found[m["hash"].(string)] = num
		}
		if num <= minHeight {
			break
		}
		parent := blk["parentHash"].(string)
		if raw, err = c.EthGetBlockByHash(ctx, parent, true); err != nil || raw == nil {
			t.Fatalf("BlockByHash(%s) from block %d: %v, %v", parent, num, raw, err)
		}
		if pn, _ := parseEthQuantity(normJSON(t, raw).(map[string]any)["number"].(string)); pn >= num {
			t.Fatalf("parent of %d has number %d", num, pn)
		}
	}
	if found[ethRcptTx1] != 0x3feed0 {
		t.Errorf("tx1 found at %d, want 0x3feed0", found[ethRcptTx1])
	}
	if found[ethRcptTx2] != 0x3feec7 {
		t.Errorf("tx2 found at %d, want 0x3feec7", found[ethRcptTx2])
	}
}

// TestEthBlockLocal_Tags pins the Lotus tag / range semantics.
func TestEthBlockLocal_Tags(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_BLOCK_FIXTURE") == "1" {
		t.Skip("record run")
	}
	c, _, _ := setupEthBlocks(t)
	ctx := context.Background()

	if _, err := c.EthGetBlockByNumber(ctx, "0x3feed1", false); !errors.Is(err, errEthFutureEpoch) {
		t.Errorf("head height: err = %v, want future epoch", err)
	}
	if _, err := c.EthGetBlockByNumber(ctx, "earliest", false); !errors.Is(err, errEthEarliest) {
		t.Errorf("earliest: err = %v", err)
	}
	// pending = the (partial, unexecuted) head: header fields of head,
	// unknown gas / bloom.
	p, err := c.EthGetBlockByNumber(ctx, "pending", false)
	if err != nil {
		t.Fatal(err)
	}
	pm := normJSON(t, p).(map[string]any)
	if pm["number"] != "0x3feed1" || pm["gasUsed"] != "0x0" || pm["logsBloom"] != "0x"+strings.Repeat("ff", 256) {
		t.Errorf("pending block: number %v gasUsed %v", pm["number"], pm["gasUsed"])
	}
	// Unknown hash: null, no error (no bridge).
	got, err := c.EthGetBlockByHash(ctx, "0x"+strings.Repeat("ab", 32), true)
	if err != nil || got != nil {
		t.Errorf("unknown hash: %v, %v", got, err)
	}
	// Out-of-range index.
	if _, err := c.EthGetTransactionByBlockNumberAndIndex(ctx, ethBlkLatest, "0x400"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("index out of range: err = %v", err)
	}
	// Block summary cache: a second fullTx=false call is served from it.
	if _, err := c.EthGetBlockByNumber(ctx, ethBlkLatest, false); err != nil {
		t.Fatal(err)
	}
	if s := c.ethBlockStateGet(); len(s.sums) == 0 {
		t.Errorf("block summary cache is empty after serving blocks")
	}
}
