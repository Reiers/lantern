package handlers

// Local, bridge-off eth blocks WITH transactions (Curio reorg check).
//
// Stock Curio-PDP's reorg check (curio tasks/pdpv0/task_reorg_check.go)
// walks the canonical chain with go-ethereum's ethclient:
//
//	BlockByNumber(nil)      -> eth_getBlockByNumber("latest", true)
//	BlockByHash(parentHash) -> eth_getBlockByHash(hash, true), repeatedly
//
// and looks for its tx hashes in blk.Transactions(). go-ethereum also
// cross-checks the header: transactionsRoot == EmptyRootHash with a
// non-empty list, or != EmptyRootHash with an empty list, is a hard error.
// The old Lantern shape (transactionsRoot 0x00, transactions []) therefore
// failed every walk, and even a parseable empty block would have made
// Curio believe its txs were reorged out.
//
// This file builds the eth block exactly like Lotus v1.38.3
// node/impl/eth/utils.go newEthBlockFromFilecoinTipSet:
//
//   - block hash = blake2b CID of the TipSetKey (EthHashFromCid(tsk.Cid())),
//     parentHash = the same for ts.Parents();
//   - transactions = every message INCLUDED in the tipset at H (execution
//     order, deduped; Lotus MessagesForTipset), converted with
//     newEthTxFromSignedMessage: delegated (EIP-1559 and legacy) as eth
//     txs, secp/BLS native messages via ethTxFromNativeMessage against the
//     post-execution state (the executing child's ParentState);
//   - gasUsed = sum of the receipts (child.ParentMessageReceipts);
//   - logsBloom = what the Lotus chain indexer stores: emitter + topics of
//     every EVM-shaped event in the tipset;
//   - tags: latest = head's parent (the newest EXECUTED tipset), pending =
//     head, numbers above latest are "future", null rounds are an error.
//
// The canonical chain is rebuilt from parent links (never from the header
// store's per-height tipset, which can be partial) into a small
// height/hash index that is updated incrementally as the head moves, so
// Curio's head-to-depth walk costs one map lookup per step. Built blocks
// are cached by tipset key (a compact summary for many tipsets, full tx
// bodies for a few), keeping per-block work cheap for the PDP tier.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	statecrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/ethtx"
	"github.com/Reiers/lantern/chain/msgsearch"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/amt"
	"github.com/Reiers/lantern/state/hamt"
)

const (
	// Lotus ethtypes constants.
	ethEmptyUncleHash = "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"
	ethEmptyRootHash  = "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
	ethZeroHash       = "0x0000000000000000000000000000000000000000000000000000000000000000"
	ethZeroAddress    = "0x0000000000000000000000000000000000000000"
	ethZeroNonce      = "0x0000000000000000"

	// ethSafeEpochDelay is Lotus ethtypes.SafeEpochDelay (v1 "safe").
	ethSafeEpochDelay abi.ChainEpoch = 30
	// ethChainFinality is the static EC finality Lotus falls back to for
	// "finalized" when no finality calculator / F3 cert is available.
	ethChainFinality abi.ChainEpoch = 900

	// ethBlockMaxDepth caps how far below head a block can be resolved by
	// number from local state (~7 days, same as the eth_getLogs cap).
	ethBlockMaxDepth abi.ChainEpoch = 20160
	// ethBlockHashScanSlack is added to the tx lookback for by-hash
	// scans: Curio's reorg walk goes ~2000 epochs (send-time window)
	// below head, plus the confirmation depth.
	ethBlockHashScanSlack abi.ChainEpoch = 900
	// ethCanonStep is how many epochs the canonical index grows per step
	// while a by-hash lookup searches below it.
	ethCanonStep abi.ChainEpoch = 256
	// ethCanonFloorBackoff suppresses re-walking below a header we failed
	// to load (outside retention) for a while.
	ethCanonFloorBackoff = 10 * time.Minute

	ethBlockSummaryCacheMax = 4096
	ethBlockFullCacheMax    = 32

	// multicodecs used by Lotus' native-message conversion.
	mcIdentity = 0x00
	mcCbor     = 0x51
	mcRaw      = 0x55
	mcDagCbor  = 0x71
)

// ethRevertedAddress is Lotus' revertedEthAddress
// (0xff0000000000000000000000ffffffffffffffff): the "to" of a native
// message whose recipient does not exist in the post-execution state.
var ethRevertedAddress = func() [20]byte {
	var a [20]byte
	a[0] = 0xff
	for i := 12; i < 20; i++ {
		a[i] = 0xff
	}
	return a
}()

var (
	errEthFutureEpoch  = errors.New("requested a future epoch (beyond 'latest')")
	errEthBeyondWindow = errors.New("requested block is beyond the local eth block window")
	errEthEarliest     = errors.New("block param \"earliest\" is not supported")
)

// ethNullRoundError mirrors lotus api.ErrNullRound's message.
type ethNullRoundError struct{ epoch abi.ChainEpoch }

func (e *ethNullRoundError) Error() string {
	return fmt.Sprintf("requested epoch was a null round (%d)", e.epoch)
}

// --- Lotus EthTx ----------------------------------------------------------

// lotusEthTx is the Lotus ethtypes.EthTx field set for one chain message.
type lotusEthTx struct {
	typ      uint64
	chainID  uint64
	nonce    uint64
	hash     [32]byte
	from     [20]byte
	to       *[20]byte
	value    big.Int
	input    []byte
	gas      uint64
	maxFee   *big.Int // EIP-1559 + native
	maxPrio  *big.Int
	gasPrice *big.Int // legacy
	native   bool     // accessList [] for native messages, null for delegated (Lotus)
	v, r, s  big.Int
}

func (t *lotusEthTx) hashHex() string { return "0x" + hex.EncodeToString(t.hash[:]) }

