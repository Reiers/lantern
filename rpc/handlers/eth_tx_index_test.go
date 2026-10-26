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
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/hamt"
)

// Persistent eth tx index (F12) against the real calibration fixture.
//
// testdata/eth_txindex_calib_4189904.json.gz holds the extra blocks the
// indexer reads for the 2-epoch window under the fixture head (every secp
// message body, receipts and events AMTs of 4189903 and 4189904) on top
// of eth_receipt_calib_4189904.json.gz.
//
// Re-record (network): LANTERN_RECORD_ETH_TXINDEX_FIXTURE=1 go test -run TestEthTxIndex_ ./rpc/handlers
const (
	ethIdxFixture = "testdata/eth_txindex_calib_4189904.json.gz"
	ethIdxWindow  = abi.ChainEpoch(2)
)

type countingGetter struct {
	inner hamt.BlockGetter
	n     atomic.Int64
	delay time.Duration
	fail  atomic.Bool // simulate an unreachable network
}

func (g *countingGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	g.n.Add(1)
	if g.fail.Load() {
		return nil, errors.New("offline")
	}
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.inner.Get(ctx, c)
}

func readGzJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(zr).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func fixtureBlocks(t testing.TB, files ...string) (mapGetter, *ethRcptFixtureFile) {
	t.Helper()
	mg := mapGetter{}
	var first *ethRcptFixtureFile
	for _, f := range files {
		var ff ethRcptFixtureFile
		readGzJSON(t, f, &ff)
		if first == nil {
			first = &ff
		}
		for k, v := range ff.Blocks {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatal(err)
			}
			mg[k] = b
		}
	}
	return mg, first
}

// newIdxChainAPI builds a ChainAPI over hs + getter with the fixture head.
func newIdxChainAPI(t testing.TB, hs *hstore.Store, getter hamt.BlockGetter, headCIDs []string) *ChainAPI {
	t.Helper()
	if hs.Head() == nil {
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
	}
	return &ChainAPI{NetworkName: "calibration", HeaderStore: hs, BlockGetter: getter, EthTxLookback: 12}
}

type idxEnv struct {
	hs     *hstore.Store
	getter hamt.BlockGetter
	head   []string
	glif   map[string]json.RawMessage
	save   func()
}

func setupEthIdx(t *testing.T) *idxEnv {
	t.Helper()
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	base, baseFile := fixtureBlocks(t, ethRcptFixture)
	env := &idxEnv{hs: hs, head: baseFile.Head, glif: baseFile.Glif}
	if os.Getenv("LANTERN_RECORD_ETH_TXINDEX_FIXTURE") == "1" {
		g := &hbGlif{url: "https://api.calibration.node.glif.io/rpc/v1", hc: &http.Client{Timeout: 60 * time.Second}}
		rg := &recordingGetter{inner: g, got: map[string][]byte{}}
		env.getter = &overlayGetterStr{m: base, inner: rg}
		env.save = func() {
			f := &ethRcptFixtureFile{Head: baseFile.Head, Blocks: map[string]string{}}
			rg.mu.Lock()
			for k, v := range rg.got {
				if _, ok := base[k]; !ok {
					f.Blocks[k] = base64.StdEncoding.EncodeToString(v)
				}
			}
			rg.mu.Unlock()
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if err := json.NewEncoder(zw).Encode(f); err != nil {
				t.Fatal(err)
			}
			_ = zw.Close()
			if err := os.WriteFile(ethIdxFixture, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("recorded %d extra blocks (%d bytes gz)", len(f.Blocks), buf.Len())
		}
	} else {
		all, _ := fixtureBlocks(t, ethRcptFixture, ethIdxFixture)
		env.getter = all
	}
	return env
}

type overlayGetterStr struct {
	m     mapGetter
	inner hamt.BlockGetter
}

func (o *overlayGetterStr) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if b, ok := o.m[c.String()]; ok {
		return b, nil
	}
	return o.inner.Get(ctx, c)
}

