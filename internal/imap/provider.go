package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	smtpclient "github.com/emersion/go-smtp"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/discover"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"golang.org/x/net/html/charset"
)

const (
	SMTPMaxReplyBytes int64 = 64 * 1024

	dialTimeout     = 10 * time.Second
	tlsTimeout      = 10 * time.Second
	greetingTimeout = 10 * time.Second
	commandTimeout  = 30 * time.Second
	logoutTimeout   = 3 * time.Second
)

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type Options struct {
	DialContext DialContextFunc
	TLSConfig   func(serverName string) *tls.Config
}

type Provider struct {
	name string
	opts Options
}

func New(opts Options) *Provider {
	return NewProvider("imap", opts)
}

func NewProvider(name string, opts Options) *Provider {
	return &Provider{name: name, opts: opts}
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Capabilities(context.Context) (provider.Capabilities, error) {
	caps := make(provider.Capabilities, len(provider.AllCapabilities()))
	for _, cap := range provider.AllCapabilities() {
		caps[cap] = provider.CapSupportedNotImpl
	}
	for _, cap := range []provider.Capability{
		provider.CapFoldersRead,
		provider.CapFoldersMutate,
		provider.CapMessagesSearch,
		provider.CapMessagesList,
		provider.CapMessagesRead,
		provider.CapThreadsRead,
		provider.CapAttachmentsRead,
		provider.CapMessagesMove,
		provider.CapMessagesCopy,
		provider.CapMessagesArchive,
		provider.CapMessagesDelete,
		provider.CapMessagesRestore,
		provider.CapMessagesJunk,
		provider.CapMessagesMarkRead,
		provider.CapMessagesFlag,
		provider.CapSendDirect,
		provider.CapDraftCreate,
		provider.CapDraftRead,
		provider.CapDraftUpdate,
		provider.CapDraftSend,
		provider.CapReplyDraftCreate,
		provider.CapForwardDraftCreate,
		provider.CapAttachmentsMutate,
	} {
		caps[cap] = provider.CapImplemented
	}
	for _, cap := range []provider.Capability{
		provider.CapCategoriesRead,
		provider.CapCategoriesMutate,
		provider.CapRulesRead,
		provider.CapRulesMutate,
		provider.CapArchivePolicyReport,
	} {
		caps[cap] = provider.CapUnsupported
	}
	return caps, nil
}

func (p *Provider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	if ac.Flow == provider.InteractiveAuthFlowDeviceCode {
		return provider.InteractiveAuthResult{}, rungrad.NewError(rungrad.ExitUsage, "--device-code works only for Outlook accounts")
	}
	setup, err := mailServerSetup(ac.Setup, ac.Provider)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	envSet := false
	if ac.Operation == provider.InteractiveLogin {
		var err error
		envSet, err = p.envPasswordSet(ac)
		if err != nil {
			return provider.InteractiveAuthResult{}, err
		}
	}
	if ac.Operation == provider.InteractiveLogin && !envSet {
		if cached, ok, err := p.reusableLoginCredential(ctx, ac, setup); err != nil {
			return provider.InteractiveAuthResult{}, err
		} else if ok {
			return provider.InteractiveAuthResult{
				Setup:      ac.Setup.Clone(),
				Credential: cached,
				Identity: provider.Identity{
					Email:     setup.Email,
					Assurance: provider.IdentityConfigured,
				},
			}, nil
		}
	}
	cred, err := p.promptCredential(ctx, ac, setup)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	if ac.Runtime != nil {
		hosts := setup.IMAP.Host
		if setup.SMTP.Host != setup.IMAP.Host {
			hosts += " and " + setup.SMTP.Host
		}
		// Progress, unlike a notice, stays quiet under --quiet and --json.
		if progress, ok := ac.Runtime.(interface{ WriteProgress(string) error }); ok {
			_ = progress.WriteProgress("Checking the login with " + hosts + "...")
		}
	}
	fresh := ac.Operation == provider.InteractiveLogin && ac.Existing == nil
	if err := p.verifyBoth(ctx, ac.Account, setup, cred, ac.Operation, fresh); err != nil {
		cred.Destroy()
		return provider.InteractiveAuthResult{}, err
	}
	return provider.InteractiveAuthResult{
		Setup:      ac.Setup.Clone(),
		Credential: cred,
		Identity: provider.Identity{
			Email:     setup.Email,
			Assurance: provider.IdentityConfigured,
		},
	}, nil
}

func (p *Provider) envPasswordSet(ac provider.InteractiveAuthContext) (bool, error) {
	if ac.Runtime == nil {
		return false, nil
	}
	_, ok, err := ac.Runtime.LookupSecretEnv(envPassword)
	return ok, err
}

func (p *Provider) reusableLoginCredential(ctx context.Context, ac provider.InteractiveAuthContext, setup maildoconfig.MailServerSetup) (*passwordCredential, bool, error) {
	if ac.Existing == nil {
		return nil, false, nil
	}
	var out *passwordCredential
	presence, err := ac.Existing.WithBytes(ctx, func(raw []byte) error {
		codec := NewCredentialCodec(ac.Provider)
		cred, err := codec.Decode(ac.Runtime, credentialBinding(ac.Provider, ac.Account), raw)
		if err != nil {
			return nil
		}
		cache, ok := cred.(*passwordCredential)
		if !ok || cache == nil || cache.Destroyed() || cache.ConfiguredEmail != setup.Email {
			if cred != nil {
				cred.Destroy()
			}
			return nil
		}
		if err := p.verifyBoth(ctx, ac.Account, setup, cache, ac.Operation, false); err != nil {
			cred.Destroy()
			return err
		}
		out, err = cache.clone()
		cred.Destroy()
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, presence == provider.CredentialPresent && out != nil, nil
}

func (p *Provider) promptCredential(ctx context.Context, ac provider.InteractiveAuthContext, setup maildoconfig.MailServerSetup) (*passwordCredential, error) {
	label := "Password for " + setup.Email + ":"
	if preset, ok := discover.PresetFor(setup.Email); ok && preset.PasswordHint != "" {
		if ac.Runtime != nil {
			_ = ac.Runtime.WriteNotice(preset.PasswordHint)
		}
		if strings.Contains(preset.PasswordHint, "app password") || strings.Contains(preset.PasswordHint, "app-specific password") {
			label = "App password for " + setup.Email + ":"
		}
	}
	if ac.Runtime == nil {
		return nil, provider.ErrProviderRuntimeDenied
	}
	if value, ok, err := ac.Runtime.LookupSecretEnv(envPassword); err != nil {
		return nil, err
	} else if ok {
		if err := ac.Runtime.RegisterSecret(value); err != nil {
			return nil, err
		}
		cred, err := newPasswordCredential(ac.Provider, ac.Account, setup.Email, []byte(value))
		if err != nil {
			return nil, rungrad.NewError(rungrad.ExitUsage, "maildo cannot use the password in "+envPassword+"; it must be one line of 1 to 255 bytes")
		}
		return cred, nil
	}
	if ac.Secrets == nil {
		// No terminal to ask on, for example in a script or an agent's shell.
		return nil, rungrad.NewError(rungrad.ExitUsage, "maildo needs the password for "+setup.Email+", but there is no terminal to ask for it. Run maildo login in a terminal, or set "+envPassword+" for this command")
	}
	var cred *passwordCredential
	err := ac.Secrets.WithSecret(ctx, label, func(secret []byte) error {
		var err error
		cred, err = newPasswordCredential(ac.Provider, ac.Account, setup.Email, secret)
		if err != nil {
			return rungrad.NewError(rungrad.ExitUsage, "maildo cannot use that password; it must be one line of 1 to 255 bytes")
		}
		if ac.Runtime != nil {
			return ac.Runtime.RegisterSecret(cred.passwordString())
		}
		return nil
	})
	if err != nil {
		if cred != nil {
			cred.Destroy()
		}
		return nil, err
	}
	return cred, nil
}

func (p *Provider) LocalHealth(ctx context.Context, ac provider.StoredAuthContext) (provider.LocalStatus, error) {
	var status provider.LocalStatus
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cred, ok := lease.Current().(*passwordCredential)
		if !ok || cred == nil || cred.Destroyed() {
			status = provider.LocalRepairRequired
			return nil
		}
		codec := NewCredentialCodec(ac.Provider)
		s, err := codec.LocalHealth(ac.Setup, credentialBinding(ac.Provider, ac.Account), cred)
		if err != nil {
			status = provider.LocalRepairRequired
			return nil
		}
		status = s
		return nil
	})
	if err != nil {
		return provider.LocalRepairRequired, nil
	}
	return status, nil
}

func (p *Provider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	setup, err := mailServerSetup(ac.Setup, ac.Provider)
	if err != nil {
		return provider.RefreshResult{}, err
	}
	var cred *passwordCredential
	err = ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*passwordCredential)
		if !ok || cache == nil || cache.Destroyed() || !validCredential(cache, ac.Provider, ac.Account, setup.Email) {
			return storedCredentialRejected(ac.Account, ac.Provider, errs.ProviderProtocolIMAP)
		}
		var err error
		cred, err = cache.clone()
		return err
	})
	if err != nil {
		return provider.RefreshResult{}, err
	}
	defer cred.Destroy()
	if err := p.verifyBoth(ctx, ac.Account, setup, cred, provider.InteractiveRepair, false); err != nil {
		return provider.RefreshResult{}, err
	}
	return provider.RefreshResult{RefreshChecked: true, CredentialChanged: false, RotatedRefreshToken: false}, nil
}

