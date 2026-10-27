// Package mimecompose builds and reads the MIME messages that maildo sends
// and saves as drafts. A Document holds a message the same way for every
// provider. Encode writes it as raw MIME, and ParseDocument and
// ParseVerificationProjection read raw MIME back, for example to check what
// a server stored. Each of these steps checks size limits.
package mimecompose

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	netmail "net/mail"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimeparam"
	"golang.org/x/net/idna"
)

type AddressHeaderMode string

const (
	AddressHeaderASCII    AddressHeaderMode = "ascii"
	AddressHeaderSMTPUTF8 AddressHeaderMode = "smtputf8"
)

type BccHeaderPolicy string

const (
	BccHeaderPolicyRetain BccHeaderPolicy = "retain"
	BccHeaderPolicyOmit   BccHeaderPolicy = "omit"
)

type PartRole string

const (
	PartRoleRelated    PartRole = "related"
	PartRoleAttachment PartRole = "attachment"
)

type Header struct{ Name, Value string }

type BinaryPart struct {
	Role        PartRole
	ContentType string
	Disposition string
	ContentID   string
	Filename    string
	Description string
	Language    string
	Location    string
	Bytes       []byte
}

type Message struct {
	From              mail.Recipient
	To                []mail.Recipient
	CC                []mail.Recipient
	BCC               []mail.Recipient
	ReplyTo           []mail.Recipient
	Subject           string
	TextBody          []byte
	HTMLBody          []byte
	HTMLRootContentID string
	InReplyTo         string
	References        []string
	RootHeaders       []Header
	Related           []BinaryPart
	Attachments       []BinaryPart
}

type AddressPatch struct {
	Set    bool
	Values []mail.Recipient
}
type StringPatch struct {
	Set   bool
	Value string
}
type BytesPatch struct {
	Set   bool
	Value []byte
}
type PartsPatch struct {
	Set    bool
	Values []BinaryPart
}

type Patch struct {
	To, CC, BCC, ReplyTo AddressPatch
	Subject              StringPatch
	TextBody             BytesPatch
	HTMLBody             BytesPatch
	HTMLRootContentID    StringPatch
	Related              PartsPatch
	Attachments          PartsPatch
}

type Limits struct {
	MaxEncodedBytes int64
	MaxHeaderBytes  int64
	MaxHeaderCount  uint32
	MaxAddressCount uint32
	MaxDepth        uint32
	MaxParts        uint32
}

type VerificationLimits struct {
	Semantic                    Limits
	MaxRawBytes                 int64
	MaxDiscardedRootHeaderBytes int64
	MaxDiscardedRootHeaderCount uint32
}

type LimitKind string

const (
	LimitHeaders                  LimitKind = "headers"
	LimitHeaderBytes              LimitKind = "header_bytes"
	LimitAddresses                LimitKind = "addresses"
	LimitMIMEDepth                LimitKind = "mime_depth"
	LimitMIMEParts                LimitKind = "mime_parts"
	LimitPhysicalLineBytes        LimitKind = "physical_line_bytes"
	LimitEncodedMessageBytes      LimitKind = "encoded_message_bytes"
	LimitVerificationRawBytes     LimitKind = "verification_raw_bytes"
	LimitDiscardedRootHeaders     LimitKind = "discarded_root_headers"
	LimitDiscardedRootHeaderBytes LimitKind = "discarded_root_header_bytes"
)

type LimitViolation struct {
	Kind            LimitKind
	Limit           uint64
	ObservedAtLeast uint64
}

type Sources struct {
	Now       func() time.Time
	MessageID func() (string, error)
	Boundary  func() (string, error)
}

type EncodeOptions struct {
	BccPolicy   BccHeaderPolicy
	AddressMode AddressHeaderMode
	Sources     Sources
}

type PartProjection struct {
	Role        PartRole
	ContentType string
	Disposition string
	ContentID   string
	Filename    string
	Description string
	Language    string
	Location    string
	DecodedSize uint64
	SHA256      [32]byte
}

type SemanticProjection struct {
	From              mail.Recipient
	To                []mail.Recipient
	CC                []mail.Recipient
	BCC               []mail.Recipient
	ReplyTo           []mail.Recipient
	Subject           string
	TextBody          []byte
	HTMLBody          []byte
	HTMLRootContentID string
	InReplyTo         string
	References        []string
	RootHeaders       []Header
	Related           []PartProjection
	Attachments       []PartProjection
}

type Document struct {
	mu        sync.Mutex
	msg       Message
	limits    Limits
	destroyed bool
}

type EncodedMessage struct {
	Bytes      []byte
	Projection SemanticProjection
}

var (
	ErrInvalidOptions          = errors.New("MIME options are invalid")
	ErrInvalidSemanticInput    = errors.New("MIME semantic input is invalid")
	ErrUnsupportedMIME         = errors.New("MIME content is unsupported")
	ErrHeaderLimitExceeded     = errors.New("MIME header limit is exceeded")
	ErrAddressLimitExceeded    = errors.New("MIME address limit is exceeded")
	ErrStructureLimitExceeded  = errors.New("MIME structure limit is exceeded")
	ErrLineLimitExceeded       = errors.New("MIME line limit is exceeded")
	ErrSourceFailure           = errors.New("MIME generation source failed")
	ErrEncodedMessageTooLarge  = errors.New("encoded message size is exceeded")
	ErrVerificationRawTooLarge = errors.New("verification raw message size is exceeded")
	ErrDestroyed               = errors.New("MIME value is destroyed")
)

type limitError struct {
	sentinel error
	fact     LimitViolation
}

func (e *limitError) Error() string { return e.sentinel.Error() }
func (e *limitError) Unwrap() error { return e.sentinel }

func DefaultLimits() Limits {
	return Limits{
		MaxEncodedBytes: int64(mail.MaxOutboundEncodedMessageBytes),
		MaxHeaderBytes:  256 * 1024,
		MaxHeaderCount:  200,
		MaxAddressCount: 200,
		MaxDepth:        32,
		MaxParts:        1000,
	}
}

func DefaultVerificationLimits() VerificationLimits {
	return VerificationLimits{
		Semantic:                    DefaultLimits(),
		MaxRawBytes:                 36_700_160,
		MaxDiscardedRootHeaderBytes: 262_144,
		MaxDiscardedRootHeaderCount: 64,
	}
}

func LimitViolationFromError(err error) (LimitViolation, bool) {
	var target *limitError
	if errors.As(err, &target) && target != nil {
		return target.fact, true
	}
	return LimitViolation{}, false
}

