package imap

import (
	"context"
	"errors"
	"io"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
)

// The selected mailbox.
//
// An IMAP connection has at most one selected mailbox, and UID SEARCH, UID
// FETCH and UID STORE all act on it. Message ids in maildo name a mailbox, its
// UIDVALIDITY and a UID, so reading a message always means: select that
// mailbox, check that UIDVALIDITY still matches the id, then fetch by UID.
//
// maildo reuses one connection for a whole command. Every operation therefore
// selects the mailbox it needs itself. Within one operation the selection is
// remembered so a loop over messages does not EXAMINE the same mailbox again.
// Between operations, and after any failed command, the remembered selection
// is dropped.

// selectedMailbox is the mailbox the connection has open right now.
type selectedMailbox struct {
	name     string
	readOnly bool
	status   imapMailboxStatus
}

func (s *imapSession) rememberSelected(name string, readOnly bool, status imapMailboxStatus) {
	s.mu.Lock()
	s.selected = selectedMailbox{name: name, readOnly: readOnly, status: status}
	s.mu.Unlock()
}

func (s *imapSession) forgetSelected() {
	s.mu.Lock()
	s.selected = selectedMailbox{}
	s.mu.Unlock()
}

func (s *imapSession) currentSelected() selectedMailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selected
}

// examine makes mailbox the selected mailbox for reading. It sends no
// EXAMINE when this operation already selected the same mailbox. A read-write
// SELECT left by an earlier step is fine for reading: maildo fetches with
// BODY.PEEK, which never marks mail as read.
func (c *accountClient) examine(ctx context.Context, session *imapSession, mailbox string, op errs.ProviderOperation) (imapMailboxStatus, error) {
	if current := session.currentSelected(); current.name == mailbox && current.status.known {
		return current.status, nil
	}
	return c.examineMailbox(ctx, session, mailbox, op)
}

func (c *accountClient) examineMailbox(ctx context.Context, session *imapSession, mailbox string, op errs.ProviderOperation) (imapMailboxStatus, error) {
	session.forgetSelected()
	var data *goimap.SelectData
	obs, err := session.withObservedReadCommand(ctx, op, "EXAMINE", func(dep *imapclient.Client) error {
		var err error
		data, err = dep.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	if err != nil {
		return imapMailboxStatus{}, err
	}
	status, err := c.mailboxStatusFromSelectData(data, obs, op)
	if err != nil {
		return imapMailboxStatus{}, err
	}
	session.rememberSelected(mailbox, true, status)
	return status, nil
}

// examineMailboxForMessage is examine for a mailbox named in a message id. A
// mailbox that no longer exists means the message is gone.
func (c *accountClient) examineMailboxForMessage(ctx context.Context, session *imapSession, mailbox string, selectorKind, selectorValue string, op errs.ProviderOperation) (imapMailboxStatus, error) {
	if current := session.currentSelected(); current.name == mailbox && current.status.known {
		return current.status, nil
	}
	session.forgetSelected()
	var data *goimap.SelectData
	obs, err := session.withObservedReadCommand(ctx, op, "EXAMINE", func(dep *imapclient.Client) error {
		var err error
		data, err = dep.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait()
		if isIMAPStatusNo(err) {
			return errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), selectorKind, selectorValue, errs.IMAPMessageNotFoundMissing)
		}
		return err
	})
	if err != nil {
		return imapMailboxStatus{}, err
	}
	status, err := c.mailboxStatusFromSelectData(data, obs, op)
	if err != nil {
		return imapMailboxStatus{}, err
	}
	session.rememberSelected(mailbox, true, status)
	return status, nil
}

func (c *accountClient) mailboxStatusFromSelectData(data *goimap.SelectData, obs imapMutationObservation, op errs.ProviderOperation) (imapMailboxStatus, error) {
	if data == nil || data.UIDValidity == 0 {
		return imapMailboxStatus{}, c.providerDataError(op, errs.ProviderDataReasonInvalidProtocol)
	}
	facts := selectedMailboxFacts(obs)
	return imapMailboxStatus{
		total:               data.NumMessages,
		uidValidity:         data.UIDValidity,
		uidNext:             uint32(data.UIDNext),
		known:               true,
		uidNotSticky:        facts.uidNotSticky,
		readWriteKnown:      facts.readWriteKnown,
		readWrite:           facts.readWrite,
		permanentFlagsKnown: facts.permanentFlagsKnown,
		permanentFlags:      append([]goimap.Flag(nil), facts.permanentFlags...),
		highestModSeqKnown:  facts.highestModSeqKnown,
		highestModSeq:       facts.highestModSeq,
		noModSeq:            facts.noModSeq,
	}, nil
}

