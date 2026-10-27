package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
)

type draftAttachmentSeal struct {
	marker byte
}

type gmailDraftAttachmentToken struct {
	mu        sync.Mutex
	account   string
	seal      *draftAttachmentSeal
	snapshot  mail.DraftAttachmentSnapshot
	destroyed bool
	consumed  bool
}

type draftAttachmentEdit struct {
	draftID    string
	messageID  string
	threadID   string
	snapshot   *gmailAttachmentSnapshot
	document   *mimecompose.Document
	message    mimecompose.Message
	projection mimecompose.SemanticProjection
	alignment  map[string]draftAttachmentPosition
}

type draftAttachmentPosition struct {
	role  mimecompose.PartRole
	index int
}

func (c *Client) readDraftAttachmentMetadata(ctx context.Context, draftID string) (mail.Draft, error) {
	resp, snapshot, normalized, err := c.readDraftAttachmentSnapshot(ctx, draftID, "")
	if err != nil {
		return mail.Draft{}, err
	}
	attachments := c.withDraftAttachmentTokens(resp.ID, snapshot)
	return mail.Draft{
		Account:           c.account,
		Provider:          "gmail",
		ID:                resp.ID,
		IsDraft:           true,
		InternetMessageID: normalized.internetMessageID,
		ThreadID:          resp.Message.ThreadID,
		Folder:            mail.FolderSummary{ID: "DRAFT", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "DRAFT"},
		From:              normalized.from,
		ReplyTo:           normalized.replyTo,
		To:                normalized.to,
		CC:                normalized.cc,
		BCC:               normalized.bcc,
		Subject:           normalized.subject,
		HasAttachments:    len(attachments) > 0,
		Attachments:       attachments,
		Body:              mail.MessageBody{},
		ProviderMeta:      gmailDraftMeta(mail.MetadataDraft, resp.Message.ID, nil),
	}, nil
}

func (c *Client) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	if strings.TrimSpace(req.DraftID) == "" || len(req.Uploads) == 0 {
		return mail.AddDraftAttachmentsResult{}, errs.NewProviderClientContractInvalid()
	}
	edit, err := c.readDraftAttachmentEdit(ctx, req.DraftID)
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	defer edit.Destroy()
	baseAttachmentCount := len(edit.message.Attachments)
	addedParts := make([]mimecompose.BinaryPart, 0, len(req.Uploads))
	for _, upload := range req.Uploads {
		part, err := binaryPartForUpload(upload)
		if err != nil {
			destroyBinaryParts(addedParts)
			return mail.AddDraftAttachmentsResult{}, err
		}
		addedParts = append(addedParts, part)
	}
	defer destroyBinaryParts(addedParts)
	nextAttachments := append(cloneBinaryParts(edit.message.Attachments), addedParts...)
	defer destroyBinaryParts(nextAttachments)
	if err := edit.document.Apply(mimecompose.Patch{Attachments: mimecompose.PartsPatch{Set: true, Values: nextAttachments}}); err != nil {
		return mail.AddDraftAttachmentsResult{}, gmailMIMEError(c.account, err)
	}
	encoded, err := encodeVerifiedDocument(c.account, edit.document)
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	defer encoded.Destroy()
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailAttachmentAdd)
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	updated, err := c.updateDraftEncodedPending(ctx, attempt, errs.WriteGmailAttachmentAdd, req.DraftID, edit.messageID, encoded.Bytes, encoded.Projection, edit.threadID, "")
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	finalRows, finalAlignment, err := c.verifyDraftAttachmentProjection(ctx, updated, encoded.Projection)
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, c.postWriteVerificationUnknown(attempt, err)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	added := rowsForAttachmentRange(finalRows, finalAlignment, baseAttachmentCount, len(req.Uploads))
	guarded := false
	return mail.AddDraftAttachmentsResult{
		DraftID:           updated.ID,
		Added:             added,
		FinalAttachments:  finalRows,
		WriteOutcomeFacts: mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
		ProviderMeta:      gmailDraftMeta(mail.MetadataDraftWrite, updated.MessageID, &guarded),
	}, nil
}

