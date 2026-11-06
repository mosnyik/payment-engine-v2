-- The NGN requirement may predate 000039 (set with jurisdictionctl), so it
-- is left in place. ETH and BNB are removed only if no corridor uses them.
DELETE FROM crypto_assets a
WHERE a.code IN ('ETH', 'BNB')
  AND NOT EXISTS (SELECT 1 FROM corridors c WHERE upper(c.crypto_asset) = a.code);
