package imap

import (
	"io"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestIDsRoundTrip(t *testing.T) {
	folderID, err := formatFolderID("inbox")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseFolderID(folderID); err != nil || got != "INBOX" {
		t.Fatalf("parseFolderID = %q, %v; want INBOX", got, err)
	}
	messageID, err := formatMessageID("Projects/2026", 77, 42)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseMessageID(messageID)
	if err != nil || identity.Mailbox != "Projects/2026" || identity.UIDValidity != 77 || identity.UID != 42 {
		t.Fatalf("identity = %#v, %v", identity, err)
	}
	partID, err := formatPartID([]uint32{2, 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parsePartID(partID); err != nil || len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("parsePartID = %#v, %v", got, err)
	}
	threadID, err := formatThreadID("<root@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if got, singleton, err := parseThreadID(threadID); err != nil || singleton || got != "<root@example.com>" {
		t.Fatalf("parseThreadID = %q singleton=%t err=%v", got, singleton, err)
	}
}

func TestBodyPartSelectionAndAttachments(t *testing.T) {
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "MIXED",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureMultiPart{
				Subtype: "ALTERNATIVE",
				Children: []goimap.BodyStructure{
					&goimap.BodyStructureSinglePart{Type: "TEXT", Subtype: "PLAIN", Params: map[string]string{"charset": "UTF-8"}, Encoding: "7BIT", Size: 11},
					&goimap.BodyStructureSinglePart{Type: "TEXT", Subtype: "HTML", Params: map[string]string{"charset": "UTF-8"}, Encoding: "7BIT", Size: 12},
				},
			},
			&goimap.BodyStructureSinglePart{
				Type: "APPLICATION", Subtype: "PDF", Encoding: "BASE64", Size: 780,
				Extended: &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "report.pdf"}}},
			},
			&goimap.BodyStructureSinglePart{
				Type: "IMAGE", Subtype: "PNG", ID: "<logo@example.com>", Encoding: "BASE64", Size: 20,
				Extended: &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "inline"}},
			},
		},
	}
	readable, err := preferredBodyPart(structure, mail.BodyModeReadable)
	if err != nil || readable == nil || pathString(readable.path) != "1.1" {
		t.Fatalf("readable part = %#v, %v", readable, err)
	}
	html, err := preferredBodyPart(structure, mail.BodyModeHTML)
	if err != nil || html == nil || pathString(html.path) != "1.2" {
		t.Fatalf("html part = %#v, %v", html, err)
	}
	client := &accountClient{account: "work", provider: "imap"}
	rows := client.attachmentsFromStructure(structure)
	if len(rows) != 2 || rows[0].ID == "" || rows[0].Name != "report.pdf" || rows[0].Size != 570 || rows[1].ContentID != "logo@example.com" {
		t.Fatalf("attachments = %+v", rows)
	}
	if !rows[0].SizeFacts.EncodedSizeKnown || rows[0].SizeFacts.EncodedSize != 780 || rows[0].SizeFacts.DecodedSizeKnown {
		t.Fatalf("size facts = %+v", rows[0].SizeFacts)
	}
}

// Servers hand RFC 2231 parameters over as they are: Dovecot merges the
// pieces into "filename*", others keep "filename*0*" and so on. Before,
// these names showed as "(no name)" and downloaded as attachment-001.
func TestAttachmentNamesFromRFC2231AndRFC2047Parameters(t *testing.T) {
	const want = "Übersicht Q3 – final.pdf"
	part := func(disposition, typeParams map[string]string) *goimap.BodyStructureSinglePart {
		return &goimap.BodyStructureSinglePart{
			Type: "APPLICATION", Subtype: "PDF", Encoding: "BASE64", Size: 40, Params: typeParams,
			Extended: &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "attachment", Params: disposition}},
		}
	}
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "MIXED",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureSinglePart{Type: "TEXT", Subtype: "PLAIN", Encoding: "7BIT", Size: 3},
			part(map[string]string{"filename*": "utf-8''%C3%9Cbersicht%20Q3%20%E2%80%93%20final.pdf"}, nil),
			part(map[string]string{"filename*0*": "utf-8''%C3%9Cbersicht%20Q3", "filename*1*": "%20%E2%80%93%20final", "filename*2": ".pdf"}, nil),
			part(nil, map[string]string{"name": "=?UTF-8?Q?=C3=9Cbersicht_Q3_=E2=80=93_final.pdf?="}),
			part(map[string]string{"filename*": "iso-8859-1''%DCbersicht%20Q3%20-%20final.pdf"}, nil),
		},
	}
	client := &accountClient{account: "work", provider: "imap"}
	rows := client.attachmentsFromStructure(structure)
	names := []string{want, want, want, "Übersicht Q3 - final.pdf"}
	if len(rows) != len(names) {
		t.Fatalf("attachments = %+v", rows)
	}
	for i, name := range names {
		if rows[i].Name != name {
			t.Fatalf("attachment %d name = %q, want %q", i, rows[i].Name, name)
		}
	}
}

