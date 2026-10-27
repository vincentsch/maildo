package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// ForwardDraftRequest asks a provider for a forward. The original message
// comes only from Source, which this command read and checked, never from a
// message selector.
type ForwardDraftRequest struct {
	To          []mail.Recipient                `json:"to,omitempty"`
	CC          []mail.Recipient                `json:"cc,omitempty"`
	BCC         []mail.Recipient                `json:"bcc,omitempty"`
	Body        mail.OutboundBody               `json:"-"`
	Action      mail.ComposeExecutionAction     `json:"action"`
	Attachments mail.ForwardAttachmentSelection `json:"attachments"`
	Source      *ComposeSourceSnapshot          `json:"-"`
	// Check runs right before a send with what the provider is about to
	// send. It is required when Action is a send.
	Check mail.SendCheck `json:"-"`
}

// ForwardDraftCreator saves a forward as a draft in the provider's own way,
// checks which of the original's attachments went along, and sends the
// draft when the action is a send.
type ForwardDraftCreator interface {
	ValidateForwardDraft(ctx context.Context, req ForwardDraftRequest) (mail.ForwardDraftResult, error)
	CreateForwardDraft(ctx context.Context, req ForwardDraftRequest) (mail.ForwardDraftResult, error)
}

// ForwardDraftPreviewer checks a forward for --dry-run. It has no method that
// writes, so a preview cannot change the mailbox.
type ForwardDraftPreviewer interface {
	ValidateForwardDraftPreview(ctx context.Context, req ForwardDraftRequest) (mail.ForwardDraftResult, error)
}

// DraftAttachmentManager adds and removes file attachments on saved drafts.
type DraftAttachmentManager interface {
	AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error)
	RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error)
}