func (p *Provider) Logout(context.Context, provider.StoredAuthContext) error { return nil }

func (p *Provider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	setup, err := mailServerSetup(ac.Setup, ac.Provider)
	if err != nil {
		return nil, err
	}
	var copied *passwordCredential
	err = ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		cache, ok := lease.Current().(*passwordCredential)
		if !ok || cache == nil || cache.Destroyed() || !validCredential(cache, ac.Provider, ac.Account, setup.Email) {
			return storedCredentialRejected(ac.Account, ac.Provider, errs.ProviderProtocolIMAP)
		}
		var err error
		copied, err = cache.clone()
		return err
	})
	if err != nil {
		return nil, err
	}
	client := &accountClient{
		provider:   ac.Provider,
		account:    ac.Account,
		senderName: ac.SenderName,
		setup:      setup,
		password:   copied.password,
		opts:       p.opts,
		done:       make(chan struct{}),
	}
	copied.password = nil
	copied.Destroy()
	return client, nil
}

type accountClient struct {
	provider   string
	account    string
	senderName string
	setup      maildoconfig.MailServerSetup
	password   []byte
	opts       Options

	mu        sync.Mutex
	destroyed bool
	wg        sync.WaitGroup
	done      chan struct{}
	closed    atomic.Bool

	// sessionMu serializes IMAP work. One connection has one selected
	// mailbox, so two operations must never interleave on it.
	sessionMu sync.Mutex
	session   *imapSession

	// partialFetchBroken is set once the server has answered a partial
	// FETCH in a form the IMAP library cannot read.
	partialFetchBroken atomic.Bool
}

func (c *accountClient) ProviderName() string { return c.provider }
func (c *accountClient) AccountName() string  { return c.account }

func (c *accountClient) Destroy() {
	if c == nil || c.closed.Swap(true) {
		return
	}
	c.mu.Lock()
	c.destroyed = true
	close(c.done)
	c.mu.Unlock()
	c.wg.Wait()
	c.sessionMu.Lock()
	if c.session != nil {
		c.session.logout()
		c.session = nil
	}
	c.sessionMu.Unlock()
	zeroBytes(c.password)
	c.password = nil
}

func (c *accountClient) begin() error {
	if c == nil {
		return errs.NewProviderClientClosed("", errs.ProviderIMAP)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return errs.NewProviderClientClosed(c.account, errs.ProviderName(c.provider))
	}
	c.wg.Add(1)
	return nil
}

func (c *accountClient) end() {
	if c != nil {
		c.wg.Done()
	}
}

func (c *accountClient) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	opCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-opCtx.Done():
		}
	}()
	return opCtx, cancel
}

// withIMAPSession runs fn on the account's IMAP connection. The first call
// connects and logs in; later calls during the same command reuse that
// connection, so a command logs in once. Every operation selects the mailbox it
// needs itself, because the previous operation may have left another one
// selected.
func (c *accountClient) withIMAPSession(ctx context.Context, fn func(*imapSession) error) error {
	if fn == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.session == nil {
		session, err := c.openIMAPSession(opCtx)
		if err != nil {
			return err
		}
		c.session = session
	}
	session := c.session
	session.forgetSelected()
	// If the caller gives up (Ctrl-C) while fn runs, close the connection so a
	// blocked read returns. The session cannot be reused after that.
	stop := context.AfterFunc(opCtx, session.abort)
	defer func() {
		if recovered := recover(); recovered != nil {
			stop()
			session.logout()
			c.session = nil
			panic(recovered)
		}
	}()
	err := fn(session)
	if !stop() || session.isClosed() || connectionBroken(err) {
		session.abort()
		c.session = nil
	}
	return err
}

