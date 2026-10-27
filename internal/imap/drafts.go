package imap

// Drafts, replies and forwards
//
// IMAP has no draft API. A draft is a message with the \Draft flag in the
// Drafts folder. maildo writes the MIME message itself and stores it with
// APPEND. To change a draft it stores the new version and deletes the old
// one, so a draft gets a new id every time it changes.
//
// Sending goes over SMTP. Afterwards maildo saves a copy in the Sent folder,
// because IMAP servers expect the mail program to do that. Gmail and
// Microsoft are the exception: their SMTP servers save the copy themselves.
//
// Replies and forwards are built like Gmail builds them (see
// provider.ReplyMessage), then saved as a draft or sent.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
	"github.com/vincentsch/maildo/internal/provider"
)

var (
	_ provider.DraftCreator                = (*accountClient)(nil)
	_ provider.DraftReader                 = (*accountClient)(nil)
	_ provider.DraftUpdater                = (*accountClient)(nil)
	_ provider.DraftSender                 = (*accountClient)(nil)
	_ provider.ReplyDraftCreator           = (*accountClient)(nil)
	_ provider.ForwardDraftCreator         = (*accountClient)(nil)
	_ provider.ForwardDraftPreviewer       = (*accountClient)(nil)
	_ provider.DraftAttachmentManager      = (*accountClient)(nil)
	_ provider.ComposeSourceMaterialReader = (*accountClient)(nil)
)

var draftFlags = []goimap.Flag{goimap.FlagDraft, goimap.FlagSeen}

// from is the From of mail this account writes: its address, with the name
// from --from or the account's sender name.
func (c *accountClient) from(requested *mail.Recipient) mail.Recipient {
	out := mail.Recipient{Name: c.senderName, Address: c.setup.Email}
	if requested != nil && strings.TrimSpace(requested.Name) != "" {
		out.Name = strings.TrimSpace(requested.Name)
	}
	return out
}

// encodeDraft encodes a message to save in Drafts. Bcc stays in the
// header, so the draft still knows its blind copies when it is sent.
func (c *accountClient) encodeDraft(msg mimecompose.Message) ([]byte, error) {
	doc, err := mimecompose.NewDocument(msg, mimecompose.DefaultLimits())
	if err != nil {
		return nil, smtpMIMEError(c.account, c.provider, err)
	}
	defer doc.Destroy()
	return c.encodeDocument(doc, mimecompose.BccHeaderPolicyRetain)
}

func (c *accountClient) encodeDocument(doc *mimecompose.Document, bcc mimecompose.BccHeaderPolicy) ([]byte, error) {
	msg, err := doc.Message()
	if err != nil {
		return nil, smtpMIMEError(c.account, c.provider, err)
	}
	mode := mimecompose.AddressHeaderASCII
	if smtpNeedsUTF8(msg.From.Address, msg.To, msg.CC, msg.BCC, msg.ReplyTo) {
		mode = mimecompose.AddressHeaderSMTPUTF8
	}
	mimecompose.DestroyMessage(&msg)
	encoded, err := doc.Encode(mimecompose.EncodeOptions{BccPolicy: bcc, AddressMode: mode})
	if err != nil {
		return nil, smtpMIMEError(c.account, c.provider, err)
	}
	defer encoded.Destroy()
	return append([]byte(nil), encoded.Bytes...), nil
}

// roleFolder finds the folder for a role such as Drafts or Sent.
func (c *accountClient) roleFolder(ctx context.Context, session *imapSession, role mail.ProviderSpecialUse, capability provider.Capability) (imapFolderRecord, error) {
	snapshot, err := c.folderSnapshot(ctx, session, false)
	if err != nil {
		return imapFolderRecord{}, err
	}
	for _, record := range snapshot.selectable {
		if record.hasRole(role) {
			return record, nil
		}
	}
	return imapFolderRecord{}, c.unsupported(capability, "special_use_role_missing", map[string]any{"role": roleName(role)})
}

