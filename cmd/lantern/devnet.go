// `lantern devnet-init` — the counterpart to `lantern init` for a
// locally-hosted Curio devnet (curio-fork/docker, `make devnet/up`).
//
// A devnet's identity (wire-name, genesis CID, gossip topics, bootstrap
// peers) is not knowable at Lantern-build-time: the operator spins up a
// fresh devnet, its genesis is generated per-boot, the wire-name comes
// from the genesis template. This command queries the devnet's own
// lotus over JSON-RPC to pull those values, writes them to
// <data-dir>/devnet/devnet-config.json, and seeds an initial
// bootstrap-anchor.json from ChainHead so `lantern daemon --network
// devnet` can start immediately after.
//
// After running this once against a fresh devnet:
//
//   $ lantern devnet-init --lotus-rpc http://localhost:1234/rpc/v1
//   $ lantern daemon --network devnet --insecure-anchor
//
// Trust posture: the operator OWNS the devnet, so a single trusted
// lotus endpoint is the canonical head source and multi-source quorum
// + F3 finality are both skipped. This is why `--insecure-anchor` is
// implicit on the daemon path.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/net/glif"
)

func cmdDevnetInit(args []string) error {
	fs := flag.NewFlagSet("devnet-init", flag.ExitOnError)
	lotusRPC := fs.String("lotus-rpc", "http://127.0.0.1:1234/rpc/v1", "JSON-RPC endpoint of the running devnet lotus")
	bootstrapPeers := fs.String("bootstrap-peers", "", "Comma-separated libp2p multiaddrs to dial on daemon startup. Empty is fine for the single-cluster docker devnet; lotus discovers itself.")
	force := fs.Bool("force", false, "Overwrite an existing devnet-config.json / bootstrap-anchor.json without prompting.")
	skipAnchor := fs.Bool("skip-anchor", false, "Only write devnet-config.json; skip seeding bootstrap-anchor.json from ChainHead (e.g. when re-initing after a chain-only reset).")
	timeout := fs.Duration("timeout", 15*time.Second, "Per-RPC timeout for the lotus queries.")
	fs.Parse(args)

	if err := migrateLegacyDataDir(build.Devnet); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	netDir := networkDataDir(build.Devnet)
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		return fmt.Errorf("create devnet data dir: %w", err)
	}

	cfgPath := filepath.Join(netDir, "devnet-config.json")
	anchorPath := filepath.Join(netDir, "bootstrap-anchor.json")

	if !*force {
		if _, err := os.Stat(cfgPath); err == nil {
			return fmt.Errorf("devnet config already exists at %s; pass --force to overwrite", cfgPath)
		}
	}

	fmt.Println("Lantern devnet-init — discovering local devnet identity")
	fmt.Println("======================================================")
	fmt.Printf("Data dir:  %s\n", netDir)
	fmt.Printf("Lotus RPC: %s\n\n", *lotusRPC)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := glif.New(*lotusRPC, *timeout)

	// 1. StateNetworkName → wire-name
	nnCtx, nnCancel := context.WithTimeout(ctx, *timeout)
	networkName, err := c.StateNetworkName(nnCtx)
	nnCancel()
	if err != nil {
		return fmt.Errorf("Filecoin.StateNetworkName on %s (is the devnet lotus running?): %w", *lotusRPC, err)
	}
	fmt.Printf("  ✓ StateNetworkName: %q\n", networkName)

	// 2. ChainGetGenesis → block 0 CID
	genCtx, genCancel := context.WithTimeout(ctx, *timeout)
	genesisCID, err := c.FetchGenesis(genCtx)
	genCancel()
	if err != nil {
		return fmt.Errorf("Filecoin.ChainGetGenesis: %w", err)
	}
	fmt.Printf("  ✓ Genesis:          %s\n", genesisCID)

	// 3. ChainHead → initial anchor tipset
	headCtx, headCancel := context.WithTimeout(ctx, *timeout)
	head, err := c.FetchHead(headCtx)
	headCancel()
	if err != nil {
		return fmt.Errorf("Filecoin.ChainHead: %w", err)
	}
	fmt.Printf("  ✓ ChainHead:        epoch %d, state root %s\n", head.Epoch, head.StateRoot)

	// 4. eth_chainId → EIP-155 chain identifier. Devnet's chainId is
	// per-config (curio-fork docker devnet defaults to 31415926,
	// custom setups may differ), so we bind it at devnet-init time.
	cidCtx, cidCancel := context.WithTimeout(ctx, *timeout)
	chainID, err := c.EthChainID(cidCtx)
	cidCancel()
	if err != nil {
		return fmt.Errorf("eth_chainId: %w", err)
	}
	fmt.Printf("  ✓ eth_chainId:      %d (0x%x)\n", chainID, chainID)

	// 5. Version.BlockDelay → block cadence in seconds. Devnet default is
	// 4s (curio-fork docker), but the operator may customize; consumers
	// computing epoch-time (curio schedulers) need the actual value.
	verCtx, verCancel := context.WithTimeout(ctx, *timeout)
	blockDelay, err := c.BlockDelaySecs(verCtx)
	verCancel()
	if err != nil {
		return fmt.Errorf("Filecoin.Version.BlockDelay: %w", err)
	}
	fmt.Printf("  ✓ BlockDelaySecs:   %d\n", blockDelay)

	// 6. StateNetworkVersion + StateActorCodeCIDs → the devnet's custom
	// (debug-compiled) builtin-actors bundle. These code CIDs are in no
	// released bundle, so recording them lets the daemon decode devnet
	// actor state (StateMinerPower/StateMinerInfo) instead of failing with
	// "unknown code CID". Best-effort: a devnet lotus that doesn't serve
	// these methods still yields a usable (if actor-blind) config.
	nvCtx, nvCancel := context.WithTimeout(ctx, *timeout)
	netVer, nverr := c.StateNetworkVersion(nvCtx)
	nvCancel()
	var codeCIDStrs map[string]string
	if nverr != nil {
		fmt.Printf("  ! StateNetworkVersion failed (%v); actor decoding disabled for this devnet\n", nverr)
	} else {
		fmt.Printf("  ✓ NetworkVersion:   %d\n", netVer)
		accCtx, accCancel := context.WithTimeout(ctx, *timeout)
		codeCIDs, aerr := c.StateActorCodeCIDs(accCtx, netVer)
		accCancel()
		if aerr != nil {
			fmt.Printf("  ! StateActorCodeCIDs failed (%v); actor decoding disabled for this devnet\n", aerr)
		} else {
			codeCIDStrs = make(map[string]string, len(codeCIDs))
			for name, cc := range codeCIDs {
				codeCIDStrs[name] = cc.String()
			}
			fmt.Printf("  ✓ ActorCodeCIDs:    %d actors (custom devnet bundle)\n", len(codeCIDStrs))
		}
	}

	// 7. Assemble + write config.
	cfg := &build.DevnetConfig{
		NetworkName:    networkName,
		GenesisCID:     genesisCID.String(),
		LotusRPC:       *lotusRPC,
		BootstrapPeers: splitCSV(*bootstrapPeers),
		EthChainID:     chainID,
		BlockDelaySecs: blockDelay,
		NetworkVersion: netVer,
		ActorCodeCIDs:  codeCIDStrs,
	}
	if err := build.SaveDevnetConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write devnet config: %w", err)
	}
	fmt.Printf("\n  ✓ Devnet config written: %s\n", cfgPath)
	fmt.Printf("    gossipsub topics:   /fil/blocks/%s, /fil/msgs/%s\n", networkName, networkName)
	if len(cfg.BootstrapPeers) > 0 {
		fmt.Printf("    bootstrap peers:    %v\n", cfg.BootstrapPeers)
	} else {
		fmt.Printf("    bootstrap peers:    (none — single-cluster devnet)\n")
	}

	// 7. Seed bootstrap-anchor.json from ChainHead, so the daemon can
	//    start without a multi-source quorum probe (which wouldn't
	//    reach quorum against a single-endpoint devnet anyway).
	if *skipAnchor {
		fmt.Println("\n(skip-anchor set; not seeding bootstrap-anchor.json)")
	} else {
		if _, err := os.Stat(anchorPath); err == nil && !*force {
			fmt.Printf("\n(bootstrap-anchor.json already exists at %s; skipping seed. Pass --force to overwrite.)\n", anchorPath)
		} else {
			if err := seedDevnetAnchor(anchorPath, head, genesisCID); err != nil {
				return fmt.Errorf("seed bootstrap-anchor: %w", err)
			}
			fmt.Printf("  ✓ Bootstrap anchor seeded from ChainHead epoch %d: %s\n", head.Epoch, anchorPath)
		}
	}

	fmt.Println("\nDevnet ready. Start the daemon with:")
	fmt.Println()
	fmt.Printf("  lantern daemon --network devnet --gateway %s\n", *lotusRPC)
	fmt.Println()
	fmt.Println("(bridge-off mode is not recommended on a single-node devnet — the")
	fmt.Println(" gossipsub mesh won't form. Leave the lotus RPC as the fallback.)")
	return nil
}

