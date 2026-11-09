package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// DraftCreator creates drafts.
type DraftCreator interface {
	CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error)
}

// DraftReader reads drafts.
type DraftReader interface {
	ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error)
}

// DraftUpdater changes drafts.
type DraftUpdater interface {
	ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error)
	UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error)
}

// DraftDeleter discards a draft the way a mail program does: the draft is
// gone and does not go to Trash.
type DraftDeleter interface {
	DeleteDraft(ctx context.Context, req mail.DeleteDraftRequest) (mail.DraftRef, error)
}

// DraftSender sends a saved draft.
type DraftSender interface {
	ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error)
	SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error)
}

// DirectSender sends a new message without saving a draft first.
type DirectSender interface {
	SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error)
}
