package compose

import (
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestBuildCreateRecipientsSenderAndSummary(t *testing.T) {
	req, summary, err := BuildCreate(CreateInput{
		Recipients: RecipientInput{
			To:  []string{`Client <client@example.test>, second@example.test`},
			CC:  []string{`Copy <copy@example.test>`},
			BCC: []string{`one@example.test`, `two@example.test`},
		},
		Sender:  SenderInput{Provided: true, Raw: `Work User <work@example.test>`},
		Subject: SubjectInput{Provided: true, Value: "Subject"},
		Body:    BodyInput{InlineSet: true, Inline: "hello"},
	}, "work@example.test")
	if err != nil {
		t.Fatalf("BuildCreate: %v", err)
	}
	if len(req.Message.To) != 2 || req.Message.To[0].Name != "Client" || req.Message.To[0].Address != "client@example.test" {
		t.Fatalf("recipients = %#v", req.Message.To)
	}
	if req.Message.From == nil || req.Message.From.Name != "Work User" {
		t.Fatalf("from = %#v", req.Message.From)
	}
	if summary.ToCount != 2 || summary.CCCount != 1 || summary.BCCCount != 2 || summary.BodySource != SourceInline || summary.BodyBytes != 5 {
		t.Fatalf("summary = %#v", summary)
	}
	if strings.Contains(summary.RequestedFrom, "hello") {
		t.Fatalf("summary leaked body: %#v", summary)
	}
}

func TestSharedMailboxParserComposeRegression(t *testing.T) {
	req, _, err := BuildCreate(CreateInput{
		Recipients: RecipientInput{To: []string{`First <first@example.test>, second@example.test`, `Third <third@example.test>`}},
		Body:       BodyInput{InlineSet: true, Inline: "body"},
	}, "work@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Message.To) != 3 || req.Message.To[0].Name != "First" || req.Message.To[0].Address != "first@example.test" || req.Message.To[1].Address != "second@example.test" || req.Message.To[2].Name != "Third" {
		t.Fatalf("recipient order/display names changed: %#v", req.Message.To)
	}
	if _, _, err := BuildCreate(CreateInput{Recipients: RecipientInput{To: []string{"<malformed"}}, Body: BodyInput{InlineSet: true, Inline: "body"}}, "work@example.test"); err == nil {
		t.Fatal("malformed recipient succeeded")
	}
	if _, _, err := BuildCreate(CreateInput{Recipients: RecipientInput{To: []string{""}}, Body: BodyInput{InlineSet: true, Inline: "body"}}, "work@example.test"); err == nil {
		t.Fatal("empty recipient succeeded")
	}
}

func TestBuildInputValidation(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
	}{
		{name: "bad recipient", run: func() error {
			_, _, err := BuildCreate(CreateInput{Recipients: RecipientInput{To: []string{"not an address"}}, Body: BodyInput{InlineSet: true, Inline: "x"}}, "work@example.test")
			return err
		}},
		{name: "bad sender", run: func() error {
			_, _, err := BuildCreate(CreateInput{Sender: SenderInput{Provided: true, Raw: "other@example.test"}, Body: BodyInput{InlineSet: true, Inline: "x"}}, "work@example.test")
			return err
		}},
		{name: "malformed sender", run: func() error {
			_, _, err := BuildCreate(CreateInput{Sender: SenderInput{Provided: true, Raw: "bad sender"}, Body: BodyInput{InlineSet: true, Inline: "x"}}, "work@example.test")
			return err
		}},
		{name: "multiple senders", run: func() error {
			_, _, err := BuildCreate(CreateInput{Sender: SenderInput{Provided: true, Raw: "work@example.test, other@example.test"}, Body: BodyInput{InlineSet: true, Inline: "x"}}, "work@example.test")
			return err
		}},
		{name: "body source exclusivity", run: func() error {
			_, _, err := BuildCreate(CreateInput{Body: BodyInput{InlineSet: true, Inline: "x", FileSet: true, Bytes: []byte("y")}}, "work@example.test")
			return err
		}},
		{name: "format exclusivity", run: func() error {
			_, _, err := BuildCreate(CreateInput{Body: BodyInput{InlineSet: true, Inline: "x", TextSet: true, HTMLSet: true}}, "work@example.test")
			return err
		}},
		{name: "create minimum", run: func() error {
			_, _, err := BuildCreate(CreateInput{Subject: SubjectInput{Provided: true, Value: ""}}, "work@example.test")
			return err
		}},
		{name: "send recipients", run: func() error {
			_, _, err := BuildSend(SendInput{Subject: SubjectInput{Provided: true, Value: "Subject"}}, "work@example.test")
			return err
		}},
		{name: "send content", run: func() error {
			_, _, err := BuildSend(SendInput{Recipients: RecipientInput{To: []string{"to@example.test"}}}, "work@example.test")
			return err
		}},
		{name: "update noop", run: func() error {
			_, _, err := BuildUpdate("draft-1", UpdateInput{}, "work@example.test")
			return err
		}},
		{name: "update clear conflict", run: func() error {
			_, _, err := BuildUpdate("draft-1", UpdateInput{Recipients: RecipientInput{To: []string{"to@example.test"}, ToSet: true, ClearTo: true}}, "work@example.test")
			return err
		}},
		// A subject is one line; a newline used to be sent encoded in the
		// header and printed raw in the preview's command.
		{name: "send subject with newline", run: func() error {
			_, _, err := BuildSend(SendInput{Recipients: RecipientInput{To: []string{"to@example.test"}}, Subject: SubjectInput{Provided: true, Value: "Hi\nX-Injected: 1"}}, "work@example.test")
			return err
		}},
		{name: "draft subject with escape code", run: func() error {
			_, _, err := BuildCreate(CreateInput{Subject: SubjectInput{Provided: true, Value: "Hi \x1b[31mred"}}, "work@example.test")
			return err
		}},
		{name: "update subject with carriage return", run: func() error {
			_, _, err := BuildUpdate("draft-1", UpdateInput{Subject: SubjectInput{Provided: true, Value: "Hi\rthere"}}, "work@example.test")
			return err
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if !hasCode(err, errs.CodeInvalidComposeInput) {
				t.Fatalf("err = %#v, want invalid_compose_input", err)
			}
		})
	}
}

