-- A corridor is the config entity binding a crypto asset+network to a fiat
-- currency, plus the provider bindings, limits, and thresholds for that
-- combination. Adding a new fiat/crypto/corridor is a row here, not a
-- redeploy; adding a genuinely new *provider type* still needs an adapter
-- implementation in code, but wiring it into a corridor is still just data.
CREATE TABLE corridors (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    crypto_asset                    TEXT NOT NULL,   -- e.g. 'USDT'
    crypto_network                  TEXT NOT NULL,   -- e.g. 'TRC20'
    fiat_currency                   TEXT NOT NULL,   -- e.g. 'NGN'
    active                          BOOLEAN NOT NULL DEFAULT true,
    min_amount_fiat                 NUMERIC(36, 18) NULL,
    max_amount_fiat                 NUMERIC(36, 18) NULL,
    -- Travel Rule: cumulative volume per end-user within the rolling window
    -- crossing this threshold requires proof of source of funds. NULL
    -- threshold means Travel Rule doesn't apply to this corridor.
    travel_rule_threshold_fiat      NUMERIC(36, 18) NULL,
    travel_rule_window_seconds      INT NOT NULL DEFAULT 86400,
    -- Corridor-configurable, not a platform constant (ARCHITECTURE.md §8) —
    -- a newer or higher-risk corridor can get a longer compliance review
    -- window without a code change.
    compliance_hold_timeout_seconds INT NOT NULL DEFAULT 86400,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (crypto_asset, crypto_network, fiat_currency)
);

-- Provider bindings per corridor, ordered by priority for failover
-- (ARCHITECTURE.md's settlement retry policy fails over to the next
-- provider in this order before retrying the one that just failed).
-- provider_name is a key into the in-code adapter registry (e.g. 'busha',
-- 'self_custody_hd', 'mongoro') — the row wires an existing adapter into
-- this corridor; it doesn't define new adapter behavior.
CREATE TABLE corridor_providers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    corridor_id   UUID NOT NULL REFERENCES corridors(id),
    provider_type TEXT NOT NULL CHECK (provider_type IN ('collection', 'settlement', 'compliance', 'rate')),
    provider_name TEXT NOT NULL,
    priority      INT NOT NULL DEFAULT 0,
    active        BOOLEAN NOT NULL DEFAULT true,
    config        JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (corridor_id, provider_type, provider_name)
);

CREATE INDEX idx_corridor_providers_lookup
    ON corridor_providers (corridor_id, provider_type, priority)
    WHERE active;
