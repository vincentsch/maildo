package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestPrepareDirectSendMovesRequestAndClearsOnFailure(t *testing.T) {
	ctx := context.Background()
	registry := directRegistryForTest(t, &directTestPreparer{provider: "fake"})
	account := directAccountForTest("work", "fake", "work@example.test", "Work")
	req := mail.SendMessageRequest{Message: mail.OutboundMessage{
		To:      []mail.Recipient{{Address: "to@example.test"}},
		Subject: "Subject",
		Body:    mail.OutboundBody{Format: mail.BodyFormatText, Text: "body"},
	}}
	summary, err := PrepareDirectSend(ctx, registry, account, &req)
	if err != nil {
		t.Fatalf("PrepareDirectSend: %v", err)
	}
	if summary.ToCount != 1 || !summary.SubjectPresent || !summary.TextBodyPresent || !directTestOutboundMessageEmpty(req.Message) {
		t.Fatalf("summary/request = %#v %#v", summary, req)
	}
	if req.Prepared == nil {
		t.Fatal("prepared carrier missing")
	}

	carrier, ok := req.Prepared.(*directTestCarrier)
	if !ok || carrier.destroyed {
		t.Fatalf("carrier = %#v", req.Prepared)
	}

	failingPreparer := &directTestPreparer{provider: "fake", err: errors.New("prepare failed")}
	failing := directRegistryForTest(t, failingPreparer)
	req = mail.SendMessageRequest{Message: mail.OutboundMessage{To: []mail.Recipient{{Address: "to@example.test"}}, Subject: "x"}}
	if _, err := PrepareDirectSend(ctx, failing, account, &req); err == nil {
		t.Fatal("failing preparer succeeded")
	}
	if !directTestOutboundMessageEmpty(req.Message) || req.Prepared != nil {
		t.Fatalf("failed request was not cleared: %#v", req)
	}
	if failingPreparer.last == nil || !failingPreparer.last.destroyed {
		t.Fatalf("failing carrier was not destroyed: %#v", failingPreparer.last)
	}
}

func TestComposeIdentityRejectsTamperedPublicFields(t *testing.T) {
	account := directAccountForTest("work", "fake", "work@example.test", "Work")
	if identity, err := ComposeIdentityForAccount(account); err != nil || identity.Email != "work@example.test" || identity.DisplayName != "Work" {
		t.Fatalf("identity = %#v err=%v", identity, err)
	}
	account.Email = "other@example.test"
	if _, err := ComposeIdentityForAccount(account); productCode(err) != errs.CodeProviderClientContractInvalid {
		t.Fatalf("tampered identity error = %v", err)
	}
}

func TestDirectSendPreparerRegistryValidatesProviderMembership(t *testing.T) {
	providers := NewRegistry()
	if err := providers.Register(ProviderRegistration{Name: "fake", Lifecycle: directTestProvider{name: "fake", send: true}, Codec: directTestCodec{name: "fake"}}); err != nil {
		t.Fatalf("provider register: %v", err)
	}
	if err := providers.Validate(); err != nil {
		t.Fatalf("provider validate: %v", err)
	}
	registry := NewDirectSendPreparerRegistry()
	if err := registry.Register("fake", ComposeIngressMIMEBounded, &directTestPreparer{provider: "fake"}); err != nil {
		t.Fatalf("preparer register: %v", err)
	}
	if err := registry.Validate(context.Background(), providers); err != nil {
		t.Fatalf("preparer validate: %v", err)
	}
	account := directAccountForTest("work", "fake", "work@example.test", "")
	policy, err := ComposeIngressPolicyForAccount(registry, account)
	if err != nil || policy != ComposeIngressMIMEBounded {
		t.Fatalf("policy = %q err=%v", policy, err)
	}
	if err := registry.Register("fake", ComposeIngressDelivered, &directTestPreparer{provider: "fake"}); err == nil {
		t.Fatal("registry accepted mutation after validation")
	}

	missing := NewDirectSendPreparerRegistry()
	if err := missing.Register("other", ComposeIngressDelivered, &directTestPreparer{provider: "other"}); err != nil {
		t.Fatalf("missing register: %v", err)
	}
	if err := missing.Validate(context.Background(), providers); err == nil {
		t.Fatal("registry accepted preparer for absent provider")
	}

	missingPreparer := NewDirectSendPreparerRegistry()
	if err := missingPreparer.Validate(context.Background(), providers); err == nil {
		t.Fatal("registry accepted send-capable provider without preparer")
	}

	nonSendProviders := NewRegistry()
	if err := nonSendProviders.Register(ProviderRegistration{Name: "fake", Lifecycle: directTestProvider{name: "fake"}, Codec: directTestCodec{name: "fake"}}); err != nil {
		t.Fatalf("non-send provider register: %v", err)
	}
	if err := nonSendProviders.Validate(); err != nil {
		t.Fatalf("non-send provider validate: %v", err)
	}
	nonSendPreparer := NewDirectSendPreparerRegistry()
	if err := nonSendPreparer.Register("fake", ComposeIngressDelivered, &directTestPreparer{provider: "fake"}); err != nil {
		t.Fatalf("non-send preparer register: %v", err)
	}
	if err := nonSendPreparer.Validate(context.Background(), nonSendProviders); err == nil {
		t.Fatal("registry accepted preparer for non-send-capable provider")
	}
}

