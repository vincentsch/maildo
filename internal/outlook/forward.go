package outlook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// ValidateForwardDraft backs the forward preview. Nothing is created in
// Outlook yet; it only reports the source message and the recipients.
func (c *Client) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result = mail.ForwardDraftResult{
			SourceID:         source.SourceID,
			SourceInternetID: source.InternetMessageID,
			To:               req.To,
			CC:               req.CC,
			BCC:              req.BCC,
			BodyFormat:       req.Body.Format,
		}
		return nil
	})
	return result, err
}

// CreateForwardDraft has Outlook create the forward, then sets the
// attachments, the recipients and maildo's body. For a send it then sends
// the draft.
func (c *Client) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		// Check the chosen attachments before the draft exists, so a wrong
		// name or id does not leave a draft behind in Outlook.
		selected, excluded, err := c.selectForwardSourceAttachments(ctx, source.SourceID, req.Attachments)
		if err != nil {
			return err
		}
		target := c.graphBaseURL + "/me/messages/" + url.PathEscape(source.SourceID) + "/createForward"
		status, body, err := c.graphWrite(ctx, http.MethodPost, target, nil)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			return c.messageNotFound(mail.ResolvedSelectorID)
		}
		if status != http.StatusCreated {
			return c.writeStatusError(errs.WriteOutlookCompose, status, c.composeError)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
			return c.writeResultMalformed(errs.WriteOutlookCompose)
		}
		result = mail.ForwardDraftResult{
			SourceID:                  source.SourceID,
			SourceInternetID:          source.InternetMessageID,
			DraftID:                   out.ID,
			SelectedSourceAttachments: selected,
			ExcludedUnsupported:       excluded,
			ProviderMeta:              graphDraftMeta(mail.MetadataDraftWrite),
		}
		// createForward copies all attachments of the original, and Graph
		// cannot be told to copy only some. maildo removes the ones not
		// chosen afterwards.
		copied, err := c.fetchAttachments(ctx, out.ID, func() error { return c.messagesError() })
		if err != nil {
			return err
		}
		final, err := c.reconcileForwardAttachments(ctx, out.ID, selected, copied)
		if err != nil {
			return err
		}
		result.FinalAttachments = final
		updated, err := c.UpdateDraft(ctx, mail.UpdateDraftRequest{
			DraftID: out.ID,
			Patch: mail.DraftPatch{
				To:   mail.RecipientPatch{Set: true, Values: req.To},
				CC:   mail.RecipientPatch{Set: true, Values: req.CC},
				BCC:  mail.RecipientPatch{Set: true, Values: req.BCC},
				Body: mail.BodyPatch{Set: true, Body: req.Body},
			},
		})
		if err != nil {
			return err
		}
		finalDraft, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: out.ID, MetadataOnly: true})
		if err != nil {
			return err
		}
		result.To = finalDraft.To
		result.CC = finalDraft.CC
		result.BCC = finalDraft.BCC
		result.Subject = finalDraft.Subject
		result.BodyFormat = req.Body.Format
		result.FinalAttachments = finalDraft.Attachments
		result.AttachmentCount = len(finalDraft.Attachments)
		result.WriteOutcomeFacts = updated.WriteOutcomeFacts
		if req.Action == mail.ComposeExecutionSend {
			send, err := c.SendDraft(ctx, mail.SendDraftRequest{DraftID: out.ID, Check: req.Check})
			if err != nil {
				return err
			}
			result.SentID = send.MessageID
			result.WriteOutcomeFacts = send.WriteOutcomeFacts
		}
		return nil
	})
	return result, err
}

// AddDraftAttachments uploads file attachments to a draft, in the given order.
func (c *Client) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	result := mail.AddDraftAttachmentsResult{DraftID: req.DraftID, ProviderMeta: graphDraftMeta(mail.MetadataDraftWrite)}
	for _, upload := range req.Uploads {
		payload := map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         upload.Name,
			"contentType":  upload.ContentType,
			"contentBytes": base64.StdEncoding.EncodeToString(upload.Bytes),
		}
		target := c.graphBaseURL + "/me/messages/" + url.PathEscape(req.DraftID) + "/attachments"
		status, body, err := c.graphWrite(ctx, http.MethodPost, target, payload)
		if err != nil {
			return result, err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			return result, c.draftNotFound(req.DraftID)
		}
		if status != http.StatusCreated {
			return result, c.writeStatusError(errs.WriteOutlookCompose, status, c.composeError)
		}
		var g graphAttachment
		if err := json.Unmarshal(body, &g); err != nil || g.ID == "" {
			return result, c.writeResultMalformed(errs.WriteOutlookCompose)
		}
		// Graph usually returns these fields. If it leaves some out, take them
		// from the upload, so the result still names the file.
		if g.ODataType == "" {
			g.ODataType = "#microsoft.graph.fileAttachment"
		}
		if g.Name == "" {
			g.Name = upload.Name
		}
		if g.ContentType == "" {
			g.ContentType = upload.ContentType
		}
		if g.Size == 0 {
			g.Size = upload.Size
		}
		result.Added = append(result.Added, graphAttachmentToMail(g))
	}
	final, err := c.fetchAttachments(ctx, req.DraftID, func() error { return c.messagesError() })
	if err != nil {
		return result, err
	}
	result.FinalAttachments = final
	return result, nil
}

