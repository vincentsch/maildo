package outlook

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
)

// Client talks to Microsoft Graph for one signed-in Outlook account.
type Client struct {
	accessToken     secretBytes
	tokenType       string
	graphBaseURL    string
	http            *http.Client
	account         string
	email           string
	searchResultCap int
	destroyed       atomic.Bool
}

func (c *Client) VerifiedEmail() string { return c.email }

func (c *Client) ProviderName() string { return "outlook" }

func (c *Client) AccountName() string { return c.account }

func (c *Client) Destroy() {
	if c == nil || c.destroyed.Swap(true) {
		return
	}
	c.accessToken.Destroy()
	c.tokenType = ""
	c.email = ""
}

func (c *Client) isDestroyed() bool {
	return c == nil || c.destroyed.Load()
}

func (c *Client) authorizationHeader() (string, error) {
	if c.isDestroyed() || c.accessToken.String() == "" {
		account := ""
		if c != nil {
			account = c.account
		}
		return "", errs.NewProviderClientClosed(account, errs.ProviderOutlook)
	}
	tokenType := c.tokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return tokenType + " " + c.accessToken.String(), nil
}

// Client returns a signed-in client. It renews the access token only when it
// is about to expire.
func (p *Provider) Client(ctx context.Context, ac provider.StoredAuthContext) (*Client, error) {
	cache, _, _, err := p.ensureFresh(ctx, ac, false)
	if err != nil {
		return nil, err
	}
	defer cache.Destroy()
	return &Client{
		accessToken:     secretFromString(cache.AccessTokenValue()),
		tokenType:       cache.TokenType,
		graphBaseURL:    p.opts.GraphBaseURL,
		http:            p.opts.HTTPClient,
		account:         ac.Account,
		email:           cache.VerifiedEmail,
		searchResultCap: p.opts.SearchResultCap,
	}, nil
}

// NewAccountClient returns a signed-in client for the account. It never
// opens a sign-in page; a login that no longer works needs auth repair.
func (p *Provider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	c, err := p.Client(ctx, ac)
	if err != nil {
		return nil, err
	}
	return c, nil
}
