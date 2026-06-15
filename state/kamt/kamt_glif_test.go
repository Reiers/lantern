package kamt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
)

// glifBlockGetter is a hamt.BlockGetter that fetches IPLD blocks live via
// Filecoin.ChainReadObj from a Lotus-compatible RPC (Glif by default).
// Used only by the network-gated KAMT verify gate.
type glifBlockGetter struct {
	url string
	hc  *http.Client
}

func (g *glifBlockGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "Filecoin.ChainReadObj",
		"params": []any{map[string]string{"/": c.String()}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", g.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("ChainReadObj %s: %s", c, out.Error.Message)
	}
	return base64.StdEncoding.DecodeString(out.Result)
}

// TestKAMT_RegistryStorage_GlifParity is the Stage-2 verify gate
// (lantern#43 Part B): walk the live calibration ServiceProviderRegistry
// storage KAMT and confirm slot reads are byte-identical to Glif's
// eth_getStorageAt.
//
// Network-gated: set LANTERN_KAMT_GLIF_TEST=1 to run (it dials Glif
// calibration). Skipped by default so CI stays offline.
//
// Captured 2026-06-15: ServiceProviderRegistry contract-storage KAMT root
// (from StateReadState .State.ContractState). eth_getStorageAt on the
// contract reports slot 0 == 0x1c, slots 1..6 == 0.
func TestKAMT_RegistryStorage_GlifParity(t *testing.T) {
	if os.Getenv("LANTERN_KAMT_GLIF_TEST") != "1" {
		t.Skip("set LANTERN_KAMT_GLIF_TEST=1 to run the Glif-backed KAMT parity gate")
	}
	const storageRoot = "bafy2bzaceddhysudhxm5amuz3v4d7wmmgtc5bsacolirsyi7ccagvpgukeh7i"
	root, err := cid.Parse(storageRoot)
	if err != nil {
		t.Fatalf("parse storage root: %v", err)
	}

	bg := &glifBlockGetter{
		url: "https://api.calibration.node.glif.io/rpc/v1",
		hc:  &http.Client{Timeout: 20 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Expected values from eth_getStorageAt (latest). Low slots probe the
	// scalar storage region (slot 0 holds a counter == 0x1c); the high slot
	// is a real Solidity-mapping entry keccak256(abi.encode(key,baseSlot))
	// whose 32-byte key exercises a full-depth, non-trivial KAMT path
	// (extensions + deep descent), returning an address-shaped value.
	want := map[string]string{
		"0": "0x1c",
		"1": "0x0",
		"2": "0x0",
		"3": "0x0",
		"4": "0x0",
		"5": "0x0",
		"6": "0x0",
		// mapping[1] @ baseSlot 1: keccak256(0x..01 || 0x..01)
		"0xcc69885fda6bcc1a4ace058b4a62bf5e179ea78fd58a1ccd71c22cc9b688792f": "0x8c8c7a9be47ed491b33b941fbc0276bd2ec25e7e",
	}
	for slotStr, exp := range want {
		slot, ok := new(big.Int).SetString(strip0x(slotStr), base(slotStr))
		if !ok {
			t.Fatalf("bad slot literal %q", slotStr)
		}
		got, _, err := GetU256(ctx, root, slot, bg)
		if err != nil {
			t.Fatalf("slot %s: GetU256: %v", slotStr, err)
		}
		expBI, _ := new(big.Int).SetString(exp[2:], 16)
		if got.Cmp(expBI) != 0 {
			t.Errorf("slot %s: got 0x%x, want %s", slotStr, got, exp)
		} else {
			t.Logf("slot %s == 0x%x  ✓ (matches Glif eth_getStorageAt)", slotStr, got)
		}
	}
}

func strip0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

func base(s string) int {
	if len(s) >= 2 && s[:2] == "0x" {
		return 16
	}
	return 10
}
