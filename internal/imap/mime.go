package imap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimeparam"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/charmap"
)

// MIME helpers for messages read over IMAP.
//
// Real mailboxes contain broken mail: bad base64, unknown charsets, header
// lines without a colon. Reading must never fail because of that. These
// helpers decode as much as they can and say when something was lost, so
// callers can show the message with a warning instead of an error.

var (
	errMIMEPartDepthExceeded = errors.New("mime part depth exceeded")
	errMIMEPartCountExceeded = errors.New("mime part count exceeded")
)

// imapBodyPart is one node of a message's BODYSTRUCTURE, with the IMAP part
// path ("1.2" is [1 2]) needed to fetch it.
type imapBodyPart struct {
	path        []uint32
	mediaType   string
	params      map[string]string
	disposition string
	dispParams  map[string]string
	filename    string
	contentID   string
	encoding    string
	encodedSize uint64
	children    []*imapBodyPart
}

// buildBodyTree converts the server's BODYSTRUCTURE into a tree with part
// paths. Very deep or very large structures are refused, to limit the work.
func buildBodyTree(structure goimap.BodyStructure) (*imapBodyPart, error) {
	if structure == nil {
		return nil, nil
	}
	count := 0
	var walk func(goimap.BodyStructure, []uint32, int) (*imapBodyPart, error)
	walk = func(part goimap.BodyStructure, path []uint32, depth int) (*imapBodyPart, error) {
		if depth > limits.IMAPMaxMIMEDepth {
			return nil, errMIMEPartDepthExceeded
		}
		count++
		if count > limits.IMAPMaxMIMEParts {
			return nil, errMIMEPartCountExceeded
		}
		node := &imapBodyPart{
			path:      append([]uint32(nil), path...),
			mediaType: strings.ToLower(part.MediaType()),
		}
		switch p := part.(type) {
		case *goimap.BodyStructureSinglePart:
			// The body of a message that is not multipart is part "1".
			if len(node.path) == 0 {
				node.path = []uint32{1}
			}
			node.params = lowerParams(p.Params)
			node.contentID = cleanContentID(p.ID)
			node.encoding = p.Encoding
			node.encodedSize = uint64(p.Size)
			if disp := p.Disposition(); disp != nil {
				node.disposition = strings.ToLower(disp.Value)
				node.dispParams = lowerParams(disp.Params)
			}
			// Servers pass RFC 2231 parameters through as they are
			// (filename*=utf-8''..., or numbered pieces).
			node.filename = mimeparam.FileName(node.dispParams, node.params)
		case *goimap.BodyStructureMultiPart:
			if p.Extended != nil {
				node.params = lowerParams(p.Extended.Params)
			}
			if disp := p.Disposition(); disp != nil {
				node.disposition = strings.ToLower(disp.Value)
				node.dispParams = lowerParams(disp.Params)
			}
			for i, child := range p.Children {
				childNode, err := walk(child, append(append([]uint32(nil), path...), uint32(i+1)), depth+1)
				if err != nil {
					return nil, err
				}
				node.children = append(node.children, childNode)
			}
		default:
			return nil, fmt.Errorf("unknown body structure %T", part)
		}
		return node, nil
	}
	return walk(structure, nil, 1)
}

func lowerParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[strings.ToLower(key)] = value
	}
	return out
}

func cleanContentID(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	return strings.TrimSuffix(raw, ">")
}

// attachmentParts lists the leaf parts a person would call attachments,
// including inline images that have a content id.
func attachmentParts(structure goimap.BodyStructure) ([]*imapBodyPart, error) {
	root, err := buildBodyTree(structure)
	if err != nil || root == nil {
		return nil, err
	}
	var out []*imapBodyPart
	var walk func(*imapBodyPart)
	walk = func(part *imapBodyPart) {
		if part == nil {
			return
		}
		if len(part.children) == 0 && isAttachmentPart(part) {
			out = append(out, part)
			return
		}
		for _, child := range part.children {
			walk(child)
		}
	}
	walk(root)
	sort.SliceStable(out, func(i, j int) bool { return pathString(out[i].path) < pathString(out[j].path) })
	return out, nil
}

func isAttachmentPart(part *imapBodyPart) bool {
	if part == nil || len(part.path) == 0 {
		return false
	}
	switch {
	case part.disposition == "attachment":
		return true
	case part.filename != "":
		return true
	case strings.HasPrefix(part.mediaType, "message/") && part.mediaType != "message/delivery-status":
		return true
	case part.contentID != "" && !strings.HasPrefix(part.mediaType, "text/"):
		return true
	}
	return false
}

// preferredBodyPart picks the part to show for a body mode: plain text first
// for readable and text, HTML for html.
func preferredBodyPart(structure goimap.BodyStructure, mode mail.BodyMode) (*imapBodyPart, error) {
	root, err := buildBodyTree(structure)
	if err != nil || root == nil {
		return nil, err
	}
	return selectBodyPart(root, mode), nil
}

