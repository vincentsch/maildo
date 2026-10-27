package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

type queryToken struct {
	value  string
	quoted bool
}

// ParseQuery reads a search written in maildo's search language. The
// language is small on purpose: a provider must translate every part exactly
// or refuse the search, so a search never matches more than you asked for.
// Relative dates such as after:yesterday or newer:7d count back from today's
// date on this computer.
func ParseQuery(raw string) (mail.Query, error) {
	return ParseQueryAt(raw, time.Now())
}

// ParseQueryAt is ParseQuery with a fixed "now", for tests.
func ParseQueryAt(raw string, now time.Time) (mail.Query, error) {
	tokens, err := tokenize(raw)
	if err != nil {
		return mail.Query{}, err
	}
	if len(tokens) == 0 {
		return mail.Query{}, invalidQuery("query is empty")
	}
	var q mail.Query
	for _, tok := range tokens {
		if tok.quoted {
			if tok.value == "" {
				return mail.Query{}, invalidQuery("quoted phrase must not be empty")
			}
			q.Phrases = append(q.Phrases, tok.value)
			continue
		}
		if strings.EqualFold(tok.value, "OR") {
			return mail.Query{}, errs.New(errs.CodeUnsupportedQueryOperator,
				"OR between search words is not supported; repeat an operator to match any of its values, like from:a@example.com from:b@example.com, or quote \"OR\" to search for the word")
		}
		// Searching for these as words would quietly change what the
		// search means, so they are refused with a way out.
		switch {
		case tok.value == "NOT" || (len(tok.value) > 1 && tok.value[0] == '-'):
			return mail.Query{}, errs.New(errs.CodeUnsupportedQueryOperator,
				fmt.Sprintf("maildo search cannot leave matches out, so %s would be searched as a word; quote it if you want to search for that text", tok.value))
		case tok.value == "AND":
			return mail.Query{}, errs.New(errs.CodeUnsupportedQueryOperator,
				"AND is not needed, because a message must match every search word anyway. Leave it out, or quote \"AND\" to search for the word")
		}
		if name, value, ok := operatorParts(tok.value); ok {
			if value == "" {
				return mail.Query{}, invalidQuery(fmt.Sprintf("missing value for %s:", name))
			}
			name = strings.ToLower(name)
			switch name {
			case "from":
				q.From = append(q.From, value)
			case "to":
				q.To = append(q.To, value)
			case "cc":
				q.CC = append(q.CC, value)
			case "subject":
				q.Subject = append(q.Subject, value)
			case "has":
				if value != "attachment" && value != "attachments" {
					return mail.Query{}, invalidQuery("has: only supports has:attachment")
				}
				if q.HasAttachment != nil {
					return mail.Query{}, invalidQuery("give has:attachment only once")
				}
				v := true
				q.HasAttachment = &v
			case "is":
				if err := applyIs(&q, strings.ToLower(value)); err != nil {
					return mail.Query{}, err
				}
			case "after", "newer", "newer_than":
				if q.After != nil {
					return mail.Query{}, invalidQuery("give only one of after: and newer:")
				}
				d, err := parseDateValue(name, value, now)
				if err != nil {
					return mail.Query{}, err
				}
				q.After = &d
			case "before", "older", "older_than":
				if q.Before != nil {
					return mail.Query{}, invalidQuery("give only one of before: and older:")
				}
				d, err := parseDateValue(name, value, now)
				if err != nil {
					return mail.Query{}, err
				}
				q.Before = &d
			default:
				// A word that looks like an operator is never searched as
				// plain text. You can quote it if you mean the text.
				return mail.Query{}, errs.New(errs.CodeUnsupportedQueryOperator,
					fmt.Sprintf("unsupported query operator %q; quote it to search for the text", name))
			}
			continue
		}
		q.Terms = append(q.Terms, tok.value)
	}
	if q.IsEmpty() {
		return mail.Query{}, invalidQuery("query is empty")
	}
	return q, nil
}

// applyIs handles is:unread, is:read, is:flagged (or is:starred) and
// is:unflagged.
func applyIs(q *mail.Query, value string) error {
	set := func(target **bool, v bool, name string) error {
		if *target != nil && **target != v {
			return invalidQuery(fmt.Sprintf("is:%s contradicts another is: operator", name))
		}
		*target = &v
		return nil
	}
	switch value {
	case "unread":
		return set(&q.Unread, true, value)
	case "read":
		return set(&q.Unread, false, value)
	case "flagged", "starred":
		return set(&q.Flagged, true, value)
	case "unflagged", "unstarred":
		return set(&q.Flagged, false, value)
	default:
		return invalidQuery(fmt.Sprintf("is:%s is not supported; use is:unread, is:read, is:flagged or is:unflagged", value))
	}
}

