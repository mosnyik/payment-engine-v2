package wallet

import "math/big"

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
