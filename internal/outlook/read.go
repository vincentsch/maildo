package outlook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

const maxBodyBytes = 1 << 20

// readMessageMetadataSelect leaves out the body on purpose. A raw read first
// reads these fields and then the raw message, so a large body cannot make
// the first read fail.
var readMessageMetadataSelect = strings.Join([]string{
	"id", "internetMessageId", "conversationId", "parentFolderId", "subject",
	"from", "sender", "replyTo", "toRecipients", "ccRecipients", "bccRecipients",
	"receivedDateTime", "sentDateTime", "createdDateTime", "lastModifiedDateTime",
	"isRead", "importance", "categories", "hasAttachments", "bodyPreview",
	"internetMessageHeaders",
}, ",")

// readMessageBodySelect lists the fields for reading a message with its body
// (every body mode except raw). Attachments are listed in a separate request,
// without their content.
var readMessageBodySelect = strings.Join([]string{
	"id", "internetMessageId", "conversationId", "parentFolderId", "subject",
	"from", "sender", "replyTo", "toRecipients", "ccRecipients", "bccRecipients",
	"receivedDateTime", "sentDateTime", "createdDateTime", "lastModifiedDateTime",
	"isRead", "importance", "categories", "hasAttachments", "bodyPreview", "body",
	"internetMessageHeaders",
}, ",")

// threadMessageSelect leaves out the body, so one large message cannot make
// a thread view fail.
var threadMessageSelect = strings.Join([]string{
	"id", "internetMessageId", "conversationId", "parentFolderId", "subject",
	"from", "receivedDateTime", "sentDateTime", "isRead", "importance",
	"hasAttachments", "bodyPreview",
}, ",")

// readThreadSourceSelect reads only what is needed to find a message's
// conversation id.
var readThreadSourceSelect = strings.Join([]string{"id", "conversationId"}, ",")

var readMessageAttachmentSourceSelect = strings.Join([]string{"id", "internetMessageId"}, ",")

type graphHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphFullMessage struct {
	ID                     string           `json:"id"`
	InternetMessageID      string           `json:"internetMessageId"`
	ConversationID         string           `json:"conversationId"`
	ParentFolderID         string           `json:"parentFolderId"`
	Subject                string           `json:"subject"`
	From                   *graphRecipient  `json:"from"`
	Sender                 *graphRecipient  `json:"sender"`
	ReplyTo                []graphRecipient `json:"replyTo"`
	ToRecipients           []graphRecipient `json:"toRecipients"`
	CcRecipients           []graphRecipient `json:"ccRecipients"`
	BccRecipients          []graphRecipient `json:"bccRecipients"`
	ReceivedDateTime       string           `json:"receivedDateTime"`
	SentDateTime           string           `json:"sentDateTime"`
	CreatedDateTime        string           `json:"createdDateTime"`
	LastModifiedDateTime   string           `json:"lastModifiedDateTime"`
	IsRead                 bool             `json:"isRead"`
	Importance             string           `json:"importance"`
	Categories             []string         `json:"categories"`
	HasAttachments         bool             `json:"hasAttachments"`
	IsDraft                bool             `json:"isDraft"`
	ChangeKey              string           `json:"changeKey"`
	BodyPreview            string           `json:"bodyPreview"`
	Body                   graphBody        `json:"body"`
	InternetMessageHeaders []graphHeader    `json:"internetMessageHeaders"`
}

type graphFullMessagePage struct {
	Value    []graphFullMessage `json:"value"`
	NextLink string             `json:"@odata.nextLink"`
}

type graphAttachment struct {
	ODataType   string `json:"@odata.type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"isInline"`
	ContentID   string `json:"contentId"`
}

type graphAttachmentPage struct {
	Value    []graphAttachment `json:"value"`
	NextLink string            `json:"@odata.nextLink"`
}

