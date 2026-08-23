package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EmailProvider is any transactional email vendor adapter — the same
// pluggable-adapter shape settlement.SettlementProvider/
// treasury.CollectionProvider already establish, so swapping vendors (or
// adding a second one later) never touches DispatchWorker. Send is assumed
// cheap enough to call inline from DispatchWorker (never from an
// eventbus.Handler — see notification.go's package doc).
type EmailProvider interface {
	Name() string
	IsEnabled() bool
	Send(ctx context.Context, to, subject, body string) error
}

const emailProviderTimeout = 8 * time.Second

// newEmailProvider selects the configured vendor adapter by name — the one
// place adding a second real provider later touches, same "one function
// keyed by provider name" shape rate/providers.go and settlement/
// providers.go use for their own multi-vendor selection. An empty or
// unrecognized Provider falls back to stubEmailProvider, which just reports
// itself disabled — safe by construction, never a startup failure.
// NewEmailProvider is the exported form of newEmailProvider, for other
// packages that need the same configured vendor adapter DispatchWorker
// uses — e.g. internal/tenant's magic-link sender (tenant.EmailSender is a
// structural subset of this interface) — so a second HTTP email-vendor
// integration is never built.
func NewEmailProvider(cfg EmailProviderConfig) EmailProvider {
	return newEmailProvider(cfg)
}

func newEmailProvider(cfg EmailProviderConfig) EmailProvider {
	switch cfg.Provider {
	case "resend":
		return newResendProvider(cfg)
	default:
		return newStubEmailProvider(cfg)
	}
}

// stubEmailProvider is the zero-config fallback — no Provider name set (or
// an unrecognized one). Always reports disabled, so DispatchWorker never
// calls Send on it (see processDelivery's IsEnabled check) — Send existing
// at all is just to satisfy the interface.
type stubEmailProvider struct {
	cfg EmailProviderConfig
}

func newStubEmailProvider(cfg EmailProviderConfig) *stubEmailProvider {
	return &stubEmailProvider{cfg: cfg}
}

func (p *stubEmailProvider) Name() string    { return "stub" }
func (p *stubEmailProvider) IsEnabled() bool { return false }

func (p *stubEmailProvider) Send(ctx context.Context, to, subject, body string) error {
	return errors.New("notification: no email provider configured")
}

// resendProvider sends via Resend's REST API (https://resend.com/docs/api-reference/emails/send-email).
// A live-verified request/response shape, unlike this codebase's other
// TODO-stub adapters (rate/treasury/settlement) — Resend's API is public
// and documented, so there was no unknown shape to leave as a placeholder.
type resendProvider struct {
	cfg    EmailProviderConfig
	apiURL string
	client *http.Client
}

func newResendProvider(cfg EmailProviderConfig) *resendProvider {
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = "https://api.resend.com/emails"
	}
	return &resendProvider{
		cfg:    cfg,
		apiURL: apiURL,
		client: &http.Client{Timeout: emailProviderTimeout},
	}
}

func (p *resendProvider) Name() string    { return "resend" }
func (p *resendProvider) IsEnabled() bool { return p.cfg.Enabled }

type resendSendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// formatFromHeader renders "Name <address>" (RFC 5322 display-name form)
// when a name is configured, so an inbox shows "2Settle" instead of the
// bare "noreply@..." address — falls back to the bare address, unchanged,
// when no name is set.
func formatFromHeader(name, address string) string {
	if name == "" {
		return address
	}
	return fmt.Sprintf("%s <%s>", name, address)
}

// resendErrorResponse is Resend's documented error shape on a non-2xx
// response — surfaced in the returned error so a dead-lettered delivery's
// last_error is actually actionable, not just "status 4xx".
type resendErrorResponse struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (p *resendProvider) Send(ctx context.Context, to, subject, body string) error {
	reqBody, err := json.Marshal(resendSendRequest{
		From:    formatFromHeader(p.cfg.FromName, p.cfg.FromAddress),
		To:      []string{to},
		Subject: subject,
		Text:    body,
	})
	if err != nil {
		return fmt.Errorf("notification: marshal resend request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("notification: build resend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("notification: resend request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var apiErr resendErrorResponse
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Message != "" {
			return fmt.Errorf("notification: resend: %s: %s", apiErr.Name, apiErr.Message)
		}
		return fmt.Errorf("notification: resend returned status %d", resp.StatusCode)
	}
	return nil
}
