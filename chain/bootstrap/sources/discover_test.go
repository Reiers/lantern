package sources

import (
	"testing"

	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/net/dnsaddr"
)

func operatorsOf(srcs []bootstrap.Source) map[string]int {
	out := map[string]int{}
	for _, s := range srcs {
		out[bootstrap.OperatorKey(s)]++
	}
	return out
}

// F10: the default public RPC sets alone already span 3 distinct
// operators per network; Glif and chain.love collapse to one.
func TestPublicRPCSetsAreOperatorDiverse(t *testing.T) {
	main := operatorsOf(BuildDefaultSources(SourceSetConfig{PublicForestURLs: MainnetPublicForestURLs}))
	if len(main) != 3 || main["chain.love"] != 2 || main["filfox.info"] != 1 || main["ankr.com"] != 1 {
		t.Fatalf("mainnet RPC operators = %v, want chain.love x2 (glif + chain.love), filfox.info, ankr.com", main)
	}
	cal := operatorsOf(BuildDefaultSources(SourceSetConfig{PublicForestURLs: CalibnetPublicForestURLs}))
	if len(cal) != 3 || cal["chain.love"] != 1 || cal["filfox.info"] != 1 || cal["ankr.com"] != 1 {
		t.Fatalf("calibration RPC operators = %v, want chain.love, filfox.info, ankr.com", cal)
	}
}

func TestLibp2pPeersDedupAndOperator(t *testing.T) {
	const devtty = "12D3KooWAke3M2ji7tGNKx3BQkTHCyxVhtV1CN68z6Fkrpmfr37F"
	entries := []dnsaddr.Entry{
		{Addr: "/dns/bootstrap.filecoin.chain.love/tcp/1235/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt", Origin: "/dns/bootstrap.filecoin.chain.love/tcp/1235/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt"},
		// Expanded from a /dnsaddr root: attributed to the root's domain.
		{Addr: "/ip4/15.235.86.73/tcp/34000/p2p/12D3KooWGnkd9GQKo3apkShQDaq1d6cKJJmsVe6KiQkacUk1T8oZ", Origin: "/dnsaddr/bootstrap-mainnet.chainsafe-fil.io"},
		// Same peer, two transports: one source.
		{Addr: "/dns/n1.mainnet.fil.devtty.eu/udp/443/quic-v1/p2p/" + devtty, Origin: "/dns/n1.mainnet.fil.devtty.eu/udp/443/quic-v1/p2p/" + devtty},
		{Addr: "/dns/n1.mainnet.fil.devtty.eu/tcp/443/p2p/" + devtty, Origin: "/dns/n1.mainnet.fil.devtty.eu/tcp/443/p2p/" + devtty},
	}
	got := libp2pPeers(entries, 0)
	if len(got) != 3 {
		t.Fatalf("want 3 distinct peers, got %d", len(got))
	}
	wantOps := []string{"chain.love", "chainsafe-fil.io", "devtty.eu"}
	for i, p := range got {
		if p.operator != wantOps[i] {
			t.Errorf("peer %d operator = %q, want %q", i, p.operator, wantOps[i])
		}
	}
	if n := len(got[2].info.Addrs); n != 2 {
		t.Errorf("duplicate peer should keep both addrs, got %d", n)
	}
	if capped := libp2pPeers(entries, 2); len(capped) != 2 {
		t.Errorf("cap of 2 peers not applied, got %d", len(capped))
	}
}
