-- Which KYB documentation fields a fiat currency's regulator requires (e.g.
-- NGN -> CBN, KES -> CBK) -- config-driven, same "row not a redeploy"
-- philosophy as corridors.required_destination_fields. No row for a
-- currency means nothing required yet, so this can't break onboarding for
-- currencies nobody has configured.
CREATE TABLE jurisdiction_kyb_requirements (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fiat_currency   TEXT NOT NULL UNIQUE,
    jurisdiction    TEXT NOT NULL,
    required_fields TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
