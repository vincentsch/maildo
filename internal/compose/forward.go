package compose

import (
	"html"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
)

// ForwardOptions is what the user gave the forward command, not yet checked.
type ForwardOptions struct {
	Draft                  bool
	Send                   bool
	Recipients             RecipientInput
	Body                   BodyInput
	IncludeAttachments     string
	IncludeAttachmentNames []string
	IncludeAttachmentIDs   []string
}

// ForwardPlan is a checked forward with its body rendered.
type ForwardPlan struct {
	Action      mail.ComposeExecutionAction
	To          []mail.Recipient
	CC          []mail.Recipient
	BCC         []mail.Recipient
	Body        mail.OutboundBody
	Summary     Summary
	Attachments mail.ForwardAttachmentSelection
}

// BuildForward checks the forward's input and renders the user's text. The
// text is required, so maildo never forwards a message without a word from
// the user.
func BuildForward(in ForwardOptions) (ForwardPlan, error) {
	if in.Draft && in.Send {
		return ForwardPlan{}, invalid("mode", "--draft and --send cannot be used together")
	}
	action := mail.ComposeExecutionDraft
	if in.Send {
		action = mail.ComposeExecutionSend
	}
	to, err := parseRecipients("to", in.Recipients.To)
	if err != nil {
		return ForwardPlan{}, err
	}
	cc, err := parseRecipients("cc", in.Recipients.CC)
	if err != nil {
		return ForwardPlan{}, err
	}
	bcc, err := parseRecipients("bcc", in.Recipients.BCC)
	if err != nil {
		return ForwardPlan{}, err
	}
	if action == mail.ComposeExecutionSend && len(to)+len(cc)+len(bcc) == 0 {
		return ForwardPlan{}, invalid("recipients", "a forward with --send needs at least one recipient; add --to")
	}
	body, source, err := parseBody(in.Body)
	if err != nil {
		return ForwardPlan{}, err
	}
	if !body.Present {
		return ForwardPlan{}, invalid("body", "the forward has no text of your own; add --body or --body-file")
	}
	raw := in.Body.Inline
	if in.Body.FileSet {
		raw = string(in.Body.Bytes)
	}
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(raw) == "" {
		return ForwardPlan{}, invalid("body", "the forward's text is blank; write something")
	}
	selection, err := BuildForwardAttachmentSelection(in.IncludeAttachments, in.IncludeAttachmentNames, in.IncludeAttachmentIDs)
	if err != nil {
		return ForwardPlan{}, err
	}
	return ForwardPlan{
		Action: action,
		To:     to,
		CC:     cc,
		BCC:    bcc,
		Body:   body,
		Summary: Summary{
			ToCount:     len(to),
			CCCount:     len(cc),
			BCCCount:    len(bcc),
			BodyPresent: body.Present,
			BodyFormat:  body.Format,
			BodySource:  source,
			BodyBytes:   body.Bytes,
		},
		Attachments: selection,
	}, nil
}

// BuildForwardBody builds the forward's body from the user's text and the
// original message.
func BuildForwardBody(plan ForwardPlan, source SourceMaterial) (mail.OutboundBody, error) {
	if source.SourceID == "" {
		return mail.OutboundBody{}, invalid("source", "maildo could not read the original message for the forward")
	}
	return buildForwardBodyFromMaterial(plan, source)
}

func buildForwardBodyFromMaterial(plan ForwardPlan, material SourceMaterial) (mail.OutboundBody, error) {
	if strings.TrimSpace(material.From.Address) == "" && strings.TrimSpace(material.From.Name) == "" {
		return mail.OutboundBody{}, invalid("source", "the original message has no sender that maildo could read")
	}
	sourceText := string(material.Text)
	if strings.TrimSpace(sourceText) == "" {
		return mail.OutboundBody{}, invalid("source", "maildo could not read the body of the original message")
	}
	forwardedPlain := forwardedPlainBlock(material, sourceText)
	if plan.Body.ContentType == "text" {
		text := joinBodies(plan.Body.Text, forwardedPlain)
		return mail.OutboundBody{
			Format:      plan.Body.Format,
			ContentType: "text",
			Text:        text,
			HTML:        textToHTML(text),
			Bytes:       len(text),
			Present:     true,
		}, nil
	}
	htmlBlock := forwardedHTMLBlock(material, sourceText)
	text := joinBodies(plan.Body.Text, forwardedPlain)
	htmlBody := joinHTMLBodies(plan.Body.HTML, htmlBlock)
	return mail.OutboundBody{
		Format:      plan.Body.Format,
		ContentType: "html",
		Text:        text,
		HTML:        htmlBody,
		Bytes:       len(text) + len(htmlBody),
		Present:     true,
	}, nil
}

