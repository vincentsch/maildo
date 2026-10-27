package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// MessageMutator changes one message, for example moves or flags it.
type MessageMutator interface {
	MoveMessage(ctx context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error)
	CopyMessage(ctx context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error)
	ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error)
	DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error)
	RestoreMessage(ctx context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error)
	JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error)
	SetMessageReadState(ctx context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error)
	SetMessageFlag(ctx context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error)
}

// CategoryManager reads and replaces the categories of one message.
type CategoryManager interface {
	ListMessageCategories(ctx context.Context, req mail.MessageCategoryListRequest) (mail.MessageCategoryListResult, error)
	SetMessageCategories(ctx context.Context, req mail.MessageCategorySetRequest) (mail.MessageMutationResult, error)
}
