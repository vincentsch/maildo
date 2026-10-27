package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
)

// overrideDraft serves draft-send with the recipient that to returns, so a
// test can change the draft between two requests.
func overrideDraft(h *outlookHarness, to func() string) {
	original := h.server.srv.Config.Handler
	h.server.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/me/messages/draft-send" {
			address := to()
			m := graphDraftFixture("draft-send", true, r.Header.Get("Prefer"))
			m["changeKey"] = "ck-" + address
			m["toRecipients"] = []map[string]any{{"emailAddress": map[string]any{"address": address}}}
			m["ccRecipients"] = []any{}
			m["bccRecipients"] = []any{}
			writeCLIJSON(w, m)
			return
		}
		original.ServeHTTP(w, r)
	})
}

// The reviewed case: the preview showed approved@, the draft changed to
// unapproved@, and the printed command must not send it.
func TestOutlookDraftChangedAfterThePreviewIsNotSent(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var mu sync.Mutex
	address := "approved@example.test"
	overrideDraft(h, func() string { mu.Lock(); defer mu.Unlock(); return address })

	var preview sendPreview
	decodeOK(t, h.runNoBrowser("draft", "send", "draft-send", "--dry-run", "--json"), &preview)
	if preview.ApprovalCode == "" || !strings.Contains(preview.SendCommand, "--approve "+preview.ApprovalCode) || !strings.Contains(preview.SendCommand, "--account work") {
		t.Fatalf("preview = %+v", preview)
	}
	mu.Lock()
	address = "unapproved@example.test"
	mu.Unlock()
	h.server.resetMessageRequests()
	res := runPrinted(t, h.runNoBrowser, preview.SendCommand)
	assertErrorCode(t, res, errs.CodeApprovalMismatch)
	if hasRequest(h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail") {
		t.Fatalf("a changed draft was sent: %s", res.Stderr)
	}
}

// An edit that lands after maildo's last read must not change what goes
// out: maildo sends its checked copy with sendMail, and it leaves the
// edited draft alone.
func TestOutlookDraftEditDuringTheSendDoesNotGoOut(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var mu sync.Mutex
	reads := 0
	overrideDraft(h, func() string {
		mu.Lock()
		defer mu.Unlock()
		reads++
		// The preview reads once. The send reads for its checks and once
		// more for the copy it sends. Every read after that sees someone
		// else's edit.
		if reads > 3 {
			return "outsider@example.test"
		}
		return "client@example.test"
	})
	res := approvedRun(t, h.runNoBrowser, "draft", "send", "draft-send", "--json")
	decodeOK(t, res, nil)
	requests := h.server.messageRequestsSnapshot()
	message := decodeRequestBody(t, requestFor(t, requests, http.MethodPost, "/me/sendMail"))["message"].(map[string]any)
	to, _ := message["toRecipients"].([]any)
	if len(to) != 1 || !strings.Contains(string(requestFor(t, requests, http.MethodPost, "/me/sendMail").Body), "client@example.test") || strings.Contains(string(requestFor(t, requests, http.MethodPost, "/me/sendMail").Body), "outsider") {
		t.Fatalf("sendMail message = %#v", message)
	}
	if hasRequest(requests, http.MethodDelete, "/me/messages/draft-send") || hasRequest(requests, http.MethodPost, "/me/messages/draft-send/send") {
		t.Fatalf("the edited draft was deleted or sent by id: %#v", requests)
	}
	if !strings.Contains(res.Stdout, "left the changed draft in Drafts") {
		t.Fatalf("result does not mention the kept draft: %s", res.Stdout)
	}
}

