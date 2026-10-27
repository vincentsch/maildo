package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestFoldersListFakeProvider(t *testing.T) {
	h := seededFoldersHarness(t, "work")

	var active folderListResult
	decodeOK(t, h.run("folders", "list", "--json"), &active)
	assertSelection(t, active.AccountSelection, "single", "work")
	assertFoldersLabelled(t, active.Folders, "work", "fake")

	var explicit folderListResult
	decodeOK(t, h.run("folders", "list", "--account", "work", "--json"), &explicit)
	assertSelection(t, explicit.AccountSelection, "single", "work")

	var many folderListResult
	decodeOK(t, h.run("folders", "list", "--account", "work", "--account", "personal", "--json"), &many)
	assertSelection(t, many.AccountSelection, "read_many", "work", "personal")
	if len(many.Folders) != 2*len(testkit.SampleFolders().Folders) {
		t.Fatalf("read_many folders = %d", len(many.Folders))
	}
	assertFolderCountsByAccount(t, many.Folders, map[string]int{"work": len(testkit.SampleFolders().Folders), "personal": len(testkit.SampleFolders().Folders)})

	var all folderListResult
	decodeOK(t, h.run("folders", "list", "--all-accounts", "--json"), &all)
	assertSelection(t, all.AccountSelection, "all", "work", "personal")
	assertFolderCountsByAccount(t, all.Folders, map[string]int{"work": len(testkit.SampleFolders().Folders), "personal": len(testkit.SampleFolders().Folders)})
}

func TestCommandSelectedFolderValidatesHydratedResponse(t *testing.T) {
	acct := provider.ResolvedAccount{Name: "work", Provider: "fake"}
	valid := mail.Folder{ID: "inbox-id", Account: "work", Provider: "fake", DisplayName: "Inbox"}
	if got, err := commandSelectedFolder(acct, []mail.Folder{valid}, "inbox-id"); err != nil || got.ID != "inbox-id" {
		t.Fatalf("valid selected folder = %#v, %v", got, err)
	}
	for _, tt := range []struct {
		name    string
		folders []mail.Folder
	}{
		{name: "missing", folders: nil},
		{name: "duplicate", folders: []mail.Folder{valid, valid}},
		{name: "wrong id", folders: []mail.Folder{{ID: "other", Account: "work", Provider: "fake"}}},
		{name: "wrong account", folders: []mail.Folder{{ID: "inbox-id", Account: "personal", Provider: "fake"}}},
		{name: "wrong provider", folders: []mail.Folder{{ID: "inbox-id", Account: "work", Provider: "outlook"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := commandSelectedFolder(acct, tt.folders, "inbox-id")
			if productErrorCode(err) != errs.CodeProviderDataError {
				t.Fatalf("error = %v, want %s", err, errs.CodeProviderDataError)
			}
			if details := safeErrorDetails(err); details["reason"] != "invalid_protocol" {
				t.Fatalf("details = %#v", details)
			}
		})
	}
}

func TestFoldersListEmptyProvider(t *testing.T) {
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", mail.FolderList{}), "work")

	human := h.run("folders", "list")
	decodeOK(t, human, nil)
	if strings.TrimSpace(human.Stdout) != "No folders." {
		t.Fatalf("empty human stdout = %q", human.Stdout)
	}
	var list folderListResult
	decodeOK(t, h.run("folders", "list", "--json"), &list)
	if len(list.Folders) != 0 {
		t.Fatalf("empty list folders = %#v", list.Folders)
	}
	assertErrorCodeExit(t, h.run("folders", "view", "inbox", "--json"), errs.CodeFolderNotFound, 5)
}

func TestFoldersDataShapeEdgeCases(t *testing.T) {
	folders := edgeCaseFolders()
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", mail.FolderList{Folders: folders}), "work")

	var list folderListResult
	decodeOK(t, h.run("folders", "list", "--json"), &list)
	if len(list.Folders) != len(folders) {
		t.Fatalf("folders = %d, want %d", len(list.Folders), len(folders))
	}
	localized := folderByIDInList(t, list.Folders, "localized-id")
	if localized.DisplayName != "Buchhaltung" || localized.TotalCount != 0 || localized.UnreadCount != 0 || localized.ChildCount != 0 {
		t.Fatalf("localized/zero folder = %#v", localized)
	}

	var deep folderViewResult
	decodeOK(t, h.run("folders", "view", "L1/L2/L3/L4", "--json"), &deep)
	if deep.Folder.ID != "deep-4" || deep.MatchedBy != mail.MatchPath || deep.Folder.ParentPath != "L1/L2/L3" {
		t.Fatalf("deep path view = %#v", deep)
	}

	plain := h.run("folders", "list", "--plain")
	decodeOK(t, plain, nil)
	lines := strings.Split(strings.TrimSuffix(plain.Stdout, "\n"), "\n")
	if len(lines) != len(folders) {
		t.Fatalf("plain rows = %d, want %d\n%s", len(lines), len(folders), plain.Stdout)
	}
	if strings.Contains(plain.Stdout, "Tabs\tAnd") || strings.Contains(plain.Stdout, "And\nLines") || !strings.Contains(plain.Stdout, "Tabs And Lines") {
		t.Fatalf("plain output did not sanitize control characters: %q", plain.Stdout)
	}
	if !strings.Contains(plain.Stdout, "Buchhaltung") {
		t.Fatalf("plain output lost localized display name: %q", plain.Stdout)
	}
}

func TestFoldersResolutionIsAccountLocal(t *testing.T) {
	folders := mail.FolderList{Folders: []mail.Folder{
		{ID: "shared-id", DisplayName: "Shared", Path: "Shared"},
		{ID: "unique-id", DisplayName: "Unique", Path: "Unique"},
	}}
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", folders), "work")

	var all folderListResult
	decodeOK(t, h.run("folders", "list", "--all-accounts", "--json"), &all)
	assertFolderCountsByAccount(t, all.Folders, map[string]int{"work": len(folders.Folders), "personal": len(folders.Folders)})

	var personal folderViewResult
	decodeOK(t, h.run("folders", "view", "shared-id", "--account", "personal", "--json"), &personal)
	if personal.Account != "personal" || personal.Folder.Account != "personal" || personal.Folder.ID != "shared-id" {
		t.Fatalf("personal shared-id resolution = %#v", personal)
	}
}

func TestFoldersOutputModesMetaAndDryRun(t *testing.T) {
	h := seededFoldersHarness(t, "work")

	human := h.run("folders", "list")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "14 folders (work)") || !strings.Contains(human.Stdout, "FOLDER") || !strings.Contains(human.Stdout, "Deleted Items (Trash)") {
		t.Fatalf("human stdout = %q", human.Stdout)
	}

	jq := h.run("folders", "list", "--json", "--jq", ".scope")
	if strings.TrimSpace(jq.Stdout) != "single" {
		t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("folders", "list", "--template", "{{.scope}}")
	if strings.TrimSpace(tmpl.Stdout) != "single" {
		t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	plain := h.run("folders", "list", "--plain")
	decodeOK(t, plain, nil)
	if !strings.Contains(plain.Stdout, "work\tfake\tinbox-id\tInbox\n") {
		t.Fatalf("plain stdout = %q", plain.Stdout)
	}

	var envelope struct {
		Data folderListResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("folders", "list", "--json", "--include-meta"), &envelope)
	assertSelection(t, envelope.Data.AccountSelection, "single", "work")
	if envelope.Meta.Extra["command_path"] != "maildo folders list" || envelope.Meta.Extra["scope"] != "single" {
		t.Fatalf("meta = %#v", envelope.Meta.Extra)
	}
	if envelope.Meta.Extra["folder_count"] != float64(len(testkit.SampleFolders().Folders)) {
		t.Fatalf("folder_count = %#v", envelope.Meta.Extra["folder_count"])
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"work"}) {
		t.Fatalf("meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake"}) {
		t.Fatalf("meta providers = %#v", got)
	}

	bad := h.run("folders", "list", "--include-meta", "--dry-run", "--json")
	assertFrameworkUsage(t, bad)
	if !strings.Contains(bad.Stderr, "--include-meta cannot be combined with --dry-run") {
		t.Fatalf("dry-run meta stderr = %s", bad.Stderr)
	}

	var dryList folderListResult
	decodeOK(t, h.run("folders", "list", "--dry-run", "--json"), &dryList)
	if len(dryList.Folders) == 0 {
		t.Fatal("dry-run list rendered no folders")
	}
	var dryView folderViewResult
	decodeOK(t, h.run("folders", "view", "inbox", "--dry-run", "--json"), &dryView)
	if dryView.Folder.ID != "inbox-id" {
		t.Fatalf("dry-run view = %#v", dryView)
	}
}

func TestFolderMutationsFakeProvider(t *testing.T) {
	t.Run("root create does not require folder reads", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		fake.FoldersMutateImplemented = true
		h := seededFoldersHarnessWithProvider(t, fake, "work")

		var result folderMutationResult
		decodeOK(t, h.run("folders", "create", "Projects", "--json"), &result)
		assertSelection(t, result.AccountSelection, "single", "work")
		if result.Operation != string(mail.FolderOperationCreate) || !result.Applied || result.ResultFolder.ID != "fake-folder-projects" {
			t.Fatalf("create result = %#v", result)
		}
		if fake.FolderListCalls.Load() != 0 || fake.FolderCreateCalls.Load() != 1 {
			t.Fatalf("root create calls list=%d create=%d", fake.FolderListCalls.Load(), fake.FolderCreateCalls.Load())
		}
	})

	t.Run("root create dry run still requires mutate capability", func(t *testing.T) {
		fake := testkit.NewFakeFolderProvider("fake", testkit.SampleFolders())
		h := seededFoldersHarnessWithProvider(t, fake, "work")
		assertErrorCode(t, h.run("folders", "create", "Projects", "--dry-run", "--json"), errs.CodeUnsupportedCapability)
		if fake.AccountClientCalls.Load() != 0 || fake.FolderCreateCalls.Load() != 0 {
			t.Fatalf("dry-run calls client=%d create=%d", fake.AccountClientCalls.Load(), fake.FolderCreateCalls.Load())
		}
	})

	t.Run("create under resolved parent and protected parent", func(t *testing.T) {
		fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
		h := seededFoldersHarnessWithProvider(t, fake, "work")

		var nested folderMutationResult
		decodeOK(t, h.run("folders", "create", "Q3", "--parent", "clients-id", "--json"), &nested)
		if nested.ParentFolder == nil || nested.ParentFolder.ID != "clients-id" || nested.ResultFolder.Path != "Clients/Q3" {
			t.Fatalf("nested create = %#v", nested)
		}
		var underInbox folderMutationResult
		decodeOK(t, h.run("folders", "create", "Allowed Child", "--parent", "inbox", "--json"), &underInbox)
		if underInbox.ParentFolder == nil || underInbox.ParentFolder.WellKnownName != mail.WellKnownInbox {
			t.Fatalf("protected parent create = %#v", underInbox)
		}
	})

	t.Run("rename and delete successes", func(t *testing.T) {
		fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
		h := seededFoldersHarnessWithProvider(t, fake, "work")

		var renamed folderMutationResult
		decodeOK(t, h.run("folders", "rename", "clients-id", "Accounts", "--json"), &renamed)
		if renamed.Operation != string(mail.FolderOperationRename) || renamed.ResultFolder.Path != "Accounts" || renamed.OldDisplayName != "Clients" || renamed.NewDisplayName != "Accounts" {
			t.Fatalf("rename result = %#v", renamed)
		}
		if fake.FolderRenamePreflightCalls.Load() != 1 || fake.FolderRenameCalls.Load() != 1 {
			t.Fatalf("rename calls preflight=%d write=%d", fake.FolderRenamePreflightCalls.Load(), fake.FolderRenameCalls.Load())
		}

		var deleted folderMutationResult
		decodeOK(t, h.run("folders", "delete", "spare-id", "--confirm", "spare-id", "--json"), &deleted)
		if deleted.Operation != string(mail.FolderOperationDelete) || !deleted.ConfirmationRequired || !deleted.ConfirmationMatched || !deleted.TotalItemCountKnown || !deleted.ChildFolderCountKnown || deleted.TotalItemCount != 0 || deleted.ChildFolderCount != 0 {
			t.Fatalf("delete by id result = %#v", deleted)
		}
		decodeOK(t, h.run("folders", "delete", "Spare", "--confirm", "Spare", "--json"), &folderMutationResult{})
		if fake.FolderDeletePreflightCalls.Load() != 2 || fake.FolderDeleteCalls.Load() != 2 {
			t.Fatalf("delete calls preflight=%d write=%d", fake.FolderDeletePreflightCalls.Load(), fake.FolderDeleteCalls.Load())
		}
	})

	t.Run("delete refuses folders with messages or subfolders", func(t *testing.T) {
		fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
		h := seededFoldersHarnessWithProvider(t, fake, "work")
		for _, tt := range []struct {
			args   []string
			reason string
		}{
			{[]string{"folders", "delete", "Clients", "--confirm", "Clients", "--json"}, "has_children"},
			{[]string{"folders", "delete", "Clients", "--dry-run", "--json"}, "has_children"},
			{[]string{"folders", "delete", "Clients/ACME", "--confirm", "Clients/ACME", "--json"}, "not_empty"},
			{[]string{"folders", "delete", "Clients/ACME", "--dry-run", "--json"}, "not_empty"},
		} {
			res := h.run(tt.args...)
			assertErrorCode(t, res, errs.CodeProtectedFolder)
			if body := errorJSON(t, res); body["reason"] != tt.reason {
				t.Fatalf("%v reason = %v", tt.args, body["reason"])
			}
		}
		if fake.FolderDeleteCalls.Load() != 0 {
			t.Fatalf("a folder with messages or subfolders was deleted: %d", fake.FolderDeleteCalls.Load())
		}
		human := h.run("folders", "delete", "Clients/ACME", "--confirm", "Clients/ACME")
		if human.Exit != 1 || !strings.Contains(human.Stderr, `"Clients/ACME" holds 4 messages, and maildo deletes only empty folders without subfolders`) || !strings.Contains(human.Stderr, "Move or delete its messages first") {
			t.Fatalf("exit=%d stderr=%s", human.Exit, human.Stderr)
		}
	})
}

