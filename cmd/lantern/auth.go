// `lantern auth` subcommands: rotate + list.
//
// Issue #7. JWT tokens issued before #7 had no exp claim; after #7 every
// token carries an explicit expiry. This file provides the operator path
// for inspecting current token expiry and for rotating the secret +
// reissuing every scope token in one shot.

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/filecoin-project/go-jsonrpc/auth"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/rpc/server"
)

// cmdAuth dispatches `lantern auth <subcommand>`.
func cmdAuth(args []string) error {
	if len(args) < 1 {
		return authUsage()
	}
	switch args[0] {
	case "rotate":
		return cmdAuthRotate(args[1:])
	case "list", "ls", "status":
		return cmdAuthList(args[1:])
	case "help", "--help", "-h":
		return authUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown auth subcommand: %s\n", args[0])
		return authUsage()
	}
}

func authUsage() error {
	fmt.Fprintln(os.Stderr, `Usage:
  lantern auth rotate    Issue a new JWT secret and rewrite all scope tokens.
                         Every previously-issued token becomes invalid.
  lantern auth list      Show currently-installed scope tokens + their expiry.

Environment:
  LANTERN_HOME    Override the data directory (default ~/.lantern)`)
	return nil
}

// cmdAuthRotate: rotate the secret + reissue every scope token.
func cmdAuthRotate(args []string) error {
	fs := flag.NewFlagSet("auth rotate", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "Skip the destructive-action confirmation prompt.")
	network := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := authSecretsDir(*network)
	if err != nil {
		return err
	}

	if !*yes {
		fmt.Printf("This will invalidate every existing Lantern JWT token under %s.\n", dir)
		fmt.Print("Continue? [y/N] ")
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" && ans != "yes" {
			return errors.New("rotate cancelled")
		}
	}

	a, err := server.LoadOrInitAuth(dir, nil)
	if err != nil {
		return fmt.Errorf("load auth: %w", err)
	}
	if err := a.Rotate(dir); err != nil {
		return err
	}

	fmt.Println("✓ JWT secret rotated; new scope tokens written:")
	fmt.Println("    " + filepath.Join(dir, "token"))
	fmt.Println("    " + filepath.Join(dir, "token-sign"))
	fmt.Println("    " + filepath.Join(dir, "token-write"))
	fmt.Println("    " + filepath.Join(dir, "token-read"))
	fmt.Println()
	fmt.Println("All previously-issued tokens are now invalid. Update FULLNODE_API_INFO consumers.")
	return nil
}

// cmdAuthList: dump every on-disk token + its claims.
func cmdAuthList(args []string) error {
	fs := flag.NewFlagSet("auth list", flag.ContinueOnError)
	network := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := authSecretsDir(*network)
	if err != nil {
		return err
	}
	a, err := server.LoadOrInitAuth(dir, nil)
	if err != nil {
		return fmt.Errorf("load auth: %w", err)
	}

	files := []string{"token", "token-sign", "token-write", "token-read"}
	type row struct {
		file    string
		perms   []auth.Permission
		issued  time.Time
		expires time.Time
		jti     string
		legacy  bool
	}
	rows := make([]row, 0, len(files))
	now := time.Now()

	for _, fname := range files {
		path := filepath.Join(dir, fname)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		info, err := a.Inspect(string(b))
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: %s: %v\n", fname, err)
			continue
		}
		rows = append(rows, row{
			file:    fname,
			perms:   info.Perms,
			issued:  info.IssuedAt,
			expires: info.Expires,
			jti:     info.JTI,
			legacy:  info.Legacy,
		})
	}
	if len(rows) == 0 {
		fmt.Println("No tokens found under", dir)
		return nil
	}
	// Stable order: token (admin) first, then progressively-less-privileged.
	order := map[string]int{"token": 0, "token-sign": 1, "token-write": 2, "token-read": 3}
	sort.SliceStable(rows, func(i, j int) bool { return order[rows[i].file] < order[rows[j].file] })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FILE\tSCOPE\tISSUED\tEXPIRES\tSTATUS\tJTI")
	for _, r := range rows {
		scope := fmt.Sprintf("%v", r.perms)
		issued := "—"
		expires := "—"
		status := "ok"
		if r.legacy {
			status = "legacy (no exp; rotate)"
		} else {
			if !r.issued.IsZero() {
				issued = r.issued.Format("2006-01-02")
			}
			if !r.expires.IsZero() {
				expires = r.expires.Format("2006-01-02")
				if now.After(r.expires) {
					status = "EXPIRED"
				} else if r.expires.Sub(now) < 14*24*time.Hour {
					status = "expiring soon"
				}
			}
		}
		jti := r.jti
		if len(jti) > 8 {
			jti = jti[:8] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.file, scope, issued, expires, status, jti)
	}
	w.Flush()
	return nil
}

// resolveDataDir picks the Lantern data directory. Mirrors what `lantern
// daemon` does so `auth` commands operate on the same files the running
// daemon reads.
func resolveDataDir() (string, error) {
	if d := os.Getenv("LANTERN_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lantern"), nil
}

// authSecretsDir resolves the Stage-2 secrets directory for a network,
// running the legacy + secrets migrations first so `auth` operates on the
// same jwt-secret + tokens the daemon uses. This fixes a pre-Stage-2 bug
// where `auth rotate` wrote to the top-level ~/.lantern while the daemon
// read from ~/.lantern/<net>/.
func authSecretsDir(networkStr string) (string, error) {
	n := build.Network(networkStr)
	if !n.Valid() {
		return "", fmt.Errorf("invalid --network %q: want one of mainnet, calibration", networkStr)
	}
	if err := migrateLegacyDataDir(n); err != nil {
		return "", fmt.Errorf("migrate legacy data dir: %w", err)
	}
	if err := migrateSecretsLayout(n); err != nil {
		return "", fmt.Errorf("migrate secrets layout: %w", err)
	}
	return secretsDir(n), nil
}
