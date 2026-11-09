// Package testkit has fake providers and sample data for tests, and an IMAP
// and SMTP server that runs inside the test process (mailserver.go).
//
// It is a normal internal package so tests in several packages can share
// it. The real program never registers these providers.
package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/gmail"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
)

// FakeProvider implements provider.Provider with observable hook counters.
//
// Login and Repair simulate interactive auth by calling Factory.OpenBrowser.
// SilentRefresh never does, which lets tests prove the silent and interactive
// paths stay separate.
type FakeProvider struct {
	ProviderName                          string
	Ident                                 provider.Identity
	RotateOnRefresh                       bool
	AccessToken                           string
	RefreshToken                          string
	LoginCalls                            atomic.Int64
	RepairCalls                           atomic.Int64
	SilentRefreshCalls                    atomic.Int64
	CapabilityCalls                       atomic.Int64
	CapabilityErr                         error
	LogoutCalls                           atomic.Int64
	ArchiveReportImplemented              bool
	ArchiveReportSupportedNotImpl         bool
	ArchiveReportUnsupported              bool
	ArchiveReporterMissing                bool
	ArchiveReportsByAccount               map[string]mail.ArchivePolicyReport
	ArchiveReportErrByAccount             map[string]error
	ArchiveReportCalls                    atomic.Int64
	FoldersImplemented                    bool
	FoldersSupportedNotImpl               bool
	FoldersUnsupported                    bool
	FolderClientMissing                   bool
	Folders                               mail.FolderList
	FoldersByAccount                      map[string]mail.FolderList
	FolderErrByAccount                    map[string]error
	FoldersMutateImplemented              bool
	FoldersMutateSupportedNotImpl         bool
	FolderMutateClientMissing             bool
	FolderListCalls                       atomic.Int64
	FolderCreateCalls                     atomic.Int64
	FolderRenamePreflightCalls            atomic.Int64
	FolderDeletePreflightCalls            atomic.Int64
	FolderRenameCalls                     atomic.Int64
	FolderDeleteCalls                     atomic.Int64
	FolderMutationCallLog                 []string
	folderMutationMu                      sync.Mutex
	RulesReadImplemented                  bool
	RulesReadSupportedNotImpl             bool
	RuleClientMissing                     bool
	RulesByAccount                        map[string][]mail.Rule
	RuleListByAccount                     map[string][]mail.Rule
	RuleGetByAccount                      map[string][]mail.Rule
	RuleErrByAccount                      map[string]error
	RuleListErrByAccount                  map[string]error
	RuleGetErrByAccount                   map[string]error
	AccountClientCalls                    atomic.Int64
	accountClientMu                       sync.Mutex
	accountClientAccounts                 []string
	AccountClientErr                      error
	RuleListCalls                         atomic.Int64
	RuleGetCalls                          atomic.Int64
	RuleReadCallLog                       []string
	ruleReadMu                            sync.Mutex
	RulesMutateImplemented                bool
	RulesMutateSupportedNotImpl           bool
	RuleMutateClientMissing               bool
	RuleMutationMissingInterface          string
	RuleMutationState                     *RuleState
	RuleMutationErrByOperation            map[mail.RuleMutationOperation]error
	RuleMutationErrByAccount              map[string]error
	RuleMutationErrByRule                 map[string]error
	RuleMutationGetErrByAccount           map[string]error
	RuleMutationReceiptOperation          mail.RuleMutationOperation
	RuleMutationReceiptMetadata           mail.RuleMutationProviderMetadata
	RuleMutationSkipStateUpdate           bool
	RuleMutationPostWriteHasError         bool
	RuleMutationPostWriteReadOnly         bool
	RuleMutationPostWriteSequence         *int
	RuleCreateCalls                       atomic.Int64
	RuleUpdateCalls                       atomic.Int64
	RuleDeleteCalls                       atomic.Int64
	RuleMutationCallLog                   []string
	ruleMutationMu                        sync.Mutex
	MailboxImplemented                    bool
	MailboxClientMissing                  bool
	MessagesByAccount                     map[string][]mail.MessageSummary
	MailboxErrByAccount                   map[string]error
	ReadImplemented                       bool
	ThreadsImplemented                    bool
	ReadClientMissing                     bool
	FullByAccount                         map[string]map[string]mail.Message
	ThreadsByAccount                      map[string]map[string]mail.Thread
	DraftImplemented                      bool
	DraftClientMissing                    bool
	ReplyImplemented                      bool
	ForwardImplemented                    bool
	AttachmentsImplemented                bool
	AttachmentsReadImplemented            bool
	AttachmentReadClientMissing           bool
	ReplyStaleMissing                     string
	Drafts                                *DraftState
	MessageAttachmentsByAccount           map[string]map[string][]mail.Attachment
	MessageAttachmentContentByAccount     map[string]map[string]map[string]FakeAttachmentContent
	MessageAttachmentListErrByAccount     map[string]map[string]error
	MessageAttachmentDownloadErrByAccount map[string]map[string]map[string]error
	MessageMetadataOnlyReadCalls          atomic.Int64
	MessageAttachmentListCalls            atomic.Int64
	MessageAttachmentDownloadCalls        atomic.Int64
	MutationsImplemented                  bool
	CategoriesImplemented                 bool
	MutationClientMissing                 bool
	MutationState                         *MutationState
	MutationErrByOperation                map[mail.MessageMutationOperation]error
	MutationErrByAccount                  map[string]error
	MutationErrByMessage                  map[string]error
	MutationCallLog                       []string
	CategoryListLog                       []string
	CategorySetLog                        []string
	MessageMutationCalls                  atomic.Int64
	CategoryListCalls                     atomic.Int64
	CategorySetCalls                      atomic.Int64
	mutationMu                            sync.Mutex
}

// NewFakeProvider returns a fake provider whose identity and tokens are
// fixed values built from name.
func NewFakeProvider(name string) *FakeProvider {
	return &FakeProvider{
		ProviderName: name,
		Ident: provider.Identity{
			Email:             name + "@example.test",
			DisplayName:       "Fake " + name,
			ProviderAccountID: "fake-" + name,
		},
		AccessToken:  "fake-access-token-" + name,
		RefreshToken: "fake-refresh-token-" + name,
		Drafts:       NewDraftState(),
	}
}

// NewFakeFolderProvider returns a fake provider with folders_read implemented.
func NewFakeFolderProvider(name string, folders mail.FolderList) *FakeProvider {
	f := NewFakeProvider(name)
	f.FoldersImplemented = true
	f.Folders = folders
	return f
}

// NewFakeStaleFolderProvider claims folders_read, but its client has no
// FolderReader. All the Stale fakes claim something their client lacks.
func NewFakeStaleFolderProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.FoldersImplemented = true
	f.FolderClientMissing = true
	f.Folders = SampleFolders()
	return f
}

// NewFakePendingFolderProvider reports folders_read as provider-supported but
// not implemented.
func NewFakePendingFolderProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.FoldersSupportedNotImpl = true
	return f
}

// NewFakeArchivePolicyProvider returns a reporter-only account client with the
// composite report and its exact-folder prerequisite implemented.
func NewFakeArchivePolicyProvider(name string, reports map[string]mail.ArchivePolicyReport) *FakeProvider {
	f := NewFakeProvider(name)
	f.ArchiveReportImplemented = true
	f.FoldersImplemented = true
	f.ArchiveReportsByAccount = reports
	return f
}

// NewFakePendingArchivePolicyProvider advertises provider support without a
// completed archive reporter.
func NewFakePendingArchivePolicyProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.ArchiveReportSupportedNotImpl = true
	f.FoldersImplemented = true
	return f
}

// NewFakeStaleArchivePolicyProvider advertises both required capabilities but
// returns an account client without ArchivePolicyReporter.
func NewFakeStaleArchivePolicyProvider(name string) *FakeProvider {
	f := NewFakeArchivePolicyProvider(name, nil)
	f.ArchiveReporterMissing = true
	return f
}

// NewFakeFolderMutationProvider returns a fake provider with folder reads and
// folder mutations implemented.
func NewFakeFolderMutationProvider(name string, folders mail.FolderList) *FakeProvider {
	f := NewFakeFolderProvider(name, folders)
	f.FoldersMutateImplemented = true
	return f
}

// NewFakeStaleFolderMutationProvider advertises folder mutations but returns a
// plain read-only folder client.
func NewFakeStaleFolderMutationProvider(name string) *FakeProvider {
	f := NewFakeFolderMutationProvider(name, SampleFolders())
	f.FolderMutateClientMissing = true
	return f
}

// NewFakePendingFolderMutationProvider reports folder mutations as supported by
// the provider but not implemented by maildo.
func NewFakePendingFolderMutationProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.FoldersMutateSupportedNotImpl = true
	return f
}

// NewFakeRuleProvider returns a fake provider with rules_read and folders_read
// implemented.
func NewFakeRuleProvider(name string, rules map[string][]mail.Rule) *FakeProvider {
	f := NewFakeFolderProvider(name, SampleFolders())
	f.RulesReadImplemented = true
	f.RulesByAccount = rules
	return f
}

// NewFakeRuleMutationProvider returns a fake provider with folder reads, rule
// reads, and rule mutations implemented over shared in-memory state.
func NewFakeRuleMutationProvider(name string, rules map[string][]mail.Rule) *FakeProvider {
	f := NewFakeRuleProvider(name, rules)
	f.RulesMutateImplemented = true
	f.RuleMutationState = NewRuleState(rules)
	return f
}

// NewFakeStaleRuleProvider advertises rules_read but returns no RuleReader.
func NewFakeStaleRuleProvider(name string) *FakeProvider {
	f := NewFakeFolderProvider(name, SampleFolders())
	f.RulesReadImplemented = true
	f.RuleClientMissing = true
	f.RulesByAccount = SampleRules()
	return f
}

// NewFakeStaleRuleMutationProvider advertises rule mutations but returns a
// read-only rule client.
func NewFakeStaleRuleMutationProvider(name string) *FakeProvider {
	f := NewFakeRuleMutationProvider(name, SampleRules())
	f.RuleMutateClientMissing = true
	return f
}

// NewFakeStaleRuleMutationInterfaceProvider returns a fake that claims rule
// changes, but whose client lacks exactly one of the interfaces it claims.
func NewFakeStaleRuleMutationInterfaceProvider(name, iface string) *FakeProvider {
	f := NewFakeRuleMutationProvider(name, SampleRules())
	if iface == "RuleManager" {
		f.RuleMutateClientMissing = true
	} else {
		f.RuleMutationMissingInterface = iface
	}
	return f
}

// NewFakePendingRuleMutationProvider reports rule mutations as supported by the
// provider but not implemented by maildo.
func NewFakePendingRuleMutationProvider(name string) *FakeProvider {
	f := NewFakeRuleProvider(name, SampleRules())
	f.RulesMutateSupportedNotImpl = true
	return f
}

// NewFakeMailboxProvider returns a fake provider with message search/list and
// folders_read implemented.
func NewFakeMailboxProvider(name string, byAccount map[string][]mail.MessageSummary) *FakeProvider {
	f := NewFakeFolderProvider(name, SampleFolders())
	f.MailboxImplemented = true
	f.MessagesByAccount = byAccount
	return f
}

// NewFakeReadProvider returns a fake provider with read/thread and query-selector
// dependencies implemented over in-memory fixtures.
func NewFakeReadProvider(name string, summaries map[string][]mail.MessageSummary, full map[string]map[string]mail.Message, threads map[string]map[string]mail.Thread) *FakeProvider {
	f := NewFakeProvider(name)
	f.ReadImplemented = true
	f.MessagesByAccount = summaries
	f.FullByAccount = full
	f.ThreadsByAccount = threads
	f.Folders = SampleFolders()
	return f
}

// NewFakeStaleThreadProvider advertises threads_read without a ThreadReader client.
func NewFakeStaleThreadProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.ThreadsImplemented = true
	return f
}

// NewFakeDraftProvider returns a fake provider with draft and send capabilities.
func NewFakeDraftProvider(name string, drafts ...mail.Draft) *FakeProvider {
	f := NewFakeProvider(name)
	f.DraftImplemented = true
	f.ReplyImplemented = true
	f.Folders = SampleFolders()
	f.Drafts = NewDraftState(drafts...)
	return f
}

// NewFakeStaleDraftProvider advertises compose capabilities without draft clients.
func NewFakeStaleDraftProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.DraftImplemented = true
	f.DraftClientMissing = true
	f.Folders = SampleFolders()
	return f
}

// NewFakeReplyProvider returns a fake with reply and draft/send support.
func NewFakeReplyProvider(name string, drafts ...mail.Draft) *FakeProvider {
	return NewFakeDraftProvider(name, drafts...)
}

// NewFakeForwardProvider returns a fake with forward and draft/send support.
func NewFakeForwardProvider(name string, drafts ...mail.Draft) *FakeProvider {
	f := NewFakeDraftProvider(name, drafts...)
	f.ForwardImplemented = true
	f.AttachmentsImplemented = true
	f.AttachmentsReadImplemented = true
	return f
}

// NewFakeDraftAttachmentProvider returns a fake with draft attachment mutation
// support.
func NewFakeDraftAttachmentProvider(name string, drafts ...mail.Draft) *FakeProvider {
	f := NewFakeDraftProvider(name, drafts...)
	f.AttachmentsImplemented = true
	return f
}

// NewFakeMessageAttachmentProvider returns a fake with source-message
// attachment list/download support.
func NewFakeMessageAttachmentProvider(name string, summaries map[string][]mail.MessageSummary, full map[string]map[string]mail.Message, attachments map[string]map[string][]mail.Attachment, content map[string]map[string]map[string]FakeAttachmentContent) *FakeProvider {
	f := NewFakeReadProvider(name, summaries, full, SampleThreads())
	f.AttachmentsReadImplemented = true
	f.MessageAttachmentsByAccount = attachments
	f.MessageAttachmentContentByAccount = content
	return f
}

// NewFakeMutationProvider returns a fake with single-message mutation and
// category support over persistent in-memory message fixtures.
func NewFakeMutationProvider(name string, summaries map[string][]mail.MessageSummary, full map[string]map[string]mail.Message) *FakeProvider {
	f := NewFakeReadProvider(name, summaries, full, SampleThreads())
	f.FoldersImplemented = true
	f.MutationsImplemented = true
	f.CategoriesImplemented = true
	f.MutationState = NewMutationState(full)
	return f
}

// NewFakeStaleMutationProvider advertises mutation capabilities but returns a
// read-capable client without the mutation/category interfaces.
func NewFakeStaleMutationProvider(name string) *FakeProvider {
	f := NewFakeReadProvider(name, SampleMessages(), SampleFullMessages(), SampleThreads())
	f.FoldersImplemented = true
	f.MutationsImplemented = true
	f.CategoriesImplemented = true
	f.MutationClientMissing = true
	f.MutationState = NewMutationState(SampleFullMessages())
	return f
}

// NewFakeStaleReplyProvider advertises reply support while omitting one narrow
// account-client interface.
func NewFakeStaleReplyProvider(name, missing string) *FakeProvider {
	f := NewFakeDraftProvider(name)
	f.ReplyStaleMissing = missing
	return f
}

// NewFakeStaleForwardProvider advertises forward support while omitting one
// narrow account-client interface.
func NewFakeStaleForwardProvider(name, missing string) *FakeProvider {
	f := NewFakeForwardProvider(name)
	f.ReplyStaleMissing = missing
	return f
}

// NewFakeStaleDraftAttachmentProvider advertises attachment mutation support
// while omitting the attachment manager.
func NewFakeStaleDraftAttachmentProvider(name string) *FakeProvider {
	f := NewFakeDraftAttachmentProvider(name)
	f.ReplyStaleMissing = "attachments"
	return f
}

// NewFakeStaleMessageAttachmentProvider advertises message attachment reads
// while returning a search/read-capable client without MessageAttachmentReader.
func NewFakeStaleMessageAttachmentProvider(name string) *FakeProvider {
	f := NewFakeReadProvider(name, SampleMessages(), SampleFullMessages(), SampleThreads())
	f.AttachmentsReadImplemented = true
	f.AttachmentReadClientMissing = true
	return f
}

// NewFakeStaleMailboxProvider advertises mailbox search but returns a client
// that does not implement MessageSearcher.
func NewFakeStaleMailboxProvider(name string) *FakeProvider {
	f := NewFakeProvider(name)
	f.MailboxImplemented = true
	f.MailboxClientMissing = true
	return f
}

// NewProviderRegistry returns a test registry with the production Outlook provider
// plus supplied fake or custom providers.
func NewProviderRegistry(extra ...provider.Provider) *provider.Registry {
	r := provider.NewRegistry()
	if !containsProviderName(extra, "outlook") {
		registerTestProvider(r, "outlook", outlook.New(outlook.Options{}), outlook.CredentialCodec{})
	}
	for _, p := range extra {
		registerTestProvider(r, p.Name(), p, fakeCredentialCodec{name: p.Name()})
	}
	return r
}

// NewDirectSendPreparerRegistry mirrors NewProviderRegistry for tests. It adds
// a preparer only for providers whose static table implements direct send.
func NewDirectSendPreparerRegistry(extra ...provider.Provider) *provider.DirectSendPreparerRegistry {
	r := provider.NewDirectSendPreparerRegistry()
	if !containsProviderName(extra, "outlook") {
		_ = r.Register("outlook", provider.ComposeIngressDelivered, outlook.DirectSendPreparer{})
	}
	for _, p := range extra {
		if p == nil {
			continue
		}
		name := p.Name()
		caps, err := p.Capabilities(provider.CapabilityValidationContext(context.Background()))
		if name == "outlook" {
			if err == nil && caps.State(provider.CapSendDirect) == provider.CapImplemented {
				_ = r.Register("outlook", provider.ComposeIngressDelivered, outlook.DirectSendPreparer{})
			}
			continue
		}
		if name == "gmail" {
			if err == nil && caps.State(provider.CapSendDirect) == provider.CapImplemented {
				_ = r.Register("gmail", provider.ComposeIngressMIMEBounded, gmail.DirectSendPreparer{})
			}
			continue
		}
		if err != nil || caps.State(provider.CapSendDirect) != provider.CapImplemented {
			continue
		}
		_ = r.Register(name, provider.ComposeIngressDelivered, FakeDirectSendPreparer{Provider: name})
	}
	return r
}

type FakeDirectSendPreparer struct {
	Provider string
}

func (p FakeDirectSendPreparer) ProviderName() string { return p.Provider }

func (p FakeDirectSendPreparer) PrepareDirectSend(ctx context.Context, identity provider.ComposeIdentity, msg mail.OutboundMessage) (mail.PreparedDirectSend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if identity.Provider != p.Provider || identity.Account == "" || identity.Email == "" {
		return nil, errs.NewProviderClientContractInvalid()
	}
	return NewFakePreparedDirectSend(msg), nil
}

type FakePreparedDirectSend struct {
	mu        sync.Mutex
	Message   mail.OutboundMessage
	Consumed  bool
	Destroyed bool
}

func NewFakePreparedDirectSend(msg mail.OutboundMessage) *FakePreparedDirectSend {
	return &FakePreparedDirectSend{Message: cloneOutboundMessageForFake(msg)}
}

func (p *FakePreparedDirectSend) Consume() (mail.OutboundMessage, error) {
	if p == nil {
		return mail.OutboundMessage{}, errs.NewProviderClientContractInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Destroyed || p.Consumed {
		return mail.OutboundMessage{}, errs.NewProviderClientContractInvalid()
	}
	p.Consumed = true
	msg := p.Message
	p.Message = mail.OutboundMessage{}
	return msg, nil
}

func (p *FakePreparedDirectSend) Destroy() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Destroyed {
		return
	}
	p.Destroyed = true
	p.Message = mail.OutboundMessage{}
}

// NewOutlookProvider returns a real Outlook provider with test overrides.
func NewOutlookProvider(opts outlook.Options) *outlook.Provider {
	return outlook.New(opts)
}

// NewOutlookRegistry returns a provider registry with an injectable Outlook provider.
func NewOutlookRegistry(opts outlook.Options, extra ...provider.Provider) *provider.Registry {
	r := provider.NewRegistry()
	registerTestProvider(r, "outlook", outlook.New(opts), outlook.CredentialCodec{})
	for _, p := range extra {
		registerTestProvider(r, p.Name(), p, fakeCredentialCodec{name: p.Name()})
	}
	return r
}

func containsProviderName(providers []provider.Provider, name string) bool {
	for _, p := range providers {
		if p != nil && p.Name() == name {
			return true
		}
	}
	return false
}

