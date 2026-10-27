package outlook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// verifyAndBuildCache asks Graph /me who signed in and turns the token answer
// into the saved login.
//
// Graph /me decides the identity. Its id ties later refreshes to the same
// Microsoft account. The tenant id from the ID token is saved only as extra
// information.
func (p *Provider) verifyAndBuildCache(ctx context.Context, registrar provider.SecretRegistrar, clientID string, token *tokenResponse) (*TokenCache, error) {
	if token == nil {
		return nil, rungrad.NewError(rungrad.ExitAuth, "Outlook sign-in failed")
	}
	if registrar != nil {
		if err := registrar.RegisterSecret("Bearer " + token.AccessToken); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.opts.GraphBaseURL+"/me", nil)
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not prepare Outlook identity verification")
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	graph := &Client{graphBaseURL: p.opts.GraphBaseURL, http: p.opts.HTTPClient}
	resp, err := graph.doGraphRequestNoRedirect(req)
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not verify the signed-in account")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not verify the signed-in account")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, rungrad.NewError(rungrad.ExitAuth, "could not verify the signed-in account")
	}
	var profile struct {
		ID                string `json:"id"`
		DisplayName       string `json:"displayName"`
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not read the account details Outlook sent")
	}
	// Work accounts usually return mail. Personal Microsoft accounts can leave
	// mail empty, so accept an email-like userPrincipalName as the mailbox email.
	email := strings.TrimSpace(profile.Mail)
	if email == "" && isEmailLike(profile.UserPrincipalName) {
		email = strings.TrimSpace(profile.UserPrincipalName)
	}
	if email == "" {
		return nil, errs.New(errs.CodeProviderIdentityUnavailable, "Outlook did not say which mail address belongs to this sign-in")
	}
	if !scopesSatisfy(token.Scopes, requiredResourceScopes) {
		return nil, rungrad.NewError(rungrad.ExitAuth, "the Outlook sign-in did not grant all the permissions maildo needs; sign in again and accept them, or ask your administrator to grant consent")
	}
	return NewTokenCache(TokenCacheData{
		Version:           1,
		Provider:          "outlook",
		Authority:         p.opts.Authority,
		ClientID:          clientID,
		Scopes:            append([]string(nil), token.Scopes...),
		TokenType:         token.TokenType,
		AccessToken:       token.AccessToken,
		RefreshToken:      token.RefreshToken,
		ExpiresAt:         token.ExpiresAt,
		VerifiedEmail:     email,
		DisplayName:       profile.DisplayName,
		ProviderAccountID: profile.ID,
		TenantID:          tenantIDFromIDToken(token.IDToken),
	}), nil
}

// isEmailLike is kept small on purpose. It only decides whether the user
// principal name looks enough like an address to stand in for one.
func isEmailLike(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Count(s, "@") != 1 {
		return false
	}
	parts := strings.SplitN(s, "@", 2)
	return parts[0] != "" && strings.Contains(parts[1], ".")
}

// tenantIDFromIDToken reads the tenant id from the ID token without checking
// its signature. The value is only saved, never trusted for a decision.
func tenantIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(padBase64URL(parts[1]))
		if err != nil {
			return ""
		}
	}
	var claims struct {
		TenantID string `json:"tid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.TenantID
}

func padBase64URL(s string) string {
	if rem := len(s) % 4; rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	return s
}
