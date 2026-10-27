// Package gmail is maildo's provider for the Gmail API. The user signs in
// with their own Google OAuth app, and Gmail labels show up as folders.
package gmail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	netmail "net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimeparam"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"golang.org/x/net/html/charset"
)

const (
	envClientID     = "MAILDO_GMAIL_CLIENT_ID"
	envClientSecret = "MAILDO_GMAIL_CLIENT_SECRET"

	requiredScope = "https://www.googleapis.com/auth/gmail.modify"

	defaultAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL     = "https://oauth2.googleapis.com/token"
	defaultAPIBaseURL   = "https://gmail.googleapis.com/gmail/v1/users/me"
	defaultBindHost     = "127.0.0.1"
)

// Options configures the Gmail provider. Tests set the URLs; when they are
// empty, the provider uses Google's OAuth and Gmail API endpoints.
type Options struct {
	AuthorizeURL    string
	TokenURL        string
	APIBaseURL      string
	HTTPClient      *http.Client
	Now             func() time.Time
	RefreshSkew     time.Duration
	LoopbackTimeout time.Duration
	BindHost        string
}

// Provider is the Gmail implementation of provider.Provider.
type Provider struct {
	opts Options
}

// New returns a Gmail provider and fills in defaults for empty options.
func New(opts Options) *Provider {
	if opts.AuthorizeURL == "" {
		opts.AuthorizeURL = defaultAuthorizeURL
	}
	if opts.TokenURL == "" {
		opts.TokenURL = defaultTokenURL
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = defaultAPIBaseURL
	}
	opts.APIBaseURL = strings.TrimRight(opts.APIBaseURL, "/")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RefreshSkew == 0 {
		opts.RefreshSkew = 5 * time.Minute
	}
	if opts.LoopbackTimeout == 0 {
		opts.LoopbackTimeout = 5 * time.Minute
	}
	if opts.BindHost == "" {
		opts.BindHost = defaultBindHost
	}
	return &Provider{opts: opts}
}

func (p *Provider) Name() string { return "gmail" }

func (p *Provider) now() time.Time { return p.opts.Now().UTC() }

// Capabilities says what maildo can do with a Gmail account.
func (p *Provider) Capabilities(context.Context) (provider.Capabilities, error) {
	caps := make(provider.Capabilities, len(provider.AllCapabilities()))
	for _, cap := range provider.AllCapabilities() {
		caps[cap] = provider.CapSupportedNotImpl
	}
	for _, cap := range []provider.Capability{
		provider.CapMessagesSearch,
		provider.CapMessagesList,
		provider.CapMessagesRead,
		provider.CapThreadsRead,
		provider.CapFoldersRead,
		provider.CapFoldersMutate,
		provider.CapDraftCreate,
		provider.CapDraftRead,
		provider.CapDraftUpdate,
		provider.CapDraftSend,
		provider.CapSendDirect,
		provider.CapReplyDraftCreate,
		provider.CapForwardDraftCreate,
		provider.CapAttachmentsRead,
		provider.CapAttachmentsMutate,
		provider.CapMessagesMove,
		provider.CapMessagesCopy,
		provider.CapMessagesArchive,
		provider.CapMessagesDelete,
		provider.CapMessagesRestore,
		provider.CapMessagesJunk,
		provider.CapMessagesMarkRead,
		provider.CapMessagesFlag,
		provider.CapCategoriesRead,
		provider.CapCategoriesMutate,
	} {
		caps[cap] = provider.CapImplemented
	}
	for _, cap := range []provider.Capability{
		provider.CapRulesRead,
		provider.CapRulesMutate,
		provider.CapArchivePolicyReport,
	} {
		caps[cap] = provider.CapUnsupported
	}
	return caps, nil
}

type SetupParser struct{}

func (SetupParser) ConfigureFlags(cmd *cobra.Command) error {
	cmd.Flags().String("gmail-client-id", "", "Client ID of your own Google OAuth app (type Desktop app)")
	return nil
}

func (SetupParser) Resolve(_ context.Context, ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	setup := maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{}}
	source := "none"
	if ctx.Existing.Gmail != nil && ctx.Existing.Gmail.ClientID != "" {
		if err := validateClientIDCandidate(ctx.Existing.Gmail.ClientID, "stored Gmail client ID is invalid"); err != nil {
			return provider.SetupResolution{}, err
		}
		setup.Gmail.ClientID = ctx.Existing.Gmail.ClientID
		source = "config"
	}
	if ctx.LookupEnv != nil {
		if value, ok, err := ctx.LookupEnv(envClientID); err != nil {
			return provider.SetupResolution{}, err
		} else if ok {
			if err := validateClientIDCandidate(value, "MAILDO_GMAIL_CLIENT_ID is invalid"); err != nil {
				return provider.SetupResolution{}, err
			}
			setup.Gmail.ClientID = value
			source = "environment"
		}
	}
	if flag, ok := ctx.Flags.Lookup("gmail-client-id"); ok && flag.Changed {
		if err := validateClientIDCandidate(flag.Value, "--gmail-client-id is invalid"); err != nil {
			return provider.SetupResolution{}, err
		}
		setup.Gmail.ClientID = flag.Value
		source = "flag"
	}
	configured := "false"
	if setup.Gmail.ClientID != "" {
		configured = "true"
	}
	if setup.Gmail.ClientID == "" {
		return provider.SetupResolution{}, errs.New(errs.CodeAuthAppConfigMissing, "the Gmail API needs the client id of your own Google Cloud OAuth app. Create one (see https://github.com/vincentsch/maildo/blob/main/docs/providers/gmail.md), then run: maildo config set gmail.client_id <id>. To log in without an app, use an app password: maildo login --email you@gmail.com")
	}
	return provider.SetupResolution{
		Setup: setup,
		Preview: []provider.SetupPreviewField{
			{Name: "client_id_configured", Value: configured},
			{Name: "client_id_source", Value: source},
		},
	}, nil
}

func (SetupParser) PromptMissing(_ context.Context, _ provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	return resolution, nil
}

func validateClientIDCandidate(value, message string) error {
	if value == "" || strings.Trim(value, " \t\r\n") == "" {
		return rungrad.NewError(rungrad.ExitUsage, message)
	}
	if strings.Trim(value, " \t\r\n") != value {
		return rungrad.NewError(rungrad.ExitUsage, message)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return rungrad.NewError(rungrad.ExitUsage, message)
		}
	}
	if !utf8.ValidString(value) {
		return rungrad.NewError(rungrad.ExitUsage, message)
	}
	return nil
}

var _ provider.SetupParser = SetupParser{}

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

// TokenCache is a saved Gmail login: the tokens, the client secret and the
// address they belong to.
type TokenCache struct {
	Version       int       `json:"version"`
	Provider      string    `json:"provider"`
	AccountName   string    `json:"account_name"`
	VerifiedEmail string    `json:"verified_email"`
	ClientID      string    `json:"client_id"`
	Scopes        []string  `json:"scopes"`
	TokenType     string    `json:"token_type"`
	ExpiresAt     time.Time `json:"expires_at"`

	clientSecret secretBytes
	accessToken  secretBytes
	refreshToken secretBytes
	destroyed    atomic.Bool
}

type TokenCacheData struct {
	Version       int       `json:"version"`
	Provider      string    `json:"provider"`
	AccountName   string    `json:"account_name"`
	VerifiedEmail string    `json:"verified_email"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	Scopes        []string  `json:"scopes"`
	TokenType     string    `json:"token_type"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func NewTokenCache(data TokenCacheData) *TokenCache {
	cache := &TokenCache{
		Version:       data.Version,
		Provider:      data.Provider,
		AccountName:   data.AccountName,
		VerifiedEmail: data.VerifiedEmail,
		ClientID:      data.ClientID,
		Scopes:        append([]string(nil), data.Scopes...),
		TokenType:     data.TokenType,
		ExpiresAt:     data.ExpiresAt,
	}
	cache.clientSecret.Set(data.ClientSecret)
	cache.accessToken.Set(data.AccessToken)
	cache.refreshToken.Set(data.RefreshToken)
	return cache
}

func (c *TokenCache) Snapshot() TokenCacheData {
	if c == nil {
		return TokenCacheData{}
	}
	return TokenCacheData{
		Version:       c.Version,
		Provider:      c.Provider,
		AccountName:   c.AccountName,
		VerifiedEmail: c.VerifiedEmail,
		ClientID:      c.ClientID,
		ClientSecret:  c.ClientSecretValue(),
		Scopes:        append([]string(nil), c.Scopes...),
		TokenType:     c.TokenType,
		AccessToken:   c.AccessTokenValue(),
		RefreshToken:  c.RefreshTokenValue(),
		ExpiresAt:     c.ExpiresAt,
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

func (c *TokenCache) ClientSecretValue() string {
	if c == nil || c.Destroyed() {
		return ""
	}
	return c.clientSecret.String()
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
	c.clientSecret.Destroy()
	c.accessToken.Destroy()
	c.refreshToken.Destroy()
	c.ClientID = ""
	c.TokenType = ""
	c.Scopes = nil
}

func (c *TokenCache) Destroyed() bool { return c == nil || c.destroyed.Load() }

func (c *TokenCache) IsExpired(now time.Time, skew time.Duration) bool {
	if c == nil || c.Destroyed() || c.ExpiresAt.IsZero() {
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

func (CredentialCodec) ProviderName() string { return "gmail" }

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
	if !validCredential(cache, binding.Provider, binding.Account, "", "") {
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
	if !validCredential(cache, binding.Provider, binding.Account, "", "") {
		return provider.LocalRepairRequired, nil
	}
	if setup.Gmail != nil && setup.Gmail.ClientID != "" && setup.Gmail.ClientID != cache.ClientID {
		return provider.LocalRepairRequired, nil
	}
	if setup.Gmail == nil && cache.ClientID == "" {
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
	if err != nil {
		return rebindRawCredential(raw, newBinding)
	}
	defer cache.Destroy()
	cache.Provider = newBinding.Provider
	cache.AccountName = newBinding.Account
	out, err := encodeTokenCache(cache)
	if err != nil {
		return auth.CredentialRebindResult{}, provider.ErrCredentialCapabilityClosed
	}
	disposition := auth.CredentialReboundNeedsRepair
	if validCredential(cache, newBinding.Provider, newBinding.Account, "", "") {
		disposition = auth.CredentialReboundUsable
	}
	return auth.CredentialRebindResult{Bytes: out, Disposition: disposition}, nil
}

func preservedCredential(raw []byte) auth.CredentialRebindResult {
	return auth.CredentialRebindResult{Bytes: append([]byte(nil), raw...), Disposition: auth.CredentialPreservedNeedsRepair}
}

func rebindRawCredential(raw []byte, newBinding auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj) == 0 {
		return preservedCredential(raw), nil
	}
	providerRaw, err := json.Marshal(newBinding.Provider)
	if err != nil {
		return auth.CredentialRebindResult{}, provider.ErrCredentialCapabilityClosed
	}
	accountRaw, err := json.Marshal(newBinding.Account)
	if err != nil {
		return auth.CredentialRebindResult{}, provider.ErrCredentialCapabilityClosed
	}
	obj["provider"] = providerRaw
	obj["account_name"] = accountRaw
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return auth.CredentialRebindResult{}, provider.ErrCredentialCapabilityClosed
	}
	data = append(data, '\n')
	return auth.CredentialRebindResult{Bytes: data, Disposition: auth.CredentialReboundNeedsRepair}, nil
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
	cache := NewTokenCache(raw)
	if closed && !validCredential(cache, raw.Provider, raw.AccountName, "", "") {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return cache, nil
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
	seen := map[string]bool{}
	for _, secret := range []string{cache.ClientSecretValue(), cache.AccessTokenValue(), cache.RefreshTokenValue()} {
		if secret == "" {
			continue
		}
		seen[secret] = true
		if err := registrar.RegisterSecret(secret); err != nil {
			return err
		}
	}
	if access := cache.AccessTokenValue(); access != "" {
		for _, header := range []string{cache.TokenType + " " + access, "Bearer " + access} {
			if seen[header] {
				continue
			}
			seen[header] = true
			if err := registrar.RegisterSecret(header); err != nil {
				return err
			}
		}
	}
	return nil
}

func validCredential(cache *TokenCache, providerName, account, expectedEmail, expectedClientID string) bool {
	if cache == nil || cache.Destroyed() {
		return false
	}
	if cache.Version != 1 || cache.Provider != providerName || cache.AccountName != account {
		return false
	}
	if cache.Provider != "gmail" || cache.ClientID == "" || cache.AccessTokenValue() == "" || cache.RefreshTokenValue() == "" || cache.ExpiresAt.IsZero() {
		return false
	}
	if !strings.EqualFold(cache.TokenType, "Bearer") {
		return false
	}
	if !sameScopeSet(cache.Scopes, []string{requiredScope}) {
		return false
	}
	if normalizeEmail(cache.VerifiedEmail) == "" {
		return false
	}
	if expectedEmail != "" && normalizeEmail(cache.VerifiedEmail) != normalizeEmail(expectedEmail) {
		return false
	}
	if expectedClientID != "" && cache.ClientID != expectedClientID {
		return false
	}
	return true
}

func sameScopeSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] || (i > 0 && g[i] == g[i-1]) {
			return false
		}
	}
	return true
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

func decodeJSONResponse(account string, body []byte, out any) error {
	if hasDuplicateJSONKey(body) {
		return providerDataError(account, "invalid_json")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return providerDataError(account, "invalid_json")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return providerDataError(account, "invalid_json")
	}
	return nil
}

func hasDuplicateJSONKey(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dup, ok := jsonValueHasDuplicateKey(dec)
	return ok && dup
}

func jsonValueHasDuplicateKey(dec *json.Decoder) (bool, bool) {
	tok, err := dec.Token()
	if err != nil {
		return false, false
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return false, true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return false, false
			}
			key, ok := tok.(string)
			if !ok {
				return false, false
			}
			if seen[key] {
				return true, true
			}
			seen[key] = true
			dup, ok := jsonValueHasDuplicateKey(dec)
			if !ok || dup {
				return dup, ok
			}
		}
		tok, err := dec.Token()
		end, ok := tok.(json.Delim)
		return false, err == nil && ok && end == '}'
	case '[':
		for dec.More() {
			dup, ok := jsonValueHasDuplicateKey(dec)
			if !ok || dup {
				return dup, ok
			}
		}
		tok, err := dec.Token()
		end, ok := tok.(json.Delim)
		return false, err == nil && ok && end == ']'
	default:
		return false, false
	}
}

type tokenResponse struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	TokenType    string
	Scopes       []string
}

