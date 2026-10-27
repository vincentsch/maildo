package main

import (
	"encoding/base64"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestOutlookReadMessageMappingBodiesAndAttachments(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var read readResult
	decodeOK(t, h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json"), &read)
	msg := read.Message
	if msg.Account != "work" || msg.Provider != "outlook" || msg.ID != "read-id" || msg.ThreadID != "thread-read-id" ||
		msg.Folder.ID != "inbox-id" || msg.From.Address != "read-id@example.test" || msg.Sender == nil ||
		len(msg.ReplyTo) != 1 || len(msg.To) != 1 || len(msg.BCC) != 1 || len(msg.Headers) != 2 ||
		msg.CreatedAt == nil || msg.LastModifiedAt == nil || msg.IsRead || !sameStrings(msg.Categories, []string{"Graph", "Fixture"}) ||
		!msg.HasAttachments || len(msg.Attachments) != 2 || msg.Attachments[1].ContentID != "cid-inline" ||
		msg.Body.Mode != mail.BodyModeReadable || msg.Body.Content != "Text body for read-id" ||
		msg.ProviderMeta == nil || msg.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph {
		t.Fatalf("read mapping = %#v", msg)
	}
	if h.server.tokenCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("read used interactive/refresh path token=%d browser=%d", h.server.tokenCalls.Load(), h.browserCalls.Load())
	}
	req := h.server.firstMessageRequest()
	if req.Path != "/me/messages/read-id" || req.Prefer != `outlook.body-content-type="text"` || !strings.Contains(req.Query.Get("$select"), "internetMessageHeaders") {
		t.Fatalf("first read request = %#v", req)
	}

	h.server.resetMessageRequests()
	read = readResult{}
	decodeOK(t, h.runNoBrowser("read", "--id", "no-attachments", "--account", "work", "--json"), &read)
	if read.Message.HasAttachments || len(read.Message.Attachments) != 0 || h.server.messageCalls.Load() != 1 {
		t.Fatalf("no-attachments result=%#v calls=%d", read.Message, h.server.messageCalls.Load())
	}

	h.server.resetMessageRequests()
	read = readResult{}
	decodeOK(t, h.runNoBrowser("read", "--id", "html-fallback", "--account", "work", "--json"), &read)
	requests := h.server.messageRequestsSnapshot()
	if len(requests) < 2 || requests[0].Prefer != `outlook.body-content-type="text"` || requests[1].Prefer != `outlook.body-content-type="html"` {
		t.Fatalf("fallback request order = %#v", requests)
	}
	if read.Message.Body.Content != "HTML body for html-fallback" || read.Message.Body.ContentType != "html" {
		t.Fatalf("fallback body = %#v", read.Message.Body)
	}

	h.server.resetMessageRequests()
	read = readResult{}
	decodeOK(t, h.runNoBrowser("read", "--id", "html-fallback", "--body-mode", "text", "--account", "work", "--json"), &read)
	requests = h.server.messageRequestsSnapshot()
	if len(requests) < 2 || requests[0].Prefer != `outlook.body-content-type="text"` || requests[1].Prefer != `outlook.body-content-type="html"` || read.Message.Body.Mode != mail.BodyModeText {
		t.Fatalf("text fallback request/body = %#v %#v", requests, read.Message.Body)
	}

	h.server.resetMessageRequests()
	read = readResult{}
	decodeOK(t, h.runNoBrowser("read", "--id", "read-id", "--body-mode", "html", "--account", "work", "--json"), &read)
	requests = h.server.messageRequestsSnapshot()
	if requests[0].Prefer != `outlook.body-content-type="html"` || !strings.Contains(read.Message.Body.Content, "<p>HTML") {
		t.Fatalf("html mode request/body = %#v %#v", requests[0], read.Message.Body)
	}
}

func TestOutlookReadRawModesAndFailures(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var read readResult
	decodeOK(t, h.runNoBrowser("read", "--id", "read-id", "--body-mode", "raw", "--account", "work", "--json"), &read)
	if read.Message.Body.Mode != mail.BodyModeRaw || read.Message.Body.Encoding != "utf8" || !strings.Contains(read.Message.Body.Content, "Raw body for read-id") {
		t.Fatalf("raw utf8 body = %#v", read.Message.Body)
	}
	requests := h.server.messageRequestsSnapshot()
	if len(requests) < 2 || requests[0].Prefer != "" || strings.Contains(","+requests[0].Query.Get("$select")+",", ",body,") || requests[1].Path != "/me/messages/read-id/$value" {
		t.Fatalf("raw request order = %#v", requests)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("read", "--id", "raw-binary", "--body-mode", "raw", "--account", "work", "--json"), &read)
	if read.Message.Body.Encoding != "base64" || read.Message.Body.Content != base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0x01}) {
		t.Fatalf("raw binary body = %#v", read.Message.Body)
	}

	assertErrorCode(t, h.runNoBrowser("read", "--id", "raw-unsupported", "--body-mode", "raw", "--account", "work", "--json"), errs.CodeUnsupportedBodyMode)
	assertErrorCodeExit(t, h.runNoBrowser("read", "--id", "raw-gone", "--body-mode", "raw", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)
	assertErrorCodeExit(t, h.runNoBrowser("read", "--id", "raw-auth", "--body-mode", "raw", "--account", "work", "--json"), errs.CodeAuthRepairRequired, 3)
	rate := h.runNoBrowser("read", "--id", "raw-rate", "--body-mode", "raw", "--account", "work", "--json")
	if rate.Exit != rungrad.ExitRateLimited {
		t.Fatalf("rate exit = %d stdout=%s stderr=%s", rate.Exit, rate.Stdout, rate.Stderr)
	}
	api := h.runNoBrowser("read", "--id", "raw-500", "--body-mode", "raw", "--account", "work", "--json")
	if api.Exit != rungrad.ExitAPI {
		t.Fatalf("raw 500 exit = %d stdout=%s stderr=%s", api.Exit, api.Stdout, api.Stderr)
	}
	oversized := h.runNoBrowser("read", "--id", "raw-oversized", "--body-mode", "raw", "--account", "work", "--json")
	if oversized.Exit != rungrad.ExitAPI || strings.TrimSpace(oversized.Stdout) != "" || strings.Contains(oversized.Stdout+oversized.Stderr, strings.Repeat("x", 128)) {
		t.Fatalf("raw oversized result exit=%d stdout=%s stderr=%s", oversized.Exit, oversized.Stdout, oversized.Stderr)
	}
}

