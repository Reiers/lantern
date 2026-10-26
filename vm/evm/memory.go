package evm

import (
	"fmt"
	"math"

	"github.com/holiman/uint256"
)

// maxMemoryBytes caps local EVM memory. The FEVM bounds offsets/sizes to
// u32 and gas bounds real usage far below that; a local eth_call that
// tries to grow memory past this cap is not served locally (it faults with
// ErrUnsupported and falls back) instead of letting a single RPC allocate
// gigabytes.
const maxMemoryBytes = 64 << 20

// memory is the EVM's byte-addressed, zero-initialised, word-expandable
// scratch memory.
type memory struct {
	store []byte
}

func (m *memory) len() int { return len(m.store) }

// region validates an (offset, size) pair the way the FEVM's
// get_memory_region does and grows memory to cover it. A zero size is a
// no-op whatever the offset (no growth, no bounds check). Offsets/sizes
// that don't fit u32, or whose end overflows u32, are an illegal memory
// access (an abort in the FEVM).
func (m *memory) region(offset, size uint256.Int) (uint64, uint64, error) {
	if size.IsZero() {
		return 0, 0, nil
	}
	if !size.IsUint64() || size.Uint64() > math.MaxUint32 {
		return 0, 0, fmt.Errorf("evm: illegal memory access: size must be less than max u32")
	}
	if !offset.IsUint64() || offset.Uint64() > math.MaxUint32 {
		return 0, 0, fmt.Errorf("evm: illegal memory access: offset must be less than max u32")
	}
	off, sz := offset.Uint64(), size.Uint64()
	end := off + sz
	if end > math.MaxUint32 {
		return 0, 0, fmt.Errorf("evm: illegal memory access: new memory size exceeds max u32")
	}
	if end > maxMemoryBytes {
		return 0, 0, fmt.Errorf("%w: memory expansion to %d bytes exceeds local cap", ErrUnsupported, end)
	}
	m.expand(off, sz)
	return off, sz, nil
}

// expand grows memory to cover [offset, offset+size). EVM memory rounds up
// to 32-byte words. Callers bound offset/size through region first.
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

// slice returns the live memory backing [off, off+size). Callers must have
// validated and grown the region via region().
func (m *memory) slice(off, size uint64) []byte {
	if size == 0 {
		return nil
	}
	return m.store[off : off+size]
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
