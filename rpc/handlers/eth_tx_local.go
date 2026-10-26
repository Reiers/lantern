package handlers

// Local, bridge-off eth_getTransactionReceipt / eth_getTransactionByHash
// for stock Curio-PDP (lantern#172 follow-up).
//
// Curio's message watcher (tasks/message/watch_eth.go) polls
// eth_getTransactionReceipt for every tx it sent and stores the receipt
// JSON; the PDP watchers then parse DataSetCreated / PiecesAdded out of
// receipt.Logs. So a receipt must not only exist, it must carry the exact
// logs Lotus would return.
//
// Resolution, Lotus-parity:
//
//  1. Walk the canonical chain backward from the local head by PARENT
//     LINKS, never by the header store's per-height tipset. The store can
//     hold a partial tipset at a height (only the blocks we happened to
//     receive); the child's Parents key is the authoritative full key, so
//     every inclusion tipset is rebuilt from it (headers from the store or,
//     if missing, fetched by CID and verified).
//  2. Per inclusion tipset, build the execution-ordered message list (per
//     block: BLS then secp, duplicates once; nonce rule applied when the
//     receipt count disagrees) and cache it.
//  3. Match the tx: by message CID when we published it
//     (eth_sendRawTransaction index), else by the CID encoded in the hash
//     (native messages), else by recomputing the eth tx hash of every
//     delegated-signature message in the tipset.
//  4. Receipt = child.ParentMessageReceipts[idx] (CID-verified AMT walk);
//     logs = the receipt's events AMT, logIndex counted over every event in
//     the tipset, emitter resolved to its f410 address like Lotus.
//
// blockHash/blockNumber/transactionIndex follow Lotus
// newEthTxFromMessageLookup: the INCLUSION tipset, hashed as the blake2b
// CID of its TipSetKey.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-keccak"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	eam19 "github.com/filecoin-project/go-state-types/builtin/v19/eam"
	statecrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/chain/ethtx"
	"github.com/Reiers/lantern/chain/msgsearch"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/amt"
	"github.com/Reiers/lantern/state/hamt"
)

// defaultEthTxLookback bounds how far back (in epochs) from head a receipt
// / tx-by-hash lookup walks. ~16.7h at 30s epochs. Override with
// ChainAPI.EthTxLookback or LANTERN_ETH_TX_LOOKBACK.
const defaultEthTxLookback abi.ChainEpoch = 2000

// ethTxIndexCacheMax caps the per-tipset message index cache.
const ethTxIndexCacheMax = 4096

// errEthTxNotFound is the internal "walked the whole window, no match".
var errEthTxNotFound = errors.New("eth tx not found within lookback")

func (c *ChainAPI) ethTxLookback() abi.ChainEpoch {
	if c.EthTxLookback > 0 {
		return c.EthTxLookback
	}
	if v := strings.TrimSpace(os.Getenv("LANTERN_ETH_TX_LOOKBACK")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return abi.ChainEpoch(n)
		}
	}
	return defaultEthTxLookback
}

// ethChainID returns the EIP-155 chain id this node serves.
func (c *ChainAPI) ethChainID() uint64 {
	if s, err := c.EthChainId(context.Background()); err == nil {
		if v := parseEthUintDefault(s, 0); v != 0 {
			return v
		}
	}
	return chainIDForNetwork(c.NetworkName)
}

// --- per-tipset message index cache ------------------------------------

type tsMsgIndex struct {
	height abi.ChainEpoch
	msgs   []cid.Cid // execution order == receipt order
	isSecp []bool
	pos    map[cid.Cid]int

	ethOnce sync.Once
	ethErr  error
	ethPos  map[[32]byte]int // eth tx hash -> idx (computed lazily)
}

type ethTxCache struct {
	mu       sync.Mutex
	byKey    map[string]*tsMsgIndex
	order    []string
	emitters map[abi.ActorID]string
}

func (c *ChainAPI) ethTxCacheGet() *ethTxCache {
	c.ethTxCacheOnce.Do(func() {
		c.ethTxC = &ethTxCache{byKey: map[string]*tsMsgIndex{}, emitters: map[abi.ActorID]string{}}
	})
	return c.ethTxC
}

func (ec *ethTxCache) get(k string) *tsMsgIndex {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return ec.byKey[k]
}

func (ec *ethTxCache) put(k string, ix *tsMsgIndex) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if _, ok := ec.byKey[k]; ok {
		return
	}
	if len(ec.order) >= ethTxIndexCacheMax {
		old := ec.order[0]
		ec.order = ec.order[1:]
		delete(ec.byKey, old)
	}
	ec.byKey[k] = ix
	ec.order = append(ec.order, k)
}

