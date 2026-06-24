package handlers

// Local FEVM write-path broadcast + confirm (lantern#45, Stages 4-5).
//
// Stage 4: eth_sendRawTransaction — decode the signed EIP-1559 tx
//   (chain/ethtx), convert to a Filecoin SignedMessage, publish via the
//   gossipsub mempool (MpoolPush). Returns the eth tx hash. Records the
//   eth-hash -> Filecoin-msg-CID mapping so the receipt lookup (Stage 5)
//   can find it.
//
// Stage 5: eth_getTransactionReceipt — resolve the eth hash to the
//   Filecoin msg CID (from the send-time map), StateSearchMsg for the
//   on-chain receipt, and shape an eth receipt with the fields the
//   curio-core #81 watcher reads (status, blockNumber, transactionHash,
//   gasUsed). Returns nil until found, matching the go-ethereum poll loop.
//
// Both keep a bridge fallback. sendRawTransaction falls back only when the
// tx can't be decoded or the mempool isn't wired (NOT after a successful
// local publish — double-broadcast would be wrong). The receipt falls back
// to the bridge when the hash isn't in our local map (e.g. a tx we didn't
// originate) so external lookups still work during rollout.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/api"

	"github.com/Reiers/lantern/chain/ethtx"
	"github.com/Reiers/lantern/chain/types"
)

// maxSentTxTracked bounds the eth-hash -> msg-CID map. The SP's own
// write->confirm loop only needs a handful of in-flight txs tracked at
// once; the cap protects against unbounded growth if receipts are never
// polled. Oldest entries are evicted FIFO.
const maxSentTxTracked = 4096

// sentTxRecord is what we remember about a tx we broadcast: the Filecoin
// message CID (for receipt/by-hash resolution) plus the parsed eth tx so
// eth_getTransactionByHash can return a full tx object locally.
type sentTxRecord struct {
	msgCID cid.Cid
	tx     *ethtx.Eth1559Tx
	from   [20]byte
}

// sentTxIndex maps a lowercase 0x eth tx hash to the record we published
// for it, so eth_getTransactionReceipt and eth_getTransactionByHash can
// resolve locally. Populated by eth_sendRawTransaction.
type sentTxIndex struct {
	mu    sync.Mutex
	byEth map[string]sentTxRecord
	order []string // FIFO eviction
}

func (s *sentTxIndex) put(ethHash string, rec sentTxRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byEth == nil {
		s.byEth = make(map[string]sentTxRecord, 64)
	}
	if _, exists := s.byEth[ethHash]; exists {
		return
	}
	if len(s.order) >= maxSentTxTracked {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byEth, oldest)
	}
	s.byEth[ethHash] = rec
	s.order = append(s.order, ethHash)
}

// get returns the msg CID for a tracked tx (back-compat helper for the
// receipt path).
func (s *sentTxIndex) get(ethHash string) (cid.Cid, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byEth[ethHash]
	if !ok {
		return cid.Undef, false
	}
	return rec.msgCID, true
}

// getRecord returns the full record for a tracked tx.
func (s *sentTxIndex) getRecord(ethHash string) (sentTxRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byEth[ethHash]
	return rec, ok
}

// sentTx returns the per-ChainAPI sent-tx index, lazily initialised.
func (c *ChainAPI) sentTx() *sentTxIndex {
	c.sentTxOnce.Do(func() {
		c.sentTxIdx = &sentTxIndex{}
	})
	return c.sentTxIdx
}

