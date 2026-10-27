package main

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestOutlookForwardCreatePatchMetadataAndSend(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var draft forwardResult
	decodeOK(t, h.runNoBrowser("forward", "--id", "forward-source", "--to", "client@example.test", "--body", "# Hi", "--markdown", "--include-attachment", "source.pdf", "--account", "work", "--json"), &draft)
	requests := h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/forward-source/createForward") ||
		!hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-extra") ||
		!hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-item") ||
		!hasPatchRequest(requests) {
		t.Fatalf("forward requests = %#v", requests)
	}
	var patch map[string]any
	for _, r := range requests {
		if r.Method == http.MethodPatch {
			patch = decodeRequestBody(t, r)
		}
	}
	body := patch["body"].(map[string]any)
	content := body["content"].(string)
	if body["contentType"] != "html" || !strings.Contains(content, "<h1>Hi</h1>") || !strings.Contains(content, "---------- Forwarded message ---------") ||
		strings.Index(content, "<h1>Hi</h1>") > strings.Index(content, "---------- Forwarded message ---------") {
		t.Fatalf("forward patch body = %#v", body)
	}
	if draft.ActionMode != "draft" || draft.DraftID != "forward-forward-source" || len(draft.SelectedSourceAttachments) != 1 || draft.AttachmentCount != 2 {
		t.Fatalf("forward result = %#v", draft)
	}
	if draft.WriteOutcomeFacts == nil || draft.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("forward write facts = %#v", draft.WriteOutcomeFacts)
	}

	h.server.resetMessageRequests()
	var sent draftWriteResult
	decodeOK(t, approvedRun(t, h.runNoBrowser, "forward", "--id", "forward-source", "--to", "client@example.test", "--body", "send", "--send", "--account", "work", "--json"), &sent)
	requests = h.server.messageRequestsSnapshot()
	if !hasRequest(requests, http.MethodPost, "/me/sendMail") || !hasRequest(requests, http.MethodDelete, "/me/messages/forward-forward-source") || sent.Operation != "draft_send" {
		t.Fatalf("sent forward requests=%#v result=%#v", requests, sent)
	}
	message := decodeRequestBody(t, requestFor(t, requests, http.MethodPost, "/me/sendMail"))["message"].(map[string]any)
	if attachments, _ := message["attachments"].([]any); len(attachments) != 3 {
		t.Fatalf("forward sent with attachments %#v", message["attachments"])
	}
	if sent.WriteOutcomeFacts == nil || sent.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("sent forward write facts = %#v", sent.WriteOutcomeFacts)
	}
}

func TestOutlookForwardAttachmentSelectionModes(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var all forwardResult
	decodeOK(t, h.runNoBrowser("forward", "--id", "forward-source", "--to", "client@example.test", "--body", "all", "--include-attachments", "all", "--account", "work", "--json"), &all)
	requests := h.server.messageRequestsSnapshot()
	if len(all.SelectedSourceAttachments) != 2 ||
		len(all.UnsupportedSourceExclusion) != 1 ||
		all.UnsupportedSourceExclusion[0].Attachment.ID != "source-item" ||
		all.AttachmentCount != 3 ||
		!hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-item") ||
		hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-file") ||
		hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-extra") {
		t.Fatalf("all-mode requests=%#v result=%#v", requests, all)
	}

	h.server.resetMessageRequests()
	var byID forwardResult
	decodeOK(t, h.runNoBrowser("forward", "--id", "forward-source", "--to", "client@example.test", "--body", "by id", "--include-attachment-id", "source-extra", "--account", "work", "--json"), &byID)
	requests = h.server.messageRequestsSnapshot()
	if len(byID.SelectedSourceAttachments) != 1 ||
		byID.SelectedSourceAttachments[0].ID != "source-extra" ||
		byID.AttachmentCount != 2 ||
		!hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-file") ||
		!hasPath(requests, "/me/messages/forward-forward-source/attachments/copy-source-item") {
		t.Fatalf("id-mode requests=%#v result=%#v", requests, byID)
	}
}

