package handlers

// Local eth_getBlockByHash (lantern#75). A bridge-off node (stock Curio /
// maxboom) has no VMBridge to forward to, so resolve recent blocks from
// the local header store. The store is height-indexed, not hash-indexed,
// so we scan a bounded window of recent heights and match the ETH block
// hash (EthHashFromCid of the tipset's first block CID, the same hash
// tipsetToEthBlock reports). PDP-path eth_getBlockByHash calls are for
// recent blocks (receipt/tx context), which fall inside the window. For
// hashes outside the window we report served=false and fall back to the
// bridge, so behaviour degrades gracefully instead of erroring.

import (
	"context"
	"strings"

	"github.com/filecoin-project/go-state-types/abi"
)

// localBlockByHashWindow caps how far back we scan for a matching block
// hash. ~900 epochs ≈ 7.5h at 30s blocks — comfortably covers any block a
// PDP node looks up by hash shortly after seeing it, while bounding the
// per-call work. Beyond this we fall back to the bridge.
const localBlockByHashWindow abi.ChainEpoch = 900

// localEthGetBlockByHash returns (ethBlock, true, nil) when the block is
// found in the recent local window, (nil, true, nil) for a well-formed
// hash that simply isn't a known recent block (Ethereum returns null),
// or (nil, false, nil) when we can't serve it locally (no header store,
// or the hash is older than the scan window) and the caller should fall
// back to the bridge.
func (c *ChainAPI) localEthGetBlockByHash(ctx context.Context, blockHash string, fullTx bool) (any, bool, error) {
	_ = ctx
	_ = fullTx
	if c.HeaderStore == nil {
		return nil, false, nil
	}
	want := normalizeHashHex(blockHash)
	if want == "" {
		return nil, false, nil // malformed -> let the bridge try
	}

	head := abi.ChainEpoch(c.HeaderStore.HeadEpoch())
	if head < 0 {
		return nil, false, nil
	}
	low := head - localBlockByHashWindow
	if low < 0 {
		low = 0
	}

	for ep := head; ep >= low; ep-- {
		ts, err := c.HeaderStore.GetTipSetByHeight(ep)
		if err != nil || ts == nil {
			if ep == 0 {
				break
			}
			continue
		}
		blocks := ts.Blocks()
		if len(blocks) == 0 {
			if ep == 0 {
				break
			}
			continue
		}
		// Match against every block CID in the tipset (the ETH "block
		// hash" we report is the first block's, but a caller could hold
		// any sibling's hash).
		for _, b := range blocks {
			if normalizeHashHex(EthHashFromCid(b.Cid())) == want {
				return tipsetToEthBlock(ts), true, nil
			}
		}
		if ep == 0 {
			break
		}
	}

	// Not found within the recent window. If the store is fully warm back
	// to genesis (low==0), this is a definitive "unknown block" -> null.
	// Otherwise the hash may be older than the window; fall back.
	if low == 0 {
		return nil, true, nil
	}
	return nil, false, nil
}

// normalizeHashHex lower-cases and 0x-normalizes a hash for comparison.
// Returns "" for an obviously malformed value.
func normalizeHashHex(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if !strings.HasPrefix(h, "0x") {
		if h == "" {
			return ""
		}
		h = "0x" + h
	}
	// 0x + 64 hex chars for a 32-byte hash.
	if len(h) != 66 {
		return ""
	}
	return h
}
