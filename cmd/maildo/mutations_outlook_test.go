package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad"
)

func TestOutlookMutationGraphRequests(t *testing.T) {
	t.Run("bulk repeated requests preserve order", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		var result bulkMutationResult
		decodeOK(t, h.runNoBrowser("mark", "read", "--id", "read-id", "--id", "slash/id", "--account", "work", "--apply", "--json"), &result)
		if result.Counts.Succeeded != 2 {
			t.Fatalf("bulk result = %#v", result)
		}
		reqs := h.server.messageRequestsSnapshot()
		if len(reqs) != 2 || reqs[0].Path != "/me/messages/read-id" || reqs[1].EscapedPath != "/me/messages/slash%2Fid" {
			t.Fatalf("bulk requests = %#v", reqs)
		}
	})

	t.Run("bulk not found records failed target and continues", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.setMessageStatus("missing-bulk", http.StatusNotFound)
		res := h.runNoBrowser("mark", "read", "--id", "read-id", "--id", "missing-bulk", "--id", "slash/id", "--account", "work", "--apply", "--json")
		assertErrorCodeExit(t, res, errs.CodeBulkPartialFailure, 5)
		if res.Stdout != "" {
			t.Fatalf("partial failure stdout = %s", res.Stdout)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		counts := body["counts"].(map[string]any)
		if counts["succeeded"] != float64(2) || counts["failed"] != float64(1) {
			t.Fatalf("partial body = %#v", body)
		}
		if reqs := h.server.messageRequestsSnapshot(); len(reqs) != 3 {
			t.Fatalf("requests = %#v", reqs)
		}
	})

	t.Run("bulk auth repair stops account", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.setMessageStatus("auth-bulk", http.StatusUnauthorized)
		res := h.runNoBrowser("mark", "read", "--id", "auth-bulk", "--id", "read-id", "--account", "work", "--apply", "--json")
		assertErrorCodeExit(t, res, errs.CodeBulkPartialFailure, 3)
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		targets := body["targets"].([]any)
		if targets[0].(map[string]any)["status"] != "failed" || targets[1].(map[string]any)["status"] != "skipped" {
			t.Fatalf("targets = %#v", targets)
		}
		if reqs := h.server.messageRequestsSnapshot(); len(reqs) != 1 {
			t.Fatalf("auth stop requests = %#v", reqs)
		}
	})

	t.Run("move escapes id and sends destination", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		var result mutationResult
		decodeOK(t, h.runNoBrowser("move", "--id", "slash/id", "--to", "archive", "--account", "work", "--json"), &result)
		req := h.server.lastMessageRequest()
		if req.Method != http.MethodPost || req.EscapedPath != "/me/messages/slash%2Fid/move" {
			t.Fatalf("move request = %#v", req)
		}
		if body := jsonBody(t, req.Body); body["destinationId"] != "archive-id" {
			t.Fatalf("move payload = %#v", body)
		}
		if result.TargetFolder == nil || result.TargetFolder.ID != "archive-id" {
			t.Fatalf("move result = %#v", result)
		}
	})

	t.Run("copy and restore use folder destination", func(t *testing.T) {
		for _, tt := range []struct {
			command string
			suffix  string
		}{
			{command: "copy", suffix: "/copy"},
			{command: "restore", suffix: "/move"},
		} {
			h := seededOutlookMessagesHarness(t)
			args := []string{tt.command, "--id", "read-id", "--to", "inbox", "--account", "work", "--json"}
			decodeOK(t, h.runNoBrowser(args...), &mutationResult{})
			req := h.server.lastMessageRequest()
			if req.Method != http.MethodPost || !strings.HasSuffix(req.Path, tt.suffix) {
				t.Fatalf("%s request = %#v", tt.command, req)
			}
			if body := jsonBody(t, req.Body); body["destinationId"] != "inbox-id" {
				t.Fatalf("%s payload = %#v", tt.command, body)
			}
		}
	})

	t.Run("well known mutations resolve aliases then move", func(t *testing.T) {
		for _, tt := range []struct {
			args      []string
			aliasPath string
			folderID  string
			operation string
		}{
			{args: []string{"archive", "--id", "read-id", "--account", "work", "--json"}, aliasPath: "/me/mailFolders/archive", folderID: "archive-id", operation: "archive"},
			{args: []string{"delete", "--id", "read-id", "--account", "work", "--json"}, aliasPath: "/me/mailFolders/deleteditems", folderID: "deleted-id", operation: "delete"},
			{args: []string{"junk", "--id", "read-id", "--account", "work", "--json"}, aliasPath: "/me/mailFolders/junkemail", folderID: "junk-id", operation: "junk"},
		} {
			h := seededOutlookMessagesHarness(t)
			var result mutationResult
			decodeOK(t, h.runNoBrowser(tt.args...), &result)
			if result.Operation != tt.operation {
				t.Fatalf("result operation = %#v", result)
			}
			if !stringSliceContains(h.server.mailFolderRequestsSnapshot(), tt.aliasPath) {
				t.Fatalf("alias %s not requested: %#v", tt.aliasPath, h.server.mailFolderRequestsSnapshot())
			}
			req := h.server.lastMessageRequest()
			if req.Method != http.MethodPost || req.Path != "/me/messages/read-id/move" {
				t.Fatalf("move request = %#v", req)
			}
			if body := jsonBody(t, req.Body); body["destinationId"] != tt.folderID {
				t.Fatalf("move payload = %#v", body)
			}
		}
	})

	t.Run("state patches send narrow payloads", func(t *testing.T) {
		for _, tt := range []struct {
			args []string
			want map[string]any
		}{
			{args: []string{"mark", "read", "--id", "read-id", "--account", "work", "--json"}, want: map[string]any{"isRead": true}},
			{args: []string{"mark", "unread", "--id", "read-id", "--account", "work", "--json"}, want: map[string]any{"isRead": false}},
			{args: []string{"flag", "set", "--id", "read-id", "--account", "work", "--json"}, want: map[string]any{"flag": map[string]any{"flagStatus": "flagged"}}},
			{args: []string{"flag", "clear", "--id", "read-id", "--account", "work", "--json"}, want: map[string]any{"flag": map[string]any{"flagStatus": "notFlagged"}}},
			{args: []string{"categories", "set", "--id", "read-id", "--category", "Client", "--category", "Urgent", "--account", "work", "--json"}, want: map[string]any{"categories": []any{"Client", "Urgent"}}},
		} {
			h := seededOutlookMessagesHarness(t)
			decodeOK(t, h.runNoBrowser(tt.args...), &mutationResult{})
			req := h.server.lastMessageRequest()
			if req.Method != http.MethodPatch || req.Path != "/me/messages/read-id" {
				t.Fatalf("patch request = %#v", req)
			}
			if body := jsonBody(t, req.Body); !reflect.DeepEqual(body, tt.want) {
				t.Fatalf("patch body = %#v, want %#v", body, tt.want)
			}
		}
	})

	t.Run("category list and named clear use narrow get then patch", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		var listed categoryListResult
		decodeOK(t, h.runNoBrowser("categories", "list", "--id", "read-id", "--account", "work", "--json"), &listed)
		req := h.server.lastMessageRequest()
		if req.Method != http.MethodGet || req.Path != "/me/messages/read-id" || req.Query.Get("$select") != "id,internetMessageId,categories" {
			t.Fatalf("category list request = %#v", req)
		}

		h.server.resetMessageRequests()
		var cleared mutationResult
		decodeOK(t, h.runNoBrowser("categories", "clear", "--id", "read-id", "--category", "Graph", "--category", "Missing", "--account", "work", "--json"), &cleared)
		reqs := h.server.messageRequestsSnapshot()
		if len(reqs) != 2 {
			t.Fatalf("clear requests = %#v", reqs)
		}
		if reqs[0].Method != http.MethodGet || reqs[0].Query.Get("$select") != "id,internetMessageId,categories" {
			t.Fatalf("clear get = %#v", reqs[0])
		}
		if reqs[1].Method != http.MethodPatch {
			t.Fatalf("clear patch = %#v", reqs[1])
		}
		if body := jsonBody(t, reqs[1].Body); !reflect.DeepEqual(body["categories"], []any{"Fixture"}) {
			t.Fatalf("clear patch body = %#v", body)
		}
		if len(cleared.Categories.Removed) != 1 || len(cleared.Categories.Skipped) != 1 {
			t.Fatalf("clear result = %#v", cleared)
		}
	})
}

