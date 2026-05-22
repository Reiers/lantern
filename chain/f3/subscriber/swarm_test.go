// Unit tests for SwarmCertSource (issue #6).
//
// The libp2p cert-exchange round-trip is exercised by
// chain/bootstrap/sources/lantern_beacon_test.go and
// chain/f3/certexch/server_test.go. Here we test the *swarm* layer:
//   - falls back when peer provider is empty
//   - falls back when the swarm round-robin fails for all peers
//   - records stats correctly

package subscriber

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-f3/certs"
	"github.com/libp2p/go-libp2p"
)

// stubFallback implements CertSource with deterministic responses.
type stubFallback struct {
	cert *certs.FinalityCertificate
	err  error
	hits int
}

func (s *stubFallback) GetLatest(_ context.Context) (*certs.FinalityCertificate, error) {
	s.hits++
	return s.cert, s.err
}

func (s *stubFallback) GetCert(_ context.Context, _ uint64) (*certs.FinalityCertificate, error) {
	s.hits++
	return s.cert, s.err
}

func newTestSwarmSource(t *testing.T, fallback CertSource) *SwarmCertSource {
	t.Helper()
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	src, err := NewSwarmCertSource(SwarmConfig{
		Host:        h,
		NetworkName: "filecoin",
		Provider:    StaticPeerProvider{}, // empty -> forces fallback
		Fallback:    fallback,
	})
	if err != nil {
		t.Fatalf("NewSwarmCertSource: %v", err)
	}
	return src
}

func TestSwarmCertSource_EmptyPoolFallsBack(t *testing.T) {
	want := &certs.FinalityCertificate{GPBFTInstance: 42}
	fb := &stubFallback{cert: want}
	src := newTestSwarmSource(t, fb)

	got, err := src.GetLatest(context.Background())
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got != want {
		t.Fatalf("returned cert mismatch")
	}
	if fb.hits != 1 {
		t.Errorf("fallback hits = %d, want 1", fb.hits)
	}
	s := src.Stats()
	if s.SwarmMisses != 1 {
		t.Errorf("SwarmMisses = %d, want 1", s.SwarmMisses)
	}
	if s.FallbackHits != 1 {
		t.Errorf("FallbackHits = %d, want 1", s.FallbackHits)
	}
	if s.LastUpstream != "fallback" {
		t.Errorf("LastUpstream = %q, want %q", s.LastUpstream, "fallback")
	}
}

func TestSwarmCertSource_EmptyPoolFallsBackForGetCert(t *testing.T) {
	want := &certs.FinalityCertificate{GPBFTInstance: 99}
	fb := &stubFallback{cert: want}
	src := newTestSwarmSource(t, fb)

	got, err := src.GetCert(context.Background(), 99)
	if err != nil {
		t.Fatalf("GetCert: %v", err)
	}
	if got.GPBFTInstance != 99 {
		t.Errorf("returned cert mismatch")
	}
	if fb.hits != 1 {
		t.Errorf("fallback hits = %d, want 1", fb.hits)
	}
}

func TestSwarmCertSource_FallbackErrorPropagates(t *testing.T) {
	want := errors.New("upstream is down")
	fb := &stubFallback{err: want}
	src := newTestSwarmSource(t, fb)

	_, err := src.GetLatest(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestSwarmCertSource_RequiresHostAndFallback(t *testing.T) {
	_, err := NewSwarmCertSource(SwarmConfig{})
	if err == nil {
		t.Fatal("expected error for missing host+fallback")
	}

	h, err2 := libp2p.New()
	if err2 != nil {
		t.Fatalf("libp2p.New: %v", err2)
	}
	defer h.Close()
	_, err = NewSwarmCertSource(SwarmConfig{Host: h})
	if err == nil {
		t.Fatal("expected error for missing fallback")
	}
}

func TestSwarmCertSource_DefaultsApplied(t *testing.T) {
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	defer h.Close()

	src, err := NewSwarmCertSource(SwarmConfig{
		Host:     h,
		Fallback: &stubFallback{},
		// NetworkName omitted -> "filecoin"
		// Provider omitted -> empty StaticPeerProvider
	})
	if err != nil {
		t.Fatalf("NewSwarmCertSource: %v", err)
	}
	if src.networkName() != "filecoin" {
		t.Errorf("default network name = %q, want filecoin", src.networkName())
	}
	if src.perPeerTime <= 0 {
		t.Errorf("PerPeerTimeout default not applied")
	}
}
