package gateway_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

type fakeLookup struct {
	secret   string
	tenantID uuid.UUID
	known    bool
}

func (f fakeLookup) LookupHMACSecret(ctx context.Context, apiKey string) (string, uuid.UUID, bool, error) {
	if !f.known {
		return "", uuid.Nil, false, nil
	}
	return f.secret, f.tenantID, true, nil
}

func newSignedRequest(t *testing.T, secret, method, path string, body []byte, ts time.Time) *http.Request {
	t.Helper()
	timestamp := strconv.FormatInt(ts.UnixMilli(), 10)
	sig := gateway.Sign(secret, method, path, timestamp, body)

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-API-Key", "pk_test")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", sig)
	return req
}

func TestHMACMiddleware_ValidRequest(t *testing.T) {
	tenantID := uuid.New()
	lookup := fakeLookup{secret: "s3cret", tenantID: tenantID, known: true}

	var gotTenant uuid.UUID
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, _ = gateway.TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"foo":"bar"}`)
	req := newSignedRequest(t, "s3cret", http.MethodPost, "/v1/sessions", body, time.Now())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotTenant != tenantID {
		t.Fatalf("expected tenant %s in context, got %s", tenantID, gotTenant)
	}
}

func TestHMACMiddleware_MissingHeaders(t *testing.T) {
	lookup := fakeLookup{known: true}
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHMACMiddleware_UnknownAPIKey(t *testing.T) {
	lookup := fakeLookup{known: false}
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	body := []byte(`{}`)
	req := newSignedRequest(t, "irrelevant", http.MethodPost, "/v1/sessions", body, time.Now())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHMACMiddleware_TamperedBody(t *testing.T) {
	tenantID := uuid.New()
	lookup := fakeLookup{secret: "s3cret", tenantID: tenantID, known: true}
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	// Sign one body, send a different one — the signature covers the body
	// hash, so this must fail even though the API key and timestamp are valid.
	signedFor := []byte(`{"amount":100}`)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := gateway.Sign("s3cret", http.MethodPost, "/v1/sessions", timestamp, signedFor)

	tampered := []byte(`{"amount":100000}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(tampered))
	req.Header.Set("X-API-Key", "pk_test")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", sig)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", rec.Code)
	}
}

func TestHMACMiddleware_ExpiredTimestamp(t *testing.T) {
	tenantID := uuid.New()
	lookup := fakeLookup{secret: "s3cret", tenantID: tenantID, known: true}
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	body := []byte(`{}`)
	req := newSignedRequest(t, "s3cret", http.MethodPost, "/v1/sessions", body, time.Now().Add(-10*time.Minute))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired timestamp, got %d", rec.Code)
	}
}

func TestHMACMiddleware_WrongSecret(t *testing.T) {
	tenantID := uuid.New()
	lookup := fakeLookup{secret: "correct-secret", tenantID: tenantID, known: true}
	handler := gateway.HMACMiddleware(lookup, 5*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	body := []byte(`{}`)
	req := newSignedRequest(t, "wrong-secret", http.MethodPost, "/v1/sessions", body, time.Now())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong secret, got %d", rec.Code)
	}
}
