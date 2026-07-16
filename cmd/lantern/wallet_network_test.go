package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/build"
)

// TestResolveCLINetwork covers every accepted --network form plus alias
// handling and the arg-stripping contract (the selector must not leak into
// the per-subcommand flag set).
func TestResolveCLINetwork(t *testing.T) {
	t.Setenv("LANTERN_NETWORK", "") // isolate from the host env

	cases := []struct {
		name    string
		args    []string
		wantNet build.Network
		wantOut []string
	}{
		{"default mainnet", []string{"list"}, build.Mainnet, []string{"list"}},
		{"space form", []string{"list", "--network", "calibration"}, build.Calibration, []string{"list"}},
		{"equals form", []string{"list", "--network=calibration"}, build.Calibration, []string{"list"}},
		{"single dash", []string{"balance", "-network", "cal"}, build.Calibration, []string{"balance"}},
		{"alias calib", []string{"new", "--network=calib"}, build.Calibration, []string{"new"}},
		{"explicit mainnet", []string{"new", "--network", "mainnet"}, build.Mainnet, []string{"new"}},
		{"preserves other args", []string{"new", "--type=bls", "--network", "cal"}, build.Calibration, []string{"new", "--type=bls"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, out, err := resolveCLINetwork(tc.args)
			require.NoError(t, err)
			require.Equal(t, tc.wantNet, net)
			require.Equal(t, tc.wantOut, out)
		})
	}
}

func TestResolveCLINetworkErrors(t *testing.T) {
	t.Setenv("LANTERN_NETWORK", "")

	_, _, err := resolveCLINetwork([]string{"list", "--network"})
	require.Error(t, err, "--network with no value must error")

	_, _, err = resolveCLINetwork([]string{"list", "--network=devnet"})
	require.Error(t, err, "unknown network must error")
}

func TestResolveCLINetworkEnv(t *testing.T) {
	t.Setenv("LANTERN_NETWORK", "calibration")
	net, out, err := resolveCLINetwork([]string{"list"})
	require.NoError(t, err)
	require.Equal(t, build.Calibration, net)
	require.Equal(t, []string{"list"}, out)

	// Explicit flag overrides the env.
	net, _, err = resolveCLINetwork([]string{"list", "--network=mainnet"})
	require.NoError(t, err)
	require.Equal(t, build.Mainnet, net)
}

func TestParseNetworkAlias(t *testing.T) {
	for _, in := range []string{"mainnet", "main", "MAINNET", ""} {
		n, err := parseNetworkAlias(in)
		require.NoError(t, err)
		require.Equal(t, build.Mainnet, n)
	}
	for _, in := range []string{"calibration", "calib", "cal", "calibnet", "CAL"} {
		n, err := parseNetworkAlias(in)
		require.NoError(t, err)
		require.Equal(t, build.Calibration, n)
	}
	_, err := parseNetworkAlias("nope")
	require.Error(t, err)
}