// seedDevnetAnchor writes a BootstrapAnchor with the current head as
// the trust root. Instance=0 is honest here: the devnet doesn't run
// F3, so there is no GPBFT instance number to record.
func seedDevnetAnchor(path string, head *glif.Head, genesis cid.Cid) error {
	tskCids := head.TipSetKey.Cids()
	tsks := make([]string, len(tskCids))
	for i, c := range tskCids {
		tsks[i] = c.String()
	}
	a := BootstrapAnchor{
		Instance:   0,
		Epoch:      int64(head.Epoch),
		TipSetKey:  tsks,
		StateRoot:  head.StateRoot.String(),
		CapturedAt: time.Now().UTC(),
		Network:    string(build.Devnet),
	}
	// SaveDevnetConfig already handles atomic tmp+rename. Reuse the
	// same pattern for the anchor: write .tmp + rename.
	tmp := path + ".tmp"
	if err := writeBootstrapAnchorJSON(tmp, &a); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename anchor: %w", err)
	}
	_ = genesis // reserved for a future genesis-cid cross-check
	return nil
}

// writeBootstrapAnchorJSON marshals a BootstrapAnchor to disk. Separate
// from cmd/lantern/init.go's writeBootstrapAnchor because that one
// takes a bootstrap.Finality and prints its own progress; devnet
// operates from a Head (no F3, no Finality) and prints its own line.
func writeBootstrapAnchorJSON(path string, a *BootstrapAnchor) error {
	if a == nil {
		return errors.New("nil anchor")
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
