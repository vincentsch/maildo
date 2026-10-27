package main

import (
	"encoding/json"
	"fmt"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOutlookFoldersListAndView(t *testing.T) {
	h := seededOutlookFoldersHarness(t)

	var list folderListResult
	decodeOK(t, h.runNoBrowser("folders", "list", "--account", "work", "--json"), &list)
	assertSelection(t, list.AccountSelection, "single", "work")
	if h.server.tokenCalls.Load() != 0 || h.server.mailFolderCalls.Load() == 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("unexpected auth path token=%d folders=%d device=%d browser=%d",
			h.server.tokenCalls.Load(), h.server.mailFolderCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}

	acme := folderByID(t, list.Folders, "acme-id")
	if acme.Path != "Clients/ACME" || acme.ParentPath != "Clients" || acme.ParentID != "clients-id" {
		t.Fatalf("ACME path fields = %#v", acme)
	}
	if acme.ProviderMeta == nil || acme.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph {
		t.Fatalf("provider metadata = %#v", acme.ProviderMeta)
	}
	if folderByID(t, list.Folders, "reports-2").DisplayName != "Reports" {
		t.Fatal("page 2 folder missing")
	}
	sent := folderByID(t, list.Folders, "sent-id")
	if sent.DisplayName != "Gesendete Elemente" || sent.WellKnownName != mail.WellKnownSent {
		t.Fatalf("sent well-known annotation = %#v", sent)
	}
	if folderByID(t, list.Folders, "inbox-id").WellKnownName != mail.WellKnownInbox ||
		folderByID(t, list.Folders, "drafts-id").WellKnownName != mail.WellKnownDrafts ||
		folderByID(t, list.Folders, "deleted-id").WellKnownName != mail.WellKnownDeleted ||
		folderByID(t, list.Folders, "archive-id").WellKnownName != mail.WellKnownArchive ||
		folderByID(t, list.Folders, "junk-id").WellKnownName != mail.WellKnownJunk {
		t.Fatalf("well-known annotations missing: %#v", list.Folders)
	}
	if inbox := folderByID(t, list.Folders, "inbox-id"); inbox.TotalCount != 10 || inbox.UnreadCount != 2 {
		t.Fatalf("counts = %#v", inbox)
	}

	for _, tt := range []struct {
		name      string
		ref       string
		wantID    string
		matchedBy string
	}{
		{name: "localized sent alias", ref: "sent", wantID: "sent-id", matchedBy: mail.MatchWellKnown},
		{name: "nested path", ref: "Clients/ACME", wantID: "acme-id", matchedBy: mail.MatchPath},
		{name: "nested id", ref: "acme-id", wantID: "acme-id", matchedBy: mail.MatchID},
		{name: "slash id", ref: "id/with/slash", wantID: "id/with/slash", matchedBy: mail.MatchID},
		{name: "slash display by id", ref: "opaque-ab-id", wantID: "opaque-ab-id", matchedBy: mail.MatchID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got folderViewResult
			decodeOK(t, h.runNoBrowser("folders", "view", tt.ref, "--account", "work", "--json"), &got)
			if got.Folder.ID != tt.wantID || got.MatchedBy != tt.matchedBy {
				t.Fatalf("view %q = id %q matched %q, want id %q matched %q", tt.ref, got.Folder.ID, got.MatchedBy, tt.wantID, tt.matchedBy)
			}
		})
	}

	assertErrorCodeExit(t, h.runNoBrowser("folders", "view", "missing", "--account", "work", "--json"), errs.CodeFolderNotFound, 5)
	assertErrorCodeExit(t, h.runNoBrowser("folders", "view", "Reports", "--account", "work", "--json"), errs.CodeAmbiguousFolder, 1)
	assertErrorCodeExit(t, h.runNoBrowser("folders", "view", "Clients//ACME", "--account", "work", "--json"), errs.CodeFolderNotFound, 5)
}

func TestOutlookFoldersArchiveAlias404(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.archiveAlias404.Store(true)

	var list folderListResult
	decodeOK(t, h.runNoBrowser("folders", "list", "--account", "work", "--json"), &list)
	archive := folderByID(t, list.Folders, "archive-id")
	if archive.DisplayName != "Archive" || archive.WellKnownName != "" {
		t.Fatalf("archive 404 annotation = %#v", archive)
	}
}