// EthSendRawTransaction broadcasts a signed raw tx. lantern#45 Stage 4:
// decoded locally and published via MpoolPush, with bridge fallback only
// when we can't decode or the mempool isn't wired.
func (c *ChainAPI) EthSendRawTransaction(ctx context.Context, signedTxHex string) (string, error) {
	raw, derr := decodeHexData(signedTxHex)
	switch {
	case derr != nil:
		// Not even valid hex: nothing local to do, let the bridge try.
		log.Debugw("eth_sendRawTransaction: local path declined (bad hex), bridging", "err", derr)
	case c.Mpool == nil:
		// The gossipsub mempool publisher was never wired (or failed to
		// join the topic at boot). This is the silent-fallback trap that
		// caused the #46 bridge-off failure, so log it loudly: a writing
		// SP cannot go bridge-off until this is non-nil.
		log.Warnw("eth_sendRawTransaction: local path UNAVAILABLE (mpool not wired), bridging")
	default:
		tx, err := ethtx.ParseSignedEIP1559(raw)
		if err != nil {
			log.Debugw("eth_sendRawTransaction: local path declined (parse), bridging", "err", err)
			break
		}
		smsg, err := tx.ToSignedFilecoinMessage()
		if err != nil {
			log.Debugw("eth_sendRawTransaction: local path declined (convert), bridging", "err", err)
			break
		}
		ethHash, err := tx.TxHash()
		if err != nil {
			log.Debugw("eth_sendRawTransaction: local path declined (txhash), bridging", "err", err)
			break
		}
		msgCID, perr := c.MpoolPush(ctx, smsg)
		if perr != nil {
			// Publish failed AFTER a clean decode: this is a real error,
			// not a "can't serve locally". Do NOT fall back to the bridge
			// (would risk a double broadcast if the gossipsub publish
			// partially succeeded). Surface it.
			return "", fmt.Errorf("local mpool publish: %w", perr)
		}
		hashHex := "0x" + hex.EncodeToString(ethHash[:])
		sender, _ := tx.Sender()
		c.sentTx().put(strings.ToLower(hashHex), sentTxRecord{msgCID: msgCID, tx: tx, from: sender})
		log.Infow("eth_sendRawTransaction: published locally",
			"ethHash", hashHex, "msgCID", msgCID, "from", smsg.Message.From)
		return hashHex, nil
	}

	if c.Bridge == nil {
		return "", errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{signedTxHex})
	if err != nil {
		return "", fmt.Errorf("marshal eth_sendRawTransaction params: %w", err)
	}
	rawResp, err := c.Bridge.RawJSONRPC(ctx, "eth_sendRawTransaction", params)
	if err != nil {
		return "", fmt.Errorf("bridge eth_sendRawTransaction: %w", err)
	}
	var txHash string
	if err := json.Unmarshal(rawResp, &txHash); err != nil {
		return "", fmt.Errorf("decode eth_sendRawTransaction result: %w", err)
	}
	return txHash, nil
}

// EthGetTransactionReceipt returns the receipt for a tx. lantern#45 Stage
// 5: for txs we originated (in the sent-tx index), resolve the Filecoin
// msg CID and StateSearchMsg locally; otherwise fall back to the bridge.
// Returns nil (not an error) when not yet on-chain, matching the standard
// go-ethereum receipt poll loop.
func (c *ChainAPI) EthGetTransactionReceipt(ctx context.Context, txHash string) (any, error) {
	if out, served, err := c.localEthGetTransactionReceipt(ctx, txHash); served {
		return out, err
	}
	if c.Bridge == nil {
		return nil, errBridgeUnconfigured
	}
	params, err := json.Marshal([]any{txHash})
	if err != nil {
		return nil, fmt.Errorf("marshal eth_getTransactionReceipt params: %w", err)
	}
	raw, err := c.Bridge.RawJSONRPC(ctx, "eth_getTransactionReceipt", params)
	if err != nil {
		return nil, fmt.Errorf("bridge eth_getTransactionReceipt: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode eth_getTransactionReceipt result: %w", err)
	}
	return out, nil
}