func TestFolderMutationValidationProtectionAndConfirmation(t *testing.T) {
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")

	assertErrorCode(t, h.run("folders", "create", "Bad/Name", "--parent", "clients-id", "--json"), errs.CodeInvalidFolderName)
	assertErrorCodeExit(t, h.run("folders", "create", "Projects", "--parent", "missing", "--json"), errs.CodeFolderNotFound, 5)
	assertErrorCode(t, h.run("folders", "create", "Projects", "--parent", "Reports", "--json"), errs.CodeAmbiguousFolder)
	assertErrorCode(t, h.run("folders", "rename", "inbox", "Bad/Name", "--json"), errs.CodeInvalidFolderName)
	assertErrorCodeExit(t, h.run("folders", "rename", "missing", "Archive 2", "--json"), errs.CodeFolderNotFound, 5)
	assertErrorCode(t, h.run("folders", "rename", "Reports", "Archive 2", "--json"), errs.CodeAmbiguousFolder)
	protected := h.run("folders", "rename", "inbox", "Archive 2", "--json")
	assertErrorCode(t, protected, errs.CodeProtectedFolder)
	protectedDetails := errorJSON(t, protected)
	if protectedDetails["account"] != "work" || protectedDetails["provider"] != "fake" ||
		protectedDetails["folder_id"] != "inbox-id" || protectedDetails["display_name"] != "Inbox" ||
		protectedDetails["reason"] != "well_known_folder" {
		t.Fatalf("protected folder details = %#v", protectedDetails)
	}
	assertErrorCode(t, h.run("folders", "delete", "inbox", "--confirm", "inbox-id", "--json"), errs.CodeProtectedFolder)
	assertErrorCode(t, h.run("folders", "delete", "junk-copy-id", "--confirm", "junk-copy-id", "--json"), errs.CodeProtectedFolder)
	assertErrorCodeExit(t, h.run("folders", "delete", "missing", "--confirm", "missing", "--json"), errs.CodeFolderNotFound, 5)
	assertErrorCode(t, h.run("folders", "delete", "Reports", "--confirm", "Reports", "--json"), errs.CodeAmbiguousFolder)
	assertErrorCode(t, h.run("folders", "delete", "junk", "--confirm", "junk-id", "--json"), errs.CodeAmbiguousFolder)

	for _, tt := range []struct {
		args      []string
		forbidden []string
	}{
		{args: []string{"folders", "delete", "spare-id", "--json"}, forbidden: []string{"spare-id", "Spare"}},
		{args: []string{"folders", "delete", "spare-id", "--confirm", "wrong", "--json"}, forbidden: []string{"spare-id", "Spare"}},
		{args: []string{"folders", "delete", "Spare", "--json"}, forbidden: []string{"spare-id", "Spare"}},
		{args: []string{"folders", "delete", "spare", "--confirm", "spare", "--json"}, forbidden: []string{"spare-id", "Spare"}},
		{args: []string{"folders", "delete", "reports-1", "--confirm", "Reports", "--json"}, forbidden: []string{"reports-1"}},
	} {
		res := h.run(tt.args...)
		assertErrorCode(t, res, errs.CodeConfirmationRequired)
		body := errorJSON(t, res)
		if _, ok := body["folder_id"]; ok {
			t.Fatalf("confirmation error leaked folder id: %#v", body)
		}
		if _, ok := body["path"]; ok {
			t.Fatalf("confirmation error leaked path: %#v", body)
		}
		if _, ok := body["ref"]; ok {
			t.Fatalf("confirmation error leaked raw ref: %#v", body)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal confirmation body: %v", err)
		}
		for _, token := range tt.forbidden {
			if strings.Contains(string(encoded), token) {
				t.Fatalf("confirmation error leaked token %q: %s", token, encoded)
			}
		}
	}

	folders := testkit.SampleFolders()
	folders.Folders = append(folders.Folders, mail.Folder{ID: "empty-path-id", DisplayName: "No Path"})
	emptyPath := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", folders), "work")
	assertErrorCode(t, emptyPath.run("folders", "delete", "empty-path-id", "--confirm", "No Path", "--json"), errs.CodeConfirmationRequired)
	decodeOK(t, emptyPath.run("folders", "delete", "empty-path-id", "--confirm", "empty-path-id", "--json"), &folderMutationResult{})
}

