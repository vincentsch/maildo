package mail

import (
	"errors"
	"fmt"
	netmail "net/mail"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
)

// QueryDate is a YYYY-MM-DD date. It means midnight UTC on that day.
type QueryDate string

// ParseQueryDate checks that s is a date in the form YYYY-MM-DD.
func ParseQueryDate(s string) (QueryDate, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil || t.Format("2006-01-02") != s {
		return "", errs.New(errs.CodeInvalidQuery, fmt.Sprintf("%q is not a date; use YYYY-MM-DD, for example 2026-01-31", s))
	}
	return QueryDate(s), nil
}

// UTCStart returns midnight UTC at the start of the date.
func (d QueryDate) UTCStart() time.Time {
	t, err := time.Parse("2006-01-02", string(d))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Query is a parsed search, the same for every provider.
type Query struct {
	Terms         []string   `json:"terms,omitempty"`
	Phrases       []string   `json:"phrases,omitempty"`
	From          []string   `json:"from,omitempty"`
	To            []string   `json:"to,omitempty"`
	CC            []string   `json:"cc,omitempty"`
	Subject       []string   `json:"subject,omitempty"`
	HasAttachment *bool      `json:"has_attachment,omitempty"`
	After         *QueryDate `json:"after,omitempty"`
	Before        *QueryDate `json:"before,omitempty"`
	// Unread is set by is:unread (true) and is:read (false).
	Unread *bool `json:"unread,omitempty"`
	// Flagged is set by is:flagged or is:starred (true) and is:unflagged
	// (false).
	Flagged *bool `json:"flagged,omitempty"`
}

// IsEmpty reports whether the query has no words and no operators.
func (q Query) IsEmpty() bool {
	return len(q.Terms) == 0 &&
		len(q.Phrases) == 0 &&
		len(q.From) == 0 &&
		len(q.To) == 0 &&
		len(q.CC) == 0 &&
		len(q.Subject) == 0 &&
		q.HasAttachment == nil &&
		q.After == nil &&
		q.Before == nil &&
		q.Unread == nil &&
		q.Flagged == nil
}

// IsSearchStyle reports whether the query needs the provider's text search,
// or whether a filter on flags and dates is enough.
func (q Query) IsSearchStyle() bool {
	return len(q.Terms) > 0 ||
		len(q.Phrases) > 0 ||
		len(q.From) > 0 ||
		len(q.To) > 0 ||
		len(q.CC) > 0 ||
		len(q.Subject) > 0
}

// ListFilter filters the messages that list shows from a folder.
type ListFilter struct {
	Unread bool `json:"unread,omitempty"`
}

// SearchRequest searches the whole mailbox or one folder. Offset leaves out
// that many of the newest matches, for paging.
//
// Without a Folder, a search leaves out Trash and Junk. SkipRoles leaves out
// more folders by role (WellKnownSent, WellKnownDrafts and so on); commands
// that change mail use it to keep Sent and Drafts out of reach. OnlyRoles
// searches just the folders with those roles instead; restore uses it to
// look in Trash and Junk. Folder wins over both.
//
// Complete asks for every match up to Limit. A provider that cannot promise
// that, such as Outlook at its search cap, returns an error instead of a
// shorter list. Commands that pick messages to change set it.
type SearchRequest struct {
	Query     Query    `json:"query"`
	Folder    *Folder  `json:"folder,omitempty"`
	Limit     int      `json:"limit"`
	Offset    int      `json:"offset,omitempty"`
	SkipRoles []string `json:"skip_roles,omitempty"`
	OnlyRoles []string `json:"only_roles,omitempty"`
	Complete  bool     `json:"complete,omitempty"`
}

// ListMessagesRequest lists the messages in one folder.
type ListMessagesRequest struct {
	Folder Folder     `json:"folder"`
	Filter ListFilter `json:"filter"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset,omitempty"`
}

// FolderSummary names the folder a message row is in.
type FolderSummary struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name,omitempty"`
	WellKnownName string `json:"well_known_name,omitempty"`
	Path          string `json:"path,omitempty"`
}

// SummaryFrom returns the FolderSummary for f.
func SummaryFrom(f Folder) FolderSummary {
	return FolderSummary{
		ID:            f.ID,
		DisplayName:   f.DisplayName,
		WellKnownName: f.WellKnownName,
		Path:          f.Path,
	}
}

// Recipient is an address with its display name.
type Recipient struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