// storeMessage saves raw in the folder with the role and returns the new
// message's id. With an attempt, the APPEND is the write that the attempt
// tracks.
func (c *accountClient) storeMessage(ctx context.Context, session *imapSession, role mail.ProviderSpecialUse, capability provider.Capability, raw []byte, flags []goimap.Flag, attempt errs.ProviderWriteAttempt, op errs.ProviderWriteOperation) (string, imapFolderRecord, error) {
	folder, err := c.roleFolder(ctx, session, role, capability)
	if err != nil {
		return "", folder, err
	}
	if attempt != nil {
		if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
			return "", folder, err
		}
	}
	var data *goimap.AppendData
	err = session.withCommand(ctx, func(dep *imapclient.Client) error {
		cmd := dep.Append(folder.mailbox, int64(len(raw)), &goimap.AppendOptions{Flags: flags, Time: time.Now()})
		if _, err := cmd.Write(raw); err != nil {
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		var err error
		data, err = cmd.Wait()
		return err
	})
	// The folder has a new message, so a remembered EXAMINE is out of date.
	session.forgetSelected()
	if err != nil {
		if attempt == nil {
			return "", folder, err
		}
		var imapErr *goimap.Error
		if errors.As(err, &imapErr) {
			return "", folder, c.reject(attempt, op, errs.NewWithDetails(errs.CodeProviderForbidden, "the IMAP server refused to store the message", map[string]any{
				"account":        c.account,
				"provider":       c.provider,
				"folder":         folderLabel(folder),
				"server_message": shortText(imapErr.Text),
			}))
		}
		return "", folder, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	if data != nil && data.UID != 0 && data.UIDValidity != 0 {
		id, err := formatMessageID(folder.mailbox, data.UIDValidity, uint32(data.UID))
		return id, folder, err
	}
	// Without UIDPLUS the server does not say where the message went; find
	// it by its Message-ID.
	id, err := c.findStored(ctx, session, folder, headerValue(raw, "Message-ID"))
	return id, folder, err
}

func (c *accountClient) findStored(ctx context.Context, session *imapSession, folder imapFolderRecord, messageID string) (string, error) {
	if messageID == "" {
		return "", nil
	}
	op := errs.ProviderOperationIMAPMessagesRead
	status, err := c.examine(ctx, session, folder.mailbox, op)
	if err != nil {
		return "", err
	}
	uids, err := c.uidSearch(ctx, session, goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: messageID}}}, op)
	if err != nil || len(uids) == 0 {
		return "", err
	}
	highest := uids[0]
	for _, uid := range uids {
		highest = max(highest, uid)
	}
	return formatMessageID(folder.mailbox, status.uidValidity, highest)
}

// headerValue reads one header from a raw message.
func headerValue(raw []byte, name string) string {
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if err != nil && len(header) == 0 {
		return ""
	}
	return strings.TrimSpace(header.Get(name))
}