// RemoveDraftAttachment deletes one attachment from a draft.
func (c *Client) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	removed := mail.Attachment{ID: req.AttachmentID}
	// Read the attachment list before deleting, so the result can name the
	// removed attachment and not just give its id.
	current, err := c.fetchAttachments(ctx, req.DraftID, func() error { return c.messagesError() })
	if err != nil {
		if _, draftErr := c.getDraftByID(ctx, req.DraftID, "isDraft", ""); draftErr != nil {
			return mail.RemoveDraftAttachmentResult{}, draftErr
		}
		return mail.RemoveDraftAttachmentResult{}, err
	}
	if found, ok := findAttachment(current, req.AttachmentID); ok {
		removed = found
	}
	if err := c.deleteDraftAttachment(ctx, req.DraftID, req.AttachmentID); err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	final, err := c.fetchAttachments(ctx, req.DraftID, func() error { return c.messagesError() })
	if err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	return mail.RemoveDraftAttachmentResult{
		DraftID:          req.DraftID,
		Removed:          removed,
		FinalAttachments: final,
		ProviderMeta:     graphDraftMeta(mail.MetadataDraftWrite),
	}, nil
}

func findAttachment(attachments []mail.Attachment, id string) (mail.Attachment, bool) {
	for _, a := range attachments {
		if a.ID == id {
			return a, true
		}
	}
	return mail.Attachment{}, false
}

func (c *Client) selectForwardSourceAttachments(ctx context.Context, sourceID string, selection mail.ForwardAttachmentSelection) ([]mail.Attachment, []mail.AttachmentExclusion, error) {
	mode := selection.Mode
	if mode == "" {
		mode = mail.ForwardAttachmentsNone
	}
	if mode == mail.ForwardAttachmentsNone && len(selection.Names)+len(selection.IDs) == 0 {
		return nil, nil, nil
	}
	source, err := c.fetchAttachments(ctx, sourceID, func() error { return c.messagesError() })
	if err != nil {
		return nil, nil, err
	}
	switch mode {
	case mail.ForwardAttachmentsAll:
		selected := make([]mail.Attachment, 0, len(source))
		excluded := make([]mail.AttachmentExclusion, 0)
		for _, a := range source {
			if supportedForwardSourceAttachment(a) {
				selected = append(selected, a)
				continue
			}
			// Inline attachments belong to the body, and Outlook may keep them
			// in the copy. Only other attachments maildo cannot forward are
			// reported as left out.
			if !a.IsInline {
				excluded = append(excluded, mail.AttachmentExclusion{Reason: "unsupported_kind", Attachment: a})
			}
		}
		return selected, excluded, nil
	case mail.ForwardAttachmentsSelected:
		return c.selectNamedForwardSourceAttachments(source, selection)
	case mail.ForwardAttachmentsNone:
		return nil, nil, nil
	default:
		return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "--include-attachments must be none or all", map[string]any{
			"field": "include_attachments",
		})
	}
}

func (c *Client) selectNamedForwardSourceAttachments(source []mail.Attachment, selection mail.ForwardAttachmentSelection) ([]mail.Attachment, []mail.AttachmentExclusion, error) {
	selected := make([]mail.Attachment, 0, len(selection.Names)+len(selection.IDs))
	selectedIDs := map[string]struct{}{}
	for _, name := range selection.Names {
		matches := make([]mail.Attachment, 0, 1)
		for _, a := range source {
			if a.Name == name && supportedForwardSourceAttachment(a) {
				matches = append(matches, a)
			}
		}
		switch len(matches) {
		case 0:
			return nil, nil, c.attachmentNotFound(map[string]any{"name": name})
		case 1:
			selected = append(selected, matches[0])
			selectedIDs[matches[0].ID] = struct{}{}
		default:
			ids := make([]string, 0, len(matches))
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			return nil, nil, c.ambiguousAttachment(map[string]any{"name": name, "candidate_ids": ids})
		}
	}
	byID := map[string]mail.Attachment{}
	for _, a := range source {
		byID[a.ID] = a
	}
	for _, id := range selection.IDs {
		a, ok := byID[id]
		if !ok {
			return nil, nil, c.attachmentNotFound(map[string]any{"attachment_id": id})
		}
		if _, overlap := selectedIDs[id]; overlap {
			return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the same attachment was picked by name and by id", map[string]any{
				"field":         "include_attachment",
				"attachment_id": id,
			})
		}
		if !supportedForwardSourceAttachment(a) {
			return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the attachment you picked is not a regular file attachment of the original message", map[string]any{
				"field":         "include_attachment_id",
				"attachment_id": id,
				"kind":          string(a.Kind),
				"inline":        a.IsInline,
			})
		}
		selected = append(selected, a)
		selectedIDs[id] = struct{}{}
	}
	if err := c.validateForwardTupleSelection(source, selected); err != nil {
		return nil, nil, err
	}
	return selected, nil, nil
}