// toJSON renders the tx exactly like Lotus marshals ethtypes.EthTx.
// blockHash / blockNumber / txIndex are nil for a pending tx.
func (t *lotusEthTx) toJSON(blockHash, blockNumber, txIndex any) map[string]any {
	var to any
	if t.to != nil {
		to = "0x" + hex.EncodeToString(t.to[:])
	}
	var accessList any // delegated: Lotus leaves AccessList nil -> null
	if t.native {
		accessList = []any{}
	}
	obj := map[string]any{
		"chainId":          "0x" + uint64Hex(t.chainID),
		"nonce":            "0x" + uint64Hex(t.nonce),
		"hash":             t.hashHex(),
		"blockHash":        blockHash,
		"blockNumber":      blockNumber,
		"transactionIndex": txIndex,
		"from":             "0x" + hex.EncodeToString(t.from[:]),
		"to":               to,
		"value":            "0x" + bigHex(&t.value),
		"type":             "0x" + uint64Hex(t.typ),
		"input":            "0x" + hex.EncodeToString(t.input),
		"gas":              "0x" + uint64Hex(t.gas),
		"accessList":       accessList,
		"v":                "0x" + bigHex(&t.v),
		"r":                "0x" + bigHex(&t.r),
		"s":                "0x" + bigHex(&t.s),
	}
	if t.maxFee != nil {
		obj["maxFeePerGas"] = "0x" + bigHex(t.maxFee)
	}
	if t.maxPrio != nil {
		obj["maxPriorityFeePerGas"] = "0x" + bigHex(t.maxPrio)
	}
	if t.gasPrice != nil {
		obj["gasPrice"] = "0x" + bigHex(t.gasPrice)
	}
	return obj
}

// ethParamsAndRecipient mirrors lotus ethtypes getEthParamsAndRecipient.
func ethParamsAndRecipient(msg *types.Message) ([]byte, *[20]byte, error) {
	var params []byte
	if len(msg.Params) > 0 {
		r := bytes.NewReader(msg.Params)
		b, err := cbg.ReadByteArray(r, uint64(len(msg.Params)))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read params byte array: %w", err)
		}
		if r.Len() != 0 {
			return nil, nil, errors.New("extra data found in params")
		}
		if len(b) == 0 {
			return nil, nil, errors.New("non-empty params encode empty byte array")
		}
		params = b
	}
	if msg.To == builtin.EthereumAddressManagerActorAddr {
		if msg.Method != builtin.MethodsEAM.CreateExternal {
			return nil, nil, errors.New("unsupported EAM method")
		}
		return params, nil, nil
	}
	if msg.Method == builtin.MethodsEVM.InvokeContract {
		a, err := ethAddrBytesFromFilecoin(msg.To)
		if err != nil {
			return nil, nil, err
		}
		return params, &a, nil
	}
	return nil, nil, fmt.Errorf("invalid methodnum %d: only allowed method is InvokeContract(%d) or CreateExternal(%d)",
		msg.Method, builtin.MethodsEVM.InvokeContract, builtin.MethodsEAM.CreateExternal)
}

// decodeDelegatedEthTx rebuilds an eth tx (EIP-1559 or legacy) from a
// delegated-signature message: lotus EthTransactionFromSignedFilecoinMessage
// followed by ToEthTx.
func decodeDelegatedEthTx(sm *types.SignedMessage, chainID uint64) (*lotusEthTx, error) {
	if sm.Signature.Type != statecrypto.SigTypeDelegated {
		return nil, errors.New("not a delegated signature")
	}
	msg := &sm.Message
	from, err := ethAddrBytesFromFilecoin(msg.From)
	if err != nil {
		return nil, errors.New("sender was not an eth account")
	}
	input, to, err := ethParamsAndRecipient(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse input params and recipient: %w", err)
	}
	if msg.Version != 0 {
		return nil, fmt.Errorf("unsupported msg version: %d", msg.Version)
	}
	sig := sm.Signature.Data
	t := &lotusEthTx{
		nonce: msg.Nonce,
		from:  from,
		to:    to,
		value: msg.Value,
		input: input,
		gas:   uint64(msg.GasLimit),
	}
	switch {
	case len(sig) == 65:
		tx := &ethtx.Eth1559Tx{
			ChainID:              chainID,
			Nonce:                msg.Nonce,
			To:                   to,
			Value:                msg.Value,
			MaxFeePerGas:         msg.GasFeeCap,
			MaxPriorityFeePerGas: msg.GasPremium,
			GasLimit:             uint64(msg.GasLimit),
			Input:                input,
		}
		if err := tx.SetSignature(sig); err != nil {
			return nil, err
		}
		h, err := tx.TxHash()
		if err != nil {
			return nil, err
		}
		fee, prio := msg.GasFeeCap, msg.GasPremium
		t.typ, t.chainID, t.hash = ethtx.EIP1559TxType, chainID, h
		t.maxFee, t.maxPrio = &fee, &prio
		t.v, t.r, t.s = tx.V, tx.R, tx.S
	case ethtx.IsLegacySigLen(len(sig), chainID):
		lt := &ethtx.LegacyTx{
			Nonce:    msg.Nonce,
			GasPrice: msg.GasFeeCap,
			GasLimit: uint64(msg.GasLimit),
			To:       to,
			Value:    msg.Value,
			Input:    input,
		}
		if err := lt.SetLegacySignature(sig, chainID); err != nil {
			return nil, err
		}
		h, err := lt.TxHash()
		if err != nil {
			return nil, err
		}
		gp := msg.GasFeeCap
		t.typ, t.hash, t.gasPrice = ethtx.LegacyTxType, h, &gp
		if lt.EIP155 {
			t.chainID = chainID
		}
		t.v, t.r, t.s = lt.V, lt.R, lt.S
	default:
		return nil, fmt.Errorf("unsupported signature length %d", len(sig))
	}
	return t, nil
}

