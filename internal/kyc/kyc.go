// Package kyc is 2Settle's identity-verification service for builders
// (POST /v2/kyc/verify): a tenant sends a person's ID number and the name
// (and optionally date of birth) it was given, and gets back whether they
// match the official record. Only the match outcome ever leaves this
// package or reaches the database — the record the provider returns
// (name, DOB, phone, address, photo) is compared in memory and dropped.
package kyc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

// IDType is the kind of identifier being checked.
type IDType string

const (
	IDTypeBVN            IDType = "bvn"
	IDTypeNIN            IDType = "nin"
	IDTypePhone          IDType = "phone"
	IDTypeDriversLicense IDType = "drivers_license"
)

// Result is the overall outcome of a check.
type Result string

const (
	// ResultMatch: every field the caller sent matches the record.
	ResultMatch Result = "match"
	// ResultPartialMatch: some fields match, others don't or couldn't be
	// compared (e.g. a date of birth the record doesn't carry).
	ResultPartialMatch Result = "partial_match"
	// ResultNoMatch: the record exists but neither name matches.
	ResultNoMatch Result = "no_match"
	// ResultNotFound: the provider has no record for this ID number.
	ResultNotFound Result = "not_found"
	// resultError is stored for a call the provider couldn't answer; it is
	// never returned to the caller as a Result (they get ErrUnavailable).
	resultError Result = "error"
)

// FieldCheck is the per-field outcome.
type FieldCheck string

const (
	FieldMatch    FieldCheck = "match"
	FieldMismatch FieldCheck = "mismatch"
	// FieldNotChecked: the caller didn't send this field.
	FieldNotChecked FieldCheck = "not_checked"
	// FieldUnavailable: the caller sent it but the record has no usable
	// value to compare against.
	FieldUnavailable FieldCheck = "unavailable"
)

var (
	// ErrInvalidRequest: the request failed validation (bad id_type, ID
	// number format, missing name, no consent). Safe to show the caller.
	ErrInvalidRequest = errors.New("kyc: invalid request")
	// ErrNotConfigured: no KYC provider is wired up on this deployment.
	ErrNotConfigured = errors.New("kyc: verification is not available")
	// ErrUnavailable: the provider couldn't answer (down, rate limited,
	// out of credit, bad key). Details stay in the server log.
	ErrUnavailable = errors.New("kyc: verification provider unavailable, try again later")
	// ErrRecordNotFound is what a Provider returns when it has no record
	// for the ID number.
	ErrRecordNotFound = errors.New("kyc: record not found")
)

// Lookup is what a Provider is asked for. FirstName/LastName are passed
// because some lookups (driver's licence) need them as query parameters.
type Lookup struct {
	IDType    IDType
	IDNumber  string
	FirstName string
	LastName  string
}

// Record is the subset of the official record used for matching.
// DateOfBirth is as the provider sent it; parseDate normalises it.
type Record struct {
	FirstName   string
	MiddleName  string
	LastName    string
	DateOfBirth string

	// Fee/FeeCurrency are what the provider billed for this lookup, if it
	// said. Fee is nil when unknown.
	Fee         *decimal.Decimal
	FeeCurrency string
}

// Provider looks an identity up. It returns ErrRecordNotFound when there's
// no record and any other error when it couldn't answer.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, l Lookup) (Record, error)
}

// Request is one verification as the tenant sent it.
type Request struct {
	IDType      IDType
	IDNumber    string
	FirstName   string
	LastName    string
	DateOfBirth string // optional, YYYY-MM-DD
	Consent     bool
}

// Check is the stored and returned outcome.
type Check struct {
	ID        uuid.UUID
	IDType    IDType
	Result    Result
	Checks    map[string]FieldCheck
	CreatedAt time.Time
}

// Service runs checks and records them.
type Service struct {
	pool     *db.Pool
	provider Provider
	hashKey  []byte
}

// New builds a Service. provider may be nil (no KYC configured): Verify
// then returns ErrNotConfigured. secret keys the ID-number hash; it's
// domain-separated here so it can safely be an existing server secret.
func New(pool *db.Pool, provider Provider, secret []byte) *Service {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("kyc-id-number-hash-v1"))
	return &Service{pool: pool, provider: provider, hashKey: mac.Sum(nil)}
}

