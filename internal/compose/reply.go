package compose

import (
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/mail"
)

const (
	MaxShortQuoteRunes   = 1200
	ShortQuoteWrapColumn = 78
	ShortQuoteDateFormat = "2006-01-02 15:04 MST"
)

// SourceMaterial is the part of the original message a reply needs, read by
// the command with the size limits applied.
type SourceMaterial struct {
	SourceID             string
	InternetMessageID    string
	ThreadID             string
	From                 mail.Recipient
	ReplyTo              []mail.Recipient
	To                   []mail.Recipient
	CC                   []mail.Recipient
	Subject              string
	SentAt               *time.Time
	InReplyTo            []string
	References           []string
	Text                 []byte
	HTML                 []byte
	HTMLHasCIDReferences bool
}

// ReplyOptions is what the user gave the reply command, not yet checked.
type ReplyOptions struct {
	ReplyAll      bool
	Draft         bool
	Send          bool
	Quote         string
	QuoteLanguage string
	Body          BodyInput
	EnvLookup     func(string) (string, bool)
}

// ReplyPlan is a checked reply with its body rendered.
type ReplyPlan struct {
	Mode     mail.ReplyMode
	Action   mail.ComposeExecutionAction
	Quote    mail.QuoteMode
	Language mail.QuoteLanguage
	Body     mail.OutboundBody
	Summary  Summary
}

// BuildReply checks the reply's input and renders the user's text. The text
// is required, so a reply is never just a quote.
func BuildReply(in ReplyOptions) (ReplyPlan, error) {
	if in.Draft && in.Send {
		return ReplyPlan{}, invalid("mode", "--draft and --send cannot be used together")
	}
	action := mail.ComposeExecutionDraft
	if in.Send {
		action = mail.ComposeExecutionSend
	}
	mode := mail.ReplyModeReply
	if in.ReplyAll {
		mode = mail.ReplyModeReplyAll
	}
	quote, err := ParseQuoteMode(in.Quote)
	if err != nil {
		return ReplyPlan{}, err
	}
	lang, err := ResolveQuoteLanguage(in.QuoteLanguage, in.EnvLookup)
	if err != nil {
		return ReplyPlan{}, err
	}
	body, source, err := parseBody(in.Body)
	if err != nil {
		return ReplyPlan{}, err
	}
	if !body.Present {
		return ReplyPlan{}, invalid("body", "the reply has no text; add --body or --body-file")
	}
	// parseBody has rendered the body; check the text as the user typed it,
	// so a body of only spaces cannot turn into a reply that is just a quote.
	raw := in.Body.Inline
	if in.Body.FileSet {
		raw = string(in.Body.Bytes)
	}
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(raw) == "" {
		return ReplyPlan{}, invalid("body", "the reply's text is blank; write something")
	}
	summary := Summary{
		BodyPresent: body.Present,
		BodyFormat:  body.Format,
		BodySource:  source,
		BodyBytes:   body.Bytes,
	}
	return ReplyPlan{
		Mode:     mode,
		Action:   action,
		Quote:    quote,
		Language: lang,
		Body:     body,
		Summary:  summary,
	}, nil
}

// ParseQuoteMode checks a --quote value.
func ParseQuoteMode(raw string) (mail.QuoteMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(mail.QuoteModeFull):
		return mail.QuoteModeFull, nil
	case string(mail.QuoteModeShort):
		return mail.QuoteModeShort, nil
	case string(mail.QuoteModeNone):
		return mail.QuoteModeNone, nil
	default:
		return "", invalid("quote", "--quote must be full, short or none")
	}
}

// ResolveQuoteLanguage checks --quote-language and turns auto into en or de,
// based on the language settings in the environment.
func ResolveQuoteLanguage(raw string, lookup func(string) (string, bool)) (mail.QuoteLanguage, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(mail.QuoteLanguageEnglish):
		return mail.QuoteLanguageEnglish, nil
	case string(mail.QuoteLanguageGerman):
		return mail.QuoteLanguageGerman, nil
	case "", string(mail.QuoteLanguageAuto):
		if lookup == nil {
			return mail.QuoteLanguageEnglish, nil
		}
		for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
			value, ok := lookup(name)
			if !ok || strings.TrimSpace(value) == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "de") {
				return mail.QuoteLanguageGerman, nil
			}
			return mail.QuoteLanguageEnglish, nil
		}
		return mail.QuoteLanguageEnglish, nil
	default:
		return "", invalid("quote_language", "--quote-language must be en, de or auto")
	}
}

