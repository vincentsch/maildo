package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
)

// An edit that lands after maildo's last read must not go out. Gmail sends
// the raw message maildo checked, uploaded with the draft id, and not
// whatever the draft holds by then.
func TestSendDraftSendsTheCheckedMessageAfterAConcurrentEdit(t *testing.T) {
	checked := []byte("From: work@example.test\r\nTo: allowed@example.test\r\nSubject: Quote\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello there")
	edited := []byte("From: work@example.test\r\nTo: denied@example.test\r\nSubject: Quote\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello there")
	var mu sync.Mutex
	stored, uploaded := checked, []byte(nil)
	var meta map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
				"id": "msg-1", "threadId": "thread-1", "labelIds": []string{"DRAFT"},
				"raw": base64.RawURLEncoding.EncodeToString(stored),
			}})
			// Someone edits the draft right after maildo read it.
			stored = edited
		case "/upload/gmail/v1/users/me/drafts/send":
			meta, uploaded = readGmailMultipartUpload(t, r)
			writeJSON(w, map[string]any{"id": "sent-1", "threadId": "thread-1", "labelIds": []string{"SENT"}})
		case "/gmail/v1/users/me/messages/sent-1":
			writeJSON(w, map[string]any{"id": "sent-1", "threadId": "thread-1", "labelIds": []string{"SENT"}, "raw": base64.RawURLEncoding.EncodeToString(uploaded)})
		case "/gmail/v1/users/me/drafts/send":
			t.Errorf("draft sent by id only, without the checked message")
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var seen []string
	_, err := testClient(server).SendDraft(context.Background(), mail.SendDraftRequest{DraftID: "draft-1", Check: func(c mail.SendContent) error {
		for _, r := range c.To {
			seen = append(seen, r.Address)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != "allowed@example.test" {
		t.Fatalf("check saw %v", seen)
	}
	if !strings.Contains(string(uploaded), "To: allowed@example.test") || strings.Contains(string(uploaded), "denied@example.test") {
		t.Fatalf("uploaded message = %q", uploaded)
	}
	message, _ := meta["message"].(map[string]any)
	if meta["id"] != "draft-1" || message["threadId"] != "thread-1" {
		t.Fatalf("upload metadata = %#v", meta)
	}
}

// A check that refuses stops the send before anything goes to Gmail.
func TestSendDraftRefusedByCheckSendsNothing(t *testing.T) {
	raw := []byte("From: work@example.test\r\nTo: allowed@example.test\r\nSubject: Quote\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello there")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
				"id": "msg-1", "threadId": "thread-1", "labelIds": []string{"DRAFT"},
				"raw": base64.RawURLEncoding.EncodeToString(raw),
			}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	refused := errors.New("not approved")
	_, err := testClient(server).SendDraft(context.Background(), mail.SendDraftRequest{DraftID: "draft-1", Check: func(mail.SendContent) error { return refused }})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
}

// The preview's content comes from the same read as its version, with
// inline parts listed as attachments.
func TestValidateDraftSendListsInlineParts(t *testing.T) {
	encoded, err := encodeGmailMessage("work", mimecompose.Message{
		From:     mail.Recipient{Address: "work@example.test"},
		To:       []mail.Recipient{{Address: "client@example.test"}},
		Subject:  "Logo",
		TextBody: []byte("See the logo"),
		HTMLBody: []byte(`<p>See <img src="cid:logo@example.test"></p>`),
		Related: []mimecompose.BinaryPart{{
			Role: mimecompose.PartRoleRelated, ContentType: "image/png", Disposition: "inline",
			ContentID: "<logo@example.test>", Filename: "logo.png", Bytes: []byte("hello"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), encoded.Bytes...)
	encoded.Destroy()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
			"id": "msg-1", "threadId": "thread-1", "labelIds": []string{"DRAFT"},
			"raw": base64.RawURLEncoding.EncodeToString(raw),
		}})
	}))
	defer server.Close()
	draft, err := testClient(server).ValidateDraftSend(context.Background(), mail.SendDraftRequest{DraftID: "draft-1"})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Send == nil || len(draft.Send.Attachments) != 1 || !draft.Send.Attachments[0].Inline || draft.Send.Attachments[0].Size != 5 {
		t.Fatalf("send content = %#v", draft.Send)
	}
	if !strings.Contains(draft.Body.Content, "See") {
		t.Fatalf("body = %q", draft.Body.Content)
	}
}
