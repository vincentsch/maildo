package main

import (
	"strings"
	"testing"
	"time"
)

// After a change that moves mail to another folder, the result names the
// new ids and prints a command that moves the messages back. Running it
// does that.
func TestChangesThatMoveMailPrintAnUndoCommand(t *testing.T) {
	for _, op := range []string{"archive", "delete", "junk", "move"} {
		t.Run(op, func(t *testing.T) {
			h := newSendHarness(t)
			h.server.Append("INBOX", "From: news@shop.example\r\nSubject: Deal one\r\nMessage-ID: <one@shop.example>\r\n\r\nbody\r\n", time.Now())
			id := searchOne(t, h, "subject:one", "INBOX")
			args := []string{op, "--id", id}
			if op == "move" {
				args = append(args, "--to", "Archive")
			}
			var result mutationResult
			decodeOK(t, h.run(append(args, "--json")...), &result)
			if result.ResultMessageID == "" || result.ResultMessageID == id || len(result.UndoCommands) != 1 {
				t.Fatalf("result = new_id %q undo %q", result.ResultMessageID, result.UndoCommands)
			}
			undo := result.UndoCommands[0]
			if !strings.HasPrefix(undo, "maildo move --config ") || !strings.Contains(undo, "--account posteo --to inbox --id "+result.ResultMessageID) {
				t.Fatalf("undo command = %q", undo)
			}
			if res := runPrinted(t, h.run, undo); res.Exit != 0 {
				t.Fatalf("undo: %s%s", res.Stdout, res.Stderr)
			}
			searchOne(t, h, "subject:one", "INBOX")
		})
	}

	t.Run("restore", func(t *testing.T) {
		h := newSendHarness(t)
		h.server.Append("Trash", "From: news@shop.example\r\nSubject: Deal one\r\nMessage-ID: <one@shop.example>\r\n\r\nbody\r\n", time.Now())
		var result mutationResult
		decodeOK(t, h.run("restore", "--id", searchOne(t, h, "subject:one", "Trash"), "--json"), &result)
		if len(result.UndoCommands) != 1 || !strings.Contains(result.UndoCommands[0], " --to deleted --id "+result.ResultMessageID) {
			t.Fatalf("undo commands = %q", result.UndoCommands)
		}
		if res := runPrinted(t, h.run, result.UndoCommands[0]); res.Exit != 0 {
			t.Fatalf("undo: %s%s", res.Stdout, res.Stderr)
		}
		searchOne(t, h, "subject:one", "Trash")
	})

	t.Run("many messages", func(t *testing.T) {
		h := newSendHarness(t)
		for _, n := range []string{"one", "two", "three"} {
			h.server.Append("INBOX", "From: news@shop.example\r\nSubject: Deal "+n+"\r\nMessage-ID: <"+n+"@shop.example>\r\n\r\nbody\r\n", time.Now())
		}
		inboxCount := func() int {
			var inbox searchResult
			decodeOK(t, h.run("search", "from:news@shop.example", "--folder", "INBOX", "--json"), &inbox)
			return len(inbox.Messages)
		}
		var preview mutationPreview
		decodeOK(t, h.run("archive", "--query", "from:news@shop.example", "--all", "--json"), &preview)
		var result bulkMutationResult
		decodeOK(t, runPrinted(t, h.run, preview.ApplyCommand), &result)
		if result.Counts.Succeeded != 3 || len(result.UndoCommands) != 1 || !strings.HasSuffix(result.UndoCommands[0], " --apply --json") {
			t.Fatalf("archive result = %+v", result)
		}
		for _, row := range result.Targets {
			if row.ResultMessageID == "" || row.ResultMessageID == row.ID || row.SourceFolder == nil || row.SourceFolder.Path != "INBOX" {
				t.Fatalf("target = %+v", row)
			}
		}
		if res := runPrinted(t, h.run, result.UndoCommands[0]); res.Exit != 0 || inboxCount() != 3 {
			t.Fatalf("undo: %s%s", res.Stdout, res.Stderr)
		}

		// People get the same command in the text output.
		decodeOK(t, h.run("archive", "--query", "from:news@shop.example", "--all", "--json"), &preview)
		text := runPrinted(t, h.run, strings.Replace(preview.ApplyCommand, " --json", "", 1))
		_, undo, found := strings.Cut(text.Stdout, "To move them back to INBOX, run:\n  ")
		if text.Exit != 0 || !found || inboxCount() != 0 {
			t.Fatalf("archive:\n%s%s", text.Stdout, text.Stderr)
		}
		if res := runPrinted(t, h.run, strings.TrimSpace(undo)); res.Exit != 0 || inboxCount() != 3 {
			t.Fatalf("undo from the text output: %s%s", res.Stdout, res.Stderr)
		}
	})

	t.Run("one message in text output", func(t *testing.T) {
		h := newSendHarness(t)
		h.server.Append("INBOX", "From: news@shop.example\r\nSubject: Deal one\r\nMessage-ID: <one@shop.example>\r\n\r\nbody\r\n", time.Now())
		res := h.run("archive", "--id", searchOne(t, h, "subject:one", "INBOX"))
		if res.Exit != 0 || !strings.Contains(res.Stdout, "To move it back to INBOX, run:\n  maildo move ") {
			t.Fatalf("archive:\n%s%s", res.Stdout, res.Stderr)
		}
	})
}

// searchOne finds the one message that matches query in folder.
func searchOne(t *testing.T, h *mailServerHarness, query, folder string) string {
	t.Helper()
	var found searchResult
	if res := h.run("search", query, "--folder", folder, "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search %q in %s: %s%s", query, folder, res.Stdout, res.Stderr)
	}
	return found.Messages[0].ID
}
