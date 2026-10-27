package imap

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	smtpclient "github.com/emersion/go-smtp"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
	"github.com/vincentsch/maildo/internal/provider"
)

var _ provider.DirectSender = (*accountClient)(nil)

type DirectSendPreparer struct {
	providerName string
}

func NewDirectSendPreparer(providerName string) DirectSendPreparer {
	return DirectSendPreparer{providerName: providerName}
}

func (p DirectSendPreparer) ProviderName() string {
	if p.providerName == "" {
		return "imap"
	}
	return p.providerName
}

func (p DirectSendPreparer) PrepareDirectSend(ctx context.Context, identity provider.ComposeIdentity, msg mail.OutboundMessage) (mail.PreparedDirectSend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providerName := p.ProviderName()
	if (providerName != "imap" && providerName != "fastmail") ||
		identity.Provider != providerName ||
		identity.Account == "" ||
		strings.TrimSpace(identity.Email) == "" {
		return nil, errs.NewProviderClientContractInvalid()
	}
	if len(msg.To)+len(msg.CC)+len(msg.BCC) == 0 {
		return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the message has no recipients; add at least one To, Cc or Bcc address", map[string]any{"field": "recipients"})
	}
	smtpUTF8 := smtpNeedsUTF8(identity.Email, msg.To, msg.CC, msg.BCC, msg.ReplyTo)
	// The From address is always the account's. The name is the one given
	// with --from "Name <address>", or the account's sender name.
	from := mail.Recipient{Name: identity.DisplayName, Address: identity.Email}
	if msg.From != nil && strings.TrimSpace(msg.From.Name) != "" {
		from.Name = strings.TrimSpace(msg.From.Name)
	}
	encoded, err := encodeSMTPMessage(identity.Account, providerName, smtpMIMEMessage(from, msg), smtpUTF8)
	if err != nil {
		return nil, err
	}
	defer encoded.Destroy()
	return &preparedSMTPDirectSend{
		account:  identity.Account,
		provider: providerName,
		encoded:  append([]byte(nil), encoded.Bytes...),
		envelope: smtpEnvelopeRecipients(msg),
		bcc:      append([]mail.Recipient(nil), msg.BCC...),
		smtpUTF8: smtpUTF8,
	}, nil
}

type preparedSMTPDirectSend struct {
	mu        sync.Mutex
	account   string
	provider  string
	encoded   []byte
	envelope  []string
	bcc       []mail.Recipient
	smtpUTF8  bool
	consumed  bool
	destroyed bool
}

func (p *preparedSMTPDirectSend) Destroy() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed {
		return
	}
	p.destroyed = true
	zeroBytes(p.encoded)
	p.encoded = nil
	p.envelope = nil
}

