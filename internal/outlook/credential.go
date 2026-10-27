package outlook

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/provider"
)

// TokenCache is a saved Outlook login. Only this package reads its fields;
// the shared auth storage keeps the encoded bytes as they are.
type TokenCache struct {
	Version           int       `json:"version"`
	Provider          string    `json:"provider"`
	AccountName       string    `json:"account_name"`
	Authority         string    `json:"authority,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	Scopes            []string  `json:"scopes,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	VerifiedEmail     string    `json:"verified_email,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	ProviderAccountID string    `json:"provider_account_id,omitempty"`
	TenantID          string    `json:"tenant_id,omitempty"`

	accessToken  secretBytes
	refreshToken secretBytes
	destroyed    atomic.Bool
}

type TokenCacheData struct {
	Version           int       `json:"version"`
	Provider          string    `json:"provider"`
	AccountName       string    `json:"account_name"`
	Authority         string    `json:"authority,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	Scopes            []string  `json:"scopes,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	VerifiedEmail     string    `json:"verified_email,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	ProviderAccountID string    `json:"provider_account_id,omitempty"`
	TenantID          string    `json:"tenant_id,omitempty"`
}

type secretBytes []byte

func secretFromString(value string) secretBytes {
	if value == "" {
		return nil
	}
	return append(secretBytes(nil), value...)
}

func (s secretBytes) String() string {
	if len(s) == 0 {
		return ""
	}
	return string(s)
}

func (s *secretBytes) Set(value string) {
	s.Destroy()
	*s = secretFromString(value)
}

func (s *secretBytes) Destroy() {
	if s == nil {
		return
	}
	for i := range *s {
		(*s)[i] = 0
	}
	*s = nil
}

func NewTokenCache(data TokenCacheData) *TokenCache {
	cache := &TokenCache{
		Version:           data.Version,
		Provider:          data.Provider,
		AccountName:       data.AccountName,
		Authority:         data.Authority,
		ClientID:          data.ClientID,
		Scopes:            append([]string(nil), data.Scopes...),
		TokenType:         data.TokenType,
		ExpiresAt:         data.ExpiresAt,
		VerifiedEmail:     data.VerifiedEmail,
		DisplayName:       data.DisplayName,
		ProviderAccountID: data.ProviderAccountID,
		TenantID:          data.TenantID,
	}
	cache.accessToken.Set(data.AccessToken)
	cache.refreshToken.Set(data.RefreshToken)
	return cache
}

func (c *TokenCache) Snapshot() TokenCacheData {
	if c == nil {
		return TokenCacheData{}
	}
	return TokenCacheData{
		Version:           c.Version,
		Provider:          c.Provider,
		AccountName:       c.AccountName,
		Authority:         c.Authority,
		ClientID:          c.ClientID,
		Scopes:            append([]string(nil), c.Scopes...),
		TokenType:         c.TokenType,
		AccessToken:       c.AccessTokenValue(),
		RefreshToken:      c.RefreshTokenValue(),
		ExpiresAt:         c.ExpiresAt,
		VerifiedEmail:     c.VerifiedEmail,
		DisplayName:       c.DisplayName,
		ProviderAccountID: c.ProviderAccountID,
		TenantID:          c.TenantID,
	}
}

func (c *TokenCache) AccessTokenValue() string {
	if c == nil || c.Destroyed() {
		return ""
	}
	return c.accessToken.String()
}

func (c *TokenCache) RefreshTokenValue() string {
	if c == nil || c.Destroyed() {
		return ""
	}
	return c.refreshToken.String()
}

func (c *TokenCache) SetAccessToken(value string) {
	if c == nil || c.Destroyed() {
		return
	}
	c.accessToken.Set(value)
}

func (c *TokenCache) SetRefreshToken(value string) {
	if c == nil || c.Destroyed() {
		return
	}
	c.refreshToken.Set(value)
}

func (c *TokenCache) Destroy() {
	if c == nil || c.destroyed.Swap(true) {
		return
	}
	c.accessToken.Destroy()
	c.refreshToken.Destroy()
	c.TokenType = ""
	c.ClientID = ""
	c.Scopes = nil
}

func (c *TokenCache) Destroyed() bool {
	return c == nil || c.destroyed.Load()
}

// IsExpired reports whether the access token expires within skew of now and
// should be renewed.
func (c *TokenCache) IsExpired(now time.Time, skew time.Duration) bool {
	if c == nil || c.ExpiresAt.IsZero() || c.Destroyed() {
		return true
	}
	return now.Add(skew).After(c.ExpiresAt)
}

func (c *TokenCache) clone() *TokenCache {
	if c == nil {
		return nil
	}
	return NewTokenCache(c.Snapshot())
}

type CredentialCodec struct{}

func (CredentialCodec) ProviderName() string { return "outlook" }

func (CredentialCodec) Decode(registrar provider.SecretRegistrar, binding auth.CredentialBinding, data []byte) (provider.Credential, error) {
	loose, looseErr := decodeTokenCache(data, false)
	if looseErr == nil && loose != nil {
		if err := registerCacheSecrets(registrar, loose); err != nil {
			loose.Destroy()
			return nil, err
		}
		defer loose.Destroy()
	}
	cache, err := decodeTokenCache(data, true)
	if err != nil {
		if cache != nil {
			cache.Destroy()
		}
		return nil, err
	}
	if cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return cache, nil
}

