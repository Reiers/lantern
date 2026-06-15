// Package kamt is a read-only reader for the ref-fvm KAMT (Keccak-AMT)
// as used by the FEVM EVM actor for contract storage (lantern#43 Part B,
// Stage 2).
//
// # What a KAMT is
//
// The KAMT is a champ-like trie that, unlike the HAMT, does NOT hash its
// keys — it indexes a fixed-size byte-array key directly, consuming
// `bit_width` bits at a time from the (big-endian) key to pick a slot at
// each level. Solidity lays out contiguous storage slots, so the KAMT uses
// "extensions" (a shared key-bit prefix stored on a Link pointer) to avoid
// degenerate depth.
//
// # FEVM contract-storage config (builtin-actors evm/src/interpreter/system.rs)
//
//	KAMT_CONFIG = { min_data_depth: 0, bit_width: 5, max_array_width: 1 }
//	StateKamt   = Kamt<U256, U256, StateHashAlgorithm>
//	as_hashed_key(slot U256) = slot.to_big_endian()   // 32 bytes BE, no hash
//
// So: the "hashed key" for an eth storage slot is just the 32-byte
// big-endian slot number. bit_width 5 => 32 slots per node (a 4-byte
// bitfield). Values buckets hold a single KV (max_array_width 1).
//
// # Node CBOR layout (ref-fvm ipld/kamt)
//
//	Node    = [ bitfield (byte string), [ pointer, ... ] ]
//	Pointer = { "v": [ KV, ... ] }            // Values bucket
//	        | { "l": [ cid, extLen u32, extBytes ] }  // Link (+ optional extension)
//	KV      = [ keyBytes, valueBytes ]        // both byte strings
//
// This reader fetches every node CID-verified through the accessor's
// BlockGetter, so a value it returns is backed by the same end-to-end
// verification as the rest of Lantern's state access.
package kamt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"

	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/state/hamt"
)

// FEVM contract-storage KAMT parameters.
const (
	// BitWidth is bits consumed from the key per level (FEVM = 5).
	BitWidth = 5
	// slotsPerNode = 2^BitWidth.
	slotsPerNode = 1 << BitWidth
	// KeyLen is the fixed key length in bytes (U256 => 32).
	KeyLen = 32
)

// ErrNotFound is returned when a slot is absent from the KAMT (which,
// for eth storage, means the slot reads as zero).
var ErrNotFound = fmt.Errorf("kamt: slot not present")

// Get looks up a 32-byte big-endian storage slot in the KAMT rooted at
// `root` and returns the raw value bytes (the stored U256, big-endian,
// minimal-length as encoded). Returns ErrNotFound if the slot is absent.
//
// The returned proof path is the list of node CIDs fetched, in order, so
// a caller can re-verify independently if desired.
func Get(ctx context.Context, root cid.Cid, slot [KeyLen]byte, bg hamt.BlockGetter) ([]byte, []cid.Cid, error) {
	hb := newHashBits(slot[:])
	var path []cid.Cid
	cur := root
	for {
		raw, err := bg.Get(ctx, cur)
		if err != nil {
			return nil, path, fmt.Errorf("fetch kamt node %s: %w", cur, err)
		}
		if err := hamt.VerifyBlockCID(cur, raw); err != nil {
			return nil, path, fmt.Errorf("kamt node %s: %w", cur, err)
		}
		path = append(path, cur)

		n, err := decodeNode(raw)
		if err != nil {
			return nil, path, fmt.Errorf("decode kamt node %s: %w", cur, err)
		}

		idx, err := hb.Next(BitWidth)
		if err != nil {
			return nil, path, fmt.Errorf("kamt key exhausted at node %s: %w", cur, err)
		}
		if !n.testBit(idx) {
			return nil, path, ErrNotFound
		}
		pos := n.popCountBelow(idx)
		if pos >= len(n.pointers) {
			return nil, path, fmt.Errorf("kamt pointer index %d out of range (%d pointers)", pos, len(n.pointers))
		}
		p := n.pointers[pos]

		switch {
		case p.isValues:
			// FEVM/ref-fvm stores the KAMT key with all leading zero bytes
			// stripped (storage slot 0 has an empty key, slot 1 has key
			// [0x01], etc.). Compare the stored key against the canonical
			// minimal-length form of our slot.
			wantKey := trimLeadingZeros(slot[:])
			for _, kv := range p.values {
				if bytes.Equal(kv.key, wantKey) {
					return kv.value, path, nil
				}
			}
			return nil, path, ErrNotFound
		default:
			// Link: consume the extension bits (shared prefix) before
			// descending, so the next-level index is taken from the
			// correct key position.
			if p.extLen > 0 {
				if err := hb.Consume(int(p.extLen)); err != nil {
					return nil, path, fmt.Errorf("kamt extension consume: %w", err)
				}
			}
			cur = p.link
		}
	}
}

