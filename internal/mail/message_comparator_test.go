package mail

import (
	"sort"
	"testing"
	"time"
)

func TestMessageSummaryLessTieBreakers(t *testing.T) {
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	rows := []MessageSummary{
		{Account: "work", Provider: "imap", ID: "b", ReceivedAt: &ts, SortGroup: "inbox", SortOrdinal: 10},
		{Account: "work", Provider: "gmail", ID: "z", ReceivedAt: &ts},
		{Account: "personal", Provider: "outlook", ID: "p", ReceivedAt: &ts},
		{Account: "work", Provider: "imap", ID: "a", ReceivedAt: &ts, SortGroup: "inbox", SortOrdinal: 10},
		{Account: "work", Provider: "imap", ID: "c", ReceivedAt: &ts, SortGroup: "inbox", SortOrdinal: 11},
	}
	sort.SliceStable(rows, func(i, j int) bool { return MessageSummaryLess(rows[i], rows[j]) })

	got := []string{}
	for _, row := range rows {
		got = append(got, row.Account+"/"+row.Provider+"/"+row.ID)
	}
	want := []string{
		"personal/outlook/p",
		"work/gmail/z",
		"work/imap/c",
		"work/imap/a",
		"work/imap/b",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}