func TestOutlookFoldersRejectsCrossHostNextLink(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.hostileNextLink.Store(true)

	res := h.runNoBrowser("folders", "list", "--account", "work", "--json")
	if res.Exit != 2 {
		t.Fatalf("exit = %d, want 2\nstdout=%s\nstderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	assertNoCode(t, res)
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("hostile nextLink produced partial stdout: %s", res.Stdout)
	}
	if h.server.mailFolderCalls.Load() != 1 {
		t.Fatalf("mailFolderCalls = %d, want only the first page", h.server.mailFolderCalls.Load())
	}
}

func TestOutlookFoldersAuthLikeGraphFailuresRequireRepair(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			h.server.mailFolderStatus.Store(int64(status))

			res := h.runNoBrowser("folders", "list", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
			if !strings.Contains(res.Stderr, "maildo auth repair work") {
				t.Fatalf("repair guidance missing: %s", res.Stderr)
			}
		})
	}
}

func TestOutlookFolderMutations(t *testing.T) {
	t.Run("create rename delete", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)

		createResult := h.runNoBrowser("folders", "create", "Projects", "--account", "work", "--json")
		var created folderMutationResult
		decodeOK(t, createResult, &created)
		if created.ResultFolder == nil || created.ResultFolder.ID != "created-projects" || created.ResultFolder.Path != "Projects" || created.VerificationStatus != mail.FolderVerificationProviderResponse {
			t.Fatalf("create result = %#v", created)
		}
		var createdJSON map[string]any
		decodeOK(t, createResult, &createdJSON)
		if _, ok := createdJSON["total_item_count"]; ok {
			t.Fatalf("create JSON contains delete-only counts: %#v", createdJSON)
		}
		var child folderMutationResult
		decodeOK(t, h.runNoBrowser("folders", "create", "Q3", "--parent", "clients-id", "--account", "work", "--json"), &child)
		if child.ParentFolder == nil || child.ParentFolder.ID != "clients-id" || child.ResultFolder.Path != "Clients/Q3" {
			t.Fatalf("child create result = %#v", child)
		}
		var renamed folderMutationResult
		decodeOK(t, h.runNoBrowser("folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"), &renamed)
		if renamed.ResultFolder == nil || renamed.ResultFolder.ID != "clients-id" || renamed.ResultFolder.Path != "Accounts" {
			t.Fatalf("rename result = %#v", renamed)
		}
		deleteResult := h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
		var deleted folderMutationResult
		decodeOK(t, deleteResult, &deleted)
		if deleted.ResultFolder == nil || deleted.ResultFolder.ID != "zero-counts-id" || !deleted.TotalItemCountKnown || !deleted.ChildFolderCountKnown || deleted.ChildFolderCount != 0 || !deleted.ConfirmationMatched {
			t.Fatalf("delete result = %#v", deleted)
		}
		var deletedJSON map[string]any
		decodeOK(t, deleteResult, &deletedJSON)
		if total, ok := deletedJSON["total_item_count"]; !ok || total != float64(0) ||
			deletedJSON["total_item_count_known"] != true || deletedJSON["child_folder_count"] != float64(0) ||
			deletedJSON["child_folder_count_known"] != true {
			t.Fatalf("delete JSON counts = %#v", deletedJSON)
		}
		records := h.server.mailFolderRequestRecordsSnapshot()
		if !mailFolderRecordsContainMethod(records, http.MethodPost) || !mailFolderRecordsContainMethod(records, http.MethodPatch) || !mailFolderRecordsContainMethod(records, http.MethodDelete) {
			t.Fatalf("missing write methods in folder requests: %#v", records)
		}
		assertFolderRequestBody(t, firstMailFolderRecord(t, records, http.MethodPost, "/me/mailFolders"), "Projects")
		assertFolderRequestBody(t, firstMailFolderRecord(t, records, http.MethodPost, "/me/mailFolders/clients-id/childFolders"), "Q3")
		assertFolderRequestBody(t, firstMailFolderRecord(t, records, http.MethodPatch, "/me/mailFolders/clients-id"), "Accounts")
		refreshIndex := indexMailFolderRecord(records, http.MethodGet, "/me/mailFolders/zero-counts-id")
		deleteIndex := indexMailFolderRecord(records, http.MethodDelete, "/me/mailFolders/zero-counts-id")
		if refreshIndex < 0 || deleteIndex < 0 || refreshIndex > deleteIndex {
			t.Fatalf("delete refresh/write order refresh=%d delete=%d records=%#v", refreshIndex, deleteIndex, records)
		}
		deleteRecord := firstMailFolderRecord(t, records, http.MethodDelete, "/me/mailFolders/zero-counts-id")
		if len(deleteRecord.Body) != 0 || strings.Contains(deleteRecord.EscapedPath, "permanentDelete") {
			t.Fatalf("delete request must use normal folder delete with no body: %#v", deleteRecord)
		}
		if h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
			t.Fatalf("folder mutations used interactive auth token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
		}
	})

	t.Run("dry-run and confirmation do not write", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		decodeOK(t, h.runNoBrowser("folders", "create", "Dry", "--account", "work", "--dry-run", "--json"), nil)
		decodeOK(t, h.runNoBrowser("folders", "rename", "clients-id", "Dry", "--account", "work", "--dry-run", "--json"), nil)
		deletePreview := decodePreview(t, h.runNoBrowser("folders", "delete", "zero-counts-id", "--account", "work", "--dry-run", "--json"))
		body := deletePreview["body"].(map[string]any)
		if body["total_item_count_known"] != "true" || body["child_folder_count"] != "0" || body["confirmation_required"] != "true" {
			t.Fatalf("delete dry-run body = %#v", body)
		}
		assertNoFolderWrite(t, h.server.mailFolderRequestRecordsSnapshot())

		res := h.runNoBrowser("folders", "delete", "zero-counts-id", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeConfirmationRequired)
		assertNoFolderWrite(t, h.server.mailFolderRequestRecordsSnapshot())
	})

	t.Run("unknown counts are refused", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		for _, args := range [][]string{
			{"folders", "delete", "count-free-id", "--account", "work", "--dry-run", "--json"},
			{"folders", "delete", "count-free-id", "--confirm", "count-free-id", "--account", "work", "--json"},
		} {
			res := h.runNoBrowser(args...)
			assertErrorCode(t, res, errs.CodeProtectedFolder)
			if body := errorJSON(t, res); body["reason"] != "counts_unknown" {
				t.Fatalf("%v body = %#v", args, body)
			}
		}
		assertNoFolderWrite(t, h.server.mailFolderRequestRecordsSnapshot())
	})
}

