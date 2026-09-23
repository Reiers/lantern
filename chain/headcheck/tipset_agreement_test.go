// Tests for #152: tipset-key agreement at a checkpoint below the head, and
// cross-source fork choice. Height-only corroboration cannot see a
// same-height eclipse fork; these cases pin the fix.

package headcheck

import (
	"context"
	"errors"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/chain/bootstrap"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func tsk(t *testing.T, tag string) ltypes.TipSetKey {
	t.Helper()
	h, err := mh.Sum([]byte(tag), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return ltypes.NewTipSetKey(cid.NewCidV1(cid.DagCBOR, h))
}

func ref(t *testing.T, ep abi.ChainEpoch, tag string, w uint64) TipSetRef {
	return TipSetRef{Epoch: ep, Key: tsk(t, tag), ParentWeight: ltypes.NewInt(w)}
}

// keySource is a key-capable mock (implements TipSetAtSource).
type keySource struct {
	mockSource
	at    TipSetRef
	atErr error
}

func (k keySource) TipSetAt(ctx context.Context, _ abi.ChainEpoch) (TipSetRef, error) {
	return k.at, k.atErr
}

func newKeyMon(local abi.ChainEpoch, localRef TipSetRef, srcs ...HeadSource) *Monitor {
	return New(Config{
		Local:         func() abi.ChainEpoch { return local },
		LocalTipSetAt: func(abi.ChainEpoch) (TipSetRef, bool) { return localRef, true },
		Sources:       srcs,
		Lookback:      DefaultLookback,
		MinAgree:      DefaultMinAgree,
	})
}

// The core #152 case: we are on an eclipse fork at the right height. Every
// honest source is within the height lookback, so pre-#152 this was AGREE.
func TestKey_SameHeightEclipseForkDiverges(t *testing.T) {
	ours := ref(t, 97, "attacker-fork", 900)
	real := ref(t, 97, "canonical", 1000)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, real, nil},
		keySource{mockSource{"user", bootstrap.KindUser, 101, nil}, real, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusDiverge {
		t.Fatalf("same-height fork must DIVERGE, got %s (agree=%d disagree=%d)", r.Status, r.Agreeing, r.Disagreeing)
	}
	if r.ForkedKinds != 2 || r.KeyChecked != 2 {
		t.Fatalf("want forked=2 keyChecked=2, got %d/%d", r.ForkedKinds, r.KeyChecked)
	}
	if r.Canonical == nil || r.Canonical.Local || r.Canonical.Ref.Key != real.Key {
		t.Fatalf("cross-source fork choice must pick the honest chain, got %+v", r.Canonical)
	}
	if r.CheckpointEpoch != 97 {
		t.Fatalf("checkpoint = head - lookback = 97, got %d", r.CheckpointEpoch)
	}
}

func TestKey_HonestSourcesOnOurChainAgree(t *testing.T) {
	ours := ref(t, 97, "canonical", 1000)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif", bootstrap.KindForest, 99, nil}, ours, nil},
		keySource{mockSource{"user", bootstrap.KindUser, 102, nil}, ours, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("want agree, got %s", r.Status)
	}
	if r.Canonical == nil || !r.Canonical.Local || len(r.Canonical.Kinds) != 2 {
		t.Fatalf("canonical should be our chain with 2 kinds, got %+v", r.Canonical)
	}
}

// One forked source against two agreeing kinds stays AGREE (majority), but
// the fork is still surfaced.
func TestKey_SingleForkedKindOutvoted(t *testing.T) {
	ours := ref(t, 97, "canonical", 1000)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, ours, nil},
		keySource{mockSource{"user", bootstrap.KindUser, 100, nil}, ours, nil},
		keySource{mockSource{"beacon", bootstrap.KindLanternBeacon, 100, nil}, ref(t, 97, "other", 1200), nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("2 agree vs 1 forked => agree, got %s", r.Status)
	}
	if r.ForkedKinds != 1 {
		t.Fatalf("want 1 forked kind surfaced, got %d", r.ForkedKinds)
	}
	if r.Canonical == nil || !r.Canonical.Local {
		t.Fatalf("more kinds beats heavier weight; canonical must be ours, got %+v", r.Canonical)
	}
}

// Same number of kinds on two chains: heavier ParentWeight wins.
func TestKey_CrossSourceTieBrokenByWeight(t *testing.T) {
	ours := ref(t, 97, "a", 1000)
	heavier := ref(t, 97, "b", 1001)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, ours, nil},
		keySource{mockSource{"user", bootstrap.KindUser, 100, nil}, heavier, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Canonical == nil || r.Canonical.Ref.Key != heavier.Key {
		t.Fatalf("tie must go to heavier weight, got %+v", r.Canonical)
	}
}

// Deterministic across repeated rounds (map iteration order must not leak).
func TestKey_CanonicalPickDeterministic(t *testing.T) {
	ours := ref(t, 97, "a", 1000)
	other := ref(t, 97, "b", 1000) // equal weight, equal kinds
	var first ltypes.TipSetKey
	for i := 0; i < 50; i++ {
		m := newKeyMon(100, ours,
			keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, ours, nil},
			keySource{mockSource{"user", bootstrap.KindUser, 100, nil}, other, nil},
		)
		r := m.CheckOnce(context.Background())
		if i == 0 {
			first = r.Canonical.Ref.Key
		} else if r.Canonical.Ref.Key != first {
			t.Fatalf("non-deterministic canonical pick on round %d", i)
		}
	}
}

// Same Kind reporting two different chains still counts as ONE kind.
func TestKey_SameKindCountsOnceInVotes(t *testing.T) {
	ours := ref(t, 97, "canonical", 1000)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif1", bootstrap.KindForest, 100, nil}, ours, nil},
		keySource{mockSource{"glif2", bootstrap.KindForest, 100, nil}, ours, nil},
		keySource{mockSource{"glif3", bootstrap.KindForest, 100, nil}, ours, nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status == StatusAgree {
		t.Fatalf("3 URLs of one kind must not satisfy MinAgree=2, got %s", r.Status)
	}
	if r.Canonical == nil || len(r.Canonical.Kinds) != 1 {
		t.Fatalf("want 1 kind in vote, got %+v", r.Canonical)
	}
}

// A key-capable source that can't answer the checkpoint (lagging) falls
// back to its height vote: no false fork, and far-behind still disagrees.
func TestKey_TipSetAtErrorFallsBackToHeight(t *testing.T) {
	ours := ref(t, 97, "canonical", 1000)
	m := newKeyMon(100, ours,
		keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, TipSetRef{}, errors.New("not yet")},
		mockSource{"user", bootstrap.KindUser, 101, nil}, // height-only source
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("height-only fallback within lookback => agree, got %s", r.Status)
	}
	if r.KeyChecked != 0 || r.ForkedKinds != 0 {
		t.Fatalf("no key comparisons expected, got keyChecked=%d forked=%d", r.KeyChecked, r.ForkedKinds)
	}
}

// Without LocalTipSetAt the monitor behaves exactly as before #152.
func TestKey_NoLocalResolverIsHeightOnly(t *testing.T) {
	m := newMon(100,
		keySource{mockSource{"glif", bootstrap.KindForest, 100, nil}, ref(t, 97, "x", 1), nil},
		keySource{mockSource{"user", bootstrap.KindUser, 100, nil}, ref(t, 97, "y", 1), nil},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree || r.CheckpointEpoch != -1 || r.Canonical != nil {
		t.Fatalf("height-only mode expected, got status=%s cp=%d canon=%v", r.Status, r.CheckpointEpoch, r.Canonical)
	}
}
