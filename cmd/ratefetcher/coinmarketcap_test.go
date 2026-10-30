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
		gotAPIKeyHeader = r.Header.Get("X-CMC_PRO_API_KEY")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"825":{"id":825,"symbol":"USDT","quote":{"NGN":{"price": 1650.5}}}}}`))
	}))
	defer server.Close()

	c := newCoinMarketCapClient(server.URL, "test-key")
	rate, err := c.FetchUSDFiatRate(context.Background(), "ngn")
	if err != nil {
		t.Fatalf("fetch usd fiat rate: %v", err)
	}
	if !rate.Equal(decimal.NewFromFloat(1650.5)) {
		t.Fatalf("expected rate 1650.5, got %s", rate)
	}
	if gotPath != "/v2/cryptocurrency/quotes/latest" {
		t.Fatalf("expected path /v2/cryptocurrency/quotes/latest, got %s", gotPath)
	}
	if !strings.Contains(gotQuery, "id=825") || !strings.Contains(gotQuery, "convert=NGN") {
		t.Fatalf("expected query to request tether (id 825) converted to NGN, got %q", gotQuery)
	}
	if gotAPIKeyHeader != "test-key" {
		t.Fatalf("expected api key header to be forwarded, got %q", gotAPIKeyHeader)
	}
}

func TestFetchUSDFiatRate_MissingCurrencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"825":{"quote":{}}}}`))
	}))
	defer server.Close()

	c := newCoinMarketCapClient(server.URL, "test-key")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a response missing the requested currency, got nil")
	}
}

func TestFetchUSDFiatRate_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"status":{"error_code":1008,"error_message":"rate limited"}}`))
	}))
	defer server.Close()

	c := newCoinMarketCapClient(server.URL, "test-key")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a non-200 response, got nil")
	}
}

func TestFetchUSDFiatRate_ZeroOrNegativeRateRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"825":{"quote":{"NGN":{"price": 0}}}}}`))
	}))
	defer server.Close()

	c := newCoinMarketCapClient(server.URL, "test-key")
	_, err := c.FetchUSDFiatRate(context.Background(), "NGN")
	if err == nil {
		t.Fatal("expected an error for a zero rate, got nil")
	}
}
