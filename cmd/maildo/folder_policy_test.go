package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

// In this beta maildo deletes only empty folders without subfolders. This
// is the reviewer's case: "Positive Counts" holds 9 messages, and of its two
// subfolders one has no count at all and the other holds 1 message. Under
// policy.max_bulk 10 the old subtree count came out as 10 and the delete
// went through. Now the folder is refused with and without a limit, and
// maildo never asks for its subfolders.
func TestOutlookFolderDeleteRefusesFolderWithMessages(t *testing.T) {
	for _, limit := range []string{"", "10"} {
		t.Run("max_bulk="+limit, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			if limit != "" {
				decodeOK(t, h.runNoBrowser("config", "set", "policy.max_bulk", limit, "--account", "work"), nil)
			}
			for _, args := range [][]string{
				{"folders", "delete", "positive-counts-id", "--confirm", "positive-counts-id", "--json"},
				{"folders", "delete", "positive-counts-id", "--dry-run", "--json"},
			} {
				res := h.runNoBrowser(args...)
				assertErrorCodeExit(t, res, errs.CodeProtectedFolder, 1)
				var got map[string]any
				if err := json.Unmarshal([]byte(res.Stderr), &got); err != nil {
					t.Fatal(err)
				}
				if got["reason"] != "not_empty" || got["total_item_count"] != float64(9) {
					t.Fatalf("error details = %#v", got)
				}
				if !strings.Contains(got["error"].(string), "holds 9 messages") || !strings.Contains(got["hint"].(string), "move or delete its messages first") {
					t.Fatalf("error = %#v", got)
				}
			}
			for _, record := range h.server.mailFolderRequestRecordsSnapshot() {
				if record.Method == http.MethodDelete {
					t.Fatalf("delete of a full folder sent %s %s", record.Method, record.Path)
				}
			}
		})
	}
}

// A folder that holds no messages itself but has subfolders, even empty
// ones, is refused too.
func TestOutlookFolderDeleteRefusesFolderWithSubfolders(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	for _, args := range [][]string{
		{"folders", "delete", "child-only-id", "--confirm", "child-only-id", "--json"},
		{"folders", "delete", "Clients", "--dry-run", "--json"},
	} {
		res := h.runNoBrowser(args...)
		assertErrorCodeExit(t, res, errs.CodeProtectedFolder, 1)
		if !strings.Contains(res.Stderr, `"reason": "has_children"`) {
			t.Fatalf("%v stderr = %s", args, res.Stderr)
		}
	}
	human := h.runNoBrowser("folders", "delete", "child-only-id", "--confirm", "child-only-id")
	if human.Exit != 1 || !strings.Contains(human.Stderr, `"Child Only" has 3 subfolders, and maildo deletes only empty folders without subfolders`) || !strings.Contains(human.Stderr, "Delete its subfolders first") {
		t.Fatalf("exit=%d stderr=%s", human.Exit, human.Stderr)
	}
	if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
		t.Fatal("a folder with subfolders was deleted")
	}
}

// When Outlook leaves out the counts, maildo cannot see that the folder is
// empty, so it does not delete it. A count that is known and above zero
// gives the more useful reason.
func TestOutlookFolderDeleteRefusesUnknownCounts(t *testing.T) {
	for _, id := range []string{"count-free-id"} {
		t.Run(id, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			for _, args := range [][]string{
				{"folders", "delete", id, "--confirm", id, "--json"},
				{"folders", "delete", id, "--dry-run", "--json"},
			} {
				res := h.runNoBrowser(args...)
				assertErrorCodeExit(t, res, errs.CodeProtectedFolder, 1)
				if !strings.Contains(res.Stderr, `"reason": "counts_unknown"`) || !strings.Contains(res.Stderr, "could not count the messages and subfolders") {
					t.Fatalf("stderr = %s", res.Stderr)
				}
			}
			if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
				t.Fatal("a folder with unknown counts was deleted")
			}
		})
	}
}

