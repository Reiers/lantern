// Pool-level restart tests for #119.
//
// These stand up a real Pool (with a real libp2p host + pubsub, DryRun
// mode so nothing hits the network) against a temp-dir persist path,
// publish some messages, close the pool, reopen a NEW Pool at the same
// path, and assert the pending set was restored across the "restart".
package mpool_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
)

// mkSignedNonce is like mkSigned in mpool_test.go but with a custom
// nonce so restart tests can push multiple distinct messages from the
// same account without collision.
func mkSignedNonce(t *testing.T, nonce uint64) *ltypes.SignedMessage {
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
			Nonce:      nonce,
			Value:      big.NewInt(1_000_000_000),
			GasLimit:   10_000_000,
			GasFeeCap:  big.NewInt(100_000_000),
			GasPremium: big.NewInt(100_000),
			Method:     0,
		},
		Signature: gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: []byte("fake-sig-bytes-96-long")},
	}
}

// newDryPoolPersist stands up a fresh libp2p host + DryRun pool with a
// persist path pointing at `path`. Each call spins its own host so the
// "restart" case truly starts a new pool from scratch.
func newDryPoolPersist(t *testing.T, ctx context.Context, path string, cfg mpool.Config) *mpool.Pool {
	t.Helper()
	h, err := llibp2p.New(ctx, llibp2p.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	require.NoError(t, err)
	t.Cleanup(func() { h.Close() })
	cfg.DryRun = true
	cfg.PersistPath = path
	if cfg.Topic == "" {
		cfg.Topic = "/fil/msgs/test-persist"
	}
	p, err := mpool.New(ctx, h.PubSub, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	return p
}

// TestPersist_RestartPreservesPending is the core #119 property test:
// publish 3 messages, close the pool, open a new Pool at the same path,
// assert Pending() returns all 3.
func TestPersist_RestartPreservesPending(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "mpool", "pending.jsonl")

	// First "session": publish 3 messages.
	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		for _, n := range []uint64{100, 101, 102} {
			_, err := p.Publish(ctx, mkSignedNonce(t, n))
			require.True(t, errors.Is(err, mpool.ErrDryRun))
		}
		require.Equal(t, 3, p.Stats().PendingCnt)
		require.NoError(t, p.Close())
	}

	// Second "session": fresh Pool at the same persist path. All 3
	// must be restored into the pending set.
	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		st := p.Stats()
		require.Equal(t, 3, st.PendingCnt, "restart must restore pending set")
		require.Equal(t, uint64(3), st.Restored, "Stats.Restored must reflect restore count")
		require.Equal(t, path, st.PersistPath)

		pending := p.Pending()
		require.Len(t, pending, 3)
		nonces := map[uint64]bool{}
		for _, sm := range pending {
			nonces[sm.Message.Nonce] = true
		}
		require.True(t, nonces[100])
		require.True(t, nonces[101])
		require.True(t, nonces[102])
	}
}

// TestPersist_ConfirmedDropsAcrossRestart: publish 2, confirm 1 via
// Reconcile, close, reopen. Only 1 must be restored.
func TestPersist_ConfirmedDropsAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "mpool", "pending.jsonl")

	var confirmedCID cid.Cid
	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		c1, err := p.Publish(ctx, mkSignedNonce(t, 200))
		require.True(t, errors.Is(err, mpool.ErrDryRun))
		_, err = p.Publish(ctx, mkSignedNonce(t, 201))
		require.True(t, errors.Is(err, mpool.ErrDryRun))
		confirmedCID = c1

		// Confirm the first message on chain via a targeted search.
		p.Reconcile(ctx, 500, func(_ context.Context, mc cid.Cid) (mpool.SearchResult, error) {
			if mc == confirmedCID {
				return mpool.SearchFound, nil
			}
			return mpool.SearchUnknown, nil
		})
		st := p.Stats()
		require.Equal(t, 1, st.PendingCnt)
		require.Equal(t, uint64(1), st.Confirmed)
		require.NoError(t, p.Close())
	}

	// Restart: only the un-confirmed message must come back.
	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		require.Equal(t, 1, p.Stats().PendingCnt)
		require.Equal(t, uint64(1), p.Stats().Restored)
		// The one that came back must NOT be the confirmed cid.
		got := p.Pending()
		require.Len(t, got, 1)
		// Verify it's the surviving nonce (201).
		require.Equal(t, uint64(201), got[0].Message.Nonce)
	}
}