// --- canonical chain walking -------------------------------------------

// ethTxBlockGetter is the retry-wrapped getter used by the tx lookups.
// Message / receipt / event blocks can lag the header by a beat on a
// gossip-fed node (see StateSearchMsg, lantern#50).
func (c *ChainAPI) ethTxBlockGetter() hamt.BlockGetter {
	return newRetryingBlockGetter(c.BlockGetter, 2, 18*time.Second)
}

// loadHeader returns a block header from the header store, or fetches it
// by CID (verified) when the store doesn't have it.
func (c *ChainAPI) loadHeader(ctx context.Context, bg hamt.BlockGetter, k cid.Cid) (*types.BlockHeader, error) {
	if c.HeaderStore != nil {
		if bh, err := c.HeaderStore.Get(k); err == nil && bh != nil {
			return bh, nil
		}
	}
	raw, err := bg.Get(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("fetch header %s: %w", k, err)
	}
	if err := hamt.VerifyBlockCID(k, raw); err != nil {
		return nil, err
	}
	return types.DecodeBlock(raw)
}

// loadFullTipSet rebuilds the tipset for key tsk from its block headers.
func (c *ChainAPI) loadFullTipSet(ctx context.Context, bg hamt.BlockGetter, tsk types.TipSetKey) (*types.TipSet, error) {
	cids := tsk.Cids()
	if len(cids) == 0 {
		return nil, errors.New("empty tipset key")
	}
	blks := make([]*types.BlockHeader, 0, len(cids))
	for _, k := range cids {
		bh, err := c.loadHeader(ctx, bg, k)
		if err != nil {
			return nil, err
		}
		blks = append(blks, bh)
	}
	return types.NewTipSet(blks)
}

// ethTipsetHash is Lotus' eth block hash for a tipset: the blake2b-256
// digest of the CBOR TipSetKey (EthHashFromCid(tsk.Cid())).
func ethTipsetHash(tsk types.TipSetKey) string {
	kc, err := tsk.Cid()
	if err != nil {
		return "0x" + strings.Repeat("00", 32)
	}
	return EthHashFromCid(kc)
}

// tipsetMsgIndex returns the (cached) execution-ordered message index for
// inclusion tipset incl, cross-checked against exec's receipt count.
func (c *ChainAPI) tipsetMsgIndex(ctx context.Context, bg hamt.BlockGetter, incl, exec *types.TipSet) (*tsMsgIndex, error) {
	cache := c.ethTxCacheGet()
	key := incl.Key().String()
	if ix := cache.get(key); ix != nil {
		return ix, nil
	}
	s := msgsearch.New(c.HeaderStore, bg)
	per, err := s.BlockMessages(ctx, incl)
	if err != nil {
		return nil, err
	}
	msgs, isSecp := msgsearch.ExecutionOrder(per)
	nrec, err := amt.CountV2(ctx, exec.Blocks()[0].ParentMessageReceipts, bg)
	if err != nil {
		return nil, fmt.Errorf("receipts root: %w", err)
	}
	if uint64(len(msgs)) != nrec {
		// Two blocks carried conflicting (sender, nonce) messages: apply
		// Lotus' BlockMsgsForTipset nonce rule with message bodies.
		msgs, isSecp, err = c.applyNonceRule(ctx, bg, incl, per)
		if err != nil {
			return nil, err
		}
		if uint64(len(msgs)) != nrec {
			return nil, fmt.Errorf("tipset %d: %d ordered messages vs %d receipts", incl.Height(), len(msgs), nrec)
		}
	}
	ix := &tsMsgIndex{height: incl.Height(), msgs: msgs, isSecp: isSecp, pos: make(map[cid.Cid]int, len(msgs))}
	for i, m := range msgs {
		ix.pos[m] = i
	}
	cache.put(key, ix)
	return ix, nil
}

