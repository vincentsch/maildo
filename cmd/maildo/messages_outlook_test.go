package main

import (
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestOutlookMessagesListAndSearchTranslation(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var list listMessagesResult
	decodeOK(t, h.runNoBrowser("list", "--account", "work", "--limit", "1", "--json"), &list)
	if h.server.tokenCalls.Load() != 0 || h.server.messageCalls.Load() != 1 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("unexpected auth/message path token=%d messages=%d device=%d browser=%d",
			h.server.tokenCalls.Load(), h.server.messageCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
	req := h.server.lastMessageRequest()
	if req.Path != "/me/mailFolders/inbox-id/messages" || req.Query.Get("$filter") != "" || req.Query.Get("$orderby") != "receivedDateTime desc" {
		t.Fatalf("default list request = %#v", req)
	}
	if list.Messages[0].Folder.ID != "inbox-id" || list.Messages[0].Folder.DisplayName != "Inbox" || list.Messages[0].InternetMessageID != "<list-new@example.test>" {
		t.Fatalf("list mapping = %#v", list.Messages[0])
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("list", "Clients/ACME", "--account", "work", "--limit", "1", "--json"), &list)
	req = h.server.lastMessageRequest()
	if req.Path != "/me/mailFolders/acme-id/messages" {
		t.Fatalf("custom folder request path = %s", req.Path)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("list", "inbox", "--account", "work", "--unread", "--limit", "1", "--json"), &list)
	req = h.server.lastMessageRequest()
	if got, want := req.Query.Get("$filter"), "receivedDateTime ge 1900-01-01T00:00:00Z and isRead eq false"; got != want {
		t.Fatalf("unread filter = %q, want %q", got, want)
	}
	if h.server.messageCalls.Load() != 1 {
		t.Fatalf("unread list drained beyond limit: calls=%d", h.server.messageCalls.Load())
	}

	h.server.resetMessageRequests()
	var filterSearch searchResult
	decodeOK(t, h.runNoBrowser("search", "has:attachment", "--account", "work", "--limit", "1", "--json"), &filterSearch)
	req = h.server.lastMessageRequest()
	if req.Path != "/me/messages" || req.Query.Get("$search") != "" {
		t.Fatalf("filter search request = %#v", req)
	}
	if got, want := req.Query.Get("$filter"), "receivedDateTime ge 1900-01-01T00:00:00Z and hasAttachments eq true"; got != want {
		t.Fatalf("attachment filter = %q, want %q", got, want)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("search", "after:2026-06-01", "before:2026-07-01", "--account", "work", "--limit", "1", "--json"), &filterSearch)
	req = h.server.lastMessageRequest()
	if got, want := req.Query.Get("$filter"), "receivedDateTime ge 2026-06-01T00:00:00Z and receivedDateTime lt 2026-07-01T00:00:00Z"; got != want {
		t.Fatalf("date filter = %q, want %q", got, want)
	}
}

func TestOutlookMessagesSearchKQLEncodingSortAndMapping(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var folderSearch searchResult
	decodeOK(t, h.runNoBrowser("search", "--folder", "Clients/ACME", `subject:"a&b c"`, "--account", "work", "--limit", "2", "--json"), &folderSearch)
	req := h.server.firstMessageRequest()
	if req.Path != "/me/mailFolders/acme-id/messages" {
		t.Fatalf("folder search path = %s", req.Path)
	}
	if got, want := req.Query.Get("$search"), `"subject:\"a&b c\""`; got != want {
		t.Fatalf("decoded search = %q, want %q", got, want)
	}
	for _, row := range folderSearch.Messages {
		if row.Folder.ID != "acme-id" || row.Folder.Path != "Clients/ACME" {
			t.Fatalf("folder-scoped row folder = %#v", row.Folder)
		}
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("search", "--folder", "id/with/slash", "invoice", "--account", "work", "--limit", "1", "--json"), &folderSearch)
	req = h.server.firstMessageRequest()
	if req.EscapedPath != "/me/mailFolders/id%2Fwith%2Fslash/messages" {
		t.Fatalf("escaped slash-id folder path = %s", req.EscapedPath)
	}

	h.server.resetMessageRequests()
	var search searchResult
	decodeOK(t, h.runNoBrowser("search", "invoice", "--account", "work", "--limit", "2", "--json"), &search)
	if got, want := messageIDs(search.Messages), []string{"search-new", "search-mid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("search ids = %#v, want %#v", got, want)
	}
	if h.server.messageCalls.Load() != 2 {
		t.Fatalf("search did not drain pages to prove completeness: calls=%d", h.server.messageCalls.Load())
	}
	first := search.Messages[0]
	// Unscoped rows name their folder, so tables and previews can say where
	// each message lives.
	if first.Folder.ID != "inbox-id" || first.Folder.DisplayName != "Inbox" || first.Folder.WellKnownName != mail.WellKnownInbox || first.ProviderMeta == nil || first.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph ||
		first.From.Address != "search-new@example.test" || first.ToCount != 1 || first.ReceivedAt == nil || first.HasAttachments != true {
		t.Fatalf("unscoped search mapping = %#v", first)
	}

	h.server.resetMessageRequests()
	var filtered searchResult
	decodeOK(t, h.runNoBrowser("search", "invoice", "has:attachment", "after:2026-06-28", "--account", "work", "--json"), &filtered)
	req = h.server.firstMessageRequest()
	if req.Query.Get("$search") == "" || req.Query.Get("$filter") != "" || h.server.messageCalls.Load() != 2 {
		t.Fatalf("search-style post-filter request/calls = %#v calls=%d", req, h.server.messageCalls.Load())
	}
	if got, want := messageIDs(filtered.Messages), []string{"search-new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-filtered search ids = %#v, want %#v", got, want)
	}

	var mapped listMessagesResult
	decodeOK(t, h.runNoBrowser("list", "--account", "work", "--limit", "3", "--json"), &mapped)
	missing := mapped.Messages[2]
	if missing.ID != "list-missing" || missing.From.Address != "" || missing.ReceivedAt != nil || missing.SentAt != nil {
		t.Fatalf("missing optional mapping = %#v", missing)
	}
}

func TestOutlookMessagesFailures(t *testing.T) {
	t.Run("search cap hit", func(t *testing.T) {
		// Outlook stops a search at its cap. Reading the results still works,
		// with a warning that older matches may be missing.
		h := seededOutlookMessagesHarnessWithCap(t, 2)
		h.server.searchCapHit.Store(true)
		var capped searchResult
		decodeOK(t, h.runNoBrowser("search", "invoice", "--account", "work", "--json"), &capped)
		if len(capped.Messages) != 2 || len(capped.Warnings) != 1 || !strings.Contains(capped.Warnings[0], "at most 2 search results") {
			t.Fatalf("capped search = %d rows, warnings %v", len(capped.Messages), capped.Warnings)
		}
		human := h.runNoBrowser("search", "invoice", "--account", "work")
		if human.Exit != 0 || !strings.Contains(human.Stderr, "Warning: Outlook returns at most 2 search results") {
			t.Fatalf("human search at the cap: exit %d stderr %s", human.Exit, human.Stderr)
		}
	})

	t.Run("hostile message next link", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.hostileMessageLink.Store(true)
		res := h.runNoBrowser("list", "--account", "work", "--json")
		if res.Exit == 0 {
			t.Fatal("hostile message nextLink succeeded")
		}
		if strings.TrimSpace(res.Stdout) != "" {
			t.Fatalf("hostile nextLink produced partial stdout: %s", res.Stdout)
		}
		if h.server.messageCalls.Load() != 1 {
			t.Fatalf("hostile nextLink should not be requested from fixture server again: calls=%d", h.server.messageCalls.Load())
		}
		if strings.Contains(res.Stdout+res.Stderr, "example.invalid") || strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) {
			t.Fatalf("hostile nextLink leaked request details: stdout=%s stderr=%s", res.Stdout, res.Stderr)
		}
	})

	t.Run("inefficient filter", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.inefficientFilter.Store(true)
		res := h.runNoBrowser("search", "has:attachment", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeUnsupportedQueryOperator)
	})

	t.Run("unsafe kql before request", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		res := h.runNoBrowser("search", `bad\term`, "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeUnsupportedQueryOperator)
		if h.server.messageCalls.Load() != 0 {
			t.Fatalf("unsafe query sent message request: %d", h.server.messageCalls.Load())
		}
	})

	t.Run("message endpoint auth repair", func(t *testing.T) {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				h.server.messageStatus.Store(int64(status))
				res := h.runNoBrowser("list", "--account", "work", "--json")
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if !strings.Contains(res.Stderr, "maildo auth repair work") {
					t.Fatalf("repair guidance missing: %s", res.Stderr)
				}
				if strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("auth failure produced partial stdout: %s", res.Stdout)
				}
			})
		}
	})

	t.Run("malformed and oversized bodies", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			set  func(*cliOutlookServer)
		}{
			{name: "malformed json", set: func(s *cliOutlookServer) { s.malformedMessages.Store(true) }},
			{name: "oversized truncated json", set: func(s *cliOutlookServer) { s.oversizedMessages.Store(true) }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				tt.set(h.server)
				res := h.runNoBrowser("list", "--account", "work", "--json")
				if res.Exit == 0 {
					t.Fatalf("%s succeeded", tt.name)
				}
				if strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("%s produced partial stdout: %s", tt.name, res.Stdout)
				}
				if strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) || strings.Contains(res.Stdout+res.Stderr, strings.Repeat("{", 128)) {
					t.Fatalf("%s leaked provider response details: stdout=%s stderr=%s", tt.name, res.Stdout, res.Stderr)
				}
			})
		}
	})
}

