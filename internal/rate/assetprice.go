package rate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// assetPriceCacheTTL matches v1 — avoids hitting CoinMarketCap on every
// LockRate call.
const assetPriceCacheTTL = 60 * time.Second

type assetPriceEntry struct {
	price     decimal.Decimal
	expiresAt time.Time
}

type assetPriceCache struct {
	mu      sync.Mutex
	entries map[string]assetPriceEntry
}

func newAssetPriceCache() *assetPriceCache {
	return &assetPriceCache{entries: make(map[string]assetPriceEntry)}
}

func (c *assetPriceCache) get(symbol string) (decimal.Decimal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[symbol]
	if !ok || time.Now().After(e.expiresAt) {
		return decimal.Decimal{}, false
	}
	return e.price, true
}

func (c *assetPriceCache) set(symbol string, price decimal.Decimal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[symbol] = assetPriceEntry{price: price, expiresAt: time.Now().Add(assetPriceCacheTTL)}
}

// GetAssetPrice returns cryptoAsset's USD price. USDT is hardcoded to 1
// (stablecoin, matches v1) — every other asset calls CoinMarketCap.
//
// Known gap, not blocking (see ARCHITECTURE.md §7): unlike the fiat side's
// multi-provider aggregation, this only ever calls CoinMarketCap with no
// fallback. Revisit post-pilot.
func (s *Store) GetAssetPrice(ctx context.Context, cryptoAsset string) (decimal.Decimal, error) {
	if cryptoAsset == "USDT" {
		return decimal.NewFromInt(1), nil
	}

	if cached, ok := s.assetPrices.get(cryptoAsset); ok {
		return cached, nil
	}

	price, err := s.fetchAssetPriceFromCMC(ctx, cryptoAsset)
	if err != nil {
		return decimal.Decimal{}, err
	}
	s.assetPrices.set(cryptoAsset, price)
	return price, nil
}

const coinMarketCapTimeout = 10 * time.Second

func (s *Store) fetchAssetPriceFromCMC(ctx context.Context, cryptoAsset string) (decimal.Decimal, error) {
	if s.cmcAPIKey == "" {
		return decimal.Decimal{}, fmt.Errorf("rate: no CoinMarketCap API key configured — cannot price %s", cryptoAsset)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest", nil)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("rate: build coinmarketcap request: %w", err)
	}
	q := req.URL.Query()
	q.Set("symbol", cryptoAsset)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-CMC_PRO_API_KEY", s.cmcAPIKey)

	client := &http.Client{Timeout: coinMarketCapTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("rate: call coinmarketcap: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("rate: read coinmarketcap response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return decimal.Decimal{}, fmt.Errorf("rate: coinmarketcap returned status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Data map[string]struct {
			Quote struct {
				USD struct {
					Price decimal.Decimal `json:"price"`
				} `json:"USD"`
			} `json:"quote"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return decimal.Decimal{}, fmt.Errorf("rate: parse coinmarketcap response: %w", err)
	}
	asset, ok := parsed.Data[cryptoAsset]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("rate: coinmarketcap response missing %s", cryptoAsset)
	}
	price := asset.Quote.USD.Price
	if price.LessThanOrEqual(decimal.Zero) {
		return decimal.Decimal{}, fmt.Errorf("rate: coinmarketcap returned invalid price for %s: %s", cryptoAsset, price)
	}
	return price, nil
}
