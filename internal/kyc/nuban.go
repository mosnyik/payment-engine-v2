package kyc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// NubanProviderName is the provider name stored on each kyc_checks row.
const NubanProviderName = "nuban"

// NubanProvider looks identities up through Nuban's KYC data API
// (https://api-kycdata.nuban.com.ng, docs at
// https://nuban-kyc-prod-ui.pages.dev/api-doc). Every call is billed
// (BVN ₦150, NIN ₦200, phone ₦120, driver's licence ₦300 at the time of
// writing) and authenticated with "Authorization: Bearer <key>".
//
// This is a separate product from the account-name lookup v1 uses
// (app.nuban.com.ng/api/<key>), and may need its own key.
type NubanProvider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewNubanProvider(baseURL, apiKey string) *NubanProvider {
	return &NubanProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *NubanProvider) Name() string { return NubanProviderName }

// nubanData is the union of the four endpoints' "data" objects. The phone
// endpoint uses camelCase (firstName) where the others use lowercase
// (firstname); encoding/json matches keys case-insensitively, so FirstName
// below picks up both.
type nubanData struct {
	FirstName  string `json:"firstname"`
	LastName   string `json:"lastname"`
	Surname    string `json:"surname"`
	MiddleName string `json:"middlename"`
	DOB        string `json:"dob"`
	Birthdate  string `json:"birthdate"`
	Billing    struct {
		Currency        string          `json:"currency"`
		VerificationFee json.RawMessage `json:"verification_fee"`
	} `json:"billing"`
}

func (p *NubanProvider) Lookup(ctx context.Context, l Lookup) (Record, error) {
	var path string
	q := url.Values{}
	switch l.IDType {
	case IDTypeBVN:
		path = "/api/bvn-data"
		q.Set("bvn_no", l.IDNumber)
	case IDTypeNIN:
		path = "/api/nin-data"
		q.Set("nin_no", l.IDNumber)
	case IDTypePhone:
		path = "/api/phone-data"
		q.Set("phone_no", l.IDNumber)
	case IDTypeDriversLicense:
		path = "/api/driver-license-data"
		q.Set("license_no", l.IDNumber)
		q.Set("first_name", l.FirstName)
		q.Set("last_name", l.LastName)
	default:
		return Record{}, fmt.Errorf("nuban: unsupported id type %q", l.IDType)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return Record{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := p.client.Do(req)
	if err != nil {
		// Never include the URL: it carries the ID number.
		return Record{}, errors.New("nuban: request failed")
	}
	defer res.Body.Close()
	// NIN and licence responses carry a base64 photo; cap well above that.
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Record{}, errors.New("nuban: could not read response")
	}

	var envelope struct {
		Status  json.RawMessage `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &envelope)

	switch {
	case res.StatusCode == http.StatusNotFound:
		return Record{}, ErrRecordNotFound
	case res.StatusCode == http.StatusBadRequest:
		// Our own validation runs first, so a 400 here means Nuban
		// rejected the number itself — treat as no such record.
		return Record{}, ErrRecordNotFound
	case res.StatusCode < 200 || res.StatusCode > 299:
		// 401 bad key, 429 rate limited, 500 out of wallet credit (per
		// Nuban's docs), anything else. Only the status and Nuban's own
		// message are kept — never the body, which may hold personal data.
		return Record{}, fmt.Errorf("nuban: HTTP %d %s", res.StatusCode, truncate(envelope.Message, 120))
	}

	if !statusOK(envelope.Status) || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		if looksNotFound(envelope.Message) {
			return Record{}, ErrRecordNotFound
		}
		return Record{}, fmt.Errorf("nuban: unsuccessful response: %s", truncate(envelope.Message, 120))
	}

	var d nubanData
	if err := json.Unmarshal(envelope.Data, &d); err != nil {
		return Record{}, errors.New("nuban: unexpected response shape")
	}
	rec := Record{
		FirstName:   d.FirstName,
		MiddleName:  d.MiddleName,
		LastName:    firstNonEmpty(d.LastName, d.Surname),
		DateOfBirth: firstNonEmpty(d.DOB, d.Birthdate),
		FeeCurrency: d.Billing.Currency,
	}
	if fee, ok := parseFee(d.Billing.VerificationFee); ok {
		rec.Fee = &fee
	}
	if rec.FirstName == "" && rec.LastName == "" {
		return Record{}, errors.New("nuban: response had no name")
	}
	return rec, nil
}

// statusOK accepts the shapes a "status" field commonly takes: true,
// "success", "ok", "true", 200. A missing status counts as OK when the
// HTTP status was 2xx.
func statusOK(raw json.RawMessage) bool {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(string(raw))), `"`)
	switch s {
	case "", "true", "success", "successful", "ok", "200":
		return true
	}
	return false
}

// looksNotFound reads a 200-with-failure message. Account problems
// (key, wallet balance, rate limit) are never "not found", even when
// worded "invalid API key" — those must surface as unavailable.
func looksNotFound(msg string) bool {
	m := strings.ToLower(msg)
	for _, account := range []string{"key", "balance", "credit", "limit", "unauthori"} {
		if strings.Contains(m, account) {
			return false
		}
	}
	return strings.Contains(m, "not found") || strings.Contains(m, "no record") || strings.Contains(m, "invalid")
}

func parseFee(raw json.RawMessage) (decimal.Decimal, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, false
	}
	return d, true
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
