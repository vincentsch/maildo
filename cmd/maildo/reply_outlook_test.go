package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestOutlookReplyFullNoneShortAndSend(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var full replyResult
	decodeOK(t, h.runNoBrowser("reply", "--id", "read-id", "--body", "# Hi", "--markdown", "--account", "work", "--json"), &full)
	requests := h.server.messageRequestsSnapshot()
	if len(requests) < 5 ||
		requests[0].Method != http.MethodGet || requests[0].Path != "/me/messages/read-id" || !strings.Contains(requests[0].Prefer, `"html"`) ||
		requests[1].Method != http.MethodPost || requests[1].Path != "/me/messages/read-id/createReply" ||
		!hasPatchRequest(requests) {
		t.Fatalf("full requests = %#v", requests)
	}
	patch := decodeRequestBody(t, requests[3])
	body := patch["body"].(map[string]any)
	content := body["content"].(string)
	if body["contentType"] != "html" || !strings.Contains(content, "<h1>Hi</h1>") || !strings.Contains(content, "<blockquote>") ||
		strings.Index(content, "<h1>Hi</h1>") > strings.Index(content, "<blockquote>") {
		t.Fatalf("full patch body = %#v", body)
	}
	if full.ReplyMode != mail.ReplyModeReply || full.QuoteMode != mail.QuoteModeFull || full.DraftID != "reply-read-id" || full.SentID != "" {
		t.Fatalf("full result = %#v", full)
	}
	if full.WriteOutcomeFacts == nil || full.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("full write facts = %#v", full.WriteOutcomeFacts)
	}

	h.server.resetMessageRequests()
	var replyAll replyResult
	decodeOK(t, h.runNoBrowser("reply", "--reply-all", "--id", "read-id", "--body", "x", "--account", "work", "--json"), &replyAll)
	if !hasPath(h.server.messageRequestsSnapshot(), "/me/messages/read-id/createReplyAll") {
		t.Fatalf("reply-all requests = %#v", h.server.messageRequestsSnapshot())
	}
	if replyAll.ReplyMode != mail.ReplyModeReplyAll {
		t.Fatalf("reply-all result = %#v", replyAll)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("reply", "--id", "read-id", "--quote", "none", "--body", "plain", "--account", "work", "--json"), &replyResult{})
	patch = lastPatchBody(t, h)
	body = patch["body"].(map[string]any)
	if body["contentType"] != "text" || body["content"] != "plain" || strings.Contains(body["content"].(string), "blockquote") {
		t.Fatalf("none text body = %#v", body)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("reply", "--id", "read-id", "--quote", "none", "--body", "# Title\n<script>x()</script>", "--markdown", "--account", "work", "--json"), &replyResult{})
	patch = lastPatchBody(t, h)
	body = patch["body"].(map[string]any)
	if body["contentType"] != "html" || !strings.Contains(body["content"].(string), "<h1>Title</h1>") || strings.Contains(strings.ToLower(body["content"].(string)), "script") {
		t.Fatalf("none markdown body = %#v", body)
	}

	h.server.resetMessageRequests()
	var short replyResult
	decodeOK(t, h.runNoBrowser("reply", "--id", "read-id", "--quote", "short", "--quote-language", "de", "--body", "Danke", "--account", "work", "--json"), &short)
	requests = h.server.messageRequestsSnapshot()
	if len(requests) < 5 || requests[0].Method != http.MethodGet || !strings.Contains(requests[0].Prefer, `"html"`) ||
		requests[1].Method != http.MethodPost || requests[1].Path != "/me/messages/read-id/createReply" {
		t.Fatalf("short requests = %#v", requests)
	}
	patch = lastPatchBody(t, h)
	body = patch["body"].(map[string]any)
	shortContent := body["content"].(string)
	if body["contentType"] != "text" || !strings.Contains(shortContent, "Danke") ||
		!strings.Contains(shortContent, "Am 2026-06-19 08:00 UTC schrieb Sender read-id <read-id@example.test>:") ||
		!strings.Contains(shortContent, "> HTML body for read-id") {
		t.Fatalf("short text body = %#v", body)
	}
	if short.QuoteLanguage != mail.QuoteLanguageGerman {
		t.Fatalf("short result = %#v", short)
	}

	// On Outlook the preview saves the reply as a draft, and the command it
	// prints sends that draft.
	h.server.resetMessageRequests()
	var sent draftWriteResult
	decodeOK(t, approvedRun(t, h.runNoBrowser, "reply", "--id", "read-id", "--body", "send", "--send", "--account", "work", "--json"), &sent)
	requests = h.server.messageRequestsSnapshot()
	if !hasRequest(requests, http.MethodPost, "/me/sendMail") || !hasRequest(requests, http.MethodDelete, "/me/messages/reply-read-id") {
		t.Fatalf("send requests = %#v", requests)
	}
	if sent.Operation != "draft_send" || sent.DraftID != "reply-read-id" {
		t.Fatalf("sent result = %#v", sent)
	}
	if sent.WriteOutcomeFacts == nil || sent.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("sent write facts = %#v", sent.WriteOutcomeFacts)
	}
}