func TestOutlookReadInternetIDAndThreadRequests(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var read readResult
	decodeOK(t, h.runNoBrowser("read", "--internet-id", "<single@example.test>", "--account", "work", "--json"), &read)
	if read.Message.ID != "internet-single" {
		t.Fatalf("internet read = %#v", read.Message)
	}
	req := h.server.firstMessageRequest()
	if got, want := req.Query.Get("$filter"), "internetMessageId eq '<single@example.test>'"; got != want {
		t.Fatalf("internet filter = %q, want %q", got, want)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("read", "--internet-id", "<o'hara@example.test>", "--account", "work", "--json"), &read)
	if got, want := h.server.firstMessageRequest().Query.Get("$filter"), "internetMessageId eq '<o''hara@example.test>'"; got != want {
		t.Fatalf("quoted internet filter = %q, want %q", got, want)
	}
	assertErrorCode(t, h.runNoBrowser("read", "--internet-id", "<duplicate@example.test>", "--account", "work", "--json"), errs.CodeAmbiguousMessageSelector)
	assertErrorCode(t, h.runNoBrowser("read", "--internet-id", "<duplicate-next@example.test>", "--account", "work", "--json"), errs.CodeAmbiguousMessageSelector)
	assertErrorCodeExit(t, h.runNoBrowser("read", "--internet-id", "<missing@example.test>", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)

	h.server.resetMessageRequests()
	var thread threadResult
	decodeOK(t, h.runNoBrowser("thread", "--id", "read-id", "--account", "work", "--json"), &thread)
	if got, want := threadMessageIDs(thread.Thread.Messages), []string{"thread-a", "thread-b", "thread-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread order = %#v, want %#v", got, want)
	}
	requests := h.server.messageRequestsSnapshot()
	if len(requests) < 2 || requests[1].Path != "/me/messages" || requests[1].Query.Get("$filter") != "conversationId eq 'thread-read-id'" {
		t.Fatalf("thread requests = %#v", requests)
	}
	var raw map[string]any
	decodeOK(t, h.runNoBrowser("thread", "--id", "read-id", "--account", "work", "--json"), &raw)
	row := raw["thread"].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if _, ok := row["body"]; ok {
		t.Fatalf("thread row has body: %#v", row)
	}
	decodeOK(t, h.runNoBrowser("thread", "--internet-id", "<single@example.test>", "--account", "work", "--json"), &thread)
	if thread.Thread.ThreadID != "thread-internet-single" {
		t.Fatalf("thread by internet id = %#v", thread.Thread)
	}
	assertErrorCodeExit(t, h.runNoBrowser("thread", "--thread-id", "missing-thread", "--account", "work", "--json"), errs.CodeThreadNotFound, 5)

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("thread", "--thread-id", "quoted'thread", "--account", "work", "--json"), &thread)
	if got, want := h.server.firstMessageRequest().Query.Get("$filter"), "conversationId eq 'quoted''thread'"; got != want {
		t.Fatalf("quoted thread filter = %q, want %q", got, want)
	}
}