func (c *Client) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	snapshot, err := c.consumeDraftAttachmentToken(req)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	edit, err := c.readDraftAttachmentEdit(ctx, req.DraftID)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	defer edit.Destroy()
	part, ok := edit.snapshot.attachments[req.AttachmentID]
	if !ok || !sameDraftAttachmentSnapshot(snapshot, part) || snapshot.SnapshotMessageID != edit.messageID {
		return mail.RemoveDraftAttachmentResult{}, errs.NewGmailAttachmentStaleSnapshot(c.account, snapshot.SnapshotMessageID, req.AttachmentID)
	}
	position, ok := edit.alignment[req.AttachmentID]
	if !ok || position.role != mimecompose.PartRoleAttachment || position.index < 0 || position.index >= len(edit.message.Attachments) {
		return mail.RemoveDraftAttachmentResult{}, errs.NewProviderClientContractInvalid()
	}
	if part.attachment.IsInline || part.attachment.Kind != mail.AttachmentKindFile {
		return mail.RemoveDraftAttachmentResult{}, errs.NewProviderClientContractInvalid()
	}
	removed := cloneGmailAttachment(part.attachment)
	nextAttachments := cloneBinaryParts(edit.message.Attachments)
	defer destroyBinaryParts(nextAttachments)
	zeroBytes(nextAttachments[position.index].Bytes)
	nextAttachments[position.index].Bytes = nil
	nextAttachments = append(nextAttachments[:position.index], nextAttachments[position.index+1:]...)
	if err := edit.document.Apply(mimecompose.Patch{Attachments: mimecompose.PartsPatch{Set: true, Values: nextAttachments}}); err != nil {
		return mail.RemoveDraftAttachmentResult{}, gmailMIMEError(c.account, err)
	}
	encoded, err := encodeVerifiedDocument(c.account, edit.document)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	defer encoded.Destroy()
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailAttachmentRemove)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	updated, err := c.updateDraftEncodedPending(ctx, attempt, errs.WriteGmailAttachmentRemove, req.DraftID, edit.messageID, encoded.Bytes, encoded.Projection, edit.threadID, "")
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	finalRows, _, err := c.verifyDraftAttachmentProjection(ctx, updated, encoded.Projection)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, c.postWriteVerificationUnknown(attempt, err)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	guarded := false
	return mail.RemoveDraftAttachmentResult{
		DraftID:           updated.ID,
		Removed:           removed,
		FinalAttachments:  finalRows,
		WriteOutcomeFacts: mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
		ProviderMeta:      gmailDraftMeta(mail.MetadataDraftWrite, updated.MessageID, &guarded),
	}, nil
}

func (c *Client) readDraftAttachmentSnapshot(ctx context.Context, draftID, selectedID string) (gmailDraftResponse, *gmailAttachmentSnapshot, normalizedMessage, error) {
	resp, stats, err := c.fetchDraftAttachmentStructure(ctx, draftID, selectedID)
	if err != nil {
		return gmailDraftResponse{}, nil, normalizedMessage{}, err
	}
	if err := attachmentStructureBudgetError(c.account, stats, resp.Message.ID, selectedID); err != nil {
		return gmailDraftResponse{}, nil, normalizedMessage{}, err
	}
	normalized, state, err := c.normalizePayloadWithState(resp.Message, true)
	if err != nil {
		return gmailDraftResponse{}, nil, normalizedMessage{}, err
	}
	snapshot, _, err := c.attachmentSnapshotFromState(resp.Message.ID, state, selectedID)
	if err != nil {
		return gmailDraftResponse{}, nil, normalizedMessage{}, err
	}
	return resp, snapshot, normalized, nil
}

func (c *Client) fetchDraftAttachmentStructure(ctx context.Context, draftID, selectedID string) (gmailDraftResponse, attachmentJSONStats, error) {
	if draftID == "" {
		return gmailDraftResponse{}, attachmentJSONStats{}, providerDataError(c.account, "invalid_protocol")
	}
	values := url.Values{}
	values.Set("format", "full")
	values.Set("fields", "id,message(id,threadId,labelIds,payload)")
	path := "/drafts/" + url.PathEscape(draftID)
	body, err := c.gmailGETLimitedReader(ctx, path, path, values, func(status int, statusBody []byte) error {
		if status == http.StatusNotFound || status == http.StatusGone {
			return draftNotFound(c.account, draftID)
		}
		return classifyHTTPStatus(c.account, path, status, statusBody, false)
	})
	if err != nil {
		return gmailDraftResponse{}, attachmentJSONStats{}, err
	}
	defer body.Close()
	limited := newAttachmentLimitReader(body, limits.GmailMaxAttachmentStructureResponseBytes)
	resp, stats, err := parseDraftAttachmentStructureJSON(c.account, limited, selectedID, draftID)
	stats.totalBytes = limited.observed()
	if err != nil && attachmentParserErrorPrecedesCompleteLimit(err) {
		return gmailDraftResponse{}, attachmentJSONStats{}, err
	}
	if limited.over() {
		return gmailDraftResponse{}, attachmentJSONStats{}, errs.NewGmailAttachmentStructureReadLimitExceeded(c.account, uint64(limits.GmailMaxAttachmentStructureResponseBytes), limited.observed())
	}
	if err != nil {
		return gmailDraftResponse{}, attachmentJSONStats{}, err
	}
	if resp.ID != draftID || resp.Message == nil || resp.Message.ID == "" || resp.Message.ThreadID == "" || resp.Message.Payload == nil || !labelsExactly(resp.Message.LabelIDs, "DRAFT") {
		return gmailDraftResponse{}, attachmentJSONStats{}, providerDataError(c.account, "invalid_protocol")
	}
	return resp, stats, nil
}

