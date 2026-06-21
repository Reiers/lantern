package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveServicePassphrase is the decision that prevents the tester footgun
// where a generated service unit omits LANTERN_PASS and the daemon then
// hard-errors on a non-TTY start. These tests pin the three modes.

func TestResolveServicePassphrase_DefaultUnencrypted(t *testing.T) {
	t.Setenv("LANTERN_PASS", "") // ensure not inherited
	os.Unsetenv("LANTERN_PASS")

	pp, err := resolveServicePassphrase("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pp.envFile != "" {
		t.Errorf("expected no envFile, got %q", pp.envFile)
	}
	if !pp.inlinePassSet || pp.inlinePass != "" {
		t.Errorf("expected explicit empty inline pass (unencrypted opt-out), got set=%v val=%q", pp.inlinePassSet, pp.inlinePass)
	}
}

func TestResolveServicePassphrase_EnvEmptyOptOut(t *testing.T) {
	t.Setenv("LANTERN_PASS", "")

	pp, err := resolveServicePassphrase("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pp.inlinePassSet || pp.inlinePass != "" {
		t.Errorf("LANTERN_PASS='' should bake an explicit empty pass; got set=%v val=%q", pp.inlinePassSet, pp.inlinePass)
	}
	if pp.envFile != "" {
		t.Errorf("did not expect an envFile for empty opt-out, got %q", pp.envFile)
	}
}

func TestResolveServicePassphrase_PassphraseFile(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	dir := t.TempDir()
	pf := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(pf, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pp, err := resolveServicePassphrase(pf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pp.envFile == "" {
		t.Fatal("expected envFile to be set")
	}
	if pp.inlinePassSet {
		t.Error("must not inline a passphrase when using a file")
	}
	abs, _ := filepath.Abs(pf)
	if pp.envFile != abs {
		t.Errorf("envFile = %q, want abs %q", pp.envFile, abs)
	}
}

func TestResolveServicePassphrase_MissingFileErrors(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	_, err := resolveServicePassphrase("/no/such/passphrase/file")
	if err == nil {
		t.Fatal("expected error for missing --passphrase-file, got nil")
	}
}

func TestResolveServicePassphrase_EnvNonEmptyWritesFile(t *testing.T) {
	os.Unsetenv("LANTERN_PASS")
	t.Setenv("LANTERN_PASS", "topsecret")
	// dataDir() honors LANTERN_HOME for the service.env write target.
	home := t.TempDir()
	t.Setenv("LANTERN_HOME", home)

	pp, err := resolveServicePassphrase("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pp.envFile == "" {
		t.Fatal("non-empty LANTERN_PASS should be persisted to an env file, not inlined")
	}
	if pp.inlinePassSet {
		t.Error("must not inline a non-empty passphrase")
	}
	b, err := os.ReadFile(pp.envFile)
	if err != nil {
		t.Fatalf("read written env file: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "LANTERN_PASS=topsecret" {
		t.Errorf("env file contents = %q, want LANTERN_PASS=topsecret", got)
	}
	// Must be 0600.
	fi, err := os.Stat(pp.envFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file perm = %o, want 600", perm)
	}
}

func TestPlistEscape(t *testing.T) {
	cases := map[string]string{
		"plain":       "plain",
		"a&b":         "a&amp;b",
		"a<b>c":       "a&lt;b&gt;c",
		"&<>":         "&amp;&lt;&gt;",
		"no-specials": "no-specials",
	}
	for in, want := range cases {
		if got := plistEscape(in); got != want {
			t.Errorf("plistEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
