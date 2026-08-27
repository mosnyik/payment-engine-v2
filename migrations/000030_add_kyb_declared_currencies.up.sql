-- Which fiat currencies/jurisdictions a KYB case is onboarding the tenant
-- for (Phase 10) -- populated only for case_type='kyb'; transaction-
-- screening cases aren't onboarding into a new jurisdiction, so they leave
-- this empty. Used by ScreenTenant to validate submitted_data against
-- jurisdiction_kyb_requirements, and by tenant.GrantCorridorEntitlement to
-- check a corridor's fiat currency was actually covered by an approved case.
ALTER TABLE compliance_cases ADD COLUMN declared_currencies TEXT[] NOT NULL DEFAULT '{}';
