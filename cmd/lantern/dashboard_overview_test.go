// Regression tests for issue #12: the /api/dashboard/overview JSON must
// carry anchor_age_seconds and a non-zero-or-explicit f3_instance when
// the daemon has a real TrustedRoot.
//
// The truth source for these fields is the TrustedRoot the daemon
// captures at boot. Anchor age was missing because AcceptedAt was the
// zero time; F3 instance was zero because the boot path never asked an
// F3 cert source. We fixed both in cmd/lantern/main.go::fetchTrustedHead.
// These tests guard the dashboard wiring side: given a TrustedRoot with
// the fields populated, the overview endpoint must surface them.

package main

import (
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
)

// TestDashboardOverview_AnchorAgeAndF3Instance verifies that when the
// daemon's TrustedRoot carries AcceptedAt and F3Instance, the dashboard
// overview JSON exposes anchor_age_seconds and f3_instance.
//
// We exercise dashboardDeps.overview() directly. The host and sync
// dependencies are nil; that's fine because their conditional gates
// skip them when nil.
func TestDashboardOverview_AnchorAgeAndF3Instance(t *testing.T) {
	c, err := cid.Parse("bafy2bzaceauklfgnswur6agiyy4ioghz3fbvl4z6eie2mbtled4pcq6g3csaw")
	if err != nil {
		t.Fatalf("cid.Parse: %v", err)
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:        abi.ChainEpoch(6038996),
		StateRoot:    c,
		TipSetKey:    types.NewTipSetKey(c),
		ParentWeight: big.NewInt(42),
		F3Instance:   466453,
		AcceptedAt:   time.Now().UTC().Add(-29 * time.Second),
	}
	deps := &dashboardDeps{tr: tr}
	out := deps.overview()

	// anchor_age_seconds must be present and roughly 29s.
	v, ok := out["anchor_age_seconds"]
	if !ok {
		t.Fatal("anchor_age_seconds missing from overview JSON")
	}
	age, ok := v.(int64)
	if !ok {
		t.Fatalf("anchor_age_seconds wrong type %T", v)
	}
	if age < 28 || age > 35 {
		t.Errorf("anchor_age_seconds = %d, want ~29", age)
	}

	// f3_instance must be present and equal to TrustedRoot.F3Instance.
	v, ok = out["f3_instance"]
	if !ok {
		t.Fatal("f3_instance missing from overview JSON")
	}
	inst, ok := v.(uint64)
	if !ok {
		t.Fatalf("f3_instance wrong type %T", v)
	}
	if inst != 466453 {
		t.Errorf("f3_instance = %d, want 466453", inst)
	}
}

// TestDashboardOverview_NoAnchorAgeWhenZeroTime: the historical bug was a
// zero AcceptedAt + the dashboard hiding the field. We keep the hide
// behaviour (better than emitting an absurd 1.7-trillion-second age) but
// require that all production daemons stamp AcceptedAt.
func TestDashboardOverview_NoAnchorAgeWhenZeroTime(t *testing.T) {
	c, _ := cid.Parse("bafy2bzaceauklfgnswur6agiyy4ioghz3fbvl4z6eie2mbtled4pcq6g3csaw")
	tr := &trustedroot.TrustedRoot{
		Epoch:     abi.ChainEpoch(1),
		StateRoot: c,
		// AcceptedAt deliberately left at the zero value.
	}
	deps := &dashboardDeps{tr: tr}
	out := deps.overview()
	if _, ok := out["anchor_age_seconds"]; ok {
		t.Error("anchor_age_seconds present with zero AcceptedAt; expected omission")
	}
}
