package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
	"github.com/vincentsch/maildo/internal/provider"
)

type DirectSendPreparer struct{}

func (DirectSendPreparer) ProviderName() string { return "gmail" }

func (DirectSendPreparer) PrepareDirectSend(ctx context.Context, identity provider.ComposeIdentity, msg mail.OutboundMessage) (mail.PreparedDirectSend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if identity.Provider != "gmail" || identity.Account == "" || strings.TrimSpace(identity.Email) == "" {
		return nil, errs.NewProviderClientContractInvalid()
	}
	encoded, err := encodeGmailMessage(identity.Account, gmailMIMEMessage(mail.Recipient{Name: identity.DisplayName, Address: identity.Email}, msg))
	if err != nil {
		return nil, err
	}
	defer encoded.Destroy()
	return &preparedDirectSend{encoded: append([]byte(nil), encoded.Bytes...), projection: cloneSemanticProjection(encoded.Projection)}, nil
}

type preparedDirectSend struct {
	mu         sync.Mutex
	encoded    []byte
	projection mimecompose.SemanticProjection
	consumed   bool
	destroyed  bool
}

func (p *preparedDirectSend) Destroy() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed {
		return
	}
	p.destroyed = true
	zeroBytes(p.encoded)
	p.encoded = nil
	p.projection.Destroy()
}

func (p *preparedDirectSend) consume() ([]byte, mimecompose.SemanticProjection, error) {
	if p == nil {
		return nil, mimecompose.SemanticProjection{}, errs.NewProviderClientContractInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed || p.consumed {
		return nil, mimecompose.SemanticProjection{}, errs.NewProviderClientContractInvalid()
	}
	p.consumed = true
	encoded := append([]byte(nil), p.encoded...)
	projection := cloneSemanticProjection(p.projection)
	zeroBytes(p.encoded)
	p.encoded = nil
	p.projection.Destroy()
	return encoded, projection, nil
}

func (c *Client) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	encoded, err := encodeGmailMessage(c.account, gmailMIMEMessage(c.composeIdentity(), req.Message))
	if err != nil {
		return mail.DraftRef{}, err
	}
	defer encoded.Destroy()
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailDraftCreate)
	if err != nil {
		return mail.DraftRef{}, err
	}
	created, facts, err := c.createDraftEncoded(ctx, attempt, errs.WriteGmailDraftCreate, encoded.Bytes, encoded.Projection, "", "")
	if err != nil {
		return mail.DraftRef{}, err
	}
	return mail.DraftRef{ID: created.ID, WriteOutcomeFacts: facts, ProviderMeta: gmailDraftMeta(mail.MetadataDraftWrite, created.MessageID, nil)}, nil
}

func (c *Client) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	if req.MetadataOnly {
		return c.readDraftAttachmentMetadata(ctx, req.DraftID)
	}
	mode := req.BodyMode
	if mode == "" {
		mode = mail.BodyModeReadable
	}
	resp, err := c.fetchDraft(ctx, req.DraftID, "full", "", "id,message(id,threadId,labelIds,internalDate,payload)", limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return mail.Draft{}, err
	}
	if resp.ID == "" || resp.Message == nil {
		return mail.Draft{}, providerDataError(c.account, "invalid_protocol")
	}
	if !labelsExactly(resp.Message.LabelIDs, "DRAFT") {
		return mail.Draft{}, providerDataError(c.account, "invalid_protocol")
	}
	if mode == mail.BodyModeRaw {
		snapshot, err := c.fetchRawDraft(ctx, req.DraftID, gmailRawDraftPublic)
		if err != nil {
			return mail.Draft{}, err
		}
		if snapshot.ID != resp.ID || snapshot.MessageID != resp.Message.ID || snapshot.ThreadID != resp.Message.ThreadID {
			return mail.Draft{}, providerDataError(c.account, "invalid_protocol")
		}
		msg, err := c.messageFromFull(ctx, *resp.Message, false, mode)
		if err != nil {
			return mail.Draft{}, err
		}
		body := mail.MessageBody{ContentType: "message/rfc822", Mode: mail.BodyModeRaw, Encoding: "utf8", Content: string(snapshot.Raw)}
		if !utf8.Valid(snapshot.Raw) {
			body.Encoding = "base64"
			body.Content = base64.StdEncoding.EncodeToString(snapshot.Raw)
		}
		return c.draftFromMessage(resp.ID, msg, body), nil
	}
	msg, err := c.messageFromFull(ctx, *resp.Message, true, mode)
	if err != nil {
		return mail.Draft{}, err
	}
	return c.draftFromMessage(resp.ID, msg, msg.Body), nil
}

func (c *Client) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	prepared, err := c.prepareDraftUpdate(ctx, req)
	if err != nil {
		return mail.DraftRef{}, err
	}
	defer prepared.Destroy()
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailDraftUpdate)
	if err != nil {
		return mail.DraftRef{}, err
	}
	updated, facts, err := c.updateDraftEncoded(ctx, attempt, req.DraftID, prepared.Encoded.Bytes, prepared.Encoded.Projection, prepared.ThreadID, prepared.DetachFromThreadID)
	if err != nil {
		return mail.DraftRef{}, err
	}
	return mail.DraftRef{ID: updated.ID, WriteOutcomeFacts: facts, ProviderMeta: gmailDraftMeta(mail.MetadataDraftWrite, updated.MessageID, nil)}, nil
}

func (c *Client) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	prepared, err := c.prepareDraftUpdate(ctx, req)
	if err != nil {
		return mail.DraftUpdateValidation{}, err
	}
	defer prepared.Destroy()
	return mail.DraftUpdateValidation{ThreadTransition: prepared.ThreadTransition}, nil
}

type gmailPreparedDraftUpdate struct {
	Encoded            *mimecompose.EncodedMessage
	ThreadID           string
	DetachFromThreadID string
	ThreadTransition   string
}

func (p *gmailPreparedDraftUpdate) Destroy() {
	if p == nil {
		return
	}
	if p.Encoded != nil {
		p.Encoded.Destroy()
		p.Encoded = nil
	}
	p.ThreadID = ""
	p.DetachFromThreadID = ""
	p.ThreadTransition = ""
}

