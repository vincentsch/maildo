package outlook

import (
	"context"
	"net/url"
	"strings"

	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// ForcedRefresh renews the access token now, with the refresh token. It never
// opens a sign-in page.
func (p *Provider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	cache, refreshed, rotated, err := p.ensureFresh(ctx, ac, true)
	if cache != nil {
		defer cache.Destroy()
	}
	if err != nil {
		return provider.RefreshResult{}, err
	}
	return provider.RefreshResult{
		RefreshChecked:      refreshed,
		CredentialChanged:   refreshed,
		RotatedRefreshToken: rotated,
	}, nil
}

// ensureFresh is used by ForcedRefresh and by Client. With force set it
// always asks Microsoft for a new token. Without it, it does so only when the
// access token is about to expire.
func (p *Provider) ensureFresh(ctx context.Context, ac provider.StoredAuthContext, force bool) (*TokenCache, bool, bool, error) {
	var currentForClient *TokenCache
	var refreshed bool
	var rotated bool
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() {
			return repairRequired(ac.Account)
		}
		if !localUsableCredential(cache, ac.Provider, ac.Account, ac.ExpectedEmail, ac.ExpectedProviderAccountID) {
			return repairRequired(ac.Account)
		}
		need := force || cache.IsExpired(p.now(), p.opts.RefreshSkew)
		if !need {
			currentForClient = cache.clone()
			return nil
		}
		clientID := strings.TrimSpace(cache.ClientID)
		if clientID == "" && ac.Setup.Outlook != nil {
			clientID = strings.TrimSpace(ac.Setup.Outlook.ClientID)
		}
		if clientID == "" {
			return repairRequired(ac.Account)
		}
		scopes := cache.Scopes
		if len(scopes) == 0 {
			scopes = p.opts.Scopes
		}
		authority := cache.Authority
		if strings.TrimSpace(authority) == "" {
			authority = p.opts.Authority
		}
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {clientID},
			"refresh_token": {cache.RefreshTokenValue()},
			"scope":         {scopeParam(scopes)},
		}
		token, err := p.exchangeToken(ctx, p.endpointForAuthority(authority, "token"), form, false, scopes, ac.Runtime)
		if err != nil {
			var tokenErr *oauthTokenError
			if asOAuthTokenError(err, &tokenErr) {
				if tokenErr.Repair {
					return repairRequired(ac.Account)
				}
				return rungrad.NewError(rungrad.ExitAPI, "could not renew the Outlook login right now; try again later")
			}
			return repairRequired(ac.Account)
		}
		if !scopesSatisfy(token.Scopes, requiredResourceScopes) {
			return repairRequired(ac.Account)
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
		next.TokenType = token.TokenType
		next.Scopes = append([]string(nil), token.Scopes...)
		next.ClientID = clientID
		if next.Authority == "" {
			next.Authority = authority
		}
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
