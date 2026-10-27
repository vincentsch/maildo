package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
	"golang.org/x/term"
)

type rungradRegistrar struct {
	f *rungrad.Factory
}

func (r rungradRegistrar) RegisterSecret(value string) error {
	if r.f != nil {
		r.f.RegisterSecret(value)
	}
	return nil
}

type secretRegistrar struct {
	f      *rungrad.Factory
	closed bool
	mu     sync.Mutex
	wg     sync.WaitGroup
}

func newSecretRegistrar(f *rungrad.Factory) *secretRegistrar {
	return &secretRegistrar{f: f}
}

func (r *secretRegistrar) begin() error {
	if r == nil {
		return provider.ErrProviderRuntimeClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return provider.ErrProviderRuntimeClosed
	}
	r.wg.Add(1)
	return nil
}

func (r *secretRegistrar) end() {
	if r != nil {
		r.wg.Done()
	}
}

func (r *secretRegistrar) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *secretRegistrar) RegisterSecret(value string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if r.f != nil {
		r.f.RegisterSecret(value)
	}
	return nil
}

type providerRuntime struct {
	f         *rungrad.Factory
	provider  string
	operation provider.InteractiveAuthOperation
	closed    bool
	mu        sync.Mutex
	wg        sync.WaitGroup
}

func newProviderRuntime(f *rungrad.Factory, providerName string, operation provider.InteractiveAuthOperation) *providerRuntime {
	return &providerRuntime{f: f, provider: providerName, operation: operation}
}

func (r *providerRuntime) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *providerRuntime) begin() error {
	if r == nil {
		return provider.ErrProviderRuntimeClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return provider.ErrProviderRuntimeClosed
	}
	r.wg.Add(1)
	return nil
}

func (r *providerRuntime) end() {
	if r != nil {
		r.wg.Done()
	}
}

func (r *providerRuntime) RegisterSecret(value string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if r.f != nil {
		r.f.RegisterSecret(value)
	}
	return nil
}

func (r *providerRuntime) LookupSecretEnv(name string) (string, bool, error) {
	if err := r.begin(); err != nil {
		return "", false, err
	}
	defer r.end()
	if !allowedSecretEnv(r.provider, name) {
		return "", false, provider.ErrProviderRuntimeDenied
	}
	if r.f != nil && r.f.LookupEnv != nil {
		value, ok := r.f.LookupEnv(name)
		return value, ok, nil
	}
	value, ok := os.LookupEnv(name)
	return value, ok, nil
}

func (r *providerRuntime) OpenBrowser(ctx context.Context, url string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if r.operation != provider.InteractiveLogin && r.operation != provider.InteractiveRepair {
		return provider.ErrProviderRuntimeDenied
	}
	if r.f == nil {
		return provider.ErrProviderRuntimeDenied
	}
	return r.f.OpenBrowser(ctx, url)
}

func (r *providerRuntime) WriteNotice(text string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if r.operation != provider.InteractiveLogin && r.operation != provider.InteractiveRepair {
		return provider.ErrProviderRuntimeDenied
	}
	text = sanitizeNotice(text)
	if r.f != nil && r.f.Stderr != nil {
		fmt.Fprintln(r.f.Stderr, text)
	}
	return nil
}

// WriteProgress prints a progress line for people. It prints nothing with
// --quiet or in JSON output, where only the result belongs.
func (r *providerRuntime) WriteProgress(text string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if r.f != nil {
		r.f.Infof("%s", sanitizeNotice(text))
	}
	return nil
}

func allowedSecretEnv(providerName, key string) bool {
	switch providerName {
	case "gmail":
		return key == "MAILDO_GMAIL_CLIENT_SECRET"
	case "imap", "fastmail":
		return key == "MAILDO_IMAP_PASSWORD"
	default:
		return false
	}
}

