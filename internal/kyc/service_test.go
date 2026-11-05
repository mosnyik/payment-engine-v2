package kyc_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/kyc"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, "KYC Test "+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

// fakeProvider returns a fixed record or error.
type fakeProvider struct {
	rec   kyc.Record
	err   error
	calls int
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Lookup(context.Context, kyc.Lookup) (kyc.Record, error) {
	f.calls++
	return f.rec, f.err
}

var secret = []byte("0123456789abcdef0123456789abcdef")

func validRequest() kyc.Request {
	return kyc.Request{IDType: kyc.IDTypeBVN, IDNumber: "22233344455", FirstName: "Ada", LastName: "Obi", DateOfBirth: "1990-01-31", Consent: true}
}

type storedRow struct {
	provider, idType, hash, result, checks string
	fee                                    *decimal.Decimal
	feeCurrency                            *string
}

func loadRow(t *testing.T, pool *db.Pool, id uuid.UUID) storedRow {
	t.Helper()
	var r storedRow
	err := pool.QueryRow(context.Background(),
		`SELECT provider, id_type, id_number_hash, result, checks::text, provider_fee, provider_fee_currency FROM kyc_checks WHERE id = $1`, id,
	).Scan(&r.provider, &r.idType, &r.hash, &r.result, &r.checks, &r.fee, &r.feeCurrency)
	if err != nil {
		t.Fatalf("load kyc_checks row: %v", err)
	}
	return r
}

func TestVerify_MatchIsRecordedWithoutPersonalData(t *testing.T) {
	pool := openTestPool(t)
	tenantID := newTenant(t, pool)
	fee := decimal.NewFromInt(150)
	p := &fakeProvider{rec: kyc.Record{FirstName: "ADA", LastName: "OBI", DateOfBirth: "31-01-1990", Fee: &fee, FeeCurrency: "NGN"}}
	svc := kyc.New(pool, p, secret)

	check, err := svc.Verify(context.Background(), tenantID, nil, validRequest())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if check.Result != kyc.ResultMatch || check.Checks["date_of_birth"] != kyc.FieldMatch {
		t.Fatalf("unexpected check: %+v", check)
	}

	row := loadRow(t, pool, check.ID)
	if row.provider != "fake" || row.idType != "bvn" || row.result != "match" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.fee == nil || !row.fee.Equal(fee) || row.feeCurrency == nil || *row.feeCurrency != "NGN" {
		t.Fatalf("expected the provider fee to be recorded, got %v %v", row.fee, row.feeCurrency)
	}
	if len(row.hash) != 64 || strings.Contains(row.hash, "22233344455") {
		t.Fatalf("expected a keyed hash, got %q", row.hash)
	}
	for _, leaked := range []string{"22233344455", "ADA", "OBI", "1990"} {
		if strings.Contains(row.checks, leaked) {
			t.Fatalf("checks column leaked %q: %s", leaked, row.checks)
		}
	}

	// Same ID number, same key → same hash (lets repeat checks be linked).
	again, err := svc.Verify(context.Background(), tenantID, nil, validRequest())
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if loadRow(t, pool, again.ID).hash != row.hash {
		t.Fatal("expected the same ID number to hash the same way")
	}
	// A different server secret → a different hash.
	other, err := kyc.New(pool, p, []byte("another-secret-another-secret-00")).Verify(context.Background(), tenantID, nil, validRequest())
	if err != nil {
		t.Fatalf("verify with other secret: %v", err)
	}
	if loadRow(t, pool, other.ID).hash == row.hash {
		t.Fatal("expected the hash to depend on the server secret")
	}
}

func TestVerify_NotFound(t *testing.T) {
	pool := openTestPool(t)
	tenantID := newTenant(t, pool)
	svc := kyc.New(pool, &fakeProvider{err: kyc.ErrRecordNotFound}, secret)

	check, err := svc.Verify(context.Background(), tenantID, nil, validRequest())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if check.Result != kyc.ResultNotFound || len(check.Checks) != 0 {
		t.Fatalf("unexpected check: %+v", check)
	}
	if row := loadRow(t, pool, check.ID); row.result != "not_found" || row.fee != nil {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestVerify_ProviderDownIsRecordedAndReturnsUnavailable(t *testing.T) {
	pool := openTestPool(t)
	tenantID := newTenant(t, pool)
	svc := kyc.New(pool, &fakeProvider{err: errors.New("HTTP 500 insufficient balance")}, secret)

	_, err := svc.Verify(context.Background(), tenantID, nil, validRequest())
	if !errors.Is(err, kyc.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM kyc_checks WHERE tenant_id = $1 AND result = 'error'`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the failed call to be recorded once, got %d", n)
	}
}

func TestVerify_InvalidRequestNeverCallsProvider(t *testing.T) {
	pool := openTestPool(t)
	tenantID := newTenant(t, pool)
	p := &fakeProvider{}
	svc := kyc.New(pool, p, secret)

	req := validRequest()
	req.Consent = false
	if _, err := svc.Verify(context.Background(), tenantID, nil, req); !errors.Is(err, kyc.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if p.calls != 0 {
		t.Fatal("provider must not be called (and billed) for an invalid request")
	}
}

func TestVerify_NotConfigured(t *testing.T) {
	svc := kyc.New(nil, nil, secret)
	if _, err := svc.Verify(context.Background(), uuid.New(), nil, validRequest()); !errors.Is(err, kyc.ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}