// nativeEthTx mirrors lotus ethTxFromNativeMessage. acc must read the
// post-execution state of the inclusion tipset. Hash is not set.
func (c *ChainAPI) nativeEthTx(ctx context.Context, acc *accessor.Accessor, msg *types.Message, chainID uint64) (*lotusEthTx, error) {
	fromS, err := c.lookupEthAddress(ctx, acc, msg.From)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup sender address %s when converting a native message to an eth txn: %w", msg.From, err)
	}
	from, err := hex20(fromS)
	if err != nil {
		return nil, err
	}
	to := ethRevertedAddress
	toS, err := c.lookupEthAddress(ctx, acc, msg.To)
	switch {
	case err == nil:
		if to, err = hex20(toS); err != nil {
			return nil, err
		}
	case errors.Is(err, accessor.ErrAddressNotFound):
		// Recipient not in the post-state: Lotus' sentinel address.
	default:
		return nil, fmt.Errorf("failed to lookup receiver address %s when converting a native message to an eth txn: %w", msg.To, err)
	}
	var codec uint64
	if len(msg.Params) > 0 {
		codec = mcCbor
	}
	fee, prio := msg.GasFeeCap, msg.GasPremium
	t := &lotusEthTx{
		typ:     ethtx.EIP1559TxType,
		chainID: chainID,
		nonce:   msg.Nonce,
		from:    from,
		to:      &to,
		value:   msg.Value,
		input:   encodeFilecoinParamsAsABI(uint64(msg.Method), codec, msg.Params),
		gas:     uint64(msg.GasLimit),
		maxFee:  &fee,
		maxPrio: &prio,
		native:  true,
	}
	if msg.Method == builtin.MethodsEVM.InvokeContract {
		if inp, err := decodeFilecoinPayload(msg.Params, codec); err == nil {
			t.input = inp
		}
	} else if msg.To == builtin.EthereumAddressManagerActorAddr && msg.Method == builtin.MethodsEAM.CreateExternal {
		if inp, err := decodeFilecoinPayload(msg.Params, codec); err == nil {
			t.input = inp
			t.to = nil
		}
	}
	return t, nil
}

// ethTxForMessage is lotus newEthTxFromSignedMessage: delegated messages
// become their eth tx; secp / BLS messages are converted natively and
// hashed by CID (msgCID is the signed CID for secp, the message CID for
// BLS, i.e. the CID the block's message AMTs carry).
func (c *ChainAPI) ethTxForMessage(ctx context.Context, acc *accessor.Accessor, sm *types.SignedMessage, msgCID cid.Cid, chainID uint64) (*lotusEthTx, error) {
	if sm.Signature.Type == statecrypto.SigTypeDelegated {
		return decodeDelegatedEthTx(sm, chainID)
	}
	t, err := c.nativeEthTx(ctx, acc, &sm.Message, chainID)
	if err != nil {
		return nil, err
	}
	d, ok := cidDigest(msgCID)
	if !ok {
		return nil, fmt.Errorf("message cid %s has no 32-byte digest", msgCID)
	}
	t.hash = d
	return t, nil
}

