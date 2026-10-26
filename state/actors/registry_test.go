package actors

import (
	"testing"

	gstactors "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"
)

func TestRegistryMainnetV18Miner(t *testing.T) {
	reg := DefaultRegistry()

	// Lifted from lotus@v1.38.3 build/builtin_actors_gen.go, mainnet v18.
	mainnetV18MinerCID := cid.MustParse("bafk2bzacead23tnh3ywx2cwudftngssfjgptsvr3nao7xv6mcfot7gvju4bak")
	info, ok := reg.Lookup(mainnetV18MinerCID)
	if !ok {
		t.Fatalf("mainnet v18 miner CID not found in registry")
	}
	if info.Kind != KindMiner {
		t.Errorf("expected KindMiner, got %s", info.Kind)
	}
	if info.Version != 18 {
		t.Errorf("expected version 18, got %d", info.Version)
	}
	if info.Network != "mainnet" {
		t.Errorf("expected mainnet, got %s", info.Network)
	}
}

func TestRegistryAllKnownActors(t *testing.T) {
	reg := DefaultRegistry()

	// Pick one well-known mainnet v17 actor of each kind to confirm the
	// table is complete. (Lifted from lotus@v1.38.3.)
	tests := []struct {
		kind Kind
		c    string
	}{
		{KindMiner, "bafk2bzaceautzxqsrstcxerpxtykn4syogslbiwpsfoh562jex262vxeluc4w"},
		{KindMarket, "bafk2bzacebsnn4nk5crrlrvhg5vdpaxsrs4r72etaofxdi7tucr72om22z6a4"},
		{KindPower, "bafk2bzacedyhaec4jvpdmaas6pgtoj7zkdlmdpljz7yjwjqtkfmwv7yb5invw"},
		{KindInit, "bafk2bzacecp5go2numz52kerspigi2e3rygesaqeqhn4gegmfgr5xoon73sde"},
		{KindVerifreg, "bafk2bzaceak2iqpfy4hw6xyyrf7c4yfh7pl4copzm7t63mokecsxfcnybxnd2"},
		{KindDatacap, "bafk2bzaceakb5v267o4y6jq3vao4b5c63sjjk3sr2jgjoabtze7ygcvbpvc6i"},
		{KindReward, "bafk2bzacebezdh75otifygspbfymgeipv34v6feti5xylrxt7xetu77pisnym"},
		{KindSystem, "bafk2bzacebf4et7elmttbioggwlpmsjhplcnzfxtmi4lnbtqr3f6tzuwsoj2a"},
		{KindAccount, "bafk2bzaceb4as5yyhjfkvxgooum37uvm5gbjr4dtbpxmqnpvvbjfpu5qouii4"},
		{KindMultisig, "bafk2bzaceblf5vqw4dwjueoetgawhg7t6he7qhdnfy3shf7ufnfv4mkwchgbm"},
	}
	for _, tc := range tests {
		c := cid.MustParse(tc.c)
		info, ok := reg.Lookup(c)
		if !ok {
			t.Errorf("%s actor CID %s not found", tc.kind, tc.c)
			continue
		}
		if info.Kind != tc.kind {
			t.Errorf("CID %s: expected kind %s, got %s", tc.c, tc.kind, info.Kind)
		}
		if info.Version != 17 {
			t.Errorf("CID %s: expected version 17, got %d", tc.c, info.Version)
		}
	}
}

func TestRegistryUnknownCID(t *testing.T) {
	reg := DefaultRegistry()
	// A made-up CID that won't be in any bundle.
	c := cid.MustParse("bafy2bzacedixrt5tkmgkpgw7p2ueq6cjg44ejhccpdwn33iezceqr5uxslp7s")
	if _, ok := reg.Lookup(c); ok {
		t.Errorf("expected lookup miss for unknown CID, got hit")
	}
}

func TestRegisterCodeCIDs_DevnetBundle(t *testing.T) {
	r := DefaultRegistry()
	devCode := cid.MustParse("bafk2bzaceal437l2hwjynf3pzvjbtnwlqn7p5gibdf7rkrauk6cnnwez7jtmw")
	// Unknown before registration.
	if _, ok := r.Lookup(devCode); ok {
		t.Fatal("devnet code CID unexpectedly known before registration")
	}
	n := r.RegisterCodeCIDs(18, "devnet", map[string]cid.Cid{
		"storagepower": devCode,
	})
	if n != 1 {
		t.Fatalf("registered %d code CIDs, want 1", n)
	}
	ci, ok := r.Lookup(devCode)
	if !ok {
		t.Fatal("devnet code CID not resolvable after registration")
	}
	if ci.Kind != KindPower || ci.Version != 18 || ci.Network != "devnet" {
		t.Fatalf("got %+v, want {power,18,devnet}", ci)
	}
}

