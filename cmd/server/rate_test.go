// Acceptance test for the public GET /rate/{fiatCurrency} endpoint
// (rate_handlers.go) — over the actual HTTP router, proving it's reachable
// with no auth header at all (unlike every other route except
// /admin/login and the inbound webhooks), not just that
// rate.Store.GetProviderRate works in isolation (already covered by
// internal/rate's own tests).
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/rate"
)

// newRateTestServer builds a fresh router against the shared dev database,
// same setup every other cmd/server acceptance test uses.
func newRateTestServer(t *testing.T) (*httptest.Server, *appStores, *db.Pool) {
	t.Helper()
	cfg := testConfig(t)
	ctx := context.Background()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)

	stores, err := buildStores(cfg, pool)
	if err != nil {
		t.Fatalf("build stores: %v", err)
	}
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, stores, pool
}

func TestGetRate_PublicEndpointReturnsCoinGeckoRate(t *testing.T) {
	srv, _, pool := newRateTestServer(t)
	client := srv.Client()
	ctx := context.Background()

	// Same "TST"+random-suffix convention session_test.go/settlement_test.go
	// already use — a unique, canonically-uppercase currency per run avoids
	// colliding with data left over from a previous run against the same
	// live dev database.
	fiatCurrency := "TST" + strings.ToUpper(uuid.NewString()[:8])
	err := rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  rate.ProviderCoinGecko,
		Rate:      decimal.RequireFromString("1354.735"),
		FetchedAt: time.Now(),
	}, fiatCurrency)
	if err != nil {
		t.Fatalf("upsert provider rate: %v", err)
	}

	var got struct {
		Rate string `json:"rate"`
	}
	// No token argument at all — doJSON only sets an Authorization header
	// when one is passed, and this call passes none, proving the route
	// really is reachable unauthenticated.
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/v2/rate/"+fiatCurrency, "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got.Rate != "1,354.735" {
		t.Fatalf("expected comma-grouped rate 1,354.735, got %q", got.Rate)
	}
}

func TestGetRate_CurrencyLowercaseIsNormalized(t *testing.T) {
	srv, _, pool := newRateTestServer(t)
	client := srv.Client()
	ctx := context.Background()

	fiatCurrency := "TST" + strings.ToUpper(uuid.NewString()[:8])
	err := rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  rate.ProviderCoinGecko,
		Rate:      decimal.NewFromInt(1650),
		FetchedAt: time.Now(),
	}, fiatCurrency)
	if err != nil {
		t.Fatalf("upsert provider rate: %v", err)
	}

	var got struct {
		Rate string `json:"rate"`
	}
	// lower-cased on the wire — real callers shouldn't have to get casing
	// exactly right (/rate/ngn should work same as /rate/NGN).
	lower := strings.ToLower(fiatCurrency)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/v2/rate/"+lower, "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a lowercase currency path segment, got %d", resp.StatusCode)
	}
	if got.Rate != "1,650" {
		t.Fatalf("expected 1,650 (whole number, no trailing decimal point), got %q", got.Rate)
	}
}

func TestGetRate_UnknownCurrencyReturns404(t *testing.T) {
	srv, _, _ := newRateTestServer(t)
	client := srv.Client()

	unseeded := "TST" + strings.ToUpper(uuid.NewString()[:8])
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/v2/rate/"+unseeded, "", nil, &struct{}{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a currency with no coingecko rate, got %d", resp.StatusCode)
	}
}

func TestGetRate_SystemRateAloneIsNotEnough(t *testing.T) {
	// This endpoint reads specifically the coingecko-published rate
	// (rate.ProviderCoinGecko in provider_rates) — a system_rate alone
	// (ops-configured ceiling, no external provider data) must NOT satisfy
	// it, since that's a different concept read via a different path
	// (rate.GetBestQuote, used by LockRate, not this endpoint).
	srv, stores, _ := newRateTestServer(t)
	client := srv.Client()
	ctx := context.Background()

	fiatCurrency := "TST" + strings.ToUpper(uuid.NewString()[:8])
	if err := stores.rate.SetSystemRate(ctx, fiatCurrency, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	resp := doJSON(t, client, http.MethodGet, srv.URL+"/v2/rate/"+fiatCurrency, "", nil, &struct{}{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 (system rate alone isn't a coingecko rate), got %d", resp.StatusCode)
	}
}