func TestOutlookFolderDeleteCountFieldsAcrossOutputTransforms(t *testing.T) {
	cases := []struct {
		id, display  string
		total, child int
		totalKnown   bool
		childKnown   bool
	}{
		// Only an empty folder without subfolders can be deleted, so the
		// counts are always known zeros. Every output still shows them.
		{id: "zero-counts-id", display: "Zero Counts", totalKnown: true, childKnown: true},
	}
	for _, tt := range cases {
		t.Run(tt.id, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			preview := decodePreview(t, h.runNoBrowser("folders", "delete", tt.id, "--account", "work", "--dry-run", "--json"))
			body := preview["body"].(map[string]any)
			if body["total_item_count"] != fmt.Sprint(tt.total) || body["total_item_count_known"] != fmt.Sprint(tt.totalKnown) ||
				body["child_folder_count"] != fmt.Sprint(tt.child) || body["child_folder_count_known"] != fmt.Sprint(tt.childKnown) ||
				body["counts_source"] != mail.FolderCountsSourceRefreshed {
				t.Fatalf("preview count shape = %#v", body)
			}

			h = seededOutlookFoldersHarness(t)
			var raw map[string]any
			decodeOK(t, h.runNoBrowser("folders", "delete", tt.id, "--confirm", tt.id, "--account", "work", "--json"), &raw)
			assertDeleteCountMap(t, raw, tt.total, tt.totalKnown, tt.child, tt.childKnown)

			h = seededOutlookFoldersHarness(t)
			var envelope struct {
				Data map[string]any `json:"data"`
				Meta struct {
					Extra map[string]any `json:"extra"`
				} `json:"meta"`
			}
			decodeOK(t, h.runNoBrowser("folders", "delete", tt.id, "--confirm", tt.id, "--account", "work", "--json", "--include-meta"), &envelope)
			assertDeleteCountMap(t, envelope.Data, tt.total, tt.totalKnown, tt.child, tt.childKnown)
			assertDeleteCountMap(t, envelope.Meta.Extra, tt.total, tt.totalKnown, tt.child, tt.childKnown)

			h = seededOutlookFoldersHarness(t)
			jq := h.runNoBrowser("folders", "delete", tt.id, "--confirm", tt.id, "--account", "work", "--json", "--jq", "[.total_item_count,.total_item_count_known,.child_folder_count,.child_folder_count_known]")
			wantJQ := fmt.Sprintf("[%d,%t,%d,%t]", tt.total, tt.totalKnown, tt.child, tt.childKnown)
			if strings.TrimSpace(jq.Stdout) != wantJQ || jq.Stderr != "" {
				t.Fatalf("jq = %q stderr=%q want %q", jq.Stdout, jq.Stderr, wantJQ)
			}

			h = seededOutlookFoldersHarness(t)
			template := h.runNoBrowser("folders", "delete", tt.id, "--confirm", tt.id, "--account", "work", "--template", "{{.total_item_count}}/{{.total_item_count_known}}/{{.child_folder_count}}/{{.child_folder_count_known}}")
			wantTemplate := fmt.Sprintf("%d/%t/%d/%t", tt.total, tt.totalKnown, tt.child, tt.childKnown)
			if strings.TrimSpace(template.Stdout) != wantTemplate || template.Stderr != "" {
				t.Fatalf("template = %q stderr=%q want %q", template.Stdout, template.Stderr, wantTemplate)
			}

			h = seededOutlookFoldersHarness(t)
			human := h.runNoBrowser("folders", "delete", tt.id, "--confirm", tt.id, "--account", "work")
			wantHuman := fmt.Sprintf("Deleted the folder %q in work.\n", tt.display)
			if human.Stdout != wantHuman || human.Stderr != "" || human.Exit != 0 {
				t.Fatalf("human exit=%d stdout=%q stderr=%q want=%q", human.Exit, human.Stdout, human.Stderr, wantHuman)
			}
		})
	}
}