func hex20(s string) ([20]byte, error) {
	var out [20]byte
	b, err := decodeEthAddr(s)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

// encodeFilecoinParamsAsABI mirrors lotus: the native-method selector
// followed by (method, codec, params) ABI-encoded.
func encodeFilecoinParamsAsABI(method, codec uint64, params []byte) []byte {
	buf := []byte{0x86, 0x8e, 0x10, 0xc4}
	return append(buf, encodeAsABIHelper(method, codec, params)...)
}

func encodeAsABIHelper(param1, param2 uint64, data []byte) []byte {
	const word = 32
	static := []uint64{param1, param2, word * 3, uint64(len(data))}
	total := len(static) + len(data)/word
	if len(data)%word != 0 {
		total++
	}
	buf := make([]byte, total*word)
	off := 0
	for _, a := range static {
		off += word
		binary.BigEndian.PutUint64(buf[off-8:off], a)
	}
	copy(buf[off:], data)
	return buf
}

// decodeFilecoinPayload mirrors lotus decodePayload.
func decodeFilecoinPayload(payload []byte, codec uint64) ([]byte, error) {
	switch codec {
	case mcIdentity:
		return nil, nil
	case mcDagCbor, mcCbor:
		return cbg.ReadByteArray(bytes.NewReader(payload), uint64(len(payload)))
	case mcRaw:
		return payload, nil
	}
	return nil, fmt.Errorf("decodePayload: unsupported codec: %d", codec)
}

// --- canonical chain index -------------------------------------------------

type ethBlockSummary struct {
	txHashes []string
	gasUsed  int64
	bloom    []byte
}

type ethBlockState struct {
	canonMu    sync.Mutex
	headKey    string
	headH      abi.ChainEpoch
	byH        map[abi.ChainEpoch]types.TipSetKey
	byHash     map[[32]byte]abi.ChainEpoch
	low        abi.ChainEpoch // lowest indexed height (byH[low] exists)
	floorH     abi.ChainEpoch
	floorUntil time.Time

	cacheMu   sync.Mutex
	sums      map[string]*ethBlockSummary
	sumOrder  []string
	full      map[string]map[string]any
	fullOrder []string
}

func (c *ChainAPI) ethBlockStateGet() *ethBlockState {
	c.ethBlkOnce.Do(func() {
		c.ethBlk = &ethBlockState{
			byH:    map[abi.ChainEpoch]types.TipSetKey{},
			byHash: map[[32]byte]abi.ChainEpoch{},
			sums:   map[string]*ethBlockSummary{},
			full:   map[string]map[string]any{},
		}
	})
	return c.ethBlk
}

func tipsetKeyHash(k types.TipSetKey) ([32]byte, bool) {
	kc, err := k.Cid()
	if err != nil {
		return [32]byte{}, false
	}
	return cidDigest(kc)
}

func keysEqual(a, b types.TipSetKey) bool { return bytes.Equal(a.Bytes(), b.Bytes()) }

func (s *ethBlockState) setH(h abi.ChainEpoch, k types.TipSetKey) {
	s.delH(h)
	s.byH[h] = k
	if kh, ok := tipsetKeyHash(k); ok {
		s.byHash[kh] = h
	}
}

func (s *ethBlockState) delH(h abi.ChainEpoch) {
	old, ok := s.byH[h]
	if !ok {
		return
	}
	if oh, ok := tipsetKeyHash(old); ok && s.byHash[oh] == h {
		delete(s.byHash, oh)
	}
	delete(s.byH, h)
}

// dropBelow forgets every entry under h and makes the lowest remaining
// indexed height the new floor of the index.
func (s *ethBlockState) dropBelow(h abi.ChainEpoch) {
	for k := range s.byH {
		if k < h {
			s.delH(k)
		}
	}
	s.low = s.headH
	for k := range s.byH {
		if k < s.low {
			s.low = k
		}
	}
}

// ethCanonRefresh brings the index in line with the current head by
// walking parent links from it until the walk rejoins the indexed chain.
// Caller holds s.canonMu.
func (c *ChainAPI) ethCanonRefresh(ctx context.Context, bg hamt.BlockGetter, s *ethBlockState, head *types.TipSet) {
	hk := head.Key()
	if s.headKey == hk.String() {
		return
	}
	if s.headKey == "" || len(s.byH) == 0 {
		s.byH = map[abi.ChainEpoch]types.TipSetKey{}
		s.byHash = map[[32]byte]abi.ChainEpoch{}
		s.headKey, s.headH = hk.String(), head.Height()
		s.setH(head.Height(), hk)
		s.low = head.Height()
		return
	}
	for h := range s.byH {
		if h > head.Height() {
			s.delH(h)
		}
	}
	s.headH = head.Height()
	s.setH(head.Height(), hk)
	curH, pk := head.Height(), head.Parents()
	for {
		if pk.IsEmpty() {
			s.dropBelow(curH)
			break
		}
		bh, err := c.loadHeader(ctx, bg, pk.Cids()[0])
		if err != nil {
			s.dropBelow(curH)
			break
		}
		ph := bh.Height
		for h := ph + 1; h < curH; h++ {
			s.delH(h) // null rounds on the new chain
		}
		if ph < s.low {
			s.setH(ph, pk)
			s.dropBelow(ph)
			break
		}
		if old, ok := s.byH[ph]; ok && keysEqual(old, pk) {
			break // rejoined the indexed chain
		}
		s.setH(ph, pk)
		curH, pk = ph, types.NewTipSetKey(bh.Parents...)
	}
	s.headKey = hk.String()
	if floor := s.headH - ethBlockMaxDepth; s.low < floor {
		s.dropBelow(floor)
	}
}

// ethCanonExtend grows the index below its floor toward target (bounded
// by ethBlockMaxDepth). Caller holds s.canonMu.
func (c *ChainAPI) ethCanonExtend(ctx context.Context, bg hamt.BlockGetter, s *ethBlockState, target abi.ChainEpoch) {
	if min := s.headH - ethBlockMaxDepth; target < min {
		target = min
	}
	for s.low > target {
		if s.low <= s.floorH && time.Now().Before(s.floorUntil) {
			return
		}
		k, ok := s.byH[s.low]
		if !ok {
			return
		}
		bh, err := c.loadHeader(ctx, bg, k.Cids()[0])
		if err != nil {
			s.floorH, s.floorUntil = s.low, time.Now().Add(ethCanonFloorBackoff)
			return
		}
		if len(bh.Parents) == 0 {
			return // genesis
		}
		pk := types.NewTipSetKey(bh.Parents...)
		pbh, err := c.loadHeader(ctx, bg, pk.Cids()[0])
		if err != nil {
			s.floorH, s.floorUntil = s.low, time.Now().Add(ethCanonFloorBackoff)
			return
		}
		s.setH(pbh.Height, pk)
		s.low = pbh.Height
	}
}

// ethTipsetAt returns the canonical tipset at height h (full key, from
// parent links) and the tipset that executed it (nil when incl is the
// head). prev=true resolves a null round to the previous non-null tipset
// (Lotus GetTipsetByHeight(prev=true)); otherwise a null round is an
// ethNullRoundError.
func (c *ChainAPI) ethTipsetAt(ctx context.Context, bg hamt.BlockGetter, h abi.ChainEpoch, prev bool) (*types.TipSet, *types.TipSet, error) {
	head := c.HeaderStore.Head()
	if head == nil {
		return nil, nil, errors.New("no local head")
	}
	s := c.ethBlockStateGet()
	s.canonMu.Lock()
	c.ethCanonRefresh(ctx, bg, s, head)
	if h > s.headH {
		s.canonMu.Unlock()
		return nil, nil, errEthFutureEpoch
	}
	if h < s.low {
		c.ethCanonExtend(ctx, bg, s, h)
	}
	if h < s.low {
		s.canonMu.Unlock()
		return nil, nil, errEthBeyondWindow
	}
	k, ok := s.byH[h]
	for !ok && prev && h > s.low {
		h--
		k, ok = s.byH[h]
	}
	if !ok {
		s.canonMu.Unlock()
		return nil, nil, &ethNullRoundError{epoch: h}
	}
	var ck types.TipSetKey
	hasChild := false
	for ch := h + 1; ch <= s.headH; ch++ {
		if kk, ok := s.byH[ch]; ok {
			ck, hasChild = kk, true
			break
		}
	}
	s.canonMu.Unlock()

	load := func(k types.TipSetKey) (*types.TipSet, error) {
		if keysEqual(k, head.Key()) {
			return head, nil
		}
		return c.loadFullTipSet(ctx, bg, k)
	}
	incl, err := load(k)
	if err != nil {
		return nil, nil, fmt.Errorf("load tipset %d: %w", h, err)
	}
	if !hasChild {
		return incl, nil, nil
	}
	exec, err := load(ck)
	if err != nil {
		return nil, nil, fmt.Errorf("load child of tipset %d: %w", h, err)
	}
	return incl, exec, nil
}

// ethTipsetByHash resolves an eth block hash (Lotus: tipset key CID) on
// the canonical chain within the scan window. For compatibility with
// hashes handed out by older Lantern builds it also accepts the hash of
// any single block CID of a canonical tipset.
func (c *ChainAPI) ethTipsetByHash(ctx context.Context, bg hamt.BlockGetter, want [32]byte) (*types.TipSet, *types.TipSet, bool, error) {
	head := c.HeaderStore.Head()
	if head == nil {
		return nil, nil, false, errors.New("no local head")
	}
	s := c.ethBlockStateGet()
	s.canonMu.Lock()
	c.ethCanonRefresh(ctx, bg, s, head)
	h, ok := s.byHash[want]
	scanFloor := s.headH - (c.ethTxLookback() + ethBlockHashScanSlack)
	if scanFloor < s.headH-ethBlockMaxDepth {
		scanFloor = s.headH - ethBlockMaxDepth
	}
	for !ok && s.low > scanFloor {
		before := s.low
		target := s.low - ethCanonStep
		if target < scanFloor {
			target = scanFloor
		}
		c.ethCanonExtend(ctx, bg, s, target)
		if s.low == before {
			break
		}
		h, ok = s.byHash[want]
	}
	if !ok {
		wantHex := "0x" + hex.EncodeToString(want[:])
	scan:
		for eh, k := range s.byH {
			for _, bc := range k.Cids() {
				if EthHashFromCid(bc) == wantHex {
					h, ok = eh, true
					break scan
				}
			}
		}
	}
	s.canonMu.Unlock()
	if !ok {
		return nil, nil, false, nil
	}
	incl, exec, err := c.ethTipsetAt(ctx, bg, h, false)
	if err != nil {
		return nil, nil, false, err
	}
	return incl, exec, true, nil
}

// ethHeightForHash is the cheap (index-only, no extension) by-hash
// height lookup used by eth_getLogs' blockHash filter.
func (c *ChainAPI) ethHeightForHash(blockHash string) (abi.ChainEpoch, bool) {
	if c.HeaderStore == nil || c.BlockGetter == nil {
		return 0, false
	}
	want, ok := parseEthHash(blockHash)
	if !ok {
		return 0, false
	}
	head := c.HeaderStore.Head()
	if head == nil {
		return 0, false
	}
	s := c.ethBlockStateGet()
	s.canonMu.Lock()
	defer s.canonMu.Unlock()
	c.ethCanonRefresh(context.Background(), c.ethTxBlockGetter(), s, head)
	h, ok := s.byHash[want]
	return h, ok
}

// --- block building ----------------------------------------------------------

func (s *ethBlockState) getSummary(k string) *ethBlockSummary {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.sums[k]
}

func (s *ethBlockState) putSummary(k string, v *ethBlockSummary) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if _, ok := s.sums[k]; ok {
		return
	}
	if len(s.sumOrder) >= ethBlockSummaryCacheMax {
		delete(s.sums, s.sumOrder[0])
		s.sumOrder = s.sumOrder[1:]
	}
	s.sums[k] = v
	s.sumOrder = append(s.sumOrder, k)
}