// An edit between maildo's checks and the copy it sends is caught by the
// last check, right before sendMail.
func TestOutlookDraftEditBeforeTheSendIsRefused(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var mu sync.Mutex
	reads := 0
	overrideDraft(h, func() string {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads > 2 {
			return "outsider@example.test"
		}
		return "client@example.test"
	})
	assertErrorCode(t, approvedRun(t, h.runNoBrowser, "draft", "send", "draft-send", "--json"), errs.CodeApprovalMismatch)
	if hasRequest(h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail") {
		t.Fatal("a draft edited before the send went out")
	}
}

// The preview has the whole body and every attachment, inline ones too,
// also when Graph's hasAttachments says there are none.
func TestOutlookDraftSendPreviewShowsTheWholeMessage(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var preview sendPreview
	decodeOK(t, h.runNoBrowser("draft", "send", "draft-inline-only", "--dry-run", "--json"), &preview)
	if !strings.Contains(preview.Message.Body, "HTML body for draft-inline-only") || !strings.Contains(preview.Message.BodyHTML, "<b>body</b>") {
		t.Fatalf("preview body = %q html %q", preview.Message.Body, preview.Message.BodyHTML)
	}
	if len(preview.Message.Attachments) != 1 || preview.Message.Attachments[0].Name != "inline-only.png" || !preview.Message.Attachments[0].Inline || preview.Message.Attachments[0].Size != 320 {
		t.Fatalf("preview attachments = %+v", preview.Message.Attachments)
	}
	human := h.runNoBrowser("draft", "send", "draft-inline-only", "--dry-run")
	if !strings.Contains(human.Stdout, "Attached: inline-only.png (320 bytes, inline)") {
		t.Fatalf("human preview:\n%s", human.Stdout)
	}
}

// A changed inline image changes the approval code.
func TestOutlookInlineImageIsPartOfTheApproval(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var preview sendPreview
	decodeOK(t, h.runNoBrowser("draft", "send", "draft-inline-only", "--dry-run", "--json"), &preview)
	h.server.mu.Lock()
	h.server.attachmentState["draft-inline-only"] = []map[string]any{
		graphAttachmentFixtureTyped("inline-only", "inline-only.png", "image/png", 321, true, "cid-inline-only", "#microsoft.graph.fileAttachment"),
	}
	h.server.mu.Unlock()
	assertErrorCode(t, runPrinted(t, h.runNoBrowser, preview.SendCommand), errs.CodeApprovalMismatch)
}

// Outlook writes a reply's recipients itself. The preview saves the reply
// as a draft and shows that real draft, so the allow list sees the real
// recipients before anything is sent.
func TestOutlookReplySendChecksTheDraftOutlookBuilt(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	decodeOK(t, h.runNoBrowser("config", "set", "policy.allow_recipients", "reply@example.test", "--account", "work"), nil)
	res := h.runNoBrowser("reply", "read-id", "--body", "test", "--quote", "none", "--send", "--dry-run", "--json")
	assertErrorCodeExit(t, res, errs.CodePolicyDenied, 4)
	if hasRequest(h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail") {
		t.Fatal("reply was sent to recipients outside the allow list")
	}
	if !strings.Contains(res.Stderr, "reply-read-id") {
		t.Fatalf("error does not name the saved draft: %s", res.Stderr)
	}
}

