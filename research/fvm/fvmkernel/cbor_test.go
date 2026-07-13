package fvmkernel

// CBOR-parse round-trip tests (lantern#130 Tier 1). We hand-encode the
// shapes ref-fvm emits for compute_unsealed_sector_cid + verify_consensus_fault
// input and verify the decoder returns the exact struct we started from.

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ipfs/go-cid"
)

// cborWriter is a tiny encoder mirroring cborReader; testing-only.
type cborWriter struct{ b []byte }

func (w *cborWriter) writeHeader(major byte, arg uint64) {
	switch {
	case arg < 24:
		w.b = append(w.b, (major<<5)|byte(arg))
	case arg <= 0xFF:
		w.b = append(w.b, (major<<5)|24, byte(arg))
	case arg <= 0xFFFF:
		var buf [3]byte
		buf[0] = (major << 5) | 25
		binary.BigEndian.PutUint16(buf[1:], uint16(arg))
		w.b = append(w.b, buf[:]...)
	case arg <= 0xFFFFFFFF:
		var buf [5]byte
		buf[0] = (major << 5) | 26
		binary.BigEndian.PutUint32(buf[1:], uint32(arg))
		w.b = append(w.b, buf[:]...)
	default:
		var buf [9]byte
		buf[0] = (major << 5) | 27
		binary.BigEndian.PutUint64(buf[1:], arg)
		w.b = append(w.b, buf[:]...)
	}
}
func (w *cborWriter) writeUint(v uint64) { w.writeHeader(0, v) }
func (w *cborWriter) writeInt(v int64) {
	if v >= 0 {
		w.writeHeader(0, uint64(v))
	} else {
		w.writeHeader(1, uint64(-1-v))
	}
}
func (w *cborWriter) writeBytes(b []byte)    { w.writeHeader(2, uint64(len(b))); w.b = append(w.b, b...) }
func (w *cborWriter) writeArrayHeader(n int) { w.writeHeader(4, uint64(n)) }
func (w *cborWriter) writeNull()             { w.b = append(w.b, 0xF6) }
func (w *cborWriter) writeTaggedCID(c cid.Cid) {
	w.writeHeader(6, 42) // tag 42
	// DagCBOR: byte-string with 0x00 multibase prefix + CID bytes
	cb := c.Bytes()
	buf := make([]byte, 1+len(cb))
	buf[0] = 0x00
	copy(buf[1:], cb)
	w.writeBytes(buf)
}

// TestDecodePieceInfoRoundTrip: hand-encode a piece array with two
// pieces and confirm the decoder returns matching structs.
func TestDecodePieceInfoRoundTrip(t *testing.T) {
	d1 := repeatByte(0x11, 32)
	d1[31] &= 0x3F
	d2 := repeatByte(0x22, 32)
	d2[31] &= 0x3F
	c1 := PieceCIDFromDigest(d1)
	c2 := PieceCIDFromDigest(d2)

	w := &cborWriter{}
	w.writeArrayHeader(2)
	// Piece 1: [128, cid1]
	w.writeArrayHeader(2)
	w.writeUint(128)
	w.writeTaggedCID(c1)
	// Piece 2: [256, cid2]
	w.writeArrayHeader(2)
	w.writeUint(256)
	w.writeTaggedCID(c2)

	pieces, err := DecodePieceInfoArray(w.b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pieces) != 2 {
		t.Fatalf("got %d pieces, want 2", len(pieces))
	}
	if pieces[0].Size != 128 || !pieces[0].CID.Equals(c1) {
		t.Errorf("piece[0] = {%d, %s}, want {128, %s}", pieces[0].Size, pieces[0].CID, c1)
	}
	if pieces[1].Size != 256 || !pieces[1].CID.Equals(c2) {
		t.Errorf("piece[1] = {%d, %s}, want {256, %s}", pieces[1].Size, pieces[1].CID, c2)
	}
}

