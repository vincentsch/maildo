package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad/testutil"
)

// approvedRun sends the way AGENTS.md tells an agent to: it runs the
// command with --dry-run, takes the approval code from the preview, then
// runs the command the preview printed. args may contain --yes and output
// flags; the preview always runs with --json. When the preview fails, its
// result comes back.
func approvedRun(t *testing.T, run func(...string) testutil.Result, args ...string) testutil.Result {
	t.Helper()
	var preview, output []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes":
		case "--json", "--include-meta", "--plain":
			output = append(output, args[i])
		case "--jq", "--template":
			if i+1 < len(args) {
				output = append(output, args[i], args[i+1])
				i++
			}
		default:
			preview = append(preview, args[i])
		}
	}
	res := run(append(preview, "--dry-run", "--json")...)
	if res.Exit != 0 {
		return res
	}
	var p struct {
		ApprovalCode string `json:"approval_code"`
		DraftSaved   bool   `json:"draft_saved"`
		SendCommand  string `json:"send_command"`
	}
	decodeOK(t, res, &p)
	if p.ApprovalCode == "" {
		t.Fatalf("preview has no approval code: %s", res.Stdout)
	}
	if p.DraftSaved {
		words := shellWords(p.SendCommand)
		var cleaned []string
		for _, w := range words[1:] {
			if w != "--json" {
				cleaned = append(cleaned, w)
			}
		}
		return run(append(cleaned, output...)...)
	}
	send := append(append(preview, output...), "--approve", p.ApprovalCode, "--yes")
	return run(send...)
}