func (s *ethBlockState) getFull(k string) map[string]any {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.full[k]
}

func (s *ethBlockState) putFull(k string, v map[string]any) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if _, ok := s.full[k]; ok {
		return
	}
	if len(s.fullOrder) >= ethBlockFullCacheMax {
		delete(s.full, s.fullOrder[0])
		s.fullOrder = s.fullOrder[1:]
	}
	s.full[k] = v
	s.fullOrder = append(s.fullOrder, k)
}

func copyBlock(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ethBlockHeader is the Lotus NewEthBlock header for incl.
func ethBlockHeader(incl *types.TipSet, gasUsed int64, bloom []byte, hasTx bool) map[string]any {
	b := incl.Blocks()[0]
	txRoot := ethEmptyRootHash
	if hasTx {
		txRoot = ethZeroHash
	}
	baseFee := big.Int(b.ParentBaseFee)
	return map[string]any{
		"hash":             ethTipsetHash(incl.Key()),
		"parentHash":       ethTipsetHash(incl.Parents()),
		"sha3Uncles":       ethEmptyUncleHash,
		"miner":            ethZeroAddress,
		"stateRoot":        ethZeroHash,
		"transactionsRoot": txRoot,
		"receiptsRoot":     ethZeroHash,
		"logsBloom":        "0x" + hex.EncodeToString(bloom),
		"difficulty":       "0x0",
		"totalDifficulty":  "0x0",
		"number":           "0x" + abiEpochHex(incl.Height()),
		"gasLimit":         "0x" + uint64Hex(uint64(build.BlockGasLimit)*uint64(len(incl.Blocks()))),
		"gasUsed":          "0x" + uint64Hex(uint64(gasUsed)),
		"timestamp":        "0x" + uint64Hex(b.Timestamp),
		"extraData":        "0x",
		"mixHash":          ethZeroHash,
		"nonce":            ethZeroNonce,
		"baseFeePerGas":    "0x" + bigHex(&baseFee),
		"size":             "0x0",
		"uncles":           []any{},
	}
}

func ethFullBloom() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = 0xff
	}
	return b
}

