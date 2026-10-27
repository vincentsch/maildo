package gmail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	composepkg "github.com/vincentsch/maildo/internal/compose"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
	"github.com/vincentsch/maildo/internal/provider"
	xhtml "golang.org/x/net/html"
)

type gmailPreparedForward struct {
	message                   mimecompose.Message
	selectedSourceAttachments []mail.Attachment
}

type gmailForwardAttachmentRow struct {
	row  mail.Attachment
	part gmailAttachmentSnapshotPart
}

func (p *gmailPreparedForward) Destroy() {
	if p == nil {
		return
	}
	mimecompose.DestroyMessage(&p.message)
	destroyGmailAttachmentRows(p.selectedSourceAttachments)
	p.selectedSourceAttachments = nil
}

func (c *Client) prepareForwardMessage(ctx context.Context, req provider.ForwardDraftRequest, source provider.ComposeSourceMaterial) (*gmailPreparedForward, error) {
	msg := provider.ForwardMessage(c.composeIdentity(), req, source)
	refs, err := cidReferencesFromHTML(c.account, composepkg.SanitizeHTML(string(source.HTML)))
	if err != nil {
		mimecompose.DestroyMessage(&msg)
		return nil, err
	}
	if req.Attachments.Mode == "" || req.Attachments.Mode == mail.ForwardAttachmentsNone {
		if len(refs) > 0 {
			mimecompose.DestroyMessage(&msg)
			return nil, errs.NewRequiredCIDAttachment()
		}
		return &gmailPreparedForward{message: msg}, nil
	}
	selected, related, attachments, err := c.materializeForwardAttachments(ctx, source, req.Attachments, refs)
	if err != nil {
		mimecompose.DestroyMessage(&msg)
		return nil, err
	}
	msg.Related = append(msg.Related, related...)
	msg.Attachments = append(msg.Attachments, attachments...)
	return &gmailPreparedForward{message: msg, selectedSourceAttachments: selected}, nil
}

func (c *Client) materializeForwardAttachments(ctx context.Context, source provider.ComposeSourceMaterial, selection mail.ForwardAttachmentSelection, cidRefs map[string]struct{}) ([]mail.Attachment, []mimecompose.BinaryPart, []mimecompose.BinaryPart, error) {
	msg, stats, err := c.fetchAttachmentStructure(ctx, source.SourceID, "")
	if err != nil {
		return nil, nil, nil, err
	}
	if msg.ThreadID != source.ThreadID {
		return nil, nil, nil, providerDataError(c.account, "invalid_protocol")
	}
	if err := attachmentStructureBudgetError(c.account, stats, "", ""); err != nil {
		return nil, nil, nil, err
	}
	_, state, err := c.normalizePayloadWithState(msg, true)
	if err != nil {
		return nil, nil, nil, err
	}
	snapshot, _, err := c.attachmentSnapshotFromState(msg.ID, state, "")
	if err != nil {
		return nil, nil, nil, err
	}
	explicit, err := selectForwardAttachmentRows(snapshot, selection)
	if err != nil {
		return nil, nil, nil, err
	}
	retained, err := appendCIDDependencies(c.account, snapshot, explicit, cidRefs)
	if err != nil {
		return nil, nil, nil, err
	}
	knownTotal, err := knownForwardAttachmentDecodedTotal(c.account, retained)
	if err != nil {
		return nil, nil, nil, err
	}
	selected := make([]mail.Attachment, 0, len(explicit))
	for _, item := range explicit {
		selected = append(selected, cloneGmailAttachment(item.row))
	}
	related := make([]mimecompose.BinaryPart, 0)
	attachments := make([]mimecompose.BinaryPart, 0)
	total := knownTotal
	for _, item := range retained {
		bytes, err := c.decodedAttachmentBytes(ctx, source.SourceID, item.row.ID, item.part)
		if err != nil {
			destroyBinaryParts(related)
			destroyBinaryParts(attachments)
			return nil, nil, nil, err
		}
		if !item.part.decodedKnown {
			if err := addForwardAttachmentDecodedSize(c.account, &total, uint64(len(bytes))); err != nil {
				zeroBytes(bytes)
				destroyBinaryParts(related)
				destroyBinaryParts(attachments)
				return nil, nil, nil, err
			}
		}
		part, err := forwardBinaryPart(c.account, item.row, bytes)
		if err != nil {
			zeroBytes(bytes)
			destroyBinaryParts(related)
			destroyBinaryParts(attachments)
			return nil, nil, nil, err
		}
		if item.row.IsInline {
			related = append(related, part)
		} else {
			attachments = append(attachments, part)
		}
	}
	return selected, related, attachments, nil
}

