package gasmeter

// RewriteV2: per-basic-block gas metering matching ref-fvm's WasmGasPrices.
//
// Upgrade from the mechanism-proof Rewrite (per-function-entry flat charge)
// to the ref-fvm-fidelity rewriter (per-basic-block charges with the exact
// Watermelon price list). This is #128.
//
// Basic-block boundaries follow fvm-wasm-instrument's metering algorithm:
// a new metered block starts at function entry, after block/loop/if/else,
// and after br/br_if/br_table/return/unreachable (since the next
// instruction is a forward-branch target reachable from a non-linear path).
//
// Each block's static cost = InstructionGas * instruction_count
//                           + HostCallGas * host_call_count.
// memory.copy/fill Linear charges are a follow-on (runtime injection).

import (
	"bytes"
	"fmt"
)

// RewriteV2 injects per-basic-block gas metering into wasm using the
// ref-fvm Watermelon price list. importedFuncCount is the number of
// imported functions (indices 0..importedFuncCount-1 are host calls).
func RewriteV2(wasm []byte, budget int64) ([]byte, error) {
	if len(wasm) < 8 || !bytes.Equal(wasm[:4], []byte{0x00, 0x61, 0x73, 0x6d}) {
		return nil, fmt.Errorf("not a wasm module")
	}
	r := &reader{buf: wasm, pos: 8}

	type section struct {
		id      byte
		content []byte
	}
	var sections []section
	for r.pos < len(r.buf) {
		id := r.buf[r.pos]
		r.pos++
		size, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		content := r.buf[r.pos : r.pos+int(size)]
		r.pos += int(size)
		sections = append(sections, section{id: id, content: append([]byte(nil), content...)})
	}

	// Count imported functions + globals.
	importedFuncs := 0
	importedGlobals := 0
	definedGlobals := 0
	for _, s := range sections {
		switch s.id {
		case secImport:
			importedFuncs, importedGlobals = countImports(s.content)
		case secGlobal:
			cnt, _ := (&reader{buf: s.content}).uvarint()
			definedGlobals = int(cnt)
		}
	}
	gasGlobalIdx := uint32(importedGlobals + definedGlobals)

	// Build the injected global: (global (mut i64) (i64.const budget)).
	var newGlobal bytes.Buffer
	newGlobal.WriteByte(0x7e) // valtype i64
	newGlobal.WriteByte(0x01) // mutable
	newGlobal.WriteByte(0x42) // i64.const
	newGlobal.Write(sleb128(budget))
	newGlobal.WriteByte(0x0b) // end

	haveGlobal := false
	for _, s := range sections {
		if s.id == secGlobal {
			haveGlobal = true
		}
	}

	var out bytes.Buffer
	out.Write(wasm[:8]) // magic + version

	writeSection := func(id byte, content []byte) {
		out.WriteByte(id)
		out.Write(uvarintBytes(uint64(len(content))))
		out.Write(content)
	}

	globalWritten := false
	emitGlobal := func() {
		var gs []byte
		if haveGlobal {
			for _, s := range sections {
				if s.id == secGlobal {
					gs = s.content
					break
				}
			}
		}
		var body bytes.Buffer
		cnt := uint64(0)
		if len(gs) > 0 {
			rr := &reader{buf: gs}
			c, _ := rr.uvarint()
			cnt = c
			body.Write(uvarintBytes(cnt + 1))
			body.Write(gs[rr.pos:])
		} else {
			body.Write(uvarintBytes(1))
		}
		body.Write(newGlobal.Bytes())
		writeSection(secGlobal, body.Bytes())
		globalWritten = true
	}

	for _, s := range sections {
		switch s.id {
		case secGlobal:
			emitGlobal()
		case secExport:
			if !haveGlobal && !globalWritten {
				emitGlobal()
			}
			writeSection(secExport, addGasExport(s.content, gasGlobalIdx))
		case secCode:
			if !haveGlobal && !globalWritten {
				emitGlobal()
			}
			newCode, err := rewriteCodeV2(s.content, gasGlobalIdx, importedFuncs)
			if err != nil {
				return nil, fmt.Errorf("rewrite code: %w", err)
			}
			writeSection(secCode, newCode)
		default:
			if !haveGlobal && !globalWritten && s.id > secGlobal {
				emitGlobal()
			}
			writeSection(s.id, s.content)
		}
	}
	if !globalWritten {
		emitGlobal()
	}
	return out.Bytes(), nil
}

