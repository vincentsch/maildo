package provider

import (
	"crypto/sha256"
	"strings"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
)

// The functions here build replies, forwards and draft changes as MIME
// messages. Gmail and IMAP both store drafts as MIME, so they share them.

// ReplyMessage builds a reply: to the Reply-To addresses or the sender,
// for a reply to all also to the other recipients, never to yourself. A
// reply to a message you wrote goes to its recipients, as in Gmail.
func ReplyMessage(from mail.Recipient, req ReplyDraftRequest, source ComposeSourceMaterial) mimecompose.Message {
	text, htmlBody := MIMEBodies(req.Body)
	return mimecompose.Message{
		From:       from,
		To:         replyRecipients(source, from.Address),
		CC:         replyAllCC(req.Mode, source, from.Address, replyRecipients(source, from.Address)),
		Subject:    prefixedSubject("Re:", source.Subject),
		TextBody:   []byte(text),
		HTMLBody:   []byte(htmlBody),
		InReplyTo:  source.InternetMessageID,
		References: replyReferences(source),
	}
}

// ForwardMessage builds a forward to the given recipients.
func ForwardMessage(from mail.Recipient, req ForwardDraftRequest, source ComposeSourceMaterial) mimecompose.Message {
	text, htmlBody := MIMEBodies(req.Body)
	return mimecompose.Message{
		From:     from,
		To:       req.To,
		CC:       req.CC,
		BCC:      req.BCC,
		Subject:  prefixedSubject("Fwd:", source.Subject),
		TextBody: []byte(text),
		HTMLBody: []byte(htmlBody),
	}
}

// MIMEPatch turns a draft change into a MIME patch.
func MIMEPatch(p mail.DraftPatch) mimecompose.Patch {
	out := mimecompose.Patch{
		To:      mimecompose.AddressPatch{Set: p.To.Set || p.To.Clear, Values: p.To.Values},
		CC:      mimecompose.AddressPatch{Set: p.CC.Set || p.CC.Clear, Values: p.CC.Values},
		BCC:     mimecompose.AddressPatch{Set: p.BCC.Set || p.BCC.Clear, Values: p.BCC.Values},
		ReplyTo: mimecompose.AddressPatch{Set: p.ReplyTo.Set || p.ReplyTo.Clear, Values: p.ReplyTo.Values},
		Subject: mimecompose.StringPatch{Set: p.Subject.Set, Value: p.Subject.Value},
	}
	if p.Body.Set {
		text, htmlBody := MIMEBodies(p.Body.Body)
		out.TextBody = mimecompose.BytesPatch{Set: true, Value: []byte(text)}
		out.HTMLBody = mimecompose.BytesPatch{Set: true, Value: []byte(htmlBody)}
	}
	return out
}

// MIMEBodies returns the text and HTML parts for a body.
func MIMEBodies(body mail.OutboundBody) (string, string) {
	switch body.Format {
	case mail.BodyFormatMarkdown, mail.BodyFormatHTML:
		return body.Text, body.HTML
	default:
		return body.Text, ""
	}
}

func replyRecipients(source ComposeSourceMaterial, self string) []mail.Recipient {
	filter := func(in []mail.Recipient) []mail.Recipient {
		out := make([]mail.Recipient, 0, len(in))
		seen := map[string]bool{}
		for _, r := range in {
			addr := strings.ToLower(strings.TrimSpace(r.Address))
			if addr == "" || strings.EqualFold(addr, self) || seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, r)
		}
		return out
	}
	if self != "" && strings.EqualFold(strings.TrimSpace(source.From.Address), self) {
		// Your own message: reply to the people you wrote to. A note to
		// yourself gets a reply to yourself.
		if to := filter(source.To); len(to) > 0 {
			return to
		}
		return []mail.Recipient{source.From}
	}
	if len(source.ReplyTo) > 0 {
		return filter(source.ReplyTo)
	}
	if strings.TrimSpace(source.From.Address) != "" || strings.TrimSpace(source.From.Name) != "" {
		return filter([]mail.Recipient{source.From})
	}
	return nil
}

func replyAllCC(mode mail.ReplyMode, source ComposeSourceMaterial, self string, primary []mail.Recipient) []mail.Recipient {
	if mode != mail.ReplyModeReplyAll {
		return nil
	}
	out := make([]mail.Recipient, 0, len(source.To)+len(source.CC))
	seen := recipientSet(primary)
	add := func(r mail.Recipient) {
		addr := strings.ToLower(strings.TrimSpace(r.Address))
		if addr == "" || strings.EqualFold(addr, self) || seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, r)
	}
	for _, r := range source.To {
		add(r)
	}
	for _, r := range source.CC {
		add(r)
	}
	return out
}

