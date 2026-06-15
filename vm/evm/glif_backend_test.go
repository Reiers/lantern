package evm_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/kamt"
	"github.com/Reiers/lantern/vm/evm"
)

// glifBackend wires the Stage-1 EVM actor loader + Stage-2 KAMT reader to
// the evm.Backend interface, fetching everything from a Lotus-compatible
// RPC. This is the Stage-3 verify harness: it lets the pure-Go interpreter
// run a real view call against live contract state and be compared to
// Glif's own eth_call.
type glifBackend struct {
	url string
	hc  *http.Client
	reg *actors.Registry
}

func (g *glifBackend) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
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

// blockGetter for the state packages.
func (g *glifBackend) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
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

// loadActor resolves a 0x eth address to its (code, head) CIDs via Glif.
func (g *glifBackend) loadActor(ctx context.Context, a evm.Address) (cid.Cid, cid.Cid, error) {
	ethHex := "0x" + hex.EncodeToString(a[:])
	res, err := g.rpc(ctx, "Filecoin.EthAddressToFilecoinAddress", []any{ethHex})
	if err != nil {
		return cid.Undef, cid.Undef, err
	}
	var filAddr string
	_ = json.Unmarshal(res, &filAddr)

	res, err = g.rpc(ctx, "Filecoin.StateGetActor", []any{filAddr, nil})
	if err != nil {
		return cid.Undef, cid.Undef, err
	}
	var act struct {
		Code struct {
			Slash string `json:"/"`
		} `json:"Code"`
		Head struct {
			Slash string `json:"/"`
		} `json:"Head"`
	}
	if err := json.Unmarshal(res, &act); err != nil {
		return cid.Undef, cid.Undef, err
	}
	code, err := cid.Parse(act.Code.Slash)
	if err != nil {
		return cid.Undef, cid.Undef, err
	}
	head, err := cid.Parse(act.Head.Slash)
	if err != nil {
		return cid.Undef, cid.Undef, err
	}
	return code, head, nil
}

func (g *glifBackend) GetCode(a evm.Address) ([]byte, error) {
	ctx := context.Background()
	code, head, err := g.loadActor(ctx, a)
	if err != nil {
		return nil, err
	}
	st, err := actors.LoadEVM(ctx, code, head, g, g.reg)
	if err != nil {
		// not an EVM actor (or unknown) -> no code
		return nil, nil
	}
	return actors.FetchBytecode(ctx, st, g)
}

func (g *glifBackend) GetStorage(a evm.Address, key uint256.Int) (uint256.Int, error) {
	ctx := context.Background()
	code, head, err := g.loadActor(ctx, a)
	if err != nil {
		return uint256.Int{}, err
	}
	st, err := actors.LoadEVM(ctx, code, head, g, g.reg)
	if err != nil {
		return uint256.Int{}, nil // non-contract -> zero storage
	}
	slot := key.ToBig()
	v, _, err := kamt.GetU256(ctx, st.StorageRoot(), slot, g)
	if err != nil {
		return uint256.Int{}, err
	}
	var out uint256.Int
	out.SetFromBig(v)
	return out, nil
}

func (g *glifBackend) GetBalance(a evm.Address) (uint256.Int, error) { return uint256.Int{}, nil }
func (g *glifBackend) BlockNumber() uint64                           { return 0 }
func (g *glifBackend) Timestamp() uint64                             { return 0 }
func (g *glifBackend) ChainID() uint64                               { return 314159 }

// TestEVMCall_RegistryGlifParity is the Stage-3 verify gate (lantern#43
// Part B): run a real ServiceProviderRegistry view call through the pure-Go
// interpreter and compare the return bytes to Glif eth_call.
//
// Network-gated: LANTERN_EVM_GLIF_TEST=1.
func TestEVMCall_RegistryGlifParity(t *testing.T) {
	if os.Getenv("LANTERN_EVM_GLIF_TEST") != "1" {
		t.Skip("set LANTERN_EVM_GLIF_TEST=1 to run the Glif-backed EVM parity gate")
	}
	be := &glifBackend{
		url: "https://api.calibration.node.glif.io/rpc/v1",
		hc:  &http.Client{Timeout: 25 * time.Second},
		reg: actors.DefaultRegistry(),
	}
	reg := mustAddr(t, "839e5c9988e4e9977d40708d0094103c0839Ac9D")
	caller := evm.Address{} // address(0)

	cases := []struct {
		name     string
		selector string // 4-byte hex, no 0x
		wantHex  string // expected eth_call result (no 0x)
	}{
		// activeProviderCount() == 0x1a (26) per Glif eth_call.
		{"activeProviderCount", "f08bbda0", "000000000000000000000000000000000000000000000000000000000000001a"},
		// owner() -> an address (SLOAD of an address-typed slot).
		{"owner", "8da5cb5b", "0000000000000000000000006386622b4915b027900d65560b0ab84f8a1ff2aa"},
		// getNextProviderId() == 0x1d (29).
		{"getNextProviderId", "d1329d4e", "000000000000000000000000000000000000000000000000000000000000001d"},
	}
	for _, tc := range cases {
		input, _ := hex.DecodeString(tc.selector)
		res, err := evm.Call(be, caller, reg, input)
		if err != nil {
			t.Fatalf("%s: Call error: %v", tc.name, err)
		}
		if res.Reverted {
			t.Fatalf("%s: unexpectedly reverted, data=%x", tc.name, res.Return)
		}
		got := hex.EncodeToString(res.Return)
		if got != tc.wantHex {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.wantHex)
		} else {
			t.Logf("%s == 0x%s  ✓ (matches Glif eth_call)", tc.name, got)
		}
	}
}

func mustAddr(t *testing.T, h string) evm.Address {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad addr hex: %v", err)
	}
	return evm.BytesToAddress(b)
}

var _ = big.NewInt