// ethTipsetMessages returns the execution-ordered message list of incl.
// With an executing child it is the receipt-verified, cached index;
// for the (unexecuted) head it is the plain BLS-then-secp dedupe.
func (c *ChainAPI) ethTipsetMessages(ctx context.Context, bg hamt.BlockGetter, incl, exec *types.TipSet) ([]cid.Cid, []bool, error) {
	if exec != nil {
		ix, err := c.tipsetMsgIndex(ctx, bg, incl, exec)
		if err != nil {
			return nil, nil, err
		}
		return ix.msgs, ix.isSecp, nil
	}
	per, err := msgsearch.New(c.HeaderStore, bg).BlockMessages(ctx, incl)
	if err != nil {
		return nil, nil, err
	}
	msgs, isSecp := msgsearch.ExecutionOrder(per)
	return msgs, isSecp, nil
}

// ethStateAccessor returns the state the Lotus conversion reads: the
// post-execution state (exec.ParentState). For the unexecuted head we
// only have the pre-state; it differs only for actors created by the
// head's own messages.
func (c *ChainAPI) ethStateAccessor(incl, exec *types.TipSet, bg hamt.BlockGetter) *accessor.Accessor {
	if exec != nil {
		return c.execAccessor(exec, bg)
	}
	return accessor.New(&trustedroot.TrustedRoot{Epoch: incl.Height(), StateRoot: incl.ParentState()}, bg)
}

// ethTipsetGasAndBloom sums the receipts' gas and builds the Lotus
// chain-indexer bloom (emitter + topics of every EVM-shaped event).
func (c *ChainAPI) ethTipsetGasAndBloom(ctx context.Context, bg hamt.BlockGetter, acc *accessor.Accessor, exec *types.TipSet) (int64, []byte, error) {
	var gas int64
	var roots []cid.Cid
	err := amt.ForEachV2Raw(ctx, exec.Blocks()[0].ParentMessageReceipts, bg, func(i uint64, raw []byte) error {
		var r types.MessageReceipt
		if err := r.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return fmt.Errorf("decode receipt %d: %w", i, err)
		}
		gas += r.GasUsed
		if r.EventsRoot != nil && r.EventsRoot.Defined() {
			roots = append(roots, *r.EventsRoot)
		}
		return nil
	})
	if err != nil {
		return 0, nil, fmt.Errorf("receipts: %w", err)
	}
	bloom := make([]byte, 256)
	emitters := map[abi.ActorID][]byte{} // one state lookup per emitter
	for _, root := range roots {
		var cbErr error
		_, err := amt.ForEachRaw(ctx, root, bg, &amt.LookupOptions{BitWidth: types.EventAMTBitwidth}, func(_ uint64, val []byte) error {
			var ev types.Event
			if err := ev.UnmarshalCBOR(bytes.NewReader(val)); err != nil {
				cbErr = fmt.Errorf("decode event: %w", err)
				return cbErr
			}
			_, topics, ok := ethLogFromEntries(ev.Entries)
			if !ok {
				return nil
			}
			ab, ok := emitters[ev.Emitter]
			if !ok {
				addr, err := c.emitterEthAddr(ctx, acc, ev.Emitter)
				if err != nil {
					cbErr = err
					return err
				}
				if ab, err = hex.DecodeString(strings.TrimPrefix(addr, "0x")); err != nil {
					cbErr = err
					return err
				}
				emitters[ev.Emitter] = ab
			}
			ethBloomSet(bloom, ab)
			for _, t := range topics {
				ethBloomSet(bloom, t)
			}
			return nil
		})
		if cbErr != nil {
			return 0, nil, cbErr
		}
		if err != nil {
			return 0, nil, fmt.Errorf("events AMT %s: %w", root, err)
		}
	}
	return gas, bloom, nil
}

// ethBlockFor builds the Lotus eth block for incl. exec is the tipset
// that executed incl (nil for the unexecuted head: gasUsed 0 and the
// "unknown" full bloom, exactly what Lotus reports without an index).
func (c *ChainAPI) ethBlockFor(ctx context.Context, bg hamt.BlockGetter, incl, exec *types.TipSet, fullTx bool) (map[string]any, error) {
	s := c.ethBlockStateGet()
	key := incl.Key().String()
	var sum *ethBlockSummary
	if exec != nil {
		if fullTx {
			if b := s.getFull(key); b != nil {
				return copyBlock(b), nil
			}
		}
		if sum = s.getSummary(key); sum != nil && !fullTx {
			blk := ethBlockHeader(incl, sum.gasUsed, sum.bloom, len(sum.txHashes) > 0)
			blk["transactions"] = hashesAsAny(sum.txHashes)
			return blk, nil
		}
	}

	msgs, isSecp, err := c.ethTipsetMessages(ctx, bg, incl, exec)
	if err != nil {
		return nil, fmt.Errorf("messages of tipset %d: %w", incl.Height(), err)
	}
	acc := c.ethStateAccessor(incl, exec, bg)
	chainID := c.ethChainID()
	blockHash := ethTipsetHash(incl.Key())
	blockNum := "0x" + abiEpochHex(incl.Height())

	sms, err := c.loadMsgsConcurrent(ctx, bg, msgs, isSecp)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(msgs))
	txObjs := make([]any, 0, len(msgs))
	for i, mc := range msgs {
		t, err := c.ethTxForMessage(ctx, acc, sms[i], mc, chainID)
		if err != nil {
			return nil, fmt.Errorf("failed to convert msg %s to ethTx: %w", mc, err)
		}
		hashes = append(hashes, t.hashHex())
		if fullTx {
			txObjs = append(txObjs, t.toJSON(blockHash, blockNum, "0x"+uint64Hex(uint64(i))))
		}
	}

	switch {
	case sum != nil:
		// gas / bloom already known (receipts + events not re-walked).
	case exec != nil:
		sum = &ethBlockSummary{txHashes: hashes}
		if sum.gasUsed, sum.bloom, err = c.ethTipsetGasAndBloom(ctx, bg, acc, exec); err != nil {
			return nil, fmt.Errorf("gas/bloom of tipset %d: %w", incl.Height(), err)
		}
		s.putSummary(key, sum)
	default:
		sum = &ethBlockSummary{txHashes: hashes, bloom: ethFullBloom()}
	}

	blk := ethBlockHeader(incl, sum.gasUsed, sum.bloom, len(hashes) > 0)
	if fullTx {
		blk["transactions"] = txObjs
		if exec != nil {
			s.putFull(key, blk)
			return copyBlock(blk), nil
		}
		return blk, nil
	}
	blk["transactions"] = hashesAsAny(hashes)
	return blk, nil
}