// applyNonceRule mirrors lotus ChainStore.BlockMsgsForTipset selectMsg.
func (c *ChainAPI) applyNonceRule(ctx context.Context, bg hamt.BlockGetter, incl *types.TipSet, per []msgsearch.BlockMsgCIDs) ([]cid.Cid, []bool, error) {
	acc := accessor.New(&trustedroot.TrustedRoot{Epoch: incl.Height(), StateRoot: incl.ParentState()}, bg)
	applied := map[address.Address]uint64{}
	useIDs := false
	resolve := func(a address.Address) (address.Address, error) {
		id, _, err := acc.LookupID(ctx, a)
		return id, err
	}
	sel := func(m *types.Message) (bool, error) {
		var sender address.Address
		var err error
		switch {
		case useIDs:
			if sender, err = resolve(m.From); err != nil {
				return false, err
			}
		case m.From.Protocol() != address.ID:
			sender = m.From
		default:
			useIDs = true
			re := map[address.Address]uint64{}
			for robust, n := range applied {
				id, err := resolve(robust)
				if err != nil {
					return false, err
				}
				re[id] = n
			}
			applied = re
			if sender, err = resolve(m.From); err != nil {
				return false, err
			}
		}
		if _, ok := applied[sender]; !ok {
			applied[sender] = m.Nonce
		}
		if applied[sender] != m.Nonce {
			return false, nil
		}
		applied[sender]++
		return true, nil
	}
	var out []cid.Cid
	var isSecp []bool
	seen := map[cid.Cid]bool{}
	for _, b := range per {
		for pass := 0; pass < 2; pass++ {
			list, secp := b.Bls, false
			if pass == 1 {
				list, secp = b.Secp, true
			}
			for _, mc := range list {
				if seen[mc] {
					continue
				}
				sm, err := c.loadChainMsg(ctx, bg, mc, secp)
				if err != nil {
					return nil, nil, err
				}
				ok, err := sel(&sm.Message)
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					continue
				}
				seen[mc] = true
				out = append(out, mc)
				isSecp = append(isSecp, secp)
			}
		}
	}
	return out, isSecp, nil
}

// loadChainMsg fetches a message body by CID. secp messages are stored as
// SignedMessage; BLS ones as bare Message (returned with a BLS sig type).
func (c *ChainAPI) loadChainMsg(ctx context.Context, bg hamt.BlockGetter, mc cid.Cid, secp bool) (*types.SignedMessage, error) {
	raw, err := bg.Get(ctx, mc)
	if err != nil {
		return nil, fmt.Errorf("fetch message %s: %w", mc, err)
	}
	if err := hamt.VerifyBlockCID(mc, raw); err != nil {
		return nil, err
	}
	if secp {
		sm, err := types.DecodeSignedMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("decode signed message %s: %w", mc, err)
		}
		return sm, nil
	}
	m, err := types.DecodeMessage(raw)
	if err != nil {
		return nil, fmt.Errorf("decode message %s: %w", mc, err)
	}
	return &types.SignedMessage{Message: *m, Signature: statecrypto.Signature{Type: statecrypto.SigTypeBLS}}, nil
}

// ensureEthHashes computes the eth tx hash of every message in ix (lotus
// ethTxHashFromSignedMessage): delegated -> keccak of the rebuilt signed
// EIP-1559 envelope; secp / BLS -> the CID digest.
func (c *ChainAPI) ensureEthHashes(ctx context.Context, bg hamt.BlockGetter, ix *tsMsgIndex) error {
	ix.ethOnce.Do(func() {
		m := make(map[[32]byte]int, len(ix.msgs))
		chainID := c.ethChainID()
		for i, mc := range ix.msgs {
			var h [32]byte
			if ix.isSecp[i] {
				sm, err := c.loadChainMsg(ctx, bg, mc, true)
				if err != nil {
					ix.ethErr = err
					return
				}
				if sm.Signature.Type == statecrypto.SigTypeDelegated {
					tx, _, err := ethTxFromSignedMessage(sm, chainID)
					if err != nil {
						continue // not an eth tx we can rebuild (e.g. legacy); unreachable by hash
					}
					if h, err = tx.TxHash(); err != nil {
						continue
					}
					m[h] = i
					continue
				}
			}
			if d, ok := cidDigest(mc); ok {
				h = d
				m[h] = i
			}
		}
		ix.ethPos = m
	})
	if ix.ethErr != nil {
		// Let a later call retry (the failure was a fetch, not a decode).
		ix.ethOnce = sync.Once{}
		err := ix.ethErr
		ix.ethErr = nil
		return err
	}
	return nil
}

func cidDigest(c cid.Cid) ([32]byte, bool) {
	var out [32]byte
	dec, err := mh.Decode(c.Hash())
	if err != nil || len(dec.Digest) != 32 {
		return out, false
	}
	copy(out[:], dec.Digest)
	return out, true
}

