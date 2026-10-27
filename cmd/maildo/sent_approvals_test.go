package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad/testutil"
)

// sendPreviewOf runs a send command with --dry-run --json and returns the
// preview.
func sendPreviewOf(t *testing.T, run func(...string) testutil.Result, args ...string) sendPreview {
	t.Helper()
	var preview sendPreview
	decodeOK(t, run(append(args, "--dry-run", "--json")...), &preview)
	if preview.ApprovalCode == "" || preview.SendCommand == "" {
		t.Fatalf("preview = %+v", preview)
	}
	return preview
}

// The decided case: an agent runs the printed send command twice. The
// second run is refused, and only one message goes out. A new preview says
// the message went out before and lets the code send it once more.
func TestApprovedSendGoesOutOnce(t *testing.T) {
	h := newSendHarness(t)
	args := []string{"send", "--to", "clara@example.org", "--subject", "Quote", "--body", "Hi Clara"}
	preview := sendPreviewOf(t, h.run, args...)
	if preview.SentBefore != nil {
		t.Fatalf("a new message has sent_before: %+v", preview.SentBefore)
	}
	decodeOK(t, runPrinted(t, h.run, preview.SendCommand), nil)

	again := runPrinted(t, h.run, preview.SendCommand)
	if again.Exit != 1 || !strings.Contains(again.Stderr, "this approved message was already sent on ") ||
		!strings.Contains(again.Stderr, "check Sent before you send it again") || !strings.Contains(again.Stderr, "--dry-run for a new preview") {
		t.Fatalf("second run: exit %d\n%s", again.Exit, again.Stderr)
	}
	used := runPrinted(t, h.run, preview.SendCommand, "--json")
	assertErrorCode(t, used, errs.CodeApprovalUsed)
	// "outcome" is for this command's own write, and it wrote nothing.
	if body := errorJSON(t, used); body["sent_outcome"] != "sent" || body["sent_at"] == nil || body["outcome"] != nil {
		t.Fatalf("approval_used error = %v", body)
	}
	if n := len(h.server.Sent()); n != 1 {
		t.Fatalf("%d messages went out, want 1", n)
	}

	human := h.run(append(args, "--dry-run")...)
	if !strings.Contains(human.Stdout, "maildo already sent this message from posteo on ") {
		t.Fatalf("preview does not say the message went out before:\n%s", human.Stdout)
	}
	repeat := sendPreviewOf(t, h.run, args...)
	if repeat.ApprovalCode != preview.ApprovalCode || repeat.SentBefore == nil || repeat.SentBefore.Outcome != "sent" {
		t.Fatalf("repeat preview = %+v", repeat)
	}
	decodeOK(t, runPrinted(t, h.run, repeat.SendCommand), nil)
	assertErrorCode(t, runPrinted(t, h.run, repeat.SendCommand, "--json"), errs.CodeApprovalUsed)
	if n := len(h.server.Sent()); n != 2 {
		t.Fatalf("%d messages went out, want 2", n)
	}
}

// A reply sent with its printed command does not go out twice either.
func TestApprovedReplyGoesOutOnce(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Kickoff\r\nMessage-ID: <kickoff@example.org>\r\n\r\nTuesday?\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "kickoff", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	preview := sendPreviewOf(t, h.run, "reply", "--id", found.Messages[0].ID, "--body", "Tuesday works.", "--send")
	decodeOK(t, runPrinted(t, h.run, preview.SendCommand), nil)
	assertErrorCode(t, runPrinted(t, h.run, preview.SendCommand, "--json"), errs.CodeApprovalUsed)
	if n := len(h.server.Sent()); n != 1 {
		t.Fatalf("%d replies went out, want 1", n)
	}
}

// Two copies of the printed command that run at the same time send the
// message once: the code is written down under a lock before the send.
func TestApprovalCodeIsClaimedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), sentApprovalsFile)
	acct := provider.ResolvedAccount{Name: "work"}
	var wg sync.WaitGroup
	errors := make([]error, 8)
	for i := range errors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errors[i] = claimApproval(path, time.Now(), acct, "abcdefgh", true)
		}(i)
	}
	wg.Wait()
	claimed := 0
	for _, err := range errors {
		if err == nil {
			claimed++
		} else if productErrorCode(err) != errs.CodeApprovalUsed {
			t.Fatalf("claim error = %v", err)
		}
	}
	if claimed != 1 {
		t.Fatalf("%d of %d commands could send, want 1", claimed, len(errors))
	}
}