// Verify validates req, looks the identity up and records the check for
// tenantID. apiKeyID may be nil. The returned Check never contains
// anything from the provider's record beyond the per-field outcome.
func (s *Service) Verify(ctx context.Context, tenantID uuid.UUID, apiKeyID *uuid.UUID, req Request) (*Check, error) {
	if s.provider == nil {
		return nil, ErrNotConfigured
	}
	norm, err := validate(req)
	if err != nil {
		return nil, err
	}

	rec, lookupErr := s.provider.Lookup(ctx, Lookup{
		IDType: norm.IDType, IDNumber: norm.IDNumber, FirstName: norm.FirstName, LastName: norm.LastName,
	})

	var result Result
	checks := map[string]FieldCheck{}
	switch {
	case lookupErr == nil:
		result, checks = compare(norm, rec)
	case errors.Is(lookupErr, ErrRecordNotFound):
		result = ResultNotFound
	default:
		result = resultError
	}

	check, err := s.record(ctx, tenantID, apiKeyID, norm, result, checks, rec, lookupErr == nil)
	if err != nil {
		return nil, err
	}
	if result == resultError {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, s.provider.Name(), lookupErr)
	}
	return check, nil
}

func (s *Service) record(ctx context.Context, tenantID uuid.UUID, apiKeyID *uuid.UUID, req Request, result Result, checks map[string]FieldCheck, rec Record, billed bool) (*Check, error) {
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return nil, fmt.Errorf("kyc: encode checks: %w", err)
	}
	var fee *decimal.Decimal
	var feeCurrency *string
	if billed && rec.Fee != nil {
		fee = rec.Fee
		if rec.FeeCurrency != "" {
			c := rec.FeeCurrency
			feeCurrency = &c
		}
	}
	c := &Check{IDType: req.IDType, Result: result, Checks: checks}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO kyc_checks (tenant_id, api_key_id, provider, id_type, id_number_hash, result, checks, provider_fee, provider_fee_currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, created_at`,
		tenantID, apiKeyID, s.provider.Name(), string(req.IDType), s.hashIDNumber(req.IDType, req.IDNumber),
		string(result), checksJSON, fee, feeCurrency,
	).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("kyc: record check: %w", err)
	}
	return c, nil
}

// hashIDNumber is a keyed hash, so the same ID can be recognised across
// checks (repeat lookups, disputes) without the number being recoverable
// from the database.
func (s *Service) hashIDNumber(t IDType, idNumber string) string {
	mac := hmac.New(sha256.New, s.hashKey)
	mac.Write([]byte(string(t) + ":" + idNumber))
	return hex.EncodeToString(mac.Sum(nil))
}

var (
	elevenDigits  = regexp.MustCompile(`^\d{11}$`)
	licenceFormat = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)
)

// validate checks req and returns it normalised (trimmed, phone in local
// 0XXXXXXXXXX form, licence upper-cased).
func validate(req Request) (Request, error) {
	bad := func(msg string) (Request, error) {
		return Request{}, fmt.Errorf("%w: %s", ErrInvalidRequest, msg)
	}
	if !req.Consent {
		return bad("consent must be true: confirm the person agreed to this identity check")
	}
	req.IDNumber = strings.TrimSpace(req.IDNumber)
	req.FirstName = strings.TrimSpace(req.FirstName)
	req.LastName = strings.TrimSpace(req.LastName)
	req.DateOfBirth = strings.TrimSpace(req.DateOfBirth)
	if req.FirstName == "" || req.LastName == "" {
		return bad("first_name and last_name are required")
	}

	switch req.IDType {
	case IDTypeBVN, IDTypeNIN:
		if !elevenDigits.MatchString(req.IDNumber) {
			return bad("id_number must be 11 digits for " + string(req.IDType))
		}
	case IDTypePhone:
		p, ok := normalizePhone(req.IDNumber)
		if !ok {
			return bad("id_number must be a Nigerian phone number, e.g. 08012345678 or +2348012345678")
		}
		req.IDNumber = p
	case IDTypeDriversLicense:
		req.IDNumber = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(req.IDNumber))
		if !licenceFormat.MatchString(req.IDNumber) {
			return bad("id_number must be a driver's licence number (letters and digits)")
		}
	default:
		return bad("id_type must be one of bvn, nin, phone, drivers_license")
	}

	if req.DateOfBirth != "" {
		if _, err := time.Parse("2006-01-02", req.DateOfBirth); err != nil {
			return bad("date_of_birth must be YYYY-MM-DD")
		}
	}
	return req, nil
}

// normalizePhone accepts 0XXXXXXXXXX, 234XXXXXXXXXX or +234XXXXXXXXXX
// (spaces/dashes allowed) and returns the 11-digit local form.
func normalizePhone(v string) (string, bool) {
	s := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(v)
	s = strings.TrimPrefix(s, "+")
	if strings.HasPrefix(s, "234") && len(s) == 13 {
		s = "0" + s[3:]
	}
	if !elevenDigits.MatchString(s) || s[0] != '0' {
		return "", false
	}
	return s, true
}