func parseDraftAttachmentStructureJSON(account string, r io.Reader, selectedID, draftID string) (gmailDraftResponse, attachmentJSONStats, error) {
	stats := attachmentJSONStats{}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	parser := &attachmentJSONParser{
		account:           account,
		dec:               dec,
		stats:             &stats,
		selectedID:        selectedID,
		snapshotMessageID: draftID,
	}
	resp, err := parser.parseDraft()
	if err != nil {
		return gmailDraftResponse{}, stats, err
	}
	if tok, err := dec.Token(); err != io.EOF || tok != nil {
		return gmailDraftResponse{}, stats, providerDataError(account, "invalid_json")
	}
	return resp, stats, nil
}

func (p *attachmentJSONParser) parseDraft() (gmailDraftResponse, error) {
	if err := p.objectStart(); err != nil {
		return gmailDraftResponse{}, err
	}
	seen := map[string]bool{}
	var resp gmailDraftResponse
	var haveID, haveMessage bool
	var err error
	for p.dec.More() {
		key, keyErr := p.objectKey(seen)
		if keyErr != nil {
			return gmailDraftResponse{}, keyErr
		}
		switch key {
		case "id":
			resp.ID, err = p.readString()
			haveID = true
		case "message":
			resp.Message, err = p.parseMessage()
			haveMessage = true
		default:
			err = p.skipValue()
		}
		if err != nil {
			return gmailDraftResponse{}, err
		}
	}
	if err := p.endObject(); err != nil {
		return gmailDraftResponse{}, err
	}
	if !haveID || !haveMessage {
		return gmailDraftResponse{}, providerDataError(p.account, "invalid_protocol")
	}
	return resp, nil
}