// startIndexed starts the index (no background loops) and runs one
// synchronous backfill pass.
func startIndexed(t testing.TB, c *ChainAPI, backfill bool) *ethTxIndexer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.StartEthTxIndex(ctx, EthTxIndexOptions{Window: ethIdxWindow, NoBackfill: true}); err != nil {
		t.Fatal(err)
	}
	ei := c.ethIdx()
	// Let the start-up head-follow pass finish so block-read counts below
	// are not racing it.
	deadline := time.Now().Add(10 * time.Second)
	for ei.follows.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if backfill {
		ei.backfillOnce(ctx)
	}
	return ei
}

func wantGlif(t *testing.T, glif map[string]json.RawMessage, key string, got any) {
	t.Helper()
	var want any
	if err := json.Unmarshal(glif[key], &want); err != nil {
		t.Fatal(err)
	}
	// Recorded Glif answers came from a backend that renders an empty
	// accessList as []; Lotus v1.38.3 and Lantern render null on eth txs.
	if m, ok := want.(map[string]any); ok {
		if al, ok := m["accessList"].([]any); ok && len(al) == 0 {
			m["accessList"] = nil
		}
	}
	if d := diffEthJSON(key, want, normJSON(t, got)); len(d) > 0 {
		t.Errorf("%s: %d field diffs vs Lotus:\n  %s", key, len(d), strings.Join(d, "\n  "))
	}
}

func TestEthTxIndex_ReceiptFromIndexMatchesLotus(t *testing.T) {
	env := setupEthIdx(t)
	c := newIdxChainAPI(t, env.hs, env.getter, env.head)
	ei := startIndexed(t, c, true)
	if env.save != nil {
		defer env.save()
	}
	head := c.HeaderStore.Head()
	if !ei.ix.Covered(ei.low(head), head.Height()-1) {
		t.Fatalf("window not covered after backfill: stats %+v", c.EthTxIndexStats())
	}
	h, _ := parseEthHash(ethRcptTx1)
	if _, _, ok, err := ei.ix.Lookup(h); err != nil || !ok {
		t.Fatalf("tx1 not in index (ok=%v err=%v)", ok, err)
	}
	ctx := context.Background()
	got, err := c.EthGetTransactionReceipt(ctx, ethRcptTx1)
	if err != nil || got == nil {
		t.Fatalf("receipt: %v %v", got, err)
	}
	wantGlif(t, env.glif, "receipt1", got)
	tx, err := c.EthGetTransactionByHash(ctx, ethRcptTx1)
	if err != nil || tx == nil {
		t.Fatalf("by hash: %v %v", tx, err)
	}
	wantGlif(t, env.glif, "tx1", tx)
}