// BuildReplyBody builds the reply's body from the user's text and the
// original message.
func BuildReplyBody(plan ReplyPlan, source SourceMaterial) (mail.OutboundBody, error) {
	switch plan.Quote {
	case mail.QuoteModeFull:
		if source.SourceID == "" {
			return mail.OutboundBody{}, invalid("quote", "maildo could not read the original message to quote it")
		}
		return buildFullReplyBody(plan, source)
	case mail.QuoteModeNone:
		return plan.Body, nil
	case mail.QuoteModeShort:
		if source.SourceID == "" {
			return mail.OutboundBody{}, invalid("quote", "maildo could not read the original message to quote it")
		}
		quote := replyQuoteSourceFromMaterial(source)
		if strings.TrimSpace(quote.Body) == "" {
			return mail.OutboundBody{}, invalid("quote", "maildo could not read the body of the original message to quote it")
		}
		return buildShortReplyBody(plan, quote), nil
	default:
		return mail.OutboundBody{}, invalid("quote", "--quote must be full, short or none")
	}
}

type replyQuoteSource struct {
	Sender mail.Recipient
	SentAt *time.Time
	Body   string
}

func replyQuoteSourceFromMaterial(material SourceMaterial) replyQuoteSource {
	body := string(material.Text)
	return replyQuoteSource{
		Sender: material.From,
		SentAt: material.SentAt,
		Body:   body,
	}
}

func buildFullReplyBody(plan ReplyPlan, material SourceMaterial) (mail.OutboundBody, error) {
	sourceText := string(material.Text)
	if strings.TrimSpace(sourceText) == "" {
		return mail.OutboundBody{}, invalid("quote", "maildo could not read the body of the original message to quote it")
	}
	// The quote starts with "On <date>, <sender> wrote:", as in a short
	// quote, and has no empty quoted line at the end.
	header := quoteHeader(plan.Language, replyQuoteSourceFromMaterial(material))
	quoted := header + "\n\n" + quotePlain(strings.TrimRight(sourceText, "\r\n"))
	if plan.Body.ContentType == "text" {
		text := joinBodies(plan.Body.Text, quoted)
		return mail.OutboundBody{
			Format:      plan.Body.Format,
			ContentType: "text",
			Text:        text,
			HTML:        textToHTML(text),
			Bytes:       len(text),
			Present:     true,
		}, nil
	}
	quoteHTML := ""
	if len(material.HTML) > 0 && !material.HTMLHasCIDReferences {
		quoteHTML = sanitize(string(material.HTML))
	}
	if strings.TrimSpace(quoteHTML) == "" {
		quoteHTML = "<pre>" + html.EscapeString(sourceText) + "</pre>"
	}
	htmlBody := joinHTMLBodies(plan.Body.HTML, "<p>"+html.EscapeString(header)+"</p><blockquote>"+quoteHTML+"</blockquote>")
	text := joinBodies(plan.Body.Text, quoted)
	return mail.OutboundBody{
		Format:      plan.Body.Format,
		ContentType: "html",
		Text:        text,
		HTML:        htmlBody,
		Bytes:       len(text) + len(htmlBody),
		Present:     true,
	}, nil
}

func buildShortReplyBody(plan ReplyPlan, source replyQuoteSource) mail.OutboundBody {
	if plan.Body.ContentType == "text" {
		text := joinBodies(plan.Body.Text, buildShortQuotePlain(plan.Language, source))
		return mail.OutboundBody{
			Format:      plan.Body.Format,
			ContentType: "text",
			Text:        text,
			HTML:        textToHTML(text),
			Bytes:       len(text),
			Present:     true,
		}
	}
	htmlBody := joinHTMLBodies(plan.Body.HTML, buildShortQuoteHTML(plan.Language, source))
	text := joinBodies(plan.Body.Text, buildShortQuotePlain(plan.Language, source))
	return mail.OutboundBody{
		Format:      plan.Body.Format,
		ContentType: "html",
		Text:        text,
		HTML:        htmlBody,
		Bytes:       len(text) + len(htmlBody),
		Present:     true,
	}
}

