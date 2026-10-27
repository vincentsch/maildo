package outlook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestReadComposeSourceMaterial(t *testing.T) {
	var sawHTMLPrefer bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/me/messages/src" {
			http.NotFound(w, r)
			return
		}
		sawHTMLPrefer = strings.Contains(r.Header.Get("Prefer"), `"html"`)
		writeJSON(w, graphReplySourceMessage("src", "2026-06-28T10:00:00Z", "2026-06-28T10:01:00Z"))
	}))
	defer srv.Close()

	got, err := testReplyClient(srv).ReadComposeSourceMaterial(context.Background(), mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: "src"})
	if err != nil {
		t.Fatalf("ReadComposeSourceMaterial: %v", err)
	}
	if !sawHTMLPrefer || got.SourceID != "src" || got.InternetMessageID != "<src@example.test>" ||
		got.From.Address != "sender@example.test" || strings.TrimSpace(string(got.Text)) == "" || strings.TrimSpace(string(got.HTML)) == "" {
		t.Fatalf("source = %#v prefer=%t", got, sawHTMLPrefer)
	}
	if got.SentAt == nil || got.SentAt.Format(time.RFC3339) != "2026-06-28T10:00:00Z" {
		t.Fatalf("sent at = %#v", got.SentAt)
	}
}

func TestCreateReplyDraftHighLevel(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/me/messages/src/createReply":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": "draft-src"})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/draft-src":
			writeJSON(w, graphDraftMessage("draft-src"))
		case r.Method == http.MethodPatch && r.URL.Path == "/me/messages/draft-src":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/draft-src/attachments":
			writeJSON(w, map[string]any{"value": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	source := replySourceSnapshot(t)
	defer source.Destroy()
	got, err := testReplyClient(srv).CreateReplyDraft(context.Background(), provider.ReplyDraftRequest{
		Mode:   mail.ReplyModeReply,
		Quote:  mail.QuoteModeFull,
		Action: mail.ComposeExecutionDraft,
		Body:   mail.OutboundBody{Format: mail.BodyFormatHTML, ContentType: "html", HTML: "<p>Hello</p>", Text: "Hello", Present: true, Bytes: 5},
		Source: source,
	})
	if err != nil {
		t.Fatalf("CreateReplyDraft: %v", err)
	}
	if got.SourceID != "src" || got.DraftID != "draft-src" || got.Subject != "RE: Source" || len(got.To) != 1 {
		t.Fatalf("result = %#v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"POST /me/messages/src/createReply",
		"GET /me/messages/draft-src",
		"PATCH /me/messages/draft-src",
		"GET /me/messages/draft-src",
		"GET /me/messages/draft-src/attachments",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestSentOrReceivedFallback(t *testing.T) {
	got := sentOrReceived(graphFullMessage{ReceivedDateTime: "2026-06-28T10:01:00Z"})
	if got == nil || got.Format(time.RFC3339) != "2026-06-28T10:01:00Z" {
		t.Fatalf("received fallback = %#v", got)
	}
	got = sentOrReceived(graphFullMessage{SentDateTime: "2026-06-28T10:00:00Z", ReceivedDateTime: "2026-06-28T10:01:00Z"})
	if got == nil || got.Format(time.RFC3339) != "2026-06-28T10:00:00Z" {
		t.Fatalf("sent preference = %#v", got)
	}
}

func replySourceSnapshot(t *testing.T) *provider.ComposeSourceSnapshot {
	t.Helper()
	sent := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	source, err := provider.NewComposeSourceSnapshotForTest("work", "outlook", provider.ComposeSourceMaterial{
		SourceID:          "src",
		InternetMessageID: "<src@example.test>",
		ThreadID:          "thread-src",
		From:              mail.Recipient{Name: "Sender", Address: "sender@example.test"},
		Subject:           "Source",
		SentAt:            &sent,
		Text:              []byte("Source body"),
		HTML:              []byte("<p>Source body</p>"),
	})
	if err != nil {
		t.Fatalf("source snapshot: %v", err)
	}
	return source
}

func graphReplySourceMessage(id, sent, received string) map[string]any {
	return map[string]any{
		"id":                id,
		"internetMessageId": "<" + id + "@example.test>",
		"conversationId":    "thread-" + id,
		"parentFolderId":    "inbox-id",
		"subject":           "Source",
		"from": map[string]any{"emailAddress": map[string]any{
			"name": "Sender", "address": "sender@example.test",
		}},
		"toRecipients":     []map[string]any{},
		"ccRecipients":     []map[string]any{},
		"bccRecipients":    []map[string]any{},
		"receivedDateTime": received,
		"sentDateTime":     sent,
		"isRead":           true,
		"importance":       "normal",
		"hasAttachments":   false,
		"body":             map[string]any{"contentType": "html", "content": "<p>Text body for " + id + "</p>"},
	}
}

func graphDraftMessage(id string) map[string]any {
	return map[string]any{
		"id":                id,
		"internetMessageId": "<" + id + "@example.test>",
		"conversationId":    "thread-src",
		"parentFolderId":    "drafts-id",
		"subject":           "RE: Source",
		"from": map[string]any{"emailAddress": map[string]any{
			"name": "Sender", "address": "sender@example.test",
		}},
		"toRecipients": []map[string]any{{
			"emailAddress": map[string]any{"name": "Sender", "address": "sender@example.test"},
		}},
		"ccRecipients":           []map[string]any{},
		"bccRecipients":          []map[string]any{},
		"receivedDateTime":       "2026-06-28T10:01:00Z",
		"sentDateTime":           "2026-06-28T10:00:00Z",
		"createdDateTime":        "2026-06-28T10:00:00Z",
		"lastModifiedDateTime":   "2026-06-28T10:00:00Z",
		"isRead":                 true,
		"isDraft":                true,
		"importance":             "normal",
		"hasAttachments":         false,
		"internetMessageHeaders": []map[string]any{},
	}
}

func testReplyClient(srv *httptest.Server) *Client {
	return &Client{
		accessToken:  secretFromString("token"),
		tokenType:    "Bearer",
		graphBaseURL: srv.URL,
		http:         srv.Client(),
		account:      "work",
	}
}
