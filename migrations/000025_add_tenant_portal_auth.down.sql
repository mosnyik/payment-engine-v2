DROP TABLE IF EXISTS tenant_sessions;
DROP TABLE IF EXISTS tenant_magic_links;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS contact_email;
