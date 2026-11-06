package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// YouverifyProviderName is the registered name of YouverifyProvider.
// cmd/server passes it as the KYB provider_name when YOUVERIFY_ENABLED is
// set (and SANDBOX_MODE is not).
const YouverifyProviderName = "youverify"

// YouverifyProvider runs the NGN KYB checks against Youverify's Quick
// Check API (doc.youverify.co, spec: /api-reference/specs/quickcheck.json):
//
//   - CAC:  POST /v2/api/verifications/ng/company/basic  (registration number)
//   - TIN:  POST /v2/api/verifications/ng/tin            (business TIN)
//   - BVN:  POST /v2/api/identity/ng/bvn                 (director BVN)
//
// It never decides on its own: every case lands in the manual hold queue
// with the findings in DecisionReason, so an admin makes the final call.
// A pass, a failure (record not found, company not active, TIN registered
// to a different company), a name mismatch and Youverify being unreachable
// or refusing the call (bad key, no balance, rate limit) differ only in
// the summary line the admin sees.
type YouverifyProvider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewYouverifyProvider builds the provider. baseURL is
// https://api.youverify.co (live, billed) or
// https://api.sandbox.youverify.co (test data).
func NewYouverifyProvider(baseURL, apiKey string) *YouverifyProvider {
	return &YouverifyProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *YouverifyProvider) Name() string { return YouverifyProviderName }

func (p *YouverifyProvider) Screen(ctx context.Context, c Case) (Decision, error) {
	if c.CaseType != CaseTypeKYB {
		// Never bound to a corridor for transaction screening; hold rather
		// than guess if it ever is.
		return Decision{Hold: true, Reason: "youverify: transaction screening is not supported, review manually"}, nil
	}

	var in struct {
		RC           string `json:"cac_registration_number"`
		TIN          string `json:"tin"`
		DirectorName string `json:"director_name"`
		DirectorBVN  string `json:"director_bvn"`
	}
	if err := json.Unmarshal(c.SubmittedData, &in); err != nil {
		return Decision{}, fmt.Errorf("youverify: read submission: %w", err)
	}
	rc := normalizeRC(in.RC)

	var (
		wg                sync.WaitGroup
		company, tin, bvn checkResult
	)
	wg.Add(3)
	go func() { defer wg.Done(); company = p.checkCompany(ctx, rc) }()
	go func() { defer wg.Done(); tin = p.checkTIN(ctx, strings.TrimSpace(in.TIN), rc) }()
	go func() { defer wg.Done(); bvn = p.checkBVN(ctx, strings.TrimSpace(in.DirectorBVN), in.DirectorName) }()
	wg.Wait()

	results := []checkResult{company, tin, bvn}
	notes := make([]string, 0, len(results))
	failed, unsure := false, false
	for _, r := range results {
		notes = append(notes, r.note)
		switch r.outcome {
		case outcomeFail:
			failed = true
		case outcomeFlag, outcomeUnavailable:
			unsure = true
		}
	}

	summary := "youverify: all checks passed, confirm to approve"
	switch {
	case failed:
		summary = "youverify: failed, verify manually before approving"
	case unsure:
		summary = "youverify: needs a closer look"
	}

	// Always the manual queue, even on a failure: Youverify's records can
	// be stale or incomplete, so an admin makes every final decision and
	// these findings are what they weigh.
	return Decision{Hold: true, Reason: summary + "\n" + strings.Join(notes, "\n")}, nil
}

type outcome int

const (
	outcomePass outcome = iota
	outcomeFlag
	outcomeFail
	outcomeUnavailable
)

type checkResult struct {
	outcome outcome
	note    string
}

func pass(format string, a ...any) checkResult {
	return checkResult{outcomePass, "✓ " + fmt.Sprintf(format, a...)}
}
func flag(format string, a ...any) checkResult {
	return checkResult{outcomeFlag, "⚠ " + fmt.Sprintf(format, a...)}
}
func fail(format string, a ...any) checkResult {
	return checkResult{outcomeFail, "✗ " + fmt.Sprintf(format, a...)}
}
func unavailable(check string, err error) checkResult {
	return checkResult{outcomeUnavailable, fmt.Sprintf("? %s: could not run (%v), check manually", check, err)}
}