// parseDateValue accepts YYYY-MM-DD, today, yesterday, or an age such as
// 7d, 2w, 3m or 1y (that long before today). newer: and older: take only
// ages.
func parseDateValue(name, value string, now time.Time) (mail.QueryDate, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	format := func(t time.Time) mail.QueryDate { return mail.QueryDate(t.Format("2006-01-02")) }
	if age, ok := parseAge(value); ok {
		return format(age(today)), nil
	}
	if name == "newer" || name == "newer_than" || name == "older" || name == "older_than" {
		return "", invalidQuery(fmt.Sprintf("%s: takes an age like 7d, 2w, 3m or 1y", name))
	}
	switch strings.ToLower(value) {
	case "today":
		return format(today), nil
	case "yesterday":
		return format(today.AddDate(0, 0, -1)), nil
	}
	d, err := mail.ParseQueryDate(value)
	if err != nil {
		return "", invalidQuery(fmt.Sprintf("invalid date %q for %s:; use YYYY-MM-DD, today, yesterday or an age like 7d", value, name))
	}
	return d, nil
}

func parseAge(value string) (func(time.Time) time.Time, bool) {
	if len(value) < 2 {
		return nil, false
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n < 0 || n > 100000 {
		return nil, false
	}
	switch strings.ToLower(value[len(value)-1:]) {
	case "d":
		return func(t time.Time) time.Time { return t.AddDate(0, 0, -n) }, true
	case "w":
		return func(t time.Time) time.Time { return t.AddDate(0, 0, -7*n) }, true
	case "m":
		return func(t time.Time) time.Time { return t.AddDate(0, -n, 0) }, true
	case "y":
		return func(t time.Time) time.Time { return t.AddDate(-n, 0, 0) }, true
	}
	return nil, false
}

// tokenize splits on spaces but keeps quoted phrases and quoted operator
// values such as subject:"quarterly report" together. There are no escapes;
// each provider refuses values it cannot search for exactly.
func tokenize(raw string) ([]queryToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []queryToken
	for i := 0; i < len(raw); {
		for i < len(raw) && unicode.IsSpace(rune(raw[i])) {
			i++
		}
		if i >= len(raw) {
			break
		}
		start := i
		if raw[i] == '"' {
			value, next, err := readQuoted(raw, i+1)
			if err != nil {
				return nil, err
			}
			if next < len(raw) && !unicode.IsSpace(rune(raw[next])) {
				return nil, invalidQuery("put a space after a quoted phrase")
			}
			out = append(out, queryToken{value: value, quoted: true})
			i = next
			continue
		}
		for i < len(raw) && !unicode.IsSpace(rune(raw[i])) {
			if raw[i] == '"' {
				if i == start || raw[i-1] != ':' {
					return nil, invalidQuery("use double quotes only around a phrase or an operator value, like subject:\"march invoice\"")
				}
				prefix := raw[start : i-1]
				if !isOperatorName(prefix) {
					return nil, invalidQuery("a quoted value must follow an operator name, like subject:\"march invoice\"")
				}
				value, next, err := readQuoted(raw, i+1)
				if err != nil {
					return nil, err
				}
				if next < len(raw) && !unicode.IsSpace(rune(raw[next])) {
					return nil, invalidQuery("put a space after a quoted operator value")
				}
				// Store it as name:value, like an unquoted operator, so
				// ParseQuery checks both the same way.
				out = append(out, queryToken{value: raw[start:i] + value})
				i = next
				goto tokenDone
			}
			i++
		}
		out = append(out, queryToken{value: raw[start:i]})
	tokenDone:
	}
	return out, nil
}

// readQuoted reads up to the next double quote. Escaped quotes are not
// accepted, because providers treat them differently.
func readQuoted(raw string, i int) (string, int, error) {
	start := i
	for i < len(raw) {
		if raw[i] == '"' {
			return raw[start:i], i + 1, nil
		}
		i++
	}
	return "", 0, invalidQuery("a double quote is not closed")
}

// operatorParts splits a token that starts with something shaped like an
// operator name. ParseQuery decides whether that operator exists.
func operatorParts(token string) (string, string, bool) {
	idx := strings.IndexByte(token, ':')
	if idx <= 0 {
		return "", "", false
	}
	name := token[:idx]
	if !isOperatorName(name) {
		return "", "", false
	}
	return name, token[idx+1:], true
}

func isOperatorName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func invalidQuery(msg string) error {
	return errs.New(errs.CodeInvalidQuery, msg)
}