// withAttachmentRead is withReadCommand for commands that return a large
// body: it allows bigger literals and more time.
func (s *imapSession) withAttachmentRead(ctx context.Context, operation errs.ProviderOperation, fn func(*imapclient.Client) error) error {
	if s == nil || s.limiter == nil {
		return s.withReadCommand(ctx, operation, fn)
	}
	restore := s.limiter.useLiteralLimit(limits.IMAPMaxEncodedAttachmentBytes)
	defer restore()
	return s.withReadCommand(ctx, operation, fn)
}

type codedProductError interface {
	Code() string
}

func errorCode(err error) string {
	var coded codedProductError
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

func isIMAPStatusNo(err error) bool {
	var imapErr *goimap.Error
	return errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeNo
}

// withReadCommand runs one read command with the command time limit and maps
// failures to maildo errors. Any failure forgets the selected mailbox, since
// the server may have left it.
func (s *imapSession) withReadCommand(ctx context.Context, operation errs.ProviderOperation, fn func(*imapclient.Client) error) error {
	if fn == nil || s == nil {
		return errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil || s.aborted.Load() {
		return providerReadConnection(s.account, s.provider, operation, errs.ProviderConnectionReasonConnectionLost)
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	if err := fn(s.client); err != nil {
		s.selected = selectedMailbox{}
		return mapKnownIMAPReadFailure(s.account, s.provider, operation, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		s.selected = selectedMailbox{}
		return mapIMAPReadContext(s.account, s.provider, operation, phaseCtx, err)
	}
	return nil
}

func mapKnownIMAPReadFailure(account, providerName string, operation errs.ProviderOperation, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isFramingError(err, errLineLimit), isFramingError(err, errLiteralLimit), isFramingError(err, errInvalidProtocol),
		errors.Is(err, io.EOF), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mapIMAPReadFailure(account, providerName, operation, ctx, err)
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) {
		return mapIMAPReadFailure(account, providerName, operation, ctx, err)
	}
	var netErr netError
	if errors.As(err, &netErr) {
		return mapIMAPReadFailure(account, providerName, operation, ctx, err)
	}
	var product *errs.Error
	if errors.As(err, &product) {
		return err
	}
	// Anything else is the IMAP library failing to parse what the server
	// sent. The connection is unusable after that.
	return errs.WithDetails(providerReadData(account, providerName, operation, errs.ProviderDataReasonInvalidProtocol), map[string]any{"cause": shortCause(err)})
}

type netError interface {
	error
	Timeout() bool
}

func mapIMAPReadFailure(account, providerName string, operation errs.ProviderOperation, ctx context.Context, err error) error {
	switch {
	case isFramingError(err, errLineLimit):
		return providerReadData(account, providerName, operation, errs.ProviderDataReasonLineLimitExceeded)
	case isFramingError(err, errLiteralLimit):
		return providerReadData(account, providerName, operation, errs.ProviderDataReasonLiteralLimitExceeded)
	case isFramingError(err, errInvalidProtocol):
		return providerReadData(account, providerName, operation, errs.ProviderDataReasonInvalidProtocol)
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) {
		var mapped error
		switch imapErr.Type {
		case goimap.StatusResponseTypeBye:
			mapped = providerReadConnection(account, providerName, operation, errs.ProviderConnectionReasonConnectionLost)
		default:
			mapped = providerReadData(account, providerName, operation, errs.ProviderDataReasonInvalidProtocol)
		}
		return withServerText(mapped, err)
	}
	return mapIMAPReadContext(account, providerName, operation, ctx, err)
}

func mapIMAPReadContext(account, providerName string, operation errs.ProviderOperation, ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return providerReadConnection(account, providerName, operation, errs.ProviderConnectionReasonTimeout)
	}
	var netErr netError
	if errors.As(err, &netErr) && netErr.Timeout() {
		return providerReadConnection(account, providerName, operation, errs.ProviderConnectionReasonTimeout)
	}
	return providerReadConnection(account, providerName, operation, errs.ProviderConnectionReasonConnectionLost)
}

func providerReadData(account, providerName string, operation errs.ProviderOperation, reason errs.ProviderDataReason) error {
	failure, err := errs.NewProviderFailureContext(account, imapErrProvider(providerName), errs.ProviderProtocolIMAP, operation)
	if err != nil {
		return err
	}
	return errs.NewProviderDataFailure(failure, reason)
}

func providerReadConnection(account, providerName string, operation errs.ProviderOperation, reason errs.ProviderConnectionReason) error {
	failure, err := errs.NewProviderFailureContext(account, imapErrProvider(providerName), errs.ProviderProtocolIMAP, operation)
	if err != nil {
		return err
	}
	return errs.NewProviderConnectionFailure(failure, reason)
}
