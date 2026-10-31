// Local FEVM eth_call execution (lantern#43 Part B, Stage 4).
//
// Wires the pure-Go EVM interpreter (vm/evm) to Lantern's verified state
// accessor: bytecode + storage come from the Stage-1 LoadEVM loader and
// Stage-2 KAMT reader, executed against the trust-anchored state tree. No
// upstream RPC. The VMBridge remains as a fallback (see EthCall) so a
// local-exec miss degrades to Glif rather than failing.
package handlers

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/builtin"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/holiman/uint256"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"golang.org/x/crypto/blake2b"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/state/kamt"
	"github.com/Reiers/lantern/vm/evm"
)

// evmBackend implements vm/evm.Backend over Lantern's verified accessor +
// blockstore. Read-only; all reads are CID-verified through the accessor.
type evmBackend struct {
	ctx     context.Context
	c       *ChainAPI
	acc     *accessor.Accessor // anchored at the call's target epoch
	reg     *actors.Registry
	chainID uint64
	blockNo uint64
	tstamp  uint64

	// bg is the retry-wrapped BlockGetter used for bytecode + KAMT
	// storage reads. Replaces the bare c.BlockGetter for the eth_call
	// hot path so lazy-fetch misses get a real second chance before we
	// declare "not served locally" (lantern#44).
	bg hamt.BlockGetter

	// execTS is the tipset the call executes in: Lotus eth_call "latest"
	// resolves to head's parent and runs on TipSetState(parent) =
	// head.ParentState(), with NUMBER/TIMESTAMP/BASEFEE/PREVRANDAO taken
	// from that tipset (chain/stmgr/call.go callInternal VMOpts). nil when
	// no header store is wired (probe / one-shot): the env-dependent
	// opcodes then fault and the call falls back.
	execTS *ltypes.TipSet

	// perf, when set, routes actor / EVM-state / bytecode / storage reads
	// through the shared burst caches (ethcall_perf.go). stateRoot is the
	// root acc is anchored at; actor lookups are cached per (stateRoot,
	// address), everything else by content address. nil perf = uncached
	// (probe / one-shot path).
	perf      *ethCallPerf
	stateRoot cid.Cid
}

// getActor resolves filAddr in the call's state, through the shared actor
// cache when available.
func (b *evmBackend) getActor(filAddr address.Address) (*accessor.Actor, error) {
	if b.perf != nil && b.stateRoot.Defined() {
		return b.perf.getActorCached(b.ctx, b.acc, b.stateRoot, filAddr)
	}
	actor, _, err := b.acc.GetActor(b.ctx, filAddr)
	return actor, err
}

// GetCode resolves addr -> EVM actor -> bytecode (CID + hash verified).
func (b *evmBackend) GetCode(a evm.Address) ([]byte, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return nil, err // miss -> empty code (not an error to callers)
	}
	if b.perf == nil {
		return actors.FetchBytecode(b.ctx, st, b.bg)
	}
	if code, ok := b.perf.code.Get(st.BytecodeCID()); ok {
		return code, nil
	}
	code, err := actors.FetchBytecode(b.ctx, st, b.bg)
	if err != nil {
		return nil, err
	}
	b.perf.code.Add(st.BytecodeCID(), code)
	return code, nil
}

// GetStorage reads slot `key` from addr's contract storage KAMT.
func (b *evmBackend) GetStorage(a evm.Address, key uint256.Int) (uint256.Int, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return uint256.Int{}, err
	}
	var sk slotCacheKey
	if b.perf != nil {
		sk = slotCacheKey{root: st.StorageRoot(), slot: key.Bytes32()}
		if b.stateRoot.Defined() {
			b.perf.warm.recordSlot(a, sk.slot, int64(b.blockNo))
		}
		if v, ok := b.perf.slots.Get(sk); ok {
			return v, nil
		}
	}
	v, _, err := kamt.GetU256(b.ctx, st.StorageRoot(), key.ToBig(), b.bg)
	if err != nil {
		return uint256.Int{}, err
	}
	var out uint256.Int
	out.SetFromBig(v)
	if b.perf != nil {
		b.perf.slots.Add(sk, out)
	}
	return out, nil
}

