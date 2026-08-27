-- Phase 13: admin SSO/OIDC login. password_hash becomes optional — an
-- SSO-only admin may never have a usable password; oidc_subject is the
-- IdP's stable `sub` claim, the correct binding per the OIDC spec (unlike
-- email, which can be reassigned or typo'd at the IdP).
ALTER TABLE admin_users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE admin_users ADD COLUMN oidc_subject TEXT UNIQUE NULL;