// TestPersist_ForgetDropsAcrossRestart mirrors the confirm test but via
// the imperative Forget path (used by callers that confirm via a
// non-Reconcile mechanism).
func TestPersist_ForgetDropsAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "mpool", "pending.jsonl")

	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		c, err := p.Publish(ctx, mkSignedNonce(t, 300))
		require.True(t, errors.Is(err, mpool.ErrDryRun))
		p.Forget(c)
		require.Equal(t, 0, p.Stats().PendingCnt)
		require.NoError(t, p.Close())
	}

	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{})
		require.Equal(t, 0, p.Stats().PendingCnt)
	}
}

// TestPersist_RetryCountSurvivesRestart: publish, run Reconcile enough
// times to rebroadcast, close, reopen; assert the retries counter was
// persisted.
func TestPersist_RetryCountSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "mpool", "pending.jsonl")

	{
		p := newDryPoolPersist(t, ctx, path, mpool.Config{
			ConfirmAfterEpochs: 3,
			MaxRetries:         5,
		})
		_, err := p.Publish(ctx, mkSignedNonce(t, 400))
		require.True(t, errors.Is(err, mpool.ErrDryRun))
		unknown := func(_ context.Context, _ cid.Cid) (mpool.SearchResult, error) {
			return mpool.SearchUnknown, nil
		}
		p.Reconcile(ctx, 100, unknown) // anchors publishedAt=100
		p.Reconcile(ctx, 110, unknown) // retries=1
		p.Reconcile(ctx, 120, unknown) // retries=2
		require.Equal(t, uint64(2), p.Stats().Rebroadcasts)
		require.NoError(t, p.Close())
	}

	// Fresh pool: the message is restored WITH its retry counter.
	// One more past-window Reconcile should bring it to 3, then 4, 5,
	// then fail at 6.
	{
		var failed bool
		p := newDryPoolPersist(t, ctx, path, mpool.Config{
			ConfirmAfterEpochs: 3,
			MaxRetries:         5,
			OnFailed:           func(*ltypes.SignedMessage, string) { failed = true },
		})
		require.Equal(t, 1, p.Stats().PendingCnt)
		unknown := func(_ context.Context, _ cid.Cid) (mpool.SearchResult, error) {
			return mpool.SearchUnknown, nil
		}
		// Anchor is now epoch 0 in memory (restored publishedAt=100
		// from journal, but Reconcile treats it as valid anchor).
		p.Reconcile(ctx, 130, unknown) // retries=3
		p.Reconcile(ctx, 140, unknown) // retries=4
		p.Reconcile(ctx, 150, unknown) // retries=5
		require.Equal(t, 1, p.Stats().PendingCnt, "not yet failed at retries=5")
		p.Reconcile(ctx, 160, unknown) // retries>=max -> failed
		require.Equal(t, 0, p.Stats().PendingCnt)
		require.Equal(t, uint64(1), p.Stats().Failed)
		require.True(t, failed, "OnFailed must fire after retry counter crosses MaxRetries across restart")
	}
}

// TestPersist_EmptyPathIsMemoryOnly: PersistPath="" means #119 is off
// (backwards-compatible default).
func TestPersist_EmptyPathIsMemoryOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Two sequential pools with the same DryRun config; no path.
	// Whatever the first publishes must NOT show up in the second.
	h1, err := llibp2p.New(ctx, llibp2p.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	require.NoError(t, err)
	defer h1.Close()
	p1, err := mpool.New(ctx, h1.PubSub, mpool.Config{
		Topic:  "/fil/msgs/test-nopersist",
		DryRun: true,
	})
	require.NoError(t, err)
	_, _ = p1.Publish(ctx, mkSignedNonce(t, 500))
	require.Equal(t, 1, p1.Stats().PendingCnt)
	require.Empty(t, p1.Stats().PersistPath)
	require.NoError(t, p1.Close())

	h2, err := llibp2p.New(ctx, llibp2p.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	require.NoError(t, err)
	defer h2.Close()
	p2, err := mpool.New(ctx, h2.PubSub, mpool.Config{
		Topic:  "/fil/msgs/test-nopersist",
		DryRun: true,
	})
	require.NoError(t, err)
	require.Equal(t, 0, p2.Stats().PendingCnt, "no persist path → nothing to restore")
	require.NoError(t, p2.Close())
}
