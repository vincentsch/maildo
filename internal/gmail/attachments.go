package gmail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

type gmailAttachmentRepresentation uint8

const (
	gmailAttachmentInline gmailAttachmentRepresentation = iota + 1
	gmailAttachmentImplicitZero
	gmailAttachmentExternal
)

type gmailAttachmentSnapshot struct {
	messageID   string
	ordered     []mail.Attachment
	attachments map[string]gmailAttachmentSnapshotPart
}

type gmailAttachmentSnapshotPart struct {
	attachment     mail.Attachment
	representation gmailAttachmentRepresentation
	decodedKnown   bool
	decodedSize    uint64
	inlineDigest   [sha256.Size]byte
	inlineEncoded  string
	externalID     string
	childIndexPath []uint16
	disposition    string
}

type attachmentJSONStats struct {
	totalBytes         uint64
	aggregateDataBytes uint64
	selectedDataBytes  uint64
}

type attachmentJSONParser struct {
	account           string
	dec               *json.Decoder
	stats             *attachmentJSONStats
	selectedID        string
	snapshotMessageID string
}

type bodyDataSelection uint8

const (
	bodyDataUnselected bodyDataSelection = iota
	bodyDataSelected
	bodyDataSelectionUnknown
)

type pendingBodyDataLimit struct {
	aggregateObserved uint64
	selectedObserved  uint64
}

var errStopBodyDataScan = errors.New("body data limit observed")

func (c *Client) ListMessageAttachments(ctx context.Context, req mail.MessageAttachmentListRequest) (mail.MessageAttachmentList, error) {
	msg, stats, err := c.fetchAttachmentStructure(ctx, req.MessageID, "")
	if err != nil {
		return mail.MessageAttachmentList{}, err
	}
	if err := attachmentStructureBudgetError(c.account, stats, "", ""); err != nil {
		return mail.MessageAttachmentList{}, err
	}
	normalized, state, err := c.normalizePayloadWithState(msg, true)
	if err != nil {
		return mail.MessageAttachmentList{}, err
	}
	snapshot, _, err := c.attachmentSnapshotFromState(msg.ID, state, "")
	if err != nil {
		return mail.MessageAttachmentList{}, err
	}
	c.mu.Lock()
	c.attachments = snapshot
	c.mu.Unlock()
	return mail.MessageAttachmentList{
		MessageID:         msg.ID,
		InternetMessageID: normalized.internetMessageID,
		Attachments:       cloneGmailAttachments(snapshot.ordered),
		ProviderMeta:      gmailAttachmentMeta(mail.MetadataAttachmentList, msg.ID),
	}, nil
}

func (c *Client) DownloadMessageAttachment(ctx context.Context, req mail.MessageAttachmentDownloadRequest) (mail.MessageAttachmentContent, error) {
	snapPart, ok := c.snapshotAttachment(req.MessageID, req.AttachmentID)
	if !ok {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentStaleSnapshot(c.account, req.MessageID, req.AttachmentID)
	}
	msg, stats, err := c.fetchAttachmentStructure(ctx, req.MessageID, req.AttachmentID)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	_, state, err := c.normalizePayloadWithState(msg, true)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	if err := attachmentStructureBudgetError(c.account, stats, req.MessageID, req.AttachmentID); err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	current, selected, err := c.attachmentSnapshotFromState(msg.ID, state, req.AttachmentID)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	currentPart, ok := current.attachments[req.AttachmentID]
	if !ok || selected == nil || !sameAttachmentSnapshotPart(snapPart, currentPart) {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentStaleSnapshot(c.account, req.MessageID, req.AttachmentID)
	}
	switch currentPart.representation {
	case gmailAttachmentInline, gmailAttachmentImplicitZero:
		encoded := ""
		if selected.part.Body.Data != nil {
			encoded = *selected.part.Body.Data
		}
		return c.inlineAttachmentContent(req.MessageID, req.AttachmentID, currentPart, encoded)
	case gmailAttachmentExternal:
		if currentPart.decodedKnown && currentPart.decodedSize > uint64(mail.MaxMessageAttachmentDownloadBytes) {
			return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentTooLarge(c.account, req.MessageID, req.AttachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), currentPart.decodedSize)
		}
		return c.externalAttachmentContent(ctx, req.MessageID, req.AttachmentID, currentPart)
	default:
		return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_protocol")
	}
}

func (c *Client) snapshotAttachment(messageID, attachmentID string) (gmailAttachmentSnapshotPart, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attachments == nil || c.attachments.messageID != messageID {
		return gmailAttachmentSnapshotPart{}, false
	}
	part, ok := c.attachments.attachments[attachmentID]
	return part, ok
}