// The F12 scenario: the index (and our sent-tx record) survive a restart;
// the first lookup after it is served from the index without indexing or
// walking anything.
func TestEthTxIndex_SurvivesRestart(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_TXINDEX_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	env := setupEthIdx(t)
	c1 := newIdxChainAPI(t, env.hs, env.getter, env.head)
	startIndexed(t, c1, true)

	// Record tx1 as published by us, with its raw signed envelope.
	h1, _ := parseEthHash(ethRcptTx1)
	loc, _, ok, _ := c1.ethIdx().ix.Lookup(h1)
	if !ok {
		t.Fatal("tx1 not indexed")
	}
	sm, err := c1.loadChainMsg(context.Background(), env.getter, loc.MsgCID, loc.IsSecp)
	if err != nil {
		t.Fatal(err)
	}
	etx, from, err := ethTxFromSignedMessage(sm, 314159)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := etx.EncodeSigned()
	if err != nil {
		t.Fatal(err)
	}
	c1.persistSentTx(ethRcptTx1, sentTxRecord{msgCID: loc.MsgCID, tx: etx, from: from, sentEpoch: 4189900}, raw)

	// "Restart": a fresh ChainAPI (empty in-memory maps and caches) on the
	// same header store, no backfill, a getter that counts fetches.
	cg := &countingGetter{inner: env.getter}
	c2 := newIdxChainAPI(t, env.hs, cg, env.head)
	ei2 := startIndexed(t, c2, false)
	cg.n.Store(0)
	if ei2.ix.Count() == 0 {
		t.Fatal("index heights not reloaded")
	}
	rec, ok := c2.sentRecord(ethRcptTx1)
	if !ok || rec.msgCID != loc.MsgCID || rec.sentEpoch != 4189900 || rec.tx == nil {
		t.Fatalf("sent record not restored: ok=%v rec=%+v", ok, rec)
	}
	if th, _ := rec.tx.TxHash(); "0x"+strings.ToLower(hexOf(th[:])) != ethRcptTx1 {
		t.Fatalf("restored tx hash mismatch")
	}
	got, err := c2.EthGetTransactionReceipt(context.Background(), ethRcptTx1)
	if err != nil || got == nil {
		t.Fatalf("receipt after restart: %v %v", got, err)
	}
	wantGlif(t, env.glif, "receipt1", got)
	if ei2.indexed.Load() != 0 {
		t.Fatalf("lookup triggered indexing")
	}
	// Render needs only the msg body, the tx's events and the two
	// tipsets' headers that are not in the header store: a handful.
	if n := cg.n.Load(); n > 16 {
		t.Fatalf("receipt after restart fetched %d blocks; want a handful", n)
	}
	t.Logf("receipt after restart: %d block reads", cg.n.Load())
}

// A hash that is not on chain, inside a fully indexed window, is null with
// zero block reads (Curio polls pending txs every few seconds).
func TestEthTxIndex_NotFoundIsFastNull(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_TXINDEX_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	env := setupEthIdx(t)
	cg := &countingGetter{inner: env.getter}
	c := newIdxChainAPI(t, env.hs, cg, env.head)
	startIndexed(t, c, true)
	before := cg.n.Load()
	const ghost = "0x3333333333333333333333333333333333333333333333333333333333333333"
	got, err := c.EthGetTransactionReceipt(context.Background(), ghost)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
	got, err = c.EthGetTransactionByHash(context.Background(), ghost)
	if err != nil || got != nil {
		t.Fatalf("by hash: want (nil, nil), got (%v, %v)", got, err)
	}
	if n := cg.n.Load() - before; n != 0 {
		t.Fatalf("not-found lookup fetched %d blocks; want 0", n)
	}
	// tx2 (4189895) is below the 2-epoch window: also an immediate null
	// bridge-off, never a chain walk.
	got, err = c.EthGetTransactionReceipt(context.Background(), ethRcptTx2)
	if err != nil || got != nil {
		t.Fatalf("outside window: want (nil, nil), got (%v, %v)", got, err)
	}
	if n := cg.n.Load() - before; n != 0 {
		t.Fatalf("outside-window lookup fetched %d blocks; want 0", n)
	}
}

// Before the window is covered (fresh index, backfill pending) a miss is
// still answered immediately, never by a chain walk.
func TestEthTxIndex_UncoveredMissIsNullNotWalk(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_TXINDEX_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	env := setupEthIdx(t)
	cg := &countingGetter{inner: env.getter}
	c := newIdxChainAPI(t, env.hs, cg, env.head)
	cg.fail.Store(true) // the indexer cannot fetch anything: window stays uncovered
	startIndexed(t, c, false)
	if st := c.EthTxIndexStats(); st.Covered || st.Heights != 0 {
		t.Fatalf("expected an empty, uncovered index: %+v", st)
	}
	before := cg.n.Load()
	got, err := c.EthGetTransactionReceipt(context.Background(), ethRcptTx1)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) while uncovered, got (%v, %v)", got != nil, err)
	}
	if n := cg.n.Load() - before; n != 0 {
		t.Fatalf("uncovered miss fetched %d blocks; want 0", n)
	}
}