// ReadMessage reads one message with its body in the requested body mode.
// When the message has attachments, it adds the attachment list.
func (c *Client) ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	if req.MetadataOnly {
		g, err := c.fetchFullMessage(ctx, req.Selector, readMessageAttachmentSourceSelect, "", c.messageNotFound)
		if err != nil {
			return mail.Message{}, err
		}
		return c.toMessage(g, mail.MessageBody{}), nil
	}
	mode := req.BodyMode
	if mode == "" {
		mode = mail.BodyModeReadable
	}
	var g graphFullMessage
	var body mail.MessageBody
	var err error
	switch mode {
	case mail.BodyModeReadable, mail.BodyModeText:
		g, body, err = c.readTextLikeBody(ctx, req.Selector, mode, readMessageBodySelect, c.messageNotFound, nil)
	case mail.BodyModeHTML:
		g, err = c.fetchFullMessage(ctx, req.Selector, readMessageBodySelect, `outlook.body-content-type="html"`, c.messageNotFound)
		if err == nil {
			body = mail.MessageBody{ContentType: g.Body.ContentType, Mode: mail.BodyModeHTML, Content: g.Body.Content}
		}
	case mail.BodyModeRaw:
		// Find the message first and then ask for $value by its id. This way
		// a missing or ambiguous message gives the usual errors.
		g, err = c.fetchFullMessage(ctx, req.Selector, readMessageMetadataSelect, "", c.messageNotFound)
		if err == nil {
			body, err = c.fetchRawBody(ctx, g.ID, func() error {
				return c.messageNotFound(mail.ResolvedSelectorID)
			})
		}
	default:
		err = errs.NewWithDetails(errs.CodeUnsupportedBodyMode, "body mode is not supported by Outlook", map[string]any{
			"account":   c.account,
			"provider":  "outlook",
			"body_mode": string(mode),
		})
	}
	if err != nil {
		return mail.Message{}, err
	}
	msg := c.toMessage(g, body)
	if msg.HasAttachments {
		attachments, err := c.fetchAttachments(ctx, msg.ID, func() error { return c.messagesError() })
		if err != nil {
			return mail.Message{}, err
		}
		msg.Attachments = attachments
	}
	return msg, nil
}

// ReadThread finds the message's Outlook conversation and returns all its
// messages, oldest first, without bodies.
func (c *Client) ReadThread(ctx context.Context, req mail.ReadThreadRequest) (mail.Thread, error) {
	conv, err := c.resolveConversationID(ctx, req.Selector)
	if err != nil {
		return mail.Thread{}, err
	}
	rows, err := c.fetchConversationMessages(ctx, conv)
	if err != nil {
		return mail.Thread{}, err
	}
	if len(rows) == 0 {
		return mail.Thread{}, errs.NewWithDetails(errs.CodeThreadNotFound, "Outlook found no messages in this thread", map[string]any{
			"account":   c.account,
			"provider":  "outlook",
			"thread_id": conv,
		})
	}
	messages := make([]mail.ThreadMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, c.toThreadMessage(row))
	}
	sort.SliceStable(messages, func(i, j int) bool {
		ti, tj := outlookThreadTime(messages[i]), outlookThreadTime(messages[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return messages[i].ID < messages[j].ID
	})
	return mail.Thread{Account: c.account, Provider: "outlook", ThreadID: conv, Messages: messages}, nil
}

// readTextLikeBody asks Graph for the text body first, since many messages
// have a good one. If that is empty or not text, it reads the message again
// as HTML and turns the HTML into readable text itself.
func (c *Client) readTextLikeBody(ctx context.Context, selector mail.ResolvedMessageSelector, mode mail.BodyMode, selectFields string, notFound func(mail.ResolvedSelectorKind) error, validateFirst func(graphFullMessage) error) (graphFullMessage, mail.MessageBody, error) {
	textMsg, err := c.fetchFullMessage(ctx, selector, selectFields, `outlook.body-content-type="text"`, notFound)
	if err != nil {
		return graphFullMessage{}, mail.MessageBody{}, err
	}
	if validateFirst != nil {
		if err := validateFirst(textMsg); err != nil {
			return graphFullMessage{}, mail.MessageBody{}, err
		}
	}
	if strings.EqualFold(textMsg.Body.ContentType, "text") && strings.TrimSpace(textMsg.Body.Content) != "" {
		return textMsg, mail.MessageBody{ContentType: "text", Mode: mode, Content: textMsg.Body.Content}, nil
	}
	// Read it again by its id, so a Message-ID that matches two messages
	// cannot switch messages between the two requests.
	htmlSelector := mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: textMsg.ID}
	htmlMsg, err := c.fetchFullMessage(ctx, htmlSelector, selectFields, `outlook.body-content-type="html"`, notFound)
	if err != nil {
		return graphFullMessage{}, mail.MessageBody{}, err
	}
	contentType := htmlMsg.Body.ContentType
	content := htmltext.ToReadable(htmlMsg.Body.Content)
	if strings.TrimSpace(htmlMsg.Body.Content) == "" {
		contentType = textMsg.Body.ContentType
	}
	return htmlMsg, mail.MessageBody{ContentType: contentType, Mode: mode, Content: content}, nil
}