func (c *Client) prepareDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (*gmailPreparedDraftUpdate, error) {
	snapshot, err := c.fetchRawDraft(ctx, req.DraftID, gmailRawDraftEditable)
	if err != nil {
		return nil, err
	}
	doc, err := mimecompose.ParseDocument(snapshot.Raw, mimecompose.DefaultLimits())
	zeroBytes(snapshot.Raw)
	if err != nil {
		return nil, gmailMIMEError(c.account, err)
	}
	defer doc.Destroy()
	before, err := doc.Projection()
	if err != nil {
		return nil, gmailMIMEError(c.account, err)
	}
	defer before.Destroy()
	if err := doc.Apply(provider.MIMEPatch(req.Patch)); err != nil {
		return nil, gmailMIMEError(c.account, err)
	}
	encoded, err := doc.Encode(mimecompose.EncodeOptions{BccPolicy: mimecompose.BccHeaderPolicyRetain, AddressMode: mimecompose.AddressHeaderASCII})
	if err != nil {
		return nil, gmailMIMEError(c.account, err)
	}
	projection, err := mimecompose.ParseVerificationProjection(encoded.Bytes, gmailVerificationLimits())
	if err != nil {
		encoded.Destroy()
		return nil, gmailMIMEError(c.account, err)
	}
	encoded.Projection.Destroy()
	encoded.Projection = projection
	threadID := snapshot.ThreadID
	detachFromThreadID := ""
	transition := "preserve"
	if encoded.Projection.Subject != before.Subject {
		detachFromThreadID = snapshot.ThreadID
		threadID = ""
		transition = "detach"
	}
	return &gmailPreparedDraftUpdate{Encoded: encoded, ThreadID: threadID, DetachFromThreadID: detachFromThreadID, ThreadTransition: transition}, nil
}

// SendDraft sends the draft exactly as maildo read it. It uploads the raw
// message it checked together with the draft id, which Gmail supports for
// drafts.send. So an edit that lands after the check cannot change what goes
// out.
func (c *Client) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	if req.Check == nil {
		return mail.SendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	snapshot, err := c.fetchRawDraft(ctx, req.DraftID, gmailRawDraftVerification)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	defer zeroBytes(snapshot.Raw)
	expected, err := mimecompose.ParseVerificationProjection(snapshot.Raw, gmailVerificationLimits())
	if err != nil {
		return mail.SendOutcome{}, gmailMIMEError(c.account, err)
	}
	defer expected.Destroy()
	if err := req.Check(provider.SendContentOfProjection(expected)); err != nil {
		return mail.SendOutcome{}, err
	}
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailDraftSend)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	out, facts, err := c.sendDraftWithAttempt(ctx, attempt, errs.WriteGmailDraftSend, req.DraftID, snapshot.Raw, expected, snapshot.ThreadID, nil)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	return mail.SendOutcome{MessageID: out.MessageID, WriteOutcomeFacts: facts, ProviderMeta: gmailDraftMeta(mail.MetadataDraftSend, out.MessageID, nil)}, nil
}

// ValidateDraftSend reads the draft for the preview and the approval code.
// SendDraft reads it again and sends only when it still matches.
func (c *Client) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	snapshot, err := c.fetchRawDraft(ctx, req.DraftID, gmailRawDraftVerification)
	if err != nil {
		return mail.Draft{}, err
	}
	expected, err := mimecompose.ParseVerificationProjection(snapshot.Raw, gmailVerificationLimits())
	zeroBytes(snapshot.Raw)
	if err != nil {
		return mail.Draft{}, gmailMIMEError(c.account, err)
	}
	defer expected.Destroy()
	content := provider.SendContentOfProjection(expected)
	body := content.Text
	if strings.TrimSpace(body) == "" && content.HTML != "" {
		body = htmltext.ToReadable(content.HTML)
	}
	var attachments []mail.Attachment
	for _, part := range content.Attachments {
		attachments = append(attachments, mail.Attachment{Name: part.Name, ContentType: part.ContentType, Size: part.Size, IsInline: part.Inline})
	}
	return mail.Draft{
		Account:        c.account,
		Provider:       "gmail",
		ID:             snapshot.ID,
		IsDraft:        true,
		ThreadID:       snapshot.ThreadID,
		Folder:         mail.FolderSummary{ID: "DRAFT", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "DRAFT"},
		From:           content.From,
		ReplyTo:        content.ReplyTo,
		To:             content.To,
		CC:             content.CC,
		BCC:            content.BCC,
		Subject:        content.Subject,
		Body:           mail.MessageBody{ContentType: "text/plain", Mode: mail.BodyModeText, Content: body},
		HasAttachments: len(attachments) > 0,
		Attachments:    attachments,
		ProviderMeta:   gmailDraftMeta(mail.MetadataDraft, snapshot.MessageID, nil),
		Send:           &content,
	}, nil
}

func (c *Client) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	if !gmailOutboundMessageEmpty(req.Message) {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	prepared, ok := req.Prepared.(*preparedDirectSend)
	if !ok {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	encoded, projection, err := prepared.consume()
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	defer zeroBytes(encoded)
	defer projection.Destroy()
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailDirectSend)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	sent, facts, err := c.sendEncodedMessage(ctx, attempt, encoded, projection, "")
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	return mail.DirectSendOutcome{MessageID: sent.MessageID, WriteOutcomeFacts: facts}, nil
}

func (c *Client) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	id, err := c.resolveMessageID(ctx, selector)
	if err != nil {
		return provider.ComposeSourceMaterial{}, err
	}
	msg, err := c.fetchMessage(ctx, id, "full", "", "id,threadId,labelIds,internalDate,payload")
	if err != nil {
		return provider.ComposeSourceMaterial{}, err
	}
	if msg.ID == "" || msg.ThreadID == "" || msg.Payload == nil {
		return provider.ComposeSourceMaterial{}, providerDataError(c.account, "invalid_protocol")
	}
	normalized, err := c.normalizePayload(msg, true)
	if err != nil {
		return provider.ComposeSourceMaterial{}, err
	}
	material := provider.ComposeSourceMaterial{
		SourceID:          msg.ID,
		InternetMessageID: normalized.internetMessageID,
		ThreadID:          msg.ThreadID,
		From:              normalized.from,
		ReplyTo:           normalized.replyTo,
		To:                normalized.to,
		CC:                normalized.cc,
		Subject:           normalized.subject,
		SentAt:            normalized.sentAt,
	}
	if material.InReplyTo, err = firstMessageIDTokens(normalized.headerMap["in-reply-to"]); err != nil {
		return provider.ComposeSourceMaterial{}, providerDataError(c.account, "invalid_message_id_header")
	}
	if material.References, err = firstMessageIDTokens(normalized.headerMap["references"]); err != nil {
		return provider.ComposeSourceMaterial{}, providerDataError(c.account, "invalid_message_id_header")
	}
	if normalized.textPart != nil {
		data, err := c.leafBodyBytes(ctx, msg.ID, normalized.textPart)
		if err != nil {
			return provider.ComposeSourceMaterial{}, err
		}
		text, err := decodeTextBody(c.account, data, normalized.textPart.charset)
		if err != nil {
			return provider.ComposeSourceMaterial{}, err
		}
		material.Text = []byte(text)
	}
	if normalized.htmlPart != nil {
		data, err := c.leafBodyBytes(ctx, msg.ID, normalized.htmlPart)
		if err != nil {
			return provider.ComposeSourceMaterial{}, err
		}
		htmlBody, err := decodeTextBody(c.account, data, normalized.htmlPart.charset)
		if err != nil {
			return provider.ComposeSourceMaterial{}, err
		}
		material.HTML = []byte(htmlBody)
		material.HTMLHasCIDReferences = strings.Contains(strings.ToLower(htmlBody), "cid:")
	}
	return material, nil
}