func (c *Client) fetchAttachmentStructure(ctx context.Context, messageID, selectedID string) (*gmailMessage, attachmentJSONStats, error) {
	if messageID == "" {
		return nil, attachmentJSONStats{}, providerDataError(c.account, "invalid_protocol")
	}
	values := url.Values{}
	values.Set("format", "full")
	values.Set("fields", "id,threadId,payload")
	path := "/messages/" + url.PathEscape(messageID)
	body, err := c.gmailGETLimitedReader(ctx, path, path, values, func(status int, statusBody []byte) error {
		return classifyHTTPStatus(c.account, path, status, statusBody, false)
	})
	if err != nil {
		var product *errs.Error
		if errors.As(err, &product) && product.Code() == errs.CodeMessageNotFound {
			return nil, attachmentJSONStats{}, messageNotFound(c.account, mail.ResolvedSelectorID)
		}
		return nil, attachmentJSONStats{}, err
	}
	defer body.Close()
	limited := newAttachmentLimitReader(body, limits.GmailMaxAttachmentStructureResponseBytes)
	msg, stats, err := parseAttachmentStructureJSON(c.account, limited, selectedID, messageID)
	stats.totalBytes = limited.observed()
	if err != nil && attachmentParserErrorPrecedesCompleteLimit(err) {
		return nil, attachmentJSONStats{}, err
	}
	if limited.over() {
		return nil, attachmentJSONStats{}, errs.NewGmailAttachmentStructureReadLimitExceeded(c.account, uint64(limits.GmailMaxAttachmentStructureResponseBytes), limited.observed())
	}
	if err != nil {
		return nil, attachmentJSONStats{}, err
	}
	if msg.ID == "" || msg.ID != messageID || msg.ThreadID == "" || msg.Payload == nil {
		return nil, attachmentJSONStats{}, providerDataError(c.account, "invalid_protocol")
	}
	return msg, stats, nil
}

func (c *Client) gmailGETLimitedReader(ctx context.Context, path, operation string, values url.Values, classify func(int, []byte) error) (io.ReadCloser, error) {
	if operation == "" {
		operation = path
	}
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return nil, err
	}
	target, err := c.buildURL(path, values)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, providerAPIError(c.account, operation)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := doNoRedirect(c.http, req)
	if err != nil {
		return nil, providerConnectionError(c.account, operation)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusBody, _, _, _ := readAllLimited(resp.Body, limits.GmailMaxErrorResponseBytes)
		_ = resp.Body.Close()
		return nil, classify(resp.StatusCode, statusBody)
	}
	return resp.Body, nil
}

type attachmentLimitReader struct {
	r     io.Reader
	limit int64
	n     uint64
}

func newAttachmentLimitReader(r io.Reader, limit int64) *attachmentLimitReader {
	if limit <= 0 {
		limit = limits.GmailMaxJSONResponseBytes
	}
	return &attachmentLimitReader{r: io.LimitReader(r, limit+1), limit: limit}
}

func (r *attachmentLimitReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += uint64(n)
	return n, err
}

func (r *attachmentLimitReader) observed() uint64 {
	if r == nil {
		return 0
	}
	return r.n
}

func (r *attachmentLimitReader) over() bool {
	return r != nil && r.n > uint64(r.limit)
}

func readAllLimited(r io.Reader, limit int64) ([]byte, uint64, bool, error) {
	if limit <= 0 {
		limit = limits.GmailMaxJSONResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, 0, false, err
	}
	observed := uint64(len(data))
	if int64(len(data)) > limit {
		return nil, observed, true, nil
	}
	return data, observed, false, nil
}

func attachmentParserErrorPrecedesCompleteLimit(err error) bool {
	var product *errs.Error
	if !errors.As(err, &product) {
		return true
	}
	switch product.Code() {
	case errs.CodeAttachmentTooLarge, errs.CodeReadLimitExceeded:
		return true
	case errs.CodeProviderDataError:
		reason, _ := product.Details()["reason"].(string)
		return reason != "invalid_json"
	default:
		return true
	}
}

func parseAttachmentStructureJSON(account string, r io.Reader, selectedID, snapshotMessageID string) (*gmailMessage, attachmentJSONStats, error) {
	stats := attachmentJSONStats{}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	parser := &attachmentJSONParser{
		account:           account,
		dec:               dec,
		stats:             &stats,
		selectedID:        selectedID,
		snapshotMessageID: snapshotMessageID,
	}
	msg, err := parser.parseMessage()
	if err != nil {
		return nil, stats, err
	}
	if tok, err := dec.Token(); err != io.EOF || tok != nil {
		return nil, stats, providerDataError(account, "invalid_json")
	}
	return msg, stats, nil
}

