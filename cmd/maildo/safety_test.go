package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad/testutil"
)

func newSendHarness(t *testing.T) *mailServerHarness {
	t.Helper()
	h := newMailServerHarness(t, "me@posteo.de", "Sent", "Trash", "Archive", "Junk", "Drafts")
	if res := h.run("login", "--email", "me@posteo.de"); res.Exit != 0 {
		t.Fatalf("login: %s", res.Stderr)
	}
	return h
}

// runInTerminal runs a command as if a person typed it, answering prompts
// from input.
func (h *mailServerHarness) runInTerminal(input string, args ...string) testutil.Result {
	h.t.Helper()
	return testutil.RunWith(h.app(), testutil.Options{
		Stdin:       strings.NewReader(input),
		Terminal:    true,
		TerminalSet: true,
		LookupEnv: func(key string) (string, bool) {
			v, ok := h.env[key]
			return v, ok
		},
	}, append([]string{"--config", h.configPath}, args...)...)
}

// A send preview shows who gets the mail, the subject, the whole body and
// the approval code, and prints a command that pins the account.
func TestSendDryRunShowsRecipientsSubjectAndBody(t *testing.T) {
	h := newSendHarness(t)
	res := h.run("send", "--to", "Clara Client <clara@example.org>", "--subject", "Quote for October", "--body", "Hi Clara, here is the quote.", "--dry-run")
	if res.Exit != 0 {
		t.Fatalf("dry run: %s", res.Stderr)
	}
	for _, want := range []string{"Would send this message from posteo:", "To:       Clara Client <clara@example.org>",
		// Empty fields still get a line, so the preview says there are none.
		"  Cc:       (none)\n  Bcc:      (none)\n  Reply-To: (none)\n  Subject:  Quote for October\n  Attached: (none)\n\n  Hi Clara, here is the quote.\n",
		// The login gave no name, so the preview says how to add one.
		"The From line has only your address and no name. To add your name, run:\n  maildo config set sender_name \"Your Name\" --account posteo\n",
		"Nothing was sent. The approval code for this message is ", "--account posteo", "--approve ", "--yes"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("preview lacks %q:\n%s", want, res.Stdout)
		}
	}
	if len(h.server.Sent()) != 0 {
		t.Fatal("dry run sent mail")
	}
	if res := h.run("config", "set", "sender_name", "Me Tester", "--account", "posteo"); res.Exit != 0 {
		t.Fatalf("config set: %s", res.Stderr)
	}
	named := h.run("send", "--to", "clara@example.org", "--subject", "Quote", "--body", "Hi", "--dry-run")
	if !strings.Contains(named.Stdout, "From:     Me Tester <me@posteo.de>") || strings.Contains(named.Stdout, "no name") {
		t.Fatalf("preview with a sender name:\n%s", named.Stdout)
	}
}

