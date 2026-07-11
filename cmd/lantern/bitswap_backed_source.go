// bitswapBackedSource: an RPCSource / BackfillSource that serves the
// content-addressed FetchBlock(cid) call from the combined (gateway +
// bitswap) fetcher first, falling back to Glif only on a miss. The two
// genuinely RPC-shaped methods (HeadEpoch, TipsetCIDsByHeight) stay on
// Glif as a last resort.
//
// Issue #53: header backfill was hardcoded to a Glif client. When Glif was
// slow/rate-limited the parent-backfill FetchBlock calls timed out, the
// header store couldn't advance contiguously, and the node fell behind live
// head. FetchBlock is content-addressed, so it can be served by the same
// bitswap/gateway fetcher already running for state reads — taking parent
// backfill off the Glif critical path.

package main

import (
	"context"
	"errors"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/blockingest"
	"github.com/Reiers/lantern/net/combined"
)

// blockGetter is the minimal surface the adapter needs from the combined
// fetcher: a content-addressed, CID-verified Get. *combined.Fetcher
// satisfies it.
type blockGetter interface {
	Get(ctx context.Context, c cid.Cid) ([]byte, error)
}

// rpcBlockSource is the RPC-shaped fallback the adapter delegates to.
// *glif.Client satisfies it. Kept as an interface so the fallback path is
// unit-testable without a live endpoint.
type rpcBlockSource interface {
	HeadEpoch(ctx context.Context) (abi.ChainEpoch, error)
	TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error)
	FetchBlock(ctx context.Context, k cid.Cid) (*types.BlockHeader, error)
}

// bitswapBackedSource wraps a Glif client and a content-addressed block
// getter. HeadEpoch/TipsetCIDsByHeight delegate to Glif (inherently
// RPC-shaped). FetchBlock tries the content-addressed getter first and
// falls back to Glif on any error, so parent backfill no longer sits on
// the Glif critical path.
//
// The getter is resolved lazily via getFetcher() on each call. This matters
// because the combined fetcher is REBUILT after libp2p comes up (to add the
// bitswap source), and the sync/gossip wiring is constructed before that
// rebuild — a lazily-resolved getter always sees the current, bitswap-
// enabled fetcher rather than a stale gateway+glif-only snapshot.
type bitswapBackedSource struct {
	glif       rpcBlockSource
	getFetcher func() blockGetter
}

func newBitswapBackedSource(g rpcBlockSource, getFetcher func() blockGetter) *bitswapBackedSource {
	return &bitswapBackedSource{glif: g, getFetcher: getFetcher}
}

// errBridgeOffNoRPC is returned by the RPC-shaped methods when the adapter
// is constructed with a nil Glif client (the #76 bridge-off path). Head
// comes from gossipsub and blocks come from bitswap, so these two calls
// simply have no source — returning a sentinel (instead of nil-panicking)
// lets callers that only need FetchBlock (the gossip parent-walk backfill)
// use the adapter safely.
var errBridgeOffNoRPC = errors.New("bitswap-backed source: no upstream RPC (bridge-off #76): head=gossipsub, blocks=bitswap")

// HeadEpoch delegates to Glif (live head also arrives via gossipsub, so in
// steady state this is rarely the limiting call).
func (s *bitswapBackedSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	if s.glif == nil {
		return 0, errBridgeOffNoRPC
	}
	return s.glif.HeadEpoch(ctx)
}

// TipsetCIDsByHeight delegates to Glif (RPC-shaped: maps height -> the
// canonical tipset's block CIDs).
func (s *bitswapBackedSource) TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	if s.glif == nil {
		return nil, errBridgeOffNoRPC
	}
	return s.glif.TipsetCIDsByHeight(ctx, h)
}

// FetchBlock returns a single CID-verified BlockHeader. It is served from
// the content-addressed fetcher (gateway race + bitswap) first; on any miss
// or decode error it falls back to Glif. The fetcher's Get is already
// CID-verified, and DecodeBlock re-derives the header from the raw bytes,
// so a bad block can't slip through this path.
func (s *bitswapBackedSource) FetchBlock(ctx context.Context, k cid.Cid) (*types.BlockHeader, error) {
	var f blockGetter
	if s.getFetcher != nil {
		f = s.getFetcher()
	}
	if f != nil {
		if raw, err := f.Get(ctx, k); err == nil {
			if bh, derr := types.DecodeBlock(raw); derr == nil {
				return bh, nil
			}
			// Decode failed on otherwise-fetched bytes: fall through to
			// Glif rather than fail the backfill outright.
		}
	}
	if s.glif == nil {
		return nil, errBridgeOffNoRPC
	}
	return s.glif.FetchBlock(ctx, k)
}

// Compile-time guards against the REAL consumer interfaces, so this file
// breaks the build if either consumer's surface drifts from what the
// adapter provides.
var (
	_ hstore.RPCSource           = (*bitswapBackedSource)(nil)
	_ blockingest.BackfillSource = (*bitswapBackedSource)(nil)
	_ blockGetter                = (*combined.Fetcher)(nil)
)

// nilRPCSource / nilBackfillSource return TRULY-NIL interfaces (not a
// typed-nil *bitswapBackedSource) for the bridge-off path (#76,
// --no-fallback-rpc). A typed-nil wrapped in an interface is non-nil and
// would nil-panic on first method call; returning the untyped nil through
// the interface return type makes the nil guards in hstore.Sync
// (src == nil) and net/blockingest (g.src == nil) fire correctly, so no
// Glif client is ever constructed and the polling Sync + gossip inline
// backfill become no-ops (gossipsub is the sole head source).
func nilRPCSource() hstore.RPCSource                { return nil }
func nilBackfillSource() blockingest.BackfillSource { return nil }