func (p *attachmentJSONParser) token() (json.Token, error) {
	tok, err := p.dec.Token()
	if err != nil {
		return nil, providerDataError(p.account, "invalid_json")
	}
	return tok, nil
}

func (p *attachmentJSONParser) objectStart() error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return providerDataError(p.account, "invalid_json")
	}
	return p.checkNonDataBudget()
}

func (p *attachmentJSONParser) endObject() error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '}' {
		return providerDataError(p.account, "invalid_json")
	}
	return p.checkNonDataBudget()
}

func (p *attachmentJSONParser) endArray() error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != ']' {
		return providerDataError(p.account, "invalid_json")
	}
	return p.checkNonDataBudget()
}

func (p *attachmentJSONParser) objectKey(seen map[string]bool) (string, error) {
	tok, err := p.token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", providerDataError(p.account, "invalid_json")
	}
	if seen[key] {
		return "", providerDataError(p.account, "invalid_json")
	}
	seen[key] = true
	return key, p.checkNonDataBudget()
}

func (p *attachmentJSONParser) readString() (string, error) {
	s, err := p.readStringUnchecked()
	if err != nil {
		return "", err
	}
	if err := p.checkNonDataBudget(); err != nil {
		return "", err
	}
	return s, nil
}

func (p *attachmentJSONParser) readStringUnchecked() (string, error) {
	tok, err := p.token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", providerDataError(p.account, "invalid_json")
	}
	return s, nil
}

func (p *attachmentJSONParser) readNumber() (int64, error) {
	tok, err := p.token()
	if err != nil {
		return 0, err
	}
	if err := p.checkNonDataBudget(); err != nil {
		return 0, err
	}
	return numberFieldValue(p.account, tok)
}

func (p *attachmentJSONParser) checkNonDataBudget() error {
	if p == nil || p.stats == nil {
		return nil
	}
	observed := uint64(p.dec.InputOffset())
	nonDataBytes := observed
	if p.stats.aggregateDataBytes <= observed {
		nonDataBytes = observed - p.stats.aggregateDataBytes
	}
	if nonDataBytes > uint64(limits.GmailMaxJSONResponseBytes) {
		return errs.NewGmailAttachmentStructureReadLimitExceeded(p.account, uint64(limits.GmailMaxJSONResponseBytes), nonDataBytes)
	}
	return nil
}

func (p *attachmentJSONParser) parseMessage() (*gmailMessage, error) {
	if err := p.objectStart(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var msg gmailMessage
	var haveID, haveThread, havePayload bool
	for p.dec.More() {
		key, err := p.objectKey(seen)
		if err != nil {
			return nil, err
		}
		switch key {
		case "id":
			msg.ID, err = p.readString()
			haveID = true
		case "threadId":
			msg.ThreadID, err = p.readString()
			haveThread = true
		case "labelIds":
			msg.LabelIDs, err = p.parseStringArray()
		case "payload":
			msg.Payload, err = p.parsePart()
			havePayload = true
		default:
			err = p.skipValue()
		}
		if err != nil {
			return nil, err
		}
	}
	if err := p.endObject(); err != nil {
		return nil, err
	}
	if !haveID || !haveThread || !havePayload {
		return nil, providerDataError(p.account, "invalid_protocol")
	}
	return &msg, nil
}

func (p *attachmentJSONParser) parseStringArray() ([]string, error) {
	tok, err := p.token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		return nil, providerDataError(p.account, "invalid_json")
	}
	var out []string
	for p.dec.More() {
		value, err := p.readString()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if err := p.endArray(); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *attachmentJSONParser) parsePart() (*gmailPart, error) {
	if err := p.objectStart(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var part gmailPart
	var pendingLimit *pendingBodyDataLimit
	for p.dec.More() {
		key, err := p.objectKey(seen)
		if err != nil {
			return nil, err
		}
		switch key {
		case "partId":
			part.PartID, err = p.readString()
		case "mimeType":
			part.MimeType, err = p.readString()
		case "filename":
			part.Filename, err = p.readString()
		case "headers":
			part.Headers, err = p.parseHeaders()
		case "body":
			selection := bodyDataUnselected
			if p.selectedID != "" {
				if part.PartID == p.selectedID {
					selection = bodyDataSelected
				} else if part.PartID == "" {
					selection = bodyDataSelectionUnknown
				}
			}
			part.Body, pendingLimit, err = p.parseBody(selection)
		case "parts":
			part.Parts, err = p.parseParts()
		default:
			err = p.skipValue()
		}
		if err != nil {
			return nil, err
		}
	}
	if err := p.endObject(); err != nil {
		return nil, err
	}
	if pendingLimit != nil {
		if p.selectedID != "" && part.PartID == p.selectedID && pendingLimit.selectedObserved > uint64(limits.GmailMaxAttachmentEncodedBytes) {
			return nil, errs.NewGmailAttachmentTooLarge(p.account, p.snapshotMessageID, p.selectedID, errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), pendingLimit.selectedObserved)
		}
		return nil, errs.NewGmailAttachmentStructureReadLimitExceeded(p.account, uint64(limits.GmailMaxAttachmentEncodedBytes), pendingLimit.aggregateObserved)
	}
	if p.selectedID != "" && part.PartID == p.selectedID && part.Body.Data != nil {
		p.stats.selectedDataBytes = uint64(len(*part.Body.Data))
	}
	return &part, nil
}

func (p *attachmentJSONParser) parseHeaders() ([]gmailHeader, error) {
	tok, err := p.token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		return nil, providerDataError(p.account, "invalid_json")
	}
	var headers []gmailHeader
	for p.dec.More() {
		header, err := p.parseHeader()
		if err != nil {
			return nil, err
		}
		headers = append(headers, header)
	}
	if err := p.endArray(); err != nil {
		return nil, err
	}
	return headers, nil
}