func registerTestProvider(r *provider.Registry, name string, lifecycle provider.Provider, codec provider.CredentialCodec) {
	if err := r.Register(provider.ProviderRegistration{Name: name, Lifecycle: lifecycle, Codec: codec}); err != nil {
		panic(err)
	}
}

type fakeCredentialCodec struct {
	name string
}

type TokenCache struct {
	Version           int       `json:"version"`
	Provider          string    `json:"provider"`
	AccountName       string    `json:"account_name"`
	Authority         string    `json:"authority,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	Scopes            []string  `json:"scopes,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	VerifiedEmail     string    `json:"verified_email,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	ProviderAccountID string    `json:"provider_account_id,omitempty"`
	TenantID          string    `json:"tenant_id,omitempty"`

	destroyed atomic.Bool
}

func (c *TokenCache) Destroy() {
	if c == nil || c.destroyed.Swap(true) {
		return
	}
	c.AccessToken = ""
	c.RefreshToken = ""
	c.TokenType = ""
	c.ClientID = ""
	c.Scopes = nil
}

func (c *TokenCache) Destroyed() bool {
	return c == nil || c.destroyed.Load()
}

func (c fakeCredentialCodec) ProviderName() string { return c.name }

func (c fakeCredentialCodec) Decode(registrar provider.SecretRegistrar, binding auth.CredentialBinding, data []byte) (provider.Credential, error) {
	var cache TokenCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	if err := registerFakeSecrets(registrar, &cache); err != nil {
		cache.Destroy()
		return nil, err
	}
	if cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return &cache, nil
}

func (c fakeCredentialCodec) Encode(binding auth.CredentialBinding, credential provider.Credential) ([]byte, error) {
	cache, ok := credential.(*TokenCache)
	if !ok || cache == nil || cache.Destroyed() || cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return encodeFakeCache(cache)
}

func (c fakeCredentialCodec) RegisterSecrets(registrar provider.SecretRegistrar, credential provider.Credential) error {
	cache, ok := credential.(*TokenCache)
	if !ok || cache == nil || cache.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	return registerFakeSecrets(registrar, cache)
}

func (c fakeCredentialCodec) LocalHealth(maildoconfig.ProviderSetup, auth.CredentialBinding, provider.Credential) (provider.LocalStatus, error) {
	return provider.LocalUsable, nil
}

func (c fakeCredentialCodec) RefreshTokenRotated(binding auth.CredentialBinding, before, after provider.Credential) (bool, error) {
	oldCache, ok := before.(*TokenCache)
	if !ok || oldCache == nil || oldCache.Destroyed() || oldCache.Provider != binding.Provider || oldCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	newCache, ok := after.(*TokenCache)
	if !ok || newCache == nil || newCache.Destroyed() || newCache.Provider != binding.Provider || newCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	return newCache.RefreshToken != "" && newCache.RefreshToken != oldCache.RefreshToken, nil
}

func (c fakeCredentialCodec) RebindCredential(registrar provider.SecretRegistrar, raw []byte, oldBinding, newBinding auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	var cache TokenCache
	if err := json.Unmarshal(raw, &cache); err != nil || cache.Provider != oldBinding.Provider || cache.AccountName != oldBinding.Account {
		return auth.CredentialRebindResult{Bytes: append([]byte(nil), raw...), Disposition: auth.CredentialPreservedNeedsRepair}, nil
	}
	if err := registerFakeSecrets(registrar, &cache); err != nil {
		cache.Destroy()
		return auth.CredentialRebindResult{}, err
	}
	cache.Provider = newBinding.Provider
	cache.AccountName = newBinding.Account
	data, err := encodeFakeCache(&cache)
	if err != nil {
		return auth.CredentialRebindResult{}, err
	}
	return auth.CredentialRebindResult{Bytes: data, Disposition: auth.CredentialReboundUsable}, nil
}

func encodeFakeCache(cache *TokenCache) ([]byte, error) {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	data = append(data, '\n')
	return data, nil
}

func cloneFakeCache(cache *TokenCache) *TokenCache {
	if cache == nil {
		return nil
	}
	return &TokenCache{
		Version:           cache.Version,
		Provider:          cache.Provider,
		AccountName:       cache.AccountName,
		Authority:         cache.Authority,
		ClientID:          cache.ClientID,
		Scopes:            append([]string(nil), cache.Scopes...),
		TokenType:         cache.TokenType,
		AccessToken:       cache.AccessToken,
		RefreshToken:      cache.RefreshToken,
		ExpiresAt:         cache.ExpiresAt,
		VerifiedEmail:     cache.VerifiedEmail,
		DisplayName:       cache.DisplayName,
		ProviderAccountID: cache.ProviderAccountID,
		TenantID:          cache.TenantID,
	}
}

func registerFakeSecrets(registrar provider.SecretRegistrar, cache *TokenCache) error {
	if registrar == nil || cache == nil {
		return nil
	}
	if err := registrar.RegisterSecret(cache.AccessToken); err != nil {
		return err
	}
	if err := registrar.RegisterSecret(cache.RefreshToken); err != nil {
		return err
	}
	if cache.AccessToken != "" {
		if err := registrar.RegisterSecret(cache.TokenType + " " + cache.AccessToken); err != nil {
			return err
		}
		if err := registrar.RegisterSecret("Bearer " + cache.AccessToken); err != nil {
			return err
		}
	}
	return nil
}

func CredentialExists(root, providerName, account string) (bool, error) {
	return auth.NewStore(root).Exists(providerName, account)
}

func LoadCredential(root, providerName, account string) (*TokenCache, error) {
	data, present, err := auth.NewStore(root).LoadBytes(providerName, account)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errs.New(errs.CodeAuthRepairRequired, "credential cache is absent")
	}
	var cache TokenCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

func (f *FakeProvider) Name() string {
	if f.ProviderName == "" {
		return "fake"
	}
	return f.ProviderName
}

type fakeClientLifetime struct {
	account  string
	provider string
	closed   atomic.Bool
}

func newFakeClientLifetime(account, providerName string) *fakeClientLifetime {
	return &fakeClientLifetime{account: account, provider: providerName}
}

func (l *fakeClientLifetime) destroy() {
	if l != nil {
		l.closed.Store(true)
	}
}

func (l *fakeClientLifetime) err() error {
	if l == nil || !l.closed.Load() {
		return nil
	}
	switch errs.ProviderName(l.provider) {
	case errs.ProviderOutlook, errs.ProviderGmail, errs.ProviderIMAP, errs.ProviderFastmail:
		return errs.NewProviderClientClosed(l.account, errs.ProviderName(l.provider))
	default:
		return errs.NewWithDetails(errs.CodeProviderClientClosed, "provider client is closed", map[string]any{
			"account":  l.account,
			"provider": l.provider,
		})
	}
}

// Capabilities is a deterministic mix covering every capability state.
func (f *FakeProvider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	validation := provider.IsCapabilityValidationContext(ctx)
	if !validation {
		f.CapabilityCalls.Add(1)
		if f.CapabilityErr != nil {
			return nil, f.CapabilityErr
		}
	}
	caps := make(provider.Capabilities, len(provider.AllCapabilities()))
	for _, cap := range provider.AllCapabilities() {
		caps[cap] = provider.CapUnsupported
	}
	caps[provider.CapMessagesList] = provider.CapImplemented
	caps[provider.CapMessagesRead] = provider.CapImplemented
	caps[provider.CapMessagesSearch] = provider.CapSupportedNotImpl
	caps[provider.CapRulesRead] = provider.CapUnsupported
	caps[provider.CapRulesMutate] = provider.CapUnsupported
	if f.FoldersImplemented {
		caps[provider.CapFoldersRead] = provider.CapImplemented
	} else if f.FoldersSupportedNotImpl {
		caps[provider.CapFoldersRead] = provider.CapSupportedNotImpl
	} else if f.FoldersUnsupported {
		caps[provider.CapFoldersRead] = provider.CapUnsupported
	}
	if f.FoldersMutateImplemented {
		caps[provider.CapFoldersMutate] = provider.CapImplemented
	} else if f.FoldersMutateSupportedNotImpl {
		caps[provider.CapFoldersMutate] = provider.CapSupportedNotImpl
	}
	if f.MailboxImplemented {
		caps[provider.CapMessagesSearch] = provider.CapImplemented
		caps[provider.CapMessagesList] = provider.CapImplemented
	}
	if f.ReadImplemented {
		caps[provider.CapMessagesRead] = provider.CapImplemented
		caps[provider.CapThreadsRead] = provider.CapImplemented
		caps[provider.CapMessagesSearch] = provider.CapImplemented
		caps[provider.CapMessagesList] = provider.CapImplemented
	}
	if f.ThreadsImplemented {
		caps[provider.CapThreadsRead] = provider.CapImplemented
	}
	if f.DraftImplemented {
		caps[provider.CapDraftCreate] = provider.CapImplemented
		caps[provider.CapDraftRead] = provider.CapImplemented
		caps[provider.CapDraftUpdate] = provider.CapImplemented
		caps[provider.CapDraftSend] = provider.CapImplemented
		caps[provider.CapSendDirect] = provider.CapImplemented
	}
	if f.ReplyImplemented {
		caps[provider.CapReplyDraftCreate] = provider.CapImplemented
	}
	if f.ForwardImplemented {
		caps[provider.CapForwardDraftCreate] = provider.CapImplemented
	}
	if f.AttachmentsImplemented {
		caps[provider.CapAttachmentsMutate] = provider.CapImplemented
	}
	if f.AttachmentsReadImplemented {
		caps[provider.CapAttachmentsRead] = provider.CapImplemented
	}
	if f.MutationsImplemented {
		caps[provider.CapMessagesMove] = provider.CapImplemented
		caps[provider.CapMessagesCopy] = provider.CapImplemented
		caps[provider.CapMessagesArchive] = provider.CapImplemented
		caps[provider.CapMessagesDelete] = provider.CapImplemented
		caps[provider.CapMessagesRestore] = provider.CapImplemented
		caps[provider.CapMessagesJunk] = provider.CapImplemented
		caps[provider.CapMessagesMarkRead] = provider.CapImplemented
		caps[provider.CapMessagesFlag] = provider.CapImplemented
	}
	if f.CategoriesImplemented {
		caps[provider.CapCategoriesRead] = provider.CapImplemented
		caps[provider.CapCategoriesMutate] = provider.CapImplemented
	}
	if f.RulesReadImplemented {
		caps[provider.CapRulesRead] = provider.CapImplemented
	} else if f.RulesReadSupportedNotImpl {
		caps[provider.CapRulesRead] = provider.CapSupportedNotImpl
	}
	if f.RulesMutateImplemented {
		caps[provider.CapRulesMutate] = provider.CapImplemented
	} else if f.RulesMutateSupportedNotImpl {
		caps[provider.CapRulesMutate] = provider.CapSupportedNotImpl
	}
	if f.ArchiveReportImplemented {
		caps[provider.CapArchivePolicyReport] = provider.CapImplemented
	} else if f.ArchiveReportSupportedNotImpl {
		caps[provider.CapArchivePolicyReport] = provider.CapSupportedNotImpl
	} else if f.ArchiveReportUnsupported {
		caps[provider.CapArchivePolicyReport] = provider.CapUnsupported
	}
	return caps, nil
}

func (f *FakeProvider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	switch ac.Operation {
	case provider.InteractiveRepair:
		f.RepairCalls.Add(1)
	default:
		f.LoginCalls.Add(1)
	}
	if err := ac.Runtime.OpenBrowser(ctx, "https://fake.invalid/auth"); err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	ident := f.identity()
	ident.Assurance = provider.IdentityProviderVerified
	return provider.InteractiveAuthResult{
		Setup:      ac.Setup.Clone(),
		Credential: f.cache(ac.Account),
		Identity:   ident,
	}, nil
}

func (f *FakeProvider) LocalHealth(ctx context.Context, ac provider.StoredAuthContext) (provider.LocalStatus, error) {
	status := provider.LocalUsable
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		if lease.Current() == nil {
			status = provider.LocalRepairRequired
		}
		return nil
	})
	if err != nil {
		return provider.LocalRepairRequired, nil
	}
	return status, nil
}

func (f *FakeProvider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	f.SilentRefreshCalls.Add(1)
	rotated := false
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*TokenCache)
		if !ok || cache == nil || cache.Destroyed() {
			return errs.New(errs.CodeAuthRepairRequired, fmt.Sprintf(
				"the saved login for %q cannot be used; run maildo auth repair %s", ac.Account, ac.Account))
		}
		if f.RotateOnRefresh {
			next := cloneFakeCache(cache)
			next.RefreshToken = cache.RefreshToken + "-rotated"
			next.AccessToken = cache.AccessToken + "-refreshed"
			next.ExpiresAt = time.Unix(1893456000, 0)
			rotated = true
			return lease.Replace(next)
		}
		return nil
	})
	if err != nil {
		return provider.RefreshResult{}, err
	}
	return provider.RefreshResult{
		RefreshChecked:      true,
		CredentialChanged:   rotated,
		RotatedRefreshToken: rotated,
	}, nil
}

func (f *FakeProvider) Logout(context.Context, provider.StoredAuthContext) error {
	f.LogoutCalls.Add(1)
	return nil
}

// FakeAccountClient is the smallest logged-in client. Tests use it to check
// that a client gets opened, without any mailbox behavior.
type FakeAccountClient struct {
	Account  string
	Provider string
	lifetime *fakeClientLifetime
}

func (c FakeAccountClient) ProviderName() string { return c.Provider }

func (c FakeAccountClient) AccountName() string { return c.Account }

func (c FakeAccountClient) Destroy() { c.lifetime.destroy() }

// FakeArchivePolicyClient implements only AccountClient and
// ArchivePolicyReporter. The report reads the folder itself, so the client
// needs no FolderReader.
type FakeArchivePolicyClient struct {
	Account     string
	Provider    string
	Report      mail.ArchivePolicyReport
	Err         error
	ReportCalls *atomic.Int64
	lifetime    *fakeClientLifetime
}

func (c FakeArchivePolicyClient) ProviderName() string { return c.Provider }

func (c FakeArchivePolicyClient) AccountName() string { return c.Account }

func (c FakeArchivePolicyClient) Destroy() { c.lifetime.destroy() }

func (c FakeArchivePolicyClient) ArchivePolicyReport(_ context.Context, _ mail.ArchivePolicyReportRequest) (mail.ArchivePolicyReport, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.ArchivePolicyReport{}, err
	}
	if c.ReportCalls != nil {
		c.ReportCalls.Add(1)
	}
	if c.Err != nil {
		return mail.ArchivePolicyReport{}, c.Err
	}
	report := cloneArchivePolicyReport(c.Report)
	report.Account = c.Account
	report.Provider = c.Provider
	if report.Warnings == nil {
		report.Warnings = []mail.ArchivePolicyWarning{}
	}
	return report, nil
}

// cloneArchivePolicyReport prevents command tests from mutating shared report
// fixtures through the warning slice or folder pointer.
func cloneArchivePolicyReport(in mail.ArchivePolicyReport) mail.ArchivePolicyReport {
	out := in
	out.Warnings = append([]mail.ArchivePolicyWarning(nil), in.Warnings...)
	if in.ArchiveFolder != nil {
		folder := *in.ArchiveFolder
		out.ArchiveFolder = &folder
	}
	return out
}

// FakeFolderClient is a folder-capable account client used by CLI tests.
type FakeFolderClient struct {
	Account   string
	Provider  string
	Folders   mail.FolderList
	ListCalls *atomic.Int64
	Err       error
	record    func(kind, account, id string)
	lifetime  *fakeClientLifetime
}

func (c FakeFolderClient) ProviderName() string { return c.Provider }

func (c FakeFolderClient) AccountName() string { return c.Account }

func (c FakeFolderClient) Destroy() { c.lifetime.destroy() }

// ListFolders returns fixture folders labelled with the account and provider
// being exercised, so shared fixtures can cover single and multi-account tests.
func (c FakeFolderClient) ListFolders(_ context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderList{}, err
	}
	if c.ListCalls != nil {
		c.ListCalls.Add(1)
	}
	if c.record != nil {
		c.record("folder_list", c.Account, "")
	}
	if c.Err != nil {
		return mail.FolderList{}, c.Err
	}
	out := make([]mail.Folder, len(c.Folders.Folders))
	for i, f := range c.Folders.Folders {
		f.Account = c.Account
		f.Provider = c.Provider
		out[i] = f
	}
	if req.Detail == mail.FolderListDetailSelected {
		byID := make(map[string]mail.Folder, len(out))
		for _, folder := range out {
			byID[folder.ID] = folder
		}
		selected := make([]mail.Folder, 0, len(req.DetailIDs))
		for _, id := range req.DetailIDs {
			if folder, ok := byID[id]; ok {
				selected = append(selected, folder)
			}
		}
		out = selected
	}
	return mail.FolderList{Folders: out}, nil
}

// FakeFolderMutationClient embeds folder reads and implements stateless folder
// writes for command tests.
type FakeFolderMutationClient struct {
	FakeFolderClient
	CreateCalls          *atomic.Int64
	RenamePreflightCalls *atomic.Int64
	DeletePreflightCalls *atomic.Int64
	RenameCalls          *atomic.Int64
	DeleteCalls          *atomic.Int64
	record               func(kind, account, id string)
}

// PrepareFolderRename records that the guard was reached and returns an
// unprotected preflight. Provider-managed protection is covered by provider
// specific fixtures instead of this generic fake.
func (c FakeFolderMutationClient) PrepareFolderRename(_ context.Context, req mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if c.RenamePreflightCalls != nil {
		c.RenamePreflightCalls.Add(1)
	}
	c.recordTarget("rename_preflight", req.Target.ID)
	target := mail.SummaryFrom(req.Target)
	return mail.FolderMutationPreflight{
		Operation:    mail.FolderOperationRename,
		Target:       target,
		ProviderMeta: fakeFolderMeta(mail.MetadataFolderMutationPreflight),
	}, nil
}

// PrepareFolderDelete reloads the target from fixture state by provider id so
// delete tests exercise refreshed counts rather than the resolver snapshot.
func (c FakeFolderMutationClient) PrepareFolderDelete(_ context.Context, req mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if c.DeletePreflightCalls != nil {
		c.DeletePreflightCalls.Add(1)
	}
	c.recordTarget("delete_preflight", req.Target.ID)
	target, ok := c.folderByID(req.Target.ID)
	if !ok {
		return mail.FolderMutationPreflight{}, errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
			"account":   c.Account,
			"provider":  c.Provider,
			"folder_id": req.Target.ID,
		})
	}
	summary := mail.SummaryFrom(target)
	return mail.FolderMutationPreflight{
		Operation:           mail.FolderOperationDelete,
		Target:              summary,
		TotalItemCount:      target.TotalCount,
		TotalItemCountKnown: true,
		ChildFolderCount:    target.ChildCount,
		ChildFolderKnown:    true,
		CountsSource:        mail.FolderCountsSourceRefreshed,
		ProviderMeta:        fakeFolderMeta(mail.MetadataFolderMutationPreflight),
	}, nil
}