func (c *accountClient) withSMTPSession(ctx context.Context, fn func(*smtpSession) (smtpSessionOutcome, error)) error {
	if fn == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	session, err := c.openSMTPSession(opCtx)
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = session.close(opCtx, 0)
			panic(recovered)
		}
	}()
	outcome, err := fn(session)
	cleanupErr := session.close(opCtx, outcome)
	if err != nil {
		return err
	}
	switch outcome {
	case smtpSessionOutcomeVerification:
		return cleanupErr
	case smtpSessionOutcomeAcceptedSubmission:
		return nil
	default:
		return errs.NewSharedProviderContractInvalid()
	}
}

type imapSession struct {
	account  string
	provider string
	client   *imapclient.Client
	conn     net.Conn
	// rawConn is the same connection as conn but never cleared, so abort can
	// close it without taking mu.
	rawConn     net.Conn
	aborted     atomic.Bool
	stop        func()
	limiter     *imapLimitConn
	mu          sync.Mutex
	closed      bool
	activeKnown bool
	activeRev   imapActiveRevision
	activeCaps  goimap.CapSet

	// The mailbox this connection has selected, so reads do not send the
	// same EXAMINE twice. It is cleared whenever a command fails or another
	// SELECT runs.
	selected selectedMailbox
}

func (s *imapSession) withCommand(ctx context.Context, fn func(*imapclient.Client) error) error {
	if fn == nil || s == nil || s.client == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	if err := phaseCtx.Err(); err != nil {
		return err
	}
	if err := fn(s.client); err != nil {
		return mapKnownIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		return mapContextPhase(s.account, s.provider, errs.ProviderProtocolIMAP, err)
	}
	return nil
}

func (s *imapSession) awaitCurrentCapabilities(ctx context.Context) (goimap.CapSet, error) {
	if s == nil || s.client == nil {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	caps := s.client.Caps()
	if err := phaseCtx.Err(); err != nil {
		return nil, mapIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	if caps == nil {
		return nil, errs.NewProviderDataFailure(mustFailureContext(s.account, s.provider, errs.ProviderProtocolIMAP), errs.ProviderDataReasonInvalidProtocol)
	}
	return caps.Copy(), nil
}

func (s *imapSession) withAttachmentFetch(ctx context.Context, uid goimap.UID, part []uint32, fn func(*imapclient.Client) error) error {
	if s == nil || s.client == nil || fn == nil || uid == 0 || len(part) == 0 {
		return errs.NewSharedProviderContractInvalid()
	}
	for _, n := range part {
		if n == 0 {
			return errs.NewSharedProviderContractInvalid()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, 2*time.Minute)
	defer cancel()
	if err := phaseCtx.Err(); err != nil {
		return err
	}
	restoreLimit := func() {}
	if s.limiter != nil {
		restoreLimit = s.limiter.useLiteralLimit(limits.IMAPMaxEncodedAttachmentBytes)
	}
	defer restoreLimit()
	if err := fn(s.client); err != nil {
		return mapKnownIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		return mapContextPhase(s.account, s.provider, errs.ProviderProtocolIMAP, err)
	}
	return nil
}

type smtpSessionOutcome uint8

const (
	smtpSessionOutcomeVerification smtpSessionOutcome = iota + 1
	smtpSessionOutcomeAcceptedSubmission
)

type smtpCleanupDisposition uint8

const (
	smtpCleanupDispositionGracefulQuit smtpCleanupDisposition = iota + 1
	smtpCleanupDispositionCloseOnly
)

type smtpSession struct {
	account  string
	provider string
	client   *smtpclient.Client
	conn     net.Conn
	stop     func()
	cleanup  smtpCleanupDisposition
	mu       sync.Mutex
	closed   bool
}

func (s *smtpSession) withCommand(ctx context.Context, fn func(*smtpclient.Client) error) error {
	if s == nil || s.client == nil || fn == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	if err := phaseCtx.Err(); err != nil {
		return err
	}
	if err := fn(s.client); err != nil {
		return mapKnownSMTPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		return mapContextPhase(s.account, s.provider, errs.ProviderProtocolSMTP, err)
	}
	return nil
}

func (s *smtpSession) armCloseOnlyCleanupBeforeRSET() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cleanup = smtpCleanupDispositionCloseOnly
	}
}

func (c *accountClient) openIMAPSession(ctx context.Context) (*imapSession, error) {
	p := &Provider{name: c.provider, opts: c.opts}
	return p.openIMAPSession(ctx, serverLogin{account: c.account, setup: c.setup, password: c.password, op: provider.InteractiveRepair})
}

// serverLogin carries what one IMAP or SMTP login needs. op and fresh decide
// how a rejected password is reported: a password the user just typed is
// "rejected", a stored one "needs repair".
type serverLogin struct {
	account  string
	setup    maildoconfig.MailServerSetup
	password []byte
	op       provider.InteractiveAuthOperation
	fresh    bool
}

// openIMAPSession connects, starts TLS, logs in and checks that the server
// speaks IMAP4rev1 or IMAP4rev2.
func (p *Provider) openIMAPSession(ctx context.Context, login serverLogin) (*imapSession, error) {
	endpoint := login.setup.IMAP
	tlsConfig, err := p.tlsConfig(endpoint.Host, login.setup.CAFile)
	if err != nil {
		return nil, err
	}
	conn, err := p.dial(ctx, endpoint)
	if err != nil {
		return nil, withEndpoint(mapDialError(login.account, p.name, errs.ProviderProtocolIMAP, err), endpoint)
	}
	// Until the session is ready, a cancelled context closes the connection so
	// a blocked handshake or read returns at once.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	session, err := p.startIMAP(ctx, conn, tlsConfig, login)
	if !stopWatch() {
		if session != nil {
			session.abort()
		}
		_ = conn.Close()
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) {
			return nil, ctxErr
		}
		if err == nil {
			err = providerConnection(login.account, p.name, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonTimeout)
		}
		return nil, withEndpoint(err, endpoint)
	}
	if err != nil {
		_ = conn.Close()
		return nil, withEndpoint(err, endpoint)
	}
	return session, nil
}

