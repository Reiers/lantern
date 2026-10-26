package bootstrap

import "testing"

func TestOperatorOf(t *testing.T) {
	cases := map[string]string{
		// F10: Glif is served from chain.love infrastructure.
		"https://api.node.glif.io/rpc/v1":             "chain.love",
		"https://api.calibration.node.glif.io/rpc/v1": "chain.love",
		"https://filfox.info/rpc/v1":                  "filfox.info",
		"https://calibration.filfox.info/rpc/v1":      "filfox.info",
		"https://api.chain.love/rpc/v1":               "chain.love",
		"https://rpc.ankr.com/filecoin":               "ankr.com",
		"https://forest-archive.chainsafe.dev/rpc/v1": "chainsafe-fil.io",
		"https://gateway.lantern.reiers.io":           "reiers.io",
		"http://127.0.0.1:1234/rpc/v1":                "127.0.0.1",
		"http://localhost:1234/rpc/v1":                "localhost",
		"api.node.glif.io":                            "chain.love",
		"API.Node.Glif.IO.":                           "chain.love",
		"api.node.glif.io:443":                        "chain.love",
		"":                                            "",
	}
	for in, want := range cases {
		if got := OperatorOf(in); got != want {
			t.Errorf("OperatorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOperatorOfMultiaddr(t *testing.T) {
	cases := map[string]string{
		"/dns/bootstrap.filecoin.chain.love/tcp/1235/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt":         "chain.love",
		"/dns/bootstrap-mainnet-1.chainsafe-fil.io/tcp/34000/p2p/12D3KooWGnkd9GQKo3apkShQDaq1d6cKJJmsVe6KiQkacUk1T8oZ": "chainsafe-fil.io",
		"/dnsaddr/bootstrap-mainnet.chainsafe-fil.io":                                                                  "chainsafe-fil.io",
		"/dns4/n1.mainnet.fil.devtty.eu/udp/443/quic-v1/p2p/12D3KooWAke3M2ji7tGNKx3BQkTHCyxVhtV1CN68z6Fkrpmfr37F":      "devtty.eu",
		"/ip4/94.124.7.230/tcp/443/p2p/12D3KooWAke3M2ji7tGNKx3BQkTHCyxVhtV1CN68z6Fkrpmfr37F":                           "94.124.7.230",
		"/p2p/12D3KooWAke3M2ji7tGNKx3BQkTHCyxVhtV1CN68z6Fkrpmfr37F":                                                    "",
		"": "",
	}
	for in, want := range cases {
		if got := OperatorOfMultiaddr(in); got != want {
			t.Errorf("OperatorOfMultiaddr(%q) = %q, want %q", in, got, want)
		}
	}
}
