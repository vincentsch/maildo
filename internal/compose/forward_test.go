package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestBuildForwardPlan(t *testing.T) {
	plan, err := BuildForward(ForwardOptions{
		Recipients: RecipientInput{To: []string{`Client <client@example.test>`}},
		Body:       BodyInput{InlineSet: true, Inline: "# FYI", MarkdownSet: true},
	})
	if err != nil {
		t.Fatalf("BuildForward default: %v", err)
	}
	if plan.Action != mail.ComposeExecutionDraft || len(plan.To) != 1 || plan.To[0].Name != "Client" {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Body.Format != mail.BodyFormatMarkdown || plan.Body.ContentType != "html" || plan.Attachments.Mode != mail.ForwardAttachmentsAll {
		t.Fatalf("body/attachments = %#v %#v", plan.Body, plan.Attachments)
	}

	send, err := BuildForward(ForwardOptions{
		Send:               true,
		Recipients:         RecipientInput{CC: []string{`copy@example.test`}},
		Body:               BodyInput{InlineSet: true, Inline: "FYI"},
		IncludeAttachments: "all",
	})
	if err != nil {
		t.Fatalf("BuildForward send: %v", err)
	}
	if send.Action != mail.ComposeExecutionSend || len(send.CC) != 1 || send.Attachments.Mode != mail.ForwardAttachmentsAll {
		t.Fatalf("send plan = %#v", send)
	}
}

// A forward takes all attachments along unless told otherwise; naming
// attachments picks just those, and none leaves them all out.
func TestForwardAttachmentDefault(t *testing.T) {
	for _, tt := range []struct {
		mode  string
		names []string
		want  mail.ForwardAttachmentMode
	}{
		{"", nil, mail.ForwardAttachmentsAll},
		{"all", nil, mail.ForwardAttachmentsAll},
		{"none", nil, mail.ForwardAttachmentsNone},
		{"", []string{"a.pdf"}, mail.ForwardAttachmentsSelected},
		{"none", []string{"a.pdf"}, mail.ForwardAttachmentsSelected},
	} {
		got, err := BuildForwardAttachmentSelection(tt.mode, tt.names, nil)
		if err != nil || got.Mode != tt.want {
			t.Fatalf("mode %q names %v = %#v, %v; want %s", tt.mode, tt.names, got, err, tt.want)
		}
	}
}

func TestForwardValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   ForwardOptions
	}{
		{name: "draft send conflict", in: ForwardOptions{Draft: true, Send: true, Body: BodyInput{InlineSet: true, Inline: "x"}}},
		{name: "send recipients", in: ForwardOptions{Send: true, Body: BodyInput{InlineSet: true, Inline: "x"}}},
		{name: "missing body", in: ForwardOptions{}},
		{name: "whitespace body", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: " \r\n\t"}}},
		{name: "body source conflict", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x", FileSet: true, Bytes: []byte("x")}}},
		{name: "body format conflict", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x", TextSet: true, HTMLSet: true}}},
		{name: "bad include mode", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x"}, IncludeAttachments: "some"}},
		{name: "all plus names", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x"}, IncludeAttachments: "all", IncludeAttachmentNames: []string{"a.pdf"}}},
		{name: "duplicate names", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x"}, IncludeAttachmentNames: []string{"a.pdf", "a.pdf"}}},
		{name: "duplicate ids", in: ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "x"}, IncludeAttachmentIDs: []string{"a", "a"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildForward(tt.in)
			if !hasCode(err, errs.CodeInvalidComposeInput) {
				t.Fatalf("err = %#v, want invalid_compose_input", err)
			}
		})
	}
}

func TestBuildForwardBodyBuildsForwardedBlock(t *testing.T) {
	plan, err := BuildForward(ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "<one>\ntwo"}})
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2026, 6, 19, 8, 0, 0, 0, time.UTC)
	source := SourceMaterial{
		SourceID: "src",
		From:     mail.Recipient{Name: "Sender", Address: "sender@example.test"},
		To:       []mail.Recipient{{Address: "to@example.test"}},
		Subject:  "Source subject",
		SentAt:   &sent,
		Text:     []byte("forwarded text"),
		HTML:     []byte("<p>forwarded html</p>"),
	}
	body, err := BuildForwardBody(plan, source)
	if err != nil {
		t.Fatalf("BuildForwardBody: %v", err)
	}
	if body.ContentType != "text" || !strings.Contains(body.Text, "---------- Forwarded message ---------") ||
		!strings.Contains(body.Text, "From: Sender <sender@example.test>") ||
		!strings.Contains(body.Text, "Subject: Source subject") ||
		!strings.Contains(body.Text, "forwarded text") {
		t.Fatalf("forward text = %q", body.Text)
	}

	htmlPlan, err := BuildForward(ForwardOptions{Body: BodyInput{InlineSet: true, Inline: "<b>FYI</b>", HTMLSet: true}})
	if err != nil {
		t.Fatal(err)
	}
	htmlBody, err := BuildForwardBody(htmlPlan, source)
	if err != nil {
		t.Fatalf("BuildForwardBody html: %v", err)
	}
	if htmlBody.ContentType != "html" || !strings.Contains(htmlBody.HTML, "<b>FYI</b>") ||
		!strings.Contains(htmlBody.HTML, "<div>From: Sender &lt;sender@example.test&gt;</div>") ||
		!strings.Contains(htmlBody.HTML, "<p>forwarded html</p>") {
		t.Fatalf("forward html = %q", htmlBody.HTML)
	}
	cidSource := source
	cidSource.HTMLHasCIDReferences = true
	cidSource.HTML = []byte(`<p>forwarded html<img src="cid:image@example.test"></p>`)
	cidBody, err := BuildForwardBody(htmlPlan, cidSource)
	if err != nil {
		t.Fatalf("BuildForwardBody cid html: %v", err)
	}
	if !strings.Contains(cidBody.HTML, `src="cid:image@example.test"`) {
		t.Fatalf("cid html = %q", cidBody.HTML)
	}
	htmlOnly := source
	htmlOnly.Text = nil
	htmlOnly.HTMLHasCIDReferences = false
	if _, err := BuildForwardBody(htmlPlan, htmlOnly); !hasCode(err, errs.CodeInvalidComposeInput) {
		t.Fatalf("html-only source error = %v", err)
	}
}
