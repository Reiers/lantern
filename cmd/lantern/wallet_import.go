// Wallet import/export commands (#93).
//
// `lantern wallet export <addr>`      - Lotus-hex KeyInfo to stdout
// `lantern wallet import [hex|-]`     - Lotus-hex KeyInfo from arg/stdin
// `lantern wallet import-lotus <dir>` - bulk import from a Lotus repo
//
// Lotus's on-disk keystore is `<repo>/keystore/<base32(name)>` with
// RawStdEncoding filenames; wallet keys are named `wallet-<addr>` and the
// file content is the KeyInfo JSON ({"Type","PrivateKey"} with base64
// key bytes). `lotus wallet export` prints hex(JSON(KeyInfo)) - the same
// wire shape both commands here speak, so keys round-trip between Lotus
// and Lantern in either direction.
//
// Everything imported lands in Lantern's passphrase-protected keystore
// (the Envelope encryption the wallet already uses); nothing is ever
// written to disk unencrypted.

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	base32 "github.com/multiformats/go-base32"
	"os"
	"path/filepath"
	"strings"

	faddr "github.com/filecoin-project/go-address"

	"github.com/Reiers/lantern/wallet"
)

// walletExport prints the Lotus-compatible hex KeyInfo for an address.
func walletExport(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: lantern wallet export <address>")
	}
	w, err := openWalletDefault()
	if err != nil {
		return err
	}
	addr, err := faddr.NewFromString(args[0])
	if err != nil {
		return fmt.Errorf("parse address: %w", err)
	}
	ki, err := w.Export(context.Background(), addr)
	if err != nil {
		return fmt.Errorf("export %s: %w", addr, err)
	}
	blob, err := json.Marshal(ki)
	if err != nil {
		return err
	}
	fmt.Println(hex.EncodeToString(blob))
	return nil
}

// walletImport imports one Lotus-hex KeyInfo from the argument or stdin.
func walletImport(args []string) error {
	var raw string
	switch {
	case len(args) == 1 && args[0] != "-":
		raw = args[0]
	default:
		// Read one line from stdin (piped `lotus wallet export` output).
		fmt.Fprintln(os.Stderr, "reading hex KeyInfo from stdin...")
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		if !sc.Scan() {
			return fmt.Errorf("no input on stdin")
		}
		raw = sc.Text()
	}
	raw = strings.TrimSpace(raw)
	blob, err := hex.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("hex-decode KeyInfo: %w", err)
	}
	var ki wallet.KeyInfo
	if err := json.Unmarshal(blob, &ki); err != nil {
		return fmt.Errorf("parse KeyInfo JSON: %w", err)
	}
	w, err := openWalletDefault()
	if err != nil {
		return err
	}
	addr, err := w.Import(context.Background(), &ki)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	fmt.Println(addr.String())
	return nil
}

// walletImportLotus bulk-imports wallet keys from a Lotus repo keystore.
func walletImportLotus(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: lantern wallet import-lotus <lotus-repo-path>   (e.g. ~/.lotus)")
	}
	repo := expandHome(args[0])
	ksDir := filepath.Join(repo, "keystore")
	if st, err := os.Stat(ksDir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a Lotus repo (no keystore/ dir)", repo)
	}

	entries, err := os.ReadDir(ksDir)
	if err != nil {
		return fmt.Errorf("read keystore dir: %w", err)
	}

	w, err := openWalletDefault()
	if err != nil {
		return err
	}

	imported, skipped := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Lotus encodes keystore entry names with base32 RawStdEncoding.
		nameBytes, err := base32.RawStdEncoding.DecodeString(e.Name())
		if err != nil {
			skipped++
			continue // not a Lotus keystore entry
		}
		name := string(nameBytes)
		if !strings.HasPrefix(name, "wallet-") {
			continue // libp2p host key, jwt secret, etc: not a wallet
		}
		blob, err := os.ReadFile(filepath.Join(ksDir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", name, err)
			skipped++
			continue
		}
		var ki wallet.KeyInfo
		if err := json.Unmarshal(blob, &ki); err != nil {
			fmt.Fprintf(os.Stderr, "  skip %s: parse KeyInfo: %v\n", name, err)
			skipped++
			continue
		}
		addr, err := w.Import(context.Background(), &ki)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skip %s: import: %v\n", name, err)
			skipped++
			continue
		}
		fmt.Printf("  imported %s (%s)\n", addr.String(), ki.Type)
		imported++
	}
	fmt.Printf("done: %d imported, %d skipped (keys are stored encrypted in Lantern's keystore)\n", imported, skipped)
	if imported == 0 && skipped == 0 {
		fmt.Println("no wallet-* entries found in the Lotus keystore")
	}
	return nil
}

// expandHome expands a leading ~/ to the user home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}
