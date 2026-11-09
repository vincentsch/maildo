package mail

import (
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
)

// BodyFormat is the format of the body the user gave, such as Markdown.
type BodyFormat string

const (
	BodyFormatText     BodyFormat = "text"
	BodyFormatMarkdown BodyFormat = "markdown"
	BodyFormatHTML     BodyFormat = "html"
)

// OutboundBody is a message body ready to send. Text and HTML stay out of
// JSON output, because they are the content of the mail.
type OutboundBody struct {
	Format      BodyFormat `json:"format"`
	ContentType string     `json:"content_type"`
	Text        string     `json:"-"`
	HTML        string     `json:"-"`
	Bytes       int        `json:"bytes"`
	Present     bool       `json:"present"`
}

// OutboundMessage is a message ready to send or save as a draft. From is the
// checked --from value. maildo only checks it against the account and never
// passes it to the provider as the sender.
type OutboundMessage struct {
	From    *Recipient   `json:"requested_from,omitempty"`
	To      []Recipient  `json:"to,omitempty"`
	CC      []Recipient  `json:"cc,omitempty"`
	BCC     []Recipient  `json:"bcc,omitempty"`
	ReplyTo []Recipient  `json:"reply_to,omitempty"`
	Subject string       `json:"subject"`
	Body    OutboundBody `json:"body"`
	// Attachments are files to send with the message.
	Attachments []AttachmentUpload `json:"-"`
}

// RecipientPatch replaces or clears one recipient field of a draft.
type RecipientPatch struct {
	Set    bool
	Clear  bool
	Values []Recipient
}

// SubjectPatch replaces a draft's subject.
type SubjectPatch struct {
	Set   bool
	Value string
}

// BodyPatch replaces a draft's body.
type BodyPatch struct {
	Set  bool
	Body OutboundBody
}

// DraftPatch holds only the fields a draft update changes.
type DraftPatch struct {
	To      RecipientPatch
	CC      RecipientPatch
	BCC     RecipientPatch
	ReplyTo RecipientPatch
	Subject SubjectPatch
	Body    BodyPatch
}

// CreateDraftRequest asks a provider to save a message as a draft.
type CreateDraftRequest struct {
	Message OutboundMessage `json:"message"`
}

// SendMessageRequest asks a provider to send a new message without saving a
// draft first.
type SendMessageRequest struct {
	Message  OutboundMessage    `json:"-"`
	Prepared PreparedDirectSend `json:"-"`
}

// PreparedDirectSend is a message a provider has prepared for sending. Only
// the provider that prepared it may send it, and Destroy wipes it.
type PreparedDirectSend interface {
	Destroy()
}

// PreparedSendSummary describes a prepared message for output. It holds only
// counts and yes/no facts, no addresses or content.
type PreparedSendSummary struct {
	ToCount               uint32 `json:"to_count"`
	CCCount               uint32 `json:"cc_count"`
	BCCCount              uint32 `json:"bcc_count"`
	ReplyToCount          uint32 `json:"reply_to_count"`
	AttachmentCount       uint32 `json:"attachment_count"`
	SubjectPresent        bool   `json:"subject_present"`
	TextBodyPresent       bool   `json:"text_body_present"`
	HTMLBodyPresent       bool   `json:"html_body_present"`
	SenderOverridePresent bool   `json:"sender_override_present"`
}

// NewPreparedSendSummary builds a PreparedSendSummary. It keeps no addresses
// and none of the message content.
func NewPreparedSendSummary(msg OutboundMessage) (PreparedSendSummary, error) {
	to, err := checkedUint32Len(uint64(len(msg.To)))
	if err != nil {
		return PreparedSendSummary{}, err
	}
	cc, err := checkedUint32Len(uint64(len(msg.CC)))
	if err != nil {
		return PreparedSendSummary{}, err
	}
	bcc, err := checkedUint32Len(uint64(len(msg.BCC)))
	if err != nil {
		return PreparedSendSummary{}, err
	}
	replyTo, err := checkedUint32Len(uint64(len(msg.ReplyTo)))
	if err != nil {
		return PreparedSendSummary{}, err
	}
	summary := PreparedSendSummary{
		ToCount:               to,
		CCCount:               cc,
		BCCCount:              bcc,
		ReplyToCount:          replyTo,
		AttachmentCount:       uint32(len(msg.Attachments)),
		SubjectPresent:        strings.TrimSpace(msg.Subject) != "",
		SenderOverridePresent: msg.From != nil,
	}
	switch msg.Body.Format {
	case BodyFormatText:
		summary.TextBodyPresent = len(msg.Body.Text) > 0
	case BodyFormatMarkdown, BodyFormatHTML:
		summary.TextBodyPresent = len(msg.Body.Text) > 0
		summary.HTMLBodyPresent = len(msg.Body.HTML) > 0
	default:
		if msg.Body.ContentType == "html" {
			summary.TextBodyPresent = len(msg.Body.Text) > 0
			summary.HTMLBodyPresent = len(msg.Body.HTML) > 0
		} else {
			summary.TextBodyPresent = len(msg.Body.Text) > 0
		}
	}
	return summary, nil
}