func sanitizeNotice(text string) string {
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.Map(func(r rune) rune {
		if r == '\u001b' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

type setupEnvLookup struct {
	f        *rungrad.Factory
	provider string
	closed   bool
	mu       sync.Mutex
	wg       sync.WaitGroup
}

func (l *setupEnvLookup) lookup(name string) (string, bool, error) {
	l.mu.Lock()
	closed := l.closed
	if !closed {
		l.wg.Add(1)
	}
	l.mu.Unlock()
	if closed {
		return "", false, provider.ErrProviderRuntimeClosed
	}
	defer l.wg.Done()
	if !allowedSetupEnv(l.provider, name) {
		return "", false, provider.ErrProviderRuntimeDenied
	}
	if l.f != nil && l.f.LookupEnv != nil {
		value, ok := l.f.LookupEnv(name)
		return value, ok, nil
	}
	value, ok := os.LookupEnv(name)
	return value, ok, nil
}

func (l *setupEnvLookup) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.wg.Wait()
}

func allowedSetupEnv(providerName, key string) bool {
	switch providerName {
	case "outlook":
		return key == "MAILDO_OUTLOOK_CLIENT_ID"
	case "gmail":
		return key == "MAILDO_GMAIL_CLIENT_ID"
	default:
		return false
	}
}

type visibleInput struct {
	f      *rungrad.Factory
	reader *bufio.Reader
}

func newVisibleInput(f *rungrad.Factory) provider.VisibleInput {
	if f == nil {
		return visibleInput{}
	}
	return visibleInput{f: f, reader: stdinReader(f)}
}

// ReadLine asks for one line. Most questions have no default; then an empty
// answer comes back empty, and the caller says what is wrong with it.
func (v visibleInput) ReadLine(ctx context.Context, label, def string) (string, error) {
	if err := requireLineSafe(label); err != nil {
		return "", err
	}
	if def != "" {
		if err := requireLineSafe(def); err != nil {
			return "", err
		}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	if v.f == nil || v.reader == nil {
		return "", provider.ErrProviderRuntimeDenied
	}
	fmt.Fprint(v.f.Stderr, label+" ")
	text, err := v.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return def, nil
	}
	return text, nil
}

func requireLineSafe(text string) error {
	if text == "" || !utf8.ValidString(text) {
		return provider.ErrProviderRuntimeDenied
	}
	for _, r := range text {
		if r == '\r' || r == '\n' || r == '\u001b' || r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return provider.ErrProviderRuntimeDenied
		}
	}
	return nil
}

type credentialSource struct {
	data    []byte
	present bool
	used    bool
	closed  bool
	mu      sync.Mutex
	wg      sync.WaitGroup
}

func newCredentialSource(data []byte, present bool) *credentialSource {
	owned := append([]byte(nil), data...)
	return &credentialSource{data: owned, present: present}
}

func (s *credentialSource) close() {
	s.mu.Lock()
	s.closed = true
	data := s.data
	s.data = nil
	s.mu.Unlock()
	zeroBytes(data)
	s.wg.Wait()
}

func (s *credentialSource) WithBytes(ctx context.Context, fn func([]byte) error) (provider.CredentialPresence, error) {
	s.mu.Lock()
	if s.closed || s.used {
		s.mu.Unlock()
		return provider.CredentialAbsent, provider.ErrCredentialCapabilityClosed
	}
	s.used = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	select {
	case <-ctx.Done():
		return provider.CredentialAbsent, ctx.Err()
	default:
	}
	if !s.present {
		return provider.CredentialAbsent, nil
	}
	if fn == nil {
		return provider.CredentialPresent, nil
	}
	data := append([]byte(nil), s.data...)
	defer zeroBytes(data)
	return provider.CredentialPresent, fn(data)
}

type credentialSession struct {
	mu           sync.Mutex
	binding      auth.CredentialBinding
	codec        provider.CredentialCodec
	registrar    provider.SecretRegistrar
	current      provider.Credential
	admitted     bool
	active       bool
	allowReplace bool
	replaced     bool
	changed      bool
	rotated      bool
	closed       bool
	wg           sync.WaitGroup
}

func newCredentialSession(binding auth.CredentialBinding, codec provider.CredentialCodec, registrar provider.SecretRegistrar, current provider.Credential, allowReplace bool) *credentialSession {
	return &credentialSession{binding: binding, codec: codec, registrar: registrar, current: current, allowReplace: allowReplace}
}

func (s *credentialSession) WithCurrent(ctx context.Context, fn func(provider.CredentialLease) error) error {
	if s == nil || fn == nil {
		return provider.ErrCredentialCapabilityClosed
	}
	s.mu.Lock()
	if s.closed || s.admitted {
		s.mu.Unlock()
		return provider.ErrCredentialCapabilityClosed
	}
	s.admitted = true
	s.active = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		s.wg.Done()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return fn(&credentialLease{session: s})
}

func (s *credentialSession) closeCapabilities() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
	if registrar, ok := s.registrar.(*secretRegistrar); ok {
		registrar.close()
	}
}

