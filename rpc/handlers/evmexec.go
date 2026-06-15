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
	"fmt"
	"strings"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/holiman/uint256"

	"github.com/Reiers/lantern/chain/trustedroot"
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
}

// GetCode resolves addr -> EVM actor -> bytecode (CID + hash verified).
func (b *evmBackend) GetCode(a evm.Address) ([]byte, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return nil, err // miss -> empty code (not an error to callers)
	}
	return actors.FetchBytecode(b.ctx, st, b.c.BlockGetter)
}

// GetStorage reads slot `key` from addr's contract storage KAMT.
func (b *evmBackend) GetStorage(a evm.Address, key uint256.Int) (uint256.Int, error) {
	st, ok, err := b.loadEVMActor(a)
	if err != nil || !ok {
		return uint256.Int{}, err
	}
	v, _, err := kamt.GetU256(b.ctx, st.StorageRoot(), key.ToBig(), b.c.BlockGetter)
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
		return uint256.Int{}, nil // unknown -> 0
	}
	var out uint256.Int
	out.SetFromBig(actor.Balance.Int)
	return out, nil
}

func (b *evmBackend) BlockNumber() uint64 { return b.blockNo }
func (b *evmBackend) Timestamp() uint64   { return b.tstamp }
func (b *evmBackend) ChainID() uint64     { return b.chainID }

// loadEVMActor resolves an eth address to its EVM actor state. ok=false
// (no error) when the address is unknown or not an EVM contract.
func (b *evmBackend) loadEVMActor(a evm.Address) (actors.EVMState, bool, error) {
	filAddr, err := ethAddrToFilecoin(a.Bytes())
	if err != nil {
		return nil, false, nil
	}
	actor, _, err := b.acc.GetActor(b.ctx, filAddr)
	if err != nil {
		return nil, false, nil
	}
	st, err := actors.LoadEVM(b.ctx, actor.Code, actor.Head, b.c.BlockGetter, b.reg)
	if err != nil {
		// Not an EVM actor (e.g. account/placeholder) -> not a contract.
		return nil, false, nil
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
}

// localEthCall executes an eth_call locally against verified state. It
// returns (result, true, nil) on a clean local execution (including a
// revert, which is surfaced as an error so the RPC layer can map it to
// code 3), or (_, false, nil) when the call can't be served locally and
// the caller should fall back to the VMBridge.
func (c *ChainAPI) localEthCall(ctx context.Context, call ethCallObject) (string, bool, error) {
	if c.Accessor == nil || c.BlockGetter == nil {
		return "", false, nil // can't serve locally
	}
	toRaw, err := decodeEthAddr(call.To)
	if err != nil {
		return "", false, nil // malformed -> let bridge try
	}
	dataHex := call.Data
	if dataHex == "" {
		dataHex = call.Input
	}
	input, err := decodeHexData(dataHex)
	if err != nil {
		return "", false, nil
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
	if c.Trusted != nil {
		blockNo = uint64(c.Trusted.Epoch)
	}
	if c.HeaderStore != nil {
		if head := c.HeaderStore.Head(); head != nil {
			liveRoot := head.ParentState()
			if liveRoot.Defined() {
				liveTR := &trustedroot.TrustedRoot{
					Epoch:     head.Height(),
					StateRoot: liveRoot,
				}
				acc = accessor.New(liveTR, c.BlockGetter)
				blockNo = uint64(head.Height())
			}
		}
	}

	be := &evmBackend{
		ctx:     ctx,
		c:       c,
		acc:     acc,
		reg:     actors.DefaultRegistry(),
		chainID: chainIDForNetwork(c.NetworkName),
		blockNo: blockNo,
	}

	res, err := evm.Call(be, from, evm.BytesToAddress(toRaw), input)
	if err != nil {
		// A local execution fault (e.g. an opcode we don't support yet)
		// is NOT a definitive answer; fall back to the bridge.
		return "", false, nil
	}
	if res.Reverted {
		// A revert IS a definitive answer. Surface it so EthCall maps it
		// to eth error code 3 with the revert payload.
		return "0x" + hex.EncodeToString(res.Return), true, &revertError{data: res.Return}
	}
	return "0x" + hex.EncodeToString(res.Return), true, nil
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
