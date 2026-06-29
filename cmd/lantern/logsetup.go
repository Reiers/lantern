package main

import (
	"os"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/term"
)

// setupLogging configures Lantern's log output once, early in main().
//
// go-log is normally driven only by env vars and defaults to a plaintext,
// uncolored encoder. For an operator watching `lantern daemon` in a terminal
// that's harder to scan than it needs to be, so we:
//
//   - default to a COLORIZED encoder when stderr is a TTY (level-colored,
//     so warnings/errors jump out), and plaintext when piped to a file or
//     journald (no escape-code noise in logs),
//   - default the level to INFO if the operator hasn't set one,
//   - always honor explicit operator overrides: GOLOG_LOG_FMT,
//     GOLOG_LOG_LEVEL, and LANTERN_LOG_COLOR (1/true to force on, 0/false to
//     force off) win over the auto-detection.
//
// This is output formatting only. It changes no log content, no levels of
// any specific logger, and nothing about behavior. Safe for every consumer
// (standalone operators, embedders, and clients).
func setupLogging() {
	// If the operator already pinned a format, don't fight them.
	if _, set := os.LookupEnv("GOLOG_LOG_FMT"); !set {
		fmt := "nocolor"
		switch colorPref() {
		case colorOn:
			fmt = "color"
		case colorOff:
			fmt = "nocolor"
		case colorAuto:
			if term.IsTerminal(int(os.Stderr.Fd())) {
				fmt = "color"
			}
		}
		_ = os.Setenv("GOLOG_LOG_FMT", fmt)
	}

	// Default to INFO when unset; an explicit GOLOG_LOG_LEVEL still wins.
	level := "info"
	if v, ok := os.LookupEnv("GOLOG_LOG_LEVEL"); ok && v != "" {
		level = v
	}

	logging.SetupLogging(logging.Config{
		Format: formatFromEnv(),
		Stderr: true,
		Level:  parseLevel(level),
	})
}

type colorChoice int

const (
	colorAuto colorChoice = iota
	colorOn
	colorOff
)

func colorPref() colorChoice {
	switch os.Getenv("LANTERN_LOG_COLOR") {
	case "1", "true", "yes", "on":
		return colorOn
	case "0", "false", "no", "off":
		return colorOff
	default:
		return colorAuto
	}
}

func formatFromEnv() logging.LogFormat {
	switch os.Getenv("GOLOG_LOG_FMT") {
	case "color":
		return logging.ColorizedOutput
	case "json":
		return logging.JSONOutput
	default:
		return logging.PlaintextOutput
	}
}

func parseLevel(s string) logging.LogLevel {
	lvl, err := logging.LevelFromString(s)
	if err != nil {
		return logging.LevelInfo
	}
	return lvl
}
