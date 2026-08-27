ALTER TABLE tenant_api_keys
    DROP COLUMN rate_limit_tier,
    DROP COLUMN allowed_cidrs;
