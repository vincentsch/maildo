package search

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func datePtr(s string) *mail.QueryDate {
	d := mail.QueryDate(s)
	return &d
}

func TestParseQuery(t *testing.T) {
	trueValue := true
	falseValue := false
	after, _ := mail.ParseQueryDate("2026-01-01")
	before, _ := mail.ParseQueryDate("2026-02-01")
	for _, tt := range []struct {
		name string
		raw  string
		want mail.Query
		code string
	}{
		{name: "free terms", raw: "invoice contract", want: mail.Query{Terms: []string{"invoice", "contract"}}},
		{name: "quoted phrase", raw: `"quarterly invoice"`, want: mail.Query{Phrases: []string{"quarterly invoice"}}},
		{name: "quoted operator value", raw: `subject:"quarterly invoice"`, want: mail.Query{Subject: []string{"quarterly invoice"}}},
		{name: "all predicates", raw: `from:a@example.test to:b@example.test cc:c@example.test subject:"Q1" has:attachment after:2026-01-01 before:2026-02-01 urgent "exact words"`, want: mail.Query{
			Terms:         []string{"urgent"},
			Phrases:       []string{"exact words"},
			From:          []string{"a@example.test"},
			To:            []string{"b@example.test"},
			CC:            []string{"c@example.test"},
			Subject:       []string{"Q1"},
			HasAttachment: &trueValue,
			After:         &after,
			Before:        &before,
		}},
		{name: "repeated field", raw: "from:a from:b to:c subject:x subject:y", want: mail.Query{From: []string{"a", "b"}, To: []string{"c"}, Subject: []string{"x", "y"}}},
		{name: "empty", raw: " \t ", code: errs.CodeInvalidQuery},
		{name: "empty phrase", raw: `""`, code: errs.CodeInvalidQuery},
		{name: "unterminated quote", raw: `"broken`, code: errs.CodeInvalidQuery},
		{name: "embedded quote", raw: `a"b`, code: errs.CodeInvalidQuery},
		{name: "operator embedded quote", raw: `subject:a"b`, code: errs.CodeInvalidQuery},
		{name: "trailing after quote", raw: `"a"b`, code: errs.CodeInvalidQuery},
		{name: "missing operator value", raw: "from:", code: errs.CodeInvalidQuery},
		{name: "bad date", raw: "after:2026-1-1", code: errs.CodeInvalidQuery},
		{name: "is unread and flagged", raw: "is:unread is:starred", want: mail.Query{Unread: &trueValue, Flagged: &trueValue}},
		{name: "is read", raw: "is:read", want: mail.Query{Unread: &falseValue}},
		{name: "contradicting is", raw: "is:read is:unread", code: errs.CodeInvalidQuery},
		{name: "unknown is", raw: "is:important", code: errs.CodeInvalidQuery},
		{name: "relative after", raw: "after:yesterday", want: mail.Query{After: datePtr("2026-10-25")}},
		{name: "newer than age", raw: "newer:7d", want: mail.Query{After: datePtr("2026-10-19")}},
		{name: "older than months", raw: "older:2m", want: mail.Query{Before: datePtr("2026-08-26")}},
		{name: "newer needs age", raw: "newer:2026-01-01", code: errs.CodeInvalidQuery},
		{name: "OR is rejected", raw: "from:a OR from:b", code: errs.CodeUnsupportedQueryOperator},
		{name: "quoted OR is a phrase", raw: `"OR"`, want: mail.Query{Phrases: []string{"OR"}}},
		{name: "NOT is rejected", raw: "NOT from:clara", code: errs.CodeUnsupportedQueryOperator},
		{name: "AND is rejected", raw: "invoice AND contract", code: errs.CodeUnsupportedQueryOperator},
		{name: "minus word is rejected", raw: "invoice -draft", code: errs.CodeUnsupportedQueryOperator},
		{name: "minus operator is rejected", raw: "-from:shop", code: errs.CodeUnsupportedQueryOperator},
		{name: "quoted NOT and minus are phrases", raw: `"NOT" "-draft"`, want: mail.Query{Phrases: []string{"NOT", "-draft"}}},
		{name: "lowercase and, not and inner hyphens are words", raw: "terms and conditions not e-mail", want: mail.Query{Terms: []string{"terms", "and", "conditions", "not", "e-mail"}}},
		{name: "has attachments plural", raw: "has:attachments", want: mail.Query{HasAttachment: &trueValue}},
		{name: "duplicate after", raw: "after:2026-01-01 after:2026-01-02", code: errs.CodeInvalidQuery},
		{name: "duplicate before", raw: "before:2026-01-01 before:2026-01-02", code: errs.CodeInvalidQuery},
		{name: "duplicate has", raw: "has:attachment has:attachment", code: errs.CodeInvalidQuery},
		{name: "bad has", raw: "has:file", code: errs.CodeInvalidQuery},
		{name: "unsupported operator", raw: "bcc:x", code: errs.CodeUnsupportedQueryOperator},
		{name: "unrecognized name value", raw: "name:value", code: errs.CodeUnsupportedQueryOperator},
		{name: "quoted url phrase", raw: `"http://example.test"`, want: mail.Query{Phrases: []string{"http://example.test"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQueryAt(tt.raw, time.Date(2026, 10, 26, 15, 0, 0, 0, time.Local))
			if tt.code != "" {
				assertSearchErrCode(t, err, tt.code)
				return
			}
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("query = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseQueryDateUTC(t *testing.T) {
	d, err := mail.ParseQueryDate("2026-06-28")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.UTCStart(), time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTCStart = %s, want %s", got, want)
	}
	for _, raw := range []string{"2026-6-28", "2026-02-30", "not-date"} {
		_, err := mail.ParseQueryDate(raw)
		assertSearchErrCode(t, err, errs.CodeInvalidQuery)
	}
}

func assertSearchErrCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %s", code)
	}
	var pe *errs.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %T = %v, want errs.Error", err, err)
	}
	if pe.Code() != code {
		t.Fatalf("code = %s, want %s", pe.Code(), code)
	}
}
