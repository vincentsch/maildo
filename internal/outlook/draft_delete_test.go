package outlook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// A discarded draft gets Graph's DELETE, as after a send, and only after
// maildo saw that the id names a draft.
func TestDeleteDraftChecksTheDraftAndDeletesIt(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/d1":
			writeJSON(w, map[string]any{"id": "d1", "isDraft": true})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/m1":
			writeJSON(w, map[string]any{"id": "m1", "isDraft": false})
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testReplyClient(srv)
	ref, err := client.DeleteDraft(context.Background(), mail.DeleteDraftRequest{DraftID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "d1" || ref.WriteOutcomeFacts == nil || ref.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("ref = %#v", ref)
	}
	// A message that is not a draft is refused and stays.
	_, err = client.DeleteDraft(context.Background(), mail.DeleteDraftRequest{DraftID: "m1"})
	if e, ok := err.(*errs.Error); !ok || e.Code() != errs.CodeDraftNotFound {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "/me/messages/d1" {
		t.Fatalf("deleted = %q", deleted)
	}
}
