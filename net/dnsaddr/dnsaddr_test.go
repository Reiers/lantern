package dnsaddr

import (
	"context"
	"errors"
	"testing"

	"github.com/multiformats/go-multiaddr"
)

type fakeResolver map[string][]string

func (f fakeResolver) Resolve(_ context.Context, ma multiaddr.Multiaddr) ([]multiaddr.Multiaddr, error) {
	v, ok := f[ma.String()]
	if !ok {
		return nil, errors.New("nxdomain")
	}
	var out []multiaddr.Multiaddr
	for _, s := range v {
		out = append(out, multiaddr.StringCast(s))
	}
	return out, nil
}

const (
	pA = "/dns/a.example.org/tcp/1/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt"
	pB = "/dns/b.example.org/tcp/1/p2p/12D3KooWGnkd9GQKo3apkShQDaq1d6cKJJmsVe6KiQkacUk1T8oZ"
	pC = "/dns/c.example.org/tcp/1/p2p/12D3KooWHQRSDFv4FvAjtU32shQ7znz7oRbLBryXzZ9NMK2feyyH"
)

func TestExpandResolvesRootsAndKeepsOrigin(t *testing.T) {
	r := fakeResolver{
		"/dnsaddr/boot.example.org":   {pB, "/dnsaddr/nested.example.org"},
		"/dnsaddr/nested.example.org": {pC},
	}
	got := ExpandWith(context.Background(), r, []string{pA, "/dnsaddr/boot.example.org", pA}, nil)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %+v", got)
	}
	if got[0].Addr != pA || got[0].Origin != pA {
		t.Fatalf("passthrough entry wrong: %+v", got[0])
	}
	for _, e := range got[1:] {
		if e.Origin != "/dnsaddr/boot.example.org" || e.Fallback {
			t.Fatalf("expanded entry should carry the root as origin: %+v", e)
		}
	}
}

func TestExpandFallsBackToSnapshot(t *testing.T) {
	snap := map[string][]string{"/dnsaddr/boot.example.org": {pB}}
	got := ExpandWith(context.Background(), fakeResolver{}, []string{"/dnsaddr/boot.example.org", "/dnsaddr/unknown.example.org"}, snap)
	if len(got) != 1 || got[0].Addr != pB || !got[0].Fallback {
		t.Fatalf("want snapshot fallback only, got %+v", got)
	}
}

func TestIsRoot(t *testing.T) {
	if !IsRoot("/dnsaddr/bootstrap-mainnet.chainsafe-fil.io") {
		t.Fatal("root not detected")
	}
	if IsRoot(pA) || IsRoot("/dnsaddr/x.example.org/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt") {
		t.Fatal("non-root misdetected")
	}
}
