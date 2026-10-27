package outlook

import (
	"context"

	"github.com/vincentsch/maildo/internal/provider"
)

// Logout has nothing to do for Outlook. The command deletes the saved login
// itself.
func (p *Provider) Logout(context.Context, provider.StoredAuthContext) error {
	return nil
}
