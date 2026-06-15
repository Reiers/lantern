package kamt

import "testing"

func TestMatchExtension(t *testing.T) {
	// key bits: 0b10110... ; ext path 0b101 (3 bits) at bit_width 5.
	key := []byte{0b10110000}
	hb := newHashBits(key)
	// ext path first 3 bits = 101 -> matches key's first 3 bits.
	m, err := matchExtension(hb, []byte{0b10100000}, 3, 5)
	if err != nil { t.Fatal(err) }
	if m != 3 { t.Errorf("full match: got %d want 3", m) }
	// now a diverging ext: key next bits after 3 consumed are 10...; ext 11 -> mismatch at first window.
	hb2 := newHashBits([]byte{0b10110000})
	m2, _ := matchExtension(hb2, []byte{0b11000000}, 2, 5)
	if m2 != 0 { t.Errorf("partial(diverge) match: got %d want 0", m2) }
	if hb2.consumed != 0 { t.Errorf("mismatch must not consume key bits, consumed=%d", hb2.consumed) }
}
