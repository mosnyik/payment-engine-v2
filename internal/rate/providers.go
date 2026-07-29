package rate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

// maxStaleAge rejects a cached provider rate older than this — the fetch
// job has either stopped running or that provider has been failing. 25
// minutes, not 5: cmd/ratefetcher (readOnlyProvider/CoinGecko, below)
// writes on a 20-minute cadence, so this must comfortably exceed that or a
// perfectly healthy CoinGecko row would spend most of its life looking
// stale between two of its own writes. Busha/LiquidRamp/Anchor's
// in-process 30s FetchJob cadence is unaffected either way — they're
// disabled TODO stubs today.
const maxStaleAge = 25 * time.Minute

// Quote is one provider's fiat-per-USD rate.
type Quote struct {
	Provider  string
	Rate      decimal.Decimal
	FetchedAt time.Time
}

// Provider is a rate source safe to call during transaction processing —
// implementations must never make a live external call inline; that's
// LiveFetcher's job, invoked only by the background fetch job (see
// fetchjob.go), so external API latency/downtime never sits on the
// transaction path.
type Provider interface {
	Name() string
	IsEnabled() bool
	FetchRate(ctx context.Context, fiatCurrency string) (Quote, error)
}

// LiveFetcher is implemented by providers whose FetchRate reads a
// background-cached value (provider_rates) — the fetch job calls
// FetchLiveRate on a timer to make the real HTTP call and refresh that
// cache. systemProvider does not implement this: its rate is admin-set,
// not fetched.
type LiveFetcher interface {
	FetchLiveRate(ctx context.Context, fiatCurrency string) (Quote, error)
}

// systemProvider reads the ops-configured ceiling rate. Always enabled —
// external providers can only push the locked rate down from here, never
// up (see ARCHITECTURE.md §7).
type systemProvider struct {
	pool *db.Pool
}

func (p *systemProvider) Name() string    { return "system" }
func (p *systemProvider) IsEnabled() bool { return true }

