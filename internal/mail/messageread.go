package mail

import (
	"encoding/json"
	"io"
	"time"
)

// SelectorKind says how the user picked a message: by id, by Message-ID, by
// thread id or by a search.
type SelectorKind string

const (
	SelectorID         SelectorKind = "id"
	SelectorInternetID SelectorKind = "internet_id"
	SelectorThreadID   SelectorKind = "thread_id"
	SelectorQuery      SelectorKind = "query"
)

// QueryPick says which search result --query picks: --latest or --first.
type QueryPick string

const (
	PickLatest QueryPick = "latest"
	PickFirst  QueryPick = "first"
)

// BodyMode says how read shows the body, such as readable text or raw MIME.
type BodyMode string

const (
	BodyModeReadable BodyMode = "readable"
	BodyModeText     BodyMode = "text"
	BodyModeHTML     BodyMode = "html"
	BodyModeRaw      BodyMode = "raw"
)

// MessageSelector is how the user picked a message on the command line. A
// search is run first, so providers only ever get a ResolvedMessageSelector.
type MessageSelector struct {
	Kind       SelectorKind `json:"kind"`
	ID         string       `json:"id,omitempty"`
	InternetID string       `json:"internet_id,omitempty"`
	ThreadID   string       `json:"thread_id,omitempty"`
	Query      *Query       `json:"query,omitempty"`
	Pick       QueryPick    `json:"pick,omitempty"`
}

// ResolvedSelectorKind says how a ResolvedMessageSelector names the message.
type ResolvedSelectorKind string

const (
	ResolvedSelectorID         ResolvedSelectorKind = "id"
	ResolvedSelectorInternetID ResolvedSelectorKind = "internet_id"
	ResolvedSelectorThreadID   ResolvedSelectorKind = "thread_id"
)

// ResolvedMessageSelector names one message for a provider. It never holds a
// search; internal/search runs that first.
type ResolvedMessageSelector struct {
	Kind       ResolvedSelectorKind `json:"kind"`
	ID         string               `json:"id,omitempty"`
	InternetID string               `json:"internet_id,omitempty"`
	ThreadID   string               `json:"thread_id,omitempty"`
}

// ReadMessageRequest reads one message, with the body in the chosen mode.
type ReadMessageRequest struct {
	Selector     ResolvedMessageSelector `json:"selector"`
	BodyMode     BodyMode                `json:"body_mode"`
	MetadataOnly bool                    `json:"metadata_only,omitempty"`
}

// ReadThreadRequest reads one thread (Outlook calls it a conversation).
type ReadThreadRequest struct {
	Selector ResolvedMessageSelector `json:"selector"`
}

// Header is one visible message header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Attachment describes an attachment. The content is downloaded separately.
type Attachment struct {
	ID               string                      `json:"id"`
	Name             string                      `json:"name,omitempty"`
	ContentType      string                      `json:"content_type,omitempty"`
	Size             int64                       `json:"size,omitempty"`
	IsInline         bool                        `json:"is_inline"`
	ContentID        string                      `json:"content_id,omitempty"`
	Kind             AttachmentKind              `json:"kind,omitempty"`
	ProviderType     string                      `json:"provider_type,omitempty"`
	ProviderMeta     *AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
	SizeFacts        AttachmentSizeFacts         `json:"-"`
	SizeFactsPresent bool                        `json:"-"`
	ExecutionToken   AttachmentExecutionToken    `json:"-"`
}

func (a Attachment) MarshalJSON() ([]byte, error) {
	type attachmentJSON struct {
		ID           string                      `json:"id"`
		Name         string                      `json:"name,omitempty"`
		ContentType  string                      `json:"content_type,omitempty"`
		Size         int64                       `json:"size,omitempty"`
		IsInline     bool                        `json:"is_inline"`
		ContentID    string                      `json:"content_id,omitempty"`
		Kind         AttachmentKind              `json:"kind,omitempty"`
		ProviderType string                      `json:"provider_type,omitempty"`
		ProviderMeta *AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
		EncodedSize  *uint64                     `json:"encoded_size,omitempty"`
		DecodedSize  *uint64                     `json:"decoded_size,omitempty"`
	}
	out := attachmentJSON{
		ID:           a.ID,
		Name:         a.Name,
		ContentType:  a.ContentType,
		Size:         a.Size,
		IsInline:     a.IsInline,
		ContentID:    a.ContentID,
		Kind:         a.Kind,
		ProviderType: a.ProviderType,
		ProviderMeta: a.ProviderMeta,
	}
	out.EncodedSize, out.DecodedSize = a.KnownSizes()
	return json.Marshal(out)
}

// KnownSizes are the encoded and decoded sizes when the provider knows
// them for sure, for JSON. An estimate is left out; Size has it.
func (a Attachment) KnownSizes() (encoded, decoded *uint64) {
	if !a.SizeFactsPresent {
		return nil, nil
	}
	if a.SizeFacts.EncodedSizeKnown {
		v := a.SizeFacts.EncodedSize
		encoded = &v
	}
	if a.SizeFacts.DecodedSizeKnown {
		v := a.SizeFacts.DecodedSize
		decoded = &v
	}
	return encoded, decoded
}

const (
	MaxMessageBodyBytes               int64 = 1 * 1024 * 1024
	MaxMessageAttachmentDownloadBytes int64 = 25 * 1024 * 1024
	MaxSimpleAttachmentBytes          int64 = 3 * 1024 * 1024
	MaxOutboundEncodedMessageBytes    int64 = 25 * 1024 * 1024
)

