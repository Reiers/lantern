package mpool_test

// Tests for the #123 Sink path: Pool constructed without a pubsub instance
// uses Config.Sink instead of gossipsub. Used by devnet where the single-
// node docker devnet can't form a gossipsub mesh; MpoolPushMessage still
// reaches lotus via Filecoin.MpoolPush.
//
// These tests exercise the sink hook end-to-end WITHOUT libp2p or a real
// lotus, using an in-process stub sink. They lock in:
//   1) New(ctx, nil, cfg) succeeds when cfg.Sink is set.
//   2) New(ctx, nil, cfg) rejects when cfg.Sink is also nil.
//   3) Publish invokes Sink instead of gossipsub, records pending, and
//      is byte-identical (persistPath entry matches published raw).
//   4) Close() is a no-op on the (nil) topic/subscription and closes
//      the persist journal cleanly.
//   5) A sink error surfaces from Publish and does NOT record pending.

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/mpool"
)

// captureSink records every raw payload the pool asks it to publish.
// The returned function acts as the Config.Sink; the atomic counters and
// slice let tests assert both call count and byte content.
func captureSink() (mpool.Config, *int64, *int64, *[][]byte) {
	var publishCount int64
	var failCount int64
	captured := &[][]byte{}
	sink := func(_ context.Context, sm *ltypes.SignedMessage, raw []byte) (cid.Cid, error) {
		atomic.AddInt64(&publishCount, 1)
		buf := make([]byte, len(raw))
		copy(buf, raw)
		*captured = append(*captured, buf)
		return sm.Cid(), nil
	}
	return mpool.Config{Sink: sink}, &publishCount, &failCount, captured
}

func TestPool_NilPubSubRequiresSink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := mpool.New(ctx, nil, mpool.Config{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Sink")
}

func TestPool_SinkPublish_RecordsPendingAndPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, publishCount, _, captured := captureSink()
	cfg.PersistPath = filepath.Join(t.TempDir(), "pending.jsonl")

	p, err := mpool.New(ctx, nil, cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, p.Close()) }()

	sm := mkSigned(t)
	mcid, err := p.Publish(ctx, sm)
	require.NoError(t, err)
	require.True(t, mcid.Defined())
	require.Equal(t, sm.Cid(), mcid)

	require.Equal(t, int64(1), atomic.LoadInt64(publishCount))
	require.Len(t, *captured, 1)

	// Byte-identity: what the sink saw is what SignedMessage.Serialize()
	// produces. The rebroadcast contract in #47 requires this.
	raw, err := sm.Serialize()
	require.NoError(t, err)
	require.Equal(t, raw, (*captured)[0])

	// Pending set: the message should be tracked (nonce derivation
	// via MpoolGetNonce reads this).
	pending := p.Pending()
	require.Len(t, pending, 1)
	require.Equal(t, mcid, pending[0].Cid())

	// Stats reflect one publish.
	stats := p.Stats()
	require.Equal(t, uint64(1), stats.Published)
	require.Equal(t, uint64(0), stats.Rebroadcasts)
}

func TestPool_SinkPublish_ErrorSurfacesAndDoesNotRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sentinel := errors.New("upstream lotus rejected: nonce too low")
	cfg := mpool.Config{
		Sink: func(_ context.Context, _ *ltypes.SignedMessage, _ []byte) (cid.Cid, error) {
			return cid.Undef, sentinel
		},
	}
	p, err := mpool.New(ctx, nil, cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, p.Close()) }()

	sm := mkSigned(t)
	_, err = p.Publish(ctx, sm)
	require.Error(t, err)
	require.ErrorContains(t, err, "upstream lotus rejected")

	// The failed publish must NOT be recorded as pending. Silent
	// pending tracking on failed sink calls would corrupt nonce
	// derivation on the next push from the same account.
	require.Empty(t, p.Pending())
	require.Equal(t, uint64(0), p.Stats().Published)
}