func NewDocument(msg Message, limits Limits) (*Document, error) {
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return nil, err
	}
	clean := cloneMessage(msg)
	cleanPartNames(clean.Attachments)
	cleanPartNames(clean.Related)
	if err := validateMessage(clean, normalized); err != nil {
		destroyMessage(&clean)
		return nil, err
	}
	return &Document{msg: clean, limits: normalized}, nil
}

// cleanPartNames replaces control characters in file names. A name such as
// "a\x1b[31mred.txt" from a hostile message would otherwise make the whole
// message impossible to send.
func cleanPartNames(parts []BinaryPart) {
	for i := range parts {
		parts[i].Filename = mimeparam.CleanName(parts[i].Filename)
	}
}

func ParseDocument(raw []byte, limits Limits) (*Document, error) {
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return nil, err
	}
	if err := checkBytesLimit(int64(len(raw)), normalized.MaxEncodedBytes, ErrEncodedMessageTooLarge, LimitEncodedMessageBytes); err != nil {
		return nil, err
	}
	if hasBareCR(raw) {
		return nil, ErrInvalidSemanticInput
	}
	if err := checkPhysicalLines(raw); err != nil {
		return nil, err
	}
	msg, err := parseMessage(raw, nil)
	if err != nil {
		return nil, err
	}
	return NewDocument(msg, normalized)
}

func ParseVerificationProjection(raw []byte, limits VerificationLimits) (SemanticProjection, error) {
	normalized, err := normalizeVerificationLimits(limits)
	if err != nil {
		return SemanticProjection{}, err
	}
	if err := checkBytesLimit(int64(len(raw)), normalized.MaxRawBytes, ErrVerificationRawTooLarge, LimitVerificationRawBytes); err != nil {
		return SemanticProjection{}, err
	}
	if hasBareCR(raw) {
		return SemanticProjection{}, ErrInvalidSemanticInput
	}
	if err := checkPhysicalLines(raw); err != nil {
		return SemanticProjection{}, err
	}
	msg, err := parseMessage(raw, &normalized)
	if err != nil {
		return SemanticProjection{}, err
	}
	doc, err := NewDocument(msg, normalized.Semantic)
	if err != nil {
		return SemanticProjection{}, err
	}
	defer doc.Destroy()
	return doc.Projection()
}

