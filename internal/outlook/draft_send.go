package outlook

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
)

// maildo sends every Outlook message with Graph's sendMail. A draft goes out
// like this: maildo reads the draft once, with its body and all attachments,
// checks that copy, sends that copy with sendMail and then deletes the draft.
// Graph cannot send a draft only if it is unchanged, so this is the way to
// make sure an edit that lands after the check does not go out.

// maxSendAttachmentBytes is how much attachment data maildo puts into one
// sendMail request. Graph takes about 3 MB of attachments in a request.
// Bigger files need an upload session on a draft, and a draft sent by id is
// not the copy maildo checked.
const maxSendAttachmentBytes = 3 << 20

// maxDraftSendResponseBytes limits the read of a draft with its attachments:
// 3 MB of attachments are 4 MB in base64, plus the body.
const maxDraftSendResponseBytes = 6 << 20

// The MAPI properties behind the In-Reply-To and References headers. Graph
// does not let maildo set those headers directly, so a reply sent with
// sendMail carries them as extended properties to stay in its thread.
const (
	inReplyToProperty  = "String 0x1042"
	referencesProperty = "String 0x1039"
)

const draftSendSelect = "id,isDraft,changeKey,subject,body,from,toRecipients,ccRecipients,bccRecipients,replyTo,conversationId,parentFolderId,internetMessageId,createdDateTime,lastModifiedDateTime,hasAttachments"

var draftSendExpand = "attachments,singleValueExtendedProperties($filter=id eq '" + inReplyToProperty + "' or id eq '" + referencesProperty + "')"

type graphDraftSnapshot struct {
	graphFullMessage
	Attachments        []graphSendAttachment   `json:"attachments"`
	ExtendedProperties []graphExtendedProperty `json:"singleValueExtendedProperties"`
}

type graphSendAttachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	IsInline     bool   `json:"isInline"`
	ContentID    string `json:"contentId"`
	ContentBytes string `json:"contentBytes"`
}

type graphExtendedProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// draftToSend is one read of a draft: what a person approves (content) and
// the sendMail request that sends exactly that (message).
type draftToSend struct {
	draft     graphFullMessage
	changeKey string
	content   mail.SendContent
	message   map[string]any
	readable  string
}

// readDraftToSend reads the draft with its body as stored and every
// attachment in one request, so all parts come from the same version.
func (c *Client) readDraftToSend(ctx context.Context, id string) (draftToSend, error) {
	values := url.Values{}
	values.Set("$select", draftSendSelect)
	values.Set("$expand", draftSendExpand)
	target := withQuery(c.graphBaseURL+"/me/messages/"+url.PathEscape(id), values)
	status, body, err := c.graphReadRequest(ctx, graphReadDraftSend, target, "")
	if err != nil {
		return draftToSend{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return draftToSend{}, c.draftNotFound(id)
	}
	if status < 200 || status >= 300 {
		return draftToSend{}, c.composeError()
	}
	var g graphDraftSnapshot
	if err := json.Unmarshal(body, &g); err != nil {
		return draftToSend{}, c.composeError()
	}
	if err := c.requireDraft(g.graphFullMessage, id); err != nil {
		return draftToSend{}, err
	}
	return c.draftToSend(g)
}

func (c *Client) draftToSend(g graphDraftSnapshot) (draftToSend, error) {
	out := draftToSend{draft: g.graphFullMessage, changeKey: g.ChangeKey}
	to, cc, bcc, replyTo := recipients(g.ToRecipients), recipients(g.CcRecipients), recipients(g.BccRecipients), recipients(g.ReplyTo)
	if len(to)+len(cc)+len(bcc) == 0 {
		return draftToSend{}, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the draft has no recipients", map[string]any{"field": "recipients", "draft_id": g.ID})
	}
	out.content = mail.SendContent{From: recipient(g.From), To: to, CC: cc, BCC: bcc, ReplyTo: replyTo, Subject: g.Subject}
	contentType := strings.ToLower(g.Body.ContentType)
	if contentType == "html" {
		out.content.HTML = g.Body.Content
		out.readable = htmltext.ToReadable(g.Body.Content)
	} else {
		contentType = "text"
		out.content.Text = g.Body.Content
		out.readable = g.Body.Content
	}
	message := map[string]any{
		"subject":       g.Subject,
		"body":          map[string]any{"contentType": contentType, "content": g.Body.Content},
		"toRecipients":  graphRecipientsPayload(to),
		"ccRecipients":  graphRecipientsPayload(cc),
		"bccRecipients": graphRecipientsPayload(bcc),
		"replyTo":       graphRecipientsPayload(replyTo),
	}
	if out.content.From.Address != "" {
		message["from"] = graphRecipientsPayload([]mail.Recipient{out.content.From})[0]
	}
	var total int64
	var attachments []any
	for _, a := range g.Attachments {
		file := a.ODataType == "#microsoft.graph.fileAttachment" || (a.ODataType == "" && a.ContentBytes != "")
		if !file {
			return draftToSend{}, errs.NewWithDetails(errs.CodeUnsupportedAttachment,
				fmt.Sprintf("the draft has an attachment maildo cannot send on Outlook: %q is an attached email or a link to a file", a.Name),
				map[string]any{"account": c.account, "provider": "outlook", "draft_id": g.ID,
					"hint": "send the draft from Outlook itself, or remove that attachment with maildo attachments remove"})
		}
		data, err := base64.StdEncoding.DecodeString(a.ContentBytes)
		if err != nil {
			return draftToSend{}, c.composeError()
		}
		total += int64(len(data))
		if total > maxSendAttachmentBytes {
			return draftToSend{}, outlookAttachmentsTooLarge(c.account)
		}
		out.content.Attachments = append(out.content.Attachments, mail.SendAttachment{
			Name: a.Name, ContentType: a.ContentType, Size: int64(len(data)), Inline: a.IsInline, SHA256: sha256.Sum256(data),
		})
		attachment := map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         a.Name,
			"contentType":  a.ContentType,
			"contentBytes": a.ContentBytes,
			"isInline":     a.IsInline,
		}
		if a.ContentID != "" {
			attachment["contentId"] = a.ContentID
		}
		attachments = append(attachments, attachment)
	}
	if len(attachments) > 0 {
		message["attachments"] = attachments
	}
	var props []any
	for _, p := range g.ExtendedProperties {
		switch {
		case p.Value == "":
			continue
		case strings.EqualFold(p.ID, inReplyToProperty):
			out.content.InReplyTo = strings.TrimSpace(p.Value)
		case strings.EqualFold(p.ID, referencesProperty):
			out.content.References = strings.Fields(p.Value)
		default:
			continue
		}
		props = append(props, map[string]any{"id": p.ID, "value": p.Value})
	}
	if len(props) > 0 {
		message["singleValueExtendedProperties"] = props
	}
	out.message = message
	return out, nil
}

