package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewEmailProvider_SelectsByName(t *testing.T) {
	if got := newEmailProvider(EmailProviderConfig{Provider: "resend", Enabled: true}); got.Name() != "resend" {
		t.Fatalf("expected resend provider, got %s", got.Name())
	}
	for _, provider := range []string{"", "unknown-vendor"} {
		got := newEmailProvider(EmailProviderConfig{Provider: provider, Enabled: true})
		if got.Name() != "stub" {
			t.Fatalf("provider %q: expected stub fallback, got %s", provider, got.Name())
		}
		// The stub must never report itself enabled, even if Enabled=true was
		// set for it by mistake — DispatchWorker's dead-letter-immediately
		// path depends on IsEnabled() being the single source of truth.
		if got.IsEnabled() {
			t.Fatalf("provider %q: expected stub to always report disabled", provider)
		}
	}
}

func TestResendProvider_SendsExpectedRequest(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"test-email-id"}`))
	}))
	defer server.Close()

	p := newResendProvider(EmailProviderConfig{
		Enabled: true, Provider: "resend",
		APIURL: server.URL, APIKey: "re_test_key", FromAddress: "ops@sirfi.test",
	})

	if err := p.Send(context.Background(), "alerts@example.com", "settlement.failed", `{"event_type":"settlement.failed"}`); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotAuth != "Bearer re_test_key" {
		t.Fatalf("expected Authorization header 'Bearer re_test_key', got %q", gotAuth)
	}
	var decoded resendSendRequest
	if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.From != "ops@sirfi.test" {
		t.Fatalf("expected from ops@sirfi.test, got %s", decoded.From)
	}
	if len(decoded.To) != 1 || decoded.To[0] != "alerts@example.com" {
		t.Fatalf("expected to [alerts@example.com], got %v", decoded.To)
	}
	if decoded.Subject != "settlement.failed" {
		t.Fatalf("expected subject settlement.failed, got %s", decoded.Subject)
	}
	if !strings.Contains(decoded.Text, "settlement.failed") {
		t.Fatalf("expected body to contain the event payload, got %s", decoded.Text)
	}
}

func TestResendProvider_APIErrorSurfacesMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"name":"validation_error","message":"from address is not verified"}`))
	}))
	defer server.Close()

	p := newResendProvider(EmailProviderConfig{Enabled: true, Provider: "resend", APIURL: server.URL, APIKey: "re_test_key", FromAddress: "ops@sirfi.test"})

	err := p.Send(context.Background(), "alerts@example.com", "subject", "body")
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "from address is not verified") {
		t.Fatalf("expected the resend API error message in the error, got %v", err)
	}
}

func TestResendProvider_DefaultAPIURL(t *testing.T) {
	p := newResendProvider(EmailProviderConfig{Enabled: true, Provider: "resend", APIKey: "k", FromAddress: "ops@sirfi.test"})
	if p.apiURL != "https://api.resend.com/emails" {
		t.Fatalf("expected the real Resend endpoint as the default, got %s", p.apiURL)
	}
}
