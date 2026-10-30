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

// coinMarketCapTetherID is Tether's CoinMarketCap ID — priced in the target
// fiat currency as a proxy for the USD/<fiat> rate, since CoinMarketCap's
// crypto quote endpoint is what the free plan exposes. Queried by ID, not
// symbol=USDT: symbols aren't unique on CoinMarketCap, IDs are. Tether, not
// USD Coin: internal/rate/assetprice.go already treats USDT as pegged 1:1
// to USD elsewhere in this codebase, so this stays consistent with that
// convention rather than introducing a second stablecoin reference.
const coinMarketCapTetherID = "825"

const coinMarketCapRequestTimeout = 10 * time.Second

type coinMarketCapClient struct {
	apiURL string
	apiKey string
	client *http.Client
}

func newCoinMarketCapClient(apiURL, apiKey string) *coinMarketCapClient {
	return &coinMarketCapClient{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		client: &http.Client{Timeout: coinMarketCapRequestTimeout},
	}
}

// FetchUSDFiatRate returns the USD/fiatCurrency rate — Tether's price in
// fiatCurrency, per coinMarketCapTetherID's doc comment.
func (c *coinMarketCapClient) FetchUSDFiatRate(ctx context.Context, fiatCurrency string) (decimal.Decimal, error) {
	convert := strings.ToUpper(fiatCurrency)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/v2/cryptocurrency/quotes/latest", nil)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: build coinmarketcap request: %w", err)
	}
	q := req.URL.Query()
	q.Set("id", coinMarketCapTetherID)
	q.Set("convert", convert)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CMC_PRO_API_KEY", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: call coinmarketcap: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: read coinmarketcap response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coinmarketcap returned status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Data map[string]struct {
			Quote map[string]struct {
				Price decimal.Decimal `json:"price"`
			} `json:"quote"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: parse coinmarketcap response: %w", err)
	}
	quote, ok := parsed.Data[coinMarketCapTetherID].Quote[convert]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coinmarketcap response missing data.%s.quote.%s: %s", coinMarketCapTetherID, convert, body)
	}
	if quote.Price.LessThanOrEqual(decimal.Zero) {
		return decimal.Decimal{}, fmt.Errorf("ratefetcher: coinmarketcap returned an invalid rate for %s: %s", fiatCurrency, quote.Price)
	}
	return quote.Price, nil
}