func (p *attachmentJSONParser) parseHeader() (gmailHeader, error) {
	if err := p.objectStart(); err != nil {
		return gmailHeader{}, err
	}
	seen := map[string]bool{}
	var header gmailHeader
	var haveName, haveValue bool
	for p.dec.More() {
		key, err := p.objectKey(seen)
		if err != nil {
			return gmailHeader{}, err
		}
		switch key {
		case "name":
			header.Name, err = p.readString()
			haveName = true
		case "value":
			header.Value, err = p.readString()
			haveValue = true
		default:
			err = p.skipValue()
		}
		if err != nil {
			return gmailHeader{}, err
		}
	}
	if err := p.endObject(); err != nil {
		return gmailHeader{}, err
	}
	if !haveName || !haveValue {
		return gmailHeader{}, providerDataError(p.account, "invalid_protocol")
	}
	return header, nil
}

func (p *attachmentJSONParser) parseParts() ([]gmailPart, error) {
	tok, err := p.token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		return nil, providerDataError(p.account, "invalid_json")
	}
	var parts []gmailPart
	for p.dec.More() {
		part, err := p.parsePart()
		if err != nil {
			return nil, err
		}
		parts = append(parts, *part)
	}
	if err := p.endArray(); err != nil {
		return nil, err
	}
	return parts, nil
}

func (p *attachmentJSONParser) parseBody(selection bodyDataSelection) (gmailBody, *pendingBodyDataLimit, error) {
	tok, err := p.token()
	if err != nil {
		return gmailBody{}, nil, err
	}
	if tok == nil {
		return gmailBody{}, nil, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return gmailBody{}, nil, providerDataError(p.account, "invalid_json")
	}
	seen := map[string]bool{}
	var body gmailBody
	var pendingLimit *pendingBodyDataLimit
	for p.dec.More() {
		key, err := p.objectKey(seen)
		if err != nil {
			return gmailBody{}, nil, err
		}
		switch key {
		case "attachmentId":
			s, err := p.readString()
			if err != nil {
				return gmailBody{}, nil, err
			}
			body.AttachmentID = &s
		case "data":
			var pending *pendingBodyDataLimit
			s, pending, err := p.readBodyDataString(selection)
			if err != nil {
				return gmailBody{}, nil, err
			}
			if pending != nil {
				pendingLimit = pending
			}
			body.Data = &s
		case "size":
			n, err := p.readNumber()
			if err != nil {
				return gmailBody{}, nil, err
			}
			body.Size = &n
		default:
			if err := p.skipValue(); err != nil {
				return gmailBody{}, nil, err
			}
		}
	}
	if err := p.endObject(); err != nil {
		return gmailBody{}, nil, err
	}
	return body, pendingLimit, nil
}

func (p *attachmentJSONParser) readBodyDataString(selection bodyDataSelection) (string, *pendingBodyDataLimit, error) {
	value, err := p.readStringUnchecked()
	if err != nil {
		return "", nil, err
	}
	pending, err := p.recordBodyData(value, selection)
	if err != nil {
		return "", nil, err
	}
	if pending == nil {
		if err := p.checkNonDataBudget(); err != nil {
			return "", nil, err
		}
	}
	return value, pending, nil
}

func numberFieldValue(account string, value any) (int64, error) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, providerDataError(account, "invalid_json")
	}
	raw := n.String()
	if strings.ContainsAny(raw, ".eE") {
		return 0, providerDataError(account, "invalid_json")
	}
	out, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, providerDataError(account, "invalid_json")
	}
	return out, nil
}

