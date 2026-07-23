package handlers

// Local eth_getLogs (lantern#73). The biggest bridge-off gap for stock
// Curio (maxboom, filecoin-project/curio#1311): PDP settlement + dataset
// watchers and FilecoinPay rail indexing call eth_getLogs, and with no
// VMBridge configured the forwarding handler hard-fails. This serves logs
// from local state by decoding per-receipt event AMTs.
//
// Pipeline, per block height in [fromBlock, toBlock]:
//  1. resolve the tipset at height H (the executed tipset),
//  2. find its child tipset (H+1) — ParentMessageReceipts lives there,
//  3. enumerate H's messages in canonical applied order (msgsearch),
//  4. for each receipt with an EventsRoot, walk the events AMT,
//  5. decode each Event -> ETH log via the t1..t4 / d recipe (mirrors
//     lotus ethLogFromEvent), resolve emitter ActorID -> 0xff||be64(id),
//  6. apply the filter's address + topics, collect.
//
// On any structural failure (no header store, missing block in range,
// cold AMT block) we report served=false and fall back to the bridge, so
// behaviour degrades gracefully rather than returning a partial result.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/Reiers/lantern/chain/msgsearch"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/amt"
	"github.com/Reiers/lantern/state/hamt"
)

// rawCodec is the multicodec for raw bytes (0x55). FEVM event entries use
// it for topic/data values; non-raw entries are dropped (built-in actors
// emit CBOR, which isn't an EVM log).
const rawCodec = 0x55

type ethLogFilter struct {
	fromBlock string
	toBlock   string
	blockHash string
	addresses map[string]bool // lower-case 0x eth addresses; empty = any
	// topics[i] is the set of acceptable 32-byte-hex values at position i;
	// nil/empty entry = wildcard. len 0 = match any.
	topics []map[string]bool
}

// ErrLocalRangeTooWide is returned by localEthGetLogs when the requested
// block range exceeds localGetLogsMaxRange. The caller uses it to surface
// an accurate error to bridge-off clients instead of masking it as
// "FEVM method requires --vm-bridge-rpc" (#76).
var ErrLocalRangeTooWide = xerrors.New("eth_getLogs: block range exceeds local scan cap; chunk the query into smaller windows")

// ErrLocalOutOfRetention is returned when a requested epoch is not in the
// local header store retention window. Bridge-off clients should either
// widen their retention (Full tier), narrow the query, or accept the
// bridge fallback.
var ErrLocalOutOfRetention = xerrors.New("eth_getLogs: requested epoch not in local retention window")

// localEthGetLogs returns (logs[], true, nil) on a clean local resolution.
// (nil, false, nil) means "cannot serve, no specific reason" — the caller
// should fall back to the bridge.
// (nil, false, err) means "cannot serve, and here is a specific reason" —
// the caller should prefer this err over errBridgeUnconfigured when the
// bridge itself is not configured.
func (c *ChainAPI) localEthGetLogs(ctx context.Context, filterRaw any) (any, bool, error) {
	if c.HeaderStore == nil || c.BlockGetter == nil {
		return nil, false, nil
	}
	f, ok := parseEthLogFilter(filterRaw)
	if !ok {
		return nil, false, nil // malformed -> bridge
	}

	head := abi.ChainEpoch(c.HeaderStore.HeadEpoch())
	if head < 0 {
		return nil, false, nil
	}

	var from, to abi.ChainEpoch
	if f.blockHash != "" {
		// blockHash form: single block. Resolve its height locally.
		h, served := c.heightForBlockHash(f.blockHash, head)
		if !served {
			return nil, false, ErrLocalOutOfRetention
		}
		from, to = h, h
	} else {
		from = resolveEpochParam(f.fromBlock, head, 0)
		to = resolveEpochParam(f.toBlock, head, head)
	}
	if from < 0 {
		from = 0
	}
	if to > head {
		to = head
	}
	if from > to {
		return []any{}, true, nil
	}
	// Bound the scan: a huge range against local state is expensive and
	// usually means an indexer-style query better served by an upstream.
	if to-from > localGetLogsMaxRange {
		return nil, false, ErrLocalRangeTooWide
	}

	bg := newRetryingBlockGetter(c.BlockGetter, 2, 8*time.Second)
	searcher := msgsearch.New(c.HeaderStore, bg)

	out := []any{}
	logIndex := uint64(0)
	for ep := from; ep <= to; ep++ {
		ts, err := c.HeaderStore.GetTipSetByHeight(ep)
		if err != nil || ts == nil {
			return nil, false, ErrLocalOutOfRetention // gap in local range -> bridge
		}
		child, err := searcher.FindChild(ts)
		if err != nil {
			// No executed child yet (ts is at/near head): no receipts to
			// read. Newer-than-executed blocks simply have no logs yet.
			continue
		}
		receiptsRoot := child.Blocks()[0].ParentMessageReceipts
		msgCIDs, err := searcher.OrderedMessageCIDs(ctx, ts)
		if err != nil {
			return nil, false, nil
		}
		blockHash := EthHashFromCid(ts.Blocks()[0].Cid())
		blockNumHex := fmt.Sprintf("0x%x", int64(ep))

		for idx := uint64(0); idx < uint64(len(msgCIDs)); idx++ {
			recRaw, _, err := amt.LookupV2(ctx, receiptsRoot, idx, bg)
			if err != nil {
				if err == amt.ErrNotFound {
					continue
				}
				return nil, false, nil
			}
			var rec ltypes.MessageReceipt
			if err := rec.UnmarshalCBOR(bytes.NewReader(recRaw)); err != nil {
				return nil, false, nil
			}
			if rec.EventsRoot == nil || !rec.EventsRoot.Defined() {
				continue
			}
			txHash := EthHashFromCid(msgCIDs[idx])
			emittedLogs, served := c.logsForReceipt(ctx, bg, *rec.EventsRoot, &f, txHash, blockHash, blockNumHex, &logIndex)
			if !served {
				return nil, false, nil
			}
			out = append(out, emittedLogs...)
		}
	}
	return out, true, nil
}

