package build

import "testing"

// Wall-clock epoch math: Filecoin epochs are exactly scheduled, so the
// expected head epoch is (now - genesis) / 30s. Verified against live
// observations on 2026-07-23: mainnet head 6216984 and calibration head
// 3916343 at ~14:20 UTC (unix 1784816400).
func TestExpectedHeadEpoch(t *testing.T) {
	const obsUnix = 1784816400
	if got := Mainnet.ExpectedHeadEpoch(obsUnix); got < 6216950 || got > 6217050 {
		t.Fatalf("mainnet expected-epoch at %d: got %d, want ~6217000", obsUnix, got)
	}
	if got := Calibration.ExpectedHeadEpoch(obsUnix); got < 3916300 || got > 3916400 {
		t.Fatalf("calibration expected-epoch at %d: got %d, want ~3916334", obsUnix, got)
	}
	// Genesis instant = epoch 0.
	if got := Mainnet.ExpectedHeadEpoch(MainnetGenesisUnix); got != 0 {
		t.Fatalf("mainnet at genesis: got %d, want 0", got)
	}
	// Before genesis = unknown.
	if got := Mainnet.ExpectedHeadEpoch(MainnetGenesisUnix - 1); got != -1 {
		t.Fatalf("mainnet before genesis: got %d, want -1", got)
	}
}