// GetU256 is a convenience wrapper returning the slot value as a
// big.Int (zero when the slot is absent). eth_getStorageAt semantics:
// absent slot == 0.
func GetU256(ctx context.Context, root cid.Cid, slot *big.Int, bg hamt.BlockGetter) (*big.Int, []cid.Cid, error) {
	var key [KeyLen]byte
	slot.FillBytes(key[:]) // big-endian, left-zero-padded to 32 bytes
	raw, path, err := Get(ctx, root, key, bg)
	if err == ErrNotFound {
		return big.NewInt(0), path, nil
	}
	if err != nil {
		return nil, path, err
	}
	return new(big.Int).SetBytes(raw), path, nil
}

// ---- node decoding ----

type kamtKV struct {
	key   []byte
	value []byte
}

type kamtPointer struct {
	isValues bool
	values   []kamtKV
	link     cid.Cid
	extLen   uint32
	extBytes []byte
}

type kamtNode struct {
	bitfield *big.Int
	pointers []kamtPointer
}

func (n *kamtNode) testBit(idx int) bool {
	return n.bitfield.Bit(idx) == 1
}

// popCountBelow counts set bits at positions [0, idx), giving the dense
// pointer-array position for slot idx.
func (n *kamtNode) popCountBelow(idx int) int {
	count := 0
	for i := 0; i < idx; i++ {
		if n.bitfield.Bit(i) == 1 {
			count++
		}
	}
	return count
}

// decodeNode parses a KAMT node: [ bitfield(bytes), [pointer,...] ].
func decodeNode(raw []byte) (*kamtNode, error) {
	br := bytes.NewReader(raw)

	maj, length, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("node header: %w", err)
	}
	if maj != cbg.MajArray || length != 2 {
		return nil, fmt.Errorf("node not a 2-array (maj %d len %d)", maj, length)
	}

	// bitfield: a CBOR byte string, big-endian bit set.
	bfMaj, bfLen, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("bitfield header: %w", err)
	}
	if bfMaj != cbg.MajByteString {
		return nil, fmt.Errorf("bitfield not a byte string (maj %d)", bfMaj)
	}
	bfBytes := make([]byte, bfLen)
	if _, err := io.ReadFull(br, bfBytes); err != nil {
		return nil, fmt.Errorf("bitfield bytes: %w", err)
	}
	bitfield := new(big.Int).SetBytes(bfBytes) // big-endian -> bit i set per SetBytes

	// pointers array.
	pMaj, pLen, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("pointers header: %w", err)
	}
	if pMaj != cbg.MajArray {
		return nil, fmt.Errorf("pointers not an array (maj %d)", pMaj)
	}
	pointers := make([]kamtPointer, 0, pLen)
	for i := uint64(0); i < pLen; i++ {
		p, err := decodePointer(br)
		if err != nil {
			return nil, fmt.Errorf("pointer %d: %w", i, err)
		}
		pointers = append(pointers, p)
	}

	return &kamtNode{bitfield: bitfield, pointers: pointers}, nil
}