// The reviewed case: the preview of an Outlook reply used to show the
// Reply-To address while Outlook sent to the sender with an extra Cc. Now
// the preview shows the draft Outlook made, and its command sends exactly
// that draft.
func TestOutlookReplyPreviewShowsTheRealRecipients(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var preview sendPreview
	decodeOK(t, h.runNoBrowser("reply", "read-id", "--body", "test", "--quote", "none", "--send", "--dry-run", "--json"), &preview)
	if !preview.DraftSaved || preview.Message.DraftID != "reply-read-id" || len(preview.Message.To) != 1 || preview.Message.To[0].Address != "source@example.test" ||
		len(preview.Message.CC) != 1 || preview.Message.CC[0].Address != "copy@example.test" {
		t.Fatalf("preview = %+v", preview)
	}
	if !strings.HasPrefix(preview.SendCommand, "maildo draft send ") || !strings.Contains(preview.SendCommand, "--id reply-read-id --approve "+preview.ApprovalCode+" --yes") {
		t.Fatalf("send command = %s", preview.SendCommand)
	}
	var sent draftWriteResult
	decodeOK(t, runPrinted(t, h.runNoBrowser, preview.SendCommand), &sent)
	body := string(requestFor(t, h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail").Body)
	if !strings.Contains(body, "source@example.test") || !strings.Contains(body, "copy@example.test") || !strings.Contains(body, "String 0x1042") {
		t.Fatalf("sendMail body = %s", body)
	}
	human := h.runNoBrowser("reply", "read-id", "--body", "test", "--quote", "none", "--send", "--dry-run")
	for _, want := range []string{"Outlook writes replies and forwards itself, so maildo saved this one as a draft", "To:       Source Sender <source@example.test>", "maildo draft send ", "If you do not want to send it, delete the draft with: maildo delete --account work --id reply-read-id"} {
		if !strings.Contains(human.Stdout, want) {
			t.Fatalf("human preview lacks %q:\n%s", want, human.Stdout)
		}
	}
}

// A reply that cannot be approved is refused before Outlook makes a draft.
func TestOutlookReplySendWithoutApprovalWritesNothing(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	for _, args := range [][]string{
		{"reply", "read-id", "--body", "test", "--send", "--yes", "--json"},
		{"reply", "read-id", "--body", "test", "--send", "--json"},
		{"forward", "read-id", "--to", "anna@example.test", "--body", "FYI", "--send", "--yes", "--json"},
	} {
		h.server.resetMessageRequests()
		assertErrorCode(t, h.runNoBrowser(args...), errs.CodeConfirmationRequired)
		if requests := h.server.messageRequestsSnapshot(); hasWriteRequest(requests) {
			t.Fatalf("%v wrote: %#v", args, requests)
		}
	}
}

// A draft send whose response is lost may have gone out. The error says so,
// so nobody retries into a second copy.
func TestOutlookDraftSendLostResponseIsUnknown(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	h.server.messageSendStatus.Store(http.StatusGatewayTimeout)
	res := approvedRun(t, h.runNoBrowser, "draft", "send", "draft-send", "--json")
	if res.Exit == 0 {
		t.Fatal("wanted an error")
	}
	for _, want := range []string{`"outcome": "unknown"`, `"retry_safe": false`, `"write_may_have_landed": true`, `"operation": "outlook_draft_send"`, "Sent folder"} {
		if !strings.Contains(res.Stderr, want) {
			t.Fatalf("missing %s in %s", want, res.Stderr)
		}
	}
}

// Outlook builds a forward on its side. maildo checks the draft Outlook
// made, so a recipient that appears there is caught before the send.
func TestOutlookForwardSendChecksTheDraftOutlookBuilt(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	decodeOK(t, h.runNoBrowser("config", "set", "policy.allow_recipients", "anna@example.test", "--account", "work"), nil)
	original := h.server.srv.Config.Handler
	h.server.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/me/messages/forward-read-id" {
			m := graphForwardDraftFixture("forward-read-id", r.Header.Get("Prefer"))
			m["toRecipients"] = []map[string]any{
				{"emailAddress": map[string]any{"address": "anna@example.test"}},
				{"emailAddress": map[string]any{"address": "eve@elsewhere.test"}},
			}
			writeCLIJSON(w, m)
			return
		}
		original.ServeHTTP(w, r)
	})
	res := h.runNoBrowser("forward", "read-id", "--to", "anna@example.test", "--body", "FYI", "--send", "--dry-run", "--json")
	assertErrorCodeExit(t, res, errs.CodePolicyDenied, 4)
	if hasRequest(h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail") {
		t.Fatal("forward was sent to a recipient outside the allow list")
	}
	if !strings.Contains(res.Stderr, "forward-read-id") {
		t.Fatalf("error does not name the saved draft: %s", res.Stderr)
	}
}

