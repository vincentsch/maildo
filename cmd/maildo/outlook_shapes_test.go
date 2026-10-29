package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

// These shapes came from a live Microsoft 365 mailbox on 2026-10-29.
func TestOutlookShapesReadWellInTheTerminal(t *testing.T) {
	// Graph's bodyPreview keeps the line breaks of the body.
	thread := threadResult{Thread: mail.Thread{Messages: []mail.ThreadMessage{{
		Account: "work", ID: "m1", Subject: "Test thread", IsRead: true,
		From:    mail.Recipient{Name: "Dana Example", Address: "dana@example.com"},
		Folder:  mail.FolderSummary{ID: "AAMkAGsentitems", DisplayName: "Sent Items"},
		Preview: "Hi Sam,\r\n\r\nA first message.\r\n\r\nDana",
	}}}}
	var out bytes.Buffer
	humanThread(&rungrad.Factory{Flags: &rungrad.GlobalFlags{JSON: true}}, thread)(&out)
	if !strings.Contains(out.String(), "  Hi Sam, A first message. Dana\n") || !strings.Contains(out.String(), "(Sent Items)") {
		t.Fatalf("thread view:\n%s", out.String())
	}

	// Outlook gives a recipient without a name their address as the name.
	if got := recipientText(mail.Recipient{Name: "sam@example.com", Address: "sam@example.com"}); got != "sam@example.com" {
		t.Fatalf("recipient = %q", got)
	}
	if got := recipientText(mail.Recipient{Name: "Sam Example", Address: "sam@example.com"}); got != "Sam Example <sam@example.com>" {
		t.Fatalf("recipient = %q", got)
	}

	// A draft has no sender yet, so a change to it names only the subject.
	if got := targetLabel(mutationTarget{Summary: &mail.MessageSummary{Subject: "Plan"}}); strings.Contains(got, "from") {
		t.Fatalf("draft label = %q", got)
	}
}
