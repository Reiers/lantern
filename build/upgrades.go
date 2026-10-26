package build

// Network upgrade schedules (epoch -> network version) for mainnet and
// calibration.
//
// Provenance: heights from filecoin-project/lotus@v1.38.3
// build/buildconstants/params_mainnet.go and params_calibnet.go; the
// upgrade -> network-version mapping from chain/consensus/filcns/upgrades.go
// at the same tag. Lotus dual-licensed Apache-2.0 / MIT.
//
// Semantics mirror Lotus's StateManager.GetNetworkVersion exactly: an
// upgrade at height H is applied while computing the state of epoch H+1,
// so epoch H itself still reports the PREVIOUS network version and H+1 is
// the first epoch at the new version. Upgrades with a negative height are
// disabled in Lotus (skipped) and are simply omitted here.
//
// When a new network upgrade ships: add its row here (and bump the actors
// bundle table, see state/actors/internal/genbundles).

import (
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/network"
)

// Upgrade is one network-version-changing (or same-version fix) upgrade.
type Upgrade struct {
	Name    string
	Height  abi.ChainEpoch // last epoch at the previous version
	Network network.Version
}

// UpgradeScheduleLotusVersion is the Lotus release the schedules mirror.
const UpgradeScheduleLotusVersion = "v1.38.3"

// GenesisNetworkVersion is the network version before the first upgrade
// (network.Version0 on both mainnet and calibration).
const GenesisNetworkVersion = network.Version0

const epochsInHour = 120
const epochsInDay = 2880

// MainnetUpgrades is the mainnet schedule, ascending by height.
var MainnetUpgrades = []Upgrade{
	{"Breeze", 41280, network.Version1},
	{"Smoke", 51000, network.Version2},
	{"Ignition", 94000, network.Version3},
	{"Refuel", 130800, network.Version3},
	{"Assembly", 138720, network.Version4},
	{"Tape", 140760, network.Version5},
	{"Liftoff", 148888, network.Version5},
	{"Kumquat", 170000, network.Version6},
	{"Calico", 265200, network.Version7},
	{"Persian", 265200 + epochsInHour*60, network.Version8},
	{"Orange", 336458, network.Version9},
	{"Trust", 550321, network.Version10},
	{"Norwegian", 665280, network.Version11},
	{"Turbo", 712320, network.Version12},
	{"Hyperdrive", 892800, network.Version13},
	{"Chocolate", 1231620, network.Version14},
	{"OhSnap", 1594680, network.Version15},
	{"Skyr", 1960320, network.Version16},
	{"Shark", 2383680, network.Version17},
	{"Hygge", 2683348, network.Version18},
	{"Lightning", 2809800, network.Version19},
	{"Thunder", 2809800 + epochsInDay*21, network.Version20},
	{"Watermelon", 3469380, network.Version21},
	{"Dragon", 3855360, network.Version22},
	{"Waffle", 4154640, network.Version23},
	{"Tuktuk", 4461240, network.Version24},
	{"Teep", 4878840, network.Version25},
	{"Tock", 4878840 + epochsInDay*90, network.Version26},
	{"GoldenWeek", 5348280, network.Version27},
	{"FireHorse", 6052800, network.Version28},
	{"Solstice", 6470279, network.Version29},
}

// CalibrationUpgrades is the calibration schedule, ascending by height.
// Breeze, Smoke, Ignition, Refuel and Liftoff have negative (disabled)
// heights on calibration and are omitted, as Lotus skips them.
var CalibrationUpgrades = []Upgrade{
	{"Assembly", 30, network.Version4},
	{"Tape", 60, network.Version5},
	{"Kumquat", 90, network.Version6},
	{"Calico", 120, network.Version7},
	{"Persian", 120 + epochsInHour*1, network.Version8},
	{"Orange", 300, network.Version9},
	{"Trust", 330, network.Version10},
	{"Norwegian", 360, network.Version11},
	{"Turbo", 390, network.Version12},
	{"Hyperdrive", 420, network.Version13},
	{"Chocolate", 450, network.Version14},
	{"OhSnap", 480, network.Version15},
	{"Skyr", 510, network.Version16},
	{"Shark", 16800, network.Version17},
	{"Hygge", 322354, network.Version18},
	{"Lightning", 489094, network.Version19},
	{"Thunder", 489094 + 3120, network.Version20},
	{"Watermelon", 1013134, network.Version21},
	{"WatermelonFix", 1070494, network.Version21},
	{"WatermelonFix2", 1108174, network.Version21},
	{"Dragon", 1427974, network.Version22},
	{"CalibrationDragonFix", 1493854, network.Version22},
	{"Waffle", 1779094, network.Version23},
	{"Tuktuk", 2078794, network.Version24},
	{"Teep", 2523454, network.Version25},
	{"Tock", 2523454 + epochsInDay*7, network.Version26},
	{"TockFix", 2558014, network.Version26},
	{"GoldenWeek", 3007294, network.Version27},
	{"FireHorse", 3694534, network.Version28},
	{"Solstice", 4109133, network.Version29},
}

// UpgradeSchedule returns the upgrade schedule for n, or nil for networks
// without a fixed schedule (devnet).
func (n Network) UpgradeSchedule() []Upgrade {
	switch n {
	case Mainnet:
		return MainnetUpgrades
	case Calibration:
		return CalibrationUpgrades
	}
	return nil
}

// NetworkVersionAt returns the network version at epoch h under schedule,
// mirroring Lotus: the version of the last upgrade whose Height is strictly
// below h, or GenesisNetworkVersion if none.
func NetworkVersionAt(schedule []Upgrade, h abi.ChainEpoch) network.Version {
	v := GenesisNetworkVersion
	for _, u := range schedule {
		if h <= u.Height {
			break
		}
		v = u.Network
	}
	return v
}

// LatestNetworkVersion returns the network version after the last upgrade
// in schedule.
func LatestNetworkVersion(schedule []Upgrade) network.Version {
	if len(schedule) == 0 {
		return GenesisNetworkVersion
	}
	return schedule[len(schedule)-1].Network
}