// CreateFolder returns a deterministic provider-style id without mutating the
// fixture tree. The fake is meant to verify command wiring and output shape, not
// act as an in-memory folder database.
func (c FakeFolderMutationClient) CreateFolder(_ context.Context, req mail.FolderCreateRequest) (mail.FolderMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if c.CreateCalls != nil {
		c.CreateCalls.Add(1)
	}
	c.recordTarget("create", req.DisplayName)
	created := mail.FolderSummary{
		ID:          "fake-folder-" + folderIDSlug(req.DisplayName),
		DisplayName: req.DisplayName,
		Path:        req.DisplayName,
	}
	if req.Parent != nil {
		created.Path = joinFolderPath(req.Parent.Path, req.DisplayName)
	}
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationCreate,
		ParentFolder:       cloneFolderSummary(req.Parent),
		CreatedFolder:      &created,
		NewDisplayName:     req.DisplayName,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		ProviderMeta:       fakeFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

// RenameFolder keeps the folder id and builds the new path from the parent's
// path, as the real providers report it.
func (c FakeFolderMutationClient) RenameFolder(_ context.Context, req mail.FolderRenameRequest) (mail.FolderMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if c.RenameCalls != nil {
		c.RenameCalls.Add(1)
	}
	c.recordTarget("rename", req.Target.ID)
	target := mail.SummaryFrom(req.Target)
	renamed := mail.FolderSummary{
		ID:          req.Target.ID,
		DisplayName: req.NewDisplayName,
		Path:        joinFolderPath(req.Target.ParentPath, req.NewDisplayName),
	}
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationRename,
		TargetFolder:       &target,
		RenamedFolder:      &renamed,
		OldDisplayName:     req.Target.DisplayName,
		NewDisplayName:     req.NewDisplayName,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		ProviderMeta:       fakeFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

// DeleteFolder echoes the preflight counts into the applied result so tests can
// prove the command passed refreshed count data through to the provider call.
func (c FakeFolderMutationClient) DeleteFolder(_ context.Context, req mail.FolderDeleteRequest) (mail.FolderMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if c.DeleteCalls != nil {
		c.DeleteCalls.Add(1)
	}
	c.recordTarget("delete", req.Target.ID)
	target := mail.SummaryFrom(req.Target)
	return mail.FolderMutationResult{
		Operation:           mail.FolderOperationDelete,
		TargetFolder:        &target,
		DeletedFolder:       &target,
		TotalItemCount:      req.Preflight.TotalItemCount,
		TotalItemCountKnown: req.Preflight.TotalItemCountKnown,
		ChildFolderCount:    req.Preflight.ChildFolderCount,
		ChildFolderKnown:    req.Preflight.ChildFolderKnown,
		CountsSource:        req.Preflight.CountsSource,
		VerificationStatus:  mail.FolderVerificationProviderResponse,
		ProviderMeta:        fakeFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

func (c FakeFolderMutationClient) recordTarget(kind, id string) {
	if c.record != nil {
		c.record(kind, c.Account, id)
	}
}

func (c FakeFolderMutationClient) folderByID(id string) (mail.Folder, bool) {
	for _, folder := range c.Folders.Folders {
		if folder.ID == id {
			folder.Account = c.Account
			folder.Provider = c.Provider
			if folder.ProviderMeta == nil {
				folder.ProviderMeta = fakeFolderMeta(mail.MetadataFolder)
			}
			return folder, true
		}
	}
	return mail.Folder{}, false
}

func cloneFolderSummary(in *mail.FolderSummary) *mail.FolderSummary {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func fakeFolderMeta(use mail.ProviderMetadataUse) *mail.FolderProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(use, mail.FolderProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeSummaryMeta() *mail.SummaryProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataMessageSummary, mail.SummaryProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeMessageMeta() *mail.MessageProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataMessage, mail.MessageProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeThreadMeta() *mail.ThreadProviderMetadata {
	complete := true
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataThread, mail.ThreadProviderMetadata{Basis: mail.ThreadBasisGmailNative, Complete: &complete})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeAttachmentMeta(use mail.ProviderMetadataUse) *mail.AttachmentProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(use, mail.AttachmentProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeMutationMeta() *mail.MutationProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataMessageMutation, mail.MutationProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func fakeDraftMeta(use mail.ProviderMetadataUse) *mail.DraftProviderMetadata {
	meta, err := mail.NewFakeProviderMetadataForTest(use, mail.DraftProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		panic(err)
	}
	return meta
}

func joinFolderPath(parentPath, name string) string {
	if parentPath == "" {
		return name
	}
	return parentPath + "/" + name
}

func folderIDSlug(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// FakeRuleClient implements folder and rule reads for CLI tests.
type FakeRuleClient struct {
	FakeFolderClient
	Rules             []mail.Rule
	ListSnapshot      []mail.Rule
	GetSnapshot       []mail.Rule
	SeparateListRules bool
	SeparateGetRules  bool
	Err               error
	ListErr           error
	GetErr            error
	ListCalls         *atomic.Int64
	GetCalls          *atomic.Int64
	recordRuleRead    func(kind, account, id string)
}

// ListRules returns fixture rules labelled with the account and provider being
// exercised.
func (c FakeRuleClient) ListRules(_ context.Context, req mail.RuleListRequest) (mail.RuleListResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.RuleListResult{}, err
	}
	if c.ListCalls != nil {
		c.ListCalls.Add(1)
	}
	if c.recordRuleRead != nil {
		c.recordRuleRead("rule_list", c.Account, "")
	}
	if c.ListErr != nil {
		return mail.RuleListResult{}, c.ListErr
	}
	if c.Err != nil {
		return mail.RuleListResult{}, c.Err
	}
	rules := c.Rules
	if c.SeparateListRules {
		rules = c.ListSnapshot
	}
	out := c.stampedRules(rules, req.Scope)
	return mail.RuleListResult{Rules: out}, nil
}

// GetRule returns one fixture rule by provider rule id.
func (c FakeRuleClient) GetRule(_ context.Context, req mail.RuleGetRequest) (mail.Rule, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Rule{}, err
	}
	if c.GetCalls != nil {
		c.GetCalls.Add(1)
	}
	if c.recordRuleRead != nil {
		c.recordRuleRead("rule_get", c.Account, req.RuleID)
	}
	if c.GetErr != nil {
		return mail.Rule{}, c.GetErr
	}
	if c.Err != nil {
		return mail.Rule{}, c.Err
	}
	scope := req.Scope
	if scope == "" {
		scope = mail.RuleScopeInbox
	}
	rules := c.Rules
	if c.SeparateGetRules {
		rules = c.GetSnapshot
	}
	for _, rule := range c.stampedRules(rules, scope) {
		if rule.ID == req.RuleID {
			return rule, nil
		}
	}
	return mail.Rule{}, errs.NewWithDetails(errs.CodeRuleNotFound, "rule not found", map[string]any{
		"account":  c.Account,
		"provider": c.Provider,
		"scope":    string(scope),
		"rule_id":  req.RuleID,
	})
}

// stampedRules applies the selected account, provider, and scope labels before
// returning cloned fixtures to command tests.
func (c FakeRuleClient) stampedRules(rules []mail.Rule, scope mail.RuleScope) []mail.Rule {
	if scope == "" {
		scope = mail.RuleScopeInbox
	}
	out := make([]mail.Rule, len(rules))
	for i, rule := range rules {
		rule.Account = c.Account
		rule.Provider = c.Provider
		rule.Scope = scope
		if rule.Conditions.Semantics.Families == "" && rule.Conditions.Semantics.RepeatedValues == "" {
			rule.Conditions.Semantics = mail.DefaultRulePredicateSemantics()
		}
		if rule.Exceptions.Semantics.Families == "" && rule.Exceptions.Semantics.RepeatedValues == "" {
			rule.Exceptions.Semantics = mail.DefaultRulePredicateSemantics()
		}
		if rule.ProviderMeta == nil {
			rule.ProviderMeta = map[string]any{"source": "fake"}
		}
		out[i] = cloneRule(rule)
	}
	return out
}

// RuleState stores rule mutations across account-client instances.
type RuleState struct {
	mu       sync.Mutex
	rules    map[string]map[string]mail.Rule
	nextRule atomic.Int64
}

// NewRuleState clones fixture rules so rule mutation tests can change state
// without altering shared sample fixtures.
func NewRuleState(fixtures map[string][]mail.Rule) *RuleState {
	s := &RuleState{rules: map[string]map[string]mail.Rule{}}
	count := int64(0)
	for account, rules := range fixtures {
		s.rules[account] = map[string]mail.Rule{}
		for _, rule := range rules {
			rule = cloneRule(rule)
			if rule.ID == "" {
				count++
				rule.ID = fmt.Sprintf("fake-rule-seed-%d", count)
			}
			s.rules[account][rule.ID] = rule
		}
	}
	s.nextRule.Store(count)
	return s
}

func (s *RuleState) snapshot(account string) []mail.Rule {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]mail.Rule, 0, len(s.rules[account]))
	for _, rule := range s.rules[account] {
		rows = append(rows, cloneRule(rule))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Sequence == rows[j].Sequence {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Sequence < rows[j].Sequence
	})
	return rows
}

func (s *RuleState) get(account, id string) (mail.Rule, error) {
	if s == nil {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule not found")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.rules[account][id]
	if !ok {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule not found")
	}
	return cloneRule(rule), nil
}

func (s *RuleState) create(account string, req mail.RuleCreateRequest) (mail.Rule, error) {
	if s == nil {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rules[account] == nil {
		s.rules[account] = map[string]mail.Rule{}
	}
	rule := cloneRule(req.Rule)
	if rule.ID == "" {
		rule.ID = fmt.Sprintf("fake-rule-%d", s.nextRule.Add(1))
	}
	rule.ProviderMeta = fakeRuleReadMeta()
	s.rules[account][rule.ID] = rule
	return cloneRule(rule), nil
}

func (s *RuleState) update(account string, req mail.RuleUpdateRequest) (mail.Rule, error) {
	if s == nil {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[account][req.RuleID]; !ok {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule not found")
	}
	rule := cloneRule(req.Expected)
	if rule.ID == "" {
		rule.ID = req.RuleID
	}
	rule.ProviderMeta = fakeRuleReadMeta()
	s.rules[account][rule.ID] = rule
	return cloneRule(rule), nil
}

func (s *RuleState) setProviderStatus(account, id string, hasError, readOnly bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.rules[account][id]
	if !ok {
		return
	}
	rule.HasError = hasError
	rule.IsReadOnly = readOnly
	s.rules[account][id] = rule
}

func (s *RuleState) setSequence(account, id string, sequence int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.rules[account][id]
	if !ok {
		return
	}
	rule.Sequence = sequence
	s.rules[account][id] = rule
}

func (s *RuleState) delete(account, id string) (mail.Rule, error) {
	if s == nil {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.rules[account][id]
	if !ok {
		return mail.Rule{}, errs.New(errs.CodeRuleNotFound, "rule not found")
	}
	delete(s.rules[account], id)
	return cloneRule(rule), nil
}

// FakeRuleMutationClient embeds rule reads and applies writes to shared state.
type FakeRuleMutationClient struct {
	FakeRuleClient
	state          *RuleState
	errByOperation map[mail.RuleMutationOperation]error
	errByAccount   map[string]error
	errByRule      map[string]error
	getErr         error
	receiptOp      mail.RuleMutationOperation
	receiptMeta    mail.RuleMutationProviderMetadata
	skipUpdate     bool
	postHasError   bool
	postReadOnly   bool
	postSequence   *int
	CreateCalls    *atomic.Int64
	UpdateCalls    *atomic.Int64
	DeleteCalls    *atomic.Int64
	record         func(kind, account, id string)
}

// fakeRuleMutationNoReader advertises writes but deliberately omits rule reads
// so command tests can exercise a stale capability/client combination.
type fakeRuleMutationNoReader struct{ inner FakeRuleMutationClient }

func (c fakeRuleMutationNoReader) ProviderName() string { return c.inner.ProviderName() }
func (c fakeRuleMutationNoReader) AccountName() string  { return c.inner.AccountName() }
func (c fakeRuleMutationNoReader) Destroy()             { c.inner.Destroy() }
func (c fakeRuleMutationNoReader) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	return c.inner.ListFolders(ctx, req)
}
func (c fakeRuleMutationNoReader) CreateRule(ctx context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error) {
	return c.inner.CreateRule(ctx, req)
}
func (c fakeRuleMutationNoReader) UpdateRule(ctx context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error) {
	return c.inner.UpdateRule(ctx, req)
}
func (c fakeRuleMutationNoReader) DeleteRule(ctx context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error) {
	return c.inner.DeleteRule(ctx, req)
}

// fakeRuleMutationNoFolder advertises rule mutation but deliberately omits the
// folder reader required to resolve and verify folder actions.
type fakeRuleMutationNoFolder struct{ inner FakeRuleMutationClient }

func (c fakeRuleMutationNoFolder) ProviderName() string { return c.inner.ProviderName() }
func (c fakeRuleMutationNoFolder) AccountName() string  { return c.inner.AccountName() }
func (c fakeRuleMutationNoFolder) Destroy()             { c.inner.Destroy() }
func (c fakeRuleMutationNoFolder) ListRules(ctx context.Context, req mail.RuleListRequest) (mail.RuleListResult, error) {
	return c.inner.ListRules(ctx, req)
}
func (c fakeRuleMutationNoFolder) GetRule(ctx context.Context, req mail.RuleGetRequest) (mail.Rule, error) {
	return c.inner.GetRule(ctx, req)
}
func (c fakeRuleMutationNoFolder) CreateRule(ctx context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error) {
	return c.inner.CreateRule(ctx, req)
}
func (c fakeRuleMutationNoFolder) UpdateRule(ctx context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error) {
	return c.inner.UpdateRule(ctx, req)
}
func (c fakeRuleMutationNoFolder) DeleteRule(ctx context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error) {
	return c.inner.DeleteRule(ctx, req)
}

func (c FakeRuleMutationClient) ListRules(ctx context.Context, req mail.RuleListRequest) (mail.RuleListResult, error) {
	c.FakeRuleClient.Rules = c.state.snapshot(c.Account)
	return c.FakeRuleClient.ListRules(ctx, req)
}

func (c FakeRuleMutationClient) GetRule(ctx context.Context, req mail.RuleGetRequest) (mail.Rule, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Rule{}, err
	}
	if c.GetCalls != nil {
		c.GetCalls.Add(1)
	}
	if c.recordRuleRead != nil {
		c.recordRuleRead("rule_get", c.Account, req.RuleID)
	}
	if c.getErr != nil {
		return mail.Rule{}, c.getErr
	}
	rule, err := c.state.get(c.Account, req.RuleID)
	if err != nil {
		return mail.Rule{}, provider.NewRuleReadFailureError(provider.RuleReadFailureNotFound, c.ruleNotFound(req.RuleID))
	}
	_ = ctx
	rows := c.FakeRuleClient.stampedRules([]mail.Rule{rule}, req.Scope)
	return rows[0], nil
}

func (c FakeRuleMutationClient) CreateRule(_ context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.RuleMutationResult{}, err
	}
	if c.CreateCalls != nil {
		c.CreateCalls.Add(1)
	}
	c.recordTarget("create", req.Rule.ID)
	if err := c.injectedError(mail.RuleOperationCreate, req.Rule.ID); err != nil {
		return mail.RuleMutationResult{}, err
	}
	rule, err := c.state.create(c.Account, req)
	if err != nil {
		return mail.RuleMutationResult{}, c.ruleNotFound(req.Rule.ID)
	}
	if c.postHasError || c.postReadOnly {
		c.state.setProviderStatus(c.Account, rule.ID, c.postHasError, c.postReadOnly)
	}
	if c.postSequence != nil {
		c.state.setSequence(c.Account, rule.ID, *c.postSequence)
	}
	return mail.RuleMutationResult{
		Operation:        c.receiptOperation(mail.RuleOperationCreate),
		RuleID:           rule.ID,
		ProviderMetadata: c.receiptMetadata(),
	}, nil
}

func (c FakeRuleMutationClient) UpdateRule(_ context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.RuleMutationResult{}, err
	}
	if c.UpdateCalls != nil {
		c.UpdateCalls.Add(1)
	}
	c.recordTarget(string(req.ChangeSet.Operation), req.RuleID)
	if err := c.injectedError(req.ChangeSet.Operation, req.RuleID); err != nil {
		return mail.RuleMutationResult{}, err
	}
	rule := cloneRule(req.Existing)
	if !c.skipUpdate {
		var err error
		rule, err = c.state.update(c.Account, req)
		if err != nil {
			return mail.RuleMutationResult{}, c.ruleNotFound(req.RuleID)
		}
	}
	if c.postHasError || c.postReadOnly {
		c.state.setProviderStatus(c.Account, req.RuleID, c.postHasError, c.postReadOnly)
	}
	if c.postSequence != nil {
		c.state.setSequence(c.Account, req.RuleID, *c.postSequence)
	}
	return mail.RuleMutationResult{
		Operation:        c.receiptOperation(req.ChangeSet.Operation),
		RuleID:           rule.ID,
		ProviderMetadata: c.receiptMetadata(),
	}, nil
}

func (c FakeRuleMutationClient) DeleteRule(_ context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.RuleDeleteResult{}, err
	}
	if c.DeleteCalls != nil {
		c.DeleteCalls.Add(1)
	}
	c.recordTarget("delete", req.RuleID)
	if err := c.injectedError(mail.RuleOperationDelete, req.RuleID); err != nil {
		return mail.RuleDeleteResult{}, err
	}
	if _, err := c.state.delete(c.Account, req.RuleID); err != nil {
		return mail.RuleDeleteResult{}, c.ruleNotFound(req.RuleID)
	}
	return mail.RuleDeleteResult{Operation: c.receiptOperation(mail.RuleOperationDelete), RuleID: req.RuleID, ProviderMetadata: c.receiptMetadata()}, nil
}

func (c FakeRuleMutationClient) receiptOperation(fallback mail.RuleMutationOperation) mail.RuleMutationOperation {
	if c.receiptOp != "" {
		return c.receiptOp
	}
	return fallback
}

func (c FakeRuleMutationClient) receiptMetadata() mail.RuleMutationProviderMetadata {
	if c.receiptMeta.Source != "" {
		return c.receiptMeta
	}
	return fakeRuleMutationMeta()
}

func (c FakeRuleMutationClient) injectedError(op mail.RuleMutationOperation, id string) error {
	if c.errByOperation != nil {
		if err := c.errByOperation[op]; err != nil {
			return err
		}
	}
	if c.errByAccount != nil {
		if err := c.errByAccount[c.Account]; err != nil {
			return err
		}
	}
	if c.errByRule != nil {
		if err := c.errByRule[id]; err != nil {
			return err
		}
	}
	return nil
}

func (c FakeRuleMutationClient) recordTarget(kind, id string) {
	if c.record != nil {
		c.record(kind, c.Account, id)
	}
}

func (c FakeRuleMutationClient) ruleNotFound(id string) error {
	return errs.NewWithDetails(errs.CodeRuleNotFound, "rule not found", map[string]any{
		"account":  c.Account,
		"provider": c.Provider,
		"scope":    string(mail.RuleScopeInbox),
		"rule_id":  id,
	})
}

func fakeRuleReadMeta() map[string]any {
	return map[string]any{"source": "fake"}
}

func fakeRuleMutationMeta() mail.RuleMutationProviderMetadata {
	metadata, err := mail.NewRuleMutationProviderMetadata(mail.RuleMutationSourceFake)
	if err != nil {
		panic(err)
	}
	return metadata
}

// FakeMailboxClient implements folder and message read interfaces for CLI tests.
// It proves account plumbing, output, merge, sort, limit, and unread behavior;
// provider-specific query and folder-selection semantics are covered by adapter
// fixtures instead.
type FakeMailboxClient struct {
	Account  string
	Provider string
	Folders  mail.FolderList
	Messages []mail.MessageSummary
	Err      error
	lifetime *fakeClientLifetime
}

func (c FakeMailboxClient) ProviderName() string { return c.Provider }

func (c FakeMailboxClient) AccountName() string { return c.Account }

func (c FakeMailboxClient) Destroy() { c.lifetime.destroy() }

func (c FakeMailboxClient) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	return FakeFolderClient{Account: c.Account, Provider: c.Provider, Folders: c.Folders, lifetime: c.lifetime}.ListFolders(ctx, req)
}

// SearchMessages returns the account's fixture rows sorted and limited. It does
// not interpret req.Query; semantic query translation belongs in provider
// fixture tests such as the Outlook Graph tests. Without a folder it keeps
// the folder roles a real provider searches, judged by each row's
// Folder.WellKnownName.
func (c FakeMailboxClient) SearchMessages(_ context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageSummaryList{}, err
	}
	if c.Err != nil {
		return mail.MessageSummaryList{}, c.Err
	}
	rows := c.stampedMessages()
	if req.Folder == nil {
		rows = scopeRowsByRole(rows, req.SkipRoles, req.OnlyRoles)
	} else {
		inFolder := rows[:0]
		for _, row := range rows {
			if row.Folder.ID == req.Folder.ID {
				inFolder = append(inFolder, row)
			}
		}
		rows = inFolder
	}
	sortMessages(rows)
	return pageMessages(rows, req.Offset, req.Limit), nil
}

// scopeRowsByRole applies mail.SearchRequest's folder roles to fixture rows:
// Trash and Junk are left out unless only names them.
func scopeRowsByRole(rows []mail.MessageSummary, skip, only []string) []mail.MessageSummary {
	has := func(roles []string, role string) bool {
		for _, r := range roles {
			if r == role {
				return true
			}
		}
		return false
	}
	out := rows[:0]
	for _, row := range rows {
		role := row.Folder.WellKnownName
		switch {
		case len(only) > 0:
			if !has(only, role) {
				continue
			}
		case role == mail.WellKnownDeleted, role == mail.WellKnownJunk, role != "" && has(skip, role):
			continue
		}
		out = append(out, row)
	}
	return out
}

// ListMessages applies the generic unread filter and stamps every returned row
// with the resolved folder so CLI tests can assert folder output consistently.
func (c FakeMailboxClient) ListMessages(_ context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageSummaryList{}, err
	}
	if c.Err != nil {
		return mail.MessageSummaryList{}, c.Err
	}
	rows := c.stampedMessages()
	out := make([]mail.MessageSummary, 0, len(rows))
	folder := mail.SummaryFrom(req.Folder)
	for _, m := range rows {
		if req.Filter.Unread && m.IsRead {
			continue
		}
		m.Folder = folder
		out = append(out, m)
	}
	sortMessages(out)
	return pageMessages(out, req.Offset, req.Limit), nil
}

