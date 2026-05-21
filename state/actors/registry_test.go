package actors

import (
	"testing"

	"github.com/ipfs/go-cid"
)

func TestRegistryMainnetV18Miner(t *testing.T) {
	reg := DefaultRegistry()

	// Lifted from lotus@v1.36.0 build/builtin_actors_gen.go, mainnet v18.
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
	// table is complete. (Lifted from lotus@v1.36.0.)
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