// localEthGetTransactionReceipt resolves a receipt locally for a tx we
// originated. Returns (receiptOrNil, served, err). served==false means
// "we don't know this hash, fall back to the bridge".
func (c *ChainAPI) localEthGetTransactionReceipt(ctx context.Context, txHash string) (any, bool, error) {
	if c.HeaderStore == nil {
		return nil, false, nil
	}
	rec, ok := c.sentTx().getRecord(strings.ToLower(txHash))
	if !ok {
		log.Debugw("eth_getTransactionReceipt: tx not in local sent index, bridging", "txHash", txHash)
		return nil, false, nil // not ours -> bridge
	}
	lookup, err := c.StateSearchMsg(ctx, types.TipSetKey{}, rec.msgCID, 0, false)
	if err != nil {
		log.Debugw("eth_getTransactionReceipt: StateSearchMsg error, bridging", "txHash", txHash, "msgCID", rec.msgCID, "err", err)
		return nil, false, nil // degrade to bridge on lookup error
	}
	if lookup == nil {
		// Known tx, not yet on-chain. Serve a definitive "pending" (nil)
		// so the poll loop keeps polling locally rather than hitting the
		// bridge for a tx only we can resolve.
		return nil, true, nil
	}
	return ethReceiptFromLookup(txHash, lookup, rec), true, nil
}

// localEthGetTransactionByHash resolves a tx object locally for a tx we
// originated. Returns (txObjOrNil, served, err). served==false means "we
// don't know this hash, fall back to the bridge". For a known tx we return
// the tx object with blockHash/blockNumber set once it's on-chain, or null
// block fields while still pending (the shape go-ethereum's TransactionByHash
// uses to report isPending).
func (c *ChainAPI) localEthGetTransactionByHash(ctx context.Context, txHash string) (any, bool, error) {
	if c.HeaderStore == nil {
		return nil, false, nil
	}
	rec, ok := c.sentTx().getRecord(strings.ToLower(txHash))
	if !ok || rec.tx == nil {
		log.Debugw("eth_getTransactionByHash: tx not in local sent index, bridging", "txHash", txHash)
		return nil, false, nil // not ours -> bridge
	}
	lookup, err := c.StateSearchMsg(ctx, types.TipSetKey{}, rec.msgCID, 0, false)
	if err != nil {
		log.Debugw("eth_getTransactionByHash: StateSearchMsg error, bridging", "txHash", txHash, "msgCID", rec.msgCID, "err", err)
		return nil, false, nil // degrade to bridge on lookup error
	}
	// lookup==nil => known tx, not yet mined: return the tx with null block
	// fields (pending). lookup!=nil => mined: include blockHash/blockNumber.
	return ethTxFromRecord(txHash, rec, lookup), true, nil
}

// ethTxFromRecord shapes the eth tx JSON object the way go-ethereum's
// TransactionByHash expects. blockHash/blockNumber are null while pending.
func ethTxFromRecord(txHash string, rec sentTxRecord, lookup *api.MsgLookup) map[string]any {
	tx := rec.tx
	obj := map[string]any{
		"hash":                 strings.ToLower(txHash),
		"from":                 "0x" + hex.EncodeToString(rec.from[:]),
		"nonce":                "0x" + uint64Hex(tx.Nonce),
		"value":                "0x" + bigHex(&tx.Value),
		"gas":                  "0x" + uint64Hex(tx.GasLimit),
		"maxFeePerGas":         "0x" + bigHex(&tx.MaxFeePerGas),
		"maxPriorityFeePerGas": "0x" + bigHex(&tx.MaxPriorityFeePerGas),
		"input":                "0x" + hex.EncodeToString(tx.Input),
		"chainId":              "0x" + uint64Hex(tx.ChainID),
		"type":                 "0x2",
		"v":                    "0x" + bigHex(&tx.V),
		"r":                    "0x" + bigHex(&tx.R),
		"s":                    "0x" + bigHex(&tx.S),
		"transactionIndex":     nil,
		"blockHash":            nil,
		"blockNumber":          nil,
	}
	if tx.To != nil {
		obj["to"] = "0x" + hex.EncodeToString(tx.To[:])
	} else {
		obj["to"] = nil
	}
	if lookup != nil {
		obj["blockNumber"] = "0x" + abiEpochHex(lookup.Height)
		obj["blockHash"] = tipsetEthHash(lookup.TipSet)
		obj["transactionIndex"] = "0x0"
	}
	return obj
}