// deleteMessage removes a message for good: \Deleted, then UID EXPUNGE.
// Servers without UIDPLUS cannot expunge one message alone, so there the
// message only gets the \Deleted flag and the result says so.
func (c *accountClient) deleteMessage(ctx context.Context, session *imapSession, id string) (string, error) {
	identity, err := parseMessageID(id)
	if err != nil {
		return "", invalidIMAPMessageSelector(c, "id", id)
	}
	status, err := c.selectMailboxReadWrite(ctx, session, identity.Mailbox, false, provider.CapDraftUpdate, "draft_delete")
	if err != nil {
		return "", err
	}
	defer session.forgetSelected()
	if status.uidValidity != identity.UIDValidity {
		return "the old draft was not deleted, because the server renumbered the folder it was in", nil
	}
	state, err := session.activeState(ctx)
	if err != nil {
		return "", err
	}
	uidPlus := state.revision == imapActiveRev2 || advertisedCapability(state.capabilities, goimap.CapUIDPlus)
	set := goimap.UIDSetNum(goimap.UID(identity.UID))
	err = session.withCommand(ctx, func(dep *imapclient.Client) error {
		if err := dep.Store(set, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Close(); err != nil {
			return err
		}
		if uidPlus {
			return dep.UIDExpunge(set).Close()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !uidPlus {
		return "the old draft is only marked as deleted, because the server lacks UIDPLUS; another mail program has to remove it for good (expunge)", nil
	}
	return "", nil
}

// draftMessage reads a draft by id: a message in the Drafts folder, or one
// with the \Draft flag.
func (c *accountClient) draftMessage(ctx context.Context, session *imapSession, id string, full bool) (imapFetchedMessage, error) {
	msg, err := c.resolveMessage(ctx, session, mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: id}, errs.ProviderOperationIMAPMessagesRead, full)
	if err != nil {
		if errorCode(err) == errs.CodeMessageNotFound || errorCode(err) == errs.CodeInvalidSelector {
			return imapFetchedMessage{}, c.draftNotFound(id)
		}
		return imapFetchedMessage{}, err
	}
	if !hasFlag(msg.flags, goimap.FlagDraft) && msg.folder.WellKnownName != mail.WellKnownDrafts {
		return imapFetchedMessage{}, c.draftNotFound(id)
	}
	return msg, nil
}

func (c *accountClient) draftNotFound(id string) error {
	return errs.NewWithDetails(errs.CodeDraftNotFound, "draft not found", map[string]any{
		"account":  c.account,
		"provider": c.provider,
		"draft_id": id,
	})
}

// rawMessage fetches a whole message from the selected mailbox.
func (c *accountClient) rawMessage(ctx context.Context, session *imapSession, msg imapFetchedMessage) ([]byte, error) {
	raw, truncated, err := c.fetchSection(ctx, session, msg.uid, nil, uint64(max(msg.rfc822Size, 0)), mail.MaxOutboundEncodedMessageBytes, errs.ProviderOperationIMAPMessagesRead)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errs.NewWithDetails(errs.CodeMessageTooLarge, "the draft is too large to send", map[string]any{"account": c.account, "provider": c.provider})
	}
	return raw, nil
}

// draftDocument reads a draft and parses its MIME.
func (c *accountClient) draftDocument(ctx context.Context, session *imapSession, id string) (*mimecompose.Document, imapFetchedMessage, error) {
	msg, err := c.draftMessage(ctx, session, id, false)
	if err != nil {
		return nil, msg, err
	}
	raw, err := c.rawMessage(ctx, session, msg)
	if err != nil {
		return nil, msg, err
	}
	doc, err := mimecompose.ParseDocument(raw, mimecompose.DefaultLimits())
	if err != nil {
		return nil, msg, smtpMIMEError(c.account, c.provider, err)
	}
	return doc, msg, nil
}

func (c *accountClient) draftMeta(use mail.ProviderMetadataUse) *mail.DraftProviderMetadata {
	meta, _ := mail.NewProviderMetadata(imapErrProvider(c.provider), use, mail.DraftProviderMetadata{Source: mail.MetadataSourceIMAP})
	return meta
}

// saveDraft stores a new draft and returns its id.
func (c *accountClient) saveDraft(ctx context.Context, msg mimecompose.Message, op errs.ProviderWriteOperation) (string, *mail.WriteOutcomeFacts, error) {
	raw, err := c.encodeDraft(msg)
	if err != nil {
		return "", nil, err
	}
	attempt, err := c.newAttempt(op)
	if err != nil {
		return "", nil, err
	}
	var id string
	err = c.withIMAPSession(ctx, func(session *imapSession) error {
		var err error
		id, _, err = c.storeMessage(ctx, session, mail.SpecialUseDrafts, provider.CapDraftCreate, raw, draftFlags, attempt, op)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return "", nil, err
	}
	return id, mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), nil
}

func (c *accountClient) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	msg := smtpMIMEMessage(c.from(req.Message.From), req.Message)
	defer mimecompose.DestroyMessage(&msg)
	id, facts, err := c.saveDraft(ctx, msg, errs.WriteIMAPDraftCreate)
	if err != nil {
		return mail.DraftRef{}, err
	}
	return mail.DraftRef{ID: id, WriteOutcomeFacts: facts, ProviderMeta: c.draftMeta(mail.MetadataDraftWrite)}, nil
}

func (c *accountClient) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	var out mail.Draft
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		msg, err := c.draftMessage(ctx, session, req.DraftID, true)
		if err != nil {
			return err
		}
		var body mail.MessageBody
		if !req.MetadataOnly {
			mode := req.BodyMode
			if mode == "" {
				mode = mail.BodyModeReadable
			}
			if body, _, err = c.fetchMessageBody(ctx, session, msg, mode); err != nil {
				return err
			}
		}
		m := c.toMessage(msg, body)
		out = mail.Draft{
			Account:           c.account,
			Provider:          c.provider,
			ID:                m.ID,
			IsDraft:           true,
			InternetMessageID: m.InternetMessageID,
			Folder:            m.Folder,
			From:              m.From,
			ReplyTo:           m.ReplyTo,
			To:                m.To,
			CC:                m.CC,
			BCC:               m.BCC,
			Subject:           m.Subject,
			CreatedAt:         m.ReceivedAt,
			HasAttachments:    m.HasAttachments,
			Attachments:       m.Attachments,
			Body:              body,
			ProviderMeta:      c.draftMeta(mail.MetadataDraft),
		}
		return nil
	})
	return out, err
}

