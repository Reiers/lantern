// Gossipsub block ingestor wiring for the standalone daemon.
//
// The ingestor logic itself now lives in the importable package
// net/blockingest, so the embedded daemon (pkg/daemon, used by
// curio-core) can mount the same gossipsub head-tracker. See lantern#40.
//
// This file keeps cmd/lantern's local names (startGossipBlocks +
// type aliases) so the rest of cmd/lantern and its tests are unchanged;
// it is a thin delegation layer over net/blockingest.
//
// Issue #1 background (why gossipsub at all): the daemon otherwise learns
// new heads by polling an upstream Lotus-compatible RPC every 6s, while
// mainnet block time is 30s, so we sit a stable 1 epoch behind. Joining
// /fil/blocks/<network> on gossipsub installs new heads immediately. The
// polling Sync agent stays as a catch-up fallback for anything gossipsub
// misses; when gossipsub is active the operator lifts the poll interval.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/Reiers/lantern/chain/ecfinality"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/net/blockingest"
	"github.com/Reiers/lantern/net/blockpub"
)

// Local aliases so existing cmd/lantern code + tests keep their names
// while the implementation lives in net/blockingest.
type gossipBlockIngestor = blockingest.Ingestor

var newGossipBlockIngestor = blockingest.New

// startGossipBlocks brings up the blockpub subscription + ingestor + the
// standalone stats log loop. Returns the ingestor (for stats / shutdown)
// + the publisher, or nil + error.
func startGossipBlocks(ctx context.Context, ps *pubsub.PubSub, store *hstore.Store, src blockingest.BackfillSource, topic string) (*gossipBlockIngestor, *blockpub.Publisher, error) {
	ing, pub, err := blockingest.Start(ctx, ps, store, src, topic)
	if err != nil {
		return nil, nil, err
	}
	go blockingest.StatsLogger(ctx, ing, pub, 60*time.Second,
		func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) })
	return ing, pub, nil
}

// newECFinality builds the #96 FRC-0089 EC finality cache over the header
// store, or nil when no store is configured. The 900-epoch lookback is
// Filecoin's ChainFinality (same on mainnet + calibration).
func newECFinality(store ecfinality.HeaderSource) *ecfinality.Cache {
	if store == nil {
		return nil
	}
	return ecfinality.NewCache(store, 900)
}