func (s *credentialSession) destroy() {
	if s == nil {
		return
	}
	s.closeCapabilities()
	s.mu.Lock()
	current := s.current
	s.current = nil
	s.mu.Unlock()
	if current != nil {
		current.Destroy()
	}
}

func (s *credentialSession) encodeFinal() ([]byte, bool, error) {
	s.mu.Lock()
	changed := s.changed
	current := s.current
	s.mu.Unlock()
	if !changed {
		return nil, false, nil
	}
	data, err := s.codec.Encode(s.binding, current)
	if err != nil {
		return nil, false, provider.ErrCredentialCapabilityClosed
	}
	return data, true, nil
}

type credentialLease struct {
	session *credentialSession
}

func (l *credentialLease) Current() provider.Credential {
	s := l.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.closed {
		return nil
	}
	return s.current
}

func (l *credentialLease) Replace(candidate provider.Credential) error {
	s := l.session
	if candidate == nil || isTypedNil(candidate) {
		return provider.ErrCredentialCapabilityClosed
	}
	owned := candidate
	s.mu.Lock()
	if !s.active || s.closed || !s.allowReplace || s.replaced || owned.Destroyed() {
		s.mu.Unlock()
		owned.Destroy()
		return provider.ErrCredentialCapabilityClosed
	}
	current := s.current
	s.mu.Unlock()
	if err := s.codec.RegisterSecrets(s.registrar, owned); err != nil {
		owned.Destroy()
		return provider.ErrCredentialCapabilityClosed
	}
	rotated, err := s.codec.RefreshTokenRotated(s.binding, current, owned)
	if err != nil {
		owned.Destroy()
		return provider.ErrCredentialCapabilityClosed
	}
	s.mu.Lock()
	if !s.active || s.closed || s.replaced || s.current != current {
		s.mu.Unlock()
		owned.Destroy()
		return provider.ErrCredentialCapabilityClosed
	}
	s.current = owned
	s.replaced = true
	s.changed = true
	s.rotated = rotated
	s.mu.Unlock()
	if current != nil {
		current.Destroy()
	}
	return nil
}

func (h *handlers) loadCredentialSessionFromBytes(f *rungrad.Factory, account maildoconfig.Account, reg *maildoconfig.Registry, data []byte, present bool, allowReplace bool) (*credentialSession, provider.Provider, provider.CredentialCodec, error) {
	p, codec, err := h.lookupProviderCodec(account.Provider)
	if err != nil {
		return nil, nil, nil, err
	}
	binding := auth.CredentialBinding{Provider: account.Provider, Account: account.Name}
	if !present {
		return nil, p, codec, repairRequired(account.Name)
	}
	owned := append([]byte(nil), data...)
	defer zeroBytes(owned)
	registrar := newSecretRegistrar(f)
	cred, err := codec.Decode(registrar, binding, owned)
	if err != nil {
		registrar.close()
		return nil, p, codec, repairRequired(account.Name)
	}
	setup, err := h.setupForRegisteredProvider(reg, account)
	if err != nil {
		registrar.close()
		cred.Destroy()
		return nil, nil, nil, err
	}
	session := newCredentialSession(binding, codec, registrar, cred, allowReplace)
	_ = setup
	return session, p, codec, nil
}

func (h *handlers) loadCredentialSession(ctx context.Context, f *rungrad.Factory, coord *auth.Coordinator, account maildoconfig.Account, reg *maildoconfig.Registry, allowReplace bool) (*credentialSession, provider.Provider, provider.CredentialCodec, auth.StateRevision, error) {
	_, rev, data, present, err := coord.Snapshot(ctx, account.Provider, account.Name)
	if err != nil {
		return nil, nil, nil, auth.StateRevision{}, err
	}
	defer zeroBytes(data)
	session, p, codec, err := h.loadCredentialSessionFromBytes(f, account, reg, data, present, allowReplace)
	return session, p, codec, rev, err
}

func (h *handlers) storedAuthContext(account maildoconfig.Account, reg *maildoconfig.Registry, session *credentialSession) (provider.StoredAuthContext, error) {
	if session == nil || session.registrar == nil {
		return provider.StoredAuthContext{}, provider.ErrCredentialCapabilityClosed
	}
	setup, err := h.setupForRegisteredProvider(reg, account)
	if err != nil {
		return provider.StoredAuthContext{}, err
	}
	return provider.StoredAuthContext{
		Runtime:                   session.registrar,
		Provider:                  account.Provider,
		Account:                   account.Name,
		Setup:                     setup,
		ExpectedEmail:             account.Email,
		ExpectedDisplayName:       account.DisplayName,
		ExpectedProviderAccountID: account.ProviderAccountID,
		SenderName:                account.SenderName,
		Credential:                session,
	}, nil
}