func selectBodyPart(part *imapBodyPart, mode mail.BodyMode) *imapBodyPart {
	if part == nil || isAttachmentPart(part) {
		return nil
	}
	if len(part.children) == 0 {
		switch mode {
		case mail.BodyModeHTML:
			if part.mediaType == "text/html" {
				return part
			}
		case mail.BodyModeReadable, mail.BodyModeText:
			if part.mediaType == "text/plain" || part.mediaType == "text/html" {
				return part
			}
		}
		return nil
	}
	if part.mediaType == "multipart/related" {
		return selectBodyPart(relatedRootPart(part), mode)
	}
	if mode == mail.BodyModeHTML {
		return firstTextPart(part, "text/html")
	}
	if plain := firstTextPart(part, "text/plain"); plain != nil {
		return plain
	}
	return firstTextPart(part, "text/html")
}

func firstTextPart(part *imapBodyPart, mediaType string) *imapBodyPart {
	if part == nil || isAttachmentPart(part) {
		return nil
	}
	if len(part.children) == 0 {
		if part.mediaType == mediaType {
			return part
		}
		return nil
	}
	if part.mediaType == "multipart/related" {
		return firstTextPart(relatedRootPart(part), mediaType)
	}
	for _, child := range part.children {
		if found := firstTextPart(child, mediaType); found != nil {
			return found
		}
	}
	return nil
}

func relatedRootPart(part *imapBodyPart) *imapBodyPart {
	if part == nil || len(part.children) == 0 {
		return nil
	}
	if start := cleanContentID(part.params["start"]); start != "" {
		for _, child := range part.children {
			if strings.EqualFold(child.contentID, start) {
				return child
			}
		}
	}
	return part.children[0]
}

func pathString(path []uint32) string {
	parts := make([]string, 0, len(path))
	for _, n := range path {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, ".")
}

func uintPathToInt(path []uint32) []int {
	out := make([]int, 0, len(path))
	for _, n := range path {
		out = append(out, int(n))
	}
	return out
}

// decodeTransfer undoes base64 or quoted-printable. Broken input decodes as
// far as possible; degraded is true when something had to be left out.
func decodeTransfer(raw []byte, encoding string) (out []byte, degraded bool) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return raw, false
	case "base64":
		return decodeBase64Lenient(raw)
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		return decoded, err != nil
	default:
		return raw, true
	}
}

// decodeBase64Lenient ignores whitespace and stops at the first broken
// character, keeping everything decoded before it. Partial fetches also end
// mid group, which this handles the same way.
func decodeBase64Lenient(raw []byte) ([]byte, bool) {
	clean := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		clean = append(clean, b)
	}
	out, err := base64.StdEncoding.DecodeString(string(clean))
	if err == nil {
		return out, false
	}
	// Decode the whole groups before the problem, then the partial group
	// with padding added.
	end := len(clean)
	var corrupt base64.CorruptInputError
	if errors.As(err, &corrupt) && int(corrupt) < end {
		end = int(corrupt)
	}
	for end > 0 && clean[end-1] == '=' {
		end--
	}
	good := end / 4 * 4
	out, _ = base64.StdEncoding.DecodeString(string(clean[:good]))
	if rest := end - good; rest >= 2 {
		tail := string(clean[good:end]) + strings.Repeat("=", 4-rest)
		if extra, err := base64.StdEncoding.DecodeString(tail); err == nil {
			out = append(out, extra...)
		}
	}
	// Input that only stopped mid group (a partial fetch) is not broken.
	degraded := !(int(corrupt) >= len(clean) || !errors.As(err, &corrupt))
	return out, degraded
}

// decodeCharset converts text to UTF-8. Unknown charsets and invalid bytes
// fall back to Latin-1 or replacement characters instead of failing.
func decodeCharset(data []byte, label string) (string, bool) {
	label = strings.TrimSpace(label)
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		if utf8.Valid(data) {
			return string(data), false
		}
		return latin1(data), true
	}
	r, err := charset.NewReaderLabel(label, bytes.NewReader(data))
	if err != nil {
		if utf8.Valid(data) {
			return string(data), true
		}
		return latin1(data), true
	}
	converted, err := io.ReadAll(r)
	text := string(converted)
	if !utf8.ValidString(text) {
		return strings.ToValidUTF8(text, "�"), true
	}
	return text, err != nil
}

func latin1(data []byte) string {
	out, err := charmap.Windows1252.NewDecoder().Bytes(data)
	if err != nil {
		return strings.ToValidUTF8(string(data), "�")
	}
	return string(out)
}

// decodeTextPart turns the raw bytes of a text part into UTF-8 text.
func decodeTextPart(raw []byte, part *imapBodyPart) (string, bool) {
	decoded, transferDegraded := decodeTransfer(raw, part.encoding)
	text, charsetDegraded := decodeCharset(decoded, part.params["charset"])
	return text, transferDegraded || charsetDegraded
}

// previewText makes a short one-line preview from the start of a text part.
func previewText(raw []byte, part *imapBodyPart) string {
	text, _ := decodeTextPart(raw, part)
	if part.mediaType == "text/html" {
		text = htmltext.ToReadable(text)
	}
	return collapsePreview(text, previewRunes)
}

// collapsePreview joins lines, drops quoted reply lines and cuts the text to
// at most max runes.
func collapsePreview(text string, max int) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
		if b.Len() > max*4 {
			break
		}
	}
	fields := strings.FieldsFunc(b.String(), unicode.IsSpace)
	out := strings.Join(fields, " ")
	if utf8.RuneCountInString(out) > max {
		runes := []rune(out)
		out = strings.TrimSpace(string(runes[:max])) + "…"
	}
	return out
}
