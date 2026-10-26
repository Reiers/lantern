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

// --- service install: network + extra daemon flags ---

func TestServiceDaemonArgs(t *testing.T) {
	got, err := serviceDaemonArgs("calibration", "172.17.0.1:1234", []string{"--allow-remote-rpc", "--metrics", "127.0.0.1:9092"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daemon", "--network", "calibration", "--listen", "172.17.0.1:1234", "--allow-remote-rpc", "--metrics", "127.0.0.1:9092"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}

	// No network: daemon default (mainnet), unchanged legacy behaviour.
	got, err = serviceDaemonArgs("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "daemon --listen "+defaultListen {
		t.Errorf("default args = %v", got)
	}

	for _, bad := range [][]string{{"--network", "mainnet"}, {"--listen=0.0.0.0:1"}, {"-network=calibration"}} {
		if _, err := serviceDaemonArgs("calibration", "", bad); err == nil {
			t.Errorf("extra %v: expected error (must be a service install flag)", bad)
		}
	}
	if _, err := serviceDaemonArgs("devnet", "", nil); err == nil {
		t.Error("devnet service: expected error")
	}
	if _, err := serviceDaemonArgs("nope", "", nil); err == nil {
		t.Error("invalid network: expected error")
	}
}

func TestSystemdUnitTextCarriesDaemonArgs(t *testing.T) {
	args, err := serviceDaemonArgs("calibration", "172.17.0.1:1234", []string{"--allow-remote-rpc"})
	if err != nil {
		t.Fatal(err)
	}
	unit := systemdUnitText("/opt/lantern/bin/lantern", args, "/srv/lantern", servicePassphrase{envFile: "/srv/lantern/pass.env"})
	for _, want := range []string{
		"ExecStart=/opt/lantern/bin/lantern daemon --network calibration --listen 172.17.0.1:1234 --allow-remote-rpc\n",
		"Environment=LANTERN_HOME=/srv/lantern\n",
		"EnvironmentFile=/srv/lantern/pass.env\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdQuoteArg(t *testing.T) {
	cases := map[string]string{
		"--allow-remote-rpc": "--allow-remote-rpc",
		"/path with space":   `"/path with space"`,
		"50%":                `"50%%"`,
		"$HOME":              `"$$HOME"`,
		`a"b`:                `"a\"b"`,
		"":                   `""`,
	}
	for in, want := range cases {
		if got := systemdQuoteArg(in); got != want {
			t.Errorf("systemdQuoteArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestLaunchdPlistTextCarriesDaemonArgs(t *testing.T) {
	args, err := serviceDaemonArgs("calibration", "127.0.0.1:1234", []string{"--metrics", "127.0.0.1:9092"})
	if err != nil {
		t.Fatal(err)
	}
	pl := launchdPlistText("/usr/local/bin/lantern", args, "/Users/x/.lantern", "", "/l.log", "/l.err")
	want := "    <string>/usr/local/bin/lantern</string>\n    <string>daemon</string>\n    <string>--network</string>\n    <string>calibration</string>\n    <string>--listen</string>\n    <string>127.0.0.1:1234</string>\n    <string>--metrics</string>\n    <string>127.0.0.1:9092</string>\n  </array>"
	if !strings.Contains(pl, want) {
		t.Errorf("plist ProgramArguments wrong:\n%s", pl)
	}
}
