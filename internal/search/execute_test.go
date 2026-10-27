package search

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestExecuteMessagesMergeSortLimit(t *testing.T) {
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
	seenLimits := map[string]int{}
	rows, err := ExecuteMessages(context.Background(), sel, func(_ context.Context, acct provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error) {
		seenLimits[acct.Name] = limit
		switch acct.Name {
		case "work":
			return mail.MessageSummaryList{Messages: []mail.MessageSummary{
				{Account: "work", ID: "work-b", ReceivedAt: at("2026-06-28T10:00:00Z")},
				{Account: "work", ID: "work-sent", SentAt: at("2026-06-27T09:00:00Z")},
			}}, nil
		case "personal":
			return mail.MessageSummaryList{Messages: []mail.MessageSummary{
				{Account: "personal", ID: "personal-a", ReceivedAt: at("2026-06-28T10:00:00Z")},
				{Account: "personal", ID: "personal-z", ReceivedAt: at("2026-06-26T09:00:00Z")},
			}}, nil
		default:
			t.Fatalf("unexpected account %s", acct.Name)
		}
		return mail.MessageSummaryList{}, nil
	}, ExecuteOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seenLimits, map[string]int{"work": 3, "personal": 3}) {
		t.Fatalf("per-account limits = %#v", seenLimits)
	}
	if got, want := ids(rows), []string{"personal-a", "work-b", "work-sent"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %#v, want %#v", got, want)
	}
}

func TestExecuteMessagesEmptyAndFirstError(t *testing.T) {
	sel := provider.AccountSelection{Accounts: []provider.ResolvedAccount{{Name: "work"}, {Name: "personal"}}}
	empty, err := ExecuteMessages(context.Background(), sel, func(context.Context, provider.ResolvedAccount, int) (mail.MessageSummaryList, error) {
		return mail.MessageSummaryList{}, nil
	}, ExecuteOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v, want non-nil empty slice", empty)
	}

	boom := errors.New("personal failed")
	calls := 0
	_, err = ExecuteMessages(context.Background(), sel, func(_ context.Context, acct provider.ResolvedAccount, _ int) (mail.MessageSummaryList, error) {
		calls++
		if acct.Name == "personal" {
			return mail.MessageSummaryList{}, boom
		}
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{{ID: "partial"}}}, nil
	}, ExecuteOptions{Limit: 10})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestExecuteMessagesZeroLimitStillChecksAccounts(t *testing.T) {
	sel := provider.AccountSelection{Accounts: []provider.ResolvedAccount{{Name: "work"}, {Name: "personal"}}}
	calls := 0
	rows, err := ExecuteMessages(context.Background(), sel, func(_ context.Context, _ provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error) {
		calls++
		if limit != 0 {
			t.Fatalf("limit = %d, want 0", limit)
		}
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{{ID: "ignored"}}}, nil
	}, ExecuteOptions{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("rows = %#v, want non-nil empty slice", rows)
	}
}

func ids(rows []mail.MessageSummary) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}
