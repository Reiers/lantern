package beacon_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/beacon"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func TestLoadMainnetConfigs(t *testing.T) {
	for name, want := range map[string]build.DrandNetwork{
		"mainnet":  build.DrandMainnet,
		"quicknet": build.DrandQuicknet,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := build.DrandConfigs[want]
			c, err := beacon.LoadConfigFromChainInfoJSON(cfg.ChainInfoJSON, cfg.IsChained)
			require.NoError(t, err)
			require.NotNil(t, c.PublicKey)
			require.NotNil(t, c.Scheme)
			require.Equal(t, cfg.IsChained, c.IsChained)
			require.NotEmpty(t, c.Hash)
		})
	}
}

// TestQuicknetMainnetVerify checks that LoadConfigFromChainInfoJSON +
// VerifyEntry on the Filecoin quicknet mainnet config can be invoked against
// known mainnet round data without panicking. Beacon round 1 on quicknet is
// not a useful fixture (you'd need a round with both round and signature on
// hand). We instead verify the negative behaviour: a bogus signature is
// rejected with a non-nil error.
func TestQuicknetVerifyRejectsBogusSig(t *testing.T) {
	cfg := build.DrandConfigs[build.DrandQuicknet]
	c, err := beacon.LoadConfigFromChainInfoJSON(cfg.ChainInfoJSON, cfg.IsChained)
	require.NoError(t, err)

	// 48-byte bogus G1 compressed point.
	bogus, _ := hex.DecodeString(
		"a00000000000000000000000000000000000000000000000" +
			"000000000000000000000000000000000000000000000000")
	require.Len(t, bogus, 48)

	err = c.VerifyEntry(ltypes.BeaconEntry{Round: 12345, Data: bogus}, nil)
	require.Error(t, err)
}