func TestOutlookReadProviderErrorMappingAndSecretSweep(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.oversizedMessages.Store(true)
		res := h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json")
		if res.Exit != rungrad.ExitAPI || strings.TrimSpace(res.Stdout) != "" || strings.Contains(res.Stdout+res.Stderr, strings.Repeat("{", 128)) {
			t.Fatalf("oversized result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.malformedMessages.Store(true)
		res := h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json")
		if res.Exit != rungrad.ExitAPI || strings.TrimSpace(res.Stdout) != "" || strings.Contains(res.Stdout+res.Stderr, `{"id":`) {
			t.Fatalf("malformed result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
	})
	t.Run("auth repair", func(t *testing.T) {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			h := seededOutlookMessagesHarness(t)
			h.server.messageStatus.Store(int64(status))
			res := h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json")
			assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
		}
	})
	t.Run("secret sweep", func(t *testing.T) {
		h := newOutlookHarness(t)
		account := outlookFoldersAccount()
		if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
			t.Fatalf("seed registry: %v", err)
		}
		secrets := []string{"outlook-read-access-token-12345", "outlook-read-refresh-token-12345"}
		if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
			c.AccessToken = secrets[0]
			c.RefreshToken = secrets[1]
			c.VerifiedEmail = account.Email
			c.ProviderAccountID = account.ProviderAccountID
		}); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
		for _, args := range [][]string{
			{"read", "--id", "read-id", "--account", "work"},
			{"read", "--id", "read-id", "--account", "work", "--json"},
			{"read", "--id", "read-id", "--account", "work", "--json", "--jq", ".message.id"},
			{"read", "--id", "read-id", "--account", "work", "--template", "{{.message.id}}"},
			{"read", "--id", "read-id", "--account", "work", "--plain"},
			{"read", "--id", "read-id", "--account", "work", "--json", "--include-meta"},
			{"thread", "--id", "read-id", "--account", "work"},
			{"thread", "--id", "read-id", "--account", "work", "--json"},
			{"thread", "--id", "read-id", "--account", "work", "--json", "--jq", ".thread.thread_id"},
			{"thread", "--id", "read-id", "--account", "work", "--template", "{{.thread.thread_id}}"},
			{"thread", "--id", "read-id", "--account", "work", "--json", "--include-meta"},
		} {
			res := h.runNoBrowser(args...)
			testkit.AssertNoSecretLeak(t, res, secrets...)
			assertNoPathLeak(t, res, h.root, h.configPath)
			if strings.Contains(res.Stdout+res.Stderr, "Authorization") || strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) {
				t.Fatalf("output leaked request details: stdout=%s stderr=%s", res.Stdout, res.Stderr)
			}
		}
	})
}

func TestOutlookReadRejectsHostilePaginationWithoutPartialOutput(t *testing.T) {
	t.Run("internet lookup pagination", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.hostileMessageLink.Store(true)
		res := h.runNoBrowser("read", "--internet-id", "<duplicate-next@example.test>", "--account", "work", "--json")
		assertHostileReadFailure(t, h, res)
		if h.server.messageCalls.Load() != 1 {
			t.Fatalf("hostile internet nextLink should not be requested: calls=%d", h.server.messageCalls.Load())
		}
	})

	t.Run("attachment pagination", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.hostileAttachmentLink.Store(true)
		res := h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json")
		assertHostileReadFailure(t, h, res)
		if h.server.messageCalls.Load() != 2 {
			t.Fatalf("hostile attachment nextLink should not be requested: calls=%d", h.server.messageCalls.Load())
		}
	})

	t.Run("thread pagination", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.hostileMessageLink.Store(true)
		res := h.runNoBrowser("thread", "--id", "read-id", "--account", "work", "--json")
		assertHostileReadFailure(t, h, res)
		if h.server.messageCalls.Load() != 2 {
			t.Fatalf("hostile thread nextLink should not be requested: calls=%d", h.server.messageCalls.Load())
		}
	})
}

func TestOutlookReadMidOperationFailuresDoNotRenderPartialOutput(t *testing.T) {
	t.Run("attachment second page fails", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.failAttachmentPage2.Store(true)
		res := h.runNoBrowser("read", "--id", "read-id", "--account", "work", "--json")
		if res.Exit != rungrad.ExitAPI || strings.TrimSpace(res.Stdout) != "" {
			t.Fatalf("attachment page failure result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
	})

	t.Run("thread second page fails", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.failThreadPage2.Store(true)
		res := h.runNoBrowser("thread", "--id", "read-id", "--account", "work", "--json")
		if res.Exit != rungrad.ExitAPI || strings.TrimSpace(res.Stdout) != "" {
			t.Fatalf("thread page failure result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
	})

	t.Run("internet id second page duplicate stays product error", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		res := h.runNoBrowser("read", "--internet-id", "<duplicate-next@example.test>", "--account", "work", "--json")
		assertErrorCode(t, res, errs.CodeAmbiguousMessageSelector)
		if strings.TrimSpace(res.Stdout) != "" {
			t.Fatalf("duplicate internet id rendered partial stdout: %s", res.Stdout)
		}
	})
}

func assertHostileReadFailure(t *testing.T, h *outlookHarness, res testutil.Result) {
	t.Helper()
	if res.Exit == 0 {
		t.Fatalf("hostile nextLink succeeded: stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("hostile nextLink rendered partial stdout: %s", res.Stdout)
	}
	if strings.Contains(res.Stdout+res.Stderr, "example.invalid") || strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) {
		t.Fatalf("hostile nextLink leaked request details: stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout+res.Stderr, "Authorization") {
		t.Fatalf("hostile nextLink leaked auth details: stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
}
