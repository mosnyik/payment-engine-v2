package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/rate"
)

// rateHandlers implements a public, unauthenticated rate-quote endpoint —
// no gateway HMAC, no admin auth, same unauthenticated tier as /admin/login
// and the inbound webhook routes. Publishes exactly what cmd/ratefetcher
// writes (rate.ProviderCoinGecko in provider_rates) via rate.GetProviderRate
// — a direct read, not run through GetBestQuote's system-rate-ceiling
// selection, since this is an informational published rate, not a real
// reservation a transaction is committed to.
type rateHandlers struct {
	rate *rate.Store
}

type rateResponse struct {
	Rate string `json:"rate"`
}

// GET /rate/{fiatCurrency} — e.g. /rate/NGN.
func (h *rateHandlers) getRate(w http.ResponseWriter, r *http.Request) {
	currency := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "fiatCurrency")))
	if currency == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("rate: fiat currency is required"))
		return
	}

	quote, err := h.rate.GetProviderRate(r.Context(), rate.ProviderCoinGecko, currency)
	if errors.Is(err, rate.ErrNoQuotesAvailable) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("rate: no rate available for %s", currency))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, rateResponse{Rate: formatWithCommas(quote.Rate)})
}

// formatWithCommas renders d as a comma-grouped decimal string —
// "1354.735" -> "1,354.735", "1650.000000" -> "1,650" (trailing zeros
// trimmed; a whole number drops the decimal point entirely). Deliberately
// not a float64 round-trip (this codebase's decimal.Decimal-only rule for
// anything money-shaped, per internal/ledger's package doc comment) — pure
// string manipulation on decimal.Decimal.String()'s exact output instead.
func formatWithCommas(d decimal.Decimal) string {
	s := d.String()
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if hasFrac {
		fracPart = strings.TrimRight(fracPart, "0")
	}

	var grouped strings.Builder
	n := len(intPart)
	for i, c := range intPart {
		if i > 0 && (n-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(c)
	}

	out := grouped.String()
	if fracPart != "" {
		out += "." + fracPart
	}
	if negative {
		out = "-" + out
	}
	return out
}