func TestFolderMutationBoundaryValidationDoesNotReachProvider(t *testing.T) {
	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	controls := make([]byte, 0, 33)
	for value := byte(0); value <= 0x1f; value++ {
		controls = append(controls, value)
	}
	controls = append(controls, 0x7f)

	for _, control := range controls {
		for position, raw := range []string{
			string([]byte{control}) + "Alpha",
			"Al" + string([]byte{control}) + "pha",
			"Alpha" + string([]byte{control}),
		} {
			for _, operation := range []string{"create", "rename"} {
				name := fmt.Sprintf("%s/0x%02x/position-%d", operation, control, position)
				t.Run(name, func(t *testing.T) {
					before := folderMutationCallCounts(fake)
					args := []string{"folders", "create", raw, "--json"}
					if operation == "rename" {
						args = []string{"folders", "rename", "clients-id", raw, "--json"}
					}
					res := h.run(args...)
					assertErrorCode(t, res, errs.CodeInvalidFolderName)
					if got := folderMutationCallCounts(fake); got != before {
						t.Fatalf("invalid name reached provider: before=%#v after=%#v", before, got)
					}
				})
			}
		}
	}
}

func TestFolderMutationInvalidOutputOptionsDoNotWrite(t *testing.T) {
	operations := []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"folders", "create", "Projects"}},
		{name: "rename", args: []string{"folders", "rename", "clients-id", "Accounts"}},
		{name: "delete", args: []string{"folders", "delete", "spare-id", "--confirm", "spare-id"}},
	}
	invalidModes := []struct {
		name string
		args []string
	}{
		{name: "invalid jq", args: []string{"--json", "--jq", ".broken["}},
		{name: "invalid template", args: []string{"--template", "{{"}},
		{name: "plain", args: []string{"--plain"}},
		{name: "dry run metadata", args: []string{"--dry-run", "--include-meta", "--json"}},
	}

	for _, operation := range operations {
		for _, mode := range invalidModes {
			t.Run(operation.name+"/"+mode.name, func(t *testing.T) {
				fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
				h := seededFoldersHarnessWithProvider(t, fake, "work")
				args := append(append([]string(nil), operation.args...), mode.args...)
				res := h.run(args...)
				if res.Exit == 0 {
					t.Fatalf("invalid output options succeeded: stdout=%s", res.Stdout)
				}
				if strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("invalid output options produced stdout: %q", res.Stdout)
				}
				if got := folderMutationCallCounts(fake); got != (folderMutationCalls{}) {
					t.Fatalf("invalid output options reached provider: %#v", got)
				}
			})
		}
	}
}

func TestFolderMutationFrameworkErrorsDoNotReachProvider(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "create extra operand", args: []string{"folders", "create", "Projects", "extra", "--json"}},
		{name: "rename extra operand", args: []string{"folders", "rename", "clients-id", "Accounts", "extra", "--json"}},
		{name: "delete extra operand", args: []string{"folders", "delete", "spare-id", "extra", "--confirm", "spare-id", "--json"}},
		{name: "create all accounts", args: []string{"folders", "create", "Projects", "--all-accounts", "--json"}},
		{name: "rename repeated account", args: []string{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--account", "personal", "--json"}},
		{name: "delete unregistered apply", args: []string{"folders", "delete", "spare-id", "--confirm", "spare-id", "--apply", "--json"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
			h := seededFoldersHarnessWithProvider(t, fake, "work")
			res := h.run(tt.args...)
			if res.Exit == 0 || strings.TrimSpace(res.Stdout) != "" {
				t.Fatalf("framework error exit=%d stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
			}
			if got := folderMutationCallCounts(fake); got != (folderMutationCalls{}) {
				t.Fatalf("framework error reached provider: %#v", got)
			}
		})
	}
}

func TestFolderCreateBlankParentUsesRoot(t *testing.T) {
	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	for _, parent := range []string{"", "   "} {
		var result folderMutationResult
		decodeOK(t, h.run("folders", "create", "  Projects  ", "--parent", parent, "--json"), &result)
		if result.ParentFolder != nil || result.NewDisplayName != "Projects" || result.ResultFolder == nil || result.ResultFolder.DisplayName != "Projects" {
			t.Fatalf("blank parent result = %#v", result)
		}
	}
	if fake.FolderListCalls.Load() != 0 || fake.FolderCreateCalls.Load() != 2 {
		t.Fatalf("blank parent calls list=%d create=%d", fake.FolderListCalls.Load(), fake.FolderCreateCalls.Load())
	}

	preview := decodePreview(t, h.run("folders", "create", "  Preview Name  ", "--parent", "   ", "--dry-run", "--json"))
	body := preview["body"].(map[string]any)
	if body["display_name"] != "Preview Name" {
		t.Fatalf("trimmed preview = %#v", preview)
	}
	if _, ok := body["parent_folder_id"]; ok {
		t.Fatalf("root preview contains parent: %#v", preview)
	}
}

func TestFolderDeleteConfirmationIsOrderIndependent(t *testing.T) {
	folders := []mail.Folder{
		{ID: "target/id", DisplayName: "Target", Path: "Clients/Target"},
		{ID: "case-id", DisplayName: "target", Path: "clients/target"},
		{ID: "duplicate-a", DisplayName: "Duplicate", Path: "Same/Path"},
		{ID: "duplicate-b", DisplayName: "Duplicate", Path: "Same/Path"},
		{ID: "empty-id", DisplayName: "No Path", Path: ""},
	}
	targets := []struct {
		id       string
		accepted map[string]bool
	}{
		{id: "target/id", accepted: map[string]bool{"target/id": true, "Clients/Target": true}},
		{id: "duplicate-a", accepted: map[string]bool{"duplicate-a": true}},
		{id: "empty-id", accepted: map[string]bool{"empty-id": true}},
	}
	confirmations := []string{"", "target/id", " target/id ", "Clients/Target", "clients/target", "Target", "target", "Same/Path", "same/path", "duplicate-a", "empty-id", "No Path", "target/", "Clients/"}
	orders := [][]mail.Folder{append([]mail.Folder(nil), folders...), append([]mail.Folder(nil), folders...)}
	for left, right := 0, len(orders[1])-1; left < right; left, right = left+1, right-1 {
		orders[1][left], orders[1][right] = orders[1][right], orders[1][left]
	}
	for _, order := range orders {
		for _, seed := range targets {
			target := folderByIDInList(t, order, seed.id)
			for _, confirm := range confirmations {
				want := seed.accepted[strings.TrimSpace(confirm)]
				if got := confirmFolderDelete(confirm, target, order); got != want {
					t.Fatalf("confirmFolderDelete(%q, %q) = %t, want %t; order=%#v", confirm, seed.id, got, want, order)
				}
			}
		}
	}
}

func TestFolderMutationHumanOutputSanitizesUnsafeProviderText(t *testing.T) {
	hostileText := "Name\x00\x07\x1b[31m\x1b]0;title\x07\x7f\u0085\u009b31m\u200b\u202e\u2028\u2029End"
	folders := mail.FolderList{Folders: []mail.Folder{{ID: "hostile-id", DisplayName: hostileText, Path: hostileText}}}
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", folders), "work")
	res := h.run("folders", "rename", "hostile-id", "Safe Name")
	decodeOK(t, res, nil)
	if strings.Count(res.Stdout, "\n") != 1 {
		t.Fatalf("unsafe output split records: %q", res.Stdout)
	}
	for _, r := range strings.TrimSuffix(res.Stdout, "\n") {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("unsafe rune U+%04X remained in output %q", r, res.Stdout)
		}
	}
}