func (d *Document) Apply(p Patch) error {
	if d == nil {
		return ErrDestroyed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyed {
		return ErrDestroyed
	}
	next := d.msg
	if p.To.Set {
		next.To = p.To.Values
	}
	if p.CC.Set {
		next.CC = p.CC.Values
	}
	if p.BCC.Set {
		next.BCC = p.BCC.Values
	}
	if p.ReplyTo.Set {
		next.ReplyTo = p.ReplyTo.Values
	}
	if p.Subject.Set {
		next.Subject = p.Subject.Value
	}
	if p.TextBody.Set {
		next.TextBody = p.TextBody.Value
	}
	if p.HTMLBody.Set {
		next.HTMLBody = p.HTMLBody.Value
	}
	if p.HTMLRootContentID.Set {
		next.HTMLRootContentID = p.HTMLRootContentID.Value
	}
	if p.Related.Set {
		next.Related = p.Related.Values
	}
	if p.Attachments.Set {
		next.Attachments = p.Attachments.Values
	}
	clean := cloneMessage(next)
	cleanPartNames(clean.Attachments)
	cleanPartNames(clean.Related)
	if err := validateMessage(clean, d.limits); err != nil {
		destroyMessage(&clean)
		return err
	}
	d.msg = clean
	return nil
}

func (d *Document) Projection() (SemanticProjection, error) {
	if d == nil {
		return SemanticProjection{}, ErrDestroyed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyed {
		return SemanticProjection{}, ErrDestroyed
	}
	return projectMessage(d.msg), nil
}

func (d *Document) Message() (Message, error) {
	if d == nil {
		return Message{}, ErrDestroyed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyed {
		return Message{}, ErrDestroyed
	}
	return cloneMessage(d.msg), nil
}

func (d *Document) Encode(opts EncodeOptions) (*EncodedMessage, error) {
	if d == nil {
		return nil, ErrDestroyed
	}
	encOpts, productionSources, err := normalizeEncodeOptions(opts)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.destroyed {
		d.mu.Unlock()
		return nil, ErrDestroyed
	}
	msg := cloneMessage(d.msg)
	limits := d.limits
	d.mu.Unlock()
	if err := validateAddressMode(msg, encOpts.AddressMode); err != nil {
		return nil, err
	}
	now := encOpts.Sources.Now()
	if now.IsZero() {
		return nil, ErrSourceFailure
	}
	var messageID string
	if productionSources {
		messageID, err = productionMessageIDForSender(msg.From.Address)
		if err != nil {
			return nil, ErrSourceFailure
		}
	} else {
		messageID, err = encOpts.Sources.MessageID()
		if err != nil {
			return nil, ErrSourceFailure
		}
		messageID, err = mail.NormalizeMessageIDScalar(messageID)
		if err != nil {
			return nil, ErrSourceFailure
		}
	}
	body := newLimitBuffer(limits.MaxEncodedBytes)
	allocator := newBoundaryAllocator(encOpts.Sources.Boundary)
	if err := writeEncodedMessage(body, msg, encOpts, allocator, now.UTC(), messageID); err != nil {
		return nil, err
	}
	if err := checkPhysicalLines(body.Bytes()); err != nil {
		return nil, err
	}
	if err := body.Err(); err != nil {
		return nil, err
	}
	return &EncodedMessage{Bytes: body.Bytes(), Projection: projectMessage(msg)}, nil
}

func (d *Document) Destroy() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyed {
		return
	}
	d.destroyed = true
	destroyMessage(&d.msg)
	d.limits = Limits{}
}

func (p *SemanticProjection) Destroy() {
	if p == nil {
		return
	}
	zeroBytes(p.TextBody)
	zeroBytes(p.HTMLBody)
	*p = SemanticProjection{}
}

func (m *EncodedMessage) Destroy() {
	if m == nil {
		return
	}
	zeroBytes(m.Bytes)
	m.Projection.Destroy()
	*m = EncodedMessage{}
}

func DestroyMessage(msg *Message) {
	destroyMessage(msg)
}

func normalizeLimits(l Limits) (Limits, error) {
	d := DefaultLimits()
	if l.MaxEncodedBytes <= 0 || l.MaxEncodedBytes > d.MaxEncodedBytes ||
		l.MaxHeaderBytes <= 0 || l.MaxHeaderBytes > d.MaxHeaderBytes ||
		l.MaxHeaderCount == 0 || l.MaxHeaderCount > d.MaxHeaderCount ||
		l.MaxAddressCount == 0 || l.MaxAddressCount > d.MaxAddressCount ||
		l.MaxDepth == 0 || l.MaxDepth > d.MaxDepth ||
		l.MaxParts == 0 || l.MaxParts > d.MaxParts {
		return Limits{}, ErrInvalidOptions
	}
	return l, nil
}

func normalizeVerificationLimits(l VerificationLimits) (VerificationLimits, error) {
	d := DefaultVerificationLimits()
	semantic, err := normalizeLimits(l.Semantic)
	if err != nil {
		return VerificationLimits{}, err
	}
	if l.MaxRawBytes <= 0 || l.MaxRawBytes > d.MaxRawBytes ||
		l.MaxDiscardedRootHeaderBytes <= 0 || l.MaxDiscardedRootHeaderBytes > d.MaxDiscardedRootHeaderBytes ||
		l.MaxDiscardedRootHeaderCount == 0 || l.MaxDiscardedRootHeaderCount > d.MaxDiscardedRootHeaderCount {
		return VerificationLimits{}, ErrInvalidOptions
	}
	l.Semantic = semantic
	return l, nil
}

func normalizeEncodeOptions(opts EncodeOptions) (EncodeOptions, bool, error) {
	if opts.BccPolicy != BccHeaderPolicyRetain && opts.BccPolicy != BccHeaderPolicyOmit {
		return EncodeOptions{}, false, ErrInvalidOptions
	}
	if opts.AddressMode != AddressHeaderASCII && opts.AddressMode != AddressHeaderSMTPUTF8 {
		return EncodeOptions{}, false, ErrInvalidOptions
	}
	empty := opts.Sources.Now == nil && opts.Sources.MessageID == nil && opts.Sources.Boundary == nil
	full := opts.Sources.Now != nil && opts.Sources.MessageID != nil && opts.Sources.Boundary != nil
	if !empty && !full {
		return EncodeOptions{}, false, ErrInvalidOptions
	}
	if empty {
		opts.Sources = Sources{
			Now:       time.Now,
			MessageID: productionMessageID,
			Boundary:  productionBoundary,
		}
	}
	return opts, empty, nil
}

func validateMessage(msg Message, limits Limits) error {
	if strings.TrimSpace(msg.From.Address) == "" {
		return ErrInvalidSemanticInput
	}
	if !utf8.ValidString(msg.Subject) || !utf8.ValidString(msg.HTMLRootContentID) || !utf8.ValidString(msg.InReplyTo) {
		return ErrInvalidSemanticInput
	}
	if !utf8.Valid(msg.TextBody) || !utf8.Valid(msg.HTMLBody) {
		return ErrInvalidSemanticInput
	}
	if hasBareCR(msg.TextBody) || hasBareCR(msg.HTMLBody) {
		return ErrInvalidSemanticInput
	}
	addresses := 1 + len(msg.To) + len(msg.CC) + len(msg.BCC) + len(msg.ReplyTo)
	if uint64(addresses) > uint64(limits.MaxAddressCount) {
		return newLimitError(ErrAddressLimitExceeded, LimitAddresses, uint64(limits.MaxAddressCount))
	}
	for _, r := range append(append(append(append([]mail.Recipient{msg.From}, msg.To...), msg.CC...), msg.BCC...), msg.ReplyTo...) {
		if strings.TrimSpace(r.Address) == "" || !utf8.ValidString(r.Address) || !utf8.ValidString(r.Name) ||
			strings.ContainsAny(r.Name, "\r\n\x00") {
			return ErrInvalidSemanticInput
		}
		if err := validateMailboxAddress(r.Address, AddressHeaderSMTPUTF8); err != nil {
			return ErrInvalidSemanticInput
		}
	}
	if msg.InReplyTo != "" {
		if _, err := mail.NormalizeMessageIDScalar(msg.InReplyTo); err != nil {
			return ErrInvalidSemanticInput
		}
	}
	if len(msg.References) > 0 {
		seen := map[string]struct{}{}
		for _, ref := range msg.References {
			normalized, err := mail.NormalizeMessageIDScalar(ref)
			if err != nil {
				return ErrInvalidSemanticInput
			}
			if _, ok := seen[normalized]; ok {
				return ErrInvalidSemanticInput
			}
			seen[normalized] = struct{}{}
		}
	}
	if len(msg.Related) > 0 && len(msg.HTMLBody) == 0 {
		return ErrInvalidSemanticInput
	}
	if msg.HTMLRootContentID != "" && len(msg.HTMLBody) == 0 {
		return ErrInvalidSemanticInput
	}
	if msg.HTMLRootContentID != "" {
		if _, err := mail.NormalizeMessageIDScalar(msg.HTMLRootContentID); err != nil {
			return ErrInvalidSemanticInput
		}
	}
	partCount := uint64(1 + 1 + len(msg.Related) + len(msg.Attachments))
	if len(msg.TextBody) > 0 && len(msg.HTMLBody) > 0 {
		partCount++
	}
	if uint64(limits.MaxParts) < partCount {
		return newLimitError(ErrStructureLimitExceeded, LimitMIMEParts, uint64(limits.MaxParts))
	}
	if limits.MaxDepth < 1 {
		return newLimitError(ErrStructureLimitExceeded, LimitMIMEDepth, uint64(limits.MaxDepth))
	}
	headerCount := 7 + len(msg.RootHeaders)
	if uint64(headerCount) > uint64(limits.MaxHeaderCount) {
		return newLimitError(ErrHeaderLimitExceeded, LimitHeaders, uint64(limits.MaxHeaderCount))
	}
	headerBytes := uint64(len(msg.Subject) + len(msg.InReplyTo) + len(strings.Join(msg.References, " ")))
	for _, h := range msg.RootHeaders {
		if err := validatePreservedRootHeader(h); err != nil {
			return err
		}
		headerBytes += uint64(len(canonicalHeaderName(h.Name)) + 2 + len(h.Value))
	}
	for _, part := range msg.Related {
		if err := validatePart(part); err != nil {
			return err
		}
		headerBytes += uint64(len(part.ContentType) + len(part.Disposition) + len(part.ContentID) + len(part.Filename) + len(part.Description) + len(part.Language) + len(part.Location))
	}
	for _, part := range msg.Attachments {
		if err := validatePart(part); err != nil {
			return err
		}
		headerBytes += uint64(len(part.ContentType) + len(part.Disposition) + len(part.ContentID) + len(part.Filename) + len(part.Description) + len(part.Language) + len(part.Location))
	}
	if headerBytes > uint64(limits.MaxHeaderBytes) {
		return newLimitError(ErrHeaderLimitExceeded, LimitHeaderBytes, uint64(limits.MaxHeaderBytes))
	}
	total := uint64(len(msg.TextBody)) + uint64(len(msg.HTMLBody))
	for _, part := range msg.Related {
		total += uint64(len(part.Bytes))
	}
	for _, part := range msg.Attachments {
		total += uint64(len(part.Bytes))
	}
	if total > uint64(limits.MaxEncodedBytes) {
		return newLimitError(ErrEncodedMessageTooLarge, LimitEncodedMessageBytes, uint64(limits.MaxEncodedBytes))
	}
	return nil
}

func validatePart(part BinaryPart) error {
	switch part.Role {
	case PartRoleRelated, PartRoleAttachment:
	default:
		return ErrInvalidSemanticInput
	}
	if part.Role == PartRoleRelated && part.Disposition == "attachment" {
		return ErrInvalidSemanticInput
	}
	if strings.TrimSpace(part.ContentType) == "" {
		return ErrInvalidSemanticInput
	}
	if _, _, err := mime.ParseMediaType(part.ContentType); err != nil {
		if !strings.Contains(part.ContentType, "/") {
			return ErrInvalidSemanticInput
		}
	}
	for _, value := range []string{part.Disposition, part.ContentID, part.Filename, part.Description, part.Language, part.Location} {
		if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
			return ErrInvalidSemanticInput
		}
	}
	if part.ContentID != "" {
		if _, err := mail.NormalizeMessageIDScalar(part.ContentID); err != nil {
			return ErrInvalidSemanticInput
		}
	}
	return nil
}

func validHeader(name, value string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(value, "\r\n\x00") || !utf8.ValidString(value) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

func validatePreservedRootHeader(h Header) error {
	if !validHeader(h.Name, h.Value) {
		return ErrInvalidSemanticInput
	}
	name := canonicalHeaderName(h.Name)
	if byteSensitiveRootHeader(name) {
		return ErrUnsupportedMIME
	}
	if controlledRootHeader(name) || !preservedRootHeader(name) {
		return ErrInvalidSemanticInput
	}
	return nil
}

func canonicalHeaderName(name string) string {
	return textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
}

func controlledRootHeader(name string) bool {
	switch strings.ToLower(name) {
	case "from", "to", "cc", "bcc", "reply-to", "subject", "date", "message-id", "mime-version", "content-type", "content-transfer-encoding", "in-reply-to", "references":
		return true
	default:
		return false
	}
}

func preservedRootHeader(name string) bool {
	switch strings.ToLower(name) {
	case "comments", "keywords", "content-language", "importance", "priority", "x-priority":
		return true
	default:
		return false
	}
}

func byteSensitiveRootHeader(name string) bool {
	switch strings.ToLower(name) {
	case "dkim-signature", "domainkey-signature", "arc-seal", "arc-message-signature", "content-md5":
		return true
	default:
		return false
	}
}

func verificationDiscardedRootHeader(name string) bool {
	switch strings.ToLower(name) {
	case "received", "return-path", "authentication-results", "arc-authentication-results", "dkim-signature", "domainkey-signature", "arc-seal", "arc-message-signature":
		return true
	default:
		return false
	}
}

func checkBytesLimit(observed int64, limit int64, sentinel error, kind LimitKind) error {
	if observed > limit {
		return newLimitError(sentinel, kind, uint64(limit))
	}
	return nil
}

func newLimitError(sentinel error, kind LimitKind, limit uint64) error {
	return &limitError{sentinel: sentinel, fact: LimitViolation{Kind: kind, Limit: limit, ObservedAtLeast: limit + 1}}
}

func parseMessage(raw []byte, verification *VerificationLimits) (Message, error) {
	reader := bytes.NewReader(normalizeRawNewlines(raw))
	parsed, err := netmail.ReadMessage(reader)
	if err != nil {
		return Message{}, ErrInvalidSemanticInput
	}
	if err := validateParsedRootHeaders(parsed.Header, verification); err != nil {
		return Message{}, err
	}
	msg := Message{}
	from, err := mail.ParseMailboxListValues([]string{parsed.Header.Get("From")})
	if err != nil || len(from) != 1 {
		return Message{}, ErrInvalidSemanticInput
	}
	msg.From = from[0]
	if msg.To, err = parseHeaderRecipients(parsed.Header, "To"); err != nil {
		return Message{}, err
	}
	if msg.CC, err = parseHeaderRecipients(parsed.Header, "Cc"); err != nil {
		return Message{}, err
	}
	if msg.BCC, err = parseHeaderRecipients(parsed.Header, "Bcc"); err != nil {
		return Message{}, err
	}
	if msg.ReplyTo, err = parseHeaderRecipients(parsed.Header, "Reply-To"); err != nil {
		return Message{}, err
	}
	msg.Subject = parsed.Header.Get("Subject")
	msg.InReplyTo = parsed.Header.Get("In-Reply-To")
	if refs := strings.TrimSpace(parsed.Header.Get("References")); refs != "" {
		tokens, err := mail.ParseMessageIDTokens(refs)
		if err != nil {
			return Message{}, ErrInvalidSemanticInput
		}
		msg.References = tokens
	}
	contentType := parsed.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		return Message{}, ErrInvalidSemanticInput
	}
	body, err = decodeTransfer(body, parsed.Header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return Message{}, err
	}
	if err := parseBody(contentType, body, &msg, 0); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func validateParsedRootHeaders(headers netmail.Header, verification *VerificationLimits) error {
	var discardedCount uint64
	var discardedBytes uint64
	for name, values := range headers {
		canonical := canonicalHeaderName(name)
		for _, value := range values {
			if !validHeader(canonical, value) {
				return ErrInvalidSemanticInput
			}
			if strings.EqualFold(canonical, "Content-Md5") {
				return ErrUnsupportedMIME
			}
			if verification != nil && verificationDiscardedRootHeader(canonical) {
				discardedCount++
				if discardedCount > uint64(verification.MaxDiscardedRootHeaderCount) {
					return newLimitError(ErrHeaderLimitExceeded, LimitDiscardedRootHeaders, uint64(verification.MaxDiscardedRootHeaderCount))
				}
				discardedBytes += uint64(len(canonical) + 2 + len(value))
				if discardedBytes > uint64(verification.MaxDiscardedRootHeaderBytes) {
					return newLimitError(ErrHeaderLimitExceeded, LimitDiscardedRootHeaderBytes, uint64(verification.MaxDiscardedRootHeaderBytes))
				}
				continue
			}
			if byteSensitiveRootHeader(canonical) {
				return ErrUnsupportedMIME
			}
		}
	}
	return nil
}

func parseHeaderRecipients(h netmail.Header, name string) ([]mail.Recipient, error) {
	values := h[textproto.CanonicalMIMEHeaderKey(name)]
	if len(values) == 0 {
		return nil, nil
	}
	return mail.ParseMailboxListValues(values)
}

func parseBody(contentType string, body []byte, msg *Message, depth uint32) error {
	if depth > DefaultLimits().MaxDepth {
		return newLimitError(ErrStructureLimitExceeded, LimitMIMEDepth, uint64(DefaultLimits().MaxDepth))
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ErrUnsupportedMIME
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return ErrInvalidSemanticInput
		}
		mr := multipart.NewReader(bytes.NewReader(body), boundary)
		switch mediaType {
		case "multipart/mixed":
			first := true
			for {
				part, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return ErrInvalidSemanticInput
				}
				partBody, _ := io.ReadAll(part)
				if first {
					first = false
					ct := part.Header.Get("Content-Type")
					if ct == "" {
						ct = "text/plain"
					}
					if strings.HasPrefix(strings.ToLower(ct), "text/") {
						var decodeErr error
						partBody, decodeErr = decodeTransfer(partBody, part.Header.Get("Content-Transfer-Encoding"))
						if decodeErr != nil {
							return decodeErr
						}
					}
					if err := parseBody(ct, partBody, msg, depth+1); err != nil {
						return err
					}
					continue
				}
				var decodeErr error
				partBody, decodeErr = decodeTransfer(partBody, part.Header.Get("Content-Transfer-Encoding"))
				if decodeErr != nil {
					return decodeErr
				}
				msg.Attachments = append(msg.Attachments, binaryPartFromMIME(part.Header, partBody, PartRoleAttachment))
			}
		case "multipart/alternative", "multipart/related":
			for {
				part, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return ErrInvalidSemanticInput
				}
				partBody, _ := io.ReadAll(part)
				ct := part.Header.Get("Content-Type")
				if ct == "" {
					ct = "text/plain"
				}
				lower := strings.ToLower(ct)
				if strings.HasPrefix(lower, "text/") || strings.HasPrefix(lower, "multipart/") {
					if strings.HasPrefix(lower, "text/") {
						var decodeErr error
						partBody, decodeErr = decodeTransfer(partBody, part.Header.Get("Content-Transfer-Encoding"))
						if decodeErr != nil {
							return decodeErr
						}
					}
					if err := parseBody(ct, partBody, msg, depth+1); err != nil {
						return err
					}
				} else {
					var decodeErr error
					partBody, decodeErr = decodeTransfer(partBody, part.Header.Get("Content-Transfer-Encoding"))
					if decodeErr != nil {
						return decodeErr
					}
					msg.Related = append(msg.Related, binaryPartFromMIME(part.Header, partBody, PartRoleRelated))
				}
			}
		default:
			return ErrUnsupportedMIME
		}
	case mediaType == "text/plain":
		msg.TextBody = append(msg.TextBody, body...)
	case mediaType == "text/html":
		msg.HTMLBody = append(msg.HTMLBody, body...)
	default:
		return ErrUnsupportedMIME
	}
	return nil
}

