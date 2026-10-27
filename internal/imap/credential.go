// Package imap is the provider for IMAP and SMTP accounts. It reads and
// changes mail over IMAP and sends it over SMTP. Fastmail accounts use it
// too, with Fastmail's servers filled in.
package imap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/provider"
)

const (
	// MailServerMaxPasswordBytes is the longest password, in bytes, that
	// maildo accepts for an IMAP or SMTP login.
	MailServerMaxPasswordBytes = 255
	envPassword                = "MAILDO_IMAP_PASSWORD"
)

type passwordCredential struct {
	Version         int
	Provider        string
	AccountName     string
	ConfiguredEmail string

	password  []byte
	destroyed atomic.Bool
}

func newPasswordCredential(providerName, account, email string, password []byte) (*passwordCredential, error) {
	if err := validatePasswordBytes(password); err != nil {
		return nil, err
	}
	owned := append([]byte(nil), password...)
	return &passwordCredential{
		Version:         1,
		Provider:        providerName,
		AccountName:     account,
		ConfiguredEmail: email,
		password:        owned,
	}, nil
}

func (c *passwordCredential) Destroy() {
	if c == nil || c.destroyed.Swap(true) {
		return
	}
	zeroBytes(c.password)
	c.password = nil
	c.ConfiguredEmail = ""
}

func (c *passwordCredential) Destroyed() bool {
	return c == nil || c.destroyed.Load()
}

func (c *passwordCredential) passwordString() string {
	if c == nil || c.Destroyed() {
		return ""
	}
	return string(c.password)
}

func (c *passwordCredential) clone() (*passwordCredential, error) {
	if c == nil || c.Destroyed() {
		return nil, provider.ErrCredentialDestroyed
	}
	return newPasswordCredential(c.Provider, c.AccountName, c.ConfiguredEmail, c.password)
}

// CredentialCodec reads and writes the saved password of an IMAP or
// Fastmail account. A saved password only loads for the provider and the
// account it was saved for.
type CredentialCodec struct {
	Provider string
}

func NewCredentialCodec(providerName string) CredentialCodec {
	return CredentialCodec{Provider: providerName}
}

func (c CredentialCodec) ProviderName() string { return c.Provider }

func (c CredentialCodec) Decode(registrar provider.SecretRegistrar, binding auth.CredentialBinding, data []byte) (provider.Credential, error) {
	defer zeroBytes(data)
	passwordRegistered, err := registerPasswordFromOpaque(registrar, data)
	if err != nil {
		return nil, err
	}
	cache, err := decodeCredential(data)
	if err != nil {
		if cache != nil {
			cache.Destroy()
		}
		return nil, err
	}
	if !passwordRegistered {
		err = registerCredentialSecrets(registrar, cache)
	}
	if err != nil {
		cache.Destroy()
		return nil, err
	}
	if cache.Provider != c.ProviderName() || cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return cache, nil
}

