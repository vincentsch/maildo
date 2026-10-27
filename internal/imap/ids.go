package imap

import (
	"encoding/base64"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
)

const (
	folderIDPrefix          = "imap-mailbox/v1/"
	messageIDPrefix         = "imap-message/v1/"
	partIDPrefix            = "imap-part/v1/"
	threadIDPrefix          = "imap-thread/v1/"
	singletonThreadIDPrefix = "imap-thread-singleton/v1/"
)

var (
	errBadOpaqueID                       = errors.New("invalid imap id")
	errBadUIDSet                         = errors.New("invalid provider uid set")
	errUIDSetSemanticCardinalityExceeded = errors.New("provider uid set semantic cardinality exceeded")
)

type messageIdentity struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

type ProviderUIDSet struct {
	segments  []uidSegment
	intervals []uidInterval
	card      uint64
}

type uidSegment struct {
	Start uint32
	Stop  uint32
}

type uidInterval struct {
	Lo uint32
	Hi uint32
}

func formatFolderID(mailbox string) (string, error) {
	canonical, err := canonicalMailboxName(mailbox)
	if err != nil {
		return "", err
	}
	return folderIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(canonical)), nil
}

func parseFolderID(id string) (string, error) {
	encoded, ok := strings.CutPrefix(id, folderIDPrefix)
	if !ok {
		return "", errBadOpaqueID
	}
	mailbox, err := decodeCanonicalBase64(encoded)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalMailboxName(mailbox)
	if err != nil || canonical != mailbox {
		return "", errBadOpaqueID
	}
	return mailbox, nil
}

func formatMessageID(mailbox string, uidValidity uint32, uid uint32) (string, error) {
	if uidValidity == 0 || uid == 0 {
		return "", errBadOpaqueID
	}
	canonical, err := canonicalMailboxName(mailbox)
	if err != nil {
		return "", err
	}
	return messageIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(canonical)) + "/" + strconv.FormatUint(uint64(uidValidity), 10) + "/" + strconv.FormatUint(uint64(uid), 10), nil
}

func parseMessageID(id string) (messageIdentity, error) {
	rest, ok := strings.CutPrefix(id, messageIDPrefix)
	if !ok {
		return messageIdentity{}, errBadOpaqueID
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return messageIdentity{}, errBadOpaqueID
	}
	mailbox, err := decodeCanonicalBase64(parts[0])
	if err != nil {
		return messageIdentity{}, errBadOpaqueID
	}
	canonical, err := canonicalMailboxName(mailbox)
	if err != nil || canonical != mailbox {
		return messageIdentity{}, errBadOpaqueID
	}
	uidValidity, err := parseCanonicalUint32(parts[1])
	if err != nil {
		return messageIdentity{}, errBadOpaqueID
	}
	uid, err := parseCanonicalUint32(parts[2])
	if err != nil {
		return messageIdentity{}, errBadOpaqueID
	}
	return messageIdentity{Mailbox: mailbox, UIDValidity: uidValidity, UID: uid}, nil
}

func formatPartID(path []uint32) (string, error) {
	parts, err := canonicalSectionPath(path)
	if err != nil {
		return "", err
	}
	return partIDPrefix + strings.Join(parts, "."), nil
}

func parsePartID(id string) ([]uint32, error) {
	rest, ok := strings.CutPrefix(id, partIDPrefix)
	if !ok {
		return nil, errBadOpaqueID
	}
	if rest == "" {
		return nil, errBadOpaqueID
	}
	fields := strings.Split(rest, ".")
	if len(fields) > limits.IMAPMaxMIMEDepth {
		return nil, errBadOpaqueID
	}
	out := make([]uint32, 0, len(fields))
	for _, field := range fields {
		n, err := parseCanonicalUint32(field)
		if err != nil {
			return nil, errBadOpaqueID
		}
		out = append(out, n)
	}
	canon, err := canonicalSectionPath(out)
	if err != nil || strings.Join(canon, ".") != rest {
		return nil, errBadOpaqueID
	}
	return out, nil
}

func formatThreadID(root string) (string, error) {
	normalized, err := mail.NormalizeMessageIDScalar(root)
	if err != nil {
		return "", err
	}
	return threadIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(normalized)), nil
}

func parseThreadID(id string) (string, bool, error) {
	if rest, ok := strings.CutPrefix(id, threadIDPrefix); ok {
		raw, err := decodeCanonicalBase64(rest)
		if err != nil {
			return "", false, errBadOpaqueID
		}
		normalized, err := mail.NormalizeMessageIDScalar(raw)
		if err != nil || normalized != raw {
			return "", false, errBadOpaqueID
		}
		return normalized, false, nil
	}
	if rest, ok := strings.CutPrefix(id, singletonThreadIDPrefix); ok {
		raw, err := decodeCanonicalBase64(rest)
		if err != nil {
			return "", false, errBadOpaqueID
		}
		if _, err := parseMessageID(raw); err != nil {
			return "", false, errBadOpaqueID
		}
		return raw, true, nil
	}
	return "", false, errBadOpaqueID
}

func formatSingletonThreadID(messageID string) (string, error) {
	if _, err := parseMessageID(messageID); err != nil {
		return "", err
	}
	return singletonThreadIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(messageID)), nil
}

func canonicalMailboxName(name string) (string, error) {
	if name == "" || !utf8.ValidString(name) || int64(len(name)) > limits.IMAPMaxMailboxNameBytes {
		return "", errBadOpaqueID
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errBadOpaqueID
		}
	}
	if asciiEqualFold(name, "INBOX") {
		return "INBOX", nil
	}
	return name, nil
}