// GetBalance returns the actor balance (attoFIL) as a 256-bit word.
func (b *evmBackend) GetBalance(a evm.Address) (uint256.Int, error) {
	filAddr, err := ethAddrToFilecoin(a.Bytes())
	if err != nil {
		return uint256.Int{}, nil
	}
	actor, err := b.getActor(filAddr)
	if err != nil {
		if isActorNotFound(err) {
			return uint256.Int{}, nil // unknown -> 0
		}
		// A fetch/decode fault is not "zero balance": fault so the call
		// falls back instead of returning a wrong result.
		return uint256.Int{}, fmt.Errorf("balance %s: %w", filAddr, err)
	}
	var out uint256.Int
	out.SetFromBig(actor.Balance.Int)
	return out, nil
}

func (b *evmBackend) BlockNumber() uint64 { return b.blockNo }
func (b *evmBackend) Timestamp() uint64   { return b.tstamp }
func (b *evmBackend) ChainID() uint64     { return b.chainID }

// isActorNotFound reports whether err means "no actor at this address"
// (as opposed to a block fetch / decode fault).
func isActorNotFound(err error) bool {
	return errors.Is(err, accessor.ErrAddressNotFound) || errors.Is(err, hamt.ErrNotFound)
}

// ActorKind implements evm.KindBackend (FEVM get_contract_type).
func (b *evmBackend) ActorKind(a evm.Address) (evm.ActorKind, error) {
	filAddr, err := ethAddrToFilecoin(a.Bytes())
	if err != nil {
		return evm.ActorNotFound, nil
	}
	actor, err := b.getActor(filAddr)
	if err != nil {
		if isActorNotFound(err) {
			return evm.ActorNotFound, nil
		}
		return 0, fmt.Errorf("actor %s: %w", filAddr, err)
	}
	if b.reg == nil {
		return 0, fmt.Errorf("actor %s: no code registry", filAddr)
	}
	ci, ok := b.reg.Lookup(actor.Code)
	if !ok {
		actors.WarnUnknownCodeOnce(actor.Code, "eth_call")
		return 0, fmt.Errorf("actor %s: unknown code CID %s", filAddr, actor.Code)
	}
	switch ci.Kind {
	case actors.KindEvm:
		return evm.ActorEVM, nil
	case actors.KindAccount, actors.KindPlaceholder, actors.KindEthAccount:
		return evm.ActorAccount, nil
	default:
		return evm.ActorNative, nil
	}
}

var errNoExecTipset = errors.New("execution tipset unavailable (no header store)")

// BaseFee implements evm.EnvBackend. Lotus eth_call builds its message
// with GasFeeCap = 0 (EthCall.ToFilecoinMessage), and callInternal then
// zeroes the VM base fee ("If the fee cap is set to zero, make gas free",
// chain/stmgr/call.go), so BASEFEE (and GASPRICE = base fee + zero premium)
// observed by an eth_call is 0, not the tipset's ParentBaseFee. Verified
// live against Glif (Multicall3.getBasefee() returns 0).
func (b *evmBackend) BaseFee() (uint256.Int, error) {
	return uint256.Int{}, nil
}

// TipsetCIDDigest implements evm.EnvBackend for BLOCKHASH: the multihash
// digest of the tipset-key CID at epoch on the execution tipset's chain
// (null rounds resolve to the nearest earlier tipset, matching the FVM's
// tipset_cid via Lotus TipSetGetterForTipset).
//
// The key is taken from the block headers' Parents links walked back from
// the execution tipset, never from the header store's canonical-by-height
// index: that index can hold a tipset with only the subset of blocks this
// node saw (e.g. 1 of 3), whose key CID differs from the real tipset's.
// Parents always carries the full key the next tipset was mined on.
func (b *evmBackend) TipsetCIDDigest(epoch uint64) ([]byte, error) {
	if b.execTS == nil || b.c == nil || b.c.HeaderStore == nil {
		return nil, errNoExecTipset
	}
	if epoch >= uint64(b.execTS.Height()) {
		return nil, fmt.Errorf("epoch %d not before execution epoch %d", epoch, b.execTS.Height())
	}
	key := b.execTS.Parents()
	for steps := 0; steps < 1024; steps++ {
		var hdr *ltypes.BlockHeader
		for _, c := range key.Cids() {
			if h, err := b.c.HeaderStore.Get(c); err == nil && h != nil {
				hdr = h
				break
			}
		}
		if hdr == nil {
			return nil, fmt.Errorf("no header for ancestor tipset %s", key)
		}
		if uint64(hdr.Height) <= epoch {
			c, err := key.Cid()
			if err != nil {
				return nil, err
			}
			dmh, err := multihash.Decode(c.Hash())
			if err != nil {
				return nil, err
			}
			return dmh.Digest, nil
		}
		key = ltypes.NewTipSetKey(hdr.Parents...)
	}
	return nil, fmt.Errorf("ancestor walk to epoch %d exceeded bound", epoch)
}

