// Tests for #153: head-source independence is counted per upstream
// operator, not per transport Kind. The Lantern gateway proxies Glif, so
// gateway + Glif must be ONE voter.

package headcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// opSource is a mock with an explicit operator.
type opSource struct {
	mockSource
	op string
}

func (o opSource) Operator() string { return o.op }

func TestIndependence_GatewayPlusGlifIsOneVoter(t *testing.T) {
	m := newMon(100,
		opSource{mockSource{"gateway", bootstrap.KindLanternGateway, 100, nil}, "glif.io"},
		opSource{mockSource{"glif", bootstrap.KindForest, 100, nil}, "glif.io"},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusInsufficient {
		t.Fatalf("gateway(upstream=glif)+glif must be 1 voter => insufficient, got %s (agree=%d)", r.Status, r.Agreeing)
	}
	if r.Agreeing != 1 {
		t.Fatalf("want 1 agreeing voter, got %d", r.Agreeing)
	}
}

func TestIndependence_DistinctOperatorsAgree(t *testing.T) {
	m := newMon(100,
		opSource{mockSource{"gateway", bootstrap.KindLanternGateway, 100, nil}, "glif.io"},
		opSource{mockSource{"glif", bootstrap.KindForest, 100, nil}, "glif.io"},
		opSource{mockSource{"chainlove", bootstrap.KindForest, 101, nil}, "chain.love"},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree || r.Agreeing != 2 {
		t.Fatalf("glif.io + chain.love = 2 voters => agree, got %s agree=%d", r.Status, r.Agreeing)
	}
}

// Same Kind, different operators: previously ONE vote (Kind), now two.
func TestIndependence_SameKindDifferentOperatorsCountTwice(t *testing.T) {
	m := newMon(100,
		opSource{mockSource{"glif", bootstrap.KindForest, 100, nil}, "glif.io"},
		opSource{mockSource{"filfox", bootstrap.KindForest, 100, nil}, "filfox.info"},
	)
	r := m.CheckOnce(context.Background())
	if r.Status != StatusAgree {
		t.Fatalf("two operators => agree, got %s", r.Status)
	}
}

func TestRPCHeadSource_OperatorIsRegistrableDomain(t *testing.T) {
	a := NewRPCHeadSource("", bootstrap.KindForest, "https://api.node.glif.io/rpc/v1", "", 0)
	b := NewRPCHeadSource("", bootstrap.KindForest, "https://api.calibration.node.glif.io/rpc/v1", "", 0)
	if a.Operator() != "glif.io" || b.Operator() != a.Operator() {
		t.Fatalf("glif URLs must share operator, got %q / %q", a.Operator(), b.Operator())
	}
}

func TestGatewayHeadSource_OperatorFromAdvertisedUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Epoch":100,"upstream":"api.chain.love"}`))
	}))
	defer srv.Close()
	g := NewGatewayHeadSource(srv.URL, 0)
	if g.Operator() != bootstrap.DefaultGatewayUpstream {
		t.Fatalf("before probe, gateway must default to %q, got %q", bootstrap.DefaultGatewayUpstream, g.Operator())
	}
	if _, err := g.HeadEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.Operator() != "chain.love" {
		t.Fatalf("advertised upstream must set operator, got %q", g.Operator())
	}
}

func TestGatewayHeadSource_LegacyGatewayDefaultsToGlif(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Epoch":100}`)) // pre-#153 gateway
	}))
	defer srv.Close()
	g := NewGatewayHeadSource(srv.URL, 0)
	if _, err := g.HeadEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.Operator() != "glif.io" {
		t.Fatalf("legacy gateway must be attributed to glif.io, got %q", g.Operator())
	}
}
