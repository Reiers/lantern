package anchorverify

import (
	"errors"
	"testing"
)

// #153: gateway (proxying Glif) + Glif agreeing is ONE operator and must
// not satisfy MinAgreeingSources=2 on its own.
func TestVerify_SameOperatorCountsOnce(t *testing.T) {
	gw := cand(t, "gateway", 100, "sr", 1000, "a")
	gw.Operator = "glif.io"
	gl := cand(t, "glif", 100, "sr", 1000, "a")
	gl.Operator = "glif.io"
	_, err := Verify([]Candidate{gw, gl}, F3Finalized{}, Policy{MinAgreeingSources: 2})
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("gateway+glif sharing an upstream must not reach agreement, got %v", err)
	}
}

func TestVerify_DistinctOperatorsAgree(t *testing.T) {
	gw := cand(t, "gateway", 100, "sr", 1000, "a")
	gw.Operator = "glif.io"
	gl := cand(t, "glif", 100, "sr", 1000, "a")
	gl.Operator = "glif.io"
	cl := cand(t, "independent-rpc", 100, "sr", 1000, "a")
	cl.Operator = "chain.love"
	res, err := Verify([]Candidate{gw, gl, cl}, F3Finalized{}, Policy{MinAgreeingSources: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.AgreeingSources != 2 || res.Method != "multi-source-agreement" {
		t.Fatalf("want 2 distinct operators via agreement, got %d %s", res.AgreeingSources, res.Method)
	}
}

// A two-candidate same-operator group must lose to a two-operator group.
func TestVerify_GroupRankedByDistinctOperators(t *testing.T) {
	a1 := cand(t, "gateway", 100, "srA", 2000, "a")
	a1.Operator = "glif.io"
	a2 := cand(t, "glif", 100, "srA", 2000, "a")
	a2.Operator = "glif.io"
	b1 := cand(t, "filfox", 100, "srB", 1000, "b")
	b1.Operator = "filfox.info"
	b2 := cand(t, "chainlove", 100, "srB", 1000, "b")
	b2.Operator = "chain.love"
	res, err := Verify([]Candidate{a1, a2, b1, b2}, F3Finalized{}, Policy{MinAgreeingSources: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen.StateRoot != b1.StateRoot {
		t.Fatalf("two independent operators must beat one operator seen twice")
	}
}
