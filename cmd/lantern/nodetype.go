package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/pkg/nodeprofile"
)

// cmdNodeType reads or sets the install-time node tier (light | pdp | full).
//
//	lantern node-type                 # print the current tier
//	lantern node-type pdp             # set the tier to PDP
//	lantern node-type pdp --cache-gb 5 --allow-block-submit
//
// The installer (get.golantern.io) calls this to persist the user's choice
// so the daemon provisions only what that tier needs - the light node stays
// small; PDP gets the persistent 2-5 GB cache + write/production surface.
func cmdNodeType(args []string) error {
	// The tier is a leading positional arg (e.g. `node-type pdp --cache-gb 5`).
	// Go's flag package stops at the first non-flag token, so pull the tier
	// off the FRONT before parsing flags - otherwise flags placed after the
	// tier arg are silently ignored (caught in smoke test: --network/--cache-gb
	// after `pdp` were dropped).
	var tierArg string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		tierArg = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}

	fs := flag.NewFlagSet("node-type", flag.ContinueOnError)
	networkFlag := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	cacheGB := fs.Float64("cache-gb", 0, "Persistent cache soft budget in GB (PDP/Full). 0 => tier default (3 GB).")
	allowSubmit := fs.Bool("allow-block-submit", false, "Record block-submit/backup opt-in (PDP/Full). Still needs --vm-bridge-rpc at run time.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	network := build.Network(*networkFlag)
	if !network.Valid() {
		return fmt.Errorf("invalid --network %q: want mainnet or calibration", *networkFlag)
	}
	home := dataDir()

	// No tier argument => print the current tier and exit.
	if tierArg == "" {
		p, err := nodeprofile.Load(home, network.String())
		if err != nil {
			return err
		}
		fmt.Printf("node-type: %s\n", p.Tier)
		if p.UsesPersistentCache() {
			fmt.Printf("  persistent cache: %d MiB\n", p.CacheBytes()>>20)
		} else {
			fmt.Printf("  cache: in-memory\n")
		}
		if p.AllowBlockSubmit {
			fmt.Printf("  block submit: opted in (requires --vm-bridge-rpc at run time)\n")
		}
		fmt.Printf("  profile: %s\n", nodeprofile.Path(home, network.String()))
		return nil
	}

	var tier nodeprofile.Tier
	switch tierArg {
	case "light":
		tier = nodeprofile.TierLight
	case "pdp":
		tier = nodeprofile.TierPDP
	case "full":
		// Accept + record, but be honest that it isn't buildable bridge-off.
		tier = nodeprofile.TierFull
		fmt.Fprintln(os.Stderr, "  note: 'full' tier is recorded but native full-node validation needs a VM bridge (no-CGo constraint). It behaves like PDP with block-submit until the full-node track lands.")
	default:
		return fmt.Errorf("unknown node-type %q: want light | pdp | full", tierArg)
	}

	p := nodeprofile.Profile{Tier: tier}
	if tier != nodeprofile.TierLight {
		if *cacheGB > 0 {
			p.PersistentCacheBytes = int64(*cacheGB * float64(1<<30))
		}
		p.AllowBlockSubmit = *allowSubmit
	} else {
		if *cacheGB > 0 || *allowSubmit {
			fmt.Fprintln(os.Stderr, "  note: --cache-gb / --allow-block-submit ignored for the light tier.")
		}
	}

	if err := nodeprofile.Save(home, network.String(), p); err != nil {
		return err
	}

	fmt.Printf("node-type set to %s (%s)\n", p.Tier, network)
	if p.UsesPersistentCache() {
		fmt.Printf("  persistent cache: %d MiB\n", p.CacheBytes()>>20)
	}
	if p.AllowBlockSubmit {
		fmt.Printf("  block submit: opted in (set --vm-bridge-rpc when running the daemon)\n")
	}
	fmt.Printf("  written: %s\n", nodeprofile.Path(home, network.String()))
	fmt.Printf("  restart the daemon for the tier to take effect.\n")
	return nil
}
