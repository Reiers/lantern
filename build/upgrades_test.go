package build

import (
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/network"
)

func TestUpgradeSchedulesSorted(t *testing.T) {
	for name, s := range map[string][]Upgrade{"mainnet": MainnetUpgrades, "calibration": CalibrationUpgrades} {
		for i := 1; i < len(s); i++ {
			if s[i].Height <= s[i-1].Height {
				t.Errorf("%s: %s (%d) not after %s (%d)", name, s[i].Name, s[i].Height, s[i-1].Name, s[i-1].Height)
			}
			if s[i].Network < s[i-1].Network {
				t.Errorf("%s: %s network version goes backwards", name, s[i].Name)
			}
		}
		if got := LatestNetworkVersion(s); got != network.Version29 {
			t.Errorf("%s: latest = %d, want 29", name, got)
		}
	}
}

// TestUpgradeHeightsPinned pins the heights called out in the NV29 fix
// against lotus@v1.38.3 buildconstants.
func TestUpgradeHeightsPinned(t *testing.T) {
	want := map[string]map[string]abi.ChainEpoch{
		"mainnet": {
			"Hygge": 2683348, "Lightning": 2809800, "Thunder": 2870280, "Watermelon": 3469380,
			"Dragon": 3855360, "Waffle": 4154640, "Tuktuk": 4461240, "Teep": 4878840,
			"Tock": 5138040, "GoldenWeek": 5348280, "FireHorse": 6052800, "Solstice": 6470279,
		},
		"calibration": {
			"Hygge": 322354, "Lightning": 489094, "Thunder": 492214, "Watermelon": 1013134,
			"Dragon": 1427974, "Waffle": 1779094, "Tuktuk": 2078794, "Teep": 2523454,
			"Tock": 2543614, "GoldenWeek": 3007294, "FireHorse": 3694534, "Solstice": 4109133,
		},
	}
	for nw, ups := range map[string][]Upgrade{"mainnet": MainnetUpgrades, "calibration": CalibrationUpgrades} {
		got := map[string]abi.ChainEpoch{}
		for _, u := range ups {
			got[u.Name] = u.Height
		}
		for name, h := range want[nw] {
			if got[name] != h {
				t.Errorf("%s %s: height %d, want %d", nw, name, got[name], h)
			}
		}
	}
}

// TestNetworkVersionAtBoundaries checks Lotus's "H is still the old
// version, H+1 is the new one" rule at every NV18..NV29 boundary.
func TestNetworkVersionAtBoundaries(t *testing.T) {
	for _, tc := range []struct {
		net Network
	}{{Mainnet}, {Calibration}} {
		s := tc.net.UpgradeSchedule()
		prev := GenesisNetworkVersion
		for _, u := range s {
			if u.Network >= network.Version18 {
				if got := NetworkVersionAt(s, u.Height); got != prev {
					t.Errorf("%s %s: at H=%d got nv%d, want nv%d (old)", tc.net, u.Name, u.Height, got, prev)
				}
				if got := NetworkVersionAt(s, u.Height+1); got != u.Network {
					t.Errorf("%s %s: at H+1=%d got nv%d, want nv%d", tc.net, u.Name, u.Height+1, got, u.Network)
				}
			}
			prev = u.Network
		}
	}

	cases := []struct {
		net  Network
		h    abi.ChainEpoch
		want network.Version
	}{
		{Calibration, 4109133, network.Version28},
		{Calibration, 4109134, network.Version29},
		{Calibration, 3694534, network.Version27},
		{Calibration, 3694535, network.Version28},
		{Calibration, 3007294, network.Version26},
		{Calibration, 3007295, network.Version27},
		{Calibration, 322354, network.Version17},
		{Calibration, 322355, network.Version18},
		{Calibration, 1080000, network.Version21}, // between Watermelon fixes
		{Mainnet, 6470279, network.Version28},
		{Mainnet, 6470280, network.Version29},
		{Mainnet, 6052800, network.Version27},
		{Mainnet, 6052801, network.Version28},
		{Mainnet, 5348280, network.Version26},
		{Mainnet, 5348281, network.Version27},
		{Mainnet, 2683349, network.Version18},
		{Mainnet, 0, network.Version0},
		{Mainnet, 1 << 40, network.Version29},
	}
	for _, c := range cases {
		if got := NetworkVersionAt(c.net.UpgradeSchedule(), c.h); got != c.want {
			t.Errorf("%s @%d: got nv%d, want nv%d", c.net, c.h, got, c.want)
		}
	}
	if Devnet.UpgradeSchedule() != nil {
		t.Error("devnet must not have a fixed schedule")
	}
}