func (p *Provider) startIMAP(ctx context.Context, conn net.Conn, tlsConfig *tls.Config, login serverLogin) (*imapSession, error) {
	account := login.account
	reader := bufio.NewReader(conn)
	syntheticGreeting := false
	if login.setup.IMAP.TLS == maildoconfig.TLSImplicit {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := handshake(ctx, tlsConn); err != nil {
			return nil, mapTLSFailure(account, p.name, errs.ProviderProtocolIMAP, err)
		}
		conn = tlsConn
	} else {
		// STARTTLS: read the plain greeting, ask for TLS, then hand the
		// encrypted connection to the IMAP client with a stand-in greeting.
		phaseCtx, cancel := withDeadline(ctx, conn, greetingTimeout)
		greeting, err := imapReadLine(reader)
		if err != nil {
			mapped := mapProtocolRead(account, p.name, errs.ProviderProtocolIMAP, phaseCtx, err)
			cancel()
			return nil, greetingFailure(mapped)
		}
		cancel()
		caps, status, err := parseIMAPGreeting(account, p.name, greeting)
		if err != nil {
			return nil, err
		}
		if status == "PREAUTH" {
			return nil, unsupportedAuth(account, p.name, errs.ProviderProtocolIMAP)
		}
		if len(caps) == 0 {
			caps, err = p.imapCapability(ctx, conn, reader, account, "MDO0001")
			if err != nil {
				return nil, err
			}
		}
		if !hasCapability(caps, "STARTTLS") {
			return nil, providerConnection(account, p.name, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonTLSRequired)
		}
		if err := p.imapTaggedCommand(ctx, conn, reader, account, "MDO0002", "STARTTLS", nil); err != nil {
			return nil, err
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := handshake(ctx, tlsConn); err != nil {
			return nil, mapTLSFailure(account, p.name, errs.ProviderProtocolIMAP, err)
		}
		conn = tlsConn
		syntheticGreeting = true
	}

	limitedConn := newIMAPLimitConn(conn)
	clientConn := net.Conn(limitedConn)
	if syntheticGreeting {
		clientConn = &prefaceConn{Conn: limitedConn, prefix: strings.NewReader("* OK maildo STARTTLS established\r\n")}
	}
	_ = clientConn.SetDeadline(time.Now().Add(greetingTimeout + commandTimeout))
	client := imapclient.New(clientConn, &imapclient.Options{WordDecoder: wordDecoder()})
	if err := client.WaitGreeting(); err != nil {
		_ = client.Close()
		return nil, greetingFailure(mapIMAPDependencyFailure(account, p.name, ctx, err))
	}
	caps := client.Caps()
	if caps == nil {
		_ = client.Close()
		return nil, providerData(account, p.name, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	username := login.setup.Username
	switch {
	case caps.Has(goimap.AuthCap("PLAIN")):
		if err := client.Authenticate(saslPlain(username, login.password)); err != nil {
			_ = client.Close()
			return nil, withServerText(mapIMAPDependencyAuthFailure(account, p.name, ctx, err, login.op, login.fresh), err)
		}
	case !caps.Has(goimap.CapLoginDisabled):
		if err := client.Login(username, string(login.password)).Wait(); err != nil {
			_ = client.Close()
			return nil, withServerText(mapIMAPDependencyAuthFailure(account, p.name, ctx, err, login.op, login.fresh), err)
		}
	default:
		_ = client.Close()
		return nil, unsupportedAuth(account, p.name, errs.ProviderProtocolIMAP)
	}
	postCaps := client.Caps()
	if postCaps == nil {
		_ = client.Close()
		return nil, providerData(account, p.name, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	if !postCaps.Has(goimap.CapIMAP4rev1) && !postCaps.Has(goimap.CapIMAP4rev2) {
		_ = client.Close()
		return nil, unsupportedIMAPVersion(account, p.name)
	}
	if postCaps.Has(goimap.CapIMAP4rev1) && !postCaps.Has(goimap.CapIMAP4rev2) && postCaps.Has(goimap.CapUTF8Accept) && postCaps.Has(goimap.CapEnable) {
		if _, err := client.Enable(goimap.CapUTF8Accept).Wait(); err != nil {
			_ = client.Close()
			return nil, mapIMAPDependencyFailure(account, p.name, ctx, err)
		}
	}
	// Per-command deadlines take over from here.
	_ = clientConn.SetDeadline(time.Time{})
	return &imapSession{account: account, provider: p.name, client: client, conn: clientConn, rawConn: clientConn, limiter: limitedConn}, nil
}

// logout ends the session politely. Errors are ignored: the command's work
// is already done, and some servers close TLS abruptly after LOGOUT.
func (s *imapSession) logout() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	client, conn := s.client, s.conn
	s.client, s.conn = nil, nil
	s.mu.Unlock()
	if client == nil {
		return
	}
	if !s.aborted.Load() && conn != nil {
		_ = conn.SetDeadline(time.Now().Add(logoutTimeout))
		_ = client.Logout().Wait()
	}
	_ = client.Close()
}

// abort drops the connection without a goodbye. It takes no lock, so it can
// run from another goroutine while a command is blocked reading from the
// server.
func (s *imapSession) abort() {
	if s == nil || s.aborted.Swap(true) {
		return
	}
	if s.rawConn != nil {
		_ = s.rawConn.Close()
	}
}

func (s *imapSession) isClosed() bool {
	if s == nil || s.aborted.Load() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return true
	}
	select {
	case <-s.client.Closed():
		return true
	default:
		return false
	}
}

func (c *accountClient) openSMTPSession(ctx context.Context) (*smtpSession, error) {
	p := &Provider{name: c.provider, opts: c.opts}
	return p.openSMTPSession(ctx, serverLogin{account: c.account, setup: c.setup, password: c.password, op: provider.InteractiveRepair})
}

// openSMTPSession connects to the submission server, starts TLS and logs in.
// The connection lives for one send, so it closes when ctx ends.
func (p *Provider) openSMTPSession(ctx context.Context, login serverLogin) (*smtpSession, error) {
	account := login.account
	endpoint := login.setup.SMTP
	tlsConfig, err := p.tlsConfig(endpoint.Host, login.setup.CAFile)
	if err != nil {
		return nil, err
	}
	conn, err := p.dial(ctx, endpoint)
	if err != nil {
		return nil, withEndpoint(mapDialError(account, p.name, errs.ProviderProtocolSMTP, err), endpoint)
	}
	stop := startCancelCloser(ctx, conn)
	keep := false
	defer func() {
		if !keep {
			stop()
			_ = conn.Close()
		}
	}()
	fail := func(err error) (*smtpSession, error) {
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) {
			return nil, ctxErr
		}
		return nil, withEndpoint(err, endpoint)
	}

	syntheticGreeting := false
	if endpoint.TLS == maildoconfig.TLSImplicit {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := handshake(ctx, tlsConn); err != nil {
			return fail(mapTLSFailure(account, p.name, errs.ProviderProtocolSMTP, err))
		}
		conn = tlsConn
	} else {
		reader := bufio.NewReader(conn)
		greeting, err := smtpReadReply(ctx, conn, reader, greetingTimeout)
		if err != nil {
			return fail(greetingFailure(mapSMTPReply(account, p.name, ctx, err, true)))
		}
		if greeting.Code != 220 {
			return fail(providerConnection(account, p.name, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonProtocolGreeting))
		}
		extensions, err := p.smtpEHLO(ctx, conn, reader, account)
		if err != nil {
			return fail(err)
		}
		if _, ok := extensions["STARTTLS"]; !ok {
			return fail(providerConnection(account, p.name, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonTLSRequired))
		}
		reply, err := smtpCommand(ctx, conn, reader, "STARTTLS", commandTimeout)
		if err != nil {
			return fail(mapSMTPReply(account, p.name, ctx, err, false))
		}
		if reply.Code != 220 {
			return fail(providerConnection(account, p.name, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonTLSRequired))
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := handshake(ctx, tlsConn); err != nil {
			return fail(mapTLSFailure(account, p.name, errs.ProviderProtocolSMTP, err))
		}
		conn = tlsConn
		syntheticGreeting = true
	}

	limitedConn := &smtpReplyLimitConn{Conn: conn}
	clientConn := net.Conn(limitedConn)
	if syntheticGreeting {
		clientConn = &prefaceConn{Conn: limitedConn, prefix: strings.NewReader("220 maildo STARTTLS established\r\n")}
	}
	client := smtpclient.NewClient(clientConn)
	client.CommandTimeout = commandTimeout
	client.SubmissionTimeout = 2 * time.Minute
	if err := client.Hello("localhost"); err != nil {
		_ = client.Close()
		return fail(mapSMTPDependencyFailure(account, p.name, ctx, err))
	}
	username := login.setup.Username
	var auth sasl.Client
	switch {
	case client.SupportsAuth("PLAIN"):
		auth = saslPlain(username, login.password)
	case client.SupportsAuth("LOGIN"):
		auth = sasl.NewLoginClient(username, string(login.password))
	default:
		_ = client.Close()
		return fail(unsupportedAuth(account, p.name, errs.ProviderProtocolSMTP))
	}
	if err := client.Auth(auth); err != nil {
		_ = client.Close()
		return fail(withServerText(mapSMTPDependencyAuthFailure(account, p.name, ctx, err, login.op, login.fresh), err))
	}
	keep = true
	return &smtpSession{account: account, provider: p.name, client: client, conn: clientConn, stop: stop, cleanup: smtpCleanupDispositionGracefulQuit}, nil
}

func (s *smtpSession) close(ctx context.Context, outcome smtpSessionOutcome) error {
	if s == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	client := s.client
	conn := s.conn
	stop := s.stop
	cleanup := s.cleanup
	s.client = nil
	s.conn = nil
	s.stop = nil
	s.mu.Unlock()

	if stop != nil {
		defer stop()
	}
	if client == nil {
		return nil
	}
	if cleanup == smtpCleanupDispositionCloseOnly {
		if err := client.Close(); err != nil && outcome != smtpSessionOutcomeAcceptedSubmission {
			return mapSMTPDependencyFailure(s.account, s.provider, ctx, err)
		}
		return nil
	}
	if cleanup != smtpCleanupDispositionGracefulQuit {
		_ = client.Close()
		return errs.NewSharedProviderContractInvalid()
	}
	if conn != nil {
		phaseCtx, cancel := withDeadline(ctx, conn, commandTimeout)
		defer cancel()
		if err := client.Quit(); err != nil {
			_ = client.Close()
			if outcome == smtpSessionOutcomeAcceptedSubmission {
				return nil
			}
			return mapSMTPDependencyFailure(s.account, s.provider, phaseCtx, err)
		}
		return nil
	}
	if err := client.Quit(); err != nil {
		_ = client.Close()
		if outcome == smtpSessionOutcomeAcceptedSubmission {
			return nil
		}
		return mapSMTPDependencyFailure(s.account, s.provider, ctx, err)
	}
	return nil
}

// verifyBoth logs in to IMAP and SMTP once each, so a wrong password or
// server setting shows up during login instead of on the first command.
func (p *Provider) verifyBoth(ctx context.Context, account string, setup maildoconfig.MailServerSetup, cred *passwordCredential, op provider.InteractiveAuthOperation, fresh bool) error {
	login := serverLogin{account: account, setup: setup, password: cred.password, op: op, fresh: fresh}
	session, err := p.openIMAPSession(ctx, login)
	if err != nil {
		return err
	}
	session.logout()
	smtp, err := p.openSMTPSession(ctx, login)
	if err != nil {
		return err
	}
	// The login worked; a failed QUIT afterwards changes nothing.
	_ = smtp.close(ctx, smtpSessionOutcomeAcceptedSubmission)
	return nil
}

func mailServerSetup(setup maildoconfig.ProviderSetup, providerName string) (maildoconfig.MailServerSetup, error) {
	if setup.MailServer == nil {
		return maildoconfig.MailServerSetup{}, provider.ErrCredentialCapabilityClosed
	}
	if err := setup.ValidateFor(providerName); err != nil {
		return maildoconfig.MailServerSetup{}, err
	}
	return *setup.MailServer, nil
}

func credentialBinding(providerName, account string) auth.CredentialBinding {
	return auth.CredentialBinding{Provider: providerName, Account: account}
}

func (p *Provider) dial(ctx context.Context, endpoint maildoconfig.ServerEndpoint) (net.Conn, error) {
	address := net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port)))
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	dial := p.opts.DialContext
	if dial == nil {
		d := net.Dialer{}
		dial = d.DialContext
	}
	conn, err := dial(dctx, "tcp", address)
	if err != nil {
		if dctx.Err() != nil {
			return nil, dctx.Err()
		}
		return nil, err
	}
	return conn, nil
}