// pageMessages returns rows offset..offset+limit with the full count, as a
// provider that knows its totals does.
func pageMessages(rows []mail.MessageSummary, offset, limit int) mail.MessageSummaryList {
	total := len(rows)
	return mail.MessageSummaryList{Messages: limitMessages(mail.DropRows(rows, offset), limit), Total: total, TotalKnown: true}
}

func (c FakeMailboxClient) stampedMessages() []mail.MessageSummary {
	out := make([]mail.MessageSummary, len(c.Messages))
	for i, m := range c.Messages {
		m.Account = c.Account
		m.Provider = c.Provider
		if m.ProviderMeta == nil {
			m.ProviderMeta = fakeSummaryMeta()
		}
		out[i] = m
	}
	return out
}

// FakeReadClient adds full-message and thread fixtures to the mailbox fake.
type FakeReadClient struct {
	Account                 string
	Provider                string
	Folders                 mail.FolderList
	Summaries               []mail.MessageSummary
	Messages                map[string]mail.Message
	Internet                map[string][]mail.Message
	Threads                 map[string]mail.Thread
	Err                     error
	Attachments             map[string][]mail.Attachment
	AttachmentContent       map[string]map[string]FakeAttachmentContent
	AttachmentListErr       map[string]error
	AttachmentDownloadErr   map[string]map[string]error
	MetadataOnlyReadCalls   *atomic.Int64
	AttachmentListCalls     *atomic.Int64
	AttachmentDownloadCalls *atomic.Int64
	lifetime                *fakeClientLifetime
}

// MutationState stores message mutations across account-client instances.
type MutationState struct {
	mu         sync.Mutex
	messages   map[string]map[string]mail.Message
	flags      map[string]map[string]bool
	nextCopyID atomic.Int64
}

// NewMutationState clones fixture messages so mutation tests can change state
// without altering the shared sample fixtures.
func NewMutationState(full map[string]map[string]mail.Message) *MutationState {
	s := &MutationState{messages: map[string]map[string]mail.Message{}, flags: map[string]map[string]bool{}}
	for account, messages := range full {
		s.messages[account] = cloneMessages(messages)
		s.flags[account] = map[string]bool{}
	}
	return s
}

// snapshot returns a cloned account view for read/search clients backed by the
// current mutation state.
// Add puts a new message in the mailbox, as if it just arrived.
func (s *MutationState) Add(account string, msg mail.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[account] == nil {
		s.messages[account] = map[string]mail.Message{}
		s.flags[account] = map[string]bool{}
	}
	s.messages[account][msg.ID] = msg
}

func (s *MutationState) snapshot(account string) map[string]mail.Message {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMessages(s.messages[account])
}

// summaries rebuilds search rows from the current full messages so query
// selectors see changes made by earlier fake mutations.
func (s *MutationState) summaries(account, providerName string) []mail.MessageSummary {
	messages := s.snapshot(account)
	out := make([]mail.MessageSummary, 0, len(messages))
	for _, msg := range messages {
		out = append(out, summaryFromMessage(account, providerName, msg))
	}
	sortMessages(out)
	return out
}

func (s *MutationState) folderMove(account, id string, folder mail.Folder) (mail.Message, mail.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages[account]
	if msgs == nil {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	msg, ok := msgs[id]
	if !ok {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	before := cloneMessage(msg)
	msg.Folder = mail.SummaryFrom(folder)
	msg.ProviderMeta = fakeMessageMeta()
	msgs[id] = msg
	return before, cloneMessage(msg), nil
}

func (s *MutationState) copyTo(account, id string, folder mail.Folder) (mail.Message, mail.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages[account]
	if msgs == nil {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	msg, ok := msgs[id]
	if !ok {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	before := cloneMessage(msg)
	copied := cloneMessage(msg)
	copied.ID = fmt.Sprintf("%s-copy-%d", id, s.nextCopyID.Add(1))
	copied.Folder = mail.SummaryFrom(folder)
	copied.ProviderMeta = fakeMessageMeta()
	msgs[copied.ID] = copied
	return before, copied, nil
}

func (s *MutationState) updateRead(account, id string, read bool) (mail.Message, mail.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages[account]
	if msgs == nil {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	msg, ok := msgs[id]
	if !ok {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	before := cloneMessage(msg)
	msg.IsRead = read
	msg.ProviderMeta = fakeMessageMeta()
	msgs[id] = msg
	return before, cloneMessage(msg), nil
}

func (s *MutationState) updateFlag(account, id string, flagged bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[account] == nil {
		return false, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	if _, ok := s.messages[account][id]; !ok {
		return false, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	if s.flags[account] == nil {
		s.flags[account] = map[string]bool{}
	}
	before := s.flags[account][id]
	s.flags[account][id] = flagged
	return before, nil
}

func (s *MutationState) categories(account, id string) (mail.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[account] == nil {
		return mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	msg, ok := s.messages[account][id]
	if !ok {
		return mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	return cloneMessage(msg), nil
}

func (s *MutationState) setCategories(account, id string, categories []string) (mail.Message, mail.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages[account]
	if msgs == nil {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	msg, ok := msgs[id]
	if !ok {
		return mail.Message{}, mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
	}
	before := cloneMessage(msg)
	msg.Categories = append([]string(nil), categories...)
	msg.ProviderMeta = fakeMessageMeta()
	msgs[id] = msg
	return before, cloneMessage(msg), nil
}

type FakeAttachmentContent struct {
	Bytes        []byte
	ContentType  string
	DeclaredSize int64
	ReadErr      error
	CloseErr     error
}

// DraftState is shared by fake draft clients so tests can inspect mutations.
type DraftState struct {
	mu                    sync.Mutex
	drafts                map[string]mail.Draft
	nextDraftID           atomic.Int64
	CreateCalls           atomic.Int64
	ReplyCreateCalls      atomic.Int64
	ForwardCreateCalls    atomic.Int64
	ReadCalls             atomic.Int64
	MetadataOnlyReadCalls atomic.Int64
	UpdateCalls           atomic.Int64
	SendCalls             atomic.Int64
	DirectCalls           atomic.Int64
	AttachmentAddCalls    atomic.Int64
	AttachmentRemoveCalls atomic.Int64
	nextAttachmentID      atomic.Int64
	FailReadDraft         atomic.Bool
	FailUpdateDraft       atomic.Bool
	FailSendDraft         atomic.Bool
	FailReplyAfterDraft   atomic.Bool
	FailForwardAfterDraft atomic.Bool
	FailRemoveAttachment  atomic.Bool
	FailAddAfter          atomic.Int64
	FailAddAfterAll       atomic.Bool
	SourceAttachments     map[string][]mail.Attachment
	// UpdateWarning, when set, comes back as a warning from draft updates,
	// as IMAP gives one when the old version could not be deleted.
	UpdateWarning string
	// DirectSendErr, when set, is what direct sends return instead of
	// sending.
	DirectSendErr error
}

// NewDraftState creates an in-memory draft store.
func NewDraftState(drafts ...mail.Draft) *DraftState {
	s := &DraftState{drafts: map[string]mail.Draft{}, SourceAttachments: map[string][]mail.Attachment{}}
	for i, d := range drafts {
		if d.ID == "" {
			d.ID = fmt.Sprintf("fake-draft-seed-%d", i+1)
		}
		s.drafts[d.ID] = cloneDraft(d)
	}
	s.nextDraftID.Store(int64(len(s.drafts)))
	return s
}

// Snapshot returns a copy of the current fake draft store.
func (s *DraftState) Snapshot() map[string]mail.Draft {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDrafts(s.drafts)
}

// FakeDraftClient implements draft lifecycle and direct send for CLI tests.
type FakeDraftClient struct {
	Account  string
	Provider string
	state    *DraftState
	lifetime *fakeClientLifetime
}

func (c FakeDraftClient) ProviderName() string { return c.Provider }

func (c FakeDraftClient) AccountName() string { return c.Account }

func (c FakeDraftClient) Destroy() { c.lifetime.destroy() }

func (c FakeDraftClient) CreateDraft(_ context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.DraftRef{}, err
	}
	c.state.CreateCalls.Add(1)
	id := fmt.Sprintf("fake-draft-%d", c.state.nextDraftID.Add(1))
	draft := c.draftFromOutbound(id, req.Message)
	c.state.mu.Lock()
	c.state.drafts[id] = draft
	c.state.mu.Unlock()
	return mail.DraftRef{ID: id, ProviderMeta: fakeDraftMeta(mail.MetadataDraftWrite)}, nil
}

func (c FakeDraftClient) ReadComposeSourceMaterial(_ context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	if err := c.lifetime.err(); err != nil {
		return provider.ComposeSourceMaterial{}, err
	}
	sourceID := selector.ID
	internetID := "<" + sanitizeMessageIDLocal(sourceID) + "@example.test>"
	if selector.Kind == mail.ResolvedSelectorInternetID {
		internetID = selector.InternetID
		sourceID = strings.Trim(selector.InternetID, "<>")
	}
	if sourceID == "" {
		return provider.ComposeSourceMaterial{}, errs.New(errs.CodeInvalidSelector, "fake compose source requires message id or internet id")
	}
	sent := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	return provider.ComposeSourceMaterial{
		SourceID:          sourceID,
		InternetMessageID: internetID,
		ThreadID:          "thread-" + sanitizeMessageIDLocal(sourceID),
		From:              mail.Recipient{Name: "Source Sender", Address: "source@example.test"},
		To:                []mail.Recipient{{Name: "Fake Account", Address: c.Provider + "@example.test"}},
		CC:                []mail.Recipient{{Address: "other@example.test"}},
		Subject:           "fake",
		SentAt:            &sent,
		Text:              []byte("original source line one\noriginal source line two"),
		HTML:              []byte("<p>original source line one<br>original source line two</p>"),
	}, nil
}

func sanitizeMessageIDLocal(value string) string {
	value = strings.Trim(value, "<> \t\r\n")
	if value == "" {
		return "source"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '+':
			b.WriteRune(r)
		}
		if b.Len() >= 48 {
			break
		}
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

func fakeReplyTo(source provider.ComposeSourceMaterial) []mail.Recipient {
	if len(source.ReplyTo) > 0 {
		return append([]mail.Recipient(nil), source.ReplyTo...)
	}
	if strings.TrimSpace(source.From.Address) != "" || strings.TrimSpace(source.From.Name) != "" {
		return []mail.Recipient{source.From}
	}
	return nil
}

func (c FakeDraftClient) ValidateReplyDraft(_ context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.ReplyDraftResult{}, err
	}
	var out mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		out = mail.ReplyDraftResult{
			SourceID:         source.SourceID,
			SourceInternetID: source.InternetMessageID,
			To:               fakeReplyTo(source),
			Subject:          "RE: " + source.Subject,
			BodyFormat:       req.Body.Format,
		}
		if req.Mode == mail.ReplyModeReplyAll {
			out.CC = append([]mail.Recipient(nil), source.CC...)
		}
		content := fakeReplyContent(mail.Draft{To: out.To, CC: out.CC, Subject: out.Subject, Body: messageBodyFromOutbound(req.Body)}, source)
		out.Send = &content
		return nil
	})
	return out, err
}

func (c FakeDraftClient) CreateReplyDraft(_ context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.ReplyDraftResult{}, err
	}
	c.state.ReplyCreateCalls.Add(1)
	id := fmt.Sprintf("fake-reply-%d", c.state.nextDraftID.Add(1))
	var out mail.ReplyDraftResult
	err := req.Source.Use(func(source provider.ComposeSourceMaterial) error {
		draft := mail.Draft{
			Account:      c.Account,
			Provider:     c.Provider,
			ID:           id,
			IsDraft:      true,
			Folder:       mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"},
			To:           fakeReplyTo(source),
			Subject:      "RE: " + source.Subject,
			Body:         messageBodyFromOutbound(req.Body),
			ProviderMeta: fakeDraftMeta(mail.MetadataDraft),
		}
		if req.Mode == mail.ReplyModeReplyAll {
			draft.CC = append([]mail.Recipient(nil), source.CC...)
		}
		sentID := ""
		if req.Action == mail.ComposeExecutionSend {
			if req.Check == nil {
				return errs.NewProviderClientContractInvalid()
			}
			if err := req.Check(fakeReplyContent(draft, source)); err != nil {
				return err
			}
			c.state.SendCalls.Add(1)
			draft.IsDraft = false
			sentID = "sent-" + id
		}
		c.state.mu.Lock()
		c.state.drafts[id] = draft
		c.state.mu.Unlock()
		out = mail.ReplyDraftResult{
			SourceID:         source.SourceID,
			SourceInternetID: source.InternetMessageID,
			DraftID:          id,
			SentID:           sentID,
			To:               draft.To,
			CC:               draft.CC,
			BCC:              draft.BCC,
			Subject:          draft.Subject,
			BodyFormat:       req.Body.Format,
			AttachmentCount:  len(draft.Attachments),
			ProviderMeta:     fakeDraftMeta(mail.MetadataDraftWrite),
		}
		if c.state.FailReplyAfterDraft.Load() {
			return rungrad.NewError(rungrad.ExitAPI, "fake reply reconciliation failed")
		}
		return nil
	})
	return out, err
}

func (c FakeDraftClient) ValidateForwardDraft(_ context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.ForwardDraftResult{}, err
	}
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(sourceMaterial provider.ComposeSourceMaterial) error {
		c.state.mu.Lock()
		source := append([]mail.Attachment(nil), c.state.SourceAttachments[sourceMaterial.SourceID]...)
		c.state.mu.Unlock()
		selected, excluded, err := fakeSelectForwardSourceAttachments(source, req.Attachments)
		if err != nil {
			return err
		}
		result = mail.ForwardDraftResult{
			SourceID:                  sourceMaterial.SourceID,
			SourceInternetID:          sourceMaterial.InternetMessageID,
			To:                        append([]mail.Recipient(nil), req.To...),
			CC:                        append([]mail.Recipient(nil), req.CC...),
			BCC:                       append([]mail.Recipient(nil), req.BCC...),
			Subject:                   "FW: " + sourceMaterial.Subject,
			BodyFormat:                req.Body.Format,
			SelectedSourceAttachments: selected,
			ExcludedUnsupported:       excluded,
		}
		content := fakeSendContent(mail.Draft{To: result.To, CC: result.CC, BCC: result.BCC, Subject: result.Subject, Body: messageBodyFromOutbound(req.Body), Attachments: selected})
		result.Send = &content
		return nil
	})
	return result, err
}

func (c FakeDraftClient) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.ValidateForwardDraft(ctx, req)
}

func (c FakeDraftClient) CreateForwardDraft(_ context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.ForwardDraftResult{}, err
	}
	c.state.ForwardCreateCalls.Add(1)
	id := fmt.Sprintf("fake-forward-%d", c.state.nextDraftID.Add(1))
	var result mail.ForwardDraftResult
	err := req.Source.Use(func(sourceMaterial provider.ComposeSourceMaterial) error {
		c.state.mu.Lock()
		source := append([]mail.Attachment(nil), c.state.SourceAttachments[sourceMaterial.SourceID]...)
		c.state.mu.Unlock()
		selected, excluded, err := fakeSelectForwardSourceAttachments(source, req.Attachments)
		if err != nil {
			return err
		}
		copied := make([]mail.Attachment, 0, len(source))
		for _, a := range source {
			a.ID = "copy-" + a.ID
			copied = append(copied, a)
		}
		final, err := fakeReconcileForwardAttachments(selected, copied)
		if err != nil {
			return err
		}
		draft := mail.Draft{
			Account:        c.Account,
			Provider:       c.Provider,
			ID:             id,
			IsDraft:        true,
			Folder:         mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"},
			To:             append([]mail.Recipient(nil), req.To...),
			CC:             append([]mail.Recipient(nil), req.CC...),
			BCC:            append([]mail.Recipient(nil), req.BCC...),
			Subject:        "FW: " + sourceMaterial.Subject,
			Body:           messageBodyFromOutbound(req.Body),
			HasAttachments: len(final) > 0,
			Attachments:    final,
			ProviderMeta:   fakeDraftMeta(mail.MetadataDraft),
		}
		sentID := ""
		if req.Action == mail.ComposeExecutionSend {
			if req.Check == nil {
				return errs.NewProviderClientContractInvalid()
			}
			sending := draft
			sending.Attachments = selected
			if err := req.Check(fakeSendContent(sending)); err != nil {
				return err
			}
			c.state.SendCalls.Add(1)
			draft.IsDraft = false
			sentID = "sent-" + id
		}
		c.state.mu.Lock()
		c.state.drafts[id] = draft
		c.state.mu.Unlock()
		result = mail.ForwardDraftResult{
			SourceID:                  sourceMaterial.SourceID,
			SourceInternetID:          sourceMaterial.InternetMessageID,
			DraftID:                   id,
			SentID:                    sentID,
			To:                        draft.To,
			CC:                        draft.CC,
			BCC:                       draft.BCC,
			Subject:                   draft.Subject,
			BodyFormat:                req.Body.Format,
			AttachmentCount:           len(final),
			SelectedSourceAttachments: selected,
			FinalAttachments:          final,
			ExcludedUnsupported:       excluded,
			ProviderMeta:              fakeDraftMeta(mail.MetadataDraftWrite),
		}
		if c.state.FailForwardAfterDraft.Load() {
			return rungrad.NewError(rungrad.ExitAPI, "fake forward reconciliation failed")
		}
		return nil
	})
	return result, err
}

func (c FakeDraftClient) ReadDraft(_ context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Draft{}, err
	}
	c.state.ReadCalls.Add(1)
	if req.MetadataOnly {
		c.state.MetadataOnlyReadCalls.Add(1)
	}
	if c.state.FailReadDraft.Load() {
		return mail.Draft{}, rungrad.NewError(rungrad.ExitAPI, "fake draft read failed")
	}
	c.state.mu.Lock()
	draft, ok := c.state.drafts[req.DraftID]
	c.state.mu.Unlock()
	if !ok || !draft.IsDraft {
		return mail.Draft{}, c.draftNotFound(req.DraftID)
	}
	draft = c.stampDraft(draft)
	if req.MetadataOnly {
		draft.Body = mail.MessageBody{}
		return draft, nil
	}
	if req.BodyMode != "" {
		draft.Body.Mode = req.BodyMode
	}
	if draft.Body.Mode == mail.BodyModeRaw && draft.Body.Encoding == "" {
		draft.Body.Encoding = "utf8"
	}
	return draft, nil
}

func (c FakeDraftClient) UpdateDraft(_ context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.DraftRef{}, err
	}
	c.state.UpdateCalls.Add(1)
	if c.state.FailUpdateDraft.Load() {
		return mail.DraftRef{}, rungrad.NewError(rungrad.ExitAPI, "fake draft update failed")
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return mail.DraftRef{}, c.draftNotFound(req.DraftID)
	}
	applyRecipients := func(p mail.RecipientPatch, dst *[]mail.Recipient) {
		switch {
		case p.Clear:
			*dst = nil
		case p.Set:
			*dst = append([]mail.Recipient(nil), p.Values...)
		}
	}
	applyRecipients(req.Patch.To, &draft.To)
	applyRecipients(req.Patch.CC, &draft.CC)
	applyRecipients(req.Patch.BCC, &draft.BCC)
	applyRecipients(req.Patch.ReplyTo, &draft.ReplyTo)
	if req.Patch.Subject.Set {
		draft.Subject = req.Patch.Subject.Value
	}
	if req.Patch.Body.Set {
		// Match the Outlook adapter: an explicit empty compose body clears the
		// draft as an empty text body, regardless of the selected input format.
		draft.Body = messageBodyFromOutbound(req.Patch.Body.Body)
	}
	draft.ProviderMeta = fakeDraftMeta(mail.MetadataDraft)
	c.state.drafts[req.DraftID] = draft
	ref := mail.DraftRef{ID: req.DraftID, ProviderMeta: fakeDraftMeta(mail.MetadataDraftWrite)}
	if c.state.UpdateWarning != "" {
		ref.Warnings = []string{c.state.UpdateWarning}
	}
	return ref, nil
}

func (c FakeDraftClient) ValidateDraftUpdate(_ context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.DraftUpdateValidation{}, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return mail.DraftUpdateValidation{}, c.draftNotFound(req.DraftID)
	}
	return mail.DraftUpdateValidation{}, nil
}

