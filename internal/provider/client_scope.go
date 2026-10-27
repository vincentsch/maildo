package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// AccountClientScope owns the account clients one command opens. Each
// account's client is opened on first use, shared by every later call in the
// command, and destroyed when the command ends.
type AccountClientScope interface {
	accountClientScope()
}

// ClientAccessKind names what a command wants to do with an account. Each
// kind lists the capabilities the account must have.
type ClientAccessKind string

const (
	ClientAccessAccountIdentityForTest                   ClientAccessKind = "account_identity_for_test"
	ClientAccessArchivePolicyReport                      ClientAccessKind = "archive_policy_report"
	ClientAccessCategoriesRead                           ClientAccessKind = "categories_read"
	ClientAccessCategoriesMutate                         ClientAccessKind = "categories_mutate"
	ClientAccessComposeSourceRead                        ClientAccessKind = "compose_source_read"
	ClientAccessDirectSend                               ClientAccessKind = "direct_send"
	ClientAccessDraftAttachmentsMutate                   ClientAccessKind = "draft_attachments_mutate"
	ClientAccessDraftCreate                              ClientAccessKind = "draft_create"
	ClientAccessDraftRead                                ClientAccessKind = "draft_read"
	ClientAccessDraftSend                                ClientAccessKind = "draft_send"
	ClientAccessDraftUpdate                              ClientAccessKind = "draft_update"
	ClientAccessFoldersMutate                            ClientAccessKind = "folders_mutate"
	ClientAccessFoldersRead                              ClientAccessKind = "folders_read"
	ClientAccessForwardDraftCreate                       ClientAccessKind = "forward_draft_create"
	ClientAccessForwardDraftSend                         ClientAccessKind = "forward_draft_send"
	ClientAccessForwardDraftCreateWithAttachments        ClientAccessKind = "forward_draft_create_with_attachments"
	ClientAccessForwardDraftSendWithAttachments          ClientAccessKind = "forward_draft_send_with_attachments"
	ClientAccessForwardDraftCreatePreviewWithAttachments ClientAccessKind = "forward_draft_create_preview_with_attachments"
	ClientAccessForwardDraftSendPreviewWithAttachments   ClientAccessKind = "forward_draft_send_preview_with_attachments"
	ClientAccessMessageAttachmentsRead                   ClientAccessKind = "message_attachments_read"
	ClientAccessMessagesList                             ClientAccessKind = "messages_list"
	ClientAccessMessageMove                              ClientAccessKind = "message_move"
	ClientAccessMessageCopy                              ClientAccessKind = "message_copy"
	ClientAccessMessageArchive                           ClientAccessKind = "message_archive"
	ClientAccessMessageDelete                            ClientAccessKind = "message_delete"
	ClientAccessMessageRestore                           ClientAccessKind = "message_restore"
	ClientAccessMessageJunk                              ClientAccessKind = "message_junk"
	ClientAccessMessageMarkRead                          ClientAccessKind = "message_mark_read"
	ClientAccessMessageFlag                              ClientAccessKind = "message_flag"
	ClientAccessMessagesRead                             ClientAccessKind = "messages_read"
	ClientAccessMessagesSearch                           ClientAccessKind = "messages_search"
	ClientAccessReplyDraftCreate                         ClientAccessKind = "reply_draft_create"
	ClientAccessReplyDraftSend                           ClientAccessKind = "reply_draft_send"
	ClientAccessRulesMutate                              ClientAccessKind = "rules_mutate"
	ClientAccessRulesRead                                ClientAccessKind = "rules_read"
	ClientAccessThreadsRead                              ClientAccessKind = "threads_read"
	ClientAccessDiagnose                                 ClientAccessKind = "diagnose"
)

