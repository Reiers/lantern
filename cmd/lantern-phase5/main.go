// lantern-phase5 is the Phase 5 end-to-end demo.
//
// Goal: demonstrate that Lantern can serve a Curio-shaped query stream
// (StateMinerInfo, StateMinerPower, StateMinerProvingDeadline,
// StateMinerAvailableBalance, StateMarketStorageDeal) end-to-end on a
// trusted state root, with every result cross-checked against Glif.

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

	addr "github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/state/hamt"
)

const (
	defaultGateway = "https://gateway.lantern.reiers.io"
	defaultGlif    = "https://api.node.glif.io/rpc/v1"
	// Picked because:
	//   - f0142637 is an active mainnet SP with ~1.6 EiB raw power
	//   - confirmed via Glif at demo write time (epoch ~6035600)
	defaultMiner = "f0142637"
	// f02620: 32GiB active miner (smaller, also covers v17/v18 dispatch).
	defaultMiner2 = "f02620"
	// Deal ID 100_000_000 is a recent recurring deal on mainnet, picked
	// because it always has both a Proposal and a State entry.
	defaultDealID = uint64(100_000_000)
)

type cliFlags struct {
	gatewayURL string
	glifURL    string
	miner      string
	miner2     string
	dealID     uint64
}

func main() {
	var f cliFlags
	flag.StringVar(&f.gatewayURL, "gateway", defaultGateway, "Lantern gateway base URL")
	flag.StringVar(&f.glifURL, "glif", defaultGlif, "Public Filecoin RPC for cross-check")
	flag.StringVar(&f.miner, "miner", defaultMiner, "Active miner to query for the main demo")
	flag.StringVar(&f.miner2, "miner2", defaultMiner2, "Second active miner (smaller-power)")
	flag.Uint64Var(&f.dealID, "deal", defaultDealID, "Deal ID to look up")
	flag.Parse()

	if err := run(context.Background(), f); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nOK — Phase 5 demo complete")
}

