// lantern-f3-anchor pulls the current F3 power table + a recent certificate
// from a Forest (or Lotus) node, verifies the cert's BLS aggregate against the
// prior power table, and writes a canonical Anchor JSON file that the chain/f3
// package embeds at build time.
//
// Usage:
//
//	export FOREST_URL=http://...:2345/rpc/v1
//	export FOREST_TOKEN=eyJ...
//	lantern-f3-anchor -network mainnet -out chain/f3/anchor/anchor_mainnet.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Reiers/lantern/chain/f3/anchor"
)

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      int    `json:"id"`
}

type rpcResp[T any] struct {
	Result T `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	var (
		network = flag.String("network", "mainnet", "network name (mainnet|calibnet)")
		out     = flag.String("out", "", "output JSON path")
		url     = flag.String("url", os.Getenv("FOREST_URL"), "Forest/Lotus RPC URL")
		token   = flag.String("token", os.Getenv("FOREST_TOKEN"), "RPC bearer token")
	)
	flag.Parse()
	if *out == "" || *url == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "missing required flags or env vars (FOREST_URL, FOREST_TOKEN)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. Chain head — we capture this for provenance.
	var head struct {
		Height int64
		Cids   []map[string]string
	}
	if err := call(ctx, *url, *token, "Filecoin.ChainHead", []any{}, &head); err != nil {
		die("ChainHead: %v", err)
	}
	fmt.Printf("chain head height=%d\n", head.Height)

	// 2. Latest F3 cert (for the instance we'll anchor to).
	var latest struct {
		GPBFTInstance uint64
	}
	if err := call(ctx, *url, *token, "Filecoin.F3GetLatestCertificate", []any{}, &latest); err != nil {
		die("F3GetLatestCertificate: %v", err)
	}
	fmt.Printf("latest F3 cert instance=%d\n", latest.GPBFTInstance)

	// 3. Current F3 power table at head.
	var pt []anchor.ForestPowerEntry
	tipsetKey := head.Cids
	if err := call(ctx, *url, *token, "Filecoin.F3GetF3PowerTable", []any{tipsetKey}, &pt); err != nil {
		die("F3GetF3PowerTable: %v", err)
	}
	fmt.Printf("power table entries=%d\n", len(pt))

	// 4. Build the anchor.
	a, err := anchor.FromForestPowerEntries(
		*network,
		latest.GPBFTInstance,
		pt,
		fmt.Sprintf("head=%d", head.Height),
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		die("build anchor: %v", err)
	}

	// 5. Sanity: materialise into a real PowerTable.
	if _, err := a.PowerTable(); err != nil {
		die("materialise power table: %v", err)
	}

	// 6. Write canonical JSON.
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		die("marshal: %v", err)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		die("write: %v", err)
	}
	fmt.Printf("anchor written to %s (%d bytes, network=%s, instance=%d, entries=%d)\n",
		*out, len(b), a.Network, a.Instance, len(a.Entries))
}

func call(ctx context.Context, url, token, method string, params any, out any) error {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var wrap struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if wrap.Error != nil {
		return fmt.Errorf("rpc error %d: %s", wrap.Error.Code, wrap.Error.Message)
	}
	return json.Unmarshal(wrap.Result, out)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