func TestOutlookMutationErrorsAndDryRuns(t *testing.T) {
	t.Run("message status mapping", func(t *testing.T) {
		for _, tt := range []struct {
			status int64
			code   string
			exit   int
		}{
			{status: http.StatusNotFound, code: errs.CodeMessageNotFound, exit: 5},
			{status: http.StatusGone, code: errs.CodeMessageNotFound, exit: 5},
			{status: http.StatusUnauthorized, code: errs.CodeAuthRepairRequired, exit: 3},
			{status: http.StatusForbidden, code: errs.CodeAuthRepairRequired, exit: 3},
		} {
			h := seededOutlookMessagesHarness(t)
			h.server.messageStatus.Store(tt.status)
			h.server.oversizedStatusBodies.Store(true)
			res := h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--json")
			assertErrorCodeExit(t, res, tt.code, tt.exit)
			if strings.Contains(res.Stderr, "mailbox mutation response exceeds") || strings.Contains(res.Stderr, strings.Repeat("{", 128)) {
				t.Fatalf("status misclassified/leaked: %s", res.Stderr)
			}
		}
	})

	t.Run("rate and server status mapping", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.messageStatus.Store(http.StatusTooManyRequests)
		h.server.oversizedStatusBodies.Store(true)
		rate := h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--json")
		if rate.Exit != rungrad.ExitRateLimited || rate.Stdout != "" || !strings.Contains(rate.Stderr, "rate limited") || strings.Contains(rate.Stderr, "mailbox mutation response exceeds") {
			t.Fatalf("rate result exit=%d stdout=%s stderr=%s", rate.Exit, rate.Stdout, rate.Stderr)
		}

		h = seededOutlookMessagesHarness(t)
		h.server.messageStatus.Store(http.StatusInternalServerError)
		h.server.oversizedStatusBodies.Store(true)
		server := h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--json")
		// A server error after the move was sent leaves the outcome open.
		if server.Exit == 0 || server.Stdout != "" || !strings.Contains(server.Stderr, `"outcome": "unknown"`) || !strings.Contains(server.Stderr, `"retry_safe": false`) ||
			strings.Contains(server.Stderr, "compose") || strings.Contains(server.Stderr, "mailbox mutation response exceeds") || strings.Contains(server.Stderr, strings.Repeat("{", 128)) {
			t.Fatalf("server result exit=%d stdout=%s stderr=%s", server.Exit, server.Stdout, server.Stderr)
		}
	})

	t.Run("alias missing maps folder not found", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.archiveAlias404.Store(true)
		assertErrorCodeExit(t, h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--json"), errs.CodeFolderNotFound, 5)
	})

	t.Run("malformed and oversized success bodies are safe", func(t *testing.T) {
		// The reviewed case: a copy answered with 201 and cut-off JSON may
		// have happened, so it is unknown and not safe to retry.
		for _, args := range [][]string{
			{"archive", "--id", "read-id", "--account", "work", "--json"},
			{"copy", "--id", "read-id", "--to", "inbox", "--account", "work", "--json"},
		} {
			h := seededOutlookMessagesHarness(t)
			h.server.malformedMessages.Store(true)
			malformed := h.runNoBrowser(args...)
			if malformed.Exit == 0 || malformed.Stdout != "" || strings.Contains(malformed.Stderr, `{"id":`) || strings.Contains(malformed.Stderr, "compose") ||
				!strings.Contains(malformed.Stderr, `"outcome": "unknown"`) || !strings.Contains(malformed.Stderr, `"retry_safe": false`) || !strings.Contains(malformed.Stderr, `"write_may_have_landed": true`) {
				t.Fatalf("%v malformed result exit=%d stdout=%s stderr=%s", args, malformed.Exit, malformed.Stdout, malformed.Stderr)
			}
		}

		h := seededOutlookMessagesHarness(t)

		h = seededOutlookMessagesHarness(t)
		h.server.oversizedMessages.Store(true)
		oversized := h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--json")
		if oversized.Exit == 0 || oversized.Stdout != "" || !strings.Contains(oversized.Stderr, `"outcome": "unknown"`) ||
			strings.Contains(oversized.Stderr, strings.Repeat("{", 128)) || strings.Contains(oversized.Stderr, "compose") {
			t.Fatalf("oversized result exit=%d stdout=%s stderr=%s", oversized.Exit, oversized.Stdout, oversized.Stderr)
		}
	})

	t.Run("successful mutation responses require message id", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			args []string
		}{
			{name: "move", args: []string{"move", "--id", "read-id", "--to", "inbox", "--account", "work", "--json"}},
			{name: "copy", args: []string{"copy", "--id", "read-id", "--to", "inbox", "--account", "work", "--json"}},
			{name: "read state", args: []string{"mark", "read", "--id", "read-id", "--account", "work", "--json"}},
			{name: "category list", args: []string{"categories", "list", "--id", "read-id", "--account", "work", "--json"}},
			{name: "category set", args: []string{"categories", "set", "--id", "read-id", "--category", "Client", "--account", "work", "--json"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				h.server.missingMutationID.Store(true)
				res := h.runNoBrowser(tt.args...)
				if tt.name == "category list" {
					if res.Exit != rungrad.ExitAPI || res.Stdout != "" || !strings.Contains(res.Stderr, "the message request to Outlook for account") || strings.Contains(res.Stderr, "compose") {
						t.Fatalf("missing id result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
					}
					return
				}
				// A change answered without an id may still have happened.
				if res.Exit == 0 || res.Stdout != "" || !strings.Contains(res.Stderr, `"outcome": "unknown"`) || !strings.Contains(res.Stderr, `"retry_safe": false`) || strings.Contains(res.Stderr, "compose") {
					t.Fatalf("missing id result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
				}
			})
		}
	})

	t.Run("missing target folder validates before selector resolution", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			args []string
		}{
			{name: "query", args: []string{"move", "--query", "anything", "--latest", "--account", "work", "--json"}},
			{name: "internet id", args: []string{"copy", "--internet-id", "<single@example.test>", "--account", "work", "--json"}},
			{name: "blank value", args: []string{"restore", "--id", "read-id", "--to", "  ", "--account", "work", "--json"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				res := h.runNoBrowser(tt.args...)
				if res.Exit != rungrad.ExitUsage || res.Stdout != "" || !strings.Contains(res.Stderr, "name the folder with --to") {
					t.Fatalf("missing --to result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
				}
				if len(h.server.messageRequestsSnapshot()) != 0 || len(h.server.mailFolderRequestsSnapshot()) != 0 {
					t.Fatalf("missing --to touched provider messages=%#v folders=%#v", h.server.messageRequestsSnapshot(), h.server.mailFolderRequestsSnapshot())
				}
			})
		}
	})

	t.Run("dry runs avoid writes", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		decodePreview(t, h.runNoBrowser("archive", "--id", "read-id", "--account", "work", "--dry-run", "--json"))
		// The preview reads the message to name it, and writes nothing.
		for _, req := range h.server.messageRequestsSnapshot() {
			if req.Method != http.MethodGet {
				t.Fatalf("explicit id dry-run wrote to Graph: %#v", req)
			}
		}

		for _, tt := range []struct {
			name string
			args []string
			op   string
		}{
			{name: "move", args: []string{"move", "--id", "read-id", "--to", "inbox", "--account", "work", "--dry-run", "--json"}, op: "move"},
			{name: "copy", args: []string{"copy", "--id", "read-id", "--to", "inbox", "--account", "work", "--dry-run", "--json"}, op: "copy"},
			{name: "restore", args: []string{"restore", "--id", "read-id", "--to", "inbox", "--account", "work", "--dry-run", "--json"}, op: "restore"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				preview := decodePreview(t, h.runNoBrowser(tt.args...))
				folder, _ := preview["target_folder"].(map[string]any)
				if preview["operation"] != tt.op || folder["id"] != "inbox-id" {
					t.Fatalf("folder dry-run preview = %#v", preview)
				}
				for _, req := range h.server.messageRequestsSnapshot() {
					if req.Method != http.MethodGet {
						t.Fatalf("folder dry-run wrote to Graph: %#v", req)
					}
				}
				if len(h.server.mailFolderRequestsSnapshot()) == 0 {
					t.Fatalf("folder dry-run did not resolve the folder")
				}
			})
		}

		h = seededOutlookMessagesHarness(t)
		decodePreview(t, h.runNoBrowser("archive", "--query", "anything", "--latest", "--account", "work", "--dry-run", "--json"))
		for _, req := range h.server.messageRequestsSnapshot() {
			if req.Method != http.MethodGet {
				t.Fatalf("query dry-run wrote request: %#v", req)
			}
		}

		h = seededOutlookMessagesHarness(t)
		decodePreview(t, h.runNoBrowser("archive", "--internet-id", "<single@example.test>", "--account", "work", "--dry-run", "--json"))
		for _, req := range h.server.messageRequestsSnapshot() {
			if req.Method != http.MethodGet {
				t.Fatalf("internet id dry-run wrote to Graph: %#v", req)
			}
		}

		h = seededOutlookMessagesHarness(t)
		decodeOK(t, h.runNoBrowser("categories", "list", "--internet-id", "<single@example.test>", "--account", "work", "--dry-run", "--json"), &categoryListResult{})
		reqs := h.server.messageRequestsSnapshot()
		if len(reqs) != 2 || reqs[0].Path != "/me/messages" || reqs[0].Query.Get("$select") != "id,internetMessageId" ||
			reqs[1].Path != "/me/messages/internet-single" || reqs[1].Query.Get("$select") != "id,internetMessageId,categories" {
			t.Fatalf("category list dry-run requests = %#v", reqs)
		}

		h = seededOutlookMessagesHarness(t)
		decodePreview(t, h.runNoBrowser("categories", "clear", "--id", "read-id", "--category", "Graph", "--account", "work", "--dry-run", "--json"))
		reqs = h.server.messageRequestsSnapshot()
		if len(reqs) == 0 {
			t.Fatalf("named clear dry-run did not read the categories")
		}
		for _, req := range reqs {
			if req.Method != http.MethodGet {
				t.Fatalf("named clear dry-run requests = %#v", reqs)
			}
		}
	})
}

func jsonBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return out
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
