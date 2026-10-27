package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// scopeGraph is a small Graph mailbox: one standard folder per role, a user
// folder "Clients", and a folder that has been deleted ("gone-id").
type scopeGraph struct {
	mu    sync.Mutex
	paths []string
	// mailbox is what /me/messages returns, in pages of pageSize.
	mailbox  []map[string]any
	pageSize int
	// noNextLink makes /me/messages answer one page without a next link,
	// as Graph does when a $search hits its cap.
	noNextLink bool
}

const scopeBase = "https://graph.example.test/v1.0"

var scopeWellKnown = map[string][2]string{
	"inbox":        {"inbox-id", "Inbox"},
	"drafts":       {"drafts-id", "Drafts"},
	"sentitems":    {"sent-id", "Sent Items"},
	"deleteditems": {"deleted-id", "Deleted Items"},
	"archive":      {"archive-id", "Archive"},
	"junkemail":    {"junk-id", "Junk Email"},
}

func scopeRow(id, folder string) map[string]any {
	return map[string]any{"id": id, "parentFolderId": folder, "subject": "invoice " + id, "receivedDateTime": "2026-10-01T10:00:00Z", "isRead": false}
}

func (g *scopeGraph) client() *Client {
	c := dispatchTestClient(scopeBase, &http.Client{Transport: roundTripFunc(g.serve)})
	c.searchResultCap = 1000
	return c
}

func (g *scopeGraph) serve(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.Path, "/v1.0")
	g.mu.Lock()
	g.paths = append(g.paths, path)
	g.mu.Unlock()
	write := func(v any) (*http.Response, error) {
		data, _ := json.Marshal(v)
		return testRoundTripResponse(req, http.StatusOK, string(data)), nil
	}
	switch {
	case path == "/me/messages":
		size := g.pageSize
		if size == 0 {
			size = len(g.mailbox)
		}
		start := 0
		if p := req.URL.Query().Get("skip"); p != "" {
			start = len(p) // one character per page already served
		}
		start *= size
		end := min(start+size, len(g.mailbox))
		body := map[string]any{"value": g.mailbox[start:end]}
		if end < len(g.mailbox) && !g.noNextLink {
			body["@odata.nextLink"] = scopeBase + "/me/messages?skip=" + strings.Repeat("x", end/size)
		}
		return write(body)
	case strings.HasSuffix(path, "/messages"):
		folder := strings.TrimSuffix(strings.TrimPrefix(path, "/me/mailFolders/"), "/messages")
		var rows []map[string]any
		for _, row := range g.mailbox {
			if row["parentFolderId"] == folder {
				rows = append(rows, row)
			}
		}
		return write(map[string]any{"value": rows})
	case strings.HasPrefix(path, "/me/mailFolders/"):
		name := strings.TrimPrefix(path, "/me/mailFolders/")
		if f, ok := scopeWellKnown[name]; ok {
			return write(map[string]any{"id": f[0], "displayName": f[1]})
		}
		if name == "clients-id" {
			return write(map[string]any{"id": "clients-id", "displayName": "Clients"})
		}
	}
	return testRoundTripResponse(req, http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound"}}`), nil
}

func (g *scopeGraph) requested(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		if p == path {
			return true
		}
	}
	return false
}

func everyFolderMailbox() []map[string]any {
	var rows []map[string]any
	for _, folder := range []string{"inbox-id", "sent-id", "drafts-id", "deleted-id", "junk-id", "archive-id", "clients-id", "gone-id"} {
		rows = append(rows, scopeRow("in-"+folder, folder))
	}
	return rows
}

func rowFolders(rows []mail.MessageSummary) string {
	var out []string
	for _, row := range rows {
		out = append(out, row.Folder.ID)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestOutlookSearchLeavesOutTrashJunkAndSkippedRoles(t *testing.T) {
	g := &scopeGraph{mailbox: everyFolderMailbox()}
	c := g.client()
	invoice := mail.Query{Terms: []string{"invoice"}}

	out, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: invoice, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowFolders(out.Messages); got != "archive-id,clients-id,drafts-id,gone-id,inbox-id,sent-id" {
		t.Fatalf("default search returned rows from %s; Deleted Items and Junk Email must be left out", got)
	}
	names := map[string]mail.FolderSummary{}
	for _, row := range out.Messages {
		names[row.Folder.ID] = row.Folder
	}
	if names["inbox-id"].DisplayName != "Inbox" || names["inbox-id"].WellKnownName != mail.WellKnownInbox {
		t.Fatalf("inbox row folder = %+v", names["inbox-id"])
	}
	if names["clients-id"].DisplayName != "Clients" || names["gone-id"].DisplayName != "" {
		t.Fatalf("user folder names = %+v / %+v", names["clients-id"], names["gone-id"])
	}

	out, err = c.SearchMessages(context.Background(), mail.SearchRequest{Query: invoice, Limit: 50, SkipRoles: []string{mail.WellKnownSent, mail.WellKnownDrafts}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowFolders(out.Messages); got != "archive-id,clients-id,gone-id,inbox-id" {
		t.Fatalf("search without Sent and Drafts returned rows from %s", got)
	}
}

func TestOutlookSearchOnlyInTrashAndJunk(t *testing.T) {
	g := &scopeGraph{mailbox: everyFolderMailbox()}
	out, err := g.client().SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Terms: []string{"invoice"}}, Limit: 50, OnlyRoles: []string{mail.WellKnownDeleted, mail.WellKnownJunk}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowFolders(out.Messages); got != "deleted-id,junk-id" {
		t.Fatalf("search in Trash and Junk returned rows from %s", got)
	}
	if !g.requested("/me/mailFolders/deleted-id/messages") || !g.requested("/me/mailFolders/junk-id/messages") || g.requested("/me/messages") {
		t.Fatalf("requests = %v", g.paths)
	}
	for _, row := range out.Messages {
		if row.Folder.WellKnownName == "" || row.Folder.DisplayName == "" {
			t.Fatalf("row folder = %+v", row.Folder)
		}
	}
}

func TestOutlookFilterSearchKeepsPagingPastLeftOutRows(t *testing.T) {
	g := &scopeGraph{pageSize: 2, mailbox: []map[string]any{
		scopeRow("t1", "deleted-id"), scopeRow("t2", "junk-id"),
		scopeRow("a", "inbox-id"), scopeRow("t3", "deleted-id"),
		scopeRow("b", "inbox-id"), scopeRow("c", "inbox-id"),
	}}
	unread := true
	out, err := g.client().SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Unread: &unread}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join([]string{out.Messages[0].ID, out.Messages[1].ID}, ","); len(out.Messages) != 2 || got != "a,b" {
		t.Fatalf("filter search = %v", out.Messages)
	}
}

func TestOutlookSearchCap(t *testing.T) {
	g := &scopeGraph{mailbox: everyFolderMailbox()[:3], noNextLink: true}
	c := g.client()
	c.searchResultCap = 3
	query := mail.Query{Terms: []string{"invoice"}}

	shown, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: query, Limit: 50})
	if err != nil {
		t.Fatalf("a search for reading must answer at the cap: %v", err)
	}
	if len(shown.Messages) != 3 || len(shown.Warnings) != 1 || !strings.Contains(shown.Warnings[0], "at most 3 search results") {
		t.Fatalf("capped search = %d rows, warnings %v", len(shown.Messages), shown.Warnings)
	}

	_, err = c.SearchMessages(context.Background(), mail.SearchRequest{Query: query, Limit: 50, Complete: true})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeReadLimitExceeded || !strings.Contains(product.Message(), "at most 3 search results") {
		t.Fatalf("complete search at the cap = %v", err)
	}
}