// tlsConfig returns the TLS settings for one server. A CA file adds extra
// trusted roots for self-hosted servers; the system roots stay trusted.
func (p *Provider) tlsConfig(serverName, caFile string) (*tls.Config, error) {
	if p.opts.TLSConfig != nil {
		if cfg := p.opts.TLSConfig(serverName); cfg != nil {
			return cfg.Clone(), nil
		}
	}
	cfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errs.NewWithDetails(errs.CodeInvalidConfig, fmt.Sprintf("cannot read the CA file %s", caFile), map[string]any{"ca_file": caFile})
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errs.NewWithDetails(errs.CodeInvalidConfig, fmt.Sprintf("the CA file %s contains no PEM certificates", caFile), map[string]any{"ca_file": caFile})
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// handshake runs the TLS handshake with its own time limit.
func handshake(ctx context.Context, conn *tls.Conn) error {
	hctx, cancel := context.WithTimeout(ctx, tlsTimeout)
	defer cancel()
	_ = conn.SetDeadline(time.Now().Add(tlsTimeout))
	err := conn.HandshakeContext(hctx)
	_ = conn.SetDeadline(time.Time{})
	return err
}

// wordDecoder decodes RFC 2047 header words in any charset Go knows. Unknown
// charsets fall back to the raw text instead of failing the whole response.
func wordDecoder() *mime.WordDecoder {
	return &mime.WordDecoder{CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
		r, err := charset.NewReaderLabel(label, input)
		if err != nil {
			return input, nil
		}
		return r, nil
	}}
}