func (p *attachmentJSONParser) recordBodyData(value string, selection bodyDataSelection) (*pendingBodyDataLimit, error) {
	start := p.stats.aggregateDataBytes
	var pending *pendingBodyDataLimit
	err := scanBase64URLValue(p.account, value, func(observedSelected uint64) error {
		observedAggregate := start + observedSelected
		if observedAggregate > uint64(limits.GmailMaxAttachmentEncodedBytes) {
			p.stats.aggregateDataBytes = observedAggregate
			switch selection {
			case bodyDataSelected:
				return errs.NewGmailAttachmentTooLarge(p.account, p.snapshotMessageID, p.selectedID, errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), observedSelected)
			case bodyDataSelectionUnknown:
				pending = &pendingBodyDataLimit{aggregateObserved: observedAggregate, selectedObserved: observedSelected}
				return errStopBodyDataScan
			default:
				return errs.NewGmailAttachmentStructureReadLimitExceeded(p.account, uint64(limits.GmailMaxAttachmentEncodedBytes), observedAggregate)
			}
		}
		return nil
	})
	if errors.Is(err, errStopBodyDataScan) {
		return pending, nil
	}
	if err != nil {
		return nil, err
	}
	p.stats.aggregateDataBytes += uint64(len(value))
	if selection == bodyDataSelected {
		p.stats.selectedDataBytes = uint64(len(value))
	}
	if _, _, err := decodedAttachmentFacts(p.account, value); err != nil {
		return nil, err
	}
	return nil, nil
}

func validateSelectedAttachmentData(account, snapshotMessageID, attachmentID, value string) error {
	if err := scanBase64URLValue(account, value, func(observed uint64) error {
		if observed > uint64(limits.GmailMaxAttachmentEncodedBytes) {
			return errs.NewGmailAttachmentTooLarge(account, snapshotMessageID, attachmentID, errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), observed)
		}
		return nil
	}); err != nil {
		return err
	}
	if _, _, err := decodedAttachmentFacts(account, value); err != nil {
		return err
	}
	return nil
}

