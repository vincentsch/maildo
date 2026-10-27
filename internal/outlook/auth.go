package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// tokenResponse is the part of Microsoft's token answer that the provider
// uses. parseTokenResponse fills in defaults and registers the secrets.
type tokenResponse struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	TokenType    string
	Scopes       []string
	IDToken      string
}

// oauthTokenError says what kind of token error Microsoft sent, apart from
// the text people see. Microsoft's own error_description and the raw answer
// never go into maildo's errors.
type oauthTokenError struct {
	Code      string
	Status    int
	Message   string
	Repair    bool
	Transient bool
}

func (e *oauthTokenError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "Outlook token request failed"
}

// InteractiveAuth signs the user in, in the browser or with a device code,
// and returns the new login and the address it belongs to.
func (p *Provider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	if ac.Operation != provider.InteractiveLogin && ac.Operation != provider.InteractiveRepair {
		return provider.InteractiveAuthResult{}, provider.ErrProviderRuntimeDenied
	}
	clientID, err := p.resolveClientID(ac.Setup)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	var cache *TokenCache
	if ac.Flow == provider.InteractiveAuthFlowDeviceCode {
		cache, err = p.deviceCodeLogin(ctx, ac.Runtime, clientID)
	} else {
		cache, err = p.authCodeLogin(ctx, ac.Runtime, clientID)
	}
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	cache.AccountName = ac.Account
	registerCacheSecrets(ac.Runtime, cache)
	return provider.InteractiveAuthResult{
		Setup:      ac.Setup.Clone(),
		Credential: cache,
		Identity: provider.Identity{
			Email:             cache.VerifiedEmail,
			DisplayName:       cache.DisplayName,
			ProviderAccountID: cache.ProviderAccountID,
			Assurance:         provider.IdentityProviderVerified,
		},
	}, nil
}

// authCodeLogin signs in through the browser (OAuth with PKCE). Microsoft
// sends the result to a short-lived listener on this computer. It returns
// only after Graph has confirmed the account.
func (p *Provider) authCodeLogin(ctx context.Context, runtime provider.InteractiveProviderRuntime, clientID string) (*TokenCache, error) {
	verifier, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}
	state, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(p.opts.BindHost, "0"))
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not start the local listener for the Outlook sign-in")
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := "http://" + net.JoinHostPort(p.opts.RedirectHost, strconv.Itoa(port)) + "/"
	callbacks := make(chan authCodeCallback, 1)
	// The listener takes only the first callback. The state check below is
	// what proves the callback belongs to this sign-in. Microsoft's error
	// text is not passed on to the terminal.
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cb := authCodeCallback{
			Code:        q.Get("code"),
			State:       q.Get("state"),
			OAuthError:  q.Get("error"),
			Description: q.Get("error_description"),
		}
		select {
		case callbacks <- cb:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>maildo Outlook sign-in</title><p>You can close this window and go back to the terminal. maildo shows there whether the sign-in worked.</p>")
	})}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case callbacks <- authCodeCallback{ServeError: err}:
			default:
			}
		}
	}()
	authorizeURL, err := p.authorizeURL(clientID, redirectURI, state, verifier)
	if err != nil {
		return nil, err
	}
	if err := provider.OpenSignInPage(ctx, runtime, authorizeURL, p.opts.LoopbackTimeout); err != nil {
		return nil, err
	}
	timer := time.NewTimer(p.opts.LoopbackTimeout)
	defer timer.Stop()
	var cb authCodeCallback
	select {
	case <-ctx.Done():
		return nil, rungrad.NewError(rungrad.ExitAuth, "sign-in was not completed")
	case <-timer.C:
		return nil, rungrad.NewError(rungrad.ExitAuth, "sign-in was not completed")
	case cb = <-callbacks:
	}
	if cb.ServeError != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "Outlook sign-in listener failed")
	}
	// Register the code as a secret before any check can fail, so no error
	// message can show it.
	if cb.Code != "" {
		if err := runtime.RegisterSecret(cb.Code); err != nil {
			return nil, err
		}
	}
	if cb.OAuthError != "" {
		return nil, rungrad.NewError(rungrad.ExitAuth, "Outlook sign-in failed")
	}
	if cb.State != state {
		return nil, rungrad.NewError(rungrad.ExitAuth, "the sign-in answer did not belong to this sign-in; try again")
	}
	if cb.Code == "" {
		return nil, rungrad.NewError(rungrad.ExitAuth, "Outlook sign-in failed")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {cb.Code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"scope":         {scopeParam(p.opts.Scopes)},
	}
	token, err := p.exchangeToken(ctx, p.opts.TokenURL, form, true, p.opts.Scopes, runtime)
	if err != nil {
		return nil, cleanLoginTokenError(err)
	}
	return p.verifyAndBuildCache(ctx, runtime, clientID, token)
}