func TestOutlookMessagesSecretSweep(t *testing.T) {
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	secrets := []string{"outlook-message-access-token-12345", "outlook-message-refresh-token-12345"}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.AccessToken = secrets[0]
		c.RefreshToken = secrets[1]
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	for _, args := range [][]string{
		{"search", "invoice", "--account", "work"},
		{"search", "invoice", "--account", "work", "--json"},
		{"search", "invoice", "--account", "work", "--json", "--jq", ".scope"},
		{"search", "invoice", "--account", "work", "--template", "{{.scope}}"},
		{"search", "invoice", "--account", "work", "--plain"},
		{"search", "invoice", "--account", "work", "--json", "--include-meta"},
		{"list", "--account", "work"},
		{"list", "--account", "work", "--json"},
		{"list", "--account", "work", "--json", "--jq", ".scope"},
		{"list", "--account", "work", "--template", "{{.scope}}"},
		{"list", "--account", "work", "--plain"},
		{"list", "--account", "work", "--json", "--include-meta"},
	} {
		res := h.runNoBrowser(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
		if strings.Contains(res.Stdout+res.Stderr, "Authorization") || strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) {
			t.Fatalf("output leaked request details: stdout=%s stderr=%s", res.Stdout, res.Stderr)
		}
	}
	if h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("secret sweep used interactive/refresh path token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
}

func seededOutlookMessagesHarness(t *testing.T) *outlookHarness {
	t.Helper()
	return seededOutlookMessagesHarnessWithCap(t, 0)
}

func seededOutlookMessagesHarnessWithCap(t *testing.T, cap int) *outlookHarness {
	t.Helper()
	h := newOutlookHarnessWithSearchCap(t, cap)
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