func (p *preparedSMTPDirectSend) consume(providerName, account string) ([]byte, []string, []mail.Recipient, bool, error) {
	if p == nil {
		return nil, nil, nil, false, errs.NewProviderClientContractInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed || p.consumed || p.provider != providerName || p.account != account {
		return nil, nil, nil, false, errs.NewProviderClientContractInvalid()
	}
	p.consumed = true
	encoded := append([]byte(nil), p.encoded...)
	envelope := append([]string(nil), p.envelope...)
	bcc := p.bcc
	smtpUTF8 := p.smtpUTF8
	zeroBytes(p.encoded)
	p.encoded = nil
	p.envelope = nil
	p.bcc = nil
	return encoded, envelope, bcc, smtpUTF8, nil
}

func (c *accountClient) SendMessage(ctx context.Context, req mail.SendMessageRequest) (mail.DirectSendOutcome, error) {
	if !smtpOutboundMessageEmpty(req.Message) {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	prepared, ok := req.Prepared.(*preparedSMTPDirectSend)
	if !ok {
		return mail.DirectSendOutcome{}, errs.NewProviderClientContractInvalid()
	}
	encoded, envelope, bcc, smtpUTF8, err := prepared.consume(c.provider, c.account)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	defer zeroBytes(encoded)
	attempt, err := errs.NewProviderWriteAttempt(c.account, imapErrProvider(c.provider), errs.WriteSMTPDirectSend)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	outcome, enteredCallback, err := c.submitSMTPMessage(ctx, attempt, encoded, envelope, smtpUTF8)
	if err != nil {
		if !enteredCallback {
			err = errs.NewWriteNotApplied(attempt, err)
		}
		return mail.DirectSendOutcome{}, err
	}
	c.keepSentCopy(ctx, mimecompose.WithBcc(encoded, bcc), &outcome)
	return outcome, nil
}

func (c *accountClient) submitSMTPMessage(ctx context.Context, attempt errs.ProviderWriteAttempt, encoded []byte, envelope []string, smtpUTF8 bool) (mail.DirectSendOutcome, bool, error) {
	var outcome mail.DirectSendOutcome
	enteredCallback := false
	err := c.withSMTPSession(ctx, func(session *smtpSession) (smtpSessionOutcome, error) {
		enteredCallback = true
		err := session.withCommand(ctx, func(client *smtpclient.Client) error {
			if err := c.checkSMTPDirectSendCapabilities(client, attempt, len(encoded), smtpUTF8); err != nil {
				return err
			}
			if err := client.Mail(c.setup.Email, c.smtpMailOptions(client, len(encoded), smtpUTF8)); err != nil {
				return c.smtpMAILFailure(attempt, ctx, err)
			}
			for i, recipient := range envelope {
				if err := client.Rcpt(recipient, nil); err != nil {
					return c.smtpRCPTFailure(session, client, attempt, ctx, err, i+1, len(envelope))
				}
			}
			data, err := client.Data()
			if err != nil {
				return c.smtpDATACommandFailure(session, client, attempt, ctx, err)
			}
			if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
				session.cleanup = smtpCleanupDispositionCloseOnly
				_ = client.Close()
				return err
			}
			session.cleanup = smtpCleanupDispositionCloseOnly
			if n, err := data.Write(encoded); err != nil || n != len(encoded) {
				return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
			}
			if err := data.Close(); err != nil {
				accepted, finalErr := c.smtpFinalReplyFailure(attempt, err)
				if finalErr != nil {
					return finalErr
				}
				if !accepted {
					return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
				}
			}
			facts, err := errs.VerifiedWriteSuccessFacts(attempt)
			if err != nil {
				return err
			}
			meta, err := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataSMTPDirectSend, mail.SMTPDirectSendProviderMetadata{
				AcceptanceKnown: true,
				SMTPUTF8Used:    smtpUTF8,
			})
			if err != nil {
				return err
			}
			outcome = mail.DirectSendOutcome{WriteOutcomeFacts: facts, ProviderMeta: meta}
			return nil
		})
		if err != nil {
			return 0, err
		}
		return smtpSessionOutcomeAcceptedSubmission, nil
	})
	return outcome, enteredCallback, err
}

func (c *accountClient) checkSMTPDirectSendCapabilities(client *smtpclient.Client, attempt errs.ProviderWriteAttempt, encodedLen int, smtpUTF8 bool) error {
	if smtpUTF8 {
		if ok, _ := client.Extension("SMTPUTF8"); !ok {
			return smtpWriteNotApplied(c.account, c.provider, attempt, smtpUTF8Unavailable(c.account, c.provider))
		}
	}
	if ok, param := client.Extension("SIZE"); ok {
		limit, hasLimit, err := smtpAdvertisedSizeLimit(param)
		if err != nil {
			return smtpWriteNotApplied(c.account, c.provider, attempt, providerData(c.account, c.provider, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol))
		}
		if hasLimit && uint64(encodedLen) > limit {
			return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewMessageTooLarge(c.account, imapErrProvider(c.provider), errs.MessageSizeSMTPAdvertised, limit, uint64(encodedLen)))
		}
	}
	return nil
}