func binaryPartFromMIME(h textproto.MIMEHeader, body []byte, role PartRole) BinaryPart {
	contentType := h.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	mediaType, typeParams := mimeparam.Split(contentType)
	disposition, dispParams := mimeparam.Split(h.Get("Content-Disposition"))
	return BinaryPart{
		Role:        role,
		ContentType: mediaType,
		Disposition: disposition,
		ContentID:   h.Get("Content-ID"),
		Filename:    mimeparam.FileName(dispParams, typeParams),
		Description: h.Get("Content-Description"),
		Language:    h.Get("Content-Language"),
		Location:    h.Get("Content-Location"),
		Bytes:       cloneBytes(body),
	}
}

func normalizeRawNewlines(raw []byte) []byte {
	if bytes.Contains(raw, []byte{'\r'}) && !bytes.Contains(raw, []byte("\r\n")) {
		return raw
	}
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
}

func hasBareCR(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\r' && (i+1 >= len(raw) || raw[i+1] != '\n') {
			return true
		}
	}
	return false
}

func decodeTransfer(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit":
		return body, nil
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, ErrInvalidSemanticInput
		}
		return out, nil
	case "base64":
		cleaned := bytes.ReplaceAll(bytes.ReplaceAll(body, []byte("\r\n"), nil), []byte("\n"), nil)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(cleaned)))
		n, err := base64.StdEncoding.Decode(out, cleaned)
		if err != nil {
			return nil, ErrInvalidSemanticInput
		}
		return out[:n], nil
	default:
		return nil, ErrUnsupportedMIME
	}
}