// A signature's single line break survives Markdown, in the HTML part and
// in the plain text part.
func TestMarkdownKeepsLineBreaks(t *testing.T) {
	req, _, err := BuildSend(SendInput{
		Recipients: RecipientInput{To: []string{"to@example.test"}},
		Body:       BodyInput{InlineSet: true, Inline: "Thanks for the quote.\n\nBest,\nAnn", MarkdownSet: true},
	}, "work@example.test")
	if err != nil {
		t.Fatal(err)
	}
	body := req.Message.Body
	if !strings.Contains(body.HTML, "Best,<br") || !strings.Contains(body.Text, "Best,\nAnn") {
		t.Fatalf("html %q\ntext %q", body.HTML, body.Text)
	}
}

func TestBodyRenderingAndSanitization(t *testing.T) {
	req, _, err := BuildCreate(CreateInput{
		Subject: SubjectInput{Provided: true, Value: "Body"},
		Body: BodyInput{
			InlineSet:   true,
			Inline:      "# Title\n\n[ok](https://example.test)\n\n<script>x()</script><a href=\"javascript:alert(1)\" onclick=\"x()\">bad</a><img src=\"https://example.test/x.png\"><table><tr><td align=\"right\" style=\"color:red\">cell</td></tr></table>",
			MarkdownSet: true,
		},
	}, "work@example.test")
	if err != nil {
		t.Fatalf("BuildCreate markdown: %v", err)
	}
	html := req.Message.Body.HTML
	for _, bad := range []string{"<script", "javascript:", "onclick", "style=", "<img"} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Fatalf("sanitized html still contains %q: %s", bad, html)
		}
	}
	for _, want := range []string{"<h1>Title</h1>", "https://example.test"} {
		if !strings.Contains(html, want) {
			t.Fatalf("sanitized html missing %q: %s", want, html)
		}
	}
	if req.Message.Body.Text == "" || !strings.Contains(req.Message.Body.Text, "Title") {
		t.Fatalf("readable fallback = %q", req.Message.Body.Text)
	}

	text, _, err := BuildCreate(CreateInput{Body: BodyInput{InlineSet: true, Inline: "<one>\ntwo"}}, "work@example.test")
	if err != nil {
		t.Fatalf("BuildCreate text: %v", err)
	}
	if text.Message.Body.HTML != "<p>&lt;one&gt;<br>\ntwo</p>" {
		t.Fatalf("text html = %q", text.Message.Body.HTML)
	}

	htmlReq, _, err := BuildCreate(CreateInput{
		Subject: SubjectInput{Provided: true, Value: "HTML"},
		Body: BodyInput{
			InlineSet: true,
			Inline: `<style>p{color:red}</style><iframe src="x"></iframe><object></object><embed></embed><form><input></form>` +
				`<table><tr onclick="x()"><td align="right" style="color:red">cell</td></tr></table>` +
				`<a href="mailto:a@example.test">mail</a><a href="http://example.test">http</a><a href="https://example.test">https</a>` +
				`<a href="data:text/plain,x">bad</a><img src="https://example.test/x.png">`,
			HTMLSet: true,
		},
	}, "work@example.test")
	if err != nil {
		t.Fatalf("BuildCreate html: %v", err)
	}
	html = htmlReq.Message.Body.HTML
	for _, want := range []string{"<table>", "align=\"right\"", "mailto:a@example.test", "http://example.test", "https://example.test"} {
		if !strings.Contains(html, want) {
			t.Fatalf("sanitized html missing %q: %s", want, html)
		}
	}
	for _, bad := range []string{"onclick", "style=", "data:", "<style", "<iframe", "<object", "<embed", "<form", "<input", "<img"} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Fatalf("sanitized html still contains %q: %s", bad, html)
		}
	}
	if htmlReq.Message.Body.Text == "" || !strings.Contains(htmlReq.Message.Body.Text, "mail") {
		t.Fatalf("html readable fallback = %q", htmlReq.Message.Body.Text)
	}
}

