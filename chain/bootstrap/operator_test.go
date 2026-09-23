package bootstrap

import "testing"

func TestOperatorOf(t *testing.T) {
	cases := map[string]string{
		"https://api.node.glif.io/rpc/v1":             "glif.io",
		"https://api.calibration.node.glif.io/rpc/v1": "glif.io",
		"https://filfox.info/rpc/v1":                  "filfox.info",
		"https://calibration.filfox.info/rpc/v1":      "filfox.info",
		"https://api.chain.love/rpc/v1":               "chain.love",
		"https://rpc.ankr.com/filecoin":               "ankr.com",
		"https://gateway.lantern.reiers.io":           "reiers.io",
		"http://127.0.0.1:1234/rpc/v1":                "127.0.0.1",
		"http://localhost:1234/rpc/v1":                "localhost",
		"api.node.glif.io":                            "glif.io",
		"API.Node.Glif.IO.":                           "glif.io",
		"api.node.glif.io:443":                        "glif.io",
		"":                                            "",
	}
	for in, want := range cases {
		if got := OperatorOf(in); got != want {
			t.Errorf("OperatorOf(%q) = %q, want %q", in, got, want)
		}
	}
}