func TestFolderMutationsDoNotChangeLocalState(t *testing.T) {
	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	before := hashTree(t, h.root)

	decodeOK(t, h.run("folders", "create", "Preview", "--dry-run", "--json"), nil)
	decodeOK(t, h.run("folders", "rename", "clients-id", "Preview", "--dry-run", "--json"), nil)
	decodeOK(t, h.run("folders", "delete", "spare-id", "--dry-run", "--json"), nil)
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("folder previews changed local state\nbefore=%#v\nafter=%#v", before, after)
	}
	decodeOK(t, h.run("folders", "create", "Projects", "--json"), &folderMutationResult{})
	decodeOK(t, h.run("folders", "rename", "clients-id", "Accounts", "--json"), &folderMutationResult{})
	decodeOK(t, h.run("folders", "delete", "spare-id", "--confirm", "spare-id", "--json"), &folderMutationResult{})

	// Applied changes add to the action log and touch nothing else.
	after := hashTree(t, h.root)
	if len(after) != len(before)+1 || !strings.HasPrefix(after[0], "audit/actions.jsonl ") || !sameStrings(before, after[1:]) {
		t.Fatalf("folder mutations changed local state\nbefore=%#v\nafter=%#v", before, after)
	}
}

type folderMutationCalls struct {
	list, create, renamePreflight, deletePreflight, rename, delete int64
}

func folderMutationCallCounts(fake *testkit.FakeProvider) folderMutationCalls {
	return folderMutationCalls{
		list:            fake.FolderListCalls.Load(),
		create:          fake.FolderCreateCalls.Load(),
		renamePreflight: fake.FolderRenamePreflightCalls.Load(),
		deletePreflight: fake.FolderDeletePreflightCalls.Load(),
		rename:          fake.FolderRenameCalls.Load(),
		delete:          fake.FolderDeleteCalls.Load(),
	}
}

func assertExactMapKeys(t *testing.T, values map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(values))
	for key := range values {
		got = append(got, key)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !sameStrings(got, want) {
		t.Fatalf("map keys = %#v, want %#v; values=%#v", got, want, values)
	}
}