// An empty folder without subfolders is deleted, also under a tight
// policy.max_bulk, since the delete reaches no messages.
func TestOutlookFolderDeleteDeletesEmptyFolder(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	decodeOK(t, h.runNoBrowser("config", "set", "policy.max_bulk", "1", "--account", "work"), nil)
	preview := h.runNoBrowser("folders", "delete", "zero-counts-id", "--dry-run")
	decodeOK(t, preview, nil)
	if !strings.Contains(preview.Stdout, "It is empty and has no subfolders.") {
		t.Fatalf("preview = %s", preview.Stdout)
	}
	decodeOK(t, h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--json"), nil)
	if !mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
		t.Fatal("the empty folder was not deleted")
	}
}

func TestFakeFolderDeleteOnlyEmptyFolders(t *testing.T) {
	folders := mail.FolderList{Folders: []mail.Folder{
		{ID: "projects", DisplayName: "Projects", Path: "Projects", TotalCount: 2, ChildCount: 1},
		{ID: "old", DisplayName: "Old", Path: "Projects/Old", ParentID: "projects", ParentPath: "Projects", TotalCount: 4},
		{ID: "shell", DisplayName: "Shell", Path: "Shell", ChildCount: 1},
		{ID: "inner", DisplayName: "Inner", Path: "Shell/Inner", ParentID: "shell", ParentPath: "Shell"},
		{ID: "empty", DisplayName: "Empty", Path: "Empty"},
	}}
	fake := testkit.NewFakeFolderMutationProvider("fake", folders)
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	decodeOK(t, h.run("config", "set", "policy.max_bulk", "100", "--account", "work"), nil)
	for id, reason := range map[string]string{"projects": "not_empty", "old": "not_empty", "shell": "has_children"} {
		res := h.run("folders", "delete", id, "--confirm", id, "--json")
		assertErrorCodeExit(t, res, errs.CodeProtectedFolder, 1)
		if body := errorJSON(t, res); body["reason"] != reason {
			t.Fatalf("%s reason = %v", id, body["reason"])
		}
	}
	if fake.FolderDeleteCalls.Load() != 0 {
		t.Fatal("a folder with messages or subfolders was deleted")
	}
	decodeOK(t, h.run("folders", "delete", "inner", "--confirm", "inner", "--json"), nil)
	decodeOK(t, h.run("folders", "delete", "empty", "--confirm", "empty", "--json"), nil)
	if fake.FolderDeleteCalls.Load() != 2 {
		t.Fatalf("delete calls = %d, want 2", fake.FolderDeleteCalls.Load())
	}
}

// Gmail labels follow the same rule. Deleting a label would leave its
// messages in the mailbox, but maildo still refuses a label that is on
// messages or has nested labels.
func TestGmailLabelDeleteRefusedWhenUsed(t *testing.T) {
	acct := provider.ResolvedAccount{Name: "personal", Provider: "gmail"}
	label := mail.Folder{ID: "Label_1", DisplayName: "Work", Path: "Work"}
	used := mail.FolderMutationPreflight{CountsSource: mail.FolderCountsSourceRefreshed, TotalItemCount: 3, TotalItemCountKnown: true, ChildFolderCount: 1, ChildFolderKnown: true}
	assertProductCodeReason(t, requireEmptyFolder(acct, label, used), errs.CodeProtectedFolder, "not_empty")
	used.TotalItemCount = 0
	assertProductCodeReason(t, requireEmptyFolder(acct, label, used), errs.CodeProtectedFolder, "has_children")
}

