// lantern-phase2 is the Phase 2 end-to-end integration runner.
//
// It:
//
//  1. Talks to gateway.lantern.reiers.io for the current finalized head
//     (epoch + tipsetKey + stateRoot).
//  2. Builds a (header-only) TrustedRoot (see PHASE2-BLOCKERS.md B9 for
//     why F3 cert chain validation isn't fully active yet).
//  3. Uses state/accessor to walk the state tree to:
//       - f01    (Init actor singleton)
//       - f05    (StorageMarket actor singleton)
//       - f01000 (well-known miner that always exists)
//  4. Cross-checks each result against Glif's StateGetActor at the same
//     tipset key (Code, Head, Balance).
//  5. Prints fetch-source attribution (cache vs gateway) and proof path
//     sizes per query.
//  6. Exits 0 on all-match, 1 on any mismatch.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	addr "github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/hsync"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/hamt"
)

const (
	defaultGateway = "https://gateway.lantern.reiers.io"
	defaultGlif    = "https://api.node.glif.io/rpc/v1"
)

type cliFlags struct {
	gatewayURL string
	gatewayIP  string
	glifURL    string
}

func main() {
	var f cliFlags
	flag.StringVar(&f.gatewayURL, "gateway", defaultGateway, "Lantern gateway base URL")
	flag.StringVar(&f.gatewayIP, "gateway-ip", "", "Force-resolve gateway hostname to this IP (workaround for stale DNS)")
	flag.StringVar(&f.glifURL, "glif", defaultGlif, "Public Filecoin RPC for cross-check (Glif default)")
	flag.Parse()

	if err := run(context.Background(), f); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println()
	fmt.Println("OK")
}

func run(ctx context.Context, f cliFlags) error {
	fmt.Println("Lantern Phase 2 — end-to-end light-client demo")
	fmt.Println("=============================================")

	gatewayClient := newGatewayClient(f)

	// 1. Probe the gateway for current head.
	fmt.Printf("Probing gateway %s for /state/root...\n", f.gatewayURL)
	head, err := gatewayStateRoot(ctx, gatewayClient, f.gatewayURL)
	if err != nil {
		return fmt.Errorf("gateway /state/root: %w", err)
	}
	fmt.Printf("  epoch:        %d\n", head.Epoch)
	fmt.Printf("  state root:   %s\n", head.StateRoot)
	fmt.Printf("  tipset key:   %d blocks\n", len(head.TipsetKey))

	stateRoot, err := cid.Parse(head.StateRoot)
	if err != nil {
		return fmt.Errorf("parse stateRoot CID: %w", err)
	}

	// 2. Build a (header-only) TrustedRoot.
	tr := &trustedroot.TrustedRoot{
		Epoch:     abi.ChainEpoch(head.Epoch),
		StateRoot: stateRoot,
	}
	fmt.Println()
	fmt.Println("TrustedRoot built (header-only; F3 cert verification deferred per B9).")

	// 3. Wire a combined fetcher: cache -> bitswap stub -> HTTP gateway.
	cache := hamt.NewMemBlockStore()
	httpClient := hsync.NewClient([]string{f.gatewayURL}, 20*time.Second)
	httpClient.SetHTTPClient(gatewayClient)

	fetcher := combined.New(cache,
		combined.Source{Name: "bitswap", Getter: bitswap.Stub{}, Timeout: 100 * time.Millisecond},
		combined.Source{Name: "gateway", Getter: httpClient, Timeout: 15 * time.Second},
	)
	acc := accessor.New(tr, fetcher)

	// 4. Run lookups.
	targets := []struct {
		name string
		a    addr.Address
	}{
		{"f01 (Init actor)", mustAddr(1)},
		{"f05 (StorageMarket)", mustAddr(5)},
		{"f01000 (miner)", mustAddr(1000)},
	}

	fmt.Println()
	fmt.Println("Walks (each prints fetch source counts + proof path size)")
	fmt.Println("---------------------------------------------------------")

	tskParams := tipsetKeyParam(head.TipsetKey)

	allOK := true
	for _, t := range targets {
		ours, proof, err := acc.GetActorByID(ctx, t.a)
		if err != nil {
			fmt.Printf("  %s: FAIL: %v\n", t.name, err)
			allOK = false
			continue
		}

		ref, err := glifGetActor(ctx, f.glifURL, t.a, tskParams)
		if err != nil {
			fmt.Printf("  %s: glif cross-check FAIL: %v\n", t.name, err)
			allOK = false
			continue
		}

		match := true
		mismatchDetail := ""
		if !ours.Code.Equals(ref.Code) {
			match = false
			mismatchDetail += fmt.Sprintf("\n      code: ours=%s ref=%s", ours.Code, ref.Code)
		}
		if !ours.Head.Equals(ref.Head) {
			match = false
			mismatchDetail += fmt.Sprintf("\n      head: ours=%s ref=%s", ours.Head, ref.Head)
		}
		if ours.Balance.String() != ref.Balance {
			match = false
			mismatchDetail += fmt.Sprintf("\n      balance: ours=%s ref=%s", ours.Balance, ref.Balance)
		}

		status := "MATCH"
		if !match {
			status = "MISMATCH" + mismatchDetail
			allOK = false
		}

		stats := fetcher.Stats()
		fmt.Printf("  %s\n", t.name)
		fmt.Printf("    code:    %s\n", ours.Code)
		fmt.Printf("    head:    %s\n", ours.Head)
		fmt.Printf("    balance: %s\n", ours.Balance)
		fmt.Printf("    proof path: %d CIDs traversed\n", len(proof))
		fmt.Printf("    fetcher stats: cache=%d gateway=%d bitswap=%d misses=%d\n",
			stats["cache"], stats["gateway"], stats["bitswap"], stats["misses"])
		fmt.Printf("    vs Glif (@tipset): %s\n", status)
	}

	fmt.Println()
	fmt.Printf("Final fetcher stats: %+v\n", fetcher.Stats())

	if !allOK {
		return fmt.Errorf("one or more actor lookups did not match Glif")
	}
	return nil
}