// countImports returns (importedFuncCount, importedGlobalCount).
func countImports(imp []byte) (int, int) {
	r := &reader{buf: imp}
	count, _ := r.uvarint()
	funcs, globals := 0, 0
	for i := uint64(0); i < count; i++ {
		ml, _ := r.uvarint()
		r.pos += int(ml)
		fl, _ := r.uvarint()
		r.pos += int(fl)
		kind := r.buf[r.pos]
		r.pos++
		switch kind {
		case 0x00: // func
			r.uvarint()
			funcs++
		case 0x01: // table
			r.pos++
			r.readLimits()
		case 0x02: // mem
			r.readLimits()
		case 0x03: // global
			r.pos += 2
			globals++
		}
	}
	return funcs, globals
}

// rewriteCodeV2 processes each function body with per-basic-block metering.
func rewriteCodeV2(code []byte, gasGlobalIdx uint32, importedFuncs int) ([]byte, error) {
	r := &reader{buf: code}
	count, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(uvarintBytes(count))
	for i := uint64(0); i < count; i++ {
		bodySize, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		body := r.buf[r.pos : r.pos+int(bodySize)]
		r.pos += int(bodySize)

		rewritten, err := rewriteFuncBody(body, gasGlobalIdx, importedFuncs)
		if err != nil {
			return nil, fmt.Errorf("func %d: %w", i, err)
		}
		out.Write(uvarintBytes(uint64(len(rewritten))))
		out.Write(rewritten)
	}
	return out.Bytes(), nil
}

// rewriteFuncBody splits one function body into basic blocks and injects
// a gas charge at the start of each block.
func rewriteFuncBody(body []byte, gasGlobalIdx uint32, importedFuncs int) ([]byte, error) {
	br := &reader{buf: body}
	// Parse locals.
	localsCount, err := br.uvarint()
	if err != nil {
		return nil, err
	}
	for j := uint64(0); j < localsCount; j++ {
		if _, err := br.uvarint(); err != nil {
			return nil, err
		}
		br.pos++ // valtype
	}
	localsEnd := br.pos

	// Walk instructions, identify basic-block boundaries, compute costs.
	type block struct {
		cost  int64
		bytes []byte
	}
	var blocks []block
	var curBytes bytes.Buffer
	instrCount := 0
	hostCalls := 0
	depth := 0 // control-flow nesting depth

	flushBlock := func() {
		if instrCount > 0 || curBytes.Len() > 0 {
			blocks = append(blocks, block{
				cost:  BlockCost(instrCount, hostCalls),
				bytes: append([]byte(nil), curBytes.Bytes()...),
			})
		}
		curBytes.Reset()
		instrCount = 0
		hostCalls = 0
	}

	for br.pos < len(br.buf) {
		opStart := br.pos
		op := br.buf[br.pos]
		br.pos++

		// Determine if this opcode starts a new basic block (charge point).
		// A new block starts BEFORE: block, loop, if, else.
		// A new block starts AFTER: br, br_if, br_table, return, unreachable, end.
		startsBlock := false
		endsBlock := false

		switch op {
		case 0x02, 0x03, 0x04: // block, loop, if
			startsBlock = true
			depth++
			// blocktype: 0x40 (void) or valtype or s33 (type index)
			br.readBlocktype()
		case 0x05: // else
			startsBlock = true
			// no immediate
		case 0x0B: // end
			endsBlock = true
			if depth > 0 {
				depth--
			}
		case 0x0C: // br
			endsBlock = true
			br.uvarint() // label
		case 0x0D: // br_if
			endsBlock = true
			br.uvarint() // label
		case 0x0E: // br_table
			endsBlock = true
			n, _ := br.uvarint()
			for k := uint64(0); k <= n; k++ {
				br.uvarint() // labels + default
			}
		case 0x0F: // return
			endsBlock = true
		case 0x00: // unreachable
			endsBlock = true
		case 0x10: // call
			funcIdx, _ := br.uvarint()
			if int(funcIdx) < importedFuncs {
				hostCalls++
			}
		case 0x11: // call_indirect
			br.uvarint() // type index
			br.uvarint() // table index (0x00 in MVP)
		default:
			// Skip the immediate bytes for all other opcodes.
			skipImmediate(op, br)
		}

		if startsBlock {
			// Flush the current block BEFORE this instruction.
			flushBlock()
		}

		// Add this instruction's bytes to the current block.
		curBytes.Write(br.buf[opStart:br.pos])
		instrCount++

		if endsBlock {
			// Flush the current block INCLUDING this instruction.
			flushBlock()
		}
	}
	flushBlock()

	// Reassemble: locals + (charge + block_bytes) for each block.
	var result bytes.Buffer
	result.Write(body[:localsEnd])
	for _, b := range blocks {
		if b.cost > 0 {
			result.Write(buildChargeV2(gasGlobalIdx, b.cost))
		}
		result.Write(b.bytes)
	}
	return result.Bytes(), nil
}

