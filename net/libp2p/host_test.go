package libp2p_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llibp2p "github.com/Reiers/lantern/net/libp2p"
)

func TestHostStartsAndListens(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	require.NotEmpty(t, h.ID().String())
	require.NotEmpty(t, h.ListenAddrs())
	require.NotNil(t, h.PubSub)
}