type limitBuffer struct {
	buf      bytes.Buffer
	limit    int64
	exceeded bool
}

func newLimitBuffer(limit int64) *limitBuffer {
	return &limitBuffer{limit: limit}
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	remaining := b.limit + 1 - int64(b.buf.Len())
	if remaining <= 0 {
		if len(p) > 0 {
			b.exceeded = true
		}
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		b.buf.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitBuffer) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

func (b *limitBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

func (b *limitBuffer) Err() error {
	if b == nil {
		return nil
	}
	if b.exceeded || int64(b.buf.Len()) > b.limit {
		return newLimitError(ErrEncodedMessageTooLarge, LimitEncodedMessageBytes, uint64(b.limit))
	}
	return nil
}

type encodedBodyNode struct {
	contentType string
	boundaries  []string
	write       func(*limitBuffer) error
}

func writeEncodedMessage(buf *limitBuffer, msg Message, opts EncodeOptions, boundaries *boundaryAllocator, now time.Time, messageID string) error {
	body, err := buildBodyPart(msg, boundaries)
	if err != nil {
		return err
	}
	headers := []Header{
		{Name: "Date", Value: now.Format("Mon, 02 Jan 2006 15:04:05 +0000")},
		{Name: "From", Value: formatAddress(msg.From)},
	}
	if len(msg.To) > 0 {
		headers = append(headers, Header{Name: "To", Value: formatAddressList("To", msg.To)})
	}
	if len(msg.CC) > 0 {
		headers = append(headers, Header{Name: "Cc", Value: formatAddressList("Cc", msg.CC)})
	}
	if opts.BccPolicy == BccHeaderPolicyRetain && len(msg.BCC) > 0 {
		headers = append(headers, Header{Name: "Bcc", Value: formatAddressList("Bcc", msg.BCC)})
	}
	if len(msg.ReplyTo) > 0 {
		headers = append(headers, Header{Name: "Reply-To", Value: formatAddressList("Reply-To", msg.ReplyTo)})
	}
	headers = append(headers,
		Header{Name: "Subject", Value: mime.QEncoding.Encode("utf-8", msg.Subject)},
		Header{Name: "Message-ID", Value: messageID},
	)
	if msg.InReplyTo != "" {
		headers = append(headers, Header{Name: "In-Reply-To", Value: msg.InReplyTo})
	}
	if len(msg.References) > 0 {
		headers = append(headers, Header{Name: "References", Value: strings.Join(msg.References, " ")})
	}
	headers = append(headers, Header{Name: "MIME-Version", Value: "1.0"})
	headers = append(headers, orderedRootHeaders(msg.RootHeaders)...)
	headers = append(headers, Header{Name: "Content-Type", Value: body.contentType})
	if strings.HasPrefix(strings.ToLower(body.contentType), "text/") {
		headers = append(headers, Header{Name: "Content-Transfer-Encoding", Value: "quoted-printable"})
	}
	for _, h := range headers {
		fmt.Fprintf(buf, "%s: %s\r\n", h.Name, h.Value)
	}
	buf.WriteString("\r\n")
	if err := body.write(buf); err != nil {
		return err
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\r\n")) {
		buf.WriteString("\r\n")
	}
	return nil
}

func buildBodyPart(msg Message, boundaries *boundaryAllocator) (encodedBodyNode, error) {
	if len(msg.Attachments) > 0 {
		body, err := buildBodyRegion(msg, boundaries)
		if err != nil {
			return encodedBodyNode{}, err
		}
		boundary, err := boundaries.Next(nil, body.boundaries)
		if err != nil {
			return encodedBodyNode{}, err
		}
		return encodedBodyNode{
			contentType: fmt.Sprintf(`multipart/mixed; boundary="%s"`, boundary),
			boundaries:  append([]string{boundary}, body.boundaries...),
			write: func(buf *limitBuffer) error {
				if err := writeMultipartPart(buf, boundary, body.contentType, body.write); err != nil {
					return err
				}
				for _, part := range msg.Attachments {
					if err := writeBinaryPart(buf, boundary, part); err != nil {
						return err
					}
				}
				fmt.Fprintf(buf, "--%s--\r\n", boundary)
				return nil
			},
		}, nil
	}
	return buildBodyRegion(msg, boundaries)
}

func buildBodyRegion(msg Message, boundaries *boundaryAllocator) (encodedBodyNode, error) {
	if len(msg.TextBody) > 0 && len(msg.HTMLBody) > 0 {
		htmlBody, err := buildHTMLRegion(msg, boundaries)
		if err != nil {
			return encodedBodyNode{}, err
		}
		boundary, err := boundaries.Next([][]byte{msg.TextBody, msg.HTMLBody}, htmlBody.boundaries)
		if err != nil {
			return encodedBodyNode{}, err
		}
		return encodedBodyNode{
			contentType: fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary),
			boundaries:  append([]string{boundary}, htmlBody.boundaries...),
			write: func(buf *limitBuffer) error {
				if err := writeMultipartPart(buf, boundary, `text/plain; charset="UTF-8"`, func(buf *limitBuffer) error {
					return writeText(buf, msg.TextBody)
				}); err != nil {
					return err
				}
				if err := writeMultipartPart(buf, boundary, htmlBody.contentType, htmlBody.write); err != nil {
					return err
				}
				fmt.Fprintf(buf, "--%s--\r\n", boundary)
				return nil
			},
		}, nil
	}
	if len(msg.HTMLBody) > 0 {
		return buildHTMLRegion(msg, boundaries)
	}
	body := msg.TextBody
	if len(body) == 0 {
		body = []byte{}
	}
	return encodedBodyNode{
		contentType: `text/plain; charset="UTF-8"`,
		write: func(buf *limitBuffer) error {
			return writeText(buf, body)
		},
	}, nil
}

func buildHTMLRegion(msg Message, boundaries *boundaryAllocator) (encodedBodyNode, error) {
	if len(msg.Related) == 0 {
		return encodedBodyNode{
			contentType: `text/html; charset="UTF-8"`,
			write: func(buf *limitBuffer) error {
				return writeText(buf, msg.HTMLBody)
			},
		}, nil
	}
	boundary, err := boundaries.Next([][]byte{msg.HTMLBody}, nil)
	if err != nil {
		return encodedBodyNode{}, err
	}
	return encodedBodyNode{
		contentType: fmt.Sprintf(`multipart/related; boundary="%s"`, boundary),
		boundaries:  []string{boundary},
		write: func(buf *limitBuffer) error {
			if err := writeMultipartPart(buf, boundary, `text/html; charset="UTF-8"`, func(buf *limitBuffer) error {
				return writeText(buf, msg.HTMLBody)
			}); err != nil {
				return err
			}
			for _, part := range msg.Related {
				if err := writeBinaryPart(buf, boundary, part); err != nil {
					return err
				}
			}
			fmt.Fprintf(buf, "--%s--\r\n", boundary)
			return nil
		},
	}, nil
}

func writeMultipartPart(buf *limitBuffer, boundary, contentType string, writeBody func(*limitBuffer) error) error {
	fmt.Fprintf(buf, "--%s\r\nContent-Type: %s\r\n", boundary, contentType)
	if strings.HasPrefix(strings.ToLower(contentType), "text/") {
		buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	}
	buf.WriteString("\r\n")
	start := len(buf.Bytes())
	if err := writeBody(buf); err != nil {
		return err
	}
	if !bytes.HasSuffix(buf.Bytes()[start:], []byte("\r\n")) {
		buf.WriteString("\r\n")
	}
	return nil
}

func writeBinaryPart(buf *limitBuffer, boundary string, part BinaryPart) error {
	if part.ContentType == "" {
		part.ContentType = "application/octet-stream"
	}
	disposition := part.Disposition
	if disposition == "" {
		disposition = "attachment"
		if part.Role == PartRoleRelated {
			disposition = "inline"
		}
	}
	fmt.Fprintf(buf, "--%s\r\nContent-Type: %s\r\n", boundary, mimeparam.FormatContentType(part.ContentType, part.Filename))
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(buf, "Content-Disposition: %s\r\n", mimeparam.FormatDisposition(disposition, part.Filename))
	if part.ContentID != "" {
		fmt.Fprintf(buf, "Content-ID: %s\r\n", part.ContentID)
	}
	if part.Description != "" {
		fmt.Fprintf(buf, "Content-Description: %s\r\n", mime.QEncoding.Encode("utf-8", part.Description))
	}
	if part.Language != "" {
		fmt.Fprintf(buf, "Content-Language: %s\r\n", part.Language)
	}
	if part.Location != "" {
		fmt.Fprintf(buf, "Content-Location: %s\r\n", part.Location)
	}
	buf.WriteString("\r\n")
	writeBase64(buf, part.Bytes)
	buf.WriteString("\r\n")
	return nil
}

func writeText(buf *limitBuffer, in []byte) error {
	writer := quotedprintable.NewWriter(buf)
	normalized := bytes.ReplaceAll(in, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\n"), []byte("\r\n"))
	if _, err := writer.Write(normalized); err != nil {
		return ErrInvalidSemanticInput
	}
	if err := writer.Close(); err != nil {
		return ErrInvalidSemanticInput
	}
	return nil
}

func writeBase64(buf *limitBuffer, in []byte) {
	var encoded [76]byte
	for len(in) > 0 {
		n := 57
		if len(in) < n {
			n = len(in)
		}
		encodedLen := base64.StdEncoding.EncodedLen(n)
		base64.StdEncoding.Encode(encoded[:encodedLen], in[:n])
		buf.Write(encoded[:encodedLen])
		in = in[n:]
		if len(in) > 0 {
			buf.WriteString("\r\n")
		}
	}
}

type boundaryAllocator struct {
	source func() (string, error)
	used   map[string]struct{}
}

func newBoundaryAllocator(source func() (string, error)) *boundaryAllocator {
	return &boundaryAllocator{source: source, used: map[string]struct{}{}}
}

func (a *boundaryAllocator) Next(bodies [][]byte, descendants []string) (string, error) {
	for i := 0; i < 32; i++ {
		b, err := a.source()
		if err != nil {
			return "", ErrSourceFailure
		}
		if !validBoundary(b) {
			continue
		}
		if _, ok := a.used[b]; ok {
			continue
		}
		if boundaryCollides(b, bodies, descendants) {
			continue
		}
		a.used[b] = struct{}{}
		return b, nil
	}
	return "", ErrSourceFailure
}

func boundaryCollides(candidate string, bodies [][]byte, descendants []string) bool {
	for _, descendant := range descendants {
		if strings.HasPrefix(candidate, descendant) || strings.HasPrefix(descendant, candidate) {
			return true
		}
	}
	prefix := []byte("--" + candidate)
	for _, body := range bodies {
		for _, line := range bytes.Split(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n")) {
			if bytes.HasPrefix(line, prefix) {
				return true
			}
		}
	}
	return false
}

func validBoundary(b string) bool {
	if len(b) == 0 || len(b) > 70 {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.ContainsRune("'()+_,-./:=?", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func checkPhysicalLines(raw []byte) error {
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		end := i
		if end > start && raw[end-1] == '\r' {
			end--
		}
		if end-start > 998 {
			return newLimitError(ErrLineLimitExceeded, LimitPhysicalLineBytes, 998)
		}
		start = i + 1
	}
	if len(raw)-start > 998 {
		return newLimitError(ErrLineLimitExceeded, LimitPhysicalLineBytes, 998)
	}
	return nil
}

func validateAddressMode(msg Message, mode AddressHeaderMode) error {
	recipients := append(append(append(append([]mail.Recipient{msg.From}, msg.To...), msg.CC...), msg.BCC...), msg.ReplyTo...)
	for _, recipient := range recipients {
		if err := validateMailboxAddress(recipient.Address, mode); err != nil {
			return ErrInvalidSemanticInput
		}
	}
	return nil
}

func validateMailboxAddress(address string, mode AddressHeaderMode) error {
	addr := strings.TrimSpace(address)
	if addr == "" || !utf8.ValidString(addr) || strings.ContainsAny(addr, "\r\n\x00") {
		return ErrInvalidSemanticInput
	}
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return ErrInvalidSemanticInput
	}
	if len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return ErrInvalidSemanticInput
	}
	if mode == AddressHeaderASCII && !isASCII(local) {
		return ErrInvalidSemanticInput
	}
	for _, r := range local {
		if r == '.' {
			continue
		}
		if r < utf8.RuneSelf {
			if !isAText(byte(r)) {
				return ErrInvalidSemanticInput
			}
			continue
		}
		if mode != AddressHeaderSMTPUTF8 || unicode.IsControl(r) || unicode.IsSpace(r) ||
			strings.ContainsRune(`"(),:;<>@[\]`, r) {
			return ErrInvalidSemanticInput
		}
	}
	asciiDomain, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return ErrInvalidSemanticInput
	}
	asciiDomain = strings.ToLower(strings.TrimSpace(asciiDomain))
	if !validMailboxDomain(asciiDomain) || len(local)+1+len(asciiDomain) > 254 {
		return ErrInvalidSemanticInput
	}
	return nil
}

func isAText(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", rune(c))
}

func validMailboxDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func formatAddress(r mail.Recipient) string {
	addr := strings.TrimSpace(r.Address)
	if strings.TrimSpace(r.Name) == "" {
		return addr
	}
	return (&netmail.Address{Name: r.Name, Address: addr}).String()
}

// WithBcc returns an encoded message with a Bcc header added. The message
// that goes out never names its blind copies; the copy kept in Sent does,
// so the sender still knows whom they sent one to.
func WithBcc(raw []byte, bcc []mail.Recipient) []byte {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if len(bcc) == 0 || end < 0 {
		return raw
	}
	header := "Bcc: " + formatAddressList("Bcc", bcc) + "\r\n"
	out := make([]byte, 0, len(raw)+len(header))
	out = append(out, raw[:end+2]...)
	out = append(out, header...)
	return append(out, raw[end+2:]...)
}

// formatAddressList writes the addresses of the header name. It starts a
// new line between two addresses when the line would get longer than 78
// characters, so a message to many people stays within the line limits of
// the mail format.
func formatAddressList(name string, in []mail.Recipient) string {
	var b strings.Builder
	line := len(name) + len(": ")
	for i, r := range in {
		addr := formatAddress(r)
		if i > 0 {
			// Keep room for the comma that ends the line when the next
			// address goes on a new one.
			if line+len(", ")+len(addr)+len(",") > 78 {
				b.WriteString(",\r\n ")
				line = 1
			} else {
				b.WriteString(", ")
				line += 2
			}
		}
		b.WriteString(addr)
		line += len(addr)
	}
	return b.String()
}

func orderedRootHeaders(headers []Header) []Header {
	out := cloneHeaders(headers)
	for i := range out {
		out[i].Name = canonicalHeaderName(out[i].Name)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return headerOrder(out[i].Name) < headerOrder(out[j].Name)
	})
	return out
}

