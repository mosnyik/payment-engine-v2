# Busha Business sandbox collection

This integration collects crypto into your Busha Business sandbox balance.
The application creates a same-currency quote (`POST /v1/quotes`), finalizes
it (`POST /v1/transfers`), and stores the returned transfer ID and deposit
instructions. No conversion or payout is requested from Busha.

## Configuration

Add these settings to `.env.sandbox` when using the sandbox Docker Compose
deployment. For `go run ./cmd/server`, set them in the process environment
or the `.env` file instead; the server does not automatically load `.env.sandbox`.
Keep the sandbox's own database and encryption key settings.

```dotenv
SANDBOX_MODE=true
SANDBOX_COLLECTION_PROVIDER=busha
SANDBOX_CORRIDORS=USDT:TRX:NGN
BUSHA_TREASURY_ENABLED=true
BUSHA_TREASURY_API_URL=https://api.sandbox.busha.so
BUSHA_TREASURY_API_KEY=<sandbox secret API key>
BUSHA_TREASURY_WEBHOOK_SECRET=<sandbox webhook signing secret>
```

Use the **secret API key**, not the public payment-request key. The webhook
secret is a separate value obtained when registering the webhook. Do not
commit either secret. The API URL is the origin, without `/v1`.

`USDT:TRX:NGN` is an example corridor; use an asset and network enabled for
your sandbox account. Network names are passed to Busha unchanged. `SANDBOX`
is the app's fake network and cannot be used with Busha.

With these settings, startup seeds Busha as the collection provider and
disables other collection bindings for those corridors. Compliance and fiat
settlement remain the app's simulated sandbox services. The fake deposit
timer only operates on reservations belonging to the `sandbox` provider;
it never confirms Busha deposits. Sandbox mode rejects a live Busha API URL.

## Webhook

In Busha's sandbox dashboard, register a publicly reachable HTTPS endpoint:

```text
https://<your-sandbox-host>/v2/webhooks/treasury/busha
```

Subscribe to `transfer.processing`, `transfer.funds_received`, and
`transfer.completed`. The handler verifies `X-BU-Signature`, a base64-encoded
HMAC-SHA256 of the unmodified request body, before processing the payload.
`transfer.pending` is not treated as a deposit.

Only transfers whose IDs match a stored Busha reservation can update a
deposit. Asset, network and address fields are checked against the reservation.
The transfer ID is the stable deposit deduplication key; the raw provider
payload retains any blockchain hash. Both success events confirm the same
deposit once, including when confirmation arrives before processing.
Unknown transfer IDs return 404; invalid signatures return 401; malformed
relevant events return 400. Irrelevant signed events return 200.

## Exercise the flow

1. Start the sandbox deployment. Startup applies migration 000033, which
   persists the session's deposit instructions.
2. Use the existing sandbox onboarding flow to create a tenant, complete its
   sandbox KYB, grant the corridor entitlement and issue a tenant API key.
   The existing FX pipeline must have a usable NGN rate.
3. Create a session using the tenant's HMAC-authenticated `POST /v2/sessions`
   with `crypto_asset=USDT`, `crypto_network=TRX`, `fiat_currency=NGN` and a
   positive `fiat_amount`.
4. Read `deposit_instructions.address`, `crypto_network`, `amount` and
   `expires_at` from the response. These also persist in `GET /v2/sessions/{id}`.
5. Use Busha's supported sandbox funding procedure for that asset/network.
   Observe the signed webhook and the session progressing to
   `deposit_confirmed`, then the app's simulated settlement.
6. Redeliver the same success event and verify there is still one confirmed
   deposit and one settlement. Also test confirmation without a processing event.

The requested crypto amount comes from this app's locked FX rate, rounded
up to six decimal places for USDT/USDC and eight for other assets. The
displayed deadline is the earliest of the session deadline, rate-lock expiry
and Busha address expiry. Existing fee calculation still occurs at settlement.

## Verification and boundaries

Local HTTP contract tests cover request authentication, quote/transfer
sequencing, 201 responses, mismatched instructions and signature verification.
Database tests cover deposit transitions, replay and confirmation ordering.
An account-level smoke test still requires your sandbox credentials and a
reachable webhook endpoint. No Busha credentials are included in the repository.

This adapter uses your business balance; it does not create Busha customer
profiles or send `X-BU-PROFILE-ID` on behalf of individual customers. Live
settlement recovery issues identified in the app review remain separate work.

## API sources

- [Crypto deposits](https://docs.busha.io/guides/deposits/process-crypto-deposits)
- [Create transfer](https://docs.busha.io/api-reference/transfers/create-transfer)
- [Environments and authentication](https://docs.busha.io/guides/getting-started/make-first-request)
- [Webhook signing](https://docs.busha.io/guides/webhooks/setup)
- [Transfer event payloads](https://docs.busha.io/guides/webhooks/webhook-events)
