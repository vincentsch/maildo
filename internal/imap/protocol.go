package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/provider"
)

func (p *Provider) imapCapability(ctx context.Context, conn net.Conn, reader *bufio.Reader, account, tag string) (map[string]bool, error) {
	var caps map[string]bool
	err := p.imapTaggedCommand(ctx, conn, reader, account, tag, "CAPABILITY", func(line string) error {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "*" && strings.EqualFold(fields[1], "CAPABILITY") {
			caps = capabilityMap(fields[2:])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if caps == nil {
		return nil, providerData(account, p.name, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	return caps, nil
}

func (p *Provider) imapTaggedCommand(ctx context.Context, conn net.Conn, reader *bufio.Reader, account, tag, command string, observe func(string) error) error {
	phaseCtx, cancel := withDeadline(ctx, conn, commandTimeout)
	defer cancel()
	if _, err := io.WriteString(conn, tag+" "+command+"\r\n"); err != nil {
		return mapProtocolWrite(account, p.name, errs.ProviderProtocolIMAP, phaseCtx, err)
	}
	for {
		line, err := imapReadLine(reader)
		if err != nil {
			return mapProtocolRead(account, p.name, errs.ProviderProtocolIMAP, phaseCtx, err)
		}
		if observe != nil {
			if err := observe(line); err != nil {
				return err
			}
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == tag {
			if strings.EqualFold(fields[1], "OK") {
				return nil
			}
			if observe != nil {
				return observe(line)
			}
			return providerData(account, p.name, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		}
	}
}

func parseIMAPGreeting(account, providerName, line string) (map[string]bool, string, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "*" {
		return nil, "", providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	status := strings.ToUpper(fields[1])
	switch status {
	case "OK", "PREAUTH":
	case "BYE":
		return nil, "", providerConnection(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderConnectionReasonProtocolGreeting)
	default:
		return nil, "", providerData(account, providerName, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	return extractCapabilityResponseCode(line), status, nil
}

func extractCapabilityResponseCode(line string) map[string]bool {
	start := strings.Index(line, "[")
	end := strings.Index(line, "]")
	if start < 0 || end <= start {
		return nil
	}
	fields := strings.Fields(line[start+1 : end])
	if len(fields) == 0 || !strings.EqualFold(fields[0], "CAPABILITY") {
		return nil
	}
	return capabilityMap(fields[1:])
}

func capabilityMap(fields []string) map[string]bool {
	out := map[string]bool{}
	for _, field := range fields {
		out[strings.ToUpper(field)] = true
	}
	return out
}

func hasCapability(caps map[string]bool, cap string) bool {
	return caps[strings.ToUpper(cap)]
}

func imapReadLine(reader *bufio.Reader) (string, error) {
	line, err := readLineLimited(reader, limits.IMAPMaxLineBytes)
	if err != nil {
		return "", err
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	if literalSize, ok, err := imapLiteralMarker(text); err != nil {
		return "", err
	} else if ok && literalSize > limits.IMAPMaxMetadataLiteralBytes {
		return "", errLiteralLimit
	}
	return text, nil
}

func imapLiteralMarker(line string) (int64, bool, error) {
	if !strings.HasSuffix(line, "}") {
		return 0, false, nil
	}
	start := strings.LastIndex(line, "{")
	if start < 0 {
		start = strings.LastIndex(line, "~{")
	}
	if start < 0 {
		return 0, false, nil
	}
	body := strings.TrimPrefix(strings.TrimSuffix(line[start+1:], "}"), "")
	body = strings.TrimSuffix(body, "+")
	if body == "" {
		return 0, true, errInvalidProtocol
	}
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil || n < 0 {
		return 0, true, errInvalidProtocol
	}
	return n, true, nil
}

func (p *Provider) smtpEHLO(ctx context.Context, conn net.Conn, reader *bufio.Reader, account string) (map[string]string, error) {
	reply, err := smtpCommand(ctx, conn, reader, "EHLO localhost", commandTimeout)
	if err != nil {
		return nil, mapSMTPReply(account, p.name, ctx, err, false)
	}
	if reply.Code != 250 {
		return nil, providerData(account, p.name, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
	extensions := map[string]string{}
	for i, line := range reply.Lines {
		if i == 0 {
			continue
		}
		text := ""
		if len(line) > 4 {
			text = line[4:]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		key := strings.ToUpper(fields[0])
		if _, exists := extensions[key]; exists {
			return nil, providerData(account, p.name, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
		}
		extensions[key] = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	}
	return extensions, nil
}

func smtpCommand(ctx context.Context, conn net.Conn, reader *bufio.Reader, command string, timeout time.Duration) (smtpReply, error) {
	phaseCtx, cancel := withDeadline(ctx, conn, timeout)
	defer cancel()
	if _, err := io.WriteString(conn, command+"\r\n"); err != nil {
		return smtpReply{}, err
	}
	return smtpReadReply(phaseCtx, conn, reader, timeout)
}

type smtpReply struct {
	Code  int
	Lines []string
}

func smtpReadReply(ctx context.Context, conn net.Conn, reader *bufio.Reader, timeout time.Duration) (smtpReply, error) {
	_, cancel := withDeadline(ctx, conn, timeout)
	defer cancel()
	var reply smtpReply
	var total int64
	for {
		line, err := readLineLimited(reader, 2000)
		if err != nil {
			return smtpReply{}, err
		}
		if int64(len(line)) > SMTPMaxReplyBytes-total {
			return smtpReply{}, errLineLimit
		}
		total += int64(len(line))
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if len(text) < 4 {
			return smtpReply{}, errInvalidProtocol
		}
		code, err := strconv.Atoi(text[:3])
		if err != nil || code < 100 || code > 599 || (text[3] != '-' && text[3] != ' ') {
			return smtpReply{}, errInvalidProtocol
		}
		if len(reply.Lines) == 0 {
			reply.Code = code
		} else if reply.Code != code {
			return smtpReply{}, errInvalidProtocol
		}
		reply.Lines = append(reply.Lines, text)
		if text[3] == ' ' {
			return reply, nil
		}
	}
}

func readLineLimited(reader *bufio.Reader, limit int64) ([]byte, error) {
	var out []byte
	for {
		part, err := reader.ReadSlice('\n')
		out = append(out, part...)
		if int64(len(out)) > limit {
			return nil, errLineLimit
		}
		if err == nil {
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

var (
	errLineLimit       = errors.New("line_limit")
	errLiteralLimit    = errors.New("literal_limit")
	errInvalidProtocol = errors.New("invalid_protocol")
)

func mapProtocolRead(account, providerName string, protocol errs.ProviderProtocol, ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errLineLimit):
		return providerData(account, providerName, protocol, errs.ProviderDataReasonLineLimitExceeded)
	case errors.Is(err, errLiteralLimit):
		return providerData(account, providerName, protocol, errs.ProviderDataReasonLiteralLimitExceeded)
	case errors.Is(err, errInvalidProtocol):
		return providerData(account, providerName, protocol, errs.ProviderDataReasonInvalidProtocol)
	case errors.Is(err, io.EOF):
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonConnectionLost)
	case ctx.Err() != nil:
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
		}
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonConnectionLost)
	}
}

func mapProtocolWrite(account, providerName string, protocol errs.ProviderProtocol, ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonTimeout)
	}
	return providerConnection(account, providerName, protocol, errs.ProviderConnectionReasonConnectionLost)
}

// mapTLSFailure explains a failed TLS handshake. It looks at the error
// itself, not at the context, so a certificate problem is reported as one
// even though the handshake's context ends right after.
func mapTLSFailure(account, providerName string, protocol errs.ProviderProtocol, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	reason := errs.ProviderConnectionReasonTLSHandshakeFailed
	var verifyErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var netErr net.Error
	switch {
	case errors.As(err, &verifyErr), errors.As(err, &unknownAuthority), errors.As(err, &hostnameErr), errors.As(err, &invalidErr):
		reason = errs.ProviderConnectionReasonTLSVerificationFailed
	case errors.As(err, &recordErr):
		reason = errs.ProviderConnectionReasonTLSNotOffered
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		reason = errs.ProviderConnectionReasonTimeout
	}
	return errs.WithDetails(providerConnection(account, providerName, protocol, reason), map[string]any{"cause": shortCause(err)})
}

func mapSMTPReply(account, providerName string, ctx context.Context, err error, greeting bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errLineLimit) {
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonLineLimitExceeded)
	}
	if errors.Is(err, errInvalidProtocol) {
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
	if errors.Is(err, io.EOF) {
		return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonConnectionLost)
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonTimeout)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonTimeout)
	}
	if greeting {
		return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonProtocolGreeting)
	}
	return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonConnectionLost)
}

func mapSMTPAuthCode(account, providerName string, code int, op provider.InteractiveAuthOperation, fresh bool) error {
	switch code {
	case 235:
		return nil
	case 535:
		return credentialRejected(account, providerName, errs.ProviderProtocolSMTP, op, fresh)
	case 432, 530, 534, 538:
		return providerForbidden(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderForbiddenReasonPolicy)
	case 500, 501, 502, 504:
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	case 421, 450, 451, 452, 454:
		return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonConnectionLost)
	default:
		if code >= 400 && code <= 499 {
			return providerConnection(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderConnectionReasonConnectionLost)
		}
		if code >= 500 && code <= 599 {
			return providerForbidden(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderForbiddenReasonUnknown)
		}
		return providerData(account, providerName, errs.ProviderProtocolSMTP, errs.ProviderDataReasonInvalidProtocol)
	}
}
