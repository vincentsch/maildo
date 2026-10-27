// Package mimeparam reads and writes the parameters of MIME headers such as
// Content-Type and Content-Disposition, above all attachment file names.
//
// Mail programs write a file name that is not plain ASCII in one of two
// ways. RFC 2231 puts a charset and percent-encoded bytes in the value, and
// may split a long value into numbered pieces:
//
//	filename*=utf-8''%C3%9Cbersicht.pdf
//	filename*0*=utf-8''%C3%9Cber; filename*1*=sicht.pdf
//
// Other programs wrap the name in an RFC 2047 encoded word, which the
// standard does not allow there but every common mail program reads:
//
//	name="=?UTF-8?B?w5xiZXJzaWNodC5wZGY=?="
//
// This package reads both, and writes names the way Thunderbird does: an
// encoded word in the Content-Type name and RFC 2231 in the disposition.
package mimeparam

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/charmap"
)

// Split splits a header value such as `attachment; filename="a.pdf"` into
// its lower-cased first value and its parameters. Parameter names are
// lower-cased and keep RFC 2231 markers ("filename*", "filename*0*"), so
// Value can decode them. Split never fails: broken parameters are left out.
func Split(header string) (string, map[string]string) {
	params := map[string]string{}
	value, rest, _ := strings.Cut(header, ";")
	value = strings.ToLower(strings.TrimSpace(value))
	for rest != "" {
		var key, val string
		key, rest = cutUntil(rest, "=;")
		key = strings.ToLower(strings.TrimSpace(key))
		if rest == "" || rest[0] == ';' {
			rest = strings.TrimPrefix(rest, ";")
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		if strings.HasPrefix(rest, `"`) {
			val, rest = quotedString(rest)
		} else {
			val, rest = cutUntil(rest, ";")
			val = strings.TrimSpace(val)
		}
		rest = strings.TrimPrefix(strings.TrimLeft(rest, " \t"), ";")
		if key == "" {
			continue
		}
		if _, seen := params[key]; !seen {
			params[key] = val
		}
	}
	return value, params
}

// cutUntil returns the text before the first byte from stop, and the rest
// starting at that byte.
func cutUntil(s, stop string) (string, string) {
	if i := strings.IndexAny(s, stop); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// quotedString reads a quoted string at the start of s, with backslash
// escapes, and returns its content and the text after the closing quote.
// An unclosed quote takes the rest of s.
func quotedString(s string) (string, string) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), ""
}

// Value returns the decoded value of parameter name from params, as Split
// returns them. An RFC 2231 value (name* or numbered pieces) wins over a
// plain one, as RFC 2231 says. A plain value may hold RFC 2047 encoded
// words. Keys must be lower case.
func Value(params map[string]string, name string) string {
	name = strings.ToLower(name)
	if v, ok := params[name+"*"]; ok {
		if out, ok := decodeExtended(v); ok {
			return out
		}
	}
	if out, ok := continuation(params, name); ok {
		return out
	}
	return decodeWords(params[name])
}

// continuation joins numbered pieces: name*0*, name*1, name*2* and so on.
// Pieces with a trailing "*" are percent-encoded, and the first of those
// names the charset.
func continuation(params map[string]string, name string) (string, bool) {
	var raw []byte
	label := ""
	found := false
	for i := 0; ; i++ {
		key := name + "*" + strconv.Itoa(i)
		if v, ok := params[key+"*"]; ok {
			if i == 0 {
				parts := strings.SplitN(v, "'", 3)
				if len(parts) == 3 {
					label, v = parts[0], parts[2]
				}
			}
			raw = append(raw, percentDecode(v)...)
		} else if v, ok := params[key]; ok {
			raw = append(raw, v...)
		} else {
			break
		}
		found = true
	}
	if !found {
		return "", false
	}
	return toUTF8(label, raw), true
}

// decodeExtended reads an RFC 2231 value: charset'language'percent-bytes.
func decodeExtended(v string) (string, bool) {
	parts := strings.SplitN(v, "'", 3)
	if len(parts) != 3 {
		return "", false
	}
	return toUTF8(parts[0], percentDecode(parts[2])), true
}

func percentDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(n))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