func TestRegisterCodeCIDs_DoesNotOverrideExisting(t *testing.T) {
	r := DefaultRegistry()
	// Pick a real known code CID (calibration v18 power) and try to
	// re-register it under a bogus kind; must be refused (first-seen wins).
	var known cid.Cid
	var knownInfo CodeInfo
	for code, info := range r.byCode {
		known, knownInfo = code, info
		break
	}
	n := r.RegisterCodeCIDs(99, "devnet", map[string]cid.Cid{"bogus": known})
	if n != 0 {
		t.Fatalf("re-registered %d existing CIDs, want 0 (must not override)", n)
	}
	if got, _ := r.Lookup(known); got != knownInfo {
		t.Fatalf("existing entry mutated: got %+v want %+v", got, knownInfo)
	}
}

// TestRegistryCalibrationV19EVM pins the calibration actors v19 (NV29,
// Solstice) EVM code CID that broke eth_getCode in the M2 dry run: Lantern
// v1.10.0 did not know it, LoadEVM failed with ErrUnknownCode and every
// FEVM contract was reported as having no code.
func TestRegistryCalibrationV19EVM(t *testing.T) {
	reg := DefaultRegistry()
	c := cid.MustParse("bafk2bzaceceiey6pgg2qfkqxhqyrlffyylq4gyefkza6uirnowshcuyixoquo")
	info, ok := reg.Lookup(c)
	if !ok {
		t.Fatal("calibration v19 EVM code CID not in registry")
	}
	if info.Kind != KindEvm || info.Version != 19 || info.Network != "calibrationnet" {
		t.Fatalf("got %+v, want {evm 19 calibrationnet}", info)
	}
}

// TestRegistryMainnetV19EVM pins the mainnet actors v19 EVM code CID.
func TestRegistryMainnetV19EVM(t *testing.T) {
	reg := DefaultRegistry()
	c := cid.MustParse("bafk2bzaceaufbdpwqvu6r7bbfpx4w4fw3cub27lm45wapyjfaiwesgvkrw5lm")
	info, ok := reg.Lookup(c)
	if !ok {
		t.Fatal("mainnet v19 EVM code CID not in registry")
	}
	if info.Kind != KindEvm || info.Version != 19 || info.Network != "mainnet" {
		t.Fatalf("got %+v, want {evm 19 mainnet}", info)
	}
}

// TestBundlesCoverEveryVersion asserts both networks carry every actors
// version from 8 to the newest one go-state-types maps a network version
// to, so a go-state-types bump without a bundles regen fails loudly here.
func TestBundlesCoverEveryVersion(t *testing.T) {
	latest, err := gstactors.VersionForNetwork(network.Version29)
	if err != nil {
		t.Fatal(err)
	}
	if int(latest) != BundlesMaxActorsVersion {
		t.Fatalf("NV29 maps to actors v%d but bundles max is v%d", latest, BundlesMaxActorsVersion)
	}
	have := map[string]map[int]bool{}
	for _, b := range Bundles {
		if have[b.Network] == nil {
			have[b.Network] = map[int]bool{}
		}
		have[b.Network][b.Version] = true
	}
	for _, nw := range []string{"mainnet", "calibrationnet"} {
		for v := 8; v <= BundlesMaxActorsVersion; v++ {
			if !have[nw][v] {
				t.Errorf("%s: missing actors v%d bundle", nw, v)
			}
		}
	}
}

func TestWarnUnknownCodeOnce(t *testing.T) {
	c := cid.MustParse("bafy2bzacedixrt5tkmgkpgw7p2ueq6cjg44ejhccpdwn33iezceqr5uxslp7s")
	if !WarnUnknownCodeOnce(c, "test") {
		t.Fatal("first call should emit the warning")
	}
	if WarnUnknownCodeOnce(c, "test") {
		t.Fatal("second call for the same CID must stay quiet")
	}
}