// Latency targets: p99 < 500 ms indexed (with a simulated 20 ms per cold
// block read), < 100 ms not-found.
func TestEthTxIndex_LatencyTargets(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_TXINDEX_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	env := setupEthIdx(t)
	c0 := newIdxChainAPI(t, env.hs, env.getter, env.head)
	startIndexed(t, c0, true)

	measure := func(n int, fn func() error) []time.Duration {
		out := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			s := time.Now()
			if err := fn(); err != nil {
				t.Fatal(err)
			}
			out = append(out, time.Since(s))
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	p99 := func(d []time.Duration) time.Duration { return d[(len(d)*99)/100-1] }

	// Cold every time: a fresh ChainAPI per lookup (empty in-memory caches)
	// over a getter that sleeps 20 ms per block read.
	slow := &countingGetter{inner: env.getter, delay: 20 * time.Millisecond}
	hit := measure(50, func() error {
		c := newIdxChainAPI(t, env.hs, slow, env.head)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := c.StartEthTxIndex(ctx, EthTxIndexOptions{Window: ethIdxWindow, NoBackfill: true}); err != nil {
			return err
		}
		r, err := c.EthGetTransactionReceipt(context.Background(), ethRcptTx1)
		if err == nil && r == nil {
			t.Fatal("nil receipt")
		}
		return err
	})
	miss := measure(500, func() error {
		_, err := c0.EthGetTransactionReceipt(context.Background(), "0x4444444444444444444444444444444444444444444444444444444444444444")
		return err
	})
	t.Logf("indexed receipt (cold, 20ms/block): p50=%v p99=%v max=%v", hit[len(hit)/2], p99(hit), hit[len(hit)-1])
	t.Logf("not-found: p50=%v p99=%v max=%v", miss[len(miss)/2], p99(miss), miss[len(miss)-1])
	if p99(hit) > 500*time.Millisecond {
		t.Errorf("indexed p99 %v > 500ms", p99(hit))
	}
	if p99(miss) > 100*time.Millisecond {
		t.Errorf("not-found p99 %v > 100ms", p99(miss))
	}
}

func BenchmarkEthTxReceipt_Indexed(b *testing.B) {
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer hs.Close()
	all, base := fixtureBlocks(b, ethRcptFixture, ethIdxFixture)
	c := newIdxChainAPI(b, hs, all, base.Head)
	startIndexed(b, c, true)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r, err := c.EthGetTransactionReceipt(ctx, ethRcptTx1); err != nil || r == nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEthTxReceipt_NotFound(b *testing.B) {
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer hs.Close()
	all, base := fixtureBlocks(b, ethRcptFixture, ethIdxFixture)
	c := newIdxChainAPI(b, hs, all, base.Head)
	startIndexed(b, c, true)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r, err := c.EthGetTransactionReceipt(ctx, "0x5555555555555555555555555555555555555555555555555555555555555555"); err != nil || r != nil {
			b.Fatal(err)
		}
	}
}

// Legacy (no index) cold lookup of tx2 (10 epochs back, in the send
// index), for comparison: every iteration starts with empty caches.
func BenchmarkEthTxReceipt_LegacyWalkCold(b *testing.B) {
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer hs.Close()
	all, base := fixtureBlocks(b, ethRcptFixture)
	mc, _ := cid.Parse(ethRcptTx2MsgCID)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := newIdxChainAPI(b, hs, all, base.Head)
		c.sentTx().put(ethRcptTx2, sentTxRecord{msgCID: mc})
		if r, err := c.EthGetTransactionReceipt(ctx, ethRcptTx2); err != nil || r == nil {
			b.Fatal(err)
		}
	}
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i], out[2*i+1] = d[v>>4], d[v&0xf]
	}
	return string(out)
}