func (c *accountClient) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		doc, _, err := c.draftDocument(ctx, session, req.DraftID)
		if err != nil {
			return err
		}
		defer doc.Destroy()
		if err := doc.Apply(provider.MIMEPatch(req.Patch)); err != nil {
			return smtpMIMEError(c.account, c.provider, err)
		}
		_, err = c.encodeDocument(doc, mimecompose.BccHeaderPolicyRetain)
		return err
	})
	return mail.DraftUpdateValidation{}, err
}

// UpdateDraft stores the changed draft and deletes the old one. The draft
// gets a new id.
func (c *accountClient) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.rewriteDraft(ctx, req.DraftID, func(doc *mimecompose.Document) error {
		return doc.Apply(provider.MIMEPatch(req.Patch))
	})
}

// rewriteDraft changes a draft's MIME with change, stores the result and
// deletes the old version.
func (c *accountClient) rewriteDraft(ctx context.Context, id string, change func(*mimecompose.Document) error) (mail.DraftRef, error) {
	attempt, err := c.newAttempt(errs.WriteIMAPDraftUpdate)
	if err != nil {
		return mail.DraftRef{}, err
	}
	var newID, warning string
	err = c.withIMAPSession(ctx, func(session *imapSession) error {
		doc, _, err := c.draftDocument(ctx, session, id)
		if err != nil {
			return err
		}
		defer doc.Destroy()
		if err := change(doc); err != nil {
			return smtpMIMEError(c.account, c.provider, err)
		}
		raw, err := c.encodeDocument(doc, mimecompose.BccHeaderPolicyRetain)
		if err != nil {
			return err
		}
		newID, _, err = c.storeMessage(ctx, session, mail.SpecialUseDrafts, provider.CapDraftUpdate, raw, draftFlags, attempt, errs.WriteIMAPDraftUpdate)
		if err != nil {
			return err
		}
		// The new version is saved; a failure to delete the old one leaves
		// a duplicate, not a lost draft.
		warning, err = c.deleteMessage(ctx, session, id)
		if err != nil {
			warning = "the old version of the draft could not be deleted: " + shortCause(err)
		}
		return nil
	})
	if err != nil {
		return mail.DraftRef{}, err
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.DraftRef{}, err
	}
	ref := mail.DraftRef{ID: newID, WriteOutcomeFacts: mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), ProviderMeta: c.draftMeta(mail.MetadataDraftWrite)}
	if warning != "" {
		// The old version may still be in Drafts, and it can still be sent.
		ref.Warnings = []string{warning}
	}
	return ref, nil
}

// ValidateDraftSend reads the draft for the preview and the approval code,
// as SendDraft would send it.
func (c *accountClient) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	draft, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: req.DraftID, BodyMode: mail.BodyModeText})
	if err != nil {
		return mail.Draft{}, err
	}
	msg, err := c.draftToSend(ctx, req.DraftID)
	if err != nil {
		return mail.Draft{}, err
	}
	defer mimecompose.DestroyMessage(&msg)
	content := provider.SendContentOf(msg)
	draft.Send = &content
	return draft, nil
}

// SendDraft sends a draft's MIME over SMTP, saves a copy in Sent and
// deletes the draft. An IMAP message never changes (an edited draft is a
// new message with a new id), and req.Check sees exactly what goes out.
func (c *accountClient) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	if req.Check == nil {
		return mail.SendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	msg, err := c.draftToSend(ctx, req.DraftID)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	defer mimecompose.DestroyMessage(&msg)
	if err := req.Check(provider.SendContentOf(msg)); err != nil {
		return mail.SendOutcome{}, err
	}
	sent, err := c.sendMIME(ctx, msg)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	out := mail.SendOutcome{MessageID: sent.MessageID, WriteOutcomeFacts: sent.WriteOutcomeFacts, SentCopy: sent.SentCopy, Warnings: sent.Warnings}
	err = c.withIMAPSession(ctx, func(session *imapSession) error {
		warning, err := c.deleteMessage(ctx, session, req.DraftID)
		if warning != "" {
			out.Warnings = append(out.Warnings, warning)
		}
		return err
	})
	if err != nil {
		out.Warnings = append(out.Warnings, "the message was sent, but the draft could not be deleted: "+shortCause(err))
	}
	return out, nil
}

