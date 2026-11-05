package kyc

import (
	"errors"
	"testing"
)

func TestCompare(t *testing.T) {
	rec := Record{FirstName: "Adebayo", MiddleName: "Oluwaseun", LastName: "Okafor-Bello", DateOfBirth: "15-03-1988"}
	cases := []struct {
		name      string
		req       Request
		want      Result
		wantFirst FieldCheck
		wantLast  FieldCheck
		wantDOB   FieldCheck
	}{
		{"exact names, no dob", Request{FirstName: "Adebayo", LastName: "Okafor-Bello"}, ResultMatch, FieldMatch, FieldMatch, FieldNotChecked},
		{"case and punctuation ignored", Request{FirstName: "ADEBAYO.", LastName: "okafor bello"}, ResultMatch, FieldMatch, FieldMatch, FieldNotChecked},
		{"middle name given as first", Request{FirstName: "Oluwaseun", LastName: "Okafor"}, ResultMatch, FieldMatch, FieldMatch, FieldNotChecked},
		{"dob matches across formats", Request{FirstName: "Adebayo", LastName: "Bello", DateOfBirth: "1988-03-15"}, ResultMatch, FieldMatch, FieldMatch, FieldMatch},
		{"dob mismatch", Request{FirstName: "Adebayo", LastName: "Bello", DateOfBirth: "1988-03-16"}, ResultPartialMatch, FieldMatch, FieldMatch, FieldMismatch},
		{"one name wrong", Request{FirstName: "Chinedu", LastName: "Okafor"}, ResultPartialMatch, FieldMismatch, FieldMatch, FieldNotChecked},
		{"extra word not on record", Request{FirstName: "Adebayo John", LastName: "Okafor"}, ResultPartialMatch, FieldMismatch, FieldMatch, FieldNotChecked},
		{"both names wrong", Request{FirstName: "Chinedu", LastName: "Eze"}, ResultNoMatch, FieldMismatch, FieldMismatch, FieldNotChecked},
		{"both names wrong even if dob right", Request{FirstName: "Chinedu", LastName: "Eze", DateOfBirth: "1988-03-15"}, ResultNoMatch, FieldMismatch, FieldMismatch, FieldMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, checks := compare(tc.req, rec)
			if got != tc.want || checks["first_name"] != tc.wantFirst || checks["last_name"] != tc.wantLast || checks["date_of_birth"] != tc.wantDOB {
				t.Fatalf("got %s %v, want %s first=%s last=%s dob=%s", got, checks, tc.want, tc.wantFirst, tc.wantLast, tc.wantDOB)
			}
		})
	}
}

func TestCompare_RecordWithoutDOB(t *testing.T) {
	// Phone lookups carry no date of birth: a DOB the caller sent can't be
	// confirmed, so the result is partial rather than a full match.
	got, checks := compare(Request{FirstName: "Ada", LastName: "Obi", DateOfBirth: "1990-01-01"}, Record{FirstName: "Ada", LastName: "Obi"})
	if got != ResultPartialMatch || checks["date_of_birth"] != FieldUnavailable {
		t.Fatalf("got %s %v", got, checks)
	}
}

func TestParseDate(t *testing.T) {
	for _, s := range []string{"1990-01-31", "31-01-1990", "31/01/1990", "31-Jan-1990", "31 Jan 1990", "31 January 1990", "1990-01-31T00:00:00Z"} {
		d, ok := parseDate(s)
		if !ok || d.Format("2006-01-02") != "1990-01-31" {
			t.Fatalf("parseDate(%q) = %v, %v", s, d, ok)
		}
	}
	if _, ok := parseDate("not a date"); ok {
		t.Fatal("expected garbage to fail")
	}
}

func TestValidate(t *testing.T) {
	base := Request{IDType: IDTypeBVN, IDNumber: "22233344455", FirstName: "Ada", LastName: "Obi", Consent: true}

	if _, err := validate(base); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	bad := map[string]func(r *Request){
		"no consent":         func(r *Request) { r.Consent = false },
		"missing first name": func(r *Request) { r.FirstName = " " },
		"missing last name":  func(r *Request) { r.LastName = "" },
		"unknown id type":    func(r *Request) { r.IDType = "passport" },
		"short bvn":          func(r *Request) { r.IDNumber = "1234" },
		"letters in nin":     func(r *Request) { r.IDType = IDTypeNIN; r.IDNumber = "1234567890a" },
		"bad phone":          func(r *Request) { r.IDType = IDTypePhone; r.IDNumber = "12345" },
		"bad licence":        func(r *Request) { r.IDType = IDTypeDriversLicense; r.IDNumber = "AB" },
		"bad dob format":     func(r *Request) { r.DateOfBirth = "31/01/1990" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if _, err := validate(r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("expected ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestNormalizePhone(t *testing.T) {
	for _, in := range []string{"08012345678", "+2348012345678", "2348012345678", "0801 234 5678", "+234-801-234-5678"} {
		got, ok := normalizePhone(in)
		if !ok || got != "08012345678" {
			t.Fatalf("normalizePhone(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"8012345678", "18012345678", "+44 7911 123456"} {
		if _, ok := normalizePhone(in); ok {
			t.Fatalf("normalizePhone(%q) accepted", in)
		}
	}
}

func TestValidate_NormalizesLicence(t *testing.T) {
	r, err := validate(Request{IDType: IDTypeDriversLicense, IDNumber: "abc-12345 yz00", FirstName: "A", LastName: "B", Consent: true})
	if err != nil || r.IDNumber != "ABC12345YZ00" {
		t.Fatalf("got %q, %v", r.IDNumber, err)
	}
}
