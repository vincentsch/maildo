package search

import (
	"context"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestResolveSelectedReturnsNewestOwnerFromOriginalSelection(t *testing.T) {
	at := func(raw string) *time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		return &parsed
	}
	sel := provider.AccountSelection{Scope: "read_many", Accounts: []provider.ResolvedAccount{
		{Name: "work", Provider: "fake"},
		{Name: "personal", Provider: "fake"},
	}}
	run := func(_ context.Context, acct provider.ResolvedAccount, _ int) (mail.MessageSummaryList, error) {
		switch acct.Name {
		case "work":
			return mail.MessageSummaryList{Messages: []mail.MessageSummary{{Account: acct.Name, Provider: acct.Provider, ID: "work-old", ReceivedAt: at("2026-06-20T00:00:00Z")}}}, nil
		case "personal":
			return mail.MessageSummaryList{Messages: []mail.MessageSummary{{Account: acct.Name, Provider: acct.Provider, ID: "personal-new", ReceivedAt: at("2026-06-28T00:00:00Z")}}}, nil
		default:
			return mail.MessageSummaryList{}, nil
		}
	}
	selected, err := ResolveSelected(context.Background(), sel, run)
	if err != nil {
		t.Fatalf("ResolveSelected: %v", err)
	}
	if selected.Message.ID != "personal-new" || selected.Owner.Name != "personal" || selected.Owner.Provider != "fake" || selected.Searched.Scope != "read_many" {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestResolveSelectedNoRows(t *testing.T) {
	sel := provider.AccountSelection{Scope: "single", Accounts: []provider.ResolvedAccount{{Name: "work", Provider: "fake"}}}
	_, err := ResolveSelected(context.Background(), sel, func(context.Context, provider.ResolvedAccount, int) (mail.MessageSummaryList, error) {
		return mail.MessageSummaryList{}, nil
	})
	if e, ok := err.(*errs.Error); !ok || e.Code() != errs.CodeMessageNotFound {
		t.Fatalf("err = %#v, want message_not_found", err)
	}
}

func TestResolveSelectedOwnerMismatch(t *testing.T) {
	sel := provider.AccountSelection{Scope: "single", Accounts: []provider.ResolvedAccount{{Name: "work", Provider: "fake"}}}
	_, err := ResolveSelected(context.Background(), sel, func(context.Context, provider.ResolvedAccount, int) (mail.MessageSummaryList, error) {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{{Account: "ghost", Provider: "fake", ID: "ghost-message"}}}, nil
	})
	if e, ok := err.(*errs.Error); !ok || e.Code() != errs.CodeMessageNotFound {
		t.Fatalf("err = %#v, want message_not_found", err)
	}
}