func recipientSet(in []mail.Recipient) map[string]bool {
	out := map[string]bool{}
	for _, r := range in {
		addr := strings.ToLower(strings.TrimSpace(r.Address))
		if addr != "" {
			out[addr] = true
		}
	}
	return out
}

func replyReferences(source ComposeSourceMaterial) []string {
	out := append([]string(nil), source.References...)
	if len(out) == 0 {
		out = append([]string(nil), source.InReplyTo...)
	}
	if source.InternetMessageID != "" {
		out = appendUniqueMessageID(out, source.InternetMessageID)
	}
	return out
}

func appendUniqueMessageID(in []string, id string) []string {
	normalized, err := mail.NormalizeMessageIDScalar(id)
	if err != nil {
		return in
	}
	for _, existing := range in {
		if existing == normalized {
			return in
		}
	}
	return append(in, normalized)
}

func prefixedSubject(prefix, subject string) string {
	trimmed := strings.TrimSpace(subject)
	if trimmed == "" {
		return prefix
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, strings.ToLower(prefix)) {
		return trimmed
	}
	return prefix + " " + trimmed
}

// SendContentOf describes a MIME message the way a person approves it. Inline
// parts count as attachments, so nothing that goes out is left out.
func SendContentOf(msg mimecompose.Message) mail.SendContent {
	out := mail.SendContent{
		From:    msg.From,
		To:      cloneRecipients(msg.To),
		CC:      cloneRecipients(msg.CC),
		BCC:     cloneRecipients(msg.BCC),
		ReplyTo: cloneRecipients(msg.ReplyTo),
		Subject: msg.Subject,
		Text:    string(msg.TextBody),
		HTML:    string(msg.HTMLBody),
	}
	for _, part := range msg.Related {
		out.Attachments = append(out.Attachments, sendAttachment(part.Filename, part.ContentType, uint64(len(part.Bytes)), true, sha256.Sum256(part.Bytes)))
	}
	for _, part := range msg.Attachments {
		out.Attachments = append(out.Attachments, sendAttachment(part.Filename, part.ContentType, uint64(len(part.Bytes)), part.Disposition == "inline", sha256.Sum256(part.Bytes)))
	}
	return out
}

// SendContentOfProjection describes a parsed MIME message like
// SendContentOf does.
func SendContentOfProjection(p mimecompose.SemanticProjection) mail.SendContent {
	out := mail.SendContent{
		From:    p.From,
		To:      cloneRecipients(p.To),
		CC:      cloneRecipients(p.CC),
		BCC:     cloneRecipients(p.BCC),
		ReplyTo: cloneRecipients(p.ReplyTo),
		Subject: p.Subject,
		Text:    string(p.TextBody),
		HTML:    string(p.HTMLBody),
	}
	for _, part := range p.Related {
		out.Attachments = append(out.Attachments, sendAttachment(part.Filename, part.ContentType, part.DecodedSize, true, part.SHA256))
	}
	for _, part := range p.Attachments {
		out.Attachments = append(out.Attachments, sendAttachment(part.Filename, part.ContentType, part.DecodedSize, part.Disposition == "inline", part.SHA256))
	}
	return out
}

// SendContentOfOutbound describes a new message before a provider prepares
// it. from is the sender maildo writes for the account.
func SendContentOfOutbound(from mail.Recipient, msg mail.OutboundMessage) mail.SendContent {
	text, htmlBody := MIMEBodies(msg.Body)
	out := mail.SendContent{
		From:    from,
		To:      cloneRecipients(msg.To),
		CC:      cloneRecipients(msg.CC),
		BCC:     cloneRecipients(msg.BCC),
		ReplyTo: cloneRecipients(msg.ReplyTo),
		Subject: msg.Subject,
		Text:    text,
		HTML:    htmlBody,
	}
	for _, a := range msg.Attachments {
		out.Attachments = append(out.Attachments, mail.SendAttachment{Name: a.Name, ContentType: a.ContentType, Size: int64(len(a.Bytes)), SHA256: sha256.Sum256(a.Bytes)})
	}
	return out
}

func sendAttachment(name, contentType string, size uint64, inline bool, sum [32]byte) mail.SendAttachment {
	return mail.SendAttachment{Name: name, ContentType: contentType, Size: int64(size), Inline: inline, SHA256: sum}
}

func cloneRecipients(in []mail.Recipient) []mail.Recipient {
	if len(in) == 0 {
		return nil
	}
	return append([]mail.Recipient(nil), in...)
}