func (c *accountClient) smtpMailOptions(client *smtpclient.Client, encodedLen int, smtpUTF8 bool) *smtpclient.MailOptions {
	opts := &smtpclient.MailOptions{Size: int64(encodedLen), UTF8: smtpUTF8}
	if smtpUTF8 {
		opts.Body = smtpclient.Body8BitMIME
	} else if ok, _ := client.Extension("8BITMIME"); ok {
		opts.Body = smtpclient.Body7Bit
	}
	return opts
}

func smtpWriteNotApplied(account, providerName string, attempt errs.ProviderWriteAttempt, cause error) error {
	if cause == nil {
		cause = providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
	return errs.NewWriteNotApplied(attempt, cause)
}

func (c *accountClient) smtpMAILFailure(attempt errs.ProviderWriteAttempt, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var smtpErr *smtpclient.SMTPError
	if !errors.As(err, &smtpErr) {
		return smtpWriteNotApplied(c.account, c.provider, attempt, mapSMTPDependencyFailure(c.account, c.provider, ctx, err))
	}
	if class, ok := smtpNonRecipientReplyClass(smtpErr.Code); ok {
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyMAIL, class))
	}
	return smtpWriteNotApplied(c.account, c.provider, attempt, providerData(c.account, c.provider, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol))
}

func (c *accountClient) smtpRCPTFailure(session *smtpSession, client *smtpclient.Client, attempt errs.ProviderWriteAttempt, ctx context.Context, err error, rejectedIndex, totalCount int) error {
	if err == nil {
		return nil
	}
	var smtpErr *smtpclient.SMTPError
	if !errors.As(err, &smtpErr) {
		smtpAbortTransaction(session, client, 0)
		return smtpWriteNotApplied(c.account, c.provider, attempt, mapSMTPDependencyFailure(c.account, c.provider, ctx, err))
	}
	smtpAbortTransaction(session, client, smtpErr.Code)
	acceptedCount := rejectedIndex - 1
	switch {
	case smtpReplyCodeIn(smtpErr.Code, 450, 451, 452, 552):
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPRecipientTemporarilyRejected(c.account, imapErrProvider(c.provider), rejectedIndex, acceptedCount, totalCount))
	case smtpReplyCodeIn(smtpErr.Code, 550, 551, 553, 554, 556):
		marked := errs.NewPreTransmissionDefinitiveProviderRejectionError(c.account, imapErrProvider(c.provider), errs.WriteSMTPDirectSend, errs.NewSMTPRecipientRejected(c.account, imapErrProvider(c.provider), rejectedIndex, acceptedCount, totalCount))
		rejection, ok := errs.AsDefinitiveProviderRejection(marked)
		if !ok {
			return errs.NewSharedProviderContractInvalid()
		}
		return errs.NewWriteRejected(attempt, rejection)
	case smtpReplyCodeIn(smtpErr.Code, 455, 500, 501, 502, 503, 504, 555):
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyRCPT, errs.SMTPReplyInvalidProtocol))
	case smtpErr.Code == 530:
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyRCPT, errs.SMTPReplyProviderPolicy))
	case smtpErr.Code == 421 || smtpErr.Code/100 == 4:
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyRCPT, errs.SMTPReplyConnectionLost))
	case smtpErr.Code/100 == 5:
		return smtpWriteNotApplied(c.account, c.provider, attempt, errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyRCPT, errs.SMTPReplyUnknownForbidden))
	default:
		return smtpWriteNotApplied(c.account, c.provider, attempt, providerData(c.account, c.provider, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol))
	}
}

func (c *accountClient) smtpDATACommandFailure(session *smtpSession, client *smtpclient.Client, attempt errs.ProviderWriteAttempt, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var smtpErr *smtpclient.SMTPError
	if !errors.As(err, &smtpErr) {
		smtpAbortTransaction(session, client, 0)
		return smtpWriteNotApplied(c.account, c.provider, attempt, mapSMTPDependencyFailure(c.account, c.provider, ctx, err))
	}
	smtpAbortTransaction(session, client, smtpErr.Code)
	class, ok := smtpNonRecipientReplyClass(smtpErr.Code)
	if !ok {
		return smtpWriteNotApplied(c.account, c.provider, attempt, providerData(c.account, c.provider, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol))
	}
	marked := errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyDATACommand, class)
	rejection, ok := errs.AsDefinitiveProviderRejection(marked)
	if !ok {
		return errs.NewSharedProviderContractInvalid()
	}
	return errs.NewWriteRejected(attempt, rejection)
}