func (c CredentialCodec) Encode(binding auth.CredentialBinding, credential provider.Credential) ([]byte, error) {
	cache, ok := credential.(*passwordCredential)
	if !ok || cache == nil || cache.Destroyed() || cache.Provider != c.ProviderName() || cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if !validCredential(cache, binding.Provider, binding.Account, cache.ConfiguredEmail) {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return encodeCredential(cache)
}

func (c CredentialCodec) RegisterSecrets(registrar provider.SecretRegistrar, credential provider.Credential) error {
	cache, ok := credential.(*passwordCredential)
	if !ok || cache == nil || cache.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	return registerCredentialSecrets(registrar, cache)
}

func (c CredentialCodec) LocalHealth(setup maildoconfig.ProviderSetup, binding auth.CredentialBinding, credential provider.Credential) (provider.LocalStatus, error) {
	cache, ok := credential.(*passwordCredential)
	if !ok || cache == nil || cache.Destroyed() || !validCredential(cache, binding.Provider, binding.Account, "") {
		return provider.LocalRepairRequired, nil
	}
	if setup.MailServer == nil || setup.MailServer.Email != cache.ConfiguredEmail {
		return provider.LocalRepairRequired, nil
	}
	if err := setup.ValidateFor(binding.Provider); err != nil {
		return provider.LocalRepairRequired, nil
	}
	return provider.LocalUsable, nil
}

func (c CredentialCodec) RefreshTokenRotated(binding auth.CredentialBinding, before, after provider.Credential) (bool, error) {
	oldCache, ok := before.(*passwordCredential)
	if !ok || oldCache == nil || oldCache.Destroyed() || !validCredential(oldCache, binding.Provider, binding.Account, "") {
		return false, provider.ErrCredentialCapabilityClosed
	}
	newCache, ok := after.(*passwordCredential)
	if !ok || newCache == nil || newCache.Destroyed() || !validCredential(newCache, binding.Provider, binding.Account, "") {
		return false, provider.ErrCredentialCapabilityClosed
	}
	return false, nil
}

func (c CredentialCodec) RebindCredential(registrar provider.SecretRegistrar, raw []byte, oldBinding, newBinding auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	passwordRegistered, registerErr := registerPasswordFromOpaque(registrar, raw)
	if registerErr != nil {
		return auth.CredentialRebindResult{}, registerErr
	}
	cache, err := decodeCredential(raw)
	if err != nil {
		if cache != nil {
			cache.Destroy()
		}
		return auth.CredentialRebindResult{Bytes: append([]byte(nil), raw...), Disposition: auth.CredentialPreservedNeedsRepair}, nil
	}
	defer cache.Destroy()
	if !passwordRegistered {
		err = registerCredentialSecrets(registrar, cache)
	}
	if err != nil {
		return auth.CredentialRebindResult{}, err
	}
	if cache.Provider != oldBinding.Provider || cache.AccountName != oldBinding.Account || cache.Provider != c.ProviderName() {
		return auth.CredentialRebindResult{Bytes: append([]byte(nil), raw...), Disposition: auth.CredentialPreservedNeedsRepair}, nil
	}
	cache.Provider = newBinding.Provider
	cache.AccountName = newBinding.Account
	out, err := encodeCredential(cache)
	if err != nil {
		return auth.CredentialRebindResult{}, err
	}
	return auth.CredentialRebindResult{Bytes: out, Disposition: auth.CredentialReboundUsable}, nil
}

func validCredential(cache *passwordCredential, providerName, account, email string) bool {
	if cache == nil || cache.Destroyed() || cache.Version != 1 || cache.Provider != providerName || cache.AccountName != account {
		return false
	}
	if canonical, err := maildoconfig.NormalizeConfiguredEmail(cache.ConfiguredEmail); err != nil || canonical != cache.ConfiguredEmail {
		return false
	}
	if email != "" && cache.ConfiguredEmail != email {
		return false
	}
	return validatePasswordBytes(cache.password) == nil
}

func validatePasswordBytes(password []byte) error {
	if len(password) == 0 || len(password) > MailServerMaxPasswordBytes || !utf8.Valid(password) || bytes.IndexByte(password, 0) >= 0 || bytes.IndexAny(password, "\r\n") >= 0 {
		return provider.ErrCredentialCapabilityClosed
	}
	return nil
}

func registerCredentialSecrets(registrar provider.SecretRegistrar, cache *passwordCredential) error {
	if registrar == nil || cache == nil || cache.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	password := cache.passwordString()
	if password == "" {
		return provider.ErrCredentialCapabilityClosed
	}
	return registrar.RegisterSecret(password)
}

func registerPasswordFromOpaque(registrar provider.SecretRegistrar, data []byte) (bool, error) {
	if registrar == nil {
		return false, provider.ErrCredentialCapabilityClosed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return false, nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return false, nil
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false, nil
		}
		key, ok := tok.(string)
		if !ok {
			return false, nil
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return false, nil
		}
		if key != "password" {
			continue
		}
		var password string
		defer zeroBytes(raw)
		if err := decodeStrictJSONString(raw, &password); err != nil {
			return false, nil
		}
		if err := registrar.RegisterSecret(password); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

type credentialJSON struct {
	Version         int    `json:"version"`
	Provider        string `json:"provider"`
	AccountName     string `json:"account_name"`
	ConfiguredEmail string `json:"configured_email"`
	Password        string `json:"password"`
}

func decodeCredential(data []byte) (*passwordCredential, error) {
	fields, err := strictCredentialFields(data)
	if err != nil {
		return nil, err
	}
	raw := credentialJSON{}
	if err := decodeStrictJSONString(fields["provider"], &raw.Provider); err != nil {
		return nil, err
	}
	if err := decodeStrictJSONString(fields["account_name"], &raw.AccountName); err != nil {
		return nil, err
	}
	if err := decodeStrictJSONString(fields["configured_email"], &raw.ConfiguredEmail); err != nil {
		return nil, err
	}
	if err := decodeStrictJSONString(fields["password"], &raw.Password); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(fields["version"], &raw.Version); err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	cache, err := newPasswordCredential(raw.Provider, raw.AccountName, raw.ConfiguredEmail, []byte(raw.Password))
	if err != nil {
		return nil, err
	}
	if raw.Version != 1 {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if canonical, err := maildoconfig.NormalizeConfiguredEmail(raw.ConfiguredEmail); err != nil || canonical != raw.ConfiguredEmail {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return cache, nil
}

func encodeCredential(cache *passwordCredential) ([]byte, error) {
	raw := credentialJSON{
		Version:         1,
		Provider:        cache.Provider,
		AccountName:     cache.AccountName,
		ConfiguredEmail: cache.ConfiguredEmail,
		Password:        cache.passwordString(),
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	data = append(data, '\n')
	return data, nil
}

func strictCredentialFields(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, provider.ErrCredentialCapabilityClosed
		}
		key, ok := tok.(string)
		if !ok {
			return nil, provider.ErrCredentialCapabilityClosed
		}
		if _, exists := fields[key]; exists {
			return nil, provider.ErrCredentialCapabilityClosed
		}
		switch key {
		case "version", "provider", "account_name", "configured_email", "password":
		default:
			return nil, provider.ErrCredentialCapabilityClosed
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, provider.ErrCredentialCapabilityClosed
		}
		fields[key] = raw
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '}' {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	for _, required := range []string{"version", "provider", "account_name", "configured_email", "password"} {
		if _, ok := fields[required]; !ok {
			return nil, provider.ErrCredentialCapabilityClosed
		}
	}
	return fields, nil
}

func decodeStrictJSONString(raw json.RawMessage, out *string) error {
	if !validJSONSurrogates(raw) {
		return provider.ErrCredentialCapabilityClosed
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return provider.ErrCredentialCapabilityClosed
	}
	if !utf8.ValidString(*out) {
		return provider.ErrCredentialCapabilityClosed
	}
	return nil
}

func validJSONSurrogates(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw)-1 {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		v, ok := parseHex4(raw[i+1 : i+5])
		if !ok {
			return false
		}
		if v >= 0xD800 && v <= 0xDBFF {
			if i+11 >= len(raw) || raw[i+5] != '\\' || raw[i+6] != 'u' {
				return false
			}
			low, ok := parseHex4(raw[i+7 : i+11])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 10
			continue
		}
		if v >= 0xDC00 && v <= 0xDFFF {
			return false
		}
		i += 4
	}
	return true
}

func parseHex4(b []byte) (rune, bool) {
	if len(b) != 4 {
		return 0, false
	}
	var out rune
	for _, c := range b {
		out <<= 4
		switch {
		case c >= '0' && c <= '9':
			out |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			out |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			out |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return out, true
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func secretUsageError(label string) error {
	return fmt.Errorf("%s is invalid", strings.TrimSuffix(label, ":"))
}
