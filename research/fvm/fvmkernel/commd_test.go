package fvmkernel

// Tests for the pure-Go CommD (unsealed sector CID) computation.
// lantern#130 Tier 1.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Known Filecoin zero-piece commitments up to level 5 (piece sizes
// 32, 64, 128, 256, 512, 1024 bytes). Values are the SHA254 zero-tree
// roots (SHA256 with top-two bits of the last byte masked). These are
// published in filecoin-project/specs as the canonical zero commitments
// and are what any correct CommD implementation MUST produce.
var knownZeroHashes = [][]byte{
	mustHex("0000000000000000000000000000000000000000000000000000000000000000"),
	mustHex("f5a5fd42d16a20302798ef6ed309979b43003d2320d9f0e8ea9831a92759fb0b"),
	mustHex("3731bb99ac689f66eef5973e4a94da188f4ddcae580724fc6f3fd60dfd488333"),
	mustHex("642a607ef886b004bf2c1978463ae1d4693ac0f410eb2d1b7a47fe205e5e750f"),
	mustHex("57a2381a28652bf47f6bef7aca679be4aede5871ab5cf3eb2c08114488cb8526"),
	mustHex("1f7ac9595510e09ea41c460b176430bb322cd6fb412ec57cb17d989a4310372f"),
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestSha254ZeroTreeMatchesFilecoinCanonicalValues: our SHA254 combine
// function, applied recursively to zero leaves, must produce the exact
// values published in the Filecoin spec.
func TestSha254ZeroTreeMatchesFilecoinCanonicalValues(t *testing.T) {
	for level, want := range knownZeroHashes {
		got := zeroPieceHash(level)
		if !bytes.Equal(got, want) {
			t.Errorf("zero hash level %d = %x, want %x", level, got, want)
		}
	}
	t.Logf("SHA254 zero-tree matches Filecoin canonical vectors through level %d", len(knownZeroHashes)-1)
}

// TestSha254CombinesArePlainSha256WithFr32Mask: sanity that our combine
// really is SHA256 with the top two bits of the last byte cleared.
func TestSha254CombinesArePlainSha256WithFr32Mask(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(i * 3)
	}
	got := combineTwoNodes(a, b)
	raw := sha256.Sum256(append(append([]byte{}, a...), b...))
	raw[31] &= 0x3F
	if !bytes.Equal(got, raw[:]) {
		t.Fatalf("combineTwoNodes != sha256+fr32:\n got=%x\nwant=%x", got, raw)
	}
}

// TestEmptySectorCommDIsZeroCommitmentAtSectorLevel: with no pieces, the
// sector CommD is the zero commitment at sectorLevel = log2(sectorSize/32).
func TestEmptySectorCommDIsZeroCommitmentAtSectorLevel(t *testing.T) {
	// 128-byte sector => level 2 => zero hash level 2.
	got, err := ComputeUnsealedSectorCID(128, nil)
	if err != nil {
		t.Fatalf("empty CommD: %v", err)
	}
	dgst, err := commPDigest(got)
	if err != nil {
		t.Fatalf("decode commd: %v", err)
	}
	if !bytes.Equal(dgst, knownZeroHashes[2]) {
		t.Fatalf("empty 128-byte sector CommD digest %x, want zero-L2 %x", dgst, knownZeroHashes[2])
	}
	if got.Prefix().Codec != commCodecUnsealed {
		t.Fatalf("CommD codec %#x, want %#x", got.Prefix().Codec, commCodecUnsealed)
	}
}

// TestSinglePieceFillingSector: one piece of size == sectorSize contributes
// its own digest as the entire tree, so CommD digest == piece digest.
func TestSinglePieceFillingSector(t *testing.T) {
	// Fake piece digest (any 32 bytes with top-two bits of last byte 0).
	pieceDigest := make([]byte, 32)
	for i := range pieceDigest {
		pieceDigest[i] = byte(0x10 + i)
	}
	pieceDigest[31] &= 0x3F
	pieceCID := PieceCIDFromDigest(pieceDigest)

	got, err := ComputeUnsealedSectorCID(128, []PieceInfo{{Size: 128, CID: pieceCID}})
	if err != nil {
		t.Fatalf("single piece: %v", err)
	}
	gotDigest, _ := commPDigest(got)
	if !bytes.Equal(gotDigest, pieceDigest) {
		t.Fatalf("CommD %x != piece %x", gotDigest, pieceDigest)
	}
}

// TestTwoEqualPiecesFillSector: two size-128 pieces in a 256-byte sector
// combine at level 3 (piece is level 2, so paired -> level 3).
func TestTwoEqualPiecesFillSector(t *testing.T) {
	d1 := repeatByte(0x11, 32)
	d1[31] &= 0x3F
	d2 := repeatByte(0x22, 32)
	d2[31] &= 0x3F

	got, err := ComputeUnsealedSectorCID(256, []PieceInfo{
		{Size: 128, CID: PieceCIDFromDigest(d1)},
		{Size: 128, CID: PieceCIDFromDigest(d2)},
	})
	if err != nil {
		t.Fatalf("two pieces: %v", err)
	}
	want := combineTwoNodes(d1, d2)
	gotDigest, _ := commPDigest(got)
	if !bytes.Equal(gotDigest, want) {
		t.Fatalf("CommD %x != combine(d1,d2) %x", gotDigest, want)
	}
}

// TestOnePieceWithZeroPadding: one 128-byte piece in a 256-byte sector.
// Zero-padded to level 3: combine(piece, zero_L2).
func TestOnePieceWithZeroPadding(t *testing.T) {
	d := repeatByte(0x99, 32)
	d[31] &= 0x3F

	got, err := ComputeUnsealedSectorCID(256, []PieceInfo{
		{Size: 128, CID: PieceCIDFromDigest(d)},
	})
	if err != nil {
		t.Fatalf("padded piece: %v", err)
	}
	// Piece is at level 2 (128/32=4=2^2); sector is level 3 (256/32=8=2^3).
	// Pad with zero_L2 to make level 3.
	want := combineTwoNodes(d, knownZeroHashes[2])
	gotDigest, _ := commPDigest(got)
	if !bytes.Equal(gotDigest, want) {
		t.Fatalf("CommD %x != combine(d, zero_L2) %x", gotDigest, want)
	}
}

// TestPiecesOverflow: pieces summing above sector size must error out.
func TestPiecesOverflow(t *testing.T) {
	d := repeatByte(0x01, 32)
	_, err := ComputeUnsealedSectorCID(128, []PieceInfo{
		{Size: 128, CID: PieceCIDFromDigest(d)},
		{Size: 128, CID: PieceCIDFromDigest(d)},
	})
	if err == nil {
		t.Fatal("expected error for pieces exceeding sector size")
	}
}

// TestRejectNonPowerOfTwo: sector and piece sizes must be power of two.
func TestRejectNonPowerOfTwo(t *testing.T) {
	d := repeatByte(0x02, 32)
	if _, err := ComputeUnsealedSectorCID(200, nil); err == nil {
		t.Error("expected error for non-power-of-two sector")
	}
	if _, err := ComputeUnsealedSectorCID(128, []PieceInfo{{Size: 150, CID: PieceCIDFromDigest(d)}}); err == nil {
		t.Error("expected error for non-power-of-two piece")
	}
}

func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