// A reply preview lists every field a person checks, with empty lists as
// [], and the Message-IDs that keep the reply in its conversation. The mail
// that goes out has the same headers.
func TestReplyPreviewShowsEveryFieldAndTheThread(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Kickoff\r\nMessage-ID: <second@example.org>\r\nIn-Reply-To: <first@example.org>\r\nReferences: <first@example.org>\r\n\r\nTuesday?\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	args := []string{"reply", "--id", found.Messages[0].ID, "--body", "Tuesday works.", "--send"}
	res := h.run(append(args, "--dry-run", "--json")...)
	var preview struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	decodeOK(t, res, &preview)
	for _, key := range []string{"cc", "bcc", "reply_to", "attachments"} {
		if got := string(preview.Message[key]); got != "[]" {
			t.Errorf("message.%s = %s, want []", key, got)
		}
	}
	if got := string(preview.Message["in_reply_to"]); got != `"<second@example.org>"` {
		t.Errorf("message.in_reply_to = %s", got)
	}
	var refs []string
	if err := json.Unmarshal(preview.Message["references"], &refs); err != nil || strings.Join(refs, " ") != "<first@example.org> <second@example.org>" {
		t.Errorf("message.references = %s", preview.Message["references"])
	}
	human := h.run(append(args, "--dry-run")...)
	if !strings.Contains(human.Stdout, "  Thread:   replies to <second@example.org>\n") {
		t.Fatalf("human preview:\n%s", human.Stdout)
	}
	if sent := approvedRun(t, h.run, args...); sent.Exit != 0 {
		t.Fatalf("send: %s%s", sent.Stdout, sent.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 || !strings.Contains(string(mails[0].Data), "In-Reply-To: <second@example.org>") || !strings.Contains(string(mails[0].Data), "References: <first@example.org> <second@example.org>") {
		t.Fatalf("sent mail = %+v", mails)
	}
}

// Without a terminal, sending needs --yes with the approval code from the
// preview. Then the mail goes out with the display name from --from and
// lands in the action log.
func TestSendNeedsConfirmation(t *testing.T) {
	h := newSendHarness(t)
	args := []string{"send", "--to", "clara@example.org", "--subject", "Quote", "--body", "Private body text", "--from", "Me Tester <me@posteo.de>"}

	refused := h.run(append(args, "--json")...)
	if refused.Exit == 0 || !strings.Contains(refused.Stderr, errs.CodeConfirmationRequired) || !strings.Contains(refused.Stderr, "--dry-run") {
		t.Fatalf("send without --yes: exit %d %s", refused.Exit, refused.Stderr)
	}
	yesOnly := h.run(append(args, "--yes", "--json")...)
	if yesOnly.Exit == 0 || !strings.Contains(yesOnly.Stderr, errs.CodeConfirmationRequired) || !strings.Contains(yesOnly.Stderr, "--approve") {
		t.Fatalf("send with --yes but no --approve: exit %d %s", yesOnly.Exit, yesOnly.Stderr)
	}
	if len(h.server.Sent()) != 0 {
		t.Fatal("mail went out without confirmation")
	}

	sent := approvedRun(t, h.run, args...)
	if sent.Exit != 0 || !strings.Contains(sent.Stdout, `Sent "Quote" to clara@example.org from posteo.`) {
		t.Fatalf("send --yes: exit %d %s%s", sent.Exit, sent.Stdout, sent.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 || !strings.Contains(string(mails[0].Data), "From: \"Me Tester\" <me@posteo.de>") {
		t.Fatalf("sent mail = %+v", mails)
	}
	logged, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
	if err != nil || !strings.Contains(string(logged), `"to":["clara@example.org"]`) || !strings.Contains(string(logged), `"subject":"Quote"`) || strings.Contains(string(logged), "Private body text") {
		t.Fatalf("action log = %s (%v)", logged, err)
	}
}

func TestSendAsksInATerminal(t *testing.T) {
	h := newSendHarness(t)
	args := []string{"send", "--to", "clara@example.org", "--subject", "Quote", "--body", "Hello"}
	declined := h.runInTerminal("n\n", args...)
	if declined.Exit == 0 || len(h.server.Sent()) != 0 || !strings.Contains(declined.Stderr, "About to send this message from posteo:") {
		t.Fatalf("declined: exit %d stderr %s", declined.Exit, declined.Stderr)
	}
	confirmed := h.runInTerminal("y\n", args...)
	if confirmed.Exit != 0 || len(h.server.Sent()) != 1 {
		t.Fatalf("confirmed: exit %d stderr %s", confirmed.Exit, confirmed.Stderr)
	}
}

func TestPoliciesLimitWhatAnAccountMayDo(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: a@example.org\r\nSubject: One\r\n\r\nbody\r\n", time.Now())
	h.server.Append("INBOX", "From: b@example.org\r\nSubject: Two\r\n\r\nbody\r\n", time.Now())

	t.Run("allow_recipients", func(t *testing.T) {
		if res := h.run("config", "set", "policy.allow_recipients", "@example.org", "--account", "posteo"); res.Exit != 0 {
			t.Fatalf("config set: %s", res.Stderr)
		}
		outside := h.run("send", "--to", "someone@example.net", "--subject", "x", "--body", "x", "--yes", "--json")
		if outside.Exit == 0 || !strings.Contains(outside.Stderr, errs.CodePolicyDenied) || !strings.Contains(outside.Stderr, "someone@example.net") {
			t.Fatalf("send outside the allow list: exit %d %s", outside.Exit, outside.Stderr)
		}
		inside := approvedRun(t, h.run, "send", "--to", "ann@example.org", "--subject", "x", "--body", "x")
		if inside.Exit != 0 {
			t.Fatalf("send inside the allow list: %s", inside.Stderr)
		}
	})

	t.Run("max_bulk", func(t *testing.T) {
		if res := h.run("config", "set", "policy.max_bulk", "1", "--account", "posteo"); res.Exit != 0 {
			t.Fatalf("config set: %s", res.Stderr)
		}
		res := h.run("archive", "--query", "body", "--all", "--json")
		if res.Exit == 0 || !strings.Contains(res.Stderr, "at most 1 messages") {
			t.Fatalf("bulk over the limit: exit %d %s", res.Exit, res.Stderr)
		}
	})

	t.Run("drafts_only", func(t *testing.T) {
		if res := h.run("config", "set", "policy.drafts_only", "true", "--account", "posteo"); res.Exit != 0 {
			t.Fatalf("config set: %s", res.Stderr)
		}
		for _, args := range [][]string{
			{"send", "--to", "ann@example.org", "--subject", "x", "--body", "x", "--yes", "--json"},
			{"archive", "--query", "One", "--latest", "--json"},
			{"folders", "create", "Projects", "--json"},
		} {
			res := h.run(args...)
			if res.Exit == 0 || !strings.Contains(res.Stderr, errs.CodePolicyDenied) {
				t.Fatalf("%v under drafts_only: exit %d %s", args, res.Exit, res.Stderr)
			}
		}
		if res := h.run("list", "--json"); res.Exit != 0 {
			t.Fatalf("list under drafts_only: %s", res.Stderr)
		}
	})

	t.Run("read_only", func(t *testing.T) {
		if res := h.run("config", "set", "policy.read_only", "true", "--account", "posteo"); res.Exit != 0 {
			t.Fatalf("config set: %s", res.Stderr)
		}
		res := h.run("mark", "read", "--query", "One", "--latest")
		if res.Exit == 0 || !strings.Contains(res.Stderr, "this account is read-only") || strings.Contains(res.Stderr, "policy.read_only false") || !strings.Contains(res.Stderr, "Only the account's owner") {
			t.Fatalf("mark read under read_only: exit %d %s", res.Exit, res.Stderr)
		}
		// Loosening a policy needs a person at a terminal.
		if res := h.run("config", "set", "policy.read_only", "false", "--account", "posteo"); res.Exit == 0 {
			t.Fatal("a script turned read_only off")
		}
	})
	if n := len(h.server.Sent()); n != 1 {
		t.Fatalf("sent %d messages, want the one inside the allow list", n)
	}
}

func TestUnattendedSendPolicySkipsTheConfirmation(t *testing.T) {
	h := newSendHarness(t)
	if res := h.runInTerminal("y\n", "config", "set", "policy.unattended_send", "true", "--account", "posteo"); res.Exit != 0 {
		t.Fatalf("config set: %s", res.Stderr)
	}
	if res := h.run("send", "--to", "ann@example.org", "--subject", "Report", "--body", "x"); res.Exit != 0 {
		t.Fatalf("unattended send: %s", res.Stderr)
	}
	if len(h.server.Sent()) != 1 {
		t.Fatal("nothing was sent")
	}
}

// The command a reply preview prints answers the message the preview
// showed, even when a newer message matches the same search later.
func TestReplyPreviewCommandNamesTheMessage(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara <clara@client.test>\r\nTo: me@posteo.de\r\nSubject: Kickoff\r\nMessage-ID: <first@client.test>\r\n\r\nCan we start Monday?\r\n", time.Now().Add(-time.Hour))
	var preview struct {
		SendCommand string `json:"send_command"`
	}
	decodeOK(t, h.run("reply", "--query", "from:clara", "--latest", "--body", "Monday.", "--send", "--dry-run", "--json"), &preview)
	if strings.Contains(preview.SendCommand, "--query") || strings.Contains(preview.SendCommand, "--latest") ||
		!strings.Contains(preview.SendCommand, "--account posteo") || !strings.Contains(preview.SendCommand, "--id imap-message/") {
		t.Fatalf("send command = %s", preview.SendCommand)
	}
	h.server.Append("INBOX", "From: Clara <clara@client.test>\r\nTo: me@posteo.de\r\nSubject: Something else\r\nMessage-ID: <second@client.test>\r\n\r\nNew mail.\r\n", time.Now())
	args := strings.Fields(preview.SendCommand)[1:]
	decodeOK(t, h.run(args...), nil)
	sent := h.server.Sent()
	if len(sent) != 1 || !strings.Contains(string(sent[0].Data), "In-Reply-To: <first@client.test>") {
		t.Fatalf("reply went to the wrong message: %q", sent)
	}
}
