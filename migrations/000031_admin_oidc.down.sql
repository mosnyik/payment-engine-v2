ALTER TABLE admin_users DROP COLUMN oidc_subject;
ALTER TABLE admin_users ALTER COLUMN password_hash SET NOT NULL;