func TestOutlookForwardSendSaysSent(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	res := approvedRun(t, h.runNoBrowser, "forward", "read-id", "--to", "anna@example.test", "--body", "FYI", "--send")
	decodeOK(t, res, nil)
	if !hasRequest(h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail") || !strings.HasPrefix(res.Stdout, "Sent ") {
		t.Fatalf("stdout = %s", res.Stdout)
	}
}

// Replies and forwards that stay drafts are in the action log, and so is
// the draft a refused send leaves behind.
func TestOutlookReplyAndForwardDraftsAreLogged(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var reply replyResult
	decodeOK(t, h.runNoBrowser("reply", "read-id", "--body", "Thanks", "--json"), &reply)
	var forward forwardResult
	decodeOK(t, h.runNoBrowser("forward", "read-id", "--to", "anna@example.test", "--body", "FYI", "--json"), &forward)
	if !reply.ActionLogStatus.Written || !forward.ActionLogStatus.Written {
		t.Fatalf("action log status reply=%+v forward=%+v", reply.ActionLogStatus, forward.ActionLogStatus)
	}
	decodeOK(t, h.runNoBrowser("config", "set", "policy.allow_recipients", "nobody@example.test", "--account", "work"), nil)
	assertErrorCodeExit(t, h.runNoBrowser("reply", "read-id", "--body", "Refused", "--send", "--dry-run", "--json"), errs.CodePolicyDenied, 4)
	data, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, `"operation":"draft_create"`) != 3 || !strings.Contains(log, `"command":"reply"`) || !strings.Contains(log, `"command":"forward"`) ||
		!strings.Contains(log, `"id":"reply-read-id"`) || !strings.Contains(log, `"id":"forward-read-id"`) || strings.Contains(log, "Thanks") {
		t.Fatalf("action log:\n%s", log)
	}
}

// maildo send --attach works on Outlook for files up to 3 MB together, and
// refuses more with a message that says what to do.
func TestOutlookSendWithAttachment(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	file := filepath.Join(h.root, "invoice.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4 test"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview := h.runNoBrowser("send", "--to", "client@example.test", "--subject", "Invoice", "--body", "Attached.", "--attach", file, "--dry-run")
	if preview.Exit != 0 || !strings.Contains(preview.Stdout, "Attached: invoice.pdf (13 bytes)") {
		t.Fatalf("preview: %s%s", preview.Stdout, preview.Stderr)
	}
	decodeOK(t, approvedRun(t, h.runNoBrowser, "send", "--to", "client@example.test", "--subject", "Invoice", "--body", "Attached.", "--attach", file, "--json"), nil)
	message := decodeRequestBody(t, requestFor(t, h.server.messageRequestsSnapshot(), http.MethodPost, "/me/sendMail"))["message"].(map[string]any)
	if attachments, _ := message["attachments"].([]any); len(attachments) != 1 || attachments[0].(map[string]any)["name"] != "invoice.pdf" {
		t.Fatalf("sendMail attachments = %#v", message["attachments"])
	}

	// Each file is below the per-file limit; together they are too much
	// for one sendMail request.
	var args []string
	for _, name := range []string{"part1.bin", "part2.bin"} {
		path := filepath.Join(h.root, name)
		if err := os.WriteFile(path, make([]byte, 2<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--attach", path)
	}
	res := h.runNoBrowser(append([]string{"send", "--to", "client@example.test", "--subject", "Big", "--body", "x", "--dry-run"}, args...)...)
	if res.Exit == 0 || !strings.Contains(res.Stderr, "at most 3 MB of attachments on Outlook") || !strings.Contains(res.Stderr, "bigger files from Outlook itself") {
		t.Fatalf("big attachment: exit %d %s", res.Exit, res.Stderr)
	}
}
