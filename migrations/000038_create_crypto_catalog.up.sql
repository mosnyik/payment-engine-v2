-- Supported crypto catalog: which networks and assets the platform accepts,
-- and which networks each asset is available on. Admin-managed
-- (GET/POST/PATCH/DELETE /v2/admin/crypto/*); POST /v2/admin/corridors
-- only accepts an active asset on an active network it is available on.
--
-- Codes are the identifiers corridors store (corridors.crypto_asset /
-- crypto_network, plain text), so they are immutable once created and
-- always uppercase.
CREATE TABLE crypto_networks (
    code       TEXT PRIMARY KEY CHECK (code ~ '^[A-Z0-9_]{2,20}$'),
    name       TEXT NOT NULL CHECK (length(trim(name)) > 0),
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE crypto_assets (
    code       TEXT PRIMARY KEY CHECK (code ~ '^[A-Z0-9_]{2,20}$'),
    name       TEXT NOT NULL CHECK (length(trim(name)) > 0),
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Deleting an asset or network removes its availability rows with it; the
-- application refuses either delete while a corridor still uses the code.
CREATE TABLE crypto_asset_networks (
    asset_code   TEXT NOT NULL REFERENCES crypto_assets(code) ON DELETE CASCADE,
    network_code TEXT NOT NULL REFERENCES crypto_networks(code) ON DELETE CASCADE,
    PRIMARY KEY (asset_code, network_code)
);

-- What the engine's self-custody wallets can actually handle
-- (internal/treasury/wallet: bitcoin, ethereum, bsc, tron; USDT contracts
-- on the three account chains).
INSERT INTO crypto_networks (code, name) VALUES
    ('TRC20',   'Tron (TRC-20)'),
    ('ERC20',   'Ethereum (ERC-20)'),
    ('BEP20',   'BNB Smart Chain (BEP-20)'),
    ('BITCOIN', 'Bitcoin');

INSERT INTO crypto_assets (code, name) VALUES
    ('USDT', 'Tether USD'),
    ('BTC',  'Bitcoin');

INSERT INTO crypto_asset_networks (asset_code, network_code) VALUES
    ('USDT', 'TRC20'), ('USDT', 'ERC20'), ('USDT', 'BEP20'),
    ('BTC',  'BITCOIN');

-- Backfill whatever existing corridors already use, so every live
-- corridor's asset/network stays valid. Named after their code; an admin
-- can rename them.
INSERT INTO crypto_networks (code, name)
SELECT DISTINCT upper(crypto_network), upper(crypto_network) FROM corridors
WHERE upper(crypto_network) ~ '^[A-Z0-9_]{2,20}$'
ON CONFLICT (code) DO NOTHING;

INSERT INTO crypto_assets (code, name)
SELECT DISTINCT upper(crypto_asset), upper(crypto_asset) FROM corridors
WHERE upper(crypto_asset) ~ '^[A-Z0-9_]{2,20}$'
ON CONFLICT (code) DO NOTHING;

INSERT INTO crypto_asset_networks (asset_code, network_code)
SELECT DISTINCT upper(crypto_asset), upper(crypto_network) FROM corridors
WHERE upper(crypto_asset) ~ '^[A-Z0-9_]{2,20}$' AND upper(crypto_network) ~ '^[A-Z0-9_]{2,20}$'
ON CONFLICT DO NOTHING;
