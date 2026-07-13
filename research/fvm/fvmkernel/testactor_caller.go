package fvmkernel

// Minimal caller-actor WASM emitter (lantern#129, Stage C3).
//
// This builds a tiny synthetic actor module whose sole job is to invoke
// `send.send` on a hardcoded target and pass the target's return data
// back to the caller. It lets us exercise the nested-from-WASM send
// path end-to-end without depending on a real builtin actor (multisig,
// init) that calls send inside its Rust source.
//
// The emitted module (~120 bytes) looks like:
//
//	(module
//	  (import "send" "send" (func $send (param i32 i32 i32 i64 i32 i64
//	                                          i64 i64 i64) (result i32)))
//	  (memory (export "memory") 1)
//	  (data (i32.const 0) "<target address bytes>")
//	  (func (export "invoke") (param $params i32) (result i32)
//	    ;; call send(ret_ptr=64, recip=0, recip_len=N,
//	    ;;          method=M, params_id=0, value=(hi,lo), gas=0, flags=0)
//	    i32.const 64
//	    i32.const 0
//	    i32.const <N>
//	    i64.const <M>
//	    i32.const 0
//	    i64.const <value_hi>
//	    i64.const <value_lo>
//	    i64.const 0
//	    i64.const 0
//	    call $send
//	    drop                  ;; drop errno (send returns errOK on success)
//	    i32.const 68          ;; offset of return_id inside the send-out struct
//	    i32.load offset=0 align=4
//	    return))              ;; propagate the target's return_id
//
// A WASM binary emitter kept in this package so the test suite stays
// hermetic: no wat2wasm toolchain, no cargo, no external dependencies.

import (
	"bytes"
	"encoding/binary"
)

