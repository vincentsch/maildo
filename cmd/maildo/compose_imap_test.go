package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/rungrad/testutil"
)

// IMAP accounts write drafts into the Drafts folder, change them by saving a
// new version, and send them over SMTP with a copy in Sent.
func TestIMAPDraftLifecycle(t *testing.T) {
	h := newSendHarness(t)
	var created draftWriteResult
	res := h.run("draft", "create", "--to", "clara@example.org", "--bcc", "boss@example.org", "--subject", "Quote", "--body", "First version", "--json")
	if err := res.JSON(&created); err != nil || res.Exit != 0 || created.DraftID == "" {
		t.Fatalf("draft create: exit %d %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	drafts := h.server.Fetch("Drafts")
	if len(drafts) != 1 || !strings.Contains(string(drafts[0]), "Bcc: boss@example.org") {
		t.Fatalf("Drafts folder = %q", drafts)
	}

	read := h.run("draft", "read", "--id", created.DraftID)
	if read.Exit != 0 || !strings.Contains(read.Stdout, "First version") {
		t.Fatalf("draft read: %s%s", read.Stdout, read.Stderr)
	}

	var updated draftWriteResult
	res = h.run("draft", "update", "--id", created.DraftID, "--subject", "Quote for October", "--json")
	if err := res.JSON(&updated); err != nil || res.Exit != 0 || updated.DraftID == created.DraftID {
		t.Fatalf("draft update: exit %d %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	if drafts := h.server.Fetch("Drafts"); len(drafts) != 1 || !strings.Contains(string(drafts[0]), "Quote for October") {
		t.Fatalf("after update, Drafts = %q", drafts)
	}

	// The human output gives a short id that the next command accepts.
	file := filepath.Join(h.root, "notes.txt")
	if err := os.WriteFile(file, []byte("line items"), 0o600); err != nil {
		t.Fatal(err)
	}
	add := h.run("attachments", "add", "--draft-id", updated.DraftID, file, "--json")
	if add.Exit != 0 {
		t.Fatalf("attachments add: %s%s", add.Stdout, add.Stderr)
	}
	var added struct {
		DraftID string `json:"draft_id"`
	}
	if err := add.JSON(&added); err != nil || added.DraftID == "" {
		t.Fatalf("attachments add result: %s", add.Stdout)
	}

	sent := approvedRun(t, h.run, "draft", "send", "--id", added.DraftID)
	if sent.Exit != 0 || !strings.Contains(sent.Stdout, "A copy is in Sent.") {
		t.Fatalf("draft send: exit %d %s%s", sent.Exit, sent.Stdout, sent.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 || len(mails[0].To) != 2 || strings.Contains(string(mails[0].Data), "Bcc:") || !strings.Contains(string(mails[0].Data), `filename="notes.txt"`) {
		t.Fatalf("sent = %+v\n%s", mails, mails[0].Data)
	}
	if drafts := h.server.Fetch("Drafts"); len(drafts) != 0 {
		t.Fatalf("the sent draft is still in Drafts")
	}
	if copies := h.server.Fetch("Sent"); len(copies) != 1 {
		t.Fatalf("Sent has %d messages, want the copy", len(copies))
	}
}

func TestIMAPSendWithAttachmentKeepsACopy(t *testing.T) {
	h := newSendHarness(t)
	file := filepath.Join(h.root, "invoice.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4 test"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview := h.run("send", "--to", "ann@example.org", "--subject", "Invoice", "--body", "Attached.", "--attach", file, "--dry-run")
	if preview.Exit != 0 || !strings.Contains(preview.Stdout, "Attached: invoice.pdf (13 bytes)") {
		t.Fatalf("preview: %s%s", preview.Stdout, preview.Stderr)
	}
	res := approvedRun(t, h.run, "send", "--to", "ann@example.org", "--subject", "Invoice", "--body", "Attached.", "--attach", file)
	if res.Exit != 0 {
		t.Fatalf("send: %s%s", res.Stdout, res.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 || !strings.Contains(string(mails[0].Data), `filename="invoice.pdf"`) {
		t.Fatalf("sent = %+v", mails)
	}
	if copies := h.server.Fetch("Sent"); len(copies) != 1 || !strings.Contains(string(copies[0]), "Subject: Invoice") {
		t.Fatalf("Sent folder = %q", copies)
	}
}

func TestIMAPReplyAndForward(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nCc: team@example.org\r\nSubject: Project kickoff\r\nMessage-ID: <kickoff@example.org>\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nShall we start on Tuesday?\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=plan.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n--b--\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	id := found.Messages[0].ID

	preview := h.run("reply", "--id", id, "--reply-all", "--body", "Tuesday works.", "--send", "--dry-run")
	if preview.Exit != 0 || !strings.Contains(preview.Stdout, "To:       Clara Client <clara@example.org>") || !strings.Contains(preview.Stdout, "Cc:       team@example.org") {
		t.Fatalf("reply preview: %s%s", preview.Stdout, preview.Stderr)
	}
	reply := approvedRun(t, h.run, "reply", "--id", id, "--reply-all", "--body", "Tuesday works.", "--send")
	if reply.Exit != 0 {
		t.Fatalf("reply: %s%s", reply.Stdout, reply.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 {
		t.Fatalf("sent %d", len(mails))
	}
	data := string(mails[0].Data)
	for _, want := range []string{"In-Reply-To: <kickoff@example.org>", "References: <kickoff@example.org>", "Subject: Re: Project kickoff", "Tuesday works.", "> Shall we start on Tuesday?"} {
		if !strings.Contains(data, want) {
			t.Fatalf("reply lacks %q:\n%s", want, data)
		}
	}

	draft := h.run("forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--include-attachments", "all")
	if draft.Exit != 0 || !strings.Contains(draft.Stdout, `Saved the draft "Fwd: Project kickoff"`) {
		t.Fatalf("forward draft: %s%s", draft.Stdout, draft.Stderr)
	}
	drafts := h.server.Fetch("Drafts")
	if len(drafts) != 1 || !strings.Contains(string(drafts[0]), "plan.pdf") {
		t.Fatalf("Drafts = %q", drafts)
	}
}

// Reply and forward use the account's sender name in From, like send.
func TestIMAPReplyAndForwardUseSenderName(t *testing.T) {
	h := newSendHarness(t)
	if res := h.run("config", "set", "sender_name", "Me Tester"); res.Exit != 0 {
		t.Fatalf("config set: %s%s", res.Stdout, res.Stderr)
	}
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Project kickoff\r\nMessage-ID: <kickoff@example.org>\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\n\r\nShall we start on Tuesday?\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	id := found.Messages[0].ID
	for _, args := range [][]string{
		{"reply", "--id", id, "--body", "Tuesday works.", "--send"},
		{"forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--send"},
	} {
		preview := h.run(append(args, "--dry-run")...)
		if preview.Exit != 0 || !strings.Contains(preview.Stdout, "From:     Me Tester <me@posteo.de>") {
			t.Fatalf("%s preview: %s%s", args[0], preview.Stdout, preview.Stderr)
		}
		if res := approvedRun(t, h.run, args...); res.Exit != 0 {
			t.Fatalf("%s: %s%s", args[0], res.Stdout, res.Stderr)
		}
	}
	var created draftWriteResult
	if res := h.run("draft", "create", "--to", "ann@example.org", "--subject", "Notes", "--body", "Hi", "--json"); res.JSON(&created) != nil || created.DraftID == "" {
		t.Fatalf("draft create: %s%s", res.Stdout, res.Stderr)
	}
	if res := approvedRun(t, h.run, "draft", "send", "--id", created.DraftID); res.Exit != 0 {
		t.Fatalf("draft send: %s%s", res.Stdout, res.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 3 {
		t.Fatalf("sent %d", len(mails))
	}
	for _, m := range mails {
		if !strings.Contains(string(m.Data), `From: "Me Tester" <me@posteo.de>`) {
			t.Fatalf("From lacks the sender name:\n%s", m.Data)
		}
	}
}

// The message that goes out never names its blind copies, but the copy in
// Sent does, so the sender still knows whom they sent one to.
func TestIMAPSentCopyKeepsBcc(t *testing.T) {
	h := newSendHarness(t)
	res := approvedRun(t, h.run, "send", "--to", "ann@example.org", "--bcc", "audit@example.org", "--subject", "Quote", "--body", "Hi")
	if res.Exit != 0 || !strings.Contains(res.Stdout, `Sent "Quote" to ann@example.org (Bcc: audit@example.org) from posteo.`) {
		t.Fatalf("send: %s%s", res.Stdout, res.Stderr)
	}
	var created draftWriteResult
	if res := h.run("draft", "create", "--to", "ann@example.org", "--bcc", "boss@example.org", "--subject", "Draft", "--body", "Hi", "--json"); res.JSON(&created) != nil || created.DraftID == "" {
		t.Fatalf("draft create: %s%s", res.Stdout, res.Stderr)
	}
	if res := approvedRun(t, h.run, "draft", "send", "--id", created.DraftID); res.Exit != 0 {
		t.Fatalf("draft send: %s%s", res.Stdout, res.Stderr)
	}
	for _, m := range h.server.Sent() {
		if strings.Contains(string(m.Data), "Bcc:") {
			t.Fatalf("transmitted message names its Bcc:\n%s", m.Data)
		}
	}
	copies := h.server.Fetch("Sent")
	if len(copies) != 2 || !strings.Contains(string(copies[0]), "Bcc: audit@example.org") || !strings.Contains(string(copies[1]), "Bcc: boss@example.org") {
		t.Fatalf("Sent copies = %q", copies)
	}
}

// A send to many people used to fail, because all addresses went on one
// header line. More addresses than maildo puts in one message get an error
// that says so.
func TestSendToManyRecipients(t *testing.T) {
	h := newSendHarness(t)
	var to []string
	for i := 0; i < 80; i++ {
		to = append(to, fmt.Sprintf("user%d@client.example", i))
	}
	decodeOK(t, approvedRun(t, h.run, "send", "--to", strings.Join(to, ","), "--subject", "Many", "--body", "Hi all", "--json"), nil)
	human := approvedRun(t, h.run, "send", "--to", strings.Join(to, ","), "--subject", "Many again", "--body", "Hi all")
	if human.Exit != 0 || !strings.Contains(human.Stdout, "user2@client.example and 77 more from posteo") {
		t.Fatalf("human result: %s%s", human.Stdout, human.Stderr)
	}
	sent := h.server.Sent()
	if len(sent) != 2 || len(sent[0].To) != 80 {
		t.Fatalf("sent %d messages, first to %d people", len(sent), len(sent[0].To))
	}
	for i := 80; i < 210; i++ {
		to = append(to, fmt.Sprintf("user%d@client.example", i))
	}
	res := h.run("send", "--to", strings.Join(to, ","), "--subject", "Too many", "--body", "Hi all", "--dry-run")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "too many addresses") {
		t.Fatalf("send to 210 people: exit %d\n%s", res.Exit, res.Stderr)
	}
}

// The result of a draft send counts the draft's attachments.
func TestDraftSendResultCountsAttachments(t *testing.T) {
	h := newSendHarness(t)
	var created draftWriteResult
	decodeOK(t, h.run("draft", "create", "--to", "ann@example.org", "--subject", "Notes", "--body", "See the file.", "--json"), &created)
	file := filepath.Join(h.root, "notes.txt")
	if err := os.WriteFile(file, []byte("line items"), 0o600); err != nil {
		t.Fatal(err)
	}
	var added struct {
		DraftID string `json:"draft_id"`
	}
	decodeOK(t, h.run("attachments", "add", "--draft-id", created.DraftID, file, "--json"), &added)
	var sent draftWriteResult
	decodeOK(t, approvedRun(t, h.run, "draft", "send", "--id", added.DraftID, "--json"), &sent)
	if sent.AttachmentCount != 1 {
		t.Fatalf("attachment_count = %d, want 1", sent.AttachmentCount)
	}
}

// thread shows the start of each message and says how to read one.
func TestThreadSaysHowToReadAMessage(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Kickoff\r\nMessage-ID: <k1@example.org>\r\n\r\nTuesday?\r\n", time.Now().Add(-time.Hour))
	h.server.Append("INBOX", "From: Clara <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Re: Kickoff\r\nMessage-ID: <k2@example.org>\r\nIn-Reply-To: <k1@example.org>\r\nReferences: <k1@example.org>\r\n\r\nOr Wednesday.\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "subject:kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 2 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	res := h.run("thread", found.Messages[0].ID)
	if res.Exit != 0 || !strings.Contains(res.Stdout, "Or Wednesday.") || !strings.Contains(res.Stdout, "To read a whole message, run: maildo read ") {
		t.Fatalf("thread: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// An address that is not valid is named in the error.
func TestInvalidAddressIsNamed(t *testing.T) {
	h := newSendHarness(t)
	for _, args := range [][]string{
		{"send", "--to", "ann@example.org", "--to", "bob@", "--subject", "Hi", "--body", "x", "--dry-run"},
		{"draft", "create", "--cc", "bob@", "--to", "ann@example.org", "--subject", "Hi", "--body", "x"},
	} {
		res := h.run(args...)
		if res.Exit == 0 || !strings.Contains(res.Stderr, `"bob@" in --`) {
			t.Fatalf("%v: exit %d\n%s", args, res.Exit, res.Stderr)
		}
	}
}

// Under drafts_only a saved draft points to draft read, since a send
// would be refused.
func TestDraftsOnlyDraftDoesNotSuggestASend(t *testing.T) {
	h := newSendHarness(t)
	decodeOK(t, h.run("config", "set", "policy.drafts_only", "true", "--account", "posteo", "--json"), nil)
	res := h.run("draft", "create", "--to", "ann@example.org", "--subject", "Hi", "--body", "x")
	if res.Exit != 0 || !strings.Contains(res.Stdout, "Read it with: maildo draft read ") || strings.Contains(res.Stdout, "draft send") {
		t.Fatalf("draft create: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// A mistyped short id gets an error that mentions short ids.
func TestMistypedShortIDIsExplained(t *testing.T) {
	h := newSendHarness(t)
	res := h.run("read", "664b65")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "short id") {
		t.Fatalf("read 664b65: exit %d\n%s", res.Exit, res.Stderr)
	}
}

// Folder results name the whole path and show quotes in names as they are.
func TestFolderResultsNameTheWholePath(t *testing.T) {
	h := newSendHarness(t)
	decodeOK(t, h.run("folders", "create", "Rechnungen", "--json"), nil)
	decodeOK(t, h.run("folders", "create", "Gross", "--parent", "Rechnungen", "--json"), nil)
	res := h.run("folders", "rename", "Rechnungen/Gross", "Groß")
	if res.Exit != 0 || !strings.Contains(res.Stdout, `to "Rechnungen/Groß"`) {
		t.Fatalf("rename: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
	res = h.run("folders", "create", `Odd "quoted"`)
	if res.Exit != 0 || !strings.Contains(res.Stdout, `"Odd "quoted""`) || strings.Contains(res.Stdout, `\"`) {
		t.Fatalf("create: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
	res = h.run("folders", "create", "Rechnungen/2026")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "--parent") {
		t.Fatalf("create with a slash: exit %d\n%s", res.Exit, res.Stderr)
	}
}

// config list says that max_bulk 0 means no limit.
func TestConfigListExplainsNoBulkLimit(t *testing.T) {
	h := newSendHarness(t)
	res := h.run("config", "list")
	if res.Exit != 0 || !strings.Contains(res.Stdout, "0 (no limit)") {
		t.Fatalf("config list: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// A preview of a body read from stdin says that the printed command needs
// the same text on stdin again.
func TestStdinBodyPreviewSaysToPipeItAgain(t *testing.T) {
	h := newSendHarness(t)
	res := testutil.RunWith(h.app(), testutil.Options{
		Stdin:     strings.NewReader("Hi Ann\n"),
		LookupEnv: func(key string) (string, bool) { v, ok := h.env[key]; return v, ok },
	}, "--config", h.configPath, "send", "--to", "ann@example.org", "--subject", "Hi", "--body-file", "-", "--dry-run")
	if res.Exit != 0 || !strings.Contains(res.Stdout, "pipe in the same text again") {
		t.Fatalf("preview: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// An account without a Sent folder gets a warning that says so, and how
// to keep copies.
func TestSendWithoutASentFolderSaysSo(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de", "Drafts", "Trash")
	if res := h.run("login", "--email", "me@posteo.de"); res.Exit != 0 {
		t.Fatalf("login: %s", res.Stderr)
	}
	res := approvedRun(t, h.run, "send", "--to", "ann@example.org", "--subject", "Hi", "--body", "x")
	if res.Exit != 0 || !strings.Contains(res.Stderr, "this account has no Sent folder, so maildo kept no copy") {
		t.Fatalf("send: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}