func TestOutlookReplySelectorsAndDryRun(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var internet replyResult
	decodeOK(t, h.runNoBrowser("reply", "--internet-id", "<single@example.test>", "--body", "x", "--account", "work", "--json"), &internet)
	requests := h.server.messageRequestsSnapshot()
	if len(requests) < 2 || requests[0].Method != http.MethodGet || !strings.Contains(requests[0].Query.Get("$filter"), "single@example.test") ||
		requests[1].Method != http.MethodPost || requests[1].Path != "/me/messages/internet-single/createReply" {
		t.Fatalf("internet requests = %#v", requests)
	}
	if internet.SourceID != "internet-single" || internet.SourceInternetID != "<single@example.test>" {
		t.Fatalf("internet result = %#v", internet)
	}

	h.server.resetMessageRequests()
	assertErrorCode(t, h.runNoBrowser("reply", "--internet-id", "<duplicate@example.test>", "--body", "x", "--account", "work", "--json"), errs.CodeAmbiguousMessageSelector)
	if hasCreateReplyRequest(h.server.messageRequestsSnapshot()) {
		t.Fatalf("duplicate created reply: %#v", h.server.messageRequestsSnapshot())
	}

	h.server.resetMessageRequests()
	assertErrorCodeExit(t, h.runNoBrowser("reply", "--internet-id", "<missing@example.test>", "--body", "x", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)
	if hasCreateReplyRequest(h.server.messageRequestsSnapshot()) {
		t.Fatalf("missing created reply: %#v", h.server.messageRequestsSnapshot())
	}

	h.server.resetMessageRequests()
	var query replyResult
	decodeOK(t, h.runNoBrowser("reply", "--query", "subject:Search", "--latest", "--body", "x", "--account", "work", "--json"), &query)
	requests = h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/search-new/createReply") ||
		query.SourceID != "search-new" ||
		query.SourceInternetID != "<search-new@example.test>" {
		t.Fatalf("query requests=%#v result=%#v", requests, query)
	}

	h.server.resetMessageRequests()
	var firstQuery replyResult
	decodeOK(t, h.runNoBrowser("reply", "--query", "subject:Search", "--first", "--body", "x", "--account", "work", "--json"), &firstQuery)
	requests = h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/search-new/createReply") ||
		firstQuery.SourceID != "search-new" ||
		firstQuery.SourceInternetID != "<search-new@example.test>" {
		t.Fatalf("first query requests=%#v result=%#v", requests, firstQuery)
	}

	h.server.resetMessageRequests()
	preview := decodePreview(t, h.runNoBrowser("reply", "--id", "read-id", "--body", "dry secret", "--dry-run", "--account", "work", "--json"))
	requests = h.server.messageRequestsSnapshot()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/me/messages/read-id" ||
		hasCreateReplyRequest(requests) || hasPatchRequest(requests) || preview["message"].(map[string]any)["subject"] != "Re: Read read-id" {
		t.Fatalf("id dry-run preview=%#v requests=%#v", preview, h.server.messageRequestsSnapshot())
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("reply", "--query", "subject:Search", "--latest", "--body", "dry secret", "--dry-run", "--account", "work", "--json"))
	requests = h.server.messageRequestsSnapshot()
	if len(requests) == 0 || hasCreateReplyRequest(requests) || hasPatchRequest(requests) {
		t.Fatalf("query dry-run requests = %#v", requests)
	}
}

func TestOutlookReplyFailureAndSecretSafety(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	h.server.messageStatus.Store(http.StatusUnauthorized)
	assertErrorCodeExit(t, h.runNoBrowser("reply", "--id", "read-id", "--body", "secret body", "--account", "work", "--json"), errs.CodeAuthRepairRequired, 3)
	h.server.messageStatus.Store(0)

	h.server.resetMessageRequests()
	h.server.oversizedMessages.Store(true)
	// Short quotes read source context before Graph creates the reply draft, so
	// an oversized source body must fail without leaving a native draft behind.
	res := h.runNoBrowser("reply", "--id", "read-id", "--quote", "short", "--body", "secret body", "--account", "work", "--json")
	if res.Exit == 0 ||
		!strings.Contains(res.Stderr, "message body exceeds the read size limit") ||
		strings.Contains(res.Stdout+res.Stderr, "Project") ||
		strings.Contains(res.Stdout+res.Stderr, "secret body") ||
		hasCreateReplyRequest(h.server.messageRequestsSnapshot()) {
		t.Fatalf("short source oversized stdout=%s stderr=%s requests=%#v", res.Stdout, res.Stderr, h.server.messageRequestsSnapshot())
	}
	h.server.oversizedMessages.Store(false)

	h.server.resetMessageRequests()
	h.server.messagePatchStatus.Store(http.StatusForbidden)
	res = h.runNoBrowser("reply", "--id", "read-id", "--body", "secret body", "--account", "work", "--json")
	assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
	if !strings.Contains(res.Stderr, "reply-read-id") || strings.Contains(res.Stdout+res.Stderr, "secret body") {
		t.Fatalf("post-create error stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	h.server.messagePatchStatus.Store(0)

	h.server.resetMessageRequests()
	h.server.messagePatchStatus.Store(http.StatusInternalServerError)
	res = h.runNoBrowser("reply", "--id", "read-id", "--body", "secret body", "--account", "work", "--json")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "reply-read-id") || !strings.Contains(res.Stderr, `"outcome": "unknown"`) || strings.Contains(res.Stdout+res.Stderr, "secret body") {
		t.Fatalf("patch 500 stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	h.server.messagePatchStatus.Store(0)

	h.server.resetMessageRequests()
	h.server.messageReplyStatus.Store(http.StatusTooManyRequests)
	if res := h.runNoBrowser("reply", "--id", "read-id", "--body", "x", "--account", "work", "--json"); res.Exit == 0 {
		t.Fatal("rate-limited createReply succeeded")
	}
	h.server.messageReplyStatus.Store(0)

	h.server.resetMessageRequests()
	h.server.malformedMessages.Store(true)
	if res := h.runNoBrowser("reply", "--id", "read-id", "--body", "x", "--account", "work", "--json"); res.Exit == 0 || res.Stdout != "" {
		t.Fatalf("malformed createReply exit=%d stdout=%q stderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	h.server.malformedMessages.Store(false)

	h.server.resetMessageRequests()
	h.server.oversizedMessages.Store(true)
	if res := h.runNoBrowser("reply", "--id", "read-id", "--body", "x", "--account", "work", "--json"); res.Exit == 0 || res.Stdout != "" {
		t.Fatalf("oversized createReply exit=%d stdout=%q stderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	h.server.oversizedMessages.Store(false)

	h.server.resetMessageRequests()
	for _, args := range [][]string{
		{"reply", "--id", "read-id", "--body", "SAFE SENTINEL"},
		{"reply", "--id", "read-id", "--body", "SAFE SENTINEL", "--json"},
		{"reply", "--id", "read-id", "--body", "SAFE SENTINEL", "--json", "--jq", ".draft_id"},
		{"reply", "--id", "read-id", "--body", "SAFE SENTINEL", "--template", "{{.reply_mode}}"},
		{"reply", "--id", "read-id", "--body", "SAFE SENTINEL", "--json", "--include-meta"},
	} {
		res := h.runNoBrowser(args...)
		if res.Exit != 0 {
			t.Fatalf("%v failed: exit=%d stdout=%s stderr=%s", args, res.Exit, res.Stdout, res.Stderr)
		}
		testkit.AssertNoSecretLeak(t, res, "seed-outlook-access-token-work", "seed-outlook-refresh-token-work")
		for _, forbidden := range []string{"SAFE SENTINEL", "Authorization", "Bearer", h.server.srv.URL} {
			if strings.Contains(res.Stdout+res.Stderr, forbidden) {
				t.Fatalf("%v leaked %q: stdout=%s stderr=%s", args, forbidden, res.Stdout, res.Stderr)
			}
		}
	}
}

func lastPatchBody(t *testing.T, h *outlookHarness) map[string]any {
	t.Helper()
	requests := h.server.messageRequestsSnapshot()
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Method == http.MethodPatch {
			return decodeRequestBody(t, requests[i])
		}
	}
	t.Fatalf("no patch request in %#v", requests)
	return nil
}

func hasCreateReplyRequest(requests []messageRequest) bool {
	for _, r := range requests {
		if r.Method == http.MethodPost && (strings.HasSuffix(r.Path, "/createReply") || strings.HasSuffix(r.Path, "/createReplyAll")) {
			return true
		}
	}
	return false
}

func hasPatchRequest(requests []messageRequest) bool {
	for _, r := range requests {
		if r.Method == http.MethodPatch {
			return true
		}
	}
	return false
}

func hasPath(requests []messageRequest, path string) bool {
	for _, r := range requests {
		if r.Path == path {
			return true
		}
	}
	return false
}