// draftToSend reads a draft the way it goes out: its MIME, with the
// account's own address as From.
func (c *accountClient) draftToSend(ctx context.Context, id string) (mimecompose.Message, error) {
	var msg mimecompose.Message
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		doc, _, err := c.draftDocument(ctx, session, id)
		if err != nil {
			return err
		}
		defer doc.Destroy()
		msg, err = doc.Message()
		return err
	})
	if err != nil {
		return mimecompose.Message{}, err
	}
	if len(msg.To)+len(msg.CC)+len(msg.BCC) == 0 {
		mimecompose.DestroyMessage(&msg)
		return mimecompose.Message{}, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the draft has no recipients", map[string]any{"field": "recipients", "draft_id": id})
	}
	// Mail always goes out from the account's own address.
	msg.From = c.from(&msg.From)
	return msg, nil
}

// sendMIME sends msg over SMTP, then saves a copy in Sent.
func (c *accountClient) sendMIME(ctx context.Context, msg mimecompose.Message) (mail.DirectSendOutcome, error) {
	utf8 := smtpNeedsUTF8(msg.From.Address, msg.To, msg.CC, msg.BCC, msg.ReplyTo)
	encoded, err := encodeSMTPMessage(c.account, c.provider, msg, utf8)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	defer encoded.Destroy()
	envelope := smtpEnvelopeRecipients(mail.OutboundMessage{To: msg.To, CC: msg.CC, BCC: msg.BCC})
	attempt, err := c.newAttempt(errs.WriteSMTPDirectSend)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	outcome, entered, err := c.submitSMTPMessage(ctx, attempt, encoded.Bytes, envelope, utf8)
	if err != nil {
		if !entered {
			err = errs.NewWriteNotApplied(attempt, err)
		}
		return mail.DirectSendOutcome{}, err
	}
	c.keepSentCopy(ctx, mimecompose.WithBcc(encoded.Bytes, msg.BCC), &outcome)
	return outcome, nil
}

// keepSentCopy saves a sent message in the Sent folder, unless the
// provider's SMTP server does that itself. Failing to save the copy does
// not undo the send, so it becomes a warning.
func (c *accountClient) keepSentCopy(ctx context.Context, raw []byte, outcome *mail.DirectSendOutcome) {
	if smtpKeepsSentCopy(c.setup.SMTP.Host) {
		return
	}
	var folder imapFolderRecord
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		var err error
		_, folder, err = c.storeMessage(ctx, session, mail.SpecialUseSent, provider.CapSendDirect, raw, []goimap.Flag{goimap.FlagSeen}, nil, "")
		return err
	})
	if err != nil {
		outcome.Warnings = append(outcome.Warnings, sentCopyWarning(err))
		return
	}
	outcome.SentCopy = folderLabel(folder)
}

// sentCopyWarning says why maildo kept no copy of a sent message.
func sentCopyWarning(err error) string {
	var product *errs.Error
	if errors.As(err, &product) && product.Details()["reason"] == "special_use_role_missing" {
		return "the message was sent, but this account has no Sent folder, so maildo kept no copy. To keep copies from now on, run maildo folders create Sent"
	}
	return "the message was sent, but no copy was saved in Sent: " + shortCause(err)
}

