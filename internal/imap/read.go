package imap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
)

// Reading one message.

func (c *accountClient) ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	var out mail.Message
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		msg, err := c.resolveMessage(ctx, session, req.Selector, errs.ProviderOperationIMAPMessagesRead, !req.MetadataOnly)
		if err != nil {
			return err
		}
		if req.MetadataOnly {
			meta, err := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataMessage, mail.MessageProviderMetadata{Source: mail.MetadataSourceIMAP})
			if err != nil {
				return err
			}
			summary := c.toSummary(msg)
			out = mail.Message{
				Account:           c.account,
				Provider:          c.provider,
				ID:                msg.id,
				InternetMessageID: msg.internetMessageID,
				Folder:            msg.folder,
				Subject:           envelopeSubject(msg.envelope),
				ReceivedAt:        summary.ReceivedAt,
				SentAt:            summary.SentAt,
				IsRead:            summary.IsRead,
				HasAttachments:    summary.HasAttachments,
				ProviderMeta:      meta,
			}
			if from := envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.From }); len(from) > 0 {
				out.From = from[0]
			}
			return nil
		}
		mode := req.BodyMode
		if mode == "" {
			mode = mail.BodyModeReadable
		}
		body, warnings, err := c.fetchMessageBody(ctx, session, msg, mode)
		if err != nil {
			return err
		}
		out = c.toMessage(msg, body)
		out.Warnings = append(out.Warnings, warnings...)
		return nil
	})
	return out, err
}

// resolveMessage finds the message a selector names and fetches its
// envelope, flags, structure and headers. The message's mailbox is selected
// afterwards, so the caller can fetch its body by UID.
func (c *accountClient) resolveMessage(ctx context.Context, session *imapSession, selector mail.ResolvedMessageSelector, op errs.ProviderOperation, full bool) (imapFetchedMessage, error) {
	switch selector.Kind {
	case mail.ResolvedSelectorID:
		identity, err := parseMessageID(selector.ID)
		if err != nil {
			return imapFetchedMessage{}, invalidIMAPMessageSelector(c, "id", selector.ID)
		}
		return c.fetchByIdentity(ctx, session, identity, selector.ID, op, full)
	case mail.ResolvedSelectorInternetID:
		normalized, err := mail.NormalizeMessageIDScalar(selector.InternetID)
		if err != nil {
			return imapFetchedMessage{}, invalidIMAPMessageSelector(c, "internet_id", selector.InternetID)
		}
		matches, err := c.findByInternetID(ctx, session, normalized, op, full)
		if err != nil {
			return imapFetchedMessage{}, err
		}
		if len(matches) == 0 {
			return imapFetchedMessage{}, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "internet_id", normalized, errs.IMAPMessageNotFoundMissing)
		}
		// A change must never pick one of several copies by guesswork. A read
		// shows the copy in the most useful folder: the inbox before archives,
		// anything before Trash and Junk.
		if !full && len(matches) > 1 {
			return imapFetchedMessage{}, errs.NewIMAPAmbiguousInternetMessageID(c.account, imapErrProvider(c.provider), normalized, uint64(len(matches)))
		}
		chosen := matches[0]
		// Leave the chosen message's mailbox selected for the body fetch.
		if _, err := c.examine(ctx, session, chosen.mailbox, op); err != nil {
			return imapFetchedMessage{}, err
		}
		return chosen, nil
	case mail.ResolvedSelectorThreadID:
		return imapFetchedMessage{}, invalidIMAPMessageSelector(c, "thread_id", selector.ThreadID)
	default:
		return imapFetchedMessage{}, invalidIMAPMessageSelector(c, string(selector.Kind), "")
	}
}