// decodePointer parses a single map-form pointer:
//
//	{ "v": [KV,...] }  or  { "l": [cid, extLen, extBytes] }
func decodePointer(br io.Reader) (kamtPointer, error) {
	var p kamtPointer
	maj, n, err := cbg.CborReadHeader(br)
	if err != nil {
		return p, fmt.Errorf("pointer header: %w", err)
	}
	if maj != cbg.MajMap || n != 1 {
		return p, fmt.Errorf("pointer not a 1-map (maj %d len %d)", maj, n)
	}
	// key: "v" or "l"
	kMaj, kLen, err := cbg.CborReadHeader(br)
	if err != nil {
		return p, fmt.Errorf("pointer key header: %w", err)
	}
	if kMaj != cbg.MajTextString || kLen != 1 {
		return p, fmt.Errorf("pointer key not 1-char text (maj %d len %d)", kMaj, kLen)
	}
	kb := make([]byte, 1)
	if _, err := io.ReadFull(br, kb); err != nil {
		return p, fmt.Errorf("pointer key byte: %w", err)
	}

	switch kb[0] {
	case 'v':
		vals, err := decodeValues(br)
		if err != nil {
			return p, err
		}
		p.isValues = true
		p.values = vals
		return p, nil
	case 'l':
		// [ cid, extLen(u32), extBytes ]
		aMaj, aLen, err := cbg.CborReadHeader(br)
		if err != nil {
			return p, fmt.Errorf("link tuple header: %w", err)
		}
		if aMaj != cbg.MajArray || aLen != 3 {
			return p, fmt.Errorf("link tuple not a 3-array (maj %d len %d)", aMaj, aLen)
		}
		c, err := readCID(br)
		if err != nil {
			return p, fmt.Errorf("link cid: %w", err)
		}
		extLen, err := readUint(br)
		if err != nil {
			return p, fmt.Errorf("link extLen: %w", err)
		}
		eMaj, eLen, err := cbg.CborReadHeader(br)
		if err != nil {
			return p, fmt.Errorf("link ext header: %w", err)
		}
		if eMaj != cbg.MajByteString {
			return p, fmt.Errorf("link ext not byte string (maj %d)", eMaj)
		}
		eb := make([]byte, eLen)
		if _, err := io.ReadFull(br, eb); err != nil {
			return p, fmt.Errorf("link ext bytes: %w", err)
		}
		p.link = c
		p.extLen = uint32(extLen)
		p.extBytes = eb
		return p, nil
	default:
		return p, fmt.Errorf("unknown pointer key %q", string(kb))
	}
}

// decodeValues parses [ KV, ... ] where KV = [keyBytes, valueBytes].
func decodeValues(br io.Reader) ([]kamtKV, error) {
	maj, n, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("values header: %w", err)
	}
	if maj != cbg.MajArray {
		return nil, fmt.Errorf("values not an array (maj %d)", maj)
	}
	out := make([]kamtKV, 0, n)
	for i := uint64(0); i < n; i++ {
		kvMaj, kvLen, err := cbg.CborReadHeader(br)
		if err != nil {
			return nil, fmt.Errorf("kv %d header: %w", i, err)
		}
		if kvMaj != cbg.MajArray || kvLen != 2 {
			return nil, fmt.Errorf("kv %d not a 2-array (maj %d len %d)", i, kvMaj, kvLen)
		}
		key, err := readByteString(br)
		if err != nil {
			return nil, fmt.Errorf("kv %d key: %w", i, err)
		}
		val, err := readByteString(br)
		if err != nil {
			return nil, fmt.Errorf("kv %d value: %w", i, err)
		}
		out = append(out, kamtKV{key: key, value: val})
	}
	return out, nil
}

// ---- low-level CBOR helpers ----

func readByteString(br io.Reader) ([]byte, error) {
	maj, n, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, err
	}
	if maj != cbg.MajByteString {
		return nil, fmt.Errorf("expected byte string, got maj %d", maj)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(br, b); err != nil {
		return nil, err
	}
	return b, nil
}

func readUint(br io.Reader) (uint64, error) {
	maj, n, err := cbg.CborReadHeader(br)
	if err != nil {
		return 0, err
	}
	if maj != cbg.MajUnsignedInt {
		return 0, fmt.Errorf("expected uint, got maj %d", maj)
	}
	return n, nil
}

// readCID reads a CBOR tag-42 CID link.
func readCID(br io.Reader) (cid.Cid, error) {
	maj, tag, err := cbg.CborReadHeader(br)
	if err != nil {
		return cid.Undef, err
	}
	if maj != cbg.MajTag || tag != 42 {
		return cid.Undef, fmt.Errorf("expected tag 42 (CID), got maj %d tag %d", maj, tag)
	}
	bsMaj, bsLen, err := cbg.CborReadHeader(br)
	if err != nil {
		return cid.Undef, err
	}
	if bsMaj != cbg.MajByteString {
		return cid.Undef, fmt.Errorf("CID payload not byte string (maj %d)", bsMaj)
	}
	buf := make([]byte, bsLen)
	if _, err := io.ReadFull(br, buf); err != nil {
		return cid.Undef, err
	}
	// CID bytes in DagCBOR links carry a leading 0x00 multibase-identity
	// prefix; strip it.
	if len(buf) == 0 || buf[0] != 0x00 {
		return cid.Undef, fmt.Errorf("CID link missing 0x00 identity prefix")
	}
	c, err := cid.Cast(buf[1:])
	if err != nil {
		return cid.Undef, fmt.Errorf("cast CID: %w", err)
	}
	return c, nil
}

// trimLeadingZeros returns b with ALL leading zero bytes removed. An
// all-zero input returns an empty slice, matching the ref-fvm KAMT key
// canonicalisation (U256 minimal big-endian byte form).
func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}
