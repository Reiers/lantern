package fvmkernel

// CommD (unsealed sector CID) computation in pure Go (lantern#130 Tier 1).
//
// Filecoin's CommD is the merkle root of the padded sector, where the
// merkle tree uses SHA254 (SHA256 with the top two bits of the last byte
// masked to 0, so the digest fits in a BLS12-381 field element -- "fr32
// encoding"). Each piece contributes its own commP as a subtree; unfilled
// gaps are padded with precomputed zero commitments at each level.
//
// This implementation matches the stack-based algorithm used across the
// Filecoin ecosystem (go-commp-utils, ref-fvm via filecoin-proofs-api).
// It has NOT been vector-matched against the network — that gate stays
// on Stage C4 acceptance before any consensus wiring.
//
// Externally-visible CID shape:
//   CIDv1, codec fil-commitment-unsealed (0xf101),
//   multihash sha2-256-trunc254-padded (0x1012), 32-byte digest.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// Multicodec constants for CommP / CommD encoding.
const (
	// fil-commitment-unsealed. https://github.com/multiformats/multicodec
	commCodecUnsealed uint64 = 0xf101
	// sha2-256-trunc254-padded. 32-byte digest, top two bits of last byte
	// masked so it fits in the BLS12-381 scalar field.
	mhSha256Trunc254     uint64 = 0x1012
	mhSha256Trunc254Size int    = 32
	commLeafSize         uint64 = 32 // 32-byte SHA254 nodes
	minPaddedPieceSize   uint64 = 128
)

// PieceInfo mirrors fvm_shared::piece::PieceInfo: {size, cid}. Size is
// the PADDED piece size (a power of two, >= 128 bytes).
type PieceInfo struct {
	Size uint64
	CID  cid.Cid
}

// ComputeUnsealedSectorCID builds the CommD over the sector of the given
// power-of-two size from the piece list. Pieces must have power-of-two
// padded sizes summing to at most the sector size; the remainder is
// filled with zero commitments at the appropriate levels.
func ComputeUnsealedSectorCID(sectorSize uint64, pieces []PieceInfo) (cid.Cid, error) {
	if !isPowerOfTwo(sectorSize) || sectorSize < minPaddedPieceSize {
		return cid.Undef, fmt.Errorf("sector size %d must be power of two >= %d", sectorSize, minPaddedPieceSize)
	}
	// Sanity: piece sizes power-of-two, sum <= sector.
	var total uint64
	for _, p := range pieces {
		if !isPowerOfTwo(p.Size) || p.Size < minPaddedPieceSize {
			return cid.Undef, fmt.Errorf("piece size %d must be power of two >= %d", p.Size, minPaddedPieceSize)
		}
		total += p.Size
	}
	if total > sectorSize {
		return cid.Undef, fmt.Errorf("pieces total %d exceed sector size %d", total, sectorSize)
	}

	// Canonical placement: smallest pieces first so the stack reduction
	// keeps top-two-same-level invariant clean.
	sorted := append([]PieceInfo(nil), pieces...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Size < sorted[j].Size })

	sectorLevel := log2Exact(sectorSize / commLeafSize)

	// Stack entries carry (level, digest). Push each piece's digest at
	// its natural level; if the incoming level is above the current top,
	// promote the top by pairing with a zero commitment first. Then
	// combine top-two while they share a level.
	type entry struct {
		level  int
		digest []byte
	}
	var stack []entry

	push := func(level int, digest []byte) {
		// Promote existing tops up to `level` with zero padding.
		for len(stack) > 0 && stack[len(stack)-1].level < level {
			top := stack[len(stack)-1]
			stack[len(stack)-1] = entry{
				level:  top.level + 1,
				digest: combineTwoNodes(top.digest, zeroPieceHash(top.level)),
			}
		}
		stack = append(stack, entry{level: level, digest: digest})
		// Combine equal-level tops.
		for len(stack) >= 2 && stack[len(stack)-1].level == stack[len(stack)-2].level {
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, entry{
				level:  a.level + 1,
				digest: combineTwoNodes(a.digest, b.digest),
			})
		}
	}

	for _, p := range sorted {
		digest, err := commPDigest(p.CID)
		if err != nil {
			return cid.Undef, fmt.Errorf("piece cid %s: %w", p.CID, err)
		}
		push(log2Exact(p.Size/commLeafSize), digest)
	}

	// If no pieces, everything is the zero commitment at sectorLevel.
	if len(stack) == 0 {
		return commCIDFromDigest(zeroPieceHash(sectorLevel)), nil
	}
	// Pad remainder up to sector level with zero commitments.
	for stack[len(stack)-1].level < sectorLevel {
		top := stack[len(stack)-1]
		stack[len(stack)-1] = entry{
			level:  top.level + 1,
			digest: combineTwoNodes(top.digest, zeroPieceHash(top.level)),
		}
	}
	if stack[0].level != sectorLevel || len(stack) != 1 {
		return cid.Undef, fmt.Errorf("reduction ended with %d entries at level %d (want 1 @ %d)",
			len(stack), stack[0].level, sectorLevel)
	}
	return commCIDFromDigest(stack[0].digest), nil
}