func (c FakeDraftClient) SendDraft(_ context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.SendOutcome{}, err
	}
	if req.Check == nil {
		return mail.SendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return mail.SendOutcome{}, c.draftNotFound(req.DraftID)
	}
	if err := req.Check(fakeSendContent(draft)); err != nil {
		return mail.SendOutcome{}, err
	}
	c.state.SendCalls.Add(1)
	if c.state.FailSendDraft.Load() {
		return mail.SendOutcome{}, rungrad.NewError(rungrad.ExitAPI, "fake draft send failed")
	}
	draft.IsDraft = false
	c.state.drafts[req.DraftID] = draft
	return mail.SendOutcome{MessageID: "sent-" + req.DraftID, ProviderMeta: fakeDraftMeta(mail.MetadataDraftSend)}, nil
}

func (c FakeDraftClient) ValidateDraftSend(_ context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Draft{}, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return mail.Draft{}, c.draftNotFound(req.DraftID)
	}
	content := fakeSendContent(draft)
	draft = c.stampDraft(draft)
	draft.Send = &content
	return draft, nil
}

func (c FakeDraftClient) SendMessage(_ context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.DirectSendOutcome{}, err
	}
	if !fakeOutboundMessageEmpty(req.Message) {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	prepared, ok := req.Prepared.(*FakePreparedDirectSend)
	if !ok {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	if _, err := prepared.Consume(); err != nil {
		return mail.DirectSendOutcome{}, err
	}
	if c.state.DirectSendErr != nil {
		return mail.DirectSendOutcome{}, c.state.DirectSendErr
	}
	id := fmt.Sprintf("fake-sent-%d", c.state.DirectCalls.Add(1))
	return mail.DirectSendOutcome{MessageID: id}, nil
}

func (c FakeDraftClient) AddDraftAttachments(_ context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.AddDraftAttachmentsResult{}, err
	}
	c.state.AttachmentAddCalls.Add(1)
	result := mail.AddDraftAttachmentsResult{DraftID: req.DraftID, ProviderMeta: fakeDraftMeta(mail.MetadataDraftWrite)}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return result, c.draftNotFound(req.DraftID)
	}
	failAfter := c.state.FailAddAfter.Load()
	for i, upload := range req.Uploads {
		if failAfter > 0 && int64(i) >= failAfter {
			return result, rungrad.NewError(rungrad.ExitAPI, "fake attachment upload failed")
		}
		att := mail.Attachment{
			ID:           fmt.Sprintf("fake-att-%d", c.state.nextAttachmentID.Add(1)),
			Name:         upload.Name,
			ContentType:  upload.ContentType,
			Size:         upload.Size,
			Kind:         mail.AttachmentKindFile,
			ProviderType: "#fake.fileAttachment",
		}
		draft.Attachments = append(draft.Attachments, att)
		result.Added = append(result.Added, att)
	}
	draft.HasAttachments = len(draft.Attachments) > 0
	draft.ProviderMeta = fakeDraftMeta(mail.MetadataDraft)
	c.state.drafts[req.DraftID] = draft
	result.FinalAttachments = append([]mail.Attachment(nil), draft.Attachments...)
	if c.state.FailAddAfterAll.Load() {
		// Simulate providers that commit every upload but fail while reading the
		// final attachment metadata. The command must not call this a partial
		// upload failure.
		return result, rungrad.NewError(rungrad.ExitAPI, "fake attachment metadata refresh failed")
	}
	return result, nil
}

func (c FakeDraftClient) RemoveDraftAttachment(_ context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.RemoveDraftAttachmentResult{}, err
	}
	c.state.AttachmentRemoveCalls.Add(1)
	if c.state.FailRemoveAttachment.Load() {
		return mail.RemoveDraftAttachmentResult{}, rungrad.NewError(rungrad.ExitAPI, "fake attachment remove failed")
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	draft, ok := c.state.drafts[req.DraftID]
	if !ok || !draft.IsDraft {
		return mail.RemoveDraftAttachmentResult{}, c.draftNotFound(req.DraftID)
	}
	for i, a := range draft.Attachments {
		if a.ID != req.AttachmentID {
			continue
		}
		draft.Attachments = append(draft.Attachments[:i], draft.Attachments[i+1:]...)
		draft.HasAttachments = len(draft.Attachments) > 0
		c.state.drafts[req.DraftID] = draft
		return mail.RemoveDraftAttachmentResult{
			DraftID:          req.DraftID,
			Removed:          a,
			FinalAttachments: append([]mail.Attachment(nil), draft.Attachments...),
			ProviderMeta:     fakeDraftMeta(mail.MetadataDraftWrite),
		}, nil
	}
	return mail.RemoveDraftAttachmentResult{}, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{
		"draft_id":      req.DraftID,
		"attachment_id": req.AttachmentID,
	})
}

func (c FakeDraftClient) draftFromOutbound(id string, msg mail.OutboundMessage) mail.Draft {
	from := mail.Recipient{Address: c.Account + "@example.test"}
	if msg.From != nil {
		from = *msg.From
	}
	return mail.Draft{
		Account:      c.Account,
		Provider:     c.Provider,
		ID:           id,
		IsDraft:      true,
		Folder:       mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"},
		From:         from,
		ReplyTo:      append([]mail.Recipient(nil), msg.ReplyTo...),
		To:           append([]mail.Recipient(nil), msg.To...),
		CC:           append([]mail.Recipient(nil), msg.CC...),
		BCC:          append([]mail.Recipient(nil), msg.BCC...),
		Subject:      msg.Subject,
		Body:         messageBodyFromOutbound(msg.Body),
		ProviderMeta: fakeDraftMeta(mail.MetadataDraft),
	}
}

// fakeReplyContent is what the fake would hand over when it sends a reply
// to source: the draft with the headers that keep it in the conversation.
func fakeReplyContent(draft mail.Draft, source provider.ComposeSourceMaterial) mail.SendContent {
	content := fakeSendContent(draft)
	if source.InternetMessageID != "" {
		content.InReplyTo = source.InternetMessageID
		content.References = append(append([]string(nil), source.References...), source.InternetMessageID)
	}
	return content
}

// fakeSendContent is what the fake would hand over when it sends draft.
func fakeSendContent(draft mail.Draft) mail.SendContent {
	content := mail.SendContent{
		From:    draft.From,
		To:      append([]mail.Recipient(nil), draft.To...),
		CC:      append([]mail.Recipient(nil), draft.CC...),
		BCC:     append([]mail.Recipient(nil), draft.BCC...),
		ReplyTo: append([]mail.Recipient(nil), draft.ReplyTo...),
		Subject: draft.Subject,
	}
	if strings.Contains(strings.ToLower(draft.Body.ContentType), "html") {
		content.HTML = draft.Body.Content
	} else {
		content.Text = draft.Body.Content
	}
	for _, a := range draft.Attachments {
		content.Attachments = append(content.Attachments, mail.SendAttachment{Name: a.Name, ContentType: a.ContentType, Size: a.Size, Inline: a.IsInline})
	}
	return content
}

func (c FakeDraftClient) stampDraft(draft mail.Draft) mail.Draft {
	draft.Account = c.Account
	draft.Provider = c.Provider
	if draft.ProviderMeta == nil {
		draft.ProviderMeta = fakeDraftMeta(mail.MetadataDraft)
	}
	return draft
}

func (c FakeDraftClient) draftNotFound(id string) error {
	return errs.NewWithDetails(errs.CodeDraftNotFound, "draft not found", map[string]any{
		"account":  c.Account,
		"provider": c.Provider,
		"draft_id": id,
	})
}

// fakeSelectForwardSourceAttachments mirrors the provider-facing selection
// rules so command tests can exercise forward output without a live adapter.
func fakeSelectForwardSourceAttachments(source []mail.Attachment, selection mail.ForwardAttachmentSelection) ([]mail.Attachment, []mail.AttachmentExclusion, error) {
	mode := selection.Mode
	if mode == "" {
		mode = mail.ForwardAttachmentsNone
	}
	switch mode {
	case mail.ForwardAttachmentsNone:
		return nil, nil, nil
	case mail.ForwardAttachmentsAll:
		selected := make([]mail.Attachment, 0, len(source))
		excluded := make([]mail.AttachmentExclusion, 0)
		for _, a := range source {
			if fakeForwardSupported(a) {
				selected = append(selected, a)
			} else if !a.IsInline {
				excluded = append(excluded, mail.AttachmentExclusion{Reason: "unsupported_kind", Attachment: a})
			}
		}
		return selected, excluded, nil
	case mail.ForwardAttachmentsSelected:
		selected := make([]mail.Attachment, 0, len(selection.Names)+len(selection.IDs))
		selectedIDs := map[string]struct{}{}
		for _, name := range selection.Names {
			matches := make([]mail.Attachment, 0, 1)
			for _, a := range source {
				if a.Name == name && fakeForwardSupported(a) {
					matches = append(matches, a)
				}
			}
			switch len(matches) {
			case 0:
				return nil, nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"name": name})
			case 1:
				selected = append(selected, matches[0])
				selectedIDs[matches[0].ID] = struct{}{}
			default:
				ids := make([]string, 0, len(matches))
				for _, match := range matches {
					ids = append(ids, match.ID)
				}
				return nil, nil, errs.NewWithDetails(errs.CodeAmbiguousAttachment, "attachment selection is ambiguous", map[string]any{"name": name, "candidate_ids": ids})
			}
		}
		byID := map[string]mail.Attachment{}
		for _, a := range source {
			byID[a.ID] = a
		}
		for _, id := range selection.IDs {
			a, ok := byID[id]
			if !ok {
				return nil, nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"attachment_id": id})
			}
			if _, ok := selectedIDs[id]; ok {
				return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "attachment name and id selections overlap", map[string]any{"field": "include_attachment", "attachment_id": id})
			}
			if !fakeForwardSupported(a) {
				return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "selected source attachment is not a non-inline file attachment", map[string]any{"field": "include_attachment_id", "attachment_id": id})
			}
			selected = append(selected, a)
			selectedIDs[id] = struct{}{}
		}
		if err := fakeValidateTupleSelection(source, selected); err != nil {
			return nil, nil, err
		}
		return selected, nil, nil
	default:
		return nil, nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "include-attachments must be none or all", map[string]any{"field": "include_attachments"})
	}
}

func fakeForwardSupported(a mail.Attachment) bool {
	return !a.IsInline && a.Kind == mail.AttachmentKindFile
}

type fakeAttachmentTuple struct {
	name        string
	size        int64
	contentType string
	inline      bool
}

func fakeTuple(a mail.Attachment) fakeAttachmentTuple {
	return fakeAttachmentTuple{name: a.Name, size: a.Size, contentType: a.ContentType, inline: a.IsInline}
}

func fakeValidateTupleSelection(source, selected []mail.Attachment) error {
	total := map[fakeAttachmentTuple]int{}
	for _, a := range source {
		if fakeForwardSupported(a) {
			total[fakeTuple(a)]++
		}
	}
	counts := map[fakeAttachmentTuple]int{}
	for _, a := range selected {
		counts[fakeTuple(a)]++
	}
	for tuple, count := range counts {
		if count > 0 && count < total[tuple] {
			return errs.NewWithDetails(errs.CodeAmbiguousAttachment, "attachment selection is ambiguous", map[string]any{
				"name":         tuple.name,
				"size":         tuple.size,
				"content_type": tuple.contentType,
			})
		}
	}
	return nil
}

// fakeReconcileForwardAttachments keeps the selected copied files and inline
// attachments, matching the Outlook adapter's provider-copy reconciliation.
func fakeReconcileForwardAttachments(selected, copied []mail.Attachment) ([]mail.Attachment, error) {
	quota := map[fakeAttachmentTuple]int{}
	for _, a := range selected {
		quota[fakeTuple(a)]++
	}
	available := map[fakeAttachmentTuple]int{}
	for _, a := range copied {
		if !a.IsInline && a.Kind == mail.AttachmentKindFile {
			available[fakeTuple(a)]++
		}
	}
	for tuple, want := range quota {
		got := available[tuple]
		if got == 0 {
			return nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"name": tuple.name})
		}
		if got < want {
			return nil, errs.NewWithDetails(errs.CodeAmbiguousAttachment, "attachment selection is ambiguous", map[string]any{"name": tuple.name})
		}
	}
	remaining := make(map[fakeAttachmentTuple]int, len(quota))
	for tuple, count := range quota {
		remaining[tuple] = count
	}
	out := make([]mail.Attachment, 0, len(copied))
	for _, a := range copied {
		if a.IsInline {
			out = append(out, a)
			continue
		}
		if a.Kind == mail.AttachmentKindFile {
			tuple := fakeTuple(a)
			if remaining[tuple] > 0 {
				remaining[tuple]--
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// The fakeDraftNo* wrappers each leave out one interface the provider claims
// and pass the rest through. Tests use them to check that the command fails
// before it creates a reply draft.
type fakeDraftNoReply struct{ inner FakeDraftClient }

func (c fakeDraftNoReply) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoReply) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoReply) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoReply) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoReply) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	return c.inner.ReadDraft(ctx, req)
}
func (c fakeDraftNoReply) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.inner.UpdateDraft(ctx, req)
}
func (c fakeDraftNoReply) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	return c.inner.ValidateDraftUpdate(ctx, req)
}
func (c fakeDraftNoReply) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	return c.inner.SendDraft(ctx, req)
}
func (c fakeDraftNoReply) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	return c.inner.ValidateDraftSend(ctx, req)
}
func (c fakeDraftNoReply) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoReply) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}
func (c fakeDraftNoReply) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraft(ctx, req)
}
func (c fakeDraftNoReply) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.CreateForwardDraft(ctx, req)
}
func (c fakeDraftNoReply) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraftPreview(ctx, req)
}
func (c fakeDraftNoReply) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	return c.inner.AddDraftAttachments(ctx, req)
}
func (c fakeDraftNoReply) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	return c.inner.RemoveDraftAttachment(ctx, req)
}

type fakeDraftNoForward struct{ inner FakeDraftClient }

func (c fakeDraftNoForward) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoForward) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoForward) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoForward) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoForward) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.ValidateReplyDraft(ctx, req)
}
func (c fakeDraftNoForward) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.CreateReplyDraft(ctx, req)
}
func (c fakeDraftNoForward) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	return c.inner.ReadDraft(ctx, req)
}
func (c fakeDraftNoForward) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.inner.UpdateDraft(ctx, req)
}
func (c fakeDraftNoForward) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	return c.inner.ValidateDraftUpdate(ctx, req)
}
func (c fakeDraftNoForward) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	return c.inner.SendDraft(ctx, req)
}
func (c fakeDraftNoForward) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	return c.inner.ValidateDraftSend(ctx, req)
}
func (c fakeDraftNoForward) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoForward) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}
func (c fakeDraftNoForward) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	return c.inner.AddDraftAttachments(ctx, req)
}
func (c fakeDraftNoForward) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	return c.inner.RemoveDraftAttachment(ctx, req)
}

type fakeDraftNoReader struct{ inner FakeDraftClient }

func (c fakeDraftNoReader) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoReader) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoReader) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoReader) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoReader) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.ValidateReplyDraft(ctx, req)
}
func (c fakeDraftNoReader) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.CreateReplyDraft(ctx, req)
}
func (c fakeDraftNoReader) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraft(ctx, req)
}
func (c fakeDraftNoReader) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.CreateForwardDraft(ctx, req)
}
func (c fakeDraftNoReader) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraftPreview(ctx, req)
}
func (c fakeDraftNoReader) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.inner.UpdateDraft(ctx, req)
}
func (c fakeDraftNoReader) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	return c.inner.ValidateDraftUpdate(ctx, req)
}
func (c fakeDraftNoReader) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	return c.inner.SendDraft(ctx, req)
}
func (c fakeDraftNoReader) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	return c.inner.ValidateDraftSend(ctx, req)
}
func (c fakeDraftNoReader) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoReader) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}
func (c fakeDraftNoReader) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	return c.inner.AddDraftAttachments(ctx, req)
}
func (c fakeDraftNoReader) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	return c.inner.RemoveDraftAttachment(ctx, req)
}

type fakeDraftNoUpdater struct{ inner FakeDraftClient }

func (c fakeDraftNoUpdater) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoUpdater) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoUpdater) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoUpdater) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoUpdater) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.ValidateReplyDraft(ctx, req)
}
func (c fakeDraftNoUpdater) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.CreateReplyDraft(ctx, req)
}
func (c fakeDraftNoUpdater) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraft(ctx, req)
}
func (c fakeDraftNoUpdater) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.CreateForwardDraft(ctx, req)
}
func (c fakeDraftNoUpdater) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraftPreview(ctx, req)
}
func (c fakeDraftNoUpdater) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	return c.inner.ReadDraft(ctx, req)
}
func (c fakeDraftNoUpdater) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	return c.inner.SendDraft(ctx, req)
}
func (c fakeDraftNoUpdater) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	return c.inner.ValidateDraftSend(ctx, req)
}
func (c fakeDraftNoUpdater) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoUpdater) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}
func (c fakeDraftNoUpdater) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	return c.inner.AddDraftAttachments(ctx, req)
}
func (c fakeDraftNoUpdater) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	return c.inner.RemoveDraftAttachment(ctx, req)
}

type fakeDraftNoSender struct{ inner FakeDraftClient }

func (c fakeDraftNoSender) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoSender) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoSender) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoSender) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoSender) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.ValidateReplyDraft(ctx, req)
}
func (c fakeDraftNoSender) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.CreateReplyDraft(ctx, req)
}
func (c fakeDraftNoSender) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraft(ctx, req)
}
func (c fakeDraftNoSender) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.CreateForwardDraft(ctx, req)
}
func (c fakeDraftNoSender) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraftPreview(ctx, req)
}
func (c fakeDraftNoSender) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	return c.inner.ReadDraft(ctx, req)
}
func (c fakeDraftNoSender) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.inner.UpdateDraft(ctx, req)
}
func (c fakeDraftNoSender) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	return c.inner.ValidateDraftUpdate(ctx, req)
}
func (c fakeDraftNoSender) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoSender) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}
func (c fakeDraftNoSender) AddDraftAttachments(ctx context.Context, req mail.AddDraftAttachmentsRequest) (mail.AddDraftAttachmentsResult, error) {
	return c.inner.AddDraftAttachments(ctx, req)
}
func (c fakeDraftNoSender) RemoveDraftAttachment(ctx context.Context, req mail.RemoveDraftAttachmentRequest) (mail.RemoveDraftAttachmentResult, error) {
	return c.inner.RemoveDraftAttachment(ctx, req)
}

type fakeDraftNoAttachments struct{ inner FakeDraftClient }

func (c fakeDraftNoAttachments) ProviderName() string { return c.inner.ProviderName() }
func (c fakeDraftNoAttachments) AccountName() string  { return c.inner.AccountName() }
func (c fakeDraftNoAttachments) Destroy()             { c.inner.Destroy() }
func (c fakeDraftNoAttachments) CreateDraft(ctx context.Context, req mail.CreateDraftRequest) (mail.DraftRef, error) {
	return c.inner.CreateDraft(ctx, req)
}
func (c fakeDraftNoAttachments) ValidateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.ValidateReplyDraft(ctx, req)
}
func (c fakeDraftNoAttachments) CreateReplyDraft(ctx context.Context, req provider.ReplyDraftRequest) (mail.ReplyDraftResult, error) {
	return c.inner.CreateReplyDraft(ctx, req)
}
func (c fakeDraftNoAttachments) ValidateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraft(ctx, req)
}
func (c fakeDraftNoAttachments) CreateForwardDraft(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.CreateForwardDraft(ctx, req)
}
func (c fakeDraftNoAttachments) ValidateForwardDraftPreview(ctx context.Context, req provider.ForwardDraftRequest) (mail.ForwardDraftResult, error) {
	return c.inner.ValidateForwardDraftPreview(ctx, req)
}
func (c fakeDraftNoAttachments) ReadDraft(ctx context.Context, req mail.ReadDraftRequest) (mail.Draft, error) {
	return c.inner.ReadDraft(ctx, req)
}
func (c fakeDraftNoAttachments) UpdateDraft(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftRef, error) {
	return c.inner.UpdateDraft(ctx, req)
}
func (c fakeDraftNoAttachments) ValidateDraftUpdate(ctx context.Context, req mail.UpdateDraftRequest) (mail.DraftUpdateValidation, error) {
	return c.inner.ValidateDraftUpdate(ctx, req)
}
func (c fakeDraftNoAttachments) SendDraft(ctx context.Context, req mail.SendDraftRequest) (mail.SendOutcome, error) {
	return c.inner.SendDraft(ctx, req)
}
func (c fakeDraftNoAttachments) ValidateDraftSend(ctx context.Context, req mail.SendDraftRequest) (mail.Draft, error) {
	return c.inner.ValidateDraftSend(ctx, req)
}
func (c fakeDraftNoAttachments) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	return c.inner.SendMessage(ctx, req)
}
func (c fakeDraftNoAttachments) ReadComposeSourceMaterial(ctx context.Context, selector mail.ResolvedMessageSelector) (provider.ComposeSourceMaterial, error) {
	return c.inner.ReadComposeSourceMaterial(ctx, selector)
}

