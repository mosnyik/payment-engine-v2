-- Native coins the deposit watcher already handles (internal/treasury
-- watcher.go: ETH on Ethereum, BNB on BSC), alongside the USDT and BTC
-- seeded in 000038. Each sits only on its own chain.
INSERT INTO crypto_assets (code, name) VALUES
    ('ETH', 'Ether'),
    ('BNB', 'BNB')
ON CONFLICT (code) DO NOTHING;

INSERT INTO crypto_asset_networks (asset_code, network_code)
SELECT v.asset, v.network
FROM (VALUES ('ETH', 'ERC20'), ('BNB', 'BEP20')) AS v(asset, network)
WHERE EXISTS (SELECT 1 FROM crypto_networks WHERE code = v.network)
ON CONFLICT DO NOTHING;

-- Payout currencies are the currencies with KYB requirements: a corridor
-- can only pay out in one (POST /v2/admin/corridors checks it). NGN's
-- requirements are the documented ones (docs/KYB_JURISDICTION_REQUIREMENTS.md),
-- added only where no NGN entry exists yet, so a deployment that already
-- set NGN with jurisdictionctl keeps its own.
INSERT INTO jurisdiction_kyb_requirements (fiat_currency, jurisdiction, required_fields)
VALUES ('NGN', 'CBN/SEC/NFIU', ARRAY[
    'cac_registration_number', 'tin', 'proof_of_business_address', 'business_description',
    'director_name', 'director_bvn', 'expected_transaction_volume'
])
ON CONFLICT (fiat_currency) DO NOTHING;
