package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestBuildReplyPlan(t *testing.T) {
	plan, err := BuildReply(ReplyOptions{
		Body:      BodyInput{InlineSet: true, Inline: "Thanks"},
		EnvLookup: env(map[string]string{"LANG": "de_DE.UTF-8"}),
	})
	if err != nil {
		t.Fatalf("BuildReply default: %v", err)
	}
	if plan.Action != mail.ComposeExecutionDraft || plan.Mode != mail.ReplyModeReply || plan.Quote != mail.QuoteModeFull || plan.Language != mail.QuoteLanguageGerman {
		t.Fatalf("default plan = %#v", plan)
	}
	if plan.Summary.BodySource != SourceInline || plan.Summary.BodyBytes != len("Thanks") {
		t.Fatalf("summary = %#v", plan.Summary)
	}

	plan, err = BuildReply(ReplyOptions{
		ReplyAll:      true,
		Send:          true,
		Quote:         "short",
		QuoteLanguage: "en",
		Body:          BodyInput{InlineSet: true, Inline: "Thanks", MarkdownSet: true},
	})
	if err != nil {
		t.Fatalf("BuildReply send/reply-all: %v", err)
	}
	if plan.Action != mail.ComposeExecutionSend || plan.Mode != mail.ReplyModeReplyAll || plan.Quote != mail.QuoteModeShort || plan.Language != mail.QuoteLanguageEnglish {
		t.Fatalf("send plan = %#v", plan)
	}
	if plan.Body.ContentType != "html" || plan.Body.Format != mail.BodyFormatMarkdown {
		t.Fatalf("body = %#v", plan.Body)
	}
}

func TestReplyValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   ReplyOptions
	}{
		{name: "draft send conflict", in: ReplyOptions{Draft: true, Send: true, Body: BodyInput{InlineSet: true, Inline: "x"}}},
		{name: "bad quote", in: ReplyOptions{Quote: "medium", Body: BodyInput{InlineSet: true, Inline: "x"}}},
		{name: "bad language", in: ReplyOptions{QuoteLanguage: "fr", Body: BodyInput{InlineSet: true, Inline: "x"}}},
		{name: "missing body", in: ReplyOptions{}},
		{name: "whitespace body", in: ReplyOptions{Body: BodyInput{InlineSet: true, Inline: "   \r\n\t"}}},
		{name: "body source conflict", in: ReplyOptions{Body: BodyInput{InlineSet: true, Inline: "x", FileSet: true, Bytes: []byte("x")}}},
		{name: "body format conflict", in: ReplyOptions{Body: BodyInput{InlineSet: true, Inline: "x", TextSet: true, HTMLSet: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildReply(tt.in)
			if !hasCode(err, errs.CodeInvalidComposeInput) {
				t.Fatalf("err = %#v, want invalid_compose_input", err)
			}
		})
	}
}

