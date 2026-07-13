package fvmkernel

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ipfs/go-cid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Blockstore is the CID->bytes store the kernel reads actor state from
// and writes new blocks to. In Lantern this is the verified IPLD block
// cache (state/cache + HAMT walker); here it's an in-memory map for the
// prototype.
type Blockstore interface {
	Get(c cid.Cid) ([]byte, bool)
	Put(c cid.Cid, data []byte)
}

// MemBlockstore is a trivial in-memory Blockstore for the prototype.
type MemBlockstore struct{ m map[string][]byte }

func NewMemBlockstore() *MemBlockstore { return &MemBlockstore{m: map[string][]byte{}} }
func (b *MemBlockstore) Get(c cid.Cid) ([]byte, bool) {
	v, ok := b.m[c.KeyString()]
	return v, ok
}
func (b *MemBlockstore) Put(c cid.Cid, data []byte) {
	b.m[c.KeyString()] = append([]byte(nil), data...)
}

// blockEntry is one entry in the per-invocation block registry.
type blockEntry struct {
	codec uint64
	data  []byte
}

// Kernel holds the state for a single actor invocation. It implements
// the ref-fvm syscall surface against a Blockstore + an in-invocation
// block registry.
type Kernel struct {
	bs Blockstore

	// Per-invocation block registry (block ids are assigned incrementally
	// starting at 1; 0 == NO_DATA_BLOCK_ID). Mirrors ref-fvm's call state.
	blocks      map[uint32]blockEntry
	nextBlockID uint32

	// The receiver actor's state root (self.root / set_root).
	stateRoot cid.Cid

	// Invocation + network context served to the actor.
	msgCtx MessageContext
	netCtx NetworkContext

	// Gas. Not metered to ref-fvm fidelity in C1 (see #128); we hand the
	// actor a large budget and only report it via gas.available.
	gasAvailable int64

	// Debug log routing.
	debugEnabled bool
	logs         []string

	// vm.exit capture. When the actor calls vm.exit, we record the exit
	// code + return block and let the subsequent WASM `unreachable` trap
	// (exit is `-> !` in Rust) surface as an expected termination.
	exited   bool
	exitCode uint32
	exitData []byte

	// send re-entry hook (Stage C3). When non-nil, send() dispatches a
	// nested invocation through this callback. nil => send returns
	// ErrForbidden (read-only single-actor mode).
	//
	// The `toBytes` argument is the raw recipient-address bytes as the
	// actor passed them to `send.send` (protocol byte + payload). The
	// caller (typically a Machine) parses those into an Address, resolves
	// it to an id, and runs the nested frame.
	sendFn SendFunc

	// SigVerifier is used by crypto.verify_consensus_fault to validate
	// block signatures. nil defaults to RejectAllVerifier.
	SigVerifier SignatureVerifier

	// ProofVer is used by crypto.verify_post / verify_seal / etc. to
	// delegate Groth16 proof verification. nil defaults to
	// RejectAllProofVerifier (safe posture).
	ProofVer ProofVerifier

	// Observability: every syscall name -> count.
	syscalls map[string]int

	// Actor registry (for restrict_internal_api caller-type checks and
	// address resolution). actorCodes maps an actor id to its code CID;
	// builtinTypes maps a code-CID keystring to its builtin Type id
	// (1=System, 2=Init, ... 14=EVM). In Lantern this comes from the
	// state tree (init actor address map + system actor's code registry);
	// here it's seeded by the harness.
	actorCodes   map[uint64]cid.Cid
	builtinTypes map[string]int32
}

// SendFunc is the type of the frame's send-syscall hook (Stage C3).
// `toBytes` is the raw recipient-address bytes (protocol byte + payload)
// as the actor passed them to `send.send`; the hook is responsible for
// parsing, resolving, and running the nested frame.
type SendFunc func(toBytes []byte, method uint64, params []byte, value TokenAmount) (exitCode uint32, retCodec uint64, ret []byte, err error)

// Builtin actor Type ids (fil_actors_runtime::runtime::builtin::Type).
const (
	TypeSystem           int32 = 1
	TypeInit             int32 = 2
	TypeCron             int32 = 3
	TypeAccount          int32 = 4
	TypePower            int32 = 5
	TypeMiner            int32 = 6
	TypeMarket           int32 = 7
	TypePaymentChannel   int32 = 8
	TypeMultisig         int32 = 9
	TypeReward           int32 = 10
	TypeVerifiedRegistry int32 = 11
	TypeDataCap          int32 = 12
	TypePlaceholder      int32 = 13
	TypeEVM              int32 = 14
	TypeEAM              int32 = 15
	TypeEthAccount       int32 = 16
)

