package outlook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

var draftMetadataSelect = readMessageMetadataSelect + ",isDraft,changeKey"
var draftBodySelect = readMessageBodySelect + ",isDraft,changeKey"

// CreateDraft saves a new draft in Outlook and returns its id.
func (c *Client) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	status, body, err := c.graphWrite(ctx, http.MethodPost, c.graphBaseURL+"/me/messages", graphOutboundMessage(req.Message))
	if err != nil {
		return mail.DraftRef{}, err
	}
	if status != http.StatusCreated {
		return mail.DraftRef{}, c.writeStatusError(errs.WriteOutlookCompose, status, c.composeError)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return mail.DraftRef{}, c.writeResultMalformed(errs.WriteOutlookCompose)
	}
	return mail.DraftRef{ID: out.ID, WriteOutcomeFacts: outlookWriteSuccessFacts(), ProviderMeta: graphDraftMeta(mail.MetadataDraftWrite)}, nil
}

// ReadDraft reads one Outlook draft by its id.
func (c *Client) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	if req.MetadataOnly {
		g, err := c.getDraftByID(ctx, req.DraftID, draftMetadataSelect, "")
		if err != nil {
			return mail.Draft{}, err
		}
		draft := c.toDraft(g, mail.MessageBody{})
		// Graph's hasAttachments flag ignores inline attachments, so a draft
		// read without the body always fetches the attachment list.
		attachments, err := c.fetchAttachments(ctx, draft.ID, func() error { return c.messagesError() })
		if err != nil {
			return mail.Draft{}, err
		}
		draft.Attachments = attachments
		if len(attachments) > 0 {
			draft.HasAttachments = true
		}
		return draft, nil
	}
	mode := req.BodyMode
	if mode == "" {
		mode = mail.BodyModeReadable
	}
	selector := mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: req.DraftID}
	notFound := func(mail.ResolvedSelectorKind) error { return c.draftNotFound(req.DraftID) }
	validateDraft := func(g graphFullMessage) error {
		return c.requireDraft(g, req.DraftID)
	}
	var g graphFullMessage
	var body mail.MessageBody
	var err error
	switch mode {
	case mail.BodyModeReadable, mail.BodyModeText:
		g, body, err = c.readTextLikeBody(ctx, selector, mode, draftBodySelect, notFound, validateDraft)
	case mail.BodyModeHTML:
		g, err = c.getDraftByID(ctx, req.DraftID, draftBodySelect, `outlook.body-content-type="html"`)
		if err == nil {
			body = mail.MessageBody{ContentType: g.Body.ContentType, Mode: mail.BodyModeHTML, Content: g.Body.Content}
		}
	case mail.BodyModeRaw:
		g, err = c.getDraftByID(ctx, req.DraftID, draftMetadataSelect, "")
		if err == nil {
			body, err = c.fetchRawBody(ctx, g.ID, func() error { return c.draftNotFound(req.DraftID) })
		}
	default:
		err = errs.NewWithDetails(errs.CodeUnsupportedBodyMode, "body mode is not supported by Outlook", map[string]any{
			"account":   c.account,
			"provider":  "outlook",
			"body_mode": string(mode),
		})
	}
	if err != nil {
		return mail.Draft{}, err
	}
	// readTextLikeBody can fall back from text to HTML, so check once more
	// that the message read is still a draft.
	if err := c.requireDraft(g, req.DraftID); err != nil {
		return mail.Draft{}, err
	}
	draft := c.toDraft(g, body)
	if draft.HasAttachments {
		attachments, err := c.fetchAttachments(ctx, draft.ID, func() error { return c.messagesError() })
		if err != nil {
			return mail.Draft{}, err
		}
		draft.Attachments = attachments
	}
	return draft, nil
}

// UpdateDraft changes an Outlook draft after checking that it is still a draft.
func (c *Client) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	if _, err := c.getDraftByID(ctx, req.DraftID, "isDraft", ""); err != nil {
		return mail.DraftRef{}, err
	}
	payload := graphDraftPatch(req.Patch)
	status, _, err := c.graphWrite(ctx, http.MethodPatch, c.graphBaseURL+"/me/messages/"+url.PathEscape(req.DraftID), payload)
	if err != nil {
		return mail.DraftRef{}, err
	}
	// Another mail program can delete or send the draft right after the check
	// above. That counts as a missing draft.
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.DraftRef{}, c.draftNotFound(req.DraftID)
	}
	if status != http.StatusOK {
		return mail.DraftRef{}, c.writeStatusError(errs.WriteOutlookCompose, status, c.composeError)
	}
	return mail.DraftRef{ID: req.DraftID, WriteOutcomeFacts: outlookWriteSuccessFacts(), ProviderMeta: graphDraftMeta(mail.MetadataDraftWrite)}, nil
}

