package headcheck

import (
	"context"
	"errors"
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// mockSource is a test HeadSource.
type mockSource struct {
	name  string
	kind  bootstrap.Kind
	epoch abi.ChainEpoch
	err   error
}

func (m mockSource) Name() string         { return m.name }
func (m mockSource) Kind() bootstrap.Kind { return m.kind }
func (m mockSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	return m.epoch, m.err
}

func newMon(local abi.ChainEpoch, srcs ...HeadSource) *Monitor {
	return New(Config{
		Local:    func() abi.ChainEpoch { return local },
		Sources:  srcs,
		Lookback: DefaultLookback, // 3
		MinAgree: DefaultMinAgree, // 2
	})
}

func TestAgree_TwoDistinctKindsWithinLookback(t *testing.T) {
	m := newMon(100,
		mockSource{"forest", bootstrap.KindForest, 100, nil},
		mockSource{"user", bootstrap.KindUser, 102, nil}, // within 3
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("want agree, got %s (agree=%d disagree=%d)", r.Status, r.Agreeing, r.Disagreeing)
	}
	if r.Agreeing != 2 {
		t.Fatalf("want 2 agreeing kinds, got %d", r.Agreeing)
	}
}

func TestDiverge_QuorumOfKindsOutsideLookback(t *testing.T) {
	// local is way behind; two independent kinds cluster at the real tip.
	m := newMon(100,
		mockSource{"forest", bootstrap.KindForest, 140, nil},
		mockSource{"user", bootstrap.KindUser, 141, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusDiverge {
		t.Fatalf("want diverge, got %s", r.Status)
	}
	if r.Disagreeing < 2 {
		t.Fatalf("want >=2 disagreeing kinds, got %d", r.Disagreeing)
	}
	if _, dv := m.Stats(); dv != 1 {
		t.Fatalf("want diverged counter 1, got %d", dv)
	}
}

func TestDiversity_SameKindCountsOnce(t *testing.T) {
	// Three Forest URLs all disagree, but they're ONE kind, so they can't
	// by themselves form the >=MinAgree(2) diverging quorum. A single user
	// peer agreeing isn't enough to AGREE either => INSUFFICIENT, not a
	// false eclipse alarm from three views of the same upstream.
	m := newMon(100,
		mockSource{"forest1", bootstrap.KindForest, 200, nil},
		mockSource{"forest2", bootstrap.KindForest, 200, nil},
		mockSource{"forest3", bootstrap.KindForest, 200, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Disagreeing != 1 {
		t.Fatalf("3 forest sources must collapse to 1 disagreeing kind, got %d", r.Disagreeing)
	}
	if r.Status == StatusDiverge {
		t.Fatalf("single kind must not trigger an eclipse alarm, got %s", r.Status)
	}
}

func TestInsufficient_NoneReachable(t *testing.T) {
	m := newMon(100,
		mockSource{"forest", bootstrap.KindForest, 0, errors.New("down")},
		mockSource{"user", bootstrap.KindUser, 0, errors.New("down")},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusInsufficient {
		t.Fatalf("want insufficient, got %s", r.Status)
	}
	if r.Reachable != 0 {
		t.Fatalf("want 0 reachable, got %d", r.Reachable)
	}
}

func TestAgree_MixedAgreeAndDisagree_AgreeWins(t *testing.T) {
	// Two kinds agree, one disagrees. Agreeing >= MinAgree AND not
	// (disagreeing >= agreeing) => AGREE.
	m := newMon(100,
		mockSource{"forest", bootstrap.KindForest, 101, nil},
		mockSource{"user", bootstrap.KindUser, 99, nil},
		mockSource{"beacon", bootstrap.KindLanternBeacon, 150, nil}, // outlier
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("want agree (2 agree vs 1 disagree), got %s", r.Status)
	}
}

func TestNoLocalHead_NeverWithinTolerance(t *testing.T) {
	m := newMon(-1,
		mockSource{"forest", bootstrap.KindForest, 100, nil},
		mockSource{"user", bootstrap.KindUser, 100, nil},
	)
	r := m.CheckOnce(context.Background())
	// local=-1 can't agree with anything; both answered+disagree => with
	// 2 disagreeing kinds and 0 agreeing this is a diverge signal (we have
	// no corroborated head while the world has one).
	if r.Status != StatusDiverge {
		t.Fatalf("no-local-head vs live external quorum should diverge, got %s", r.Status)
	}
}

func TestMedianExtHead(t *testing.T) {
	m := newMon(100,
		mockSource{"a", bootstrap.KindForest, 100, nil},
		mockSource{"b", bootstrap.KindUser, 102, nil},
		mockSource{"c", bootstrap.KindLibp2p, 101, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.MedianExtHead != 101 {
		t.Fatalf("want median 101, got %d", r.MedianExtHead)
	}
}

func TestWithinLookback_Boundaries(t *testing.T) {
	if !withinLookback(100, 97, 3) {
		t.Fatal("97 should be within 3 of 100")
	}
	if withinLookback(100, 96, 3) {
		t.Fatal("96 should be outside 3 of 100")
	}
	if !withinLookback(100, 103, 3) {
		t.Fatal("103 should be within 3 of 100")
	}
	if withinLookback(-1, 100, 3) {
		t.Fatal("no local head is never within tolerance")
	}
}

func TestStartStop(t *testing.T) {
	m := New(Config{
		Local:    func() abi.ChainEpoch { return 100 },
		Sources:  []HeadSource{mockSource{"f", bootstrap.KindForest, 100, nil}, mockSource{"u", bootstrap.KindUser, 100, nil}},
		Interval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	m.Stop()
	if r, _ := m.Stats(); r == 0 {
		t.Fatal("expected at least one round")
	}
	if m.Last().Status != StatusAgree {
		t.Fatalf("expected agree from running monitor, got %s", m.Last().Status)
	}
}
