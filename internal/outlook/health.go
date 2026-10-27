package outlook

import (
	"context"
	"strings"

	"github.com/vincentsch/maildo/internal/provider"
)

// LocalHealth checks the saved Outlook login without going online.
func (p *Provider) LocalHealth(ctx context.Context, ac provider.StoredAuthContext) (provider.LocalStatus, error) {
	var status provider.LocalStatus
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() {
			status = provider.LocalRepairRequired
			return nil
		}
		if !localUsableCredential(cache, ac.Provider, ac.Account, ac.ExpectedEmail, ac.ExpectedProviderAccountID) {
			status = provider.LocalRepairRequired
			return nil
		}
		if strings.TrimSpace(cache.ClientID) == "" && (ac.Setup.Outlook == nil || strings.TrimSpace(ac.Setup.Outlook.ClientID) == "") {
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

func localUsableCredential(cache *TokenCache, providerName, account, expectedEmail, expectedProviderAccountID string) bool {
	if cache == nil || cache.Destroyed() {
		return false
	}
	if cache.Version != 1 || cache.Provider != providerName || cache.AccountName != account {
		return false
	}
	if cache.AccessTokenValue() == "" || cache.RefreshTokenValue() == "" {
		return false
	}
	if !scopesSatisfy(cache.Scopes, requiredResourceScopes) {
		return false
	}
	if expectedProviderAccountID != "" {
		return cache.ProviderAccountID == expectedProviderAccountID
	}
	if expectedEmail != "" {
		return strings.EqualFold(strings.TrimSpace(cache.VerifiedEmail), strings.TrimSpace(expectedEmail))
	}
	return true
}