// fetchRawBody reads the raw MIME message from Outlook's $value endpoint.
// UTF-8 comes back as text. Other bytes are base64-encoded, so JSON and the
// terminal still get valid text and the body mode stays raw.
func (c *Client) fetchRawBody(ctx context.Context, id string, notFound func() error) (mail.MessageBody, error) {
	target := c.graphBaseURL + "/me/messages/" + url.PathEscape(id) + "/$value"
	status, body, err := c.graphReadRequest(ctx, graphReadMessage, target, "")
	if err != nil {
		return mail.MessageBody{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		if notFound != nil {
			return mail.MessageBody{}, notFound()
		}
		return mail.MessageBody{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	if status >= 400 && status < 500 {
		return mail.MessageBody{}, errs.NewWithDetails(errs.CodeUnsupportedBodyMode, "Outlook cannot show this message in raw form; try another --body-mode", map[string]any{
			"account":   c.account,
			"provider":  "outlook",
			"body_mode": string(mail.BodyModeRaw),
		})
	}
	if status < 200 || status >= 300 {
		return mail.MessageBody{}, c.messagesError()
	}
	encoding := "utf8"
	content := string(body)
	if !utf8.Valid(body) {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(body)
	}
	return mail.MessageBody{ContentType: "message/rfc822", Mode: mail.BodyModeRaw, Encoding: encoding, Content: content}, nil
}

// fetchFullMessage reads one message, picked by id or by Message-ID, with
// the given fields and Prefer header.
func (c *Client) fetchFullMessage(ctx context.Context, selector mail.ResolvedMessageSelector, selectFields, prefer string, notFound func(mail.ResolvedSelectorKind) error) (graphFullMessage, error) {
	if notFound == nil {
		notFound = c.messageNotFound
	}
	switch selector.Kind {
	case mail.ResolvedSelectorID:
		values := url.Values{}
		values.Set("$select", selectFields)
		target := withQuery(c.graphBaseURL+"/me/messages/"+url.PathEscape(selector.ID), values)
		status, body, err := c.graphReadRequest(ctx, graphReadMessage, target, prefer)
		if err != nil {
			return graphFullMessage{}, err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			return graphFullMessage{}, notFound(selector.Kind)
		}
		if status < 200 || status >= 300 {
			return graphFullMessage{}, c.messagesError()
		}
		var out graphFullMessage
		if err := json.Unmarshal(body, &out); err != nil {
			return graphFullMessage{}, c.messagesError()
		}
		return out, nil
	case mail.ResolvedSelectorInternetID:
		values := url.Values{}
		values.Set("$select", selectFields)
		values.Set("$filter", "internetMessageId eq "+c.odataLiteral(selector.InternetID))
		values.Set("$top", "2")
		// Ask for two rows. Two rows or a next link mean the Message-ID is not
		// unique, which can happen in a mailbox.
		target := withQuery(c.graphBaseURL+"/me/messages", values)
		status, body, err := c.graphReadRequest(ctx, graphReadMessage, target, prefer)
		if err != nil {
			return graphFullMessage{}, err
		}
		if status < 200 || status >= 300 {
			return graphFullMessage{}, c.messagesError()
		}
		var page graphFullMessagePage
		if err := json.Unmarshal(body, &page); err != nil {
			return graphFullMessage{}, c.messagesError()
		}
		if len(page.Value) == 0 {
			return graphFullMessage{}, notFound(selector.Kind)
		}
		if len(page.Value) > 1 || page.NextLink != "" {
			return graphFullMessage{}, c.ambiguousInternetID(selector.InternetID, page.Value)
		}
		return page.Value[0], nil
	default:
		return graphFullMessage{}, errs.New(errs.CodeInvalidSelector, "reading a message needs --id or --internet-id")
	}
}

// resolveConversationID takes a conversation id as it is, or finds the
// conversation of the given message.
func (c *Client) resolveConversationID(ctx context.Context, selector mail.ResolvedMessageSelector) (string, error) {
	switch selector.Kind {
	case mail.ResolvedSelectorThreadID:
		return selector.ThreadID, nil
	case mail.ResolvedSelectorID, mail.ResolvedSelectorInternetID:
		msg, err := c.fetchFullMessage(ctx, selector, readThreadSourceSelect, "", c.messageNotFound)
		if err != nil {
			return "", err
		}
		return msg.ConversationID, nil
	default:
		return "", errs.New(errs.CodeInvalidSelector, "reading a thread needs --id, --internet-id or --thread-id")
	}
}

// fetchConversationMessages reads every page of the conversation. A thread
// view shows all messages; it has no --limit like list and search.
func (c *Client) fetchConversationMessages(ctx context.Context, conv string) ([]graphMessage, error) {
	values := url.Values{}
	values.Set("$select", threadMessageSelect)
	values.Set("$filter", "conversationId eq "+c.odataLiteral(conv))
	values.Set("$top", fmt.Sprintf("%d", messagePageSize))
	next := withQuery(c.graphBaseURL+"/me/messages", values)
	out := make([]graphMessage, 0)
	var pages graphPageGuard
	for next != "" {
		if err := pages.visit(ctx, c, next); err != nil {
			return nil, err
		}
		status, body, err := c.graphReadRequest(ctx, graphReadMessage, next, "")
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, c.messagesError()
		}
		var page graphMessagePage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, c.messagesError()
		}
		out = append(out, page.Value...)
		next = page.NextLink
	}
	return out, nil
}

// fetchAttachments lists a message's attachments, without their content.
func (c *Client) fetchAttachments(ctx context.Context, id string, notFound func() error) ([]mail.Attachment, error) {
	values := url.Values{}
	values.Set("$select", "id,name,contentType,size,isInline,contentId")
	next := withQuery(c.graphBaseURL+"/me/messages/"+url.PathEscape(id)+"/attachments", values)
	out := make([]mail.Attachment, 0)
	var pages graphPageGuard
	for next != "" {
		if err := pages.visit(ctx, c, next); err != nil {
			return nil, err
		}
		status, body, err := c.graphReadRequest(ctx, graphReadMessage, next, "")
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			if notFound != nil {
				return nil, notFound()
			}
			return nil, c.messagesError()
		}
		if status < 200 || status >= 300 {
			return nil, c.messagesError()
		}
		var page graphAttachmentPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, c.messagesError()
		}
		for _, a := range page.Value {
			out = append(out, graphAttachmentToMail(a))
		}
		next = page.NextLink
	}
	return out, nil
}

