package fvmkernel

// Minimal DagCBOR decoder for the specific input shapes the FVM
// crypto syscalls need to parse (lantern#130 Tier 1).
//
// We do NOT want to pull refmt / go-ipld-cbor as a direct dep for a
// two-syscall use case; the shapes are simple enough to walk by hand.
// Only the constructs we actually encounter are handled: unsigned ints,
// negative ints (as int64), byte strings, arrays, and tag 42 (CID).

import (
	"encoding/binary"
	"fmt"

	"github.com/ipfs/go-cid"
)

type cborReader struct {
	buf []byte
	pos int
}

func newCborReader(b []byte) *cborReader { return &cborReader{buf: b} }

func (r *cborReader) remaining() int { return len(r.buf) - r.pos }

func (r *cborReader) readByte() (byte, error) {
	if r.pos >= len(r.buf) {
		return 0, fmt.Errorf("cbor: unexpected end of input")
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

// readHeader returns (majorType, argument). CBOR encodes each item as
// a 1-byte header with (major<<5) | additional-info, followed by 0/1/2/4/8
// bytes of "argument" depending on additional-info.
func (r *cborReader) readHeader() (byte, uint64, error) {
	first, err := r.readByte()
	if err != nil {
		return 0, 0, err
	}
	major := first >> 5
	addl := first & 0x1F
	var arg uint64
	switch {
	case addl < 24:
		arg = uint64(addl)
	case addl == 24:
		b, err := r.readByte()
		if err != nil {
			return 0, 0, err
		}
		arg = uint64(b)
	case addl == 25:
		if r.remaining() < 2 {
			return 0, 0, fmt.Errorf("cbor: short uint16")
		}
		arg = uint64(binary.BigEndian.Uint16(r.buf[r.pos:]))
		r.pos += 2
	case addl == 26:
		if r.remaining() < 4 {
			return 0, 0, fmt.Errorf("cbor: short uint32")
		}
		arg = uint64(binary.BigEndian.Uint32(r.buf[r.pos:]))
		r.pos += 4
	case addl == 27:
		if r.remaining() < 8 {
			return 0, 0, fmt.Errorf("cbor: short uint64")
		}
		arg = binary.BigEndian.Uint64(r.buf[r.pos:])
		r.pos += 8
	case addl == 31:
		return 0, 0, fmt.Errorf("cbor: indefinite-length not supported")
	default:
		return 0, 0, fmt.Errorf("cbor: reserved additional info %d", addl)
	}
	return major, arg, nil
}

// readUint decodes a CBOR unsigned integer (major type 0).
func (r *cborReader) readUint() (uint64, error) {
	m, v, err := r.readHeader()
	if err != nil {
		return 0, err
	}
	if m != 0 {
		return 0, fmt.Errorf("cbor: expected uint (major 0), got major %d", m)
	}
	return v, nil
}

// readInt decodes a CBOR signed integer (major type 0 or 1) as int64.
func (r *cborReader) readInt() (int64, error) {
	m, v, err := r.readHeader()
	if err != nil {
		return 0, err
	}
	switch m {
	case 0:
		return int64(v), nil
	case 1:
		return -1 - int64(v), nil
	default:
		return 0, fmt.Errorf("cbor: expected int (major 0/1), got major %d", m)
	}
}

// readBytes decodes a CBOR byte string (major type 2).
func (r *cborReader) readBytes() ([]byte, error) {
	m, n, err := r.readHeader()
	if err != nil {
		return nil, err
	}
	if m != 2 {
		return nil, fmt.Errorf("cbor: expected byte-string (major 2), got major %d", m)
	}
	if int(n) > r.remaining() {
		return nil, fmt.Errorf("cbor: byte-string len %d > remaining %d", n, r.remaining())
	}
	out := append([]byte(nil), r.buf[r.pos:r.pos+int(n)]...)
	r.pos += int(n)
	return out, nil
}

// readArrayHeader decodes a CBOR array (major type 4) header and
// returns its length.
func (r *cborReader) readArrayHeader() (int, error) {
	m, n, err := r.readHeader()
	if err != nil {
		return 0, err
	}
	if m != 4 {
		return 0, fmt.Errorf("cbor: expected array (major 4), got major %d", m)
	}
	return int(n), nil
}

// readTaggedCID decodes tag 42 + byte-string(0x00 + CID.Bytes()) which
// is DagCBOR's representation of a link.
func (r *cborReader) readTaggedCID() (cid.Cid, error) {
	m, tag, err := r.readHeader()
	if err != nil {
		return cid.Undef, err
	}
	if m != 6 || tag != 42 {
		return cid.Undef, fmt.Errorf("cbor: expected tag(42), got major %d tag %d", m, tag)
	}
	raw, err := r.readBytes()
	if err != nil {
		return cid.Undef, err
	}
	if len(raw) == 0 || raw[0] != 0x00 {
		return cid.Undef, fmt.Errorf("cbor: dag-cbor link missing multibase prefix")
	}
	_, c, err := cid.CidFromBytes(raw[1:])
	if err != nil {
		return cid.Undef, fmt.Errorf("cbor: parse cid: %w", err)
	}
	return c, nil
}

// DecodePieceInfoArray parses a CBOR-encoded [[size, cid], ...] tuple
// array into a []PieceInfo. Serde-tuple encoding used by ref-fvm.
func DecodePieceInfoArray(b []byte) ([]PieceInfo, error) {
	r := newCborReader(b)
	n, err := r.readArrayHeader()
	if err != nil {
		return nil, err
	}
	pieces := make([]PieceInfo, 0, n)
	for i := 0; i < n; i++ {
		fields, err := r.readArrayHeader()
		if err != nil {
			return nil, fmt.Errorf("piece %d header: %w", i, err)
		}
		if fields != 2 {
			return nil, fmt.Errorf("piece %d has %d fields, want 2", i, fields)
		}
		size, err := r.readUint()
		if err != nil {
			return nil, fmt.Errorf("piece %d size: %w", i, err)
		}
		c, err := r.readTaggedCID()
		if err != nil {
			return nil, fmt.Errorf("piece %d cid: %w", i, err)
		}
		pieces = append(pieces, PieceInfo{Size: size, CID: c})
	}
	return pieces, nil
}

// DecodeBlockHeader parses the minimal subset of a Filecoin BlockHeader
// tuple that we need for consensus-fault detection. Fields we don't need
// are skipped by walking their CBOR shape. The tuple has 16 fields in
// canonical order:
//
//	0  Miner (byte-string)
//	1  Ticket (nullable {vrf-proof: bytes})
//	2  ElectionProof (nullable {win-count: i64, vrf-proof: bytes})
//	3  BeaconEntries (array of {round: uint, data: bytes})
//	4  WinPoStProof (array of {registered-proof: uint, proof-bytes: bytes})
//	5  Parents (array of tagged CIDs)
//	6  ParentWeight (BigInt as bytes)
//	7  Height (i64)
//	8  ParentStateRoot (tagged CID)
//	9  ParentMessageReceipts (tagged CID)
//	10 Messages (tagged CID)
//	11 BLSAggregate ({sig-type: uint, data: bytes})
//	12 Timestamp (uint)
//	13 BlockSig ({sig-type: uint, data: bytes})
//	14 ForkSignaling (uint)
//	15 ParentBaseFee (BigInt as bytes)
func DecodeBlockHeader(b []byte) (*BlockHeader, error) {
	r := newCborReader(b)
	fields, err := r.readArrayHeader()
	if err != nil {
		return nil, err
	}
	if fields < 12 {
		return nil, fmt.Errorf("block header has only %d fields; corrupt or truncated", fields)
	}
	// 0: Miner
	minerRaw, err := r.readBytes()
	if err != nil {
		return nil, fmt.Errorf("miner: %w", err)
	}
	miner, err := ParseAddress(minerRaw)
	if err != nil {
		return nil, fmt.Errorf("miner address: %w", err)
	}
	// 1-4: skip ticket, election-proof, beacon-entries, winpost-proof.
	// Each is either null (0xF6) or a structured value; skipAny walks it.
	for i := 1; i <= 4; i++ {
		if err := skipAny(r); err != nil {
			return nil, fmt.Errorf("skip field %d: %w", i, err)
		}
	}
	// 5: Parents (array of tagged CIDs).
	parentCount, err := r.readArrayHeader()
	if err != nil {
		return nil, fmt.Errorf("parents header: %w", err)
	}
	parents := make([]cid.Cid, 0, parentCount)
	for i := 0; i < parentCount; i++ {
		c, err := r.readTaggedCID()
		if err != nil {
			return nil, fmt.Errorf("parent %d: %w", i, err)
		}
		parents = append(parents, c)
	}
	// 6: ParentWeight (BigInt-as-bytes)
	if _, err := r.readBytes(); err != nil {
		return nil, fmt.Errorf("parent weight: %w", err)
	}
	// 7: Height (int)
	height, err := r.readInt()
	if err != nil {
		return nil, fmt.Errorf("height: %w", err)
	}
	// 8, 9, 10: three tagged CIDs to skip.
	for i, name := range []string{"state-root", "receipts", "messages"} {
		if _, err := r.readTaggedCID(); err != nil {
			return nil, fmt.Errorf("field %d %s: %w", 8+i, name, err)
		}
	}
	// 11: BLSAggregate = tuple(sig-type u8, data bytes)
	if _, err := r.readArrayHeader(); err != nil {
		return nil, fmt.Errorf("bls agg header: %w", err)
	}
	if _, err := r.readUint(); err != nil {
		return nil, fmt.Errorf("bls agg sig-type: %w", err)
	}
	blsSig, err := r.readBytes()
	if err != nil {
		return nil, fmt.Errorf("bls agg data: %w", err)
	}
	// We don't need the remaining fields (12-15) for fault detection.

	return &BlockHeader{
		Miner:        miner,
		Parents:      parents,
		Height:       height,
		BLSSignature: blsSig,
	}, nil
}

// skipAny walks past one CBOR item of any type. Recursive for arrays.
// Used to skip fields we don't parse (ticket, election-proof, etc).
func skipAny(r *cborReader) error {
	m, arg, err := r.readHeader()
	if err != nil {
		return err
	}
	switch m {
	case 0, 1: // uint / negint
		return nil
	case 2, 3: // bytes / text
		if int(arg) > r.remaining() {
			return fmt.Errorf("skip bytes/text: %d > remaining %d", arg, r.remaining())
		}
		r.pos += int(arg)
		return nil
	case 4: // array
		for i := uint64(0); i < arg; i++ {
			if err := skipAny(r); err != nil {
				return err
			}
		}
		return nil
	case 5: // map
		for i := uint64(0); i < arg*2; i++ {
			if err := skipAny(r); err != nil {
				return err
			}
		}
		return nil
	case 6: // tag: skip the tagged value
		return skipAny(r)
	case 7:
		// simple values (null=22, undef=23, true=21, false=20, ...);
		// float16/32/64 also live here. Arg already consumed enough.
		return nil
	}
	return fmt.Errorf("skip: unexpected major %d", m)
}