// authCodeCallback is what the local listener got from the browser, or the
// listener's own error.
type authCodeCallback struct {
	Code        string
	State       string
	OAuthError  string
	Description string
	ServeError  error
}

// authorizeURL builds the address of Microsoft's sign-in page. It always
// shows the account picker, so a repair can pick the right mailbox.
func (p *Provider) authorizeURL(clientID, redirectURI, state, verifier string) (string, error) {
	u, err := url.Parse(p.opts.AuthorizeURL)
	if err != nil {
		return "", rungrad.NewError(rungrad.ExitAPI, "Outlook authorize URL is invalid")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopeParam(p.opts.Scopes))
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// deviceCodeLogin signs in on a machine without a browser: the user opens a
// page on another device and types a code. Only that page and the user code
// are shown. The device code itself is registered as a secret.
func (p *Provider) deviceCodeLogin(ctx context.Context, runtime provider.InteractiveProviderRuntime, clientID string) (*TokenCache, error) {
	form := url.Values{
		"client_id": {clientID},
		"scope":     {scopeParam(p.opts.Scopes)},
	}
	resp, err := formPost(ctx, p.opts.HTTPClient, p.opts.DeviceCodeURL, form)
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not start Outlook device sign-in")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not read Microsoft's answer to the device sign-in")
	}
	device, err := parseDeviceCodeResponse(resp.StatusCode, body)
	if err != nil {
		return nil, err
	}
	if err := runtime.RegisterSecret(device.DeviceCode); err != nil {
		return nil, err
	}
	if err := runtime.WriteNotice(fmt.Sprintf("To sign in, open %s and enter the code %s", device.VerificationURI, device.UserCode)); err != nil {
		return nil, err
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresAt := p.now().Add(time.Duration(device.ExpiresIn) * time.Second)
	for {
		if !p.now().Before(expiresAt) {
			return nil, rungrad.NewError(rungrad.ExitAuth, "the device code expired before the sign-in was finished; run the command again")
		}
		if err := p.opts.DevicePollSleep(ctx, interval); err != nil {
			return nil, rungrad.NewError(rungrad.ExitAuth, "sign-in was not completed")
		}
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {clientID},
			"device_code": {device.DeviceCode},
		}
		token, err := p.exchangeToken(ctx, p.opts.TokenURL, form, true, p.opts.Scopes, runtime)
		if err == nil {
			return p.verifyAndBuildCache(ctx, runtime, clientID, token)
		}
		var tokenErr *oauthTokenError
		if !asOAuthTokenError(err, &tokenErr) {
			return nil, err
		}
		// While the user has not finished, Microsoft answers the polling
		// with errors. Only authorization_pending and slow_down mean "keep
		// asking". Every other error ends the sign-in, without Microsoft's
		// text.
		switch tokenErr.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return nil, rungrad.NewError(rungrad.ExitAuth, "the device code expired before the sign-in was finished; run the command again")
		case "access_denied", "authorization_declined":
			return nil, rungrad.NewError(rungrad.ExitAuth, "the Outlook sign-in was declined")
		default:
			return nil, cleanLoginTokenError(err)
		}
	}
}

