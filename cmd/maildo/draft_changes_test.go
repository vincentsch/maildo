package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/audit"
)

// Draft and draft attachment changes show readable previews and results,
// and each change that reaches the server goes to the action log.
func TestIMAPDraftChangesAreReadableAndLogged(t *testing.T) {
	h := newSendHarness(t)

	preview := h.run("draft", "create", "--to", "clara@example.org", "--subject", "Quote", "--body", "First version", "--dry-run")
	for _, want := range []string{
		"Would save this draft in posteo:",
		"  To:       clara@example.org\n",
		"  Subject:  Quote\n\n  First version\n",
		"Nothing was saved. To save it, run:\n  maildo draft create",
		"--account posteo",
	} {
		if preview.Exit != 0 || !strings.Contains(preview.Stdout, want) {
			t.Fatalf("create preview lacks %q:\n%s%s", want, preview.Stdout, preview.Stderr)
		}
	}

	var created draftWriteResult
	if res := h.run("draft", "create", "--to", "clara@example.org", "--subject", "Quote", "--body", "First version", "--json"); res.JSON(&created) != nil || !created.ActionLogStatus.Written {
		t.Fatalf("create: %s%s", res.Stdout, res.Stderr)
	}
	short := shortID("posteo", created.DraftID)

	update := h.run("draft", "update", "--id", created.DraftID, "--body", "Second version", "--dry-run")
	for _, want := range []string{
		"Would change the draft " + short + " in posteo:",
		"  Body:     Second version\n",
		"The rest of the draft would stay the same.\nNothing was changed.",
	} {
		if update.Exit != 0 || !strings.Contains(update.Stdout, want) {
			t.Fatalf("update preview lacks %q:\n%s%s", want, update.Stdout, update.Stderr)
		}
	}

	// An update that keeps the subject must not call the draft "(no
	// subject)". IMAP gives the new version a new id.
	updated := h.run("draft", "update", "--id", created.DraftID, "--body", "Second version")
	if updated.Exit != 0 || strings.Contains(updated.Stdout, "(no subject)") ||
		!strings.HasPrefix(updated.Stdout, "Updated the draft "+short+" in posteo.\nIts id is now ") {
		t.Fatalf("update result:\n%s%s", updated.Stdout, updated.Stderr)
	}
	newID := ""
	for _, entry := range actionLogEntries(t, h.root) {
		if entry.Change != nil && entry.Operation == "draft_update" {
			newID = entry.Change.NewID
		}
	}
	if newID == "" || newID == created.DraftID {
		t.Fatalf("update log has no new id")
	}

	file := filepath.Join(h.root, "notes.txt")
	if err := os.WriteFile(file, []byte("line items"), 0o600); err != nil {
		t.Fatal(err)
	}
	addPreview := h.run("attachments", "add", "--draft-id", newID, file, "--dry-run")
	if !strings.Contains(addPreview.Stdout, `Would add notes.txt to the draft "Quote" in posteo.`) {
		t.Fatalf("add preview:\n%s%s", addPreview.Stdout, addPreview.Stderr)
	}
	added := h.run("attachments", "add", "--draft-id", newID, file)
	if !strings.HasPrefix(added.Stdout, `Added notes.txt to the draft "Quote" in posteo. It now has 1 attachment.`+"\nDraft id: ") {
		t.Fatalf("add result:\n%s%s", added.Stdout, added.Stderr)
	}
	entries := actionLogEntries(t, h.root)
	last := entries[len(entries)-1]
	if last.Operation != "attachments_add" || last.Change == nil || last.Change.Kind != "attachment" || last.Change.Name != "notes.txt" || last.Change.NewID == "" {
		t.Fatalf("add log = %#v", last.Change)
	}
	removed := h.run("attachments", "remove", "--draft-id", last.Change.NewID, "--name", "notes.txt")
	if !strings.HasPrefix(removed.Stdout, `Removed notes.txt from the draft "Quote" in posteo. It now has 0 attachments.`) {
		t.Fatalf("remove result:\n%s%s", removed.Stdout, removed.Stderr)
	}

	var ops []string
	for _, entry := range actionLogEntries(t, h.root) {
		if entry.Change != nil {
			ops = append(ops, string(entry.Operation)+":"+entry.Change.Kind+":"+entry.Change.Outcome)
		}
	}
	want := "draft_create:draft:done draft_update:draft:done attachments_add:attachment:done attachments_remove:attachment:done"
	if strings.Join(ops, " ") != want {
		t.Fatalf("logged %v, want %s", ops, want)
	}
}

func actionLogEntries(t *testing.T, root string) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, audit.RelativePath))
	if err != nil {
		t.Fatalf("read action log: %v", err)
	}
	var out []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode action log line: %v", err)
		}
		out = append(out, entry)
	}
	return out
}

// A warning from a draft change reaches JSON and the terminal.
func TestDraftUpdateWarningsReachTheUser(t *testing.T) {
	h, fake := seededDraftHarness(t)
	fake.Drafts.UpdateWarning = "the old version of the draft could not be deleted"
	var result draftWriteResult
	if res := h.run("draft", "update", "--id", "seed-draft", "--subject", "New", "--json"); res.JSON(&result) != nil {
		t.Fatalf("update: %s%s", res.Stdout, res.Stderr)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != fake.Drafts.UpdateWarning {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	human := h.run("draft", "update", "--id", "seed-draft", "--subject", "New")
	if human.Exit != 0 || !strings.Contains(human.Stderr, "Warning: the old version of the draft could not be deleted") || !strings.HasPrefix(human.Stdout, `Updated the draft "New" in work.`) {
		t.Fatalf("human update:\n%s%s", human.Stdout, human.Stderr)
	}
}