func (c *Client) ListMessageAttachments(ctx context.Context, req mail.MessageAttachmentListRequest) (mail.MessageAttachmentList, error) {
	// Here a missing attachment list means the message is missing. Draft and
	// read pass their own callback and get the general error instead.
	attachments, err := c.fetchAttachments(ctx, req.MessageID, func() error {
		return c.messageNotFound(mail.ResolvedSelectorID)
	})
	if err != nil {
		return mail.MessageAttachmentList{}, err
	}
	return mail.MessageAttachmentList{
		MessageID:    req.MessageID,
		Attachments:  attachments,
		ProviderMeta: graphAttachmentMeta(mail.MetadataAttachmentList),
	}, nil
}

// DownloadMessageAttachment streams an attachment from Graph's $value
// endpoint. It looks at the HTTP status before the size, so a large error
// answer cannot hide a login problem or a missing attachment.
func (c *Client) DownloadMessageAttachment(ctx context.Context, req mail.MessageAttachmentDownloadRequest) (mail.MessageAttachmentContent, error) {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	target := c.graphBaseURL + "/me/messages/" + url.PathEscape(req.MessageID) + "/attachments/" + url.PathEscape(req.AttachmentID) + "/$value"
	if !c.validGraphURL(target) {
		return mail.MessageAttachmentContent{}, c.messagesError()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return mail.MessageAttachmentContent{}, rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook attachment request")
	}
	httpReq.Header.Set("Authorization", authHeader)
	resp, err := c.doGraphRequestNoRedirect(httpReq)
	if err != nil {
		return mail.MessageAttachmentContent{}, c.messagesError()
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return mail.MessageAttachmentContent{}, repairRequired(c.account)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		resp.Body.Close()
		return mail.MessageAttachmentContent{}, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{
			"account":       c.account,
			"provider":      "outlook",
			"message_id":    req.MessageID,
			"attachment_id": req.AttachmentID,
		})
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return mail.MessageAttachmentContent{}, rungrad.NewError(rungrad.ExitRateLimited, "Outlook attachment request was rate limited")
	}
	if resp.StatusCode >= 500 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return mail.MessageAttachmentContent{}, c.messagesError()
	}
	declared := int64(0)
	if resp.ContentLength >= 0 {
		declared = resp.ContentLength
	}
	return mail.MessageAttachmentContent{
		AttachmentID: req.AttachmentID,
		Content:      resp.Body,
		ContentType:  resp.Header.Get("Content-Type"),
		DeclaredSize: declared,
	}, nil
}