func knownForwardAttachmentDecodedTotal(account string, rows []gmailForwardAttachmentRow) (uint64, error) {
	var total uint64
	for _, item := range rows {
		if !item.part.decodedKnown {
			continue
		}
		if err := addForwardAttachmentDecodedSize(account, &total, item.part.decodedSize); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func addForwardAttachmentDecodedSize(account string, total *uint64, size uint64) error {
	limit := uint64(mail.MaxOutboundEncodedMessageBytes)
	if size > limit || *total > limit-size {
		return errs.NewMessageTooLarge(account, errs.ProviderGmail, errs.MessageSizeComposeSource, limit, limit+1)
	}
	*total += size
	return nil
}

func selectForwardAttachmentRows(snapshot *gmailAttachmentSnapshot, selection mail.ForwardAttachmentSelection) ([]gmailForwardAttachmentRow, error) {
	if snapshot == nil {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	mode := selection.Mode
	if mode == "" {
		mode = mail.ForwardAttachmentsNone
	}
	switch mode {
	case mail.ForwardAttachmentsNone:
		return nil, nil
	case mail.ForwardAttachmentsAll:
		out := make([]gmailForwardAttachmentRow, 0, len(snapshot.ordered))
		for _, row := range snapshot.ordered {
			part, ok := snapshot.attachments[row.ID]
			if !ok {
				return nil, errs.NewSharedProviderContractInvalid()
			}
			if row.Kind == mail.AttachmentKindFile {
				out = append(out, gmailForwardAttachmentRow{row: cloneGmailAttachment(row), part: part})
			}
		}
		return out, nil
	case mail.ForwardAttachmentsSelected:
		return selectNamedForwardAttachmentRows(snapshot, selection)
	default:
		return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "--include-attachments must be none or all", map[string]any{"field": "include_attachments"})
	}
}

func selectNamedForwardAttachmentRows(snapshot *gmailAttachmentSnapshot, selection mail.ForwardAttachmentSelection) ([]gmailForwardAttachmentRow, error) {
	out := make([]gmailForwardAttachmentRow, 0, len(selection.Names)+len(selection.IDs))
	seen := map[string]struct{}{}
	for _, name := range selection.Names {
		matches := make([]mail.Attachment, 0, 1)
		for _, row := range snapshot.ordered {
			if row.Name == name && row.Kind == mail.AttachmentKindFile {
				matches = append(matches, row)
			}
		}
		switch len(matches) {
		case 0:
			return nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"name": name})
		case 1:
			row := matches[0]
			part, ok := snapshot.attachments[row.ID]
			if !ok {
				return nil, errs.NewSharedProviderContractInvalid()
			}
			out = append(out, gmailForwardAttachmentRow{row: cloneGmailAttachment(row), part: part})
			seen[row.ID] = struct{}{}
		default:
			ids := make([]string, 0, len(matches))
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			return nil, errs.NewWithDetails(errs.CodeAmbiguousAttachment, fmt.Sprintf("the original has more than one attachment named %q; pick one with --include-attachment-id", name), map[string]any{"name": name, "candidate_ids": ids})
		}
	}
	for _, id := range selection.IDs {
		row, ok := attachmentRowByID(snapshot.ordered, id)
		if !ok {
			return nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"attachment_id": id})
		}
		if _, exists := seen[id]; exists {
			return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the same attachment was picked by name and by id", map[string]any{"field": "include_attachment", "attachment_id": id})
		}
		if row.Kind != mail.AttachmentKindFile {
			return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the attachment you picked is not a file attachment of the original message", map[string]any{"field": "include_attachment_id", "attachment_id": id})
		}
		part, ok := snapshot.attachments[row.ID]
		if !ok {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		out = append(out, gmailForwardAttachmentRow{row: cloneGmailAttachment(row), part: part})
		seen[row.ID] = struct{}{}
	}
	return out, nil
}

