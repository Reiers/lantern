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
	"github.com/ipfs/go-cid"
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

// --- #47 reconcile state-machine tests (DryRun: no real gossip) ---

func newDryPool(t *testing.T, ctx context.Context, cfg mpool.Config) *mpool.Pool {
	t.Helper()
	h, err := llibp2p.New(ctx, llibp2p.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	require.NoError(t, err)
	t.Cleanup(func() { h.Close() })
	cfg.DryRun = true
	if cfg.Topic == "" {
		cfg.Topic = "/fil/msgs/test-reconcile"
	}
	p, err := mpool.New(ctx, h.PubSub, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	return p
}

func TestReconcileConfirmedDropsPending(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := newDryPool(t, ctx, mpool.Config{})

	_, err := p.Publish(ctx, mkSigned(t))
	require.True(t, errors.Is(err, mpool.ErrDryRun))
	require.Equal(t, 1, p.Stats().PendingCnt)

	// Search reports it landed -> dropped + confirmed counter.
	p.Reconcile(ctx, 100, func(_ context.Context, _ cid.Cid) (mpool.SearchResult, error) {
		return mpool.SearchFound, nil
	})
	st := p.Stats()
	require.Equal(t, 0, st.PendingCnt)
	require.Equal(t, uint64(1), st.Confirmed)
}

func TestReconcileWaitsWithinWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := newDryPool(t, ctx, mpool.Config{ConfirmAfterEpochs: 3, MaxRetries: 5})

	_, _ = p.Publish(ctx, mkSigned(t))

	// First reconcile anchors publishedAt=100. Not found, age 0 < window.
	p.Reconcile(ctx, 100, unknownSearch)
	require.Equal(t, 1, p.Stats().PendingCnt)
	require.Equal(t, uint64(0), p.Stats().Rebroadcasts)

	// age 2 still < window 3: still waiting, no rebroadcast.
	p.Reconcile(ctx, 102, unknownSearch)
	require.Equal(t, uint64(0), p.Stats().Rebroadcasts)
}

func TestReconcileFailsLoudlyAfterMaxRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var failedMsg *ltypes.SignedMessage
	var failedReason string
	p := newDryPool(t, ctx, mpool.Config{
		ConfirmAfterEpochs: 3,
		MaxRetries:         2,
		OnFailed: func(sm *ltypes.SignedMessage, reason string) {
			failedMsg = sm
			failedReason = reason
		},
	})
	_, _ = p.Publish(ctx, mkSigned(t))

	// Anchor at 100.
	p.Reconcile(ctx, 100, unknownSearch)
	// Past the window each call: retries increment until max, then fail.
	p.Reconcile(ctx, 110, unknownSearch) // retry 1
	p.Reconcile(ctx, 120, unknownSearch) // retry 2
	require.Equal(t, 1, p.Stats().PendingCnt)
	p.Reconcile(ctx, 130, unknownSearch) // retries>=max -> failed
	st := p.Stats()
	require.Equal(t, 0, st.PendingCnt, "failed message must not stay pending")
	require.Equal(t, uint64(1), st.Failed)
	require.NotNil(t, failedMsg, "OnFailed must fire")
	require.Contains(t, failedReason, "max retries")
}

func unknownSearch(_ context.Context, _ cid.Cid) (mpool.SearchResult, error) {
	return mpool.SearchUnknown, nil
}