func TestOutlookForwardAttachmentSelectionRejectsUnsafeIDs(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	for _, tt := range []struct {
		name string
		args []string
		code string
		exit int
	}{
		{
			name: "overlap",
			args: []string{"forward", "--id", "forward-source", "--body", "x", "--include-attachment", "source.pdf", "--include-attachment-id", "source-file", "--account", "work", "--json"},
			code: errs.CodeInvalidComposeInput,
			exit: 1,
		},
		{
			name: "inline",
			args: []string{"forward", "--id", "forward-source", "--body", "x", "--include-attachment-id", "source-inline", "--account", "work", "--json"},
			code: errs.CodeInvalidComposeInput,
			exit: 1,
		},
		{
			name: "item",
			args: []string{"forward", "--id", "forward-source", "--body", "x", "--include-attachment-id", "source-item", "--account", "work", "--json"},
			code: errs.CodeInvalidComposeInput,
			exit: 1,
		},
		{
			name: "missing name",
			args: []string{"forward", "--id", "forward-source", "--body", "x", "--include-attachment", "missing.pdf", "--account", "work", "--json"},
			code: errs.CodeAttachmentNotFound,
			exit: 5,
		},
		{
			name: "missing id",
			args: []string{"forward", "--id", "forward-source", "--body", "x", "--include-attachment-id", "missing-attachment", "--account", "work", "--json"},
			code: errs.CodeAttachmentNotFound,
			exit: 5,
		},
		{
			name: "tuple ambiguity before create",
			args: []string{"forward", "--id", "forward-duplicates", "--body", "x", "--include-attachment-id", "dup-a", "--account", "work", "--json"},
			code: errs.CodeAmbiguousAttachment,
			exit: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h.server.resetMessageRequests()
			assertErrorCodeExit(t, h.runNoBrowser(tt.args...), tt.code, tt.exit)
			if hasPath(h.server.messageRequestsSnapshot(), "/me/messages/"+tt.args[2]+"/createForward") {
				t.Fatalf("%s created a forward draft: %#v", tt.name, h.server.messageRequestsSnapshot())
			}
		})
	}
}

func TestOutlookForwardAttachmentReconcileDetectsMissingCopiedTuple(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	assertErrorCode(t, h.runNoBrowser("forward", "--id", "forward-copy-missing", "--body", "x", "--include-attachment-id", "missing-a", "--include-attachment-id", "missing-b", "--account", "work", "--json"), errs.CodeAmbiguousAttachment)
	requests := h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/forward-copy-missing/createForward") || hasPatchRequest(requests) {
		t.Fatalf("missing-copy reconcile requests = %#v", requests)
	}
}

func TestOutlookForwardQuerySelectionAndDryRunBoundaries(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var query forwardResult
	decodeOK(t, h.runNoBrowser("forward", "--query", "subject:Search", "--latest", "--to", "client@example.test", "--body", "FYI", "--account", "work", "--json"), &query)
	requests := h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/search-new/createForward") ||
		query.SourceID != "search-new" ||
		query.SourceInternetID != "<search-new@example.test>" {
		t.Fatalf("query forward requests=%#v result=%#v", requests, query)
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("forward", "--query", "subject:Search", "--latest", "--body", "dry secret", "--dry-run", "--account", "work", "--json"))
	requests = h.server.messageRequestsSnapshot()
	if len(requests) == 0 || hasPath(requests, "/me/messages/search-new/createForward") || hasPatchRequest(requests) {
		t.Fatalf("query dry-run requests = %#v", requests)
	}

	h.server.resetMessageRequests()
	decodePreview(t, h.runNoBrowser("forward", "--internet-id", "<single@example.test>", "--body", "dry secret", "--dry-run", "--account", "work", "--json"))
	// The preview reads the original and its attachment list, and writes
	// nothing.
	requests = h.server.messageRequestsSnapshot()
	if len(requests) == 0 || !strings.Contains(requests[0].Query.Get("$filter"), "single@example.test") ||
		hasPath(requests, "/me/messages/internet-single/createForward") || hasPatchRequest(requests) {
		t.Fatalf("internet-id dry-run requests: %#v", requests)
	}
	for _, r := range requests {
		if r.Method != http.MethodGet {
			t.Fatalf("internet-id dry-run wrote: %#v", r)
		}
	}
}