func (c FakeReadClient) ProviderName() string { return c.Provider }

func (c FakeReadClient) AccountName() string { return c.Account }

func (c FakeReadClient) Destroy() { c.lifetime.destroy() }

func (c FakeReadClient) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	return FakeFolderClient{Account: c.Account, Provider: c.Provider, Folders: c.Folders, lifetime: c.lifetime}.ListFolders(ctx, req)
}

func (c FakeReadClient) SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	return FakeMailboxClient{Account: c.Account, Provider: c.Provider, Folders: c.Folders, Messages: c.Summaries, Err: c.Err, lifetime: c.lifetime}.SearchMessages(ctx, req)
}

func (c FakeReadClient) ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	return FakeMailboxClient{Account: c.Account, Provider: c.Provider, Folders: c.Folders, Messages: c.Summaries, Err: c.Err, lifetime: c.lifetime}.ListMessages(ctx, req)
}

// ReadMessage takes only selectors that name a message. As with the real
// providers, a query has to go through SearchMessages first.
func (c FakeReadClient) ReadMessage(_ context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Message{}, err
	}
	msg, err := c.resolveMessage(req.Selector)
	if err != nil {
		return mail.Message{}, err
	}
	msg = c.stampMessage(msg)
	if req.MetadataOnly {
		if c.MetadataOnlyReadCalls != nil {
			c.MetadataOnlyReadCalls.Add(1)
		}
		msg.Body = mail.MessageBody{}
		msg.Attachments = nil
		return msg, nil
	}
	msg.Body.Mode = req.BodyMode
	if req.BodyMode == mail.BodyModeRaw && msg.Body.Encoding == "" {
		msg.Body.Encoding = "utf8"
	}
	return msg, nil
}

func (c FakeReadClient) ListMessageAttachments(_ context.Context, req mail.MessageAttachmentListRequest) (mail.MessageAttachmentList, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageAttachmentList{}, err
	}
	if c.AttachmentListCalls != nil {
		c.AttachmentListCalls.Add(1)
	}
	if err := c.AttachmentListErr[req.MessageID]; err != nil {
		return mail.MessageAttachmentList{}, err
	}
	attachments, ok := c.Attachments[req.MessageID]
	if !ok {
		if _, msgOK := c.Messages[req.MessageID]; !msgOK {
			return mail.MessageAttachmentList{}, c.messageNotFound(mail.ResolvedSelectorID)
		}
	}
	msg := c.Messages[req.MessageID]
	return mail.MessageAttachmentList{
		MessageID:         req.MessageID,
		InternetMessageID: msg.InternetMessageID,
		Attachments:       append([]mail.Attachment(nil), attachments...),
		ProviderMeta:      fakeAttachmentMeta(mail.MetadataAttachmentList),
	}, nil
}

func (c FakeReadClient) DownloadMessageAttachment(_ context.Context, req mail.MessageAttachmentDownloadRequest) (mail.MessageAttachmentContent, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	if c.AttachmentDownloadCalls != nil {
		c.AttachmentDownloadCalls.Add(1)
	}
	if perMessage := c.AttachmentDownloadErr[req.MessageID]; perMessage != nil {
		if err := perMessage[req.AttachmentID]; err != nil {
			return mail.MessageAttachmentContent{}, err
		}
	}
	perMessage := c.AttachmentContent[req.MessageID]
	if perMessage == nil {
		return mail.MessageAttachmentContent{}, c.attachmentNotFound(req.MessageID, req.AttachmentID)
	}
	content, ok := perMessage[req.AttachmentID]
	if !ok {
		return mail.MessageAttachmentContent{}, c.attachmentNotFound(req.MessageID, req.AttachmentID)
	}
	declared := content.DeclaredSize
	if declared == 0 && content.Bytes != nil {
		declared = int64(len(content.Bytes))
	}
	if content.ContentType == "" {
		content.ContentType = "application/octet-stream"
	}
	return mail.MessageAttachmentContent{
		AttachmentID: req.AttachmentID,
		Content:      fakeReadCloser{Reader: bytes.NewReader(content.Bytes), readErr: content.ReadErr, closeErr: content.CloseErr},
		ContentType:  content.ContentType,
		DeclaredSize: declared,
	}, nil
}

// ReadThread accepts the same selector families as the production provider and
// returns body-free rows sorted in conversation order.
func (c FakeReadClient) ReadThread(_ context.Context, req mail.ReadThreadRequest) (mail.Thread, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.Thread{}, err
	}
	conv, err := c.resolveThreadID(req.Selector)
	if err != nil {
		return mail.Thread{}, err
	}
	thread, ok := c.Threads[conv]
	if !ok || len(thread.Messages) == 0 {
		return mail.Thread{}, errs.NewWithDetails(errs.CodeThreadNotFound, "no messages for this thread", map[string]any{
			"account":   c.Account,
			"provider":  c.Provider,
			"thread_id": conv,
		})
	}
	thread.Account = c.Account
	thread.Provider = c.Provider
	thread.ThreadID = conv
	for i := range thread.Messages {
		thread.Messages[i].Account = c.Account
		thread.Messages[i].Provider = c.Provider
		if thread.Messages[i].ThreadID == "" {
			thread.Messages[i].ThreadID = conv
		}
	}
	sort.SliceStable(thread.Messages, func(i, j int) bool {
		ti, tj := fakeThreadTime(thread.Messages[i]), fakeThreadTime(thread.Messages[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return thread.Messages[i].ID < thread.Messages[j].ID
	})
	return thread, nil
}

// resolveMessage mirrors Outlook's internet-message-id behavior closely enough
// for command tests: no match is not found, one match succeeds, multiple matches
// are ambiguous and expose candidate provider ids.
func (c FakeReadClient) resolveMessage(selector mail.ResolvedMessageSelector) (mail.Message, error) {
	switch selector.Kind {
	case mail.ResolvedSelectorID:
		msg, ok := c.Messages[selector.ID]
		if !ok {
			return mail.Message{}, c.messageNotFound(selector.Kind)
		}
		return msg, nil
	case mail.ResolvedSelectorInternetID:
		matches := c.Internet[selector.InternetID]
		switch len(matches) {
		case 0:
			return mail.Message{}, c.messageNotFound(selector.Kind)
		case 1:
			return matches[0], nil
		default:
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return mail.Message{}, errs.NewWithDetails(errs.CodeAmbiguousMessageSelector, "internet message id matches multiple messages; retry with --id from search/list/read output", map[string]any{
				"account":             c.Account,
				"provider":            c.Provider,
				"internet_message_id": selector.InternetID,
				"candidate_ids":       ids,
			})
		}
	default:
		return mail.Message{}, errs.New(errs.CodeInvalidSelector, "read message selector must be --id or --internet-id")
	}
}

// resolveThreadID lets thread tests start from either a direct thread id or a
// source message selector, just like Outlook conversation resolution.
func (c FakeReadClient) resolveThreadID(selector mail.ResolvedMessageSelector) (string, error) {
	switch selector.Kind {
	case mail.ResolvedSelectorThreadID:
		return selector.ThreadID, nil
	case mail.ResolvedSelectorID:
		msg, ok := c.Messages[selector.ID]
		if !ok {
			return "", c.messageNotFound(selector.Kind)
		}
		return msg.ThreadID, nil
	case mail.ResolvedSelectorInternetID:
		msg, err := c.resolveMessage(selector)
		if err != nil {
			return "", err
		}
		return msg.ThreadID, nil
	default:
		return "", errs.New(errs.CodeInvalidSelector, "thread selector must be --id, --internet-id, or --thread-id")
	}
}

func (c FakeReadClient) stampMessage(msg mail.Message) mail.Message {
	msg.Account = c.Account
	msg.Provider = c.Provider
	if msg.ProviderMeta == nil {
		msg.ProviderMeta = fakeMessageMeta()
	}
	return msg
}

func (c FakeReadClient) messageNotFound(kind mail.ResolvedSelectorKind) error {
	return errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{
		"account":       c.Account,
		"provider":      c.Provider,
		"selector_kind": string(kind),
	})
}

func (c FakeReadClient) attachmentNotFound(messageID, attachmentID string) error {
	return errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{
		"account":       c.Account,
		"provider":      c.Provider,
		"message_id":    messageID,
		"attachment_id": attachmentID,
	})
}

type fakeReadCloser struct {
	io.Reader
	readErr  error
	closeErr error
}

func (r fakeReadCloser) Read(p []byte) (int, error) {
	if r.readErr != nil {
		return 0, r.readErr
	}
	return r.Reader.Read(p)
}

func (r fakeReadCloser) Close() error {
	return r.closeErr
}

type fakeReadNoMessageAttachments struct{ inner FakeReadClient }

func (c fakeReadNoMessageAttachments) ProviderName() string { return c.inner.ProviderName() }
func (c fakeReadNoMessageAttachments) AccountName() string  { return c.inner.AccountName() }
func (c fakeReadNoMessageAttachments) Destroy()             { c.inner.Destroy() }
func (c fakeReadNoMessageAttachments) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	return c.inner.ListFolders(ctx, req)
}
func (c fakeReadNoMessageAttachments) SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	return c.inner.SearchMessages(ctx, req)
}
func (c fakeReadNoMessageAttachments) ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	return c.inner.ListMessages(ctx, req)
}
func (c fakeReadNoMessageAttachments) ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	return c.inner.ReadMessage(ctx, req)
}
func (c fakeReadNoMessageAttachments) ReadThread(ctx context.Context, req mail.ReadThreadRequest) (mail.Thread, error) {
	return c.inner.ReadThread(ctx, req)
}

// FakeMutationClient combines the read-capable fake with persistent mutation
// behavior used by command tests.
type FakeMutationClient struct {
	FakeReadClient
	state                *MutationState
	errByOperation       map[mail.MessageMutationOperation]error
	errByAccount         map[string]error
	errByMessage         map[string]error
	record               func(kind, account, id string)
	MessageMutationCalls *atomic.Int64
	CategoryListCalls    *atomic.Int64
	CategorySetCalls     *atomic.Int64
}

// beforeMutation records the call before it returns an injected error, so
// tests can tell the calls that were tried from the ones that were skipped.
func (c FakeMutationClient) beforeMutation(op mail.MessageMutationOperation, id string) error {
	if err := c.lifetime.err(); err != nil {
		return err
	}
	if c.MessageMutationCalls != nil {
		c.MessageMutationCalls.Add(1)
	}
	c.recordTarget("mutation", id)
	if c.errByAccount != nil {
		if err := c.errByAccount[c.Account]; err != nil {
			return err
		}
	}
	if c.errByMessage != nil {
		if err := c.errByMessage[id]; err != nil {
			return err
		}
	}
	if c.errByOperation != nil {
		return c.errByOperation[op]
	}
	return nil
}

func (c FakeMutationClient) recordTarget(kind, id string) {
	if c.record != nil {
		c.record(kind, c.Account, id)
	}
}

// MoveMessage updates the stored folder in place so follow-up reads/searches
// observe the move.
func (c FakeMutationClient) MoveMessage(_ context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error) {
	if err := c.beforeMutation(mail.MutationMove, req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	before, after, err := c.state.folderMove(c.Account, req.MessageID, req.Folder)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return c.folderMutationResult(before, after, after.ID), nil
}

// CopyMessage creates a new stored message id to mirror providers that return a
// separate copied message.
func (c FakeMutationClient) CopyMessage(_ context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error) {
	if err := c.beforeMutation(mail.MutationCopy, req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	before, copied, err := c.state.copyTo(c.Account, req.MessageID, req.Folder)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return c.folderMutationResult(before, copied, copied.ID), nil
}

func (c FakeMutationClient) ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.moveToWellKnown(ctx, mail.MutationArchive, req.MessageID, mail.WellKnownArchive)
}

func (c FakeMutationClient) DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.moveToWellKnown(ctx, mail.MutationDelete, req.MessageID, mail.WellKnownDeleted)
}

func (c FakeMutationClient) RestoreMessage(_ context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error) {
	if err := c.beforeMutation(mail.MutationRestore, req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	before, after, err := c.state.folderMove(c.Account, req.MessageID, req.Folder)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return c.folderMutationResult(before, after, after.ID), nil
}

func (c FakeMutationClient) JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.moveToWellKnown(ctx, mail.MutationJunk, req.MessageID, mail.WellKnownJunk)
}

func (c FakeMutationClient) SetMessageReadState(_ context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error) {
	op := mail.MutationMarkUnread
	if req.IsRead {
		op = mail.MutationMarkRead
	}
	if err := c.beforeMutation(op, req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	before, after, err := c.state.updateRead(c.Account, req.MessageID, req.IsRead)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	beforeRead := before.IsRead
	return mail.MessageMutationResult{
		MessageID:         after.ID,
		InternetMessageID: after.InternetMessageID,
		ReadState:         &mail.BoolChange{Before: &beforeRead, After: after.IsRead},
		ProviderMeta:      fakeMutationMeta(),
	}, nil
}

func (c FakeMutationClient) SetMessageFlag(_ context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error) {
	op := mail.MutationFlagClear
	if req.Flagged {
		op = mail.MutationFlagSet
	}
	if err := c.beforeMutation(op, req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	before, err := c.state.updateFlag(c.Account, req.MessageID, req.Flagged)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	msg, err := c.state.categories(c.Account, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return mail.MessageMutationResult{
		MessageID:         msg.ID,
		InternetMessageID: msg.InternetMessageID,
		FlagState:         &mail.BoolChange{Before: &before, After: req.Flagged},
		ProviderMeta:      fakeMutationMeta(),
	}, nil
}

// ListMessageCategories returns provider-order categories from the current
// stored message state.
func (c FakeMutationClient) ListMessageCategories(_ context.Context, req mail.MessageCategoryListRequest) (mail.MessageCategoryListResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageCategoryListResult{}, err
	}
	c.recordTarget("category_list", req.MessageID)
	if c.errByAccount != nil {
		if err := c.errByAccount[c.Account]; err != nil {
			return mail.MessageCategoryListResult{}, err
		}
	}
	if c.errByMessage != nil {
		if err := c.errByMessage[req.MessageID]; err != nil {
			return mail.MessageCategoryListResult{}, err
		}
	}
	if c.errByOperation != nil {
		if err := c.errByOperation[mail.MutationCategoriesList]; err != nil {
			return mail.MessageCategoryListResult{}, err
		}
	}
	if c.CategoryListCalls != nil {
		c.CategoryListCalls.Add(1)
	}
	msg, err := c.state.categories(c.Account, req.MessageID)
	if err != nil {
		return mail.MessageCategoryListResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	categories := append([]string(nil), msg.Categories...)
	return mail.MessageCategoryListResult{
		MessageID:         msg.ID,
		InternetMessageID: msg.InternetMessageID,
		Categories:        categories,
		Count:             len(categories),
		ProviderMeta:      fakeMutationMeta(),
	}, nil
}

// SetMessageCategories replaces the stored category list exactly as supplied by
// the command layer.
func (c FakeMutationClient) SetMessageCategories(_ context.Context, req mail.MessageCategorySetRequest) (mail.MessageMutationResult, error) {
	if err := c.lifetime.err(); err != nil {
		return mail.MessageMutationResult{}, err
	}
	if c.CategorySetCalls != nil {
		c.CategorySetCalls.Add(1)
	}
	c.recordTarget("category_set", req.MessageID)
	if c.errByAccount != nil {
		if err := c.errByAccount[c.Account]; err != nil {
			return mail.MessageMutationResult{}, err
		}
	}
	if c.errByMessage != nil {
		if err := c.errByMessage[req.MessageID]; err != nil {
			return mail.MessageMutationResult{}, err
		}
	}
	if c.errByOperation != nil {
		if err := c.errByOperation[mail.MutationCategoriesSet]; err != nil {
			return mail.MessageMutationResult{}, err
		}
	}
	before, after, err := c.state.setCategories(c.Account, req.MessageID, req.Categories)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return mail.MessageMutationResult{
		MessageID:         after.ID,
		InternetMessageID: after.InternetMessageID,
		Categories: &mail.CategoryChange{
			Before: append([]string(nil), before.Categories...),
			After:  append([]string(nil), after.Categories...),
			Count:  len(after.Categories),
		},
		ProviderMeta: fakeMutationMeta(),
	}, nil
}

// moveToWellKnown keeps archive/delete/junk behavior in the fake aligned with
// providers that resolve their own well-known destination folders.
func (c FakeMutationClient) moveToWellKnown(_ context.Context, op mail.MessageMutationOperation, id, wellKnown string) (mail.MessageMutationResult, error) {
	if err := c.beforeMutation(op, id); err != nil {
		return mail.MessageMutationResult{}, err
	}
	folder, ok := firstWellKnownFolder(c.Folders.Folders, wellKnown)
	if !ok {
		return mail.MessageMutationResult{}, errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
			"account":         c.Account,
			"provider":        c.Provider,
			"well_known_name": wellKnown,
		})
	}
	before, after, err := c.state.folderMove(c.Account, id, folder)
	if err != nil {
		return mail.MessageMutationResult{}, c.messageNotFound(mail.ResolvedSelectorID)
	}
	return c.folderMutationResult(before, after, after.ID), nil
}

func (c FakeMutationClient) folderMutationResult(before, after mail.Message, resultID string) mail.MessageMutationResult {
	source := before.Folder
	target := after.Folder
	return mail.MessageMutationResult{
		MessageID:         before.ID,
		ResultMessageID:   resultID,
		InternetMessageID: after.InternetMessageID,
		SourceFolder:      &source,
		TargetFolder:      &target,
		ProviderMeta:      fakeMutationMeta(),
	}
}

func firstWellKnownFolder(folders []mail.Folder, wellKnown string) (mail.Folder, bool) {
	for _, folder := range folders {
		if folder.WellKnownName == wellKnown {
			return folder, true
		}
	}
	return mail.Folder{}, false
}

func fakeThreadTime(row mail.ThreadMessage) time.Time {
	if row.SentAt != nil {
		return *row.SentAt
	}
	if row.ReceivedAt != nil {
		return *row.ReceivedAt
	}
	return time.Time{}
}

// NewAccountClient loads the saved login without asking anything. As with the
// real providers, a missing or broken login tells the user to run auth repair
// and never starts a login.
func (f *FakeProvider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	f.AccountClientCalls.Add(1)
	f.accountClientMu.Lock()
	f.accountClientAccounts = append(f.accountClientAccounts, ac.Account)
	f.accountClientMu.Unlock()
	if f.AccountClientErr != nil {
		return nil, f.AccountClientErr
	}
	if err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		if lease.Current() == nil {
			return errs.New(errs.CodeAuthRepairRequired, fmt.Sprintf(
				"the saved login for %q cannot be used; run maildo auth repair %s", ac.Account, ac.Account))
		}
		return nil
	}); err != nil {
		return nil, errs.New(errs.CodeAuthRepairRequired, fmt.Sprintf(
			"the saved login for %q cannot be used; run maildo auth repair %s", ac.Account, ac.Account))
	}
	lifetime := newFakeClientLifetime(ac.Account, ac.Provider)
	// Check for the archive policy fake first, so its client stays
	// reporter-only and tests show that the command needs no FolderReader.
	if f.ArchiveReportImplemented {
		if f.ArchiveReporterMissing {
			return FakeAccountClient{Account: ac.Account, Provider: ac.Provider, lifetime: lifetime}, nil
		}
		return FakeArchivePolicyClient{
			Account: ac.Account, Provider: ac.Provider,
			Report:      f.ArchiveReportsByAccount[ac.Account],
			Err:         f.ArchiveReportErrByAccount[ac.Account],
			ReportCalls: &f.ArchiveReportCalls,
			lifetime:    lifetime,
		}, nil
	}
	if f.ReplyImplemented && f.ReplyStaleMissing != "" {
		if f.Drafts == nil {
			f.Drafts = NewDraftState()
		}
		base := FakeDraftClient{Account: ac.Account, Provider: ac.Provider, state: f.Drafts, lifetime: lifetime}
		switch f.ReplyStaleMissing {
		case "reply":
			return fakeDraftNoReply{inner: base}, nil
		case "forward":
			return fakeDraftNoForward{inner: base}, nil
		case "read":
			return fakeDraftNoReader{inner: base}, nil
		case "update":
			return fakeDraftNoUpdater{inner: base}, nil
		case "send":
			return fakeDraftNoSender{inner: base}, nil
		case "attachments":
			return fakeDraftNoAttachments{inner: base}, nil
		}
	}
	if f.DraftImplemented && !f.DraftClientMissing {
		if f.Drafts == nil {
			f.Drafts = NewDraftState()
		}
		return FakeDraftClient{Account: ac.Account, Provider: ac.Provider, state: f.Drafts, lifetime: lifetime}, nil
	}
	if (f.MutationsImplemented || f.CategoriesImplemented) && !f.MutationClientMissing {
		if f.MutationState == nil {
			f.MutationState = NewMutationState(f.FullByAccount)
		}
		client := f.mutationClient(ac.Account, ac.Provider, lifetime)
		return client, nil
	}
	if f.RulesMutateImplemented && !f.RuleMutateClientMissing {
		if f.RuleMutationState == nil {
			f.RuleMutationState = NewRuleState(f.RulesByAccount)
		}
		client := f.ruleMutationClient(ac.Account, ac.Provider, lifetime)
		switch f.RuleMutationMissingInterface {
		case "RuleReader":
			return fakeRuleMutationNoReader{inner: client}, nil
		case "FolderReader":
			return fakeRuleMutationNoFolder{inner: client}, nil
		}
		return client, nil
	}
	if f.RulesReadImplemented && !f.RuleClientMissing {
		rules := cloneRules(f.RulesByAccount[ac.Account])
		listRules, separateList := f.RuleListByAccount[ac.Account]
		getRules, separateGet := f.RuleGetByAccount[ac.Account]
		return FakeRuleClient{
			FakeFolderClient: FakeFolderClient{
				Account:   ac.Account,
				Provider:  ac.Provider,
				Folders:   f.foldersForAccount(ac.Account),
				ListCalls: &f.FolderListCalls,
				Err:       f.FolderErrByAccount[ac.Account],
				record:    f.recordRuleRead,
				lifetime:  lifetime,
			},
			Rules:             rules,
			ListSnapshot:      cloneRules(listRules),
			GetSnapshot:       cloneRules(getRules),
			SeparateListRules: separateList,
			SeparateGetRules:  separateGet,
			Err:               f.RuleErrByAccount[ac.Account],
			ListErr:           f.RuleListErrByAccount[ac.Account],
			GetErr:            f.RuleGetErrByAccount[ac.Account],
			ListCalls:         &f.RuleListCalls,
			GetCalls:          &f.RuleGetCalls,
			recordRuleRead:    f.recordRuleRead,
		}, nil
	}
	if (f.ReadImplemented || f.AttachmentsReadImplemented) && !f.ReadClientMissing {
		var err error
		if f.MailboxErrByAccount != nil {
			err = f.MailboxErrByAccount[ac.Account]
		}
		full := cloneMessages(f.FullByAccount[ac.Account])
		client := FakeReadClient{
			Account:                 ac.Account,
			Provider:                ac.Provider,
			Folders:                 f.Folders,
			Summaries:               append([]mail.MessageSummary(nil), f.MessagesByAccount[ac.Account]...),
			Messages:                full,
			Internet:                indexMessagesByInternetID(full),
			Threads:                 cloneThreads(f.ThreadsByAccount[ac.Account]),
			Err:                     err,
			Attachments:             cloneAttachmentMap(f.MessageAttachmentsByAccount[ac.Account]),
			AttachmentContent:       cloneAttachmentContentMap(f.MessageAttachmentContentByAccount[ac.Account]),
			AttachmentListErr:       f.MessageAttachmentListErrByAccount[ac.Account],
			AttachmentDownloadErr:   f.MessageAttachmentDownloadErrByAccount[ac.Account],
			MetadataOnlyReadCalls:   &f.MessageMetadataOnlyReadCalls,
			AttachmentListCalls:     &f.MessageAttachmentListCalls,
			AttachmentDownloadCalls: &f.MessageAttachmentDownloadCalls,
			lifetime:                lifetime,
		}
		if f.AttachmentReadClientMissing {
			return fakeReadNoMessageAttachments{inner: client}, nil
		}
		return client, nil
	}
	if f.MailboxImplemented && !f.MailboxClientMissing {
		var err error
		if f.MailboxErrByAccount != nil {
			err = f.MailboxErrByAccount[ac.Account]
		}
		return FakeMailboxClient{
			Account:  ac.Account,
			Provider: ac.Provider,
			Folders:  f.Folders,
			Messages: append([]mail.MessageSummary(nil), f.MessagesByAccount[ac.Account]...),
			Err:      err,
			lifetime: lifetime,
		}, nil
	}
	if f.FoldersMutateImplemented && !f.FolderMutateClientMissing {
		return f.folderMutationClient(ac.Account, ac.Provider, lifetime), nil
	}
	if f.FoldersImplemented && !f.FolderClientMissing {
		return FakeFolderClient{
			Account:   ac.Account,
			Provider:  ac.Provider,
			Folders:   f.foldersForAccount(ac.Account),
			ListCalls: &f.FolderListCalls,
			Err:       f.FolderErrByAccount[ac.Account],
			record:    f.recordRuleRead,
			lifetime:  lifetime,
		}, nil
	}
	return FakeAccountClient{Account: ac.Account, Provider: ac.Provider, lifetime: lifetime}, nil
}

