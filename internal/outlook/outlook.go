// Package outlook is maildo's provider for Outlook.com and Microsoft 365. It
// signs in with the user's own Microsoft app registration and works on the
// mailbox through Microsoft Graph.
package outlook

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad"
)

const (
	defaultAuthority    = "https://login.microsoftonline.com/common"
	defaultGraphBaseURL = "https://graph.microsoft.com/v1.0"
	defaultRedirectHost = "localhost"
	defaultBindHost     = "127.0.0.1"
	envClientID         = "MAILDO_OUTLOOK_CLIENT_ID"
)

// DefaultScopes are requested during Outlook sign-in.
var DefaultScopes = []string{
	"openid",
	"profile",
	"offline_access",
	"User.Read",
	"Mail.ReadWrite",
	"Mail.Send",
	"MailboxSettings.ReadWrite",
}

var requiredResourceScopes = []string{
	"User.Read",
	"Mail.ReadWrite",
	"Mail.Send",
	"MailboxSettings.ReadWrite",
}

// Options configures the Outlook provider. Empty fields get the defaults.
//
// Tests set the URLs. An empty URL is built from Authority, and a refresh
// builds it again from the authority saved with the login.
type Options struct {
	Authority       string
	AuthorizeURL    string
	TokenURL        string
	DeviceCodeURL   string
	GraphBaseURL    string
	Scopes          []string
	HTTPClient      *http.Client
	Now             func() time.Time
	RefreshSkew     time.Duration
	LoopbackTimeout time.Duration
	SearchResultCap int
	RedirectHost    string
	BindHost        string
	DevicePollSleep func(ctx context.Context, d time.Duration) error
}

// Provider is the Outlook implementation of provider.Provider.
//
// The override fields remember which URLs the caller set before New filled
// in defaults. A refresh needs this: a saved login can carry another
// authority, and a URL New filled in must not replace it.
type Provider struct {
	opts               Options
	authorizeOverride  bool
	tokenOverride      bool
	deviceCodeOverride bool
}

// New returns an Outlook provider and fills in defaults for empty options.
func New(opts Options) *Provider {
	authorizeOverride := opts.AuthorizeURL != ""
	tokenOverride := opts.TokenURL != ""
	deviceCodeOverride := opts.DeviceCodeURL != ""
	if strings.TrimSpace(opts.Authority) == "" {
		opts.Authority = defaultAuthority
	}
	opts.Authority = strings.TrimRight(opts.Authority, "/")
	if opts.AuthorizeURL == "" {
		opts.AuthorizeURL = endpoint(opts.Authority, "authorize")
	}
	if opts.TokenURL == "" {
		opts.TokenURL = endpoint(opts.Authority, "token")
	}
	if opts.DeviceCodeURL == "" {
		opts.DeviceCodeURL = endpoint(opts.Authority, "devicecode")
	}
	if strings.TrimSpace(opts.GraphBaseURL) == "" {
		opts.GraphBaseURL = defaultGraphBaseURL
	}
	opts.GraphBaseURL = strings.TrimRight(opts.GraphBaseURL, "/")
	if len(opts.Scopes) == 0 {
		opts.Scopes = append([]string(nil), DefaultScopes...)
	} else {
		opts.Scopes = append([]string(nil), opts.Scopes...)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RefreshSkew == 0 {
		opts.RefreshSkew = 300 * time.Second
	}
	if opts.LoopbackTimeout == 0 {
		opts.LoopbackTimeout = 180 * time.Second
	}
	if opts.SearchResultCap == 0 {
		opts.SearchResultCap = 1000
	}
	if strings.TrimSpace(opts.RedirectHost) == "" {
		opts.RedirectHost = defaultRedirectHost
	}
	if strings.TrimSpace(opts.BindHost) == "" {
		opts.BindHost = defaultBindHost
	}
	if opts.DevicePollSleep == nil {
		opts.DevicePollSleep = sleepContext
	}
	return &Provider{
		opts:               opts,
		authorizeOverride:  authorizeOverride,
		tokenOverride:      tokenOverride,
		deviceCodeOverride: deviceCodeOverride,
	}
}

func (p *Provider) Name() string { return "outlook" }

func endpoint(authority, kind string) string {
	return strings.TrimRight(authority, "/") + "/oauth2/v2.0/" + kind
}

// endpointForAuthority builds the sign-in endpoint for authority, unless the
// caller set that URL in Options.
func (p *Provider) endpointForAuthority(authority, kind string) string {
	switch kind {
	case "authorize":
		if p.authorizeOverride {
			return p.opts.AuthorizeURL
		}
	case "token":
		if p.tokenOverride {
			return p.opts.TokenURL
		}
	case "devicecode":
		if p.deviceCodeOverride {
			return p.opts.DeviceCodeURL
		}
	}
	if strings.TrimSpace(authority) == "" {
		authority = p.opts.Authority
	}
	return endpoint(authority, kind)
}

func (p *Provider) now() time.Time {
	return p.opts.Now()
}

func randomBase64URL(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", rungrad.NewError(rungrad.ExitAPI, "could not start the Outlook sign-in")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func scopeParam(scopes []string) string {
	return strings.Join(scopes, " ")
}

// splitScopes reads a missing scope field as "the scopes maildo asked for".
// Some token answers leave the field out.
func splitScopes(scope string, fallback []string) []string {
	fields := strings.Fields(scope)
	if len(fields) == 0 {
		return append([]string(nil), fallback...)
	}
	return fields
}

func scopesSatisfy(granted, required []string) bool {
	for _, req := range required {
		if !scopeGranted(granted, req) {
			return false
		}
	}
	return true
}

// scopeGranted accepts both short Graph scopes and fully qualified resource
// scopes, e.g. "Mail.Send" and "https://graph.microsoft.com/Mail.Send".
func scopeGranted(granted []string, required string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	for _, scope := range granted {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == required || strings.HasSuffix(scope, "/"+required) {
			return true
		}
	}
	return false
}

func repairRequired(account string) error {
	return errs.New(errs.CodeAuthRepairRequired, fmt.Sprintf("the saved login for %q cannot be used; run %s", account, repairCommand(account)))
}

func repairCommand(account string) string {
	return "maildo auth repair " + account
}

func formPost(ctx context.Context, client *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not prepare Outlook request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return noRedirectHTTPClient(client).Do(req)
}