func decodeCanonicalBase64(encoded string) (string, error) {
	if encoded == "" || strings.Contains(encoded, "=") {
		return "", errBadOpaqueID
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errBadOpaqueID
	}
	if base64.RawURLEncoding.EncodeToString(raw) != encoded || !utf8.Valid(raw) {
		return "", errBadOpaqueID
	}
	return string(raw), nil
}

func parseCanonicalUint32(s string) (uint32, error) {
	if s == "" || s == "0" || (len(s) > 1 && s[0] == '0') {
		return 0, errBadOpaqueID
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 || n > math.MaxUint32 {
		return 0, errBadOpaqueID
	}
	if strconv.FormatUint(n, 10) != s {
		return 0, errBadOpaqueID
	}
	return uint32(n), nil
}

func canonicalSectionPath(path []uint32) ([]string, error) {
	if len(path) == 0 || len(path) > limits.IMAPMaxMIMEDepth {
		return nil, errBadOpaqueID
	}
	out := make([]string, 0, len(path))
	for _, n := range path {
		if n == 0 {
			return nil, errBadOpaqueID
		}
		out = append(out, strconv.FormatUint(uint64(n), 10))
	}
	return out, nil
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ab, bb := a[i], b[i]
		if ab >= 'A' && ab <= 'Z' {
			ab += 'a' - 'A'
		}
		if bb >= 'A' && bb <= 'Z' {
			bb += 'a' - 'A'
		}
		if ab != bb {
			return false
		}
	}
	return true
}

func ParseProviderUIDSet(raw string, lower uint32, upper uint32, maxSemanticCardinality uint64) (ProviderUIDSet, error) {
	if raw == "" || lower == 0 || upper == 0 || lower > upper || maxSemanticCardinality == 0 {
		return ProviderUIDSet{}, errBadUIDSet
	}
	var set ProviderUIDSet
	for _, elem := range strings.Split(raw, ",") {
		if elem == "" {
			return ProviderUIDSet{}, errBadUIDSet
		}
		startText, stopText, ranged := strings.Cut(elem, ":")
		start, err := parseCanonicalUint32(startText)
		if err != nil {
			return ProviderUIDSet{}, errBadUIDSet
		}
		stop := start
		if ranged {
			if strings.Contains(stopText, ":") {
				return ProviderUIDSet{}, errBadUIDSet
			}
			stop, err = parseCanonicalUint32(stopText)
			if err != nil {
				return ProviderUIDSet{}, errBadUIDSet
			}
		}
		if start < lower || start > upper || stop < lower || stop > upper {
			return ProviderUIDSet{}, errBadUIDSet
		}
		set.segments = append(set.segments, uidSegment{Start: start, Stop: stop})
		lo, hi := start, stop
		if lo > hi {
			lo, hi = hi, lo
		}
		set.intervals = appendInterval(set.intervals, uidInterval{Lo: lo, Hi: hi})
		set.card = intervalCardinality(set.intervals)
		if set.card > maxSemanticCardinality {
			return ProviderUIDSet{}, errUIDSetSemanticCardinalityExceeded
		}
	}
	return set, nil
}

func (s ProviderUIDSet) Cardinality() uint64 { return s.card }

func (s ProviderUIDSet) SemanticCardinality() uint64 { return s.card }

func (s ProviderUIDSet) Contains(uid uint32) bool {
	for _, interval := range s.intervals {
		if uid >= interval.Lo && uid <= interval.Hi {
			return true
		}
	}
	return false
}

func (s ProviderUIDSet) ExactSingleton() (uid uint32, ok bool) {
	if s.card != 1 || len(s.intervals) != 1 || s.intervals[0].Lo != s.intervals[0].Hi {
		return 0, false
	}
	return s.intervals[0].Lo, true
}

func (s ProviderUIDSet) WalkIntervals(visit func(lower, upper uint32) error) error {
	if visit == nil {
		return errBadUIDSet
	}
	for _, interval := range s.intervals {
		if err := visit(interval.Lo, interval.Hi); err != nil {
			return err
		}
	}
	return nil
}

func (s ProviderUIDSet) Segments() [][2]uint32 {
	out := make([][2]uint32, 0, len(s.segments))
	for _, segment := range s.segments {
		out = append(out, [2]uint32{segment.Start, segment.Stop})
	}
	return out
}

func appendInterval(intervals []uidInterval, next uidInterval) []uidInterval {
	intervals = append(intervals, next)
	sortIntervals(intervals)
	out := intervals[:0]
	for _, interval := range intervals {
		if len(out) == 0 || interval.Lo > out[len(out)-1].Hi+1 {
			out = append(out, interval)
			continue
		}
		if interval.Hi > out[len(out)-1].Hi {
			out[len(out)-1].Hi = interval.Hi
		}
	}
	return out
}

func sortIntervals(intervals []uidInterval) {
	for i := 1; i < len(intervals); i++ {
		v := intervals[i]
		j := i - 1
		for j >= 0 && (intervals[j].Lo > v.Lo || (intervals[j].Lo == v.Lo && intervals[j].Hi > v.Hi)) {
			intervals[j+1] = intervals[j]
			j--
		}
		intervals[j+1] = v
	}
}

func intervalCardinality(intervals []uidInterval) uint64 {
	var total uint64
	for _, interval := range intervals {
		total += uint64(interval.Hi - interval.Lo + 1)
	}
	return total
}