func (c *Client) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	if _, err := c.getDraftByID(ctx, req.DraftID, "isDraft", ""); err != nil {
		return mail.DraftUpdateValidation{}, err
	}
	return mail.DraftUpdateValidation{}, nil
}

// SendMessage sends a new message without saving a draft first.
func (c *Client) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	if !outboundMessageEmpty(req.Message) {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	prepared, ok := req.Prepared.(*preparedDirectSend)
	if !ok {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	message, err := prepared.consume()
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	return c.submitDirectSend(ctx, message)
}

func outlookWriteSuccessFacts() *mail.WriteOutcomeFacts {
	return &mail.WriteOutcomeFacts{
		Outcome:             mail.WriteOutcomeSucceeded,
		WriteMayHaveLanded:  true,
		RetrySafe:           false,
		AcceptedByProvider:  true,
		ConfirmedAfterWrite: true,
		Succeeded:           true,
	}
}

func outboundMessageEmpty(m mail.OutboundMessage) bool {
	return m.From == nil &&
		len(m.To) == 0 &&
		len(m.CC) == 0 &&
		len(m.BCC) == 0 &&
		len(m.ReplyTo) == 0 &&
		m.Subject == "" &&
		m.Body == (mail.OutboundBody{})
}

// graphWrite sends a compose request (drafts, replies, forwards, draft
// attachments). Errors carry the write outcome, see graphRequest.
func (c *Client) graphWrite(ctx context.Context, method, target string, payload any) (int, []byte, error) {
	return c.graphWriteOp(ctx, errs.WriteOutlookCompose, method, target, payload)
}

func (c *Client) graphWriteOp(ctx context.Context, op errs.ProviderWriteOperation, method, target string, payload any) (int, []byte, error) {
	return c.graphRequest(ctx, noRedirectHTTPClient(c.http), op, method, target, payload, "", c.composeError, "could not prepare an Outlook compose request", "Outlook compose request was rate limited", "compose response exceeds the read size limit")
}

func (c *Client) graphDoWithMessages(ctx context.Context, method, target string, payload any, prefer string, wrap func() error, prepareMessage, rateMessage, sizeMessage string) (int, []byte, error) {
	return c.graphRequest(ctx, noRedirectHTTPClient(c.http), "", method, target, payload, prefer, wrap, prepareMessage, rateMessage, sizeMessage)
}

// graphDoWithMessagesClient serves folder changes, whose client follows
// some redirects. Running one again cannot do harm (the folder exists or
// is gone already), so it does not track the write outcome.
func (c *Client) graphDoWithMessagesClient(ctx context.Context, client *http.Client, method, target string, payload any, prefer string, wrap func() error, prepareMessage, rateMessage, sizeMessage string) (int, []byte, error) {
	return c.graphRequest(ctx, client, "", method, target, payload, prefer, wrap, prepareMessage, rateMessage, sizeMessage)
}

// graphRequest sends one Graph request and returns the status and body.
// 404 and 410 come back as a status with no error, for the caller to name
// what was missing.
//
// A write (op set) says in its error whether the change may have happened,
// the same way direct sends do. Before the request left maildo it is
// not_applied and safe to retry. Once it was sent, a lost response, a server
// error or a timeout is unknown: the change may have happened, and running
// the command again could repeat it, which for a send means a second copy
// of the mail.
func (c *Client) graphRequest(ctx context.Context, client *http.Client, op errs.ProviderWriteOperation, method, target string, payload any, prefer string, wrap func() error, prepareMessage, rateMessage, sizeMessage string) (int, []byte, error) {
	var attempt errs.ProviderWriteAttempt
	if op != "" {
		var err error
		if attempt, err = errs.NewProviderWriteAttempt(c.account, errs.ProviderOutlook, op); err != nil {
			return 0, nil, err
		}
	}
	notSent := func(err error) error {
		if attempt == nil {
			return err
		}
		return outlookWriteNotApplied(c.account, attempt, err)
	}
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, notSent(err)
	}
	// Check the URL before the request carries the token, so the token never
	// goes to a host other than Graph.
	if !c.validGraphURL(target) {
		return 0, nil, notSent(wrap())
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, notSent(wrap())
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, notSent(rungrad.NewError(rungrad.ExitAPI, prepareMessage))
	}
	req.Header.Set("Authorization", authHeader)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	sent := func() bool { return false }
	if attempt != nil {
		req, sent = trackTransmission(req, client, attempt)
	}
	unknown := func(reason errs.OutcomeUnknownReason) error {
		if !sent() {
			_ = errs.CrossProviderWriteTransmissionBoundary(attempt)
		}
		return errs.NewOutcomeUnknown(attempt, reason)
	}
	resp, err := client.Do(req)
	if err != nil {
		if attempt != nil && sent() {
			return 0, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
		}
		return 0, nil, notSent(wrap())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, nil, repairRequired(c.account)
	}
	// Read one byte past the limit, so a response that is too large is
	// noticed without reading all of it.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		if attempt != nil {
			return resp.StatusCode, nil, unknown(errs.OutcomeProviderResultMalformed)
		}
		return resp.StatusCode, nil, wrap()
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, nil, rungrad.NewError(rungrad.ExitRateLimited, rateMessage)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
		if attempt != nil {
			return resp.StatusCode, nil, unknown(errs.OutcomeAcceptanceResponseLost)
		}
		return resp.StatusCode, nil, wrap()
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return resp.StatusCode, nil, nil
	}
	if len(respBody) > maxBodyBytes {
		if attempt != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, nil, unknown(errs.OutcomeProviderResultMalformed)
		}
		return resp.StatusCode, nil, rungrad.NewError(rungrad.ExitAPI, sizeMessage)
	}
	return resp.StatusCode, respBody, nil
}

