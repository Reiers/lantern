// Regression test for lantern#51 embedded-path wiring: the embedded
// pkg/daemon must enable the StaleReset auto-heal so a node offline longer
// than MaxBacktrack re-anchors near live head unattended, instead of
// wedging its head and requiring a manual `lantern reset --chain-state`.
// The standalone cmd/lantern daemon already wired this; pkg/daemon did
// not, which is what surfaced as the "wipe chain state after upgrade"
// workaround for embedded testers.

package daemon

import (
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
)

func TestResolveStaleResetThreshold(t *testing.T) {
	cases := []struct {
		name string
		in   abi.ChainEpoch
		want abi.ChainEpoch
	}{
		{"zero uses default (auto-heal ON)", 0, defaultStaleResetThreshold},
		{"negative disables", -1, 0},
		{"explicit positive kept", 500, 500},
		{"large explicit kept", 10000, 10000},
	}
	for _, tc := range cases {
		if got := resolveStaleResetThreshold(tc.in); got != tc.want {
			t.Errorf("%s: resolveStaleResetThreshold(%d) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// The default must be a real, positive value: a zero here would mean the
// embedded auto-heal is silently OFF (the original bug).
func TestDefaultStaleResetThresholdIsEnabled(t *testing.T) {
	if defaultStaleResetThreshold <= 0 {
		t.Fatalf("embedded StaleReset default must be > 0 (auto-heal enabled), got %d", defaultStaleResetThreshold)
	}
}
