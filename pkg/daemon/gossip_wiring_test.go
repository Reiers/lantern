package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/wallet"
)

func TestSplitCSV(t *testing.T) {
	require.Nil(t, splitCSV(""))
	require.Equal(t, []string{"a"}, splitCSV("a"))
	require.Equal(t, []string{"a", "b"}, splitCSV("a,b"))
	require.Equal(t, []string{"a", "b"}, splitCSV(" a , b "))
	require.Equal(t, []string{"a"}, splitCSV("a,,"))
	require.Equal(t, []string{
		"/ip4/0.0.0.0/tcp/0",
		"/ip4/0.0.0.0/udp/0/quic-v1",
	}, splitCSV("/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1"))
}

// Before Start (and with libp2p disabled), the gossipsub accessors are
// nil-safe and report "not active". This guards the curio-core default
// (NoLibp2p:true) path: GossipStats must not panic and must report false.
func TestGossipAccessors_NilSafeBeforeStart(t *testing.T) {
	w, err := wallet.New(context.Background(), t.TempDir(), "")
	require.NoError(t, err)
	d, err := New(Config{
		DataDir:  t.TempDir(),
		Wallet:   w,
		NoLibp2p: true,
	})
	require.NoError(t, err)

	require.Nil(t, d.Host(), "Host must be nil before Start / with libp2p disabled")
	stats, active := d.GossipStats()
	require.False(t, active, "gossipsub must report inactive when libp2p disabled")
	require.Zero(t, stats.Installed)
}