// InteractiveAuth signs the user in through the browser. Google sends the
// result to a short-lived listener on this computer (OAuth with PKCE).
func (p *Provider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	if ac.Operation != provider.InteractiveLogin && ac.Operation != provider.InteractiveRepair {
		return provider.InteractiveAuthResult{}, provider.ErrProviderRuntimeDenied
	}
	if ac.Flow == provider.InteractiveAuthFlowDeviceCode {
		return provider.InteractiveAuthResult{}, rungrad.NewError(rungrad.ExitUsage, "--device-code is not valid for provider \"gmail\"")
	}
	clientID, err := p.resolveClientID(ac.Setup)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	clientSecret, err := p.resolveClientSecret(ctx, ac, clientID)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	// Google refuses the sign-in of a Desktop app without its secret, but
	// only after the user has clicked through the consent screen. Say so
	// before opening the browser.
	if clientSecret == "" {
		return provider.InteractiveAuthResult{}, errs.New(errs.CodeAuthAppConfigMissing, "the Gmail sign-in also needs the client secret of your Google Cloud OAuth app. Put it in MAILDO_GMAIL_CLIENT_SECRET for this command, for example: MAILDO_GMAIL_CLIENT_SECRET=<secret> maildo login --provider gmail. maildo saves it with the login, so later commands do not need it")
	}
	cache, err := p.authCodeLogin(ctx, ac.Runtime, ac.Account, clientID, clientSecret, ac.Operation)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	cache.AccountName = ac.Account
	if err := registerCacheSecrets(ac.Runtime, cache); err != nil {
		cache.Destroy()
		return provider.InteractiveAuthResult{}, err
	}
	return provider.InteractiveAuthResult{
		Setup:      ac.Setup.Clone(),
		Credential: cache,
		Identity: provider.Identity{
			Email:     cache.VerifiedEmail,
			Assurance: provider.IdentityProviderVerified,
		},
	}, nil
}

func (p *Provider) resolveClientID(setup maildoconfig.ProviderSetup) (string, error) {
	if setup.Gmail != nil && setup.Gmail.ClientID != "" {
		return setup.Gmail.ClientID, nil
	}
	return "", errs.New(errs.CodeAuthAppConfigMissing, "the Gmail API needs the client id of your own Google Cloud OAuth app. Create one (see https://github.com/vincentsch/maildo/blob/main/docs/providers/gmail.md), then run: maildo config set gmail.client_id <id>. To log in without an app, use an app password: maildo login --email you@gmail.com")
}