func (c *accountClient) smtpFinalReplyFailure(attempt errs.ProviderWriteAttempt, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var smtpErr *smtpclient.SMTPError
	if !errors.As(err, &smtpErr) {
		return false, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	if smtpErr.Code >= 200 && smtpErr.Code <= 259 {
		return true, nil
	}
	class, ok := smtpNonRecipientReplyClass(smtpErr.Code)
	if !ok {
		return false, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	marked := errs.NewSMTPClassifiedReply(c.account, imapErrProvider(c.provider), errs.SMTPReplyFinal, class)
	rejection, ok := errs.AsDefinitiveProviderRejection(marked)
	if !ok {
		return false, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	return false, errs.NewWriteRejected(attempt, rejection)
}

func smtpAbortTransaction(session *smtpSession, client *smtpclient.Client, code int) {
	if session != nil {
		session.cleanup = smtpCleanupDispositionCloseOnly
	}
	if client == nil {
		return
	}
	if code == 421 {
		_ = client.Close()
		return
	}
	_ = client.Reset()
}

func smtpNonRecipientReplyClass(code int) (errs.SMTPReplyClass, bool) {
	switch {
	case code == 421 || code/100 == 4:
		return errs.SMTPReplyConnectionLost, true
	case smtpReplyCodeIn(code, 530, 550, 551, 552, 553, 554):
		return errs.SMTPReplyProviderPolicy, true
	case smtpReplyCodeIn(code, 500, 501, 502, 503, 504):
		return errs.SMTPReplyInvalidProtocol, true
	case code/100 == 5:
		return errs.SMTPReplyUnknownForbidden, true
	default:
		return "", false
	}
}

func smtpReplyCodeIn(code int, values ...int) bool {
	for _, value := range values {
		if code == value {
			return true
		}
	}
	return false
}

func smtpAdvertisedSizeLimit(param string) (uint64, bool, error) {
	param = strings.TrimSpace(param)
	if param == "" {
		return 0, false, nil
	}
	if strings.ContainsAny(param, " \t") {
		return 0, false, strconv.ErrSyntax
	}
	limit, err := strconv.ParseUint(param, 10, 63)
	if err != nil {
		return 0, false, err
	}
	if limit == 0 {
		return 0, false, nil
	}
	return limit, true, nil
}

func smtpUTF8Unavailable(account, providerName string) error {
	ctx, err := errs.NewProviderFailureContext(account, imapErrProvider(providerName), errs.ProviderProtocolSMTP, errs.ProviderOperationSMTPDirectSend)
	if err != nil {
		return err
	}
	return errs.NewProviderCapabilityUnavailable(ctx, errs.ProviderCapabilityReasonSMTPUTF8Required, errs.ProviderRequirementSMTPUTF8)
}

func encodeSMTPMessage(account, providerName string, msg mimecompose.Message, smtpUTF8 bool) (*mimecompose.EncodedMessage, error) {
	doc, err := mimecompose.NewDocument(msg, mimecompose.DefaultLimits())
	if err != nil {
		return nil, smtpMIMEError(account, providerName, err)
	}
	defer doc.Destroy()
	mode := mimecompose.AddressHeaderASCII
	if smtpUTF8 {
		mode = mimecompose.AddressHeaderSMTPUTF8
	}
	encoded, err := doc.Encode(mimecompose.EncodeOptions{BccPolicy: mimecompose.BccHeaderPolicyOmit, AddressMode: mode})
	if err != nil {
		return nil, smtpMIMEError(account, providerName, err)
	}
	projection, err := mimecompose.ParseVerificationProjection(encoded.Bytes, smtpVerificationLimits())
	if err != nil {
		encoded.Destroy()
		return nil, smtpMIMEError(account, providerName, err)
	}
	projection.Destroy()
	return encoded, nil
}

func smtpMIMEMessage(from mail.Recipient, msg mail.OutboundMessage) mimecompose.Message {
	text, htmlBody := provider.MIMEBodies(msg.Body)
	return mimecompose.Message{
		From:        from,
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		ReplyTo:     msg.ReplyTo,
		Subject:     msg.Subject,
		TextBody:    []byte(text),
		HTMLBody:    []byte(htmlBody),
		Attachments: mimecompose.AttachmentParts(msg.Attachments),
	}
}

func smtpVerificationLimits() mimecompose.VerificationLimits {
	semantic := mimecompose.DefaultLimits()
	semantic.MaxEncodedBytes = mail.MaxOutboundEncodedMessageBytes
	return mimecompose.VerificationLimits{
		Semantic:                    semantic,
		MaxRawBytes:                 mail.MaxOutboundEncodedMessageBytes,
		MaxDiscardedRootHeaderBytes: 256 * 1024,
		MaxDiscardedRootHeaderCount: 64,
	}
}

func smtpMIMEError(account, providerName string, err error) error {
	if err == nil {
		return nil
	}
	if fact, ok := mimecompose.LimitViolationFromError(err); ok {
		switch fact.Kind {
		case mimecompose.LimitEncodedMessageBytes, mimecompose.LimitVerificationRawBytes:
			return errs.NewMessageTooLarge(account, imapErrProvider(providerName), errs.MessageSizeOutboundEncoded, fact.Limit, fact.ObservedAtLeast)
		case mimecompose.LimitAddresses:
			return errs.NewTooManyAddresses(fact.Limit)
		default:
			return errs.New(errs.CodeInvalidComposeInput, "maildo could not build this message, because it breaks a limit of the mail format, such as the length of a header or a line")
		}
	}
	switch {
	case errors.Is(err, mimecompose.ErrEncodedMessageTooLarge), errors.Is(err, mimecompose.ErrVerificationRawTooLarge):
		return errs.NewMessageTooLarge(account, imapErrProvider(providerName), errs.MessageSizeOutboundEncoded, uint64(mail.MaxOutboundEncodedMessageBytes), uint64(mail.MaxOutboundEncodedMessageBytes+1))
	case errors.Is(err, mimecompose.ErrInvalidSemanticInput), errors.Is(err, mimecompose.ErrInvalidOptions):
		return errs.New(errs.CodeInvalidComposeInput, "maildo could not build this message, because it breaks a limit of the mail format, such as the length of a header or a line")
	default:
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
}

func smtpEnvelopeRecipients(msg mail.OutboundMessage) []string {
	out := make([]string, 0, len(msg.To)+len(msg.CC)+len(msg.BCC))
	appendRecipients := func(in []mail.Recipient) {
		for _, r := range in {
			addr := strings.TrimSpace(r.Address)
			if addr != "" {
				out = append(out, addr)
			}
		}
	}
	appendRecipients(msg.To)
	appendRecipients(msg.CC)
	appendRecipients(msg.BCC)
	return out
}

func smtpNeedsUTF8(from string, groups ...[]mail.Recipient) bool {
	if smtpAddressLocalNeedsUTF8(from) {
		return true
	}
	for _, group := range groups {
		for _, r := range group {
			if smtpAddressLocalNeedsUTF8(r.Address) {
				return true
			}
		}
	}
	return false
}

func smtpAddressLocalNeedsUTF8(address string) bool {
	local, _, ok := strings.Cut(strings.TrimSpace(address), "@")
	return ok && strings.IndexFunc(local, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0
}

func smtpOutboundMessageEmpty(m mail.OutboundMessage) bool {
	return m.From == nil &&
		len(m.To) == 0 &&
		len(m.CC) == 0 &&
		len(m.BCC) == 0 &&
		len(m.ReplyTo) == 0 &&
		m.Subject == "" &&
		m.Body == (mail.OutboundBody{})
}