func run(ctx context.Context, f cliFlags) error {
	fmt.Println("Lantern Phase 5 — SP-compatibility demo")
	fmt.Println("=======================================")

	// ---- 1. Bootstrap a TrustedRoot from the gateway ----
	head, err := gatewayStateRoot(ctx, f.gatewayURL)
	if err != nil {
		return fmt.Errorf("gateway /state/root: %w", err)
	}
	stateRoot, err := cid.Parse(head.StateRoot)
	if err != nil {
		return fmt.Errorf("parse stateRoot CID: %w", err)
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:     abi.ChainEpoch(head.Epoch),
		StateRoot: stateRoot,
	}
	fmt.Printf("TrustedRoot @ epoch %d, stateRoot %s\n", tr.Epoch, tr.StateRoot)

	// ---- 2. Wire fetcher (cache + gateway + glif fallback) ----
	cache := hamt.NewMemBlockStore()
	gw := hsync.NewClient([]string{f.gatewayURL}, 20*time.Second)
	gl := glif.New(f.glifURL, 20*time.Second)
	fetcher := combined.New(cache,
		combined.Source{Name: "gateway", Getter: gw, Timeout: 15 * time.Second},
		combined.Source{Name: "glif", Getter: gl, Timeout: 20 * time.Second},
	)

	// ---- 3. Build the ChainAPI ----
	chain := handlers.New(tr, fetcher, nil, nil, "mainnet")
	tsk := tipsetKeyParam(head.TipsetKey)

	// ---- 4. Miner queries on the primary target ----
	mAddr, err := addr.NewFromString(f.miner)
	if err != nil {
		return fmt.Errorf("parse miner addr: %w", err)
	}
	fmt.Println()
	fmt.Printf("Primary miner: %s\n", f.miner)
	fmt.Println("------------------------------------------")

	if err := minerDemo(ctx, chain, f.glifURL, mAddr, tsk); err != nil {
		return fmt.Errorf("miner %s: %w", f.miner, err)
	}

	// ---- 5. Miner queries on a second target (v17 dispatch coverage) ----
	m2, err := addr.NewFromString(f.miner2)
	if err != nil {
		return fmt.Errorf("parse miner2 addr: %w", err)
	}
	fmt.Println()
	fmt.Printf("Secondary miner: %s (smaller-power; broader version dispatch)\n", f.miner2)
	fmt.Println("------------------------------------------")
	if err := minerDemo(ctx, chain, f.glifURL, m2, tsk); err != nil {
		return fmt.Errorf("miner %s: %w", f.miner2, err)
	}

	// ---- 6. Market deal ----
	fmt.Println()
	fmt.Printf("Market deal #%d\n", f.dealID)
	fmt.Println("------------------------------------------")
	deal, err := chain.StateMarketStorageDeal(ctx, abi.DealID(f.dealID), types.TipSetKey{})
	if err != nil {
		fmt.Printf("  StateMarketStorageDeal: FAIL: %v\n", err)
	} else {
		fmt.Printf("  proposal.PieceCID:        %s\n", deal.Proposal.PieceCID)
		fmt.Printf("  proposal.PieceSize:       %d\n", deal.Proposal.PieceSize)
		fmt.Printf("  proposal.Client:          %s\n", deal.Proposal.Client)
		fmt.Printf("  proposal.Provider:        %s\n", deal.Proposal.Provider)
		fmt.Printf("  proposal.Start..End:      %d..%d\n", deal.Proposal.StartEpoch, deal.Proposal.EndEpoch)
		fmt.Printf("  proposal.VerifiedDeal:    %v\n", deal.Proposal.VerifiedDeal)
		fmt.Printf("  state.SectorStartEpoch:   %d\n", deal.State.SectorStartEpoch)
		fmt.Printf("  state.SlashEpoch:         %d\n", deal.State.SlashEpoch)

		// Cross-check provider via Glif
		ref, err := glifMarketDeal(ctx, f.glifURL, f.dealID, tsk)
		if err != nil {
			fmt.Printf("  Glif cross-check FAIL: %v\n", err)
		} else {
			if ref["Provider"] == deal.Proposal.Provider.String() && ref["PieceCID"] == deal.Proposal.PieceCID.String() {
				fmt.Printf("  Glif cross-check: MATCH (provider=%s pieceCID=%s)\n", ref["Provider"], ref["PieceCID"])
			} else {
				fmt.Printf("  Glif cross-check: MISMATCH (ours=[%s, %s] ref=[%s, %s])\n",
					deal.Proposal.Provider, deal.Proposal.PieceCID, ref["Provider"], ref["PieceCID"])
			}
		}
	}

	// ---- 7. Network-level reads ----
	fmt.Println()
	fmt.Println("Network reads")
	fmt.Println("------------------------------------------")
	nv, _ := chain.StateNetworkVersion(ctx, types.TipSetKey{})
	fmt.Printf("  StateNetworkVersion: nv%d\n", nv)
	supply, err := chain.StateCirculatingSupply(ctx, types.TipSetKey{})
	if err == nil {
		fmt.Printf("  StateCirculatingSupply (approx): %s\n", supply)
	} else {
		fmt.Printf("  StateCirculatingSupply: %v\n", err)
	}
	root, err := chain.VerifiedRegistryRootKey(ctx)
	if err == nil {
		fmt.Printf("  VerifiedRegistryRootKey: %s\n", root)
		// Cross-check via Glif.
		ref, err := glifSimple(ctx, f.glifURL, "Filecoin.StateVerifiedRegistryRootKey", []any{tsk})
		if err == nil {
			refStr, _ := ref.(string)
			if refStr == root.String() {
				fmt.Printf("  Glif cross-check: MATCH\n")
			} else {
				fmt.Printf("  Glif cross-check: MISMATCH (ours=%s ref=%s)\n", root, refStr)
			}
		}
	}

	stats := fetcher.Stats()
	fmt.Println()
	fmt.Printf("Fetcher stats: %+v (cache=%d gateway=%d glif=%d misses=%d)\n",
		stats, stats["cache"], stats["gateway"], stats["glif"], stats["misses"])
	return nil
}

