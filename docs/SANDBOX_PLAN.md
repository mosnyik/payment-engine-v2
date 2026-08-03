# Sandbox Environment — Implementation Plan

Status: not started. Companion to `ARCHITECTURE.md`/`IMPLEMENTATION_PLAN.md` but describes an *operational* addition (a second, tenant-facing environment), not a new business module in the Phase 0–8 module map.

## Goal

A Stripe-style sandbox: tenants integrate against `sandbox.2settle.io/v2` with fake money and fake settlement before going live on `api.2settle.io/v2`, using the same codebase and API shape as production.

## Chosen approach: separate deployment (not a `live`/`sandbox` mode flag)

Two options were considered:

- **(A) One deployment, mode flag threaded through `tenant`/`session`/`treasury`/`settlement`** — a single API with two key types (Stripe's actual model). Rejected for now: invasive across most modules, and not justified before there are real tenants asking for it.
- **(B) A second, fully independent deployment of the same code** — separate database, separate config, separate subdomain. Cheaper, and the module boundaries already in place (pluggable providers) make faking collection/payout in a separate env straightforward.

**(B) is the chosen approach.** It is not literally zero-code: `treasury.Store.providers` and `settlement.Store.providers` are hardcoded maps built in `New()` (see `internal/treasury/providers.go:47-52`, `internal/settlement/providers.go:59-68`) with no dynamic `Register`, and `SettlementProvider` has an unexported `webhookSecret()` method — so a fake provider must live inside those packages, not bolt on from outside. `compliance.Registry` (`internal/compliance/compliance.go:77-97`) is already pluggable and needs no core changes.

## Phase 1 — Fake providers (code)

- [ ] **`internal/platform/config`** — add `SandboxMode bool`, read from `SANDBOX_MODE` (default `false`), same pattern as every other config flag.
- [ ] **`internal/compliance/sandbox_provider.go`** (new) — a `ScreeningProvider` (`Name() string`, `Screen(ctx, Case) (Decision, error)`) that always returns `Decision{Approved: true}`. Registered into the `Registry` in `cmd/server/stores.go` only when `cfg.SandboxMode`.
- [ ] **`internal/treasury/sandbox_provider.go`** (new) — a `CollectionProvider` (`Name`, `IsEnabled`, `CustodyType`, `ReserveAddress`) returning a deterministic fake deposit address. `treasury.New()` registers it into `providers` when `cfg.SandboxMode`. **Open decision — how deposits get confirmed (see below).**
- [ ] **`internal/settlement/sandbox_provider.go`** (new) — a `SettlementProvider` (`Name`, `IsEnabled`, `Dispatch`, `webhookSecret`) whose `Dispatch` returns immediate success. `settlement.New()` registers it when `cfg.SandboxMode`; `DispatchWorker` settles on its next tick with no real payout API involved.
- [ ] **`cmd/server/handlers.go` (`submitKYB`)** — when `cfg.SandboxMode`, force `provider_name` to the sandbox compliance provider's name regardless of what's submitted, so sandbox tenants don't need to know a magic string to skip the manual hold queue.

### Open decision: deposit confirmation mechanism

- **Timer-based auto-confirm** — a ticker (same shape as `session.TTLJob`/`settlement.DispatchWorker`) flips a sandbox reservation to confirmed N seconds after `ReserveAddress`. Tenants watch the full session lifecycle play out unattended — closest to a real integration test, no extra route.
- **Manual trigger endpoint** — e.g. `POST /v2/sandbox/deposits/{id}/confirm`, letting a tenant's test suite decide exactly when to simulate the deposit — closer to how Stripe's test-mode webhook simulator works, but is a new authenticated route to design and document.

*Decide before starting Phase 1's treasury provider — the two aren't a small variation on each other, they're different code (a background job vs. a new route).*

## Phase 2 — Deployment wiring (config, no new code)

- [ ] **New database** — e.g. `2settle_sandbox`. No new migration files: the existing `migrations/` apply automatically on first boot of any binary, same as every environment today.
- [ ] **`.env.sandbox`** — `SANDBOX_MODE=true`, sandbox `DATABASE_URL`, every real-provider `*_ENABLED` flag omitted (irrelevant once the sandbox fakes take over), sandbox's own `TENANT_SECRET_ENCRYPTION_KEY`.
- [ ] **`docker-compose.sandbox.yml`** (new) — own `postgres` service (new volume, new host port e.g. `5434`), `server` built the same way but `env_file: .env.sandbox` and a distinct host port (e.g. `3701:3700`), so it runs alongside the existing dev stack rather than replacing it. `ratefetcher` can be omitted — the real rate pipeline is read-only and harmless to reuse as-is, or seed one fixed `system_rates` row instead for deterministic sandbox quotes.
- [ ] **Seed corridors + provider bindings in the sandbox DB** pointed at the fake provider names — a normal admin-API runbook step (`POST /admin/tenants/{id}/corridors/{corridorID}` etc.), run once against the sandbox deployment instead of production. No new code.

## Phase 3 — Domain routing (infra, outside the Go app)

Confirmed compatible with Phase 2: chi's router matches paths, not hostnames, so `api.2settle.io` vs. `sandbox.2settle.io` is entirely a DNS + reverse-proxy concern sitting in front of two already-independent deployments — no app code needed for this part.

- [ ] **DNS** — `api.2settle.io` and `sandbox.2settle.io` pointed at wherever each deployment lives.
- [ ] **Reverse proxy / ingress routing by `Host` header** to the matching backend (main server vs. sandbox server).
- [ ] **TLS** — wildcard cert (`*.2settle.io`) or one cert per subdomain.

**Blocked on a hosting decision** — no deployment config exists in this repo yet (no `fly.toml`, no Kubernetes manifests, no nginx/Caddy config). The concrete mechanism differs by target:
- Self-managed VM → nginx or Caddy virtual hosts proxying to two local ports.
- Kubernetes → an `Ingress` with host-based routing rules.
- PaaS (Fly.io / Railway / Render) → each app gets its own domain; mostly a "add custom domain" step per app, no proxy config to write.

## Phase 4 — Validation

- [ ] Bring up `docker-compose.sandbox.yml`.
- [ ] Run the same sequence `cmd/server/onboarding_test.go` already exercises against it manually: register tenant → KYB (auto-approved via the forced sandbox provider) → issue API key → set corridor entitlement → create session.
- [ ] Confirm the session reaches `settled` with no real provider ever called (check logs / DB rows for the sandbox provider names, not the real ones).
- [ ] Confirm the real `api.2settle.io` deployment's data is untouched throughout.

## Rough size

~150–200 lines of new/changed Go across 4 files (`config.go` + 3 new `sandbox_provider.go` files) plus small edits to `treasury.New`/`settlement.New`/`stores.go`/`handlers.go`, one new compose file, one new env file. Domain/TLS/proxy work is separate and gated on a hosting decision.