// accessCapabilities lists, for each access kind, what the account's provider
// must support before maildo opens a client for it.
var accessCapabilities = map[ClientAccessKind][]Capability{
	ClientAccessAccountIdentityForTest:                   nil,
	ClientAccessArchivePolicyReport:                      {CapArchivePolicyReport, CapFoldersRead},
	ClientAccessCategoriesRead:                           {CapCategoriesRead},
	ClientAccessCategoriesMutate:                         {CapCategoriesMutate},
	ClientAccessComposeSourceRead:                        {CapMessagesRead},
	ClientAccessDirectSend:                               {CapSendDirect},
	ClientAccessDraftAttachmentsMutate:                   {CapDraftRead, CapAttachmentsMutate},
	ClientAccessDraftCreate:                              {CapDraftCreate},
	ClientAccessDraftRead:                                {CapDraftRead},
	ClientAccessDraftSend:                                {CapDraftRead, CapDraftSend},
	ClientAccessDraftUpdate:                              {CapDraftRead, CapDraftUpdate},
	ClientAccessFoldersMutate:                            {CapFoldersMutate},
	ClientAccessFoldersRead:                              {CapFoldersRead},
	ClientAccessForwardDraftCreate:                       {CapForwardDraftCreate},
	ClientAccessForwardDraftSend:                         {CapForwardDraftCreate, CapDraftSend},
	ClientAccessForwardDraftCreateWithAttachments:        {CapForwardDraftCreate, CapAttachmentsRead, CapAttachmentsMutate},
	ClientAccessForwardDraftSendWithAttachments:          {CapForwardDraftCreate, CapDraftSend, CapAttachmentsRead, CapAttachmentsMutate},
	ClientAccessForwardDraftCreatePreviewWithAttachments: {CapForwardDraftCreate, CapAttachmentsRead, CapAttachmentsMutate},
	ClientAccessForwardDraftSendPreviewWithAttachments:   {CapForwardDraftCreate, CapDraftSend, CapAttachmentsRead, CapAttachmentsMutate},
	ClientAccessMessageAttachmentsRead:                   {CapAttachmentsRead},
	ClientAccessMessagesList:                             {CapMessagesList},
	ClientAccessMessageMove:                              {CapMessagesMove},
	ClientAccessMessageCopy:                              {CapMessagesCopy},
	ClientAccessMessageArchive:                           {CapMessagesArchive},
	ClientAccessMessageDelete:                            {CapMessagesDelete},
	ClientAccessMessageRestore:                           {CapMessagesRestore},
	ClientAccessMessageJunk:                              {CapMessagesJunk},
	ClientAccessMessageMarkRead:                          {CapMessagesMarkRead},
	ClientAccessMessageFlag:                              {CapMessagesFlag},
	ClientAccessMessagesRead:                             {CapMessagesRead},
	ClientAccessMessagesSearch:                           {CapMessagesSearch},
	ClientAccessReplyDraftCreate:                         {CapReplyDraftCreate},
	ClientAccessReplyDraftSend:                           {CapReplyDraftCreate, CapDraftSend},
	ClientAccessRulesMutate:                              {CapRulesMutate},
	ClientAccessRulesRead:                                {CapRulesRead},
	ClientAccessThreadsRead:                              {CapThreadsRead},
	ClientAccessDiagnose:                                 nil,
}

type commandAccountClientScope struct {
	mu      sync.Mutex
	closing bool
	closed  bool
	cache   map[accountClientKey]*clientEntry
	order   []accountClientKey
	wg      sync.WaitGroup
}

func (*commandAccountClientScope) accountClientScope() {}

type accountClientKey struct {
	provider string
	account  string
}

type clientEntry struct {
	client AccountClient
	err    error
	ready  chan struct{}
}

type resolvedAccountToken struct {
	scope      *commandAccountClientScope
	provider   string
	account    string
	email      string
	display    string
	senderName string
	open       AccountClientOpenFunc
	caps       Capabilities
}

// WithAccountClientScope runs fn with a fresh scope and destroys every client
// opened in it when fn returns, even after a panic.
func WithAccountClientScope(ctx context.Context, fn func(AccountClientScope) error) error {
	if fn == nil {
		return errs.NewProviderClientContractInvalid()
	}
	scope := &commandAccountClientScope{cache: map[accountClientKey]*clientEntry{}}
	defer scope.close()
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fn(scope)
}

func (s *commandAccountClientScope) active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closing && !s.closed
}