func withDeadline(ctx context.Context, conn net.Conn, d time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(d)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if conn != nil {
		_ = conn.SetDeadline(deadline)
	}
	return context.WithDeadline(ctx, deadline)
}

func startCancelCloser(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func failureContext(account, providerName string, protocol errs.ProviderProtocol) (errs.ProviderFailureContext, error) {
	operation := errs.ProviderOperationIMAPAuth
	if protocol == errs.ProviderProtocolSMTP {
		operation = errs.ProviderOperationSMTPAuth
	}
	return errs.NewProviderFailureContext(account, errs.ProviderName(providerName), protocol, operation)
}

func mustFailureContext(account, providerName string, protocol errs.ProviderProtocol) errs.ProviderFailureContext {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return nil
	}
	return ctx
}

func storedCredentialRejected(account, providerName string, protocol errs.ProviderProtocol) error {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return err
	}
	return errs.NewStoredCredentialRejected(ctx)
}

func credentialRejected(account, providerName string, protocol errs.ProviderProtocol, op provider.InteractiveAuthOperation, fresh bool) error {
	if fresh && op == provider.InteractiveLogin {
		return errs.NewPreCommitCredentialRejected(account, errs.ProviderName(providerName))
	}
	return storedCredentialRejected(account, providerName, protocol)
}

func providerConnection(account, providerName string, protocol errs.ProviderProtocol, reason errs.ProviderConnectionReason) error {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return err
	}
	return errs.NewProviderConnectionFailure(ctx, reason)
}

func providerForbidden(account, providerName string, protocol errs.ProviderProtocol, reason errs.ProviderForbiddenReason) error {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return err
	}
	return errs.NewProviderForbiddenFailure(ctx, reason)
}

func providerData(account, providerName string, protocol errs.ProviderProtocol, reason errs.ProviderDataReason) error {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return err
	}
	return errs.NewProviderDataFailure(ctx, reason)
}

func unsupportedAuth(account, providerName string, protocol errs.ProviderProtocol) error {
	ctx, err := failureContext(account, providerName, protocol)
	if err != nil {
		return err
	}
	return errs.NewUnsupportedAuthentication(ctx, errs.ProviderAuthRequirementPLAIN)
}

func unsupportedIMAPVersion(account, providerName string) error {
	ctx, err := failureContext(account, providerName, errs.ProviderProtocolIMAP)
	if err != nil {
		return err
	}
	return errs.NewProviderCapabilityUnavailable(ctx, errs.ProviderCapabilityReasonIMAPVersion, errs.ProviderRequirementIMAP4rev1OrRev2)
}

func mapDialError(account, providerName string, protocol errs.ProviderProtocol, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonDNS)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonRefused)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	}
	return errs.WithDetails(providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonDial), map[string]any{"cause": shortCause(err)})
}

// withEndpoint adds the server address to a connection error so the message
// can say which server failed.
func withEndpoint(err error, endpoint maildoconfig.ServerEndpoint) error {
	return errs.WithDetails(err, map[string]any{
		"host":     endpoint.Host,
		"port":     int(endpoint.Port),
		"tls_mode": string(endpoint.TLS),
	})
}

// withServerText adds the server's own words (for example "Invalid
// credentials") to an error. Login rejections are most useful with them.
func withServerText(mapped, raw error) error {
	var imapErr *goimap.Error
	if errors.As(raw, &imapErr) && strings.TrimSpace(imapErr.Text) != "" {
		return errs.WithDetails(mapped, map[string]any{"server_message": shortText(imapErr.Text)})
	}
	var smtpErr *smtpclient.SMTPError
	if errors.As(raw, &smtpErr) && strings.TrimSpace(smtpErr.Message) != "" {
		return errs.WithDetails(mapped, map[string]any{"server_message": shortText(smtpErr.Message)})
	}
	return mapped
}

// greetingFailure marks a connection that failed before the server said
// hello. That usually means the TLS mode does not match the port.
func greetingFailure(err error) error {
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeProviderConnectionError {
		return err
	}
	if reason, _ := product.Details()["reason"].(string); reason == string(errs.ProviderConnectionReasonTimeout) {
		return errs.WithDetails(err, map[string]any{"phase": "greeting"})
	}
	return errs.WithDetails(err, map[string]any{"phase": "greeting"})
}

// connectionBroken reports errors after which the connection cannot be
// trusted for another command.
func connectionBroken(err error) bool {
	if err == nil {
		return false
	}
	var product *errs.Error
	if errors.As(err, &product) {
		switch product.Code() {
		case errs.CodeProviderConnectionError:
			return true
		case errs.CodeProviderDataError:
			reason, _ := product.Details()["reason"].(string)
			return reason == string(errs.ProviderDataReasonLineLimitExceeded) || reason == string(errs.ProviderDataReasonLiteralLimitExceeded)
		}
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF)
}

func shortCause(err error) string {
	if err == nil {
		return ""
	}
	return shortText(err.Error())
}

func shortText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

func mapContextPhase(account, providerName string, protocol errs.ProviderProtocol, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	}
	return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonConnectionLost)
}

