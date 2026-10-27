package mail

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeMessageIDScalar(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want string
	}{
		{raw: "local@example.com", want: "<local@example.com>"},
		{raw: " <Mixed.Case+tag@example-domain.test>\t", want: "<Mixed.Case+tag@example-domain.test>"},
	} {
		got, err := NormalizeMessageIDScalar(tt.raw)
		if err != nil || got != tt.want {
			t.Fatalf("NormalizeMessageIDScalar(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
		}
	}
}

func TestNormalizeMessageIDScalarRejectsInvalidInput(t *testing.T) {
	for _, raw := range []string{
		"",
		"missing-at",
		"two@@example.com",
		"<unbalanced@example.com",
		"space local@example.com",
		"local@example..com",
		".local@example.com",
		"local@example.com.",
		"local@-example.com",
	} {
		if _, err := NormalizeMessageIDScalar(raw); !errors.Is(err, ErrInvalidMessageID) {
			t.Fatalf("NormalizeMessageIDScalar(%q) error = %v", raw, err)
		}
	}
}

func TestParseMessageIDTokens(t *testing.T) {
	got, err := ParseMessageIDTokens("<a@example.com> <b+tag@example.org>\t<c@example.net>")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"<a@example.com>", "<b+tag@example.org>", "<c@example.net>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %#v, want %#v", got, want)
	}
}

func TestParseMessageIDTokensRejectsMalformedOrDuplicateTokens(t *testing.T) {
	for _, raw := range []string{
		"a@example.com",
		"<a@example.com>,<b@example.com>",
		"<a@example.com> text",
		"<a@example.com> <a@example.com>",
		"<>",
	} {
		if _, err := ParseMessageIDTokens(raw); !errors.Is(err, ErrInvalidMessageID) {
			t.Fatalf("ParseMessageIDTokens(%q) error = %v", raw, err)
		}
	}
}
