// Phase 11 Part A — `lantern doctor` and `lantern repair`.
//
//   doctor — re-runs the bootstrap quorum check on demand. Doesn't
//            modify state. Useful for debugging when something's wrong
//            with the chain head view.
//
//   repair — re-runs the quorum check and OVERWRITES the persisted
//            bootstrap-anchor.json with the new finality. Used when
//            the embedded F3 anchor has aged out or the operator wants
//            to refresh trust state after a long outage.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
)

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	quorum := fs.Int("bootstrap-quorum", 0, "Number of agreeing sources required for a healthy verdict (0 = network-default: mainnet=5, calibration=3)")
	timeout := fs.Duration("bootstrap-timeout", 60*time.Second, "Total wall-clock budget for the probe")
	gateway := fs.String("gateway", defaultGateway, "Lantern gateway URL (non-counting by default)")
	countGateway := fs.Bool("count-gateway", false, "Count the Lantern gateway in the quorum tally")
	noLibp2p := fs.Bool("no-libp2p", false, "Skip libp2p sources (HTTP only)")
	libp2pSettle := fs.Duration("libp2p-settle", 8*time.Second, "Bootstrap connection settle delay")
	network := fs.String("network", "filecoin", "F3 network name. Auto-derived from --filecoin-network when not set.")
	filNetwork := fs.String("filecoin-network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	var peers peerList
	fs.Var(&peers, "peer", "Additional source URL (repeatable)")
	fs.Parse(args)

	filNet := build.Network(*filNetwork)
	if !filNet.Valid() {
		return fmt.Errorf("invalid --filecoin-network %q: want one of mainnet, calibration", *filNetwork)
	}
	if *network == "filecoin" && filNet == build.Calibration {
		*network = "calibrationnet2"
	}
	if *quorum == 0 {
		if filNet == build.Calibration {
			*quorum = 3
		} else {
			*quorum = 5
		}
	}

	if err := migrateLegacyDataDir(filNet); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	dir := networkDataDir(filNet)
	fmt.Println("Lantern doctor — quorum health check")
	fmt.Println("====================================")
	fmt.Printf("Data dir: %s (network: %s)\n\n", dir, filNet)
	if a, err := ReadBootstrapAnchor(dir); err == nil && a != nil {
		fmt.Printf("Previously-anchored finality: instance=%d epoch=%d state=%s (captured %s)\n",
			a.Instance, a.Epoch, shortStr(a.StateRoot, 24),
			a.CapturedAt.Format("2006-01-02 15:04:05Z"))
	} else {
		fmt.Println("No bootstrap-anchor.json present yet (run `lantern init`).")
	}
	fmt.Println()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fin, err := runBootstrapQuorum(ctx, bootstrapParams{
		Quorum:       *quorum,
		Timeout:      *timeout,
		Gateway:      *gateway,
		CountGateway: *countGateway,
		NoLibp2p:     *noLibp2p,
		Libp2pSettle: *libp2pSettle,
		NetworkName:  *network,
		Network:      filNet,
		UserPeers:    []string(peers),
		Progress:     prettyProgress,
	})
	fmt.Println()
	if err != nil {
		if errors.Is(err, bootstrap.ErrQuorumNotReached) || errors.Is(err, bootstrap.ErrInsufficientSources) {
			fmt.Println("✗ Lantern is NOT in a healthy quorum state.")
			fmt.Println("  Possible fixes:")
			fmt.Println("    - check network connectivity (try `curl https://api.node.glif.io/rpc/v1`)")
			fmt.Println("    - add a trusted --peer URL")
			fmt.Println("    - lower --bootstrap-quorum (NOT recommended below 3)")
			fmt.Println("    - run `lantern repair` once the swarm is reachable to refresh the anchor")
			return err
		}
		return err
	}
	fmt.Printf("✓ Healthy: %s\n", fin)
	return nil
}

func cmdRepair(args []string) error {
	fs := flag.NewFlagSet("repair", flag.ExitOnError)
	quorum := fs.Int("bootstrap-quorum", 0, "Number of agreeing sources required (0 = network-default: mainnet=5, calibration=3)")
	timeout := fs.Duration("bootstrap-timeout", 60*time.Second, "Total wall-clock budget")
	gateway := fs.String("gateway", defaultGateway, "Lantern gateway URL")
	countGateway := fs.Bool("count-gateway", false, "Count the gateway in the quorum")
	noLibp2p := fs.Bool("no-libp2p", false, "Skip libp2p sources")
	libp2pSettle := fs.Duration("libp2p-settle", 8*time.Second, "Bootstrap settle delay")
	network := fs.String("network", "filecoin", "F3 network name. Auto-derived from --filecoin-network when not set.")
	filNetwork := fs.String("filecoin-network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	var peers peerList
	fs.Var(&peers, "peer", "Additional source URL (repeatable)")
	fs.Parse(args)

	filNet := build.Network(*filNetwork)
	if !filNet.Valid() {
		return fmt.Errorf("invalid --filecoin-network %q: want one of mainnet, calibration", *filNetwork)
	}
	if *network == "filecoin" && filNet == build.Calibration {
		*network = "calibrationnet2"
	}
	if *quorum == 0 {
		if filNet == build.Calibration {
			*quorum = 3
		} else {
			*quorum = 5
		}
	}

	if err := migrateLegacyDataDir(filNet); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	dir := networkDataDir(filNet)
	fmt.Println("Lantern repair — refreshing trust anchor from live swarm")
	fmt.Println("========================================================")
	fmt.Printf("Data dir: %s (network: %s)\n\n", dir, filNet)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fin, err := runBootstrapQuorum(ctx, bootstrapParams{
		Quorum:       *quorum,
		Timeout:      *timeout,
		Gateway:      *gateway,
		CountGateway: *countGateway,
		NoLibp2p:     *noLibp2p,
		Libp2pSettle: *libp2pSettle,
		NetworkName:  *network,
		Network:      filNet,
		UserPeers:    []string(peers),
		Progress:     prettyProgress,
	})
	if err != nil {
		fmt.Println()
		fmt.Println("✗ Refusing to overwrite trust anchor — quorum not reached.")
		return err
	}
	if err := writeBootstrapAnchor(dir, fin, filNet); err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("✓ Trust anchor refreshed: %s\n", fin)
	return nil
}

func shortStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// helpers shared between doctor/repair/init for arg formatting:

func formatPeerList(p peerList) string {
	if len(p) == 0 {
		return "(none)"
	}
	return strings.Join([]string(p), ", ")
}
