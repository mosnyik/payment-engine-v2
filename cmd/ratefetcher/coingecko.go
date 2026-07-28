package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// coinGeckoStablecoinID is priced in the target fiat currency as a proxy
// for the USD/<fiat> rate — CoinGecko has no direct fiat/fiat endpoint.
// Tether, not USD Coin: internal/rate/assetprice.go already treats USDT as
// pegged 1:1 to USD elsewhere in this codebase, so this stays consistent
// with that convention rather than introducing a second stablecoin
// reference.
const coinGeckoStablecoinID = "tether"

const coinGeckoRequestTimeout = 10 * time.Second

type coinGeckoClient struct {
	apiURL string
	apiKey string
	client *http.Client
}

func newCoinGeckoClient(apiURL, apiKey string) *coinGeckoClient {
	return &coinGeckoClient{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		client: &http.Client{Timeout: coinGeckoRequestTimeout},
	}
}

// FetchUSDFiatRate returns the USD/fiatCurrency rate — Tether's price in
// fiatCurrency, per this file's package doc comment.
func (c *coinGeckoClient) FetchUSDFiatRate(ctx context.Context, fiatCurrency string) (decimal.Decimal, error) {
	vsCurrency := strings.ToLower(fiatCurrency)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/simple/price", nil)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: build coingecko request: %w", err)
	}
	q := req.URL.Query()
	q.Set("ids", coinGeckoStablecoinID)
	q.Set("vs_currencies", vsCurrency)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("x-cg-demo-api-key", c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: call coingecko: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: read coingecko response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coingecko returned status %d: %s", resp.StatusCode, body)
	}

	var parsed map[string]map[string]decimal.Decimal
	if err := json.Unmarshal(body, &parsed); err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: parse coingecko response: %w", err)
	}
	rate, ok := parsed[coinGeckoStablecoinID][vsCurrency]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coingecko response missing %s.%s: %s", coinGeckoStablecoinID, vsCurrency, body)
	}
	if rate.LessThanOrEqual(decimal.Zero) {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coingecko returned an invalid rate for %s: %s", fiatCurrency, rate)
	}
	return rate, nil
}
