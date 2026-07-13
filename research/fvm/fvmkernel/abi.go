// Package fvmkernel is a pure-Go Filecoin FVM execution kernel prototype
// (lantern#89). It runs Filecoin builtin-actor WASM under wazero and
// services the ref-fvm syscall ABI against a real IPLD blockstore.
//
// ABI target: fvm_sdk ~4.7 (matches builtin-actors v17). Struct layouts
// and syscall signatures are lifted from ref-fvm fvm@v4.7.5:
//   - shared/src/sys/out.rs  (MessageContext, IpldOpen, IpldStat, NetworkContext, Send)
//   - shared/src/sys/mod.rs  (TokenAmount)
//   - sdk/src/sys/{ipld,sself,vm,send,actor,network}.rs (signatures)
//
// This is Stage C1 of the #87 Full Node epic: the read-side actor
// executor. It is NOT a consensus-safe FVM. Gas is not metered to
// ref-fvm fidelity (Stage C2 / #128), send recursion is minimal
// (Stage C3), and proof-verify syscalls are unimplemented (Stage C4 /
// overlaps #88). Do not wire this into block validation until those
// land and vector-matching passes.
package fvmkernel

import (
	"encoding/binary"
	"fmt"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// le64 writes a little-endian u64.
func le64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

// uvarint decodes a LEB128 unsigned varint, returning value + bytes read.
func uvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, fmt.Errorf("bad uvarint")
	}
	return v, n, nil
}

// cidFromBytesPrefix parses a CID from the front of a buffer (CIDs are
// self-describing; trailing bytes are ignored).
func cidFromBytesPrefix(b []byte) (int, cid.Cid, error) {
	return cid.CidFromBytes(b)
}

// FVM error numbers (fvm_shared::error::ErrorNumber). 0 == success.
const (
	errOK               uint32 = 0
	errIllegalArgument  uint32 = 1
	errIllegalOperation uint32 = 2
	errLimitExceeded    uint32 = 3
	errAssertionFailed  uint32 = 4
	errInsufficientFund uint32 = 5
	errNotFound         uint32 = 6
	errInvalidHandle    uint32 = 7
	errIllegalCid       uint32 = 8
	errIllegalCodec     uint32 = 9
	errSerialization    uint32 = 10
	errForbidden        uint32 = 11
	errBufferTooSmall   uint32 = 12
)

// NO_DATA_BLOCK_ID: the reserved "no block" id (fvm_sdk).
const noDataBlockID uint32 = 0

// dag-cbor codec + blake2b-256 multihash code (Filecoin state blocks).
const (
	codecDagCBOR   uint64 = 0x71
	codecRaw       uint64 = 0x55
	mhBlake2b256   uint64 = 0xb220
	mhBlake2b256Sz        = 32
)

// cidOfBlock computes the CIDv1 (given codec + blake2b-256) of raw block bytes.
func cidOfBlock(codec uint64, data []byte) (cid.Cid, error) {
	h, err := mh.Sum(data, mhBlake2b256, mhBlake2b256Sz)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(codec, h), nil
}

// --- syscall out-struct encoders (all little-endian, repr(packed, C)) ---

// messageContextBytes serializes an 80-byte MessageContext exactly as
// ref-fvm's shared/src/sys/out.rs vm::MessageContext (repr packed C).
//
// layout:
//
//	origin u64 | nonce u64 | caller u64 | receiver u64 | method_number u64
//	value_received {lo u64, hi u64} | gas_premium {lo u64, hi u64} | flags u64
func messageContextBytes(mc MessageContext) []byte {
	b := make([]byte, 80)
	le := binary.LittleEndian
	le.PutUint64(b[0:], mc.Origin)
	le.PutUint64(b[8:], mc.Nonce)
	le.PutUint64(b[16:], mc.Caller)
	le.PutUint64(b[24:], mc.Receiver)
	le.PutUint64(b[32:], mc.MethodNumber)
	le.PutUint64(b[40:], mc.ValueReceivedLo)
	le.PutUint64(b[48:], mc.ValueReceivedHi)
	le.PutUint64(b[56:], mc.GasPremiumLo)
	le.PutUint64(b[64:], mc.GasPremiumHi)
	le.PutUint64(b[72:], mc.Flags)
	return b
}

// ipldOpenBytes: IpldOpen { codec u64, id u32, size u32 } = 16 bytes.
func ipldOpenBytes(codec uint64, id, size uint32) []byte {
	b := make([]byte, 16)
	le := binary.LittleEndian
	le.PutUint64(b[0:], codec)
	le.PutUint32(b[8:], id)
	le.PutUint32(b[12:], size)
	return b
}

// ipldStatBytes: IpldStat { codec u64, size u32 } = 12 bytes.
func ipldStatBytes(codec uint64, size uint32) []byte {
	b := make([]byte, 12)
	le := binary.LittleEndian
	le.PutUint64(b[0:], codec)
	le.PutUint32(b[8:], size)
	return b
}

// networkContextBytes: NetworkContext { epoch i64, timestamp u64,
// base_fee {lo,hi}, chain_id u64, network_version u32 } = 44 bytes.
func networkContextBytes(nc NetworkContext) []byte {
	b := make([]byte, 44)
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(nc.Epoch))
	le.PutUint64(b[8:], nc.Timestamp)
	le.PutUint64(b[16:], nc.BaseFeeLo)
	le.PutUint64(b[24:], nc.BaseFeeHi)
	le.PutUint64(b[32:], nc.ChainID)
	le.PutUint32(b[40:], nc.NetworkVersion)
	return b
}

// MessageContext is the Go mirror of the ref-fvm syscall struct.
type MessageContext struct {
	Origin          uint64
	Nonce           uint64
	Caller          uint64
	Receiver        uint64
	MethodNumber    uint64
	ValueReceivedLo uint64
	ValueReceivedHi uint64
	GasPremiumLo    uint64
	GasPremiumHi    uint64
	Flags           uint64
}

// NetworkContext is the Go mirror of the ref-fvm syscall struct.
type NetworkContext struct {
	Epoch          int64
	Timestamp      uint64
	BaseFeeLo      uint64
	BaseFeeHi      uint64
	ChainID        uint64
	NetworkVersion uint32
}
