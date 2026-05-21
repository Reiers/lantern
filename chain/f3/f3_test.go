package f3_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/f3"
)

func TestParseMainnetManifest(t *testing.T) {
	m, err := f3.ParseManifest(build.F3ManifestMainnetJSON)
	require.NoError(t, err)
	require.Equal(t, "filecoin", string(m.NetworkName))
	require.Equal(t, int64(4_920_480), m.BootstrapEpoch)
	require.True(t, m.InitialPowerTable.Defined(), "mainnet manifest must carry InitialPowerTable CID")
}

func TestParseCalibnetManifest(t *testing.T) {
	m, err := f3.ParseManifest(build.F3ManifestCalibnetJSON)
	require.NoError(t, err)
	require.NotEmpty(t, string(m.NetworkName))
}

func TestVerifier_Constructable(t *testing.T) {
	v := f3.Verifier()
	require.NotNil(t, v)
}