func (p *YouverifyProvider) checkCompany(ctx context.Context, rc string) checkResult {
	var data struct {
		Status             string `json:"status"`
		Name               string `json:"name"`
		RegistrationNumber string `json:"registrationNumber"`
		CompanyStatus      string `json:"companyStatus"`
		TypeOfEntity       string `json:"typeOfEntity"`
		RegistrationDate   string `json:"registrationDate"`
	}
	found, err := p.post(ctx, "/v2/api/verifications/ng/company/basic", map[string]any{
		"registrationNumber": rc,
		"isConsent":          true,
	}, &data)
	if err != nil {
		return unavailable("CAC", err)
	}
	if !found || data.Status != "found" {
		return fail("CAC: no company registered under %s", rc)
	}
	if !strings.EqualFold(data.CompanyStatus, "ACTIVE") {
		return fail("CAC: %s (%s) is %s, not active", data.Name, rc, orUnknown(data.CompanyStatus))
	}
	return pass("CAC: %s, %s, %s, registered %s", data.Name, rc, orUnknown(data.TypeOfEntity), orUnknown(data.RegistrationDate))
}

func (p *YouverifyProvider) checkTIN(ctx context.Context, tin, rc string) checkResult {
	var data struct {
		Status             string `json:"status"`
		Name               string `json:"name"`
		RegistrationNumber string `json:"registrationNumber"`
		TaxOffice          string `json:"taxOffice"`
	}
	// The spec's schema says isSubjectConsent, its example says isConsent.
	found, err := p.post(ctx, "/v2/api/verifications/ng/tin", map[string]any{
		"tin":              tin,
		"isSubjectConsent": true,
		"isConsent":        true,
	}, &data)
	if err != nil {
		return unavailable("TIN", err)
	}
	if !found || data.Status != "found" {
		return fail("TIN: %s is not registered", tin)
	}
	if data.RegistrationNumber != "" && normalizeRC(data.RegistrationNumber) != rc {
		return fail("TIN: %s belongs to %s (%s), not %s", tin, data.Name, data.RegistrationNumber, rc)
	}
	return pass("TIN: registered to %s, tax office %s", data.Name, orUnknown(data.TaxOffice))
}

func (p *YouverifyProvider) checkBVN(ctx context.Context, bvn, directorName string) checkResult {
	var data struct {
		Status     string  `json:"status"`
		FirstName  string  `json:"firstName"`
		MiddleName *string `json:"middleName"`
		LastName   string  `json:"lastName"`
	}
	found, err := p.post(ctx, "/v2/api/identity/ng/bvn", map[string]any{
		"id":               bvn,
		"isSubjectConsent": true,
	}, &data)
	if err != nil {
		return unavailable("BVN", err)
	}
	// Never echo the BVN itself into the case notes.
	if !found || data.Status != "found" {
		return fail("BVN: not found")
	}
	onRecord := strings.Join(nonEmpty(data.FirstName, deref(data.MiddleName), data.LastName), " ")
	if !namesMatch(directorName, data.FirstName, data.LastName) {
		return flag("BVN: found, but the name on record is %q and the director given is %q", onRecord, directorName)
	}
	return pass("BVN: found, name on record %q matches the director", onRecord)
}

// post calls a Quick Check endpoint and decodes its data field into out.
// found is false for a 404 (Youverify's "not found" for some lookups).
// Any other non-2xx, or success=false, is an error.
func (p *YouverifyProvider) post(ctx context.Context, path string, body any, out any) (found bool, err error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("token", p.apiKey)

	res, err := p.client.Do(req)
	if err != nil {
		return false, errors.New("request failed")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return false, errors.New("could not read response")
	}

	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	var envelope struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if res.StatusCode < 200 || res.StatusCode > 299 || !envelope.Success {
		return false, fmt.Errorf("HTTP %d %s", res.StatusCode, envelope.Message)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return false, errors.New("unexpected response shape")
	}
	return true, nil
}

var rcDigits = regexp.MustCompile(`^\d+$`)

// normalizeRC turns "rc 1234567", "RC-1234567" or "1234567" into
// "RC1234567". Other CAC prefixes (BN, IT, LP, LLP) are kept as given.
func normalizeRC(v string) string {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", ".", "").Replace(strings.TrimSpace(v)))
	if rcDigits.MatchString(s) {
		return "RC" + s
	}
	return s
}

// namesMatch is deliberately loose: the director's given name must contain
// both the first and last name on the BVN record, in any order and case.
// Anything stricter trips on middle names and ordering; anything looser is
// what the admin review is for.
func namesMatch(given, first, last string) bool {
	tokens := map[string]bool{}
	for _, t := range strings.Fields(strings.ToLower(given)) {
		tokens[strings.Trim(t, ".,-'")] = true
	}
	f, l := strings.ToLower(strings.TrimSpace(first)), strings.ToLower(strings.TrimSpace(last))
	return f != "" && l != "" && tokens[f] && tokens[l]
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonEmpty(parts ...string) []string {
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}
