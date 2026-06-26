package anchorverify

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// --- test helpers ---

func mustCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatalf("mh.Sum: %v", err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

func tsk(t *testing.T, seeds ...string) ltypes.TipSetKey {
	t.Helper()
	cids := make([]cid.Cid, 0, len(seeds))
	for _, s := range seeds {
		cids = append(cids, mustCID(t, s))
	}
	return ltypes.NewTipSetKey(cids...)
}

func cand(t *testing.T, src string, epoch int64, srRoot string, weight uint64, keySeeds ...string) Candidate {
	t.Helper()
	return Candidate{
		Source:       src,
		Epoch:        abi.ChainEpoch(epoch),
		StateRoot:    mustCID(t, srRoot),
		TipSetKey:    tsk(t, keySeeds...),
		ParentWeight: ltypes.NewInt(weight),
	}
}

// --- tests ---

func TestVerify_NoCandidates(t *testing.T) {
	_, err := Verify(nil, F3Finalized{}, Policy{})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("want ErrNoCandidates, got %v", err)
	}
	// also: all-invalid candidates
	bad := []Candidate{{Source: "x"}} // undefined StateRoot/empty key
	_, err = Verify(bad, F3Finalized{}, Policy{})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("want ErrNoCandidates for invalid set, got %v", err)
	}
}

func TestVerify_MultiSourceAgreement(t *testing.T) {
	cands := []Candidate{
		cand(t, "gateway", 100, "sr-A", 500, "blk-A"),
		cand(t, "glif", 100, "sr-A", 500, "blk-A"),
	}
	res, err := Verify(cands, F3Finalized{}, Policy{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Method != "multi-source-agreement" {
		t.Fatalf("method = %q, want multi-source-agreement", res.Method)
	}
	if res.AgreeingSources != 2 {
		t.Fatalf("agreeing = %d, want 2", res.AgreeingSources)
	}
}

func TestVerify_DisagreementNoF3_Refuses(t *testing.T) {
	cands := []Candidate{
		cand(t, "gateway", 100, "sr-A", 500, "blk-A"),
		cand(t, "glif", 100, "sr-B", 600, "blk-B"),
	}
	var warned bool
	_, err := Verify(cands, F3Finalized{}, Policy{Warnf: func(string, ...any) { warned = true }})
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("want ErrNoAgreement, got %v", err)
	}
	if !warned {
		t.Fatalf("expected a warning on disagreement")
	}
}

func TestVerify_DisagreementWithF3_PicksHeaviestConsistent(t *testing.T) {
	// Two distinct heads ABOVE F3 finality (epoch 90); F3 can't disprove
	// either, so the heaviest wins and we warn.
	cands := []Candidate{
		cand(t, "gateway", 100, "sr-A", 500, "blk-A"),
		cand(t, "glif", 100, "sr-B", 600, "blk-B"),
	}
	f3 := F3Finalized{Available: true, Instance: 7, Epoch: 90, TipSetKey: tsk(t, "final-90")}
	res, err := Verify(cands, f3, Policy{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Method != "f3-consistent-heaviest" {
		t.Fatalf("method = %q, want f3-consistent-heaviest", res.Method)
	}
	if res.Chosen.Source != "glif" { // weight 600 > 500
		t.Fatalf("chosen = %q, want glif (heaviest)", res.Chosen.Source)
	}
	if !res.F3Checked {
		t.Fatalf("F3Checked should be true")
	}
}

func TestVerify_F3Conflict_BelowFinality_Rejected(t *testing.T) {
	// A candidate AT the F3 finalized epoch but with a different tipset is a
	// fork below finality — must be rejected outright.
	cands := []Candidate{
		cand(t, "evil", 90, "sr-evil", 999, "blk-evil"),
		cand(t, "gateway", 90, "sr-good", 100, "final-90"),
	}
	f3 := F3Finalized{Available: true, Instance: 7, Epoch: 90, TipSetKey: tsk(t, "final-90")}
	_, err := Verify(cands, f3, Policy{})
	if !errors.Is(err, ErrF3Conflict) {
		t.Fatalf("want ErrF3Conflict, got %v", err)
	}
}

func TestVerify_F3Consistent_AtFinality_Accepted(t *testing.T) {
	// Both sources agree on the exact F3-finalized tipset at finality epoch.
	cands := []Candidate{
		cand(t, "gateway", 90, "sr-good", 100, "final-90"),
		cand(t, "glif", 90, "sr-good", 100, "final-90"),
	}
	f3 := F3Finalized{Available: true, Instance: 7, Epoch: 90, TipSetKey: tsk(t, "final-90")}
	res, err := Verify(cands, f3, Policy{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Method != "multi-source-agreement" {
		t.Fatalf("method = %q, want multi-source-agreement", res.Method)
	}
}

func TestVerify_InsecureBypass(t *testing.T) {
	cands := []Candidate{cand(t, "solo", 100, "sr-A", 500, "blk-A")}
	var warned bool
	res, err := Verify(cands, F3Finalized{}, Policy{
		InsecureAllowSingleSource: true,
		Warnf:                     func(string, ...any) { warned = true },
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Method != "insecure-single-source" {
		t.Fatalf("method = %q, want insecure-single-source", res.Method)
	}
	if !warned {
		t.Fatalf("insecure bypass should warn loudly")
	}
}

func TestVerify_SingleSourceSecure_Refuses(t *testing.T) {
	// One source, no F3, secure mode: cannot reach 2-way agreement.
	cands := []Candidate{cand(t, "solo", 100, "sr-A", 500, "blk-A")}
	_, err := Verify(cands, F3Finalized{}, Policy{})
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("want ErrNoAgreement for lone secure source, got %v", err)
	}
}

func TestVerify_ThreeSources_MajorityWins(t *testing.T) {
	cands := []Candidate{
		cand(t, "gateway", 100, "sr-A", 500, "blk-A"),
		cand(t, "glif", 100, "sr-A", 500, "blk-A"),
		cand(t, "rogue", 100, "sr-X", 999, "blk-X"),
	}
	res, err := Verify(cands, F3Finalized{}, Policy{MinAgreeingSources: 2})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.AgreeingSources != 2 || res.Chosen.StateRoot.String() != mustCID(t, "sr-A").String() {
		t.Fatalf("majority (sr-A x2) should win over heavier rogue; got src=%s agree=%d",
			res.Chosen.Source, res.AgreeingSources)
	}
}

func TestFinalizedFromCert_Nil(t *testing.T) {
	f, err := FinalizedFromCert(nil)
	if err != nil {
		t.Fatalf("nil cert should be benign, got %v", err)
	}
	if f.Available {
		t.Fatalf("nil cert must yield Available=false")
	}
}

// --- Gather ---

type stubFetcher struct {
	c   Candidate
	err error
}

func (s stubFetcher) FetchCandidate(context.Context) (Candidate, error) { return s.c, s.err }

func TestGather_SkipsFailures(t *testing.T) {
	good := cand(t, "gateway", 100, "sr-A", 500, "blk-A")
	fetchers := []HeadFetcher{
		stubFetcher{c: good},
		stubFetcher{err: errors.New("boom")},
	}
	got := Gather(context.Background(), Policy{}, fetchers...)
	if len(got) != 1 || got[0].Source != "gateway" {
		t.Fatalf("Gather should keep only the successful source; got %+v", got)
	}
}