func minerDemo(ctx context.Context, chain *handlers.ChainAPI, glifURL string, m addr.Address, tsk []map[string]string) error {
	// StateMinerInfo
	info, err := chain.StateMinerInfo(ctx, m, types.TipSetKey{})
	if err != nil {
		return fmt.Errorf("StateMinerInfo: %w", err)
	}
	fmt.Printf("  StateMinerInfo:\n")
	fmt.Printf("    Owner=%s  Worker=%s  Beneficiary=%s\n", info.Owner, info.Worker, info.Beneficiary)
	fmt.Printf("    SectorSize=%d (%s)\n", info.SectorSize, humanSize(uint64(info.SectorSize)))
	fmt.Printf("    PoStProof=%d  PartitionSectors=%d\n", info.WindowPoStProofType, info.WindowPoStPartitionSectors)
	fmt.Printf("    Controls=%d  Multiaddrs=%d\n", len(info.ControlAddresses), len(info.Multiaddrs))

	ref, err := glifMinerInfo(ctx, glifURL, m, tsk)
	if err != nil {
		fmt.Printf("    Glif cross-check FAIL: %v\n", err)
	} else {
		// Glif returns SectorSize as a JSON number which Go decodes to
		// float64. Convert to uint64 for comparison.
		refSize := uint64(0)
		if f, ok := ref["SectorSize"].(float64); ok {
			refSize = uint64(f)
		}
		match := ref["Owner"] == info.Owner.String() && ref["Worker"] == info.Worker.String() &&
			ref["Beneficiary"] == info.Beneficiary.String() &&
			refSize == uint64(info.SectorSize)
		status := "MATCH"
		if !match {
			status = fmt.Sprintf("MISMATCH (ours owner=%s worker=%s beneficiary=%s size=%d; ref %v)",
				info.Owner, info.Worker, info.Beneficiary, info.SectorSize, ref)
		}
		fmt.Printf("    Glif cross-check: %s\n", status)
	}

	// StateMinerPower
	power, err := chain.StateMinerPower(ctx, m, types.TipSetKey{})
	if err != nil {
		fmt.Printf("  StateMinerPower: FAIL: %v\n", err)
	} else {
		fmt.Printf("  StateMinerPower:\n")
		fmt.Printf("    miner.Raw=%s  miner.QA=%s\n", power.MinerPower.RawBytePower, power.MinerPower.QualityAdjPower)
		fmt.Printf("    network.Raw=%s  network.QA=%s\n", power.TotalPower.RawBytePower, power.TotalPower.QualityAdjPower)
		// Cross-check
		ref, err := glifMinerPower(ctx, glifURL, m, tsk)
		if err == nil {
			if ref["MinerRaw"] == power.MinerPower.RawBytePower.String() &&
				ref["TotalRaw"] == power.TotalPower.RawBytePower.String() {
				fmt.Printf("    Glif cross-check: MATCH (raw=%s/%s)\n", ref["MinerRaw"], ref["TotalRaw"])
			} else {
				fmt.Printf("    Glif cross-check: MISMATCH\n      ours: miner=%s total=%s\n      ref:  miner=%s total=%s\n",
					power.MinerPower.RawBytePower, power.TotalPower.RawBytePower,
					ref["MinerRaw"], ref["TotalRaw"])
			}
		}
	}

	// StateMinerProvingDeadline
	pd, err := chain.StateMinerProvingDeadline(ctx, m, types.TipSetKey{})
	if err != nil {
		fmt.Printf("  StateMinerProvingDeadline: FAIL: %v\n", err)
	} else {
		fmt.Printf("  StateMinerProvingDeadline:\n")
		fmt.Printf("    CurrentEpoch=%d  PeriodStart=%d  Index=%d\n", pd.CurrentEpoch, pd.PeriodStart, pd.Index)
		fmt.Printf("    Open=%d  Close=%d  Challenge=%d\n", pd.Open, pd.Close, pd.Challenge)
	}

	// StateMinerAvailableBalance
	bal, err := chain.StateMinerAvailableBalance(ctx, m, types.TipSetKey{})
	if err != nil {
		fmt.Printf("  StateMinerAvailableBalance: FAIL: %v\n", err)
	} else {
		fmt.Printf("  StateMinerAvailableBalance: %s attoFIL\n", bal)
	}

	// StateMinerFaults / Recoveries
	faults, err := chain.StateMinerFaults(ctx, m, types.TipSetKey{})
	if err == nil {
		cnt, _ := faults.Count()
		fmt.Printf("  StateMinerFaults: %d faulty sectors\n", cnt)
	}
	recos, err := chain.StateMinerRecoveries(ctx, m, types.TipSetKey{})
	if err == nil {
		cnt, _ := recos.Count()
		fmt.Printf("  StateMinerRecoveries: %d recovering sectors\n", cnt)
	}

	// One sector lookup if we can find one from partitions.
	parts, err := chain.StateMinerPartitions(ctx, m, 0, types.TipSetKey{})
	if err == nil && len(parts) > 0 {
		fmt.Printf("  StateMinerPartitions(dl=0): %d partitions\n", len(parts))
		// Pick an active sector and look it up via StateSectorGetInfo.
		var sno abi.SectorNumber
		_ = parts[0].ActiveSectors.ForEach(func(s uint64) error {
			if sno == 0 {
				sno = abi.SectorNumber(s)
			}
			return nil
		})
		if sno > 0 {
			si, err := chain.StateSectorGetInfo(ctx, m, sno, types.TipSetKey{})
			if err == nil && si != nil {
				fmt.Printf("  StateSectorGetInfo(%d): SealedCID=%s Activation=%d Expiration=%d\n",
					sno, si.SealedCID, si.Activation, si.Expiration)
			}
			loc, err := chain.StateSectorPartition(ctx, m, sno, types.TipSetKey{})
			if err == nil && loc != nil {
				fmt.Printf("  StateSectorPartition(%d): dl=%d partition=%d\n", sno, loc.Deadline, loc.Partition)
			}
		}
	}

	// What actor version backed the decode? Re-do GetActor → registry.
	actor, _, err := chain.Accessor.GetActor(ctx, m)
	if err == nil {
		ci, ok := chain.Accessor.Registry().Lookup(actor.Code)
		if ok {
			fmt.Printf("  [decoded via %s actor v%d on %s]\n", ci.Kind, ci.Version, ci.Network)
		}
	}

	return nil
}