func (p *Provider) resolveClientSecret(ctx context.Context, ac provider.InteractiveAuthContext, clientID string) (string, error) {
	if ac.Runtime == nil {
		return "", provider.ErrProviderRuntimeDenied
	}
	if value, ok, err := ac.Runtime.LookupSecretEnv(envClientSecret); err != nil {
		return "", err
	} else if ok {
		if value != "" {
			if err := ac.Runtime.RegisterSecret(value); err != nil {
				return "", err
			}
		}
		return value, nil
	}
	var inherited string
	if ac.Existing == nil {
		return "", nil
	}
	_, err := ac.Existing.WithBytes(ctx, func(raw []byte) error {
		cred, err := CredentialCodec{}.Decode(ac.Runtime, auth.CredentialBinding{Provider: "gmail", Account: ac.Account}, raw)
		if err != nil {
			return nil
		}
		defer cred.Destroy()
		cache, ok := cred.(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() || cache.ClientID != clientID {
			return nil
		}
		inherited = cache.ClientSecretValue()
		return nil
	})
	if err != nil {
		return "", err
	}
	return inherited, nil
}

func (p *Provider) authCodeLogin(ctx context.Context, runtime provider.InteractiveProviderRuntime, account, clientID, clientSecret string, op provider.InteractiveAuthOperation) (*TokenCache, error) {
	verifier, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}
	state, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}
	if err := runtime.RegisterSecret(verifier); err != nil {
		return nil, err
	}
	if err := runtime.RegisterSecret(state); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(p.opts.BindHost, "0"))
	if err != nil {
		return nil, errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthLoopbackUnavailable)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := "http://" + net.JoinHostPort(p.opts.BindHost, strconv.Itoa(port))
	callbacks := make(chan oauthCallback, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb := validateOAuthCallback(r, state, listener.Addr().String(), account)
		select {
		case callbacks <- cb:
		default:
			cb = oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>maildo sign-in</title><p>You can close this window and go back to the terminal. maildo shows there whether the sign-in worked.</p>")
	})}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case callbacks <- oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthLoopbackUnavailable)}:
			default:
			}
		}
	}()
	authorizeURL, err := p.authorizeURL(account, clientID, redirectURI, state, verifier)
	if err != nil {
		return nil, err
	}
	if err := provider.OpenSignInPage(ctx, runtime, authorizeURL, p.opts.LoopbackTimeout); err != nil {
		return nil, errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthBrowserUnavailable)
	}
	timer := time.NewTimer(p.opts.LoopbackTimeout)
	defer timer.Stop()
	var cb oauthCallback
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackTimeout)
	case cb = <-callbacks:
	}
	if cb.Err != nil {
		return nil, cb.Err
	}
	if cb.Code != "" {
		if err := runtime.RegisterSecret(cb.Code); err != nil {
			return nil, err
		}
	}
	if cb.Denied {
		return nil, errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthAuthorizationDenied)
	}
	form := url.Values{
		"client_id":     {clientID},
		"code":          {cb.Code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	token, err := p.exchangeToken(ctx, account, form, true, []string{requiredScope}, runtime)
	if err != nil {
		return nil, p.interactiveTokenError(account, op, err)
	}
	if !sameScopeSet(token.Scopes, []string{requiredScope}) {
		if op == provider.InteractiveLogin {
			return nil, errs.NewPreCommitScopeMissing(account, errs.ProviderGmail)
		}
		return nil, repairRequired(account, "scope_missing")
	}
	email, err := p.getProfile(ctx, token.AccessToken, account, op == provider.InteractiveLogin)
	if err != nil {
		return nil, err
	}
	return NewTokenCache(TokenCacheData{
		Version:       1,
		Provider:      "gmail",
		AccountName:   account,
		VerifiedEmail: email,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		Scopes:        []string{requiredScope},
		TokenType:     "Bearer",
		AccessToken:   token.AccessToken,
		RefreshToken:  token.RefreshToken,
		ExpiresAt:     token.ExpiresAt,
	}), nil
}

type oauthCallback struct {
	Code   string
	Denied bool
	Err    error
}

func validateOAuthCallback(r *http.Request, expectedState, expectedHost, account string) oauthCallback {
	if r == nil || r.Method != http.MethodGet || r.URL == nil || r.URL.Path != "/" || r.URL.RawPath != "" || r.URL.Fragment != "" || r.Host != expectedHost {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	if len(r.URL.RawQuery) > int(limits.OAuthMaxCallbackQueryBytes) || strings.Contains(r.RequestURI, "#") || strings.HasPrefix(r.RequestURI, "http://") || strings.HasPrefix(r.RequestURI, "https://") {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	for key, values := range q {
		if len(values) != 1 {
			return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
		}
		switch key {
		case "code", "state", "error", "scope", "authuser", "prompt", "hd", "error_description", "error_uri":
		default:
		}
	}
	state := q.Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	code := q.Get("code")
	oauthErr := q.Get("error")
	if code != "" && oauthErr != "" {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	if code == "" && oauthErr == "" {
		return oauthCallback{Err: errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthCallbackInvalid)}
	}
	if oauthErr != "" {
		return oauthCallback{Denied: true}
	}
	return oauthCallback{Code: code}
}

func (p *Provider) authorizeURL(account, clientID, redirectURI, state, verifier string) (string, error) {
	u, err := url.Parse(p.opts.AuthorizeURL)
	if err != nil || !validOAuthEndpoint(u, "https", "accounts.google.com", "/o/oauth2/v2/auth") {
		return "", errs.NewPreCommitOAuthFailure(account, errs.PreCommitOAuthLoopbackUnavailable)
	}
	q := url.Values{}
	q.Set("access_type", "offline")
	q.Set("client_id", clientID)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "consent")
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", requiredScope)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randomBase64URL(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", rungrad.NewError(rungrad.ExitAPI, "could not start the Gmail sign-in")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (p *Provider) exchangeToken(ctx context.Context, account string, form url.Values, requireRefresh bool, fallbackScopes []string, registrar provider.SecretRegistrar) (*tokenResponse, error) {
	u, err := url.Parse(p.opts.TokenURL)
	if err != nil || !validOAuthEndpoint(u, "https", "oauth2.googleapis.com", "/token") {
		return nil, providerAPIError(account, "token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, providerAPIError(account, "token")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := doNoRedirect(p.opts.HTTPClient, req)
	if err != nil {
		return nil, providerConnectionError(account, "token")
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, limits.OAuthMaxResponseBytes)
	if err != nil {
		return nil, providerDataError(account, "invalid_json")
	}
	return p.parseTokenResponse(account, resp.StatusCode, body, requireRefresh, fallbackScopes, registrar)
}

func (p *Provider) parseTokenResponse(account string, status int, body []byte, requireRefresh bool, fallbackScopes []string, registrar provider.SecretRegistrar) (*tokenResponse, error) {
	var raw struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken *string         `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		TokenType    string          `json:"token_type"`
		Scope        *string         `json:"scope"`
		Error        string          `json:"error"`
	}
	if hasDuplicateJSONKey(body) {
		if status < 200 || status >= 300 {
			return nil, classifyHTTPStatus(account, "token", status, nil, false)
		}
		return nil, providerDataError(account, "invalid_json")
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		if status < 200 || status >= 300 {
			return nil, classifyHTTPStatus(account, "token", status, nil, false)
		}
		return nil, providerDataError(account, "invalid_json")
	}
	if raw.Error != "" {
		if status >= 200 && status < 300 {
			return nil, providerDataError(account, "invalid_json")
		}
		return nil, classifyTokenError(account, status, body, raw.Error)
	}
	if status < 200 || status >= 300 {
		return nil, classifyHTTPStatus(account, "token", status, body, false)
	}
	if raw.AccessToken != "" && registrar != nil {
		if err := registrar.RegisterSecret(raw.AccessToken); err != nil {
			return nil, err
		}
		if err := registrar.RegisterSecret("Bearer " + raw.AccessToken); err != nil {
			return nil, err
		}
	}
	if raw.RefreshToken != nil && *raw.RefreshToken != "" && registrar != nil {
		if err := registrar.RegisterSecret(*raw.RefreshToken); err != nil {
			return nil, err
		}
	}
	var expires int64
	if err := json.Unmarshal(raw.ExpiresIn, &expires); err != nil || expires <= 0 {
		return nil, providerDataError(account, "invalid_json")
	}
	const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
	if expires > maxDurationSeconds {
		return nil, providerDataError(account, "invalid_json")
	}
	now := p.now()
	expiresAt := now.Add(time.Duration(expires) * time.Second)
	if !expiresAt.After(now) {
		return nil, providerDataError(account, "invalid_json")
	}
	if raw.AccessToken == "" || !strings.EqualFold(raw.TokenType, "Bearer") {
		return nil, providerDataError(account, "invalid_json")
	}
	refresh := ""
	if raw.RefreshToken != nil {
		refresh = *raw.RefreshToken
	}
	if requireRefresh && refresh == "" {
		return nil, providerDataError(account, "invalid_json")
	}
	if raw.RefreshToken != nil && refresh == "" {
		return nil, providerDataError(account, "invalid_json")
	}
	scopes := append([]string(nil), fallbackScopes...)
	if raw.Scope != nil {
		scopes = strings.Fields(*raw.Scope)
		sort.Strings(scopes)
		scopes = uniqueStrings(scopes)
	}
	return &tokenResponse{
		AccessToken:  raw.AccessToken,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		TokenType:    "Bearer",
		Scopes:       scopes,
	}, nil
}

func (p *Provider) interactiveTokenError(account string, op provider.InteractiveAuthOperation, err error) error {
	if err == nil {
		return nil
	}
	var product *errs.Error
	if errors.As(err, &product) {
		switch product.Code() {
		case errs.CodeAuthRepairRequired:
			if op == provider.InteractiveLogin {
				return errs.NewPreCommitCredentialRejected(account, errs.ProviderGmail)
			}
			return repairRequired(account, "credential_rejected")
		case errs.CodeProviderDataError, errs.CodeProviderRateLimited, errs.CodeProviderForbidden, errs.CodeProviderConnectionError:
			return err
		}
	}
	return err
}

func (p *Provider) LocalHealth(ctx context.Context, ac provider.StoredAuthContext) (provider.LocalStatus, error) {
	var status provider.LocalStatus
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() {
			status = provider.LocalRepairRequired
			return nil
		}
		clientID := ""
		if ac.Setup.Gmail != nil {
			clientID = ac.Setup.Gmail.ClientID
		}
		if !validCredential(cache, ac.Provider, ac.Account, ac.ExpectedEmail, clientID) {
			status = provider.LocalRepairRequired
			return nil
		}
		status = provider.LocalUsable
		return nil
	})
	if err != nil {
		return provider.LocalRepairRequired, nil
	}
	return status, nil
}

func (p *Provider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	_, refreshed, rotated, err := p.ensureFresh(ctx, ac, true)
	if err != nil {
		return provider.RefreshResult{}, err
	}
	return provider.RefreshResult{RefreshChecked: refreshed, CredentialChanged: refreshed, RotatedRefreshToken: rotated}, nil
}

func (p *Provider) ensureFresh(ctx context.Context, ac provider.StoredAuthContext, force bool) (*TokenCache, bool, bool, error) {
	var currentForClient *TokenCache
	var refreshed, rotated bool
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() {
			return repairRequired(ac.Account, "credential_rejected")
		}
		clientID := cache.ClientID
		if ac.Setup.Gmail != nil && ac.Setup.Gmail.ClientID != "" {
			if cache.ClientID != "" && cache.ClientID != ac.Setup.Gmail.ClientID {
				return repairRequired(ac.Account, "client_id_mismatch")
			}
			clientID = ac.Setup.Gmail.ClientID
		}
		if !validCredential(cache, ac.Provider, ac.Account, ac.ExpectedEmail, "") || clientID == "" {
			return repairRequired(ac.Account, "credential_rejected")
		}
		if !force && !cache.IsExpired(p.now(), p.opts.RefreshSkew) {
			currentForClient = cache.clone()
			return nil
		}
		form := url.Values{
			"client_id":     {clientID},
			"grant_type":    {"refresh_token"},
			"refresh_token": {cache.RefreshTokenValue()},
		}
		if secret := cache.ClientSecretValue(); secret != "" {
			form.Set("client_secret", secret)
		}
		token, err := p.exchangeToken(ctx, ac.Account, form, false, cache.Scopes, ac.Runtime)
		if err != nil {
			var product *errs.Error
			if errors.As(err, &product) {
				switch product.Code() {
				case errs.CodeAuthRepairRequired:
					return repairRequired(ac.Account, "credential_rejected")
				case errs.CodeProviderDataError, errs.CodeProviderRateLimited, errs.CodeProviderForbidden, errs.CodeProviderConnectionError:
					return err
				}
			}
			return err
		}
		if !sameScopeSet(token.Scopes, []string{requiredScope}) {
			return repairRequired(ac.Account, "scope_missing")
		}
		email, err := p.getProfile(ctx, token.AccessToken, ac.Account, false)
		if err != nil {
			return err
		}
		if normalizeEmail(email) != normalizeEmail(ac.ExpectedEmail) && ac.ExpectedEmail != "" {
			return repairRequired(ac.Account, "identity_mismatch")
		}
		next := cache.clone()
		oldRefresh := next.RefreshTokenValue()
		newRefresh := token.RefreshToken
		if newRefresh == "" {
			newRefresh = oldRefresh
		}
		next.SetAccessToken(token.AccessToken)
		next.SetRefreshToken(newRefresh)
		next.ExpiresAt = token.ExpiresAt
		next.TokenType = "Bearer"
		next.Scopes = []string{requiredScope}
		next.VerifiedEmail = email
		next.ClientID = clientID
		rotated = token.RefreshToken != "" && token.RefreshToken != oldRefresh
		currentForClient = next.clone()
		if err := lease.Replace(next); err != nil {
			return err
		}
		refreshed = true
		return nil
	})
	if err != nil {
		if currentForClient != nil {
			currentForClient.Destroy()
		}
		return nil, false, false, err
	}
	return currentForClient, refreshed, rotated, nil
}

func (p *Provider) Logout(context.Context, provider.StoredAuthContext) error { return nil }

func (p *Provider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	cache, _, _, err := p.ensureFresh(ctx, ac, false)
	if err != nil {
		return nil, err
	}
	defer cache.Destroy()
	return &Client{
		account:     ac.Account,
		email:       cache.VerifiedEmail,
		displayName: ac.ExpectedDisplayName,
		accessToken: secretFromString(cache.AccessTokenValue()),
		tokenType:   cache.TokenType,
		apiBaseURL:  p.opts.APIBaseURL,
		http:        p.opts.HTTPClient,
	}, nil
}

// Client talks to the Gmail API for one signed-in account.
type Client struct {
	account     string
	email       string
	displayName string
	accessToken secretBytes
	tokenType   string
	apiBaseURL  string
	http        *http.Client
	destroyed   atomic.Bool

	mu           sync.Mutex
	labelBase    []gmailLabel
	labelDetails map[string]gmailLabelDetail
	attachments  *gmailAttachmentSnapshot
	attachSeal   *draftAttachmentSeal
	streams      map[*encodedAttachmentStream]struct{}
}

func (c *Client) ProviderName() string { return "gmail" }
func (c *Client) AccountName() string  { return c.account }

func (c *Client) Destroy() {
	if c == nil || c.destroyed.Swap(true) {
		return
	}
	c.mu.Lock()
	c.attachments = nil
	c.attachSeal = nil
	streams := make([]*encodedAttachmentStream, 0, len(c.streams))
	for stream := range c.streams {
		streams = append(streams, stream)
	}
	c.streams = nil
	c.mu.Unlock()
	for _, stream := range streams {
		stream.destroy()
	}
	c.accessToken.Destroy()
	c.tokenType = ""
	c.email = ""
}

func (c *Client) authorizationHeader() (string, error) {
	if c == nil || c.destroyed.Load() || c.accessToken.String() == "" {
		account := ""
		if c != nil {
			account = c.account
		}
		return "", errs.NewProviderClientClosed(account, errs.ProviderGmail)
	}
	tokenType := c.tokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return tokenType + " " + c.accessToken.String(), nil
}

func (c *Client) getJSON(ctx context.Context, path string, values url.Values, limit int64, out any) error {
	status, body, err := c.gmailRequest(ctx, http.MethodGet, path, values, nil, limit)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return classifyHTTPStatus(c.account, path, status, body, false)
	}
	return decodeJSONResponse(c.account, body, out)
}

func (c *Client) gmailRequest(ctx context.Context, method, path string, values url.Values, body io.Reader, limit int64) (int, []byte, error) {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, err
	}
	target, err := c.buildURL(path, values)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, providerAPIError(c.account, path)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := doNoRedirect(c.http, req)
	if err != nil {
		return 0, nil, providerConnectionError(c.account, path)
	}
	defer resp.Body.Close()
	responseLimit := limit
	if responseLimit <= 0 {
		responseLimit = limits.GmailMaxJSONResponseBytes
	}
	data, err := readLimited(resp.Body, responseLimit)
	if err != nil {
		return resp.StatusCode, nil, providerDataError(c.account, "invalid_json")
	}
	return resp.StatusCode, data, nil
}

func (c *Client) buildURL(path string, values url.Values) (string, error) {
	return buildGmailAPIURL(c.account, c.apiBaseURL, path, values)
}

func buildGmailAPIURL(account, apiBaseURL, path string, values url.Values) (string, error) {
	base, err := url.Parse(apiBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", providerAPIError(account, path)
	}
	if !validAPIBaseURL(base) {
		return "", providerAPIError(account, path)
	}
	if strings.Contains(path, "://") || strings.Contains(path, "..") || !strings.HasPrefix(path, "/") {
		return "", providerAPIError(account, path)
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = values.Encode()
	return base.String(), nil
}

func validAPIBaseURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	if path != "/gmail/v1/users/me" {
		return false
	}
	if u.Scheme == "https" && strings.EqualFold(u.Host, "gmail.googleapis.com") {
		return true
	}
	return (u.Scheme == "http" || u.Scheme == "https") && isLoopbackHost(u.Hostname())
}

func doNoRedirect(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := copyClient.Do(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp, nil
	}
	return resp, err
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = limits.GmailMaxJSONResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return data, nil
}

func validOAuthEndpoint(u *url.URL, scheme, host, path string) bool {
	if u == nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == scheme && strings.EqualFold(u.Host, host) && u.Path == path {
		return true
	}
	return (u.Scheme == "http" || u.Scheme == "https") && isLoopbackHost(u.Hostname()) && u.Path != ""
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *Provider) getProfile(ctx context.Context, accessToken, account string, preCommit bool) (string, error) {
	values := url.Values{"fields": {"emailAddress"}}
	target, err := buildGmailAPIURL(account, p.opts.APIBaseURL, "/profile", values)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", providerAPIError(account, "profile")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := doNoRedirect(p.opts.HTTPClient, req)
	if err != nil {
		return "", providerConnectionError(account, "profile")
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, limits.OAuthMaxResponseBytes)
	if err != nil {
		return "", providerDataError(account, "invalid_json")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", classifyHTTPStatus(account, "profile", resp.StatusCode, body, preCommit)
	}
	var raw struct {
		EmailAddress any `json:"emailAddress"`
	}
	if err := decodeJSONResponse(account, body, &raw); err != nil {
		return "", err
	}
	email, ok := raw.EmailAddress.(string)
	if !ok {
		if preCommit {
			return "", errs.NewPreCommitIdentityMismatch(account, errs.ProviderGmail)
		}
		return "", repairRequired(account, "identity_mismatch")
	}
	normalized := normalizeEmail(email)
	if normalized == "" {
		if preCommit {
			return "", errs.NewPreCommitIdentityMismatch(account, errs.ProviderGmail)
		}
		return "", repairRequired(account, "identity_mismatch")
	}
	return normalized, nil
}

func normalizeEmail(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n,<>") {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	if strings.Count(s, "@") != 1 {
		return ""
	}
	addr, err := netmail.ParseAddress(s)
	if err != nil || addr.Name != "" || addr.Address != s {
		return ""
	}
	parts := strings.SplitN(s, "@", 2)
	if parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(s)
}

func repairRequired(account, reason string) error {
	details := map[string]any{
		"account":        account,
		"provider":       "gmail",
		"reason":         reason,
		"repair_command": "maildo auth repair " + account,
	}
	return errs.NewWithDetails(errs.CodeAuthRepairRequired, fmt.Sprintf("the saved login for %q cannot be used; run %s", account, details["repair_command"]), details)
}

func classifyHTTPStatus(account, operation string, status int, body []byte, preCommitProfile bool) error {
	if status == http.StatusUnauthorized {
		if preCommitProfile {
			return errs.NewPreCommitCredentialRejected(account, errs.ProviderGmail)
		}
		return repairRequired(account, "credential_rejected")
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return notFoundError(account, operation)
	}
	reason := googleErrorReason(body)
	if status == http.StatusForbidden {
		switch reason {
		case "authError", "invalidCredentials":
			if preCommitProfile {
				return errs.NewPreCommitCredentialRejected(account, errs.ProviderGmail)
			}
			return repairRequired(account, "credential_rejected")
		case "insufficientPermissions":
			if preCommitProfile {
				return errs.NewPreCommitScopeMissing(account, errs.ProviderGmail)
			}
			return repairRequired(account, "scope_missing")
		case "dailyLimitExceeded":
			return providerRateLimited(account, "daily_quota")
		case "quotaExceeded":
			return providerRateLimited(account, "quota")
		case "userRateLimitExceeded":
			return providerRateLimited(account, "user_rate")
		case "rateLimitExceeded":
			return providerRateLimited(account, "rate_limit")
		case "domainPolicy":
			return providerForbidden(account, "domain_policy")
		case "accessNotConfigured":
			return providerForbidden(account, "api_not_enabled")
		case "forbidden":
			return providerForbidden(account, "provider_policy")
		default:
			return providerForbidden(account, "unknown_forbidden")
		}
	}
	if status == http.StatusTooManyRequests {
		return providerRateLimited(account, "rate_limit")
	}
	if status >= 300 && status < 400 {
		return providerAPIError(account, operation)
	}
	if status >= 500 || status >= 400 {
		return providerAPIError(account, operation)
	}
	return nil
}

func classifyTokenError(account string, status int, body []byte, oauthError string) error {
	switch oauthError {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return repairRequired(account, "credential_rejected")
	case "invalid_scope":
		return repairRequired(account, "scope_missing")
	}
	return classifyHTTPStatus(account, "token", status, body, false)
}

func notFoundError(account, operation string) error {
	switch {
	case strings.HasPrefix(operation, "/labels/"):
		folderID, _ := url.PathUnescape(strings.TrimPrefix(operation, "/labels/"))
		return errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
			"account":   account,
			"provider":  "gmail",
			"folder_id": folderID,
		})
	case strings.HasPrefix(operation, "/threads/"):
		threadID, _ := url.PathUnescape(strings.TrimPrefix(operation, "/threads/"))
		return threadNotFound(account, threadID)
	case strings.HasPrefix(operation, "/messages/"):
		return errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{
			"account":  account,
			"provider": "gmail",
		})
	default:
		return providerAPIError(account, operation)
	}
}

func googleErrorReason(body []byte) string {
	var payload struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	precedence := []string{
		"authError",
		"invalidCredentials",
		"insufficientPermissions",
		"dailyLimitExceeded",
		"quotaExceeded",
		"userRateLimitExceeded",
		"rateLimitExceeded",
		"domainPolicy",
		"accessNotConfigured",
		"forbidden",
	}
	seen := map[string]bool{}
	for _, e := range payload.Error.Errors {
		seen[e.Reason] = true
	}
	for _, p := range precedence {
		if seen[p] {
			return p
		}
	}
	return ""
}

func providerRateLimited(account, reason string) error {
	return errs.NewWithDetails(errs.CodeProviderRateLimited, "Gmail request was rate limited", map[string]any{
		"account":  account,
		"provider": "gmail",
		"reason":   reason,
	})
}

func providerForbidden(account, reason string) error {
	return errs.NewWithDetails(errs.CodeProviderForbidden, "Gmail rejected the operation", map[string]any{
		"account":  account,
		"provider": "gmail",
		"reason":   reason,
	})
}

func providerConnectionError(account, operation string) error {
	return errs.NewWithDetails(errs.CodeProviderConnectionError, "Gmail connection failed", map[string]any{
		"account":   account,
		"provider":  "gmail",
		"operation": operation,
		"reason":    "connection_lost",
	})
}

func providerDataError(account, reason string) error {
	return errs.NewWithDetails(errs.CodeProviderDataError, "Gmail sent a response maildo did not expect", map[string]any{
		"account":  account,
		"provider": "gmail",
		"reason":   reason,
	})
}

func providerAPIError(account, operation string) error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not complete Gmail %s request for account %q", operation, account))
}

func messageNotFound(account string, kind mail.ResolvedSelectorKind) error {
	return errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{
		"account":       account,
		"provider":      "gmail",
		"selector_kind": string(kind),
	})
}

func threadNotFound(account, threadID string) error {
	return errs.NewWithDetails(errs.CodeThreadNotFound, "thread not found", map[string]any{
		"account":   account,
		"provider":  "gmail",
		"thread_id": threadID,
	})
}

func ambiguousInternetID(account, internetID string, ids []string) error {
	return errs.NewWithDetails(errs.CodeAmbiguousMessageSelector, "this Message-ID belongs to more than one message; pick one by its id with --id", map[string]any{
		"account":             account,
		"provider":            "gmail",
		"internet_message_id": internetID,
		"candidate_ids":       ids,
	})
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type gmailLabelDetail struct {
	ID                    string      `json:"id"`
	Name                  string      `json:"name"`
	Type                  string      `json:"type"`
	MessagesTotal         *int        `json:"messagesTotal"`
	MessagesUnread        *int        `json:"messagesUnread"`
	MessageListVisibility *string     `json:"messageListVisibility"`
	LabelListVisibility   *string     `json:"labelListVisibility"`
	Color                 *gmailColor `json:"color"`
}

type gmailColor struct {
	TextColor       string `json:"textColor"`
	BackgroundColor string `json:"backgroundColor"`
}

type labelsResponse struct {
	Labels []gmailLabel `json:"labels"`
}

type labelSnapshot struct {
	folders []mail.Folder
	byID    map[string]mail.Folder
	labels  map[string]gmailLabel
}

func (c *Client) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	normalized, err := mail.NewFolderListRequest(req.Detail, req.DetailIDs)
	if err != nil {
		return mail.FolderList{}, err
	}
	switch normalized.Detail {
	case mail.FolderListDetailIdentity:
		snap, err := c.identityLabels(ctx)
		if err != nil {
			return mail.FolderList{}, err
		}
		return mail.FolderList{Folders: cloneFolders(snap.folders)}, nil
	case mail.FolderListDetailSelected:
		snap, err := c.identityLabels(ctx)
		if err != nil {
			return mail.FolderList{}, err
		}
		for _, id := range normalized.DetailIDs {
			if _, ok := snap.byID[id]; !ok {
				return mail.FolderList{}, errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
					"account":   c.account,
					"provider":  "gmail",
					"folder_id": id,
				})
			}
		}
		if err := c.ensureLabelDetails(ctx, normalized.DetailIDs); err != nil {
			return mail.FolderList{}, err
		}
		snap, err = c.snapshotWithDetails(ctx, normalized.DetailIDs)
		if err != nil {
			return mail.FolderList{}, err
		}
		return mail.FolderList{Folders: cloneFolders(snap.folders)}, nil
	default:
		snap, err := c.identityLabels(ctx)
		if err != nil {
			return mail.FolderList{}, err
		}
		ids := make([]string, 0, len(snap.folders))
		for _, f := range snap.folders {
			ids = append(ids, f.ID)
		}
		if err := c.ensureLabelDetails(ctx, ids); err != nil {
			return mail.FolderList{}, err
		}
		snap, err = c.snapshotWithDetails(ctx, ids)
		if err != nil {
			return mail.FolderList{}, err
		}
		return mail.FolderList{Folders: cloneFolders(snap.folders)}, nil
	}
}

func (c *Client) identityLabels(ctx context.Context) (labelSnapshot, error) {
	c.mu.Lock()
	have := c.labelBase != nil
	base := append([]gmailLabel(nil), c.labelBase...)
	c.mu.Unlock()
	if !have {
		values := url.Values{"fields": {"labels(id,name,type)"}}
		var resp labelsResponse
		if err := c.getJSON(ctx, "/labels", values, limits.GmailMaxJSONResponseBytes, &resp); err != nil {
			return labelSnapshot{}, err
		}
		if len(resp.Labels) > limits.GmailMaxLabels {
			return labelSnapshot{}, errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitLabels, uint64(limits.GmailMaxLabels+1))
		}
		seen := map[string]bool{}
		for _, label := range resp.Labels {
			if label.ID == "" || label.Name == "" || (label.Type != "system" && label.Type != "user") || seen[label.ID] {
				return labelSnapshot{}, providerDataError(c.account, "invalid_protocol")
			}
			seen[label.ID] = true
		}
		c.mu.Lock()
		if c.labelBase == nil {
			c.labelBase = append([]gmailLabel(nil), resp.Labels...)
			if c.labelDetails == nil {
				c.labelDetails = map[string]gmailLabelDetail{}
			}
		}
		base = append([]gmailLabel(nil), c.labelBase...)
		c.mu.Unlock()
	}
	return c.buildSnapshot(base, nil)
}

func (c *Client) ensureLabelDetails(ctx context.Context, ids []string) error {
	c.mu.Lock()
	if c.labelDetails == nil {
		c.labelDetails = map[string]gmailLabelDetail{}
	}
	missing := make([]string, 0)
	for _, id := range ids {
		if _, ok := c.labelDetails[id]; !ok {
			missing = append(missing, id)
		}
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return nil
	}
	type detailResult struct {
		id     string
		detail gmailLabelDetail
	}
	results, err := provider.RunBoundedOrdered(ctx, limits.GmailHydrationConcurrency, missing, func(ctx context.Context, _ int, id string) (detailResult, error) {
		values := url.Values{"fields": {"id,name,type,messagesTotal,messagesUnread,color(textColor,backgroundColor)"}}
		var detail gmailLabelDetail
		if err := c.getJSON(ctx, "/labels/"+url.PathEscape(id), values, limits.GmailMaxJSONResponseBytes, &detail); err != nil {
			return detailResult{}, err
		}
		return detailResult{id: id, detail: detail}, nil
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labelDetails == nil {
		c.labelDetails = map[string]gmailLabelDetail{}
	}
	for _, result := range results {
		c.labelDetails[result.id] = result.detail
	}
	return nil
}

func (c *Client) snapshotWithDetails(ctx context.Context, selected []string) (labelSnapshot, error) {
	c.mu.Lock()
	base := append([]gmailLabel(nil), c.labelBase...)
	details := make(map[string]gmailLabelDetail, len(c.labelDetails))
	for k, v := range c.labelDetails {
		details[k] = v
	}
	c.mu.Unlock()
	if base == nil {
		return c.identityLabels(ctx)
	}
	selectedSet := map[string]bool{}
	for _, id := range selected {
		selectedSet[id] = true
	}
	return c.buildSnapshot(base, selectedSetWithDetails(selectedSet, details))
}

func selectedSetWithDetails(selected map[string]bool, details map[string]gmailLabelDetail) map[string]gmailLabelDetail {
	out := map[string]gmailLabelDetail{}
	for id := range selected {
		if d, ok := details[id]; ok {
			out[id] = d
		}
	}
	return out
}

func (c *Client) buildSnapshot(base []gmailLabel, details map[string]gmailLabelDetail) (labelSnapshot, error) {
	labels := make(map[string]gmailLabel, len(base))
	userNameToID := map[string]string{}
	for _, label := range base {
		labels[label.ID] = label
		if label.Type == "user" {
			userNameToID[label.Name] = label.ID
		}
	}
	folders := make([]mail.Folder, 0, len(base))
	for _, label := range base {
		detail, haveDetail := details[label.ID]
		row, err := c.labelToFolder(label, detail, haveDetail)
		if err != nil {
			return labelSnapshot{}, err
		}
		if label.Type == "user" {
			if idx := strings.LastIndex(label.Name, "/"); idx > 0 {
				parentName := label.Name[:idx]
				if parentID := userNameToID[parentName]; parentID != "" {
					row.ParentID = parentID
					row.ParentPath = parentName
				}
			}
		}
		folders = append(folders, row)
	}
	sort.SliceStable(folders, func(i, j int) bool {
		if folders[i].Path != folders[j].Path {
			return folders[i].Path < folders[j].Path
		}
		if folders[i].DisplayName != folders[j].DisplayName {
			return folders[i].DisplayName < folders[j].DisplayName
		}
		return folders[i].ID < folders[j].ID
	})
	byID := make(map[string]mail.Folder, len(folders))
	for _, f := range folders {
		byID[f.ID] = f
	}
	return labelSnapshot{folders: folders, byID: byID, labels: labels}, nil
}

func (c *Client) labelToFolder(label gmailLabel, detail gmailLabelDetail, haveDetail bool) (mail.Folder, error) {
	if haveDetail {
		if detail.ID != label.ID || detail.Name != label.Name || detail.Type != label.Type {
			return mail.Folder{}, providerDataError(c.account, "invalid_protocol")
		}
	}
	labelType := mail.LabelTypeSystem
	if label.Type == "user" {
		labelType = mail.LabelTypeUser
	}
	selectable := true
	meta := mail.FolderProviderMetadata{
		Source:     mail.MetadataSourceGmailAPI,
		LabelType:  labelType,
		Selectable: &selectable,
	}
	display, path := label.Name, label.Name
	if label.Type == "user" {
		if idx := strings.LastIndex(label.Name, "/"); idx >= 0 && idx < len(label.Name)-1 {
			display = label.Name[idx+1:]
		}
	} else if wk := gmailWellKnown(label.ID); wk != "" {
		display = systemDisplayName(wk, label.Name)
	}
	row := mail.Folder{
		Account:       c.account,
		Provider:      "gmail",
		ID:            label.ID,
		DisplayName:   display,
		WellKnownName: gmailWellKnown(label.ID),
		Path:          path,
		ProviderMeta:  mustGmailMetadata(mail.MetadataFolder, meta),
	}
	if haveDetail {
		if detail.MessagesTotal != nil {
			if *detail.MessagesTotal < 0 {
				return mail.Folder{}, providerDataError(c.account, "invalid_json")
			}
			row.TotalCount = *detail.MessagesTotal
			row.TotalCountKnown = true
		}
		if detail.MessagesUnread != nil {
			if *detail.MessagesUnread < 0 {
				return mail.Folder{}, providerDataError(c.account, "invalid_json")
			}
			row.UnreadCount = *detail.MessagesUnread
			row.UnreadCountKnown = true
		}
		if detail.Color != nil {
			if label.Type != "user" || !validHexColor(detail.Color.TextColor) || !validHexColor(detail.Color.BackgroundColor) {
				return mail.Folder{}, providerDataError(c.account, "invalid_protocol")
			}
			meta.ColorText = detail.Color.TextColor
			meta.ColorBackground = detail.Color.BackgroundColor
			row.ProviderMeta = mustGmailMetadata(mail.MetadataFolder, meta)
		}
	}
	return row, nil
}

func cloneFolders(in []mail.Folder) []mail.Folder {
	out := make([]mail.Folder, len(in))
	copy(out, in)
	for i := range out {
		out[i].ProviderMeta, _ = mail.CloneProviderMetadata(out[i].ProviderMeta)
	}
	return out
}

func gmailWellKnown(id string) string {
	switch id {
	case "INBOX":
		return mail.WellKnownInbox
	case "DRAFT":
		return mail.WellKnownDrafts
	case "SENT":
		return mail.WellKnownSent
	case "TRASH":
		return mail.WellKnownDeleted
	case "SPAM":
		return mail.WellKnownJunk
	default:
		return ""
	}
}

func systemDisplayName(wk, fallback string) string {
	switch wk {
	case mail.WellKnownInbox:
		return "Inbox"
	case mail.WellKnownDrafts:
		return "Drafts"
	case mail.WellKnownSent:
		return "Sent"
	case mail.WellKnownDeleted:
		return "Trash"
	case mail.WellKnownJunk:
		return "Spam"
	default:
		return fallback
	}
}

func validHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func mustGmailMetadata[T mail.ProviderMetadataValue](use mail.ProviderMetadataUse, value T) *T {
	meta, err := mail.NewProviderMetadata(errs.ProviderGmail, use, value)
	if err != nil {
		panic(err)
	}
	return meta
}

type messageListResponse struct {
	Messages      []messageListItem `json:"messages"`
	NextPageToken string            `json:"nextPageToken"`
}

type messageListItem struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type gmailMessage struct {
	ID           string     `json:"id"`
	ThreadID     string     `json:"threadId"`
	LabelIDs     []string   `json:"labelIds"`
	Snippet      string     `json:"snippet"`
	InternalDate string     `json:"internalDate"`
	Payload      *gmailPart `json:"payload"`
	Raw          string     `json:"raw"`
}

type gmailThread struct {
	ID       string         `json:"id"`
	Messages []gmailMessage `json:"messages"`
}

type gmailPart struct {
	PartID   string        `json:"partId"`
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     gmailBody     `json:"body"`
	Parts    []gmailPart   `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailBody struct {
	AttachmentID *string `json:"attachmentId"`
	Size         *int64  `json:"size"`
	Data         *string `json:"data"`
}

func (c *Client) ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	// Gmail pages by token, not by position, so maildo also lists the ids
	// before the page and fetches details only for the page itself.
	ids, err := c.collectMessageIDs(ctx, listCollectRequest{
		LabelIDs:         []string{req.Folder.ID},
		Unread:           req.Filter.Unread,
		IncludeSpamTrash: true,
		Limit:            req.Offset + req.Limit,
	})
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	if req.Offset >= len(ids) {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	rows, err := c.hydrateSummaries(ctx, ids[req.Offset:], &req.Folder)
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	return mail.MessageSummaryList{Messages: rows}, nil
}

// SearchMessages lets Gmail do the matching. Gmail lists matches newest
// first, so a page needs only the first offset+limit ids, and maildo
// fetches details only for the rows on the page. A local check then makes
// date and attachment limits exact. Rows it drops are replaced from further
// down the list. Paging counts ids in Gmail's order, so the page after one
// where the check dropped rows can repeat a row.
func (c *Client) SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	q, err := serializeQuery(req.Query)
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	list := listCollectRequest{Query: q}
	if req.Folder != nil {
		list.LabelIDs = []string{req.Folder.ID}
		list.IncludeSpamTrash = true
	} else {
		scope, includeSpamTrash, err := gmailSearchScope(req.SkipRoles, req.OnlyRoles)
		if err != nil {
			return mail.MessageSummaryList{}, err
		}
		list.Query = strings.TrimSpace(q + " " + scope)
		list.IncludeSpamTrash = includeSpamTrash
	}
	pager := c.newMessagePager(list)
	skip := req.Offset
	hydrated := 0
	rows := make([]mail.MessageSummary, 0, min(req.Limit, 100))
	for len(rows) < req.Limit {
		items, more, err := pager.next(ctx, skip+req.Limit-len(rows))
		if err != nil {
			return mail.MessageSummaryList{}, err
		}
		if skip > 0 {
			n := min(skip, len(items))
			skip -= n
			items = items[n:]
		}
		for len(items) > 0 && len(rows) < req.Limit {
			n := min(len(items), req.Limit-len(rows))
			batch := items[:n]
			items = items[n:]
			hydrated += n
			if hydrated > limits.GmailMaxHydratedMessages {
				return mail.MessageSummaryList{}, errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitHydratedMessages, uint64(hydrated))
			}
			found, err := c.hydrateSummaries(ctx, batch, req.Folder)
			if err != nil {
				return mail.MessageSummaryList{}, err
			}
			rows = append(rows, applyGmailPostFilter(found, req.Query)...)
		}
		if !more {
			break
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return mail.MessageSummaryLess(rows[i], rows[j]) })
	return mail.MessageSummaryList{Messages: rows}, nil
}

// gmailSearchScope turns the folder roles of a search without a folder into
// Gmail search terms. Gmail leaves Spam and Trash out unless
// includeSpamTrash is set, so the default search needs no terms.
func gmailSearchScope(skip, only []string) (terms string, includeSpamTrash bool, err error) {
	if len(only) > 0 {
		parts := make([]string, 0, len(only))
		for _, role := range only {
			switch role {
			case mail.WellKnownDeleted, mail.WellKnownJunk:
				includeSpamTrash = true
			case mail.WellKnownArchive:
				// Archived Gmail mail is mail without the Inbox label.
				parts = append(parts, "-in:inbox")
				continue
			}
			term, ok := gmailRoleTerms[role]
			if !ok {
				return "", false, gmailUnsupportedRole(role)
			}
			parts = append(parts, term)
		}
		if len(parts) == 1 {
			return parts[0], includeSpamTrash, nil
		}
		return "{" + strings.Join(parts, " ") + "}", includeSpamTrash, nil
	}
	parts := make([]string, 0, len(skip))
	for _, role := range skip {
		if role == mail.WellKnownDeleted || role == mail.WellKnownJunk {
			continue
		}
		term, ok := gmailRoleTerms[role]
		if !ok {
			return "", false, gmailUnsupportedRole(role)
		}
		parts = append(parts, "-"+term)
	}
	return strings.Join(parts, " "), false, nil
}

var gmailRoleTerms = map[string]string{
	mail.WellKnownInbox:   "in:inbox",
	mail.WellKnownSent:    "in:sent",
	mail.WellKnownDrafts:  "in:drafts",
	mail.WellKnownDeleted: "in:trash",
	mail.WellKnownJunk:    "in:spam",
}

func gmailUnsupportedRole(role string) error {
	return errs.NewWithDetails(errs.CodeUnsupportedQueryOperator,
		fmt.Sprintf("Gmail has no %s folder that a search can leave out", role),
		map[string]any{"provider": "gmail", "role": role})
}

type listCollectRequest struct {
	Query            string
	LabelIDs         []string
	Unread           bool
	IncludeSpamTrash bool
	Limit            int
}

// collectMessageIDs lists up to req.Limit message ids.
func (c *Client) collectMessageIDs(ctx context.Context, req listCollectRequest) ([]messageListItem, error) {
	if req.Limit <= 0 {
		return nil, nil
	}
	pager := c.newMessagePager(req)
	out := make([]messageListItem, 0, min(req.Limit, 100))
	for len(out) < req.Limit {
		items, more, err := pager.next(ctx, req.Limit-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if !more {
			break
		}
	}
	if len(out) > req.Limit {
		out = out[:req.Limit]
	}
	return out, nil
}

// messagePager walks the pages of one Gmail message list and checks that
// Gmail does not repeat ids or page tokens.
type messagePager struct {
	c          *Client
	req        listCollectRequest
	pageToken  string
	pages      int
	done       bool
	seenTokens map[string]bool
	seenIDs    map[string]bool
}

func (c *Client) newMessagePager(req listCollectRequest) *messagePager {
	return &messagePager{c: c, req: req, seenTokens: map[string]bool{}, seenIDs: map[string]bool{}}
}

// next fetches the next page, asking for up to want ids (Gmail sends at
// most 500 per page). more is false after the last page.
func (p *messagePager) next(ctx context.Context, want int) (items []messageListItem, more bool, err error) {
	if p.done {
		return nil, false, nil
	}
	p.pages++
	if p.pages > limits.GmailMaxPages {
		return nil, false, errs.NewReadLimitExceeded(p.c.account, errs.ProviderGmail, errs.ReadLimitPages, uint64(limits.GmailMaxPages+1))
	}
	values := url.Values{}
	values.Set("fields", "messages(id,threadId),nextPageToken")
	if p.req.IncludeSpamTrash {
		values.Set("includeSpamTrash", "true")
	}
	values.Set("maxResults", strconv.Itoa(max(1, min(want, 500))))
	if p.req.Query != "" {
		values.Set("q", p.req.Query)
	}
	for _, id := range p.req.LabelIDs {
		values.Add("labelIds", id)
	}
	if p.req.Unread {
		values.Add("labelIds", "UNREAD")
	}
	if p.pageToken != "" {
		values.Set("pageToken", p.pageToken)
	}
	var resp messageListResponse
	if err := p.c.getJSON(ctx, "/messages", values, limits.GmailMaxJSONResponseBytes, &resp); err != nil {
		return nil, false, err
	}
	for _, item := range resp.Messages {
		if item.ID == "" || item.ThreadID == "" || p.seenIDs[item.ID] {
			return nil, false, providerDataError(p.c.account, "invalid_protocol")
		}
		p.seenIDs[item.ID] = true
	}
	if resp.NextPageToken == "" {
		p.done = true
		return resp.Messages, false, nil
	}
	if p.seenTokens[resp.NextPageToken] {
		return nil, false, providerDataError(p.c.account, "invalid_protocol")
	}
	p.seenTokens[resp.NextPageToken] = true
	p.pageToken = resp.NextPageToken
	return resp.Messages, true, nil
}

func (c *Client) hydrateSummaries(ctx context.Context, items []messageListItem, folder *mail.Folder) ([]mail.MessageSummary, error) {
	if len(items) == 0 {
		return []mail.MessageSummary{}, nil
	}
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := provider.RunBoundedOrdered(ctx, limits.GmailHydrationConcurrency, items, func(ctx context.Context, _ int, item messageListItem) (mail.MessageSummary, error) {
		msg, err := c.fetchMessage(ctx, item.ID, "full", "", "id,threadId,labelIds,snippet,internalDate,payload")
		if err != nil {
			return mail.MessageSummary{}, err
		}
		if msg.ID != item.ID || msg.ThreadID != item.ThreadID {
			return mail.MessageSummary{}, providerDataError(c.account, "invalid_protocol")
		}
		return c.messageToSummary(*msg, snap, folder)
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (c *Client) fetchMessage(ctx context.Context, id, format, metadataHeader, fields string) (*gmailMessage, error) {
	if id == "" {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	values := url.Values{}
	values.Set("format", format)
	values.Set("fields", fields)
	if metadataHeader != "" {
		values.Add("metadataHeaders", metadataHeader)
	}
	var msg gmailMessage
	err := c.getJSON(ctx, "/messages/"+url.PathEscape(id), values, limits.GmailMaxJSONResponseBytes, &msg)
	if err != nil {
		var product *errs.Error
		if errors.As(err, &product) && product.Code() == errs.CodeMessageNotFound {
			return nil, messageNotFound(c.account, mail.ResolvedSelectorID)
		}
		return nil, err
	}
	if msg.ID != id {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	return &msg, nil
}

func serializeQuery(q mail.Query) (string, error) {
	parts := make([]string, 0)
	addFamily := func(field string, values []string) error {
		if len(values) == 0 {
			return nil
		}
		out := make([]string, 0, len(values))
		for _, value := range values {
			quoted, err := gmailQuoted(value)
			if err != nil {
				return err
			}
			out = append(out, field+":"+quoted)
		}
		if len(out) == 1 {
			parts = append(parts, out[0])
		} else {
			parts = append(parts, "{"+strings.Join(out, " ")+"}")
		}
		return nil
	}
	for _, spec := range []struct {
		field  string
		values []string
	}{
		{"from", q.From},
		{"to", q.To},
		{"cc", q.CC},
		{"subject", q.Subject},
	} {
		if err := addFamily(spec.field, spec.values); err != nil {
			return "", err
		}
	}
	for _, term := range q.Terms {
		quoted, err := gmailQuoted(term)
		if err != nil {
			return "", err
		}
		parts = append(parts, quoted)
	}
	for _, phrase := range q.Phrases {
		quoted, err := gmailQuoted(phrase)
		if err != nil {
			return "", err
		}
		parts = append(parts, quoted)
	}
	if q.HasAttachment != nil && *q.HasAttachment {
		parts = append(parts, "has:attachment")
	}
	if q.HasAttachment != nil && !*q.HasAttachment {
		return "", errs.New(errs.CodeUnsupportedQueryOperator, "Gmail search cannot represent has:attachment=false exactly")
	}
	if q.Unread != nil {
		parts = append(parts, map[bool]string{true: "is:unread", false: "-is:unread"}[*q.Unread])
	}
	if q.Flagged != nil {
		parts = append(parts, map[bool]string{true: "is:starred", false: "-is:starred"}[*q.Flagged])
	}
	if q.After != nil {
		parts = append(parts, "after:"+strconv.FormatInt(q.After.UTCStart().Unix()-1, 10))
	}
	if q.Before != nil {
		parts = append(parts, "before:"+strconv.FormatInt(q.Before.UTCStart().Unix(), 10))
	}
	return strings.Join(parts, " "), nil
}

func gmailQuoted(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\"\\") {
		return "", errs.New(errs.CodeUnsupportedQueryOperator, "Gmail search cannot match this text exactly; leave out quotes and backslashes")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errs.New(errs.CodeUnsupportedQueryOperator, "Gmail search cannot match this text exactly; leave out quotes and backslashes")
		}
	}
	return `"` + value + `"`, nil
}

func applyGmailPostFilter(rows []mail.MessageSummary, q mail.Query) []mail.MessageSummary {
	out := make([]mail.MessageSummary, 0, len(rows))
	for _, row := range rows {
		if q.After != nil || q.Before != nil {
			if row.ReceivedAt == nil {
				continue
			}
			if q.After != nil && row.ReceivedAt.Before(q.After.UTCStart()) {
				continue
			}
			if q.Before != nil && !row.ReceivedAt.Before(q.Before.UTCStart()) {
				continue
			}
		}
		if q.HasAttachment != nil && row.HasAttachments != *q.HasAttachment {
			continue
		}
		out = append(out, row)
	}
	return out
}

type normalizedMessage struct {
	headers           []mail.Header
	headerMap         map[string][]string
	internetMessageID string
	from              mail.Recipient
	sender            *mail.Recipient
	replyTo           []mail.Recipient
	to                []mail.Recipient
	cc                []mail.Recipient
	bcc               []mail.Recipient
	subject           string
	sentAt            *time.Time
	attachments       []mail.Attachment
	hasAttachments    bool
	textPart          *mimeLeaf
	htmlPart          *mimeLeaf
}

type mimeLeaf struct {
	partID       string
	contentType  string
	charset      string
	data         *string
	attachmentID *string
	size         *int64
}

type mimeWalkState struct {
	account         string
	messageID       string
	partCount       int
	headerCount     int
	headerBytes     int64
	addressCount    int
	attachmentIDs   map[string]bool
	classifiedParts []classifiedMimeAttachment
}

type classifiedMimeAttachment struct {
	part           *gmailPart
	attachment     mail.Attachment
	childIndexPath []uint16
}

func (c *Client) messageToSummary(msg gmailMessage, snap labelSnapshot, fixedFolder *mail.Folder) (mail.MessageSummary, error) {
	if msg.ID == "" || msg.ThreadID == "" || msg.Payload == nil {
		return mail.MessageSummary{}, providerDataError(c.account, "invalid_protocol")
	}
	received, err := parseInternalDate(msg.InternalDate)
	if err != nil {
		return mail.MessageSummary{}, providerDataError(c.account, "invalid_protocol")
	}
	normalized, err := c.normalizePayload(&msg, false)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	folder, basis, userLabels, err := c.summaryFolder(msg.LabelIDs, snap, fixedFolder)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	starred := hasLabel(msg.LabelIDs, "STARRED")
	meta := mustGmailMetadata(mail.MetadataMessageSummary, mail.SummaryProviderMetadata{
		Source:             mail.MetadataSourceGmailAPI,
		UserLabels:         userLabels,
		Starred:            &starred,
		SummaryFolderBasis: basis,
	})
	return mail.MessageSummary{
		Account:           c.account,
		Provider:          "gmail",
		ID:                msg.ID,
		InternetMessageID: normalized.internetMessageID,
		ThreadID:          msg.ThreadID,
		Folder:            folder,
		From:              normalized.from,
		ToCount:           len(normalized.to),
		CCCount:           len(normalized.cc),
		Subject:           normalized.subject,
		ReceivedAt:        &received,
		SentAt:            normalized.sentAt,
		IsRead:            !hasLabel(msg.LabelIDs, "UNREAD"),
		Flagged:           starred,
		HasAttachments:    normalized.hasAttachments,
		Preview:           msg.Snippet,
		ProviderMeta:      meta,
	}, nil
}

func (c *Client) normalizePayload(msg *gmailMessage, keepHeaders bool) (normalizedMessage, error) {
	out, _, err := c.normalizePayloadWithState(msg, keepHeaders)
	return out, err
}

func (c *Client) normalizePayloadWithState(msg *gmailMessage, keepHeaders bool) (normalizedMessage, *mimeWalkState, error) {
	if msg == nil || msg.Payload == nil {
		return normalizedMessage{}, nil, providerDataError(c.account, "invalid_protocol")
	}
	state := &mimeWalkState{account: c.account, messageID: msg.ID, attachmentIDs: map[string]bool{}}
	out := normalizedMessage{headerMap: map[string][]string{}, attachments: []mail.Attachment{}}
	if err := c.walkPart(msg.Payload, 1, true, nil, state, &out, keepHeaders); err != nil {
		return normalizedMessage{}, nil, err
	}
	return out, state, nil
}

func (c *Client) walkPart(part *gmailPart, depth int, root bool, path []uint16, state *mimeWalkState, out *normalizedMessage, keepHeaders bool) error {
	if depth > limits.GmailMaxMIMEDepth {
		return errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitMIMEDepth, uint64(depth))
	}
	state.partCount++
	if state.partCount > limits.GmailMaxMIMEParts {
		return errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitMIMEParts, uint64(state.partCount))
	}
	state.headerCount += len(part.Headers)
	if state.headerCount > limits.GmailMaxHeaderCount {
		return errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitHeaders, uint64(state.headerCount))
	}
	for _, h := range part.Headers {
		state.headerBytes += int64(len(h.Name) + len(h.Value))
		if state.headerBytes > limits.GmailMaxHeaderBytes {
			return errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitHeaderBytes, uint64(state.headerBytes))
		}
		name := strings.TrimSpace(h.Name)
		value := h.Value
		if root {
			out.headerMap[strings.ToLower(name)] = append(out.headerMap[strings.ToLower(name)], value)
			if keepHeaders {
				out.headers = append(out.headers, mail.Header{Name: name, Value: value})
			}
		}
	}
	if root {
		out.internetMessageID = firstUsableMessageID(out.headerMap["message-id"])
		out.subject = decodeHeader(firstHeader(out.headerMap["subject"]))
		out.from = firstAddress(out.headerMap["from"], state)
		if sender := firstAddress(out.headerMap["sender"], state); sender.Name != "" || sender.Address != "" {
			out.sender = &sender
		}
		out.replyTo = addressList(out.headerMap["reply-to"], state)
		out.to = addressList(out.headerMap["to"], state)
		out.cc = addressList(out.headerMap["cc"], state)
		out.bcc = addressList(out.headerMap["bcc"], state)
		if state.addressCount > limits.GmailMaxAddressCount {
			return errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitAddresses, uint64(state.addressCount))
		}
		if t, ok := parseDate(firstHeader(out.headerMap["date"])); ok {
			out.sentAt = &t
		}
	}
	container := len(part.Parts) > 0
	if container {
		for i := range part.Parts {
			if i > int(^uint16(0)) {
				return providerDataError(c.account, "invalid_protocol")
			}
			childPath := append(append([]uint16(nil), path...), uint16(i))
			if err := c.walkPart(&part.Parts[i], depth+1, false, childPath, state, out, keepHeaders); err != nil {
				return err
			}
		}
		return nil
	}
	if err := c.validatePartBodyData(part); err != nil {
		return err
	}
	attachment, err := c.classifyAttachment(part, state)
	if err != nil {
		return err
	}
	if attachment != nil {
		out.attachments = append(out.attachments, *attachment)
		out.hasAttachments = true
		state.classifiedParts = append(state.classifiedParts, classifiedMimeAttachment{part: part, attachment: *attachment, childIndexPath: append([]uint16(nil), path...)})
		return nil
	}
	mediaType, charsetName := parsedContentType(part.MimeType)
	leaf := &mimeLeaf{
		partID:       part.PartID,
		contentType:  mediaType,
		charset:      charsetName,
		data:         part.Body.Data,
		attachmentID: part.Body.AttachmentID,
		size:         part.Body.Size,
	}
	switch leaf.contentType {
	case "text/plain":
		if out.textPart == nil {
			out.textPart = leaf
		}
	case "text/html":
		if out.htmlPart == nil {
			out.htmlPart = leaf
		}
	}
	return nil
}

func (c *Client) validatePartBodyData(part *gmailPart) error {
	if part == nil {
		return providerDataError(c.account, "invalid_protocol")
	}
	if part.Body.Size != nil && *part.Body.Size < 0 {
		return providerDataError(c.account, "invalid_json")
	}
	if part.Body.Data == nil {
		return nil
	}
	decodedSize, _, err := decodedAttachmentFacts(c.account, *part.Body.Data)
	if err != nil {
		return err
	}
	if part.Body.Size != nil && uint64(*part.Body.Size) != decodedSize {
		return providerDataError(c.account, "invalid_protocol")
	}
	return nil
}

func (c *Client) classifyAttachment(part *gmailPart, state *mimeWalkState) (*mail.Attachment, error) {
	if part.Body.AttachmentID != nil && *part.Body.AttachmentID == "" {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	if part.Body.Size != nil && *part.Body.Size < 0 {
		return nil, providerDataError(c.account, "invalid_json")
	}
	dataPresent := part.Body.Data != nil
	dataNonEmpty := dataPresent && *part.Body.Data != ""
	externalNonEmpty := part.Body.AttachmentID != nil && *part.Body.AttachmentID != ""
	if dataNonEmpty && externalNonEmpty {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	mediaType := normalizedContentType(part.MimeType)
	dispositionHeader := firstHeader(headerValues(part.Headers, "content-disposition"))
	disposition, _ := mimeparam.Split(dispositionHeader)
	// Gmail decodes the name into "filename" for most mail; the headers
	// are the fallback for names it leaves out, such as RFC 2231 pieces.
	filename := mimeparam.FileName(map[string]string{"filename": part.Filename}, nil)
	if filename == "" {
		filename = mimeparam.FileNameFromHeaders(dispositionHeader, firstHeader(headerValues(part.Headers, "content-type")))
	}
	contentID := normalizeContentID(firstHeader(headerValues(part.Headers, "content-id")))
	bodyType := mediaType == "text/plain" || mediaType == "text/html"
	hasCIDAttachment := contentID != "" && !bodyType
	evidence := disposition == "attachment" || filename != "" || hasCIDAttachment || (disposition == "inline" && !bodyType)
	if !evidence {
		return nil, nil
	}
	if !dataPresent && !externalNonEmpty && (part.Body.Size == nil || *part.Body.Size != 0) {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	if dataPresent && *part.Body.Data == "" && !externalNonEmpty && part.Body.Size != nil && *part.Body.Size != 0 {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	if part.PartID == "" || state.attachmentIDs[part.PartID] {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	state.attachmentIDs[part.PartID] = true
	isInline := disposition == "inline" || (hasCIDAttachment && disposition != "attachment")
	var decoded uint64
	decodedKnown := false
	if part.Body.Size != nil {
		decoded = uint64(*part.Body.Size)
		decodedKnown = true
	}
	meta := mail.AttachmentProviderMetadata{
		Source:            mail.MetadataSourceGmailAPI,
		SnapshotMessageID: state.messageID,
	}
	if part.Body.AttachmentID != nil && *part.Body.AttachmentID != "" {
		trueValue := true
		meta.ExternalAttachmentIDPresent = &trueValue
	}
	att := mail.Attachment{
		ID:               part.PartID,
		Name:             filename,
		ContentType:      mediaType,
		IsInline:         isInline,
		ContentID:        contentID,
		Kind:             mail.AttachmentKindFile,
		ProviderType:     "gmail_mime_part",
		ProviderMeta:     mustGmailMetadata(mail.MetadataAttachment, meta),
		SizeFactsPresent: true,
		SizeFacts: mail.AttachmentSizeFacts{
			DecodedSize:      decoded,
			DecodedSizeKnown: decodedKnown,
		},
	}
	if decodedKnown {
		att.Size = int64(decoded)
	}
	return &att, nil
}

func (c *Client) summaryFolder(labelIDs []string, snap labelSnapshot, fixed *mail.Folder) (mail.FolderSummary, mail.SummaryFolderBasis, []string, error) {
	for _, id := range labelIDs {
		if _, ok := snap.labels[id]; !ok {
			return mail.FolderSummary{}, "", nil, providerDataError(c.account, "invalid_protocol")
		}
	}
	userLabels := userLabelNames(labelIDs, snap)
	if fixed != nil {
		return mail.SummaryFrom(*fixed), basisForFolder(*fixed), userLabels, nil
	}
	for _, id := range []string{"INBOX", "SPAM", "TRASH", "SENT", "DRAFT"} {
		if hasLabel(labelIDs, id) {
			if f, ok := snap.byID[id]; ok {
				return mail.SummaryFrom(f), mail.SummaryBasisSystem, userLabels, nil
			}
		}
	}
	userFolders := make([]mail.Folder, 0)
	labelSet := make(map[string]bool, len(labelIDs))
	for _, id := range labelIDs {
		labelSet[id] = true
	}
	for _, f := range snap.folders {
		if labelSet[f.ID] && f.ProviderMeta != nil && f.ProviderMeta.LabelType == mail.LabelTypeUser {
			userFolders = append(userFolders, f)
		}
	}
	if len(userFolders) > 0 {
		return mail.SummaryFrom(userFolders[0]), mail.SummaryBasisUserLabel, userLabels, nil
	}
	return mail.FolderSummary{ID: "gmail-summary-unfiled/v1", DisplayName: "Unfiled"}, mail.SummaryBasisFallback, userLabels, nil
}

func basisForFolder(folder mail.Folder) mail.SummaryFolderBasis {
	if folder.ProviderMeta != nil && folder.ProviderMeta.LabelType == mail.LabelTypeUser {
		return mail.SummaryBasisUserLabel
	}
	return mail.SummaryBasisSystem
}

func userLabelNames(labelIDs []string, snap labelSnapshot) []string {
	out := make([]string, 0)
	for _, id := range labelIDs {
		label, ok := snap.labels[id]
		if ok && label.Type == "user" {
			out = append(out, label.Name)
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func hasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

func parseInternalDate(raw string) (time.Time, error) {
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms < 0 {
		return time.Time{}, err
	}
	return time.Unix(0, ms*int64(time.Millisecond)).UTC(), nil
}

func firstHeader(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func decodeHeader(value string) string {
	if value == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: func(charsetName string, input io.Reader) (io.Reader, error) {
		return charset.NewReaderLabel(charsetName, input)
	}}
	decoded, err := dec.DecodeHeader(value)
	if err != nil || !utf8.ValidString(decoded) {
		return value
	}
	return decoded
}

func headerValues(headers []gmailHeader, name string) []string {
	var out []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

func firstUsableMessageID(values []string) string {
	for _, value := range values {
		id, err := mail.NormalizeMessageIDScalar(value)
		if err == nil {
			return id
		}
	}
	return ""
}

func firstAddress(values []string, state *mimeWalkState) mail.Recipient {
	list := addressList(values, state)
	if len(list) == 0 {
		return mail.Recipient{}
	}
	return list[0]
}

func addressList(values []string, state *mimeWalkState) []mail.Recipient {
	var out []mail.Recipient
	for _, value := range values {
		addrs, err := netmail.ParseAddressList(value)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			state.addressCount++
			if state.addressCount > limits.GmailMaxAddressCount {
				return out
			}
			out = append(out, mail.Recipient{Name: addr.Name, Address: addr.Address})
		}
	}
	return out
}

func parseDate(value string) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	t, err := netmail.ParseDate(value)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func normalizedContentType(raw string) string {
	mediaType, _ := parsedContentType(raw)
	return mediaType
}

func parsedContentType(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "application/octet-stream", ""
	}
	mediaType, params, err := mime.ParseMediaType(raw)
	if err != nil {
		if i := strings.Index(raw, ";"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		return strings.ToLower(raw), ""
	}
	return strings.ToLower(mediaType), strings.ToLower(strings.TrimSpace(params["charset"]))
}

func safeOptionalString(value string) string {
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	if strings.TrimSpace(value) != value {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func normalizeContentID(value string) string {
	value = strings.Trim(value, " \t")
	if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
		value = value[1 : len(value)-1]
	}
	if value == "" || strings.ContainsAny(value, "<> \t\r\n") {
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return ""
		}
	}
	return value
}

func (c *Client) ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	if req.MetadataOnly {
		return c.readMetadataOnly(ctx, req.Selector)
	}
	mode := req.BodyMode
	if mode == "" {
		mode = mail.BodyModeReadable
	}
	switch mode {
	case mail.BodyModeReadable, mail.BodyModeText, mail.BodyModeHTML:
		id, err := c.resolveMessageID(ctx, req.Selector)
		if err != nil {
			return mail.Message{}, err
		}
		msg, err := c.fetchMessage(ctx, id, "full", "", "id,threadId,labelIds,snippet,internalDate,payload")
		if err != nil {
			return mail.Message{}, err
		}
		return c.fullMessage(ctx, *msg, mode)
	case mail.BodyModeRaw:
		id, err := c.resolveMessageID(ctx, req.Selector)
		if err != nil {
			return mail.Message{}, err
		}
		meta, err := c.fetchMessage(ctx, id, "full", "", "id,threadId,labelIds,snippet,internalDate,payload")
		if err != nil {
			return mail.Message{}, err
		}
		out, err := c.messageFromFull(ctx, *meta, false, mail.BodyModeRaw)
		if err != nil {
			return mail.Message{}, err
		}
		raw, err := c.fetchMessage(ctx, id, "raw", "", "id,threadId,raw")
		if err != nil {
			return mail.Message{}, err
		}
		if raw.ID != meta.ID || raw.ThreadID != meta.ThreadID || raw.Raw == "" {
			return mail.Message{}, providerDataError(c.account, "invalid_protocol")
		}
		data, err := decodeBase64URLBounded(c.account, raw.Raw, mail.MaxMessageBodyBytes)
		if err != nil {
			return mail.Message{}, err
		}
		encoding := "utf8"
		content := string(data)
		if !utf8.Valid(data) {
			encoding = "base64"
			content = base64.StdEncoding.EncodeToString(data)
		}
		out.Body = mail.MessageBody{ContentType: "message/rfc822", Mode: mail.BodyModeRaw, Encoding: encoding, Content: content}
		return out, nil
	default:
		return mail.Message{}, errs.NewWithDetails(errs.CodeUnsupportedBodyMode, "body mode is not supported by Gmail", map[string]any{
			"account":   c.account,
			"provider":  "gmail",
			"body_mode": string(mode),
		})
	}
}

func (c *Client) readMetadataOnly(ctx context.Context, selector mail.ResolvedMessageSelector) (mail.Message, error) {
	var msg *gmailMessage
	var err error
	if selector.Kind == mail.ResolvedSelectorInternetID {
		msg, err = c.resolveInternetID(ctx, selector.InternetID)
	} else {
		id, err := c.resolveMessageID(ctx, selector)
		if err != nil {
			return mail.Message{}, err
		}
		msg, err = c.fetchMessage(ctx, id, "metadata", "Message-ID", "id,threadId,payload/headers")
	}
	if err != nil {
		return mail.Message{}, err
	}
	internetID := ""
	if msg.Payload != nil {
		internetID = firstUsableMessageID(headerValues(msg.Payload.Headers, "Message-ID"))
	}
	return mail.Message{
		Account:           c.account,
		Provider:          "gmail",
		ID:                msg.ID,
		InternetMessageID: internetID,
		ThreadID:          msg.ThreadID,
		ProviderMeta:      mustGmailMetadata(mail.MetadataMessage, mail.MessageProviderMetadata{Source: mail.MetadataSourceGmailAPI}),
	}, nil
}

func (c *Client) fullMessage(ctx context.Context, msg gmailMessage, mode mail.BodyMode) (mail.Message, error) {
	return c.messageFromFull(ctx, msg, true, mode)
}

func (c *Client) messageFromFull(ctx context.Context, msg gmailMessage, includeBody bool, mode mail.BodyMode) (mail.Message, error) {
	if msg.ID == "" || msg.ThreadID == "" || msg.Payload == nil {
		return mail.Message{}, providerDataError(c.account, "invalid_protocol")
	}
	received, err := parseInternalDate(msg.InternalDate)
	if err != nil {
		return mail.Message{}, providerDataError(c.account, "invalid_protocol")
	}
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.Message{}, err
	}
	normalized, err := c.normalizePayload(&msg, true)
	if err != nil {
		return mail.Message{}, err
	}
	folder, _, userLabels, err := c.summaryFolder(msg.LabelIDs, snap, nil)
	if err != nil {
		return mail.Message{}, err
	}
	var body mail.MessageBody
	if includeBody {
		body, err = c.selectBody(ctx, msg.ID, normalized, mode)
		if err != nil {
			return mail.Message{}, err
		}
	}
	starred := hasLabel(msg.LabelIDs, "STARRED")
	out := mail.Message{
		Account:           c.account,
		Provider:          "gmail",
		ID:                msg.ID,
		InternetMessageID: normalized.internetMessageID,
		ThreadID:          msg.ThreadID,
		Folder:            folder,
		From:              normalized.from,
		Sender:            normalized.sender,
		ReplyTo:           normalized.replyTo,
		To:                normalized.to,
		CC:                normalized.cc,
		BCC:               normalized.bcc,
		Subject:           normalized.subject,
		ReceivedAt:        &received,
		SentAt:            normalized.sentAt,
		Headers:           normalized.headers,
		IsRead:            !hasLabel(msg.LabelIDs, "UNREAD"),
		Categories:        userLabels,
		HasAttachments:    normalized.hasAttachments,
		Attachments:       normalized.attachments,
		Body:              body,
		ProviderMeta:      mustGmailMetadata(mail.MetadataMessage, mail.MessageProviderMetadata{Source: mail.MetadataSourceGmailAPI, Starred: &starred}),
	}
	if out.ReplyTo == nil {
		out.ReplyTo = []mail.Recipient{}
	}
	if out.To == nil {
		out.To = []mail.Recipient{}
	}
	if out.CC == nil {
		out.CC = []mail.Recipient{}
	}
	if out.BCC == nil {
		out.BCC = []mail.Recipient{}
	}
	if out.Categories == nil {
		out.Categories = []string{}
	}
	if out.Headers == nil {
		out.Headers = []mail.Header{}
	}
	if out.Attachments == nil {
		out.Attachments = []mail.Attachment{}
	}
	return out, nil
}

func (c *Client) selectBody(ctx context.Context, messageID string, normalized normalizedMessage, mode mail.BodyMode) (mail.MessageBody, error) {
	var leaf *mimeLeaf
	switch mode {
	case mail.BodyModeReadable, mail.BodyModeText:
		leaf = normalized.textPart
		if leaf == nil {
			leaf = normalized.htmlPart
		}
	case mail.BodyModeHTML:
		leaf = normalized.htmlPart
	}
	if leaf == nil {
		return mail.MessageBody{}, errs.NewWithDetails(errs.CodeUnsupportedBodyMode, fmt.Sprintf("this message has no body that --body-mode %s can show; --body-mode raw shows the whole message", mode), map[string]any{
			"account":   c.account,
			"provider":  "gmail",
			"body_mode": string(mode),
		})
	}
	data, err := c.leafBodyBytes(ctx, messageID, leaf)
	if err != nil {
		return mail.MessageBody{}, err
	}
	contentType := leaf.contentType
	content, err := decodeTextBody(c.account, data, leaf.charset)
	if err != nil {
		return mail.MessageBody{}, err
	}
	if (mode == mail.BodyModeReadable || mode == mail.BodyModeText) && leaf.contentType == "text/html" {
		content = htmltext.ToReadable(content)
	}
	return mail.MessageBody{ContentType: contentType, Mode: mode, Content: content}, nil
}

func decodeTextBody(account string, data []byte, charsetName string) (string, error) {
	charsetName = strings.TrimSpace(charsetName)
	switch {
	case charsetName == "", strings.EqualFold(charsetName, "utf-8"), strings.EqualFold(charsetName, "utf8"):
		if !utf8.Valid(data) {
			return "", providerDataError(account, "invalid_charset")
		}
		return string(data), nil
	case strings.EqualFold(charsetName, "us-ascii"):
		for _, b := range data {
			if b >= utf8.RuneSelf {
				return "", providerDataError(account, "invalid_charset")
			}
		}
		return string(data), nil
	}
	r, err := charset.NewReaderLabel(charsetName, bytes.NewReader(data))
	if err != nil {
		return "", providerDataError(account, "invalid_charset")
	}
	decoded, err := io.ReadAll(r)
	if err != nil || !utf8.Valid(decoded) {
		return "", providerDataError(account, "invalid_charset")
	}
	return string(decoded), nil
}

func (c *Client) leafBodyBytes(ctx context.Context, messageID string, leaf *mimeLeaf) ([]byte, error) {
	if leaf == nil {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	if leaf.data != nil && *leaf.data != "" && leaf.attachmentID != nil && *leaf.attachmentID != "" {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	if leaf.data != nil && *leaf.data != "" {
		data, err := decodeBase64URLBounded(c.account, *leaf.data, mail.MaxMessageBodyBytes)
		if err != nil {
			return nil, err
		}
		if leaf.size != nil && *leaf.size < 0 {
			return nil, providerDataError(c.account, "invalid_json")
		}
		if leaf.size != nil && int64(len(data)) != *leaf.size {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		return data, nil
	}
	if leaf.attachmentID != nil {
		if *leaf.attachmentID == "" {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		values := url.Values{"fields": {"size,data"}}
		var resp gmailBody
		path := "/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(*leaf.attachmentID)
		operation := "/messages/" + url.PathEscape(messageID) + "/attachments/{attachment}"
		bodyStream, err := c.gmailGETLimitedReader(ctx, path, operation, values, func(status int, statusBody []byte) error {
			if status == http.StatusNotFound || status == http.StatusGone {
				return providerDataError(c.account, "invalid_protocol")
			}
			return classifyHTTPStatus(c.account, operation, status, statusBody, false)
		})
		if err != nil {
			return nil, err
		}
		defer bodyStream.Close()
		body, _, over, err := readAllLimited(bodyStream, limits.GmailMaxJSONResponseBytes)
		if err != nil || over {
			return nil, providerDataError(c.account, "invalid_json")
		}
		if err := decodeJSONResponse(c.account, body, &resp); err != nil {
			return nil, err
		}
		if resp.AttachmentID != nil || resp.Data == nil {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		data, err := decodeBase64URLBounded(c.account, *resp.Data, mail.MaxMessageBodyBytes)
		if err != nil {
			return nil, err
		}
		if resp.Size != nil && *resp.Size < 0 {
			return nil, providerDataError(c.account, "invalid_json")
		}
		if resp.Size != nil && int64(len(data)) != *resp.Size {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		if leaf.size != nil && *leaf.size < 0 {
			return nil, providerDataError(c.account, "invalid_json")
		}
		if leaf.size != nil && int64(len(data)) != *leaf.size {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		return data, nil
	}
	if leaf.size != nil && *leaf.size < 0 {
		return nil, providerDataError(c.account, "invalid_json")
	}
	if leaf.size != nil && *leaf.size != 0 {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	return []byte{}, nil
}

func decodeBase64URLBounded(account, raw string, limit int64) ([]byte, error) {
	if int64(len(raw)) > limits.GmailMaxRawEncodedBytes && limit == mail.MaxMessageBodyBytes {
		// Nothing to do: a body that is too large is reported as a body limit
		// after decoding, so the encoded size alone does not stop it here.
	}
	data, err := decodeBase64URL(account, raw)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errs.NewReadLimitExceeded(account, errs.ProviderGmail, errs.ReadLimitBodyBytes, uint64(limit+1))
	}
	return data, nil
}

func decodeBase64URL(account, raw string) ([]byte, error) {
	decoder, err := newBase64URLDecoder(account, raw)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(decoder)
	if err != nil {
		return nil, providerDataError(account, "invalid_base64")
	}
	return data, nil
}

func newBase64URLDecoder(account, raw string) (io.Reader, error) {
	enc, err := base64URLEncoding(account, raw)
	if err != nil {
		return nil, err
	}
	return base64.NewDecoder(enc, strings.NewReader(raw)), nil
}

func base64URLEncoding(account, raw string) (*base64.Encoding, error) {
	if strings.ContainsAny(raw, "+/ \t\r\n") || strings.Count(raw, "=") > 2 {
		return nil, providerDataError(account, "invalid_base64")
	}
	if idx := strings.IndexByte(raw, '='); idx >= 0 {
		for i := idx; i < len(raw); i++ {
			if raw[i] != '=' {
				return nil, providerDataError(account, "invalid_base64")
			}
		}
	}
	if strings.Contains(raw, "=") {
		return base64.URLEncoding.Strict(), nil
	} else if rem := len(raw) % 4; rem == 1 {
		return nil, providerDataError(account, "invalid_base64")
	}
	return base64.RawURLEncoding.Strict(), nil
}

func (c *Client) resolveMessageID(ctx context.Context, selector mail.ResolvedMessageSelector) (string, error) {
	switch selector.Kind {
	case mail.ResolvedSelectorID:
		if selector.ID == "" {
			return "", errs.New(errs.CodeInvalidSelector, "message id is required")
		}
		return selector.ID, nil
	case mail.ResolvedSelectorInternetID:
		msg, err := c.resolveInternetID(ctx, selector.InternetID)
		if err != nil {
			return "", err
		}
		return msg.ID, nil
	default:
		return "", errs.New(errs.CodeInvalidSelector, "reading a message needs --id or --internet-id")
	}
}

func (c *Client) resolveInternetID(ctx context.Context, raw string) (*gmailMessage, error) {
	normalized, err := mail.NormalizeMessageIDScalar(raw)
	if err != nil {
		return nil, errs.New(errs.CodeInvalidSelector, "the --internet-id value is not a valid Message-ID")
	}
	q, err := gmailQuoted(normalized)
	if err != nil {
		return nil, err
	}
	ids, err := c.collectMessageIDs(ctx, listCollectRequest{Query: "rfc822msgid:" + q, IncludeSpamTrash: true, Limit: limits.GmailMaxHydratedMessages})
	if err != nil {
		return nil, err
	}
	var matches []gmailMessage
	for _, item := range ids {
		msg, err := c.fetchMessage(ctx, item.ID, "metadata", "Message-ID", "id,threadId,payload/headers")
		if err != nil {
			return nil, err
		}
		if msg.ID != item.ID || msg.ThreadID != item.ThreadID || msg.Payload == nil {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		if firstUsableMessageID(headerValues(msg.Payload.Headers, "Message-ID")) == normalized {
			matches = append(matches, *msg)
			if len(matches) == 2 {
				return nil, ambiguousInternetID(c.account, normalized, []string{matches[0].ID, matches[1].ID})
			}
		}
	}
	if len(matches) == 0 {
		return nil, messageNotFound(c.account, mail.ResolvedSelectorInternetID)
	}
	return &matches[0], nil
}

func (c *Client) ReadThread(ctx context.Context, req mail.ReadThreadRequest) (mail.Thread, error) {
	threadID := ""
	switch req.Selector.Kind {
	case mail.ResolvedSelectorThreadID:
		threadID = req.Selector.ThreadID
	case mail.ResolvedSelectorID:
		msg, err := c.fetchMessage(ctx, req.Selector.ID, "metadata", "Message-ID", "id,threadId,payload/headers")
		if err != nil {
			return mail.Thread{}, err
		}
		threadID = msg.ThreadID
	case mail.ResolvedSelectorInternetID:
		msg, err := c.resolveInternetID(ctx, req.Selector.InternetID)
		if err != nil {
			return mail.Thread{}, err
		}
		threadID = msg.ThreadID
	default:
		return mail.Thread{}, errs.New(errs.CodeInvalidSelector, "reading a thread needs --id, --internet-id or --thread-id")
	}
	if threadID == "" {
		return mail.Thread{}, providerDataError(c.account, "invalid_protocol")
	}
	values := url.Values{}
	values.Set("format", "full")
	values.Set("fields", "id,messages(id,threadId,labelIds,snippet,internalDate,payload)")
	var gt gmailThread
	err := c.getJSON(ctx, "/threads/"+url.PathEscape(threadID), values, limits.GmailMaxJSONResponseBytes, &gt)
	if err != nil {
		var product *errs.Error
		if errors.As(err, &product) && product.Code() == errs.CodeMessageNotFound {
			return mail.Thread{}, threadNotFound(c.account, threadID)
		}
		return mail.Thread{}, err
	}
	if gt.ID != threadID || len(gt.Messages) == 0 {
		return mail.Thread{}, threadNotFound(c.account, threadID)
	}
	if len(gt.Messages) > limits.GmailMaxHydratedMessages {
		return mail.Thread{}, errs.NewReadLimitExceeded(c.account, errs.ProviderGmail, errs.ReadLimitHydratedMessages, uint64(limits.GmailMaxHydratedMessages+1))
	}
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.Thread{}, err
	}
	seen := map[string]bool{}
	rows := make([]mail.ThreadMessage, 0, len(gt.Messages))
	for _, msg := range gt.Messages {
		if msg.ID == "" || msg.ThreadID != threadID || seen[msg.ID] {
			return mail.Thread{}, providerDataError(c.account, "invalid_protocol")
		}
		seen[msg.ID] = true
		summary, err := c.messageToSummary(msg, snap, nil)
		if err != nil {
			return mail.Thread{}, err
		}
		rows = append(rows, mail.ThreadMessage{
			Account:           summary.Account,
			Provider:          summary.Provider,
			ID:                summary.ID,
			InternetMessageID: summary.InternetMessageID,
			ThreadID:          summary.ThreadID,
			Folder:            summary.Folder,
			From:              summary.From,
			Subject:           summary.Subject,
			ReceivedAt:        summary.ReceivedAt,
			SentAt:            summary.SentAt,
			IsRead:            summary.IsRead,
			HasAttachments:    summary.HasAttachments,
			Preview:           summary.Preview,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := threadTime(rows[i]), threadTime(rows[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return rows[i].ID < rows[j].ID
	})
	complete := true
	return mail.Thread{
		Account:      c.account,
		Provider:     "gmail",
		ThreadID:     threadID,
		Messages:     rows,
		ProviderMeta: mustGmailMetadata(mail.MetadataThread, mail.ThreadProviderMetadata{Basis: mail.ThreadBasisGmailNative, Complete: &complete}),
	}, nil
}

func threadTime(row mail.ThreadMessage) time.Time {
	if row.ReceivedAt != nil {
		return *row.ReceivedAt
	}
	if row.SentAt != nil {
		return *row.SentAt
	}
	return time.Time{}
}
