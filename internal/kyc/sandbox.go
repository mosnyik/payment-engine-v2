package kyc

import (
	"context"
	"errors"
	"strings"
)

// SandboxProviderName is the provider name stored for sandbox checks.
const SandboxProviderName = "sandbox"

// SandboxProvider answers without calling anyone, so builders can
// integrate against SANDBOX_MODE for free. Outcomes are chosen by the ID
// number's last digit:
//
//   - ...0  not_found
//   - ...1  no_match (record is "Sandbox Person")
//   - ...2  partial_match (first name matches, last name doesn't)
//   - ...9  provider unavailable (503)
//   - else  match (record echoes the given names, DOB 1990-01-31)
//
// A request with no date_of_birth never checks it, so "match" stays a
// match; send 1990-01-31 to see a DOB match, anything else for a mismatch.
type SandboxProvider struct{}

func (SandboxProvider) Name() string { return SandboxProviderName }

func (SandboxProvider) Lookup(_ context.Context, l Lookup) (Record, error) {
	last := l.IDNumber[len(l.IDNumber)-1]
	switch last {
	case '0':
		return Record{}, ErrRecordNotFound
	case '1':
		return Record{FirstName: "Sandbox", LastName: "Person", DateOfBirth: "1990-01-31"}, nil
	case '2':
		return Record{FirstName: l.FirstName, LastName: "Different", DateOfBirth: "1990-01-31"}, nil
	case '9':
		return Record{}, errors.New("sandbox: simulated provider outage")
	}
	return Record{FirstName: strings.TrimSpace(l.FirstName), LastName: strings.TrimSpace(l.LastName), DateOfBirth: "31-01-1990"}, nil
}