// SetActor registers an actor id -> (code CID, builtin type). Used to
// satisfy restrict_internal_api caller-type checks for internal methods.
func (k *Kernel) SetActor(id uint64, codeCID cid.Cid, builtinType int32) {
	if k.actorCodes == nil {
		k.actorCodes = map[uint64]cid.Cid{}
	}
	if k.builtinTypes == nil {
		k.builtinTypes = map[string]int32{}
	}
	k.actorCodes[id] = codeCID
	k.builtinTypes[codeCID.KeyString()] = builtinType
}

// SyntheticCode returns a deterministic code CID for a builtin type, so
// the harness can seed SetActor without a real bundle. get_actor_code_cid
// + get_builtin_actor_type agree on it.
func SyntheticCode(builtinType int32) cid.Cid {
	c, _ := cidOfBlock(codecRaw, []byte{byte(builtinType), 'c', 'o', 'd', 'e'})
	return c
}

// NewKernel builds a kernel bound to a blockstore.
func NewKernel(bs Blockstore) *Kernel {
	return &Kernel{
		bs:           bs,
		blocks:       map[uint32]blockEntry{},
		nextBlockID:  1,
		gasAvailable: 1 << 60,
		syscalls:     map[string]int{},
	}
}

func (k *Kernel) count(name string) { k.syscalls[name]++ }

// putBlock registers a block in the per-invocation registry, returning its id.
func (k *Kernel) putBlock(codec uint64, data []byte) uint32 {
	id := k.nextBlockID
	k.nextBlockID++
	k.blocks[id] = blockEntry{codec: codec, data: append([]byte(nil), data...)}
	return id
}

// readMem / writeMem are memory helpers with FVM-style error returns.
func readMem(m api.Module, off, n uint32) ([]byte, bool) {
	return m.Memory().Read(off, n)
}
func writeMem(m api.Module, off uint32, data []byte) bool {
	return m.Memory().Write(off, data)
}

// Register wires every syscall module onto the runtime as host modules,
// bound to this kernel. Must be called before instantiating the actor.
func (k *Kernel) Register(ctx context.Context, rt wazero.Runtime) error {
	// ---- vm ----
	if _, err := rt.NewHostModuleBuilder("vm").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.vmMessageContext),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("message_context").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.vmExit),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("exit").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- ipld ----
	if _, err := rt.NewHostModuleBuilder("ipld").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.ipldBlockOpen),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("block_open").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.ipldBlockCreate),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("block_create").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.ipldBlockRead),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("block_read").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.ipldBlockStat),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("block_stat").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.ipldBlockLink),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("block_link").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- self ----
	if _, err := rt.NewHostModuleBuilder("self").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.selfRoot),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("root").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.selfSetRoot),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("set_root").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.selfCurrentBalance),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("current_balance").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.selfSelfDestruct),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("self_destruct").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- network ----
	if _, err := rt.NewHostModuleBuilder("network").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.networkContext),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("context").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.stub3("network.tipset_cid")),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("tipset_cid").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.networkTotalFilCircSupply),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("total_fil_circ_supply").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- debug ----
	if _, err := rt.NewHostModuleBuilder("debug").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.debugEnabledFn),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("enabled").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.debugLog),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("log").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- gas ----
	if _, err := rt.NewHostModuleBuilder("gas").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.gasAvailableFn),
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("available").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- rand ----
	if _, err := rt.NewHostModuleBuilder("rand").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.randGetChain),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64}, []api.ValueType{api.ValueTypeI32}).Export("get_chain_randomness").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.randGetBeacon),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64}, []api.ValueType{api.ValueTypeI32}).Export("get_beacon_randomness").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- event ----
	if _, err := rt.NewHostModuleBuilder("event").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.eventEmit),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).Export("emit_event").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- actor ----  (address resolution + code lookups; stubbed in C1)
	if err := k.registerActorModule(ctx, rt); err != nil {
		return err
	}

	// ---- send ----
	if _, err := rt.NewHostModuleBuilder("send").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(k.sendSend),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}, []api.ValueType{api.ValueTypeI32}).Export("send").
		Instantiate(ctx); err != nil {
		return err
	}

	// ---- crypto ----  (unimplemented in C1; overlaps #88 Stage B)
	if err := k.registerCryptoModule(ctx, rt); err != nil {
		return err
	}
	return nil
}

