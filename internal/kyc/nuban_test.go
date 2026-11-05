package kyc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNuban serves one canned response and records what it was asked.
type fakeNuban struct {
	status int
	body   string

	gotPath, gotQuery, gotAuth string
}

func (f *fakeNuban) start(t *testing.T) *NubanProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath, f.gotQuery, f.gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(srv.Close)
	return NewNubanProvider(srv.URL+"/", "test-key")
}

func TestNuban_BVN(t *testing.T) {
	f := &fakeNuban{status: 200, body: `{"status":true,"message":"Verification Successful","data":{
		"firstname":"ADA","lastname":"OBI","bvn":"22233344455","dob":"31-01-1990","phone":"08000000000",
		"billing":{"currency":"NGN","verification_fee":150,"available_credits":1000}}}`}
	p := f.start(t)

	rec, err := p.Lookup(context.Background(), Lookup{IDType: IDTypeBVN, IDNumber: "22233344455"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if f.gotPath != "/api/bvn-data" || f.gotQuery != "bvn_no=22233344455" || f.gotAuth != "Bearer test-key" {
		t.Fatalf("unexpected request: path=%q query=%q auth=%q", f.gotPath, f.gotQuery, f.gotAuth)
	}
	if rec.FirstName != "ADA" || rec.LastName != "OBI" || rec.DateOfBirth != "31-01-1990" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Fee == nil || rec.Fee.String() != "150" || rec.FeeCurrency != "NGN" {
		t.Fatalf("unexpected fee: %v %q", rec.Fee, rec.FeeCurrency)
	}
}

func TestNuban_NINUsesSurnameAndBirthdate(t *testing.T) {
	f := &fakeNuban{status: 200, body: `{"status":"success","data":{"firstname":"Ada","surname":"Obi","birthdate":"1990-01-31","photo":"aGVsbG8=","billing":{"currency":"NGN","verification_fee":"200.00"}}}`}
	rec, err := f.start(t).Lookup(context.Background(), Lookup{IDType: IDTypeNIN, IDNumber: "12345678901"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if f.gotPath != "/api/nin-data" || f.gotQuery != "nin_no=12345678901" {
		t.Fatalf("unexpected request: %q %q", f.gotPath, f.gotQuery)
	}
	if rec.LastName != "Obi" || rec.DateOfBirth != "1990-01-31" || rec.Fee.String() != "200" {
		t.Fatalf("unexpected record: %+v fee=%v", rec, rec.Fee)
	}
}

func TestNuban_PhoneCamelCase(t *testing.T) {
	f := &fakeNuban{status: 200, body: `{"status":true,"data":{"firstName":"Ada","surname":"Obi","phoneNumber":"08012345678","nin":"123"}}`}
	rec, err := f.start(t).Lookup(context.Background(), Lookup{IDType: IDTypePhone, IDNumber: "08012345678"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if f.gotPath != "/api/phone-data" || f.gotQuery != "phone_no=08012345678" {
		t.Fatalf("unexpected request: %q %q", f.gotPath, f.gotQuery)
	}
	if rec.FirstName != "Ada" || rec.LastName != "Obi" || rec.DateOfBirth != "" || rec.Fee != nil {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestNuban_DriversLicenceSendsNames(t *testing.T) {
	f := &fakeNuban{status: 200, body: `{"status":true,"message":"DL Verification Successful","data":{"driversLicense":"ABC12345YZ00","firstname":"Ada","lastname":"Obi","middlename":"Chioma","birthdate":"31/01/1990"}}`}
	rec, err := f.start(t).Lookup(context.Background(), Lookup{IDType: IDTypeDriversLicense, IDNumber: "ABC12345YZ00", FirstName: "Ada", LastName: "Obi"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if f.gotPath != "/api/driver-license-data" || f.gotQuery != "first_name=Ada&last_name=Obi&license_no=ABC12345YZ00" {
		t.Fatalf("unexpected request: %q %q", f.gotPath, f.gotQuery)
	}
	if rec.MiddleName != "Chioma" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestNuban_NotFound(t *testing.T) {
	for name, f := range map[string]*fakeNuban{
		"http 404":         {status: 404, body: `{"status":false,"message":"Not found"}`},
		"http 400":         {status: 400, body: `{"status":false,"message":"Invalid bvn"}`},
		"200 status false": {status: 200, body: `{"status":false,"message":"No record found for this BVN"}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.start(t).Lookup(context.Background(), Lookup{IDType: IDTypeBVN, IDNumber: "22233344455"})
			if !errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("expected ErrRecordNotFound, got %v", err)
			}
		})
	}
}

func TestNuban_Unavailable(t *testing.T) {
	for name, f := range map[string]*fakeNuban{
		"bad key":           {status: 401, body: `{"status":false,"message":"Invalid API key"}`},
		"rate limited":      {status: 429, body: `{"message":"Rate limit exceeded"}`},
		"out of credit":     {status: 500, body: `{"status":false,"message":"Insufficient wallet balance"}`},
		"garbage":           {status: 200, body: `<html>oops</html>`},
		"200 failure":       {status: 200, body: `{"status":false,"message":"Service temporarily down"}`},
		"200 bad key":       {status: 200, body: `{"error":true,"status":false,"message":"Invalid API key"}`},
		"200 no balance":    {status: 200, body: `{"status":false,"message":"Insufficient balance"}`},
		"success, no names": {status: 200, body: `{"status":true,"data":{"bvn":"22233344455"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.start(t).Lookup(context.Background(), Lookup{IDType: IDTypeBVN, IDNumber: "22233344455"})
			if err == nil || errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("expected an unavailable error, got %v", err)
			}
			if strings.Contains(err.Error(), "22233344455") {
				t.Fatalf("error must never contain the ID number: %v", err)
			}
		})
	}
}

func TestNuban_ConnectionErrorHidesURL(t *testing.T) {
	p := NewNubanProvider("http://127.0.0.1:1", "test-key")
	_, err := p.Lookup(context.Background(), Lookup{IDType: IDTypeBVN, IDNumber: "22233344455"})
	if err == nil || strings.Contains(err.Error(), "22233344455") || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("expected a sanitised error, got %v", err)
	}
}