// localGetLogsMaxRange caps a single eth_getLogs local scan. Raised from
// 2880 (24h) to 20160 (~7d at 30s blocks) so first-boot Curio watchers
// can backfill a normal weekly window without chunking. Beyond that
// clients should chunk (ErrLocalRangeTooWide).
const localGetLogsMaxRange = abi.ChainEpoch(20160)

// logsForReceipt walks one receipt's events AMT and returns the ETH logs
// that pass the filter. served=false signals a structural decode failure
// (caller should fall back to the bridge).
func (c *ChainAPI) logsForReceipt(ctx context.Context, bg hamt.BlockGetter, eventsRoot cid.Cid, f *ethLogFilter, txHash, blockHash, blockNumHex string, logIndex *uint64) ([]any, bool) {
	var logs []any
	failed := false
	_, err := amt.ForEachRaw(ctx, eventsRoot, bg, &amt.LookupOptions{BitWidth: ltypes.EventAMTBitwidth}, func(_ uint64, val []byte) error {
		var ev ltypes.Event
		if err := ev.UnmarshalCBOR(bytes.NewReader(val)); err != nil {
			failed = true
			return fmt.Errorf("decode event: %w", err)
		}
		data, topics, ok := ethLogFromEntries(ev.Entries)
		if !ok {
			// Non-EVM / malformed event: skip (not a fatal decode error).
			return nil
		}
		emitterAddr := ethAddrFromActorID(ev.Emitter)
		if !f.matches(emitterAddr, topics) {
			return nil
		}
		topicHex := make([]string, len(topics))
		for i, t := range topics {
			topicHex[i] = "0x" + hex.EncodeToString(t)
		}
		logs = append(logs, map[string]any{
			"address":          emitterAddr,
			"topics":           topicHex,
			"data":             "0x" + hex.EncodeToString(data),
			"blockHash":        blockHash,
			"blockNumber":      blockNumHex,
			"transactionHash":  txHash,
			"transactionIndex": "0x0",
			"logIndex":         fmt.Sprintf("0x%x", *logIndex),
			"removed":          false,
		})
		*logIndex++
		return nil
	})
	if err != nil && failed {
		return nil, false
	}
	if err != nil {
		// Cold/missing AMT node -> not definitive.
		return nil, false
	}
	return logs, true
}

// ethLogFromEntries converts an actor event's entries to ETH log (data,
// topics). Mirrors lotus node/impl/eth.ethLogFromEvent: only raw-codec
// t1..t4 (32-byte) topics and a single `d` data field are accepted; any
// non-raw codec or malformed topic drops the whole event (ok=false).
func ethLogFromEntries(entries []ltypes.EventEntry) (data []byte, topics [][]byte, ok bool) {
	var found [4]bool
	count := 0
	dataFound := false
	topics = make([][]byte, 0, 4)
	for _, e := range entries {
		if e.Codec != rawCodec {
			return nil, nil, false
		}
		if len(e.Key) == 2 && e.Key[0] == 't' && e.Key[1] >= '1' && e.Key[1] <= '4' {
			idx := int(e.Key[1] - '1')
			if len(e.Value) != 32 {
				return nil, nil, false
			}
			if found[idx] {
				return nil, nil, false
			}
			found[idx] = true
			count++
			for len(topics) <= idx {
				topics = append(topics, nil)
			}
			cp := make([]byte, 32)
			copy(cp, e.Value)
			topics[idx] = cp
		} else if e.Key == "d" {
			if dataFound {
				return nil, nil, false
			}
			dataFound = true
			data = e.Value
		}
		// unknown keys: skip (forward-compat), matching lotus.
	}
	if len(topics) != count {
		return nil, nil, false // a skipped topic index (e.g. t1 missing, t2 set)
	}
	return data, topics, true
}

// ethAddrFromActorID renders an actor ID as the masked-ID eth address
// 0xff || 11 zero bytes || be64(id), matching EthAddressFromFilecoinIDActor.
func ethAddrFromActorID(id abi.ActorID) string {
	a, err := address.NewIDAddress(uint64(id))
	if err != nil {
		var b [20]byte
		b[0] = 0xff
		for i := 0; i < 8; i++ {
			b[19-i] = byte(uint64(id) >> (8 * i))
		}
		return "0x" + hex.EncodeToString(b[:])
	}
	return EthAddressFromFilecoinIDActor(a)
}

func (f *ethLogFilter) matches(addr string, topics [][]byte) bool {
	if len(f.addresses) > 0 && !f.addresses[strings.ToLower(addr)] {
		return false
	}
	for i, want := range f.topics {
		if len(want) == 0 {
			continue // wildcard
		}
		if i >= len(topics) {
			return false
		}
		got := "0x" + hex.EncodeToString(topics[i])
		if !want[strings.ToLower(got)] {
			return false
		}
	}
	return true
}