func headerOrder(name string) int {
	switch strings.ToLower(name) {
	case "comments":
		return 0
	case "keywords":
		return 1
	case "content-language":
		return 2
	case "importance":
		return 3
	case "priority":
		return 4
	case "x-priority":
		return 5
	default:
		return 100
	}
}

func productionMessageID() (string, error) {
	return productionMessageIDWithDomain("maildo.local")
}

func productionMessageIDForSender(address string) (string, error) {
	_, domain, ok := strings.Cut(strings.TrimSpace(address), "@")
	if !ok || domain == "" {
		return "", ErrInvalidSemanticInput
	}
	asciiDomain, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", err
	}
	asciiDomain = strings.ToLower(asciiDomain)
	if !validMailboxDomain(asciiDomain) {
		return "", ErrInvalidSemanticInput
	}
	return productionMessageIDWithDomain(asciiDomain)
}

func productionMessageIDWithDomain(domain string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return mail.NormalizeMessageIDScalar(hex.EncodeToString(b[:]) + "@" + domain)
}

func productionBoundary() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "maildo-" + hex.EncodeToString(b[:]), nil
}

func projectMessage(msg Message) SemanticProjection {
	return SemanticProjection{
		From:              cloneRecipient(msg.From),
		To:                cloneRecipients(msg.To),
		CC:                cloneRecipients(msg.CC),
		BCC:               cloneRecipients(msg.BCC),
		ReplyTo:           cloneRecipients(msg.ReplyTo),
		Subject:           strings.Clone(msg.Subject),
		TextBody:          cloneBytes(msg.TextBody),
		HTMLBody:          cloneBytes(msg.HTMLBody),
		HTMLRootContentID: strings.Clone(msg.HTMLRootContentID),
		InReplyTo:         strings.Clone(msg.InReplyTo),
		References:        cloneStrings(msg.References),
		RootHeaders:       cloneHeaders(msg.RootHeaders),
		Related:           projectParts(msg.Related),
		Attachments:       projectParts(msg.Attachments),
	}
}