// combineTwoNodes returns SHA254(a || b): SHA256 with the top two bits
// of the last output byte masked to zero.
func combineTwoNodes(a, b []byte) []byte {
	h := sha256.New()
	h.Write(a)
	h.Write(b)
	d := h.Sum(nil)
	d[31] &= 0x3F
	return d
}

// zeroHashCache memoizes the zero commitment at each merkle level.
// Level 0 = 32 zero bytes (a zero leaf). Level i = SHA254 of two copies
// of level i-1. Grows lazily.
var zeroHashCache [][]byte

func init() {
	zeroHashCache = make([][]byte, 1)
	zeroHashCache[0] = make([]byte, commLeafSize)
}

func zeroPieceHash(level int) []byte {
	for len(zeroHashCache) <= level {
		prev := zeroHashCache[len(zeroHashCache)-1]
		zeroHashCache = append(zeroHashCache, combineTwoNodes(prev, prev))
	}
	return zeroHashCache[level]
}

// commPDigest extracts the 32-byte digest from a piece CID's multihash.
func commPDigest(pieceCID cid.Cid) ([]byte, error) {
	dm, err := mh.Decode(pieceCID.Hash())
	if err != nil {
		return nil, err
	}
	if dm.Code != mhSha256Trunc254 {
		return nil, fmt.Errorf("piece cid multihash %#x != sha2-256-trunc254-padded", dm.Code)
	}
	if len(dm.Digest) != mhSha256Trunc254Size {
		return nil, fmt.Errorf("piece cid digest %d bytes, want %d", len(dm.Digest), mhSha256Trunc254Size)
	}
	return dm.Digest, nil
}

// commCIDFromDigest builds a CIDv1 with fil-commitment-unsealed codec
// and a sha2-256-trunc254-padded multihash over `digest`.
func commCIDFromDigest(digest []byte) cid.Cid {
	// Multihash prefix: uvarint(code) + uvarint(len) + digest.
	prefix := make([]byte, 0, 4)
	prefix = appendUvarint(prefix, mhSha256Trunc254)
	prefix = appendUvarint(prefix, uint64(len(digest)))
	mhBytes := append(prefix, digest...)
	// go-cid's NewCidV1 wants a full multihash (bytes), not a Multihash
	// struct, so cast:
	h := mh.Multihash(mhBytes)
	return cid.NewCidV1(commCodecUnsealed, h)
}

// PieceCIDFromDigest is the CommP-side equivalent of commCIDFromDigest:
// same multihash, but the unsealed-commitment codec is shared with CommD
// (both use fil-commitment-unsealed 0xf101 -- pieces are "unsealed" data).
// Exposed as a helper for tests that need to construct piece CIDs.
func PieceCIDFromDigest(digest []byte) cid.Cid { return commCIDFromDigest(digest) }

// appendUvarint appends a uvarint encoding of v to b.
func appendUvarint(b []byte, v uint64) []byte {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, v)
	return append(b, buf[:n]...)
}

// log2Exact returns log2(v), assuming v is a power of two.
func log2Exact(v uint64) int {
	n := 0
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

func isPowerOfTwo(v uint64) bool { return v > 0 && (v&(v-1)) == 0 }