func mapIMAPDependencyAuthFailure(account, providerName string, ctx context.Context, err error, op provider.InteractiveAuthOperation, fresh bool) error {
	if err == nil {
		return nil
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) {
		switch imapErr.Type {
		case goimap.StatusResponseTypeNo:
			switch strings.ToUpper(string(imapErr.Code)) {
			case "", string(goimap.ResponseCodeAuthenticationFailed), string(goimap.ResponseCodeExpired):
				return credentialRejected(account, providerName, errs.ProviderProtocolIMAP, op, fresh)
			case string(goimap.ResponseCodeAuthorizationFailed), string(goimap.ResponseCodeCannot), string(goimap.ResponseCodeContactAdmin), string(goimap.ResponseCodeNoPerm), string(goimap.ResponseCodePrivacyRequired):
				return providerForbidden(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderForbiddenReasonPolicy)
			case string(goimap.ResponseCodeUnavailable):
				return providerConnection(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonConnectionLost)
			default:
				return providerForbidden(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderForbiddenReasonUnknown)
			}
		case goimap.StatusResponseTypeBad:
			return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		case goimap.StatusResponseTypeBye:
			return providerConnection(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonConnectionLost)
		default:
			return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		}
	}
	return mapIMAPDependencyFailure(account, providerName, ctx, err)
}

func mapIMAPDependencyFailure(account, providerName string, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isFramingError(err, errLineLimit):
		return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonLineLimitExceeded)
	case isFramingError(err, errLiteralLimit):
		return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonLiteralLimitExceeded)
	case isFramingError(err, errInvalidProtocol):
		return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) {
		switch imapErr.Type {
		case goimap.StatusResponseTypeNo, goimap.StatusResponseTypeBad:
			return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		case goimap.StatusResponseTypeBye:
			return providerConnection(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonConnectionLost)
		default:
			return providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		}
	}
	return mapContextPhase(account, providerName, errs.ProviderProtocolIMAP, err)
}

func mapKnownIMAPDependencyFailure(account, providerName string, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isFramingError(err, errLineLimit), isFramingError(err, errLiteralLimit), isFramingError(err, errInvalidProtocol),
		errors.Is(err, io.EOF), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mapIMAPDependencyFailure(account, providerName, ctx, err)
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) {
		return mapIMAPDependencyFailure(account, providerName, ctx, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return mapIMAPDependencyFailure(account, providerName, ctx, err)
	}
	return err
}

func mapSMTPDependencyAuthFailure(account, providerName string, ctx context.Context, err error, op provider.InteractiveAuthOperation, fresh bool) error {
	if err == nil {
		return nil
	}
	var smtpErr *smtpclient.SMTPError
	if errors.As(err, &smtpErr) {
		return mapSMTPAuthCode(account, providerName, smtpErr.Code, op, fresh)
	}
	return mapSMTPDependencyFailure(account, providerName, ctx, err)
}

func mapSMTPDependencyFailure(account, providerName string, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isFramingError(err, errLineLimit):
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonLineLimitExceeded)
	case isFramingError(err, errInvalidProtocol):
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
	var smtpErr *smtpclient.SMTPError
	if errors.As(err, &smtpErr) {
		if smtpErr.Code == 421 {
			return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonConnectionLost)
		}
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
	return mapContextPhase(account, providerName, errs.ProviderProtocolSMTP, err)
}

func mapKnownSMTPDependencyFailure(account, providerName string, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isFramingError(err, errLineLimit), isFramingError(err, errInvalidProtocol),
		errors.Is(err, io.EOF), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mapSMTPDependencyFailure(account, providerName, ctx, err)
	}
	var smtpErr *smtpclient.SMTPError
	if errors.As(err, &smtpErr) {
		return mapSMTPDependencyFailure(account, providerName, ctx, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return mapSMTPDependencyFailure(account, providerName, ctx, err)
	}
	return err
}

func isFramingError(err, target error) bool {
	return errors.Is(err, target) || (err != nil && target != nil && strings.Contains(err.Error(), target.Error()))
}

type imapLimitConn struct {
	net.Conn
	mu               sync.Mutex
	pending          []byte
	line             []byte
	literalRemaining int64
	literalLimit     atomic.Int64
	failed           error
	observerMu       sync.Mutex
	observer         imapCommandObserver
}

type imapCommandObserver struct {
	active      bool
	expected    string
	tag         string
	writeLine   []byte
	result      imapMutationObservation
	removalSeen bool
}

type imapMutationObservation struct {
	Tag        string
	Command    string
	Status     string
	Completed  bool
	Tagged     []imapObservedResponseCode
	Untagged   []imapObservedResponseCode
	Expunged   bool
	CommandRaw bool
}

type imapObservedResponseCode struct {
	Name          string
	Raw           string
	Args          []string
	Tagged        bool
	Status        string
	Malformed     bool
	RemovalBefore bool
}

func newIMAPLimitConn(conn net.Conn) *imapLimitConn {
	c := &imapLimitConn{Conn: conn}
	c.literalLimit.Store(limits.IMAPMaxMetadataLiteralBytes)
	return c
}

func (c *imapLimitConn) useLiteralLimit(limit int64) func() {
	if c == nil {
		return func() {}
	}
	previous := c.literalLimit.Swap(limit)
	return func() { c.literalLimit.Store(previous) }
}

func (c *imapLimitConn) armMutationObserver(expected string) func() imapMutationObservation {
	if c == nil {
		return func() imapMutationObservation { return imapMutationObservation{} }
	}
	c.observerMu.Lock()
	c.observer = imapCommandObserver{
		active:   true,
		expected: strings.ToUpper(strings.TrimSpace(expected)),
	}
	c.observerMu.Unlock()
	return func() imapMutationObservation {
		c.observerMu.Lock()
		defer c.observerMu.Unlock()
		result := c.observer.result
		if result.Tag == "" {
			result.Tag = c.observer.tag
		}
		if result.Command == "" {
			result.Command = c.observer.expected
		}
		result.Tagged = append([]imapObservedResponseCode(nil), result.Tagged...)
		result.Untagged = append([]imapObservedResponseCode(nil), result.Untagged...)
		c.observer = imapCommandObserver{}
		return result
	}
}

func (c *imapLimitConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.observerMu.Lock()
		c.observeClientBytesLocked(p)
		c.observerMu.Unlock()
	}
	return c.Conn.Write(p)
}