func (c *Client) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var result mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg := provider.ReplyMessage(c.composeIdentity(), req, source)
		encoded, err := encodeGmailMessage(c.account, msg)
		if err != nil {
			return err
		}
		defer encoded.Destroy()
		content := provider.SendContentOf(msg)
		result = mail.ReplyDraftResult{
			SourceID:         source.SourceID,
			SourceInternetID: source.InternetMessageID,
			To:               msg.To,
			CC:               msg.CC,
			BCC:              msg.BCC,
			Subject:          msg.Subject,
			BodyFormat:       req.Body.Format,
			Send:             &content,
		}
		return nil
	})
	return result, err
}

func (c *Client) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var result mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		msg := provider.ReplyMessage(c.composeIdentity(), req, source)
		if err := checkBeforeSend(req.Action, req.Check, msg); err != nil {
			return err
		}
		encoded, err := encodeGmailMessage(c.account, msg)
		if err != nil {
			return err
		}
		defer encoded.Destroy()
		attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailReply)
		if err != nil {
			return err
		}
		created, err := c.createDraftEncodedPending(ctx, attempt, errs.WriteGmailReply, encoded.Bytes, encoded.Projection, source.ThreadID, "")
		if err != nil {
			return err
		}
		var facts *mail.WriteOutcomeFacts
		if req.Action == mail.ComposeExecutionDraft {
			success, err := errs.VerifiedWriteSuccessFacts(attempt)
			if err != nil {
				return err
			}
			facts = mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(success))
		}
		result = mail.ReplyDraftResult{
			SourceID:          source.SourceID,
			SourceInternetID:  source.InternetMessageID,
			DraftID:           created.ID,
			To:                msg.To,
			CC:                msg.CC,
			BCC:               msg.BCC,
			Subject:           msg.Subject,
			BodyFormat:        req.Body.Format,
			WriteOutcomeFacts: facts,
			ProviderMeta:      gmailDraftMeta(mail.MetadataDraftWrite, created.MessageID, nil),
		}
		if req.Action == mail.ComposeExecutionSend {
			plan, err := errs.NewKnownPartialPlan(errs.KnownGmailReplySend, []errs.KnownPartialPhase{errs.KnownPhaseDraftVerified, errs.KnownPhaseSend})
			if err != nil {
				return err
			}
			sendAttempt, err := errs.AdvanceVerifiedWritePhase(attempt, plan, 1)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				if crossErr := errs.CrossProviderWriteTransmissionBoundary(sendAttempt); crossErr != nil {
					return crossErr
				}
				return errs.NewKnownPartialBeforeNextPhase(sendAttempt, err, plan, 1, created.ID)
			}
			sent, sendFacts, err := c.sendDraftWithAttempt(ctx, sendAttempt, errs.WriteGmailReply, created.ID, encoded.Bytes, encoded.Projection, source.ThreadID, &gmailKnownPartial{plan: plan, verifiedDraftID: created.ID})
			if err != nil {
				return err
			}
			result.SentID = sent.MessageID
			result.WriteOutcomeFacts = sendFacts
		}
		return nil
	})
	return result, err
}

func (c *Client) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		prepared, err := c.prepareForwardMessage(ctx, req, source)
		if err != nil {
			return err
		}
		defer prepared.Destroy()
		if err := checkBeforeSend(req.Action, req.Check, prepared.message); err != nil {
			return err
		}
		encoded, err := encodeGmailMessage(c.account, prepared.message)
		if err != nil {
			return err
		}
		defer encoded.Destroy()
		attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailForward)
		if err != nil {
			return err
		}
		created, err := c.createDraftEncodedPending(ctx, attempt, errs.WriteGmailForward, encoded.Bytes, encoded.Projection, "", "")
		if err != nil {
			return err
		}
		finalAttachments := []mail.Attachment(nil)
		if req.Attachments.Mode != "" && req.Attachments.Mode != mail.ForwardAttachmentsNone {
			rows, _, err := c.verifyDraftAttachmentProjection(ctx, created, encoded.Projection)
			if err != nil {
				return c.postWriteVerificationUnknown(attempt, err)
			}
			finalAttachments = rows
		}
		var facts *mail.WriteOutcomeFacts
		if req.Action == mail.ComposeExecutionDraft {
			success, err := errs.VerifiedWriteSuccessFacts(attempt)
			if err != nil {
				return err
			}
			facts = mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(success))
		}
		result = mail.ForwardDraftResult{
			SourceID:                  source.SourceID,
			SourceInternetID:          source.InternetMessageID,
			DraftID:                   created.ID,
			To:                        prepared.message.To,
			CC:                        prepared.message.CC,
			BCC:                       prepared.message.BCC,
			Subject:                   prepared.message.Subject,
			BodyFormat:                req.Body.Format,
			AttachmentCount:           len(finalAttachments),
			SelectedSourceAttachments: prepared.selectedSourceAttachments,
			FinalAttachments:          finalAttachments,
			WriteOutcomeFacts:         facts,
			ProviderMeta:              gmailDraftMeta(mail.MetadataDraftWrite, created.MessageID, nil),
		}
		if req.Action == mail.ComposeExecutionSend {
			plan, err := errs.NewKnownPartialPlan(errs.KnownGmailForwardSend, []errs.KnownPartialPhase{errs.KnownPhaseDraftVerified, errs.KnownPhaseSend})
			if err != nil {
				return err
			}
			sendAttempt, err := errs.AdvanceVerifiedWritePhase(attempt, plan, 1)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				if crossErr := errs.CrossProviderWriteTransmissionBoundary(sendAttempt); crossErr != nil {
					return crossErr
				}
				return errs.NewKnownPartialBeforeNextPhase(sendAttempt, err, plan, 1, created.ID)
			}
			sent, sendFacts, err := c.sendDraftWithAttempt(ctx, sendAttempt, errs.WriteGmailForward, created.ID, encoded.Bytes, encoded.Projection, "", &gmailKnownPartial{plan: plan, verifiedDraftID: created.ID})
			if err != nil {
				return err
			}
			result.SentID = sent.MessageID
			result.WriteOutcomeFacts = sendFacts
		}
		return nil
	})
	return result, err
}

