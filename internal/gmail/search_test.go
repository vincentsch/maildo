package gmail

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

// bigMailbox answers message lists the way Gmail does for a search with
// thousands of matches: full pages of 500 ids, newest first, and always a
// next page. Message ids are message-0 (newest), message-1, ...
type bigMailbox struct {
	labels *gmailAPIFixture
	mu     sync.Mutex
	lists  []url.Values
	reads  int
	// old marks messages older than the search's after: date, so the
	// local date check drops them.
	old map[int]bool
}

func newBigMailbox(t *testing.T) (*bigMailbox, *httptest.Server) {
	m := &bigMailbox{labels: &gmailAPIFixture{t: t}}
	server := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(server.Close)
	return m, server
}

func (m *bigMailbox) handle(w http.ResponseWriter, r *http.Request) {
	const base = "/gmail/v1/users/me/messages"
	switch {
	case r.URL.Path == base:
		m.mu.Lock()
		m.lists = append(m.lists, r.URL.Query())
		m.mu.Unlock()
		start := 0
		if token := r.URL.Query().Get("pageToken"); token != "" {
			start, _ = strconv.Atoi(strings.TrimPrefix(token, "from-"))
		}
		rows := make([]map[string]string, 0, 500)
		for i := start; i < start+500; i++ {
			rows = append(rows, map[string]string{"id": fmt.Sprintf("message-%d", i), "threadId": fmt.Sprintf("thread-%d", i)})
		}
		writeJSON(w, map[string]any{"messages": rows, "nextPageToken": fmt.Sprintf("from-%d", start+500)})
	case strings.HasPrefix(r.URL.Path, base+"/message-"):
		m.mu.Lock()
		m.reads++
		m.mu.Unlock()
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, base+"/message-"))
		// Newer messages have larger dates: message-0 is the newest.
		date := int64(1_790_000_000_000) - int64(n)*60_000
		if m.old[n] {
			date = 1_600_000_000_000
		}
		writeJSON(w, gmailFixtureMessage(fmt.Sprintf("message-%d", n), fmt.Sprintf("thread-%d", n), "body", fmt.Sprintf("Newsletter %d", n), date))
	default:
		m.labels.handle(w, r)
	}
}

func TestSearchHonoursLimitWithoutListingEveryMatch(t *testing.T) {
	m, server := newBigMailbox(t)
	c := testClient(server)
	out, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Terms: []string{"newsletter"}}, Limit: 1})
	if err != nil {
		t.Fatalf("search --limit 1 over more than 1,000 matches: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].ID != "message-0" {
		t.Fatalf("rows = %v", messageSummaryIDs(out.Messages))
	}
	if len(m.lists) != 1 || m.reads != 1 {
		t.Fatalf("search made %d list requests and %d message reads, want 1 and 1", len(m.lists), m.reads)
	}
	if got := m.lists[0].Get("maxResults"); got != "1" {
		t.Fatalf("maxResults = %s, want 1", got)
	}
}

func TestSearchPagesDeepIntoTheResultsWithoutReadingSkippedMessages(t *testing.T) {
	m, server := newBigMailbox(t)
	c := testClient(server)
	out, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Terms: []string{"newsletter"}}, Limit: 2, Offset: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(messageSummaryIDs(out.Messages), ","); got != "message-1200,message-1201" {
		t.Fatalf("page = %s", got)
	}
	if m.reads != 2 || len(m.lists) != 3 {
		t.Fatalf("paging read %d messages over %d lists, want 2 reads", m.reads, len(m.lists))
	}
}

func TestSearchReplacesRowsTheDateCheckDrops(t *testing.T) {
	m, server := newBigMailbox(t)
	m.old = map[int]bool{1: true}
	c := testClient(server)
	after := mail.QueryDate("2026-01-01")
	out, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Terms: []string{"newsletter"}, After: &after}, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(messageSummaryIDs(out.Messages), ","); got != "message-0,message-2,message-3" {
		t.Fatalf("rows = %s; the old message must be dropped and replaced", got)
	}
}

func TestSearchScopeTermsAndSpamTrash(t *testing.T) {
	cases := []struct {
		name      string
		req       mail.SearchRequest
		wantQ     string
		spamTrash bool
		labels    []string
	}{
		{name: "default leaves Spam and Trash out", req: mail.SearchRequest{}, wantQ: `"invoice"`},
		{name: "skip Sent and Drafts", req: mail.SearchRequest{SkipRoles: []string{mail.WellKnownSent, mail.WellKnownDrafts, mail.WellKnownDeleted}}, wantQ: `"invoice" -in:sent -in:drafts`},
		{name: "only Trash and Junk", req: mail.SearchRequest{OnlyRoles: []string{mail.WellKnownDeleted, mail.WellKnownJunk}}, wantQ: `"invoice" {in:trash in:spam}`, spamTrash: true},
		{name: "a folder wins", req: mail.SearchRequest{Folder: &mail.Folder{ID: "TRASH"}, SkipRoles: []string{mail.WellKnownSent}}, wantQ: `"invoice"`, spamTrash: true, labels: []string{"TRASH"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, server := newBigMailbox(t)
			req := tc.req
			req.Query = mail.Query{Terms: []string{"invoice"}}
			req.Limit = 1
			if _, err := testClient(server).SearchMessages(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			got := m.lists[0]
			if got.Get("q") != tc.wantQ {
				t.Fatalf("q = %q, want %q", got.Get("q"), tc.wantQ)
			}
			if (got.Get("includeSpamTrash") == "true") != tc.spamTrash {
				t.Fatalf("includeSpamTrash = %q, want %v", got.Get("includeSpamTrash"), tc.spamTrash)
			}
			if strings.Join(got["labelIds"], ",") != strings.Join(tc.labels, ",") {
				t.Fatalf("labelIds = %v, want %v", got["labelIds"], tc.labels)
			}
		})
	}
}
