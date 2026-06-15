// Minimal RLP codec for EIP-1559 Ethereum transactions (lantern#45
// Stage 3). Ported in shape from go-ethereum / Lotus ethtypes RLP, kept
// deliberately narrow: we only need to decode/encode the flat structures
// an EIP-1559 tx uses (byte strings + a single top-level list, with one
// empty nested list for the access list). No structs, no recursion beyond
// one level of nesting, no streaming.
//
// CGO-free, no go-ethereum dependency.
package ethtx

import (
	"bytes"
	"fmt"
)

// EncodeRLP encodes a value that is either []byte or []interface{} (whose
// elements are themselves RLP-encodable) into RLP bytes.
func EncodeRLP(val interface{}) ([]byte, error) {
	switch v := val.(type) {
	case []byte:
		return encodeRLPBytes(v), nil
	case []interface{}:
		var buf bytes.Buffer
		for _, item := range v {
			enc, err := EncodeRLP(item)
			if err != nil {
				return nil, err
			}
			buf.Write(enc)
		}
		return encodeRLPListPrefix(buf.Len(), buf.Bytes()), nil
	default:
		return nil, fmt.Errorf("rlp: unsupported type %T", val)
	}
}

func encodeRLPBytes(data []byte) []byte {
	// Single byte in [0x00, 0x7f] encodes as itself.
	if len(data) == 1 && data[0] <= 0x7f {
		return data
	}
	return append(encodeLength(len(data), 0x80), data...)
}

func encodeRLPListPrefix(length int, payload []byte) []byte {
	return append(encodeLength(length, 0xc0), payload...)
}

// encodeLength encodes a length with the given short-form offset (0x80 for
// strings, 0xc0 for lists), matching the RLP spec.
func encodeLength(length int, offset byte) []byte {
	if length < 56 {
		return []byte{offset + byte(length)}
	}
	lenBytes := bigEndianBytes(uint64(length))
	prefix := offset + 55 + byte(len(lenBytes))
	return append([]byte{prefix}, lenBytes...)
}

func bigEndianBytes(v uint64) []byte {
	if v == 0 {
		return []byte{}
	}
	var out []byte
	for v > 0 {
		out = append([]byte{byte(v & 0xff)}, out...)
		v >>= 8
	}
	return out
}

// DecodeRLP decodes a single RLP item from data. Returns []byte for a
// string item or []interface{} for a list. Errors if there is trailing
// data after the first complete item (callers decode exactly one tx).
func DecodeRLP(data []byte) (interface{}, error) {
	item, rest, err := decodeRLP(data)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("rlp: %d trailing bytes after top-level item", len(rest))
	}
	return item, nil
}

func decodeRLP(data []byte) (item interface{}, rest []byte, err error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("rlp: unexpected end of data")
	}
	b := data[0]
	switch {
	case b <= 0x7f:
		// single byte, itself
		return data[:1], data[1:], nil

	case b <= 0xb7:
		// short string, length b-0x80
		l := int(b - 0x80)
		if len(data) < 1+l {
			return nil, nil, fmt.Errorf("rlp: short string overruns buffer")
		}
		return cloneBytes(data[1 : 1+l]), data[1+l:], nil

	case b <= 0xbf:
		// long string, length-of-length b-0xb7
		ll := int(b - 0xb7)
		if len(data) < 1+ll {
			return nil, nil, fmt.Errorf("rlp: long string length prefix overruns buffer")
		}
		l, err := readLength(data[1 : 1+ll])
		if err != nil {
			return nil, nil, err
		}
		if len(data) < 1+ll+l {
			return nil, nil, fmt.Errorf("rlp: long string overruns buffer")
		}
		return cloneBytes(data[1+ll : 1+ll+l]), data[1+ll+l:], nil

	case b <= 0xf7:
		// short list, payload length b-0xc0
		l := int(b - 0xc0)
		if len(data) < 1+l {
			return nil, nil, fmt.Errorf("rlp: short list overruns buffer")
		}
		items, err := decodeList(data[1 : 1+l])
		if err != nil {
			return nil, nil, err
		}
		return items, data[1+l:], nil

	default:
		// long list, length-of-length b-0xf7
		ll := int(b - 0xf7)
		if len(data) < 1+ll {
			return nil, nil, fmt.Errorf("rlp: long list length prefix overruns buffer")
		}
		l, err := readLength(data[1 : 1+ll])
		if err != nil {
			return nil, nil, err
		}
		if len(data) < 1+ll+l {
			return nil, nil, fmt.Errorf("rlp: long list overruns buffer")
		}
		items, err := decodeList(data[1+ll : 1+ll+l])
		if err != nil {
			return nil, nil, err
		}
		return items, data[1+ll+l:], nil
	}
}

func decodeList(payload []byte) ([]interface{}, error) {
	var out []interface{}
	rest := payload
	for len(rest) > 0 {
		item, r, err := decodeRLP(rest)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		rest = r
	}
	if out == nil {
		out = []interface{}{} // distinguish empty list from nil
	}
	return out, nil
}

func readLength(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("rlp: empty length prefix")
	}
	if b[0] == 0x00 {
		return 0, fmt.Errorf("rlp: length prefix has leading zero")
	}
	var l uint64
	for _, x := range b {
		l = (l << 8) | uint64(x)
	}
	if l > (1 << 31) {
		return 0, fmt.Errorf("rlp: length %d too large", l)
	}
	return int(l), nil
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
