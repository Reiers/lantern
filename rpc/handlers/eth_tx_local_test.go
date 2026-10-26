package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// Real calibration fixture for the bridge-off receipt path (stock
// Curio-PDP on a calibration test node): two txs Curio sent through Lantern.
//
//	tx1 0xfd05...8865  createDataSet  incl 4189904 (3-block tipset), 10 logs
//	tx2 0xb3dc...9903  registerProvider incl 4189895 (2-block tipset; node2's
//	                   header store only had 1 of the 2 blocks)
//
// The header store holds ONLY a partial head tipset at 4189905 (1 of its 4
// blocks), exactly the shape that broke the old StateSearchMsg path: every
// inclusion tipset must be rebuilt from parent links. Expected values are
// the Glif (Lotus) responses captured at record time.
//
// Re-record (network): LANTERN_RECORD_ETH_RECEIPT_FIXTURE=1 go test -run TestEthTxLocal ./rpc/handlers
const (
	ethRcptFixture   = "testdata/eth_receipt_calib_4189904.json.gz"
	ethRcptTx1       = "0xfd05fb0fda451a69eb1ef47378924b7c4b44a3d8faf54478716f019f0ac28865"
	ethRcptTx2       = "0xb3dcd20214bac8706dbb3740e4f23a698ddae5e20fd3f485ff899f5f696d9903"
	ethRcptTx2MsgCID = "bafy2bzacedizrb4utbz66xdmsnf6vcjcxnuuh75mmu7dkkhhkcwi7gcgm6rty"
	ethRcptHeadBlock = "bafy2bzacecqmyp2rsdn33myukcaf2o5kwm56c6uyml3bjdywkm4z6nbbptcei" // 1 of 4 blocks @4189905
)

type ethRcptFixtureFile struct {
	Head   []string                   `json:"head"`
	Blocks map[string]string          `json:"blocks"`
	Glif   map[string]json.RawMessage `json:"glif"`
}

type recordingGetter struct {
	inner interface {
		Get(context.Context, cid.Cid) ([]byte, error)
	}
	mu  sync.Mutex
	got map[string][]byte
}

func (r *recordingGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	b, err := r.inner.Get(ctx, c)
	if err == nil {
		r.mu.Lock()
		r.got[c.String()] = b
		r.mu.Unlock()
	}
	return b, err
}

type mapGetter map[string][]byte

func (m mapGetter) Get(_ context.Context, c cid.Cid) ([]byte, error) {
	if b, ok := m[c.String()]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("fixture: block %s not recorded", c)
}

func setupEthRcpt(t *testing.T) (*ChainAPI, map[string]json.RawMessage, func() *ethRcptFixtureFile) {
	t.Helper()
	record := os.Getenv("LANTERN_RECORD_ETH_RECEIPT_FIXTURE") == "1"
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
		getter = rg
		glif = map[string]json.RawMessage{}
		ctx := context.Background()
		for k, call := range map[string][]any{
			"receipt1": {"eth_getTransactionReceipt", ethRcptTx1},
			"receipt2": {"eth_getTransactionReceipt", ethRcptTx2},
			"tx1":      {"eth_getTransactionByHash", ethRcptTx1},
			"tx2":      {"eth_getTransactionByHash", ethRcptTx2},
		} {
			res, err := g.rpc(ctx, call[0].(string), []any{call[1]})
			if err != nil {
				t.Fatalf("glif %s: %v", k, err)
			}
			glif[k] = res
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
		raw, err := os.ReadFile(ethRcptFixture)
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
	c := &ChainAPI{NetworkName: "calibration", HeaderStore: hs, BlockGetter: getter, EthTxLookback: 12}
	return c, glif, dump
}

// diffEthJSON compares two JSON values field by field and returns
// human-readable differences (empty == identical).
func diffEthJSON(prefix string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object, got %T", prefix, got)}
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var out []string
		for _, k := range ks {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !wok:
				out = append(out, fmt.Sprintf("%s.%s: extra field (got %v)", prefix, k, gv))
			case !gok:
				out = append(out, fmt.Sprintf("%s.%s: missing (want %v)", prefix, k, wv))
			default:
				out = append(out, diffEthJSON(prefix+"."+k, wv, gv)...)
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %d items, got %v", prefix, len(w), lenOf(got))}
		}
		var out []string
		for i := range w {
			out = append(out, diffEthJSON(fmt.Sprintf("%s[%d]", prefix, i), w[i], g[i])...)
		}
		return out
	default:
		if ws, ok := want.(string); ok {
			if gs, ok := got.(string); ok && strings.EqualFold(ws, gs) {
				return nil
			}
		}
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: want %v, got %v", prefix, want, got)}
		}
		return nil
	}
}

