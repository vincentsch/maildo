package outlook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// ReadComposeSourceMaterial reads the message a reply or forward is based on,
// within maildo's size limits.
func (c *Client) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	g, err := c.fetchFullMessage(ctx, selector, readMessageBodySelect, `outlook.body-content-type="html"`, c.messageNotFound)
	if err != nil {
		return provider.ComposeSourceMaterial{}, err
	}
	body := g.Body.Content
	material := provider.ComposeSourceMaterial{
		SourceID:             g.ID,
		InternetMessageID:    g.InternetMessageID,
		ThreadID:             g.ConversationID,
		From:                 recipient(g.From),
		ReplyTo:              recipients(g.ReplyTo),
		To:                   recipients(g.ToRecipients),
		CC:                   recipients(g.CcRecipients),
		Subject:              g.Subject,
		SentAt:               sentOrReceived(g),
		InReplyTo:            messageIDHeaderTokens(g.InternetMessageHeaders, "In-Reply-To"),
		References:           messageIDHeaderTokens(g.InternetMessageHeaders, "References"),
		HTMLHasCIDReferences: strings.Contains(strings.ToLower(body), "cid:"),
	}
	if strings.EqualFold(g.Body.ContentType, "html") {
		material.HTML = []byte(body)
		material.Text = []byte(htmltext.ToReadable(body))
	} else {
		material.Text = []byte(body)
	}
	return material, nil
}

// ValidateReplyDraft backs the reply preview. Outlook builds the reply itself
// when maildo creates it, so before that maildo only knows which message the
// reply answers.
func (c *Client) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var result mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result = mail.ReplyDraftResult{
			SourceID:         source.SourceID,
			SourceInternetID: source.InternetMessageID,
			BodyFormat:       req.Body.Format,
		}
		return nil
	})
	return result, err
}

// CreateReplyDraft has Outlook create the reply (or reply to all), puts
// maildo's body into it and reads the draft back. For a send it then sends
// the draft.
func (c *Client) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	var result mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		action := "createReply"
		if req.Mode == mail.ReplyModeReplyAll {
			action = "createReplyAll"
		}
		target := c.graphBaseURL + "/me/messages/" + url.PathEscape(source.SourceID) + "/" + action
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
		updated, err := c.UpdateDraft(ctx, mail.UpdateDraftRequest{
			DraftID: out.ID,
			Patch:   mail.DraftPatch{Body: mail.BodyPatch{Set: true, Body: req.Body}},
		})
		if err != nil {
			result.DraftID = out.ID
			return err
		}
		finalDraft, err := c.ReadDraft(ctx, mail.ReadDraftRequest{DraftID: out.ID, MetadataOnly: true})
		if err != nil {
			result.DraftID = out.ID
			return err
		}
		result = mail.ReplyDraftResult{
			SourceID:          source.SourceID,
			SourceInternetID:  source.InternetMessageID,
			DraftID:           out.ID,
			To:                finalDraft.To,
			CC:                finalDraft.CC,
			BCC:               finalDraft.BCC,
			Subject:           finalDraft.Subject,
			BodyFormat:        req.Body.Format,
			AttachmentCount:   len(finalDraft.Attachments),
			WriteOutcomeFacts: updated.WriteOutcomeFacts,
			ProviderMeta:      graphDraftMeta(mail.MetadataDraftWrite),
		}
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

func sentOrReceived(g graphFullMessage) *time.Time {
	if t, ok := parseGraphTime(g.SentDateTime); ok {
		return &t
	}
	if t, ok := parseGraphTime(g.ReceivedDateTime); ok {
		return &t
	}
	return nil
}

func messageIDHeaderTokens(headers []graphHeader, name string) []string {
	for _, header := range headers {
		if !strings.EqualFold(header.Name, name) {
			continue
		}
		tokens, err := mail.ParseMessageIDTokens(header.Value)
		if err != nil {
			return nil
		}
		return tokens
	}
	return nil
}