// A send without --approve is written down but never refused: a person
// confirmed it in the terminal, or the account lets a script send the same
// report every week. A printed command for that message is refused after
// it went out.
func TestSendWithoutACodeIsNeverRefused(t *testing.T) {
	h := newSendHarness(t)
	args := []string{"send", "--to", "ann@example.org", "--subject", "Weekly report", "--body", "All good."}
	preview := sendPreviewOf(t, h.run, args...)
	if res := h.runInTerminal("y\n", args...); res.Exit != 0 {
		t.Fatalf("send in the terminal: %s", res.Stderr)
	}
	second := h.runInTerminal("y\n", args...)
	if second.Exit != 0 || !strings.Contains(second.Stderr, "maildo already sent this message from posteo on ") {
		t.Fatalf("second send in the terminal: exit %d\n%s", second.Exit, second.Stderr)
	}
	assertErrorCode(t, runPrinted(t, h.run, preview.SendCommand, "--json"), errs.CodeApprovalUsed)

	if res := h.runInTerminal("y\n", "config", "set", "policy.unattended_send", "true", "--account", "posteo"); res.Exit != 0 {
		t.Fatalf("config set: %s", res.Stderr)
	}
	for i := 0; i < 2; i++ {
		decodeOK(t, h.run(append(args, "--json")...), nil)
	}
	if n := len(h.server.Sent()); n != 4 {
		t.Fatalf("%d messages went out, want 4", n)
	}
}

// A send that surely did not go out gives the code back. One that may
// have gone out uses it up, and so does a send that maildo never finished.
func TestApprovalAfterAFailedSend(t *testing.T) {
	h, fake := seededDraftHarness(t)
	args := []string{"send", "--to", "client@example.test", "--subject", "Quote", "--body", "Hi", "--account", "work"}
	attempt := func() errs.ProviderWriteAttempt {
		a, err := errs.NewProviderWriteAttempt("work", errs.ProviderIMAP, errs.WriteSMTPDirectSend)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	preview := sendPreviewOf(t, h.run, args...)
	fake.Drafts.DirectSendErr = errs.NewWriteNotApplied(attempt(), errs.New(errs.CodeProviderConnectionError, "connection refused"))
	if res := runPrinted(t, h.run, preview.SendCommand, "--json"); res.Exit == 0 {
		t.Fatal("the failing send worked")
	}
	fake.Drafts.DirectSendErr = nil
	decodeOK(t, runPrinted(t, h.run, preview.SendCommand, "--json"), nil)
	if n := fake.Drafts.DirectCalls.Load(); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}

	preview = sendPreviewOf(t, h.run, append(args, "--cc", "boss@example.test")...)
	lost := attempt()
	if err := errs.CrossProviderWriteTransmissionBoundary(lost); err != nil {
		t.Fatal(err)
	}
	fake.Drafts.DirectSendErr = errs.NewOutcomeUnknown(lost, errs.OutcomeAcceptanceResponseLost)
	if res := runPrinted(t, h.run, preview.SendCommand, "--json"); res.Exit == 0 {
		t.Fatal("the failing send worked")
	}
	fake.Drafts.DirectSendErr = nil
	res := runPrinted(t, h.run, preview.SendCommand)
	if res.Exit != 1 || !strings.Contains(res.Stderr, "this approved message may already have been sent on ") {
		t.Fatalf("retry after an unknown outcome: exit %d\n%s", res.Exit, res.Stderr)
	}
	if n := fake.Drafts.DirectCalls.Load(); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}
	var repeat sendPreview
	decodeOK(t, h.run(append(args, "--cc", "boss@example.test", "--dry-run", "--json")...), &repeat)
	if repeat.SentBefore == nil || repeat.SentBefore.Outcome != "unknown" {
		t.Fatalf("preview after an unknown outcome = %+v", repeat.SentBefore)
	}

	// maildo stopped during a send and never wrote down how it went.
	preview = sendPreviewOf(t, h.run, append(args, "--subject", "Other")...)
	path := filepath.Join(h.root, sentApprovalsFile)
	file, err := loadSentApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Sent = append(file.Sent, sentApproval{Code: preview.ApprovalCode, Account: "work", At: time.Now().UTC(), Outcome: "sending"})
	data, _ := json.Marshal(file)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res = runPrinted(t, h.run, preview.SendCommand)
	if res.Exit != 1 || !strings.Contains(res.Stderr, "may already have been sent") {
		t.Fatalf("retry after a stopped send: exit %d\n%s", res.Exit, res.Stderr)
	}
}

// When maildo cannot write the send down, it does not send, and the
// error names no file path.
func TestSendStopsWhenItCannotBeWrittenDown(t *testing.T) {
	h, fake := seededDraftHarness(t)
	args := []string{"send", "--to", "client@example.test", "--subject", "Quote", "--body", "Hi", "--account", "work"}
	preview := sendPreviewOf(t, h.run, args...)
	if err := os.WriteFile(filepath.Join(h.root, sentApprovalsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runPrinted(t, h.run, preview.SendCommand)
	if res.Exit != 1 || !strings.Contains(res.Stderr, "could not write down this send") || strings.Contains(res.Stderr, h.root) {
		t.Fatalf("send with a damaged record: exit %d\n%s", res.Exit, res.Stderr)
	}
	if fake.Drafts.DirectCalls.Load() != 0 {
		t.Fatal("maildo sent without writing the send down")
	}
	// A preview still works.
	sendPreviewOf(t, h.run, args...)
}