// cidFromEthHash builds the dag-cbor/blake2b-256 CID whose digest is h (the
// native-message form of an eth tx hash).
func cidFromEthHash(h [32]byte) (cid.Cid, error) {
	enc, err := mh.Encode(h[:], mh.BLAKE2B_MIN+31)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(cid.DagCBOR, enc), nil
}

func parseEthHash(s string) ([32]byte, bool) {
	var out [32]byte
	n := normalizeHashHex(s)
	if n == "" {
		return out, false
	}
	b, err := hex.DecodeString(n[2:])
	if err != nil || len(b) != 32 {
		return out, false
	}
	copy(out[:], b)
	return out, true
}

// ethTxHit is a located message.
type ethTxHit struct {
	incl, exec *types.TipSet
	idx        int
	msgCID     cid.Cid
	isSecp     bool
	receipt    types.MessageReceipt
}

// findEthTx walks the canonical chain from head looking for the tx.
// knownCID (from our send index) short-circuits hash recomputation.
// Returns errEthTxNotFound when the window holds no match.
// minEpoch >= 0 raises the lower bound of the walk (never lowers it).
func (c *ChainAPI) findEthTx(ctx context.Context, bg hamt.BlockGetter, hash [32]byte, knownCID cid.Cid, minEpoch abi.ChainEpoch) (*ethTxHit, error) {
	if c.HeaderStore == nil || c.BlockGetter == nil {
		return nil, errors.New("header store / block getter not configured")
	}
	exec := c.HeaderStore.Head()
	if exec == nil {
		return nil, errors.New("no local head")
	}
	minH := exec.Height() - c.ethTxLookback()
	if minEpoch > minH {
		minH = minEpoch
	}
	nativeCID, _ := cidFromEthHash(hash)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pk := exec.Parents()
		if pk.IsEmpty() {
			return nil, errEthTxNotFound
		}
		incl, err := c.loadFullTipSet(ctx, bg, pk)
		if err != nil {
			return nil, fmt.Errorf("load parent tipset of %d: %w", exec.Height(), err)
		}
		if incl.Height() < minH {
			return nil, errEthTxNotFound
		}
		ix, err := c.tipsetMsgIndex(ctx, bg, incl, exec)
		if err != nil {
			return nil, fmt.Errorf("index tipset %d: %w", incl.Height(), err)
		}
		idx, ok := -1, false
		if knownCID.Defined() {
			idx, ok = ix.pos[knownCID]
		} else {
			if idx, ok = ix.pos[nativeCID]; !ok {
				if err := c.ensureEthHashes(ctx, bg, ix); err != nil {
					return nil, fmt.Errorf("eth hashes for tipset %d: %w", incl.Height(), err)
				}
				idx, ok = ix.ethPos[hash]
			}
		}
		if ok {
			rraw, _, err := amt.LookupV2(ctx, exec.Blocks()[0].ParentMessageReceipts, uint64(idx), bg)
			if err != nil {
				return nil, fmt.Errorf("receipt %d at %d: %w", idx, exec.Height(), err)
			}
			var rec types.MessageReceipt
			if err := rec.UnmarshalCBOR(bytes.NewReader(rraw)); err != nil {
				return nil, fmt.Errorf("decode receipt: %w", err)
			}
			return &ethTxHit{incl: incl, exec: exec, idx: idx, msgCID: ix.msgs[idx], isSecp: ix.isSecp[idx], receipt: rec}, nil
		}
		exec = incl
	}
}

// --- eth tx reconstruction ---------------------------------------------

