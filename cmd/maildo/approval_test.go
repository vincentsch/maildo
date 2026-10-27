package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
)

// runApplyCommand runs a printed apply command through the harness.
func runApplyCommand(t *testing.T, h *cliHarness, command string) {
	t.Helper()
	args := strings.Fields(command)
	if len(args) == 0 || args[0] != "maildo" {
		t.Fatalf("apply command = %q", command)
	}
	decodeOK(t, h.run(args[1:]...), nil)
}

func arrivedMessage(id, subject string) mail.Message {
	now := time.Now()
	return mail.Message{
		ID: id, Account: "work", Provider: "fake",
		Folder:  mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
		From:    mail.Recipient{Name: "Boss", Address: "boss@example.test"},
		Subject: subject, ReceivedAt: &now,
	}
}

// The command a preview prints changes the messages the preview showed,
// even when matching mail arrives in between.
func TestApplyCommandChangesOnlyThePreviewedMessages(t *testing.T) {
	h, fake := seededMutationHarness(t)
	preview := decodePreview(t, h.run("archive", "--query", "anything", "--all", "--json"))
	command, _ := preview["apply_command"].(string)
	if strings.Contains(command, "--query") {
		t.Fatalf("apply command searches again: %s", command)
	}
	fake.MutationState.Add("work", arrivedMessage("work-arrived", "Please sign today"))
	runApplyCommand(t, h, command)
	for _, call := range fake.MutationCallLog {
		if strings.Contains(call, "work-arrived") {
			t.Fatalf("a message that arrived after the preview was changed: %v", fake.MutationCallLog)
		}
	}
	if got := fake.MessageMutationCalls.Load(); got != 4 {
		t.Fatalf("mutation calls = %d, want the 4 previewed", got)
	}
}

// A --latest preview names the message it found, so a newer match cannot
// take its place.
func TestApplyCommandForLatestNamesTheMessage(t *testing.T) {
	h, fake := seededMutationHarness(t)
	preview := decodePreview(t, h.run("delete", "--query", "anything", "--latest", "--dry-run", "--json"))
	command, _ := preview["apply_command"].(string)
	if !strings.Contains(command, "--id work-new") || strings.Contains(command, "--latest") {
		t.Fatalf("apply command = %s", command)
	}
	fake.MutationState.Add("work", arrivedMessage("work-arrived", "Signed contract, keep this"))
	runApplyCommand(t, h, command)
	if len(fake.MutationCallLog) != 1 || !strings.Contains(fake.MutationCallLog[0], "work-new") {
		t.Fatalf("mutations = %v", fake.MutationCallLog)
	}
}

// Switching the active account between preview and apply does not move the
// change to the other account.
func TestApplyCommandKeepsThePreviewedAccount(t *testing.T) {
	h, fake := seededMutationHarness(t)
	preview := decodePreview(t, h.run("archive", "--query", "anything", "--all", "--json"))
	command, _ := preview["apply_command"].(string)
	if !strings.Contains(command, "--account work") {
		t.Fatalf("apply command lacks the account: %s", command)
	}
	decodeOK(t, h.run("auth", "switch", "personal", "--json"), nil)
	runApplyCommand(t, h, command)
	for _, call := range fake.MutationCallLog {
		if !strings.Contains(call, "work") {
			t.Fatalf("change went to another account: %v", fake.MutationCallLog)
		}
	}
}

