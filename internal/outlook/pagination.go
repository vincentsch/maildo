package outlook

import (
	"context"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/rungrad"
)

// graphPageGuard stops a walk through Graph's pages from running forever,
// even when the pages are empty. It notices a page address it has seen
// before, also when the query parameters come in another order.
type graphPageGuard struct {
	seen map[string]struct{}
}

func (p *graphPageGuard) visit(ctx context.Context, c *Client, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, valid := c.rulePageKey(target)
	if !valid {
		return rungrad.NewError(rungrad.ExitAPI, "Outlook pagination target is invalid")
	}
	if _, duplicate := p.seen[key]; duplicate {
		return errs.NewWithDetails(errs.CodeProviderDataError, "Outlook sent the same page of results twice", map[string]any{"account": c.account, "provider": "outlook"})
	}
	if len(p.seen) >= limits.OutlookMaxPages {
		return errs.NewReadLimitExceeded(c.account, errs.ProviderOutlook, errs.ReadLimitPages, uint64(limits.OutlookMaxPages+1))
	}
	if p.seen == nil {
		p.seen = make(map[string]struct{})
	}
	p.seen[key] = struct{}{}
	return nil
}
