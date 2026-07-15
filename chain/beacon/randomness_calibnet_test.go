package beacon_test

import (
	"testing"

	"github.com/Reiers/lantern/chain/beacon"
)

// TestCalibnetQuicknet_LiveData_2026_07_15 pins the calibration params
// against a real observed tipset (fil epoch 3893385, drand round 30441512)
// so any future regression in FilecoinGenesisTime is caught at build time.
func TestCalibnetQuicknet_LiveData_2026_07_15(t *testing.T) {
	p := beacon.CalibnetQuicknetParams()
	got := p.MaxBeaconRoundForEpoch(3893385)
	if got != 30441512 {
		t.Fatalf("MaxBeaconRoundForEpoch(3893385) = %d, want 30441512", got)
	}
}