// ---------------------------------------------------------------------

func mustAddr(id uint64) addr.Address {
	a, err := addr.NewIDAddress(id)
	if err != nil {
		panic(err)
	}
	return a
}

func newGatewayClient(f cliFlags) *http.Client {
	tr := &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if f.gatewayIP != "" {
		// Override host resolution. Used as a workaround when the local DNS
		// resolver has a stale entry from before the DNS migration.
		override := f.gatewayIP
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			// `address` is "host:port". Always dial the override IP, keep port.
			parts := strings.SplitN(address, ":", 2)
			port := "443"
			if len(parts) == 2 {
				port = parts[1]
			}
			dialer := &net.Dialer{Timeout: 15 * time.Second}
			return dialer.DialContext(ctx, network, override+":"+port)
		}
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

type stateHead struct {
	Epoch        int64    `json:"epoch"`
	TipsetKey    []string `json:"tipsetKey"`
	StateRoot    string   `json:"stateRoot"`
	ParentWeight string   `json:"parentWeight"`
}

func gatewayStateRoot(ctx context.Context, hc *http.Client, base string) (*stateHead, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/state/root", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out stateHead
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// tipsetKeyParam shapes a tipsetKey string list into the JSON-RPC param
// expected by Glif: an array of {"/": cid} objects.
func tipsetKeyParam(cids []string) []map[string]string {
	out := make([]map[string]string, 0, len(cids))
	for _, c := range cids {
		out = append(out, map[string]string{"/": c})
	}
	return out
}

type glifActor struct {
	Code    cid.Cid
	Head    cid.Cid
	Nonce   uint64
	Balance string
}

func glifGetActor(ctx context.Context, glifURL string, a addr.Address, tsk []map[string]string) (*glifActor, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Filecoin.StateGetActor",
		"params":  []any{a.String(), tsk},
		"id":      1,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", glifURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(bytes.NewReader(all))
	var raw struct {
		Result *struct {
			Code    struct{ Slash string `json:"/"` } `json:"Code"`
			Head    struct{ Slash string `json:"/"` } `json:"Head"`
			Nonce   uint64 `json:"Nonce"`
			Balance string `json:"Balance"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw.Error != nil {
		return nil, fmt.Errorf("glif: %s", raw.Error.Message)
	}
	if raw.Result == nil {
		return nil, fmt.Errorf("glif returned no result")
	}
	cCode, _ := cid.Parse(raw.Result.Code.Slash)
	cHead, _ := cid.Parse(raw.Result.Head.Slash)
	return &glifActor{Code: cCode, Head: cHead, Nonce: raw.Result.Nonce, Balance: raw.Result.Balance}, nil
}
