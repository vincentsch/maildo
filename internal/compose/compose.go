// Package compose turns what the user typed for a new message, reply or
// forward into a checked message ready for any provider. It renders Markdown,
// cleans HTML and builds the summary that previews show.
package compose

import (
	"bytes"
	"html"
	netmail "net/mail"
	"regexp"
	"strings"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldhtml "github.com/yuin/goldmark/renderer/html"
)

// RecipientInput is the recipient flags before parsing. The Set fields tell a
// flag that was not given apart from one given with an empty or bad value.
type RecipientInput struct {
	To, CC, BCC, ReplyTo                     []string
	ToSet, CCSet, BCCSet, ReplyToSet         bool
	ClearTo, ClearCC, ClearBCC, ClearReplyTo bool
}

// SenderInput is the --from flag. maildo only checks it against the account;
// it never lets a provider send as someone else.
type SenderInput struct {
	Provided bool
	Raw      string
}

// SubjectInput records whether --subject was given, so draft update can
// clear the subject with --subject "".
type SubjectInput struct {
	Provided bool
	Value    string
}

// BodyInput is the body and its format. cmd/maildo reads a file or stdin
// before this package checks and renders the body.
type BodyInput struct {
	InlineSet   bool
	Inline      string
	FileSet     bool
	Bytes       []byte
	FromStdin   bool
	TextSet     bool
	MarkdownSet bool
	HTMLSet     bool
}

// CreateInput is what the user gave draft create, not yet checked.
type CreateInput struct {
	Recipients RecipientInput
	Sender     SenderInput
	Subject    SubjectInput
	Body       BodyInput
}

// SendInput is what the user gave send, not yet checked.
type SendInput struct {
	Recipients RecipientInput
	Sender     SenderInput
	Subject    SubjectInput
	Body       BodyInput
}

// UpdateInput is what the user gave draft update, not yet checked.
type UpdateInput struct {
	Recipients RecipientInput
	Sender     SenderInput
	Subject    SubjectInput
	Body       BodyInput
}

// BodySource says where the body came from, for --dry-run output. It never
// holds the file path or the body.
type BodySource string

const (
	SourceInline BodySource = "inline"
	SourceFile   BodySource = "file"
	SourceStdin  BodySource = "stdin"
)

// Summary describes a message for previews and results, without the body.
type Summary struct {
	ToCount       int
	CCCount       int
	BCCCount      int
	ReplyToCount  int
	SubjectSet    bool
	BodyPresent   bool
	BodyFormat    mail.BodyFormat
	BodySource    BodySource
	BodyBytes     int
	RequestedFrom string
}

// Mail is not a web page: a single line break in a Markdown body, as in a
// signature ("Best,\nAnn"), stays a line break.
var markdownRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(goldhtml.WithHardWraps()))
var sanitizer = mailPolicy()

// BuildCreate checks and renders a new draft.
func BuildCreate(in CreateInput, accountEmail string) (mail.CreateDraftRequest, Summary, error) {
	msg, summary, err := buildMessage(in.Recipients, in.Sender, in.Subject, in.Body, accountEmail)
	if err != nil {
		return mail.CreateDraftRequest{}, Summary{}, err
	}
	if len(msg.To)+len(msg.CC)+len(msg.BCC) == 0 &&
		strings.TrimSpace(msg.Subject) == "" &&
		!msg.Body.Present {
		return mail.CreateDraftRequest{}, Summary{}, invalid("message", "the draft is empty; give at least a recipient, a --subject or a body")
	}
	return mail.CreateDraftRequest{Message: msg}, summary, nil
}

