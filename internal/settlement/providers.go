package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// DispatchOutcome is the settlement provider's job to classify a
// synchronous dispatch response into (ARCHITECTURE.md §8's retry policy —
// "only [the provider adapter] knows that provider's error semantics").
// Async outcomes (buckets 2/3) arrive later via the webhook, not through
// this type.
type DispatchOutcome string

const (
	// OutcomeAccepted means the provider took custody of the payout request
	// and will report the real result asynchronously via webhook.
	OutcomeAccepted DispatchOutcome = "accepted"
	// OutcomeRejectedRetryable is bucket 1: network error, dispatch-call
	// timeout, or an explicit synchronous rejection — the provider never
	// took custody of the request.
	OutcomeRejectedRetryable DispatchOutcome = "rejected_retryable"
	// OutcomeRejectedTerminal is bucket 4: the provider says this will
	// never succeed (e.g. invalid account details).
	OutcomeRejectedTerminal DispatchOutcome = "rejected_terminal"
)

// PayoutRequest is what DispatchWorker hands to a SettlementProvider.
type PayoutRequest struct {
	SessionID             uuid.UUID
	AttemptIdempotencyKey string
	FiatCurrency          string
	Amount                decimal.Decimal // the tenant-payable amount, post-fee
	// Destination is an opaque bank-details payload (account number,
	// routing details, etc.) — no tenant-level payout-destination schema
	// exists yet, so this is supplied per dispatch/retry rather than looked
	// up from tenant config.
	Destination json.RawMessage
}

// PayoutResult is a SettlementProvider's synchronous dispatch response.
type PayoutResult struct {
	Outcome           DispatchOutcome
	ProviderReference string
	FailureReason     string
}

// SettlementProvider is a fiat-payout adapter bound to a corridor via
// corridor.ProviderBinding (provider_type = settlement). Dispatching is
// assumed cheap enough to call inline from DispatchWorker (never from an
// eventbus.Handler — see settlement.go's package doc).
type SettlementProvider interface {
	Name() string
	IsEnabled() bool
	Dispatch(ctx context.Context, req PayoutRequest) (PayoutResult, error)
	// webhookSecret returns the secret this provider's inbound webhook
	// signature is verified against (webhook.go). Unexported: only
	// meaningful within this package, same discipline other
	// implementation-detail interface methods here follow.
	webhookSecret() string
}

// SettlementProviderConfig configures one external settlement adapter.
// Mirrors treasury.CollectionProviderConfig.
type SettlementProviderConfig struct {
	Enabled       bool
	APIURL        string
	APIKey        string
	WebhookSecret string
}

const httpProviderTimeout = 8 * time.Second

// httpSettlementProvider is the shared adapter shape every named provider
// below builds from — same pattern as internal/rate/providers.go's
// httpProvider (one struct + per-provider closures), chosen over
// treasury/providers.go's single-custom-type shape because settlement has
// several named candidate partners (cNGN, Flutterwave, Paystack, Monnify,
// Hydrogen Pay) from the start, not just one.
//
// None of these have a real endpoint/response shape supplied yet — no
// settlement partner is integrated. Each constructor below is a TODO stub,
// disabled by default, exactly like rate's Busha/LiquidRamp/Anchor and
// treasury's Busha adapters.
type httpSettlementProvider struct {
	name         string
	cfg          SettlementProviderConfig
	client       *http.Client
	buildRequest func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error)
	// parseResponse extracts the outcome from a 2xx response body. Non-2xx
	// responses and transport errors are always OutcomeRejectedRetryable —
	// nothing in a TODO-shaped response can currently signal "terminal"
	// (bucket 4); that path stays unreachable until a real response shape
	// is known.
	parseResponse func(body []byte) (PayoutResult, error)
}

func (p *httpSettlementProvider) Name() string          { return p.name }
func (p *httpSettlementProvider) IsEnabled() bool       { return p.cfg.Enabled }
func (p *httpSettlementProvider) webhookSecret() string { return p.cfg.WebhookSecret }

func (p *httpSettlementProvider) Dispatch(ctx context.Context, req PayoutRequest) (PayoutResult, error) {
	httpReq, err := p.buildRequest(p.cfg, req)
	if err != nil {
		return PayoutResult{}, fmt.Errorf("settlement: build %q request: %w", p.name, err)
	}
	httpReq = httpReq.WithContext(ctx)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return PayoutResult{Outcome: OutcomeRejectedRetryable, FailureReason: err.Error()}, nil
	}
	defer resp.Body.Close()

	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return PayoutResult{Outcome: OutcomeRejectedRetryable, FailureReason: err.Error()}, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PayoutResult{
			Outcome:       OutcomeRejectedRetryable,
			FailureReason: fmt.Sprintf("%s returned status %d: %s", p.name, resp.StatusCode, body.String()),
		}, nil
	}

	result, err := p.parseResponse(body.Bytes())
	if err != nil {
		return PayoutResult{}, fmt.Errorf("settlement: parse %q response: %w", p.name, err)
	}
	return result, nil
}

