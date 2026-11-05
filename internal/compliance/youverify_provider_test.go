package compliance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
)

// fakeYouverify answers the three Quick Check endpoints the provider uses.
// Each handler field returns (HTTP status, data object); nil means 404.
type fakeYouverify struct {
	company, tin, bvn func() (int, map[string]any)
	mu                sync.Mutex // the provider runs its three lookups in parallel
	gotToken          string
	gotBodies         map[string]map[string]any
}

func (f *fakeYouverify) server(t *testing.T) *httptest.Server {
	t.Helper()
	f.gotBodies = map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.gotToken = r.Header.Get("token")
		f.gotBodies[r.URL.Path] = body
		f.mu.Unlock()

		var h func() (int, map[string]any)
		switch r.URL.Path {
		case "/v2/api/verifications/ng/company/basic":
			h = f.company
		case "/v2/api/verifications/ng/tin":
			h = f.tin
		case "/v2/api/identity/ng/bvn":
			h = f.bvn
		}
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		code, data := h()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":    code >= 200 && code < 300,
			"statusCode": code,
			"message":    map[bool]string{true: "success", false: "Insufficient fund"}[code < 300],
			"data":       data,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okCompany() (int, map[string]any) {
	return 200, map[string]any{"status": "found", "name": "ACME FINANCE LTD", "registrationNumber": "RC1234567", "companyStatus": "ACTIVE", "typeOfEntity": "PRIVATE COMPANY LIMITED BY SHARES", "registrationDate": "2017-06-09"}
}
func okTIN() (int, map[string]any) {
	return 200, map[string]any{"status": "found", "name": "ACME FINANCE LTD", "registrationNumber": "RC1234567", "tin": "12345678-0001", "taxOffice": "MSTO IKEJA"}
}
func okBVN() (int, map[string]any) {
	return 200, map[string]any{"status": "found", "firstName": "Adaeze", "middleName": nil, "lastName": "Okafor"}
}

func kybCase(t *testing.T, fields map[string]string) compliance.Case {
	t.Helper()
	data := map[string]string{
		"cac_registration_number": "1234567",
		"tin":                     "12345678-0001",
		"director_name":           "Okafor Adaeze",
		"director_bvn":            "22233344455",
	}
	for k, v := range fields {
		data[k] = v
	}
	raw, _ := json.Marshal(data)
	return compliance.Case{CaseType: compliance.CaseTypeKYB, ReferenceType: "tenant", ReferenceID: uuid.New(), SubmittedData: raw}
}

func screen(t *testing.T, f *fakeYouverify, c compliance.Case) compliance.Decision {
	t.Helper()
	srv := f.server(t)
	d, err := compliance.NewYouverifyProvider(srv.URL, "test-key").Screen(context.Background(), c)
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	return d
}

func TestYouverify_AllPassHoldsForAdmin(t *testing.T) {
	f := &fakeYouverify{company: okCompany, tin: okTIN, bvn: okBVN}
	d := screen(t, f, kybCase(t, nil))

	if !d.Hold {
		t.Fatalf("a clean pass must still go to an admin, got %+v", d)
	}
	if !strings.Contains(d.Reason, "all checks passed") {
		t.Fatalf("reason should say all checks passed: %q", d.Reason)
	}
	if f.gotToken != "test-key" {
		t.Fatalf("token header = %q", f.gotToken)
	}
	if got := f.gotBodies["/v2/api/verifications/ng/company/basic"]["registrationNumber"]; got != "RC1234567" {
		t.Fatalf("bare RC digits should be sent prefixed, got %v", got)
	}
	if f.gotBodies["/v2/api/identity/ng/bvn"]["isSubjectConsent"] != true {
		t.Fatal("BVN lookup must send isSubjectConsent=true")
	}
	if strings.Contains(d.Reason, "22233344455") {
		t.Fatal("the BVN must never appear in the case notes")
	}
}

// A failed check falls back to manual review, marked as failed, instead
// of rejecting the business on Youverify's word alone.
func assertFailedToManual(t *testing.T, d compliance.Decision, wantNote string) {
	t.Helper()
	if !d.Hold || d.Approved {
		t.Fatalf("a failed check must go to manual review, got %+v", d)
	}
	if !strings.HasPrefix(d.Reason, "youverify: failed, verify manually") {
		t.Fatalf("summary should flag the failure: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, wantNote) {
		t.Fatalf("reason should contain %q: %q", wantNote, d.Reason)
	}
}

func TestYouverify_CompanyNotFoundFallsBackToManual(t *testing.T) {
	f := &fakeYouverify{company: nil, tin: okTIN, bvn: okBVN}
	assertFailedToManual(t, screen(t, f, kybCase(t, nil)), "no company registered")
}

func TestYouverify_InactiveCompanyFallsBackToManual(t *testing.T) {
	f := &fakeYouverify{company: func() (int, map[string]any) {
		_, d := okCompany()
		d["companyStatus"] = "INACTIVE"
		return 200, d
	}, tin: okTIN, bvn: okBVN}
	assertFailedToManual(t, screen(t, f, kybCase(t, nil)), "not active")
}

func TestYouverify_TINForAnotherCompanyFallsBackToManual(t *testing.T) {
	f := &fakeYouverify{company: okCompany, tin: func() (int, map[string]any) {
		_, d := okTIN()
		d["registrationNumber"] = "RC7654321"
		return 200, d
	}, bvn: okBVN}
	assertFailedToManual(t, screen(t, f, kybCase(t, nil)), "belongs to")
}

// A provider that errors outright must not fail the tenant's submission:
// the case is held for manual review with the error noted.
func TestScreenTenant_ProviderErrorFallsBackToManual(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider-error", err: errors.New("boom")})
	s := compliance.New(pool, reg)

	c, err := s.ScreenTenant(ctx, uuid.New(), json.RawMessage(`{}`), []string{"XTS"}, "test-provider-error")
	if err != nil {
		t.Fatalf("a provider error must not fail the submission: %v", err)
	}
	if c.Status != compliance.StatusHold || c.ResolvedAt != nil {
		t.Fatalf("expected an open hold, got status=%s", c.Status)
	}
	if c.DecisionReason == nil || !strings.Contains(*c.DecisionReason, "verify manually") {
		t.Fatalf("hold should tell the admin to verify manually, got %v", c.DecisionReason)
	}
}

func TestYouverify_BVNNameMismatchFlagsForReview(t *testing.T) {
	f := &fakeYouverify{company: okCompany, tin: okTIN, bvn: okBVN}
	d := screen(t, f, kybCase(t, map[string]string{"director_name": "John Smith"}))
	if !d.Hold {
		t.Fatalf("a name mismatch is for a person to judge, got %+v", d)
	}
	if !strings.Contains(d.Reason, "needs a closer look") {
		t.Fatalf("reason: %q", d.Reason)
	}
}

func TestYouverify_VendorErrorHoldsInsteadOfFailing(t *testing.T) {
	f := &fakeYouverify{
		company: func() (int, map[string]any) { return 402, map[string]any{} },
		tin:     okTIN, bvn: okBVN,
	}
	d := screen(t, f, kybCase(t, nil))
	if !d.Hold {
		t.Fatalf("vendor trouble must not reject the tenant, got %+v", d)
	}
	if !strings.Contains(d.Reason, "check manually") {
		t.Fatalf("reason: %q", d.Reason)
	}
}

// A provider Hold decision lands in the queue with its findings, the
// stored BVN is masked, and resolving keeps the findings under the
// admin's reason.
func TestScreenTenant_ProviderHoldMasksAndKeepsFindings(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider-hold", decision: compliance.Decision{Hold: true, Reason: "youverify: all checks passed"}})
	s := compliance.New(pool, reg)

	tenantID := uuid.New()
	c, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{"director_bvn":"22233344455"}`), []string{"XTS"}, "test-provider-hold")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}
	if c.Status != compliance.StatusHold || c.ResolvedAt != nil {
		t.Fatalf("expected an open hold, got status=%s resolved_at=%v", c.Status, c.ResolvedAt)
	}
	var stored map[string]string
	_ = json.Unmarshal(c.SubmittedData, &stored)
	if stored["director_bvn"] != "•••••••4455" {
		t.Fatalf("stored BVN should be masked, got %q", stored["director_bvn"])
	}

	if err := s.CheckKYBSubmissionAllowed(ctx, tenantID); err != compliance.ErrKYBUnderReview {
		t.Fatalf("second submission while held: got %v, want ErrKYBUnderReview", err)
	}

	resolved, err := s.ResolveHold(ctx, c.ID, mustCreateAdmin(t, pool), true, "looks good")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.DecisionReason == nil || !strings.HasPrefix(*resolved.DecisionReason, "looks good") ||
		!strings.Contains(*resolved.DecisionReason, "all checks passed") {
		t.Fatalf("resolution should keep the findings, got %v", resolved.DecisionReason)
	}
}

func TestCheckKYBSubmissionAllowed_CapsPerDay(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider-reject-cap", decision: compliance.Decision{Approved: false, Reason: "no"}})
	s := compliance.New(pool, reg)

	tenantID := uuid.New()
	for i := 0; i < compliance.MaxKYBSubmissionsPerDay; i++ {
		if err := s.CheckKYBSubmissionAllowed(ctx, tenantID); err != nil {
			t.Fatalf("submission %d should be allowed: %v", i+1, err)
		}
		if _, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{}`), []string{"XTS"}, "test-provider-reject-cap"); err != nil {
			t.Fatalf("screen %d: %v", i+1, err)
		}
	}
	if err := s.CheckKYBSubmissionAllowed(ctx, tenantID); err != compliance.ErrKYBTooManyAttempts {
		t.Fatalf("got %v, want ErrKYBTooManyAttempts", err)
	}
}