func graphAttachmentToMail(a graphAttachment) mail.Attachment {
	kind, providerType := graphAttachmentKind(a.ODataType)
	return mail.Attachment{
		ID:           a.ID,
		Name:         a.Name,
		ContentType:  a.ContentType,
		Size:         a.Size,
		IsInline:     a.IsInline,
		ContentID:    a.ContentID,
		Kind:         kind,
		ProviderType: providerType,
	}
}

func graphAttachmentKind(raw string) (mail.AttachmentKind, string) {
	switch strings.TrimSpace(raw) {
	case "":
		return "", ""
	case "#microsoft.graph.fileAttachment":
		return mail.AttachmentKindFile, raw
	case "#microsoft.graph.itemAttachment":
		return mail.AttachmentKindItem, raw
	case "#microsoft.graph.referenceAttachment":
		return mail.AttachmentKindReference, raw
	default:
		return mail.AttachmentKindUnknown, raw
	}
}

// graphReadSurface says what kind of read graphReadRequest does. It picks
// the error texts, and for rules the typed read failure.
type graphReadSurface uint8

const (
	graphReadMessage graphReadSurface = iota
	graphReadDraft
	graphReadRule
	graphReadArchivePolicy
	// graphReadDraftSend reads a draft with all its attachments before a
	// send, so it may be larger than other reads.
	graphReadDraftSend
)

// graphReadErrors holds the fixed errors for one kind of read. None of them
// contains what Graph sent.
type graphReadErrors struct {
	broad   error
	prepare error
	rate    error
	size    error
}