// buildChargeV2 emits the gas-charge prologue for a basic block with the
// given cost. Same shape as buildPrologue but with the specific cost.
func buildChargeV2(gasIdx uint32, cost int64) []byte {
	var b bytes.Buffer
	b.WriteByte(0x23)
	b.Write(uvarintBytes(uint64(gasIdx))) // global.get $gas
	b.WriteByte(0x42)
	b.Write(sleb128(cost)) // i64.const cost
	b.WriteByte(0x7d)      // i64.sub
	b.WriteByte(0x24)
	b.Write(uvarintBytes(uint64(gasIdx))) // global.set $gas
	b.WriteByte(0x23)
	b.Write(uvarintBytes(uint64(gasIdx))) // global.get $gas
	b.WriteByte(0x42)
	b.WriteByte(0x00) // i64.const 0
	b.WriteByte(0x53) // i64.lt_s
	b.WriteByte(0x04)
	b.WriteByte(0x40) // if (void)
	b.WriteByte(0x00) // unreachable
	b.WriteByte(0x0b) // end
	return b.Bytes()
}

// skipImmediate advances the reader past the immediate bytes for the given
// opcode (opcodes not handled by the main switch). This is the minimal set
// needed for Filecoin's v17 builtin-actor WASMs (MVP WASM + sign-extension
// + bulk-memory proposals).
func skipImmediate(op byte, r *reader) {
	switch {
	// No immediate: nop, drop, select, ref.null/is_null/func,
	// i32/i64/f32/f64 arithmetic/comparison/conversion, and all
	// sign-extension instructions (0xC0..0xC4).
	case op == 0x01 || op == 0x1A || op == 0x1B:
		// nop, drop, select: no immediate
	case op >= 0x45 && op <= 0xC4:
		// i32/i64/f32/f64 operations + sign-extension: no immediate
	case op == 0xD0: // ref.null
		r.pos++ // reftype
	case op == 0xD1: // ref.is_null
		// no immediate
	case op == 0xD2: // ref.func
		r.uvarint() // funcidx

	// Variable access: local.get/set/tee, global.get/set.
	case op >= 0x20 && op <= 0x24:
		r.uvarint()

	// Memory load/store: 2 immediates (align + offset).
	case op >= 0x28 && op <= 0x3E:
		r.uvarint() // align
		r.uvarint() // offset

	// memory.size, memory.grow: 1 immediate (memory index, always 0 in MVP).
	case op == 0x3F || op == 0x40:
		r.uvarint()

	// Constants.
	case op == 0x41: // i32.const
		r.readSignedLEB()
	case op == 0x42: // i64.const
		r.readSignedLEB()
	case op == 0x43: // f32.const
		r.pos += 4
	case op == 0x44: // f64.const
		r.pos += 8

	// Multi-byte opcodes (0xFC prefix).
	case op == 0xFC:
		subOp, _ := r.uvarint()
		switch subOp {
		case 8: // memory.init
			r.uvarint() // dataidx
			r.uvarint() // memidx (0)
		case 9: // data.drop
			r.uvarint()
		case 10: // memory.copy
			r.uvarint() // dst mem (0)
			r.uvarint() // src mem (0)
		case 11: // memory.fill
			r.uvarint() // mem (0)
		case 12: // table.init
			r.uvarint() // elemidx
			r.uvarint() // tableidx
		case 13: // elem.drop
			r.uvarint()
		case 14: // table.copy
			r.uvarint()
			r.uvarint()
		case 15, 16, 17: // table.grow, table.size, table.fill
			r.uvarint()
		default:
			// i32.trunc_sat_f32_s .. i64.trunc_sat_f64_u (0..7): no extra immediate
		}

	// 0xFD prefix: SIMD — not used by Filecoin actors, skip if encountered.
	case op == 0xFD:
		r.uvarint() // sub-opcode
		// SIMD opcodes may have immediates; skipping for now (not in actors).

	// select with type: 0x1C
	case op == 0x1C:
		n, _ := r.uvarint()
		r.pos += int(n) // valtypes
	}
}

// readBlocktype reads a WASM blocktype immediate: 0x40 (void), a valtype,
// or a signed LEB128 type index (s33).
func (r *reader) readBlocktype() {
	if r.pos >= len(r.buf) {
		return
	}
	b := r.buf[r.pos]
	switch {
	case b == 0x40: // void
		r.pos++
	case b == 0x7F || b == 0x7E || b == 0x7D || b == 0x7C || b == 0x7B || b == 0x70 || b == 0x6F:
		// valtype
		r.pos++
	default:
		// s33 type index
		r.readSignedLEB()
	}
}

// readSignedLEB advances past a signed LEB128 value.
func (r *reader) readSignedLEB() {
	for r.pos < len(r.buf) {
		b := r.buf[r.pos]
		r.pos++
		if b&0x80 == 0 {
			return
		}
	}
}