type AttachmentSizeFacts struct {
	EncodedSize      uint64
	EncodedSizeKnown bool
	DecodedSize      uint64
	DecodedSizeKnown bool
}

// DraftAttachmentSnapshot records exactly where an attachment sat in a draft
// when maildo listed it. Only the provider reads it, through an
// AttachmentExecutionToken, so a removal hits that attachment and no other.
type DraftAttachmentSnapshot struct {
	DraftID           string
	SnapshotMessageID string
	PartID            string
	ChildIndexPath    []uint16
	ContentType       string
	Disposition       string
	ContentID         string
	DecodedSize       uint64
	DecodedSizeKnown  bool
	Kind              AttachmentKind
}

// AttachmentExecutionToken lets a provider remove one specific attachment.
// It must never appear in JSON output.
type AttachmentExecutionToken interface {
	Clone() AttachmentExecutionToken
	Destroy()
}

// MessageAttachmentListRequest asks a provider for the attachments of one
// message.
type MessageAttachmentListRequest struct {
	MessageID string `json:"message_id"`
}

// MessageAttachmentList describes a message's attachments without their
// content. Only a download reads the content, so listing never holds file
// data in memory or prints it.
type MessageAttachmentList struct {
	MessageID         string                      `json:"message_id"`
	InternetMessageID string                      `json:"internet_message_id,omitempty"`
	Attachments       []Attachment                `json:"attachments"`
	ProviderMeta      *AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
}

// MessageAttachmentDownloadRequest asks for one attachment of one message.
type MessageAttachmentDownloadRequest struct {
	MessageID    string `json:"message_id"`
	AttachmentID string `json:"attachment_id"`
}

// MessageAttachmentContent returns a stream, not a byte slice, so callers can
// enforce size limits while writing.
type MessageAttachmentContent struct {
	AttachmentID string        `json:"attachment_id"`
	Content      io.ReadCloser `json:"-"`
	ContentType  string        `json:"content_type,omitempty"`
	DeclaredSize int64         `json:"declared_size,omitempty"`
}

// AttachmentKind says what kind of attachment it is, as far as that matters
// for changing drafts safely. Empty means the provider did not say.
type AttachmentKind string

const (
	AttachmentKindFile      AttachmentKind = "file"
	AttachmentKindItem      AttachmentKind = "item"
	AttachmentKindReference AttachmentKind = "reference"
	AttachmentKindUnknown   AttachmentKind = "unknown"
)

// MessageBody is the body as read shows it, with the original content type
// and the mode it was rendered in.
type MessageBody struct {
	ContentType string   `json:"content_type"`
	Mode        BodyMode `json:"mode"`
	Encoding    string   `json:"encoding,omitempty"`
	Content     string   `json:"content"`
}

// Message is a whole message as read returns it.
type Message struct {
	Account           string                   `json:"account"`
	Provider          string                   `json:"provider"`
	ID                string                   `json:"id"`
	InternetMessageID string                   `json:"internet_message_id,omitempty"`
	ThreadID          string                   `json:"thread_id,omitempty"`
	Folder            FolderSummary            `json:"folder"`
	From              Recipient                `json:"from"`
	Sender            *Recipient               `json:"sender,omitempty"`
	ReplyTo           []Recipient              `json:"reply_to,omitempty"`
	To                []Recipient              `json:"to,omitempty"`
	CC                []Recipient              `json:"cc,omitempty"`
	BCC               []Recipient              `json:"bcc,omitempty"`
	Subject           string                   `json:"subject"`
	ReceivedAt        *time.Time               `json:"received_at,omitempty"`
	SentAt            *time.Time               `json:"sent_at,omitempty"`
	CreatedAt         *time.Time               `json:"created_at,omitempty"`
	LastModifiedAt    *time.Time               `json:"last_modified_at,omitempty"`
	Headers           []Header                 `json:"headers,omitempty"`
	IsRead            bool                     `json:"is_read"`
	Importance        string                   `json:"importance,omitempty"`
	Categories        []string                 `json:"categories,omitempty"`
	HasAttachments    bool                     `json:"has_attachments"`
	Attachments       []Attachment             `json:"attachments,omitempty"`
	Body              MessageBody              `json:"body"`
	ProviderMeta      *MessageProviderMetadata `json:"provider_metadata,omitempty"`
	// Warnings say when maildo had to work around broken mail, for example a
	// body in an unknown charset, or cut a very long body short.
	Warnings []string `json:"warnings,omitempty"`
}

// ThreadMessage is one message of a thread, without its body.
type ThreadMessage struct {
	Account           string        `json:"account"`
	Provider          string        `json:"provider"`
	ID                string        `json:"id"`
	InternetMessageID string        `json:"internet_message_id,omitempty"`
	ThreadID          string        `json:"thread_id,omitempty"`
	Folder            FolderSummary `json:"folder"`
	From              Recipient     `json:"from"`
	Subject           string        `json:"subject"`
	ReceivedAt        *time.Time    `json:"received_at,omitempty"`
	SentAt            *time.Time    `json:"sent_at,omitempty"`
	IsRead            bool          `json:"is_read"`
	Importance        string        `json:"importance,omitempty"`
	HasAttachments    bool          `json:"has_attachments"`
	Preview           string        `json:"preview,omitempty"`
}

// Thread is a whole thread. Its messages come without bodies.
type Thread struct {
	Account      string                  `json:"account"`
	Provider     string                  `json:"provider"`
	ThreadID     string                  `json:"thread_id"`
	Messages     []ThreadMessage         `json:"messages"`
	ProviderMeta *ThreadProviderMetadata `json:"provider_metadata,omitempty"`
}