// shellWords splits a command maildo printed. It understands the quoting
// shellQuote writes: single quotes, with '"'"' for a quote inside.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	inWord, single, double := false, false, false
	for _, r := range s {
		switch {
		case single:
			if r == '\'' {
				single = false
			} else {
				cur.WriteRune(r)
			}
		case double:
			if r == '"' {
				double = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'':
			single, inWord = true, true
		case r == '"':
			double, inWord = true, true
		case r == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// runPrinted runs a command a preview printed, without the leading
// "maildo".
func runPrinted(t *testing.T, run func(...string) testutil.Result, command string, extra ...string) testutil.Result {
	t.Helper()
	words := shellWords(command)
	if len(words) == 0 || words[0] != "maildo" {
		t.Fatalf("printed command = %q", command)
	}
	return run(append(words[1:], extra...)...)
}

// The reviewed case: a draft changed between the preview and the printed
// command. The command refuses, and nothing is sent.
func TestDraftChangedAfterThePreviewIsNotSent(t *testing.T) {
	h, fake := seededDraftHarness(t)
	var preview sendPreview
	decodeOK(t, h.run("draft", "send", "--id", "seed-draft", "--dry-run", "--json"), &preview)
	decodeOK(t, h.run("draft", "update", "--id", "seed-draft", "--to", "unapproved@example.test", "--json"), nil)
	res := runPrinted(t, h.run, preview.SendCommand, "--json")
	assertErrorCode(t, res, errs.CodeApprovalMismatch)
	if fake.Drafts.SendCalls.Load() != 0 {
		t.Fatal("a changed draft was sent")
	}
	// The same code is refused for a new message too.
	res = h.run("send", "--to", "client@example.test", "--subject", "Other", "--body", "x", "--approve", preview.ApprovalCode, "--yes", "--json")
	assertErrorCode(t, res, errs.CodeApprovalMismatch)
	if fake.Drafts.DirectCalls.Load() != 0 {
		t.Fatal("a message with another message's code was sent")
	}
}

// The reviewed case: the active account changed between the preview and
// the printed command. The command still sends from the account the
// preview showed.
func TestPrintedSendCommandKeepsThePreviewedAccount(t *testing.T) {
	h, _ := seededDraftHarness(t)
	seedPersonalAccount(t, h)
	decodeOK(t, h.run("auth", "switch", "work", "--json"), nil)
	var preview sendPreview
	decodeOK(t, h.run("send", "--to", "client@example.test", "--subject", "Quote", "--body", "Hi", "--dry-run", "--json"), &preview)
	if preview.Account != "work" || !strings.Contains(preview.SendCommand, "--account work") {
		t.Fatalf("preview = %+v", preview)
	}
	decodeOK(t, h.run("auth", "switch", "personal", "--json"), nil)
	var sent directSendResult
	decodeOK(t, runPrinted(t, h.run, preview.SendCommand), &sent)
	if sent.Account != "work" {
		t.Fatalf("sent from %q, previewed work", sent.Account)
	}
	var draftPreview sendPreview
	decodeOK(t, h.run("draft", "send", "--id", "seed-draft", "--account", "work", "--dry-run", "--json"), &draftPreview)
	if !strings.Contains(draftPreview.SendCommand, "--account work") {
		t.Fatalf("draft send command = %s", draftPreview.SendCommand)
	}
}

// The reviewed case: the preview cut the body at 200 characters, so a
// private paragraph at the end went out unseen. JSON now has the whole
// body; the human preview shows long bodies up to a limit, says how much
// it left out, and shows all of it with --full.
func TestSendPreviewShowsTheWholeBody(t *testing.T) {
	h, _ := seededDraftHarness(t)
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d of the quote", i))
	}
	lines = append(lines, "PRIVATE TRAILING PARAGRAPH")
	body := strings.Join(lines, "\n")
	args := []string{"send", "--to", "client@example.test", "--subject", "Quote", "--body", body, "--dry-run"}
	var preview sendPreview
	decodeOK(t, h.run(append(args, "--json")...), &preview)
	if preview.Message.Body != body {
		t.Fatalf("JSON body = %q", preview.Message.Body)
	}
	human := h.run(args...)
	if !strings.Contains(human.Stdout, "  line 40 of the quote\n") || strings.Contains(human.Stdout, "line 41 of") ||
		!strings.Contains(human.Stdout, "11 more lines of the body are not shown. Add --full to see all of it.") {
		t.Fatalf("human preview:\n%s", human.Stdout)
	}
	full := h.run(append(args, "--full")...)
	if !strings.Contains(full.Stdout, "  PRIVATE TRAILING PARAGRAPH\n") || strings.Contains(full.Stdout, "not shown") {
		t.Fatalf("full preview:\n%s", full.Stdout)
	}
	short := h.run("send", "--to", "client@example.test", "--subject", "Quote", "--body", "Hi\n\nSee you.", "--dry-run")
	if !strings.Contains(short.Stdout, "  Hi\n\n  See you.\n") || strings.Contains(short.Stdout, "not shown") {
		t.Fatalf("short preview:\n%s", short.Stdout)
	}
}

// Every part of a message changes its approval code; the same message
// always gets the same code.
func TestApprovalCodeCoversTheWholeMessage(t *testing.T) {
	base := mail.SendContent{
		From:    mail.Recipient{Name: "Me", Address: "me@example.test"},
		To:      []mail.Recipient{{Address: "ann@example.test"}},
		Subject: "Quote",
		Text:    "Hello\r\nAnn",
		HTML:    "<p>Hello</p>",
		Attachments: []mail.SendAttachment{{Name: "quote.pdf", Size: 3, SHA256: [32]byte{1}},
			{Name: "logo.png", Size: 2, Inline: true, SHA256: [32]byte{2}}},
	}
	code := approvalCode("work", base)
	if len(code) != 8 || code != approvalCode("work", base) {
		t.Fatalf("code = %q", code)
	}
	same := base
	same.Text = "Hello\nAnn"
	if approvalCode("work", same) != code {
		t.Fatal("line endings changed the code")
	}
	changes := map[string]func(*mail.SendContent){
		"from":     func(c *mail.SendContent) { c.From.Address = "other@example.test" },
		"to":       func(c *mail.SendContent) { c.To = []mail.Recipient{{Address: "eve@example.test"}} },
		"to name":  func(c *mail.SendContent) { c.To = []mail.Recipient{{Name: "Eve", Address: "ann@example.test"}} },
		"cc":       func(c *mail.SendContent) { c.CC = []mail.Recipient{{Address: "ann@example.test"}} },
		"bcc":      func(c *mail.SendContent) { c.BCC = []mail.Recipient{{Address: "eve@example.test"}} },
		"reply-to": func(c *mail.SendContent) { c.ReplyTo = []mail.Recipient{{Address: "eve@example.test"}} },
		"subject":  func(c *mail.SendContent) { c.Subject = "Quote!" },
		"text":     func(c *mail.SendContent) { c.Text += " " },
		"html":     func(c *mail.SendContent) { c.HTML += " " },
		"attachment name": func(c *mail.SendContent) {
			c.Attachments = []mail.SendAttachment{{Name: "quote2.pdf", Size: 3, SHA256: [32]byte{1}}, c.Attachments[1]}
		},
		"attachment size": func(c *mail.SendContent) {
			c.Attachments = []mail.SendAttachment{{Name: "quote.pdf", Size: 4, SHA256: [32]byte{1}}, c.Attachments[1]}
		},
		"attachment data": func(c *mail.SendContent) {
			c.Attachments = []mail.SendAttachment{{Name: "quote.pdf", Size: 3, SHA256: [32]byte{9}}, c.Attachments[1]}
		},
		"inline dropped": func(c *mail.SendContent) { c.Attachments = c.Attachments[:1] },
	}
	for name, change := range changes {
		changed := base
		changed.Attachments = append([]mail.SendAttachment(nil), base.Attachments...)
		change(&changed)
		if approvalCode("work", changed) == code {
			t.Errorf("%s did not change the code", name)
		}
	}
	if approvalCode("personal", base) == code {
		t.Error("the account did not change the code")
	}
}

// With policy.unattended_send a script sends without a code, but a wrong
// code is still refused.
func TestUnattendedSendStillChecksAGivenCode(t *testing.T) {
	h := newSendHarness(t)
	if res := h.runInTerminal("y\n", "config", "set", "policy.unattended_send", "true", "--account", "posteo"); res.Exit != 0 {
		t.Fatalf("config set: %s", res.Stderr)
	}
	res := h.run("send", "--to", "ann@example.org", "--subject", "Report", "--body", "x", "--approve", "aaaaaaaa", "--yes", "--json")
	assertErrorCode(t, res, errs.CodeApprovalMismatch)
	if len(h.server.Sent()) != 0 {
		t.Fatal("a send with a wrong code went out")
	}
}

// IMAP drafts get a new id when they change, so the printed command of an
// old preview finds no draft and sends nothing.
func TestIMAPDraftChangedAfterThePreviewIsNotSent(t *testing.T) {
	h := newSendHarness(t)
	var created draftWriteResult
	if res := h.run("draft", "create", "--to", "ann@example.org", "--subject", "Quote", "--body", "First", "--json"); res.JSON(&created) != nil || created.DraftID == "" {
		t.Fatalf("draft create: %s%s", res.Stdout, res.Stderr)
	}
	var preview sendPreview
	decodeOK(t, h.run("draft", "send", "--id", created.DraftID, "--dry-run", "--json"), &preview)
	decodeOK(t, h.run("draft", "update", "--id", created.DraftID, "--to", "eve@example.org", "--json"), nil)
	res := runPrinted(t, h.run, preview.SendCommand)
	if res.Exit == 0 || len(h.server.Sent()) != 0 || !strings.Contains(res.Stderr, "maildo list Drafts") {
		t.Fatalf("old preview sent the changed draft: exit %d %s", res.Exit, res.Stderr)
	}
}

// Replies and forwards that stay drafts are in the action log, like draft
// create.
func TestReplyAndForwardDraftsAreLogged(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Kickoff\r\nMessage-ID: <kickoff@example.org>\r\n\r\nTuesday?\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	id := found.Messages[0].ID
	var reply replyResult
	decodeOK(t, h.run("reply", "--id", id, "--body", "Private reply text", "--json"), &reply)
	var forward forwardResult
	decodeOK(t, h.run("forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--json"), &forward)
	if !reply.ActionLogStatus.Written || !forward.ActionLogStatus.Written {
		t.Fatalf("action log status reply=%+v forward=%+v", reply.ActionLogStatus, forward.ActionLogStatus)
	}
	data, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{`"command":"reply"`, `"command":"forward"`, `"operation":"draft_create"`, `"name":"Re: Kickoff"`, `"name":"Fwd: Kickoff"`, `"id":"` + reply.DraftID + `"`} {
		if !strings.Contains(log, want) {
			t.Fatalf("action log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, "Private reply text") {
		t.Fatal("the action log has the body")
	}
}
