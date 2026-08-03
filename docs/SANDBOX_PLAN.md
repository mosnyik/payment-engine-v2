# Sandbox Environment — Implementation Plan

Status: Phases 1, 2 & 4 done (validated end-to-end against real Docker containers). Phase 3 (domain routing) blocked on a hosting decision. Companion to `ARCHITECTURE.md`/`IMPLEMENTATION_PLAN.md` but describes an *operational* addition (a second, tenant-facing environment), not a new business module in the Phase 0–8 module map.

## Goal

A Stripe-style sandbox: tenants integrate against `sandbox.2settle.io/v2` with fake money and fake settlement before going live on `api.2settle.io/v2`, using the same codebase and API shape as production.

## Chosen approach: separate deployment (not a `live`/`sandbox` mode flag)

Two options were considered:

- **(A) One deployment, mode flag threaded through `tenant`/`session`/`treasury`/`settlement`** — a single API with two key types (Stripe's actual model). Rejected for now: invasive across most modules, and not justified before there are real tenants asking for it.
- **(B) A second, fully independent deployment of the same code** — separate database, separate config, separate subdomain. Cheaper, and the module boundaries already in place (pluggable providers) make faking collection/payout in a separate env straightforward.

**(B) is the chosen approach.** It is not literally zero-code: `treasury.Store.providers` and `settlement.Store.providers` are hardcoded maps built in `New()` (see `internal/treasury/providers.go:47-52`, `internal/settlement/providers.go:59-68`) with no dynamic `Register`, and `SettlementProvider` has an unexported `webhookSecret()` method — so a fake provider must live inside those packages, not bolt on from outside. `compliance.Registry` (`internal/compliance/compliance.go:77-97`) is already pluggable and needs no core changes.

## Phase 1 — Fake providers (code) ✅

- [x] **`internal/platform/config`** — added `SandboxMode bool`, read from `SANDBOX_MODE` (default `false`), same pattern as every other config flag.
- [x] **`internal/compliance/sandbox_provider.go`** — a `ScreeningProvider` (`SandboxProvider`) that always returns `Decision{Approved: true}`. Registered into the `Registry` in `cmd/server/stores.go` only when `cfg.SandboxMode`.
- [x] **`internal/treasury/sandbox_provider.go`** — a `CollectionProvider` (`sandboxCollectionProvider`) returning a fake deposit address/reference. `treasury.New()` registers it into `providers` when `cfg.SandboxMode`. Deposit confirmation resolved as **timer-based auto-confirm** (see below).
- [x] **`internal/settlement/sandbox_provider.go`** — a `SettlementProvider` (`sandboxSettlementProvider`) whose `Dispatch` returns immediate `OutcomeAccepted`. `settlement.New()` registers it when `cfg.SandboxMode`; `DispatchWorker.Run` calls `confirmSandboxDispatches` every tick to settle dispatched sandbox attempts with no real payout API or webhook involved.
- [x] **`cmd/server/handlers.go` (`submitKYB`)** — when `cfg.SandboxMode`, forces `provider_name` to `compliance.SandboxProviderName` regardless of what's submitted.

### Deposit confirmation mechanism: timer-based auto-confirm (decided)

`internal/treasury/sandbox_provider.go`'s `SandboxConfirmJob` (started from `main.go` only when `cfg.SandboxMode`) ticks every 3s and, for any sandbox reservation older than 10s with no deposit recorded yet, computes the crypto amount that exactly covers the owning session's locked fiat amount (joined straight from `sessions`/`rate_locks` by SQL) and confirms it via the same `recordDepositTransition` entrypoint the real Busha webhook uses. No new route — a sandbox tenant's test suite just waits and watches the session progress on its own.

## Phase 2 — Deployment wiring ✅

Revised from the original "no new code" framing: `corridor.UpsertCorridor`/`UpsertProviderBinding` were never actually reachable from an HTTP endpoint (only `POST /admin/tenants/{id}/corridors/{corridorID}` exists, which sets *tenant entitlement* to an already-existing corridor, not corridor/provider-binding creation itself — confirmed by grepping `cmd/server`'s routes and `onboarding_test.go`'s own scoping note). Decided: corridor/provider-binding config is **config-driven, not a manual runbook step** — `cmd/server` upserts it automatically at boot.

- [x] **New database** — `2settle_sandbox`, provisioned by `docker-compose.sandbox.yml`. No new migration files — `migrations/` applies automatically on first boot, same as every environment.
- [x] **`internal/platform/config`** — added `SandboxCorridors string` (`SANDBOX_CORRIDORS`, default `"USDT:SANDBOX:NGN"`, comma-separated `asset:network:fiat` triples).
- [x] **`cmd/server/sandbox.go`** (new) — `seedSandboxCorridors` parses `SandboxCorridors` and upserts one corridor per entry with compliance/collection/settlement provider bindings all pointed at the sandbox fakes (`compliance.SandboxProviderName`/`treasury.SandboxProviderName`/`settlement.SandboxProviderName`, all now exported). Called from `buildStores` (now `ctx`-threaded) only when `cfg.SandboxMode`, before any background job or request can run.
- [x] **`.env.sandbox`** — `SANDBOX_MODE=true`, `SANDBOX_CORRIDORS`, sandbox `DATABASE_URL`, own `TENANT_SECRET_ENCRYPTION_KEY` placeholder, every real-provider `*_ENABLED` flag omitted.
- [x] **`docker-compose.sandbox.yml`** (new) — own `postgres-sandbox` (port `5434`, own volume), `server-sandbox` (`env_file: .env.sandbox`, port `3701:3700`), **and `ratefetcher-sandbox`** — deliberately *not* omitted or special-cased: rates are sourced from the exact same pipeline production uses (real CoinGecko fetch + `rate.CurrentRateJob`), no fixed/hardcoded sandbox rate. Pinned to its own compose project name (`payment-engine-v2-sandbox`) so it never collides with the main dev stack.

### Bugs found and fixed during validation (pre-existing, exposed by sandbox — not introduced by it)

- **`submitKYB`'s auto-approve path never activated the tenant.** `resolveHold` (the hold-queue path) had a "bridge" calling `tenant.ApproveKYB` after an approval; `submitKYB`'s direct-approve path (when a registered `ScreeningProvider` approves outright, skipping the hold queue) did not. Unreachable until now because no `ScreeningProvider` had ever been registered in this codebase before — `SandboxProvider` is the first. Fixed in `cmd/server/handlers.go` with the same bridge.
- **My own Phase 1 gap:** the sandbox deposit confirm job published `treasury.deposit_confirmed` directly, but `session.handleDepositConfirmed`'s CAS only fires from `deposit_detected` (`internal/session/events.go`) — the same two-step sequence a real Busha webhook pair drives. Fixed by having `confirmDueSandboxDeposits` go through `recordDepositTransition(..., "detected", ...)` then `"confirmed", ...)`, same as production would receive.

## Phase 3 — Domain routing (infra, outside the Go app)

Confirmed compatible with Phase 2: chi's router matches paths, not hostnames, so `api.2settle.io` vs. `sandbox.2settle.io` is entirely a DNS + reverse-proxy concern sitting in front of two already-independent deployments — no app code needed for this part.

- [ ] **DNS** — `api.2settle.io` and `sandbox.2settle.io` pointed at wherever each deployment lives.
- [ ] **Reverse proxy / ingress routing by `Host` header** to the matching backend (main server vs. sandbox server).
- [ ] **TLS** — wildcard cert (`*.2settle.io`) or one cert per subdomain.

**Blocked on a hosting decision** — no deployment config exists in this repo yet (no `fly.toml`, no Kubernetes manifests, no nginx/Caddy config). The concrete mechanism differs by target:
- Self-managed VM → nginx or Caddy virtual hosts proxying to two local ports.
- Kubernetes → an `Ingress` with host-based routing rules.
- PaaS (Fly.io / Railway / Render) → each app gets its own domain; mostly a "add custom domain" step per app, no proxy config to write.

## Phase 4 — Validation ✅

- [x] Brought up `docker-compose.sandbox.yml` for real (`docker compose -f docker-compose.sandbox.yml up -d --build`).
- [x] Ran the full sequence manually over real HTTP against the running sandbox server: register tenant → KYB (auto-approved via the forced sandbox provider) → issue API key → set corridor entitlement → create session (HMAC-signed, `internal/platform/gateway.Sign`).
- [x] Confirmed the session reaches `settled` with no real provider ever called: `treasury_deposits` row confirmed by the sandbox job (amount computed from the locked rate, matching `rate.Lock.FiatToCrypto`'s formula), `settlement_attempts` row `succeeded` with `provider_name = 'sandbox'`, `settlements.status = 'settled'`, `sessions.status = 'settled'`.
- [x] `ratefetcher-sandbox` confirmed writing real CoinGecko quotes into the sandbox DB independent of the main stack.
- [ ] Confirm the real `api.2settle.io` deployment's data is untouched — not yet applicable, no production deployment exists yet.

Torn down after validation (`docker compose -f docker-compose.sandbox.yml down`, volume preserved) — bring back up the same way when needed.

## Rough size

Grew somewhat from the original estimate once seeding and validation surfaced real gaps: ~250–300 lines of new/changed Go across `config.go`, 3 `sandbox_provider.go` files, `cmd/server/sandbox.go` (new), `cmd/server/handlers.go` (sandbox force + the `submitKYB` tenant-activation bridge fix), `stores.go`/`main.go` (ctx-threading, job wiring), `dispatch.go`, plus one new compose file and one new env file. Domain/TLS/proxy work (Phase 3) is separate and gated on a hosting decision.