// deviceCodeResponse is Microsoft's answer when a device sign-in starts.
// Only VerificationURI and UserCode may be shown to the user.
type deviceCodeResponse struct {
	DeviceCode       string `json:"device_code"`
	UserCode         string `json:"user_code"`
	VerificationURI  string `json:"verification_uri"`
	ExpiresIn        int    `json:"expires_in"`
	Interval         int    `json:"interval"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// parseDeviceCodeResponse checks the fields maildo needs. It ignores
// Microsoft's ready-made message, so no text from Microsoft reaches the
// terminal.
func parseDeviceCodeResponse(status int, body []byte) (deviceCodeResponse, error) {
	var out deviceCodeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return out, rungrad.NewError(rungrad.ExitAPI, "could not read Microsoft's answer to the device sign-in")
	}
	if status < 200 || status >= 300 || out.Error != "" {
		return out, rungrad.NewError(rungrad.ExitAuth, "could not start Outlook device sign-in")
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" || out.ExpiresIn <= 0 {
		return out, rungrad.NewError(rungrad.ExitAuth, "Microsoft's answer to the device sign-in was incomplete")
	}
	return out, nil
}

// exchangeToken sends a token request. Errors come back as oauthTokenError,
// never with Microsoft's raw answer.
func (p *Provider) exchangeToken(ctx context.Context, endpoint string, form url.Values, requireRefresh bool, requestedScopes []string, registrar provider.SecretRegistrar) (*tokenResponse, error) {
	resp, err := formPost(ctx, p.opts.HTTPClient, endpoint, form)
	if err != nil {
		return nil, &oauthTokenError{Message: "could not reach Outlook token endpoint", Transient: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &oauthTokenError{Message: "could not read Outlook token response", Transient: true}
	}
	return p.parseTokenResponse(resp.StatusCode, body, requireRefresh, requestedScopes, func(v string) error {
		if registrar != nil {
			return registrar.RegisterSecret(v)
		}
		return nil
	})
}

func (p *Provider) resolveClientID(setup maildoconfig.ProviderSetup) (string, error) {
	if setup.Outlook != nil && strings.TrimSpace(setup.Outlook.ClientID) != "" {
		return strings.TrimSpace(setup.Outlook.ClientID), nil
	}
	return "", errs.New(errs.CodeAuthAppConfigMissing, "Outlook needs the client id of your own Microsoft app registration. Register one (see https://github.com/vincentsch/maildo/blob/main/docs/providers/outlook.md), then run: maildo config set outlook.client_id <id>")
}

// parseTokenResponse reads a token answer, successful or not. Tokens are
// registered as secrets before any check, so even a broken answer cannot
// show them in later output.
func (p *Provider) parseTokenResponse(status int, body []byte, requireRefresh bool, requestedScopes []string, register func(string) error) (*tokenResponse, error) {
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		TokenType        string `json:"token_type"`
		Scope            string `json:"scope"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		if status < 200 || status >= 300 {
			return nil, classifyTokenError(fmt.Sprintf("http_%d", status), status)
		}
		return nil, &oauthTokenError{Message: "could not decode Outlook token response", Repair: true}
	}
	if raw.AccessToken != "" {
		if err := register(raw.AccessToken); err != nil {
			return nil, err
		}
	}
	if raw.RefreshToken != "" {
		if err := register(raw.RefreshToken); err != nil {
			return nil, err
		}
	}
	if raw.IDToken != "" {
		if err := register(raw.IDToken); err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 || raw.Error != "" {
		code := raw.Error
		if code == "" {
			code = fmt.Sprintf("http_%d", status)
		}
		return nil, classifyTokenError(code, status)
	}
	if raw.AccessToken == "" {
		return nil, rungrad.NewError(rungrad.ExitAuth, "Outlook token response did not include an access token")
	}
	if requireRefresh && raw.RefreshToken == "" {
		return nil, rungrad.NewError(rungrad.ExitAuth, "Outlook token response did not include a refresh token")
	}
	expiresIn := raw.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	tokenType := raw.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return &tokenResponse{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    p.now().Add(time.Duration(expiresIn) * time.Second),
		TokenType:    tokenType,
		Scopes:       splitScopes(raw.Scope, requestedScopes),
		IDToken:      raw.IDToken,
	}, nil
}

// classifyTokenError sorts Microsoft's token errors into those that need a
// new sign-in (Repair) and those worth trying again later (Transient).
func classifyTokenError(code string, status int) *oauthTokenError {
	code = strings.ToLower(strings.TrimSpace(code))
	err := &oauthTokenError{Code: code, Status: status, Message: "Outlook token request failed"}
	switch code {
	case "invalid_grant", "interaction_required", "consent_required", "invalid_scope", "expired_token", "access_denied", "authorization_declined":
		err.Repair = true
	case "authorization_pending", "slow_down":
	case "temporarily_unavailable", "server_error":
		err.Transient = true
	default:
		if status >= 500 || status == 0 {
			err.Transient = true
		}
	}
	return err
}

// cleanLoginTokenError turns a token error during login into a short login
// error. Refresh handles token errors differently, because there a failure
// that needs a new sign-in becomes auth_repair_required.
func cleanLoginTokenError(err error) error {
	var tokenErr *oauthTokenError
	if asOAuthTokenError(err, &tokenErr) {
		if tokenErr.Transient {
			return rungrad.NewError(rungrad.ExitAPI, "could not reach Microsoft to finish the sign-in; try again")
		}
		return rungrad.NewError(rungrad.ExitAuth, "Outlook sign-in failed")
	}
	return err
}

func asOAuthTokenError(err error, out **oauthTokenError) bool {
	if err == nil {
		return false
	}
	tokenErr, ok := err.(*oauthTokenError)
	if ok && out != nil {
		*out = tokenErr
	}
	return ok
}
