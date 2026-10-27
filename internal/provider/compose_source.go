package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// ComposeSourceAccess is the permission a command needs before it reads the
// original message for a reply or forward. It cannot be built or copied from
// outside this package.
type ComposeSourceAccess struct {
	account  string
	provider string
	email    string
	scope    *commandAccountClientScope
	policy   ComposeIngressPolicy
	valid    bool
}

// ComposeSourceAccessForAccount returns the permission to read the original
// message. It checks that the account is the one this command picked and
// that its provider is registered for replies and forwards.
func ComposeSourceAccessForAccount(r *DirectSendPreparerRegistry, account ResolvedAccount) (ComposeSourceAccess, error) {
	token := account.token
	if token == nil || token.account != account.Name || token.provider != account.Provider || token.email != account.Email || token.display != account.DisplayName {
		return ComposeSourceAccess{}, errs.NewProviderClientContractInvalid()
	}
	policy, err := ComposeIngressPolicyForAccount(r, account)
	if err != nil {
		return ComposeSourceAccess{}, err
	}
	if account.Provider == "gmail" && policy != ComposeIngressMIMEBounded {
		return ComposeSourceAccess{}, errs.NewProviderClientContractInvalid()
	}
	if policy != ComposeIngressMIMEBounded && policy != ComposeIngressDelivered {
		return ComposeSourceAccess{}, errs.NewProviderClientContractInvalid()
	}
	return ComposeSourceAccess{
		account:  strings.Clone(account.Name),
		provider: strings.Clone(account.Provider),
		email:    strings.Clone(account.Email),
		scope:    token.scope,
		policy:   policy,
		valid:    true,
	}, nil
}

// ComposeSourceMaterial is the original message of a reply or forward, as
// the provider read it. No field goes into JSON, because the body and
// headers are the content of someone's mail.
type ComposeSourceMaterial struct {
	SourceID             string           `json:"-"`
	InternetMessageID    string           `json:"-"`
	ThreadID             string           `json:"-"`
	From                 mail.Recipient   `json:"-"`
	ReplyTo              []mail.Recipient `json:"-"`
	To                   []mail.Recipient `json:"-"`
	CC                   []mail.Recipient `json:"-"`
	Subject              string           `json:"-"`
	SentAt               *time.Time       `json:"-"`
	InReplyTo            []string         `json:"-"`
	References           []string         `json:"-"`
	Text                 []byte           `json:"-"`
	HTML                 []byte           `json:"-"`
	HTMLHasCIDReferences bool             `json:"-"`
}

// ComposeSourceMaterialReader is what a provider implements to read the
// original message. Commands never get it directly; they use
// ComposeSourceReader.
type ComposeSourceMaterialReader interface {
	ReadComposeSourceMaterial(context.Context, mail.ResolvedMessageSelector) (ComposeSourceMaterial, error)
}

// ComposeSourceReader is how a command reads the original message of a reply
// or forward.
type ComposeSourceReader interface {
	ReadComposeSource(context.Context, ComposeSourceAccess, mail.ResolvedMessageSelector) (*ComposeSourceSnapshot, error)
}

// ComposeSourceSnapshot holds a checked copy of the original message for the
// command and account that read it. Destroy overwrites the body with zeros.
type ComposeSourceSnapshot struct {
	mu        sync.Mutex
	account   string
	provider  string
	scope     *commandAccountClientScope
	material  ComposeSourceMaterial
	destroyed bool
}

func newComposeSourceSnapshot(account, providerName string, scope *commandAccountClientScope, material ComposeSourceMaterial) (*ComposeSourceSnapshot, error) {
	if scope == nil || material.SourceID == "" || material.ThreadID == "" {
		return nil, errs.NewProviderClientContractInvalid()
	}
	if material.InternetMessageID != "" {
		normalized, err := mail.NormalizeMessageIDScalar(material.InternetMessageID)
		if err != nil {
			return nil, errs.NewProviderClientContractInvalid()
		}
		material.InternetMessageID = normalized
	}
	if material.InReplyTo == nil {
		material.InReplyTo = []string{}
	}
	if material.References == nil {
		material.References = []string{}
	}
	for _, id := range material.InReplyTo {
		if _, err := mail.NormalizeMessageIDScalar(id); err != nil {
			return nil, errs.NewProviderClientContractInvalid()
		}
	}
	for _, id := range material.References {
		if _, err := mail.NormalizeMessageIDScalar(id); err != nil {
			return nil, errs.NewProviderClientContractInvalid()
		}
	}
	if int64(len(material.Text)+len(material.HTML)) > mail.MaxMessageBodyBytes {
		return nil, errs.NewReadLimitExceeded(account, errs.ProviderName(providerName), errs.ReadLimitBodyBytes, uint64(mail.MaxMessageBodyBytes+1))
	}
	return &ComposeSourceSnapshot{
		account:  strings.Clone(account),
		provider: strings.Clone(providerName),
		scope:    scope,
		material: cloneComposeSourceMaterial(material),
	}, nil
}