// BuildCallerWASM emits a minimal module that calls `send.send` on the
// given target (encoded as wire bytes: protocol byte + payload) with the
// given method + value, then returns 0 (no return block).
//
// The emitted module is a full valid WebAssembly binary (magic + version
// + Type + Import + Function + Memory + Export + Code + Data sections).
func BuildCallerWASM(target []byte, method uint64, value TokenAmount) []byte {
	// ---- Type section: two function types.
	// Type 0: (i32 i32 i32 i64 i32 i64 i64 i64 i64) -> i32   (for send.send)
	// Type 1: (i32) -> i32                                    (for invoke)
	typeSec := new(bytes.Buffer)
	writeULEB(typeSec, 2) // 2 types
	// type 0
	typeSec.WriteByte(0x60)
	writeULEB(typeSec, 9)
	typeSec.Write([]byte{0x7F, 0x7F, 0x7F, 0x7E, 0x7F, 0x7E, 0x7E, 0x7E, 0x7E})
	writeULEB(typeSec, 1)
	typeSec.WriteByte(0x7F)
	// type 1
	typeSec.WriteByte(0x60)
	writeULEB(typeSec, 1)
	typeSec.WriteByte(0x7F)
	writeULEB(typeSec, 1)
	typeSec.WriteByte(0x7F)

	// ---- Import section: send.send : type 0 -> funcidx 0.
	impSec := new(bytes.Buffer)
	writeULEB(impSec, 1)
	writeName(impSec, "send")
	writeName(impSec, "send")
	impSec.WriteByte(0x00) // kind: func
	writeULEB(impSec, 0)   // type index

	// ---- Function section: one function (invoke), type index 1.
	fnSec := new(bytes.Buffer)
	writeULEB(fnSec, 1)
	writeULEB(fnSec, 1)

	// ---- Memory section: one memory, min = 1 page.
	memSec := new(bytes.Buffer)
	writeULEB(memSec, 1)
	memSec.WriteByte(0x00) // flag: min only
	writeULEB(memSec, 1)

	// ---- Export section: "memory" -> mem 0, "invoke" -> funcidx 1.
	expSec := new(bytes.Buffer)
	writeULEB(expSec, 2)
	writeName(expSec, "memory")
	expSec.WriteByte(0x02) // kind: memory
	writeULEB(expSec, 0)
	writeName(expSec, "invoke")
	expSec.WriteByte(0x00) // kind: func
	writeULEB(expSec, 1)

	// ---- Code section: invoke body.
	// invoke pushes the 9 send-args, calls funcidx 0 (send.send), drops
	// the errno, and returns i32.const 0.
	body := new(bytes.Buffer)
	writeULEB(body, 0) // no local groups
	// i32.const 64 (ret_ptr)
	body.WriteByte(0x41)
	writeSLEB(body, 64)
	// i32.const 0 (recip_off)
	body.WriteByte(0x41)
	writeSLEB(body, 0)
	// i32.const <recip_len>
	body.WriteByte(0x41)
	writeSLEB(body, int64(len(target)))
	// i64.const method
	body.WriteByte(0x42)
	writeSLEB(body, int64(method))
	// i32.const 0 (params_id)
	body.WriteByte(0x41)
	writeSLEB(body, 0)
	// i64.const value_hi
	body.WriteByte(0x42)
	writeSLEB(body, int64(value.Hi))
	// i64.const value_lo
	body.WriteByte(0x42)
	writeSLEB(body, int64(value.Lo))
	// i64.const 0 (gas_limit)
	body.WriteByte(0x42)
	writeSLEB(body, 0)
	// i64.const 0 (flags)
	body.WriteByte(0x42)
	writeSLEB(body, 0)
	// call funcidx 0 (send.send)
	body.WriteByte(0x10)
	writeULEB(body, 0)
	// drop errno (we don't error-handle at this layer; if the send failed,
	// the return_id at offset 68 will be zero and the parent sees no data)
	body.WriteByte(0x1A)
	// i32.const 68 -- offset of return_id (u32) inside the send-out
	// struct at memory offset 64. Struct layout is
	//   { exit_code:u32 @0, return_id:u32 @4, return_codec:u64 @8, return_size:u32 @16 }
	body.WriteByte(0x41)
	writeSLEB(body, 68)
	// i32.load align=2 offset=0 (loads target's return-block id)
	body.WriteByte(0x28)
	writeULEB(body, 2) // align = log2(4) = 2
	writeULEB(body, 0) // offset = 0
	// end
	body.WriteByte(0x0B)

	codeSec := new(bytes.Buffer)
	writeULEB(codeSec, 1) // 1 function body
	writeULEB(codeSec, uint64(body.Len()))
	codeSec.Write(body.Bytes())

	// ---- Data section: target address bytes at memory offset 0.
	dataSec := new(bytes.Buffer)
	writeULEB(dataSec, 1) // 1 data segment
	writeULEB(dataSec, 0) // memory index 0
	// Offset expr: i32.const 0 + end
	dataSec.WriteByte(0x41)
	writeSLEB(dataSec, 0)
	dataSec.WriteByte(0x0B)
	writeULEB(dataSec, uint64(len(target)))
	dataSec.Write(target)

	// ---- Assemble the module.
	out := new(bytes.Buffer)
	out.Write([]byte{0x00, 0x61, 0x73, 0x6D}) // magic
	out.Write([]byte{0x01, 0x00, 0x00, 0x00}) // version
	writeSection(out, 1, typeSec.Bytes())
	writeSection(out, 2, impSec.Bytes())
	writeSection(out, 3, fnSec.Bytes())
	writeSection(out, 5, memSec.Bytes())
	writeSection(out, 7, expSec.Bytes())
	writeSection(out, 10, codeSec.Bytes())
	writeSection(out, 11, dataSec.Bytes())
	return out.Bytes()
}

// writeSection prepends a section id + LEB128 length header to payload.
func writeSection(out *bytes.Buffer, id byte, payload []byte) {
	out.WriteByte(id)
	writeULEB(out, uint64(len(payload)))
	out.Write(payload)
}

// writeName writes a WASM name: LEB128 length + raw bytes.
func writeName(out *bytes.Buffer, s string) {
	writeULEB(out, uint64(len(s)))
	out.WriteString(s)
}

// writeULEB writes an unsigned LEB128.
func writeULEB(out *bytes.Buffer, v uint64) {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, v)
	out.Write(buf[:n])
}

// writeSLEB writes a WASM-format signed LEB128 (NOT Go's zigzag Varint).
// The loop stops when the remaining value is 0/-1 AND the byte's sign
// bit (bit 6) matches the sign of the source.
func writeSLEB(out *bytes.Buffer, v int64) {
	for {
		b := byte(v & 0x7F)
		v >>= 7 // arithmetic shift preserves sign in Go for int64
		signBitSet := b&0x40 != 0
		if (v == 0 && !signBitSet) || (v == -1 && signBitSet) {
			out.WriteByte(b)
			return
		}
		out.WriteByte(b | 0x80)
	}
}
