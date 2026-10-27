package imap

import (
	"context"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

// maildo deletes IMAP folders only when they are empty and have no
// subfolders. The preflight counts the folder's messages and marks a full
// folder as not deletable.
func TestPrepareFolderDeleteCountsMessages(t *testing.T) {
	s := startMemServer(t, "Projects", "Empty")
	s.add("Projects", testMessage{subject: "Kept"}.raw(), day(1))
	client := s.client()
	for _, tt := range []struct {
		name      string
		count     int
		protected bool
	}{
		{"Projects", 1, true},
		{"Empty", 0, false},
	} {
		preflight, err := client.PrepareFolderDelete(context.Background(), mail.FolderDeletePreflightRequest{Target: folderFor(t, tt.name)})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !preflight.TotalItemCountKnown || preflight.TotalItemCount != tt.count || preflight.Protected != tt.protected {
			t.Fatalf("%s preflight = %#v", tt.name, preflight)
		}
	}
}
