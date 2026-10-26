package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/state/accessor"
)

// hbGlif is a hamt.BlockGetter + chain-head source backed by a
// Lotus-compatible RPC, for the Stage-4 integration verify gate only.
type hbGlif struct {
	url string
	hc  *http.Client
}

func (g *hbGlif) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, "POST", g.url, bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
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
		return nil, fmt.Errorf("%s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

func (g *hbGlif) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	res, err := g.rpc(ctx, "Filecoin.ChainReadObj", []any{map[string]string{"/": c.String()}})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(res, &s); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (g *hbGlif) head(ctx context.Context) (int64, cid.Cid, error) {
	res, err := g.rpc(ctx, "Filecoin.ChainHead", nil)
	if err != nil {
		return 0, cid.Undef, err
	}
	var h struct {
		Height int64 `json:"Height"`
		Blocks []struct {
			ParentStateRoot struct {
				Slash string `json:"/"`
			} `json:"ParentStateRoot"`
		} `json:"Blocks"`
	}
	if err := json.Unmarshal(res, &h); err != nil {
		return 0, cid.Undef, err
	}
	sr, err := cid.Parse(h.Blocks[0].ParentStateRoot.Slash)
	return h.Height, sr, err
}

// TestEthCall_LocalIntegration is the Stage-4 verify gate (lantern#43
// Part B): exercise the full ChainAPI.EthCall path (call-object decode ->
// local FEVM exec against the verified accessor -> ABI return) with NO
// VMBridge configured, and confirm it matches Glif eth_call. Because
// c.Bridge is nil, a correct result PROVES the answer came from local
// execution, not a fallback.
//
// Network-gated: LANTERN_EVM_GLIF_TEST=1.
func TestEthCall_LocalIntegration(t *testing.T) {
	if os.Getenv("LANTERN_EVM_GLIF_TEST") != "1" {
		t.Skip("set LANTERN_EVM_GLIF_TEST=1 to run the local eth_call integration gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bg := &hbGlif{url: "https://api.calibration.node.glif.io/rpc/v1", hc: &http.Client{Timeout: 25 * time.Second}}
	epoch, stateRoot, err := bg.head(ctx)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	tr := &trustedroot.TrustedRoot{Epoch: abi.ChainEpoch(epoch), StateRoot: stateRoot}
	acc := accessor.New(tr, bg)

	c := &ChainAPI{
		Trusted:     tr,
		BlockGetter: bg,
		Accessor:    acc,
		NetworkName: "calibrationnet",
		Bridge:      nil, // no fallback: a correct result is necessarily local
	}

	const reg = "0x839e5c9988e4e9977d40708d0094103c0839Ac9D"
	cases := []struct {
		name, data, want string
	}{
		{"activeProviderCount", "0xf08bbda0", "0x000000000000000000000000000000000000000000000000000000000000001a"},
		{"getNextProviderId", "0xd1329d4e", "0x000000000000000000000000000000000000000000000000000000000000001d"},
		{"owner", "0x8da5cb5b", "0x0000000000000000000000006386622b4915b027900d65560b0ab84f8a1ff2aa"},
	}
	for _, tc := range cases {
		got, err := c.EthCall(ctx, map[string]any{"to": reg, "data": tc.data}, "latest")
		if err != nil {
			t.Fatalf("%s: EthCall: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		} else {
			t.Logf("%s == %s  ✓ (local FEVM exec, no bridge, matches Glif)", tc.name, got)
		}
	}
}

// TestEthCall_LocalCancunOpcodes replays the two eth_call shapes that
// missed locally in prod on lantern-m2 with "unsupported opcode 0x5e"
// (MCOPY) and checks local FEVM exec now matches Glif eth_call at the
// same state. Network-gated: LANTERN_EVM_GLIF_TEST=1.
func TestEthCall_LocalCancunOpcodes(t *testing.T) {
	if os.Getenv("LANTERN_EVM_GLIF_TEST") != "1" {
		t.Skip("set LANTERN_EVM_GLIF_TEST=1 to run the local eth_call integration gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	bg := &hbGlif{url: "https://api.calibration.node.glif.io/rpc/v1", hc: &http.Client{Timeout: 25 * time.Second}}
	epoch, stateRoot, err := bg.head(ctx)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	tr := &trustedroot.TrustedRoot{Epoch: abi.ChainEpoch(epoch), StateRoot: stateRoot}
	c := &ChainAPI{Trusted: tr, BlockGetter: bg, Accessor: accessor.New(tr, bg), NetworkName: "calibrationnet"}
	// Glif's eth_call at the tipset whose parent state we execute against.
	blk := fmt.Sprintf("0x%x", epoch-1)
	const curio = "000000000000000000000000d44e13e90f36ad26b5e2b7922c5100e673ee4bee"
	cases := []struct{ name, to, data string }{
		{"PDPVerifier VERSION()", "0x85e366Cf9DD2c0aE37E963d9556F5f4718d6417C", "0xffa1ad74"},
		{"SPRegistry 0x2335bde0(curio)", "0x839e5c9988e4e9977d40708d0094103c0839Ac9D", "0x2335bde0" + curio},
	}
	for _, tc := range cases {
		got, err := c.EthCall(ctx, map[string]any{"to": tc.to, "data": tc.data}, "latest")
		if err != nil {
			t.Fatalf("%s: local EthCall: %v", tc.name, err)
		}
		raw, err := bg.rpc(ctx, "eth_call", []any{map[string]any{"to": tc.to, "data": tc.data}, blk})
		if err != nil {
			t.Fatalf("%s: glif eth_call: %v", tc.name, err)
		}
		var want string
		_ = json.Unmarshal(raw, &want)
		if got != want {
			t.Errorf("%s: local %s != glif %s", tc.name, got, want)
		} else {
			t.Logf("%s: local == glif (%d bytes)", tc.name, (len(got)-2)/2)
		}
	}
}