// smtpKeepsSentCopy reports SMTP servers that put sent mail into Sent on
// their own: Gmail's and Microsoft's.
func smtpKeepsSentCopy(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, domain := range []string{"gmail.com", "googlemail.com", "office365.com", "outlook.com", "hotmail.com", "live.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// ReadComposeSourceMaterial reads what a reply or forward needs from the
// original message: its addresses, ids and text.
func (c *accountClient) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	var out provider.ComposeSourceMaterial
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		msg, err := c.resolveMessage(ctx, session, selector, errs.ProviderOperationIMAPMessagesRead, true)
		if err != nil {
			return err
		}
		m := c.toMessage(msg, mail.MessageBody{})
		out = provider.ComposeSourceMaterial{
			SourceID:          msg.id,
			InternetMessageID: msg.internetMessageID,
			ThreadID:          msg.threadID,
			From:              m.From,
			ReplyTo:           envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.ReplyTo }),
			To:                m.To,
			CC:                m.CC,
			Subject:           m.Subject,
			SentAt:            m.SentAt,
			InReplyTo:         append([]string(nil), msg.header.inReplyTo...),
			References:        append([]string(nil), msg.header.references...),
		}
		if text, _, err := c.fetchMessageBody(ctx, session, msg, mail.BodyModeText); err == nil {
			out.Text = []byte(text.Content)
		}
		if part, _ := preferredBodyPart(msg.bodyStructure, mail.BodyModeHTML); part != nil && part.mediaType == "text/html" {
			if html, _, err := c.fetchMessageBody(ctx, session, msg, mail.BodyModeHTML); err == nil {
				out.HTML = []byte(html.Content)
				out.HTMLHasCIDReferences = strings.Contains(strings.ToLower(html.Content), "cid:")
			}
		}
		return nil
	})
	return out, err
}

func (c *accountClient) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var out mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		msg := provider.ReplyMessage(c.from(nil), req, source)
		defer mimecompose.DestroyMessage(&msg)
		if _, err := c.encodeDraft(msg); err != nil {
			return err
		}
		out = replyResult(source, msg, req.Body.Format)
		return nil
	})
	return out, err
}

func (c *accountClient) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var out mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		msg := provider.ReplyMessage(c.from(nil), req, source)
		defer mimecompose.DestroyMessage(&msg)
		out = replyResult(source, msg, req.Body.Format)
		if req.Action == mail.ComposeExecutionSend {
			if err := checkBeforeSend(req.Check, msg); err != nil {
				return err
			}
			sent, err := c.sendMIME(ctx, msg)
			out.SentID, out.WriteOutcomeFacts, out.SentCopy, out.Warnings = sent.MessageID, sent.WriteOutcomeFacts, sent.SentCopy, sent.Warnings
			return err
		}
		id, facts, err := c.saveDraft(ctx, msg, errs.WriteIMAPReply)
		out.DraftID, out.WriteOutcomeFacts = id, facts
		out.ProviderMeta = c.draftMeta(mail.MetadataDraftWrite)
		return err
	})
	return out, err
}

// checkBeforeSend runs the caller's check on a reply or forward right
// before it goes out. A send without a check is a bug in the caller.
func checkBeforeSend(check mail.SendCheck, msg mimecompose.Message) error {
	if check == nil {
		return errs.NewProviderClientContractInvalid()
	}
	return check(provider.SendContentOf(msg))
}

func replyResult(source provider.ComposeSourceMaterial, msg mimecompose.Message, format mail.BodyFormat) mail.ReplyDraftResult {
	content := provider.SendContentOf(msg)
	return mail.ReplyDraftResult{
		Send:             &content,
		SourceID:         source.SourceID,
		SourceInternetID: source.InternetMessageID,
		To:               append([]mail.Recipient(nil), msg.To...),
		CC:               append([]mail.Recipient(nil), msg.CC...),
		BCC:              append([]mail.Recipient(nil), msg.BCC...),
		Subject:          msg.Subject,
		BodyFormat:       format,
	}
}