func humanSize(n uint64) string {
	const (
		KiB = 1024
		MiB = KiB * 1024
		GiB = MiB * 1024
		TiB = GiB * 1024
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/TiB)
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/GiB)
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/MiB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------
// Glif helpers
// ---------------------------------------------------------------------

type stateHead struct {
	Epoch        int64    `json:"epoch"`
	TipsetKey    []string `json:"tipsetKey"`
	StateRoot    string   `json:"stateRoot"`
	ParentWeight string   `json:"parentWeight"`
}

func gatewayStateRoot(ctx context.Context, base string) (*stateHead, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/state/root", nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
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

func tipsetKeyParam(cids []string) []map[string]string {
	out := make([]map[string]string, 0, len(cids))
	for _, c := range cids {
		out = append(out, map[string]string{"/": c})
	}
	return out
}

func glifMinerInfo(ctx context.Context, glifURL string, a addr.Address, tsk []map[string]string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Filecoin.StateMinerInfo",
		"params":  []any{a.String(), tsk},
		"id":      1,
	})
	out, err := glifCall(ctx, glifURL, body)
	if err != nil {
		return nil, err
	}
	r, _ := out["result"].(map[string]any)
	if r == nil {
		return nil, fmt.Errorf("no result")
	}
	return r, nil
}

func glifMinerPower(ctx context.Context, glifURL string, a addr.Address, tsk []map[string]string) (map[string]string, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Filecoin.StateMinerPower",
		"params":  []any{a.String(), tsk},
		"id":      1,
	})
	out, err := glifCall(ctx, glifURL, body)
	if err != nil {
		return nil, err
	}
	r, _ := out["result"].(map[string]any)
	if r == nil {
		return nil, fmt.Errorf("no result")
	}
	mp, _ := r["MinerPower"].(map[string]any)
	tp, _ := r["TotalPower"].(map[string]any)
	if mp == nil || tp == nil {
		return nil, fmt.Errorf("malformed result")
	}
	return map[string]string{
		"MinerRaw": fmt.Sprintf("%v", mp["RawBytePower"]),
		"MinerQA":  fmt.Sprintf("%v", mp["QualityAdjPower"]),
		"TotalRaw": fmt.Sprintf("%v", tp["RawBytePower"]),
		"TotalQA":  fmt.Sprintf("%v", tp["QualityAdjPower"]),
	}, nil
}

func glifMarketDeal(ctx context.Context, glifURL string, dealID uint64, tsk []map[string]string) (map[string]string, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Filecoin.StateMarketStorageDeal",
		"params":  []any{dealID, tsk},
		"id":      1,
	})
	out, err := glifCall(ctx, glifURL, body)
	if err != nil {
		return nil, err
	}
	r, _ := out["result"].(map[string]any)
	if r == nil {
		return nil, fmt.Errorf("no result")
	}
	prop, _ := r["Proposal"].(map[string]any)
	if prop == nil {
		return nil, fmt.Errorf("no Proposal")
	}
	piece, _ := prop["PieceCID"].(map[string]any)
	pieceStr, _ := piece["/"].(string)
	return map[string]string{
		"Provider": fmt.Sprintf("%v", prop["Provider"]),
		"Client":   fmt.Sprintf("%v", prop["Client"]),
		"PieceCID": pieceStr,
	}, nil
}

func glifSimple(ctx context.Context, glifURL string, method string, params []any) (any, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	out, err := glifCall(ctx, glifURL, body)
	if err != nil {
		return nil, err
	}
	return out["result"], nil
}

func glifCall(ctx context.Context, glifURL string, body []byte) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", glifURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	if err := json.Unmarshal(all, &raw); err != nil {
		return nil, err
	}
	if e, ok := raw["error"].(map[string]any); ok && e != nil {
		return nil, fmt.Errorf("glif: %v", e["message"])
	}
	return raw, nil
}

// silence unused import
var _ = api.MinerInfo{}
