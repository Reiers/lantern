// `lantern reset` — issue #51.
//
// The official, key-safe recovery path for a node that has been stopped
// for a long time and boots stuck on a stale head. It clears ONLY
// rebuildable chain state (the persistent header store and the on-disk
// bootstrap anchor) and then the next `lantern daemon` re-syncs from the
// live head.
//
// It is deliberately impossible for this command to delete secrets. The
// only thing it removes is an explicit allow-list of chain-state paths;
// it never recurses over the whole data dir, and it refuses to run if
// asked (via the escape hatch flag) to touch anything in the secrets
// allow-list.
//
// This exists because the previous "recovery" was hand-rolled
// `rm -rf ~/.lantern/...` which sits next to keystore/ and jwt-secret in
// the same directory — one slip wipes irreplaceable keys (observed
// 2026-06-18). No README, --help, or UI hint should ever again tell a
// user to rm under ~/.lantern directly.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Reiers/lantern/build"
)

// chainStateNames is the EXHAUSTIVE list of per-network entries `reset
// --chain-state` is allowed to remove. Everything else in the network
// dir (keystore/, jwt-secret, token*, rpc-listen) is preserved.
var chainStateNames = []string{
	"headerstore",
	"bootstrap-anchor.json",
}

// secretsNames are never-touch entries. Used as a belt-and-suspenders
// assertion: if a future edit ever adds one of these to the removal
// set, the command aborts instead of deleting a key.
var secretsNames = map[string]bool{
	"keystore":    true,
	"jwt-secret":  true,
	"token":       true,
	"token-read":  true,
	"token-sign":  true,
	"token-write": true,
}

func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	chainState := fs.Bool("chain-state", false, "Clear rebuildable chain state (header store + bootstrap anchor) so the node re-syncs from live head. This is the only supported mode.")
	filNetwork := fs.String("filecoin-network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	yes := fs.Bool("yes", false, "Skip the confirmation prompt.")
	fs.Parse(args)

	if !*chainState {
		return fmt.Errorf("nothing to do: pass --chain-state.\n" +
			"  `lantern reset --chain-state` clears the header store + bootstrap anchor\n" +
			"  (rebuildable; re-synced from live head on next start). Keys are never touched.")
	}

	filNet := build.Network(*filNetwork)
	if !filNet.Valid() {
		return fmt.Errorf("invalid --filecoin-network %q: want one of mainnet, calibration", *filNetwork)
	}

	if err := migrateLegacyDataDir(filNet); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	dir := networkDataDir(filNet)

	// Belt-and-suspenders: assert we are not about to delete a secret.
	for _, n := range chainStateNames {
		if secretsNames[n] {
			return fmt.Errorf("internal error: chain-state removal list contains protected secret %q; aborting", n)
		}
	}

	// Figure out what actually exists, so the prompt + summary are honest.
	type target struct {
		name string
		path string
	}
	var present []target
	for _, n := range chainStateNames {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			present = append(present, target{n, p})
		}
	}

	fmt.Println("Lantern reset — clear chain state (keys are NOT touched)")
	fmt.Println("=======================================================")
	fmt.Printf("Network:  %s\n", filNet)
	fmt.Printf("Data dir: %s\n\n", dir)

	if len(present) == 0 {
		fmt.Println("No chain state to clear (header store + anchor already absent).")
		fmt.Println("Next `lantern daemon` will cold-bootstrap from live head.")
		return nil
	}

	fmt.Println("Will remove (rebuildable chain state):")
	for _, t := range present {
		fmt.Printf("  - %s\n", t.path)
	}
	fmt.Println("\nWill PRESERVE (your secrets):")
	for _, n := range []string{"keystore", "jwt-secret", "token", "token-read", "token-sign", "token-write"} {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("  ✓ %s\n", p)
		}
	}
	fmt.Println()

	if !*yes {
		fmt.Print("Proceed? [y/N] ")
		var reply string
		_, _ = fmt.Scanln(&reply)
		if reply != "y" && reply != "Y" && reply != "yes" {
			fmt.Println("Aborted. Nothing removed.")
			return nil
		}
	}

	removed := 0
	for _, t := range present {
		// Final guard at the moment of deletion.
		if secretsNames[t.name] {
			return fmt.Errorf("refusing to remove protected secret %q", t.path)
		}
		if err := os.RemoveAll(t.path); err != nil {
			return fmt.Errorf("remove %s: %w", t.path, err)
		}
		fmt.Printf("  ✓ removed %s\n", t.name)
		removed++
	}

	fmt.Printf("\nDone. Cleared %d chain-state entr%s.\n", removed, plural(removed))
	fmt.Println("Start the daemon again and it will re-sync from live head:")
	fmt.Printf("  lantern daemon --network %s\n", filNet)
	fmt.Println("\nNote: if the daemon is currently running, stop it first")
	fmt.Println("(`lantern stop`) so the header store isn't recreated mid-reset.")
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
