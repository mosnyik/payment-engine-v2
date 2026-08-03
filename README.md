# Payment Rail v2

A crypto-to-fiat payment rail, built as a modular monolith in Go + PostgreSQL, designed for banks and fintechs to plug into as a settlement rail: collect crypto, settle fiat to bank accounts. Every fiat currency, crypto asset, collection partner, settlement partner, and compliance provider is addable via configuration, without a redeploy.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the full design (module map, ledger schema, session state machine, corridor/provider config).

## Stack

Go, PostgreSQL (`pgx`, `golang-migrate`, no ORM), `chi` for HTTP routing, goroutine+ticker background workers.

## Layout

```
cmd/
  server/       the main HTTP app — every module wired together (see cmd/server/router.go)
  ratefetcher/  standalone service: fetches USD/<fiat> from CoinGecko every 20 min, writes to Postgres
  adminctl/     one-off provisioning CLI (create admin accounts, init the self-custody HD wallet seed)
internal/
  ledger/       double-entry source of truth for every value movement
  tenant/       bank/fintech onboarding, KYB, API credentials
  corridor/     asset × network × fiat currency config, provider bindings, limits
  compliance/   KYB/transaction screening, manual hold queue, Travel Rule
  rate/         FX rate aggregation, locking, the persisted "current rate" pipeline
  session/      the orchestrator: corridor lookup → compliance → rate lock → deposit instructions
  treasury/     crypto collection (self-custody HD wallets + partner-custodied adapters)
  settlement/   fiat payout dispatch, retries, reversals
  notification/ webhook + email delivery to tenants and ops
  platform/     shared infra — db, config, gateway (tenant auth), adminauth, eventbus
migrations/     golang-migrate SQL migrations, applied automatically on startup
docs/           architecture and build-order docs
```

## Running it

### Docker Compose (recommended)

Brings up Postgres, the main server, and the rate fetcher as three independent containers:

```
docker compose up
```

- Server listens on `:3700`, health at `/v2/health`.
- Postgres is reachable from the host at `localhost:5433` (not 5432, to avoid clashing with a native install).
- To run just the server (Postgres comes up automatically since it's a dependency):
  ```
  docker compose up server
  ```
- The rate fetcher (`ratefetcher`) is fully independent — it never needs the server running, only Postgres.

### Sandbox environment

A second, fully independent deployment (own database, own ports, own config) with fake compliance/treasury/settlement providers instead of real ones — same codebase, same API shape. See [`docs/SANDBOX_PLAN.md`](docs/SANDBOX_PLAN.md) for the full design.

Bring both stacks up together — one command per file starts every container in it, not one command per container:

```
docker compose -f docker-compose.yml up -d --build
docker compose -f docker-compose.sandbox.yml up -d --build
```

(drop `--build` once the images already reflect your current code — faster startup)

- Sandbox server: `:3701` (main stack stays on `:3700`). Sandbox Postgres: `:5434` (main stack stays on `:5433`).
- One-time: `.env.sandbox` ships with a placeholder `TENANT_SECRET_ENCRYPTION_KEY` — replace it with a real one (`openssl rand -hex 32`), same as `.env.example`'s.
- One-time: provision a sandbox admin account — the same `adminctl` command from [Provisioning](#provisioning) below, but with `DATABASE_URL`/`TENANT_SECRET_ENCRYPTION_KEY` pointed at the sandbox database (`localhost:5434`/`2settle_sandbox`) and `.env.sandbox`'s key, instead of `.env`'s.
- Corridors are seeded automatically at startup from `.env.sandbox`'s `SANDBOX_CORRIDORS` (default `USDT:SANDBOX:NGN`) — no manual corridor/provider-binding setup needed.
- Tear down: `docker compose -f docker-compose.sandbox.yml down` (add `-v` to also wipe the sandbox database).

### Against a database outside Docker

Build and run the server image standalone, pointing at any reachable Postgres:

```
docker build -f cmd/server/Dockerfile -t payment-engine-v2-server .
docker run -p 3700:3700 --env-file .env \
  -e DATABASE_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  payment-engine-v2-server
```

If that Postgres is running on your own machine (not in a container), use `host.docker.internal` instead of `localhost` in `DATABASE_URL` — the container's `localhost` refers to itself.

### Locally, without Docker

```
go run ./cmd/server
```

Requires `DATABASE_URL` and `TENANT_SECRET_ENCRYPTION_KEY` set (see `.env.example`) — migrations apply automatically on startup.

For a live-reloading dev server (rebuilds and restarts on every `.go` change), use [air](https://github.com/air-verse/air) — configured in `.air.toml`, pinned as a Go tool dependency in `go.mod`, no separate install needed:

```
go tool air
```

## Configuration

Copy `.env.example` to `.env` and fill in what you need. Only `DATABASE_URL` and `TENANT_SECRET_ENCRYPTION_KEY` are required; everything else defaults to sensible values or "disabled" until configured — see the comments in `.env.example` for what each section controls (rate providers, treasury/collection adapters, self-custody chains, settlement providers, notifications).

`cmd/ratefetcher` and `cmd/adminctl` are separate binaries with their own minimal env surfaces — see `.env.example`'s `COINGECKO_*` section for the fetcher.

## Provisioning

There's no self-service admin signup. Create the first admin account with:

```
go run ./cmd/adminctl -email=you@example.com
```

(a random password is generated and printed once if `-password` is omitted). To initialize the self-custody HD wallet seed:

```
go run ./cmd/adminctl -init-wallet
```

## API surface

All routes are versioned under `/v2`.

- **Public, unauthenticated**: `GET /v2/health`, `GET /v2/rate/{fiatCurrency}` — the current published FX rate (e.g. `/v2/rate/NGN`).
- **Tenant-authenticated** (API key + HMAC signature): `POST /v2/sessions`, `GET /v2/sessions/{id}`.
- **Admin-authenticated** (`POST /v2/admin/login` to get a token): tenant onboarding/KYB, corridor entitlements, compliance hold review, settlement retry/reversal, notification dead-letter queue, ledger reconciliation — see `cmd/server/router.go` for the full list.
- **Inbound webhooks** (self-verified by signature, not tenant/admin auth): settlement provider callbacks, Busha deposit notifications.

Full OpenAPI 3.0 spec: [`docs/openapi.yaml`](docs/openapi.yaml) — hand-maintained, update it alongside any route change in `cmd/server/router.go`.

With the server running (`APP_ENV` unset or not `production`), browse it live at **http://localhost:3700/docs** (Swagger UI, raw spec at `/docs/openapi.yaml`). This route is skipped entirely in production. Without a running server, view it with:

```
npx @redocly/cli preview-docs docs/openapi.yaml
```

or paste its contents into [editor.swagger.io](https://editor.swagger.io).

## Testing

Tests are integration tests against a real Postgres (no mocks for the database) — set `TEST_DATABASE_URL` (`.env` is loaded automatically; see `.env.example`) and run:

```
go test ./...
```

`TEST_DATABASE_URL` is intentionally separate from `DATABASE_URL` — tests write real rows and don't all clean up after themselves, so pointing them at your dev database lets fixture data pile up there. Point it at any Postgres database (dockerized or native) reachable from your machine, and run migrations against it once (any binary in `cmd/` applies them automatically on startup) before running tests the first time.

Some tests share state across packages against the same test database; if you see flakiness running the full suite in parallel, run serially instead:

```
go test -p 1 ./...
```

## Migrations

Plain SQL files under `migrations/`, applied automatically by every binary (`cmd/server`, `cmd/ratefetcher`, `cmd/adminctl`) on startup via `golang-migrate` — no separate migrate step required.
