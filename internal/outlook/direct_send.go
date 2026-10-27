package outlook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// submitDirectSend sends a new message with Graph's sendMail. It never
// follows redirects, and the request body cannot be sent a second time.
// With the standard HTTP transport, the mail counts as possibly sent once
// the transport has a connection, because a cancelled request may still be
// writing. A transport maildo cannot watch counts as possibly sent as soon
// as the request starts.
func (c *Client) submitDirectSend(ctx context.Context, message mail.OutboundMessage) (mail.DirectSendOutcome, error) {
	return c.submitSendMail(ctx, errs.WriteOutlookDirectSend, graphOutboundMessage(message))
}

// submitSendMail sends one message with sendMail. op names the kind of send
// in the write outcome.
func (c *Client) submitSendMail(ctx context.Context, op errs.ProviderWriteOperation, message map[string]any) (mail.DirectSendOutcome, error) {
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderOutlook, op)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	notApplied := func() (mail.DirectSendOutcome, error) {
		return mail.DirectSendOutcome{}, errs.NewWriteNotApplied(attempt, errs.New(errs.CodeProviderConnectionError, "maildo did not hand the message to Outlook"))
	}
	if ctx.Err() != nil {
		return notApplied()
	}
	header, err := c.authorizationHeader()
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	target := c.graphBaseURL + "/me/sendMail"
	if !c.validGraphURL(target) {
		return notApplied()
	}
	data, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		return notApplied()
	}
	defer clear(data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return notApplied()
	}
	req.Header.Set("Authorization", header)
	req.Header.Set("Content-Type", "application/json")
	client := noRedirectHTTPClient(c.http)
	req, sent := trackTransmission(req, client, attempt)
	defer req.Body.Close()
	markTransmitted := func() {
		if !sent() {
			_ = errs.CrossProviderWriteTransmissionBoundary(attempt)
		}
	}
	resp, dispatchErr := client.Do(req)
	responseClosed := false
	if resp != nil && resp.Body != nil {
		defer func() {
			if !responseClosed {
				_ = resp.Body.Close()
			}
		}()
	}
	unknown := func() (mail.DirectSendOutcome, error) {
		markTransmitted()
		return mail.DirectSendOutcome{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	if dispatchErr != nil || resp == nil || resp.Body == nil {
		if !sent() {
			return notApplied()
		}
		return unknown()
	}
	// Any response proves the request went out, also for transports maildo
	// cannot watch.
	markTransmitted()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout {
		var cause error = errs.New(errs.CodeProviderForbidden, "Outlook refused to send the message")
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cause = repairRequired(c.account)
		} else if resp.StatusCode == http.StatusTooManyRequests {
			cause = errs.New(errs.CodeProviderRateLimited, "Outlook compose request was rate limited")
		}
		marked := errs.NewDefinitiveProviderRejectionError(c.account, errs.ProviderOutlook, op, cause)
		rejection, ok := errs.AsDefinitiveProviderRejection(marked)
		if !ok {
			return mail.DirectSendOutcome{}, errs.NewSharedProviderContractInvalid()
		}
		rejected := errs.NewWriteRejected(attempt, rejection)
		if resp.StatusCode == http.StatusTooManyRequests {
			return mail.DirectSendOutcome{}, &rungrad.Error{Code: rungrad.ExitRateLimited, Err: rejected}
		}
		return mail.DirectSendOutcome{}, rejected
	}
	if resp.StatusCode != http.StatusAccepted {
		return unknown()
	}
	// Graph accepts a sendMail with a 202 and an empty body. A body, or one
	// maildo cannot read, does not prove the mail was accepted.
	acceptance, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	closeErr := resp.Body.Close()
	responseClosed = true
	defer clear(acceptance)
	if readErr != nil || closeErr != nil || len(acceptance) != 0 {
		return unknown()
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.DirectSendOutcome{}, err
	}
	return mail.DirectSendOutcome{WriteOutcomeFacts: facts}, nil
}

// trackTransmission marks attempt as sent once the request may have left
// maildo: when the connection is handed over or the first byte of the body
// is read. A transport maildo cannot observe counts as sent right away. The
// returned function reports whether that happened. The request must not be
// replayed on a stale connection, so it loses GetBody.
func trackTransmission(req *http.Request, client *http.Client, attempt errs.ProviderWriteAttempt) (*http.Request, func() bool) {
	var transmitted atomic.Bool
	var once sync.Once
	mark := func() {
		once.Do(func() {
			_ = errs.CrossProviderWriteTransmissionBoundary(attempt)
			transmitted.Store(true)
		})
	}
	trace := &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { mark() },
		WroteHeaders: mark,
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = &graphSendBody{body: req.Body, start: mark}
	}
	req.GetBody = nil
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, observable := transport.(*http.Transport); !observable {
		mark()
	}
	return req, transmitted.Load
}

type graphSendBody struct {
	mu     sync.Mutex
	body   io.ReadCloser
	closed bool
	start  func()
}

func (b *graphSendBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.start()
	return b.body.Read(p)
}

func (b *graphSendBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.body.Close()
}

type DirectSendPreparer struct{}

func (DirectSendPreparer) ProviderName() string { return "outlook" }

func (DirectSendPreparer) PrepareDirectSend(ctx context.Context, identity provider.ComposeIdentity, msg mail.OutboundMessage) (mail.PreparedDirectSend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if identity.Provider != "outlook" || identity.Account == "" || identity.Email == "" {
		return nil, errs.NewProviderClientContractInvalid()
	}
	var total int64
	for _, a := range msg.Attachments {
		total += int64(len(a.Bytes))
	}
	if total > maxSendAttachmentBytes {
		return nil, outlookAttachmentsTooLarge(identity.Account)
	}
	return &preparedDirectSend{message: cloneOutboundMessage(msg)}, nil
}

type preparedDirectSend struct {
	mu        sync.Mutex
	message   mail.OutboundMessage
	consumed  bool
	destroyed bool
}

func (p *preparedDirectSend) Destroy() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed {
		return
	}
	p.destroyed = true
	p.message = mail.OutboundMessage{}
}

func (p *preparedDirectSend) consume() (mail.OutboundMessage, error) {
	if p == nil {
		return mail.OutboundMessage{}, errs.NewProviderClientContractInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed || p.consumed {
		return mail.OutboundMessage{}, errs.NewProviderClientContractInvalid()
	}
	p.consumed = true
	msg := p.message
	p.message = mail.OutboundMessage{}
	return msg, nil
}

func cloneOutboundMessage(in mail.OutboundMessage) mail.OutboundMessage {
	out := in
	out.From = cloneRecipientPtr(in.From)
	out.To = cloneRecipients(in.To)
	out.CC = cloneRecipients(in.CC)
	out.BCC = cloneRecipients(in.BCC)
	out.ReplyTo = cloneRecipients(in.ReplyTo)
	return out
}

func cloneRecipientPtr(in *mail.Recipient) *mail.Recipient {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneRecipients(in []mail.Recipient) []mail.Recipient {
	if len(in) == 0 {
		return nil
	}
	return append([]mail.Recipient(nil), in...)
}
