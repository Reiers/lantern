package mpool_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
)

func mkSigned(t *testing.T) *ltypes.SignedMessage {
	t.Helper()
	from, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	to, err := address.NewIDAddress(1001)
	require.NoError(t, err)
	return &ltypes.SignedMessage{
		Message: ltypes.Message{
			Version:    0,
			From:       from,
			To:         to,
			Nonce:      42,
			Value:      big.NewInt(1_000_000_000),
			GasLimit:   10_000_000,
			GasFeeCap:  big.NewInt(100_000_000),
			GasPremium: big.NewInt(100_000),
			Method:     0,
		},
		Signature: gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: []byte("fake-sig-bytes-96-long")},
	}
}

func TestMpoolDryRunPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	p, err := mpool.New(ctx, h.PubSub, mpool.Config{
		Topic:  "/fil/msgs/test-mpool",
		DryRun: true,
	})
	require.NoError(t, err)
	defer p.Close()

	sm := mkSigned(t)
	c, err := p.Publish(ctx, sm)
	require.True(t, errors.Is(err, mpool.ErrDryRun))
	require.True(t, c.Defined())

	stats := p.Stats()
	require.Equal(t, 1, stats.PendingCnt)
	require.Equal(t, uint64(0), stats.Published)
}

func TestMpoolValidatesSignedMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	require.NoError(t, err)
	defer h.Close()

	p, err := mpool.New(ctx, h.PubSub, mpool.Config{
		Topic:  "/fil/msgs/test-mpool-validate",
		DryRun: true,
	})
	require.NoError(t, err)
	defer p.Close()

	// Empty signed message must be rejected.
	_, err = p.Publish(ctx, &ltypes.SignedMessage{})
	require.Error(t, err)
	require.False(t, errors.Is(err, mpool.ErrDryRun))
}
