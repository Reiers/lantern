package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reiers/lantern/build"
)

// withHome points LANTERN_HOME at a temp dir for the duration of a test.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LANTERN_HOME", home)
	return home
}

// TestMigrateSecretsLayout: loose secrets in <net>/ are relocated into
// <net>/secrets/, idempotently, without touching chain state.
func TestMigrateSecretsLayout(t *testing.T) {
	home := withHome(t)
	net := build.Calibration
	netDir := filepath.Join(home, string(net))

	// Seed an old-layout install: loose secrets + chain state.
	mustMkdir(t, filepath.Join(netDir, "keystore"))
	mustWrite(t, filepath.Join(netDir, "keystore", "wallet-f1abc"), "PRIVKEY")
	mustWrite(t, filepath.Join(netDir, "jwt-secret"), "JWTSECRET")
	mustWrite(t, filepath.Join(netDir, "token"), "ADMINTOKEN")
	mustWrite(t, filepath.Join(netDir, "token-read"), "READTOKEN")
	// chain state that must NOT move:
	mustMkdir(t, filepath.Join(netDir, "headerstore"))
	mustWrite(t, filepath.Join(netDir, "headerstore", "000.vlog"), "badger")
	mustWrite(t, filepath.Join(netDir, "bootstrap-anchor.json"), "{}")

	if err := migrateSecretsLayout(net); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Secrets moved.
	assertContent(t, filepath.Join(netDir, "secrets", "keystore", "wallet-f1abc"), "PRIVKEY")
	assertContent(t, filepath.Join(netDir, "secrets", "jwt-secret"), "JWTSECRET")
	assertContent(t, filepath.Join(netDir, "secrets", "token"), "ADMINTOKEN")
	assertContent(t, filepath.Join(netDir, "secrets", "token-read"), "READTOKEN")

	// Old loose locations gone.
	assertAbsent(t, filepath.Join(netDir, "keystore"))
	assertAbsent(t, filepath.Join(netDir, "jwt-secret"))
	assertAbsent(t, filepath.Join(netDir, "token"))

	// Chain state untouched.
	assertContent(t, filepath.Join(netDir, "headerstore", "000.vlog"), "badger")
	assertContent(t, filepath.Join(netDir, "bootstrap-anchor.json"), "{}")

	// Breadcrumb left behind.
	if _, err := os.Stat(filepath.Join(netDir, "SECRETS-MOVED.txt")); err != nil {
		t.Errorf("expected SECRETS-MOVED.txt breadcrumb: %v", err)
	}

	// Idempotent: second run is a no-op and doesn't error.
	if err := migrateSecretsLayout(net); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	assertContent(t, filepath.Join(netDir, "secrets", "keystore", "wallet-f1abc"), "PRIVKEY")
}

// TestMigrateSecretsLayoutFreshInstall: nothing to move => no secrets dir
// is created prematurely (init creates it).
func TestMigrateSecretsLayoutFreshInstall(t *testing.T) {
	home := withHome(t)
	net := build.Calibration
	if err := migrateSecretsLayout(net); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertAbsent(t, filepath.Join(home, string(net), "secrets"))
}

// TestBackupSecrets: a tar of secrets/ is written, restorable, and the
// keep-N pruning works.
func TestBackupSecrets(t *testing.T) {
	home := withHome(t)
	net := build.Calibration
	sdir := secretsDir(net)
	mustMkdir(t, filepath.Join(sdir, "keystore"))
	mustWrite(t, filepath.Join(sdir, "keystore", "wallet-x"), "KEYDATA")
	mustWrite(t, filepath.Join(sdir, "jwt-secret"), "JWT")

	path, err := backupSecrets(net)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if path == "" {
		t.Fatal("expected a backup path")
	}
	// Backup contains the key.
	if !tarContains(t, path, "secrets/keystore/wallet-x", "KEYDATA") {
		t.Errorf("backup tar missing keystore/wallet-x with right content")
	}

	_ = home
	// Pruning: write keepBackups+3 stamped files, ensure only keepBackups remain.
	bdir := filepath.Join(networkDataDir(net), backupsDirName)
	for i := 0; i < keepBackups+3; i++ {
		// Distinct, sortable names.
		name := filepath.Join(bdir, "secrets-2020010"+string(rune('0'+i%10))+"-00000"+string(rune('0'+i))+".tar")
		mustWrite(t, name, "x")
	}
	pruneBackups(bdir, keepBackups)
	entries, _ := os.ReadDir(bdir)
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "secrets-") && strings.HasSuffix(e.Name(), ".tar") {
			count++
		}
	}
	if count != keepBackups {
		t.Errorf("after prune want %d backups, got %d", keepBackups, count)
	}
}

// TestBackupSecretsEmpty: an empty/absent secrets dir produces no backup.
func TestBackupSecretsEmpty(t *testing.T) {
	withHome(t)
	net := build.Calibration
	// No secrets dir at all.
	if p, err := backupSecrets(net); err != nil || p != "" {
		t.Fatalf("absent secrets: want (\"\", nil), got (%q, %v)", p, err)
	}
	// Empty secrets dir.
	mustMkdir(t, secretsDir(net))
	if p, err := backupSecrets(net); err != nil || p != "" {
		t.Fatalf("empty secrets: want (\"\", nil), got (%q, %v)", p, err)
	}
}

// TestKeystorePath ensures the canonical keystore path is under secrets/.
func TestKeystorePath(t *testing.T) {
	withHome(t)
	got := keystorePath(build.Mainnet)
	want := filepath.Join(networkDataDir(build.Mainnet), "secrets", "keystore")
	if got != want {
		t.Errorf("keystorePath = %q, want %q", got, want)
	}
}

// ---- helpers ----

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s: content %q, want %q", path, string(b), want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected %s to be absent, but it exists", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func tarContains(t *testing.T, tarPath, wantName, wantContent string) bool {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == wantName {
			b, _ := io.ReadAll(tr)
			return string(b) == wantContent
		}
	}
	return false
}