// joinBodies puts one empty line between the user's text and the quote. A
// body from a file usually ends with a line break; it does not add a
// second empty line.
func joinBodies(user, quoted string) string {
	switch {
	case user == "":
		return quoted
	case quoted == "":
		return user
	default:
		return strings.TrimRight(user, "\r\n") + "\n\n" + quoted
	}
}

func joinHTMLBodies(user, quoted string) string {
	switch {
	case user == "":
		return quoted
	case quoted == "":
		return user
	default:
		return user + "<br><br>" + quoted
	}
}

func quotePlain(body string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// buildShortQuotePlain quotes the start of the original for a text reply,
// with "> " in front of each line.
func buildShortQuotePlain(lang mail.QuoteLanguage, source replyQuoteSource) string {
	lines := shortQuoteBodyLines(source.Body)
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return quoteHeader(lang, source) + "\n\n" + strings.Join(lines, "\n")
}

// buildShortQuoteHTML puts the start of the original into a blockquote. It
// escapes the text first, because the quote is plain text, not HTML.
func buildShortQuoteHTML(lang mail.QuoteLanguage, source replyQuoteSource) string {
	lines := shortQuoteBodyLines(source.Body)
	return "<p>" + html.EscapeString(quoteHeader(lang, source)) + "</p>" +
		"<blockquote><pre>" + html.EscapeString(strings.Join(lines, "\n")) + "</pre></blockquote>"
}

func quoteHeader(lang mail.QuoteLanguage, source replyQuoteSource) string {
	date := ""
	if source.SentAt != nil {
		date = source.SentAt.Format(ShortQuoteDateFormat)
	}
	sender := shortQuoteSender(source, lang)
	if lang == mail.QuoteLanguageGerman {
		return "Am " + date + " schrieb " + sender + ":"
	}
	return "On " + date + ", " + sender + " wrote:"
}

func shortQuoteSender(source replyQuoteSource, lang mail.QuoteLanguage) string {
	name := strings.TrimSpace(source.Sender.Name)
	addr := strings.TrimSpace(source.Sender.Address)
	switch {
	case name != "" && addr != "":
		return name + " <" + addr + ">"
	case addr != "":
		return addr
	case name != "":
		return name
	default:
		if lang == mail.QuoteLanguageGerman {
			return "unbekannter Absender"
		}
		return "unknown sender"
	}
}

// shortQuoteBodyLines cuts the original to a fixed number of characters and
// wraps the lines, so a short quote is always short and always looks the
// same.
func shortQuoteBodyLines(body string) []string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	if utf8.RuneCountInString(body) > MaxShortQuoteRunes {
		runes := []rune(body)
		body = string(runes[:MaxShortQuoteRunes])
	}
	rawLines := strings.Split(body, "\n")
	out := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		out = append(out, wrapRunes(line, ShortQuoteWrapColumn)...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// wrapRunes breaks lines between words where it can. It splits a very long
// word only between characters, never inside one, so non-ASCII text stays
// intact.
func wrapRunes(line string, col int) []string {
	line = strings.TrimSpace(line)
	if line == "" || col <= 0 {
		return []string{line}
	}
	words := strings.Fields(line)
	out := make([]string, 0, len(words))
	current := ""
	for _, word := range words {
		for utf8.RuneCountInString(word) > col {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			runes := []rune(word)
			out = append(out, string(runes[:col]))
			word = string(runes[col:])
		}
		if word == "" {
			continue
		}
		if current == "" {
			current = word
			continue
		}
		if utf8.RuneCountInString(current)+1+utf8.RuneCountInString(word) <= col {
			current += " " + word
			continue
		}
		out = append(out, current)
		current = word
	}
	if current != "" {
		out = append(out, current)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