func outlookAttachmentsTooLarge(account string) error {
	return errs.NewWithDetails(errs.CodeAttachmentTooLarge,
		"the message has more than 3 MB of attachments, and maildo sends at most 3 MB of attachments on Outlook",
		map[string]any{"account": account, "provider": "outlook", "limit_bytes": maxSendAttachmentBytes,
			"hint": "send bigger files from Outlook itself, or share them as a link"})
}

// ValidateDraftSend reads the draft for the preview and the approval code.
// SendDraft reads it again and sends only when it still matches.
func (c *Client) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	snap, err := c.readDraftToSend(ctx, req.DraftID)
	if err != nil {
		return mail.Draft{}, err
	}
	draft := c.toDraft(snap.draft, mail.MessageBody{ContentType: "text/plain", Mode: mail.BodyModeText, Content: snap.readable})
	for _, a := range snap.content.Attachments {
		draft.Attachments = append(draft.Attachments, mail.Attachment{Name: a.Name, ContentType: a.ContentType, Size: a.Size, IsInline: a.Inline, Kind: mail.AttachmentKindFile})
	}
	draft.HasAttachments = len(draft.Attachments) > 0
	draft.Send = &snap.content
	return draft, nil
}

// SendDraft reads the draft, lets req.Check approve exactly that copy,
// sends the copy with sendMail and then deletes the draft.
func (c *Client) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	if req.Check == nil {
		return mail.SendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	snap, err := c.readDraftToSend(ctx, req.DraftID)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	if err := req.Check(snap.content); err != nil {
		return mail.SendOutcome{}, err
	}
	sent, err := c.submitSendMail(ctx, errs.WriteOutlookDraftSend, snap.message)
	if err != nil {
		return mail.SendOutcome{}, err
	}
	out := mail.SendOutcome{WriteOutcomeFacts: sent.WriteOutcomeFacts, ProviderMeta: graphDraftMeta(mail.MetadataDraftSend)}
	if warning := c.removeSentDraft(ctx, req.DraftID, snap.changeKey); warning != "" {
		out.Warnings = append(out.Warnings, warning)
	}
	return out, nil
}

// removeSentDraft deletes the draft once its copy is sent. When the draft
// changed in the meantime, someone is still working on it, so maildo leaves
// it alone. The mail is out either way, so problems here are warnings.
func (c *Client) removeSentDraft(ctx context.Context, id, changeKey string) string {
	current, err := c.getDraftByID(ctx, id, "isDraft,changeKey", "")
	if err != nil {
		return "the message was sent, but maildo could not check the draft afterwards, so it is still in Drafts"
	}
	if current.ChangeKey != changeKey {
		return "the message was sent as approved, but the draft changed in the meantime, so maildo left the changed draft in Drafts"
	}
	status, _, err := c.graphDoWithMessages(ctx, http.MethodDelete, c.graphBaseURL+"/me/messages/"+url.PathEscape(id), nil, "", c.composeError,
		"could not prepare an Outlook compose request", "Outlook compose request was rate limited", "compose response exceeds the read size limit")
	if err != nil || (status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotFound && status != http.StatusGone) {
		return "the message was sent, but maildo could not delete the draft, so it is still in Drafts"
	}
	return ""
}