func lenOf(v any) string {
	if a, ok := v.([]any); ok {
		return fmt.Sprint(len(a))
	}
	return fmt.Sprintf("%T", v)
}

func normJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEthTxLocal_ReceiptsMatchLotus(t *testing.T) {
	c, glif, dump := setupEthRcpt(t)
	ctx := context.Background()

	// tx2 is in our send index (published through Lantern).
	mc, _ := cid.Parse(ethRcptTx2MsgCID)
	c.sentTx().put(ethRcptTx2, sentTxRecord{msgCID: mc})

	cases := []struct {
		name, hash, glifKey string
		receipt             bool
	}{
		{"receipt tx1 (not in send index: eth-hash scan)", ethRcptTx1, "receipt1", true},
		{"receipt tx2 (send index, partial store tipset)", ethRcptTx2, "receipt2", true},
		{"tx1 by hash", ethRcptTx1, "tx1", false},
		{"tx2 by hash", ethRcptTx2, "tx2", false},
	}
	for _, tc := range cases {
		var got any
		var err error
		if tc.receipt {
			got, err = c.EthGetTransactionReceipt(ctx, tc.hash)
		} else {
			got, err = c.EthGetTransactionByHash(ctx, tc.hash)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got == nil {
			t.Fatalf("%s: nil result", tc.name)
		}
		var want any
		if err := json.Unmarshal(glif[tc.glifKey], &want); err != nil {
			t.Fatal(err)
		}
		// Glif may have served Forest; compare against the Lotus rendering.
		want = asLotusShape(want)
		if d := diffEthJSON(tc.glifKey, want, normJSON(t, got)); len(d) > 0 {
			t.Errorf("%s: %d field diffs vs Lotus:\n  %s", tc.name, len(d), strings.Join(d, "\n  "))
		}
	}

	// A tx we published that is not on chain yet: null (pending), served
	// locally, no error, no bridge.
	ghost, _ := cid.Parse("bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	const ghostHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	c.sentTx().put(ghostHash, sentTxRecord{msgCID: ghost, sentEpoch: c.HeaderStore.HeadEpoch() + 10})
	got, err := c.EthGetTransactionReceipt(ctx, ghostHash)
	if err != nil || got != nil {
		t.Fatalf("pending tx: want (nil, nil), got (%v, %v)", got, err)
	}

	if dump != nil {
		f := dump()
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(f); err != nil {
			t.Fatal(err)
		}
		_ = zw.Close()
		if err := os.MkdirAll(filepath.Dir(ethRcptFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ethRcptFixture, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d blocks (%d bytes gz)", len(f.Blocks), buf.Len())
	}
}

// An unknown hash with no bridge returns null like Lotus (not
// errBridgeUnconfigured).
func TestEthTxLocal_UnknownNoBridgeIsNull(t *testing.T) {
	if os.Getenv("LANTERN_RECORD_ETH_RECEIPT_FIXTURE") == "1" {
		t.Skip("record mode")
	}
	c, _, _ := setupEthRcpt(t)
	c.EthTxLookback = 1 // only the head's parent tipset (fully recorded)
	got, err := c.EthGetTransactionReceipt(context.Background(),
		"0x2222222222222222222222222222222222222222222222222222222222222222")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}
