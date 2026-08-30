-- Phase 9 (security hardening): ISP §3's per-key permission list — scoping
-- a key to a subset of actions, genuinely separate work from the
-- rate-limit-tier/IP-allowlist columns migration 000028 already added.
-- Empty (the default) means unrestricted — same opt-in-restriction
-- convention as allowed_cidrs, so every existing key keeps working
-- unchanged.
ALTER TABLE tenant_api_keys
    ADD COLUMN permissions TEXT[] NOT NULL DEFAULT '{}';