// loadMsgsConcurrent loads a tipset's messages with bounded parallelism
// (local blockstore reads are cheap; network fetches for missing ones
// overlap instead of queueing).
func (c *ChainAPI) loadMsgsConcurrent(ctx context.Context, bg hamt.BlockGetter, msgs []cid.Cid, isSecp []bool) ([]*types.SignedMessage, error) {
	out := make([]*types.SignedMessage, len(msgs))
	errs := make([]error, len(msgs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := ethTxScanConcurrency
	if len(msgs) < workers {
		workers = len(msgs)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i], errs[i] = c.loadChainMsg(ctx, bg, msgs[i], isSecp[i])
			}
		}()
	}
	for i := range msgs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func hashesAsAny(h []string) []any {
	out := make([]any, len(h))
	for i, v := range h {
		out[i] = v
	}
	return out
}

// --- block param resolution ---------------------------------------------

func parseEthQuantity(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	if len(t) >= 2 && (t[:2] == "0x" || t[:2] == "0X") {
		return strconv.ParseUint(t[2:], 16, 64)
	}
	return strconv.ParseUint(t, 10, 64)
}

// ethResolveBlockParam maps an eth block tag / number to (incl, exec)
// with Lotus tipSetResolver semantics (v1, no F3 finality).
func (c *ChainAPI) ethResolveBlockParam(ctx context.Context, bg hamt.BlockGetter, blockParam string) (*types.TipSet, *types.TipSet, error) {
	head := c.HeaderStore.Head()
	if head == nil {
		return nil, nil, errors.New("no local head")
	}
	switch strings.ToLower(strings.TrimSpace(blockParam)) {
	case "earliest":
		return nil, nil, errEthEarliest
	case "pending":
		return head, nil, nil
	case "latest", "":
		if head.Parents().IsEmpty() {
			return head, nil, nil
		}
		pbh, err := c.loadHeader(ctx, bg, head.Parents().Cids()[0])
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get parent tipset: %w", err)
		}
		return c.ethTipsetAt(ctx, bg, pbh.Height, false)
	case "safe":
		h := head.Height() - ethSafeEpochDelay
		if h < 0 {
			h = 0
		}
		return c.ethTipsetAt(ctx, bg, h, true)
	case "finalized":
		h := head.Height() - ethChainFinality
		if h < 0 {
			h = 0
		}
		return c.ethTipsetAt(ctx, bg, h, true)
	}
	n, err := parseEthQuantity(blockParam)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse block number: %v", err)
	}
	if abi.ChainEpoch(n) > head.Height()-1 {
		return nil, nil, errEthFutureEpoch
	}
	return c.ethTipsetAt(ctx, bg, abi.ChainEpoch(n), false)
}

// ethLocalBlockMode reports whether the full local block path can run.
func (c *ChainAPI) ethLocalBlockMode() bool {
	return c.HeaderStore != nil && c.BlockGetter != nil
}

// localEthGetBlockByNumber: (block, true, nil) served; (nil, true, err)
// a definitive error (future epoch, null round, ...); (nil, false, nil)
// not servable locally, try the bridge.
func (c *ChainAPI) localEthGetBlockByNumber(ctx context.Context, blockParam string, fullTx bool) (any, bool, error) {
	if !c.ethLocalBlockMode() {
		return nil, false, nil
	}
	bg := c.ethTxBlockGetter()
	incl, exec, err := c.ethResolveBlockParam(ctx, bg, blockParam)
	if err != nil {
		if errors.Is(err, errEthBeyondWindow) && c.Bridge != nil {
			return nil, false, nil
		}
		var nr *ethNullRoundError
		if errors.Is(err, errEthFutureEpoch) || errors.As(err, &nr) || errors.Is(err, errEthEarliest) || c.Bridge == nil {
			return nil, true, err
		}
		ethLocalMiss("eth_getBlockByNumber", blockParam, "", "resolve: "+err.Error(), true)
		return nil, false, nil
	}
	blk, err := c.ethBlockFor(ctx, bg, incl, exec, fullTx)
	if err != nil {
		ethLocalMiss("eth_getBlockByNumber", blockParam, "", "build: "+err.Error(), c.Bridge != nil)
		if c.Bridge != nil {
			return nil, false, nil
		}
		if !fullTx {
			// Degraded: messages / receipts not loadable right now. Keep
			// header readers (go-ethereum HeaderByNumber for the send
			// path's base fee) working with the header-only block; a
			// full-tx request (reorg check) gets the error instead of a
			// misleading empty body.
			return tipsetToEthBlock(incl), true, nil
		}
		return nil, true, err
	}
	return blk, true, nil
}

// localEthGetBlockByHash: same contract as localEthGetBlockByNumber; an
// unknown hash with no bridge is null (Ethereum convention).
func (c *ChainAPI) localEthGetBlockByHash(ctx context.Context, blockHash string, fullTx bool) (any, bool, error) {
	if c.HeaderStore == nil {
		return nil, false, nil
	}
	if c.BlockGetter == nil {
		return c.headerOnlyEthGetBlockByHash(blockHash)
	}
	want, ok := parseEthHash(blockHash)
	if !ok {
		return nil, false, nil
	}
	bg := c.ethTxBlockGetter()
	incl, exec, found, err := c.ethTipsetByHash(ctx, bg, want)
	if err != nil {
		if c.Bridge != nil {
			return nil, false, nil
		}
		return nil, true, err
	}
	if !found {
		if c.Bridge != nil {
			return nil, false, nil
		}
		return nil, true, nil
	}
	blk, err := c.ethBlockFor(ctx, bg, incl, exec, fullTx)
	if err != nil {
		ethLocalMiss("eth_getBlockByHash", blockHash, "", "build: "+err.Error(), c.Bridge != nil)
		if c.Bridge != nil {
			return nil, false, nil
		}
		return nil, true, err
	}
	return blk, true, nil
}

