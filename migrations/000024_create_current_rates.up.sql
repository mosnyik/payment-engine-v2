-- The persisted "best rate" — the one value both LockRate (real
-- transactions) and the public GET /v2/rate/{fiatCurrency} endpoint read.
-- Computed periodically (internal/rate/currentrate.go's CurrentRateJob,
-- every 15 min by default) from rate.GetBestQuote's existing selection
-- (system_rates ceiling vs. every enabled provider's quote, now including
-- CoinGecko via cmd/ratefetcher's provider_rates writes) rather than
-- re-derived live on every caller — one number, one source of truth.
CREATE TABLE current_rates (
    fiat_currency TEXT PRIMARY KEY,
    rate          NUMERIC(20, 6) NOT NULL,
    provider      TEXT NOT NULL,
    computed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