type attachmentTuple struct {
	name        string
	size        int64
	contentType string
	inline      bool
}

func tupleForAttachment(a mail.Attachment) attachmentTuple {
	return attachmentTuple{name: a.Name, size: a.Size, contentType: a.ContentType, inline: a.IsInline}
}

func supportedForwardSourceAttachment(a mail.Attachment) bool {
	return !a.IsInline && a.Kind == mail.AttachmentKindFile
}

func (c *Client) validateForwardTupleSelection(source, selected []mail.Attachment) error {
	total := map[attachmentTuple]int{}
	for _, a := range source {
		if supportedForwardSourceAttachment(a) {
			total[tupleForAttachment(a)]++
		}
	}
	counts := map[attachmentTuple]int{}
	for _, a := range selected {
		counts[tupleForAttachment(a)]++
	}
	for tuple, selectedCount := range counts {
		if selectedCount > 0 && selectedCount < total[tuple] {
			// Graph gives copied attachments new ids. When only some of the
			// attachments with the same name, size and type are chosen, maildo
			// cannot tell which copy belongs to which original.
			return c.ambiguousAttachment(map[string]any{
				"name":         tuple.name,
				"size":         tuple.size,
				"content_type": tuple.contentType,
			})
		}
	}
	return nil
}

func (c *Client) reconcileForwardAttachments(ctx context.Context, draftID string, selected, copied []mail.Attachment) ([]mail.Attachment, error) {
	// Match copies by name, size and type, since Graph gives them new ids.
	// Inline attachments are left alone, so the body's references to them
	// still work.
	quota := map[attachmentTuple]int{}
	for _, a := range selected {
		quota[tupleForAttachment(a)]++
	}
	available := map[attachmentTuple]int{}
	for _, a := range copied {
		if !a.IsInline && a.Kind == mail.AttachmentKindFile {
			available[tupleForAttachment(a)]++
		}
	}
	for tuple, want := range quota {
		got := available[tuple]
		if got == 0 {
			return nil, c.attachmentNotFound(map[string]any{
				"name":         tuple.name,
				"size":         tuple.size,
				"content_type": tuple.contentType,
			})
		}
		if got < want {
			return nil, c.ambiguousAttachment(map[string]any{
				"name":         tuple.name,
				"size":         tuple.size,
				"content_type": tuple.contentType,
			})
		}
	}
	remaining := make(map[attachmentTuple]int, len(quota))
	for tuple, count := range quota {
		remaining[tuple] = count
	}
	for _, a := range copied {
		if a.IsInline {
			continue
		}
		if a.Kind == mail.AttachmentKindFile {
			tuple := tupleForAttachment(a)
			if remaining[tuple] > 0 {
				remaining[tuple]--
				continue
			}
		}
		if err := c.deleteDraftAttachment(ctx, draftID, a.ID); err != nil {
			return nil, err
		}
	}
	return c.fetchAttachments(ctx, draftID, func() error { return c.messagesError() })
}

func (c *Client) deleteDraftAttachment(ctx context.Context, draftID, attachmentID string) error {
	target := c.graphBaseURL + "/me/messages/" + url.PathEscape(draftID) + "/attachments/" + url.PathEscape(attachmentID)
	status, _, err := c.graphWrite(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		if _, draftErr := c.getDraftByID(ctx, draftID, "isDraft", ""); draftErr != nil {
			return draftErr
		}
		return c.attachmentNotFound(map[string]any{"draft_id": draftID, "attachment_id": attachmentID})
	}
	return c.composeError()
}

func (c *Client) attachmentNotFound(extra map[string]any) error {
	details := map[string]any{"account": c.account, "provider": "outlook"}
	for k, v := range extra {
		details[k] = v
	}
	return errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", details)
}

func (c *Client) ambiguousAttachment(extra map[string]any) error {
	details := map[string]any{"account": c.account, "provider": "outlook"}
	for k, v := range extra {
		details[k] = v
	}
	return errs.NewWithDetails(errs.CodeAmbiguousAttachment, "attachment selection is ambiguous", details)
}