func (p *systemProvider) FetchRate(ctx context.Context, fiatCurrency string) (Quote, error) {
	var rate decimal.Decimal
	var updatedAt time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT current_rate, updated_at FROM system_rates WHERE fiat_currency = $1`,
		fiatCurrency,
	).Scan(&rate, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, fmt.Errorf("%w: %s", ErrSystemRateNotSet, fiatCurrency)
	}
	if err != nil {
		return Quote{}, fmt.Errorf("rate: fetch system rate: %w", err)
	}
	return Quote{Provider: p.Name(), Rate: rate, FetchedAt: updatedAt}, nil
}

// httpProvider is a generic external rate-provider adapter. Busha,
// LiquidRamp, and Anchor are all ported as TODO stubs, exactly as they
// were in v1 — none of them has a real endpoint/response shape supplied
// yet. Each is disabled by default; setting its *_RATE_ENABLED/
// *_RATE_API_URL/*_RATE_API_KEY env vars and filling in buildRequest/
// parseResponse below is what activates one for real.
type httpProvider struct {
	name          string
	pool          *db.Pool
	cfg           ProviderConfig
	client        *http.Client
	buildRequest  func(cfg ProviderConfig, fiatCurrency string) (*http.Request, error)
	parseResponse func(body []byte) (decimal.Decimal, error)
}

func (p *httpProvider) Name() string    { return p.name }
func (p *httpProvider) IsEnabled() bool { return p.cfg.Enabled }

// FetchRate reads the last rate the fetch job cached in provider_rates —
// it never makes a live call itself, so this provider's latency/downtime
// never sits on the transaction path.
func (p *httpProvider) FetchRate(ctx context.Context, fiatCurrency string) (Quote, error) {
	return fetchCachedProviderRate(ctx, p.pool, p.name, fiatCurrency)
}

// fetchCachedProviderRate reads provider_rates for (name, fiatCurrency),
// rejecting a row older than maxStaleAge — shared by httpProvider (whose
// own FetchLiveRate is what keeps the row warm) and readOnlyProvider (whose
// row is kept warm by an entirely separate process, e.g. cmd/ratefetcher).
func fetchCachedProviderRate(ctx context.Context, pool *db.Pool, name, fiatCurrency string) (Quote, error) {
	var rate decimal.Decimal
	var fetchedAt time.Time
	err := pool.QueryRow(ctx,
		`SELECT rate, fetched_at FROM provider_rates WHERE provider = $1 AND fiat_currency = $2`,
		name, fiatCurrency,
	).Scan(&rate, &fetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, fmt.Errorf("rate: no cached rate for provider %q — nothing has written it yet", name)
	}
	if err != nil {
		return Quote{}, fmt.Errorf("rate: fetch cached rate for %q: %w", name, err)
	}
	if age := time.Since(fetchedAt); age > maxStaleAge {
		return Quote{}, fmt.Errorf("rate: cached rate for %q is stale (%s old)", name, age.Round(time.Second))
	}
	return Quote{Provider: name, Rate: rate, FetchedAt: fetchedAt}, nil
}

// readOnlyProvider surfaces a provider_rates row into GetBestQuote's
// selection without ever attempting its own live fetch — deliberately does
// NOT implement LiveFetcher, unlike httpProvider, so FetchJob (which only
// drives LiveFetcher-implementing providers) never tries to duplicate a
// live HTTP call this provider's data-gathering already happens elsewhere
// for (cmd/ratefetcher, a wholly separate process/container, for
// ProviderCoinGecko). Always enabled: if nothing has written its row yet,
// FetchRate fails the same way any other provider's does, and
// GetAllQuotes already treats a single failing provider as skippable, not
// fatal.
type readOnlyProvider struct {
	name string
	pool *db.Pool
}

func (p *readOnlyProvider) Name() string    { return p.name }
func (p *readOnlyProvider) IsEnabled() bool { return true }

func (p *readOnlyProvider) FetchRate(ctx context.Context, fiatCurrency string) (Quote, error) {
	return fetchCachedProviderRate(ctx, p.pool, p.name, fiatCurrency)
}

// FetchLiveRate makes the actual HTTP call — invoked only by the
// background fetch job, never during transaction processing.
func (p *httpProvider) FetchLiveRate(ctx context.Context, fiatCurrency string) (Quote, error) {
	req, err := p.buildRequest(p.cfg, fiatCurrency)
	if err != nil {
		return Quote{}, fmt.Errorf("rate: build request for %q: %w", p.name, err)
	}
	req = req.WithContext(ctx)

	resp, err := p.client.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("rate: call %q: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Quote{}, fmt.Errorf("rate: read %q response: %w", p.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("rate: %q returned status %d: %s", p.name, resp.StatusCode, body)
	}

	rate, err := p.parseResponse(body)
	if err != nil {
		return Quote{}, fmt.Errorf("rate: parse %q response: %w", p.name, err)
	}
	if rate.LessThanOrEqual(decimal.Zero) {
		return Quote{}, fmt.Errorf("rate: %q returned an invalid rate: %s", p.name, rate)
	}
	return Quote{Provider: p.name, Rate: rate, FetchedAt: time.Now()}, nil
}

const httpProviderTimeout = 8 * time.Second

// newBushaProvider builds the Busha adapter.
//
// TODO: plug in the real Busha endpoint and response shape.
//  1. Set BUSHA_RATE_ENABLED=true, BUSHA_RATE_API_URL, BUSHA_RATE_API_KEY.
//  2. Update buildRequest with the correct path/auth headers.
//  3. Update parseResponse to extract the fiat/USD rate from the real response.
func newBushaProvider(pool *db.Pool, cfg ProviderConfig) *httpProvider {
	return &httpProvider{
		name:   "busha",
		pool:   pool,
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg ProviderConfig, _ string) (*http.Request, error) {
			// TODO: replace with the real Busha rate endpoint.
			req, err := http.NewRequest(http.MethodGet, cfg.APIURL+"/TODO/rates/NGN", nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			req.Header.Set("Accept", "application/json")
			return req, nil
		},
		parseResponse: parseJSONField("rate"),
	}
}

// newLiquidRampProvider builds the LiquidRamp adapter. Same TODO status as
// newBushaProvider — see LIQUIDRAMP_RATE_ENABLED/_API_URL/_API_KEY.
func newLiquidRampProvider(pool *db.Pool, cfg ProviderConfig) *httpProvider {
	return &httpProvider{
		name:   "liquidramp",
		pool:   pool,
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg ProviderConfig, _ string) (*http.Request, error) {
			// TODO: replace with the real LiquidRamp rate endpoint.
			req, err := http.NewRequest(http.MethodGet, cfg.APIURL+"/TODO/rates", nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("X-API-Key", cfg.APIKey)
			req.Header.Set("Accept", "application/json")
			return req, nil
		},
		parseResponse: parseJSONField("ngn_usd"),
	}
}

// newAnchorProvider builds the Anchor adapter. Same TODO status as
// newBushaProvider — see ANCHOR_RATE_ENABLED/_API_URL/_API_KEY.
func newAnchorProvider(pool *db.Pool, cfg ProviderConfig) *httpProvider {
	return &httpProvider{
		name:   "anchor",
		pool:   pool,
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg ProviderConfig, _ string) (*http.Request, error) {
			// TODO: replace with the real Anchor FX rate endpoint.
			req, err := http.NewRequest(http.MethodGet, cfg.APIURL+"/TODO/fx-rates", nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			req.Header.Set("Accept", "application/json")
			return req, nil
		},
		// TODO: real Anchor response nests the rate under data.usd_ngn.
		parseResponse: parseJSONPath("data", "usd_ngn"),
	}
}

// selectBest applies the selection rule: lowest quote among all providers.
// The system rate acts as a ceiling — it only wins when every external
// quote is higher (see ARCHITECTURE.md §7).
func selectBest(quotes []Quote) Quote {
	best := quotes[0]
	for _, q := range quotes[1:] {
		if q.Rate.LessThan(best.Rate) {
			best = q
		}
	}
	return best
}

// GetAllQuotes fetches from every enabled provider concurrently. A failing
// provider is skipped, not fatal — LockRate only needs one usable quote.
func (s *Store) GetAllQuotes(ctx context.Context, fiatCurrency string) []Quote {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		quotes []Quote
	)
	for _, p := range s.providers {
		if !p.IsEnabled() {
			continue
		}
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			q, err := p.FetchRate(ctx, fiatCurrency)
			if err != nil {
				log.Printf("rate: provider %q failed: %v", p.Name(), err)
				return
			}
			mu.Lock()
			quotes = append(quotes, q)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return quotes
}

// GetBestQuote returns the quote selected by the selection rule.
func (s *Store) GetBestQuote(ctx context.Context, fiatCurrency string) (Quote, error) {
	quotes := s.GetAllQuotes(ctx, fiatCurrency)
	if len(quotes) == 0 {
		return Quote{}, ErrNoQuotesAvailable
	}
	return selectBest(quotes), nil
}

// parseJSONField extracts a single top-level numeric field, e.g.
// {"rate": 1650.5}. Placeholder shape for a stub provider's TODO response
// — replace with the real field name once an endpoint is wired in.
func parseJSONField(field string) func([]byte) (decimal.Decimal, error) {
	return func(body []byte) (decimal.Decimal, error) {
		var parsed map[string]decimal.Decimal
		if err := json.Unmarshal(body, &parsed); err != nil {
			return decimal.Decimal{}, fmt.Errorf("unexpected response shape: %s", body)
		}
		rate, ok := parsed[field]
		if !ok {
			return decimal.Decimal{}, fmt.Errorf("missing field %q in response: %s", field, body)
		}
		return rate, nil
	}
}

// parseJSONPath extracts a nested numeric field, e.g. {"data": {"usd_ngn":
// 1648.0}}. Placeholder shape for a stub provider's TODO response.
func parseJSONPath(outer, inner string) func([]byte) (decimal.Decimal, error) {
	return func(body []byte) (decimal.Decimal, error) {
		var parsed map[string]map[string]decimal.Decimal
		if err := json.Unmarshal(body, &parsed); err != nil {
			return decimal.Decimal{}, fmt.Errorf("unexpected response shape: %s", body)
		}
		rate, ok := parsed[outer][inner]
		if !ok {
			return decimal.Decimal{}, fmt.Errorf("missing field %q.%q in response: %s", outer, inner, body)
		}
		return rate, nil
	}
}
