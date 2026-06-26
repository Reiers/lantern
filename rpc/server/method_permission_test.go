package server

import (
	"testing"

	"github.com/Reiers/lantern/api"
)

// TestMethodPermission_EthNamespaceGating is the regression test for #56:
// before the fix, eth_* method names (underscore-separated) bypassed the
// dot-strip + Filecoin switch entirely and fell through to PermRead, so the
// live signing write path eth_sendRawTransaction was callable with the
// unauthenticated default read perms.
func TestMethodPermission_EthNamespaceGating(t *testing.T) {
	cases := []struct {
		method string
		want   string // api.Permission
	}{
		// The mutating write path must require sign.
		{"eth_sendRawTransaction", string(api.PermSign)},
		{"eth_sendTransaction", string(api.PermSign)},
		// Read-only eth_* stays read so dapps/synapse-sdk work tokenless.
		{"eth_call", string(api.PermRead)},
		{"eth_getBalance", string(api.PermRead)},
		{"eth_blockNumber", string(api.PermRead)},
		{"eth_getTransactionReceipt", string(api.PermRead)},
		{"eth_estimateGas", string(api.PermRead)},
		{"net_version", string(api.PermRead)},
		{"web3_clientVersion", string(api.PermRead)},
		// Filecoin.* gating unchanged.
		{"Filecoin.WalletSign", string(api.PermSign)},
		{"Filecoin.WalletNew", string(api.PermWrite)},
		{"Filecoin.AuthNew", string(api.PermAdmin)},
		{"Filecoin.ChainHead", string(api.PermRead)},
	}
	for _, c := range cases {
		got := string(methodPermission(c.method))
		if got != c.want {
			t.Errorf("methodPermission(%q) = %q, want %q", c.method, got, c.want)
		}
	}
}
