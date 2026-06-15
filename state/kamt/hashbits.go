package kamt

import "fmt"

// hashBits consumes the key bit-by-bit, MSB-first (big-endian), matching
// ref-fvm's HashBits: at each KAMT level we take `bit_width` bits to index
// into the node's slot bitfield, and extensions consume an arbitrary
// number of bits as a shared prefix.
//
// The key is the 32-byte big-endian storage slot. Bit position 0 is the
// most-significant bit of byte 0.
type hashBits struct {
	b        []byte
	consumed int // bits consumed so far
}

func newHashBits(b []byte) *hashBits {
	return &hashBits{b: b}
}

// Next consumes the next `n` bits (n <= 8 in practice for bit_width) and
// returns them as the low bits of an int, MSB-first within the window.
func (h *hashBits) Next(n int) (int, error) {
	if n <= 0 || n > 32 {
		return 0, fmt.Errorf("hashBits.Next: bad width %d", n)
	}
	v := 0
	for i := 0; i < n; i++ {
		bit, err := h.nextBit()
		if err != nil {
			return 0, err
		}
		v = (v << 1) | bit
	}
	return v, nil
}

// Consume advances past `n` bits without returning them (used for
// extension prefixes on Link pointers).
func (h *hashBits) Consume(n int) error {
	for i := 0; i < n; i++ {
		if _, err := h.nextBit(); err != nil {
			return err
		}
	}
	return nil
}

// matchExtension compares an extension's `length` path bits (held in
// pathBytes, MSB-first) against the key's next bits, consuming bit_width
// bits at a time, and returns the number of bits matched. It mirrors
// ref-fvm Extension::longest_match: it consumes from the key only the bits
// that match the extension. A return value < length means the key diverges
// (the caller treats that as slot-absent); == length means a full match and
// the caller descends.
func matchExtension(hb *hashBits, pathBytes []byte, length, bitWidth int) (int, error) {
	path := &hashBits{b: pathBytes}
	matched := 0
	for matched < length {
		take := bitWidth
		if rem := length - matched; rem < take {
			take = rem
		}
		// Snapshot the key cursor so a non-matching window doesn't consume
		// key bits (ref-fvm restores `hashed_key.consumed` on mismatch).
		before := hb.consumed
		keyChunk, err := hb.Next(take)
		if err != nil {
			return matched, err
		}
		extChunk, err := path.Next(take)
		if err != nil {
			return matched, err
		}
		if keyChunk != extChunk {
			hb.consumed = before // un-consume the mismatched window
			return matched, nil
		}
		matched += take
	}
	return matched, nil
}

func (h *hashBits) nextBit() (int, error) {
	byteIdx := h.consumed / 8
	if byteIdx >= len(h.b) {
		return 0, fmt.Errorf("hashBits: out of bits at position %d (key %d bytes)", h.consumed, len(h.b))
	}
	bitOffset := uint(7 - (h.consumed % 8)) // MSB-first
	bit := int((h.b[byteIdx] >> bitOffset) & 1)
	h.consumed++
	return bit, nil
}
