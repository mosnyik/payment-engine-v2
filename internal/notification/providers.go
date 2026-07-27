package notification

import (
	"context"
	"errors"
)

// EmailProvider is the internal ops-alert email adapter. Send is assumed
// cheap enough to call inline from DispatchWorker (never from an
// eventbus.Handler — see notification.go's package doc).
type EmailProvider interface {
	IsEnabled() bool
	Send(ctx context.Context, to, subject, body string) error
}

// stubEmailProvider is a TODO-stub: no real vendor (SES/SendGrid/etc.) has
// been chosen yet, same "disabled by default, no real endpoint wired in
// yet" status as settlement's five named provider adapters. Unlike those,
// there isn't even a named candidate to shape a request/response TODO
// around, so Send unconditionally errors — DispatchWorker only calls it at
// all when IsEnabled() is true, and nothing sets that until a real
// implementation replaces this one.
type stubEmailProvider struct {
	cfg EmailProviderConfig
}

func newStubEmailProvider(cfg EmailProviderConfig) *stubEmailProvider {
	return &stubEmailProvider{cfg: cfg}
}

func (p *stubEmailProvider) IsEnabled() bool { return p.cfg.Enabled }

func (p *stubEmailProvider) Send(ctx context.Context, to, subject, body string) error {
	return errors.New("notification: email provider not implemented")
}