func TestBuildUpdatePatchSemantics(t *testing.T) {
	req, summary, err := BuildUpdate("draft-1", UpdateInput{
		Recipients: RecipientInput{To: []string{"new@example.test"}, ToSet: true, ClearCC: true},
		Subject:    SubjectInput{Provided: true, Value: ""},
		Body:       BodyInput{FileSet: true, Bytes: []byte{}, FromStdin: true, HTMLSet: true},
	}, "work@example.test")
	if err != nil {
		t.Fatalf("BuildUpdate: %v", err)
	}
	if !req.Patch.To.Set || len(req.Patch.To.Values) != 1 || !req.Patch.CC.Clear || !req.Patch.Subject.Set || req.Patch.Subject.Value != "" || !req.Patch.Body.Set {
		t.Fatalf("patch = %#v", req.Patch)
	}
	if summary.BodySource != SourceStdin || summary.BodyBytes != 0 || summary.BodyPresent {
		t.Fatalf("summary = %#v", summary)
	}
	if req.Patch.Body.Body.Format != mail.BodyFormatHTML {
		t.Fatalf("body format = %q", req.Patch.Body.Body.Format)
	}
}

func hasCode(err error, code string) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(interface{ ErrorDetails() any }); ok {
		if details, ok := e.ErrorDetails().(map[string]any); ok {
			return details["code"] == code
		}
	}
	return false
}
