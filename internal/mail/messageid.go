package mail

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidMessageID = errors.New("invalid message id")

func NormalizeMessageIDScalar(raw string) (string, error) {
	s := strings.Trim(raw, " \t")
	if s == "" {
		return "", fmt.Errorf("scalar: %w", ErrInvalidMessageID)
	}
	if strings.HasPrefix(s, "<") || strings.HasSuffix(s, ">") {
		if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") || strings.Count(s, "<") != 1 || strings.Count(s, ">") != 1 {
			return "", fmt.Errorf("brackets: %w", ErrInvalidMessageID)
		}
		s = s[1 : len(s)-1]
	}
	if strings.ContainsAny(s, "<> \t\r\n") {
		return "", fmt.Errorf("bytes: %w", ErrInvalidMessageID)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("ascii: %w", ErrInvalidMessageID)
		}
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') || at == len(s)-1 {
		return "", fmt.Errorf("at: %w", ErrInvalidMessageID)
	}
	local, domain := s[:at], s[at+1:]
	if !validMessageIDLocal(local) || !validMessageIDDomain(domain) {
		return "", fmt.Errorf("grammar: %w", ErrInvalidMessageID)
	}
	return "<" + s + ">", nil
}

func ParseMessageIDTokens(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("tokens: %w", ErrInvalidMessageID)
	}
	out := []string{}
	seen := map[string]struct{}{}
	for i := 0; i < len(raw); {
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
			i++
		}
		if i >= len(raw) {
			break
		}
		if raw[i] != '<' {
			return nil, fmt.Errorf("token: %w", ErrInvalidMessageID)
		}
		end := strings.IndexByte(raw[i:], '>')
		if end < 0 {
			return nil, fmt.Errorf("token: %w", ErrInvalidMessageID)
		}
		end += i
		token, err := NormalizeMessageIDScalar(raw[i : end+1])
		if err != nil {
			return nil, err
		}
		if _, ok := seen[token]; ok {
			return nil, fmt.Errorf("duplicate: %w", ErrInvalidMessageID)
		}
		seen[token] = struct{}{}
		out = append(out, token)
		i = end + 1
		if i < len(raw) && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '<' {
			return nil, fmt.Errorf("separator: %w", ErrInvalidMessageID)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tokens: %w", ErrInvalidMessageID)
	}
	return out, nil
}

func validMessageIDLocal(local string) bool {
	if len(local) == 0 || len(local) > 64 || local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return false
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		if strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func validMessageIDDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if !isDomainAlnum(label[0]) || !isDomainAlnum(label[len(label)-1]) {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if isDomainAlnum(c) || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func isDomainAlnum(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