func scanBase64URLValue(account, value string, observe func(uint64) error) error {
	padding := 0
	paddingStarted := false
	for i := 0; i < len(value); i++ {
		b := value[i]
		if !base64URLByte(b) {
			return providerDataError(account, "invalid_base64")
		}
		if b == '=' {
			padding++
			paddingStarted = true
			if padding > 2 {
				return providerDataError(account, "invalid_base64")
			}
		} else if paddingStarted {
			return providerDataError(account, "invalid_base64")
		}
		if observe != nil {
			if err := observe(uint64(i + 1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func base64URLByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '-' ||
		b == '_' ||
		b == '='
}

func (p *attachmentJSONParser) skipValue() error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	switch d := tok.(type) {
	case json.Delim:
		if err := p.checkNonDataBudget(); err != nil {
			return err
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for p.dec.More() {
				if _, err := p.objectKey(seen); err != nil {
					return err
				}
				if err := p.skipValue(); err != nil {
					return err
				}
			}
			return p.endObject()
		case '[':
			for p.dec.More() {
				if err := p.skipValue(); err != nil {
					return err
				}
			}
			return p.endArray()
		default:
			return providerDataError(p.account, "invalid_json")
		}
	case string, json.Number, bool, nil:
		return p.checkNonDataBudget()
	default:
		return providerDataError(p.account, "invalid_json")
	}
}

func attachmentStructureBudgetError(account string, stats attachmentJSONStats, snapshotMessageID, selectedID string) error {
	nonDataBytes := stats.totalBytes
	if stats.aggregateDataBytes <= stats.totalBytes {
		nonDataBytes = stats.totalBytes - stats.aggregateDataBytes
	}
	if nonDataBytes > uint64(limits.GmailMaxJSONResponseBytes) {
		return errs.NewGmailAttachmentStructureReadLimitExceeded(account, uint64(limits.GmailMaxJSONResponseBytes), nonDataBytes)
	}
	if stats.aggregateDataBytes > uint64(limits.GmailMaxAttachmentEncodedBytes) {
		if selectedID != "" && stats.selectedDataBytes > uint64(limits.GmailMaxAttachmentEncodedBytes) {
			return errs.NewGmailAttachmentTooLarge(account, snapshotMessageID, selectedID, errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), stats.selectedDataBytes)
		}
		return errs.NewGmailAttachmentStructureReadLimitExceeded(account, uint64(limits.GmailMaxAttachmentEncodedBytes), stats.aggregateDataBytes)
	}
	return nil
}

func (c *Client) attachmentSnapshotFromState(messageID string, state *mimeWalkState, selectedID string) (*gmailAttachmentSnapshot, *classifiedMimeAttachment, error) {
	if state == nil {
		return nil, nil, providerDataError(c.account, "invalid_protocol")
	}
	snapshot := &gmailAttachmentSnapshot{
		messageID:   messageID,
		ordered:     make([]mail.Attachment, 0, len(state.classifiedParts)),
		attachments: make(map[string]gmailAttachmentSnapshotPart, len(state.classifiedParts)),
	}
	var selected *classifiedMimeAttachment
	for i := range state.classifiedParts {
		item := state.classifiedParts[i]
		part, err := c.snapshotPartFromClassified(messageID, item)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := snapshot.attachments[item.attachment.ID]; exists {
			return nil, nil, providerDataError(c.account, "invalid_protocol")
		}
		snapshot.attachments[item.attachment.ID] = part
		snapshot.ordered = append(snapshot.ordered, cloneGmailAttachment(item.attachment))
		if item.attachment.ID == selectedID {
			selected = &state.classifiedParts[i]
		}
	}
	return snapshot, selected, nil
}

func (c *Client) snapshotPartFromClassified(messageID string, item classifiedMimeAttachment) (gmailAttachmentSnapshotPart, error) {
	part := item.part
	if part == nil {
		return gmailAttachmentSnapshotPart{}, providerDataError(c.account, "invalid_protocol")
	}
	rep := gmailAttachmentRepresentationFor(part)
	out := gmailAttachmentSnapshotPart{
		attachment:     cloneGmailAttachment(item.attachment),
		representation: rep,
		decodedKnown:   item.attachment.SizeFacts.DecodedSizeKnown,
		decodedSize:    item.attachment.SizeFacts.DecodedSize,
		childIndexPath: append([]uint16(nil), item.childIndexPath...),
		disposition:    attachmentDisposition(item.attachment),
	}
	switch rep {
	case gmailAttachmentInline:
		decodedSize, digest, err := decodedAttachmentFacts(c.account, *part.Body.Data)
		if err != nil {
			return gmailAttachmentSnapshotPart{}, err
		}
		if out.decodedKnown && out.decodedSize != decodedSize {
			return gmailAttachmentSnapshotPart{}, providerDataError(c.account, "invalid_protocol")
		}
		out.decodedKnown = true
		out.decodedSize = decodedSize
		out.inlineDigest = digest
		out.inlineEncoded = *part.Body.Data
	case gmailAttachmentImplicitZero:
		out.decodedKnown = true
		out.decodedSize = 0
		out.inlineDigest = sha256.Sum256(nil)
	case gmailAttachmentExternal:
		out.externalID = *part.Body.AttachmentID
	default:
		return gmailAttachmentSnapshotPart{}, providerDataError(c.account, "invalid_protocol")
	}
	if out.attachment.ProviderMeta == nil || out.attachment.ProviderMeta.SnapshotMessageID != messageID {
		return gmailAttachmentSnapshotPart{}, providerDataError(c.account, "invalid_protocol")
	}
	return out, nil
}

func gmailAttachmentRepresentationFor(part *gmailPart) gmailAttachmentRepresentation {
	if part == nil {
		return 0
	}
	if part.Body.AttachmentID != nil && *part.Body.AttachmentID != "" && (part.Body.Data == nil || *part.Body.Data == "") {
		return gmailAttachmentExternal
	}
	if part.Body.Data != nil {
		return gmailAttachmentInline
	}
	if part.Body.Size != nil && *part.Body.Size == 0 {
		return gmailAttachmentImplicitZero
	}
	return 0
}

// sameAttachmentSnapshotPart leaves out externalID on purpose: Gmail hands out
// a new attachmentId each time it returns the message, so it never matches.
func sameAttachmentSnapshotPart(a, b gmailAttachmentSnapshotPart) bool {
	if a.representation != b.representation ||
		a.decodedKnown != b.decodedKnown ||
		a.decodedSize != b.decodedSize ||
		a.inlineDigest != b.inlineDigest ||
		a.disposition != b.disposition ||
		!equalUint16Slices(a.childIndexPath, b.childIndexPath) {
		return false
	}
	aa, bb := a.attachment, b.attachment
	return aa.ID == bb.ID &&
		aa.Name == bb.Name &&
		aa.ContentType == bb.ContentType &&
		aa.Size == bb.Size &&
		aa.IsInline == bb.IsInline &&
		aa.ContentID == bb.ContentID &&
		aa.Kind == bb.Kind &&
		aa.ProviderType == bb.ProviderType &&
		aa.SizeFactsPresent == bb.SizeFactsPresent &&
		aa.SizeFacts == bb.SizeFacts &&
		gmailExternalPresence(aa.ProviderMeta) == gmailExternalPresence(bb.ProviderMeta)
}

func gmailExternalPresence(meta *mail.AttachmentProviderMetadata) bool {
	return meta != nil && meta.ExternalAttachmentIDPresent != nil && *meta.ExternalAttachmentIDPresent
}

func (c *Client) inlineAttachmentContent(messageID, attachmentID string, part gmailAttachmentSnapshotPart, encoded string) (mail.MessageAttachmentContent, error) {
	if uint64(len(encoded)) > uint64(limits.GmailMaxAttachmentEncodedBytes) {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), uint64(len(encoded)))
	}
	decodedSize, digest, err := decodedAttachmentFacts(c.account, encoded)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	if decodedSize > uint64(mail.MaxMessageAttachmentDownloadBytes) {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), decodedSize)
	}
	if part.decodedKnown && part.decodedSize != decodedSize {
		return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_protocol")
	}
	if digest != part.inlineDigest {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentStaleSnapshot(c.account, messageID, attachmentID)
	}
	stream, err := c.newEncodedAttachmentStream(encoded)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	return mail.MessageAttachmentContent{
		AttachmentID: attachmentID,
		Content:      stream,
		ContentType:  part.attachment.ContentType,
		DeclaredSize: int64(decodedSize),
	}, nil
}

