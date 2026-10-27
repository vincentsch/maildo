package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// ReplyDraftRequest asks a provider for a reply. The original message comes
// only from Source, which this command read and checked, never from a
// message selector.
type ReplyDraftRequest struct {
	Mode     mail.ReplyMode              `json:"mode"`
	Quote    mail.QuoteMode              `json:"quote"`
	Language mail.QuoteLanguage          `json:"quote_language"`
	Action   mail.ComposeExecutionAction `json:"action"`
	Body     mail.OutboundBody           `json:"-"`
	Source   *ComposeSourceSnapshot      `json:"-"`
	// Check runs right before a send with what the provider is about to
	// send. It is required when Action is a send.
	Check mail.SendCheck `json:"-"`
}

// ReplyDraftCreator saves a reply or reply to all as a draft in the
// provider's own way, and sends the draft when the action is a send.
type ReplyDraftCreator interface {
	ValidateReplyDraft(ctx context.Context, req ReplyDraftRequest) (mail.ReplyDraftResult, error)
	CreateReplyDraft(ctx context.Context, req ReplyDraftRequest) (mail.ReplyDraftResult, error)
}
