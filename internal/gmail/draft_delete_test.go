package gmail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// A discarded draft goes through drafts.delete, which removes it for good,
// and not through a move to Trash.
func TestDeleteDraftUsesDraftsDelete(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/drafts/draft-1":
			writeJSON(w, map[string]any{"id": "draft-1"})
		case r.Method == http.MethodDelete && r.URL.Path == "/gmail/v1/users/me/drafts/draft-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref, err := testClient(server).DeleteDraft(context.Background(), mail.DeleteDraftRequest{DraftID: "draft-1"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "draft-1" || ref.WriteOutcomeFacts == nil || ref.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("ref = %#v", ref)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1] != "DELETE /gmail/v1/users/me/drafts/draft-1" {
		t.Fatalf("calls = %q", calls)
	}
}

// An id that names no draft is refused before anything is deleted.
func TestDeleteDraftOfAMissingDraftDeletesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := testClient(server).DeleteDraft(context.Background(), mail.DeleteDraftRequest{DraftID: "msg-1"})
	assertProductCode(t, err, errs.CodeDraftNotFound)
}