// ---- vm ----

// vmMessageContext: (ret_ptr) -> errno. Writes the 80-byte MessageContext.
func (k *Kernel) vmMessageContext(_ context.Context, m api.Module, stack []uint64) {
	k.count("vm.message_context")
	retPtr := api.DecodeU32(stack[0])
	if !writeMem(m, retPtr, messageContextBytes(k.msgCtx)) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// vmExit: (code, blk_id, msg_off, msg_len) -> ! . Records exit state and
// returns; the actor's Rust marks the following as unreachable, which the
// executor treats as an expected termination when k.exited is set.
func (k *Kernel) vmExit(_ context.Context, m api.Module, stack []uint64) {
	k.count("vm.exit")
	code := api.DecodeU32(stack[0])
	blkID := api.DecodeU32(stack[1])
	k.exited = true
	k.exitCode = code
	if blkID != noDataBlockID {
		if be, ok := k.blocks[blkID]; ok {
			k.exitData = be.data
		}
	}
	stack[0] = uint64(errOK)
}

// ---- ipld ----

// ipldBlockOpen: (ret_ptr, cid_ptr) -> errno. Loads the block at the CID
// from the blockstore, registers it, writes IpldOpen{codec,id,size}.
func (k *Kernel) ipldBlockOpen(_ context.Context, m api.Module, stack []uint64) {
	k.count("ipld.block_open")
	retPtr := api.DecodeU32(stack[0])
	cidPtr := api.DecodeU32(stack[1])
	raw, ok := readMem(m, cidPtr, 100) // CIDs are self-describing; read a bounded window
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	n, c, err := cid.CidFromBytes(raw)
	if err != nil {
		stack[0] = uint64(errIllegalCid)
		return
	}
	_ = n
	data, ok := k.bs.Get(c)
	if !ok {
		stack[0] = uint64(errNotFound)
		return
	}
	id := k.putBlock(c.Prefix().Codec, data)
	if !writeMem(m, retPtr, ipldOpenBytes(c.Prefix().Codec, id, uint32(len(data)))) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ipldBlockCreate: (ret_ptr, codec, data_ptr, len) -> errno. Registers a
// new block, writes its id to ret_ptr.
func (k *Kernel) ipldBlockCreate(_ context.Context, m api.Module, stack []uint64) {
	k.count("ipld.block_create")
	retPtr := api.DecodeU32(stack[0])
	codec := stack[1]
	dataPtr := api.DecodeU32(stack[2])
	length := api.DecodeU32(stack[3])
	data, ok := readMem(m, dataPtr, length)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	id := k.putBlock(codec, data)
	var idb [4]byte
	binary.LittleEndian.PutUint32(idb[:], id)
	if !writeMem(m, retPtr, idb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ipldBlockRead: (ret_ptr, id, offset, obuf, max_len) -> errno.
// Copies up to max_len bytes of block[id] starting at offset into obuf,
// writes the "remaining" count (i32) to ret_ptr.
func (k *Kernel) ipldBlockRead(_ context.Context, m api.Module, stack []uint64) {
	k.count("ipld.block_read")
	retPtr := api.DecodeU32(stack[0])
	id := api.DecodeU32(stack[1])
	offset := api.DecodeU32(stack[2])
	obuf := api.DecodeU32(stack[3])
	maxLen := api.DecodeU32(stack[4])
	be, ok := k.blocks[id]
	if !ok {
		stack[0] = uint64(errInvalidHandle)
		return
	}
	if int(offset) > len(be.data) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	avail := uint32(len(be.data)) - offset
	toCopy := avail
	if toCopy > maxLen {
		toCopy = maxLen
	}
	if toCopy > 0 {
		if !writeMem(m, obuf, be.data[offset:offset+toCopy]) {
			stack[0] = uint64(errIllegalArgument)
			return
		}
	}
	remaining := int32(avail) - int32(toCopy)
	var rb [4]byte
	binary.LittleEndian.PutUint32(rb[:], uint32(remaining))
	if !writeMem(m, retPtr, rb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ipldBlockStat: (ret_ptr, id) -> errno. Writes IpldStat{codec,size}.
func (k *Kernel) ipldBlockStat(_ context.Context, m api.Module, stack []uint64) {
	k.count("ipld.block_stat")
	retPtr := api.DecodeU32(stack[0])
	id := api.DecodeU32(stack[1])
	be, ok := k.blocks[id]
	if !ok {
		stack[0] = uint64(errInvalidHandle)
		return
	}
	if !writeMem(m, retPtr, ipldStatBytes(be.codec, uint32(len(be.data)))) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ipldBlockLink: (ret_ptr, id, hash_fun, hash_len, cid_ptr, cid_max_len) -> errno.
// Computes the CID of block[id], persists to the blockstore, writes the CID.
func (k *Kernel) ipldBlockLink(_ context.Context, m api.Module, stack []uint64) {
	k.count("ipld.block_link")
	retPtr := api.DecodeU32(stack[0])
	id := api.DecodeU32(stack[1])
	// hash_fun (stack[2]) + hash_len (stack[3]) ignored: we always use
	// blake2b-256 like Filecoin state. cid_ptr = stack[4], max = stack[5].
	cidPtr := api.DecodeU32(stack[4])
	cidMax := api.DecodeU32(stack[5])
	be, ok := k.blocks[id]
	if !ok {
		stack[0] = uint64(errInvalidHandle)
		return
	}
	c, err := cidOfBlock(be.codec, be.data)
	if err != nil {
		stack[0] = uint64(errSerialization)
		return
	}
	k.bs.Put(c, be.data)
	cb := c.Bytes()
	if uint32(len(cb)) > cidMax {
		stack[0] = uint64(errBufferTooSmall)
		return
	}
	if !writeMem(m, cidPtr, cb) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	var lb [4]byte
	binary.LittleEndian.PutUint32(lb[:], uint32(len(cb)))
	if !writeMem(m, retPtr, lb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ---- self ----

// selfRoot: (ret_ptr, cid_ptr, cid_max_len) -> errno. Writes the state
// root CID + its length.
func (k *Kernel) selfRoot(_ context.Context, m api.Module, stack []uint64) {
	k.count("self.root")
	retPtr := api.DecodeU32(stack[0])
	cidPtr := api.DecodeU32(stack[1])
	cidMax := api.DecodeU32(stack[2])
	if !k.stateRoot.Defined() {
		stack[0] = uint64(errIllegalOperation)
		return
	}
	cb := k.stateRoot.Bytes()
	if uint32(len(cb)) > cidMax {
		stack[0] = uint64(errBufferTooSmall)
		return
	}
	if !writeMem(m, cidPtr, cb) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	var lb [4]byte
	binary.LittleEndian.PutUint32(lb[:], uint32(len(cb)))
	if !writeMem(m, retPtr, lb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// selfSetRoot: (cid_ptr) -> errno. Parses the CID at cid_ptr, sets it as
// the receiver's new state root. (Empty return; no ret_ptr.)
func (k *Kernel) selfSetRoot(_ context.Context, m api.Module, stack []uint64) {
	k.count("self.set_root")
	cidPtr := api.DecodeU32(stack[0])
	raw, ok := readMem(m, cidPtr, 100)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	_, c, err := cid.CidFromBytes(raw)
	if err != nil {
		stack[0] = uint64(errIllegalCid)
		return
	}
	k.stateRoot = c
	stack[0] = uint64(errOK)
}

// selfCurrentBalance: (ret_ptr) -> errno. Writes a TokenAmount {lo,hi}.
// Prototype hands back zero balance.
func (k *Kernel) selfCurrentBalance(_ context.Context, m api.Module, stack []uint64) {
	k.count("self.current_balance")
	retPtr := api.DecodeU32(stack[0])
	var b [16]byte // {lo u64, hi u64} = 0
	if !writeMem(m, retPtr, b[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

func (k *Kernel) selfSelfDestruct(_ context.Context, m api.Module, stack []uint64) {
	k.count("self.self_destruct")
	stack[0] = uint64(errOK)
}

// ---- network ----

func (k *Kernel) networkContext(_ context.Context, m api.Module, stack []uint64) {
	k.count("network.context")
	retPtr := api.DecodeU32(stack[0])
	if !writeMem(m, retPtr, networkContextBytes(k.netCtx)) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

func (k *Kernel) networkTotalFilCircSupply(_ context.Context, m api.Module, stack []uint64) {
	k.count("network.total_fil_circ_supply")
	retPtr := api.DecodeU32(stack[0])
	var b [16]byte // TokenAmount zero
	if !writeMem(m, retPtr, b[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ---- debug ----

func (k *Kernel) debugEnabledFn(_ context.Context, m api.Module, stack []uint64) {
	k.count("debug.enabled")
	if k.debugEnabled {
		stack[0] = 1
	} else {
		stack[0] = 0
	}
}

func (k *Kernel) debugLog(_ context.Context, m api.Module, stack []uint64) {
	k.count("debug.log")
	ptr := api.DecodeU32(stack[0])
	sz := api.DecodeU32(stack[1])
	if b, ok := readMem(m, ptr, sz); ok {
		k.logs = append(k.logs, string(b))
	}
	stack[0] = uint64(errOK)
}

// ---- gas ----

func (k *Kernel) gasAvailableFn(_ context.Context, m api.Module, stack []uint64) {
	k.count("gas.available")
	// Rust sig: available() -> Result<u64>; WASM (ret_ptr) -> errno.
	retPtr := api.DecodeU32(stack[0])
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(k.gasAvailable))
	if !writeMem(m, retPtr, b[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// ---- rand ----  (Lantern has real DRAND + ticket randomness; prototype
// returns zeroed 32-byte randomness so read paths don't fault.)

func (k *Kernel) randGetChain(_ context.Context, m api.Module, stack []uint64) {
	k.count("rand.get_chain_randomness")
	stack[0] = uint64(errOK)
}
func (k *Kernel) randGetBeacon(_ context.Context, m api.Module, stack []uint64) {
	k.count("rand.get_beacon_randomness")
	stack[0] = uint64(errOK)
}

// ---- event ----

func (k *Kernel) eventEmit(_ context.Context, m api.Module, stack []uint64) {
	k.count("event.emit_event")
	stack[0] = uint64(errOK)
}

// ---- send (Stage C3) ----

// sendSend: (ret_ptr, recipient_off, recipient_len, method, params_id,
// value_hi, value_lo, gas_limit, flags) -> errno. Dispatches a nested
// invocation via k.sendFn if wired; otherwise ErrForbidden.
func (k *Kernel) sendSend(_ context.Context, m api.Module, stack []uint64) {
	k.count("send.send")
	retPtr := api.DecodeU32(stack[0])
	recipOff := api.DecodeU32(stack[1])
	recipLen := api.DecodeU32(stack[2])
	methodN := stack[3]
	paramsID := api.DecodeU32(stack[4])
	valueHi := stack[5]
	valueLo := stack[6]
	_ = stack[7] // gas_limit (not enforced at this layer yet)
	_ = stack[8] // flags (read-only propagation lives on the Machine)
	if k.sendFn == nil {
		stack[0] = uint64(errForbidden)
		return
	}
	recipRaw, ok := readMem(m, recipOff, recipLen)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	// Dispatch the nested send through the hook. The hook parses the
	// recipient address, resolves it to an id, snapshots state, and
	// runs the target actor's frame.
	var params []byte
	if paramsID != noDataBlockID {
		if be, ok := k.blocks[paramsID]; ok {
			params = be.data
		}
	}
	exitCode, retCodec, ret, err := k.sendFn(recipRaw, methodN, params, TokenAmount{Hi: valueHi, Lo: valueLo})
	if err != nil {
		stack[0] = uint64(errIllegalOperation)
		return
	}
	var retID uint32
	if len(ret) > 0 {
		retID = k.putBlock(retCodec, ret)
	}
	// Send out-struct: {exit_code u32, return_id u32, return_codec u64, return_size u32} = 20 bytes
	out := make([]byte, 20)
	binary.LittleEndian.PutUint32(out[0:], exitCode)
	binary.LittleEndian.PutUint32(out[4:], retID)
	binary.LittleEndian.PutUint64(out[8:], retCodec)
	binary.LittleEndian.PutUint32(out[16:], uint32(len(ret)))
	if !writeMem(m, retPtr, out) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// stub3 returns a generic OK-returning handler for a named syscall we
// haven't specialized yet (records the call for observability).
func (k *Kernel) stub3(name string) api.GoModuleFunc {
	return func(_ context.Context, m api.Module, stack []uint64) {
		k.count(name)
		stack[0] = uint64(errOK)
	}
}

// Syscalls returns the per-invocation syscall counts (observability).
func (k *Kernel) Syscalls() map[string]int { return k.syscalls }

// Logs returns any debug.log lines the actor emitted.
func (k *Kernel) Logs() []string { return k.logs }

// StateRoot returns the current receiver state root (post-invocation, so
// callers can observe a set_root effect).
func (k *Kernel) StateRoot() cid.Cid { return k.stateRoot }

func (k *Kernel) errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
