// Package gasmeter is a pure-Go WASM binary rewriter that injects gas
// metering into a module (lantern#89 Stage C2 / #128 mechanism proof).
//
// ref-fvm meters gas at the WASM instruction level via a compile-time
// transform (there is no gas.charge syscall; the runtime deducts per
// opcode). wazero ships no equivalent, so a pure-Go FVM needs its own.
// This package proves the mechanism: it appends a mutable i64 "remaining
// gas" global to a module and injects, at every function entry, a charge
// + out-of-gas check:
//
//	global.get $gas
//	i64.const <cost>
//	i64.sub
//	global.set $gas
//	global.get $gas
//	i64.const 0
//	i64.lt_s
//	if
//	  unreachable        ;; out of gas
//	end
//
// It exports the gas global as "remaining_gas" so a host can read
// consumption after a run, and initializes it to the supplied budget.
//
// This is a MECHANISM proof, not ref-fvm gas fidelity. Real fidelity
// requires per-basic-block charges with ref-fvm's exact per-opcode price
// list, vector-matched against filecoin-vm-workshop (that's the #128
// deliverable). What this proves: the injection technique works in pure
// Go, charges accumulate, and a tight budget traps with unreachable.
package gasmeter

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	secType     = 1
	secImport   = 2
	secFunction = 3
	secGlobal   = 6
	secExport   = 7
	secCode     = 10
)

// Rewrite injects gas metering into wasm, returns the new module bytes.
// costPerFunc is the flat gas charged at each function entry (mechanism
// proof; real fidelity uses a per-block cost derived from the price list).
func Rewrite(wasm []byte, budget int64, costPerFunc int64) ([]byte, error) {
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

	// Count imported globals + defined globals to compute the new global
	// index (imports occupy the low indices).
	importedGlobals := 0
	definedGlobals := 0
	for _, s := range sections {
		switch s.id {
		case secImport:
			importedGlobals += countImportedGlobals(s.content)
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

	// Prologue injected at each function entry.
	prologue := buildPrologue(gasGlobalIdx, costPerFunc)

	// Rebuild sections, mutating global + export + code. If there is no
	// global section, we must add one (and it must appear before export/
	// code per the WASM section-order rules).
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
		// Append our global to the (possibly empty) global section.
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
			body.Write(gs[rr.pos:]) // existing globals verbatim
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
			emitGlobal() // replaces the original global section
		case secExport:
			writeSection(secExport, addGasExport(s.content, gasGlobalIdx))
		case secCode:
			newCode, err := rewriteCode(s.content, prologue)
			if err != nil {
				return nil, fmt.Errorf("rewrite code: %w", err)
			}
			writeSection(secCode, newCode)
		default:
			// If there's no global section, inject ours right before the
			// export section (section id order: global=6 < export=7).
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

func buildPrologue(gasIdx uint32, cost int64) []byte {
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

// rewriteCode injects prologue at the start of each function body (after
// its locals declaration).
func rewriteCode(code []byte, prologue []byte) ([]byte, error) {
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

		// Parse the locals vec to find where instructions begin.
		br := &reader{buf: body}
		localsCount, err := br.uvarint()
		if err != nil {
			return nil, err
		}
		for j := uint64(0); j < localsCount; j++ {
			if _, err := br.uvarint(); err != nil { // count
				return nil, err
			}
			br.pos++ // valtype
		}
		localsEnd := br.pos

		var nb bytes.Buffer
		nb.Write(body[:localsEnd]) // locals declaration verbatim
		nb.Write(prologue)         // injected charge + OOG check
		nb.Write(body[localsEnd:]) // original instructions + end

		out.Write(uvarintBytes(uint64(nb.Len())))
		out.Write(nb.Bytes())
	}
	return out.Bytes(), nil
}

// addGasExport appends an export "remaining_gas" -> global gasIdx.
func addGasExport(exp []byte, gasIdx uint32) []byte {
	r := &reader{buf: exp}
	count, _ := r.uvarint()
	var body bytes.Buffer
	body.Write(uvarintBytes(count + 1))
	body.Write(exp[r.pos:]) // existing exports verbatim
	name := []byte("remaining_gas")
	body.Write(uvarintBytes(uint64(len(name))))
	body.Write(name)
	body.WriteByte(0x03) // export kind: global
	body.Write(uvarintBytes(uint64(gasIdx)))
	return body.Bytes()
}

func countImportedGlobals(imp []byte) int {
	r := &reader{buf: imp}
	count, _ := r.uvarint()
	n := 0
	for i := uint64(0); i < count; i++ {
		ml, _ := r.uvarint()
		r.pos += int(ml) // module name
		fl, _ := r.uvarint()
		r.pos += int(fl) // field name
		kind := r.buf[r.pos]
		r.pos++
		switch kind {
		case 0x00: // func: typeidx
			r.uvarint()
		case 0x01: // table: reftype + limits
			r.pos++ // reftype
			r.readLimits()
		case 0x02: // mem: limits
			r.readLimits()
		case 0x03: // global: valtype + mut
			r.pos += 2
			n++
		}
	}
	return n
}

// --- minimal LEB128 + reader ---

type reader struct {
	buf []byte
	pos int
}

func (r *reader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.buf[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("bad uvarint at %d", r.pos)
	}
	r.pos += n
	return v, nil
}

func (r *reader) readLimits() {
	flag := r.buf[r.pos]
	r.pos++
	r.uvarint() // min
	if flag == 0x01 {
		r.uvarint() // max
	}
}

func uvarintBytes(v uint64) []byte {
	var b [10]byte
	n := binary.PutUvarint(b[:], v)
	return b[:n]
}

func sleb128(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			out = append(out, b)
			return out
		}
		out = append(out, b|0x80)
	}
}