// ethResolveBlockRef resolves either a block param (number / tag) or a
// block hash to (incl, exec). found=false only for an unknown hash.
func (c *ChainAPI) ethResolveBlockRef(ctx context.Context, bg hamt.BlockGetter, blockParam, blockHash string) (*types.TipSet, *types.TipSet, bool, error) {
	if blockHash == "" {
		incl, exec, err := c.ethResolveBlockParam(ctx, bg, blockParam)
		return incl, exec, err == nil, err
	}
	want, ok := parseEthHash(blockHash)
	if !ok {
		return nil, nil, false, fmt.Errorf("invalid block hash %q", blockHash)
	}
	return c.ethTipsetByHash(ctx, bg, want)
}

// localEthTxByBlockAndIndex mirrors lotus getTransactionByTipsetAndIndex.
func (c *ChainAPI) localEthTxByBlockAndIndex(ctx context.Context, blockParam, blockHash, index string) (any, bool, error) {
	if !c.ethLocalBlockMode() {
		return nil, false, nil
	}
	idx, err := parseEthQuantity(index)
	if err != nil {
		return nil, true, fmt.Errorf("invalid transaction index %q: %w", index, err)
	}
	bg := c.ethTxBlockGetter()
	incl, exec, found, err := c.ethResolveBlockRef(ctx, bg, blockParam, blockHash)
	if err != nil || !found {
		if c.Bridge != nil {
			return nil, false, nil
		}
		if err == nil {
			err = fmt.Errorf("failed to get tipset by hash: %s not found", blockHash)
		}
		return nil, true, err
	}
	msgs, isSecp, err := c.ethTipsetMessages(ctx, bg, incl, exec)
	if err != nil {
		if c.Bridge != nil {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("failed to get messages for tipset: %w", err)
	}
	if idx >= uint64(len(msgs)) {
		return nil, true, fmt.Errorf("index %d out of range: tipset contains %d messages", idx, len(msgs))
	}
	sm, err := c.loadChainMsg(ctx, bg, msgs[idx], isSecp[idx])
	if err != nil {
		if c.Bridge != nil {
			return nil, false, nil
		}
		return nil, true, err
	}
	t, err := c.ethTxForMessage(ctx, c.ethStateAccessor(incl, exec, bg), sm, msgs[idx], c.ethChainID())
	if err != nil {
		return nil, true, fmt.Errorf("failed to create Ethereum transaction: %w", err)
	}
	return t.toJSON(ethTipsetHash(incl.Key()), "0x"+abiEpochHex(incl.Height()), "0x"+uint64Hex(idx)), true, nil
}

// localEthBlockTxCount mirrors lotus countTipsetMsgs over
// BlockMsgsForTipset, which already applies the cross-block dedupe and
// nonce rule: the count equals the block's transaction list length.
func (c *ChainAPI) localEthBlockTxCount(ctx context.Context, blockParam, blockHash string) (string, bool, error) {
	if !c.ethLocalBlockMode() {
		return "", false, nil
	}
	bg := c.ethTxBlockGetter()
	incl, exec, found, err := c.ethResolveBlockRef(ctx, bg, blockParam, blockHash)
	if err != nil || !found {
		if c.Bridge != nil {
			return "", false, nil
		}
		if err == nil {
			err = fmt.Errorf("failed to get tipset by hash: %s not found", blockHash)
		}
		return "", true, err
	}
	msgs, _, err := c.ethTipsetMessages(ctx, bg, incl, exec)
	if err != nil {
		if c.Bridge != nil {
			return "", false, nil
		}
		return "", true, fmt.Errorf("error loading messages for tipset: %w", err)
	}
	return "0x" + uint64Hex(uint64(len(msgs))), true, nil
}

// EthGetTransactionByBlockHashAndIndex returns the index-th tx of a
// block identified by hash (local first, bridge fallback).
func (c *ChainAPI) EthGetTransactionByBlockHashAndIndex(ctx context.Context, blockHash string, index string) (any, error) {
	if out, served, err := c.localEthTxByBlockAndIndex(ctx, "", blockHash, index); served {
		return out, err
	}
	return c.forwardEth(ctx, "eth_getTransactionByBlockHashAndIndex", []any{blockHash, index})
}

// EthGetBlockTransactionCountByNumber returns the message count of a
// block (Lotus countTipsetMsgs semantics).
func (c *ChainAPI) EthGetBlockTransactionCountByNumber(ctx context.Context, blockParam string) (string, error) {
	if out, served, err := c.localEthBlockTxCount(ctx, blockParam, ""); served {
		return out, err
	}
	raw, err := c.forwardEth(ctx, "eth_getBlockTransactionCountByNumber", []any{blockParam})
	if err != nil {
		return "", err
	}
	s, _ := raw.(string)
	return s, nil
}

// EthGetBlockTransactionCountByHash is the by-hash variant.
func (c *ChainAPI) EthGetBlockTransactionCountByHash(ctx context.Context, blockHash string) (string, error) {
	if out, served, err := c.localEthBlockTxCount(ctx, "", blockHash); served {
		return out, err
	}
	raw, err := c.forwardEth(ctx, "eth_getBlockTransactionCountByHash", []any{blockHash})
	if err != nil {
		return "", err
	}
	s, _ := raw.(string)
	return s, nil
}