// writeResultMalformed reports a write whose success answer maildo cannot
// read. The write may have happened, so the outcome is unknown and running
// the command again is not safe.
func (c *Client) writeResultMalformed(op errs.ProviderWriteOperation) error {
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderOutlook, op)
	if err != nil {
		return err
	}
	_ = errs.CrossProviderWriteTransmissionBoundary(attempt)
	return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
}

// writeStatusError is the error for a write answered with a status maildo
// did not expect. Another success status means the write may have happened.
func (c *Client) writeStatusError(op errs.ProviderWriteOperation, status int, fallback func() error) error {
	if status >= 200 && status < 300 {
		return c.writeResultMalformed(op)
	}
	return fallback()
}

// outlookWriteNotApplied marks an error as a write that never reached
// Outlook, so it is safe to run the command again.
func outlookWriteNotApplied(account string, attempt errs.ProviderWriteAttempt, cause error) error {
	var product *errs.Error
	if !errors.As(cause, &product) || product == nil {
		product = errs.NewWithDetails(errs.CodeProviderConnectionError, cause.Error(), map[string]any{"account": account, "provider": "outlook"})
	}
	if out := errs.NewWriteNotApplied(attempt, product); out != nil {
		return out
	}
	return cause
}

// getDraftByID reads the given fields of a draft. A missing message and a
// message that is not a draft both count as draft_not_found.
func (c *Client) getDraftByID(ctx context.Context, id, selectFields, prefer string) (graphFullMessage, error) {
	values := url.Values{}
	values.Set("$select", selectFields)
	target := withQuery(c.graphBaseURL+"/me/messages/"+url.PathEscape(id), values)
	status, body, err := c.graphReadRequest(ctx, graphReadDraft, target, prefer)
	if err != nil {
		return graphFullMessage{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return graphFullMessage{}, c.draftNotFound(id)
	}
	if status < 200 || status >= 300 {
		return graphFullMessage{}, c.composeError()
	}
	var out graphFullMessage
	if err := json.Unmarshal(body, &out); err != nil {
		return graphFullMessage{}, c.composeError()
	}
	if err := c.requireDraft(out, id); err != nil {
		return graphFullMessage{}, err
	}
	return out, nil
}

func (c *Client) requireDraft(g graphFullMessage, id string) error {
	if !g.IsDraft {
		return c.draftNotFound(id)
	}
	return nil
}

// graphOutboundMessage turns a message into Graph's format. It leaves out
// from and sender on purpose: --from is only checked against the account,
// it does not let you send as someone else.
func graphOutboundMessage(m mail.OutboundMessage) map[string]any {
	out := map[string]any{}
	if m.Subject != "" {
		out["subject"] = m.Subject
	}
	if m.Body.Present {
		out["body"] = graphBodyPayload(m.Body)
	}
	if len(m.To) > 0 {
		out["toRecipients"] = graphRecipientsPayload(m.To)
	}
	if len(m.CC) > 0 {
		out["ccRecipients"] = graphRecipientsPayload(m.CC)
	}
	if len(m.BCC) > 0 {
		out["bccRecipients"] = graphRecipientsPayload(m.BCC)
	}
	if len(m.ReplyTo) > 0 {
		out["replyTo"] = graphRecipientsPayload(m.ReplyTo)
	}
	if len(m.Attachments) > 0 {
		attachments := make([]any, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			attachments = append(attachments, map[string]any{
				"@odata.type":  "#microsoft.graph.fileAttachment",
				"name":         a.Name,
				"contentType":  a.ContentType,
				"contentBytes": base64.StdEncoding.EncodeToString(a.Bytes),
			})
		}
		out["attachments"] = attachments
	}
	return out
}

// graphDraftPatch contains only the fields that change. An empty body is
// sent as an empty text body, so clearing the body works the same for every
// body format.
func graphDraftPatch(p mail.DraftPatch) map[string]any {
	out := map[string]any{}
	addRecipients := func(name string, patch mail.RecipientPatch) {
		switch {
		case patch.Clear:
			out[name] = []any{}
		case patch.Set:
			out[name] = graphRecipientsPayload(patch.Values)
		}
	}
	addRecipients("toRecipients", p.To)
	addRecipients("ccRecipients", p.CC)
	addRecipients("bccRecipients", p.BCC)
	addRecipients("replyTo", p.ReplyTo)
	if p.Subject.Set {
		out["subject"] = p.Subject.Value
	}
	if p.Body.Set {
		if !p.Body.Body.Present {
			out["body"] = map[string]any{
				"contentType": "text",
				"content":     "",
			}
		} else {
			out["body"] = graphBodyPayload(p.Body.Body)
		}
	}
	return out
}

func graphBodyPayload(body mail.OutboundBody) map[string]any {
	content := body.Text
	if body.ContentType != "text" {
		content = body.HTML
	}
	return map[string]any{
		"contentType": body.ContentType,
		"content":     content,
	}
}

func graphRecipientsPayload(in []mail.Recipient) []any {
	out := make([]any, 0, len(in))
	for _, r := range in {
		email := map[string]any{"address": r.Address}
		if r.Name != "" {
			email["name"] = r.Name
		}
		out = append(out, map[string]any{"emailAddress": email})
	}
	return out
}

func (c *Client) toDraft(g graphFullMessage, body mail.MessageBody) mail.Draft {
	d := mail.Draft{
		Account:           c.account,
		Provider:          "outlook",
		ID:                g.ID,
		IsDraft:           g.IsDraft,
		InternetMessageID: g.InternetMessageID,
		ThreadID:          g.ConversationID,
		Folder:            mail.FolderSummary{ID: g.ParentFolderID},
		From:              recipient(g.From),
		ReplyTo:           recipients(g.ReplyTo),
		To:                recipients(g.ToRecipients),
		CC:                recipients(g.CcRecipients),
		BCC:               recipients(g.BccRecipients),
		Subject:           g.Subject,
		HasAttachments:    g.HasAttachments,
		Body:              body,
		ProviderMeta:      graphDraftMeta(mail.MetadataDraft),
	}
	if t, ok := parseGraphTime(g.CreatedDateTime); ok {
		d.CreatedAt = &t
	}
	if t, ok := parseGraphTime(g.LastModifiedDateTime); ok {
		d.LastModifiedAt = &t
	}
	return d
}

func (c *Client) draftNotFound(id string) error {
	details := map[string]any{
		"account":  c.account,
		"provider": "outlook",
	}
	if id != "" {
		details["draft_id"] = id
	}
	return errs.NewWithDetails(errs.CodeDraftNotFound, "draft not found", details)
}

func (c *Client) composeError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("the request to Outlook for account %q failed", c.account))
}
