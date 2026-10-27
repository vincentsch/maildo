package search

import (
	"context"
	"fmt"
	"sort"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// MessageSort is a sort order for merged results.
type MessageSort int

const (
	// SortTimeDesc orders messages by Time desc, account asc, id asc.
	SortTimeDesc MessageSort = iota
)

// ExecuteOptions sets how many rows ExecuteMessages returns and in which order.
type ExecuteOptions struct {
	Limit int
	Sort  MessageSort
}

// AccountSearchFunc searches one account. It also checks that the account
// can search at all.
type AccountSearchFunc func(ctx context.Context, acct provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error)

// ExecuteMessages searches each account, then merges, sorts and cuts the rows
// to the limit. If one account fails it returns no rows at all, so a result
// from several accounts never looks complete when it is not.
func ExecuteMessages(ctx context.Context, sel provider.AccountSelection, run AccountSearchFunc, opts ExecuteOptions) ([]mail.MessageSummary, error) {
	out := make([]mail.MessageSummary, 0)
	limit := opts.Limit
	if limit < 0 {
		limit = 0
	}
	// Call every account even with a limit of zero. Callers use that to check
	// that each account can search and is logged in, without getting rows.
	for _, acct := range sel.Accounts {
		list, err := run(ctx, acct, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Messages...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return mail.MessageSummaryLess(out[i], out[j])
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PageFunc fetches rows offset..offset+limit of one account's result.
type PageFunc func(ctx context.Context, acct provider.ResolvedAccount, offset, limit int) (mail.MessageSummaryList, error)

// Page is one page of rows merged across accounts. Total is set when every
// account reported its count. Warnings name messages that were left out and
// accounts that failed.
type Page struct {
	Messages   []mail.MessageSummary
	Total      int
	TotalKnown bool
	HasMore    bool
	Warnings   []string
}

// ExecutePage fetches one page for list and search output. With one account
// the provider pages itself. With several, each account returns its first
// offset+limit rows and the merged, sorted rows are cut to the page.
//
// Unlike ExecuteMessages, it carries on when one of several accounts fails.
// That account's rows are left out and a warning names it. Commands that
// change mail must not use it, because the failed account would quietly stay
// unchanged.
func ExecutePage(ctx context.Context, sel provider.AccountSelection, run PageFunc, offset, limit int) (Page, error) {
	if offset < 0 {
		offset = 0
	}
	single := len(sel.Accounts) == 1
	page := Page{Messages: []mail.MessageSummary{}, TotalKnown: true}
	var firstErr error
	failed := 0
	for _, acct := range sel.Accounts {
		accountOffset, accountLimit := offset, limit
		if !single {
			accountOffset, accountLimit = 0, offset+limit
		}
		list, err := run(ctx, acct, accountOffset, accountLimit)
		if err != nil {
			if single || ctx.Err() != nil {
				return Page{}, err
			}
			if firstErr == nil {
				firstErr = err
			}
			failed++
			page.Warnings = append(page.Warnings, fmt.Sprintf("account %s is left out: %v", acct.Name, err))
			page.TotalKnown = false
			continue
		}
		page.Messages = append(page.Messages, list.Messages...)
		page.Total += list.Total
		page.TotalKnown = page.TotalKnown && list.TotalKnown
		for _, w := range list.Warnings {
			if len(sel.Accounts) > 1 {
				w = acct.Name + ": " + w
			}
			page.Warnings = append(page.Warnings, w)
		}
	}
	if failed > 0 && failed == len(sel.Accounts) {
		return Page{}, firstErr
	}
	sort.SliceStable(page.Messages, func(i, j int) bool {
		return mail.MessageSummaryLess(page.Messages[i], page.Messages[j])
	})
	if !single {
		page.Messages = mail.DropRows(page.Messages, offset)
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		page.HasMore = true
	}
	if page.TotalKnown {
		page.HasMore = offset+len(page.Messages) < page.Total
	} else if len(page.Messages) == limit {
		page.HasMore = true
	}
	if !page.TotalKnown {
		page.Total = 0
	}
	return page, nil
}
