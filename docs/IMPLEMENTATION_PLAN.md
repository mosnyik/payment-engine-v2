# Payment Rail v2 — Implementation Plan

Status: living doc, pre-implementation. Companion to `ARCHITECTURE.md` (design decisions, module map, ledger schema, session state machine) — this doc is about *build order and sequencing*, not design. Update as phases complete or sequencing changes.

## How this is organized

Distinct buildable parts, grouped into: the foundation layer everything depends on, the nine business modules from the module map, and cross-cutting ops tooling — ordered by dependency, with one deliberate sequencing call-out (Phase 4).

## Phase 0 — Foundation ✅ complete

Nothing else can start without this. No business functionality yet.

- ✅ **`platform/db`** — Postgres connection pooling (`pgx`), `golang-migrate` setup, base migration tooling. Verified against a live local Postgres (Docker, port 5433 to avoid a conflicting native install).
- ✅ **`platform/gateway`** — tenant HMAC+API key authentication: signature covers method+path+timestamp+body-hash, constant-time compare, 5-min replay window. mTLS deliberately deferred — it needs a tenant record to say which tenants use it, which doesn't exist until Phase 2; the router's shape doesn't change when it's added. Routing via `chi`.
- ✅ **`platform/admin-auth`** — separate credential space for staff: bcrypt-hashed passwords, sessions looked up by token hash (no secret-comparison step at all, sidestepping timing attacks structurally rather than via constant-time compare), login-timing normalized against user enumeration, full audit log table. Direct replacement for v1's single shared `ADMIN_SECRET`.
- ✅ **`platform/eventbus`** — `outbox_events` table, dispatcher claims one row per transaction via `SELECT ... FOR UPDATE SKIP LOCKED` (isolates a failing handler to its own event, not the whole batch), immediate wake-up + poll-interval backstop. Verified: successful dispatch marks `dispatched_at`; a failing handler rolls back cleanly and redelivers next tick.
- ✅ **`platform/idempotency`** — inbound-request idempotency store, scoped per-tenant (`(tenant_id, key)`, not a bare key — two tenants can pick the same client-supplied string safely). Three-outcome model (`Claimed`/`InFlight`/`Completed`) verified against the live DB.

All five have integration tests passing against a live Postgres instance (`go test ./...` from the repo root).

## Phase 1 — Ledger

Build immediately after the foundation, before any money-moving module — even in sandbox/test mode.

- `ledger_accounts`, `ledger_transactions`, `ledger_entries`, `ledger_balances` (see `ARCHITECTURE.md` §6 for schema).
- The single `Post()` write path: enforces balance-per-asset invariant and idempotency-key uniqueness. No other code is allowed to write to `ledger_entries`.

Everything downstream posts through this — it needs to exist and be trustworthy before anything else that moves value.

## Phase 2 — Tenant, Corridor, Compliance (parallelizable)

These three don't depend heavily on each other and can be built concurrently.

- **`tenant`** — profile, fee schedule, corridor entitlements, credential storage.
- **`corridor`** — config entity binding asset+network × fiat currency to provider bindings, limits, Travel Rule thresholds, `compliance_hold_timeout` (default 24h, overridable per corridor).
- **`compliance`** — provider registry, KYB case flow, manual-hold queue. Ships as hold-queue-only initially (no screening vendor selected yet); automated providers slot in later per corridor/currency without touching callers. Compliance cases persist independently of session lifecycle — a session expiring out of `compliance_hold` never closes the underlying case.
- **Onboarding workflow** — not a separate module, but the concrete end-to-end sequence that has to work by the end of this phase (see `ARCHITECTURE.md` §5 for the full breakdown): tenant registration → KYB submission → KYB review (via `platform/admin-auth`) → credential issuance (via `platform/gateway`, nothing issued before KYB clears) → corridor entitlement assignment → webhook config (SSRF-validated). A tenant can't call the API at all until this whole chain works, so treat it as the Phase 2 acceptance test, not an afterthought.

## Phase 3 — Rate engine

Ported from v1 (see `ARCHITECTURE.md` §7): provider adapters, background fetch job, aggregator (system-rate-as-ceiling selection rule), `LockRate()`, plus the new `rate_locks` table (not in v1). Depends only on `corridor` (active-currency list) and the foundation.

## Phase 4 — Treasury *(sequencing call, not strict dependency order)*

The highest-stakes module — self-custody HD wallets, KMS-backed key encryption, per-chain watchers, per-asset sweep policy — is also the most complex and, per the v1 audit, the most consequence-heavy if built wrong.