// fetchByIdentity selects the id's mailbox, checks its UIDVALIDITY and
// fetches the message by UID.
func (c *accountClient) fetchByIdentity(ctx context.Context, session *imapSession, identity messageIdentity, rawID string, op errs.ProviderOperation, full bool) (imapFetchedMessage, error) {
	status, err := c.examineMailboxForMessage(ctx, session, identity.Mailbox, "id", rawID, op)
	if err != nil {
		return imapFetchedMessage{}, err
	}
	if status.uidValidity != identity.UIDValidity {
		return imapFetchedMessage{}, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", rawID, errs.IMAPMessageNotFoundUIDValidityChanged)
	}
	folder := c.recordForMailbox(identity.Mailbox, status)
	msgs, _, err := c.fetchMessageDetails(ctx, session, folder, []uint32{identity.UID}, full, op)
	if err != nil {
		return imapFetchedMessage{}, err
	}
	if len(msgs) == 0 {
		return imapFetchedMessage{}, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", rawID, errs.IMAPMessageNotFoundMissing)
	}
	return msgs[0], nil
}

// recordForMailbox builds a folder record for a mailbox known only by name,
// as when a message id names it.
func (c *accountClient) recordForMailbox(mailbox string, status imapMailboxStatus) imapFolderRecord {
	folderID, _ := formatFolderID(mailbox)
	display := mailbox
	if asciiEqualFold(mailbox, "INBOX") {
		display = "INBOX"
	}
	return imapFolderRecord{
		folder:      mail.Folder{Account: c.account, Provider: c.provider, ID: folderID, DisplayName: display, Path: display},
		mailbox:     mailbox,
		selectable:  true,
		uidValidity: status.uidValidity,
		uidNext:     status.uidNext,
	}
}

// findByInternetID looks for a Message-ID in every folder. IMAP's HEADER
// search matches substrings, so each hit is checked against the exact id.
// Results come in preference order: Inbox, other folders, Sent and Drafts,
// then Trash and Junk.
func (c *accountClient) findByInternetID(ctx context.Context, session *imapSession, internetID string, op errs.ProviderOperation, full bool) ([]imapFetchedMessage, error) {
	snapshot, err := c.folderSnapshot(ctx, session, false)
	if err != nil {
		return nil, err
	}
	var matches []imapFetchedMessage
	for _, folder := range foldersByPreference(snapshot) {
		status, refused, err := c.examineForSearch(ctx, session, folder.mailbox, op)
		if err != nil {
			return nil, err
		}
		if refused || status.total == 0 {
			continue
		}
		folder.uidValidity = status.uidValidity
		folder.uidNext = status.uidNext
		uids, err := c.uidSearch(ctx, session, goimap.SearchCriteria{
			Header: []goimap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: internetID}},
		}, op)
		if err != nil {
			return nil, err
		}
		if len(uids) == 0 {
			continue
		}
		msgs, _, err := c.fetchMessageDetails(ctx, session, folder, uids, full, op)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if msg.internetMessageID == internetID {
				matches = append(matches, msg)
			}
		}
	}
	return matches, nil
}

