package main

import (
	"testing"

	"github.com/Reiers/lantern/build"
)

// fallbackRPCURL: an override always wins; empty override => per-network default.
// Regression cover for the 2026-07-23 Hetzner census incident where Glif public
// began 403'ing our egress IP and the chain-fetch source had no override path.
func TestFallbackRPCURL(t *testing.T) {
	cases := []struct {
		name     string
		override string
		net      build.Network
		want     string
	}{
		{
			name: "empty override, mainnet -> default glif (empty means glif.New's mainnet default)",
			net:  build.Mainnet,
			want: "",
		},
		{
			name: "empty override, calibration -> calibration glif",
			net:  build.Calibration,
			want: "https://api.calibration.node.glif.io/rpc/v1",
		},
		{
			name:     "override wins over network default",
			override: "https://api.chain.love/rpc/v1",
			net:      build.Mainnet,
			want:     "https://api.chain.love/rpc/v1",
		},
		{
			name:     "override wins on calibration too",
			override: "https://filfox.info/rpc/v1",
			net:      build.Calibration,
			want:     "https://filfox.info/rpc/v1",
		},
		{
			name:     "whitespace-only override treated as empty",
			override: "   ",
			net:      build.Calibration,
			want:     "https://api.calibration.node.glif.io/rpc/v1",
		},
		{
			name:     "surrounding whitespace on override is trimmed",
			override: "  https://rpc.ankr.com/filecoin  ",
			net:      build.Mainnet,
			want:     "https://rpc.ankr.com/filecoin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fallbackRPCURL(tc.override, tc.net)
			if got != tc.want {
				t.Fatalf("fallbackRPCURL(%q, %s) = %q; want %q", tc.override, tc.net, got, tc.want)
			}
		})
	}
}