// AccountClientAccounts returns the accounts passed to NewAccountClient.
func (f *FakeProvider) AccountClientAccounts() []string {
	f.accountClientMu.Lock()
	defer f.accountClientMu.Unlock()
	return append([]string(nil), f.accountClientAccounts...)
}

func (f *FakeProvider) foldersForAccount(account string) mail.FolderList {
	if folders, ok := f.FoldersByAccount[account]; ok {
		return folders
	}
	return f.Folders
}

func (f *FakeProvider) recordRuleRead(kind, account, id string) {
	f.ruleReadMu.Lock()
	defer f.ruleReadMu.Unlock()
	entry := account + ":" + kind
	if id != "" {
		entry += ":" + id
	}
	f.RuleReadCallLog = append(f.RuleReadCallLog, entry)
}

// RuleReadCalls returns a stable snapshot of the observed folder/rule read
// order so command tests do not race with account client activity.
func (f *FakeProvider) RuleReadCalls() []string {
	f.ruleReadMu.Lock()
	defer f.ruleReadMu.Unlock()
	return append([]string(nil), f.RuleReadCallLog...)
}

// folderMutationClient wires the fake's shared counters and call log into the
// account client returned for a specific account.
func (f *FakeProvider) folderMutationClient(account, providerName string, lifetime *fakeClientLifetime) FakeFolderMutationClient {
	return FakeFolderMutationClient{
		FakeFolderClient:     FakeFolderClient{Account: account, Provider: providerName, Folders: f.Folders, ListCalls: &f.FolderListCalls, lifetime: lifetime},
		CreateCalls:          &f.FolderCreateCalls,
		RenamePreflightCalls: &f.FolderRenamePreflightCalls,
		DeletePreflightCalls: &f.FolderDeletePreflightCalls,
		RenameCalls:          &f.FolderRenameCalls,
		DeleteCalls:          &f.FolderDeleteCalls,
		record:               f.recordFolderMutationTarget,
	}
}

func (f *FakeProvider) mutationClient(account, providerName string, lifetime *fakeClientLifetime) FakeMutationClient {
	var err error
	if f.MailboxErrByAccount != nil {
		err = f.MailboxErrByAccount[account]
	}
	// Build read/search views from the latest mutation state so command tests
	// exercise the same client across selector resolution and writes.
	full := f.MutationState.snapshot(account)
	return FakeMutationClient{
		FakeReadClient: FakeReadClient{
			Account:               account,
			Provider:              providerName,
			Folders:               f.Folders,
			Summaries:             f.MutationState.summaries(account, providerName),
			Messages:              full,
			Internet:              indexMessagesByInternetID(full),
			Threads:               cloneThreads(f.ThreadsByAccount[account]),
			Err:                   err,
			MetadataOnlyReadCalls: &f.MessageMetadataOnlyReadCalls,
			lifetime:              lifetime,
		},
		state:                f.MutationState,
		errByOperation:       f.MutationErrByOperation,
		errByAccount:         f.MutationErrByAccount,
		errByMessage:         f.MutationErrByMessage,
		record:               f.recordMutationTarget,
		MessageMutationCalls: &f.MessageMutationCalls,
		CategoryListCalls:    &f.CategoryListCalls,
		CategorySetCalls:     &f.CategorySetCalls,
	}
}

func (f *FakeProvider) ruleMutationClient(account, providerName string, lifetime *fakeClientLifetime) FakeRuleMutationClient {
	var err error
	if f.RuleErrByAccount != nil {
		err = f.RuleErrByAccount[account]
	}
	return FakeRuleMutationClient{
		FakeRuleClient: FakeRuleClient{
			FakeFolderClient: FakeFolderClient{
				Account: account, Provider: providerName, Folders: f.foldersForAccount(account),
				ListCalls: &f.FolderListCalls, Err: f.FolderErrByAccount[account], record: f.recordRuleRead, lifetime: lifetime,
			},
			Rules:          f.RuleMutationState.snapshot(account),
			Err:            err,
			ListErr:        f.RuleListErrByAccount[account],
			ListCalls:      &f.RuleListCalls,
			GetCalls:       &f.RuleGetCalls,
			recordRuleRead: f.recordRuleRead,
		},
		state:          f.RuleMutationState,
		errByOperation: f.RuleMutationErrByOperation,
		errByAccount:   f.RuleMutationErrByAccount,
		errByRule:      f.RuleMutationErrByRule,
		getErr:         f.RuleMutationGetErrByAccount[account],
		receiptOp:      f.RuleMutationReceiptOperation,
		receiptMeta:    f.RuleMutationReceiptMetadata,
		skipUpdate:     f.RuleMutationSkipStateUpdate,
		postHasError:   f.RuleMutationPostWriteHasError,
		postReadOnly:   f.RuleMutationPostWriteReadOnly,
		postSequence:   f.RuleMutationPostWriteSequence,
		CreateCalls:    &f.RuleCreateCalls,
		UpdateCalls:    &f.RuleUpdateCalls,
		DeleteCalls:    &f.RuleDeleteCalls,
		record:         f.recordRuleMutationTarget,
	}
}

func (f *FakeProvider) recordMutationTarget(kind, account, id string) {
	f.mutationMu.Lock()
	defer f.mutationMu.Unlock()
	value := account + ":" + id
	switch kind {
	case "mutation":
		f.MutationCallLog = append(f.MutationCallLog, value)
	case "category_list":
		f.CategoryListLog = append(f.CategoryListLog, value)
	case "category_set":
		f.CategorySetLog = append(f.CategorySetLog, value)
	}
}

// recordFolderMutationTarget keeps the operation name with the target so tests
// can assert guard ordering without relying only on aggregate counters.
func (f *FakeProvider) recordFolderMutationTarget(kind, account, id string) {
	f.folderMutationMu.Lock()
	defer f.folderMutationMu.Unlock()
	f.FolderMutationCallLog = append(f.FolderMutationCallLog, kind+":"+account+":"+id)
}

func (f *FakeProvider) recordRuleMutationTarget(kind, account, id string) {
	f.ruleMutationMu.Lock()
	defer f.ruleMutationMu.Unlock()
	f.RuleMutationCallLog = append(f.RuleMutationCallLog, kind+":"+account+":"+id)
}

func (f *FakeProvider) identity() provider.Identity {
	if f.Ident.Email == "" && f.Ident.DisplayName == "" && f.Ident.ProviderAccountID == "" {
		return provider.Identity{
			Email:             fmt.Sprintf("%s@example.test", f.Name()),
			DisplayName:       "Fake " + f.Name(),
			ProviderAccountID: "fake-" + f.Name(),
		}
	}
	return f.Ident
}

func (f *FakeProvider) cache(account string) *TokenCache {
	ident := f.identity()
	access := f.AccessToken
	if access == "" {
		access = "fake-access-token-" + account
	}
	refresh := f.RefreshToken
	if refresh == "" {
		refresh = "fake-refresh-token-" + account
	}
	return &TokenCache{
		Version:           1,
		Provider:          f.Name(),
		AccountName:       account,
		Scopes:            []string{"User.Read", "Mail.ReadWrite", "Mail.Send", "MailboxSettings.ReadWrite"},
		TokenType:         "Bearer",
		AccessToken:       access,
		RefreshToken:      refresh,
		ExpiresAt:         time.Unix(1893456000, 0),
		ProviderAccountID: ident.ProviderAccountID,
		VerifiedEmail:     ident.Email,
		DisplayName:       ident.DisplayName,
	}
}

// SeedRegistry writes a registry fixture.
func SeedRegistry(path, active string, accounts ...maildoconfig.Account) error {
	reg := &maildoconfig.Registry{
		Version:       1,
		ActiveAccount: active,
		Accounts:      append([]maildoconfig.Account(nil), accounts...),
	}
	return maildoconfig.Save(path, reg, recognizedProvidersFor(accounts...))
}

// SeedCredential writes a credential fixture for account.
func SeedCredential(root string, account maildoconfig.Account, cache *TokenCache) error {
	if cache == nil {
		cache = &TokenCache{
			Version:           1,
			Provider:          account.Provider,
			AccountName:       account.Name,
			Scopes:            []string{"User.Read", "Mail.ReadWrite", "Mail.Send", "MailboxSettings.ReadWrite"},
			TokenType:         "Bearer",
			AccessToken:       "seed-access-token-" + account.Name,
			RefreshToken:      "seed-refresh-token-" + account.Name,
			ExpiresAt:         time.Unix(1893456000, 0),
			ProviderAccountID: account.ProviderAccountID,
			VerifiedEmail:     account.Email,
			DisplayName:       account.DisplayName,
		}
	}
	data, err := encodeFakeCache(cache)
	if err != nil {
		return err
	}
	return auth.NewStore(root).SaveBytes(account.Provider, account.Name, data)
}

// SeedOutlookCache writes a healthy Outlook cache and lets tests mutate it first.
func SeedOutlookCache(root, account string, mutate func(*TokenCache)) error {
	cache := &TokenCache{
		Version:           1,
		Provider:          "outlook",
		AccountName:       account,
		Authority:         "https://login.microsoftonline.com/common",
		ClientID:          "test-client-id",
		Scopes:            append([]string(nil), outlook.DefaultScopes...),
		TokenType:         "Bearer",
		AccessToken:       "seed-outlook-access-token-" + account,
		RefreshToken:      "seed-outlook-refresh-token-" + account,
		ExpiresAt:         time.Unix(1893456000, 0),
		VerifiedEmail:     account + "@example.test",
		DisplayName:       "Outlook " + account,
		ProviderAccountID: "outlook-" + account,
	}
	if mutate != nil {
		mutate(cache)
	}
	data, err := encodeFakeCache(cache)
	if err != nil {
		return err
	}
	return auth.NewStore(root).SaveBytes("outlook", account, data)
}

// SampleFolders returns a deterministic folder tree for resolver and CLI tests.
func SampleFolders() mail.FolderList {
	return mail.FolderList{Folders: []mail.Folder{
		{
			ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox,
			ParentID: "root", Path: "Inbox", TotalCount: 10, UnreadCount: 2,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "sent-id", DisplayName: "Sent", WellKnownName: mail.WellKnownSent,
			ParentID: "root", Path: "Sent", TotalCount: 7, UnreadCount: 0,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts,
			ParentID: "root", Path: "Drafts", TotalCount: 1, UnreadCount: 0,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "archive-id", DisplayName: "Archive", WellKnownName: mail.WellKnownArchive,
			ParentID: "root", Path: "Archive", TotalCount: 3, UnreadCount: 0,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "deleted-id", DisplayName: "Deleted Items", WellKnownName: mail.WellKnownDeleted,
			ParentID: "root", Path: "Deleted Items", TotalCount: 3, UnreadCount: 0,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "junk-id", DisplayName: "Junk", WellKnownName: mail.WellKnownJunk,
			ParentID: "root", Path: "Junk", TotalCount: 2, UnreadCount: 1,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "junk-copy-id", DisplayName: "Junk Copy", WellKnownName: mail.WellKnownJunk,
			ParentID: "root", Path: "Junk Copy", TotalCount: 1, UnreadCount: 0,
			ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "clients-id", DisplayName: "Clients", ParentID: "root", Path: "Clients",
			TotalCount: 0, UnreadCount: 0, ChildCount: 1, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "acme-id", DisplayName: "ACME", ParentID: "clients-id", ParentPath: "Clients", Path: "Clients/ACME",
			TotalCount: 4, UnreadCount: 1, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "reports-1", DisplayName: "Reports", ParentID: "root", Path: "Reports",
			TotalCount: 0, UnreadCount: 0, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "reports-2", DisplayName: "Reports", ParentID: "root", Path: "Reports",
			TotalCount: 0, UnreadCount: 0, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "spare-id", DisplayName: "Spare", ParentID: "root", Path: "Spare",
			TotalCount: 0, UnreadCount: 0, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "id/with/slash", DisplayName: "Slash ID", ParentID: "root", Path: "Slash ID",
			TotalCount: 1, UnreadCount: 0, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
		{
			ID: "opaque-ab-id", DisplayName: "a/b", ParentID: "root", Path: "a/b",
			TotalCount: 1, UnreadCount: 0, ProviderMeta: fakeFolderMeta(mail.MetadataFolder),
		},
	}}
}

// SampleRules returns deterministic rule fixtures for work and personal.
func SampleRules() map[string][]mail.Rule {
	yes := true
	return map[string][]mail.Rule{
		"work": {
			{
				ID:          "rule-move-client",
				DisplayName: "Move client mail",
				Sequence:    1,
				IsEnabled:   true,
				Conditions: mail.RulePredicates{
					Semantics:       mail.DefaultRulePredicateSemantics(),
					FromAddresses:   []mail.Recipient{{Name: "Client", Address: "client@example.test"}},
					SubjectContains: []string{"invoice", "contract"},
				},
				Actions: mail.RuleActions{
					MoveToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "acme-id"},
					MarkAsRead:          &yes,
					StopProcessingRules: true,
				},
				ProviderFieldPresence: mail.RuleProviderFieldPresence{
					ActionsMarkAsRead: true, ActionsStopProcessingRules: true,
					ActionsStopProcessingRulesValueAvailable: true,
				},
				ProviderMeta: map[string]any{"source": "fake"},
			},
			{
				ID:          "rule-forward-alert",
				DisplayName: "Forward alerts",
				Sequence:    2,
				IsEnabled:   false,
				Conditions: mail.RulePredicates{
					Semantics:      mail.DefaultRulePredicateSemantics(),
					SenderContains: []string{"alerts"},
					HasAttachments: &yes,
				},
				ProviderFieldPresence: mail.RuleProviderFieldPresence{ConditionsHasAttachments: true},
				Actions: mail.RuleActions{
					ForwardTo: []mail.Recipient{{Address: "ops@external.test"}},
				},
				UnsupportedFields: []mail.RuleUnsupportedField{{
					Area: "actions", Field: "forwardTo", ValueKind: "array", Count: 1,
					Summary: "preserved documented provider field",
				}},
				ProviderMeta: map[string]any{"source": "fake"},
			},
		},
		"personal": {
			{
				ID:          "rule-personal-news",
				DisplayName: "Newsletters",
				Sequence:    1,
				IsEnabled:   true,
				Conditions: mail.RulePredicates{
					Semantics:      mail.DefaultRulePredicateSemantics(),
					HeaderContains: []string{"List-Unsubscribe"},
				},
				Actions:      mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "archive-id"}},
				ProviderMeta: map[string]any{"source": "fake"},
			},
		},
	}
}

