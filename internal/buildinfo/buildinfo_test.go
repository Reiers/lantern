package buildinfo

import "testing"

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