func TestFolderMutationOutputDryRunNoPromptAndActionLog(t *testing.T) {
	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")

	human := h.run("folders", "rename", "clients-id", "Accounts")
	decodeOK(t, human, nil)
	if human.Stdout != "Renamed the folder \"Clients\" to \"Accounts\" in work.\n" {
		t.Fatalf("human rename stdout = %q", human.Stdout)
	}
	hostileFolders := mail.FolderList{Folders: []mail.Folder{{
		ID:          "hostile-id",
		DisplayName: "Tabs\tAnd\nLines",
		Path:        "Tabs\tAnd\nLines",
	}}}
	hostile := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", hostileFolders), "work")
	hostileHuman := hostile.run("folders", "rename", "hostile-id", "Safe Name")
	decodeOK(t, hostileHuman, nil)
	if strings.Count(hostileHuman.Stdout, "\n") != 1 || strings.ContainsAny(hostileHuman.Stdout, "\t\r") || !strings.Contains(hostileHuman.Stdout, "Tabs And Lines") {
		t.Fatalf("human mutation output did not sanitize provider controls: %q", hostileHuman.Stdout)
	}
	createHuman := h.run("folders", "create", "Projects")
	decodeOK(t, createHuman, nil)
	if strings.Contains(createHuman.Stdout, "name:->") {
		t.Fatalf("human create rendered rename transition: %q", createHuman.Stdout)
	}
	jq := h.run("folders", "create", "Projects", "--json", "--jq", ".operation")
	if strings.TrimSpace(jq.Stdout) != "folder_create" {
		t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("folders", "create", "Projects", "--template", "{{.operation}}")
	if strings.TrimSpace(tmpl.Stdout) != "folder_create" {
		t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	var envelope struct {
		Data folderMutationResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("folders", "delete", "spare-id", "--confirm", "spare-id", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["command_path"] != "maildo folders delete" || envelope.Meta.Extra["operation"] != string(mail.FolderOperationDelete) || envelope.Meta.Extra["confirmation_matched"] != true {
		t.Fatalf("delete meta = %#v", envelope.Meta.Extra)
	}
	logged, err := os.ReadFile(filepath.Join(h.root, "audit", "actions.jsonl"))
	if err != nil || !strings.Contains(string(logged), `"kind":"folder"`) {
		t.Fatalf("folder mutations were not logged: %v\n%s", err, logged)
	}

	beforeCreate := fake.FolderCreateCalls.Load()
	createPreview := decodePreview(t, h.run("folders", "create", "Dry Run", "--dry-run", "--json"))
	if fake.FolderCreateCalls.Load() != beforeCreate || createPreview["method"] != string(mail.FolderOperationCreate) {
		t.Fatalf("create dry-run mutated=%d preview=%#v", fake.FolderCreateCalls.Load()-beforeCreate, createPreview)
	}
	beforeRename := fake.FolderRenameCalls.Load()
	beforeRenamePreflight := fake.FolderRenamePreflightCalls.Load()
	renamePreview := decodePreview(t, h.run("folders", "rename", "clients-id", "Dry Rename", "--dry-run", "--json"))
	if fake.FolderRenameCalls.Load() != beforeRename || fake.FolderRenamePreflightCalls.Load() != beforeRenamePreflight+1 || renamePreview["method"] != string(mail.FolderOperationRename) {
		t.Fatalf("rename dry-run calls preflight=%d write=%d preview=%#v", fake.FolderRenamePreflightCalls.Load(), fake.FolderRenameCalls.Load(), renamePreview)
	}
	beforeDelete := fake.FolderDeleteCalls.Load()
	beforeDeletePreflight := fake.FolderDeletePreflightCalls.Load()
	deletePreview := decodePreview(t, h.run("folders", "delete", "spare-id", "--confirm", "wrong", "--dry-run", "--json"))
	body := deletePreview["body"].(map[string]any)
	if fake.FolderDeleteCalls.Load() != beforeDelete ||
		fake.FolderDeletePreflightCalls.Load() != beforeDeletePreflight+1 ||
		body["target_folder_id"] != "spare-id" ||
		body["target_folder"] != "Spare" ||
		body["total_item_count"] != "0" ||
		body["total_item_count_known"] != "true" ||
		body["child_folder_count"] != "0" ||
		body["child_folder_count_known"] != "true" ||
		body["counts_source"] != mail.FolderCountsSourceRefreshed ||
		body["confirmation_required"] != "true" ||
		body["path_confirmation_available"] != "true" {
		t.Fatalf("delete dry-run calls preflight=%d write=%d preview=%#v", fake.FolderDeletePreflightCalls.Load(), fake.FolderDeleteCalls.Load(), deletePreview)
	}
	badMeta := h.run("folders", "delete", "spare-id", "--dry-run", "--include-meta", "--json")
	assertFrameworkUsage(t, badMeta)
	plain := h.run("folders", "create", "Projects", "--plain")
	assertFrameworkUsage(t, plain)

	reader := &failOnRead{}
	res := h.runWith(testutil.Options{Stdin: reader, TerminalSet: true, Terminal: true}, "folders", "delete", "spare-id")
	if res.Exit != 1 || !strings.Contains(res.Stderr, "needs --confirm") {
		t.Fatalf("human delete without confirm = exit %d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	if reader.reads.Load() != 0 {
		t.Fatal("folders delete attempted to read stdin")
	}
	assertErrorCode(t, h.run("folders", "delete", "spare-id", "--no-prompt", "--json"), errs.CodeConfirmationRequired)
}

func TestFolderMutationPreviewSchemasAndTransforms(t *testing.T) {
	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	cases := []struct {
		name     string
		args     []string
		bodyKeys []string
	}{
		{name: "root create", args: []string{"folders", "create", "Projects", "--dry-run", "--json"}, bodyKeys: []string{"account", "applied", "command", "display_name", "operation", "provider"}},
		{name: "child create", args: []string{"folders", "create", "Q3", "--parent", "clients-id", "--dry-run", "--json"}, bodyKeys: []string{"account", "applied", "command", "display_name", "operation", "parent_folder", "parent_folder_id", "provider"}},
		{name: "rename", args: []string{"folders", "rename", "clients-id", "Accounts", "--dry-run", "--json"}, bodyKeys: []string{"account", "applied", "command", "new_display_name", "operation", "provider", "target_folder", "target_folder_id"}},
		{name: "delete", args: []string{"folders", "delete", "spare-id", "--dry-run", "--json"}, bodyKeys: []string{"account", "applied", "child_folder_count", "child_folder_count_known", "command", "confirmation_required", "counts_source", "operation", "path_confirmation_available", "provider", "target_folder", "target_folder_id", "total_item_count", "total_item_count_known"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			preview := decodePreview(t, h.run(tt.args...))
			assertExactMapKeys(t, preview, []string{"body", "dry_run", "method", "path"})
			if preview["dry_run"] != true {
				t.Fatalf("preview dry_run = %#v", preview["dry_run"])
			}
			body, ok := preview["body"].(map[string]any)
			if !ok {
				t.Fatalf("preview body = %#v", preview["body"])
			}
			assertExactMapKeys(t, body, tt.bodyKeys)
			for key, value := range body {
				if _, ok := value.(string); !ok {
					t.Fatalf("preview field %s = %#v, want string", key, value)
				}
			}
		})
	}

	first := h.run("folders", "create", "Deterministic", "--dry-run", "--json")
	second := h.run("folders", "create", "Deterministic", "--dry-run", "--json")
	if first.Stdout != second.Stdout || first.Stderr != "" || second.Stderr != "" {
		t.Fatalf("preview output changed\nfirst=%q\nsecond=%q", first.Stdout, second.Stdout)
	}

	jq := h.run("folders", "create", "Projects", "--dry-run", "--json", "--jq", ".body.display_name")
	if strings.TrimSpace(jq.Stdout) != "Projects" || jq.Stderr != "" {
		t.Fatalf("dry-run jq stdout=%q stderr=%q", jq.Stdout, jq.Stderr)
	}
	template := h.run("folders", "rename", "clients-id", "Accounts", "--dry-run", "--template", "{{.body.operation}}/{{.body.new_display_name}}")
	if strings.TrimSpace(template.Stdout) != "folder_rename/Accounts" || template.Stderr != "" {
		t.Fatalf("dry-run template stdout=%q stderr=%q", template.Stdout, template.Stderr)
	}

	metaJQ := h.run("folders", "create", "Projects", "--json", "--include-meta", "--jq", "[.data.operation,.meta.extra.operation]")
	if strings.TrimSpace(metaJQ.Stdout) != `["folder_create","folder_create"]` || metaJQ.Stderr != "" {
		t.Fatalf("metadata jq stdout=%q stderr=%q", metaJQ.Stdout, metaJQ.Stderr)
	}
	metaTemplate := h.run("folders", "rename", "clients-id", "Accounts", "--include-meta", "--template", "{{.data.operation}}/{{.meta.extra.operation}}")
	if strings.TrimSpace(metaTemplate.Stdout) != "folder_rename/folder_rename" || metaTemplate.Stderr != "" {
		t.Fatalf("metadata template stdout=%q stderr=%q", metaTemplate.Stdout, metaTemplate.Stderr)
	}
}

func TestFolderMutationAppliedResultSchemas(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		dataKeys []string
		metaKeys []string
	}{
		{
			name:     "create",
			args:     []string{"folders", "create", "Projects"},
			dataKeys: []string{"accounts", "action_log_status", "applied", "created_folder", "new_display_name", "operation", "provider_metadata", "result_folder", "scope", "verification_status"},
			metaKeys: []string{"accounts", "applied", "command_path", "operation", "providers", "result_folder_id", "scope", "verification_status"},
		},
		{
			name:     "rename",
			args:     []string{"folders", "rename", "clients-id", "Accounts"},
			dataKeys: []string{"accounts", "action_log_status", "applied", "new_display_name", "old_display_name", "operation", "provider_metadata", "result_folder", "scope", "target_folder", "verification_status"},
			metaKeys: []string{"accounts", "applied", "command_path", "operation", "providers", "result_folder_id", "scope", "target_folder_id", "verification_status"},
		},
		{
			name:     "delete",
			args:     []string{"folders", "delete", "spare-id", "--confirm", "spare-id"},
			dataKeys: []string{"accounts", "action_log_status", "applied", "child_folder_count", "child_folder_count_known", "confirmation_matched", "confirmation_required", "counts_source", "deleted_folder", "operation", "provider_metadata", "result_folder", "scope", "target_folder", "total_item_count", "total_item_count_known", "verification_status"},
			metaKeys: []string{"accounts", "applied", "child_folder_count", "child_folder_count_known", "command_path", "confirmation_matched", "confirmation_required", "counts_source", "operation", "providers", "result_folder_id", "scope", "target_folder_id", "total_item_count", "total_item_count_known", "verification_status"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
			h := seededFoldersHarnessWithProvider(t, fake, "work")
			var raw map[string]any
			rawResult := h.run(append(append([]string(nil), tt.args...), "--json")...)
			decodeOK(t, rawResult, &raw)
			assertExactMapKeys(t, raw, tt.dataKeys)
			if tt.name == "delete" {
				for _, key := range []string{"total_item_count", "total_item_count_known", "child_folder_count", "child_folder_count_known"} {
					if count := strings.Count(rawResult.Stdout, `"`+key+`"`); count != 1 {
						t.Fatalf("raw delete key %q occurs %d times: %s", key, count, rawResult.Stdout)
					}
				}
			}

			fake = testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
			h = seededFoldersHarnessWithProvider(t, fake, "work")
			var envelope struct {
				Data map[string]any `json:"data"`
				Meta struct {
					Extra map[string]any `json:"extra"`
				} `json:"meta"`
			}
			decodeOK(t, h.run(append(append([]string(nil), tt.args...), "--json", "--include-meta")...), &envelope)
			assertExactMapKeys(t, envelope.Data, tt.dataKeys)
			assertExactMapKeys(t, envelope.Meta.Extra, tt.metaKeys)
		})
	}

	fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
	h := seededFoldersHarnessWithProvider(t, fake, "work")
	first := h.run("folders", "create", "Deterministic", "--json")
	second := h.run("folders", "create", "Deterministic", "--json")
	if first.Stdout != second.Stdout || first.Stderr != "" || second.Stderr != "" {
		t.Fatalf("applied result changed\nfirst=%q\nsecond=%q", first.Stdout, second.Stdout)
	}
}

func TestFolderMutationGlobalPresentationFlagsKeepSingleWrites(t *testing.T) {
	operations := []struct {
		name   string
		args   []string
		writes func(*testkit.FakeProvider) int64
	}{
		{name: "create", args: []string{"folders", "create", "Projects"}, writes: func(fake *testkit.FakeProvider) int64 { return fake.FolderCreateCalls.Load() }},
		{name: "rename", args: []string{"folders", "rename", "clients-id", "Accounts"}, writes: func(fake *testkit.FakeProvider) int64 { return fake.FolderRenameCalls.Load() }},
		{name: "delete", args: []string{"folders", "delete", "spare-id", "--confirm", "spare-id"}, writes: func(fake *testkit.FakeProvider) int64 { return fake.FolderDeleteCalls.Load() }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
			h := seededFoldersHarnessWithProvider(t, fake, "work")
			args := append(append([]string(nil), operation.args...), "--no-color", "--no-ansi", "--no-pager", "--quiet")
			res := h.run(args...)
			decodeOK(t, res, nil)
			if strings.Count(res.Stdout, "\n") != 1 || res.Stderr != "" {
				t.Fatalf("presentation flags stdout=%q stderr=%q", res.Stdout, res.Stderr)
			}
			if got := operation.writes(fake); got != 1 {
				t.Fatalf("write attempts = %d, want 1", got)
			}
		})
	}
}

func TestFolderDeleteRequiresRefreshedCounts(t *testing.T) {
	acct := provider.ResolvedAccount{Name: "work", Provider: "fake"}
	folder := mail.Folder{ID: "clients-id", DisplayName: "Clients", Path: "Clients"}
	for _, preflight := range []mail.FolderMutationPreflight{
		{CountsSource: "", TotalItemCountKnown: true, ChildFolderKnown: true},
		{CountsSource: "snapshot", TotalItemCountKnown: true, ChildFolderKnown: true},
		{CountsSource: mail.FolderCountsSourceRefreshed, ChildFolderKnown: true},
		{CountsSource: mail.FolderCountsSourceRefreshed, TotalItemCountKnown: true},
	} {
		err := requireEmptyFolder(acct, folder, preflight)
		assertProductCodeReason(t, err, errs.CodeProtectedFolder, "counts_unknown")
		if !strings.Contains(err.Error(), `could not count the messages and subfolders in "Clients"`) {
			t.Fatalf("preflight %#v error = %q", preflight, err)
		}
	}
	if err := requireEmptyFolder(acct, folder, mail.FolderMutationPreflight{CountsSource: mail.FolderCountsSourceRefreshed, TotalItemCountKnown: true, ChildFolderKnown: true}); err != nil {
		t.Fatalf("empty folder refused: %v", err)
	}
}

func assertProductCodeReason(t *testing.T, err error, code, reason string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("error = %#v, want code %s", err, code)
	}
	if got := product.Details()["reason"]; got != reason {
		t.Fatalf("reason = %v, want %s", got, reason)
	}
}

func TestFoldersViewFakeProvider(t *testing.T) {
	h := seededFoldersHarness(t, "work")
	for _, tt := range []struct {
		name      string
		ref       string
		wantID    string
		matchedBy string
	}{
		{name: "alias", ref: "inbox", wantID: "inbox-id", matchedBy: mail.MatchWellKnown},
		{name: "id", ref: "acme-id", wantID: "acme-id", matchedBy: mail.MatchID},
		{name: "exact display", ref: "Slash ID", wantID: "id/with/slash", matchedBy: mail.MatchExactDisplay},
		{name: "ci display", ref: "slash id", wantID: "id/with/slash", matchedBy: mail.MatchCIDisplay},
		{name: "nested path", ref: "Clients/ACME", wantID: "acme-id", matchedBy: mail.MatchPath},
		{name: "slash id", ref: "id/with/slash", wantID: "id/with/slash", matchedBy: mail.MatchID},
		{name: "slash display by id", ref: "opaque-ab-id", wantID: "opaque-ab-id", matchedBy: mail.MatchID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got folderViewResult
			decodeOK(t, h.run("folders", "view", tt.ref, "--json"), &got)
			assertSelection(t, got.AccountSelection, "single", "work")
			if got.Folder.ID != tt.wantID || got.MatchedBy != tt.matchedBy {
				t.Fatalf("view %q = id %q matched %q, want id %q matched %q", tt.ref, got.Folder.ID, got.MatchedBy, tt.wantID, tt.matchedBy)
			}
			if tt.ref == "inbox" && got.Folder.WellKnownName != mail.WellKnownInbox {
				t.Fatalf("inbox well_known_name = %q, want %q", got.Folder.WellKnownName, mail.WellKnownInbox)
			}
		})
	}

	var ci folderViewResult
	decodeOK(t, h.run("folders", "view", "clients/acme", "--json"), &ci)
	if ci.Folder.ID != "acme-id" || ci.MatchedBy != mail.MatchPath {
		t.Fatalf("ci path = %#v", ci)
	}

	var meta struct {
		Data folderViewResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("folders", "view", "inbox", "--json", "--include-meta"), &meta)
	if meta.Meta.Extra["ref"] != "inbox" || meta.Meta.Extra["ref_kind"] != string(mail.FolderRefWellKnown) || meta.Meta.Extra["matched_by"] != mail.MatchWellKnown {
		t.Fatalf("view meta = %#v", meta.Meta.Extra)
	}
}

func TestFoldersErrorsFakeProvider(t *testing.T) {
	h := seededFoldersHarness(t, "work")

	res := h.run("folders", "view", "Reports", "--json")
	assertErrorCodeExit(t, res, errs.CodeAmbiguousFolder, 1)
	body := errorJSON(t, res)
	if body["account"] != "work" || body["provider"] != "fake" || body["ref"] != "Reports" || body["matched_by"] != mail.MatchExactDisplay {
		t.Fatalf("ambiguous body = %#v", body)
	}
	if body["ref_kind"] != string(mail.FolderRefOpaque) {
		t.Fatalf("ambiguous ref_kind = %#v", body["ref_kind"])
	}
	if candidates, ok := body["candidates"].([]any); !ok || len(candidates) == 0 {
		t.Fatalf("candidates = %#v", body["candidates"])
	}

	res = h.run("folders", "view", "spam", "--json")
	assertErrorCodeExit(t, res, errs.CodeAmbiguousFolder, 1)
	body = errorJSON(t, res)
	if body["matched_by"] != mail.MatchWellKnown {
		t.Fatalf("spam ambiguity body = %#v", body)
	}

	res = h.run("folders", "view", "missing", "--json")
	assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)
	body = errorJSON(t, res)
	if body["account"] != "work" || body["provider"] != "fake" || body["ref"] != "missing" {
		t.Fatalf("not found body = %#v", body)
	}
	if body["ref_kind"] != string(mail.FolderRefOpaque) {
		t.Fatalf("not found ref_kind = %#v", body["ref_kind"])
	}

	assertErrorCodeExit(t, h.run("folders", "view", "Clients//ACME", "--json"), errs.CodeFolderNotFound, 5)

	plain := h.run("folders", "view", "inbox", "--plain")
	assertFrameworkUsage(t, plain)
	if !strings.Contains(plain.Stderr, "does not support --plain") {
		t.Fatalf("plain stderr = %s", plain.Stderr)
	}
}