// BuildSend checks and renders a new message to send.
func BuildSend(in SendInput, accountEmail string) (mail.SendMessageRequest, Summary, error) {
	msg, summary, err := buildMessage(in.Recipients, in.Sender, in.Subject, in.Body, accountEmail)
	if err != nil {
		return mail.SendMessageRequest{}, Summary{}, err
	}
	if len(msg.To)+len(msg.CC)+len(msg.BCC) == 0 {
		return mail.SendMessageRequest{}, Summary{}, invalid("recipients", "add at least one recipient with --to, --cc or --bcc")
	}
	if strings.TrimSpace(msg.Subject) == "" && !bodyHasActualContent(msg.Body) {
		return mail.SendMessageRequest{}, Summary{}, invalid("message", "the message needs a --subject or a body")
	}
	return mail.SendMessageRequest{Message: msg}, summary, nil
}

// BuildUpdate checks and renders a draft update. An empty body given on
// purpose is a change too: it clears the draft's body.
func BuildUpdate(draftID string, in UpdateInput, accountEmail string) (mail.UpdateDraftRequest, Summary, error) {
	if strings.TrimSpace(draftID) == "" {
		return mail.UpdateDraftRequest{}, Summary{}, invalid("id", "--id requires a value")
	}
	from, requestedFrom, err := parseSender(in.Sender, accountEmail)
	if err != nil {
		return mail.UpdateDraftRequest{}, Summary{}, err
	}
	_ = from
	body, source, err := parseBody(in.Body)
	if err != nil {
		return mail.UpdateDraftRequest{}, Summary{}, err
	}
	patch := mail.DraftPatch{}
	summary := Summary{
		SubjectSet:    in.Subject.Provided,
		BodyPresent:   body.Present,
		BodyFormat:    body.Format,
		BodySource:    source,
		BodyBytes:     body.Bytes,
		RequestedFrom: requestedFrom,
	}
	changed := false
	// Every recipient field is replaced or cleared the same way.
	for _, spec := range []struct {
		field string
		set   bool
		clear bool
		raw   []string
		dst   *mail.RecipientPatch
		count *int
	}{
		{field: "to", set: in.Recipients.ToSet, clear: in.Recipients.ClearTo, raw: in.Recipients.To, dst: &patch.To, count: &summary.ToCount},
		{field: "cc", set: in.Recipients.CCSet, clear: in.Recipients.ClearCC, raw: in.Recipients.CC, dst: &patch.CC, count: &summary.CCCount},
		{field: "bcc", set: in.Recipients.BCCSet, clear: in.Recipients.ClearBCC, raw: in.Recipients.BCC, dst: &patch.BCC, count: &summary.BCCCount},
		{field: "reply_to", set: in.Recipients.ReplyToSet, clear: in.Recipients.ClearReplyTo, raw: in.Recipients.ReplyTo, dst: &patch.ReplyTo, count: &summary.ReplyToCount},
	} {
		if spec.set && spec.clear {
			return mail.UpdateDraftRequest{}, Summary{}, invalid(spec.field, "--"+flagName(spec.field)+" and --clear-"+flagName(spec.field)+" cannot be used together")
		}
		if spec.clear {
			spec.dst.Clear = true
			changed = true
			continue
		}
		if spec.set {
			values, err := parseRecipients(spec.field, spec.raw)
			if err != nil {
				return mail.UpdateDraftRequest{}, Summary{}, err
			}
			spec.dst.Set = true
			spec.dst.Values = values
			*spec.count = len(values)
			changed = true
		}
	}
	if in.Subject.Provided {
		if err := checkSubject(in.Subject.Value); err != nil {
			return mail.UpdateDraftRequest{}, Summary{}, err
		}
		patch.Subject.Set = true
		patch.Subject.Value = in.Subject.Value
		changed = true
	}
	if in.Body.InlineSet || in.Body.FileSet {
		patch.Body.Set = true
		patch.Body.Body = body
		changed = true
	}
	if !changed {
		return mail.UpdateDraftRequest{}, Summary{}, invalid("message", "nothing to change; give the fields to change, such as --subject or --to")
	}
	return mail.UpdateDraftRequest{DraftID: strings.TrimSpace(draftID), Patch: patch}, summary, nil
}