func projectParts(parts []BinaryPart) []PartProjection {
	if len(parts) == 0 {
		return nil
	}
	out := make([]PartProjection, len(parts))
	for i, p := range parts {
		out[i] = PartProjection{
			Role:        p.Role,
			ContentType: strings.Clone(p.ContentType),
			Disposition: strings.Clone(p.Disposition),
			ContentID:   strings.Clone(p.ContentID),
			Filename:    strings.Clone(p.Filename),
			Description: strings.Clone(p.Description),
			Language:    strings.Clone(p.Language),
			Location:    strings.Clone(p.Location),
			DecodedSize: uint64(len(p.Bytes)),
			SHA256:      sha256.Sum256(p.Bytes),
		}
	}
	return out
}

func cloneMessage(in Message) Message {
	return Message{
		From:              cloneRecipient(in.From),
		To:                cloneRecipients(in.To),
		CC:                cloneRecipients(in.CC),
		BCC:               cloneRecipients(in.BCC),
		ReplyTo:           cloneRecipients(in.ReplyTo),
		Subject:           strings.Clone(in.Subject),
		TextBody:          cloneBytes(in.TextBody),
		HTMLBody:          cloneBytes(in.HTMLBody),
		HTMLRootContentID: strings.Clone(in.HTMLRootContentID),
		InReplyTo:         strings.Clone(in.InReplyTo),
		References:        cloneStrings(in.References),
		RootHeaders:       cloneHeaders(in.RootHeaders),
		Related:           cloneParts(in.Related),
		Attachments:       cloneParts(in.Attachments),
	}
}