// TestDecodePieceInfoDrivesCommDViaSyscallPath: emulate the syscall's
// full pipeline (bytes -> DecodePieceInfoArray -> ComputeUnsealedSectorCID)
// and assert the result matches a direct call, catching any pipeline
// wiring bug (e.g. size mis-order between size and CID).
func TestDecodePieceInfoDrivesCommDViaSyscallPath(t *testing.T) {
	d := repeatByte(0x77, 32)
	d[31] &= 0x3F
	c := PieceCIDFromDigest(d)

	direct, err := ComputeUnsealedSectorCID(256, []PieceInfo{{Size: 128, CID: c}})
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	w := &cborWriter{}
	w.writeArrayHeader(1)
	w.writeArrayHeader(2)
	w.writeUint(128)
	w.writeTaggedCID(c)
	decoded, err := DecodePieceInfoArray(w.b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	viaPipeline, err := ComputeUnsealedSectorCID(256, decoded)
	if err != nil {
		t.Fatalf("via pipeline: %v", err)
	}
	if !direct.Equals(viaPipeline) {
		t.Fatalf("CommD mismatch: direct=%s pipeline=%s", direct, viaPipeline)
	}
}

// TestDecodeBlockHeaderRoundTrip: encode the 16-field BlockHeader tuple
// with plausible values, decode, and verify miner + parents + height +
// bls-sig match.
func TestDecodeBlockHeaderRoundTrip(t *testing.T) {
	miner := IDAddress(1234)
	parents := []cid.Cid{fakeCID(10), fakeCID(11)}
	blsSig := []byte{0xB1, 0xB2, 0xB3, 0xB4}

	w := &cborWriter{}
	w.writeArrayHeader(16)
	// 0: Miner (byte string of the address bytes)
	w.writeBytes(miner.Bytes())
	// 1: Ticket -- null
	w.writeNull()
	// 2: ElectionProof -- null
	w.writeNull()
	// 3: BeaconEntries -- empty array
	w.writeArrayHeader(0)
	// 4: WinPoStProof -- empty array
	w.writeArrayHeader(0)
	// 5: Parents
	w.writeArrayHeader(len(parents))
	for _, p := range parents {
		w.writeTaggedCID(p)
	}
	// 6: ParentWeight (BigInt as byte-string)
	w.writeBytes([]byte{0x00, 0x01})
	// 7: Height
	w.writeInt(12345)
	// 8-10: three tagged CIDs
	w.writeTaggedCID(fakeCID(20))
	w.writeTaggedCID(fakeCID(21))
	w.writeTaggedCID(fakeCID(22))
	// 11: BLSAggregate: [sig-type, data]
	w.writeArrayHeader(2)
	w.writeUint(2) // sig-type = BLS
	w.writeBytes(blsSig)
	// 12: Timestamp
	w.writeUint(1_700_000_000)
	// 13: BlockSig: [sig-type, data]
	w.writeArrayHeader(2)
	w.writeUint(2)
	w.writeBytes([]byte{0xC1})
	// 14: ForkSignaling
	w.writeUint(0)
	// 15: ParentBaseFee
	w.writeBytes([]byte{0x03, 0xE8})

	hdr, err := DecodeBlockHeader(w.b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hdr.Miner.String() != miner.String() {
		t.Errorf("miner %s, want %s", hdr.Miner.String(), miner.String())
	}
	if hdr.Height != 12345 {
		t.Errorf("height %d, want 12345", hdr.Height)
	}
	if len(hdr.Parents) != 2 || !hdr.Parents[0].Equals(parents[0]) || !hdr.Parents[1].Equals(parents[1]) {
		t.Errorf("parents mismatch: got %v", hdr.Parents)
	}
	if !bytes.Equal(hdr.BLSSignature, blsSig) {
		t.Errorf("bls sig %x, want %x", hdr.BLSSignature, blsSig)
	}
}

// TestDecodeBlockHeaderRejectsShortTuple: guard against silently
// accepting a truncated header.
func TestDecodeBlockHeaderRejectsShortTuple(t *testing.T) {
	w := &cborWriter{}
	w.writeArrayHeader(3) // way too few fields
	w.writeBytes([]byte{0x00, 0x01})
	w.writeNull()
	w.writeNull()
	if _, err := DecodeBlockHeader(w.b); err == nil {
		t.Fatal("expected error on short tuple")
	}
}