func TestQuoteParsingAndLanguageResolution(t *testing.T) {
	for _, raw := range []string{"", "full", "short", "none"} {
		if _, err := ParseQuoteMode(raw); err != nil {
			t.Fatalf("ParseQuoteMode(%q): %v", raw, err)
		}
	}
	if _, err := ParseQuoteMode("bad"); !hasCode(err, errs.CodeInvalidComposeInput) {
		t.Fatalf("bad quote err = %#v", err)
	}

	for _, tt := range []struct {
		name string
		raw  string
		env  map[string]string
		want mail.QuoteLanguage
	}{
		{name: "explicit en", raw: "en", want: mail.QuoteLanguageEnglish},
		{name: "explicit de", raw: "de", want: mail.QuoteLanguageGerman},
		{name: "lc all german", raw: "auto", env: map[string]string{"LC_ALL": "de_DE.UTF-8", "LANG": "en_US.UTF-8"}, want: mail.QuoteLanguageGerman},
		{name: "lc all precedence", raw: "auto", env: map[string]string{"LC_ALL": "en_US.UTF-8", "LC_MESSAGES": "de_DE.UTF-8"}, want: mail.QuoteLanguageEnglish},
		{name: "lang english", raw: "", env: map[string]string{"LANG": "en_US.UTF-8"}, want: mail.QuoteLanguageEnglish},
		{name: "empty fallback", raw: "auto", env: map[string]string{}, want: mail.QuoteLanguageEnglish},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveQuoteLanguage(tt.raw, env(tt.env))
			if err != nil {
				t.Fatalf("ResolveQuoteLanguage: %v", err)
			}
			if got != tt.want {
				t.Fatalf("language = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildReplyBodyQuoteModes(t *testing.T) {
	plan, err := BuildReply(ReplyOptions{Body: BodyInput{InlineSet: true, Inline: "<one>\ntwo"}})
	if err != nil {
		t.Fatal(err)
	}
	source := replyTestSource()
	full, err := BuildReplyBody(plan, source)
	if err != nil {
		t.Fatalf("full body: %v", err)
	}
	if full.ContentType != "text" || !strings.Contains(full.Text, "<one>\ntwo") || !strings.Contains(full.Text, "> original source line one") {
		t.Fatalf("full text = %q", full.Text)
	}
	// A full quote starts with the "On ... wrote:" line, like a short one,
	// and ends without an empty quoted line.
	if !strings.Contains(full.Text, "On "+source.SentAt.Format(ShortQuoteDateFormat)+", Sender <sender@example.test> wrote:\n\n> original source line one") || strings.HasSuffix(full.Text, "> ") {
		t.Fatalf("full text = %q", full.Text)
	}
	plan.Language = mail.QuoteLanguageGerman
	german, err := BuildReplyBody(plan, SourceMaterial{SourceID: "src", From: source.From, SentAt: source.SentAt, Text: []byte("Hallo\n\n")})
	if err != nil || !strings.Contains(german.Text, "schrieb Sender <sender@example.test>:\n\n> Hallo") || strings.HasSuffix(german.Text, "> ") {
		t.Fatalf("german full text = %q, %v", german.Text, err)
	}
	plan.Language = mail.QuoteLanguageEnglish
	htmlPlan, err := BuildReply(ReplyOptions{Body: BodyInput{InlineSet: true, Inline: "<h1>Hi</h1>", HTMLSet: true}})
	if err != nil {
		t.Fatal(err)
	}
	htmlFull, err := BuildReplyBody(htmlPlan, source)
	if err != nil {
		t.Fatalf("html full body: %v", err)
	}
	if htmlFull.ContentType != "html" || !strings.Contains(htmlFull.HTML, "<h1>Hi</h1>") || !strings.Contains(htmlFull.HTML, "<blockquote><p>original source") {
		t.Fatalf("full html = %q", htmlFull.HTML)
	}
	cidSource := source
	cidSource.HTMLHasCIDReferences = true
	cidFull, err := BuildReplyBody(htmlPlan, cidSource)
	if err != nil {
		t.Fatalf("cid full body: %v", err)
	}
	if strings.Contains(cidFull.HTML, "<p>original source") || !strings.Contains(cidFull.HTML, "<pre>original source line one") {
		t.Fatalf("cid fallback html = %q", cidFull.HTML)
	}
	htmlOnly := source
	htmlOnly.Text = nil
	htmlOnly.HTML = []byte("<p>html only</p>")
	htmlOnly.HTMLHasCIDReferences = false
	if _, err := BuildReplyBody(htmlPlan, htmlOnly); !hasCode(err, errs.CodeInvalidComposeInput) {
		t.Fatalf("html-only full quote error = %v", err)
	}

	plan.Quote = mail.QuoteModeNone
	none, err := BuildReplyBody(plan, SourceMaterial{})
	if err != nil {
		t.Fatalf("none body: %v", err)
	}
	if none.ContentType != "text" || none.Text != "<one>\ntwo" || strings.Contains(none.Text, "ignored") {
		t.Fatalf("none body = %#v", none)
	}

	mdPlan, err := BuildReply(ReplyOptions{Quote: "none", Body: BodyInput{InlineSet: true, Inline: "# Hi\n<script>x()</script>", MarkdownSet: true}})
	if err != nil {
		t.Fatal(err)
	}
	mdNone, err := BuildReplyBody(mdPlan, SourceMaterial{})
	if err != nil {
		t.Fatalf("markdown none body: %v", err)
	}
	if mdNone.ContentType != "html" || strings.Contains(strings.ToLower(mdNone.HTML), "script") || !strings.Contains(mdNone.HTML, "<h1>Hi</h1>") {
		t.Fatalf("markdown none = %#v", mdNone)
	}
}

func TestBuildReplyBodyShortQuote(t *testing.T) {
	source := replyTestSource()
	plan, err := BuildReply(ReplyOptions{Quote: "short", QuoteLanguage: "de", Body: BodyInput{InlineSet: true, Inline: "Danke"}})
	if err != nil {
		t.Fatal(err)
	}
	short, err := BuildReplyBody(plan, source)
	if err != nil {
		t.Fatalf("short text: %v", err)
	}
	for _, want := range []string{"Danke", "Am 2026-06-28 10:00 UTC schrieb Sender <sender@example.test>:", "> original source line one"} {
		if !strings.Contains(short.Text, want) {
			t.Fatalf("short text missing %q: %q", want, short.Text)
		}
	}

	htmlPlan, err := BuildReply(ReplyOptions{Quote: "short", QuoteLanguage: "en", Body: BodyInput{InlineSet: true, Inline: "<b>ok</b>", HTMLSet: true}})
	if err != nil {
		t.Fatal(err)
	}
	hostile := source
	hostile.Text = []byte(`<script>alert(1)</script>` + strings.Repeat("x", MaxShortQuoteRunes+20))
	htmlBody, err := BuildReplyBody(htmlPlan, hostile)
	if err != nil {
		t.Fatalf("short html: %v", err)
	}
	if !strings.Contains(htmlBody.HTML, "On 2026-06-28 10:00 UTC, Sender &lt;sender@example.test&gt; wrote:") ||
		!strings.Contains(htmlBody.HTML, "<blockquote><pre>") ||
		strings.Contains(htmlBody.HTML, "<script>") ||
		strings.Count(htmlBody.HTML, "x") > MaxShortQuoteRunes {
		t.Fatalf("short html = %q", htmlBody.HTML)
	}

	plan.Quote = mail.QuoteModeShort
	if _, err := BuildReplyBody(plan, SourceMaterial{}); !hasCode(err, errs.CodeInvalidComposeInput) {
		t.Fatalf("nil source err = %#v", err)
	}
}

func replyTestSource() SourceMaterial {
	sent := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	return SourceMaterial{
		SourceID: "src",
		From:     mail.Recipient{Name: "Sender", Address: "sender@example.test"},
		SentAt:   &sent,
		Text:     []byte("original source line one\noriginal source line two"),
		HTML:     []byte("<p>original source line one<br>original source line two</p>"),
	}
}

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// A body that ends with line breaks, as a file usually does, gets exactly
// one empty line before the quote, in every quote mode and in forwards.
func TestOneEmptyLineBeforeTheQuote(t *testing.T) {
	source := replyTestSource()
	for _, body := range []string{"Hi Maria,\n\nthanks!\n\nBest,\nSam", "Hi Maria,\n\nthanks!\n\nBest,\nSam\n", "Hi Maria,\n\nthanks!\n\nBest,\nSam\r\n\r\n"} {
		for _, quote := range []string{"full", "short"} {
			plan, err := BuildReply(ReplyOptions{Quote: quote, QuoteLanguage: "en", Body: BodyInput{FileSet: true, Bytes: []byte(body)}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := BuildReplyBody(plan, source)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got.Text, "Hi Maria,\n\nthanks!\n\nBest,\nSam\n\nOn ") {
				t.Errorf("%s quote of %q = %q", quote, body, got.Text)
			}
		}
		plan, err := BuildForward(ForwardOptions{Body: BodyInput{FileSet: true, Bytes: []byte(body)}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := BuildForwardBody(plan, source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got.Text, "Hi Maria,\n\nthanks!\n\nBest,\nSam\n\n---------- Forwarded message") {
			t.Errorf("forward of %q = %q", body, got.Text)
		}
	}
}
