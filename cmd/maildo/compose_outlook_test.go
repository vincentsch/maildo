package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
)

func TestOutlookComposeCreateReadUpdateAndSend(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var created draftWriteResult
	decodeOK(t, h.runNoBrowser("draft", "create",
		"--to", "Client <client@example.test>",
		"--cc", "copy@example.test",
		"--bcc", "blind@example.test",
		"--reply-to", "reply@example.test",
		"--from", "Work <work@example.test>",
		"--subject", "Draft subject",
		"--body", "# Hello\n<script>x()</script>",
		"--markdown",
		"--json",
	), &created)
	if created.DraftID != "draft-created" || created.RequestedFrom == "" {
		t.Fatalf("created = %#v", created)
	}
	req := h.server.lastMessageRequest()
	if req.Method != http.MethodPost || req.Path != "/me/messages" {
		t.Fatalf("create request = %#v", req)
	}
	payload := decodeRequestBody(t, req)
	if _, ok := payload["from"]; ok {
		t.Fatalf("create payload set from: %#v", payload)
	}
	if _, ok := payload["sender"]; ok {
		t.Fatalf("create payload set sender: %#v", payload)
	}
	body := payload["body"].(map[string]any)
	if body["contentType"] != "html" || strings.Contains(strings.ToLower(body["content"].(string)), "script") {
		t.Fatalf("create body = %#v", body)
	}
	if len(payload["toRecipients"].([]any)) != 1 || len(payload["replyTo"].([]any)) != 1 {
		t.Fatalf("create recipients = %#v", payload)
	}

	h.server.resetMessageRequests()
	var read draftReadResult
	decodeOK(t, h.runNoBrowser("draft", "read", "--id", "draft-read", "--json"), &read)
	if !read.Draft.IsDraft || read.Draft.ID != "draft-read" || !strings.Contains(read.Draft.Body.Content, "Text body for draft-read") {
		t.Fatalf("read = %#v", read.Draft)
	}
	readReq := h.server.firstMessageRequest()
	if readReq.Method != http.MethodGet || !strings.Contains(readReq.Query.Get("$select"), "isDraft") {
		t.Fatalf("read request = %#v", readReq)
	}
	var rawRead draftReadResult
	decodeOK(t, h.runNoBrowser("draft", "read", "--id", "raw-binary", "--body-mode", "raw", "--json"), &rawRead)
	if rawRead.Draft.Body.Mode != "raw" || rawRead.Draft.Body.Encoding != "base64" {
		t.Fatalf("raw draft read = %#v", rawRead.Draft.Body)
	}
	assertErrorCodeExit(t, h.runNoBrowser("draft", "read", "--id", "not-a-draft", "--json"), errs.CodeDraftNotFound, 5)

	h.server.resetMessageRequests()
	var updated draftWriteResult
	decodeOK(t, h.runNoBrowser("draft", "update", "--id", "draft-update", "--to", "new@example.test", "--clear-cc", "--subject", "Updated", "--body", "Updated body", "--json"), &updated)
	if updated.DraftID != "draft-update" {
		t.Fatalf("updated = %#v", updated)
	}
	requests := h.server.messageRequestsSnapshot()
	if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPatch {
		t.Fatalf("update requests = %#v", requests)
	}
	patch := decodeRequestBody(t, requests[1])
	if patch["subject"] != "Updated" || len(patch["toRecipients"].([]any)) != 1 || len(patch["ccRecipients"].([]any)) != 0 {
		t.Fatalf("patch payload = %#v", patch)
	}
	if _, ok := patch["bccRecipients"]; ok {
		t.Fatalf("patch contained unchanged bcc: %#v", patch)
	}

	h.server.resetMessageRequests()
	decodeOK(t, h.runNoBrowser("draft", "update", "--id", "draft-clear-body", "--subject", "", "--body", "", "--html", "--json"), &draftWriteResult{})
	requests = h.server.messageRequestsSnapshot()
	if len(requests) != 2 || requests[1].Method != http.MethodPatch {
		t.Fatalf("clear-body requests = %#v", requests)
	}
	patch = decodeRequestBody(t, requests[1])
	if patch["subject"] != "" {
		t.Fatalf("clear-subject patch = %#v", patch)
	}
	clearedBody := patch["body"].(map[string]any)
	if clearedBody["contentType"] != "text" || clearedBody["content"] != "" {
		t.Fatalf("clear-body patch = %#v", patch)
	}

	h.server.resetMessageRequests()
	assertErrorCodeExit(t, h.runNoBrowser("draft", "update", "--id", "not-a-draft", "--subject", "Nope", "--json"), errs.CodeDraftNotFound, 5)
	if got := h.server.messageRequestsSnapshot(); len(got) != 1 || got[0].Method != http.MethodGet {
		t.Fatalf("non-draft update requests = %#v", got)
	}

	h.server.resetMessageRequests()
	var draftSent draftWriteResult
	decodeOK(t, approvedRun(t, h.runNoBrowser, "draft", "send", "--id", "draft-send", "--json"), &draftSent)
	// maildo reads the draft with its attachments, sends that copy with
	// sendMail and then deletes the draft.
	requests = h.server.messageRequestsSnapshot()
	if writeRequestCount(requests) != 2 || !hasRequest(requests, http.MethodPost, "/me/sendMail") || !hasRequest(requests, http.MethodDelete, "/me/messages/draft-send") ||
		hasRequest(requests, http.MethodPost, "/me/messages/draft-send/send") {
		t.Fatalf("draft send requests = %#v", requests)
	}
	sendMail := decodeRequestBody(t, requestFor(t, requests, http.MethodPost, "/me/sendMail"))["message"].(map[string]any)
	if attachments, _ := sendMail["attachments"].([]any); len(attachments) != 2 || sendMail["subject"] != "Draft draft-send" {
		t.Fatalf("sendMail message = %#v", sendMail)
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("draft", "send", "--yes", "--id", "draft-send", "--dry-run", "--json"))
	requests = h.server.messageRequestsSnapshot()
	if len(requests) == 0 || hasWriteRequest(requests) {
		t.Fatalf("draft send dry-run requests = %#v", requests)
	}
}

