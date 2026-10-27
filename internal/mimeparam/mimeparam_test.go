package mimeparam

import (
	"mime"
	"net/textproto"
	"strings"
	"testing"
)

func TestFileNameFromHeaders(t *testing.T) {
	const want = "Übersicht Q3 – final.pdf"
	cases := []struct {
		name, disposition, contentType, want string
	}{
		{"plain", `attachment; filename="report.pdf"`, "application/pdf", "report.pdf"},
		{"content type name only", "attachment", `application/pdf; name="report.pdf"`, "report.pdf"},
		{"rfc 2231 utf-8", `attachment; filename*=utf-8''%C3%9Cbersicht%20Q3%20%E2%80%93%20final.pdf`, "application/pdf", want},
		{"rfc 2231 upper case charset and language", `attachment; filename*=UTF-8'de'%C3%9Cbersicht%20Q3%20%E2%80%93%20final.pdf`, "application/pdf", want},
		{"rfc 2231 continuation", "attachment; filename*0*=utf-8''%C3%9Cbersicht%20Q3; filename*1*=%20%E2%80%93%20final; filename*2=.pdf", "application/pdf", want},
		{"rfc 2231 iso-8859-1", "attachment; filename*=iso-8859-1''Gr%FC%DFe.txt", "text/plain", "Grüße.txt"},
		{"rfc 2231 wins over plain", `attachment; filename="Ubersicht.pdf"; filename*=utf-8''%C3%9Cbersicht.pdf`, "application/pdf", "Übersicht.pdf"},
		{"rfc 2047 in filename", `attachment; filename="=?UTF-8?B?w5xiZXJzaWNodC5wZGY=?="`, "application/pdf", "Übersicht.pdf"},
		{"rfc 2047 in content type name", "attachment", `application/pdf; name="=?iso-8859-1?Q?Gr=FC=DFe.pdf?="`, "Grüße.pdf"},
		{"raw latin-1 bytes", "attachment; filename=\"Gr\xfc\xdfe.txt\"", "text/plain", "Grüße.txt"},
		{"control characters", "attachment; filename=\"a\x1b[31mred.txt\"", "text/plain", "a_[31mred.txt"},
		{"semicolon in quotes", `attachment; filename="a;b.txt"; size=3`, "text/plain", "a;b.txt"},
		{"escaped quote", `attachment; filename="say \"hi\".txt"`, "text/plain", `say "hi".txt`},
		{"none", "inline", "image/png", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FileNameFromHeaders(tc.disposition, tc.contentType); got != tc.want {
				t.Fatalf("FileNameFromHeaders(%q, %q) = %q, want %q", tc.disposition, tc.contentType, got, tc.want)
			}
		})
	}
}

func TestSplitLowerCasesAndKeepsMarkers(t *testing.T) {
	value, params := Split(`Attachment; FileName*0*=utf-8''%C3%9C; filename*1=x; broken; size=12`)
	if value != "attachment" {
		t.Fatalf("value = %q", value)
	}
	if params["filename*0*"] != "utf-8''%C3%9C" || params["filename*1"] != "x" || params["size"] != "12" {
		t.Fatalf("params = %#v", params)
	}
}

// What maildo writes must read back as the same name, both with this
// package and with Go's own parser, which other programs resemble.
func TestFormatRoundTrip(t *testing.T) {
	for _, name := range []string{
		"report.pdf",
		`say "hi".txt`,
		"Übersicht Q3 – final.pdf",
		"Grüße aus München " + strings.Repeat("ä", 80) + ".pdf",
		"日本語のファイル名.txt",
	} {
		disposition := FormatDisposition("attachment", name)
		contentType := FormatContentType("application/pdf", name)
		// An encoded word may be 75 characters, so only the disposition
		// keeps to the usual 78; both stay far below the hard limit of 998.
		for header, max := range map[string]int{disposition: 78, contentType: 100} {
			for _, line := range strings.Split(header, "\r\n") {
				if len(line) > max && !isPlain(name) {
					t.Fatalf("header line too long for %q: %q", name, line)
				}
			}
		}
		unfold := func(s string) string { return strings.ReplaceAll(s, "\r\n", "") }
		if got := FileNameFromHeaders(unfold(disposition), ""); got != name {
			t.Fatalf("disposition round trip: %q -> %q -> %q", name, disposition, got)
		}
		if got := FileNameFromHeaders("", unfold(contentType)); got != name {
			t.Fatalf("content type round trip: %q -> %q -> %q", name, contentType, got)
		}
		_, params, err := mime.ParseMediaType(unfold(disposition))
		if err != nil || params["filename"] != name {
			t.Fatalf("Go parser read %q as %q (%v)", disposition, params["filename"], err)
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", unfold(contentType))
		_, params, err = mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil {
			t.Fatalf("Go parser rejected %q: %v", contentType, err)
		}
		decoded, err := new(mime.WordDecoder).DecodeHeader(params["name"])
		if err != nil || decoded != name {
			t.Fatalf("Go parser read name %q as %q (%v)", contentType, decoded, err)
		}
	}
}

func TestCleanName(t *testing.T) {
	if got := CleanName(" a\x1b[0m\tb\x00c\xff.txt "); got != "a_[0m_b_c_.txt" {
		t.Fatalf("CleanName = %q", got)
	}
}