// checkSubject refuses line breaks and other control characters. A subject
// is one line; a pasted newline would otherwise end up encoded in the
// header, and in the command a preview prints back.
func checkSubject(subject string) error {
	for _, r := range subject {
		if unicode.IsControl(r) && r != '\t' {
			return invalid("subject", "the subject must be one line without control characters")
		}
	}
	return nil
}

// buildMessage turns the input into a message ready to send, and the summary
// that previews show.
func buildMessage(recipients RecipientInput, sender SenderInput, subject SubjectInput, bodyIn BodyInput, accountEmail string) (mail.OutboundMessage, Summary, error) {
	to, err := parseRecipients("to", recipients.To)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	cc, err := parseRecipients("cc", recipients.CC)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	bcc, err := parseRecipients("bcc", recipients.BCC)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	replyTo, err := parseRecipients("reply_to", recipients.ReplyTo)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	from, requestedFrom, err := parseSender(sender, accountEmail)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	body, source, err := parseBody(bodyIn)
	if err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	if err := checkSubject(subject.Value); err != nil {
		return mail.OutboundMessage{}, Summary{}, err
	}
	msg := mail.OutboundMessage{
		From:    from,
		To:      to,
		CC:      cc,
		BCC:     bcc,
		ReplyTo: replyTo,
		Subject: subject.Value,
		Body:    body,
	}
	summary := Summary{
		ToCount:       len(to),
		CCCount:       len(cc),
		BCCCount:      len(bcc),
		ReplyToCount:  len(replyTo),
		SubjectSet:    subject.Provided,
		BodyPresent:   body.Present,
		BodyFormat:    body.Format,
		BodySource:    source,
		BodyBytes:     body.Bytes,
		RequestedFrom: requestedFrom,
	}
	return msg, summary, nil
}

// parseRecipients reads a repeatable address flag. Each value may hold a
// comma-separated address list; display names are kept.
func parseRecipients(field string, values []string) ([]mail.Recipient, error) {
	out, err := mail.ParseMailboxListValues(values)
	if err != nil {
		return nil, invalid(field, mail.InvalidAddressMessage(flagName(field), err))
	}
	return out, nil
}

// flagName turns a field name such as reply_to into its flag, reply-to.
func flagName(field string) string {
	return strings.ReplaceAll(field, "_", "-")
}

// parseSender checks that --from is the account's own address.
func parseSender(in SenderInput, accountEmail string) (*mail.Recipient, string, error) {
	if !in.Provided {
		return nil, "", nil
	}
	addrs, err := netmail.ParseAddressList(in.Raw)
	if err != nil {
		return nil, "", invalid("from", "--from is not a valid address")
	}
	if len(addrs) != 1 {
		return nil, "", invalid("from", "--from takes one address")
	}
	addr := strings.TrimSpace(addrs[0].Address)
	if !strings.EqualFold(addr, strings.TrimSpace(accountEmail)) {
		return nil, "", invalid("from", "--from must be the account's own address")
	}
	return &mail.Recipient{Name: addrs[0].Name, Address: addr}, in.Raw, nil
}

// parseBody checks that the body comes from one place in one format, then
// renders it.
func parseBody(in BodyInput) (mail.OutboundBody, BodySource, error) {
	if in.InlineSet && in.FileSet {
		return mail.OutboundBody{}, "", invalid("body", "--body and --body-file cannot be used together")
	}
	formatCount := boolInt(in.TextSet) + boolInt(in.MarkdownSet) + boolInt(in.HTMLSet)
	if formatCount > 1 {
		return mail.OutboundBody{}, "", invalid("body_format", "use only one of --text, --markdown and --html")
	}
	format := mail.BodyFormatText
	if in.MarkdownSet {
		format = mail.BodyFormatMarkdown
	}
	if in.HTMLSet {
		format = mail.BodyFormatHTML
	}
	if !in.InlineSet && !in.FileSet {
		return mail.OutboundBody{Format: format, ContentType: "text"}, "", nil
	}
	source := SourceInline
	src := []byte(in.Inline)
	if in.FileSet {
		source = SourceFile
		if in.FromStdin {
			source = SourceStdin
		}
		src = in.Bytes
	}
	body, err := renderBody(src, format)
	if err != nil {
		return mail.OutboundBody{}, "", err
	}
	return body, source, nil
}

