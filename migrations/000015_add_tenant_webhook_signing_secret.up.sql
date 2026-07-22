-- Signing secret for outbound webhooks this system sends to a tenant
-- (e.g. treasury's tenant-custom-wallet deposit notifications) — a
-- separate secret from the tenant's inbound API HMAC secret
-- (tenant_api_keys.hmac_secret_encrypted), same reasoning treasury's
-- gas-funding wallet uses a separate HD account from deposit addresses:
-- don't share key material across purposes. Nullable — only set once a
-- tenant registers a webhook URL.
ALTER TABLE tenants ADD COLUMN webhook_signing_secret_encrypted TEXT;