// ethTxFromSignedMessage rebuilds the EIP-1559 tx from a delegated-signed
// Filecoin message (lotus EthTransactionFromSignedFilecoinMessage +
// EthTxArgsFromUnsignedFilecoinMessage). Returns the tx and the 20-byte
// sender.
func ethTxFromSignedMessage(sm *types.SignedMessage, chainID uint64) (*ethtx.Eth1559Tx, [20]byte, error) {
	var from [20]byte
	if sm.Signature.Type != statecrypto.SigTypeDelegated {
		return nil, from, errors.New("not a delegated signature")
	}
	if len(sm.Signature.Data) != 65 {
		return nil, from, fmt.Errorf("unsupported delegated signature length %d (only EIP-1559)", len(sm.Signature.Data))
	}
	msg := &sm.Message
	if msg.Version != 0 {
		return nil, from, fmt.Errorf("unsupported message version %d", msg.Version)
	}
	f, ok := f410Payload(msg.From)
	if !ok {
		return nil, from, fmt.Errorf("sender %s is not an f410 address", msg.From)
	}
	from = f
	var input []byte
	if len(msg.Params) > 0 {
		r := bytes.NewReader(msg.Params)
		b, err := cbg.ReadByteArray(r, uint64(len(msg.Params)))
		if err != nil {
			return nil, from, fmt.Errorf("decode params: %w", err)
		}
		if r.Len() != 0 {
			return nil, from, errors.New("extra data after params")
		}
		input = b
	}
	var to *[20]byte
	switch {
	case msg.To == builtin.EthereumAddressManagerActorAddr:
		if msg.Method != builtin.MethodsEAM.CreateExternal {
			return nil, from, fmt.Errorf("EAM method %d is not CreateExternal", msg.Method)
		}
	case msg.Method == builtin.MethodsEVM.InvokeContract:
		a, err := ethAddrBytesFromFilecoin(msg.To)
		if err != nil {
			return nil, from, err
		}
		to = &a
	default:
		return nil, from, fmt.Errorf("invalid method %d for an eth tx", msg.Method)
	}
	sig := sm.Signature.Data
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
		return nil, from, err
	}
	return tx, from, nil
}

// f410Payload returns the 20-byte eth address of an EAM-namespace f4.
func f410Payload(a address.Address) ([20]byte, bool) {
	var out [20]byte
	if a.Protocol() != address.Delegated {
		return out, false
	}
	payload := a.Payload()
	// payload = uvarint(namespace) || subaddress
	n, w := uvarint(payload)
	if w <= 0 || n != uint64(builtin.EthereumAddressManagerActorID) {
		return out, false
	}
	sub := payload[w:]
	if len(sub) != 20 {
		return out, false
	}
	copy(out[:], sub)
	return out, true
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

// ethAddrBytesFromFilecoin mirrors lotus EthAddressFromFilecoinAddress for
// the two convertible protocols: ID -> masked ID, f410 -> payload.
func ethAddrBytesFromFilecoin(a address.Address) ([20]byte, error) {
	var out [20]byte
	switch a.Protocol() {
	case address.ID:
		id, err := address.IDFromAddress(a)
		if err != nil {
			return out, err
		}
		out[0] = 0xff
		for i := 0; i < 8; i++ {
			out[19-i] = byte(id >> (8 * i))
		}
		return out, nil
	case address.Delegated:
		if p, ok := f410Payload(a); ok {
			return p, nil
		}
	}
	return out, fmt.Errorf("cannot convert %s to an eth address", a)
}

// lookupEthAddress mirrors lotus lookupEthAddress: f410 directly, else the
// actor's delegated address if it has one, else its masked ID.
func (c *ChainAPI) lookupEthAddress(ctx context.Context, acc *accessor.Accessor, a address.Address) (string, error) {
	if p, ok := f410Payload(a); ok {
		return "0x" + hex.EncodeToString(p[:]), nil
	}
	id, _, err := acc.LookupID(ctx, a)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", a, err)
	}
	actID, err := address.IDFromAddress(id)
	if err != nil {
		return "", err
	}
	return c.emitterEthAddr(ctx, acc, abi.ActorID(actID))
}

// emitterEthAddr resolves an actor ID to the eth address Lotus reports
// for it (event emitter / tx party): its f410 if it has a delegated
// address, else the masked ID. A missing actor maps to the masked ID
// (lotus chainindex actorToDelegatedAddress); any other read error is
// returned so we never silently emit a wrong address.
func (c *ChainAPI) emitterEthAddr(ctx context.Context, acc *accessor.Accessor, id abi.ActorID) (string, error) {
	cache := c.ethTxCacheGet()
	cache.mu.Lock()
	if s, ok := cache.emitters[id]; ok {
		cache.mu.Unlock()
		return s, nil
	}
	cache.mu.Unlock()
	masked := ethAddrFromActorID(id)
	idAddr, err := address.NewIDAddress(uint64(id))
	if err != nil {
		return masked, nil
	}
	act, _, err := acc.GetActorByID(ctx, idAddr)
	if err != nil {
		if errors.Is(err, hamt.ErrNotFound) {
			return masked, nil
		}
		return "", fmt.Errorf("load actor %d: %w", id, err)
	}
	out := masked
	if act.DelegatedAddress != nil {
		if p, ok := f410Payload(*act.DelegatedAddress); ok {
			out = "0x" + hex.EncodeToString(p[:])
		}
	}
	cache.mu.Lock()
	cache.emitters[id] = out
	cache.mu.Unlock()
	return out, nil
}

