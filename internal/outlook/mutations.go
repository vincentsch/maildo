package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

var categorySelect = "id,internetMessageId,categories"

// graphMutationMessage is the part of Graph's answer that message changes
// need. The id is required, so an incomplete answer is never reported as a
// change that happened.
type graphMutationMessage struct {
	ID                string           `json:"id"`
	InternetMessageID string           `json:"internetMessageId"`
	ParentFolderID    string           `json:"parentFolderId"`
	IsRead            bool             `json:"isRead"`
	Categories        []string         `json:"categories"`
	Flag              graphMessageFlag `json:"flag"`
}

type graphMessageFlag struct {
	FlagStatus string `json:"flagStatus"`
}

// MoveMessage moves a message with Graph's move action. The command has
// already found the target folder.
func (c *Client) MoveMessage(ctx context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error) {
	return c.moveMessageToFolder(ctx, req.MessageID, req.Folder)
}

// CopyMessage copies a message with Graph's copy action and returns the id of
// the copy.
func (c *Client) CopyMessage(ctx context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error) {
	status, body, err := c.graphMutationDo(ctx, http.MethodPost, c.graphBaseURL+"/me/messages/"+url.PathEscape(req.MessageID)+"/copy", map[string]any{"destinationId": req.Folder.ID})
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageMutationResult{}, c.mutationMessageNotFound(req.MessageID)
	}
	if status != http.StatusCreated {
		return mail.MessageMutationResult{}, c.writeStatusError(errs.WriteOutlookMessageChange, status, c.mutationError)
	}
	g, err := c.decodeWrittenMessage(body)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	target := mail.SummaryFrom(req.Folder)
	return mail.MessageMutationResult{
		MessageID:         req.MessageID,
		ResultMessageID:   g.ID,
		InternetMessageID: g.InternetMessageID,
		TargetFolder:      &target,
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// ArchiveMessage moves a message to Outlook's Archive folder, found by its
// well-known name.
func (c *Client) ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	folder, err := c.resolveWellKnownMutationFolder(ctx, mail.MutationArchive, "archive", mail.WellKnownArchive)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	return c.moveMessageToFolder(ctx, req.MessageID, folder)
}

// DeleteMessage moves a message to Deleted Items, where it can be restored.
func (c *Client) DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	folder, err := c.resolveWellKnownMutationFolder(ctx, mail.MutationDelete, "deleteditems", mail.WellKnownDeleted)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	return c.moveMessageToFolder(ctx, req.MessageID, folder)
}

// RestoreMessage moves a message to the folder the command picked.
func (c *Client) RestoreMessage(ctx context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error) {
	return c.moveMessageToFolder(ctx, req.MessageID, req.Folder)
}

// JunkMessage moves a message to Outlook's Junk Email folder, found by its
// well-known name.
func (c *Client) JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	folder, err := c.resolveWellKnownMutationFolder(ctx, mail.MutationJunk, "junkemail", mail.WellKnownJunk)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	return c.moveMessageToFolder(ctx, req.MessageID, folder)
}