func (h *handlers) setupForRegisteredProvider(reg *maildoconfig.Registry, account maildoconfig.Account) (maildoconfig.ProviderSetup, error) {
	setup, err := maildoconfig.SetupFor(reg, account)
	if err == nil {
		return setup, nil
	}
	if maildoconfig.ProviderHasSetup(account.Provider) {
		return maildoconfig.ProviderSetup{}, err
	}
	if h.deps.Providers != nil {
		if _, _, ok := h.deps.Providers.Get(account.Provider); ok {
			return maildoconfig.ProviderSetup{}, nil
		}
	}
	return maildoconfig.ProviderSetup{}, err
}

func (h *handlers) applyAuthSetup(reg *maildoconfig.Registry, account *maildoconfig.Account, setup maildoconfig.ProviderSetup) error {
	if account == nil || maildoconfig.ProviderHasSetup(account.Provider) {
		return maildoconfig.ApplySetup(reg, account, setup)
	}
	if !setup.Equal(maildoconfig.ProviderSetup{}) {
		return maildoconfig.ApplySetup(reg, account, setup)
	}
	return nil
}

func (h *handlers) localStatus(ctx context.Context, f *rungrad.Factory, coord *auth.Coordinator, account maildoconfig.Account, reg *maildoconfig.Registry) (provider.LocalStatus, error) {
	present, err := coord.CredentialExists(ctx, account.Provider, account.Name, false)
	if err != nil {
		return provider.LocalAbsent, err
	}
	if !present {
		return provider.LocalAbsent, nil
	}
	session, p, _, _, err := h.loadCredentialSession(ctx, f, coord, account, reg, false)
	if err != nil {
		if isAuthRepairRequired(err) {
			return provider.LocalRepairRequired, nil
		}
		return provider.LocalAbsent, err
	}
	defer session.destroy()
	ac, err := h.storedAuthContext(account, reg, session)
	if err != nil {
		return provider.LocalAbsent, err
	}
	status, err := p.LocalHealth(ctx, ac)
	session.closeCapabilities()
	if err != nil {
		return provider.LocalAbsent, err
	}
	return status, nil
}

func (h *handlers) forceRefresh(ctx context.Context, f *rungrad.Factory, coord *auth.Coordinator, account maildoconfig.Account, reg *maildoconfig.Registry, operation errs.AuthStateOperation) (provider.RefreshResult, error) {
	session, p, _, rev, err := h.loadCredentialSession(ctx, f, coord, account, reg, true)
	if err != nil {
		return provider.RefreshResult{}, err
	}
	defer session.destroy()
	ac, err := h.storedAuthContext(account, reg, session)
	if err != nil {
		return provider.RefreshResult{}, err
	}
	result, err := p.ForcedRefresh(ctx, ac)
	session.closeCapabilities()
	if err != nil {
		return provider.RefreshResult{}, mapProviderContractError(err)
	}
	if err := ctx.Err(); err != nil {
		return provider.RefreshResult{}, err
	}
	if result.RotatedRefreshToken != session.rotated {
		return provider.RefreshResult{}, provider.ErrCredentialCapabilityClosed
	}
	data, changed, err := session.encodeFinal()
	if err != nil {
		return provider.RefreshResult{}, mapProviderContractError(err)
	}
	defer zeroBytes(data)
	if changed {
		if err := coord.SaveCredentialBytes(ctx, rev, account.Provider, account.Name, data, operation); err != nil {
			return provider.RefreshResult{}, err
		}
	}
	result.RefreshChecked = true
	result.CredentialChanged = changed
	return result, nil
}

