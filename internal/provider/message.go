package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// MessageSearcher searches messages.
type MessageSearcher interface {
	SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error)
}

// MessageLister lists the messages in a folder.
type MessageLister interface {
	ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error)
}

// MessageReader reads whole messages.
type MessageReader interface {
	ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error)
}

// MessageAttachmentReader lists a message's attachments and streams their
// content.
type MessageAttachmentReader interface {
	ListMessageAttachments(ctx context.Context, req mail.MessageAttachmentListRequest) (mail.MessageAttachmentList, error)
	DownloadMessageAttachment(ctx context.Context, req mail.MessageAttachmentDownloadRequest) (mail.MessageAttachmentContent, error)
}

// ThreadReader reads a thread (Outlook calls it a conversation).
type ThreadReader interface {
	ReadThread(ctx context.Context, req mail.ReadThreadRequest) (mail.Thread, error)
}

// Diagnoser checks an account's connection step by step for maildo doctor.
// It reports problems in the result instead of returning an error.
type Diagnoser interface {
	Diagnose(ctx context.Context) mail.Diagnosis
}
