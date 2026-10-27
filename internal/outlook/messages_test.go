package outlook

import (
	"errors"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestBuildSearchKQL(t *testing.T) {
	q, err := mail.ParseQueryDate("2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	_ = q
	got, err := buildSearchKQL(mail.Query{
		Terms:   []string{"invoice"},
		Phrases: []string{"quarterly report"},
		From:    []string{"a@example.test", "b@example.test"},
		To:      []string{"to@example.test"},
		CC:      []string{"cc@example.test"},
		Subject: []string{"contract"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `"(from:\"a@example.test\" OR from:\"b@example.test\") AND to:\"to@example.test\" AND cc:\"cc@example.test\" AND subject:\"contract\" AND \"invoice\" AND \"quarterly report\""`
	if got != want {
		t.Fatalf("kql = %q, want %q", got, want)
	}
	if _, err := buildSearchKQL(mail.Query{Terms: []string{`bad\term`}}); !hasErrCode(err, errs.CodeUnsupportedQueryOperator) {
		t.Fatalf("backslash err = %v", err)
	}
}

func TestBuildMessageFilters(t *testing.T) {
	after, _ := mail.ParseQueryDate("2026-01-01")
	before, _ := mail.ParseQueryDate("2026-02-01")
	has := true
	for _, tt := range []struct {
		name string
		q    mail.Query
		want string
	}{
		{name: "anchor for attachment only", q: mail.Query{HasAttachment: &has}, want: "receivedDateTime ge 1900-01-01T00:00:00Z and hasAttachments eq true"},
		{name: "after before", q: mail.Query{After: &after, Before: &before}, want: "receivedDateTime ge 2026-01-01T00:00:00Z and receivedDateTime lt 2026-02-01T00:00:00Z"},
		{name: "before first date predicate", q: mail.Query{Before: &before, HasAttachment: &has}, want: "receivedDateTime lt 2026-02-01T00:00:00Z and hasAttachments eq true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildQueryFilter(tt.q); got != tt.want {
				t.Fatalf("filter = %q, want %q", got, tt.want)
			}
		})
	}
	if got, want := buildListFilter(mail.ListFilter{Unread: true}), "receivedDateTime ge 1900-01-01T00:00:00Z and isRead eq false"; got != want {
		t.Fatalf("list filter = %q, want %q", got, want)
	}
}

func hasErrCode(err error, code string) bool {
	var pe *errs.Error
	return errors.As(err, &pe) && pe.Code() == code
}