func (c *Client) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		prepared, err := c.prepareForwardMessage(ctx, req, source)
		if err != nil {
			return err
		}
		defer prepared.Destroy()
		encoded, err := encodeGmailMessage(c.account, prepared.message)
		if err != nil {
			return err
		}
		defer encoded.Destroy()
		content := provider.SendContentOf(prepared.message)
		result = mail.ForwardDraftResult{
			SourceID:                  source.SourceID,
			SourceInternetID:          source.InternetMessageID,
			To:                        prepared.message.To,
			CC:                        prepared.message.CC,
			BCC:                       prepared.message.BCC,
			Subject:                   prepared.message.Subject,
			BodyFormat:                req.Body.Format,
			AttachmentCount:           len(prepared.message.Related) + len(prepared.message.Attachments),
			SelectedSourceAttachments: prepared.selectedSourceAttachments,
			Send:                      &content,
		}
		return nil
	})
	return result, err
}

func (c *Client) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.ValidateForwardDraft(ctx, req)
}

type gmailDraftWriteResult struct {
	ID        string
	MessageID string
	ThreadID  string
	LabelIDs  []string
}

type gmailSendWriteResult struct {
	MessageID string
	ThreadID  string
	LabelIDs  []string
}

type gmailDraftResponse struct {
	ID      string        `json:"id"`
	Message *gmailMessage `json:"message"`
}

type gmailRawDraftPurpose int

const (
	gmailRawDraftPublic gmailRawDraftPurpose = iota
	gmailRawDraftEditable
	gmailRawDraftVerification
)

type gmailRawDraft struct {
	ID        string
	MessageID string
	ThreadID  string
	LabelIDs  []string
	Raw       []byte
}

type gmailKnownPartial struct {
	plan            errs.KnownPartialPlan
	verifiedDraftID string
}

func (c *Client) createDraftEncoded(ctx context.Context, attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, encoded []byte, projection mimecompose.SemanticProjection, threadID, detachFromThreadID string) (gmailDraftWriteResult, *mail.WriteOutcomeFacts, error) {
	result, err := c.createDraftEncodedPending(ctx, attempt, operation, encoded, projection, threadID, detachFromThreadID)
	if err != nil {
		return gmailDraftWriteResult{}, nil, err
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return gmailDraftWriteResult{}, nil, err
	}
	return result, mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), nil
}

