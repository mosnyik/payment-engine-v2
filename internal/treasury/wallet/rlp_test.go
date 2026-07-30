package wallet

import (
	"bytes"
	"math/big"
	"testing"
)

// These assert the encoder against the RLP spec's own defining rules
// (github.com/ethereum/wiki/wiki/RLP): a single byte < 0x80 encodes as
// itself; the empty string is 0x80; short strings are 0x80+len prefixed;
// the empty list is 0xc0.
func TestRLPEncodeBytes_SpecCases(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"empty", nil, []byte{0x80}},
		{"single byte < 0x80", []byte{0x00}, []byte{0x00}},
		{"single byte 0x7f", []byte{0x7f}, []byte{0x7f}},
		{"single byte >= 0x80", []byte{0x80}, []byte{0x81, 0x80}},
		{"short string", []byte("dog"), []byte{0x83, 'd', 'o', 'g'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rlpEncodeBytes(tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("rlpEncodeBytes(%v) = %x, want %x", tc.in, got, tc.want)
			}
		})
	}
}

func TestRLPEncodeUint_SpecCases(t *testing.T) {
	cases := []struct {
		in   uint64
		want []byte
	}{
		{0, []byte{0x80}},
		{15, []byte{0x0f}},
		{127, []byte{0x7f}},
		{128, []byte{0x81, 0x80}},
		{1024, []byte{0x82, 0x04, 0x00}},
	}
	for _, tc := range cases {
		got := rlpEncodeUint(tc.in)
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("rlpEncodeUint(%d) = %x, want %x", tc.in, got, tc.want)
		}
	}
}

func TestRLPEncodeList_EmptyList(t *testing.T) {
	got := rlpEncodeList()
	want := []byte{0xc0}
	if !bytes.Equal(got, want) {
		t.Fatalf("rlpEncodeList() = %x, want %x", got, want)
	}
}

// TestRLPRoundTrip_EVMTxShape exercises the decoder this package's own
// tests use for signature verification, against the exact 12-field shape
// encodeUnsignedEVMTx/BuildAndSignEVMTx produce — decode(encode(x)) == x
// for every field, independent of any external test vector.
func TestRLPRoundTrip_EVMTxShape(t *testing.T) {
	to := [20]byte{0x11, 0x22, 0x33}
	fields := [][]byte{
		rlpEncodeUint(1),                       // chain id
		rlpEncodeUint(42),                      // nonce
		rlpEncodeBigInt(big.NewInt(1_000_000)), // max priority fee
		rlpEncodeBigInt(big.NewInt(2_000_000)), // max fee
		rlpEncodeUint(21000),                   // gas limit
		rlpEncodeBytes(to[:]),                  // destination
		rlpEncodeBigInt(big.NewInt(0)),         // amount
		rlpEncodeBytes([]byte{0xde, 0xad}),     // data
		rlpEncodeList(),                        // empty access list
	}
	encoded := rlpEncodeList(fields...)

	decoded, err := rlpDecodeTopLevelList(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(fields) {
		t.Fatalf("decoded %d fields, want %d", len(decoded), len(fields))
	}

	wantValues := [][]byte{
		{1}, {42}, {0x0f, 0x42, 0x40}, {0x1e, 0x84, 0x80}, {0x52, 0x08}, to[:], nil, {0xde, 0xad}, nil,
	}
	for i, want := range wantValues {
		if !bytes.Equal(decoded[i], want) {
			t.Fatalf("field %d = %x, want %x", i, decoded[i], want)
		}
	}
}