func (c *Client) externalAttachmentContent(ctx context.Context, messageID, attachmentID string, part gmailAttachmentSnapshotPart) (mail.MessageAttachmentContent, error) {
	values := url.Values{"fields": {"size,data"}}
	path := "/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(part.externalID)
	operation := "/messages/" + url.PathEscape(messageID) + "/attachments/{attachment}"
	body, err := c.gmailGETLimitedReader(ctx, path, operation, values, func(status int, statusBody []byte) error {
		if status == http.StatusNotFound || status == http.StatusGone {
			return providerDataError(c.account, "invalid_protocol")
		}
		return classifyHTTPStatus(c.account, operation, status, statusBody, false)
	})
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	defer body.Close()
	limited := newAttachmentLimitReader(body, limits.GmailMaxAttachmentContentResponseBytes)
	encoded, declared, err := parseExternalAttachmentJSON(c.account, limited, messageID, attachmentID)
	if err != nil && attachmentParserErrorPrecedesCompleteLimit(err) {
		return mail.MessageAttachmentContent{}, err
	}
	if err != nil {
		if limited.over() {
			return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_json")
		}
		return mail.MessageAttachmentContent{}, err
	}
	if limited.over() {
		return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_json")
	}
	decodedSize, _, err := decodedAttachmentFacts(c.account, encoded)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	if decodedSize > uint64(mail.MaxMessageAttachmentDownloadBytes) {
		return mail.MessageAttachmentContent{}, errs.NewGmailAttachmentTooLarge(c.account, messageID, attachmentID, errs.GmailAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), decodedSize)
	}
	if uint64(declared) != decodedSize {
		return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_protocol")
	}
	if part.decodedKnown && part.decodedSize != decodedSize {
		return mail.MessageAttachmentContent{}, providerDataError(c.account, "invalid_protocol")
	}
	stream, err := c.newEncodedAttachmentStream(encoded)
	if err != nil {
		return mail.MessageAttachmentContent{}, err
	}
	return mail.MessageAttachmentContent{
		AttachmentID: attachmentID,
		Content:      stream,
		ContentType:  part.attachment.ContentType,
		DeclaredSize: int64(decodedSize),
	}, nil
}

func parseExternalAttachmentJSON(account string, r io.Reader, snapshotMessageID, attachmentID string) (string, int64, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	parser := &attachmentJSONParser{account: account, dec: dec}
	if err := parser.objectStart(); err != nil {
		return "", 0, err
	}
	seen := map[string]bool{}
	var encoded string
	var declared int64
	var haveSize, haveData bool
	for dec.More() {
		key, err := parser.objectKey(seen)
		if err != nil {
			return "", 0, err
		}
		switch key {
		case "size":
			declared, err = parser.readNumber()
			haveSize = true
		case "data":
			encoded, err = parser.readSelectedAttachmentDataString(snapshotMessageID, attachmentID)
			haveData = true
		default:
			return "", 0, providerDataError(account, "invalid_protocol")
		}
		if err != nil {
			return "", 0, err
		}
	}
	if err := parser.endObject(); err != nil {
		return "", 0, err
	}
	if tok, err := dec.Token(); err != io.EOF || tok != nil {
		return "", 0, providerDataError(account, "invalid_json")
	}
	if !haveSize {
		return "", 0, providerDataError(account, "invalid_protocol")
	}
	if declared < 0 {
		return "", 0, providerDataError(account, "invalid_json")
	}
	if !haveData {
		if declared == 0 {
			return "", 0, nil
		}
		return "", 0, providerDataError(account, "invalid_protocol")
	}
	return encoded, declared, nil
}

