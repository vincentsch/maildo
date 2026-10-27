package provider

import (
	"context"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

type ComposeIdentity struct {
	Account     string `json:"-"`
	Provider    string `json:"-"`
	Email       string `json:"-"`
	DisplayName string `json:"-"`
}

type ComposeIngressPolicy string

const (
	ComposeIngressDelivered   ComposeIngressPolicy = "delivered"
	ComposeIngressMIMEBounded ComposeIngressPolicy = "mime_bounded"
)

type DirectSendPreparer interface {
	ProviderName() string
	PrepareDirectSend(context.Context, ComposeIdentity, mail.OutboundMessage) (mail.PreparedDirectSend, error)
}

type directSendPreparerEntry struct {
	provider string
	ingress  ComposeIngressPolicy
	preparer DirectSendPreparer
}

type DirectSendPreparerRegistry struct {
	byName    map[string]directSendPreparerEntry
	order     []string
	frozen    bool
	validated bool
}

type capabilityValidationContextKey struct{}

func CapabilityValidationContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, capabilityValidationContextKey{}, true)
}

func IsCapabilityValidationContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(capabilityValidationContextKey{}).(bool)
	return value
}

func NewDirectSendPreparerRegistry() *DirectSendPreparerRegistry {
	return &DirectSendPreparerRegistry{byName: map[string]directSendPreparerEntry{}}
}

func (r *DirectSendPreparerRegistry) Register(provider string, ingress ComposeIngressPolicy, p DirectSendPreparer) error {
	if r == nil || r.frozen || !IsProviderName(provider) || !validComposeIngressPolicy(ingress) || isNil(p) {
		return errInvalidWiring
	}
	name, err := safeDirectSendPreparerName(p)
	if err != nil || name != provider {
		return errInvalidWiring
	}
	if r.byName == nil {
		r.byName = map[string]directSendPreparerEntry{}
	}
	if _, exists := r.byName[provider]; exists {
		return errInvalidWiring
	}
	r.byName[provider] = directSendPreparerEntry{provider: provider, ingress: ingress, preparer: p}
	r.order = append(r.order, provider)
	return nil
}

func (r *DirectSendPreparerRegistry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

func (r *DirectSendPreparerRegistry) Validate(ctx context.Context, providers *Registry) error {
	if r == nil || providers == nil {
		return errInvalidWiring
	}
	seen := map[string]bool{}
	for _, name := range r.order {
		entry, ok := r.byName[name]
		if !ok || seen[name] || !IsProviderName(name) || !validComposeIngressPolicy(entry.ingress) || isNil(entry.preparer) {
			return errInvalidWiring
		}
		seen[name] = true
		preparerName, err := safeDirectSendPreparerName(entry.preparer)
		if err != nil || preparerName != name {
			return errInvalidWiring
		}
	}
	if len(seen) != len(r.byName) {
		return errInvalidWiring
	}

	implemented := map[string]bool{}
	ctx = CapabilityValidationContext(ctx)
	for _, name := range providers.Names() {
		caps, err := providers.Capabilities(ctx, name)
		if err != nil {
			return errInvalidWiring
		}
		if caps.State(CapSendDirect) == CapImplemented {
			implemented[name] = true
			if _, ok := r.byName[name]; !ok {
				return errInvalidWiring
			}
		}
	}
	for name := range r.byName {
		if !implemented[name] {
			return errInvalidWiring
		}
	}
	r.frozen = true
	r.validated = true
	return nil
}

func ComposeIdentityForAccount(account ResolvedAccount) (ComposeIdentity, error) {
	token := account.token
	if token == nil || token.account != account.Name || token.provider != account.Provider || token.email != account.Email || token.display != account.DisplayName {
		return ComposeIdentity{}, errs.NewProviderClientContractInvalid()
	}
	if strings.TrimSpace(token.email) == "" {
		return ComposeIdentity{}, errs.NewProviderClientContractInvalid()
	}
	name := token.display
	if token.senderName != "" {
		name = token.senderName
	}
	return ComposeIdentity{
		Account:     strings.Clone(token.account),
		Provider:    strings.Clone(token.provider),
		Email:       strings.Clone(token.email),
		DisplayName: strings.Clone(name),
	}, nil
}

func ComposeIngressPolicyForAccount(r *DirectSendPreparerRegistry, account ResolvedAccount) (ComposeIngressPolicy, error) {
	if _, err := ComposeIdentityForAccount(account); err != nil {
		return "", err
	}
	entry, err := r.entry(account.Provider)
	if err != nil {
		return "", err
	}
	return entry.ingress, nil
}

func PrepareDirectSend(ctx context.Context, r *DirectSendPreparerRegistry, account ResolvedAccount, req *mail.SendMessageRequest) (summary mail.PreparedSendSummary, err error) {
	if req == nil || req.Prepared != nil {
		if req != nil {
			clearSendMessageRequest(req)
		}
		return mail.PreparedSendSummary{}, errs.NewProviderClientContractInvalid()
	}
	entry, entryErr := r.entry(account.Provider)
	if entryErr != nil {
		clearSendMessageRequest(req)
		return mail.PreparedSendSummary{}, entryErr
	}
	identity, err := ComposeIdentityForAccount(account)
	if err != nil {
		clearSendMessageRequest(req)
		return mail.PreparedSendSummary{}, err
	}
	summary, err = mail.NewPreparedSendSummary(req.Message)
	if err != nil {
		clearSendMessageRequest(req)
		return mail.PreparedSendSummary{}, err
	}
	carrier, err := entry.preparer.PrepareDirectSend(ctx, identity, req.Message)
	if err != nil {
		if carrier != nil && !isNil(carrier) {
			carrier.Destroy()
		}
		clearSendMessageRequest(req)
		if ctxErr := ctx.Err(); ctxErr != nil && err == ctxErr {
			return mail.PreparedSendSummary{}, ctxErr
		}
		return mail.PreparedSendSummary{}, err
	}
	if carrier == nil || isNil(carrier) {
		clearSendMessageRequest(req)
		return mail.PreparedSendSummary{}, errs.NewProviderClientContractInvalid()
	}
	req.Prepared = carrier
	req.Message = mail.OutboundMessage{}
	return summary, nil
}

func (r *DirectSendPreparerRegistry) entry(provider string) (directSendPreparerEntry, error) {
	if r == nil || !r.frozen || !r.validated {
		return directSendPreparerEntry{}, errs.NewProviderClientContractInvalid()
	}
	entry, ok := r.byName[provider]
	if !ok || !validComposeIngressPolicy(entry.ingress) || isNil(entry.preparer) {
		return directSendPreparerEntry{}, errs.NewProviderClientContractInvalid()
	}
	return entry, nil
}

func clearSendMessageRequest(req *mail.SendMessageRequest) {
	if req == nil {
		return
	}
	if req.Prepared != nil && !isNil(req.Prepared) {
		req.Prepared.Destroy()
	}
	*req = mail.SendMessageRequest{}
}

func validComposeIngressPolicy(policy ComposeIngressPolicy) bool {
	switch policy {
	case ComposeIngressDelivered, ComposeIngressMIMEBounded:
		return true
	default:
		return false
	}
}

func safeDirectSendPreparerName(p DirectSendPreparer) (name string, err error) {
	defer func() {
		if recover() != nil {
			name = ""
			err = errInvalidWiring
		}
	}()
	name = p.ProviderName()
	if !IsProviderName(name) {
		return "", errInvalidWiring
	}
	return name, nil
}