func (s *commandAccountClientScope) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closing = true
	s.mu.Unlock()
	s.wg.Wait()

	s.mu.Lock()
	keys := append([]accountClientKey(nil), s.order...)
	entries := make(map[accountClientKey]*clientEntry, len(s.cache))
	for k, entry := range s.cache {
		entries[k] = entry
	}
	s.mu.Unlock()
	// Destroy in reverse order of opening. A panicking Destroy must not stop
	// the others from running.
	for i := len(keys) - 1; i >= 0; i-- {
		entry := entries[keys[i]]
		if entry == nil {
			continue
		}
		<-entry.ready
		if entry.client != nil && !isNil(entry.client) {
			func() {
				defer func() { _ = recover() }()
				entry.client.Destroy()
			}()
		}
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// WithAccountClient checks that account can do what access needs, opens the
// account's client (once per command) and passes it to fn as T, one of the
// narrow interfaces in this package. A client that lacks T's methods gets a
// "capability not implemented" error.
func WithAccountClient[T any](ctx context.Context, scope AccountClientScope, account ResolvedAccount, access ClientAccessKind, fn func(T) error) error {
	realScope, ok := scope.(*commandAccountClientScope)
	if !ok || realScope == nil || fn == nil {
		return errs.NewProviderClientContractInvalid()
	}
	token := account.token
	if token == nil || token.scope != realScope || token.account != account.Name || token.provider != account.Provider || token.open == nil {
		return errs.NewProviderClientContractInvalid()
	}
	caps, known := accessCapabilities[access]
	if !known {
		return errs.NewProviderClientContractInvalid()
	}
	if access == ClientAccessAccountIdentityForTest && !account.allowIdentityAccessForTest {
		return errs.NewProviderClientContractInvalid()
	}
	for _, capability := range caps {
		if err := CheckCapability(account, token.caps, capability); err != nil {
			return err
		}
	}
	if err := CheckPolicy(account, access); err != nil {
		return err
	}
	s := realScope
	s.mu.Lock()
	if s.closing || s.closed {
		s.mu.Unlock()
		return errs.NewProviderClientContractInvalid()
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	client, err := s.client(ctx, token)
	if err != nil {
		return err
	}
	// The command may have finished while the client was opening.
	if !s.active() {
		return errs.NewProviderClientContractInvalid()
	}
	var target any = client
	if access == ClientAccessComposeSourceRead {
		raw, ok := client.(ComposeSourceMaterialReader)
		if !ok {
			return notImplemented(account, access)
		}
		target = composeSourceReader{raw: raw, account: account.Name, provider: account.Provider, scope: s}
	}
	typed, ok := target.(T)
	if !ok {
		return notImplemented(account, access)
	}
	return fn(typed)
}

// CheckClientAccess checks the capabilities behind an access kind without
// opening the account client.
func CheckClientAccess(account ResolvedAccount, access ClientAccessKind) error {
	token := account.token
	if token == nil || token.account != account.Name || token.provider != account.Provider {
		return errs.NewProviderClientContractInvalid()
	}
	caps, known := accessCapabilities[access]
	if !known {
		return errs.NewProviderClientContractInvalid()
	}
	for _, capability := range caps {
		if err := CheckCapability(account, token.caps, capability); err != nil {
			return err
		}
	}
	return CheckPolicy(account, access)
}

// client opens the account's client the first time it is needed. Callers
// that arrive while it is opening wait for that one attempt.
func (s *commandAccountClientScope) client(ctx context.Context, token *resolvedAccountToken) (AccountClient, error) {
	key := accountClientKey{provider: token.provider, account: token.account}
	s.mu.Lock()
	if entry := s.cache[key]; entry != nil {
		s.mu.Unlock()
		select {
		case <-entry.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return entry.client, entry.err
	}
	entry := &clientEntry{ready: make(chan struct{})}
	s.cache[key] = entry
	s.order = append(s.order, key)
	s.mu.Unlock()

	client, err := token.open(ctx)
	if err == nil {
		err = validateScopedClient(client, token.provider, token.account)
	}
	if err != nil && client != nil && !isNil(client) {
		client.Destroy()
		client = nil
	}
	entry.client, entry.err = client, err
	close(entry.ready)
	return client, err
}

func validateScopedClient(client AccountClient, providerName, accountName string) error {
	if client == nil || isNil(client) {
		return ErrAccountClientInvalid
	}
	gotProvider, gotAccount, err := safeClientIdentity(client)
	if err != nil || gotProvider != providerName || gotAccount != accountName {
		return ErrAccountClientInvalid
	}
	return nil
}

// composeSourceReader turns the original message read from the provider into
// a snapshot that only this command and account can use.
type composeSourceReader struct {
	raw      ComposeSourceMaterialReader
	account  string
	provider string
	scope    *commandAccountClientScope
}

func (r composeSourceReader) ReadComposeSource(ctx context.Context, access ComposeSourceAccess, selector mail.ResolvedMessageSelector) (*ComposeSourceSnapshot, error) {
	if !access.valid || access.scope != r.scope || access.account != r.account || access.provider != r.provider {
		return nil, errs.NewProviderClientContractInvalid()
	}
	material, err := r.raw.ReadComposeSourceMaterial(ctx, selector)
	if err != nil {
		return nil, err
	}
	defer destroyComposeSourceMaterial(&material)
	if err := validateComposeSourceSelector(selector, material); err != nil {
		return nil, err
	}
	return newComposeSourceSnapshot(r.account, r.provider, r.scope, material)
}

func notImplemented(account ResolvedAccount, access ClientAccessKind) error {
	details := map[string]any{
		"account":  account.Name,
		"provider": account.Provider,
		"access":   string(access),
	}
	if caps := accessCapabilities[access]; len(caps) > 0 {
		details["capability"] = string(caps[0])
	}
	return errs.NewWithDetails(errs.CodeCapabilityNotImplemented,
		fmt.Sprintf("account %q (%s) cannot do this yet (%s)", account.Name, account.Provider, access), details)
}
