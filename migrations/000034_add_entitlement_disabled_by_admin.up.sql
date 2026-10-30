-- Tenant self-service corridor entitlement (POST /v2/portal/corridors/{id}):
-- records whether an entitlement was last switched off by an admin, so a
-- tenant can't undo an admin revocation from the portal. Only the admin
-- path (tenant.SetCorridorEntitlement) writes it — set on revoke, cleared
-- on grant.
ALTER TABLE tenant_corridor_entitlements
    ADD COLUMN disabled_by_admin BOOLEAN NOT NULL DEFAULT false;

-- Until now the admin endpoint was the only way to write an entitlement, so
-- every already-inactive row is an admin revocation and must stay one.
UPDATE tenant_corridor_entitlements SET disabled_by_admin = true WHERE NOT active;
