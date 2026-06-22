// Unit tests for the keystore passphrase resolution flow (issue #2).
//
// We can't easily test the TTY-interactive prompt path in a Go unit
// test (would need a pseudo-terminal). We CAN test:
//   - resolvePassphrase returns the env value when set and non-empty
//   - resolvePassphrase returns "" with a warning when env is set-empty
//   - resolvePassphrase errors loudly when env is unset and no TTY
//   - keystoreHasKeys correctly distinguishes empty / non-empty / missing
//
// The non-TTY error path is the most security-relevant: it's what
// stops a misconfigured systemd unit from silently writing keys to an
// unencrypted keystore.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeystoreHasKeys(t *testing.T) {
	dir := t.TempDir()

	// No files yet.
	if keystoreHasKeys(dir) {
		t.Errorf("empty dir reported as having keys")
	}

	// Drop a non-wallet file.
	if err := os.WriteFile(filepath.Join(dir, "jwt-secret"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write jwt-secret: %v", err)
	}
	if keystoreHasKeys(dir) {
		t.Errorf("dir with only jwt-secret reported as having keys")
	}

	// Drop a wallet- file: now it counts.
	if err := os.WriteFile(filepath.Join(dir, "wallet-f3aaa.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write wallet file: %v", err)
	}
	if !keystoreHasKeys(dir) {
		t.Errorf("dir with wallet-* file did NOT report as having keys")
	}

	// Backup files should be skipped.
	bakDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bakDir, "wallet-f3aaa.json.bak"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write bak file: %v", err)
	}
	if keystoreHasKeys(bakDir) {
		t.Errorf("dir with only .bak files reported as having keys")
	}

	// Missing dir returns false (not an error).
	if keystoreHasKeys(filepath.Join(dir, "does-not-exist")) {
		t.Errorf("missing dir reported as having keys")
	}
}

func TestResolvePassphrase_EnvSetNonEmpty(t *testing.T) {
	t.Setenv("LANTERN_PASS", "correct horse battery staple")
	dir := t.TempDir()
	got, err := resolvePassphrase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "correct horse battery staple" {
		t.Errorf("got %q, want LANTERN_PASS value", got)
	}
}

func TestResolvePassphrase_EnvExplicitEmpty(t *testing.T) {
	// Capture the warning via the package-level writer override instead of
	// swapping the global os.Stderr (which races with any concurrent
	// logging, e.g. leaked libp2p goroutines, under -race).
	t.Setenv("LANTERN_PASS", "")

	var buf bytes.Buffer
	oldW := passphraseErrW
	passphraseErrW = &buf
	defer func() { passphraseErrW = oldW }()

	dir := t.TempDir()
	got, err2 := resolvePassphrase(dir)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	stderr := buf.String()
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "unencrypted") {
		t.Errorf("expected warning about unencrypted keystore, got: %q", stderr)
	}
}

// Issue #1 (Rabinovitch 2026-06-22): an EMPTY keystore + no TTY + no env var
// is the fresh-install-as-a-service case. A read-serve node has nothing to
// encrypt, so this must NOT hard-error (that broke the systemd/launchd path
// on first boot). It should default to unencrypted and persist the sentinel.
func TestResolvePassphrase_EmptyKeystoreNoTTYDefaultsUnencrypted(t *testing.T) {
	t.Setenv("LANTERN_PASS", "")
	os.Unsetenv("LANTERN_PASS")

	var buf bytes.Buffer
	oldW := passphraseErrW
	passphraseErrW = &buf
	defer func() { passphraseErrW = oldW }()

	dir := t.TempDir() // empty keystore, no keys
	got, err := resolvePassphrase(dir)
	if err != nil {
		t.Fatalf("empty keystore + no TTY should not error, got: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (unencrypted default)", got)
	}
	if !hasUnencryptedSentinel(dir) {
		t.Error("empty keystore + no TTY did not persist the unencrypted sentinel")
	}
}

// SECURITY INVARIANT: when real keys EXIST but there's no TTY and no
// LANTERN_PASS, resolvePassphrase MUST fail loudly. It must never silently
// run with an empty passphrase against real signing keys.
func TestResolvePassphrase_KeysPresentNoTTYErrors(t *testing.T) {
	t.Setenv("LANTERN_PASS", "")
	os.Unsetenv("LANTERN_PASS")

	dir := t.TempDir()
	// Drop a wallet-* key so keystoreHasKeys() == true.
	if err := os.WriteFile(filepath.Join(dir, "wallet-f3aaa.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write wallet file: %v", err)
	}
	_, err := resolvePassphrase(dir)
	if err == nil {
		t.Fatal("expected error when keys present, LANTERN_PASS unset, no TTY, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"LANTERN_PASS", "TTY", "EnvironmentFile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
	// And it must NOT have written a sentinel (real keys stay protected).
	if hasUnencryptedSentinel(dir) {
		t.Error("BUG: wrote unencrypted sentinel for a keystore that HOLDS keys")
	}
}

// Issue #3: a previously-recorded unencrypted choice (sentinel present) must
// make resolvePassphrase silently return "" even with no TTY and no env var,
// so a read-serve chain node does not re-prompt or hard-error on every boot.
func TestResolvePassphrase_SentinelSilentNoTTY(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	dir := t.TempDir()
	markUnencrypted(dir)
	if !hasUnencryptedSentinel(dir) {
		t.Fatal("markUnencrypted did not create the sentinel")
	}
	got, err := resolvePassphrase(dir)
	if err != nil {
		t.Fatalf("sentinel present but got error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string for sentinel path", got)
	}
}

// Issue #3: the explicit-empty LANTERN_PASS path should persist the choice
// (write the sentinel) so later boots without the env var stay silent.
func TestResolvePassphrase_EnvEmptyWritesSentinel(t *testing.T) {
	t.Setenv("LANTERN_PASS", "")
	var buf bytes.Buffer
	oldW := passphraseErrW
	passphraseErrW = &buf
	defer func() { passphraseErrW = oldW }()

	dir := t.TempDir()
	if _, err := resolvePassphrase(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasUnencryptedSentinel(dir) {
		t.Error("explicit-empty LANTERN_PASS did not persist the unencrypted sentinel")
	}
}

// Regression (issue #2): the empty-string fallback must NEVER come back for a
// keystore that HOLDS keys. (An empty keystore legitimately returns "" now —
// it has nothing to protect — so this guard is scoped to the keys-present
// case, which is the one that was a security hole.)
func TestResolvePassphrase_NeverSilentlyEmptyWithKeys(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wallet-f3aaa.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write wallet file: %v", err)
	}
	got, err := resolvePassphrase(dir)
	if err == nil && got == "" {
		t.Fatal("BUG: resolvePassphrase silently returned empty string with keys present -- issue #2 regression")
	}
}