func TestHTMLModeAndRelatedStart(t *testing.T) {
	plainOnly := &goimap.BodyStructureSinglePart{Type: "TEXT", Subtype: "PLAIN", Encoding: "7BIT"}
	if html, _ := preferredBodyPart(plainOnly, mail.BodyModeHTML); html != nil {
		t.Fatalf("html part of a plain message = %#v", html)
	}
	related := &goimap.BodyStructureMultiPart{
		Subtype: "RELATED",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureSinglePart{Type: "IMAGE", Subtype: "PNG", ID: "<logo@example.com>", Encoding: "BASE64"},
			&goimap.BodyStructureSinglePart{Type: "TEXT", Subtype: "HTML", ID: "<body@example.com>", Encoding: "7BIT"},
		},
		Extended: &goimap.BodyStructureMultiPartExt{Params: map[string]string{"start": "<body@example.com>"}},
	}
	html, _ := preferredBodyPart(related, mail.BodyModeHTML)
	if html == nil || pathString(html.path) != "2" {
		t.Fatalf("related html part = %#v", html)
	}
}

func TestFolderNameWithSlashKeepsParent(t *testing.T) {
	parent := &imapFolderBuild{mailbox: "Projects", delimiter: '.', attrs: map[goimap.MailboxAttr]bool{}, selectable: true}
	child := &imapFolderBuild{mailbox: "Projects.Foo/Bar", delimiter: '.', attrs: map[goimap.MailboxAttr]bool{}, selectable: true}
	byName := map[string]*imapFolderBuild{"Projects": parent, "Projects.Foo/Bar": child}
	for _, b := range []*imapFolderBuild{parent, child} {
		if err := deriveFolderBuildNames(b, byName); err != nil {
			t.Fatal(err)
		}
	}
	wantParentID, _ := formatFolderID("Projects")
	if child.parentID != wantParentID || child.displayName != "Foo/Bar" || child.path != "" {
		t.Fatalf("child = parent %q display %q path %q", child.parentID, child.displayName, child.path)
	}
}

func TestLenientDecoding(t *testing.T) {
	got, degraded := decodeBase64Lenient([]byte("SGVsbG8gd29y\r\nbGQ@@@broken"))
	if string(got) != "Hello world" || !degraded {
		t.Fatalf("base64 = %q degraded=%t", got, degraded)
	}
	got, _ = decodeBase64Lenient([]byte("SGVsbG8gd29ybGQ")) // cut mid group, as a partial fetch is
	if string(got) != "Hello wor" {
		t.Fatalf("partial base64 = %q", got)
	}
	if text, degraded := decodeCharset([]byte("Gr\xfc\xdfe"), "x-unknown"); text != "Grüße" || !degraded {
		t.Fatalf("unknown charset = %q degraded=%t", text, degraded)
	}
	if text, _ := decodeCharset([]byte("Gr\xfc\xdfe"), "iso-8859-1"); text != "Grüße" {
		t.Fatalf("latin1 = %q", text)
	}
	if text, degraded := decodeCharset([]byte("plain ascii"), ""); text != "plain ascii" || degraded {
		t.Fatalf("ascii = %q degraded=%t", text, degraded)
	}
}

func TestCollapsePreview(t *testing.T) {
	in := "Hi Clara,\n\n> quoted earlier mail\n  thanks   for the notes.\n"
	if got := collapsePreview(in, 200); got != "Hi Clara, thanks for the notes." {
		t.Fatalf("preview = %q", got)
	}
	if got := collapsePreview(strings.Repeat("word ", 100), 12); got != "word word wo…" {
		t.Fatalf("cut preview = %q", got)
	}
}

func TestAttachmentDecodeKeepsWhatDecodes(t *testing.T) {
	client := &accountClient{account: "work", provider: "imap"}
	stream, err := client.decodeAttachmentLiteral(strings.NewReader("SGVs\r\nbG8 =\r\n"), "base64", "message", "attachment")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if string(data) != "Hello" {
		t.Fatalf("decoded = %q", data)
	}
	broken, err := client.decodeAttachmentLiteral(strings.NewReader("SGVsbG8gd29ybGQ!!!!"), "base64", "message", "attachment")
	if err != nil {
		t.Fatalf("broken base64 failed the download: %v", err)
	}
	defer broken.Close()
	data, _ = io.ReadAll(broken)
	if !strings.HasPrefix(string(data), "Hello wor") {
		t.Fatalf("broken decode = %q", data)
	}
}