**Build the partner-custodied adapter (Busha) first.** It's simpler (no HD derivation, no KMS integration, no watcher/sweep policy to get right) and the SLA already exists. This gets a working end-to-end pilot (collection → settlement) running fastest, while self-custody gets the time and scrutiny it deserves without blocking everything else behind it.

1. `treasury` — Busha (partner-custodied) collection provider adapter.
2. `treasury` — self-custody HD wallet manager (KMS-backed key encryption), per-chain deposit watchers, confirmation policy, sweep policy execution (volatile-immediate / stable-batched).

## Phase 5 — Session

The orchestrator: `corridor` → `compliance.ScreenSession()` → `rate.LockRate()` → `treasury.GetDepositInstructions()`, the state machine, and TTL/SLA behavior. Depends on all of Phases 2–4, so naturally last among the "input" modules.

**TTL/SLA design locked** (see `ARCHITECTURE.md` §8): 30 minutes is a hard expiry only pre-deposit (`screening`/`compliance_hold`/`pending`) — nothing real has happened yet, safe to abandon. Once a deposit is detected, 30 minutes becomes an SLA rather than an expiry: `sla_breached_at` is set once and the right people are notified if `settled` isn't reached in time, but `status` keeps reflecting the real pipeline stage and the session is never force-abandoned while real funds are in flight. Nothing left blocking this phase on the TTL front.

## Phase 6 — Settlement

Provider adapter(s) for the launch PSP/aggregator, the ledger-claim-then-dispatch pattern (atomic idempotency-key claim before calling the external provider), signature-verified webhook handling. Depends on `session` (subscribes to `deposit_confirmed`) and `ledger`.

*Note: if a specific settlement partner requires ISO 20022 messaging (flagged as possible for UBA directly, or for PAPSS on pan-African corridors — see conversation), that translation is scoped entirely inside that one adapter and doesn't affect this phase's core design.*

## Phase 7 — Notification

Webhook delivery (signed, retried, dead-lettered) + email, subscribing to session/settlement events. Last — nothing depends on it, it only depends on events existing.

## Phase 8 — Ops/observability *(parallel track, not a hard blocker)*

Can be built incrementally alongside Phases 5–7 rather than as a discrete blocking phase:

- Reconciliation job (ledger balance drift detection against summed entries).
- SLA-breach alerting.
- Hold-queue review surface for compliance analysts.
- Manual settlement-retry tooling.

## Dependency summary

```
Phase 0 (foundation)
   └─▶ Phase 1 (ledger)
          └─▶ Phase 2 (tenant, corridor, compliance — parallel)
                 └─▶ Phase 3 (rate)
                 └─▶ Phase 4 (treasury: Busha first, then self-custody)
                        └─▶ Phase 5 (session)
                               └─▶ Phase 6 (settlement)
                                      └─▶ Phase 7 (notification)

Phase 8 (ops/observability) — parallel to 5–7, not blocking
```

## Open items carried over from ARCHITECTURE.md

- ~~Session TTL/SLA mechanics past deposit-detection~~ — resolved (§8): hard expiry pre-deposit, SLA-breach flag + notification post-deposit, status never overwritten.
- ~~Full `reversed`-state handling~~ — resolved (§8): ops-triggered re-settle with corrected bank details (bounded retry, same cap as `settlement_failed`) or manual closure via `reversal_resolved`; crypto side is never unwound, only the fiat leg is compensated.
- ~~Settlement retry policy specifics~~ — resolved (§8): 4-bucket failure classification; buckets 1/2 (rejected-before-acceptance, explicit async failure) auto-retry/failover up to 3 attempts across the corridor's providers, ~2 min total window; bucket 3 (confirmation timeout, 10 min) never auto-retries — risks a real double bank transfer, escalates to ops for manual verification instead; bucket 4 (terminal rejection) never auto-retries. Shared by `settlement_failed` and `reversed`.
- ~~Compliance hold timeout duration~~ — resolved (§8): corridor-configurable (`compliance_hold_timeout` on `corridor`), defaults to 24h if unset. Hold cases are queued to ops immediately on creation, not deferred to the timeout; the compliance case persists independently of the session's expiry.

All open items from the original design pass are resolved.

---
*Next: no open design blockers remain across Phases 0–8. Ready to execute from Phase 0, or continue detailing a specific module's internals (e.g. the corridor config schema) before writing code.*
