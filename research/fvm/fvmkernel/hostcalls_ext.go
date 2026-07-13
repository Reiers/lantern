package fvmkernel

import (
	"context"
	"crypto/sha256"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/crypto/blake2b"
)

// hostFn describes one host function for table-driven registration.
type hostFn struct {
	name    string
	params  []api.ValueType
	results []api.ValueType
	handler api.GoModuleFunc
}

func registerModule(ctx context.Context, rt wazero.Runtime, mod string, fns []hostFn) error {
	b := rt.NewHostModuleBuilder(mod)
	for _, f := range fns {
		b = b.NewFunctionBuilder().WithGoModuleFunction(f.handler, f.params, f.results).Export(f.name)
	}
	_, err := b.Instantiate(ctx)
	return err
}

const (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

// registerActorModule wires the `actor` address-resolution surface.
// C1 implements resolve_address as identity for ID-addresses and stubs
// the rest with ABI-correct signatures. Real address resolution needs the
// init-actor address map walk (Lantern has this via state/actors, wired
// in a later stage).
func (k *Kernel) registerActorModule(ctx context.Context, rt wazero.Runtime) error {
	okRet := func(name string) api.GoModuleFunc {
		return func(_ context.Context, m api.Module, stack []uint64) {
			k.count("actor." + name)
			// Most return a value at ret_ptr; leave zero + OK. For calls
			// the C1 target paths don't exercise, this is a safe no-op.
			stack[0] = uint64(errOK)
		}
	}
	return registerModule(ctx, rt, "actor", []hostFn{
		{"resolve_address", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, k.actorResolveAddress},
		{"lookup_delegated_address", []api.ValueType{i32, i64, i32, i32}, []api.ValueType{i32}, okRet("lookup_delegated_address")},
		{"get_actor_code_cid", []api.ValueType{i32, i64, i32, i32}, []api.ValueType{i32}, k.actorGetCodeCID},
		{"get_builtin_actor_type", []api.ValueType{i32, i32}, []api.ValueType{i32}, k.actorGetBuiltinType},
		{"get_code_cid_for_type", []api.ValueType{i32, i32, i32, i32}, []api.ValueType{i32}, okRet("get_code_cid_for_type")},
		{"next_actor_address", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("next_actor_address")},
		{"create_actor", []api.ValueType{i64, i32, i32, i32}, []api.ValueType{i32}, okRet("create_actor")},
		{"balance_of", []api.ValueType{i32, i64}, []api.ValueType{i32}, okRet("balance_of")},
	})
}

// registerCryptoModule wires the `crypto` surface. crypto.hash is real.
// Tier 1 of #130 makes compute_unsealed_sector_cid + verify_consensus_fault
// real (CommD merkle over pieces, header parsing + fault detection).
// verify_signature + recover_secp remain ABI-correct stubs pending the
// crypto core (#88), and the proof-verify family (verify_post et al.)
// stays stubbed per Tier 2 of #130.
func (k *Kernel) registerCryptoModule(ctx context.Context, rt wazero.Runtime) error {
	okRet := func(name string) api.GoModuleFunc {
		return func(_ context.Context, m api.Module, stack []uint64) {
			k.count("crypto." + name)
			stack[0] = uint64(errOK)
		}
	}
	return registerModule(ctx, rt, "crypto", []hostFn{
		// hash(ret_ptr, hash_code i64, data_off, data_len, digest_off, digest_len) -> errno
		{"hash", []api.ValueType{i32, i64, i32, i32, i32, i32}, []api.ValueType{i32}, k.cryptoHash},
		{"verify_signature", []api.ValueType{i32, i32, i32, i32, i32, i32, i32, i32}, []api.ValueType{i32}, okRet("verify_signature")},
		{"recover_secp_public_key", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("recover_secp_public_key")},
		{"verify_post", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("verify_post")},
		{"verify_replica_update", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("verify_replica_update")},
		{"verify_aggregate_seals", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("verify_aggregate_seals")},
		{"batch_verify_seals", []api.ValueType{i32, i32, i32}, []api.ValueType{i32}, okRet("batch_verify_seals")},
		{"compute_unsealed_sector_cid", []api.ValueType{i32, i64, i32, i32, i32, i32}, []api.ValueType{i32}, k.cryptoComputeUnsealedSectorCID},
		{"verify_consensus_fault", []api.ValueType{i32, i32, i32, i32, i32, i32, i32}, []api.ValueType{i32}, k.cryptoVerifyConsensusFault},
	})
}

// SectorSizeForProof returns the sector byte size for a Filecoin
// registered-seal-proof identifier (fvm_shared::sector::RegisteredSealProof).
// Enum values match ref-fvm's canonical layout:
//
//	0..4   V1 (2KiB, 8MiB, 512MiB, 32GiB, 64GiB)
//	5..9   V1_1
//	10..14 V1_1 synthetic-PoRep
//	15..19 V1_2 NI-PoRep
func SectorSizeForProof(proofType int64) (uint64, bool) {
	sizes := []uint64{2 << 10, 8 << 20, 512 << 20, 32 << 30, 64 << 30}
	if proofType < 0 || proofType > 19 {
		return 0, false
	}
	return sizes[proofType%5], true
}

// cryptoComputeUnsealedSectorCID implements the CommD-computation syscall.
// Signature: (ret_ptr, proof_type i64, pieces_off, pieces_len,
//
//	cid_off, cid_len) -> errno.
//
// Writes the CID bytes to cid_off (up to cid_len) and the CID length
// to ret_ptr.
func (k *Kernel) cryptoComputeUnsealedSectorCID(_ context.Context, m api.Module, stack []uint64) {
	k.count("crypto.compute_unsealed_sector_cid")
	retPtr := api.DecodeU32(stack[0])
	proofType := int64(stack[1])
	piecesOff := api.DecodeU32(stack[2])
	piecesLen := api.DecodeU32(stack[3])
	cidOff := api.DecodeU32(stack[4])
	cidLen := api.DecodeU32(stack[5])

	sectorSize, ok := SectorSizeForProof(proofType)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	piecesBytes, ok := readMem(m, piecesOff, piecesLen)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	pieces, err := DecodePieceInfoArray(piecesBytes)
	if err != nil {
		stack[0] = uint64(errSerialization)
		return
	}
	commD, err := ComputeUnsealedSectorCID(sectorSize, pieces)
	if err != nil {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	cb := commD.Bytes()
	if uint32(len(cb)) > cidLen {
		stack[0] = uint64(errBufferTooSmall)
		return
	}
	if !writeMem(m, cidOff, cb) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	var lb [4]byte
	le32(lb[:], uint32(len(cb)))
	if !writeMem(m, retPtr, lb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// cryptoVerifyConsensusFault implements the consensus-fault detection
// syscall. Signature: (ret_ptr, h1_off, h1_len, h2_off, h2_len,
//
//	extra_off, extra_len) -> errno.
//
// Writes a 24-byte VerifyConsensusFault out-struct to ret_ptr
// (epoch i64, target u64, fault_type u32, pad u32).
//
// The default signature verifier lives on the Kernel (k.SigVerifier);
// unset defaults to RejectAllVerifier, which suppresses fault reporting.
// Wiring a real verifier is #130 Tier 2 / #88.
func (k *Kernel) cryptoVerifyConsensusFault(_ context.Context, m api.Module, stack []uint64) {
	k.count("crypto.verify_consensus_fault")
	retPtr := api.DecodeU32(stack[0])
	h1Off := api.DecodeU32(stack[1])
	h1Len := api.DecodeU32(stack[2])
	h2Off := api.DecodeU32(stack[3])
	h2Len := api.DecodeU32(stack[4])
	extraOff := api.DecodeU32(stack[5])
	extraLen := api.DecodeU32(stack[6])

	h1Raw, ok := readMem(m, h1Off, h1Len)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	h2Raw, ok := readMem(m, h2Off, h2Len)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	h1, err := DecodeBlockHeader(h1Raw)
	if err != nil {
		stack[0] = uint64(errSerialization)
		return
	}
	h2, err := DecodeBlockHeader(h2Raw)
	if err != nil {
		stack[0] = uint64(errSerialization)
		return
	}
	var extra *BlockHeader
	if extraLen > 0 {
		extraRaw, ok := readMem(m, extraOff, extraLen)
		if !ok {
			stack[0] = uint64(errIllegalArgument)
			return
		}
		extra, err = DecodeBlockHeader(extraRaw)
		if err != nil {
			stack[0] = uint64(errSerialization)
			return
		}
	}
	verifier := k.SigVerifier
	if verifier == nil {
		verifier = RejectAllVerifier{}
	}
	res := VerifyConsensusFault(h1, h2, extra, verifier)
	if !writeMem(m, retPtr, res.Bytes()) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// actorResolveAddress: (ret_ptr, addr_off, addr_len) -> errno. Resolves
// an address to an actor id, writing the u64 id to ret_ptr. For an
// id-address (protocol 0) this is the embedded id. Other protocols look
// up the harness registry by exact match (prototype scope).
func (k *Kernel) actorResolveAddress(_ context.Context, m api.Module, stack []uint64) {
	k.count("actor.resolve_address")
	retPtr := api.DecodeU32(stack[0])
	addrOff := api.DecodeU32(stack[1])
	addrLen := api.DecodeU32(stack[2])
	raw, ok := readMem(m, addrOff, addrLen)
	if !ok || len(raw) == 0 {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	if raw[0] == 0x00 { // id-address: protocol 0, leb128 id follows
		id, _, err := uvarint(raw[1:])
		if err != nil {
			stack[0] = uint64(errIllegalArgument)
			return
		}
		var b [8]byte
		le64(b[:], id)
		if !writeMem(m, retPtr, b[:]) {
			stack[0] = uint64(errIllegalArgument)
			return
		}
		stack[0] = uint64(errOK)
		return
	}
	stack[0] = uint64(errNotFound)
}

// actorGetCodeCID: (ret_ptr, actor_id i64, obuf_off, obuf_len) -> errno.
// Writes the actor's code CID to obuf and the CID length to ret_ptr.
// Returns ErrNotFound when the id isn't registered.
func (k *Kernel) actorGetCodeCID(_ context.Context, m api.Module, stack []uint64) {
	k.count("actor.get_actor_code_cid")
	retPtr := api.DecodeU32(stack[0])
	actorID := stack[1]
	obuf := api.DecodeU32(stack[2])
	obufLen := api.DecodeU32(stack[3])
	c, ok := k.actorCodes[actorID]
	if !ok {
		stack[0] = uint64(errNotFound)
		return
	}
	cb := c.Bytes()
	if uint32(len(cb)) > obufLen {
		stack[0] = uint64(errBufferTooSmall)
		return
	}
	if !writeMem(m, obuf, cb) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	var lb [4]byte
	le32(lb[:], uint32(len(cb)))
	if !writeMem(m, retPtr, lb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// actorGetBuiltinType: (ret_ptr, cid_off) -> errno. Reads the code CID at
// cid_off, writes the i32 builtin type id to ret_ptr (0 = unknown).
func (k *Kernel) actorGetBuiltinType(_ context.Context, m api.Module, stack []uint64) {
	k.count("actor.get_builtin_actor_type")
	retPtr := api.DecodeU32(stack[0])
	cidOff := api.DecodeU32(stack[1])
	raw, ok := readMem(m, cidOff, 100)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	_, c, err := cidFromBytesPrefix(raw)
	if err != nil {
		stack[0] = uint64(errIllegalCid)
		return
	}
	t := k.builtinTypes[c.KeyString()] // 0 if unknown
	var b [4]byte
	le32(b[:], uint32(t))
	if !writeMem(m, retPtr, b[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}

// cryptoHash implements the FVM `crypto.hash` syscall for real.
// Signature (WASM): (ret_ptr, hash_code i64, data_off, data_len, digest_off, digest_len) -> errno.
// Writes the digest to digest_off (truncated to digest_len) and the full
// digest length to ret_ptr.
func (k *Kernel) cryptoHash(_ context.Context, m api.Module, stack []uint64) {
	k.count("crypto.hash")
	retPtr := api.DecodeU32(stack[0])
	hashCode := stack[1]
	dataOff := api.DecodeU32(stack[2])
	dataLen := api.DecodeU32(stack[3])
	digestOff := api.DecodeU32(stack[4])
	digestLen := api.DecodeU32(stack[5])

	data, ok := readMem(m, dataOff, dataLen)
	if !ok {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	var full []byte
	switch hashCode {
	case mhBlake2b256: // 0xb220
		h := blake2b.Sum256(data)
		full = h[:]
	case 0x12: // sha2-256
		h := sha256.Sum256(data)
		full = h[:]
	default:
		// Other multicodec hashes (keccak, blake2b-512, ...) not yet wired.
		stack[0] = uint64(errIllegalArgument)
		return
	}
	out := full
	if uint32(len(out)) > digestLen {
		out = out[:digestLen]
	}
	if len(out) > 0 {
		if !writeMem(m, digestOff, out) {
			stack[0] = uint64(errIllegalArgument)
			return
		}
	}
	var lb [4]byte
	le32(lb[:], uint32(len(full)))
	if !writeMem(m, retPtr, lb[:]) {
		stack[0] = uint64(errIllegalArgument)
		return
	}
	stack[0] = uint64(errOK)
}