func cloneRecipient(in mail.Recipient) mail.Recipient {
	return mail.Recipient{Name: strings.Clone(in.Name), Address: strings.Clone(in.Address)}
}

func cloneRecipients(in []mail.Recipient) []mail.Recipient {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Recipient, len(in))
	for i, r := range in {
		out[i] = cloneRecipient(r)
	}
	return out
}

func cloneHeaders(in []Header) []Header {
	if len(in) == 0 {
		return nil
	}
	out := make([]Header, len(in))
	for i, h := range in {
		out[i] = Header{Name: strings.Clone(h.Name), Value: strings.Clone(h.Value)}
	}
	return out
}

func cloneParts(in []BinaryPart) []BinaryPart {
	if len(in) == 0 {
		return nil
	}
	out := make([]BinaryPart, len(in))
	for i, p := range in {
		out[i] = p
		out[i].ContentType = strings.Clone(p.ContentType)
		out[i].Disposition = strings.Clone(p.Disposition)
		out[i].ContentID = strings.Clone(p.ContentID)
		out[i].Filename = strings.Clone(p.Filename)
		out[i].Description = strings.Clone(p.Description)
		out[i].Language = strings.Clone(p.Language)
		out[i].Location = strings.Clone(p.Location)
		out[i].Bytes = cloneBytes(p.Bytes)
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.Clone(s)
	}
	return out
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	return append([]byte(nil), in...)
}

func destroyMessage(msg *Message) {
	zeroBytes(msg.TextBody)
	zeroBytes(msg.HTMLBody)
	for i := range msg.Related {
		zeroBytes(msg.Related[i].Bytes)
	}
	for i := range msg.Attachments {
		zeroBytes(msg.Attachments[i].Bytes)
	}
	*msg = Message{}
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// AttachmentParts turns files given with --attach into attachment parts.
func AttachmentParts(uploads []mail.AttachmentUpload) []BinaryPart {
	if len(uploads) == 0 {
		return nil
	}
	parts := make([]BinaryPart, 0, len(uploads))
	for _, u := range uploads {
		contentType := u.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		parts = append(parts, BinaryPart{
			Role:        PartRoleAttachment,
			ContentType: contentType,
			Disposition: "attachment",
			Filename:    u.Name,
			Bytes:       append([]byte(nil), u.Bytes...),
		})
	}
	return parts
}