// Many ids go into a file next to the config instead of the command line.
func TestApplyCommandPutsManyIDsInAFile(t *testing.T) {
	summaries := testkit.SampleMessages()
	full := testkit.SampleFullMessages()
	for i := 0; i < 30; i++ {
		msg := arrivedMessage(fmt.Sprintf("work-bulk-message-with-a-long-provider-id-%02d", i), fmt.Sprintf("Bulk %d", i))
		full["work"][msg.ID] = msg
	}
	fake := testkit.NewFakeMutationProvider("fake", summaries, full)
	h, fake := seedMutationHarness(t, fake)
	preview := decodePreview(t, h.run("archive", "--query", "anything", "--all", "--json"))
	command, _ := preview["apply_command"].(string)
	fields := strings.Fields(command)
	path := ""
	for i, f := range fields {
		if f == "--ids-file" && i+1 < len(fields) {
			path = fields[i+1]
		}
	}
	if path == "" || !strings.Contains(command, "--account work") || !strings.HasSuffix(command, "--apply") {
		t.Fatalf("apply command = %s", command)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != int(preview["count"].(float64)) {
		t.Fatalf("ids file has %d lines, preview %v targets", lines, preview["count"])
	}
	fake.MutationState.Add("work", arrivedMessage("work-arrived", "Later"))
	runApplyCommand(t, h, command)
	if got := fake.MessageMutationCalls.Load(); got != int64(preview["count"].(float64)) {
		t.Fatalf("mutation calls = %d, want %v", got, preview["count"])
	}
}

// The ids file of a preview is checked before it is applied: a file that
// changed after the preview, or one from the preview of another command,
// is refused.
func TestApplyCommandRefusesAChangedIDsFile(t *testing.T) {
	summaries := testkit.SampleMessages()
	full := testkit.SampleFullMessages()
	for i := 0; i < 30; i++ {
		msg := arrivedMessage(fmt.Sprintf("work-bulk-message-with-a-long-provider-id-%02d", i), fmt.Sprintf("Bulk %d", i))
		full["work"][msg.ID] = msg
	}
	fake := testkit.NewFakeMutationProvider("fake", summaries, full)
	h, fake := seedMutationHarness(t, fake)
	preview := decodePreview(t, h.run("mark", "read", "--query", "anything", "--all", "--json"))
	command, _ := preview["apply_command"].(string)
	args := strings.Fields(command)[1:]
	path := ""
	for i, f := range args {
		if f == "--ids-file" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	if !strings.HasPrefix(filepath.Base(path), "mark-read-") {
		t.Fatalf("ids file = %s", path)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte(nil), original...), "work-arrived\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.run(append(args, "--json")...)
	assertErrorCode(t, res, errs.CodeInvalidSelector)
	if !strings.Contains(res.Stderr, "changed after the preview") || fake.MessageMutationCalls.Load() != 0 {
		t.Fatalf("changed ids file: calls %d\n%s", fake.MessageMutationCalls.Load(), res.Stderr)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	other := append([]string{"delete"}, args[2:]...)
	res = h.run(append(other, "--json")...)
	assertErrorCode(t, res, errs.CodeInvalidSelector)
	if !strings.Contains(res.Stderr, "from a preview of maildo mark read") || fake.MessageMutationCalls.Load() != 0 {
		t.Fatalf("ids file of another command: calls %d\n%s", fake.MessageMutationCalls.Load(), res.Stderr)
	}
	runApplyCommand(t, h, command)
	if got := fake.MessageMutationCalls.Load(); got != int64(preview["count"].(float64)) {
		t.Fatalf("mutation calls = %d, want %v", got, preview["count"])
	}
}

// Two messages whose short ids start the same get longer ids, and the
// shared 7-character id is refused with both candidates.
func TestShortIDCollisionNeverPicksOneMessage(t *testing.T) {
	seen := map[string]string{}
	var a, b, short string
	for n := 1; n < 500000 && a == ""; n++ {
		id := fmt.Sprintf("imap-message/v1/SU5CT1g/1/%d", n)
		s := shortID("work", id)
		if previous, ok := seen[s]; ok {
			a, b, short = previous, id, s
		}
		seen[s] = id
	}
	if a == "" {
		t.Fatal("no collision found")
	}
	sample := testkit.SampleMessages()["work"][0]
	sa, sb := sample, sample
	sa.ID, sa.Subject = a, "FIRST message"
	sb.ID, sb.Subject = b, "SECOND message"
	full := testkit.SampleFullMessages()["work"]["work-new"]
	fa, fb := full, full
	fa.ID, fa.Subject = a, sa.Subject
	fb.ID, fb.Subject = b, sb.Subject
	fake := testkit.NewFakeMutationProvider("fake", map[string][]mail.MessageSummary{"work": {sa, sb}}, map[string]map[string]mail.Message{"work": {a: fa, b: fb}})
	h, _ := seedMutationHarness(t, fake)

	list := h.run("list")
	decodeOK(t, list, nil)
	longA, longB := idHash("work", a)[:8], idHash("work", b)[:8]
	if strings.Contains(list.Stdout, short+" ") || !strings.Contains(list.Stdout, longA) || !strings.Contains(list.Stdout, longB) {
		t.Fatalf("table does not tell the two apart:\n%s", list.Stdout)
	}

	res := h.run("archive", short, "--json")
	assertErrorCode(t, res, errs.CodeInvalidSelector)
	if !strings.Contains(res.Stderr, "FIRST message") || !strings.Contains(res.Stderr, "SECOND message") {
		t.Fatalf("refusal does not list both candidates: %s", res.Stderr)
	}
	if fake.MessageMutationCalls.Load() != 0 {
		t.Fatal("an ambiguous short id changed a message")
	}

	decodeOK(t, h.run("archive", longB, "--json"), nil)
	if len(fake.MutationCallLog) != 1 || !strings.Contains(fake.MutationCallLog[0], b) {
		t.Fatalf("mutations = %v", fake.MutationCallLog)
	}
}

// A broad query in a command that changes mail leaves your Sent and Drafts
// alone unless --folder names them; restore looks in Trash and Junk.
func TestMutationQueriesStayOutOfSentAndDrafts(t *testing.T) {
	h, fake := seededMutationHarness(t)
	sent := arrivedMessage("work-sent", "My own reply")
	sent.Folder = mail.FolderSummary{ID: "sent-id", DisplayName: "Sent", WellKnownName: mail.WellKnownSent, Path: "Sent"}
	draft := arrivedMessage("work-draft", "Unfinished draft")
	draft.Folder = mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"}
	trashed := arrivedMessage("work-trashed", "Autumn sale")
	trashed.Folder = mail.FolderSummary{ID: "deleted-id", DisplayName: "Trash", WellKnownName: mail.WellKnownDeleted, Path: "Trash"}
	for _, m := range []mail.Message{sent, draft, trashed} {
		fake.MutationState.Add("work", m)
	}

	preview := decodePreview(t, h.run("delete", "--query", "anything", "--all", "--json"))
	for _, target := range previewTargets(t, preview) {
		switch target["id"] {
		case "work-sent", "work-draft", "work-trashed":
			t.Fatalf("delete --query reached %v", target["id"])
		}
		if target["folder"] == nil {
			t.Fatalf("preview target has no folder: %v", target)
		}
	}

	restore := decodePreview(t, h.run("restore", "--query", "anything", "--latest", "--dry-run", "--json"))
	if got := previewTargets(t, restore)[0]["id"]; got != "work-trashed" {
		t.Fatalf("restore --query picked %v, want the message in Trash", got)
	}

	named := decodePreview(t, h.run("archive", "--query", "anything", "--all", "--folder", "Sent", "--json"))
	if targets := previewTargets(t, named); len(targets) != 1 || targets[0]["id"] != "work-sent" {
		t.Fatalf("--folder Sent targets = %v", targets)
	}
}

// A policy error tells an agent to ask, and does not hand it the command
// that loosens the policy.
func TestPolicyErrorsDoNotHandOutTheLooseningCommand(t *testing.T) {
	h, _ := seededMutationHarness(t)
	decodeOK(t, h.run("config", "set", "policy.read_only", "true", "--account", "work"), nil)
	for _, args := range [][]string{
		{"archive", "--id", "work-new", "--json"},
		{"archive", "--id", "work-new"},
	} {
		res := h.run(args...)
		if res.Exit != 4 {
			t.Fatalf("%v exit = %d, stderr %s", args, res.Exit, res.Stderr)
		}
		if strings.Contains(res.Stderr, "config set policy") || !strings.Contains(res.Stderr, "Only the account's owner can change the policy") {
			t.Fatalf("%v hint: %s", args, res.Stderr)
		}
	}
}

func TestIDsFileFromStdin(t *testing.T) {
	h, fake := seededMutationHarness(t)
	preview := decodePreview(t, h.runWith(testutil.Options{Stdin: strings.NewReader("work-new\n\"work-old\"\n")}, "archive", "--ids-file", "-", "--json"))
	if preview["count"] != float64(2) || fake.MessageMutationCalls.Load() != 0 {
		t.Fatalf("preview = %v", preview)
	}
}

func TestMaxTargetsOverflowSaysWhatToDo(t *testing.T) {
	h, _ := seededMutationHarness(t)
	res := h.run("archive", "--query", "anything", "--all", "--max-targets", "2", "--json")
	assertErrorCode(t, res, errs.CodeBulkTargetLimitExceeded)
	if !strings.Contains(res.Stderr, "more than 2 messages") || !strings.Contains(res.Stderr, "raise --max-targets") {
		t.Fatalf("error = %s", res.Stderr)
	}
}

func TestLooseningAPolicyWithoutATerminalIsAPolicyError(t *testing.T) {
	h, _ := seededMutationHarness(t)
	decodeOK(t, h.run("config", "set", "policy.read_only", "true", "--account", "work"), nil)
	assertErrorCodeExit(t, h.run("config", "set", "policy.read_only", "false", "--account", "work", "--json"), errs.CodePolicyDenied, 4)
}