// SetMessageReadState changes only Graph's isRead field.
func (c *Client) SetMessageReadState(ctx context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error) {
	status, body, err := c.graphMutationDo(ctx, http.MethodPatch, c.graphBaseURL+"/me/messages/"+url.PathEscape(req.MessageID), map[string]any{"isRead": req.IsRead})
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageMutationResult{}, c.mutationMessageNotFound(req.MessageID)
	}
	if status != http.StatusOK {
		return mail.MessageMutationResult{}, c.writeStatusError(errs.WriteOutlookMessageChange, status, c.mutationError)
	}
	g, err := c.decodeWrittenMessage(body)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	return mail.MessageMutationResult{
		MessageID:         g.ID,
		InternetMessageID: g.InternetMessageID,
		ReadState:         &mail.BoolChange{After: req.IsRead},
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// SetMessageFlag knows only flagged and notFlagged. maildo has no commands
// for due dates or completed flags.
func (c *Client) SetMessageFlag(ctx context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error) {
	statusValue := "notFlagged"
	if req.Flagged {
		statusValue = "flagged"
	}
	payload := map[string]any{"flag": map[string]any{"flagStatus": statusValue}}
	status, body, err := c.graphMutationDo(ctx, http.MethodPatch, c.graphBaseURL+"/me/messages/"+url.PathEscape(req.MessageID), payload)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageMutationResult{}, c.mutationMessageNotFound(req.MessageID)
	}
	if status != http.StatusOK {
		return mail.MessageMutationResult{}, c.writeStatusError(errs.WriteOutlookMessageChange, status, c.mutationError)
	}
	g, err := c.decodeWrittenMessage(body)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	after := req.Flagged
	if g.Flag.FlagStatus != "" {
		after = g.Flag.FlagStatus == "flagged"
	}
	return mail.MessageMutationResult{
		MessageID:         g.ID,
		InternetMessageID: g.InternetMessageID,
		FlagState:         &mail.BoolChange{After: after},
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// ListMessageCategories reads a message's categories.
func (c *Client) ListMessageCategories(ctx context.Context, req mail.MessageCategoryListRequest) (mail.MessageCategoryListResult, error) {
	values := url.Values{}
	values.Set("$select", categorySelect)
	status, body, err := c.graphMutationDo(ctx, http.MethodGet, withQuery(c.graphBaseURL+"/me/messages/"+url.PathEscape(req.MessageID), values), nil)
	if err != nil {
		return mail.MessageCategoryListResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageCategoryListResult{}, c.mutationMessageNotFound(req.MessageID)
	}
	if status != http.StatusOK {
		return mail.MessageCategoryListResult{}, c.mutationError()
	}
	g, err := c.decodeMutationMessage(body)
	if err != nil {
		return mail.MessageCategoryListResult{}, err
	}
	categories := append([]string(nil), g.Categories...)
	return mail.MessageCategoryListResult{
		MessageID:         g.ID,
		InternetMessageID: g.InternetMessageID,
		Categories:        categories,
		Count:             len(categories),
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// SetMessageCategories replaces all categories of a message.
func (c *Client) SetMessageCategories(ctx context.Context, req mail.MessageCategorySetRequest) (mail.MessageMutationResult, error) {
	payload := map[string]any{"categories": append([]string(nil), req.Categories...)}
	status, body, err := c.graphMutationDo(ctx, http.MethodPatch, c.graphBaseURL+"/me/messages/"+url.PathEscape(req.MessageID), payload)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageMutationResult{}, c.mutationMessageNotFound(req.MessageID)
	}
	if status != http.StatusOK {
		return mail.MessageMutationResult{}, c.writeStatusError(errs.WriteOutlookMessageChange, status, c.mutationError)
	}
	g, err := c.decodeWrittenMessage(body)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	after := append([]string(nil), g.Categories...)
	if after == nil {
		after = append([]string(nil), req.Categories...)
	}
	return mail.MessageMutationResult{
		MessageID:         g.ID,
		InternetMessageID: g.InternetMessageID,
		Categories:        &mail.CategoryChange{After: after, Count: len(after)},
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// moveMessageToFolder does the move for every command that moves mail.
func (c *Client) moveMessageToFolder(ctx context.Context, id string, folder mail.Folder) (mail.MessageMutationResult, error) {
	status, body, err := c.graphMutationDo(ctx, http.MethodPost, c.graphBaseURL+"/me/messages/"+url.PathEscape(id)+"/move", map[string]any{"destinationId": folder.ID})
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.MessageMutationResult{}, c.mutationMessageNotFound(id)
	}
	if status != http.StatusCreated {
		return mail.MessageMutationResult{}, c.writeStatusError(errs.WriteOutlookMessageChange, status, c.mutationError)
	}
	g, err := c.decodeWrittenMessage(body)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	target := mail.SummaryFrom(folder)
	return mail.MessageMutationResult{
		MessageID:         id,
		ResultMessageID:   g.ID,
		InternetMessageID: g.InternetMessageID,
		TargetFolder:      &target,
		ProviderMeta:      graphMutationMeta(),
	}, nil
}

// resolveWellKnownMutationFolder looks up a folder by a well-known name such
// as archive or junkemail, for the result of the change.
func (c *Client) resolveWellKnownMutationFolder(ctx context.Context, op mail.MessageMutationOperation, alias, wellKnown string) (mail.Folder, error) {
	status, body, err := c.graphMutationDo(ctx, http.MethodGet, c.graphBaseURL+"/me/mailFolders/"+url.PathEscape(alias), nil)
	if err != nil {
		return mail.Folder{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.Folder{}, c.wellKnownFolderNotFound(op, alias)
	}
	if status != http.StatusOK {
		return mail.Folder{}, c.mutationError()
	}
	var g graphFolder
	if err := json.Unmarshal(body, &g); err != nil || g.ID == "" {
		return mail.Folder{}, c.mutationError()
	}
	return mail.Folder{
		Account:       c.account,
		Provider:      "outlook",
		ID:            g.ID,
		DisplayName:   g.DisplayName,
		WellKnownName: wellKnown,
		ParentID:      g.ParentFolderID,
		TotalCount:    g.TotalItemCount,
		UnreadCount:   g.UnreadItemCount,
		ChildCount:    g.ChildFolderCount,
		ProviderMeta:  graphFolderMeta(mail.MetadataFolder),
	}, nil
}

// graphMutationDo sends a message request through graphRequest with its own
// error texts, so errors about message changes do not talk about composing.
func (c *Client) graphMutationDo(ctx context.Context, method, target string, payload any) (int, []byte, error) {
	var op errs.ProviderWriteOperation
	if method != http.MethodGet {
		op = errs.WriteOutlookMessageChange
	}
	return c.graphRequest(ctx, noRedirectHTTPClient(c.http), op, method, target, payload, "", c.mutationError, "could not prepare the message request to Outlook", "the message request to Outlook was rate limited", "mailbox mutation response exceeds the read size limit")
}

// decodeMutationMessage treats a successful answer without an id as broken.
// A change is reported as done only with an id Graph confirmed.
func (c *Client) decodeMutationMessage(body []byte) (graphMutationMessage, error) {
	var out graphMutationMessage
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return graphMutationMessage{}, c.mutationError()
	}
	return out, nil
}

// decodeWrittenMessage reads Graph's answer to a message change. An answer
// maildo cannot read does not prove the change failed, so it is unknown.
func (c *Client) decodeWrittenMessage(body []byte) (graphMutationMessage, error) {
	var out graphMutationMessage
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return graphMutationMessage{}, c.writeResultMalformed(errs.WriteOutlookMessageChange)
	}
	return out, nil
}

func (c *Client) mutationError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("the message request to Outlook for account %q failed", c.account))
}

func (c *Client) mutationMessageNotFound(id string) error {
	return errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{
		"account":       c.account,
		"provider":      "outlook",
		"selector_kind": string(mail.ResolvedSelectorID),
		"message_id":    id,
	})
}

func (c *Client) wellKnownFolderNotFound(op mail.MessageMutationOperation, alias string) error {
	return errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
		"account":         c.account,
		"provider":        "outlook",
		"operation":       string(op),
		"well_known_name": alias,
	})
}