// forwardMessage builds a forward with the chosen attachments of the
// original message.
func (c *accountClient) forwardMessage(ctx context.Context, req provider.ForwardDraftRequest, source provider.ComposeSourceMaterial) (mimecompose.Message, []mail.Attachment, error) {
	msg := provider.ForwardMessage(c.from(nil), req, source)
	if req.Attachments.Mode == "" || req.Attachments.Mode == mail.ForwardAttachmentsNone {
		return msg, nil, nil
	}
	var selected []mail.Attachment
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		original, err := c.resolveMessage(ctx, session, mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: source.SourceID}, errs.ProviderOperationIMAPAttachmentsRead, false)
		if err != nil {
			return err
		}
		rows := c.attachmentsFromStructure(original.bodyStructure)
		selected, err = selectForwardAttachments(c, source.SourceID, rows, req.Attachments)
		if err != nil {
			return err
		}
		parts, _ := attachmentParts(original.bodyStructure)
		for _, row := range selected {
			path, err := parsePartID(row.ID)
			if err != nil {
				return attachmentNotFound(c, source.SourceID, row.ID)
			}
			var part *imapBodyPart
			for _, candidate := range parts {
				if samePath(candidate.path, path) {
					part = candidate
				}
			}
			if part == nil {
				return attachmentNotFound(c, source.SourceID, row.ID)
			}
			if part.encodedSize > uint64(limits.IMAPMaxEncodedAttachmentBytes) {
				return errs.NewIMAPAttachmentTooLarge(c.account, imapErrProvider(c.provider), source.SourceID, row.ID, errs.IMAPAttachmentSizeEncoded, uint64(limits.IMAPMaxEncodedAttachmentBytes), part.encodedSize)
			}
			content, err := c.fetchAttachmentContent(ctx, session, original.uid, path, part, source.SourceID, row.ID)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(content.Content, mail.MaxOutboundEncodedMessageBytes+1))
			_ = content.Content.Close()
			if err != nil {
				return err
			}
			name := row.Name
			if name == "" {
				name = "attachment"
			}
			msg.Attachments = append(msg.Attachments, mimecompose.BinaryPart{
				Role:        mimecompose.PartRoleAttachment,
				ContentType: firstNonEmptyString(row.ContentType, "application/octet-stream"),
				Disposition: "attachment",
				Filename:    name,
				Bytes:       data,
			})
		}
		return nil
	})
	if err != nil {
		mimecompose.DestroyMessage(&msg)
		return mimecompose.Message{}, nil, err
	}
	return msg, selected, nil
}

// selectForwardAttachments picks the original's attachments to forward:
// all of them (without inline images), or the ones named by name or id.
func selectForwardAttachments(c *accountClient, sourceID string, rows []mail.Attachment, sel mail.ForwardAttachmentSelection) ([]mail.Attachment, error) {
	if sel.Mode == mail.ForwardAttachmentsAll {
		var out []mail.Attachment
		for _, row := range rows {
			if !row.IsInline {
				out = append(out, row)
			}
		}
		return out, nil
	}
	var out []mail.Attachment
	seen := map[string]bool{}
	add := func(row mail.Attachment) {
		if !seen[row.ID] {
			seen[row.ID] = true
			out = append(out, row)
		}
	}
	for _, name := range sel.Names {
		var matches []mail.Attachment
		for _, row := range rows {
			if row.Name == name {
				matches = append(matches, row)
			}
		}
		switch len(matches) {
		case 0:
			return nil, attachmentNotFound(c, sourceID, name)
		case 1:
			add(matches[0])
		default:
			return nil, errs.NewWithDetails(errs.CodeAmbiguousAttachment, fmt.Sprintf("the original has %d attachments named %q", len(matches), name), map[string]any{
				"account": c.account, "provider": c.provider, "name": name,
				"hint": "pick one with --include-attachment-id; maildo attachments list shows the ids",
			})
		}
	}
	for _, id := range sel.IDs {
		found := false
		for _, row := range rows {
			if row.ID == id {
				add(row)
				found = true
			}
		}
		if !found {
			return nil, attachmentNotFound(c, sourceID, id)
		}
	}
	return out, nil
}

func (c *accountClient) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var out mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		msg, selected, err := c.forwardMessage(ctx, req, source)
		if err != nil {
			return err
		}
		defer mimecompose.DestroyMessage(&msg)
		if _, err := c.encodeDraft(msg); err != nil {
			return err
		}
		out = forwardResult(source, msg, req, selected)
		return nil
	})
	return out, err
}

func (c *accountClient) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.ValidateForwardDraft(ctx, req)
}

func (c *accountClient) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var out mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		msg, selected, err := c.forwardMessage(ctx, req, source)
		if err != nil {
			return err
		}
		defer mimecompose.DestroyMessage(&msg)
		out = forwardResult(source, msg, req, selected)
		if req.Action == mail.ComposeExecutionSend {
			if err := checkBeforeSend(req.Check, msg); err != nil {
				return err
			}
			sent, err := c.sendMIME(ctx, msg)
			out.SentID, out.WriteOutcomeFacts, out.SentCopy, out.Warnings = sent.MessageID, sent.WriteOutcomeFacts, sent.SentCopy, sent.Warnings
			return err
		}
		id, facts, err := c.saveDraft(ctx, msg, errs.WriteIMAPForward)
		out.DraftID, out.WriteOutcomeFacts = id, facts
		out.ProviderMeta = c.draftMeta(mail.MetadataDraftWrite)
		return err
	})
	return out, err
}