// bigHex formats a state-types big.Int as minimal hex (no 0x prefix),
// "0" for zero/nil.
func bigHex(v *big.Int) string {
	if v == nil || v.Int == nil || v.Sign() == 0 {
		return "0"
	}
	return v.Text(16)
}

// ethReceiptFromLookup shapes the eth-receipt JSON object from a Filecoin
// MsgLookup. We populate the fields go-ethereum clients (and the
// curio-core #81 watcher) read: status, blockNumber, transactionHash,
// gasUsed, blockHash, transactionIndex, cumulativeGasUsed, logs,
// logsBloom. status = 1 when the message exit code is Ok, else 0.
func ethReceiptFromLookup(txHash string, lookup *api.MsgLookup, rec sentTxRecord) map[string]any {
	status := "0x0"
	if lookup.Receipt.ExitCode == exitcode.Ok {
		status = "0x1"
	}
	blockNum := "0x" + abiEpochHex(lookup.Height)

	// blockHash: first CID of the including tipset, hashed into a 32-byte
	// eth-shaped hash slot. Clients mostly key off blockNumber + status;
	// we provide a stable non-empty hash from the tipset key.
	blockHash := tipsetEthHash(lookup.TipSet)

	// Standard eth_getTransactionReceipt requires from/to/contractAddress and
	// (post-1559) effectiveGasPrice. Omitting them breaks strict clients
	// (cast/ethers deserialize "missing field `from`") and any cost display
	// (effectiveGasPrice * gasUsed). from/to come from the locally-tracked
	// sent-tx record; effectiveGasPrice is the tx's maxFeePerGas as a
	// conservative stand-in (Lantern V1 has no per-tipset base-fee receipt
	// reconstruction; clients that need exact effective price still bridge).
	out := map[string]any{
		"transactionHash":   strings.ToLower(txHash),
		"transactionIndex":  "0x0",
		"blockHash":         blockHash,
		"blockNumber":       blockNum,
		"from":              "0x" + hex.EncodeToString(rec.from[:]),
		"cumulativeGasUsed": "0x" + uint64Hex(uint64(lookup.Receipt.GasUsed)),
		"gasUsed":           "0x" + uint64Hex(uint64(lookup.Receipt.GasUsed)),
		"status":            status,
		"logs":              []any{},
		"logsBloom":         "0x" + strings.Repeat("00", 256),
		"type":              "0x2",
	}
	// to / contractAddress: contract creation has nil To (*[20]byte).
	if rec.tx != nil && rec.tx.To != nil {
		out["to"] = "0x" + hex.EncodeToString(rec.tx.To[:])
		out["contractAddress"] = nil
	} else {
		out["to"] = nil
		// contractAddress left nil; Lantern V1 doesn't reconstruct created addr.
		out["contractAddress"] = nil
	}
	if rec.tx != nil {
		out["effectiveGasPrice"] = "0x" + rec.tx.MaxFeePerGas.Text(16)
	}
	return out
}

// abiEpochHex / uint64Hex format quantities without a leading-zero-padded
// hex (eth QUANTITY encoding: minimal hex, "0x0" for zero).
func abiEpochHex(e abi.ChainEpoch) string {
	if e <= 0 {
		return "0"
	}
	return uint64Hex(uint64(e))
}

func uint64Hex(v uint64) string {
	if v == 0 {
		return "0"
	}
	const digits = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}

// tipsetEthHash derives a stable 32-byte 0x hash from a tipset key for the
// receipt's blockHash field. Not consensus-meaningful; clients key off
// blockNumber + status. We use the first block CID's bytes, right-padded.
func tipsetEthHash(tsk types.TipSetKey) string {
	cids := tsk.Cids()
	if len(cids) == 0 {
		return "0x" + strings.Repeat("00", 32)
	}
	b := cids[0].Bytes()
	var out [32]byte
	// Use the multihash digest tail (skip CID prefix) for entropy.
	if len(b) >= 32 {
		copy(out[:], b[len(b)-32:])
	} else {
		copy(out[32-len(b):], b)
	}
	return "0x" + hex.EncodeToString(out[:])
}