func TestOutlookDraftAttachmentListAddRemove(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var listed draftAttachmentsResult
	decodeOK(t, h.runNoBrowser("attachments", "list", "--draft-id", "draft-attachments", "--account", "work", "--json"), &listed)
	if listed.Count != 3 || listed.Attachments[0].Kind != mail.AttachmentKindFile || listed.Attachments[0].ProviderType == "" {
		t.Fatalf("listed = %#v", listed)
	}
	plain := h.runNoBrowser("attachments", "list", "--draft-id", "draft-attachments", "--account", "work", "--plain")
	decodeOK(t, plain, nil)
	if len(strings.Split(strings.Split(strings.TrimSpace(plain.Stdout), "\n")[0], "\t")) != 6 {
		t.Fatalf("plain attachments = %q", plain.Stdout)
	}
	var inlineOnly draftAttachmentsResult
	decodeOK(t, h.runNoBrowser("attachments", "list", "--draft-id", "draft-inline-only", "--account", "work", "--json"), &inlineOnly)
	if inlineOnly.Count != 1 || !inlineOnly.Attachments[0].IsInline || inlineOnly.Attachments[0].ID != "inline-only" {
		t.Fatalf("inline-only attachments = %#v", inlineOnly)
	}
	assertErrorCode(t, h.runNoBrowser("attachments", "remove", "--draft-id", "draft-inline-only", "--account", "work", "--attachment-id", "inline-only", "--json"), errs.CodeInvalidComposeInput)

	dir := t.TempDir()
	file := filepath.Join(dir, "upload.txt")
	if err := os.WriteFile(file, []byte("upload bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.resetMessageRequests()
	var added draftAttachmentAddResult
	decodeOK(t, h.runNoBrowser("attachments", "add", "--draft-id", "draft-attachments", "--account", "work", file, "--json"), &added)
	requests := h.server.messageRequestsSnapshot()
	var upload map[string]any
	for _, r := range requests {
		if r.Method == http.MethodPost && r.Path == "/me/messages/draft-attachments/attachments" {
			upload = decodeRequestBody(t, r)
		}
	}
	if upload["@odata.type"] != "#microsoft.graph.fileAttachment" || upload["name"] != "upload.txt" ||
		upload["contentBytes"] != base64.StdEncoding.EncodeToString([]byte("upload bytes")) ||
		strings.Contains(previewString(map[string]any{"data": added}), file) {
		t.Fatalf("upload payload=%#v result=%#v", upload, added)
	}
	addPost := firstRequestIndex(requests, http.MethodPost, "/me/messages/draft-attachments/attachments")
	if firstRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments") < 0 ||
		firstRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments/attachments") < 0 ||
		addPost < 0 ||
		lastRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments/attachments") <= addPost {
		t.Fatalf("add request order = %#v", requests)
	}

	h.server.resetMessageRequests()
	var removed draftAttachmentRemoveResult
	decodeOK(t, h.runNoBrowser("attachments", "remove", "--draft-id", "draft-attachments", "--account", "work", "--attachment-id", "draft-file", "--json"), &removed)
	requests = h.server.messageRequestsSnapshot()
	if !hasPath(requests, "/me/messages/draft-attachments/attachments/draft-file") ||
		removed.Removed.ID != "draft-file" ||
		removed.Removed.Name != "draft.pdf" ||
		removed.Removed.ContentType != "application/pdf" ||
		removed.Removed.Kind != mail.AttachmentKindFile ||
		removed.Removed.ProviderType == "" {
		t.Fatalf("remove requests=%#v result=%#v", requests, removed)
	}
	removeDelete := firstRequestIndex(requests, http.MethodDelete, "/me/messages/draft-attachments/attachments/draft-file")
	if firstRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments/attachments") < 0 ||
		removeDelete < 0 ||
		lastRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments/attachments") <= removeDelete {
		t.Fatalf("remove request order = %#v", requests)
	}

	staleHarness := seededOutlookMessagesHarness(t)
	staleHarness.server.messageSendStatus.Store(http.StatusNotFound)
	stale := staleHarness.runNoBrowser("attachments", "remove", "--draft-id", "draft-attachments", "--account", "work", "--attachment-id", "draft-file", "--json")
	staleHarness.server.messageSendStatus.Store(0)
	assertErrorCodeExit(t, stale, errs.CodeAttachmentNotFound, 5)
	requests = staleHarness.server.messageRequestsSnapshot()
	if firstRequestIndex(requests, http.MethodDelete, "/me/messages/draft-attachments/attachments/draft-file") < 0 ||
		firstRequestIndex(requests, http.MethodGet, "/me/messages/draft-attachments") < 0 {
		t.Fatalf("stale remove requests=%#v stderr=%s", requests, stale.Stderr)
	}
}

func firstRequestIndex(requests []messageRequest, method, path string) int {
	for i, r := range requests {
		if r.Method == method && r.Path == path {
			return i
		}
	}
	return -1
}

func lastRequestIndex(requests []messageRequest, method, path string) int {
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Method == method && requests[i].Path == path {
			return i
		}
	}
	return -1
}