func (h *handlers) openAccountClient(ctx context.Context, f *rungrad.Factory, coord *auth.Coordinator, req provider.AccountClientRequest) (provider.AccountClient, error) {
	account := maildoconfig.Account{
		Name:              req.Binding.Account,
		Provider:          req.Binding.Provider,
		Email:             req.ExpectedEmail,
		DisplayName:       req.ExpectedDisplayName,
		ProviderAccountID: req.ExpectedProviderAccountID,
		AuthRef:           req.AuthRef,
	}
	reg := &maildoconfig.Registry{Version: 1, Accounts: []maildoconfig.Account{account}}
	switch req.Binding.Provider {
	case "outlook":
		if req.Setup.Outlook != nil && req.Setup.Outlook.ClientID != "" {
			reg.Outlook = &maildoconfig.OutlookConfig{ClientID: req.Setup.Outlook.ClientID}
		}
	case "gmail":
		if req.Setup.Gmail != nil && req.Setup.Gmail.ClientID != "" {
			reg.Gmail = &maildoconfig.GmailConfig{ClientID: req.Setup.Gmail.ClientID}
		}
	case "imap", "fastmail":
		if req.Setup.MailServer != nil {
			account.Email = req.Setup.MailServer.Email
			account.Username = req.Setup.MailServer.Username
			account.MailServer = &maildoconfig.MailServerConfig{IMAP: req.Setup.MailServer.IMAP, SMTP: req.Setup.MailServer.SMTP, CAFile: req.Setup.MailServer.CAFile}
			reg.Accounts[0] = account
		}
	}
	currentReg, rev, data, present, err := coord.Snapshot(ctx, req.Binding.Provider, req.Binding.Account)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(data)
	current, ok := currentReg.Find(req.Binding.Account)
	if !ok || current.Provider != req.Binding.Provider || current.AuthRef != req.AuthRef || current.Email != req.ExpectedEmail || current.DisplayName != req.ExpectedDisplayName || current.ProviderAccountID != req.ExpectedProviderAccountID {
		return nil, errs.NewAuthStateChanged(req.Binding.Account, req.Binding.Provider, errs.AuthStateClientOpen)
	}
	currentSetup, err := h.setupForRegisteredProvider(currentReg, *current)
	if err != nil {
		return nil, err
	}
	if !currentSetup.Equal(req.Setup) {
		return nil, errs.NewAuthStateChanged(req.Binding.Account, req.Binding.Provider, errs.AuthStateClientOpen)
	}
	// Replies, forwards and draft sends put the sender name in From.
	account.SenderName = current.SenderName
	session, p, _, err := h.loadCredentialSessionFromBytes(f, account, reg, data, present, true)
	if err != nil {
		return nil, err
	}
	defer session.destroy()
	ac, err := h.storedAuthContext(account, reg, session)
	if err != nil {
		return nil, err
	}
	client, err := p.NewAccountClient(ctx, ac)
	if err != nil {
		destroyAccountClient(client)
		return nil, mapProviderContractError(err)
	}
	session.closeCapabilities()
	if err := ctx.Err(); err != nil {
		destroyAccountClient(client)
		return nil, err
	}
	if err := validateAccountClient(client, req.Binding.Provider, req.Binding.Account); err != nil {
		destroyAccountClient(client)
		return nil, err
	}
	data, changed, err := session.encodeFinal()
	if err != nil {
		destroyAccountClient(client)
		return nil, mapProviderContractError(err)
	}
	defer zeroBytes(data)
	if changed {
		if err := ctx.Err(); err != nil {
			destroyAccountClient(client)
			return nil, err
		}
		if err := coord.SaveCredentialBytes(ctx, rev, req.Binding.Provider, req.Binding.Account, data, errs.AuthStateClientRefresh); err != nil {
			destroyAccountClient(client)
			return nil, err
		}
	}
	return client, nil
}

func (h *handlers) clientBinder(f *rungrad.Factory) provider.AccountClientBinder {
	coord, coordErr := h.coordinator(f)
	return func(req provider.AccountClientRequest) (provider.AccountClientOpenFunc, error) {
		if coordErr != nil {
			return nil, coordErr
		}
		owned := provider.AccountClientRequest{
			Binding:                   req.Binding,
			AuthRef:                   req.AuthRef,
			Setup:                     req.Setup.Clone(),
			ExpectedEmail:             req.ExpectedEmail,
			ExpectedDisplayName:       req.ExpectedDisplayName,
			ExpectedProviderAccountID: req.ExpectedProviderAccountID,
		}
		return func(ctx context.Context) (provider.AccountClient, error) {
			return h.openAccountClient(ctx, f, coord, owned)
		}, nil
	}
}

