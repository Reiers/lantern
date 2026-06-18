// Secrets layout + migration + backup — issue #51 Stage 2.
//
// The Stage 1 fix (reset --chain-state) made it *safe* to clear chain
// state. Stage 2 removes the footgun structurally: secrets get their own
// subdirectory, physically separated from rebuildable chain state.
//
// Before Stage 2 (loose in the per-network dir):
//
//	~/.lantern/<net>/keystore/        signing keys
//	~/.lantern/<net>/jwt-secret       RPC auth secret
//	~/.lantern/<net>/token*           pre-minted scope tokens
//	~/.lantern/<net>/headerstore/     chain state (rebuildable)
//	~/.lantern/<net>/bootstrap-anchor.json   chain state (rebuildable)
//
// After Stage 2:
//
//	~/.lantern/<net>/secrets/keystore/
//	~/.lantern/<net>/secrets/jwt-secret
//	~/.lantern/<net>/secrets/token*
//	~/.lantern/<net>/headerstore/             (unchanged)
//	~/.lantern/<net>/bootstrap-anchor.json    (unchanged)
//	~/.lantern/<net>/backups/secrets-*.tar    (rolling, last keepBackups)
//
// Keys are still per-network (a mainnet key can't sign calibration), so
// secrets/ lives under the network dir, not at the top level.
//
// Migration is automatic + idempotent: the first run of a Stage-2 binary
// moves any loose secret files into secrets/. A backup of secrets/ is
// written on every daemon start so even a hand `rm -rf` of the whole
// lantern dir has a same-machine recovery path until the backups go too.

package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Reiers/lantern/build"
)

const (
	secretsDirName = "secrets"
	backupsDirName = "backups"
	keepBackups    = 7
)

// secretFileNames are the loose secret entries that Stage 2 relocates
// into secrets/. keystore is a directory; the rest are files.
var secretFileNames = []string{
	"keystore",
	"jwt-secret",
	"token",
	"token-read",
	"token-sign",
	"token-write",
}

// secretsDir is the per-network secrets directory.
func secretsDir(n build.Network) string {
	return filepath.Join(networkDataDir(n), secretsDirName)
}

// keystorePath returns the canonical keystore directory for a network,
// honouring the Stage-2 layout. After migration this is
// <net>/secrets/keystore; callers must use this instead of hand-joining
// "keystore" onto the network dir.
func keystorePath(n build.Network) string {
	return filepath.Join(secretsDir(n), "keystore")
}

// secretPath returns the path to a named secret file (jwt-secret,
// token*) under the Stage-2 secrets dir.
func secretPath(n build.Network, name string) string {
	return filepath.Join(secretsDir(n), name)
}

// migrateSecretsLayout moves loose secret files from <net>/ into
// <net>/secrets/. Idempotent: if secrets/ already holds a keystore, or
// there are no loose secrets to move, it's a no-op.
//
// Ordering: this must run AFTER migrateLegacyDataDir (which lifts pre-V1.3
// top-level state into <net>/) so all secrets are in <net>/ before we
// relocate them into <net>/secrets/.
func migrateSecretsLayout(n build.Network) error {
	net := networkDataDir(n)
	sdir := secretsDir(n)

	// Already migrated? secrets/keystore present => done.
	if _, err := os.Stat(filepath.Join(sdir, "keystore")); err == nil {
		return nil
	}

	// Is there anything loose to move?
	anyLoose := false
	for _, name := range secretFileNames {
		if _, err := os.Stat(filepath.Join(net, name)); err == nil {
			anyLoose = true
			break
		}
	}
	if !anyLoose {
		return nil // fresh install, or already clean
	}

	if err := os.MkdirAll(sdir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir %s: %w", sdir, err)
	}

	moved := 0
	for _, name := range secretFileNames {
		src := filepath.Join(net, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat %s: %w", src, err)
		}
		dst := filepath.Join(sdir, name)
		// Don't clobber an existing dst (defensive; shouldn't happen
		// since we bailed when secrets/keystore exists).
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("relocate secret %s -> %s: %w", src, dst, err)
		}
		moved++
	}
	if moved > 0 {
		fmt.Fprintf(os.Stderr, "lantern: relocated %d secret(s) into %s (Stage 2 layout)\n", moved, sdir)
		// Leave a breadcrumb so anyone poking the old location knows why
		// it's empty.
		readme := filepath.Join(net, "SECRETS-MOVED.txt")
		_ = os.WriteFile(readme, []byte(
			"Lantern v1.7.19+ moved keystore, jwt-secret, and API tokens into the\n"+
				"secrets/ subdirectory next to this file. This separates your\n"+
				"irreplaceable keys from rebuildable chain state so recovery\n"+
				"operations can never delete them.\n\n"+
				"Your keys are at: "+filepath.Join(sdir, "keystore")+"\n\n"+
				"To recover from a stale/corrupt chain state, use:\n"+
				"  lantern reset --chain-state\n"+
				"NEVER rm -rf anything under ~/.lantern by hand.\n"), 0o600)
	}
	return nil
}

// backupSecrets writes a tar of <net>/secrets/ to
// <net>/backups/secrets-YYYYMMDD-HHMMSS.tar and prunes to the most
// recent keepBackups. Best-effort: a backup failure is logged but never
// blocks daemon start. Returns the path written (empty if skipped).
func backupSecrets(n build.Network) (string, error) {
	sdir := secretsDir(n)
	if fi, err := os.Stat(sdir); err != nil || !fi.IsDir() {
		return "", nil // nothing to back up yet
	}
	// Skip if secrets/ is empty (fresh install before first wallet).
	empty, err := dirEmpty(sdir)
	if err != nil {
		return "", err
	}
	if empty {
		return "", nil
	}

	bdir := filepath.Join(networkDataDir(n), backupsDirName)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return "", fmt.Errorf("create backups dir: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	out := filepath.Join(bdir, "secrets-"+stamp+".tar")

	if err := tarDir(sdir, out); err != nil {
		// Clean a half-written file.
		_ = os.Remove(out)
		return "", err
	}
	if err := os.Chmod(out, 0o600); err != nil {
		return "", err
	}

	pruneBackups(bdir, keepBackups)
	return out, nil
}

func dirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

// tarDir writes a tar archive of the directory tree rooted at src to the
// file at dst. Paths in the archive are relative to src's parent so the
// archive restores as secrets/...
func tarDir(src, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	base := filepath.Dir(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // skip symlinks/sockets/etc.
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

// pruneBackups keeps the most recent `keep` secrets-*.tar files in dir,
// deleting older ones. Best-effort; errors are ignored.
func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var backups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "secrets-") && strings.HasSuffix(name, ".tar") {
			backups = append(backups, name)
		}
	}
	if len(backups) <= keep {
		return
	}
	// Names are timestamp-sortable lexicographically.
	sort.Strings(backups)
	for _, old := range backups[:len(backups)-keep] {
		_ = os.Remove(filepath.Join(dir, old))
	}
}