// graphReadRequest sends the GET requests for messages, drafts, rules and
// the archive settings. It checks the URL before the token is added and
// follows no redirects. It looks at the status before reading the body, and
// reads the body only for a 200, up to a size limit.
func (c *Client) graphReadRequest(ctx context.Context, surface graphReadSurface, target, prefer string) (int, []byte, error) {
	errsForSurface, ok := c.graphReadErrors(surface)
	if !ok {
		return 0, nil, rungrad.NewError(rungrad.ExitAPI, "invalid Outlook Graph read surface")
	}
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, graphReadFailure(surface, provider.RuleReadFailureTransport, errsForSurface.broad)
	}
	validTarget := c.validGraphURL(target)
	// Rule and archive URLs can end in an id that is "." or "..", which maildo
	// encodes itself. They still must point to Graph.
	if surface == graphReadRule || surface == graphReadArchivePolicy {
		validTarget = c.validGraphRuleRequestURL(target)
	}
	if !validTarget {
		return 0, nil, graphReadFailure(surface, provider.RuleReadFailureProviderAPI, errsForSurface.broad)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, graphReadFailure(surface, provider.RuleReadFailureProviderAPI, errsForSurface.prepare)
	}
	req.Header.Set("Authorization", authHeader)
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	resp, dispatchErr := c.doGraphRequestNoRedirect(req)
	// A transport can return a response and an error together. Whenever there
	// is a response, its body is closed here, even if it is not read.
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if dispatchErr != nil || resp == nil || resp.Body == nil {
		return 0, nil, graphReadFailure(surface, provider.RuleReadFailureTransport, errsForSurface.broad)
	}
	status := resp.StatusCode
	// Error statuses are sorted without reading the body. The body is not
	// needed for that and may contain data from Graph.
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureAuthRepairRequired, repairRequired(c.account))
	case status == http.StatusTooManyRequests:
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureRateLimited, errsForSurface.rate)
	case status >= 300 && status < 400:
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureProviderAPI, errsForSurface.broad)
	case status >= 400 && status < 500:
		// Other 4xx statuses go back to the caller. A raw read turns them into
		// an unsupported body mode; rule and archive reads look for missing data.
		return status, nil, nil
	case status >= 500:
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureProviderAPI, errsForSurface.broad)
	case status != http.StatusOK:
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureProviderAPI, errsForSurface.broad)
	}
	limit := maxBodyBytes
	if surface == graphReadDraftSend {
		limit = maxDraftSendResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureTransport, errsForSurface.broad)
	}
	if len(body) > limit {
		return status, nil, graphReadFailure(surface, provider.RuleReadFailureOversizedResponse, errsForSurface.size)
	}
	return status, body, nil
}

// graphReadErrors returns fixed error texts, never text from Graph. An
// unknown kind of read is refused before any URL or token is touched.
func (c *Client) graphReadErrors(surface graphReadSurface) (graphReadErrors, bool) {
	switch surface {
	case graphReadMessage, graphReadDraft:
		return graphReadErrors{
			broad:   c.messagesError(),
			prepare: rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook message request"),
			rate:    rungrad.NewError(rungrad.ExitRateLimited, "Outlook message request was rate limited"),
			size:    rungrad.NewError(rungrad.ExitAPI, "message body exceeds the read size limit"),
		}, true
	case graphReadDraftSend:
		return graphReadErrors{
			broad:   c.messagesError(),
			prepare: rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook message request"),
			rate:    rungrad.NewError(rungrad.ExitRateLimited, "Outlook message request was rate limited"),
			size:    outlookAttachmentsTooLarge(c.account),
		}, true
	case graphReadRule:
		return graphReadErrors{
			broad:   c.rulesError(),
			prepare: rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook rule request"),
			rate:    rungrad.NewError(rungrad.ExitRateLimited, "Outlook rule request was rate limited"),
			size:    rungrad.NewError(rungrad.ExitAPI, "Outlook rule response exceeds the read size limit"),
		}, true
	case graphReadArchivePolicy:
		return graphReadErrors{
			broad:   c.archivePolicyError(),
			prepare: rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook archive-policy request"),
			rate:    rungrad.NewError(rungrad.ExitRateLimited, "Outlook archive-policy request was rate limited"),
			size:    rungrad.NewError(rungrad.ExitAPI, "Outlook archive-policy response exceeds the read size limit"),
		}, true
	default:
		return graphReadErrors{}, false
	}
}

// graphReadFailure wraps rule read errors with their reason, which the rule
// checks need. Other reads return the plain error.
func graphReadFailure(surface graphReadSurface, reason provider.RuleReadFailureReason, outward error) error {
	if surface == graphReadRule {
		return provider.NewRuleReadFailureError(reason, outward)
	}
	return outward
}

