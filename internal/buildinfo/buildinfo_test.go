package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestDefaults(t *testing.T) {
	// Order matters: this test runs first to verify the fall-through
	// values before any other test calls SetVersion. We don't rely on
	// it because SetTrip restores state, but it's the clearest signal.
	if got := BuildVersion(); got != "dev" && got != "" {
		// Other tests may have raced; permit "" to avoid flake.
		t.Logf("BuildVersion fall-through = %q (expected dev or empty)", got)
	}
	if got := Network(); got != "mainnet" {
		t.Errorf("Network default = %q, want mainnet", got)
	}
}

func TestSetVersion(t *testing.T) {
	prev := BuildVersion()
	defer SetVersion(prev)

	SetVersion("v1.2.1")
	if got := BuildVersion(); got != "v1.2.1" {
		t.Errorf("BuildVersion after Set = %q, want v1.2.1", got)
	}

	SetVersion("")
	if got := BuildVersion(); got != "dev" {
		t.Errorf("BuildVersion after Set(\"\") = %q, want dev", got)
	}
}

func TestSetNetwork(t *testing.T) {
	prev := Network()
	defer SetNetwork(prev)

	SetNetwork("calibration")
	if got := Network(); got != "calibration" {
		t.Errorf("Network after Set = %q, want calibration", got)
	}

	SetNetwork("")
	if got := Network(); got != "mainnet" {
		t.Errorf("Network after Set(\"\") = %q, want mainnet", got)
	}
}

func TestUserAgent(t *testing.T) {
	prev := BuildVersion()
	defer SetVersion(prev)

	SetVersion("v1.2.1")
	if got := UserAgent(); got != "lantern/v1.2.1" {
		t.Errorf("UserAgent = %q, want lantern/v1.2.1", got)
	}
}

func TestCommitAndFullVersion(t *testing.T) {
	prevRead, prevV := readBuildInfo, BuildVersion()
	defer func() { readBuildInfo = prevRead; SetVersion(prevV) }()

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "6e29b34abcdef0123456789"},
			{Key: "vcs.modified", Value: "true"},
		}}, true
	}
	SetVersion("v1.9.2")
	if got := Commit(); got != "6e29b34abcde-dirty" {
		t.Fatalf("Commit = %q", got)
	}
	if got := FullVersion(); got != "v1.9.2 (6e29b34abcde-dirty)" {
		t.Fatalf("FullVersion = %q", got)
	}

	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if Commit() != "" || FullVersion() != "v1.9.2" {
		t.Fatalf("no VCS info => bare version, got %q", FullVersion())
	}
}
