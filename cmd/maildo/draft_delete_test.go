package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
)

// draft delete discards a draft for good, as a mail program does. The
// preview shows the draft and prints the command, the command needs
// --confirm, and the draft goes neither to Trash nor anywhere else.
func TestDraftDeleteDiscardsTheDraftForGood(t *testing.T) {
	h := newSendHarness(t)
	var saved draftWriteResult
	decodeOK(t, h.run("draft", "create", "--to", "jonas@example.org", "--subject", "Docs URLs", "--body", "The docs can stay.", "--json"), &saved)
	if len(h.server.Fetch("Drafts")) != 1 {
		t.Fatal("the draft was not saved")
	}

	var preview draftDeletePreview
	decodeOK(t, h.run("draft", "delete", saved.DraftID, "--dry-run", "--json"), &preview)
	if !preview.DryRun || preview.Operation != "draft_delete" || preview.Draft.Subject != "Docs URLs" || !strings.Contains(preview.Draft.Body.Content, "The docs can stay.") {
		t.Fatalf("preview = %+v", preview)
	}
	human := h.run("draft", "delete", saved.DraftID, "--dry-run")
	for _, want := range []string{"Would discard this draft in posteo:", "To:       jonas@example.org", "Subject:  Docs URLs", "The docs can stay.", "The draft would be gone for good. It does not go to Trash.", "To discard it, run:\n  maildo draft delete "} {
		if !strings.Contains(human.Stdout, want) {
			t.Fatalf("preview lacks %q:\n%s", want, human.Stdout)
		}
	}
	if len(h.server.Fetch("Drafts")) != 1 {
		t.Fatal("the preview changed the mailbox")
	}

	refused := h.run("draft", "delete", saved.DraftID, "--json")
	assertErrorCode(t, refused, errs.CodeConfirmationRequired)
	if len(h.server.Fetch("Drafts")) != 1 {
		t.Fatal("a delete without --confirm removed the draft")
	}

	var result draftWriteResult
	decodeOK(t, runThroughShell(t, h.run, preview.ApplyCommand), &result)
	if result.Operation != "draft_delete" || result.DraftID != saved.DraftID || result.Subject != "Docs URLs" || !result.ActionLogStatus.Written {
		t.Fatalf("result = %+v", result)
	}
	if n, trash := len(h.server.Fetch("Drafts")), len(h.server.Fetch("Trash")); n != 0 || trash != 0 {
		t.Fatalf("after the delete: %d drafts, %d in Trash", n, trash)
	}
	again := h.run("draft", "delete", saved.DraftID, "--confirm", saved.DraftID, "--json")
	assertErrorCodeExit(t, again, errs.CodeDraftNotFound, 5)
}

// Only drafts can be discarded this way. Other mail stays where it is.
func TestDraftDeleteRefusesMailThatIsNoDraft(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: maria@example.org\r\nSubject: Logo files\r\nMessage-ID: <logo@example.org>\r\n\r\nHere.\r\n", time.Now())
	id := searchOne(t, h, "subject:logo", "INBOX")
	assertErrorCodeExit(t, h.run("draft", "delete", id, "--dry-run", "--json"), errs.CodeDraftNotFound, 5)
	assertErrorCodeExit(t, h.run("draft", "delete", id, "--confirm", id, "--json"), errs.CodeDraftNotFound, 5)
	if len(h.server.Fetch("INBOX")) != 1 {
		t.Fatal("a message that is no draft was deleted")
	}
}

// Discarding a draft is draft work: drafts_only allows it, read_only does
// not.
func TestDraftDeleteFollowsThePolicy(t *testing.T) {
	h, fake := seededDraftHarness(t)
	decodeOK(t, h.run("config", "set", "policy.read_only", "true", "--account", "work"), nil)
	assertErrorCodeExit(t, h.run("draft", "delete", "seed-draft", "--confirm", "seed-draft", "--json"), errs.CodePolicyDenied, 4)
	if fake.Drafts.DeleteCalls.Load() != 0 {
		t.Fatal("read_only deleted a draft")
	}

	h, fake = seededDraftHarness(t)
	decodeOK(t, h.run("config", "set", "policy.drafts_only", "true", "--account", "work"), nil)
	var result draftWriteResult
	decodeOK(t, h.run("draft", "delete", "seed-draft", "--confirm", "seed-draft", "--json"), &result)
	if fake.Drafts.DeleteCalls.Load() != 1 || result.Operation != "draft_delete" {
		t.Fatalf("drafts_only: %d deletes, result %+v", fake.Drafts.DeleteCalls.Load(), result)
	}
	if _, ok := fake.Drafts.Snapshot()["seed-draft"]; ok {
		t.Fatal("the draft is still there")
	}
}
