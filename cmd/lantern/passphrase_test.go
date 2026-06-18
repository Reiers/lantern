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

func TestResolvePassphrase_NoEnvNoTTY(t *testing.T) {
	// Make sure LANTERN_PASS is unset for this test.
	t.Setenv("LANTERN_PASS", "")
	os.Unsetenv("LANTERN_PASS")

	// In a `go test` run, stdin is not a terminal, so we hit the no-TTY
	// path directly. resolvePassphrase MUST return an error here.
	dir := t.TempDir()
	_, err := resolvePassphrase(dir)
	if err == nil {
		t.Fatal("expected error when LANTERN_PASS unset and no TTY, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"LANTERN_PASS", "TTY", "EnvironmentFile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}

// Regression: the empty-string fallback that #2 fixes must NOT come back.
// If anyone ever changes resolvePassphrase to silently return "" when env
// is unset and there's no TTY, this test will catch it.
func TestResolvePassphrase_NeverSilentlyEmpty(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	dir := t.TempDir()
	got, err := resolvePassphrase(dir)
	if err == nil && got == "" {
		t.Fatal("BUG: resolvePassphrase silently returned empty string -- issue #2 regression")
	}
}