// PrevRandao implements evm.EnvBackend: builtin-actors System::get_randomness
// = get_randomness_from_beacon(EvmPrevRandao, curr_epoch, "prevrandao"),
// i.e. DrawRandomnessFromDigest(blake2b(beacon.Data), 10, epoch, entropy)
// with the beacon entry for the execution epoch (the last entry carried by
// the execution tipset's blocks).
func (b *evmBackend) PrevRandao() (uint256.Int, error) {
	if b.execTS == nil || len(b.execTS.Blocks()) == 0 {
		return uint256.Int{}, errNoExecTipset
	}
	entries := b.execTS.Blocks()[0].BeaconEntries
	if len(entries) == 0 {
		return uint256.Int{}, fmt.Errorf("execution tipset %d carries no beacon entry", b.execTS.Height())
	}
	digest := blake2b.Sum256(entries[len(entries)-1].Data)
	const dstEvmPrevRandao = gscrypto.DomainSeparationTag(10) // builtin-actors DomainSeparationTag::EvmPrevRandao
	r, err := beacon.DrawRandomnessFromDigest(digest, dstEvmPrevRandao, b.execTS.Height(), []byte("prevrandao"))
	if err != nil {
		return uint256.Int{}, err
	}
	var out uint256.Int
	out.SetBytes(r)
	return out, nil
}

// NetworkVersion implements evm.EnvBackend.
func (b *evmBackend) NetworkVersion() (uint64, bool) {
	if b.execTS == nil || b.c == nil {
		return 0, false
	}
	sched := networkUpgradeSchedule(b.c.NetworkName)
	if sched == nil {
		return 0, false
	}
	return uint64(build.NetworkVersionAt(sched, b.execTS.Height())), true
}

var (
	_ evm.Backend     = (*evmBackend)(nil)
	_ evm.KindBackend = (*evmBackend)(nil)
	_ evm.EnvBackend  = (*evmBackend)(nil)
)

