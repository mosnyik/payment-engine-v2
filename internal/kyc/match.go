package kyc

import (
	"strings"
	"time"
	"unicode"
)

// compare matches the (validated) request against the record.
//
//   - first_name matches if every word of it appears in the record's
//     first or middle name (people often give their middle name first).
//   - last_name matches if every word of it appears in the record's last
//     name.
//   - date_of_birth is compared only when sent.
//
// Overall: match when every sent field matches; no_match when neither name
// matches; partial_match otherwise.
func compare(req Request, rec Record) (Result, map[string]FieldCheck) {
	checks := map[string]FieldCheck{
		"first_name":    nameCheck(req.FirstName, rec.FirstName+" "+rec.MiddleName),
		"last_name":     nameCheck(req.LastName, rec.LastName),
		"date_of_birth": FieldNotChecked,
	}
	if req.DateOfBirth != "" {
		checks["date_of_birth"] = dobCheck(req.DateOfBirth, rec.DateOfBirth)
	}

	if checks["first_name"] != FieldMatch && checks["last_name"] != FieldMatch {
		return ResultNoMatch, checks
	}
	for _, c := range checks {
		if c != FieldMatch && c != FieldNotChecked {
			return ResultPartialMatch, checks
		}
	}
	return ResultMatch, checks
}

func nameCheck(given, onRecord string) FieldCheck {
	recordWords := nameWords(onRecord)
	if len(recordWords) == 0 {
		return FieldUnavailable
	}
	have := make(map[string]bool, len(recordWords))
	for _, w := range recordWords {
		have[w] = true
	}
	givenWords := nameWords(given)
	if len(givenWords) == 0 {
		return FieldMismatch
	}
	for _, w := range givenWords {
		if !have[w] {
			return FieldMismatch
		}
	}
	return FieldMatch
}

// nameWords lowercases, drops punctuation and splits on spaces and
// hyphens, so "Adé-Ola" and "ade ola" compare equal word for word (accents
// are kept: "adé" != "ade" — records and callers both usually omit them).
func nameWords(s string) []string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '-' || unicode.IsSpace(r):
			return ' '
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		default:
			return -1
		}
	}, s)
	return strings.Fields(s)
}

func dobCheck(given, onRecord string) FieldCheck {
	rec, ok := parseDate(onRecord)
	if !ok {
		return FieldUnavailable
	}
	if rec.Format("2006-01-02") == given {
		return FieldMatch
	}
	return FieldMismatch
}

// dateLayouts are the formats provider records have been seen to use.
// Day-first for the slash/dash numeric forms: Nigerian records are
// DD-MM-YYYY, never US month-first.
var dateLayouts = []string{
	"2006-01-02",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"02-01-2006",
	"02/01/2006",
	"2-1-2006",
	"2/1/2006",
	"02-Jan-2006",
	"2-Jan-2006",
	"02 Jan 2006",
	"2 January 2006",
	"January 2, 2006",
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