func forwardResult(source provider.ComposeSourceMaterial, msg mimecompose.Message, req provider.ForwardDraftRequest, selected []mail.Attachment) mail.ForwardDraftResult {
	content := provider.SendContentOf(msg)
	return mail.ForwardDraftResult{
		Send:                      &content,
		SourceID:                  source.SourceID,
		SourceInternetID:          source.InternetMessageID,
		To:                        append([]mail.Recipient(nil), msg.To...),
		CC:                        append([]mail.Recipient(nil), msg.CC...),
		BCC:                       append([]mail.Recipient(nil), msg.BCC...),
		Subject:                   msg.Subject,
		BodyFormat:                req.Body.Format,
		AttachmentCount:           len(selected),
		SelectedSourceAttachments: selected,
		FinalAttachments:          selected,
	}
}

// AddDraftAttachments adds files to a draft by storing a new version of it.
func (c *accountClient) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	ref, err := c.rewriteDraft(ctx, req.DraftID, func(doc *mimecompose.Document) error {
		msg, err := doc.Message()
		if err != nil {
			return err
		}
		defer mimecompose.DestroyMessage(&msg)
		parts := append(append([]mimecompose.BinaryPart(nil), msg.Attachments...), mimecompose.AttachmentParts(req.Uploads)...)
		return doc.Apply(mimecompose.Patch{Attachments: mimecompose.PartsPatch{Set: true, Values: parts}})
	})
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	final, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: ref.ID, MetadataOnly: true})
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	added := make([]mail.Attachment, 0, len(req.Uploads))
	for _, upload := range req.Uploads {
		for _, a := range final.Attachments {
			if a.Name == upload.Name {
				added = append(added, a)
				break
			}
		}
	}
	return mail.AddDraftAttachmentsResult{DraftID: ref.ID, Added: added, FinalAttachments: final.Attachments, WriteOutcomeFacts: ref.WriteOutcomeFacts, ProviderMeta: ref.ProviderMeta, Warnings: ref.Warnings}, nil
}

// RemoveDraftAttachment removes one attachment from a draft by storing a
// new version of it. The attachment is found by its name and its position
// among attachments with that name.
func (c *accountClient) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	before, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: req.DraftID, MetadataOnly: true})
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	var removed mail.Attachment
	nth := -1
	count := map[string]int{}
	for _, a := range before.Attachments {
		if a.ID == req.AttachmentID {
			removed, nth = a, count[a.Name]
			break
		}
		count[a.Name]++
	}
	if nth < 0 {
		return mail.RemoveDraftAttachmentResult{}, attachmentNotFound(c, req.DraftID, req.AttachmentID)
	}
	ref, err := c.rewriteDraft(ctx, req.DraftID, func(doc *mimecompose.Document) error {
		msg, err := doc.Message()
		if err != nil {
			return err
		}
		defer mimecompose.DestroyMessage(&msg)
		keep := func(parts []mimecompose.BinaryPart) ([]mimecompose.BinaryPart, bool) {
			var out []mimecompose.BinaryPart
			seen, dropped := 0, false
			for _, p := range parts {
				if !dropped && p.Filename == removed.Name {
					if seen == nth {
						dropped = true
						continue
					}
					seen++
				}
				out = append(out, p)
			}
			return out, dropped
		}
		if parts, ok := keep(msg.Attachments); ok {
			return doc.Apply(mimecompose.Patch{Attachments: mimecompose.PartsPatch{Set: true, Values: parts}})
		}
		if parts, ok := keep(msg.Related); ok {
			return doc.Apply(mimecompose.Patch{Related: mimecompose.PartsPatch{Set: true, Values: parts}})
		}
		return attachmentNotFound(c, req.DraftID, req.AttachmentID)
	})
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	final, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: ref.ID, MetadataOnly: true})
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	return mail.RemoveDraftAttachmentResult{DraftID: ref.ID, Removed: removed, FinalAttachments: final.Attachments, WriteOutcomeFacts: ref.WriteOutcomeFacts, ProviderMeta: ref.ProviderMeta, Warnings: ref.Warnings}, nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
