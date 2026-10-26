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
}

// GetCode resolves addr -> EVM actor -> bytecode (CID + hash verified).
func (b *evmBackend) GetCode(a evm.Address) ([]byte, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return nil, err // miss -> empty code (not an error to callers)
	}
	return actors.FetchBytecode(b.ctx, st, b.bg)
}

// GetStorage reads slot `key` from addr's contract storage KAMT.
func (b *evmBackend) GetStorage(a evm.Address, key uint256.Int) (uint256.Int, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return uint256.Int{}, err
	}
	v, _, err := kamt.GetU256(b.ctx, st.StorageRoot(), key.ToBig(), b.bg)
	if err != nil {
		return uint256.Int{}, err
	}
	var out uint256.Int
	out.SetFromBig(v)
	return out, nil
}

// GetBalance returns the actor balance (attoFIL) as a 256-bit word.
func (b *evmBackend) GetBalance(a evm.Address) (uint256.Int, error) {
	filAddr, err := ethAddrToFilecoin(a.Bytes())
	if err != nil {
		return uint256.Int{}, nil
	}
	actor, _, err := b.acc.GetActor(b.ctx, filAddr)
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
	actor, _, err := b.acc.GetActor(b.ctx, filAddr)
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
	actor, _, err := b.acc.GetActor(b.ctx, filAddr)
	if err != nil {
		if isActorNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("load actor %s: %w", filAddr, err)
	}
	st, err := actors.LoadEVM(b.ctx, actor.Code, actor.Head, b.bg, b.reg)
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
	acc := c.Accessor
	blockNo := uint64(0)
	tstamp := uint64(0)
	if c.Trusted != nil {
		blockNo = uint64(c.Trusted.Epoch)
	}
	var execTS *ltypes.TipSet
	if c.HeaderStore != nil {
		if head := c.HeaderStore.Head(); head != nil {
			liveRoot := head.ParentState()
			if liveRoot.Defined() {
				liveTR := &trustedroot.TrustedRoot{
					Epoch:     head.Height(),
					StateRoot: liveRoot,
				}
				acc = accessor.New(liveTR, c.BlockGetter)
				// head.ParentState() is the state after executing head's
				// parent: exactly what Lotus eth_call "latest" runs on
				// (TipSetState(head.Parents())). The FEVM environment
				// (NUMBER, TIMESTAMP, BASEFEE, PREVRANDAO, BLOCKHASH
				// window) is that parent tipset's, so resolve it rather
				// than reporting head's height.
				parent, err := c.HeaderStore.GetTipSet(head.Parents())
				if err != nil || parent == nil {
					return "", false, fmt.Sprintf("execution tipset (head %d parent) not in header store", head.Height()), nil
				}
				execTS = parent
				blockNo = uint64(parent.Height())
				tstamp = parent.MinTimestamp()
			}
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
	bg := newRetryingBlockGetter(c.BlockGetter, retries, total)

	be := &evmBackend{
		ctx:     ctx,
		c:       c,
		acc:     acc,
		reg:     acc.Registry(),
		chainID: chainIDForNetwork(c.NetworkName),
		blockNo: blockNo,
		tstamp:  tstamp,
		bg:      bg,
		execTS:  execTS,
	}

	// Override the accessor's view of the blockstore so its actor lookups
	// also benefit from the retry layer. We rebuild the accessor at the
	// live head above; rebuild once more so it uses bg, not c.BlockGetter.
	if acc != nil {
		if c.HeaderStore != nil {
			if head := c.HeaderStore.Head(); head != nil {
				liveRoot := head.ParentState()
				if liveRoot.Defined() {
					liveTR := &trustedroot.TrustedRoot{
						Epoch:     head.Height(),
						StateRoot: liveRoot,
					}
					be.acc = accessor.New(liveTR, bg)
				}
			}
		}
	}

	atomic.AddUint64(&c.localEthCallTotal, 1)

	res, err := evm.Call(be, from, evm.BytesToAddress(toRaw), input)
	if err != nil {
		// A local execution fault (e.g. an opcode we don't support yet)
		// is NOT a definitive answer; fall back to the bridge. Record the
		// reason so we can see what local coverage is still missing.
		log.Debugw("eth_call: local exec fault, will fall back", "to", call.To, "reason", err.Error())
		return "", false, "local exec fault: " + err.Error(), nil
	}
	if res.Reverted {
		// A revert IS a definitive answer. Surface it so EthCall maps it
		// to eth error code 3 with the revert payload.
		return "0x" + hex.EncodeToString(res.Return), true, "", &revertError{data: res.Return}
	}
	return "0x" + hex.EncodeToString(res.Return), true, "", nil
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
