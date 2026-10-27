package imap

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/search"
)

func mustQuery(t *testing.T, raw string) mail.Query {
	t.Helper()
	q, err := search.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func subjects(rows []mail.MessageSummary) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Subject
	}
	return out
}

func TestListShowsNewestFirstAndPages(t *testing.T) {
	s := startMemServer(t)
	for i := 0; i < 30; i++ {
		flags := []goimap.Flag{goimap.FlagSeen}
		if i%3 == 0 {
			flags = nil
		}
		s.add("INBOX", testMessage{from: "a@example.net", subject: fmt.Sprintf("Message %02d", i)}.raw(), day(i), flags...)
	}
	c := s.client()
	ctx := context.Background()

	first, err := c.ListMessages(ctx, mail.ListMessagesRequest{Folder: folderFor(t, "INBOX"), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 30 || len(first.Messages) != 10 || first.Messages[0].Subject != "Message 29" || first.Messages[9].Subject != "Message 20" {
		t.Fatalf("first page = total %d %v", first.Total, subjects(first.Messages))
	}
	second, err := c.ListMessages(ctx, mail.ListMessagesRequest{Folder: folderFor(t, "INBOX"), Limit: 10, Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages[0].Subject != "Message 19" {
		t.Fatalf("second page = %v", subjects(second.Messages))
	}
	unread, err := c.ListMessages(ctx, mail.ListMessagesRequest{Folder: folderFor(t, "INBOX"), Limit: 100, Filter: mail.ListFilter{Unread: true}})
	if err != nil {
		t.Fatal(err)
	}
	if unread.Total != 10 {
		t.Fatalf("unread total = %d, want 10", unread.Total)
	}
	for _, row := range unread.Messages {
		if row.IsRead {
			t.Fatalf("read message in unread list: %v", row.Subject)
		}
	}
}

func TestSearchRunsOnTheServerInFewRoundTrips(t *testing.T) {
	s := startMemServer(t, "Bulk")
	for i := 0; i < 600; i++ {
		s.add("Bulk", testMessage{from: fmt.Sprintf("sender%d@bulk.example", i%50), subject: fmt.Sprintf("Bulk %d", i), body: fmt.Sprintf("Body keyword%d", i%100)}.raw(), day(0).Add(time.Duration(i)*time.Minute))
	}
	c := s.client()
	s.commands.reset()
	out, err := c.SearchMessages(context.Background(), mail.SearchRequest{Query: mustQuery(t, "keyword42"), Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 6 || len(out.Messages) != 6 || out.Messages[0].Subject != "Bulk 542" {
		t.Fatalf("search = total %d %v", out.Total, subjects(out.Messages))
	}
	if n := s.commands.count("UID FETCH") + s.commands.count("FETCH"); n > 8 {
		t.Fatalf("search used %d FETCH commands; it should batch them", n)
	}
}

func TestSearchMatchesNamesFlagsAndAttachments(t *testing.T) {
	s := startMemServer(t, "Trash", "Sent")
	s.add("INBOX", testMessage{from: "Clara Client <client@client.test>", subject: "Contract notes", body: "Payment terms"}.raw(), day(1))
	s.add("INBOX", testMessage{from: "Bob <bob@builder.example>", subject: "Site visit", body: "Friday"}.raw(), day(2), goimap.FlagSeen, goimap.FlagFlagged)
	s.add("INBOX", testMessage{
		from: "Billing <billing@vendor.example>", subject: "Invoice 104",
		contentType: `multipart/mixed; boundary="b1"`,
		body:        "--b1\nContent-Type: text/plain\n\nAttached.\n--b1\nContent-Type: application/pdf; name=\"invoice.pdf\"\nContent-Disposition: attachment; filename=\"invoice.pdf\"\nContent-Transfer-Encoding: base64\n\nJVBERi0xLjQK\n--b1--",
	}.raw(), day(3), goimap.FlagSeen)
	s.add("Trash", testMessage{from: "Clara Client <client@client.test>", subject: "Old contract", body: "Payment terms"}.raw(), day(4))
	c := s.client()
	ctx := context.Background()
	run := func(q string, folder *mail.Folder) []string {
		t.Helper()
		out, err := c.SearchMessages(ctx, mail.SearchRequest{Query: mustQuery(t, q), Folder: folder, Limit: 25})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return subjects(out.Messages)
	}
	if got := run(`from:clara`, nil); strings.Join(got, ",") != "Contract notes" {
		t.Fatalf("from display name = %v (Trash must be left out)", got)
	}
	trash := folderFor(t, "Trash")
	if got := run(`from:clara`, &trash); strings.Join(got, ",") != "Old contract" {
		t.Fatalf("explicit Trash search = %v", got)
	}
	if got := run(`is:unread`, nil); strings.Join(got, ",") != "Contract notes" {
		t.Fatalf("is:unread = %v", got)
	}
	if got := run(`is:flagged`, nil); strings.Join(got, ",") != "Site visit" {
		t.Fatalf("is:flagged = %v", got)
	}
	if got := run(`has:attachment`, nil); strings.Join(got, ",") != "Invoice 104" {
		t.Fatalf("has:attachment = %v", got)
	}
	if got := run(`"payment terms" after:2026-10-01 before:2026-10-03`, nil); strings.Join(got, ",") != "Contract notes" {
		t.Fatalf("phrase with dates = %v", got)
	}
}

func TestSearchAndReadSurviveBrokenMail(t *testing.T) {
	s := startMemServer(t, "Dirty")
	broken := []string{
		"From: shop@old.example\nTo: me@example.com\nSubject: Gr\xfc\xdfe raw latin1\nDate: Mon, 12 Oct 2026 10:00:00 +0000\nMessage-ID: <d1@old.example>\n\nbody one\n",
		"From: a@b.example\nTo: me@example.com\nThis line has no colon\nSubject: garbage header line\nMessage-ID: <d3@b.example>\n\nbody three\n",
		"From: a@b.example\nTo: me@example.com\nSubject: bad date\nDate: Mon, 32 Foo 2026 25:61:00 +9999\n\nbody four\n",
		"From: =?x-foo?Q?Weird?= <w@b.example>\nTo: me@example.com\nSubject: =?x-unknown?B?SGVsbG8=?= charset\nMessage-ID: <d5@b.example>\nContent-Type: text/plain; charset=x-unknown\n\nbody bytes \xe9\n",
		"From: a@b.example\nTo: me@example.com\nSubject: broken base64\nMessage-ID: <d6@b.example>\nMIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\nSGVsbG8gd29y@@@not base64!!!\n",
		"From: \nTo: me@example.com\nSubject: empty from\n\nbody seven\n",
	}
	for i, raw := range broken {
		s.add("Dirty", raw, day(i))
	}
	s.add("Dirty", testMessage{from: "clean@example.net", subject: "Clean invoice", body: "invoice inside"}.raw(), day(10))
	c := s.client()
	ctx := context.Background()

	found, err := c.SearchMessages(ctx, mail.SearchRequest{Query: mustQuery(t, "invoice"), Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Messages) != 1 || found.Messages[0].Subject != "Clean invoice" {
		t.Fatalf("search = %v", subjects(found.Messages))
	}
	listed, err := c.ListMessages(ctx, mail.ListMessagesRequest{Folder: folderFor(t, "Dirty"), Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Messages) != len(broken)+1 {
		t.Fatalf("list returned %d rows, want %d: %v", len(listed.Messages), len(broken)+1, listed.Warnings)
	}
	for _, row := range listed.Messages {
		msg, err := c.ReadMessage(ctx, mail.ReadMessageRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: row.ID}, BodyMode: mail.BodyModeReadable})
		if err != nil {
			t.Fatalf("read %q: %v", row.Subject, err)
		}
		if row.Subject == "broken base64" {
			if !strings.HasPrefix(msg.Body.Content, "Hello wor") || len(msg.Warnings) == 0 {
				t.Fatalf("broken base64 body = %q warnings=%v", msg.Body.Content, msg.Warnings)
			}
		}
	}
}

// A message read by Message-ID must come with its own body even when another
// folder has a message with the same UID.
func TestReadByInternetIDUsesTheMessagesOwnFolder(t *testing.T) {
	s := startMemServer(t, "Trash")
	for i := 0; i < 3; i++ {
		s.add("Trash", testMessage{from: "x@example.net", subject: fmt.Sprintf("Trash %d", i), body: "WRONG BODY FROM TRASH"}.raw(), day(i))
	}
	s.add("INBOX", testMessage{from: "a@example.net", subject: "Filler 1", body: "filler"}.raw(), day(0))
	s.add("INBOX", testMessage{from: "a@example.net", subject: "Filler 2", body: "filler"}.raw(), day(0))
	uid := s.add("INBOX", testMessage{from: "Clara <client@client.test>", subject: "Project kickoff", messageID: "<kickoff@client.test>", body: "Shall we kick off next week?"}.raw(), day(5))
	if uid != 3 {
		t.Fatalf("setup: kickoff UID = %d, want 3 so it collides with Trash", uid)
	}
	c := s.client()
	msg, err := c.ReadMessage(context.Background(), mail.ReadMessageRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorInternetID, InternetID: "<kickoff@client.test>"}, BodyMode: mail.BodyModeText})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != "Project kickoff" || !strings.Contains(msg.Body.Content, "kick off next week") || msg.Folder.DisplayName != "INBOX" {
		t.Fatalf("read = subject %q folder %q body %q", msg.Subject, msg.Folder.DisplayName, msg.Body.Content)
	}
}

func TestThreadAcrossFoldersIsComplete(t *testing.T) {
	s := startMemServer(t, "Sent")
	s.add("INBOX", testMessage{from: "Clara <client@client.test>", subject: "Project kickoff", messageID: "<t1@client.test>", date: "Sat, 03 Oct 2026 09:00:00 +0000", body: "Shall we?"}.raw(), day(2))
	s.add("Sent", testMessage{from: "Me <me@example.com>", subject: "Re: Project kickoff", messageID: "<t2@example.com>", references: "<t1@client.test>", inReplyTo: "<t1@client.test>", date: "Sat, 03 Oct 2026 10:00:00 +0000", body: "Yes"}.raw(), day(2))
	s.add("INBOX", testMessage{from: "Clara <client@client.test>", subject: "Re: Project kickoff", messageID: "<t3@client.test>", references: "<t1@client.test> <t2@example.com>", inReplyTo: "<t2@example.com>", date: "Sat, 03 Oct 2026 12:00:00 +0000", body: "Great"}.raw(), day(2))
	s.add("INBOX", testMessage{from: "Other <o@example.net>", subject: "Unrelated", messageID: "<other@example.net>"}.raw(), day(3))
	c := s.client()
	ctx := context.Background()
	for _, selector := range []mail.ResolvedMessageSelector{
		{Kind: mail.ResolvedSelectorInternetID, InternetID: "<t3@client.test>"},
		{Kind: mail.ResolvedSelectorInternetID, InternetID: "<t1@client.test>"},
	} {
		thread, err := c.ReadThread(ctx, mail.ReadThreadRequest{Selector: selector})
		if err != nil {
			t.Fatal(err)
		}
		if len(thread.Messages) != 3 || thread.ProviderMeta == nil || thread.ProviderMeta.Complete == nil || !*thread.ProviderMeta.Complete {
			t.Fatalf("thread from %s = %d messages, meta %+v", selector.InternetID, len(thread.Messages), thread.ProviderMeta)
		}
		if thread.Messages[1].Folder.DisplayName != "Sent" {
			t.Fatalf("thread order = %+v", thread.Messages)
		}
	}
}

func TestRowsCarryPreviews(t *testing.T) {
	s := startMemServer(t)
	s.add("INBOX", testMessage{from: "news@example.net", subject: "Digest", contentType: "text/html; charset=utf-8", body: "<html><body><h1>Weekly</h1><p>Hello <b>reader</b>, top stories.</p></body></html>"}.raw(), day(1))
	s.add("INBOX", testMessage{
		from: "bob@example.net", subject: "Alt", contentType: `multipart/alternative; boundary="x"`,
		body: "--x\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\nGr=C3=BC=C3=9Fe aus M=C3=BCnchen\n> quoted line\n--x\nContent-Type: text/html\n\n<p>html</p>\n--x--",
	}.raw(), day(2))
	c := s.client()
	out, err := c.ListMessages(context.Background(), mail.ListMessagesRequest{Folder: folderFor(t, "INBOX"), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Preview != "Grüße aus München" || !strings.Contains(out.Messages[1].Preview, "Hello reader, top stories.") {
		t.Fatalf("previews = %q / %q", out.Messages[0].Preview, out.Messages[1].Preview)
	}
}

func TestAttachmentsListWithSizesAndDownload(t *testing.T) {
	s := startMemServer(t)
	pdf := strings.Repeat("JVBERi0xLjQK", 40)
	uid := s.add("INBOX", testMessage{
		from: "billing@vendor.example", subject: "Invoice", contentType: `multipart/mixed; boundary="b"`,
		body: "--b\nContent-Type: text/plain\n\nSee attached.\n--b\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=\"invoice.pdf\"\nContent-Transfer-Encoding: base64\n\n" + pdf +
			"\n--b\nContent-Type: text/plain\nContent-Disposition: attachment; filename=\"../../etc/passwd\"\n\nnot really\n--b--",
	}.raw(), day(1))
	c := s.client()
	ctx := context.Background()
	id, _ := formatMessageID("INBOX", 1, uid)
	list, err := c.ListMessageAttachments(ctx, mail.MessageAttachmentListRequest{MessageID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Attachments) != 2 || list.Attachments[0].Name != "invoice.pdf" || list.Attachments[0].Size < 300 || list.Attachments[1].Name != "../../etc/passwd" {
		t.Fatalf("attachments = %+v", list.Attachments)
	}
	content, err := c.DownloadMessageAttachment(ctx, mail.MessageAttachmentDownloadRequest{MessageID: id, AttachmentID: list.Attachments[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	defer content.Content.Close()
	data, _ := io.ReadAll(content.Content)
	if !strings.HasPrefix(string(data), "%PDF-1.4") || len(data) != 360 {
		t.Fatalf("download = %d bytes %q", len(data), string(data[:min(8, len(data))]))
	}
}

func TestOneLoginServesAWholeCommand(t *testing.T) {
	s := startMemServer(t, "Archive")
	s.add("INBOX", testMessage{from: "a@example.net", subject: "Hello"}.raw(), day(1))
	c := s.client()
	ctx := context.Background()
	s.commands.reset()
	if _, err := c.ListFolders(ctx, mail.FolderListRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListMessages(ctx, mail.ListMessagesRequest{Folder: folderFor(t, "INBOX"), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if n := s.commands.count("AUTHENTICATE") + s.commands.count("LOGIN"); n != 1 {
		t.Fatalf("logins = %d, want 1", n)
	}
}

func TestFoldersWithoutSpecialUseGetRolesFromNames(t *testing.T) {
	s := startMemServer(t, "Trash", "Junk", "Sent Items", "Archive", "Clients", "Clients/Archive")
	c := s.client()
	list, err := c.ListFolders(context.Background(), mail.FolderListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, f := range list.Folders {
		roles[f.Path] = f.WellKnownName
	}
	want := map[string]string{"INBOX": "inbox", "Trash": "deleted", "Junk": "junk", "Sent Items": "sent", "Archive": "archive", "Clients": "", "Clients/Archive": ""}
	for path, role := range want {
		if roles[path] != role {
			t.Fatalf("role of %q = %q, want %q (all: %v)", path, roles[path], role, roles)
		}
	}
}
