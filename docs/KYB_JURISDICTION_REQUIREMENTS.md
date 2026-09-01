# KYB Jurisdiction Requirements

Reference doc for what a business tenant must submit to pass KYB per fiat
currency/jurisdiction, and how that maps into the running system. Companion
to `ARCHITECTURE.md`/`IMPLEMENTATION_PLAN.md` (Phase 10) but this file is
about *what the requirements are*, not the code that enforces them.

**Not a legal opinion.** This is a working draft compiled for engineering/
testing purposes, not a compliance/legal sign-off. Before any of this
becomes binding policy for real tenants, get it reviewed by actual
compliance/legal counsel for the relevant jurisdiction.

## How this connects to the running system

- The enforcement mechanism lives in `jurisdiction_kyb_requirements`
  (migration `000029`) — one row per fiat currency, holding a
  `required_fields` list. See `internal/compliance/jurisdiction.go`.
- **Only flat, top-level string fields can be enforced.** `ScreenTenant`
  checks that each name in `required_fields` exists as a non-empty string
  key in the submitted JSON — no nested objects or arrays (e.g. a list of
  directors) can be validated this way. The full requirement below is
  organized the way a compliance document would be; the *configured*
  subset in the system is necessarily a flattened version of it.
- **Set/update requirements** with the CLI tool built for this —
  `cmd/jurisdictionctl` — not an HTTP endpoint (deliberate, same
  config-driven precedent as corridor setup):
  ```
  go run ./cmd/jurisdictionctl -currency=NGN -jurisdiction="CBN/SEC/NFIU" -fields="..."
  go run ./cmd/jurisdictionctl -list
  ```

## NGN — Nigeria

Regulators: **CBN** (Central Bank of Nigeria, payments), **SEC** (Securities
and Exchange Commission — primary digital-assets regulator since the 2025
Investments and Securities Act), **NFIU** (Nigerian Financial Intelligence
Unit, AML/CFT).

### Full requirement (source of truth)

**Company identity**
- CAC (Corporate Affairs Commission) registration number
- Certificate of Incorporation
- CAC status report — shows current directors/shareholders
- Tax Identification Number (TIN)

**People behind the company**
- Full list of directors and beneficial owners (anyone owning ~5%+ or with
  significant control)
- Each of their BVN (Bank Verification Number) and NIN (National
  Identification Number)
- Each of their government-issued ID (passport, driver's license, or
  voter's card)
- Whether any of them is a PEP (politically exposed person) — needs
  disclosure, not disqualification

**Business substance**
- Proof of business address (utility bill or tenancy agreement)
- Nature of business / what the company actually does
- Expected transaction volume and source of funds — this is what
  regulators actually use for risk-rating a customer, not just a checkbox

**Crypto-specific**
- SCUML certificate — Nigeria requires this for certain non-bank financial
  businesses; whether it applies depends on how your own company is
  licensed, not the tenant, but some counterparties may ask for it
- Travel Rule data (originator/beneficiary info) — this isn't a KYB field,
  it's already a separate mechanism your `corridor` module has
  (`travel_rule_window`), so it doesn't belong in `required_fields`

### Configured in the system (flattened)

The "people behind the company" category can't be enforced as a real list
of directors under the current flat-field check — configured instead as a
single representative director's name + BVN, standing in for "at least one
director's identity is on file." A future improvement would be a proper
multi-director submission shape with its own validation, if this becomes
a real requirement rather than a testing stand-in.

```
go run ./cmd/jurisdictionctl -currency=NGN -jurisdiction="CBN/SEC/NFIU" -fields="cac_registration_number,tin,proof_of_business_address,business_description,director_name,director_bvn,expected_transaction_volume"
```

| Field key | Covers |
|---|---|
| `cac_registration_number` | Company identity |
| `tin` | Company identity |
| `proof_of_business_address` | Business substance |
| `business_description` | Business substance |
| `director_name` | People behind the company (representative director) |
| `director_bvn` | People behind the company (representative director) |
| `expected_transaction_volume` | Business substance |

Not enforced as `required_fields` (out of scope for this mechanism):
Certificate of Incorporation / CAC status report / director ID documents /
PEP disclosure (all document-upload or nested-list items, not flat string
fields — KYB here is currently declarative data, not document upload);
SCUML certificate (own-company licensing, not tenant-side); Travel Rule
data (handled by `corridor.travel_rule_window`, not KYB).

## Adding another currency

1. Identify the country's regulators: central bank, financial intelligence
   unit, and a digital-assets/securities regulator if crypto is in scope.
2. Search each regulator's published KYC/CDD (customer due diligence)
   guideline for businesses — most countries follow a similar FATF-driven
   baseline (registration number, tax ID, proof of address, beneficial
   ownership, source of funds); what differs is mostly the local ID scheme
   (Nigeria: BVN/NIN; Ghana: Ghana Card; Kenya: KRA PIN; etc.).
3. Flatten the checklist into snake_case field keys, same as NGN above.
4. Run `jurisdictionctl` with the new currency code and field list, and add
   a section to this doc so it's on record.