func TestOutlookComposeDirectSendDryRunAndFailures(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("draft", "create", "--to", "client@example.test", "--subject", "Preview", "--body", "secret body", "--dry-run", "--json"))
	if got := h.server.messageRequestsSnapshot(); len(got) != 0 {
		t.Fatalf("draft create dry-run made requests: %#v", got)
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("draft", "update", "--id", "draft-update", "--subject", "Preview", "--dry-run", "--json"))
	if got := h.server.messageRequestsSnapshot(); len(got) == 0 || hasWriteRequest(got) {
		t.Fatalf("draft update dry-run requests = %#v", got)
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("send", "--yes", "--to", "client@example.test", "--subject", "Preview", "--body", "secret body", "--dry-run", "--json"))
	if got := h.server.messageRequestsSnapshot(); len(got) != 0 {
		t.Fatalf("direct send dry-run made requests: %#v", got)
	}

	var sent draftWriteResult
	decodeOK(t, approvedRun(t, h.runNoBrowser, "send", "--to", "client@example.test", "--subject", "Send", "--body", "<b>ok</b><script>x()</script>", "--html", "--json"), &sent)
	req := h.server.lastMessageRequest()
	if req.Method != http.MethodPost || req.Path != "/me/sendMail" {
		t.Fatalf("send request = %#v", req)
	}
	payload := decodeRequestBody(t, req)
	msg := payload["message"].(map[string]any)
	if _, ok := msg["from"]; ok {
		t.Fatalf("send payload set from: %#v", msg)
	}
	body := msg["body"].(map[string]any)
	if body["contentType"] != "html" || strings.Contains(strings.ToLower(body["content"].(string)), "script") {
		t.Fatalf("send body = %#v", body)
	}

	h.server.messageStatus.Store(http.StatusForbidden)
	assertErrorCodeExit(t, approvedRun(t, h.runNoBrowser, "send", "--to", "client@example.test", "--subject", "Send", "--body", "x", "--json"), errs.CodeAuthRepairRequired, 3)
	h.server.messageStatus.Store(0)

	// A 201 with cut-off JSON may still have created the draft.
	h.server.malformedMessages.Store(true)
	res := h.runNoBrowser("draft", "create", "--to", "client@example.test", "--subject", "Bad", "--json")
	if res.Exit == 0 || !strings.Contains(res.Stderr, `"outcome": "unknown"`) || !strings.Contains(res.Stderr, `"retry_safe": false`) || !strings.Contains(res.Stderr, `"reason": "provider_result_malformed"`) {
		t.Fatalf("malformed create response: exit=%d stderr=%s", res.Exit, res.Stderr)
	}
	h.server.malformedMessages.Store(false)

	h.server.oversizedMessages.Store(true)
	res = h.runNoBrowser("draft", "create", "--to", "client@example.test", "--subject", "Too large", "--json")
	if res.Exit == 0 || res.Stdout != "" {
		t.Fatalf("oversized create response exit=%d stdout=%q stderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	// The draft may have been created; the error says so instead of
	// claiming a plain failure.
	if !strings.Contains(res.Stderr, `"outcome": "unknown"`) || !strings.Contains(res.Stderr, `"reason": "provider_result_malformed"`) ||
		strings.Contains(res.Stderr, "Project ") || strings.Contains(res.Stderr, "p0") {
		t.Fatalf("oversized create response stderr = %s", res.Stderr)
	}
	h.server.oversizedMessages.Store(false)
}

func TestOutlookComposeOversizedStatusBodiesKeepErrorClass(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int64
		wantExit int
		wantText string
	}{
		{
			name:     "rate limit",
			status:   http.StatusTooManyRequests,
			wantExit: rungrad.ExitRateLimited,
			wantText: "rate limited",
		},
		{
			name:     "server error",
			status:   http.StatusInternalServerError,
			wantExit: 1,
			wantText: "maildo could not confirm the result",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookMessagesHarness(t)
			h.server.messageStatus.Store(tt.status)
			h.server.oversizedStatusBodies.Store(true)

			res := approvedRun(t, h.runNoBrowser, "send", "--to", "client@example.test", "--subject", "x", "--body", "secret body", "--json")
			if res.Exit != tt.wantExit {
				t.Fatalf("exit=%d want=%d stdout=%s stderr=%s", res.Exit, tt.wantExit, res.Stdout, res.Stderr)
			}
			if res.Stdout != "" {
				t.Fatalf("failure wrote partial stdout: %s", res.Stdout)
			}
			out := res.Stdout + res.Stderr
			for _, forbidden := range []string{"secret body", strings.Repeat("{", 128), "compose response exceeds the read size limit"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("failure leaked or misclassified %q: stdout=%s stderr=%s", forbidden, res.Stdout, res.Stderr)
				}
			}
			if !strings.Contains(out, tt.wantText) {
				t.Fatalf("stderr did not include %q: %s", tt.wantText, res.Stderr)
			}
		})
	}

	h := seededOutlookMessagesHarness(t)
	h.server.messageSendStatus.Store(http.StatusNotFound)
	h.server.oversizedStatusBodies.Store(true)
	res := approvedRun(t, h.runNoBrowser, "draft", "send", "--id", "draft-race", "--json")
	assertErrorCodeExit(t, res, errs.CodeProviderForbidden, 4)
	if res.Stdout != "" || strings.Contains(res.Stderr, "compose response exceeds the read size limit") || strings.Contains(res.Stderr, strings.Repeat("{", 128)) || !strings.Contains(res.Stderr, `"outcome": "rejected"`) {
		t.Fatalf("refused send result stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
}

func TestOutlookComposeFailureMatrixDoesNotLeakBodyOrPartialSuccess(t *testing.T) {
	const secretBody = "FAILURE BODY MUST NOT LEAK"
	for _, tt := range []struct {
		name  string
		args  []string
		setup func(*outlookHarness)
		code  string
		exit  int
	}{
		{
			name: "create auth repair",
			args: []string{"draft", "create", "--to", "client@example.test", "--subject", "x", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusUnauthorized)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "create rate limited",
			args: []string{"draft", "create", "--to", "client@example.test", "--subject", "x", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusTooManyRequests)
			},
		},
		{
			name: "read auth repair",
			args: []string{"draft", "read", "--id", "draft-read", "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusForbidden)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "read missing",
			args: []string{"draft", "read", "--id", "missing", "--json"},
			code: errs.CodeDraftNotFound,
			exit: 5,
		},
		{
			name: "raw rate limited",
			args: []string{"draft", "read", "--id", "raw-rate", "--body-mode", "raw", "--json"},
		},
		{
			name: "update preflight auth repair",
			args: []string{"draft", "update", "--id", "draft-update", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusForbidden)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "update write auth repair",
			args: []string{"draft", "update", "--id", "draft-update", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messagePatchStatus.Store(http.StatusForbidden)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "update write server error",
			args: []string{"draft", "update", "--id", "draft-update", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messagePatchStatus.Store(http.StatusInternalServerError)
			},
		},
		{
			name: "draft send preflight auth repair",
			args: []string{"draft", "send", "--yes", "--id", "draft-send", "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusUnauthorized)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "draft send write auth repair",
			args: []string{"draft", "send", "--yes", "--id", "draft-send", "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageSendStatus.Store(http.StatusForbidden)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "draft send write server error",
			args: []string{"draft", "send", "--yes", "--id", "draft-send", "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageSendStatus.Store(http.StatusInternalServerError)
			},
		},
		{
			name: "direct send auth repair",
			args: []string{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusUnauthorized)
			},
			code: errs.CodeAuthRepairRequired,
			exit: 3,
		},
		{
			name: "direct send server error",
			args: []string{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--body", secretBody, "--json"},
			setup: func(h *outlookHarness) {
				h.server.messageStatus.Store(http.StatusInternalServerError)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookMessagesHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			run := h.runNoBrowser
			if slices.Contains(tt.args, "--yes") {
				run = func(a ...string) testutil.Result { return approvedRun(t, h.runNoBrowser, a...) }
			}
			res := run(tt.args...)
			if tt.code != "" {
				assertErrorCodeExit(t, res, tt.code, tt.exit)
			} else if res.Exit == 0 {
				t.Fatalf("command succeeded unexpectedly: stdout=%s stderr=%s", res.Stdout, res.Stderr)
			}
			if res.Stdout != "" {
				t.Fatalf("failure wrote partial stdout: %s", res.Stdout)
			}
			out := res.Stdout + res.Stderr
			for _, forbidden := range []string{secretBody, "Authorization", "Bearer", h.server.srv.URL, "seed-outlook-access-token-work", "seed-outlook-refresh-token-work"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("failure leaked %q: stdout=%s stderr=%s", forbidden, res.Stdout, res.Stderr)
				}
			}
		})
	}
}

func TestOutlookDraftWriteRaceMapsToDraftNotFound(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("update %d", status), func(t *testing.T) {
			h := seededOutlookMessagesHarness(t)
			h.server.messagePatchStatus.Store(int64(status))
			assertErrorCodeExit(t, h.runNoBrowser("draft", "update", "--id", "draft-race", "--subject", "Updated", "--json"), errs.CodeDraftNotFound, 5)
			requests := h.server.messageRequestsSnapshot()
			if len(requests) < 2 || requests[0].Method != http.MethodGet || writeRequestCount(requests) != 1 || requests[len(requests)-1].Method != http.MethodPatch {
				t.Fatalf("race update requests = %#v", requests)
			}
		})
		// A draft deleted after the preview is not sent: maildo reads it
		// again before the send and finds it gone.
		t.Run(fmt.Sprintf("send %d", status), func(t *testing.T) {
			h := seededOutlookMessagesHarness(t)
			var preview sendPreview
			decodeOK(t, h.runNoBrowser("draft", "send", "--id", "draft-race", "--dry-run", "--json"), &preview)
			h.server.setMessageStatus("draft-race", status)
			h.server.resetMessageRequests()
			assertErrorCodeExit(t, runPrinted(t, h.runNoBrowser, preview.SendCommand), errs.CodeDraftNotFound, 5)
			if requests := h.server.messageRequestsSnapshot(); hasWriteRequest(requests) {
				t.Fatalf("race send requests = %#v", requests)
			}
		})
	}
}

func TestOutlookComposeSecretSweep(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	secrets := []string{"seed-outlook-access-token-work", "seed-outlook-refresh-token-work"}
	for _, args := range [][]string{
		{"draft", "create", "--to", "client@example.test", "--subject", "Draft", "--body", "Safe body"},
		{"draft", "create", "--to", "client@example.test", "--subject", "Draft", "--body", "Safe body", "--json"},
		{"draft", "create", "--to", "client@example.test", "--subject", "Draft", "--body", "Safe body", "--json", "--jq", ".operation"},
		{"draft", "create", "--to", "client@example.test", "--subject", "Draft", "--body", "Safe body", "--template", "{{.operation}}"},
		{"draft", "create", "--to", "client@example.test", "--subject", "Draft", "--body", "Safe body", "--json", "--include-meta"},
		{"draft", "read", "--id", "draft-read"},
		{"draft", "read", "--id", "draft-read", "--json"},
		{"draft", "read", "--id", "draft-read", "--json", "--jq", ".draft.id"},
		{"draft", "read", "--id", "draft-read", "--template", "{{.draft.id}}"},
		{"draft", "read", "--id", "draft-read", "--plain"},
		{"draft", "read", "--id", "draft-read", "--json", "--include-meta"},
		{"draft", "update", "--id", "draft-update", "--subject", "Updated"},
		{"draft", "update", "--id", "draft-update", "--subject", "Updated", "--json"},
		{"draft", "update", "--id", "draft-update", "--subject", "Updated", "--json", "--jq", ".operation"},
		{"draft", "update", "--id", "draft-update", "--subject", "Updated", "--template", "{{.operation}}"},
		{"draft", "update", "--id", "draft-update", "--subject", "Updated", "--json", "--include-meta"},
		{"draft", "send", "--yes", "--id", "draft-send"},
		{"draft", "send", "--yes", "--id", "draft-send", "--json"},
		{"draft", "send", "--yes", "--id", "draft-send", "--json", "--jq", ".operation"},
		{"draft", "send", "--yes", "--id", "draft-send", "--template", "{{.operation}}"},
		{"draft", "send", "--yes", "--id", "draft-send", "--json", "--include-meta"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "Send", "--body", "Safe body"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "Send", "--body", "Safe body", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "Send", "--body", "Safe body", "--json", "--jq", ".operation"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "Send", "--body", "Safe body", "--template", "{{.operation}}"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "Send", "--body", "Safe body", "--json", "--include-meta"},
	} {
		run := h.runNoBrowser
		if slices.Contains(args, "--yes") {
			run = func(a ...string) testutil.Result { return approvedRun(t, h.runNoBrowser, a...) }
		}
		res := run(args...)
		if res.Exit != 0 {
			t.Fatalf("%v failed: exit=%d stdout=%s stderr=%s", args, res.Exit, res.Stdout, res.Stderr)
		}
		testkit.AssertNoSecretLeak(t, res, secrets...)
		if strings.Contains(res.Stdout+res.Stderr, "Authorization") || strings.Contains(res.Stdout+res.Stderr, h.server.srv.URL) {
			t.Fatalf("%v leaked request details: stdout=%s stderr=%s", args, res.Stdout, res.Stderr)
		}
	}
	if h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("secret sweep used interactive/refresh path token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
}

func decodeRequestBody(t *testing.T, req messageRequest) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(req.Body, &out); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, req.Body)
	}
	return out
}

func hasWriteRequest(requests []messageRequest) bool {
	for _, r := range requests {
		if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			return true
		}
	}
	return false
}

func hasRequest(requests []messageRequest, method, path string) bool {
	for _, r := range requests {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func requestFor(t *testing.T, requests []messageRequest, method, path string) messageRequest {
	t.Helper()
	for _, r := range requests {
		if r.Method == method && r.Path == path {
			return r
		}
	}
	t.Fatalf("no %s %s in %#v", method, path, requests)
	return messageRequest{}
}

func writeRequestCount(requests []messageRequest) int {
	n := 0
	for _, r := range requests {
		if r.Method != http.MethodGet {
			n++
		}
	}
	return n
}
