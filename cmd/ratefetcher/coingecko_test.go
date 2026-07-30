package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestFetchUSDFiatRate_HappyPath(t *testing.T) {
	var gotPath, gotQuery, gotAPIKeyHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKeyHeader = r.Header.Get("x-cg-demo-api-key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tether":{"ngn": 1650.5}}`))
	}))
	defer server.Close()

	c := newCoinGeckoClient(server.URL, "test-key")
	rate, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err != nil {
		t.Fatalf("fetch usd fiat rate: %v", err)
	}
	if !rate.Equal(decimal.NewFromFloat(1650.5)) {
		t.Fatalf("expected rate 1650.5, got %s", rate)
	}
	if gotPath != "/simple/price" {
		t.Fatalf("expected path /simple/price, got %s", gotPath)
	}
	if !strings.Contains(gotQuery, "ids=tether") || !strings.Contains(gotQuery, "vs_currencies=ngn") {
		t.Fatalf("expected query to request tether priced in ngn, got %q", gotQuery)
	}
	if gotAPIKeyHeader != "test-key" {
		t.Fatalf("expected api key header to be forwarded, got %q", gotAPIKeyHeader)
	}
}

func TestFetchUSDFiatRate_NoAPIKey_HeaderOmitted(t *testing.T) {
	var gotHeader string
	sawHeader := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, sawHeader = r.Header.Get("x-cg-demo-api-key"), r.Header.Get("x-cg-demo-api-key") != ""
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tether":{"ngn": 1650.5}}`))
	}))
	defer server.Close()

	c := newCoinGeckoClient(server.URL, "")
	if _, err := c.FetchUSDFiatRate(context.Background(), "NGN"); err != nil {
		t.Fatalf("fetch usd fiat rate: %v", err)
	}
	if sawHeader {
		t.Fatalf("expected no api key header when unconfigured, got %q", gotHeader)
	}
}

func TestFetchUSDFiatRate_MissingCurrencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tether":{}}`))
	}))
	defer server.Close()

	c := newCoinGeckoClient(server.URL, "")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a response missing the requested currency, got nil")
	}
}

func TestFetchUSDFiatRate_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`))
	}))
	defer server.Close()

	c := newCoinGeckoClient(server.URL, "")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a non-200 response, got nil")
	}
}

func TestFetchUSDFiatRate_ZeroOrNegativeRateRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tether":{"ngn": 0}}`))
	}))
	defer server.Close()

	c := newCoinGeckoClient(server.URL, "")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a zero rate, got nil")
	}
}
