package search

import (
	"context"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// SelectedMessage is the message that --query with --latest or --first picked.
type SelectedMessage struct {
	Message  mail.MessageSummary
	Owner    provider.ResolvedAccount
	Searched provider.AccountSelection
}

// ResolveSelected runs the search now and returns the first result with the
// account it belongs to. Search results come newest first, so --latest and
// --first both take that first row.
func ResolveSelected(ctx context.Context, sel provider.AccountSelection, run AccountSearchFunc) (SelectedMessage, error) {
	rows, err := ExecuteMessages(ctx, sel, run, ExecuteOptions{Limit: 1})
	if err != nil {
		return SelectedMessage{}, err
	}
	if len(rows) == 0 {
		return SelectedMessage{}, errs.NewWithDetails(errs.CodeMessageNotFound, "no message matches the search", map[string]any{
			"selector_kind": string(mail.SelectorQuery),
		})
	}
	row := rows[0]
	for _, acct := range sel.Accounts {
		if acct.Name == row.Account && acct.Provider == row.Provider {
			return SelectedMessage{Message: row, Owner: acct, Searched: sel}, nil
		}
	}
	return SelectedMessage{}, errs.New(errs.CodeMessageNotFound, "the message found is not in any of the searched accounts")
}