func checkedUint32Len(n uint64) (uint32, error) {
	if n > uint64(^uint32(0)) {
		return 0, errs.NewSharedProviderContractInvalid()
	}
	return uint32(n), nil
}

// UpdateDraftRequest asks a provider to change some fields of a draft.
type UpdateDraftRequest struct {
	DraftID string     `json:"draft_id"`
	Patch   DraftPatch `json:"patch"`
}

// DraftUpdateValidation is what a provider reports when it checks a draft
// update for --dry-run. It holds no draft content.
type DraftUpdateValidation struct {
	ThreadTransition string `json:"thread_transition,omitempty"`
}

// SendDraftRequest asks a provider to send a saved draft.
//
// Check is required for SendDraft. The provider reads the draft, calls
// Check with exactly what it is about to send, and sends only when Check
// returns nil. So a draft that changed after the user approved it does not
// go out.
type SendDraftRequest struct {
	DraftID string    `json:"draft_id"`
	Check   SendCheck `json:"-"`
}

// SendContent is everything a send hands to the provider, inline
// attachments included. A person approves this, and maildo's approval code
// is a hash of it.
type SendContent struct {
	From        Recipient
	To          []Recipient
	CC          []Recipient
	BCC         []Recipient
	ReplyTo     []Recipient
	Subject     string
	Text        string
	HTML        string
	Attachments []SendAttachment
	// InReplyTo and References are the Message-IDs that keep a reply in
	// its conversation. A new message has neither.
	InReplyTo  string
	References []string
}

// SendAttachment is one attachment of a message to send.
type SendAttachment struct {
	Name        string
	ContentType string
	Size        int64
	Inline      bool
	SHA256      [32]byte
}

// SendCheck runs right before a provider hands a message over for sending,
// with exactly the content it is about to send. An error stops the send,
// and nothing goes out.
type SendCheck func(SendContent) error

// ReadDraftRequest reads a saved draft. BodyMode works as it does for
// messages.
type ReadDraftRequest struct {
	DraftID      string   `json:"draft_id"`
	BodyMode     BodyMode `json:"body_mode,omitempty"`
	MetadataOnly bool     `json:"metadata_only,omitempty"`
}

// DraftRef is the provider id returned after creating or updating a draft.
type DraftRef struct {
	ID string `json:"id"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	// Warnings are problems that did not stop the change, such as an old
	// version of a draft that could not be deleted.
	Warnings []string `json:"warnings,omitempty"`
}

// SendOutcome is small because many providers return no lasting id for the
// sent message.
type SendOutcome struct {
	MessageID string `json:"message_id,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	// SentCopy names the folder that holds the copy of the sent message,
	// when maildo saved one.
	SentCopy string   `json:"sent_copy,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// DirectSendOutcome is what a provider reports after sending a new message.
// Sending a draft reports a SendOutcome.
type DirectSendOutcome struct {
	MessageID string `json:"message_id,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *SMTPDirectSendProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy     string                          `json:"sent_copy,omitempty"`
	Warnings     []string                        `json:"warnings,omitempty"`
}