// --- gas / bloom --------------------------------------------------------

const (
	gasOveruseNum   = 11
	gasOveruseDenom = 10
)

// computeGasOverestimationBurn mirrors lotus vm.ComputeGasOverestimationBurn.
func computeGasOverestimationBurn(gasUsed, gasLimit int64) (int64, int64) {
	if gasUsed == 0 {
		return 0, gasLimit
	}
	over := gasLimit - (gasOveruseNum*gasUsed)/gasOveruseDenom
	if over < 0 {
		return gasLimit - gasUsed, 0
	}
	if over > gasUsed {
		over = gasUsed
	}
	gasToBurn := big.NewInt(gasLimit - gasUsed)
	gasToBurn = big.Mul(gasToBurn, big.NewInt(over))
	gasToBurn = big.Div(gasToBurn, big.NewInt(gasUsed))
	return gasLimit - gasUsed - gasToBurn.Int64(), gasToBurn.Int64()
}

// effectiveGasPrice mirrors lotus newEthTxReceipt: (BaseFeeBurn + MinerTip
// + OverEstimationBurn) / gasUsed from vm.ComputeGasOutputs(charge=true).
func effectiveGasPrice(gasUsed, gasLimit int64, baseFee, feeCap, premium big.Int) big.Int {
	if gasUsed <= 0 {
		return big.Zero()
	}
	if baseFee.Int == nil {
		baseFee = big.Zero()
	}
	if feeCap.Int == nil {
		feeCap = big.Zero()
	}
	if premium.Int == nil {
		premium = big.Zero()
	}
	used := big.NewInt(gasUsed)
	baseToPay := baseFee
	if baseFee.GreaterThan(feeCap) {
		baseToPay = feeCap
	}
	baseFeeBurn := big.Mul(baseToPay, used)
	tip := premium
	if big.Add(baseToPay, tip).GreaterThan(feeCap) {
		tip = big.Sub(feeCap, baseToPay)
	}
	minerTip := big.Mul(tip, big.NewInt(gasLimit))
	_, burned := computeGasOverestimationBurn(gasUsed, gasLimit)
	overBurn := big.Zero()
	if burned != 0 {
		overBurn = big.Mul(baseToPay, big.NewInt(burned))
	}
	total := big.Add(big.Add(baseFeeBurn, minerTip), overBurn)
	return big.Div(total, used)
}

// ethBloomSet sets the three 2048-bit Ethereum bloom bits for v.
func ethBloomSet(bloom []byte, v []byte) {
	h := keccak.NewLegacyKeccak256()
	h.Write(v)
	d := h.Sum(nil)
	for i := 0; i < 6; i += 2 {
		bit := (uint(d[i])<<8 | uint(d[i+1])) & 2047
		bloom[256-1-bit/8] |= 1 << (bit % 8)
	}
}

// --- logs ----------------------------------------------------------------

