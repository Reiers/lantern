// Phase 11 Part B — `lantern service` subcommand: cross-platform
// background-daemon lifecycle (macOS launchd / Linux systemd user).
//
// Subcommands:
//
//   lantern service install   — writes the OS service definition
//   lantern service uninstall — removes the definition, stops the service
//   lantern service start     — starts the service
//   lantern service stop      — stops the service
//   lantern service restart   — stop+start
//   lantern service status    — prints OS-reported status
//
// We intentionally keep this thin: launchctl / systemctl do the heavy
// lifting. The shell installer (install.sh) calls into `lantern
// service install` so OS-specific logic lives in exactly one place.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// macOS launchd identifier. Lives in ~/Library/LaunchAgents/.
	launchdLabel = "io.lantern.daemon"
	// Linux systemd unit name. Lives in
	// ~/.config/systemd/user/.
	systemdUnit = "lantern.service"
)

func cmdService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lantern service {install|uninstall|start|stop|restart|status}")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "install":
		return serviceInstall(rest)
	case "uninstall":
		return serviceUninstall(rest)
	case "start":
		return serviceStart(rest)
	case "stop":
		return serviceStop(rest)
	case "restart":
		return serviceRestart(rest)
	case "status":
		return serviceStatus(rest)
	}
	return fmt.Errorf("service: unknown subcommand %q", sub)
}

func serviceInstall(args []string) error {
	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	binary := fs.String("binary", currentBinary(), "Path to the lantern binary")
	listen := fs.String("listen", defaultListen, "RPC listen address")
	passphraseFile := fs.String("passphrase-file", "", "Path to a file containing LANTERN_PASS for an encrypted keystore (chmod 600). When set, the service reads the passphrase from this file instead of running unencrypted.")
	fs.Parse(args)

	pp, err := resolveServicePassphrase(*passphraseFile)
	if err != nil {
		return err
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(*binary, *listen, pp)
	case "linux":
		return installSystemd(*binary, *listen, pp)
	default:
		return fmt.Errorf("service install: GOOS=%s not supported (try foreground: `lantern daemon`)", runtime.GOOS)
	}
}

// servicePassphrase describes how the background service authenticates the
// keystore. Exactly one of the three modes is active.
//
// The daemon's resolvePassphrase() hard-errors when there is no TTY and
// LANTERN_PASS is unset, so a generated service unit MUST supply a passphrase
// decision or the service silently fails to start. That was the tester footgun
// this resolves.
type servicePassphrase struct {
	// envFile, when non-empty, is an EnvironmentFile path the service reads
	// LANTERN_PASS from (encrypted keystore; secret stays out of the unit).
	envFile string
	// inlinePass, when set, is baked into the unit as LANTERN_PASS. Used for
	// the unencrypted-keystore default (value ""), which the daemon treats as
	// an explicit opt-out of encryption.
	inlinePass    string
	inlinePassSet bool
}

// resolveServicePassphrase decides how the service unit will satisfy the
// daemon's keystore-passphrase requirement.
//
//   - --passphrase-file given: the service reads LANTERN_PASS from that file
//     (encrypted keystore, secret never enters the unit).
//   - LANTERN_PASS set in the install environment (e.g. the operator chose a
//     passphrase during install.sh and exported it): preserve it. A non-empty
//     value is written to a 0600 EnvironmentFile so it does not sit in a
//     world-readable unit; an explicit empty value bakes LANTERN_PASS="".
//   - otherwise: default to an unencrypted keystore (LANTERN_PASS=""), which is
//     the right call for a read-only backup chain node that holds no funds.
func resolveServicePassphrase(passphraseFile string) (servicePassphrase, error) {
	if passphraseFile != "" {
		abs, err := filepath.Abs(passphraseFile)
		if err != nil {
			return servicePassphrase{}, fmt.Errorf("resolve --passphrase-file: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return servicePassphrase{}, fmt.Errorf("--passphrase-file %s: %w (create it with `printf %%s 'your-pass' > %s && chmod 600 %s`)", abs, err, abs, abs)
		}
		fmt.Println("  keystore: encrypted — service will read LANTERN_PASS from", abs)
		return servicePassphrase{envFile: abs}, nil
	}

	if env, ok := os.LookupEnv("LANTERN_PASS"); ok {
		if env == "" {
			fmt.Println("  keystore: unencrypted (LANTERN_PASS set empty in install env)")
			return servicePassphrase{inlinePass: "", inlinePassSet: true}, nil
		}
		// Non-empty passphrase in the install environment: persist it to a
		// 0600 EnvironmentFile rather than embedding it in the unit.
		ef := filepath.Join(dataDir(), "service.env")
		if err := os.WriteFile(ef, []byte("LANTERN_PASS="+env+"\n"), 0o600); err != nil {
			return servicePassphrase{}, fmt.Errorf("write service env file %s: %w", ef, err)
		}
		fmt.Println("  keystore: encrypted — wrote passphrase to", ef, "(chmod 600)")
		return servicePassphrase{envFile: ef}, nil
	}

	fmt.Println("  keystore: unencrypted (no --passphrase-file given).")
	fmt.Println("  For an encrypted keystore, reinstall with: lantern service install --passphrase-file <path>")
	return servicePassphrase{inlinePass: "", inlinePassSet: true}, nil
}

func serviceUninstall(_ []string) error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		return fmt.Errorf("service uninstall: GOOS=%s not supported", runtime.GOOS)
	}
}

func serviceStart(_ []string) error {
	switch runtime.GOOS {
	case "darwin":
		return runVerbose("launchctl", "load", "-w", launchdPlistPath())
	case "linux":
		return runVerbose("systemctl", "--user", "start", systemdUnit)
	}
	return fmt.Errorf("service start: GOOS=%s not supported", runtime.GOOS)
}