// SampleMessages returns deterministic message rows for work and personal.
func SampleMessages() map[string][]mail.MessageSummary {
	t := func(s string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return &parsed
	}
	return map[string][]mail.MessageSummary{
		"work": {
			{
				ID: "work-new", InternetMessageID: "<work-new@example.test>", ThreadID: "thread-work-new",
				Folder: mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:   mail.Recipient{Name: "Client", Address: "client@example.test"}, ToCount: 2,
				Subject: "Contract update", ReceivedAt: t("2026-06-28T10:00:00Z"),
				IsRead: false, Importance: "high", HasAttachments: true, Preview: "Latest contract update.",
				ProviderMeta: fakeSummaryMeta(),
			},
			{
				ID: "work-old", InternetMessageID: "<work-old@example.test>", ThreadID: "thread-work-old",
				Folder: mail.FolderSummary{ID: "archive-id", DisplayName: "Archive", WellKnownName: mail.WellKnownArchive, Path: "Archive"},
				From:   mail.Recipient{Address: "archive@example.test"}, ToCount: 1, CCCount: 1,
				Subject: "Archived invoice", ReceivedAt: t("2026-06-20T08:00:00Z"),
				IsRead: true, Importance: "normal", HasAttachments: true, Preview: "Invoice details.",
				ProviderMeta: fakeSummaryMeta(),
			},
			{
				ID: "work-sent-fallback", InternetMessageID: "<work-sent@example.test>", ThreadID: "thread-work-sent",
				Folder: mail.FolderSummary{ID: "sent-id", DisplayName: "Sent", WellKnownName: mail.WellKnownSent, Path: "Sent"},
				From:   mail.Recipient{Name: "Work", Address: "work@example.test"}, ToCount: 1,
				Subject: "Sent fallback row", SentAt: t("2026-06-27T12:00:00Z"),
				IsRead: true, Importance: "normal", HasAttachments: false, Preview: "Sent row.",
				ProviderMeta: fakeSummaryMeta(),
			},
		},
		"personal": {
			{
				ID: "personal-new", InternetMessageID: "<personal-new@example.test>", ThreadID: "thread-personal-new",
				Folder: mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:   mail.Recipient{Name: "Vendor", Address: "vendor@example.test"}, ToCount: 1,
				Subject: "Invoice ready", ReceivedAt: t("2026-06-28T09:00:00Z"),
				IsRead: false, Importance: "normal", HasAttachments: true, Preview: "Your invoice is ready.",
				ProviderMeta: fakeSummaryMeta(),
			},
			{
				ID: "personal-mid", InternetMessageID: "<personal-mid@example.test>", ThreadID: "thread-personal-mid",
				Folder: mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:   mail.Recipient{Address: "friend@example.test"}, ToCount: 1,
				Subject: "Weekend", ReceivedAt: t("2026-06-26T09:00:00Z"),
				IsRead: true, Importance: "low", HasAttachments: false, Preview: "Plans.",
				ProviderMeta: fakeSummaryMeta(),
			},
			{
				ID: "personal-tie", InternetMessageID: "<personal-tie@example.test>", ThreadID: "thread-personal-tie",
				Folder: mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:   mail.Recipient{Address: "tie@example.test"}, ToCount: 1,
				Subject: "Tie breaker", ReceivedAt: t("2026-06-20T08:00:00Z"),
				IsRead: false, Importance: "normal", HasAttachments: false, Preview: "Same timestamp.",
				ProviderMeta: fakeSummaryMeta(),
			},
		},
	}
}

// SampleFullMessages returns full-message fixtures keyed by account and id.
func SampleFullMessages() map[string]map[string]mail.Message {
	t := func(s string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return &parsed
	}
	return map[string]map[string]mail.Message{
		"work": {
			"work-new": {
				ID: "work-new", InternetMessageID: "<work-new@example.test>", ThreadID: "thread-work-new",
				Folder: mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:   mail.Recipient{Name: "Client", Address: "client@example.test"},
				Sender: &mail.Recipient{Name: "Client Sender", Address: "sender@example.test"},
				ReplyTo: []mail.Recipient{
					{Name: "Client Replies", Address: "reply@example.test"},
				},
				To: []mail.Recipient{
					{Name: "Work", Address: "work@example.test"},
					{Address: "team@example.test"},
				},
				CC: []mail.Recipient{{Address: "legal@example.test"}},
				BCC: []mail.Recipient{
					{Address: "audit@example.test"},
				},
				Subject: "Contract update", ReceivedAt: t("2026-06-28T10:00:00Z"), SentAt: t("2026-06-28T09:59:00Z"),
				CreatedAt: t("2026-06-28T09:58:00Z"), LastModifiedAt: t("2026-06-28T10:01:00Z"),
				Headers: []mail.Header{
					{Name: "Message-ID", Value: "<work-new@example.test>"},
					{Name: "X-Test", Value: "fixture"},
				},
				IsRead: false, Importance: "high", Categories: []string{"Contracts", "Client"},
				HasAttachments: true,
				Attachments: []mail.Attachment{
					{ID: "att-1", Name: "contract.pdf", ContentType: "application/pdf", Size: 1200},
					{ID: "att-2", Name: "logo.png", ContentType: "image/png", Size: 320, IsInline: true, ContentID: "logo-cid"},
				},
				Body:         mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: "Full body for work-new."},
				ProviderMeta: fakeMessageMeta(),
			},
			"work-old": {
				ID: "work-old", InternetMessageID: "<work-old@example.test>", ThreadID: "thread-work-old",
				Folder:  mail.FolderSummary{ID: "archive-id", DisplayName: "Archive", WellKnownName: mail.WellKnownArchive, Path: "Archive"},
				From:    mail.Recipient{Address: "archive@example.test"},
				To:      []mail.Recipient{{Address: "work@example.test"}},
				Subject: "Archived invoice", ReceivedAt: t("2026-06-20T08:00:00Z"),
				IsRead: true, Importance: "normal", HasAttachments: false,
				Body:         mail.MessageBody{ContentType: "html", Mode: mail.BodyModeReadable, Content: "<p>Archived invoice body.</p>"},
				ProviderMeta: fakeMessageMeta(),
			},
			"work-dupe-a": {
				ID: "work-dupe-a", InternetMessageID: "<duplicate@example.test>", ThreadID: "thread-dupe",
				Folder:       mail.FolderSummary{ID: "inbox-id"},
				From:         mail.Recipient{Address: "dupe-a@example.test"},
				Subject:      "Duplicate A",
				ReceivedAt:   t("2026-06-28T07:00:00Z"),
				IsRead:       true,
				Body:         mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: "Duplicate A"},
				ProviderMeta: fakeMessageMeta(),
			},
			"work-dupe-b": {
				ID: "work-dupe-b", InternetMessageID: "<duplicate@example.test>", ThreadID: "thread-dupe",
				Folder:       mail.FolderSummary{ID: "inbox-id"},
				From:         mail.Recipient{Address: "dupe-b@example.test"},
				Subject:      "Duplicate B",
				ReceivedAt:   t("2026-06-28T07:01:00Z"),
				IsRead:       true,
				Body:         mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: "Duplicate B"},
				ProviderMeta: fakeMessageMeta(),
			},
		},
		"personal": {
			"personal-new": {
				ID: "personal-new", InternetMessageID: "<personal-new@example.test>", ThreadID: "thread-personal-new",
				Folder:       mail.FolderSummary{ID: "inbox-id", DisplayName: "Inbox", WellKnownName: mail.WellKnownInbox, Path: "Inbox"},
				From:         mail.Recipient{Name: "Vendor", Address: "vendor@example.test"},
				To:           []mail.Recipient{{Address: "personal@example.test"}},
				Subject:      "Invoice ready",
				ReceivedAt:   t("2026-06-28T09:00:00Z"),
				IsRead:       false,
				Importance:   "normal",
				Categories:   []string{"Bills"},
				Body:         mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: "Personal invoice body."},
				ProviderMeta: fakeMessageMeta(),
			},
		},
	}
}

// SampleThreads returns thread fixtures keyed by account and thread id.
func SampleThreads() map[string]map[string]mail.Thread {
	t := func(s string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return &parsed
	}
	return map[string]map[string]mail.Thread{
		"work": {
			"thread-work-new": {
				ThreadID: "thread-work-new",
				Messages: []mail.ThreadMessage{
					{
						ID: "work-thread-b", InternetMessageID: "<work-thread-b@example.test>", ThreadID: "thread-work-new",
						Folder: mail.FolderSummary{ID: "sent-id"}, From: mail.Recipient{Address: "work@example.test"},
						Subject: "Re: Contract update", SentAt: t("2026-06-28T10:03:00Z"),
						IsRead: true, Importance: "normal", Preview: "Second reply.",
					},
					{
						ID: "work-new", InternetMessageID: "<work-new@example.test>", ThreadID: "thread-work-new",
						Folder: mail.FolderSummary{ID: "inbox-id"}, From: mail.Recipient{Address: "client@example.test"},
						Subject: "Contract update", SentAt: t("2026-06-28T09:59:00Z"),
						IsRead: false, Importance: "high", HasAttachments: true, Preview: "Latest contract update.",
					},
					{
						ID: "work-thread-a", InternetMessageID: "<work-thread-a@example.test>", ThreadID: "thread-work-new",
						Folder: mail.FolderSummary{ID: "sent-id"}, From: mail.Recipient{Address: "work@example.test"},
						Subject: "Re: Contract update", SentAt: t("2026-06-28T10:03:00Z"),
						IsRead: true, Importance: "normal", Preview: "First reply.",
					},
				},
			},
		},
		"personal": {
			"thread-personal-new": {
				ThreadID: "thread-personal-new",
				Messages: []mail.ThreadMessage{
					{
						ID: "personal-new", InternetMessageID: "<personal-new@example.test>", ThreadID: "thread-personal-new",
						Folder: mail.FolderSummary{ID: "inbox-id"}, From: mail.Recipient{Address: "vendor@example.test"},
						Subject: "Invoice ready", ReceivedAt: t("2026-06-28T09:00:00Z"),
						IsRead: false, HasAttachments: true, Preview: "Your invoice is ready.",
					},
				},
			},
		},
	}
}

func cloneMessages(in map[string]mail.Message) map[string]mail.Message {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]mail.Message, len(in))
	for k, v := range in {
		out[k] = cloneMessage(v)
	}
	return out
}

func cloneMessage(in mail.Message) mail.Message {
	msg := in
	msg.Headers = append([]mail.Header(nil), in.Headers...)
	msg.ReplyTo = append([]mail.Recipient(nil), in.ReplyTo...)
	msg.To = append([]mail.Recipient(nil), in.To...)
	msg.CC = append([]mail.Recipient(nil), in.CC...)
	msg.BCC = append([]mail.Recipient(nil), in.BCC...)
	msg.Categories = append([]string(nil), in.Categories...)
	msg.Attachments = append([]mail.Attachment(nil), in.Attachments...)
	if in.ProviderMeta != nil {
		msg.ProviderMeta = cloneMessageMeta(in.ProviderMeta)
	}
	return msg
}

func summaryFromMessage(account, providerName string, msg mail.Message) mail.MessageSummary {
	return mail.MessageSummary{
		Account:           account,
		Provider:          providerName,
		ID:                msg.ID,
		InternetMessageID: msg.InternetMessageID,
		ThreadID:          msg.ThreadID,
		Folder:            msg.Folder,
		From:              msg.From,
		ToCount:           len(msg.To),
		CCCount:           len(msg.CC),
		Subject:           msg.Subject,
		ReceivedAt:        msg.ReceivedAt,
		SentAt:            msg.SentAt,
		IsRead:            msg.IsRead,
		Importance:        msg.Importance,
		HasAttachments:    msg.HasAttachments,
		Preview:           msg.Body.Content,
		ProviderMeta:      fakeSummaryMeta(),
	}
}

func indexMessagesByInternetID(in map[string]mail.Message) map[string][]mail.Message {
	out := map[string][]mail.Message{}
	for _, msg := range in {
		if msg.InternetMessageID == "" {
			continue
		}
		out[msg.InternetMessageID] = append(out[msg.InternetMessageID], msg)
	}
	return out
}

func cloneThreads(in map[string]mail.Thread) map[string]mail.Thread {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]mail.Thread, len(in))
	for k, v := range in {
		v.Messages = append([]mail.ThreadMessage(nil), v.Messages...)
		out[k] = v
	}
	return out
}

// cloneRules keeps fixture data isolated from command-side annotation and
// output tests, which can resolve folders and attach health findings.
func cloneRules(in []mail.Rule) []mail.Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Rule, len(in))
	for i, rule := range in {
		out[i] = cloneRule(rule)
	}
	return out
}

func cloneRule(in mail.Rule) mail.Rule {
	r := in
	r.Conditions = clonePredicates(in.Conditions)
	r.Exceptions = clonePredicates(in.Exceptions)
	r.Actions = cloneActions(in.Actions)
	r.HealthFindings = cloneRuleHealthFindings(in.HealthFindings)
	r.UnsupportedFields = append([]mail.RuleUnsupportedField(nil), in.UnsupportedFields...)
	r.ProviderMeta = cloneAnyMap(in.ProviderMeta)
	r.ProviderState = cloneAnyMap(in.ProviderState)
	return r
}

func cloneRuleHealthFindings(in []mail.RuleHealthFinding) []mail.RuleHealthFinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.RuleHealthFinding, len(in))
	for i, finding := range in {
		out[i] = finding
		out[i].Details = cloneAnyMap(finding.Details)
	}
	return out
}

func clonePredicates(in mail.RulePredicates) mail.RulePredicates {
	p := in
	p.FromAddresses = append([]mail.Recipient(nil), in.FromAddresses...)
	p.SenderContains = append([]string(nil), in.SenderContains...)
	p.RecipientContains = append([]string(nil), in.RecipientContains...)
	p.SubjectContains = append([]string(nil), in.SubjectContains...)
	p.HeaderContains = append([]string(nil), in.HeaderContains...)
	p.HasAttachments = cloneBool(in.HasAttachments)
	p.BodyContains = append([]string(nil), in.BodyContains...)
	p.BodyOrSubjectContains = append([]string(nil), in.BodyOrSubjectContains...)
	p.Categories = append([]string(nil), in.Categories...)
	p.WithinSizeRange = cloneRuleSizeRange(in.WithinSizeRange)
	p.SentToAddresses = append([]mail.Recipient(nil), in.SentToAddresses...)
	p.IsApprovalRequest = cloneBool(in.IsApprovalRequest)
	p.IsAutomaticForward = cloneBool(in.IsAutomaticForward)
	p.IsAutomaticReply = cloneBool(in.IsAutomaticReply)
	p.IsEncrypted = cloneBool(in.IsEncrypted)
	p.IsMeetingRequest = cloneBool(in.IsMeetingRequest)
	p.IsMeetingResponse = cloneBool(in.IsMeetingResponse)
	p.IsNonDeliveryReport = cloneBool(in.IsNonDeliveryReport)
	p.IsPermissionControlled = cloneBool(in.IsPermissionControlled)
	p.IsReadReceipt = cloneBool(in.IsReadReceipt)
	p.IsSigned = cloneBool(in.IsSigned)
	p.IsVoicemail = cloneBool(in.IsVoicemail)
	p.NotSentToMe = cloneBool(in.NotSentToMe)
	p.SentCCMe = cloneBool(in.SentCCMe)
	p.SentOnlyToMe = cloneBool(in.SentOnlyToMe)
	p.SentToMe = cloneBool(in.SentToMe)
	p.SentToOrCCMe = cloneBool(in.SentToOrCCMe)
	return p
}

func cloneActions(in mail.RuleActions) mail.RuleActions {
	a := in
	a.MoveToFolder = cloneRuleFolderTarget(in.MoveToFolder)
	a.CopyToFolder = cloneRuleFolderTarget(in.CopyToFolder)
	a.MarkAsRead = cloneBool(in.MarkAsRead)
	a.AssignCategories = append([]string(nil), in.AssignCategories...)
	a.ForwardTo = append([]mail.Recipient(nil), in.ForwardTo...)
	a.ForwardAsAttachment = append([]mail.Recipient(nil), in.ForwardAsAttachment...)
	a.RedirectTo = append([]mail.Recipient(nil), in.RedirectTo...)
	return a
}

func cloneRuleFolderTarget(in *mail.RuleFolderTarget) *mail.RuleFolderTarget {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneRuleSizeRange(in *mail.RuleSizeRange) *mail.RuleSizeRange {
	if in == nil {
		return nil
	}
	out := *in
	out.Minimum = cloneInt(in.Minimum)
	out.Maximum = cloneInt(in.Maximum)
	return &out
}

func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneAttachmentMap(in map[string][]mail.Attachment) map[string][]mail.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]mail.Attachment, len(in))
	for k, v := range in {
		out[k] = append([]mail.Attachment(nil), v...)
	}
	return out
}

func cloneAttachmentContentMap(in map[string]map[string]FakeAttachmentContent) map[string]map[string]FakeAttachmentContent {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]FakeAttachmentContent, len(in))
	for messageID, perMessage := range in {
		copyPerMessage := make(map[string]FakeAttachmentContent, len(perMessage))
		for attachmentID, content := range perMessage {
			content.Bytes = append([]byte(nil), content.Bytes...)
			copyPerMessage[attachmentID] = content
		}
		out[messageID] = copyPerMessage
	}
	return out
}

func cloneDrafts(in map[string]mail.Draft) map[string]mail.Draft {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]mail.Draft, len(in))
	for k, v := range in {
		out[k] = cloneDraft(v)
	}
	return out
}

func cloneDraft(in mail.Draft) mail.Draft {
	d := in
	d.ReplyTo = append([]mail.Recipient(nil), in.ReplyTo...)
	d.To = append([]mail.Recipient(nil), in.To...)
	d.CC = append([]mail.Recipient(nil), in.CC...)
	d.BCC = append([]mail.Recipient(nil), in.BCC...)
	d.Attachments = append([]mail.Attachment(nil), in.Attachments...)
	if in.ProviderMeta != nil {
		d.ProviderMeta = cloneDraftMeta(in.ProviderMeta)
	}
	return d
}

func cloneOutboundMessageForFake(in mail.OutboundMessage) mail.OutboundMessage {
	out := in
	if in.From != nil {
		from := *in.From
		out.From = &from
	}
	out.To = append([]mail.Recipient(nil), in.To...)
	out.CC = append([]mail.Recipient(nil), in.CC...)
	out.BCC = append([]mail.Recipient(nil), in.BCC...)
	out.ReplyTo = append([]mail.Recipient(nil), in.ReplyTo...)
	return out
}

func fakeOutboundMessageEmpty(m mail.OutboundMessage) bool {
	return m.From == nil &&
		len(m.To) == 0 &&
		len(m.CC) == 0 &&
		len(m.BCC) == 0 &&
		len(m.ReplyTo) == 0 &&
		m.Subject == "" &&
		m.Body == (mail.OutboundBody{})
}

func messageBodyFromOutbound(body mail.OutboundBody) mail.MessageBody {
	// Empty outbound bodies represent an explicit clear, not an omitted body.
	if !body.Present {
		return mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: ""}
	}
	content := body.Text
	contentType := body.ContentType
	if contentType != "text" {
		content = body.HTML
	}
	if contentType == "" {
		contentType = "text"
	}
	return mail.MessageBody{ContentType: contentType, Mode: mail.BodyModeReadable, Content: content}
}

func sortMessages(rows []mail.MessageSummary) {
	sort.SliceStable(rows, func(i, j int) bool {
		return mail.MessageSummaryLess(rows[i], rows[j])
	})
}

func limitMessages(rows []mail.MessageSummary, limit int) []mail.MessageSummary {
	if limit < 0 {
		limit = 0
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func cloneMessageMeta(in *mail.MessageProviderMetadata) *mail.MessageProviderMetadata {
	out, err := mail.CloneProviderMetadata(in)
	if err != nil {
		panic(err)
	}
	return out
}

func cloneDraftMeta(in *mail.DraftProviderMetadata) *mail.DraftProviderMetadata {
	out, err := mail.CloneProviderMetadata(in)
	if err != nil {
		panic(err)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneAnyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneAny(item)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []mail.Recipient:
		return append([]mail.Recipient(nil), x...)
	default:
		return x
	}
}

// SeedAccount adds an account to the registry and optionally writes its cache.
func SeedAccount(root, configPath, active string, account maildoconfig.Account, cache *TokenCache) error {
	reg, err := maildoconfig.Load(configPath, []string{account.Provider, "outlook"})
	if err != nil {
		return err
	}
	if _, ok := reg.Find(account.Name); !ok {
		if err := reg.Add(account); err != nil {
			return err
		}
	}
	if active != "" {
		reg.SetActive(active)
	}
	if err := maildoconfig.Save(configPath, reg, recognizedProvidersFor(reg.Accounts...)); err != nil {
		return err
	}
	if cache != nil {
		return SeedCredential(root, account, cache)
	}
	return nil
}

func recognizedProvidersFor(accounts ...maildoconfig.Account) []string {
	seen := map[string]bool{"outlook": true}
	providers := []string{"outlook"}
	for _, account := range accounts {
		if account.Provider == "" || seen[account.Provider] {
			continue
		}
		seen[account.Provider] = true
		providers = append(providers, account.Provider)
	}
	return providers
}

// AssertNoSecretLeak runs rungrad's check that none of the secrets appears in
// the output.
func AssertNoSecretLeak(t *testing.T, res testutil.Result, secrets ...string) {
	t.Helper()
	testutil.AssertRedacted(t, res, secrets...)
}
