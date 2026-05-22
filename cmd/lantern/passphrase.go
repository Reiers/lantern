// Helpers for the keystore passphrase resolution flow.
// Issue #2: replace the empty-string fallback with a real TTY prompt path.

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// isInteractive reports whether stdin is a terminal. We use this to
// decide whether to prompt interactively for the keystore passphrase or
// to fail loudly with an actionable error.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// readPassword reads a password from stdin without echoing it. Falls back
// to a buffered ReadString when stdin is not a TTY (defensive; the caller
// gates this with isInteractive() before invoking).
func readPassword() (string, error) {
	if isInteractive() {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr) // ReadPassword swallows the trailing newline; restore it
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-TTY fallback (echo will show). Should never be hit in practice.
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// keystoreHasKeys returns true if the keystore directory exists and
// contains at least one wallet-* file. Used to distinguish unlock
// (existing keystore, prompt once) from init (fresh keystore, prompt
// twice with confirmation).
func keystoreHasKeys(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "wallet-") && !strings.HasSuffix(name, ".bak") {
			return true
		}
	}
	return false
}

// keystoreDir resolves the absolute keystore path used by openWallet
// and surfaces it in error messages.
func keystoreDir() string {
	return filepath.Join(dataDir(), "keystore")
}
