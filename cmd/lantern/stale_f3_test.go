package main

import (
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/anchorverify"
)

// F11: mainnet F3 stalled at instance 466453 / epoch 5824156. Such a cert
// must not be used as the boot-anchor cross-check, because it makes the
// "F3-consistent heaviest" single-source fallback reachable.
func TestDropStaleF3(t *testing.T) {
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	var warned bool
	warn := func(string, ...any) { warned = true }

	stale := anchorverify.F3Finalized{Available: true, Instance: 466453, Epoch: 5824156}
	if got := dropStaleF3(stale, build.Mainnet, now, warn); got.Available || !warned {
		t.Fatalf("stale F3 must be dropped with a warning, got %+v warned=%v", got, warned)
	}

	warned = false
	head := build.Mainnet.ExpectedHeadEpoch(now.Unix())
	fresh := anchorverify.F3Finalized{Available: true, Instance: 500000, Epoch: abi.ChainEpoch(head - 10)}
	if got := dropStaleF3(fresh, build.Mainnet, now, warn); !got.Available || warned {
		t.Fatalf("fresh F3 must be kept silently, got %+v warned=%v", got, warned)
	}

	if got := dropStaleF3(anchorverify.F3Finalized{}, build.Mainnet, now, warn); got.Available {
		t.Fatal("absent F3 stays absent")
	}
}