func appendCIDDependencies(account string, snapshot *gmailAttachmentSnapshot, retained []gmailForwardAttachmentRow, cidRefs map[string]struct{}) ([]gmailForwardAttachmentRow, error) {
	if len(cidRefs) == 0 {
		return retained, nil
	}
	retainedIDs := map[string]struct{}{}
	for _, row := range retained {
		retainedIDs[row.row.ID] = struct{}{}
	}
	neededIDs := map[string]struct{}{}
	for cid := range cidRefs {
		var matches []mail.Attachment
		for _, row := range snapshot.ordered {
			if row.ContentID == cid && row.Kind == mail.AttachmentKindFile {
				matches = append(matches, row)
			}
		}
		switch len(matches) {
		case 0:
			return nil, providerDataError(account, "invalid_mime")
		case 1:
			if _, ok := retainedIDs[matches[0].ID]; !ok {
				neededIDs[matches[0].ID] = struct{}{}
			}
		default:
			return nil, providerDataError(account, "invalid_mime")
		}
	}
	for _, row := range snapshot.ordered {
		if _, ok := neededIDs[row.ID]; !ok {
			continue
		}
		part, ok := snapshot.attachments[row.ID]
		if !ok {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		retained = append(retained, gmailForwardAttachmentRow{row: cloneGmailAttachment(row), part: part})
	}
	return retained, nil
}

func attachmentRowByID(rows []mail.Attachment, id string) (mail.Attachment, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return mail.Attachment{}, false
}

func forwardBinaryPart(account string, row mail.Attachment, bytes []byte) (mimecompose.BinaryPart, error) {
	contentID := ""
	if row.ContentID != "" {
		normalized, err := mail.NormalizeMessageIDScalar(row.ContentID)
		if err != nil {
			return mimecompose.BinaryPart{}, providerDataError(account, "invalid_mime")
		}
		contentID = normalized
	}
	role := mimecompose.PartRoleAttachment
	disposition := "attachment"
	if row.IsInline {
		role = mimecompose.PartRoleRelated
		disposition = "inline"
	}
	return mimecompose.BinaryPart{
		Role:        role,
		ContentType: nonemptyAttachmentContentType(row.ContentType),
		Disposition: disposition,
		ContentID:   contentID,
		Filename:    row.Name,
		Bytes:       bytes,
	}, nil
}

func nonemptyAttachmentContentType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "application/octet-stream"
	}
	return value
}

func cidReferencesFromHTML(account, htmlBody string) (map[string]struct{}, error) {
	refs := map[string]struct{}{}
	if !strings.Contains(strings.ToLower(htmlBody), "cid:") {
		return refs, nil
	}
	tokenizer := xhtml.NewTokenizer(bytes.NewReader([]byte(htmlBody)))
	for {
		switch tokenizer.Next() {
		case xhtml.ErrorToken:
			if tokenizer.Err() == nil || tokenizer.Err() == io.EOF {
				return refs, nil
			}
			return nil, providerDataError(account, "invalid_mime")
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			token := tokenizer.Token()
			for _, attr := range token.Attr {
				key := strings.ToLower(attr.Key)
				switch key {
				case "href", "src", "poster", "background":
					cid, ok, err := cidReferenceFromValue(attr.Val)
					if err != nil {
						return nil, providerDataError(account, "invalid_mime")
					}
					if ok {
						refs[cid] = struct{}{}
					}
				case "srcset":
					if err := addCIDReferencesFromSrcset(refs, attr.Val); err != nil {
						return nil, providerDataError(account, "invalid_mime")
					}
				}
			}
		}
	}
}

func addCIDReferencesFromSrcset(refs map[string]struct{}, value string) error {
	for _, candidate := range strings.Split(value, ",") {
		fields := strings.Fields(strings.TrimSpace(candidate))
		if len(fields) == 0 {
			continue
		}
		cid, ok, err := cidReferenceFromValue(fields[0])
		if err != nil {
			return err
		}
		if ok {
			refs[cid] = struct{}{}
		}
	}
	return nil
}

func cidReferenceFromValue(value string) (string, bool, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "cid:") {
		return "", false, nil
	}
	opaque := value[len("cid:"):]
	if opaque == "" {
		return "", false, mail.ErrInvalidMessageID
	}
	decoded, err := url.PathUnescape(opaque)
	if err != nil {
		return "", false, err
	}
	normalized, err := mail.NormalizeMessageIDScalar(decoded)
	if err != nil {
		return "", false, err
	}
	return strings.Trim(normalized, "<>"), true, nil
}

func destroyGmailAttachmentRows(rows []mail.Attachment) {
	for i := range rows {
		if rows[i].ExecutionToken != nil {
			rows[i].ExecutionToken.Destroy()
			rows[i].ExecutionToken = nil
		}
	}
}
