package gasmeter

// WasmGasPrices mirrors ref-fvm's WasmGasPrices from price_list.rs.
// These are the Filecoin Watermelon prices (unchanged through current
// mainnet, used by every network version since nv21).
//
// ref-fvm assigns the same cost to every WASM instruction (no per-opcode
// differentiation); the only special cases are host-call overhead and
// memory.copy/fill which carry a runtime-linear charge.
//
// All values are in milligas. 1 gas = 1000 milligas.

// InstructionGas is the flat cost per WASM instruction (4 gas = 4000 milligas).
const InstructionGas int64 = 4

// HostCallGas is the additional cost per call to an imported (host) function
// (14000 gas), on top of the per-instruction 4.
const HostCallGas int64 = 14000

// MemoryCopyPerByteMilligas is the per-byte cost for memory.copy (0.4 gas = 400 milligas).
// Applied at runtime via a Linear gas charge (base + stack_top * unit).
const MemoryCopyPerByteMilligas int64 = 400

// MemoryFillPerByteMilligas is the per-byte cost for memory.fill (0.4 gas = 400 milligas).
const MemoryFillPerByteMilligas int64 = 400

// BlockCost computes the static gas cost of a basic block containing
// `nInstructions` total instructions, of which `nHostCalls` are calls to
// imported functions. The result is in gas (not milligas).
func BlockCost(nInstructions, nHostCalls int) int64 {
	return InstructionGas*int64(nInstructions) + HostCallGas*int64(nHostCalls)
}