// loadEVMActor resolves an eth address to its EVM actor state. ok=false
// (no error) when the address is unknown or its code CID is known to be a
// non-EVM kind. An unknown code CID (network upgrade this build predates),
// an unsupported actors version or a head fetch/decode fault is returned as
// an error so the interpreter faults and EthCall falls back to the bridge
// instead of executing against "empty code" and returning a wrong result.
func (b *evmBackend) loadEVMActor(a evm.Address) (actors.EVMState, bool, error) {
	filAddr, err := ethAddrToFilecoin(a.Bytes())
	if err != nil {
		return nil, false, nil
	}
	actor, err := b.getActor(filAddr)
	if err != nil {
		if isActorNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("load actor %s: %w", filAddr, err)
	}
	var sk evmStateKey
	if b.perf != nil {
		if b.stateRoot.Defined() {
			b.perf.warm.recordActor(a, int64(b.blockNo))
		}
		sk = evmStateKey{code: actor.Code, head: actor.Head}
		if st, ok := b.perf.evmStates.Get(sk); ok {
			return st, true, nil
		}
	}
	st, err := actors.LoadEVM(b.ctx, actor.Code, actor.Head, b.bg, b.reg)
	if err == nil && b.perf != nil {
		b.perf.evmStates.Add(sk, st)
	}
	if err != nil {
		if isDefinitelyNotEVM(err, actor.Code, "eth_call") {
			// Known non-EVM actor (e.g. account/placeholder) -> not a contract.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("load evm actor %s (code %s): %w", filAddr, actor.Code, err)
	}
	return st, true, nil
}

// ethAddrToFilecoin maps a 20-byte eth address to its Filecoin address:
// masked-ID (0xff || 11 zero || be64 id) -> ID address; otherwise the
// f4/EAM delegated address. Mirrors the recipe in EthGetBalance.
func ethAddrToFilecoin(raw []byte) (address.Address, error) {
	if len(raw) != 20 {
		return address.Undef, fmt.Errorf("eth address must be 20 bytes, got %d", len(raw))
	}
	maskedID := raw[0] == 0xff
	for i := 1; i < 12 && maskedID; i++ {
		if raw[i] != 0x00 {
			maskedID = false
		}
	}
	if maskedID {
		actorID := uint64(0)
		for i := 12; i < 20; i++ {
			actorID = (actorID << 8) | uint64(raw[i])
		}
		return address.NewIDAddress(actorID)
	}
	return address.NewDelegatedAddress(builtin.EthereumAddressManagerActorID, raw)
}

// ethCallObject is the subset of the eth_call call-object we read.
type ethCallObject struct {
	To   string `json:"to"`
	From string `json:"from"`
	Data string `json:"data"`
	// `input` is an alias for `data` used by some clients.
	Input string `json:"input"`
	// Value is the attoFIL sent with the call. The local interpreter does
	// not model balance moves, so a non-zero value is not served locally.
	Value string `json:"value"`
}

// localEthCall executes an eth_call locally against verified state. It
// returns (result, true, nil) on a clean local execution (including a
// revert, which is surfaced as an error so the RPC layer can map it to
// code 3), or (_, false, nil) when the call can't be served locally and
// the caller should fall back to the VMBridge.
func (c *ChainAPI) localEthCall(ctx context.Context, call ethCallObject) (string, bool, error) {
	res, served, _, err := c.localEthCallWithReason(ctx, call)
	return res, served, err
}

// localEthCallWithReason is localEthCall plus, when served==false, a short
// human-readable reason for the local miss (logged by EthCall).
func (c *ChainAPI) localEthCallWithReason(ctx context.Context, call ethCallObject) (string, bool, string, error) {
	if c.Accessor == nil || c.BlockGetter == nil {
		return "", false, "local state not wired (no accessor/block getter)", nil
	}
	toRaw, err := decodeEthAddr(call.To)
	if err != nil {
		return "", false, "malformed or missing 'to' (contract creation / bad address)", nil
	}
	dataHex := call.Data
	if dataHex == "" {
		dataHex = call.Input
	}
	input, err := decodeHexData(dataHex)
	if err != nil {
		return "", false, "malformed call data hex", nil
	}
	if v := strings.TrimPrefix(strings.TrimPrefix(call.Value, "0x"), "0X"); v != "" {
		if n, ok := new(big.Int).SetString(v, 16); !ok || n.Sign() != 0 {
			return "", false, "call carries a value transfer (not modeled locally)", nil
		}
	}
	var from evm.Address
	if call.From != "" {
		if fb, err := decodeEthAddr(call.From); err == nil {
			from = evm.BytesToAddress(fb)
		}
	}

	// Anchor the read at the LIVE head state root when a header store is
	// present (long-running daemon / embedded curio-core), so eth_call sees
	// current contract state rather than the boot anchor. Fall back to the
	// boot TrustedRoot when no header store is wired (probe / one-shot).
	//
	// The head is read ONCE: the state root, the execution tipset and the
	// cache generation below must all describe the same head, even if a
	// new head lands while this call is running.
	blockNo := uint64(0)
	tstamp := uint64(0)
	if c.Trusted != nil {
		blockNo = uint64(c.Trusted.Epoch)
	}
	var execTS *ltypes.TipSet
	var head *ltypes.TipSet
	if c.HeaderStore != nil {
		if h := c.HeaderStore.Head(); h != nil && h.ParentState().Defined() {
			head = h
			// head.ParentState() is the state after executing head's
			// parent: exactly what Lotus eth_call "latest" runs on
			// (TipSetState(head.Parents())). The FEVM environment
			// (NUMBER, TIMESTAMP, BASEFEE, PREVRANDAO, BLOCKHASH
			// window) is that parent tipset's, so resolve it rather
			// than reporting head's height.
			parent, err := c.HeaderStore.GetTipSet(h.Parents())
			if err != nil || parent == nil {
				return "", false, fmt.Sprintf("execution tipset (head %d parent) not in header store", h.Height()), nil
			}
			execTS = parent
			blockNo = uint64(parent.Height())
			tstamp = parent.MinTimestamp()
		}
	}

	// Wrap the BlockGetter with a retry layer for the eth_call hot path
	// (lantern#44). Retries are bounded by LocalFEVMFetchRetries +
	// LocalFEVMFetchTimeout; zero values mean transparent passthrough.
	retries := c.LocalFEVMFetchRetries
	total := c.LocalFEVMFetchTimeout
	if retries == 0 && total == 0 {
		// Sensible defaults: 2 retries inside an 8s budget. Cheap
		// robustness against cold storage-trie blocks under load.
		retries = 2
		total = 8 * time.Second
	}
	to := evm.BytesToAddress(toRaw)

	atomic.AddUint64(&c.localEthCallTotal, 1)

	if head == nil {
		// Probe / one-shot path: boot-anchored accessor, no shared caches.
		bg := newRetryingBlockGetter(c.BlockGetter, retries, total)
		acc := c.Accessor
		be := &evmBackend{
			ctx:     ctx,
			c:       c,
			acc:     acc,
			reg:     acc.Registry(),
			chainID: chainIDForNetwork(c.NetworkName),
			blockNo: blockNo,
			tstamp:  tstamp,
			bg:      bg,
		}
		o := runLocalEthCall(be, from, to, input, call.To)
		return o.ret, o.served, o.reason, o.err
	}

	// Live-head path: shared burst caches + singleflight (ethcall_perf.go).
	// The result cache generation is the execution state identity, so a
	// result is only ever reused for a call on the very same state.
	perf := c.ethPerf()
	liveRoot := head.ParentState()
	gen := ethCallGen(execTS.Key().String(), liveRoot)
	key := hex.EncodeToString(from[:]) + "|" + hex.EncodeToString(toRaw) + "|" + hex.EncodeToString(input)
	if o, ok := perf.results.get(gen, key); ok {
		perf.resultHits.Add(1)
		return o.ret, o.served, o.reason, o.err
	}
	perf.resultMisses.Add(1)
	o, shared, err := sharedDo(ctx, &perf.callFlight, gen+"#"+key, func(sctx context.Context) (ethCallOutcome, error) {
		if o, ok := perf.results.get(gen, key); ok {
			return o, nil
		}
		bg := newRetryingBlockGetter(perf.blockGetter(), retries, total)
		acc := accessor.New(&trustedroot.TrustedRoot{Epoch: head.Height(), StateRoot: liveRoot}, bg)
		be := &evmBackend{
			ctx:       sctx,
			c:         c,
			acc:       acc,
			reg:       acc.Registry(),
			chainID:   chainIDForNetwork(c.NetworkName),
			blockNo:   blockNo,
			tstamp:    tstamp,
			bg:        bg,
			execTS:    execTS,
			perf:      perf,
			stateRoot: liveRoot,
		}
		perf.executions.Add(1)
		o := runLocalEthCall(be, from, to, input, call.To)
		perf.results.put(gen, key, o)
		return o, nil
	})
	if shared {
		perf.callShared.Add(1)
	}
	if err != nil {
		// Only the caller's own context ends up here; the shared run keeps
		// going and fills the cache for the next poll.
		return "", false, "caller context done: " + err.Error(), nil
	}
	return o.ret, o.served, o.reason, o.err
}

// runLocalEthCall executes one call on be and classifies the outcome.
func runLocalEthCall(be *evmBackend, from, to evm.Address, input []byte, callTo string) ethCallOutcome {
	res, err := evm.Call(be, from, to, input)
	if err != nil {
		// A local execution fault (e.g. an opcode we don't support yet)
		// is NOT a definitive answer; fall back to the bridge. Record the
		// reason so we can see what local coverage is still missing.
		log.Debugw("eth_call: local exec fault, will fall back", "to", callTo, "reason", err.Error())
		return ethCallOutcome{reason: "local exec fault: " + err.Error()}
	}
	if res.Reverted {
		// A revert IS a definitive answer. Surface it so EthCall maps it
		// to eth error code 3 with the revert payload.
		return ethCallOutcome{served: true, ret: "0x" + hex.EncodeToString(res.Return), err: &revertError{data: res.Return}}
	}
	return ethCallOutcome{served: true, ret: "0x" + hex.EncodeToString(res.Return)}
}

// revertError carries EVM revert data for the JSON-RPC error mapping
// (eth code 3, EExecutionReverted; cf. lotus #13467).
type revertError struct{ data []byte }

func (e *revertError) Error() string {
	return "execution reverted: 0x" + hex.EncodeToString(e.data)
}

// RevertData exposes the raw revert payload so the RPC server can attach
// it to the error response.
func (e *revertError) RevertData() []byte { return e.data }

func decodeEthAddr(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) != 40 {
		return nil, fmt.Errorf("eth address must be 20 bytes")
	}
	return hex.DecodeString(s)
}

func decodeHexData(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) == 0 {
		return nil, nil
	}
	if len(s)%2 != 0 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func chainIDForNetwork(name string) uint64 {
	switch {
	case strings.Contains(name, "cali"):
		return 314159
	case name == "" || strings.Contains(name, "main"):
		return 314
	default:
		return 314
	}
}

// ensure hamt import is used (BlockGetter is hamt.BlockGetter on ChainAPI).
var _ hamt.BlockGetter