func serviceStop(_ []string) error {
	switch runtime.GOOS {
	case "darwin":
		return runVerbose("launchctl", "unload", "-w", launchdPlistPath())
	case "linux":
		return runVerbose("systemctl", "--user", "stop", systemdUnit)
	}
	return fmt.Errorf("service stop: GOOS=%s not supported", runtime.GOOS)
}

func serviceRestart(_ []string) error {
	if err := serviceStop(nil); err != nil {
		// ignore stop errors (service may not be loaded)
		fmt.Fprintf(os.Stderr, "stop: %v (continuing)\n", err)
	}
	return serviceStart(nil)
}

func serviceStatus(_ []string) error {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "list", launchdLabel).CombinedOutput()
		fmt.Print(string(out))
		return err
	case "linux":
		out, err := exec.Command("systemctl", "--user", "status", "--no-pager", systemdUnit).CombinedOutput()
		fmt.Print(string(out))
		return err
	}
	return fmt.Errorf("service status: GOOS=%s not supported", runtime.GOOS)
}

// plistEscape XML-escapes a value destined for a <string> element in the
// launchd plist so passphrases containing &, <, > etc. don't corrupt the XML.
func plistEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// ---------- macOS launchd ----------

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func installLaunchd(binary, listen string, pp servicePassphrase) error {
	dir := dataDir()
	logPath := filepath.Join(dir, "lantern.log")
	errPath := filepath.Join(dir, "lantern.err")

	// launchd has no EnvironmentFile equivalent. For the encrypted (envFile)
	// case we read the passphrase out of the file at install time and inline
	// it into the plist (which we then chmod 600). For the unencrypted default
	// we inline LANTERN_PASS="".
	passValue := ""
	switch {
	case pp.envFile != "":
		b, err := os.ReadFile(pp.envFile)
		if err != nil {
			return fmt.Errorf("read passphrase file %s: %w", pp.envFile, err)
		}
		passValue = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "LANTERN_PASS="))
	case pp.inlinePassSet:
		passValue = pp.inlinePass
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>--listen</string>
    <string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>LANTERN_HOME</key>
    <string>%s</string>
    <key>LANTERN_PASS</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>WorkingDirectory</key>
  <string>%s</string>
</dict>
</plist>
`, launchdLabel, binary, listen, dir, plistEscape(passValue), logPath, errPath, dir)

	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	// When the plist carries a real (non-empty) passphrase, keep it 0600 so it
	// is not world-readable. The unencrypted default (empty LANTERN_PASS) can
	// stay 0644 like any other LaunchAgent.
	plistMode := os.FileMode(0o644)
	if passValue != "" {
		plistMode = 0o600
	}
	if err := os.WriteFile(path, []byte(plist), plistMode); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// os.WriteFile does not chmod an existing file; enforce the mode explicitly
	// so a pre-existing plist can't keep looser perms when it now holds a secret.
	if err := os.Chmod(path, plistMode); err != nil {
		return fmt.Errorf("chmod plist: %w", err)
	}
	fmt.Println("Wrote", path)
	// Reload (unload-then-load) so changes take effect.
	_ = exec.Command("launchctl", "unload", "-w", path).Run()
	if err := runVerbose("launchctl", "load", "-w", path); err != nil {
		return err
	}
	fmt.Println("✓ launchd service", launchdLabel, "loaded and running")
	return nil
}

func uninstallLaunchd() error {
	path := launchdPlistPath()
	if _, err := os.Stat(path); err == nil {
		_ = exec.Command("launchctl", "unload", "-w", path).Run()
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove plist: %w", err)
		}
		fmt.Println("✓ Removed", path)
	} else {
		fmt.Println("(no plist at", path, ")")
	}
	return nil
}

// ---------- Linux systemd user ----------

func systemdUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit)
}

func installSystemd(binary, listen string, pp servicePassphrase) error {
	dir := dataDir()

	// Build the keystore-passphrase line for the unit. The daemon refuses to
	// start non-interactively unless LANTERN_PASS is set (encrypted) or
	// explicitly empty (unencrypted opt-out), so one of these is always emitted.
	var passLine string
	switch {
	case pp.envFile != "":
		passLine = fmt.Sprintf("EnvironmentFile=%s", pp.envFile)
	case pp.inlinePassSet:
		// LANTERN_PASS="" -> explicit unencrypted opt-out.
		passLine = fmt.Sprintf("Environment=LANTERN_PASS=%s", pp.inlinePass)
	default:
		passLine = "Environment=LANTERN_PASS="
	}

	unit := fmt.Sprintf(`[Unit]
Description=Lantern Filecoin light node
After=network.target

[Service]
Type=simple
Environment=LANTERN_HOME=%s
%s
ExecStart=%s daemon --listen %s
Restart=on-failure
RestartSec=5s
WorkingDirectory=%s
StandardOutput=append:%s/lantern.log
StandardError=append:%s/lantern.err

[Install]
WantedBy=default.target
`, dir, passLine, binary, listen, dir, dir, dir)

	path := systemdUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir systemd user dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}
	fmt.Println("Wrote", path)
	if err := runVerbose("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runVerbose("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
		return err
	}
	fmt.Println("✓ systemd user unit", systemdUnit, "enabled and started")
	fmt.Println("  Tip: enable lingering so the service survives logout:")
	fmt.Println("    sudo loginctl enable-linger", os.Getenv("USER"))
	return nil
}

func uninstallSystemd() error {
	path := systemdUnitPath()
	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove unit: %w", err)
		}
		fmt.Println("✓ Removed", path)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

// ---------- utils ----------

func currentBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "lantern" // PATH lookup at run time
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe
	}
	return abs
}

func runVerbose(name string, args ...string) error {
	fmt.Printf("  $ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
