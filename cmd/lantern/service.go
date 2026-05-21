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
	fs.Parse(args)
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(*binary, *listen)
	case "linux":
		return installSystemd(*binary, *listen)
	default:
		return fmt.Errorf("service install: GOOS=%s not supported (try foreground: `lantern daemon`)", runtime.GOOS)
	}
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

// ---------- macOS launchd ----------

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func installLaunchd(binary, listen string) error {
	dir := dataDir()
	logPath := filepath.Join(dir, "lantern.log")
	errPath := filepath.Join(dir, "lantern.err")
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
`, launchdLabel, binary, listen, dir, logPath, errPath, dir)

	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
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

func installSystemd(binary, listen string) error {
	dir := dataDir()
	unit := fmt.Sprintf(`[Unit]
Description=Lantern Filecoin light node
After=network.target

[Service]
Type=simple
Environment=LANTERN_HOME=%s
ExecStart=%s daemon --listen %s
Restart=on-failure
RestartSec=5s
WorkingDirectory=%s
StandardOutput=append:%s/lantern.log
StandardError=append:%s/lantern.err

[Install]
WantedBy=default.target
`, dir, binary, listen, dir, dir, dir)

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