func TestOutlookFolderMutationsUseSilentAccountCredentials(t *testing.T) {
	operations := []struct {
		name, method string
		args         []string
	}{
		{name: "create", method: http.MethodPost, args: []string{"folders", "create", "Projects", "--account", "work", "--json"}},
		{name: "rename", method: http.MethodPatch, args: []string{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"}},
		{name: "delete", method: http.MethodDelete, args: []string{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json"}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
				c.ExpiresAt = h.now.Add(-time.Minute)
			}); err != nil {
				t.Fatal(err)
			}
			res := h.runNoBrowser(operation.args...)
			decodeOK(t, res, nil)
			if h.server.tokenCalls.Load() != 1 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("silent refresh token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
			if got := countMailFolderMethod(h.server.mailFolderRequestRecordsSnapshot(), operation.method); got != 1 {
				t.Fatalf("%s attempts = %d, want 1", operation.method, got)
			}
			cache, err := loadCredentialCache(h, "outlook", "work")
			if err != nil {
				t.Fatal(err)
			}
			if cache.AccessToken != "cli-refreshed-access-secret-12345" || cache.RefreshToken != "cli-rotated-refresh-secret-12345" || !cache.ExpiresAt.After(h.now) {
				t.Fatalf("rotated cache was not persisted: %#v", cache)
			}
		})
	}
}

func TestOutlookFolderMutationsRejectBrokenCredentialsBeforeProviderCalls(t *testing.T) {
	credentialCases := []struct {
		name      string
		seed      bool
		mutate    func(*testkit.TokenCache)
		configure func(*outlookHarness)
	}{
		{name: "missing cache"},
		{name: "missing refresh", seed: true, mutate: func(c *testkit.TokenCache) { c.RefreshToken = ""; c.ExpiresAt = time.Unix(1, 0) }},
		{name: "under scoped", seed: true, mutate: func(c *testkit.TokenCache) { c.Scopes = []string{"User.Read"} }},
		{name: "revoked refresh", seed: true, mutate: func(c *testkit.TokenCache) { c.ExpiresAt = time.Unix(1, 0) }, configure: func(h *outlookHarness) { h.server.invalidGrant.Store(true) }},
	}
	operations := []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"folders", "create", "Projects", "--account", "work", "--json"}},
		{name: "rename", args: []string{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"}},
		{name: "delete", args: []string{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json"}},
	}
	for _, credentials := range credentialCases {
		for _, operation := range operations {
			t.Run(credentials.name+"/"+operation.name, func(t *testing.T) {
				h := newOutlookHarness(t)
				account := outlookFoldersAccount()
				if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
					t.Fatal(err)
				}
				if credentials.seed {
					if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
						c.VerifiedEmail = account.Email
						c.ProviderAccountID = account.ProviderAccountID
						credentials.mutate(c)
					}); err != nil {
						t.Fatal(err)
					}
				}
				if credentials.configure != nil {
					credentials.configure(h)
				}
				res := h.runNoBrowser(operation.args...)
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if res.Stdout != "" || h.server.mailFolderCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
					t.Fatalf("provider/UI work occurred stdout=%q folders=%d device=%d browser=%d", res.Stdout, h.server.mailFolderCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
				}
				if !strings.Contains(res.Stderr, "maildo auth repair work") {
					t.Fatalf("repair guidance missing: %s", res.Stderr)
				}
			})
		}
	}
}

