package evm

// memory is the EVM's byte-addressed, zero-initialised, word-expandable
// scratch memory.
type memory struct {
	store []byte
}

func (m *memory) len() int { return len(m.store) }

// expand grows memory to cover [offset, offset+size). EVM memory rounds up
// to 32-byte words.
func (m *memory) expand(offset, size uint64) {
	if size == 0 {
		return
	}
	need := offset + size
	// round up to word boundary
	if r := need % 32; r != 0 {
		need += 32 - r
	}
	if uint64(len(m.store)) < need {
		grown := make([]byte, need)
		copy(grown, m.store)
		m.store = grown
	}
}

// set writes b at offset, expanding as needed.
func (m *memory) set(offset uint64, b []byte) {
	if len(b) == 0 {
		return
	}
	m.expand(offset, uint64(len(b)))
	copy(m.store[offset:offset+uint64(len(b))], b)
}

// set32 writes a 32-byte word at offset.
func (m *memory) set32(offset uint64, val [32]byte) {
	m.expand(offset, 32)
	copy(m.store[offset:offset+32], val[:])
}

// set8 writes a single byte at offset.
func (m *memory) set8(offset uint64, b byte) {
	m.expand(offset, 1)
	m.store[offset] = b
}

// get returns a copy of size bytes at offset, zero-padded past the end.
func (m *memory) get(offset, size uint64) []byte {
	if size == 0 {
		return nil
	}
	out := make([]byte, size)
	if offset < uint64(len(m.store)) {
		copy(out, m.store[offset:min64(offset+size, uint64(len(m.store)))])
	}
	return out
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
