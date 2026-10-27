package gmail

import (
	"context"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

// The preflight counts the label's own messages and its nested labels. The
// command refuses the delete when either is above zero.
func TestPrepareLabelDeleteCountsMessagesAndNestedLabels(t *testing.T) {
	api := newGmailAPIFixture(t)
	defer api.server.Close()
	client := &Client{
		account:     "work",
		email:       "work@example.test",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  api.server.URL + "/gmail/v1/users/me",
		http:        api.server.Client(),
	}
	preflight, err := client.PrepareFolderDelete(context.Background(), mail.FolderDeletePreflightRequest{Target: mail.Folder{
		Account: "work", Provider: "gmail", ID: "Label_parent", DisplayName: "Work", Path: "Work",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.TotalItemCountKnown || preflight.TotalItemCount != 3 || !preflight.ChildFolderKnown || preflight.ChildFolderCount != 1 {
		t.Fatalf("preflight = %#v", preflight)
	}
}
