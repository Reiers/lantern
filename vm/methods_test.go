package vm

import (
	"context"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/state/actors"
)

func TestResolveMethod_v18Miner(t *testing.T) {
	mi, err := ResolveMethod(context.Background(), actors.KindMiner, 18, abi.MethodNum(6))
	if err != nil {
		t.Fatalf("resolve miner.PreCommitSector: %v", err)
	}
	if mi.Meta.Name != "PreCommitSector" {
		t.Errorf("name: want PreCommitSector, got %q", mi.Meta.Name)
	}
}

func TestResolveMethod_v17Market(t *testing.T) {
	mi, err := ResolveMethod(context.Background(), actors.KindMarket, 17, abi.MethodNum(4))
	if err != nil {
		t.Fatalf("resolve market.PublishStorageDeals: %v", err)
	}
	if mi.Meta.Name != "PublishStorageDeals" {
		t.Errorf("name: want PublishStorageDeals, got %q", mi.Meta.Name)
	}
}

func TestResolveMethod_UnknownMethod(t *testing.T) {
	_, err := ResolveMethod(context.Background(), actors.KindAccount, 18, abi.MethodNum(99))
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestResolveMethod_UnsupportedVersion(t *testing.T) {
	_, err := ResolveMethod(context.Background(), actors.KindMiner, 8, abi.MethodNum(1))
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
}

func TestResolveMethod_AllKindsV18(t *testing.T) {
	// Smoke test: every kind should have at least its Constructor (m=1)
	// resolvable, except Placeholder which has no methods.
	for _, k := range []actors.Kind{
		actors.KindAccount, actors.KindCron, actors.KindInit, actors.KindMarket,
		actors.KindMiner, actors.KindMultisig, actors.KindPaych, actors.KindPower,
		actors.KindReward, actors.KindSystem, actors.KindVerifreg, actors.KindDatacap,
		actors.KindEvm, actors.KindEam, actors.KindEthAccount,
	} {
		_, err := ResolveMethod(context.Background(), k, 18, abi.MethodNum(1))
		if err != nil && k != actors.KindEthAccount {
			t.Errorf("v18 %s.Constructor (m=1): %v", k, err)
		}
	}
}