func (p *attachmentJSONParser) readSelectedAttachmentDataString(snapshotMessageID, attachmentID string) (string, error) {
	value, err := p.readString()
	if err != nil {
		return "", err
	}
	if err := validateSelectedAttachmentData(p.account, snapshotMessageID, attachmentID, value); err != nil {
		return "", err
	}
	return value, nil
}

func decodedAttachmentFacts(account, encoded string) (uint64, [sha256.Size]byte, error) {
	decoder, err := newBase64URLDecoder(account, encoded)
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, decoder)
	if err != nil {
		return 0, [sha256.Size]byte{}, providerDataError(account, "invalid_base64")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return uint64(n), digest, nil
}

func (c *Client) newEncodedAttachmentStream(encoded string) (io.ReadCloser, error) {
	if encoded == "" {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	encoding, err := base64URLEncoding(c.account, encoded)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "maildo-gmail-attachment-*")
	if err != nil {
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not write a temporary file for the attachment download")
	}
	path := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not write a temporary file for the attachment download")
	}
	if _, err := io.WriteString(f, encoded); err != nil {
		cleanup()
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not write a temporary file for the attachment download")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, rungrad.NewError(rungrad.ExitAPI, "could not write a temporary file for the attachment download")
	}
	stream := &encodedAttachmentStream{
		owner:   c,
		file:    f,
		path:    path,
		decoder: base64.NewDecoder(encoding, f),
	}
	if !c.registerAttachmentStream(stream) {
		stream.destroy()
		return nil, providerConnectionError(c.account, "/messages/{message}/attachments/{attachment}")
	}
	return stream, nil
}

type encodedAttachmentStream struct {
	owner   *Client
	mu      sync.Mutex
	file    *os.File
	path    string
	decoder io.Reader
	err     error
	closed  bool
}

func (s *encodedAttachmentStream) Read(p []byte) (int, error) {
	if s == nil || s.decoder == nil {
		return 0, io.ErrClosedPipe
	}
	return s.decoder.Read(p)
}

func (s *encodedAttachmentStream) Close() error {
	if s == nil {
		return nil
	}
	return s.close()
}

func (s *encodedAttachmentStream) close() error {
	s.mu.Lock()
	if s.closed {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			s.err = rungrad.NewError(rungrad.ExitAPI, "could not remove the temporary file of the attachment download")
			err := s.err
			s.mu.Unlock()
			return err
		}
		s.file = nil
	}
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			if retry := os.Remove(s.path); retry != nil && !errors.Is(retry, os.ErrNotExist) {
				s.err = rungrad.NewError(rungrad.ExitAPI, "could not remove the temporary file of the attachment download")
				err := s.err
				s.mu.Unlock()
				return err
			}
		}
		s.path = ""
	}
	s.closed = true
	owner := s.owner
	s.owner = nil
	s.mu.Unlock()
	if owner != nil {
		owner.unregisterAttachmentStream(s)
	}
	return nil
}

func (s *encodedAttachmentStream) destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			s.mu.Unlock()
			return
		}
		s.file = nil
	}
	if s.path != "" {
		if err := os.Remove(s.path); err == nil || errors.Is(err, os.ErrNotExist) {
			s.path = ""
			s.closed = true
		}
	}
	s.mu.Unlock()
}

func (c *Client) registerAttachmentStream(stream *encodedAttachmentStream) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed.Load() {
		return false
	}
	if c.streams == nil {
		c.streams = map[*encodedAttachmentStream]struct{}{}
	}
	c.streams[stream] = struct{}{}
	return true
}

func (c *Client) unregisterAttachmentStream(stream *encodedAttachmentStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams != nil {
		delete(c.streams, stream)
	}
}

func gmailAttachmentMeta(use mail.ProviderMetadataUse, messageID string) *mail.AttachmentProviderMetadata {
	meta, err := mail.NewProviderMetadata(errs.ProviderGmail, use, mail.AttachmentProviderMetadata{
		Source:            mail.MetadataSourceGmailAPI,
		SnapshotMessageID: messageID,
	})
	if err != nil {
		panic(err)
	}
	return meta
}

func cloneGmailAttachments(in []mail.Attachment) []mail.Attachment {
	out := make([]mail.Attachment, len(in))
	for i, att := range in {
		out[i] = cloneGmailAttachment(att)
	}
	return out
}

func cloneGmailAttachment(att mail.Attachment) mail.Attachment {
	out := att
	out.ExecutionToken = nil
	out.ProviderMeta, _ = mail.CloneProviderMetadata(att.ProviderMeta)
	return out
}

func equalUint16Slices(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
