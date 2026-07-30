package wallet

import (
	"fmt"
	"math/big"
)

// rlpEncodeBytes encodes b as an RLP byte string (spec: a single byte in
// [0x00, 0x7f] encodes as itself; otherwise a length-prefixed string).
func rlpEncodeBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpEncodeLength(len(b), 0x80), b...)
}

// rlpEncodeLength encodes an RLP length prefix: short form (length < 56)
// is a single byte offset+length; long form is offset+55+lenOfLen followed
// by the length itself in minimal big-endian bytes.
func rlpEncodeLength(l int, offset byte) []byte {
	if l < 56 {
		return []byte{offset + byte(l)}
	}
	lenBytes := bigEndianMinimal(uint64(l))
	return append([]byte{offset + 55 + byte(len(lenBytes))}, lenBytes...)
}

// bigEndianMinimal returns v as big-endian bytes with no leading zero byte
// (empty slice for v == 0, per RLP's canonical integer encoding).
func bigEndianMinimal(v uint64) []byte {
	if v == 0 {
		return nil
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte(v & 0xff)}, buf...)
		v >>= 8
	}
	return buf
}

// rlpEncodeList encodes items (each already RLP-encoded) as an RLP list.
func rlpEncodeList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpEncodeLength(len(payload), 0xc0), payload...)
}

// rlpEncodeUint encodes a non-negative integer per RLP's canonical
// encoding (as a minimal big-endian byte string).
func rlpEncodeUint(v uint64) []byte {
	return rlpEncodeBytes(bigEndianMinimal(v))
}

// rlpEncodeBigInt encodes a non-negative big.Int the same way rlpEncodeUint
// does, for values that don't fit in a uint64 (e.g. token amounts, though
// EIP-1559's own fee/value fields are typically small enough for uint64 in
// practice for this system's use).
func rlpEncodeBigInt(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return rlpEncodeBytes(nil)
	}
	return rlpEncodeBytes(v.Bytes())
}

// rlpDecodeTopLevelList decodes data as a single top-level RLP list of flat
// byte strings — exactly the shape encodeUnsignedEVMTx/the signed tx
// envelope produce (the only nested structure either ever contains is the
// empty access_list, which decodes here as a zero-length item). Not a
// general-purpose RLP decoder — only what this package's own round-trip
// tests need to verify the hand-rolled encoder against itself.
func rlpDecodeTopLevelList(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("rlp: empty input")
	}
	prefix := data[0]
	if prefix < 0xc0 {
		return nil, fmt.Errorf("rlp: not a list (prefix 0x%x)", prefix)
	}

	var payload []byte
	switch {
	case prefix <= 0xf7:
		length := int(prefix - 0xc0)
		payload = data[1:]
		if len(payload) != length {
			return nil, fmt.Errorf("rlp: list length mismatch: header says %d, got %d", length, len(payload))
		}
	default:
		lenOfLen := int(prefix - 0xf7)
		if len(data) < 1+lenOfLen {
			return nil, fmt.Errorf("rlp: truncated long-list length")
		}
		length := 0
		for _, b := range data[1 : 1+lenOfLen] {
			length = length<<8 | int(b)
		}
		payload = data[1+lenOfLen:]
		if len(payload) != length {
			return nil, fmt.Errorf("rlp: list length mismatch: header says %d, got %d", length, len(payload))
		}
	}

	var items [][]byte
	for len(payload) > 0 {
		item, rest, err := rlpDecodeOneString(payload)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		payload = rest
	}
	return items, nil
}

// rlpDecodeOneString decodes one RLP byte-string item (including the empty
// list, which this package only ever uses for the empty access_list, so
// it's treated as an empty byte string here) from the front of data,
// returning the remainder.
func rlpDecodeOneString(data []byte) (item []byte, rest []byte, err error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("rlp: unexpected end of input")
	}
	prefix := data[0]
	switch {
	case prefix < 0x80:
		return data[0:1], data[1:], nil
	case prefix == 0xc0: // empty list (this package's empty access_list)
		return nil, data[1:], nil
	case prefix <= 0xb7:
		length := int(prefix - 0x80)
		if len(data) < 1+length {
			return nil, nil, fmt.Errorf("rlp: truncated string")
		}
		return data[1 : 1+length], data[1+length:], nil
	case prefix <= 0xbf:
		lenOfLen := int(prefix - 0xb7)
		if len(data) < 1+lenOfLen {
			return nil, nil, fmt.Errorf("rlp: truncated long-string length")
		}
		length := 0
		for _, b := range data[1 : 1+lenOfLen] {
			length = length<<8 | int(b)
		}
		start := 1 + lenOfLen
		if len(data) < start+length {
			return nil, nil, fmt.Errorf("rlp: truncated long string")
		}
		return data[start : start+length], data[start+length:], nil
	default:
		return nil, nil, fmt.Errorf("rlp: unsupported item prefix 0x%x (nested list)", prefix)
	}
}