// toUTF8 converts raw bytes in the named charset to UTF-8. Bytes in an
// unknown charset that are not valid UTF-8 are read as Windows-1252, the
// usual charset of old mail programs that leave it out.
func toUTF8(label string, raw []byte) string {
	label = strings.ToLower(strings.TrimSpace(label))
	switch label {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		if utf8.Valid(raw) {
			return string(raw)
		}
	default:
		if enc, _ := charset.Lookup(label); enc != nil {
			if out, err := enc.NewDecoder().Bytes(raw); err == nil && utf8.Valid(out) {
				return string(out)
			}
		}
		if utf8.Valid(raw) {
			return string(raw)
		}
	}
	out, _ := charmap.Windows1252.NewDecoder().Bytes(raw)
	return string(out)
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
	return charset.NewReaderLabel(label, input)
}}

// decodeWords decodes RFC 2047 encoded words in a plain value. Text that
// is not valid UTF-8 is read as Windows-1252.
func decodeWords(v string) string {
	if strings.Contains(v, "=?") {
		if out, err := wordDecoder.DecodeHeader(v); err == nil {
			v = out
		}
	}
	if !utf8.ValidString(v) {
		return toUTF8("", []byte(v))
	}
	return v
}

// FileName returns an attachment's file name from the parameters of its
// Content-Disposition (filename) and Content-Type (name), as Split returns
// them. Control characters become "_", so a name can neither break a
// terminal nor a message maildo writes later.
func FileName(disposition, contentType map[string]string) string {
	name := Value(disposition, "filename")
	if strings.TrimSpace(name) == "" {
		name = Value(contentType, "name")
	}
	return CleanName(name)
}

// FileNameFromHeaders is FileName for raw Content-Disposition and
// Content-Type header values.
func FileNameFromHeaders(disposition, contentType string) string {
	_, dp := Split(disposition)
	_, cp := Split(contentType)
	return FileName(dp, cp)
}

// CleanName replaces control characters and invalid UTF-8 in a file name
// with "_" and trims surrounding spaces.
func CleanName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

// FormatContentType returns a Content-Type value with the file name in a
// name parameter. A name that is not plain ASCII is written as RFC 2047
// encoded words, which most mail programs read in this place.
func FormatContentType(mediaType, filename string) string {
	if filename == "" {
		return mediaType
	}
	if isPlain(filename) {
		return fmt.Sprintf(`%s; name="%s"`, mediaType, escape(filename))
	}
	return fmt.Sprintf("%s;\r\n name=\"%s\"", mediaType, foldWords(mime.BEncoding.Encode("utf-8", filename)))
}

// FormatDisposition returns a Content-Disposition value with the file
// name. A name that is not plain ASCII is written in RFC 2231 form and
// split into numbered pieces, so no header line gets too long.
func FormatDisposition(disposition, filename string) string {
	if filename == "" {
		return disposition
	}
	if isPlain(filename) {
		return fmt.Sprintf(`%s; filename="%s"`, disposition, escape(filename))
	}
	encoded := percentEncode(filename)
	const piece = 50
	if len("utf-8''")+len(encoded) <= piece {
		return fmt.Sprintf("%s;\r\n filename*=utf-8''%s", disposition, encoded)
	}
	var b bytes.Buffer
	b.WriteString(disposition)
	for i, n := 0, 0; i < len(encoded); n++ {
		end := min(i+piece, len(encoded))
		// Never split a %XX escape.
		for end < len(encoded) && end > i && (encoded[end-1] == '%' || (end >= 2 && encoded[end-2] == '%')) {
			end--
		}
		prefix := ""
		if n == 0 {
			prefix = "utf-8''"
		}
		fmt.Fprintf(&b, ";\r\n filename*%d*=%s%s", n, prefix, encoded[i:end])
		i = end
	}
	return b.String()
}

// isPlain reports whether a name can go into a quoted string as it is:
// printable ASCII only.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// percentEncode writes s as RFC 2231 attribute characters with %XX for
// everything else.
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > 0x20 && c < 0x7f && !strings.ContainsRune(`*'%()<>@,;:\"/[]?=`, rune(c)) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// foldWords puts each encoded word after the first on its own header line.
func foldWords(words string) string {
	return strings.ReplaceAll(words, "?= =?", "?=\r\n =?")
}
