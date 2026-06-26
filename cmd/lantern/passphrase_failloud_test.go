package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #58: an empty passphrase must be refused when the keystore already holds
// keys (possibly funded signing keys), unless explicitly acknowledged.

func writeKey(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "wallet-f3xxx.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write wallet file: %v", err)
	}
}

func TestGuardEmptyPassphrase_NoKeys_OK(t *testing.T) {
	dir := t.TempDir()
	if err := guardEmptyPassphraseWithKeys(dir); err != nil {
		t.Fatalf("fresh keystore should allow empty passphrase, got %v", err)
	}
}

func TestGuardEmptyPassphrase_WithKeys_Refused(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir)
	t.Setenv("LANTERN_ALLOW_EMPTY_PASS", "") // not allowed
	err := guardEmptyPassphraseWithKeys(dir)
	if err == nil {
		t.Fatalf("keystore with keys + empty pass + no opt-in must be refused")
	}
	if !strings.Contains(err.Error(), "#58") {
		t.Fatalf("error should reference #58, got %v", err)
	}
}

func TestGuardEmptyPassphrase_WithKeys_Allowed(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir)
	t.Setenv("LANTERN_ALLOW_EMPTY_PASS", "1")
	if err := guardEmptyPassphraseWithKeys(dir); err != nil {
		t.Fatalf("explicit opt-in should allow empty pass, got %v", err)
	}
}

// The end-to-end resolvePassphrase path: LANTERN_PASS="" on a keystore that
// holds keys must error unless opted in.
func TestResolvePassphrase_ExplicitEmpty_WithKeys_Refused(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir)
	t.Setenv("LANTERN_PASS", "")
	t.Setenv("LANTERN_ALLOW_EMPTY_PASS", "")
	if _, err := resolvePassphrase(dir); err == nil {
		t.Fatalf("expected refusal for empty pass on populated keystore")
	}
	// With opt-in it should pass.
	t.Setenv("LANTERN_ALLOW_EMPTY_PASS", "1")
	if _, err := resolvePassphrase(dir); err != nil {
		t.Fatalf("opt-in should permit empty pass, got %v", err)
	}
}

func TestEmptyPassAllowed_Parsing(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "Yes"} {
		t.Setenv("LANTERN_ALLOW_EMPTY_PASS", v)
		if !emptyPassAllowed() {
			t.Errorf("value %q should enable empty pass", v)
		}
	}
	for _, v := range []string{"", "0", "no", "false"} {
		t.Setenv("LANTERN_ALLOW_EMPTY_PASS", v)
		if emptyPassAllowed() {
			t.Errorf("value %q should NOT enable empty pass", v)
		}
	}
}