func TestFoldersScopeAndCapabilityErrors(t *testing.T) {
	t.Run("missing active", func(t *testing.T) {
		h := seededFoldersHarness(t, "")
		res := h.run("folders", "list", "--json")
		assertErrorCode(t, res, errs.CodeMissingActiveAccount)
		body := errorJSON(t, res)
		msg := body["error"].(string)
		if !strings.Contains(msg, "maildo auth switch <name>") || !strings.Contains(msg, "--account <name>") {
			t.Fatalf("missing-active message = %q", msg)
		}
	})

	t.Run("unknown and duplicate account", func(t *testing.T) {
		h := seededFoldersHarness(t, "work")
		assertErrorCode(t, h.run("folders", "list", "--account", "missing", "--json"), errs.CodeUnknownAccount)
		assertErrorCode(t, h.run("folders", "list", "--account", "work", "--account", "work", "--json"), errs.CodeInvalidAccountScope)
		listAllAndAccount := h.run("folders", "list", "--all-accounts", "--account", "work", "--json")
		assertFrameworkUsage(t, listAllAndAccount)
		assertNoCode(t, listAllAndAccount)
		assertErrorCode(t, h.run("folders", "view", "inbox", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidAccountScope)
		viewAll := h.run("folders", "view", "inbox", "--all-accounts", "--json")
		assertFrameworkUsage(t, viewAll)
		assertNoCode(t, viewAll)
	})

	t.Run("mutation account scope", func(t *testing.T) {
		h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
		var explicit folderMutationResult
		decodeOK(t, h.run("folders", "create", "Projects", "--account", "personal", "--json"), &explicit)
		assertSelection(t, explicit.AccountSelection, "single", "personal")

		for _, args := range [][]string{
			{"folders", "create", "Projects", "--account", "work", "--account", "personal", "--json"},
			{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--account", "personal", "--json"},
			{"folders", "delete", "spare-id", "--confirm", "spare-id", "--account", "work", "--account", "personal", "--json"},
		} {
			assertErrorCode(t, h.run(args...), errs.CodeInvalidAccountScope)
		}
		for _, args := range [][]string{
			{"folders", "create", "Projects", "--all-accounts", "--json"},
			{"folders", "rename", "clients-id", "Accounts", "--all-accounts", "--json"},
			{"folders", "delete", "spare-id", "--confirm", "spare-id", "--all-accounts", "--json"},
		} {
			res := h.run(args...)
			assertFrameworkUsage(t, res)
			assertNoCode(t, res)
		}
	})

	t.Run("unknown capability", func(t *testing.T) {
		h := seededFoldersHarnessWithProvider(t, testkit.NewFakeProvider("fake"), "work")
		assertErrorCode(t, h.run("folders", "list", "--json"), errs.CodeUnsupportedCapability)
	})

	t.Run("mutation capability errors", func(t *testing.T) {
		h := seededFoldersHarnessWithProvider(t, testkit.NewFakeProvider("fake"), "work")
		assertErrorCode(t, h.run("folders", "create", "Projects", "--json"), errs.CodeUnsupportedCapability)

		readOnly := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", testkit.SampleFolders()), "work")
		assertErrorCode(t, readOnly.run("folders", "rename", "clients-id", "Accounts", "--json"), errs.CodeUnsupportedCapability)

		pending := seededFoldersHarnessWithProvider(t, testkit.NewFakePendingFolderMutationProvider("fake"), "work")
		assertErrorCode(t, pending.run("folders", "create", "Projects", "--json"), errs.CodeCapabilityNotImplemented)

		stale := seededFoldersHarnessWithProvider(t, testkit.NewFakeStaleFolderMutationProvider("fake"), "work")
		res := stale.run("folders", "create", "Projects", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body := errorJSON(t, res)
		if body["account"] != "work" || body["provider"] != "fake" || body["capability"] != string(provider.CapFoldersMutate) {
			t.Fatalf("stale folder manager body = %#v", body)
		}
	})

	t.Run("supported not implemented", func(t *testing.T) {
		h := seededFoldersHarnessWithProvider(t, testkit.NewFakePendingFolderProvider("fake"), "work")
		assertErrorCode(t, h.run("folders", "list", "--json"), errs.CodeCapabilityNotImplemented)
	})

	t.Run("stale implemented client", func(t *testing.T) {
		h := seededFoldersHarnessWithProvider(t, testkit.NewFakeStaleFolderProvider("fake"), "work")
		res := h.run("folders", "list", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body := errorJSON(t, res)
		if body["account"] != "work" || body["provider"] != "fake" || body["capability"] != string(provider.CapFoldersRead) {
			t.Fatalf("stale body = %#v", body)
		}
		res = h.run("folders", "view", "inbox", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body = errorJSON(t, res)
		if body["account"] != "work" || body["provider"] != "fake" || body["capability"] != string(provider.CapFoldersRead) {
			t.Fatalf("stale view body = %#v", body)
		}
	})
}

func TestFoldersListAllAccountsFailsWithoutPartialOutput(t *testing.T) {
	ok := testkit.NewFakeFolderProvider("fake", testkit.SampleFolders())
	pending := testkit.NewFakePendingFolderProvider("pending")
	h := newFakeHarness(t, ok)
	h.providers = testkit.NewProviderRegistry(ok, pending)
	accounts := []maildoconfig.Account{
		folderWorkAccount(),
		{Name: "blocked", Provider: "pending", AuthRef: "pending/blocked"},
	}
	seedRegistry(t, h.configPath, "work", accounts)
	for _, account := range accounts {
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential %s: %v", account.Name, err)
		}
	}

	res := h.run("folders", "list", "--all-accounts", "--json")
	assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("all-or-nothing failure produced partial stdout: %s", res.Stdout)
	}
	body := errorJSON(t, res)
	if !strings.Contains(body["error"].(string), `account "blocked" (pending)`) {
		t.Fatalf("failure did not report failing account/provider: %#v", body)
	}
}

func TestFoldersSecretSweep(t *testing.T) {
	h := seededFoldersHarness(t, "work")
	account := folderWorkAccount()
	secrets := []string{"folder-access-token-12345", "folder-refresh-token-12345"}
	if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	for _, args := range [][]string{
		{"folders", "list"},
		{"folders", "list", "--json"},
		{"folders", "list", "--json", "--jq", ".scope"},
		{"folders", "list", "--template", "{{.scope}}"},
		{"folders", "list", "--plain"},
		{"folders", "list", "--json", "--include-meta"},
		{"folders", "view", "inbox"},
		{"folders", "view", "inbox", "--json"},
		{"folders", "view", "inbox", "--json", "--jq", ".matched_by"},
		{"folders", "view", "inbox", "--template", "{{.matched_by}}"},
		{"folders", "view", "inbox", "--json", "--include-meta"},
		{"folders", "view", "missing", "--json"},
	} {
		res := h.run(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
	}

	mutation := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
	if err := testkit.SeedCredential(mutation.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed mutation credential: %v", err)
	}
	for _, args := range [][]string{
		{"folders", "create", "Projects"},
		{"folders", "create", "Projects", "--json"},
		{"folders", "create", "Projects", "--json", "--jq", ".operation"},
		{"folders", "create", "Projects", "--template", "{{.operation}}"},
		{"folders", "create", "Projects", "--dry-run", "--json"},
		{"folders", "create", "Projects", "--json", "--include-meta"},
		{"folders", "rename", "clients-id", "Accounts"},
		{"folders", "rename", "clients-id", "Accounts", "--json"},
		{"folders", "rename", "clients-id", "Accounts", "--json", "--jq", ".operation"},
		{"folders", "rename", "clients-id", "Accounts", "--template", "{{.operation}}"},
		{"folders", "rename", "clients-id", "Accounts", "--dry-run", "--json"},
		{"folders", "rename", "clients-id", "Accounts", "--json", "--include-meta"},
		{"folders", "delete", "spare-id", "--dry-run", "--json"},
		{"folders", "delete", "spare-id", "--confirm", "spare-id"},
		{"folders", "delete", "spare-id", "--confirm", "spare-id", "--json"},
		{"folders", "delete", "spare-id", "--confirm", "spare-id", "--json", "--jq", ".operation"},
		{"folders", "delete", "spare-id", "--confirm", "spare-id", "--template", "{{.operation}}"},
		{"folders", "delete", "spare-id", "--confirm", "spare-id", "--json", "--include-meta"},
		{"folders", "delete", "spare-id", "--json"},
	} {
		res := mutation.run(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, mutation.root, mutation.configPath)
	}
}

func TestFoldersErrorDetailsJSON(t *testing.T) {
	h := seededFoldersHarness(t, "work")
	res := h.run("folders", "view", "missing", "--json")
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("decode stderr: %v\n%s", err, res.Stderr)
	}
	if body["exit_code"] != float64(5) || body["code"] != errs.CodeFolderNotFound {
		t.Fatalf("body = %#v", body)
	}
}

func TestFoldersReadOnlyCommandsDoNotMutateHealthyCache(t *testing.T) {
	h := seededFoldersHarness(t, "work")
	before := hashTree(t, h.root)

	decodeOK(t, h.run("folders", "list", "--json"), &folderListResult{})
	decodeOK(t, h.run("folders", "view", "inbox", "--json"), &folderViewResult{})
	decodeOK(t, h.run("folders", "list", "--dry-run", "--json"), &folderListResult{})
	decodeOK(t, h.run("folders", "view", "inbox", "--dry-run", "--json"), &folderViewResult{})

	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("folder reads mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestFoldersConcurrentSubprocessReadOnlyOutputs(t *testing.T) {
	h := seededFoldersHarness(t, "work")
	before := hashTree(t, h.root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	outDir := t.TempDir()

	var cmds []*exec.Cmd
	for i := 0; i < 10; i++ {
		outPath := filepath.Join(outDir, fmt.Sprintf("list-%02d.json", i))
		cmd := exec.Command(exe, "-test.run=^TestFoldersConcurrentHelperProcess$", "--", h.configPath, outPath)
		cmd.Env = append(os.Environ(), "MAILDO_FOLDERS_HELPER_PROCESS=1")
		cmds = append(cmds, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start subprocess %d: %v", i, err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("subprocess %d failed: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		outPath := filepath.Join(outDir, fmt.Sprintf("list-%02d.json", i))
		data, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read subprocess output %d: %v", i, err)
		}
		var list folderListResult
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatalf("decode subprocess output %d: %v\n%s", i, err, data)
		}
		if len(list.Folders) != len(testkit.SampleFolders().Folders) {
			t.Fatalf("subprocess %d folders = %d, want %d", i, len(list.Folders), len(testkit.SampleFolders().Folders))
		}
		assertFoldersLabelled(t, list.Folders, "work", "fake")
	}

	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("concurrent subprocess folder reads mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestFoldersConcurrentSubprocessMutationOutputsFromReadOnlyState(t *testing.T) {
	h := seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders()), "work")
	before := hashTree(t, h.root)
	makeTreeReadOnly(t, h.root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	outDir := t.TempDir()
	operations := []string{"create", "rename", "delete"}

	type processCase struct {
		operation string
		outPath   string
		cmd       *exec.Cmd
	}
	var processes []processCase
	for _, operation := range operations {
		for i := 0; i < 4; i++ {
			outPath := filepath.Join(outDir, fmt.Sprintf("%s-%02d.json", operation, i))
			cmd := exec.Command(exe, "-test.run=^TestFoldersConcurrentHelperProcess$", "--", h.configPath, outPath)
			cmd.Env = append(os.Environ(), "MAILDO_FOLDERS_HELPER_PROCESS=1", "MAILDO_FOLDERS_HELPER_OPERATION="+operation)
			processes = append(processes, processCase{operation: operation, outPath: outPath, cmd: cmd})
			if err := cmd.Start(); err != nil {
				t.Fatalf("start %s subprocess %d: %v", operation, i, err)
			}
		}
	}
	for i, process := range processes {
		if err := process.cmd.Wait(); err != nil {
			t.Fatalf("%s subprocess %d failed: %v", process.operation, i, err)
		}
		data, err := os.ReadFile(process.outPath)
		if err != nil {
			t.Fatalf("read %s subprocess %d: %v", process.operation, i, err)
		}
		var result folderMutationResult
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("decode %s subprocess %d: %v\n%s", process.operation, i, err, data)
		}
		if !result.Applied || result.Operation != "folder_"+process.operation {
			t.Fatalf("%s subprocess %d result = %#v", process.operation, i, result)
		}
	}

	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("concurrent subprocess folder mutations changed read-only state\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestFoldersConcurrentHelperProcess(t *testing.T) {
	if os.Getenv("MAILDO_FOLDERS_HELPER_PROCESS") != "1" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(os.Args) < sep+3 {
		fmt.Fprintln(os.Stderr, "missing helper args")
		os.Exit(2)
	}
	configPath := os.Args[sep+1]
	outPath := os.Args[sep+2]
	operation := os.Getenv("MAILDO_FOLDERS_HELPER_OPERATION")
	fake := testkit.NewFakeFolderProvider("fake", testkit.SampleFolders())
	args := []string{"--config", configPath, "folders", "list", "--json"}
	if operation != "" {
		fake = testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
		switch operation {
		case "create":
			args = []string{"--config", configPath, "folders", "create", "Projects", "--json"}
		case "rename":
			args = []string{"--config", configPath, "folders", "rename", "clients-id", "Accounts", "--json"}
		case "delete":
			args = []string{"--config", configPath, "folders", "delete", "spare-id", "--confirm", "spare-id", "--json"}
		default:
			fmt.Fprintf(os.Stderr, "unsupported helper operation %q\n", operation)
			os.Exit(2)
		}
	}
	registry := testkit.NewProviderRegistry(fake)
	app := newApp(Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())})
	res := testutil.Run(app, args...)
	if res.Exit != 0 {
		fmt.Fprintf(os.Stderr, "exit=%d\nstdout=%s\nstderr=%s\n", res.Exit, res.Stdout, res.Stderr)
		os.Exit(res.Exit)
	}
	if err := os.WriteFile(outPath, []byte(res.Stdout), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

func makeTreeReadOnly(t *testing.T, root string) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		// Managed credential paths must stay private even when read-only.
		if path == filepath.Join(root, "auth") || strings.HasPrefix(path, filepath.Join(root, "auth")+string(filepath.Separator)) {
			if d.IsDir() {
				return os.Chmod(path, 0o500)
			}
			return os.Chmod(path, 0o400)
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatalf("make tree read-only: %v", err)
	}
	t.Cleanup(func() {
		for i := len(paths) - 1; i >= 0; i-- {
			info, err := os.Stat(paths[i])
			if err != nil {
				continue
			}
			mode := os.FileMode(0o600)
			if info.IsDir() {
				mode = 0o700
			}
			_ = os.Chmod(paths[i], mode)
		}
	})
}

func seededFoldersHarness(t *testing.T, active string) *cliHarness {
	t.Helper()
	return seededFoldersHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", testkit.SampleFolders()), active)
}

func seededFoldersHarnessWithProvider(t *testing.T, fake *testkit.FakeProvider, active string) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	accounts := []maildoconfig.Account{folderWorkAccount(), folderPersonalAccount()}
	seedRegistry(t, h.configPath, active, accounts)
	for _, account := range accounts {
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential %s: %v", account.Name, err)
		}
	}
	return h
}

func folderWorkAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:              "work",
		Provider:          "fake",
		Email:             "work@example.test",
		DisplayName:       "Work Account",
		ProviderAccountID: "fake-work",
		AuthRef:           "fake/work",
		DefaultFrom:       "work@example.test",
	}
}

func folderPersonalAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:     "personal",
		Provider: "fake",
		AuthRef:  "fake/personal",
	}
}

func assertFoldersLabelled(t *testing.T, folders []mail.Folder, account, providerName string) {
	t.Helper()
	if len(folders) == 0 {
		t.Fatal("no folders returned")
	}
	for i, folder := range folders {
		if folder.Account != account || folder.Provider != providerName {
			t.Fatalf("folder[%d] label = %s/%s, want %s/%s; folder=%#v", i, folder.Account, folder.Provider, account, providerName, folder)
		}
	}
}

func assertFolderCountsByAccount(t *testing.T, folders []mail.Folder, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for _, folder := range folders {
		got[folder.Account]++
	}
	for account, count := range want {
		if got[account] != count {
			t.Fatalf("folders for account %q = %d, want %d; all counts=%#v", account, got[account], count, got)
		}
	}
}

func edgeCaseFolders() []mail.Folder {
	folders := []mail.Folder{
		{ID: "localized-id", DisplayName: "Buchhaltung", Path: "Buchhaltung"},
		{ID: "control-id", DisplayName: "Tabs\tAnd\nLines", Path: "Tabs And Lines"},
		{ID: "deep-1", DisplayName: "L1", Path: "L1", ChildCount: 1},
		{ID: "deep-2", DisplayName: "L2", ParentID: "deep-1", ParentPath: "L1", Path: "L1/L2", ChildCount: 1},
		{ID: "deep-3", DisplayName: "L3", ParentID: "deep-2", ParentPath: "L1/L2", Path: "L1/L2/L3", ChildCount: 1},
		{ID: "deep-4", DisplayName: "L4", ParentID: "deep-3", ParentPath: "L1/L2/L3", Path: "L1/L2/L3/L4"},
	}
	for i := 0; i < 250; i++ {
		name := fmt.Sprintf("Bulk-%03d", i)
		folders = append(folders, mail.Folder{ID: fmt.Sprintf("bulk-%03d", i), DisplayName: name, Path: name})
	}
	return folders
}

func folderByIDInList(t *testing.T, folders []mail.Folder, id string) mail.Folder {
	t.Helper()
	for _, folder := range folders {
		if folder.ID == id {
			return folder
		}
	}
	t.Fatalf("folder id %q not found in %#v", id, folders)
	return mail.Folder{}
}

func hashTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == filepath.Join("auth", ".transaction.lock") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out = append(out, fmt.Sprintf("%s %x", rel, sum))
		return nil
	}); err != nil {
		t.Fatalf("hash tree: %v", err)
	}
	sort.Strings(out)
	return out
}
