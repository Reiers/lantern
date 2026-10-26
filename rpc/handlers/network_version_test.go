package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/actors"
)

func nvTestAPI(netName string, epoch abi.ChainEpoch) *ChainAPI {
	root := cid.MustParse("bafy2bzacecotea5anb46fwhsp7sqb7jzqxl7r7heht2q3ycwxfxldc2uqyduk")
	return &ChainAPI{
		Trusted:     &trustedroot.TrustedRoot{Epoch: epoch, StateRoot: root},
		NetworkName: netName,
	}
}

// TestStateNetworkVersion_Boundaries: StateNetworkVersion follows the
// per-network schedule at the head (empty key), with Lotus's "upgrade
// height still reports the old version" rule.
func TestStateNetworkVersion_Boundaries(t *testing.T) {
	cases := []struct {
		net  string
		h    abi.ChainEpoch
		want network.Version
	}{
		{"calibration", 4109133, network.Version28},
		{"calibration", 4109134, network.Version29},
		{"calibration", 3694535, network.Version28},
		{"calibration", 3007295, network.Version27},
		{"calibration", 3007294, network.Version26},
		{"mainnet", 6470279, network.Version28},
		{"mainnet", 6470280, network.Version29},
		{"mainnet", 6052801, network.Version28},
		{"mainnet", 5348281, network.Version27},
		{"", 6470280, network.Version29}, // "" is mainnet
	}
	for _, c := range cases {
		api := nvTestAPI(c.net, c.h)
		got, err := api.StateNetworkVersion(context.Background(), types.EmptyTSK)
		if err != nil {
			t.Fatalf("%s@%d: %v", c.net, c.h, err)
		}
		if got != c.want {
			t.Errorf("%s@%d: got nv%d, want nv%d", c.net, c.h, got, c.want)
		}
	}
}

// TestStateNetworkVersion_TipSetKey resolves the height from a non-empty
// key, and falls back to the head for a key we do not have.
func TestStateNetworkVersion_TipSetKey(t *testing.T) {
	api := nvTestAPI("calibration", 4109134)
	head, err := api.ChainHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.StateNetworkVersion(context.Background(), head.Key())
	if err != nil || got != network.Version29 {
		t.Fatalf("head key: got nv%d err=%v, want nv29", got, err)
	}
	unknown := types.NewTipSetKey(cid.MustParse("bafy2bzacedixrt5tkmgkpgw7p2ueq6cjg44ejhccpdwn33iezceqr5uxslp7s"))
	got, err = api.StateNetworkVersion(context.Background(), unknown)
	if err != nil || got != network.Version29 {
		t.Fatalf("unknown key: got nv%d err=%v, want head fallback nv29", got, err)
	}
}

// TestStateNetworkVersion_NoHead reports the newest scheduled version when
// there is no head to resolve at all.
func TestStateNetworkVersion_NoHead(t *testing.T) {
	api := &ChainAPI{NetworkName: "mainnet"}
	got, err := api.StateNetworkVersion(context.Background(), types.EmptyTSK)
	if err != nil || got != build.LatestNetworkVersion(build.MainnetUpgrades) {
		t.Fatalf("got nv%d err=%v", got, err)
	}
}

func TestStateNetworkVersion_DevnetFallback(t *testing.T) {
	if build.IsDevnetConfigured() {
		t.Skip("devnet config set by another test in this process")
	}
	api := nvTestAPI("devnet", 100)
	got, err := api.StateNetworkVersion(context.Background(), types.EmptyTSK)
	if err != nil || got != network.Version27 {
		t.Fatalf("devnet without config: got nv%d err=%v, want nv27", got, err)
	}
}

func TestNetworkVersionToActorVersion(t *testing.T) {
	for nv, want := range map[network.Version]int{
		network.Version23: 14,
		network.Version24: 15,
		network.Version25: 16,
		network.Version26: 16,
		network.Version27: 17,
		network.Version28: 18,
		network.Version29: 19,
	} {
		if got := networkVersionToActorVersion(nv); got != want {
			t.Errorf("nv%d: got v%d, want v%d", nv, got, want)
		}
	}
	if got := networkVersionToActorVersion(network.Version(999)); got != 0 {
		t.Errorf("unknown nv: got %d, want 0", got)
	}
}

// TestStateActorCodeCIDs_CalibrationNV29 returns the v19 manifest, whose
// EVM code CID is the one that broke eth_getCode in the M2 dry run.
func TestStateActorCodeCIDs_CalibrationNV29(t *testing.T) {
	api := &ChainAPI{NetworkName: "calibrationnet"}
	m, err := api.StateActorCodeCIDs(context.Background(), network.Version29)
	if err != nil {
		t.Fatal(err)
	}
	if got := m["evm"].String(); got != "bafk2bzaceceiey6pgg2qfkqxhqyrlffyylq4gyefkza6uirnowshcuyixoquo" {
		t.Fatalf("calibration nv29 evm code = %s", got)
	}
}

// TestIsDefinitelyNotEVM: only a known non-EVM kind is a definitive "no
// contract". Unknown code CIDs and any other fault must not be.
func TestIsDefinitelyNotEVM(t *testing.T) {
	code := cid.MustParse("bafy2bzacedixrt5tkmgkpgw7p2ueq6cjg44ejhccpdwn33iezceqr5uxslp7s")
	if !isDefinitelyNotEVM(actors.ErrWrongKind{Code: code, Got: actors.KindAccount, Want: actors.KindEvm}, code, "test") {
		t.Error("ErrWrongKind must be definitive")
	}
	if isDefinitelyNotEVM(actors.ErrUnknownCode{Code: code}, code, "test") {
		t.Error("ErrUnknownCode must NOT be definitive")
	}
	if isDefinitelyNotEVM(actors.ErrUnsupportedVersion{Kind: actors.KindEvm, Version: 7}, code, "test") {
		t.Error("ErrUnsupportedVersion must NOT be definitive")
	}
	if isDefinitelyNotEVM(errors.New("fetch evm head: timeout"), code, "test") {
		t.Error("fetch fault must NOT be definitive")
	}
}
