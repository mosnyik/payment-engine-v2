package wallet

import "testing"

func TestChainForNetwork(t *testing.T) {
	cases := map[string]struct {
		want Chain
		ok   bool
	}{
		"tron": {Tron, true}, "TRON": {Tron, true}, "TRC20": {Tron, true}, "trx": {Tron, true},
		"ERC20": {Ethereum, true}, "ethereum": {Ethereum, true}, "ETH": {Ethereum, true},
		"BEP20": {BSC, true}, "bsc": {BSC, true},
		"BITCOIN": {Bitcoin, true}, "btc": {Bitcoin, true},
		" Trc20 ": {Tron, true},
		"SOLANA":  {"solana", false},
	}
	for in, c := range cases {
		got, ok := ChainForNetwork(in)
		if got != c.want || ok != c.ok {
			t.Errorf("ChainForNetwork(%q) = %q, %v; want %q, %v", in, got, ok, c.want, c.ok)
		}
	}
}