func (c *Client) createDraftEncodedPending(ctx context.Context, attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, encoded []byte, projection mimecompose.SemanticProjection, threadID, detachFromThreadID string) (gmailDraftWriteResult, error) {
	resp, err := c.uploadDraft(ctx, attempt, operation, http.MethodPost, "/drafts", "", encoded, threadID)
	if err != nil {
		return gmailDraftWriteResult{}, err
	}
	result, err := draftWriteResult(resp)
	if err != nil {
		return gmailDraftWriteResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if err := c.verifyDraftProjection(ctx, result, threadID, detachFromThreadID, projection); err != nil {
		return result, c.postWriteVerificationUnknown(attempt, err)
	}
	return result, nil
}

func (c *Client) updateDraftEncoded(ctx context.Context, attempt errs.ProviderWriteAttempt, draftID string, encoded []byte, projection mimecompose.SemanticProjection, threadID, detachFromThreadID string) (gmailDraftWriteResult, *mail.WriteOutcomeFacts, error) {
	resp, err := c.uploadDraft(ctx, attempt, errs.WriteGmailDraftUpdate, http.MethodPut, "/drafts/"+url.PathEscape(draftID), draftID, encoded, threadID)
	if err != nil {
		return gmailDraftWriteResult{}, nil, err
	}
	result, err := draftWriteResult(resp)
	if err != nil {
		return gmailDraftWriteResult{}, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if result.ID != draftID {
		return result, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if err := c.verifyDraftProjection(ctx, result, threadID, detachFromThreadID, projection); err != nil {
		return result, nil, c.postWriteVerificationUnknown(attempt, err)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return gmailDraftWriteResult{}, nil, err
	}
	return result, mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), nil
}

func (c *Client) uploadDraft(ctx context.Context, attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, method, path, draftID string, encoded []byte, threadID string) (gmailDraftResponse, error) {
	metadata := map[string]any{"message": map[string]any{}}
	if threadID != "" {
		metadata["message"].(map[string]any)["threadId"] = threadID
	}
	values := url.Values{}
	values.Set("uploadType", "multipart")
	values.Set("fields", "id,message(id,threadId,labelIds)")
	status, body, err := c.gmailUploadRequest(ctx, method, path, values, metadata, encoded, attempt, limits.GmailMaxComposeVerificationResponseBytes)
	if err != nil {
		return gmailDraftResponse{}, err
	}
	if status < 200 || status >= 300 {
		return gmailDraftResponse{}, c.writeStatusError(attempt, operation, path, draftID, status, body)
	}
	var resp gmailDraftResponse
	if err := decodeJSONResponse(c.account, body, &resp); err != nil {
		return gmailDraftResponse{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	return resp, nil
}

func (c *Client) sendEncodedMessage(ctx context.Context, attempt errs.ProviderWriteAttempt, encoded []byte, projection mimecompose.SemanticProjection, threadID string) (gmailSendWriteResult, *mail.WriteOutcomeFacts, error) {
	metadata := map[string]any{}
	if threadID != "" {
		metadata["threadId"] = threadID
	}
	values := url.Values{}
	values.Set("uploadType", "multipart")
	values.Set("fields", "id,threadId,labelIds")
	status, body, err := c.gmailUploadRequest(ctx, http.MethodPost, "/messages/send", values, metadata, encoded, attempt, limits.GmailMaxComposeVerificationResponseBytes)
	if err != nil {
		return gmailSendWriteResult{}, nil, err
	}
	if status < 200 || status >= 300 {
		return gmailSendWriteResult{}, nil, c.writeStatusError(attempt, errs.WriteGmailDirectSend, "/messages/send", "", status, body)
	}
	var msg gmailMessage
	if err := decodeJSONResponse(c.account, body, &msg); err != nil || msg.ID == "" || msg.ThreadID == "" || !sentLabelsValid(msg.LabelIDs) {
		return gmailSendWriteResult{}, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	result := gmailSendWriteResult{MessageID: msg.ID, ThreadID: msg.ThreadID, LabelIDs: append([]string(nil), msg.LabelIDs...)}
	if err := c.verifySentProjection(ctx, result, "", projection); err != nil {
		return gmailSendWriteResult{}, nil, c.postWriteVerificationUnknown(attempt, err)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return gmailSendWriteResult{}, nil, err
	}
	return result, mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), nil
}

// sendDraftWithAttempt sends a draft with the raw message maildo checked,
// through the upload form of drafts.send. Gmail replaces the draft's content
// with that message and sends it, so what goes out is what maildo checked.
func (c *Client) sendDraftWithAttempt(ctx context.Context, attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, draftID string, raw []byte, projection mimecompose.SemanticProjection, expectedThreadID string, partial *gmailKnownPartial) (gmailSendWriteResult, *mail.WriteOutcomeFacts, error) {
	message := map[string]any{}
	if expectedThreadID != "" {
		message["threadId"] = expectedThreadID
	}
	metadata := map[string]any{"id": draftID, "message": message}
	values := url.Values{}
	values.Set("uploadType", "multipart")
	values.Set("fields", "id,threadId,labelIds")
	status, body, err := c.gmailUploadRequest(ctx, http.MethodPost, "/drafts/send", values, metadata, raw, attempt, limits.GmailMaxComposeVerificationResponseBytes)
	if err != nil {
		return gmailSendWriteResult{}, nil, err
	}
	if status < 200 || status >= 300 {
		return gmailSendWriteResult{}, nil, c.writeStatusErrorWithPartial(attempt, operation, "/drafts/send", draftID, status, body, partial)
	}
	var msg gmailMessage
	if err := decodeJSONResponse(c.account, body, &msg); err != nil || msg.ID == "" || msg.ThreadID == "" || !sentLabelsValid(msg.LabelIDs) {
		return gmailSendWriteResult{}, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	result := gmailSendWriteResult{MessageID: msg.ID, ThreadID: msg.ThreadID, LabelIDs: append([]string(nil), msg.LabelIDs...)}
	if err := c.verifySentProjection(ctx, result, expectedThreadID, projection); err != nil {
		return gmailSendWriteResult{}, nil, c.postWriteVerificationUnknown(attempt, err)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return gmailSendWriteResult{}, nil, err
	}
	return result, mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), nil
}

func (c *Client) gmailUploadRequest(ctx context.Context, method, path string, values url.Values, metadata any, encoded []byte, attempt errs.ProviderWriteAttempt, limit int64) (int, []byte, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	if err := json.NewEncoder(metaPart).Encode(metadata); err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	mediaHeader := textproto.MIMEHeader{}
	mediaHeader.Set("Content-Type", "message/rfc822")
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	if _, err := mediaPart.Write(encoded); err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	if err := writer.Close(); err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	contentType := "multipart/related; boundary=" + writer.Boundary()
	return c.gmailWriteRequest(ctx, method, path, values, contentType, bytes.NewReader(body.Bytes()), attempt, limit, true)
}

func (c *Client) gmailJSONWriteRequest(ctx context.Context, method, path string, values url.Values, payload any, attempt errs.ProviderWriteAttempt, limit int64) (int, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	return c.gmailWriteRequest(ctx, method, path, values, "application/json", bytes.NewReader(data), attempt, limit, false)
}

func (c *Client) gmailWriteRequest(ctx context.Context, method, path string, values url.Values, contentType string, body io.Reader, attempt errs.ProviderWriteAttempt, limit int64, upload bool) (int, []byte, error) {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, err)
	}
	target, err := c.buildComposeURL(path, values, upload)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", contentType)
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		return 0, nil, err
	}
	resp, err := doNoRedirect(c.http, req)
	if err != nil {
		return 0, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if int64(len(data)) > limit {
		return resp.StatusCode, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	return resp.StatusCode, data, nil
}

func (c *Client) buildComposeURL(path string, values url.Values, upload bool) (string, error) {
	if !upload {
		return c.buildURL(path, values)
	}
	base, err := url.Parse(c.apiBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || !validAPIBaseURL(base) {
		return "", providerAPIError(c.account, path)
	}
	if strings.Contains(path, "://") || strings.Contains(path, "..") || !strings.HasPrefix(path, "/") {
		return "", providerAPIError(c.account, path)
	}
	base.Path = "/upload" + strings.TrimRight(base.Path, "/") + path
	base.RawQuery = values.Encode()
	return base.String(), nil
}

func (c *Client) writeNotApplied(attempt errs.ProviderWriteAttempt, cause error) error {
	var product *errs.Error
	if errors.As(cause, &product) && product != nil {
		if err := errs.NewWriteNotApplied(attempt, product); err == nil {
			return err
		}
	}
	return cause
}

func (c *Client) postWriteVerificationUnknown(attempt errs.ProviderWriteAttempt, cause error) error {
	var product *errs.Error
	if errors.As(cause, &product) && product != nil && product.Code() == errs.CodeVerificationFailed {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
}

func (c *Client) writeRejected(attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, cause error) error {
	rejection := c.definitiveWriteRejection(cause, operation)
	if rejection == nil {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	return errs.NewWriteRejected(attempt, rejection)
}

func (c *Client) writeStatusError(attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, path, draftID string, status int, body []byte) error {
	rejection := c.writeStatusRejection(operation, path, draftID, status, body)
	if rejection != nil {
		return errs.NewWriteRejected(attempt, rejection)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
}

func (c *Client) writeStatusErrorWithPartial(attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, path, draftID string, status int, body []byte, partial *gmailKnownPartial) error {
	rejection := c.writeStatusRejection(operation, path, draftID, status, body)
	if rejection == nil {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	if partial != nil && partial.plan != nil {
		return errs.NewKnownPartial(attempt, rejection, partial.plan, 1, errs.KnownObservedLastVerified, partial.verifiedDraftID)
	}
	return errs.NewWriteRejected(attempt, rejection)
}

func (c *Client) writeStatusRejection(operation errs.ProviderWriteOperation, path, draftID string, status int, body []byte) errs.DefinitiveProviderRejection {
	cause := c.writeStatusCause(path, draftID, status, body)
	return c.definitiveWriteRejection(cause, operation)
}

func (c *Client) writeStatusCause(path, draftID string, status int, body []byte) error {
	reason := googleErrorReason(body)
	switch status {
	case http.StatusBadRequest:
		return errs.NewWithDetails(errs.CodeInvalidComposeInput, "Gmail rejected the message", map[string]any{
			"account":  c.account,
			"provider": "gmail",
			"reason":   nonemptyReason(reason, "bad_request"),
		})
	case http.StatusUnauthorized:
		return errs.NewWithDetails(errs.CodeAuthRepairRequired, "Gmail no longer accepts the saved login", map[string]any{
			"account":  c.account,
			"provider": "gmail",
		})
	case http.StatusForbidden:
		cause := classifyHTTPStatus(c.account, path, status, body, false)
		var product *errs.Error
		if errors.As(cause, &product) && product != nil {
			return product
		}
		return providerForbidden(c.account, nonemptyReason(reason, "forbidden"))
	case http.StatusNotFound, http.StatusGone:
		if strings.HasPrefix(path, "/drafts/") || path == "/drafts/send" {
			return draftNotFound(c.account, draftID)
		}
		return providerDataError(c.account, "endpoint_not_found")
	case http.StatusConflict:
		return errs.NewWithDetails(errs.CodeProviderConflict, "Gmail refused the change because the mailbox changed in the meantime", map[string]any{
			"account":  c.account,
			"provider": "gmail",
			"reason":   nonemptyReason(reason, "conflict"),
		})
	case http.StatusRequestEntityTooLarge:
		return errs.NewMessageTooLarge(c.account, errs.ProviderGmail, errs.MessageSizeOutboundEncoded, uint64(mail.MaxOutboundEncodedMessageBytes), uint64(mail.MaxOutboundEncodedMessageBytes+1))
	case http.StatusTooManyRequests:
		return providerRateLimited(c.account, nonemptyReason(reason, "rate_limit"))
	default:
		return nil
	}
}

func nonemptyReason(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (c *Client) definitiveWriteRejection(cause error, operation errs.ProviderWriteOperation) errs.DefinitiveProviderRejection {
	if cause == nil {
		return nil
	}
	if operation == "" {
		operation = errs.WriteGmailDraftCreate
	}
	wrapped := errs.NewDefinitiveProviderRejectionError(c.account, errs.ProviderGmail, operation, cause)
	rejection, ok := errs.AsDefinitiveProviderRejection(wrapped)
	if !ok {
		return nil
	}
	return rejection
}

func (c *Client) fetchDraft(ctx context.Context, draftID, format, metadataHeaders, fields string, limit int64) (gmailDraftResponse, error) {
	values := url.Values{}
	values.Set("format", format)
	if metadataHeaders != "" {
		values.Set("metadataHeaders", metadataHeaders)
	}
	if fields != "" {
		values.Set("fields", fields)
	}
	path := "/drafts/" + url.PathEscape(draftID)
	status, body, err := c.gmailRequest(ctx, http.MethodGet, path, values, nil, limit)
	if err != nil {
		return gmailDraftResponse{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return gmailDraftResponse{}, draftNotFound(c.account, draftID)
	}
	if status < 200 || status >= 300 {
		return gmailDraftResponse{}, classifyHTTPStatus(c.account, path, status, body, false)
	}
	var resp gmailDraftResponse
	if err := decodeJSONResponse(c.account, body, &resp); err != nil {
		return gmailDraftResponse{}, err
	}
	return resp, nil
}

func (c *Client) fetchRawDraft(ctx context.Context, draftID string, purpose gmailRawDraftPurpose) (gmailRawDraft, error) {
	responseLimit, encodedLimit, decodedLimit, encodedDimension, decodedDimension := rawDraftLimits(purpose)
	resp, err := c.fetchDraft(ctx, draftID, "raw", "", "id,message(id,threadId,labelIds,raw)", responseLimit)
	if err != nil {
		return gmailRawDraft{}, err
	}
	if resp.ID != draftID || resp.Message == nil || resp.Message.ID == "" || resp.Message.ThreadID == "" || resp.Message.Raw == "" || !labelsExactly(resp.Message.LabelIDs, "DRAFT") {
		return gmailRawDraft{}, providerDataError(c.account, "invalid_protocol")
	}
	raw, err := decodeComposeRaw(c.account, resp.Message.Raw, encodedLimit, decodedLimit, encodedDimension, decodedDimension)
	if err != nil {
		return gmailRawDraft{}, err
	}
	return gmailRawDraft{
		ID:        resp.ID,
		MessageID: resp.Message.ID,
		ThreadID:  resp.Message.ThreadID,
		LabelIDs:  append([]string(nil), resp.Message.LabelIDs...),
		Raw:       raw,
	}, nil
}

func (c *Client) fetchRawMessage(ctx context.Context, id string) (*gmailMessage, []byte, error) {
	msg, err := c.fetchMessage(ctx, id, "raw", "", "id,threadId,labelIds,raw")
	if err != nil {
		return nil, nil, err
	}
	if msg == nil || msg.ID == "" || msg.Raw == "" {
		return nil, nil, providerDataError(c.account, "invalid_protocol")
	}
	raw, err := decodeComposeRaw(c.account, msg.Raw, limits.GmailMaxComposeVerificationEncodedBytes, limits.GmailMaxComposeVerificationDecodedRFCBytes, errs.ReadLimitComposeVerificationEncodedBytes, errs.ReadLimitComposeVerificationRawBytes)
	if err != nil {
		return nil, nil, err
	}
	return msg, raw, nil
}

func (c *Client) verifyDraftProjection(ctx context.Context, expectedResult gmailDraftWriteResult, expectedThreadID, detachFromThreadID string, expected mimecompose.SemanticProjection) error {
	snapshot, err := c.fetchRawDraft(ctx, expectedResult.ID, gmailRawDraftVerification)
	if err != nil {
		return err
	}
	if snapshot.ID != expectedResult.ID || snapshot.MessageID != expectedResult.MessageID || snapshot.ThreadID != expectedResult.ThreadID || !labelsExactly(snapshot.LabelIDs, "DRAFT") {
		return errs.New(errs.CodeVerificationFailed, "Gmail draft verification identity did not match the accepted response")
	}
	if expectedThreadID != "" && snapshot.ThreadID != expectedThreadID {
		return errs.New(errs.CodeVerificationFailed, "Gmail draft verification did not preserve the expected thread")
	}
	if detachFromThreadID != "" && snapshot.ThreadID == detachFromThreadID {
		return errs.New(errs.CodeVerificationFailed, "Gmail draft verification did not detach from the prior thread")
	}
	actual, err := mimecompose.ParseVerificationProjection(snapshot.Raw, gmailVerificationLimits())
	if err != nil {
		return gmailMIMEError(c.account, err)
	}
	defer actual.Destroy()
	if !semanticProjectionEqual(expected, actual, false) {
		return errs.New(errs.CodeVerificationFailed, "Gmail draft verification did not match the submitted MIME")
	}
	return nil
}

func (c *Client) verifySentProjection(ctx context.Context, expectedResult gmailSendWriteResult, expectedThreadID string, expected mimecompose.SemanticProjection) error {
	msg, raw, err := c.fetchRawMessage(ctx, expectedResult.MessageID)
	if err != nil {
		return err
	}
	if msg.ID != expectedResult.MessageID || msg.ThreadID != expectedResult.ThreadID || !sentLabelsValid(msg.LabelIDs) {
		return providerDataError(c.account, "invalid_protocol")
	}
	if expectedThreadID != "" && msg.ThreadID != expectedThreadID {
		return errs.New(errs.CodeVerificationFailed, "Gmail sent-message verification did not preserve the expected thread")
	}
	actual, err := mimecompose.ParseVerificationProjection(raw, gmailVerificationLimits())
	if err != nil {
		return gmailMIMEError(c.account, err)
	}
	defer actual.Destroy()
	if !semanticProjectionEqual(expected, actual, true) {
		return errs.New(errs.CodeVerificationFailed, "Gmail sent-message verification did not match the submitted MIME")
	}
	return nil
}

func draftWriteResult(resp gmailDraftResponse) (gmailDraftWriteResult, error) {
	if resp.ID == "" || resp.Message == nil || resp.Message.ID == "" || resp.Message.ThreadID == "" || !labelsExactly(resp.Message.LabelIDs, "DRAFT") {
		return gmailDraftWriteResult{}, errs.NewProviderClientContractInvalid()
	}
	return gmailDraftWriteResult{ID: resp.ID, MessageID: resp.Message.ID, ThreadID: resp.Message.ThreadID, LabelIDs: append([]string(nil), resp.Message.LabelIDs...)}, nil
}

func (c *Client) draftFromMessage(draftID string, msg mail.Message, body mail.MessageBody) mail.Draft {
	return mail.Draft{
		Account:           c.account,
		Provider:          "gmail",
		ID:                draftID,
		IsDraft:           true,
		InternetMessageID: msg.InternetMessageID,
		ThreadID:          msg.ThreadID,
		Folder:            mail.FolderSummary{ID: "DRAFT", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "DRAFT"},
		From:              msg.From,
		ReplyTo:           msg.ReplyTo,
		To:                msg.To,
		CC:                msg.CC,
		BCC:               msg.BCC,
		Subject:           msg.Subject,
		CreatedAt:         msg.ReceivedAt,
		LastModifiedAt:    msg.ReceivedAt,
		HasAttachments:    msg.HasAttachments,
		Attachments:       msg.Attachments,
		Body:              body,
		ProviderMeta:      gmailDraftMeta(mail.MetadataDraft, msg.ID, nil),
	}
}

func encodeGmailMessage(account string, msg mimecompose.Message) (*mimecompose.EncodedMessage, error) {
	doc, err := mimecompose.NewDocument(msg, mimecompose.DefaultLimits())
	if err != nil {
		return nil, gmailMIMEError(account, err)
	}
	defer doc.Destroy()
	encoded, err := doc.Encode(mimecompose.EncodeOptions{BccPolicy: mimecompose.BccHeaderPolicyRetain, AddressMode: mimecompose.AddressHeaderASCII})
	if err != nil {
		return nil, gmailMIMEError(account, err)
	}
	projection, err := mimecompose.ParseVerificationProjection(encoded.Bytes, gmailVerificationLimits())
	if err != nil {
		encoded.Destroy()
		return nil, gmailMIMEError(account, err)
	}
	encoded.Projection.Destroy()
	encoded.Projection = projection
	return encoded, nil
}

func (c *Client) composeIdentity() mail.Recipient {
	return mail.Recipient{Name: c.displayName, Address: c.email}
}

func gmailMIMEMessage(from mail.Recipient, msg mail.OutboundMessage) mimecompose.Message {
	text, htmlBody := provider.MIMEBodies(msg.Body)
	return mimecompose.Message{
		From:        from,
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		ReplyTo:     msg.ReplyTo,
		Subject:     msg.Subject,
		TextBody:    []byte(text),
		HTMLBody:    []byte(htmlBody),
		Attachments: mimecompose.AttachmentParts(msg.Attachments),
	}
}

func firstMessageIDTokens(values []string) ([]string, error) {
	for _, value := range values {
		tokens, err := mail.ParseMessageIDTokens(value)
		if err == nil {
			return tokens, nil
		}
		return nil, err
	}
	return nil, nil
}

func gmailDraftMeta(use mail.ProviderMetadataUse, messageID string, guarded *bool) *mail.DraftProviderMetadata {
	meta, err := mail.NewProviderMetadata(errs.ProviderGmail, use, mail.DraftProviderMetadata{
		Source:             mail.MetadataSourceGmailAPI,
		SnapshotMessageID:  messageID,
		ConcurrencyGuarded: guarded,
	})
	if err != nil {
		panic(err)
	}
	return meta
}

func gmailVerificationLimits() mimecompose.VerificationLimits {
	semantic := mimecompose.DefaultLimits()
	semantic.MaxEncodedBytes = mail.MaxOutboundEncodedMessageBytes
	return mimecompose.VerificationLimits{
		Semantic:                    semantic,
		MaxRawBytes:                 limits.GmailMaxComposeVerificationDecodedRFCBytes,
		MaxDiscardedRootHeaderBytes: limits.GmailMaxComposeVerificationDiscardedRootHeaderBytes,
		MaxDiscardedRootHeaderCount: limits.GmailMaxComposeVerificationDiscardedRootHeaderCount,
	}
}

func rawDraftLimits(purpose gmailRawDraftPurpose) (responseLimit, encodedLimit, decodedLimit int64, encodedDimension, decodedDimension errs.ReadLimitDimension) {
	switch purpose {
	case gmailRawDraftPublic:
		return limits.GmailMaxJSONResponseBytes, 0, mail.MaxMessageBodyBytes, "", errs.ReadLimitBodyBytes
	case gmailRawDraftEditable:
		return limits.GmailMaxComposeRawResponseBytes, limits.GmailMaxAttachmentEncodedBytes, mail.MaxOutboundEncodedMessageBytes, "", ""
	default:
		return limits.GmailMaxComposeVerificationResponseBytes, limits.GmailMaxComposeVerificationEncodedBytes, limits.GmailMaxComposeVerificationDecodedRFCBytes, errs.ReadLimitComposeVerificationEncodedBytes, errs.ReadLimitComposeVerificationRawBytes
	}
}

func decodeComposeRaw(account, raw string, encodedLimit, decodedLimit int64, encodedDimension, decodedDimension errs.ReadLimitDimension) ([]byte, error) {
	if encodedLimit > 0 && int64(len(raw)) > encodedLimit {
		if encodedDimension != "" {
			return nil, errs.NewReadLimitExceeded(account, errs.ProviderGmail, encodedDimension, uint64(encodedLimit+1))
		}
		return nil, errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeOutboundEncoded, uint64(mail.MaxOutboundEncodedMessageBytes), uint64(mail.MaxOutboundEncodedMessageBytes+1))
	}
	data, err := decodeBase64URL(account, raw)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > decodedLimit {
		if decodedDimension != "" {
			return nil, errs.NewReadLimitExceeded(account, errs.ProviderGmail, decodedDimension, uint64(decodedLimit+1))
		}
		return nil, errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeOutboundEncoded, uint64(mail.MaxOutboundEncodedMessageBytes), uint64(mail.MaxOutboundEncodedMessageBytes+1))
	}
	return data, nil
}

func gmailMIMEError(account string, err error) error {
	if err == nil {
		return nil
	}
	if fact, ok := mimecompose.LimitViolationFromError(err); ok {
		switch fact.Kind {
		case mimecompose.LimitAddresses:
			return errs.NewTooManyAddresses(fact.Limit)
		case mimecompose.LimitVerificationRawBytes:
			return errs.NewReadLimitExceeded(account, errs.ProviderGmail, errs.ReadLimitComposeVerificationRawBytes, fact.ObservedAtLeast)
		case mimecompose.LimitDiscardedRootHeaders:
			return errs.NewReadLimitExceeded(account, errs.ProviderGmail, errs.ReadLimitComposeVerificationDiscardedHeaders, fact.ObservedAtLeast)
		case mimecompose.LimitDiscardedRootHeaderBytes:
			return errs.NewReadLimitExceeded(account, errs.ProviderGmail, errs.ReadLimitComposeVerificationDiscardedHeaderBytes, fact.ObservedAtLeast)
		case mimecompose.LimitEncodedMessageBytes:
			return errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeOutboundEncoded, fact.Limit, fact.ObservedAtLeast)
		default:
			return errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeComposeSource, fact.Limit, fact.ObservedAtLeast)
		}
	}
	switch {
	case errors.Is(err, mimecompose.ErrEncodedMessageTooLarge):
		return errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeOutboundEncoded, uint64(mail.MaxOutboundEncodedMessageBytes), uint64(mail.MaxOutboundEncodedMessageBytes+1))
	case errors.Is(err, mimecompose.ErrInvalidSemanticInput), errors.Is(err, mimecompose.ErrInvalidOptions):
		return errs.NewWithDetails(errs.CodeInvalidComposeInput, "maildo could not build a valid message from this input", map[string]any{"field": "body"})
	default:
		return providerDataError(account, "invalid_mime")
	}
}

func semanticProjectionEqual(expected, actual mimecompose.SemanticProjection, allowBCCSubset bool) bool {
	left := cloneSemanticProjection(expected)
	right := cloneSemanticProjection(actual)
	defer left.Destroy()
	defer right.Destroy()
	if allowBCCSubset {
		if !recipientSubset(right.BCC, left.BCC) {
			return false
		}
		left.BCC = nil
		right.BCC = nil
	}
	return reflect.DeepEqual(left, right)
}

func labelsExactly(labels []string, want ...string) bool {
	if len(labels) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, label := range labels {
		seen[label]++
	}
	for _, label := range want {
		if seen[label] != 1 {
			return false
		}
	}
	return true
}

func sentLabelsValid(labels []string) bool {
	return hasLabel(labels, "SENT") && !hasLabel(labels, "DRAFT")
}

func recipientSubset(subset, superset []mail.Recipient) bool {
	remaining := map[mail.Recipient]int{}
	for _, r := range superset {
		remaining[r]++
	}
	for _, r := range subset {
		if remaining[r] == 0 {
			return false
		}
		remaining[r]--
	}
	return true
}

func cloneSemanticProjection(in mimecompose.SemanticProjection) mimecompose.SemanticProjection {
	out := in
	out.To = append([]mail.Recipient(nil), in.To...)
	out.CC = append([]mail.Recipient(nil), in.CC...)
	out.BCC = append([]mail.Recipient(nil), in.BCC...)
	out.ReplyTo = append([]mail.Recipient(nil), in.ReplyTo...)
	out.TextBody = append([]byte(nil), in.TextBody...)
	out.HTMLBody = append([]byte(nil), in.HTMLBody...)
	out.References = append([]string(nil), in.References...)
	out.RootHeaders = append([]mimecompose.Header(nil), in.RootHeaders...)
	out.Related = append([]mimecompose.PartProjection(nil), in.Related...)
	out.Attachments = append([]mimecompose.PartProjection(nil), in.Attachments...)
	return out
}

// checkBeforeSend runs the caller's check on a reply or forward that is
// about to be sent. A send without a check is a bug in the caller.
func checkBeforeSend(action mail.ComposeExecutionAction, check mail.SendCheck, msg mimecompose.Message) error {
	if action != mail.ComposeExecutionSend {
		return nil
	}
	if check == nil {
		return errs.NewProviderClientContractInvalid()
	}
	return check(provider.SendContentOf(msg))
}

func gmailOutboundMessageEmpty(m mail.OutboundMessage) bool {
	return m.From == nil &&
		len(m.To) == 0 &&
		len(m.CC) == 0 &&
		len(m.BCC) == 0 &&
		len(m.ReplyTo) == 0 &&
		m.Subject == "" &&
		m.Body == (mail.OutboundBody{})
}

func draftNotFound(account, draftID string) error {
	return errs.NewWithDetails(errs.CodeDraftNotFound, "draft not found", map[string]any{
		"account":  account,
		"provider": "gmail",
		"draft_id": draftID,
	})
}

func (c *Client) draftNotFoundForWrite(draftID string) error {
	return draftNotFound(c.account, draftID)
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