func NewComposeSourceSnapshotForTest(account, providerName string, material ComposeSourceMaterial) (*ComposeSourceSnapshot, error) {
	if account == "" || !IsProviderName(providerName) {
		return nil, errs.NewProviderClientContractInvalid()
	}
	return newComposeSourceSnapshot(account, providerName, &commandAccountClientScope{}, material)
}

// Use passes a copy of the message to fn and wipes the copy when fn returns.
func (s *ComposeSourceSnapshot) Use(fn func(ComposeSourceMaterial) error) error {
	if fn == nil {
		return errs.NewProviderClientContractInvalid()
	}
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return errs.NewProviderClientContractInvalid()
	}
	material := cloneComposeSourceMaterial(s.material)
	s.mu.Unlock()
	defer destroyComposeSourceMaterial(&material)
	return fn(material)
}

// Clone returns a separate copy for the same command and account.
func (s *ComposeSourceSnapshot) Clone() (*ComposeSourceSnapshot, error) {
	if s == nil {
		return nil, errs.NewProviderClientContractInvalid()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed || s.scope == nil {
		return nil, errs.NewProviderClientContractInvalid()
	}
	return &ComposeSourceSnapshot{
		account:  strings.Clone(s.account),
		provider: strings.Clone(s.provider),
		scope:    s.scope,
		material: cloneComposeSourceMaterial(s.material),
	}, nil
}

// Transfer moves the message into a new snapshot for the provider's write
// and makes the old snapshot unusable.
func (s *ComposeSourceSnapshot) Transfer() (*ComposeSourceSnapshot, error) {
	if s == nil {
		return nil, errs.NewProviderClientContractInvalid()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed || s.scope == nil {
		return nil, errs.NewProviderClientContractInvalid()
	}
	out := &ComposeSourceSnapshot{
		account:  strings.Clone(s.account),
		provider: strings.Clone(s.provider),
		scope:    s.scope,
		material: cloneComposeSourceMaterial(s.material),
	}
	s.destroyed = true
	destroyComposeSourceMaterial(&s.material)
	s.account = ""
	s.provider = ""
	s.scope = nil
	return out, nil
}

func (s *ComposeSourceSnapshot) Destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return
	}
	s.destroyed = true
	destroyComposeSourceMaterial(&s.material)
	s.account = ""
	s.provider = ""
	s.scope = nil
}

func (s *ComposeSourceSnapshot) sourceIDs() (sourceID, internetID string, ok bool) {
	if s == nil {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return "", "", false
	}
	return s.material.SourceID, s.material.InternetMessageID, true
}

func cloneComposeSourceMaterial(in ComposeSourceMaterial) ComposeSourceMaterial {
	out := in
	out.ReplyTo = append([]mail.Recipient(nil), in.ReplyTo...)
	out.To = append([]mail.Recipient(nil), in.To...)
	out.CC = append([]mail.Recipient(nil), in.CC...)
	out.InReplyTo = append([]string(nil), in.InReplyTo...)
	out.References = append([]string(nil), in.References...)
	out.Text = append([]byte(nil), in.Text...)
	out.HTML = append([]byte(nil), in.HTML...)
	if in.SentAt != nil {
		t := *in.SentAt
		out.SentAt = &t
	}
	return out
}

func destroyComposeSourceMaterial(m *ComposeSourceMaterial) {
	if m == nil {
		return
	}
	for i := range m.Text {
		m.Text[i] = 0
	}
	for i := range m.HTML {
		m.HTML[i] = 0
	}
	*m = ComposeSourceMaterial{}
}

func validateComposeSourceSelector(selector mail.ResolvedMessageSelector, material ComposeSourceMaterial) error {
	switch selector.Kind {
	case mail.ResolvedSelectorID:
		if selector.ID == "" || selector.ID != material.SourceID {
			return errs.NewProviderClientContractInvalid()
		}
	case mail.ResolvedSelectorInternetID:
		normalized, err := mail.NormalizeMessageIDScalar(selector.InternetID)
		if err != nil || normalized != material.InternetMessageID {
			return errs.NewProviderClientContractInvalid()
		}
	default:
		return errs.NewProviderClientContractInvalid()
	}
	return nil
}