func TestFolderPreviewsAndResultsAreReadable(t *testing.T) {
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")

	create := h.run("folders", "create", "Projects", "--parent", "Clients", "--dry-run")
	decodeOK(t, create, nil)
	for _, want := range []string{
		`Would create the folder "Projects" under "Clients" in work.`,
		"Nothing was changed. To create it, run:",
		"maildo folders create Projects",
		"--parent Clients --account work",
	} {
		if !strings.Contains(create.Stdout, want) {
			t.Fatalf("create preview lacks %q:\n%s", want, create.Stdout)
		}
	}

	del := h.run("folders", "delete", "Spare", "--dry-run")
	decodeOK(t, del, nil)
	for _, want := range []string{
		`Would delete the folder "Spare" in work.`,
		"It is empty and has no subfolders.",
		"--account work --confirm Spare",
	} {
		if !strings.Contains(del.Stdout, want) {
			t.Fatalf("delete preview lacks %q:\n%s", want, del.Stdout)
		}
	}
	if strings.Contains(del.Stdout, " = ") || strings.Contains(del.Stdout, "DRY RUN") {
		t.Fatalf("delete preview still lists fields:\n%s", del.Stdout)
	}

	view := h.run("folders", "view", "Clients/ACME")
	decodeOK(t, view, nil)
	if !strings.Contains(view.Stdout, "Clients/ACME (work)") || strings.Contains(view.Stdout, "Matched by") {
		t.Fatalf("folder view:\n%s", view.Stdout)
	}

	created := h.run("folders", "create", "Projects", "--parent", "Clients")
	if created.Stdout != "Created the folder \"Clients/Projects\" under \"Clients\" in work.\n" {
		t.Fatalf("create result = %q", created.Stdout)
	}
}

// Folder changes go to the action log. When the log cannot be written,
// the change still counts and people get a warning.
func TestFolderChangesAreLogged(t *testing.T) {
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
	decodeOK(t, h.run("folders", "create", "Projects", "--json"), nil)
	decodeOK(t, h.run("folders", "rename", "Clients", "Accounts", "--json"), nil)
	decodeOK(t, h.run("folders", "delete", "reports-1", "--confirm", "reports-1", "--json"), nil)
	entries := readActionLog(t, h)
	if len(entries) != 3 {
		t.Fatalf("entries = %#v", entries)
	}
	for i, want := range []audit.Change{
		{Kind: "folder", ID: "fake-folder-projects", Name: "Projects"},
		{Kind: "folder", ID: "clients-id", Name: "Clients", NewName: "Accounts"},
		{Kind: "folder", ID: "reports-1", Name: "Reports"},
	} {
		want.Account, want.Provider, want.Outcome = "work", "fake", "done"
		got := entries[i]
		if got.Change == nil || *got.Change != want || got.Sent != nil || len(got.Targets) != 0 {
			t.Fatalf("entry %d = %#v, change %#v", i, got, got.Change)
		}
	}
	if entries[0].Operation != "folder_create" || entries[1].Operation != "folder_rename" || entries[2].Operation != "folder_delete" {
		t.Fatalf("operations = %s %s %s", entries[0].Operation, entries[1].Operation, entries[2].Operation)
	}

	broken := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
	makeActionLogDirectory(t, broken)
	var result folderMutationResult
	res := broken.run("folders", "create", "Projects", "--json")
	decodeOK(t, res, &result)
	if result.ActionLogStatus.Written || result.ActionLogStatus.Reason != "append_failed" || result.ActionLogStatus.ErrorCode != errs.CodeActionLogWriteFailed {
		t.Fatalf("action log status = %#v", result.ActionLogStatus)
	}
	human := broken.run("folders", "create", "Projects")
	if human.Exit != 0 || !strings.Contains(human.Stderr, "Warning: the change was not written to the action log") {
		t.Fatalf("exit=%d stderr=%s", human.Exit, human.Stderr)
	}
}

// read_only and drafts_only block every folder change, previews included.
func TestFolderChangesBlockedByPolicy(t *testing.T) {
	for _, policy := range []string{"policy.read_only", "policy.drafts_only"} {
		t.Run(policy, func(t *testing.T) {
			h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
			decodeOK(t, h.run("config", "set", policy, "true", "--account", "work"), nil)
			for _, args := range [][]string{
				{"folders", "create", "Projects", "--json"},
				{"folders", "create", "Projects", "--dry-run", "--json"},
				{"folders", "rename", "Clients", "Accounts", "--json"},
				{"folders", "rename", "Clients", "Accounts", "--dry-run", "--json"},
				{"folders", "delete", "Clients", "--confirm", "Clients", "--json"},
				{"folders", "delete", "Clients", "--dry-run", "--json"},
			} {
				assertErrorCodeExit(t, h.run(args...), errs.CodePolicyDenied, 4)
			}
		})
	}
}