func forwardedPlainBlock(material SourceMaterial, sourceText string) string {
	lines := []string{"---------- Forwarded message ---------"}
	lines = append(lines, "From: "+formatRecipient(material.From))
	if material.SentAt != nil {
		lines = append(lines, "Date: "+material.SentAt.Format(time.RFC1123Z))
	}
	lines = append(lines, "Subject: "+material.Subject)
	if len(material.To) > 0 {
		lines = append(lines, "To: "+formatRecipients(material.To))
	}
	if len(material.CC) > 0 {
		lines = append(lines, "Cc: "+formatRecipients(material.CC))
	}
	lines = append(lines, "", normalizeNewlines(sourceText))
	return strings.Join(lines, "\n")
}

func forwardedHTMLBlock(material SourceMaterial, sourceText string) string {
	var b strings.Builder
	b.WriteString("<div>---------- Forwarded message ---------</div>")
	b.WriteString("<div>From: ")
	b.WriteString(html.EscapeString(formatRecipient(material.From)))
	b.WriteString("</div>")
	if material.SentAt != nil {
		b.WriteString("<div>Date: ")
		b.WriteString(html.EscapeString(material.SentAt.Format(time.RFC1123Z)))
		b.WriteString("</div>")
	}
	b.WriteString("<div>Subject: ")
	b.WriteString(html.EscapeString(material.Subject))
	b.WriteString("</div>")
	if len(material.To) > 0 {
		b.WriteString("<div>To: ")
		b.WriteString(html.EscapeString(formatRecipients(material.To)))
		b.WriteString("</div>")
	}
	if len(material.CC) > 0 {
		b.WriteString("<div>Cc: ")
		b.WriteString(html.EscapeString(formatRecipients(material.CC)))
		b.WriteString("</div>")
	}
	b.WriteString("<br>")
	if len(material.HTML) > 0 {
		b.WriteString(sanitize(string(material.HTML)))
	} else {
		b.WriteString(textToHTML(sourceText))
	}
	return b.String()
}

func formatRecipients(recipients []mail.Recipient) string {
	out := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		formatted := formatRecipient(recipient)
		if formatted != "" {
			out = append(out, formatted)
		}
	}
	return strings.Join(out, ", ")
}

func formatRecipient(recipient mail.Recipient) string {
	name := strings.TrimSpace(recipient.Name)
	addr := strings.TrimSpace(recipient.Address)
	switch {
	case name != "" && addr != "":
		return name + " <" + addr + ">"
	case addr != "":
		return addr
	case name != "":
		return name
	default:
		return ""
	}
}

func normalizeNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func BuildForwardAttachmentSelection(modeRaw string, names, ids []string) (mail.ForwardAttachmentSelection, error) {
	// Like mail programs, a forward takes the original's attachments along
	// unless told otherwise. An empty mode means the user chose nothing.
	mode := mail.ForwardAttachmentsAll
	switch strings.ToLower(strings.TrimSpace(modeRaw)) {
	case "":
	case string(mail.ForwardAttachmentsNone):
		mode = mail.ForwardAttachmentsNone
	case string(mail.ForwardAttachmentsAll):
		mode = mail.ForwardAttachmentsAll
	default:
		return mail.ForwardAttachmentSelection{}, invalid("include_attachments", "--include-attachments must be all or none")
	}
	cleanNames, err := cleanUniqueStrings("include_attachment", names)
	if err != nil {
		return mail.ForwardAttachmentSelection{}, err
	}
	cleanIDs, err := cleanUniqueStrings("include_attachment_id", ids)
	if err != nil {
		return mail.ForwardAttachmentSelection{}, err
	}
	if len(cleanNames)+len(cleanIDs) > 0 {
		if mode == mail.ForwardAttachmentsAll && strings.TrimSpace(modeRaw) != "" {
			return mail.ForwardAttachmentSelection{}, invalid("include_attachments", "--include-attachments all cannot be used with --include-attachment or --include-attachment-id")
		}
		// Naming attachments means "only these". The provider checks that they
		// exist and can be forwarded, because each provider copies them its own
		// way.
		mode = mail.ForwardAttachmentsSelected
	}
	return mail.ForwardAttachmentSelection{Mode: mode, Names: cleanNames, IDs: cleanIDs}, nil
}

func cleanUniqueStrings(field string, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		clean := strings.TrimSpace(value)
		if clean == "" {
			return nil, invalid(field, "an attachment name or id is empty")
		}
		if _, ok := seen[clean]; ok {
			return nil, invalid(field, "the same attachment is named twice")
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out, nil
}