func (c *Client) odataLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (c *Client) toMessage(g graphFullMessage, body mail.MessageBody) mail.Message {
	msg := mail.Message{
		Account:           c.account,
		Provider:          "outlook",
		ID:                g.ID,
		InternetMessageID: g.InternetMessageID,
		ThreadID:          g.ConversationID,
		Folder:            mail.FolderSummary{ID: g.ParentFolderID},
		From:              recipient(g.From),
		ReplyTo:           recipients(g.ReplyTo),
		To:                recipients(g.ToRecipients),
		CC:                recipients(g.CcRecipients),
		BCC:               recipients(g.BccRecipients),
		Subject:           g.Subject,
		Headers:           headers(g.InternetMessageHeaders),
		IsRead:            g.IsRead,
		Importance:        g.Importance,
		Categories:        append([]string(nil), g.Categories...),
		HasAttachments:    g.HasAttachments,
		Body:              body,
		ProviderMeta:      graphMessageMeta(),
	}
	if sender := recipient(g.Sender); sender.Name != "" || sender.Address != "" {
		msg.Sender = &sender
	}
	if t, ok := parseGraphTime(g.ReceivedDateTime); ok {
		msg.ReceivedAt = &t
	}
	if t, ok := parseGraphTime(g.SentDateTime); ok {
		msg.SentAt = &t
	}
	if t, ok := parseGraphTime(g.CreatedDateTime); ok {
		msg.CreatedAt = &t
	}
	if t, ok := parseGraphTime(g.LastModifiedDateTime); ok {
		msg.LastModifiedAt = &t
	}
	return msg
}

func (c *Client) toThreadMessage(g graphMessage) mail.ThreadMessage {
	row := mail.ThreadMessage{
		Account:           c.account,
		Provider:          "outlook",
		ID:                g.ID,
		InternetMessageID: g.InternetMessageID,
		ThreadID:          g.ConversationID,
		Folder:            mail.FolderSummary{ID: g.ParentFolderID},
		From:              recipient(g.From),
		Subject:           g.Subject,
		IsRead:            g.IsRead,
		Importance:        g.Importance,
		HasAttachments:    g.HasAttachments,
		Preview:           g.BodyPreview,
	}
	if t, ok := parseGraphTime(g.ReceivedDateTime); ok {
		row.ReceivedAt = &t
	}
	if t, ok := parseGraphTime(g.SentDateTime); ok {
		row.SentAt = &t
	}
	return row
}

func recipient(g *graphRecipient) mail.Recipient {
	if g == nil {
		return mail.Recipient{}
	}
	return mail.Recipient{Name: g.EmailAddress.Name, Address: g.EmailAddress.Address}
}

func recipients(in []graphRecipient) []mail.Recipient {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Recipient, 0, len(in))
	for i := range in {
		out = append(out, recipient(&in[i]))
	}
	return out
}

func headers(in []graphHeader) []mail.Header {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Header, 0, len(in))
	for _, h := range in {
		out = append(out, mail.Header{Name: h.Name, Value: h.Value})
	}
	return out
}

func outlookThreadTime(row mail.ThreadMessage) time.Time {
	if row.SentAt != nil {
		return *row.SentAt
	}
	if row.ReceivedAt != nil {
		return *row.ReceivedAt
	}
	return time.Time{}
}

func (c *Client) messageNotFound(kind mail.ResolvedSelectorKind) error {
	return errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{
		"account":       c.account,
		"provider":      "outlook",
		"selector_kind": string(kind),
	})
}

func (c *Client) ambiguousInternetID(internetID string, rows []graphFullMessage) error {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	return errs.NewWithDetails(errs.CodeAmbiguousMessageSelector, "this Message-ID belongs to more than one message; pick one by its id with --id", map[string]any{
		"account":             c.account,
		"provider":            "outlook",
		"internet_message_id": internetID,
		"candidate_ids":       ids,
	})
}
