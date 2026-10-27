package imap

import (
	"context"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/mail"
)

// An IMAP draft changes by storing a new version and deleting the old one.
// Without UIDPLUS the old version can only be flagged \Deleted, so it may
// stay visible and sendable in other mail programs. The caller hears so.
func TestDraftChangesWarnWhenTheOldVersionStays(t *testing.T) {
	s := startMemServerWithCaps(t, goimap.CapSet{
		goimap.CapIMAP4rev1: {}, goimap.CapMove: {}, goimap.CapESearch: {}, goimap.CapListStatus: {}, goimap.CapNamespace: {},
	}, "Drafts")
	client := s.client()
	ctx := context.Background()
	ref, err := client.CreateDraft(ctx, mail.CreateDraftRequest{Message: mail.OutboundMessage{
		To:      []mail.Recipient{{Address: "clara@example.test"}},
		Subject: "Quote",
		Body:    mail.OutboundBody{Format: mail.BodyFormatText, ContentType: "text/plain", Text: "Hello", Bytes: 5, Present: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ref.Warnings) != 0 {
		t.Fatalf("create warnings = %v", ref.Warnings)
	}

	updated, err := client.UpdateDraft(ctx, mail.UpdateDraftRequest{DraftID: ref.ID, Patch: mail.DraftPatch{Subject: mail.SubjectPatch{Set: true, Value: "Quote v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID == ref.ID || len(updated.Warnings) != 1 || !strings.Contains(updated.Warnings[0], "UIDPLUS") {
		t.Fatalf("update = %#v", updated)
	}

	added, err := client.AddDraftAttachments(ctx, mail.AddDraftAttachmentsRequest{DraftID: updated.ID, Uploads: []mail.AttachmentUpload{
		{Name: "quote.pdf", ContentType: "application/pdf", Size: 3, Bytes: []byte("PDF")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if added.DraftID == updated.ID || len(added.Warnings) != 1 {
		t.Fatalf("add = %#v", added)
	}

	removed, err := client.RemoveDraftAttachment(ctx, mail.RemoveDraftAttachmentRequest{DraftID: added.DraftID, AttachmentID: added.Added[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if removed.DraftID == added.DraftID || len(removed.Warnings) != 1 {
		t.Fatalf("remove = %#v", removed)
	}
}

// With UIDPLUS the old version is gone and there is nothing to warn about.
func TestDraftUpdateWithUIDPlusHasNoWarning(t *testing.T) {
	s := startMemServer(t, "Drafts")
	client := s.client()
	ctx := context.Background()
	ref, err := client.CreateDraft(ctx, mail.CreateDraftRequest{Message: mail.OutboundMessage{
		To:      []mail.Recipient{{Address: "clara@example.test"}},
		Subject: "Quote",
		Body:    mail.OutboundBody{Format: mail.BodyFormatText, ContentType: "text/plain", Text: "Hello", Bytes: 5, Present: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.UpdateDraft(ctx, mail.UpdateDraftRequest{DraftID: ref.ID, Patch: mail.DraftPatch{Subject: mail.SubjectPatch{Set: true, Value: "Quote v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Warnings) != 0 {
		t.Fatalf("warnings = %v", updated.Warnings)
	}
}
