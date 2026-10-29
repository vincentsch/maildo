package outlook

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

// threadGraph answers a conversation read the way Graph did for a Microsoft
// 365 mailbox on 2026-10-29. A message that was sent over SMTP comes back
// with an Exchange address (/O=EXCHANGELABS/...) as its sender, unless the
// request also selects toRecipients. Folders are looked up by id.
type threadGraph struct {
	t             *testing.T
	folderLookups atomic.Int32
	folderMissing bool
}

func (g *threadGraph) serve(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.Path, "/v1.0")
	switch {
	case path == "/me/messages":
		data, err := os.ReadFile("testdata/graph-thread-rows.json")
		if err != nil {
			g.t.Fatal(err)
		}
		if strings.Contains(req.URL.Query().Get("$select"), "toRecipients") {
			var page map[string]any
			if err := json.Unmarshal(data, &page); err != nil {
				g.t.Fatal(err)
			}
			for _, row := range page["value"].([]any) {
				m := row.(map[string]any)
				m["from"] = map[string]any{"emailAddress": map[string]any{"name": "Dana Example", "address": "dana@example.com"}}
				m["toRecipients"] = []any{map[string]any{"emailAddress": map[string]any{"name": "sam@example.com", "address": "sam@example.com"}}}
			}
			data, _ = json.Marshal(page)
		}
		return testRoundTripResponse(req, http.StatusOK, string(data)), nil
	case path == "/me/mailFolders/AAMkAGsentitems":
		g.folderLookups.Add(1)
		if g.folderMissing {
			return testRoundTripResponse(req, http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound"}}`), nil
		}
		return testRoundTripResponse(req, http.StatusOK, `{"id":"AAMkAGsentitems","displayName":"Sent Items"}`), nil
	}
	return testRoundTripResponse(req, http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound"}}`), nil
}

func readTestThread(t *testing.T, g *threadGraph) mail.Thread {
	t.Helper()
	c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(g.serve)})
	thread, err := c.ReadThread(context.Background(), mail.ReadThreadRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorThreadID, ThreadID: "AAQkAGconv"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(thread.Messages))
	}
	return thread
}

func TestThreadShowsMailAddressesNotExchangeAddresses(t *testing.T) {
	thread := readTestThread(t, &threadGraph{t: t})
	for _, m := range thread.Messages {
		if m.From.Address != "dana@example.com" {
			t.Fatalf("sender of %s = %q, want dana@example.com", m.ID, m.From.Address)
		}
	}
}

func TestThreadNamesTheFoldersOnce(t *testing.T) {
	g := &threadGraph{t: t}
	thread := readTestThread(t, g)
	for _, m := range thread.Messages {
		if m.Folder.ID != "AAMkAGsentitems" || m.Folder.DisplayName != "Sent Items" {
			t.Fatalf("folder of %s = %+v, want Sent Items", m.ID, m.Folder)
		}
	}
	if n := g.folderLookups.Load(); n != 1 {
		t.Fatalf("looked up the folder %d times, want once", n)
	}
}

func TestThreadWithAFolderThatIsGoneKeepsTheID(t *testing.T) {
	thread := readTestThread(t, &threadGraph{t: t, folderMissing: true})
	if f := thread.Messages[0].Folder; f.ID != "AAMkAGsentitems" || f.DisplayName != "" {
		t.Fatalf("folder = %+v, want only the id", f)
	}
}

func TestReadAndDraftReadNameTheFolder(t *testing.T) {
	var lookups atomic.Int32
	serve := func(req *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.0")
		switch path {
		case "/me/messages/AAMkAGmsg":
			return testRoundTripResponse(req, http.StatusOK, `{"id":"AAMkAGmsg","subject":"Hello","parentFolderId":"AAMkAGdrafts","isDraft":true,"changeKey":"CQAAAA","body":{"contentType":"text","content":"Hi"},"from":{"emailAddress":{"name":"Dana Example","address":"dana@example.com"}}}`), nil
		case "/me/mailFolders/AAMkAGdrafts":
			lookups.Add(1)
			return testRoundTripResponse(req, http.StatusOK, `{"id":"AAMkAGdrafts","displayName":"Drafts"}`), nil
		}
		return testRoundTripResponse(req, http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound"}}`), nil
	}
	c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(serve)})
	msg, err := c.ReadMessage(context.Background(), mail.ReadMessageRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: "AAMkAGmsg"}, BodyMode: mail.BodyModeText})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Folder.DisplayName != "Drafts" {
		t.Fatalf("read folder = %+v, want Drafts", msg.Folder)
	}
	draft, err := c.ReadDraft(context.Background(), mail.ReadDraftRequest{DraftID: "AAMkAGmsg", BodyMode: mail.BodyModeText})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Folder.DisplayName != "Drafts" {
		t.Fatalf("draft folder = %+v, want Drafts", draft.Folder)
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("looked up the folder %d times, want once", n)
	}
}