// foldersByPreference orders folders for lookups by Message-ID.
func foldersByPreference(snapshot imapFolderSnapshot) []imapFolderRecord {
	rank := func(r imapFolderRecord) int {
		switch {
		case asciiEqualFold(r.mailbox, "INBOX"):
			return 0
		case r.hasRole(mail.SpecialUseTrash), r.hasRole(mail.SpecialUseJunk):
			return 4
		case r.hasRole(mail.SpecialUseAll):
			return 3
		case r.hasRole(mail.SpecialUseSent), r.hasRole(mail.SpecialUseDrafts):
			return 2
		default:
			return 1
		}
	}
	out := append([]imapFolderRecord(nil), snapshot.selectable...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && rank(out[j]) < rank(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// fetchMessageDetails fetches messages in the selected folder. full adds the
// complete header block, which reading and replying need.
func (c *accountClient) fetchMessageDetails(ctx context.Context, session *imapSession, folder imapFolderRecord, uids []uint32, full bool, op errs.ProviderOperation) ([]imapFetchedMessage, []string, error) {
	if !full {
		return c.fetchMessages(ctx, session, folder, uids, false, op)
	}
	header := &goimap.FetchItemBodySection{Specifier: goimap.PartSpecifierHeader, Peek: true}
	options := &goimap.FetchOptions{
		UID:           true,
		Flags:         true,
		InternalDate:  true,
		RFC822Size:    true,
		Envelope:      true,
		BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*goimap.FetchItemBodySection{header},
	}
	var bufs []*imapclient.FetchMessageBuffer
	err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
		var err error
		bufs, err = dep.Fetch(uidSetOf(uids), options).Collect()
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var out []imapFetchedMessage
	var skipped []string
	for _, buf := range bufs {
		if buf == nil || buf.UID == 0 {
			continue
		}
		msg, err := c.fetchedMessage(folder, uint32(buf.UID), buf, buf.FindBodySection(header))
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("left out message %d in %s: %v", buf.UID, folderLabel(folder), err))
			continue
		}
		out = append(out, msg)
	}
	return out, skipped, nil
}

func invalidIMAPMessageSelector(c *accountClient, kind, value string) error {
	message := "IMAP accounts cannot pick a message this way"
	if kind == "id" {
		message = "this is neither an IMAP message id nor a short id from maildo's tables"
	}
	return errs.NewWithDetails(errs.CodeInvalidSelector, message, map[string]any{
		"account":        c.account,
		"provider":       c.provider,
		"selector_kind":  kind,
		"selector_value": value,
	})
}

// fetchMessageBody fetches and decodes the body for a mode. The message's
// mailbox must be selected. Broken encodings decode as far as they can and
// add a warning.
func (c *accountClient) fetchMessageBody(ctx context.Context, session *imapSession, msg imapFetchedMessage, mode mail.BodyMode) (mail.MessageBody, []string, error) {
	switch mode {
	case mail.BodyModeReadable, mail.BodyModeText, mail.BodyModeHTML:
		part, err := preferredBodyPart(msg.bodyStructure, mode)
		if err != nil {
			return mail.MessageBody{Mode: mode}, []string{"the message structure is too complex to show its text"}, nil
		}
		if part == nil {
			if mode == mail.BodyModeHTML {
				return mail.MessageBody{}, nil, unsupportedIMAPBodyMode(c, mode)
			}
			return mail.MessageBody{Mode: mode}, nil, nil
		}
		raw, truncated, err := c.fetchSection(ctx, session, msg.uid, part.path, part.encodedSize, mail.MaxMessageBodyBytes, errs.ProviderOperationIMAPMessagesRead)
		var refused sectionRefusedError
		if errors.As(err, &refused) {
			return mail.MessageBody{ContentType: part.mediaType, Mode: mode}, []string{"the server could not send the message text: " + shortText(refused.text)}, nil
		}
		if err != nil {
			return mail.MessageBody{}, nil, err
		}
		content, degraded := decodeTextPart(raw, part)
		var warnings []string
		if degraded {
			warnings = append(warnings, "parts of the body could not be decoded; maildo shows what it could read")
		}
		if truncated {
			warnings = append(warnings, "the body is longer than 1 MB and was cut short; use --body-mode raw for the whole message")
		}
		if part.mediaType == "text/html" && mode != mail.BodyModeHTML {
			content = htmltext.ToReadable(content)
		}
		return mail.MessageBody{ContentType: part.mediaType, Mode: mode, Content: content}, warnings, nil
	case mail.BodyModeRaw:
		raw, truncated, err := c.fetchSection(ctx, session, msg.uid, nil, uint64(max(msg.rfc822Size, 0)), mail.MaxMessageAttachmentDownloadBytes, errs.ProviderOperationIMAPMessagesRead)
		if err != nil {
			return mail.MessageBody{}, nil, err
		}
		var warnings []string
		if truncated {
			warnings = append(warnings, "the message is larger than 25 MB and was cut short")
		}
		body := mail.MessageBody{ContentType: "message/rfc822", Mode: mail.BodyModeRaw, Content: string(raw)}
		if !utf8.Valid(raw) {
			body.Encoding = "base64"
			body.Content = base64.StdEncoding.EncodeToString(raw)
		}
		return body, warnings, nil
	default:
		return mail.MessageBody{}, nil, unsupportedIMAPBodyMode(c, mode)
	}
}

func unsupportedIMAPBodyMode(c *accountClient, mode mail.BodyMode) error {
	return errs.NewWithDetails(errs.CodeUnsupportedBodyMode, fmt.Sprintf("this message has no %s body", mode), map[string]any{
		"account":   c.account,
		"provider":  c.provider,
		"body_mode": string(mode),
	})
}

// fetchSection fetches one body part (or the whole message for a nil path)
// from the selected mailbox, at most limit bytes of it. size is the size the
// server reported for the part; a part known to fit is fetched whole, since
// some servers format partial answers wrongly.
func (c *accountClient) fetchSection(ctx context.Context, session *imapSession, uid uint32, path []uint32, size uint64, limit int64, op errs.ProviderOperation) ([]byte, bool, error) {
	// Base64 makes text a third larger, so allow for that before decoding.
	fetchLimit := limit*4/3 + 4096
	section := &goimap.FetchItemBodySection{Part: uintPathToInt(path), Peek: true}
	if size == 0 || size > uint64(fetchLimit) {
		section.Partial = &goimap.SectionPartial{Offset: 0, Size: fetchLimit + 1}
	}
	var bufs []*imapclient.FetchMessageBuffer
	refusal := ""
	err := session.withAttachmentRead(ctx, op, func(dep *imapclient.Client) error {
		var err error
		bufs, err = dep.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
		var imapErr *goimap.Error
		if errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeNo {
			refusal = strings.TrimSpace(imapErr.Text)
			return nil
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if refusal != "" {
		return nil, false, sectionRefusedError{text: refusal}
	}
	for _, buf := range bufs {
		if buf != nil && uint32(buf.UID) == uid {
			raw := buf.FindBodySection(section)
			if int64(len(raw)) > fetchLimit {
				return raw[:fetchLimit], true, nil
			}
			return raw, false, nil
		}
	}
	return nil, false, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "uid", fmt.Sprint(uid), errs.IMAPMessageNotFoundMissing)
}

// sectionRefusedError means the server said NO to fetching a body part.
// Some servers decode parts themselves and give up on broken encodings.
type sectionRefusedError struct{ text string }

func (e sectionRefusedError) Error() string { return "the server could not send this part: " + e.text }

func (c *accountClient) toMessage(msg imapFetchedMessage, body mail.MessageBody) mail.Message {
	attachments := c.attachmentsFromStructure(msg.bodyStructure)
	flagged := hasFlag(msg.flags, goimap.FlagFlagged)
	meta, _ := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataMessage, mail.MessageProviderMetadata{
		Source:  mail.MetadataSourceIMAP,
		Flagged: &flagged,
	})
	message := mail.Message{
		Account:           c.account,
		Provider:          c.provider,
		ID:                msg.id,
		InternetMessageID: msg.internetMessageID,
		ThreadID:          msg.threadID,
		Folder:            msg.folder,
		Subject:           envelopeSubject(msg.envelope),
		Headers:           msg.header.visible,
		IsRead:            hasFlag(msg.flags, goimap.FlagSeen),
		HasAttachments:    len(attachments) > 0,
		Attachments:       attachments,
		Body:              body,
		ProviderMeta:      meta,
	}
	if msg.internalDate != nil {
		t := *msg.internalDate
		message.ReceivedAt = &t
	}
	if msg.envelope != nil && !msg.envelope.Date.IsZero() {
		t := msg.envelope.Date
		message.SentAt = &t
	}
	from := envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.From })
	if len(from) > 0 {
		message.From = from[0]
	}
	// The envelope repeats From as Sender and Reply-To when the message has
	// neither, so maildo shows them only when they differ from From.
	if sender := envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.Sender }); len(sender) > 0 && !sameRecipients(sender, from) {
		message.Sender = &sender[0]
	}
	if replyTo := envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.ReplyTo }); !sameRecipients(replyTo, from) {
		message.ReplyTo = replyTo
	}
	message.To = envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.To })
	message.CC = envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.Cc })
	message.BCC = envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.Bcc })
	return message
}

func sameRecipients(a, b []mail.Recipient) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Address, b[i].Address) {
			return false
		}
	}
	return true
}