func directRegistryForTest(t *testing.T, preparer DirectSendPreparer) *DirectSendPreparerRegistry {
	t.Helper()
	providers := NewRegistry()
	if err := providers.Register(ProviderRegistration{Name: "fake", Lifecycle: directTestProvider{name: "fake", send: true}, Codec: directTestCodec{name: "fake"}}); err != nil {
		t.Fatalf("provider register: %v", err)
	}
	if err := providers.Validate(); err != nil {
		t.Fatalf("provider validate: %v", err)
	}
	registry := NewDirectSendPreparerRegistry()
	if err := registry.Register("fake", ComposeIngressDelivered, preparer); err != nil {
		t.Fatalf("preparer register: %v", err)
	}
	if err := registry.Validate(context.Background(), providers); err != nil {
		t.Fatalf("preparer validate: %v", err)
	}
	return registry
}

func directAccountForTest(account, providerName, email, display string) ResolvedAccount {
	return ResolvedAccount{
		Name:        account,
		Provider:    providerName,
		Email:       email,
		DisplayName: display,
		token: &resolvedAccountToken{
			account:  account,
			provider: providerName,
			email:    email,
			display:  display,
			caps:     implementedCaps(),
		},
	}
}

func productCode(err error) string {
	var product *errs.Error
	if errors.As(err, &product) {
		return product.Code()
	}
	return ""
}

func directTestOutboundMessageEmpty(m mail.OutboundMessage) bool {
	return m.From == nil &&
		len(m.To) == 0 &&
		len(m.CC) == 0 &&
		len(m.BCC) == 0 &&
		len(m.ReplyTo) == 0 &&
		m.Subject == "" &&
		m.Body == (mail.OutboundBody{})
}

type directTestPreparer struct {
	provider string
	err      error
	last     *directTestCarrier
}

func (p *directTestPreparer) ProviderName() string { return p.provider }

func (p *directTestPreparer) PrepareDirectSend(context.Context, ComposeIdentity, mail.OutboundMessage) (mail.PreparedDirectSend, error) {
	carrier := &directTestCarrier{}
	p.last = carrier
	if p.err != nil {
		return carrier, p.err
	}
	return carrier, nil
}

type directTestCarrier struct{ destroyed bool }

func (c *directTestCarrier) Destroy() {
	if c != nil {
		c.destroyed = true
	}
}

type directTestProvider struct {
	name string
	send bool
}

func (p directTestProvider) Name() string { return p.name }
func (p directTestProvider) Capabilities(context.Context) (Capabilities, error) {
	caps := Capabilities{}
	for _, cap := range allCapabilities {
		caps[cap] = CapUnsupported
	}
	if p.send {
		caps[CapSendDirect] = CapImplemented
	}
	return caps, nil
}
func (p directTestProvider) InteractiveAuth(context.Context, InteractiveAuthContext) (InteractiveAuthResult, error) {
	return InteractiveAuthResult{}, errors.New("unused")
}
func (p directTestProvider) LocalHealth(context.Context, StoredAuthContext) (LocalStatus, error) {
	return LocalAbsent, errors.New("unused")
}
func (p directTestProvider) ForcedRefresh(context.Context, StoredAuthContext) (RefreshResult, error) {
	return RefreshResult{}, errors.New("unused")
}
func (p directTestProvider) Logout(context.Context, StoredAuthContext) error {
	return errors.New("unused")
}
func (p directTestProvider) NewAccountClient(context.Context, StoredAuthContext) (AccountClient, error) {
	return nil, errors.New("unused")
}

type directTestCodec struct{ name string }

func (c directTestCodec) ProviderName() string { return c.name }
func (c directTestCodec) Decode(SecretRegistrar, auth.CredentialBinding, []byte) (Credential, error) {
	return nil, errors.New("unused")
}
func (c directTestCodec) Encode(auth.CredentialBinding, Credential) ([]byte, error) {
	return nil, errors.New("unused")
}
func (c directTestCodec) RegisterSecrets(SecretRegistrar, Credential) error {
	return errors.New("unused")
}
func (c directTestCodec) LocalHealth(maildoconfig.ProviderSetup, auth.CredentialBinding, Credential) (LocalStatus, error) {
	return LocalAbsent, errors.New("unused")
}
func (c directTestCodec) RefreshTokenRotated(auth.CredentialBinding, Credential, Credential) (bool, error) {
	return false, errors.New("unused")
}
func (c directTestCodec) RebindCredential(SecretRegistrar, []byte, auth.CredentialBinding, auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	return auth.CredentialRebindResult{}, errors.New("unused")
}