// Draft is a saved draft as draft read shows it. It holds the body, because
// draft read is the one compose command that prints it.
type Draft struct {
	Account           string                 `json:"account"`
	Provider          string                 `json:"provider"`
	ID                string                 `json:"id"`
	IsDraft           bool                   `json:"is_draft"`
	InternetMessageID string                 `json:"internet_message_id,omitempty"`
	ThreadID          string                 `json:"thread_id,omitempty"`
	Folder            FolderSummary          `json:"folder"`
	From              Recipient              `json:"from"`
	ReplyTo           []Recipient            `json:"reply_to,omitempty"`
	To                []Recipient            `json:"to,omitempty"`
	CC                []Recipient            `json:"cc,omitempty"`
	BCC               []Recipient            `json:"bcc,omitempty"`
	Subject           string                 `json:"subject"`
	CreatedAt         *time.Time             `json:"created_at,omitempty"`
	LastModifiedAt    *time.Time             `json:"last_modified_at,omitempty"`
	HasAttachments    bool                   `json:"has_attachments"`
	Attachments       []Attachment           `json:"attachments,omitempty"`
	Body              MessageBody            `json:"body"`
	ProviderMeta      *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	// Send is what sending the draft would hand over. Only ValidateDraftSend
	// fills it.
	Send *SendContent `json:"-"`
}

// ForwardAttachmentMode says which attachments of the original message go
// along with a forward.
type ForwardAttachmentMode string

const (
	ForwardAttachmentsNone     ForwardAttachmentMode = "none"
	ForwardAttachmentsAll      ForwardAttachmentMode = "all"
	ForwardAttachmentsSelected ForwardAttachmentMode = "selected"
)

// ForwardAttachmentSelection is the user's choice of the original's
// attachments. The provider matches it against the attachments it finds.
type ForwardAttachmentSelection struct {
	Mode  ForwardAttachmentMode `json:"mode"`
	Names []string              `json:"names,omitempty"`
	IDs   []string              `json:"ids,omitempty"`
}

// AttachmentExclusion names an attachment of the original that did not go
// along with a forward, because maildo cannot forward that kind of
// attachment.
type AttachmentExclusion struct {
	Reason     string     `json:"reason"`
	Attachment Attachment `json:"attachment"`
}

// ForwardDraftResult is what a provider reports after a forward. The body
// and the original message never go into JSON.
type ForwardDraftResult struct {
	SourceID                  string                `json:"source_id,omitempty"`
	SourceInternetID          string                `json:"source_internet_id,omitempty"`
	DraftID                   string                `json:"draft_id"`
	SentID                    string                `json:"sent_id,omitempty"`
	To                        []Recipient           `json:"to,omitempty"`
	CC                        []Recipient           `json:"cc,omitempty"`
	BCC                       []Recipient           `json:"bcc,omitempty"`
	Subject                   string                `json:"subject"`
	BodyFormat                BodyFormat            `json:"body_format,omitempty"`
	AttachmentCount           int                   `json:"attachment_count"`
	SelectedSourceAttachments []Attachment          `json:"selected_source_attachments,omitempty"`
	FinalAttachments          []Attachment          `json:"final_attachments,omitempty"`
	ExcludedUnsupported       []AttachmentExclusion `json:"excluded_unsupported,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy     string                 `json:"sent_copy,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
	// Send is what sending the forward would hand over. Providers that build
	// the forward from MIME fill it when they check it; Outlook builds the
	// forward on its side and leaves it empty.
	Send *SendContent `json:"-"`
}

// AttachmentUpload is a file to attach, without its local path. The command
// checks the size before a provider sees it.
type AttachmentUpload struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Bytes       []byte `json:"-"`
}

// AddDraftAttachmentsRequest asks a provider to add files to a draft, in the
// order the user gave them.
type AddDraftAttachmentsRequest struct {
	DraftID string             `json:"draft_id"`
	Uploads []AttachmentUpload `json:"uploads"`
}

// AddDraftAttachmentsResult lists the files that were added and all
// attachments the draft has afterwards, without their content.
type AddDraftAttachmentsResult struct {
	DraftID          string       `json:"draft_id"`
	Added            []Attachment `json:"added_attachments,omitempty"`
	FinalAttachments []Attachment `json:"final_attachments,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
}

// RemoveDraftAttachmentRequest asks a provider to remove one attachment from
// a draft.
type RemoveDraftAttachmentRequest struct {
	DraftID        string                   `json:"draft_id"`
	AttachmentID   string                   `json:"attachment_id"`
	ExecutionToken AttachmentExecutionToken `json:"-"`
}

// RemoveDraftAttachmentResult names the removed attachment and lists the
// attachments the draft has afterwards, without their content.
type RemoveDraftAttachmentResult struct {
	DraftID          string       `json:"draft_id"`
	Removed          Attachment   `json:"removed_attachment"`
	FinalAttachments []Attachment `json:"final_attachments,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
}
