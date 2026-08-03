// Phase 8 acceptance test: drives the reconciliation job's ops surface over
// the actual HTTP router, not just internal/ledger's own Store-level tests
// (internal/ledger/reconcile_test.go covers sweepOnce/flagDrift directly).
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func TestLedgerReconciliationOpsSurfaceOverHTTP(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)

	stores, err := buildStores(ctx, cfg, pool)
	if err != nil {
		t.Fatalf("build stores: %v", err)
	}
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	// --- Seed a real drift the only realistic way it can happen: direct
	// cache corruption after a normal Post (see internal/ledger/
	// reconcile_test.go's identical reasoning). ---
	a, err := stores.ledger.GetOrCreateAccount(ctx, nil, "ledger_http_test_a_"+uuid.NewString()[:8], "USD", "fiat", "A")
	if err != nil {
		t.Fatalf("get or create account a: %v", err)
	}
	b, err := stores.ledger.GetOrCreateAccount(ctx, nil, "ledger_http_test_b_"+uuid.NewString()[:8], "USD", "fiat", "B")
	if err != nil {
		t.Fatalf("get or create account b: %v", err)
	}
	if _, err := stores.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: a, Direction: ledger.Debit, Amount: decimal.NewFromInt(20), AssetCode: "USD"},
			{AccountID: b, Direction: ledger.Credit, Amount: decimal.NewFromInt(20), AssetCode: "USD"},
		},
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_balances SET balance = balance + 3 WHERE account_id = $1`, a); err != nil {
		t.Fatalf("corrupt cached balance: %v", err)
	}

	// --- Run one reconcile sweep directly (main.go's own job isn't started
	// by buildStores/buildRouter — same split settlement_test.go's comment
	// on background jobs notes). ---
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	ledger.NewReconcileJob(stores.ledger, 20*time.Millisecond).Run(reconcileCtx)
	cancel()

	// --- Admin logs in. ---
	adminEmail := "ledger-test-" + uuid.NewString() + "@sirfi.test"
	if _, err := stores.admin.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &loginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}

	// --- The discrepancy shows up in the default (open) listing. ---
	var listResp []struct {
		ID         string `json:"id"`
		AccountID  string `json:"account_id"`
		ResolvedAt string `json:"resolved_at"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/ledger/discrepancies", loginResp.Token, nil, &listResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list discrepancies: expected 200, got %d", resp.StatusCode)
	}
	var discrepancyID string
	for _, row := range listResp {
		if row.AccountID == a.String() {
			discrepancyID = row.ID
		}
	}
	if discrepancyID == "" {
		t.Fatalf("expected an open discrepancy for account %s, got %+v", a, listResp)
	}

	// --- Resolving it drops it out of the open listing. ---
	var resolveResp struct {
		ResolvedAt string `json:"resolved_at"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/ledger/discrepancies/"+discrepancyID+"/resolve", loginResp.Token, map[string]string{
		"note": "investigated, cache write bug",
	}, &resolveResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve discrepancy: expected 200, got %d", resp.StatusCode)
	}
	if resolveResp.ResolvedAt == "" {
		t.Fatal("expected resolved_at to be set in the response")
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/ledger/discrepancies", loginResp.Token, nil, &listResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list discrepancies after resolve: expected 200, got %d", resp.StatusCode)
	}
	for _, row := range listResp {
		if row.ID == discrepancyID {
			t.Fatalf("expected the resolved discrepancy to drop out of the default open listing, still present: %+v", row)
		}
	}
}