// ParseMailboxListValues reads the values of a repeatable address flag. Each
// value may be a comma-separated address list (RFC 5322); display names are
// kept.
// InvalidMailboxValue is the flag value that ParseMailboxListValues could
// not read, so error messages can name it.
type InvalidMailboxValue struct {
	Value string
	Err   error
}

func (e *InvalidMailboxValue) Error() string { return e.Err.Error() }

// InvalidAddressMessage says which value of an address flag is not valid.
func InvalidAddressMessage(flag string, err error) string {
	var bad *InvalidMailboxValue
	if errors.As(err, &bad) {
		value := bad.Value
		if len(value) > 80 {
			value = value[:77] + "..."
		}
		return fmt.Sprintf("%q in --%s is not a valid address", value, flag)
	}
	return "--" + flag + " has an address that is not valid"
}

func ParseMailboxListValues(values []string) ([]Recipient, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]Recipient, 0, len(values))
	for _, raw := range values {
		addrs, err := netmail.ParseAddressList(raw)
		if err != nil {
			return nil, &InvalidMailboxValue{Value: raw, Err: err}
		}
		for _, addr := range addrs {
			out = append(out, Recipient{Name: addr.Name, Address: addr.Address})
		}
	}
	return out, nil
}

// MessageSummary is one message row in list and search results.
type MessageSummary struct {
	Account           string                   `json:"account"`
	Provider          string                   `json:"provider"`
	ID                string                   `json:"id"`
	InternetMessageID string                   `json:"internet_message_id,omitempty"`
	ThreadID          string                   `json:"thread_id,omitempty"`
	Folder            FolderSummary            `json:"folder"`
	From              Recipient                `json:"from"`
	ToCount           int                      `json:"to_count"`
	CCCount           int                      `json:"cc_count"`
	Subject           string                   `json:"subject"`
	ReceivedAt        *time.Time               `json:"received_at,omitempty"`
	SentAt            *time.Time               `json:"sent_at,omitempty"`
	IsRead            bool                     `json:"is_read"`
	Flagged           bool                     `json:"flagged"`
	Importance        string                   `json:"importance,omitempty"`
	HasAttachments    bool                     `json:"has_attachments"`
	Preview           string                   `json:"preview,omitempty"`
	ProviderMeta      *SummaryProviderMetadata `json:"provider_metadata,omitempty"`
	SortGroup         string                   `json:"-"`
	SortOrdinal       uint64                   `json:"-"`
}

// Time returns when the message arrived, or else when it was sent, or else
// the zero time.
func (m MessageSummary) Time() time.Time {
	if m.ReceivedAt != nil {
		return *m.ReceivedAt
	}
	if m.SentAt != nil {
		return *m.SentAt
	}
	return time.Time{}
}

func MessageSummaryLess(a, b MessageSummary) bool {
	at, bt := a.Time(), b.Time()
	if !at.Equal(bt) {
		return at.After(bt)
	}
	if a.Account != b.Account {
		return a.Account < b.Account
	}
	if a.Provider != b.Provider {
		return a.Provider < b.Provider
	}
	if a.SortGroup != b.SortGroup {
		return a.SortGroup < b.SortGroup
	}
	if a.SortOrdinal != b.SortOrdinal {
		return a.SortOrdinal > b.SortOrdinal
	}
	return a.ID < b.ID
}

// DropRows removes the first n rows, for providers that serve a page by
// fetching the rows before it too.
func DropRows(rows []MessageSummary, n int) []MessageSummary {
	if n >= len(rows) {
		return []MessageSummary{}
	}
	return rows[n:]
}

// MessageSummaryList is one provider/account page of message rows. Total is
// the number of matches across all pages when the provider knows it.
// Warnings name things that were skipped, such as a message the server sent
// in a form maildo could not read.
type MessageSummaryList struct {
	Messages   []MessageSummary `json:"messages"`
	Total      int              `json:"-"`
	TotalKnown bool             `json:"-"`
	Warnings   []string         `json:"-"`
}

// DiagnosisCheck is one step of maildo doctor: DNS, TLS, login and so on.
// Status is ok, warning, failed or skipped.
type DiagnosisCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Err is the error behind a failed check. The CLI turns it into the
	// same explanation other commands give.
	Err error `json:"-"`
}

// Diagnosis is what maildo doctor found for one account: the checks, the
// commands that work, and the ones that do not with the reason.
type Diagnosis struct {
	Checks []DiagnosisCheck `json:"checks"`
	Works  []string         `json:"works"`
	Limits []string         `json:"limits"`
}