// defaultDispatchBody is the placeholder outbound request shape every
// TODO-stub constructor below shares — field names are a guess, replace
// once a real partner's API is known.
func defaultDispatchBody(req PayoutRequest) ([]byte, error) {
	return json.Marshal(struct {
		Reference   string          `json:"reference"`
		Currency    string          `json:"currency"`
		Amount      decimal.Decimal `json:"amount"`
		Destination json.RawMessage `json:"destination"`
	}{
		Reference:   req.AttemptIdempotencyKey,
		Currency:    req.FiatCurrency,
		Amount:      req.Amount,
		Destination: req.Destination,
	})
}

// defaultParseResponse is the placeholder inbound response shape every
// TODO-stub constructor below shares.
func defaultParseResponse(body []byte) (PayoutResult, error) {
	var parsed struct {
		ReferenceID string `json:"reference_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return PayoutResult{}, err
	}
	if parsed.ReferenceID == "" {
		return PayoutResult{Outcome: OutcomeRejectedRetryable, FailureReason: "response missing reference_id"}, nil
	}
	return PayoutResult{Outcome: OutcomeAccepted, ProviderReference: parsed.ReferenceID}, nil
}

// newCNGNProvider builds the cNGN settlement adapter.
//
// TODO: plug in the real cNGN payout API.
//  1. Set CNGN_SETTLEMENT_ENABLED=true, CNGN_SETTLEMENT_API_URL,
//     CNGN_SETTLEMENT_API_KEY, CNGN_SETTLEMENT_WEBHOOK_SECRET.
//  2. Update buildRequest with the correct path/auth headers/body shape.
//  3. Update parseResponse to extract the real accept/reject signal and
//     reference id from cNGN's actual response.
//  4. Update the webhook payload shape in webhook.go once cNGN's callback
//     scheme is known.
func newCNGNProvider(cfg SettlementProviderConfig) *httpSettlementProvider {
	return &httpSettlementProvider{
		name:   "cngn",
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error) {
			body, err := defaultDispatchBody(req)
			if err != nil {
				return nil, err
			}
			httpReq, err := http.NewRequest(http.MethodPost, cfg.APIURL+"/TODO/payouts", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			httpReq.Header.Set("Content-Type", "application/json")
			return httpReq, nil
		},
		parseResponse: defaultParseResponse,
	}
}

// newFlutterwaveProvider builds the Flutterwave settlement adapter. Same
// TODO status as newCNGNProvider — see FLUTTERWAVE_SETTLEMENT_ENABLED/
// _API_URL/_API_KEY/_WEBHOOK_SECRET.
func newFlutterwaveProvider(cfg SettlementProviderConfig) *httpSettlementProvider {
	return &httpSettlementProvider{
		name:   "flutterwave",
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error) {
			body, err := defaultDispatchBody(req)
			if err != nil {
				return nil, err
			}
			httpReq, err := http.NewRequest(http.MethodPost, cfg.APIURL+"/TODO/transfers", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			httpReq.Header.Set("Content-Type", "application/json")
			return httpReq, nil
		},
		parseResponse: defaultParseResponse,
	}
}

// newPaystackProvider builds the Paystack settlement adapter. Same TODO
// status as newCNGNProvider — see PAYSTACK_SETTLEMENT_ENABLED/_API_URL/
// _API_KEY/_WEBHOOK_SECRET.
func newPaystackProvider(cfg SettlementProviderConfig) *httpSettlementProvider {
	return &httpSettlementProvider{
		name:   "paystack",
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error) {
			body, err := defaultDispatchBody(req)
			if err != nil {
				return nil, err
			}
			httpReq, err := http.NewRequest(http.MethodPost, cfg.APIURL+"/TODO/transfer", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			httpReq.Header.Set("Content-Type", "application/json")
			return httpReq, nil
		},
		parseResponse: defaultParseResponse,
	}
}

// newMonnifyProvider builds the Monnify settlement adapter. Same TODO
// status as newCNGNProvider — see MONNIFY_SETTLEMENT_ENABLED/_API_URL/
// _API_KEY/_WEBHOOK_SECRET.
func newMonnifyProvider(cfg SettlementProviderConfig) *httpSettlementProvider {
	return &httpSettlementProvider{
		name:   "monnify",
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error) {
			body, err := defaultDispatchBody(req)
			if err != nil {
				return nil, err
			}
			httpReq, err := http.NewRequest(http.MethodPost, cfg.APIURL+"/TODO/disbursements/single", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			httpReq.Header.Set("Content-Type", "application/json")
			return httpReq, nil
		},
		parseResponse: defaultParseResponse,
	}
}

// newHydrogenPayProvider builds the Hydrogen Pay settlement adapter. Same
// TODO status as newCNGNProvider — see HYDROGENPAY_SETTLEMENT_ENABLED/
// _API_URL/_API_KEY/_WEBHOOK_SECRET.
func newHydrogenPayProvider(cfg SettlementProviderConfig) *httpSettlementProvider {
	return &httpSettlementProvider{
		name:   "hydrogenpay",
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
		buildRequest: func(cfg SettlementProviderConfig, req PayoutRequest) (*http.Request, error) {
			body, err := defaultDispatchBody(req)
			if err != nil {
				return nil, err
			}
			httpReq, err := http.NewRequest(http.MethodPost, cfg.APIURL+"/TODO/payout", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			httpReq.Header.Set("Content-Type", "application/json")
			return httpReq, nil
		},
		parseResponse: defaultParseResponse,
	}
}