func TestOutlookFolderMutationLocalErrorsDoNotRefreshCredentials(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "invalid name", args: []string{"folders", "create", "Bad/Name", "--account", "work", "--json"}},
		{name: "invalid renderer", args: []string{"folders", "create", "Projects", "--account", "work", "--json", "--jq", ".broken["}},
		{name: "invalid scope", args: []string{"folders", "create", "Projects", "--all-accounts", "--json"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
				c.ExpiresAt = h.now.Add(-time.Minute)
			}); err != nil {
				t.Fatal(err)
			}
			res := h.runNoBrowser(tt.args...)
			if res.Exit == 0 || res.Stdout != "" {
				t.Fatalf("local error exit=%d stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
			}
			if h.server.tokenCalls.Load() != 0 || h.server.mailFolderCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("local error reached auth/provider token=%d folders=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.mailFolderCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
		})
	}
}

func TestOutlookFolderMutationAuthorizationFailuresStopWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		cases := []struct {
			name, writeMethod string
			configure         func(*outlookHarness)
			args              []string
			wantWrites        int
		}{
			{name: "root create write", writeMethod: http.MethodPost, configure: func(h *outlookHarness) { h.server.mailFolderWriteStatus.Store(int64(status)) }, args: []string{"folders", "create", "Projects", "--account", "work", "--json"}, wantWrites: 1},
			{name: "rename protection lookup", writeMethod: http.MethodPatch, configure: func(h *outlookHarness) { h.server.setMailFolderStatus("outbox", status) }, args: []string{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"}},
			{name: "delete count refresh", writeMethod: http.MethodDelete, configure: func(h *outlookHarness) { h.server.setMailFolderStatus("zero-counts-id", status) }, args: []string{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json"}},
			{name: "rename write", writeMethod: http.MethodPatch, configure: func(h *outlookHarness) { h.server.mailFolderWriteStatus.Store(int64(status)) }, args: []string{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"}, wantWrites: 1},
			{name: "delete write", writeMethod: http.MethodDelete, configure: func(h *outlookHarness) { h.server.mailFolderWriteStatus.Store(int64(status)) }, args: []string{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json"}, wantWrites: 1},
		}
		for _, tt := range cases {
			t.Run(http.StatusText(status)+"/"+tt.name, func(t *testing.T) {
				h := seededOutlookFoldersHarness(t)
				tt.configure(h)
				res := h.runNoBrowser(tt.args...)
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if res.Stdout != "" || h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
					t.Fatalf("auth failure retried or used UI stdout=%q token=%d device=%d browser=%d", res.Stdout, h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
				}
				if got := countMailFolderMethod(h.server.mailFolderRequestRecordsSnapshot(), tt.writeMethod); got != tt.wantWrites {
					t.Fatalf("%s attempts = %d, want %d", tt.writeMethod, got, tt.wantWrites)
				}
			})
		}
	}
}

func TestOutlookFolderMutationFailures(t *testing.T) {
	t.Run("duplicate names map to invalid folder name", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.mailFolderWriteStatus.Store(http.StatusConflict)
		assertErrorCode(t, h.runNoBrowser("folders", "create", "Projects", "--account", "work", "--json"), errs.CodeInvalidFolderName)

		h = seededOutlookFoldersHarness(t)
		h.server.mailFolderWriteStatus.Store(http.StatusConflict)
		res := h.runNoBrowser("folders", "create", "Q3", "--parent", "clients-id", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeInvalidFolderName)
		details := errorJSON(t, res)
		if details["account"] != "work" || details["provider"] != "outlook" || details["display_name"] != "Q3" ||
			details["parent_folder_id"] != "clients-id" || details["parent_path"] != "Clients" ||
			details["error"] != "folder name is already taken in that parent" {
			t.Fatalf("duplicate child folder details = %#v", details)
		}
		if strings.Contains(res.Stdout+res.Stderr, "mail folder write fixture status") {
			t.Fatalf("duplicate child folder error exposed provider message: stdout=%s stderr=%s", res.Stdout, res.Stderr)
		}

		h = seededOutlookFoldersHarness(t)
		h.server.mailFolderWriteStatus.Store(http.StatusConflict)
		assertErrorCode(t, h.runNoBrowser("folders", "rename", "clients-id", "Projects", "--account", "work", "--json"), errs.CodeInvalidFolderName)
	})

	t.Run("provider validation maps from graph error code", func(t *testing.T) {
		for _, tt := range []struct {
			name      string
			graphCode string
			wantCode  bool
		}{
			{name: "invalid property", graphCode: "ErrorInvalidPropertyValue", wantCode: true},
			{name: "invalid parameter", graphCode: "ErrorInvalidParameter", wantCode: true},
			{name: "unrelated code", graphCode: "ErrorFixture", wantCode: false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h := seededOutlookFoldersHarness(t)
				h.server.mailFolderWriteStatus.Store(http.StatusBadRequest)
				h.server.mailFolderWriteCode.Store(tt.graphCode)
				res := h.runNoBrowser("folders", "create", "Projects", "--account", "work", "--json")
				if tt.wantCode {
					assertErrorCode(t, res, errs.CodeInvalidFolderName)
					return
				}
				if res.Exit != 2 {
					t.Fatalf("unrecognized graph code exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
				}
				assertNoCode(t, res)
			})
		}
	})

	t.Run("malformed success and missing id fail broadly", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.malformedMessages.Store(true)
		res := h.runNoBrowser("folders", "create", "Projects", "--account", "work", "--json")
		if res.Exit != 2 {
			t.Fatalf("malformed create exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)

		h = seededOutlookFoldersHarness(t)
		h.server.missingMutationID.Store(true)
		res = h.runNoBrowser("folders", "rename", "clients-id", "Accounts", "--account", "work", "--json")
		if res.Exit != 2 {
			t.Fatalf("missing id rename exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
	})

	t.Run("create and rename write not found failures", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusGone} {
			h := seededOutlookFoldersHarness(t)
			h.server.mailFolderWriteStatus.Store(int64(status))
			res := h.runNoBrowser("folders", "create", "Q3", "--parent", "clients-id", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)

			h = seededOutlookFoldersHarness(t)
			h.server.mailFolderWriteStatus.Store(int64(status))
			res = h.runNoBrowser("folders", "rename", "clients-id", "Accounts", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)
		}
	})

	t.Run("refresh 404 happens before confirmation write", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusGone} {
			h := seededOutlookFoldersHarness(t)
			h.server.setMailFolderStatus("zero-counts-id", status)
			res := h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)
			if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
				t.Fatalf("DELETE sent after refresh status %d", status)
			}
		}
	})

	t.Run("mismatched refresh identity stops before delete", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.mismatchedFolderRefreshID.Store(true)
		res := h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
		if res.Exit != 2 || res.Stdout != "" {
			t.Fatalf("mismatched refresh exit=%d stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
		if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
			t.Fatal("DELETE sent after mismatched refreshed identity")
		}
	})

	t.Run("provider-specific protection and fail closed protection map", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		res := h.runNoBrowser("folders", "delete", "Outbox", "--confirm", "outbox-id", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeProtectedFolder)
		body := errorJSON(t, res)
		if body["reason"] != "provider_managed_folder" {
			t.Fatalf("delete protection reason = %#v", body)
		}
		if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
			t.Fatal("DELETE sent for provider-protected Outbox")
		}

		h = seededOutlookFoldersHarness(t)
		res = h.runNoBrowser("folders", "rename", "Outbox", "Queue", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeProtectedFolder)
		body = errorJSON(t, res)
		if body["reason"] != "provider_managed_folder" {
			t.Fatalf("rename protection reason = %#v", body)
		}
		if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodPatch) {
			t.Fatal("PATCH sent for provider-protected Outbox")
		}

		h = seededOutlookFoldersHarness(t)
		h.server.setMailFolderStatus("outbox", http.StatusInternalServerError)
		res = h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
		if res.Exit != 2 {
			t.Fatalf("protection failure exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
		if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
			t.Fatal("DELETE sent after protection-map failure")
		}

		h = seededOutlookFoldersHarness(t)
		h.server.setMailFolderStatus("outbox", http.StatusInternalServerError)
		res = h.runNoBrowser("folders", "rename", "clients-id", "Accounts", "--account", "work", "--json")
		if res.Exit != 2 {
			t.Fatalf("rename protection failure exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
		if mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodPatch) {
			t.Fatal("PATCH sent after protection-map failure")
		}
	})

	t.Run("ids containing slash are escaped", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		decodeOK(t, h.runNoBrowser("folders", "rename", "id/with/slash", "Slash Renamed", "--account", "work", "--json"), &folderMutationResult{})
		if !mailFolderRecordsContainEscapedPath(h.server.mailFolderRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/id%2Fwith%2Fslash") {
			t.Fatalf("escaped slash PATCH missing: %#v", h.server.mailFolderRequestRecordsSnapshot())
		}
	})

	t.Run("delete write failures map without success", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusGone} {
			h := seededOutlookFoldersHarness(t)
			h.server.mailFolderWriteStatus.Store(int64(status))
			res := h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)
			if !mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
				t.Fatalf("DELETE not sent for write status %d", status)
			}
		}
		h := seededOutlookFoldersHarness(t)
		h.server.mailFolderWriteStatus.Store(http.StatusInternalServerError)
		res := h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
		if res.Exit != 2 {
			t.Fatalf("delete write status %d exit=%d stdout=%s stderr=%s", http.StatusInternalServerError, res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
		if !mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
			t.Fatalf("DELETE not sent for write status %d", http.StatusInternalServerError)
		}

		h = seededOutlookFoldersHarness(t)
		h.server.mailFolderWriteStatus.Store(http.StatusTooManyRequests)
		res = h.runNoBrowser("folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json")
		if res.Exit != 6 {
			t.Fatalf("delete write status %d exit=%d stdout=%s stderr=%s", http.StatusTooManyRequests, res.Exit, res.Stdout, res.Stderr)
		}
		assertNoCode(t, res)
		if !mailFolderRecordsContainMethod(h.server.mailFolderRequestRecordsSnapshot(), http.MethodDelete) {
			t.Fatalf("DELETE not sent for write status %d", http.StatusTooManyRequests)
		}
	})
}