func (c *imapLimitConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if c.failed != nil {
		return 0, c.failed
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.literalRemaining > 0 {
		max := int64(len(p))
		if max > c.literalRemaining {
			max = c.literalRemaining
		}
		n, err := c.Conn.Read(p[:int(max)])
		if n > 0 {
			c.literalRemaining -= int64(n)
		}
		return n, err
	}
	for {
		var b [1]byte
		m, err := c.Conn.Read(b[:])
		if m > 0 {
			c.line = append(c.line, b[0])
			if int64(len(c.line)) > limits.IMAPMaxLineBytes {
				c.failed = errLineLimit
				return 0, c.failed
			}
			if b[0] == '\n' {
				line := append([]byte(nil), c.line...)
				text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
				literalSize, ok, parseErr := imapLiteralMarker(text)
				if parseErr != nil {
					c.failed = parseErr
					c.line = c.line[:0]
					return 0, c.failed
				}
				if ok {
					if literalSize > c.literalLimit.Load() {
						c.failed = errLiteralLimit
						c.line = c.line[:0]
						return 0, c.failed
					}
					c.literalRemaining = literalSize
				}
				c.observeServerLineLocked(text)
				c.line = c.line[:0]
				c.pending = line
				n := copy(p, c.pending)
				c.pending = c.pending[n:]
				return n, nil
			}
		}
		if err != nil {
			return 0, err
		}
	}
}

func (c *imapLimitConn) observeClientBytesLocked(p []byte) {
	if !c.observer.active || c.observer.tag != "" {
		return
	}
	for _, b := range p {
		c.observer.writeLine = append(c.observer.writeLine, b)
		if b != '\n' {
			continue
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(c.observer.writeLine), "\n"), "\r")
		c.observer.writeLine = c.observer.writeLine[:0]
		fields := strings.Fields(text)
		if len(fields) < 2 {
			continue
		}
		command := strings.ToUpper(fields[1])
		if command == "UID" && len(fields) >= 3 {
			command += " " + strings.ToUpper(fields[2])
		}
		if command != c.observer.expected {
			continue
		}
		c.observer.tag = fields[0]
		c.observer.result.Tag = fields[0]
		c.observer.result.Command = command
		c.observer.result.CommandRaw = true
		return
	}
}

func (c *imapLimitConn) observeServerLineLocked(line string) {
	c.observerMu.Lock()
	defer c.observerMu.Unlock()
	if !c.observer.active || c.observer.tag == "" || c.observer.result.Completed {
		return
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return
	}
	if fields[0] == "*" {
		c.observeUntaggedServerLineLocked(line, fields)
		return
	}
	if fields[0] != c.observer.tag {
		return
	}
	status := strings.ToUpper(fields[1])
	switch status {
	case "OK", "NO", "BAD":
	default:
		return
	}
	c.observer.result.Status = status
	c.observer.result.Completed = true
	if code, ok := parseObservedResponseCode(line); ok {
		code.Tagged = true
		code.Status = status
		c.observer.result.Tagged = append(c.observer.result.Tagged, code)
	}
}

func (c *imapLimitConn) observeUntaggedServerLineLocked(line string, fields []string) {
	if len(fields) < 2 {
		return
	}
	if len(fields) >= 3 && isCanonicalUint32Token(fields[1]) && strings.EqualFold(fields[2], "EXPUNGE") {
		c.observer.removalSeen = true
		c.observer.result.Expunged = true
		return
	}
	if strings.EqualFold(fields[1], "ENABLED") {
		code := imapObservedResponseCode{Name: "ENABLED", Raw: strings.Join(fields[1:], " "), Args: append([]string(nil), fields[2:]...)}
		c.observer.result.Untagged = append(c.observer.result.Untagged, code)
		return
	}
	if strings.EqualFold(fields[1], "VANISHED") {
		c.observer.removalSeen = true
		c.observer.result.Expunged = true
		return
	}
	status := strings.ToUpper(fields[1])
	switch status {
	case "OK", "NO", "BAD":
	default:
		return
	}
	if code, ok := parseObservedResponseCode(line); ok {
		code.Tagged = false
		code.Status = status
		code.RemovalBefore = c.observer.removalSeen
		c.observer.result.Untagged = append(c.observer.result.Untagged, code)
	}
}

func parseObservedResponseCode(line string) (imapObservedResponseCode, bool) {
	start := strings.Index(line, "[")
	if start < 0 {
		return imapObservedResponseCode{}, false
	}
	end := strings.Index(line[start+1:], "]")
	if end < 0 {
		return imapObservedResponseCode{}, false
	}
	end += start + 1
	raw := line[start+1 : end]
	if raw == "" {
		return imapObservedResponseCode{Malformed: true}, true
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return imapObservedResponseCode{Malformed: true}, true
	}
	code := imapObservedResponseCode{Name: strings.ToUpper(fields[0]), Raw: raw}
	for _, arg := range fields[1:] {
		if strings.ContainsAny(arg, "\"{}[]") {
			code.Malformed = true
		}
		code.Args = append(code.Args, arg)
	}
	return code, true
}

func isCanonicalUint32Token(s string) bool {
	if s == "" || s == "0" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

type smtpReplyLimitConn struct {
	net.Conn
	mu      sync.Mutex
	pending []byte
	line    []byte
	total   int64
	failed  error
}

func (c *smtpReplyLimitConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if c.failed != nil {
		return 0, c.failed
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	for {
		var b [1]byte
		m, err := c.Conn.Read(b[:])
		if m > 0 {
			c.line = append(c.line, b[0])
			if len(c.line) > 2000 {
				c.failed = errLineLimit
				return 0, c.failed
			}
			if b[0] == '\n' {
				line := append([]byte(nil), c.line...)
				c.total += int64(len(line))
				if c.total > SMTPMaxReplyBytes {
					c.failed = errLineLimit
					c.line = c.line[:0]
					return 0, c.failed
				}
				text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
				if len(text) < 4 {
					c.failed = errInvalidProtocol
					c.line = c.line[:0]
					return 0, c.failed
				}
				code, convErr := strconv.Atoi(text[:3])
				if convErr != nil || code < 100 || code > 599 || (text[3] != '-' && text[3] != ' ') {
					c.failed = errInvalidProtocol
					c.line = c.line[:0]
					return 0, c.failed
				}
				if text[3] == ' ' {
					c.total = 0
				}
				c.line = c.line[:0]
				c.pending = line
				n := copy(p, c.pending)
				c.pending = c.pending[n:]
				return n, nil
			}
		}
		if err != nil {
			return 0, err
		}
	}
}

type prefaceConn struct {
	net.Conn
	prefix *strings.Reader
}

func (c *prefaceConn) Read(p []byte) (int, error) {
	if c.prefix != nil && c.prefix.Len() > 0 {
		return c.prefix.Read(p)
	}
	return c.Conn.Read(p)
}

func saslPlain(username string, password []byte) sasl.Client {
	return sasl.NewPlainClient("", username, string(password))
}