// receiptLogs decodes the eth logs of receipt idx of exec's
// ParentMessageReceipts. logIndex counts every event emitted earlier in
// the tipset (lotus chain index event_index), EVM-shaped or not.
func (c *ChainAPI) receiptLogs(ctx context.Context, bg hamt.BlockGetter, acc *accessor.Accessor, hit *ethTxHit, txHash, blockHash, blockNum, txIndex string) ([]any, error) {
	if hit.receipt.EventsRoot == nil || !hit.receipt.EventsRoot.Defined() {
		return []any{}, nil
	}
	base := uint64(0)
	if hit.idx > 0 {
		var roots []cid.Cid
		err := amt.ForEachV2Raw(ctx, hit.exec.Blocks()[0].ParentMessageReceipts, bg, func(i uint64, raw []byte) error {
			if i >= uint64(hit.idx) {
				return nil
			}
			var r types.MessageReceipt
			if err := r.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
				return fmt.Errorf("decode receipt %d: %w", i, err)
			}
			if r.EventsRoot != nil && r.EventsRoot.Defined() {
				roots = append(roots, *r.EventsRoot)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, er := range roots {
			n, err := countEvents(ctx, bg, er)
			if err != nil {
				return nil, err
			}
			base += n
		}
	}
	out := []any{}
	k := uint64(0)
	var cbErr error
	_, err := amt.ForEachRaw(ctx, *hit.receipt.EventsRoot, bg, &amt.LookupOptions{BitWidth: types.EventAMTBitwidth}, func(_ uint64, val []byte) error {
		li := base + k
		k++
		var ev types.Event
		if err := ev.UnmarshalCBOR(bytes.NewReader(val)); err != nil {
			cbErr = fmt.Errorf("decode event: %w", err)
			return cbErr
		}
		data, topics, ok := ethLogFromEntries(ev.Entries)
		if !ok {
			return nil
		}
		addr, err := c.emitterEthAddr(ctx, acc, ev.Emitter)
		if err != nil {
			cbErr = err
			return err
		}
		th := make([]string, len(topics))
		for i, t := range topics {
			th[i] = "0x" + hex.EncodeToString(t)
		}
		out = append(out, map[string]any{
			"address":          addr,
			"data":             "0x" + hex.EncodeToString(data),
			"topics":           th,
			"removed":          false,
			"logIndex":         "0x" + uint64Hex(li),
			"transactionIndex": txIndex,
			"transactionHash":  txHash,
			"blockHash":        blockHash,
			"blockNumber":      blockNum,
		})
		return nil
	})
	if cbErr != nil {
		return nil, cbErr
	}
	if err != nil {
		return nil, fmt.Errorf("events AMT %s: %w", hit.receipt.EventsRoot, err)
	}
	return out, nil
}

func countEvents(ctx context.Context, bg hamt.BlockGetter, root cid.Cid) (uint64, error) {
	var n uint64
	_, err := amt.ForEachRaw(ctx, root, bg, &amt.LookupOptions{BitWidth: types.EventAMTBitwidth}, func(_ uint64, _ []byte) error {
		n++
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count events %s: %w", root, err)
	}
	return n, nil
}

// --- receipt / tx builders -----------------------------------------------

type ethTxView struct {
	hash             string
	from             string
	to               any // string or nil
	txType           string
	tx               *ethtx.Eth1559Tx // nil for native messages
	msg              *types.Message
	blockHash        string
	blockNumber      string
	transactionIndex string
}

func (c *ChainAPI) ethTxViewFor(ctx context.Context, bg hamt.BlockGetter, acc *accessor.Accessor, hit *ethTxHit, txHash string) (*ethTxView, error) {
	sm, err := c.loadChainMsg(ctx, bg, hit.msgCID, hit.isSecp)
	if err != nil {
		return nil, err
	}
	v := &ethTxView{
		hash:             strings.ToLower(txHash),
		txType:           "0x2",
		msg:              &sm.Message,
		blockHash:        ethTipsetHash(hit.incl.Key()),
		blockNumber:      "0x" + abiEpochHex(hit.incl.Height()),
		transactionIndex: "0x" + uint64Hex(uint64(hit.idx)),
	}
	if sm.Signature.Type == statecrypto.SigTypeDelegated {
		tx, from, err := ethTxFromSignedMessage(sm, c.ethChainID())
		if err != nil {
			return nil, err
		}
		v.tx = tx
		v.from = "0x" + hex.EncodeToString(from[:])
		if tx.To != nil {
			v.to = "0x" + hex.EncodeToString(tx.To[:])
		}
		return v, nil
	}
	// Native message: lotus ethTxFromNativeMessage addressing.
	if v.from, err = c.lookupEthAddress(ctx, acc, sm.Message.From); err != nil {
		return nil, err
	}
	to, err := c.lookupEthAddress(ctx, acc, sm.Message.To)
	if err != nil {
		return nil, err
	}
	v.to = to
	return v, nil
}

func (c *ChainAPI) execAccessor(exec *types.TipSet, bg hamt.BlockGetter) *accessor.Accessor {
	return accessor.New(&trustedroot.TrustedRoot{Epoch: exec.Height(), StateRoot: exec.ParentState()}, bg)
}

// buildEthReceipt shapes the Lotus EthTxReceipt for a located message.
func (c *ChainAPI) buildEthReceipt(ctx context.Context, bg hamt.BlockGetter, hit *ethTxHit, txHash string) (map[string]any, error) {
	acc := c.execAccessor(hit.exec, bg)
	v, err := c.ethTxViewFor(ctx, bg, acc, hit, txHash)
	if err != nil {
		return nil, err
	}
	rec := hit.receipt
	status := "0x0"
	if rec.ExitCode.IsSuccess() {
		status = "0x1"
	}
	baseFee := hit.incl.Blocks()[0].ParentBaseFee
	egp := effectiveGasPrice(rec.GasUsed, v.msg.GasLimit, big.Int(baseFee), v.msg.GasFeeCap, v.msg.GasPremium)
	var contract any
	if v.to == nil && rec.ExitCode.IsSuccess() {
		var ret eam19.CreateExternalReturn
		if err := ret.UnmarshalCBOR(bytes.NewReader(rec.Return)); err != nil {
			return nil, fmt.Errorf("parse contract creation result: %w", err)
		}
		contract = "0x" + hex.EncodeToString(ret.EthAddress[:])
	}
	logs, err := c.receiptLogs(ctx, bg, acc, hit, v.hash, v.blockHash, v.blockNumber, v.transactionIndex)
	if err != nil {
		return nil, err
	}
	bloom := make([]byte, 256)
	for _, l := range logs {
		lm := l.(map[string]any)
		for _, t := range lm["topics"].([]string) {
			b, _ := hex.DecodeString(t[2:])
			ethBloomSet(bloom, b)
		}
		ab, _ := hex.DecodeString(lm["address"].(string)[2:])
		ethBloomSet(bloom, ab)
	}
	return map[string]any{
		"transactionHash":   v.hash,
		"transactionIndex":  v.transactionIndex,
		"blockHash":         v.blockHash,
		"blockNumber":       v.blockNumber,
		"from":              v.from,
		"to":                v.to,
		"root":              "0x" + strings.Repeat("00", 32),
		"status":            status,
		"contractAddress":   contract,
		"cumulativeGasUsed": "0x0",
		"gasUsed":           "0x" + uint64Hex(uint64(rec.GasUsed)),
		"effectiveGasPrice": "0x" + bigHex(&egp),
		"logsBloom":         "0x" + hex.EncodeToString(bloom),
		"logs":              logs,
		"type":              v.txType,
	}, nil
}

// buildEthTxObject shapes the Lotus EthTx JSON for a mined eth tx.
// Native (non-delegated) messages are not rebuilt (returns nil, false).
func (c *ChainAPI) buildEthTxObject(ctx context.Context, bg hamt.BlockGetter, hit *ethTxHit, txHash string) (map[string]any, bool, error) {
	acc := c.execAccessor(hit.exec, bg)
	v, err := c.ethTxViewFor(ctx, bg, acc, hit, txHash)
	if err != nil {
		return nil, false, err
	}
	if v.tx == nil {
		return nil, false, nil
	}
	obj := ethTxJSON(v.hash, v.from, v.tx)
	obj["blockHash"] = v.blockHash
	obj["blockNumber"] = v.blockNumber
	obj["transactionIndex"] = v.transactionIndex
	return obj, true, nil
}

// ethTxJSON is the Lotus EthTx field set for an EIP-1559 tx (block fields
// null, i.e. pending; callers fill them when mined).
func ethTxJSON(hash, from string, tx *ethtx.Eth1559Tx) map[string]any {
	obj := map[string]any{
		"chainId":              "0x" + uint64Hex(tx.ChainID),
		"nonce":                "0x" + uint64Hex(tx.Nonce),
		"hash":                 strings.ToLower(hash),
		"blockHash":            nil,
		"blockNumber":          nil,
		"transactionIndex":     nil,
		"from":                 from,
		"to":                   nil,
		"value":                "0x" + bigHex(&tx.Value),
		"type":                 "0x2",
		"input":                "0x" + hex.EncodeToString(tx.Input),
		"gas":                  "0x" + uint64Hex(tx.GasLimit),
		"maxFeePerGas":         "0x" + bigHex(&tx.MaxFeePerGas),
		"maxPriorityFeePerGas": "0x" + bigHex(&tx.MaxPriorityFeePerGas),
		"accessList":           []any{},
		"v":                    "0x" + bigHex(&tx.V),
		"r":                    "0x" + bigHex(&tx.R),
		"s":                    "0x" + bigHex(&tx.S),
	}
	if tx.To != nil {
		obj["to"] = "0x" + hex.EncodeToString(tx.To[:])
	}
	return obj
}

// EthAddressToFilecoinAddress mirrors the Lotus Filecoin.* method: a
// masked-ID eth address (0xff || 11 zero || be64 id) maps to its f0 ID
// address, anything else to the EAM f410 delegated address. Pure
// conversion, no state read (Lotus does the same).
func (c *ChainAPI) EthAddressToFilecoinAddress(_ context.Context, ethAddress string) (address.Address, error) {
	raw, err := decodeEthAddr(ethAddress)
	if err != nil {
		return address.Undef, fmt.Errorf("EthAddressToFilecoinAddress: %w", err)
	}
	return ethAddrToFilecoin(raw)
}