// renderBody returns the body for the provider and a plain text version.
// Commands that create, update or send never print either.
func renderBody(src []byte, format mail.BodyFormat) (mail.OutboundBody, error) {
	switch format {
	case mail.BodyFormatText:
		text := string(src)
		return mail.OutboundBody{
			Format:      mail.BodyFormatText,
			ContentType: "text",
			Text:        text,
			HTML:        textToHTML(text),
			Bytes:       len(src),
			Present:     len(src) > 0,
		}, nil
	case mail.BodyFormatMarkdown:
		rendered, err := markdownToHTML(src)
		if err != nil {
			return mail.OutboundBody{}, invalid("body", "maildo could not turn the Markdown body into HTML")
		}
		htmlBody := sanitize(rendered)
		return mail.OutboundBody{
			Format:      mail.BodyFormatMarkdown,
			ContentType: "html",
			Text:        htmltext.ToReadable(htmlBody),
			HTML:        htmlBody,
			Bytes:       len(src),
			Present:     len(src) > 0,
		}, nil
	case mail.BodyFormatHTML:
		htmlBody := sanitize(string(src))
		return mail.OutboundBody{
			Format:      mail.BodyFormatHTML,
			ContentType: "html",
			Text:        htmltext.ToReadable(htmlBody),
			HTML:        htmlBody,
			Bytes:       len(src),
			Present:     len(src) > 0,
		}, nil
	default:
		return mail.OutboundBody{}, invalid("body_format", "unsupported body format")
	}
}

func bodyHasActualContent(body mail.OutboundBody) bool {
	switch body.Format {
	case mail.BodyFormatText:
		return len(body.Text) > 0
	case mail.BodyFormatMarkdown, mail.BodyFormatHTML:
		return len(body.Text) > 0 || len(body.HTML) > 0
	default:
		return len(body.Text) > 0 || len(body.HTML) > 0
	}
}

func markdownToHTML(src []byte) (string, error) {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert(src, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func sanitize(src string) string {
	return sanitizer.Sanitize(src)
}

// SanitizeHTML cleans HTML with the same rules maildo uses for the bodies it
// sends.
func SanitizeHTML(src string) string {
	return sanitize(src)
}

func textToHTML(src string) string {
	if src == "" {
		return ""
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	return "<p>" + strings.ReplaceAll(html.EscapeString(normalized), "\n", "<br>\n") + "</p>"
}

// mailPolicy keeps the formatting mail programs show and drops anything
// active, such as scripts and forms. Links keep only the schemes allowed
// below.
func mailPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"p", "br", "blockquote", "ul", "ol", "li", "pre", "code",
		"h1", "h2", "h3", "h4", "h5", "h6", "strong", "em", "b", "i", "a",
		"table", "thead", "tbody", "tr", "td", "th", "hr", "span", "div", "img",
	)
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("src").Matching(regexp.MustCompile(`(?i)^cid:[^[:space:]<>"']+$`)).OnElements("img")
	p.AllowAttrs("alt", "title").OnElements("img")
	p.AllowAttrs("title").Globally()
	p.AllowAttrs("align").OnElements("td", "th", "tr")
	p.AllowAttrs("colspan", "rowspan").OnElements("td", "th")
	p.AllowURLSchemes("http", "https", "mailto", "cid")
	p.RequireParseableURLs(true)
	return p
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func invalid(field, message string) error {
	return errs.NewWithDetails(errs.CodeInvalidComposeInput, message, map[string]any{
		"field": field,
	})
}