func (CredentialCodec) Encode(binding auth.CredentialBinding, credential provider.Credential) ([]byte, error) {
	cache, ok := credential.(*TokenCache)
	if !ok || cache == nil || cache.Destroyed() || cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return encodeTokenCache(cache)
}

func (CredentialCodec) RegisterSecrets(registrar provider.SecretRegistrar, credential provider.Credential) error {
	cache, ok := credential.(*TokenCache)
	if !ok || cache == nil || cache.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	return registerCacheSecrets(registrar, cache)
}

func (CredentialCodec) LocalHealth(setup maildoconfig.ProviderSetup, binding auth.CredentialBinding, credential provider.Credential) (provider.LocalStatus, error) {
	cache, ok := credential.(*TokenCache)
	if !ok || cache == nil || cache.Destroyed() {
		return provider.LocalRepairRequired, nil
	}
	if !localUsableCredential(cache, binding.Provider, binding.Account, "", "") {
		return provider.LocalRepairRequired, nil
	}
	if strings.TrimSpace(cache.ClientID) == "" && (setup.Outlook == nil || strings.TrimSpace(setup.Outlook.ClientID) == "") {
		return provider.LocalRepairRequired, nil
	}
	return provider.LocalUsable, nil
}

func (CredentialCodec) RefreshTokenRotated(binding auth.CredentialBinding, before, after provider.Credential) (bool, error) {
	oldCache, ok := before.(*TokenCache)
	if !ok || oldCache == nil || oldCache.Destroyed() || oldCache.Provider != binding.Provider || oldCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	newCache, ok := after.(*TokenCache)
	if !ok || newCache == nil || newCache.Destroyed() || newCache.Provider != binding.Provider || newCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	return newCache.RefreshTokenValue() != "" && newCache.RefreshTokenValue() != oldCache.RefreshTokenValue(), nil
}

func (CredentialCodec) RebindCredential(registrar provider.SecretRegistrar, raw []byte, oldBinding, newBinding auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	loose, looseErr := decodeTokenCache(raw, false)
	if looseErr == nil && loose != nil {
		if err := registerCacheSecrets(registrar, loose); err != nil {
			loose.Destroy()
			return auth.CredentialRebindResult{}, err
		}
		defer loose.Destroy()
	}
	if looseErr != nil || loose.Provider != oldBinding.Provider || loose.AccountName != oldBinding.Account {
		return preservedCredential(raw), nil
	}
	if loose.Version != 1 {
		return preservedCredential(raw), nil
	}
	cache, err := decodeTokenCache(raw, true)
	if err == nil {
		defer cache.Destroy()
		cache.Provider = newBinding.Provider
		cache.AccountName = newBinding.Account
		out, encodeErr := encodeTokenCache(cache)
		if encodeErr != nil {
			return auth.CredentialRebindResult{}, provider.ErrCredentialCapabilityClosed
		}
		disposition := auth.CredentialReboundNeedsRepair
		if localUsableCredential(cache, newBinding.Provider, newBinding.Account, "", "") {
			disposition = auth.CredentialReboundUsable
		}
		return auth.CredentialRebindResult{Bytes: out, Disposition: disposition}, nil
	}
	out, err := rebindRawCredential(raw, newBinding)
	if err != nil {
		return preservedCredential(raw), nil
	}
	return auth.CredentialRebindResult{Bytes: out, Disposition: auth.CredentialReboundNeedsRepair}, nil
}

func preservedCredential(raw []byte) auth.CredentialRebindResult {
	return auth.CredentialRebindResult{
		Bytes:       append([]byte(nil), raw...),
		Disposition: auth.CredentialPreservedNeedsRepair,
	}
}

func rebindRawCredential(raw []byte, newBinding auth.CredentialBinding) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	providerRaw, err := json.Marshal(newBinding.Provider)
	if err != nil {
		return nil, err
	}
	accountRaw, err := json.Marshal(newBinding.Account)
	if err != nil {
		return nil, err
	}
	obj["provider"] = providerRaw
	obj["account_name"] = accountRaw
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	return data, nil
}

func decodeTokenCache(data []byte, closed bool) (*TokenCache, error) {
	if hasDuplicateTopLevelJSONKey(data) {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if closed {
		dec.DisallowUnknownFields()
	}
	var raw TokenCacheData
	if err := dec.Decode(&raw); err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return NewTokenCache(raw), nil
}

func encodeTokenCache(cache *TokenCache) ([]byte, error) {
	data, err := json.MarshalIndent(cache.Snapshot(), "", "  ")
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	data = append(data, '\n')
	return data, nil
}

func registerCacheSecrets(registrar provider.SecretRegistrar, cache *TokenCache) error {
	if registrar == nil || cache == nil {
		return nil
	}
	accessToken := cache.AccessTokenValue()
	refreshToken := cache.RefreshTokenValue()
	if err := registrar.RegisterSecret(accessToken); err != nil {
		return err
	}
	if err := registrar.RegisterSecret(refreshToken); err != nil {
		return err
	}
	if accessToken != "" {
		if err := registrar.RegisterSecret(cache.TokenType + " " + accessToken); err != nil {
			return err
		}
		if err := registrar.RegisterSecret("Bearer " + accessToken); err != nil {
			return err
		}
	}
	return nil
}

func hasDuplicateTopLevelJSONKey(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		key, ok := tok.(string)
		if !ok {
			return false
		}
		if seen[key] {
			return true
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return false
		}
	}
	return false
}