func (h *handlers) localLogoutIntentFromBytes(ctx context.Context, f *rungrad.Factory, account maildoconfig.Account, reg *maildoconfig.Registry, data []byte, present bool) (bool, error) {
	if !present {
		return false, nil
	}
	session, p, _, err := h.loadCredentialSessionFromBytes(f, account, reg, data, present, false)
	if err == nil {
		ac, ctxErr := h.storedAuthContext(account, reg, session)
		if ctxErr != nil {
			session.destroy()
			return false, ctxErr
		}
		err = p.Logout(ctx, ac)
		session.closeCapabilities()
		session.destroy()
		if err != nil {
			return false, mapProviderContractError(err)
		}
	} else if !isAuthRepairRequired(err) {
		return false, err
	}
	return true, nil
}

type terminalSecretReader struct{}

func (terminalSecretReader) ReadHidden(ctx context.Context, f *rungrad.Factory, label string) ([]byte, error) {
	if err := requireLineSafe(label); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if f == nil || f.Stderr == nil {
		return nil, provider.ErrProviderRuntimeDenied
	}
	file, ok := f.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		what := strings.TrimSuffix(label, ":")
		what = strings.ToLower(what[:1]) + what[1:]
		return nil, rungrad.NewError(rungrad.ExitUsage, "maildo has to ask for the "+what+", but there is no terminal. Run the command in a terminal, or give the secret in its environment variable (MAILDO_IMAP_PASSWORD for IMAP accounts)")
	}
	fmt.Fprint(f.Stderr, label+" ")
	secret, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(f.Stderr)
	if err != nil {
		zeroBytes(secret)
		return nil, err
	}
	select {
	case <-ctx.Done():
		zeroBytes(secret)
		return nil, ctx.Err()
	default:
	}
	return secret, nil
}

type interactiveSecretSource struct {
	reader provider.SecretReader
	f      *rungrad.Factory
	used   bool
	closed bool
	mu     sync.Mutex
	wg     sync.WaitGroup
}

func (h *handlers) newInteractiveSecretSource(f *rungrad.Factory) *interactiveSecretSource {
	reader := h.deps.SecretReader
	if reader == nil {
		reader = terminalSecretReader{}
	}
	return &interactiveSecretSource{reader: reader, f: f}
}

func (s *interactiveSecretSource) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *interactiveSecretSource) WithSecret(ctx context.Context, label string, fn func([]byte) error) error {
	s.mu.Lock()
	if s.closed || s.used {
		s.mu.Unlock()
		return provider.ErrCredentialCapabilityClosed
	}
	s.used = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	if fn == nil {
		return provider.ErrCredentialCapabilityClosed
	}
	secret, err := s.reader.ReadHidden(ctx, s.f, label)
	if err != nil {
		return err
	}
	defer zeroBytes(secret)
	return fn(secret)
}

func destroyInteractiveResult(result provider.InteractiveAuthResult) {
	if result.Credential == nil || isTypedNil(result.Credential) {
		return
	}
	result.Credential.Destroy()
}

func validateAccountClient(client provider.AccountClient, providerName, accountName string) (err error) {
	if client == nil || isTypedNil(client) {
		return provider.ErrAccountClientInvalid
	}
	defer func() {
		if recover() != nil {
			err = provider.ErrAccountClientInvalid
		}
	}()
	if client.ProviderName() != providerName || client.AccountName() != accountName {
		return provider.ErrAccountClientInvalid
	}
	return nil
}

func destroyAccountClient(client provider.AccountClient) {
	if client == nil || isTypedNil(client) {
		return
	}
	defer func() {
		_ = recover()
	}()
	client.Destroy()
}

func mapProviderContractError(err error) error {
	if err == nil {
		return nil
	}
	for _, target := range []error{
		provider.ErrProviderRuntimeClosed,
		provider.ErrProviderRuntimeDenied,
		provider.ErrCredentialCapabilityClosed,
		provider.ErrCredentialDestroyed,
		provider.ErrAccountClientInvalid,
	} {
		if errors.Is(err, target) {
			return errs.New(errs.CodeCapabilityNotImplemented, "maildo hit an internal error in its code for this mail service; please report it")
		}
	}
	return err
}

func repairRequired(account string) error {
	return errs.New(errs.CodeAuthRepairRequired, fmt.Sprintf("the saved login for %q cannot be used; run %s", account, repairCommand(account)))
}

func isTypedNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func previewFields(fields []provider.SetupPreviewField) []output.Field {
	out := make([]output.Field, 0, len(fields))
	for _, field := range fields {
		out = append(out, output.Field{Name: field.Name, Value: field.Value})
	}
	return out
}