func TestOutlookFoldersSecretSweep(t *testing.T) {
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	secrets := []string{"outlook-folder-access-token-12345", "outlook-folder-refresh-token-12345"}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.AccessToken = secrets[0]
		c.RefreshToken = secrets[1]
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	for _, args := range [][]string{
		{"folders", "list", "--account", "work"},
		{"folders", "list", "--account", "work", "--json"},
		{"folders", "list", "--account", "work", "--json", "--jq", ".scope"},
		{"folders", "list", "--account", "work", "--template", "{{.scope}}"},
		{"folders", "list", "--account", "work", "--plain"},
		{"folders", "list", "--account", "work", "--json", "--include-meta"},
		{"folders", "view", "inbox", "--account", "work"},
		{"folders", "view", "inbox", "--account", "work", "--json"},
		{"folders", "view", "inbox", "--account", "work", "--json", "--jq", ".matched_by"},
		{"folders", "view", "inbox", "--account", "work", "--template", "{{.matched_by}}"},
		{"folders", "view", "inbox", "--account", "work", "--json", "--include-meta"},
		{"folders", "view", "missing", "--account", "work", "--json"},
		{"folders", "create", "Projects", "--account", "work"},
		{"folders", "create", "Projects", "--account", "work", "--json"},
		{"folders", "create", "Projects", "--account", "work", "--json", "--jq", ".operation"},
		{"folders", "create", "Projects", "--account", "work", "--template", "{{.operation}}"},
		{"folders", "create", "Projects", "--account", "work", "--dry-run", "--json"},
		{"folders", "create", "Projects", "--account", "work", "--json", "--include-meta"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json", "--jq", ".operation"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--template", "{{.operation}}"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--dry-run", "--json"},
		{"folders", "rename", "clients-id", "Accounts", "--account", "work", "--json", "--include-meta"},
		{"folders", "delete", "zero-counts-id", "--account", "work", "--dry-run", "--json"},
		{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work"},
		{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json"},
		{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json", "--jq", ".operation"},
		{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--template", "{{.operation}}"},
		{"folders", "delete", "zero-counts-id", "--confirm", "zero-counts-id", "--account", "work", "--json", "--include-meta"},
		{"folders", "delete", "zero-counts-id", "--account", "work", "--json"},
	} {
		res := h.runNoBrowser(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
		if strings.Contains(res.Stdout+res.Stderr, "Authorization") {
			t.Fatalf("output leaked header vocabulary: stdout=%s stderr=%s", res.Stdout, res.Stderr)
		}
	}
	if h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("secret sweep used interactive/refresh path token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
}

func mailFolderRecordsContainMethod(records []mailFolderRequest, method string) bool {
	for _, record := range records {
		if record.Method == method {
			return true
		}
	}
	return false
}

func mailFolderRecordsContainEscapedPath(records []mailFolderRequest, method, escapedPath string) bool {
	for _, record := range records {
		if record.Method == method && record.EscapedPath == escapedPath {
			return true
		}
	}
	return false
}

func firstMailFolderRecord(t *testing.T, records []mailFolderRequest, method, escapedPath string) mailFolderRequest {
	t.Helper()
	for _, record := range records {
		if record.Method == method && record.EscapedPath == escapedPath {
			return record
		}
	}
	t.Fatalf("missing %s %s in folder records: %#v", method, escapedPath, records)
	return mailFolderRequest{}
}

func indexMailFolderRecord(records []mailFolderRequest, method, escapedPath string) int {
	for i, record := range records {
		if record.Method == method && record.EscapedPath == escapedPath {
			return i
		}
	}
	return -1
}

func assertFolderRequestBody(t *testing.T, record mailFolderRequest, displayName string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(record.Body, &body); err != nil {
		t.Fatalf("decode folder request body for %s %s: %v", record.Method, record.EscapedPath, err)
	}
	if len(body) != 1 || body["displayName"] != displayName {
		t.Fatalf("folder request body for %s %s = %#v, want displayName %q", record.Method, record.EscapedPath, body, displayName)
	}
}

func assertNoFolderWrite(t *testing.T, records []mailFolderRequest) {
	t.Helper()
	for _, record := range records {
		if record.Method != http.MethodGet {
			t.Fatalf("unexpected folder write request: %#v", records)
		}
	}
}

func countMailFolderMethod(records []mailFolderRequest, method string) int {
	count := 0
	for _, record := range records {
		if record.Method == method {
			count++
		}
	}
	return count
}

func assertDeleteCountMap(t *testing.T, body map[string]any, total int, totalKnown bool, child int, childKnown bool) {
	t.Helper()
	if body["total_item_count"] != float64(total) || body["total_item_count_known"] != totalKnown ||
		body["child_folder_count"] != float64(child) || body["child_folder_count_known"] != childKnown {
		t.Fatalf("delete count map = %#v", body)
	}
}

func seededOutlookFoldersHarness(t *testing.T) *outlookHarness {
	t.Helper()
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	return h
}

func outlookFoldersAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:              "work",
		Provider:          "outlook",
		Email:             "work@example.test",
		DisplayName:       "Work",
		ProviderAccountID: "outlook-work",
		AuthRef:           "outlook/work",
		DefaultFrom:       "work@example.test",
	}
}

func folderByID(t *testing.T, folders []mail.Folder, id string) mail.Folder {
	t.Helper()
	for _, folder := range folders {
		if folder.ID == id {
			return folder
		}
	}
	t.Fatalf("folder id %q not found in %#v", id, folders)
	return mail.Folder{}
}