func (c *Client) readDraftAttachmentEdit(ctx context.Context, draftID string) (*draftAttachmentEdit, error) {
	resp, snapshot, _, err := c.readDraftAttachmentSnapshot(ctx, draftID, "")
	if err != nil {
		return nil, err
	}
	raw, err := c.fetchRawDraft(ctx, draftID, gmailRawDraftEditable)
	if err != nil {
		return nil, err
	}
	if raw.ID != resp.ID || raw.MessageID != resp.Message.ID || raw.ThreadID != resp.Message.ThreadID || !labelsExactly(raw.LabelIDs, "DRAFT") {
		zeroBytes(raw.Raw)
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	doc, err := mimecompose.ParseDocument(raw.Raw, mimecompose.DefaultLimits())
	zeroBytes(raw.Raw)
	if err != nil {
		return nil, gmailMIMEError(c.account, err)
	}
	message, err := doc.Message()
	if err != nil {
		doc.Destroy()
		return nil, gmailMIMEError(c.account, err)
	}
	projection, err := doc.Projection()
	if err != nil {
		mimecompose.DestroyMessage(&message)
		doc.Destroy()
		return nil, gmailMIMEError(c.account, err)
	}
	alignment, err := c.alignSnapshotWithProjection(ctx, raw.MessageID, snapshot, projection, false)
	if err != nil {
		projection.Destroy()
		mimecompose.DestroyMessage(&message)
		doc.Destroy()
		return nil, err
	}
	return &draftAttachmentEdit{
		draftID:    raw.ID,
		messageID:  raw.MessageID,
		threadID:   raw.ThreadID,
		snapshot:   snapshot,
		document:   doc,
		message:    message,
		projection: projection,
		alignment:  alignment,
	}, nil
}

func (e *draftAttachmentEdit) Destroy() {
	if e == nil {
		return
	}
	if e.document != nil {
		e.document.Destroy()
		e.document = nil
	}
	mimecompose.DestroyMessage(&e.message)
	e.projection.Destroy()
	e.snapshot = nil
	e.alignment = nil
	e.draftID = ""
	e.messageID = ""
	e.threadID = ""
}

func (c *Client) updateDraftEncodedPending(ctx context.Context, attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, draftID, priorMessageID string, encoded []byte, projection mimecompose.SemanticProjection, threadID, detachFromThreadID string) (gmailDraftWriteResult, error) {
	resp, err := c.uploadDraft(ctx, attempt, operation, http.MethodPut, "/drafts/"+url.PathEscape(draftID), draftID, encoded, threadID)
	if err != nil {
		return gmailDraftWriteResult{}, err
	}
	result, err := draftWriteResult(resp)
	if err != nil {
		return gmailDraftWriteResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if result.ID != draftID {
		return result, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if priorMessageID != "" && result.MessageID == priorMessageID {
		return result, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if err := c.verifyDraftProjection(ctx, result, threadID, detachFromThreadID, projection); err != nil {
		return result, c.postWriteVerificationUnknown(attempt, err)
	}
	return result, nil
}

func encodeVerifiedDocument(account string, doc *mimecompose.Document) (*mimecompose.EncodedMessage, error) {
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

func (c *Client) verifyDraftAttachmentProjection(ctx context.Context, result gmailDraftWriteResult, expected mimecompose.SemanticProjection) ([]mail.Attachment, map[string]draftAttachmentPosition, error) {
	resp, snapshot, _, err := c.readDraftAttachmentSnapshot(ctx, result.ID, "")
	if err != nil {
		return nil, nil, err
	}
	if resp.Message.ID != result.MessageID || resp.Message.ThreadID != result.ThreadID || !labelsExactly(resp.Message.LabelIDs, "DRAFT") {
		return nil, nil, errs.New(errs.CodeVerificationFailed, "Gmail draft attachment verification identity did not match the accepted response")
	}
	alignment, err := c.alignSnapshotWithProjection(ctx, result.MessageID, snapshot, expected, true)
	if err != nil {
		return nil, nil, err
	}
	return cloneGmailAttachments(snapshot.ordered), alignment, nil
}

func (c *Client) alignSnapshotWithProjection(ctx context.Context, messageID string, snapshot *gmailAttachmentSnapshot, projection mimecompose.SemanticProjection, postWrite bool) (map[string]draftAttachmentPosition, error) {
	if snapshot == nil || snapshot.messageID != messageID {
		return nil, providerDataError(c.account, "invalid_protocol")
	}
	alignment := make(map[string]draftAttachmentPosition, len(snapshot.ordered))
	relatedIndex := 0
	attachmentIndex := 0
	for _, row := range snapshot.ordered {
		part, ok := snapshot.attachments[row.ID]
		if !ok {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		role := mimecompose.PartRoleAttachment
		index := 0
		var expected mimecompose.PartProjection
		if row.IsInline {
			role = mimecompose.PartRoleRelated
			index = relatedIndex
			if relatedIndex >= len(projection.Related) {
				return nil, draftAttachmentProjectionMismatch(c.account, postWrite)
			}
			expected = projection.Related[relatedIndex]
			relatedIndex++
		} else {
			index = attachmentIndex
			if attachmentIndex >= len(projection.Attachments) {
				return nil, draftAttachmentProjectionMismatch(c.account, postWrite)
			}
			expected = projection.Attachments[attachmentIndex]
			attachmentIndex++
		}
		decodedSize, digest, err := c.snapshotDecodedDigest(ctx, messageID, row.ID, part)
		if err != nil {
			return nil, err
		}
		if !projectionPartMatchesSnapshot(expected, part, decodedSize, digest) {
			return nil, draftAttachmentProjectionMismatch(c.account, postWrite)
		}
		alignment[row.ID] = draftAttachmentPosition{role: role, index: index}
	}
	if relatedIndex != len(projection.Related) || attachmentIndex != len(projection.Attachments) {
		return nil, draftAttachmentProjectionMismatch(c.account, postWrite)
	}
	return alignment, nil
}

func draftAttachmentProjectionMismatch(account string, postWrite bool) error {
	if postWrite {
		return errs.New(errs.CodeVerificationFailed, "Gmail draft attachment verification did not match the submitted MIME")
	}
	return providerDataError(account, "invalid_mime")
}

func projectionPartMatchesSnapshot(expected mimecompose.PartProjection, part gmailAttachmentSnapshotPart, decodedSize uint64, digest [sha256.Size]byte) bool {
	attachment := part.attachment
	if normalizedContentType(expected.ContentType) != normalizedContentType(attachment.ContentType) {
		return false
	}
	if attachment.ContentID != "" && normalizeContentID(expected.ContentID) != attachment.ContentID {
		return false
	}
	if expected.DecodedSize != decodedSize || expected.SHA256 != digest {
		return false
	}
	return true
}

func (c *Client) snapshotDecodedDigest(ctx context.Context, messageID, attachmentID string, part gmailAttachmentSnapshotPart) (uint64, [sha256.Size]byte, error) {
	switch part.representation {
	case gmailAttachmentInline:
		return part.decodedSize, part.inlineDigest, nil
	case gmailAttachmentImplicitZero:
		return 0, sha256.Sum256(nil), nil
	case gmailAttachmentExternal:
		content, err := c.externalAttachmentContent(ctx, messageID, attachmentID, part)
		if err != nil {
			return 0, [sha256.Size]byte{}, err
		}
		defer content.Content.Close()
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(content.Content, mail.MaxMessageAttachmentDownloadBytes+1))
		if err != nil {
			return 0, [sha256.Size]byte{}, providerDataError(c.account, "invalid_base64")
		}
		if n > mail.MaxMessageAttachmentDownloadBytes {
			return 0, [sha256.Size]byte{}, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), uint64(n))
		}
		if content.DeclaredSize >= 0 && n != content.DeclaredSize {
			return 0, [sha256.Size]byte{}, providerDataError(c.account, "invalid_protocol")
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		return uint64(n), digest, nil
	default:
		return 0, [sha256.Size]byte{}, providerDataError(c.account, "invalid_protocol")
	}
}

func binaryPartForUpload(upload mail.AttachmentUpload) (mimecompose.BinaryPart, error) {
	if strings.TrimSpace(upload.Name) == "" || strings.TrimSpace(upload.ContentType) == "" ||
		upload.Size < 0 || int64(len(upload.Bytes)) != upload.Size || upload.Size >= mail.MaxSimpleAttachmentBytes {
		return mimecompose.BinaryPart{}, errs.NewProviderClientContractInvalid()
	}
	return mimecompose.BinaryPart{
		Role:        mimecompose.PartRoleAttachment,
		ContentType: upload.ContentType,
		Disposition: "attachment",
		Filename:    upload.Name,
		Bytes:       append([]byte(nil), upload.Bytes...),
	}, nil
}

func rowsForAttachmentRange(rows []mail.Attachment, alignment map[string]draftAttachmentPosition, start, count int) []mail.Attachment {
	if count <= 0 {
		return nil
	}
	out := make([]mail.Attachment, 0, count)
	end := start + count
	for _, row := range rows {
		position, ok := alignment[row.ID]
		if ok && position.role == mimecompose.PartRoleAttachment && position.index >= start && position.index < end {
			out = append(out, cloneGmailAttachment(row))
		}
	}
	return out
}

func cloneBinaryParts(in []mimecompose.BinaryPart) []mimecompose.BinaryPart {
	if len(in) == 0 {
		return nil
	}
	out := make([]mimecompose.BinaryPart, len(in))
	for i, part := range in {
		out[i] = part
		out[i].Bytes = append([]byte(nil), part.Bytes...)
	}
	return out
}

func destroyBinaryParts(parts []mimecompose.BinaryPart) {
	for i := range parts {
		zeroBytes(parts[i].Bytes)
		parts[i].Bytes = nil
	}
}

func (c *Client) draftAttachmentSeal() *draftAttachmentSeal {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed.Load() {
		return nil
	}
	if c.attachSeal == nil {
		c.attachSeal = &draftAttachmentSeal{}
	}
	return c.attachSeal
}

func (c *Client) withDraftAttachmentTokens(draftID string, snapshot *gmailAttachmentSnapshot) []mail.Attachment {
	rows := cloneGmailAttachments(snapshot.ordered)
	seal := c.draftAttachmentSeal()
	if seal == nil {
		return rows
	}
	for i := range rows {
		part, ok := snapshot.attachments[rows[i].ID]
		if !ok {
			continue
		}
		snap := mail.DraftAttachmentSnapshot{
			DraftID:           draftID,
			SnapshotMessageID: snapshot.messageID,
			PartID:            rows[i].ID,
			ChildIndexPath:    append([]uint16(nil), part.childIndexPath...),
			ContentType:       rows[i].ContentType,
			Disposition:       part.disposition,
			ContentID:         rows[i].ContentID,
			DecodedSize:       part.decodedSize,
			DecodedSizeKnown:  part.decodedKnown,
			Kind:              rows[i].Kind,
		}
		rows[i].ExecutionToken = &gmailDraftAttachmentToken{account: c.account, seal: seal, snapshot: snap}
	}
	return rows
}

func (c *Client) consumeDraftAttachmentToken(req mail.RemoveDraftAttachmentRequest) (mail.DraftAttachmentSnapshot, error) {
	token, ok := req.ExecutionToken.(*gmailDraftAttachmentToken)
	if !ok || token == nil {
		return mail.DraftAttachmentSnapshot{}, errs.NewSharedProviderContractInvalid()
	}
	return token.consume(c.account, c.draftAttachmentSeal(), req.DraftID, req.AttachmentID)
}

func (t *gmailDraftAttachmentToken) Clone() mail.AttachmentExecutionToken {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.destroyed || t.consumed || t.account == "" || t.seal == nil {
		return nil
	}
	return &gmailDraftAttachmentToken{
		account:  strings.Clone(t.account),
		seal:     t.seal,
		snapshot: cloneDraftAttachmentSnapshot(t.snapshot),
	}
}

func (t *gmailDraftAttachmentToken) Destroy() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.destroyed {
		return
	}
	t.destroyed = true
	for i := range t.snapshot.ChildIndexPath {
		t.snapshot.ChildIndexPath[i] = 0
	}
	t.account = ""
	t.seal = nil
	t.snapshot = mail.DraftAttachmentSnapshot{}
}

func (t *gmailDraftAttachmentToken) consume(account string, seal *draftAttachmentSeal, draftID, attachmentID string) (mail.DraftAttachmentSnapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.destroyed || t.consumed || t.account != account || t.seal == nil || t.seal != seal ||
		t.snapshot.DraftID != draftID || t.snapshot.PartID != attachmentID {
		return mail.DraftAttachmentSnapshot{}, errs.NewSharedProviderContractInvalid()
	}
	t.consumed = true
	return cloneDraftAttachmentSnapshot(t.snapshot), nil
}

func cloneDraftAttachmentSnapshot(in mail.DraftAttachmentSnapshot) mail.DraftAttachmentSnapshot {
	out := in
	out.ChildIndexPath = append([]uint16(nil), in.ChildIndexPath...)
	return out
}

func sameDraftAttachmentSnapshot(snap mail.DraftAttachmentSnapshot, part gmailAttachmentSnapshotPart) bool {
	return snap.PartID == part.attachment.ID &&
		snap.ContentType == part.attachment.ContentType &&
		snap.Disposition == part.disposition &&
		snap.ContentID == part.attachment.ContentID &&
		snap.DecodedSize == part.decodedSize &&
		snap.DecodedSizeKnown == part.decodedKnown &&
		snap.Kind == part.attachment.Kind &&
		equalUint16Slices(snap.ChildIndexPath, part.childIndexPath)
}

func attachmentDisposition(att mail.Attachment) string {
	if att.IsInline {
		return "inline"
	}
	return "attachment"
}

func (c *Client) decodedAttachmentBytes(ctx context.Context, messageID, attachmentID string, part gmailAttachmentSnapshotPart) ([]byte, error) {
	switch part.representation {
	case gmailAttachmentInline:
		data, err := decodeBase64URL(c.account, part.inlineEncoded)
		if err != nil {
			return nil, err
		}
		if part.decodedKnown && uint64(len(data)) != part.decodedSize {
			zeroBytes(data)
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		if uint64(len(data)) > uint64(mail.MaxMessageAttachmentDownloadBytes) {
			zeroBytes(data)
			return nil, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), uint64(len(data)))
		}
		return data, nil
	case gmailAttachmentImplicitZero:
		return nil, nil
	case gmailAttachmentExternal:
		content, err := c.externalAttachmentContent(ctx, messageID, attachmentID, part)
		if err != nil {
			return nil, err
		}
		defer content.Content.Close()
		data, err := io.ReadAll(io.LimitReader(content.Content, mail.MaxMessageAttachmentDownloadBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > mail.MaxMessageAttachmentDownloadBytes {
			zeroBytes(data)
			return nil, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), uint64(len(data)))
		}
		if content.DeclaredSize >= 0 && int64(len(data)) != content.DeclaredSize {
			zeroBytes(data)
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		return data, nil
	default:
		return nil, providerDataError(c.account, "invalid_protocol")
	}
}
