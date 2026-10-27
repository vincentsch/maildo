package mail

// ReplyMode says whether to reply to the sender only or to all.
type ReplyMode string

const (
	ReplyModeReply    ReplyMode = "reply"
	ReplyModeReplyAll ReplyMode = "reply_all"
)

// QuoteMode says how much of the original message the reply quotes.
type QuoteMode string

const (
	QuoteModeFull  QuoteMode = "full"
	QuoteModeShort QuoteMode = "short"
	QuoteModeNone  QuoteMode = "none"
)

// QuoteLanguage is the language of the line above a short quote, such as
// "On 2026-06-28 10:00 UTC, Anna <anna@example.com> wrote:".
type QuoteLanguage string

const (
	QuoteLanguageEnglish QuoteLanguage = "en"
	QuoteLanguageGerman  QuoteLanguage = "de"
	QuoteLanguageAuto    QuoteLanguage = "auto"
)

// ComposeExecutionAction says whether a reply or forward stays a draft, or
// is sent once the draft is saved and checked.
type ComposeExecutionAction string

const (
	ComposeExecutionDraft ComposeExecutionAction = "draft"
	ComposeExecutionSend  ComposeExecutionAction = "send"
)

// ReplyDraftResult is what a provider reports after a reply. The body and the
// original message never go into JSON.
type ReplyDraftResult struct {
	SourceID         string      `json:"source_id,omitempty"`
	SourceInternetID string      `json:"source_internet_id,omitempty"`
	DraftID          string      `json:"draft_id"`
	SentID           string      `json:"sent_id,omitempty"`
	To               []Recipient `json:"to,omitempty"`
	CC               []Recipient `json:"cc,omitempty"`
	BCC              []Recipient `json:"bcc,omitempty"`
	Subject          string      `json:"subject"`
	BodyFormat       BodyFormat  `json:"-"`
	AttachmentCount  int         `json:"attachment_count"`
	*WriteOutcomeFacts
	ProviderMeta *DraftProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy     string                 `json:"sent_copy,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
	// Send is what sending the reply would hand over. Providers that build
	// the reply from MIME fill it when they check it; Outlook builds the
	// reply on its side and leaves it empty.
	Send *SendContent `json:"-"`
}
