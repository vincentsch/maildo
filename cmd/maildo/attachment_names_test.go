package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mimeparam"
)

// Non-ASCII attachment names (RFC 2231 as Thunderbird and Apple Mail write
// them) survive list, download and forward. A name with terminal escape
// codes is cleaned instead of making the forward fail.
func TestIMAPAttachmentNamesSurviveListDownloadAndForward(t *testing.T) {
	h := newSendHarness(t)
	const name = "Übersicht Q3 – final.pdf"
	h.server.Append("INBOX", "From: Clara Client <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Quarterly numbers\r\nMessage-ID: <q3@example.org>\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n"+
		"--b\r\nContent-Type: text/plain\r\n\r\nNumbers attached.\r\n"+
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment;\r\n filename*0*=utf-8''%C3%9Cbersicht%20Q3;\r\n filename*1*=%20%E2%80%93%20final.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n"+
		"--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"a\x1b[31mred.txt\"\r\n\r\nred\r\n--b--\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "quarterly", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	id := found.Messages[0].ID

	var listed struct {
		Attachments []struct {
			Name string `json:"name"`
		} `json:"attachments"`
	}
	if res := h.run("attachments", "list", "--id", id, "--json"); res.JSON(&listed) != nil || len(listed.Attachments) != 2 || listed.Attachments[0].Name != name || listed.Attachments[1].Name != "a_[31mred.txt" {
		t.Fatalf("attachments list: %s%s", res.Stdout, res.Stderr)
	}

	dir := t.TempDir()
	if res := h.run("attachments", "download", "--id", id, "--all", "--to", dir); res.Exit != 0 {
		t.Fatalf("download: %s%s", res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("download did not keep the name: %v (dir has %v)", err, entries)
	}

	// A forward takes the attachments along by default, and the preview
	// lists them. With none, the preview lists what stays behind.
	preview := h.run("forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--send", "--dry-run")
	if preview.Exit != 0 || !strings.Contains(preview.Stdout, "Attached: "+name+" (8 bytes), a_[31mred.txt (3 bytes)") {
		t.Fatalf("forward preview: %s%s", preview.Stdout, preview.Stderr)
	}
	none := h.run("forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--include-attachments", "none", "--dry-run")
	if none.Exit != 0 || strings.Contains(none.Stdout, "Attach:") || !strings.Contains(none.Stdout, "Left out: "+name) {
		t.Fatalf("forward preview without attachments: %s%s", none.Stdout, none.Stderr)
	}
	if res := approvedRun(t, h.run, "forward", "--id", id, "--to", "ann@example.org", "--body", "FYI", "--send"); res.Exit != 0 {
		t.Fatalf("forward: %s%s", res.Stdout, res.Stderr)
	}
	mails := h.server.Sent()
	if len(mails) != 1 {
		t.Fatalf("sent %d", len(mails))
	}
	if got := attachmentNamesIn(t, mails[0].Data); strings.Join(got, "|") != name+"|a_[31mred.txt" {
		t.Fatalf("forwarded attachment names = %q\n%s", got, mails[0].Data)
	}
	// Mail programs that only read the Content-Type name see it too.
	if !strings.Contains(string(mails[0].Data), `name="=?utf-8?b?`) {
		t.Fatalf("forward lacks an encoded Content-Type name:\n%s", mails[0].Data)
	}
}

// attachmentNamesIn returns the file names of a message's attachments, read
// the way other mail programs read them.
func attachmentNamesIn(t *testing.T, raw []byte) []string {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		if name := mimeparam.FileNameFromHeaders(part.Header.Get("Content-Disposition"), part.Header.Get("Content-Type")); name != "" {
			names = append(names, name)
		}
	}
}
