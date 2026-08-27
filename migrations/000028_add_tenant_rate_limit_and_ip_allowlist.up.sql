-- Phase 9 (security hardening): ISP §3/§6 assume a per-key rate-limit tier
-- and an optional CIDR-aware IP allowlist both exist. rate_limit_tier drives
-- gateway.HMACMiddleware's sliding-window limiter (100/1,000/10,000 req/min
-- by tier — see internal/platform/ratelimit); allowed_cidrs is NULL/empty by
-- default (no restriction), opt-in defense-in-depth per gateway/hmac.go's
-- own doc comment on IP allowlisting.
ALTER TABLE tenant_api_keys
    ADD COLUMN rate_limit_tier TEXT NOT NULL DEFAULT 'standard'
        CHECK (rate_limit_tier IN ('basic', 'standard', 'enterprise')),
    ADD COLUMN allowed_cidrs TEXT[] NULL;
