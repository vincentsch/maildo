package imap

import (
	"context"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

// On a server without SPECIAL-USE, a folder named Trash is the trash.
// Creating it, as doctor suggests, must report success, not an unknown
// outcome.
func TestCreateStandardFolderWithoutSpecialUse(t *testing.T) {
	s := startMemServer(t)
	client := s.client()
	defer client.Destroy()
	for _, name := range []string{"Trash", "Archive", "Projects"} {
		result, err := client.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if result.CreatedFolder == nil || result.CreatedFolder.Path != name || !result.ConfirmedAfterWrite {
			t.Fatalf("create %s = %#v", name, result)
		}
	}
}
