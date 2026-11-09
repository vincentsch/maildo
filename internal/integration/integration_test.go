//go:build integration

// Package integration runs the maildo binary against real IMAP and SMTP
// servers: Dovecot (with SPECIAL-USE and CONDSTORE) and GreenMail (without).
// Start them with scripts/mailservers.sh; `make integration` starts them, runs
// these tests and stops them again.
package integration

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/mimeparam"
)

var maildoBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "maildo-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	maildoBin = filepath.Join(dir, "maildo")
	if runtime.GOOS == "windows" {
		maildoBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", maildoBin, "../../cmd/maildo")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build maildo:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// server describes one mail server under test.
type server struct {
	name       string
	imapPort   string
	plainPort  string // IMAP port that offers STARTTLS, if any
	smtpPort   string
	password   string
	specialUse bool
	// keepsDates is false for servers that ignore the date given with
	// APPEND, so all seeded mail "arrived" at the same moment.
	keepsDates bool
	// namesInFrom is false for servers whose FROM search matches only the
	// address, not the display name.
	namesInFrom bool
}

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s is not set; start the servers with scripts/mailservers.sh", key)
	}
	return v
}

func TestDovecot(t *testing.T) {
	runSuite(t, server{
		name:        "dovecot",
		imapPort:    env(t, "MAILDO_IT_DOVECOT_IMAPS"),
		plainPort:   env(t, "MAILDO_IT_DOVECOT_IMAP"),
		smtpPort:    env(t, "MAILDO_IT_GREENMAIL_SMTPS"),
		password:    "secret",
		specialUse:  true,
		keepsDates:  true,
		namesInFrom: true,
	})
}

func TestGreenMail(t *testing.T) {
	runSuite(t, server{
		name:     "greenmail",
		imapPort: env(t, "MAILDO_IT_GREENMAIL_IMAPS"),
		smtpPort: env(t, "MAILDO_IT_GREENMAIL_SMTPS"),
		password: "secret",
	})
}

// cli runs maildo with its own home directory, as a new user would.
type cli struct {
	home string
	env  []string
}

func newCLI(t *testing.T, password string) *cli {
	home := t.TempDir()
	return &cli{home: home, env: []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"PATH=" + os.Getenv("PATH"),
		"MAILDO_IMAP_PASSWORD=" + password,
		"NO_COLOR=1",
	}}
}

type result struct {
	stdout, stderr string
	exit           int
}

func (c *cli) run(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(maildoBin, args...)
	cmd.Env = c.env
	cmd.Stdin = strings.NewReader("")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run maildo: %v", err)
	}
	return result{stdout: out.String(), stderr: errOut.String(), exit: exit}
}

// ok runs a command that must succeed and decodes its JSON into v.
func (c *cli) ok(t *testing.T, v any, args ...string) {
	t.Helper()
	res := c.run(t, append(args, "--json")...)
	if res.exit != 0 {
		t.Fatalf("maildo %s: exit %d\n%s%s", strings.Join(args, " "), res.exit, res.stdout, res.stderr)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(res.stdout), v); err != nil {
			t.Fatalf("maildo %s: bad JSON: %v\n%s", strings.Join(args, " "), err, res.stdout)
		}
	}
}

// approved sends the way AGENTS.md tells an agent to: a --dry-run preview
// first, then the same command with the approval code from the preview. It
// returns the code.
func (c *cli) approved(t *testing.T, args ...string) string {
	t.Helper()
	var preview struct {
		ApprovalCode string `json:"approval_code"`
		SendCommand  string `json:"send_command"`
	}
	c.ok(t, &preview, append(args, "--dry-run")...)
	if preview.ApprovalCode == "" || !strings.Contains(preview.SendCommand, "--approve "+preview.ApprovalCode) {
		t.Fatalf("maildo %s: preview without an approval code: %+v", strings.Join(args, " "), preview)
	}
	c.ok(t, nil, append(args, "--approve", preview.ApprovalCode, "--yes")...)
	return preview.ApprovalCode
}

// fails runs a command that must fail and returns its JSON error body.
func (c *cli) fails(t *testing.T, args ...string) map[string]any {
	t.Helper()
	res := c.run(t, append(args, "--json")...)
	if res.exit == 0 {
		t.Fatalf("maildo %s succeeded, want an error:\n%s", strings.Join(args, " "), res.stdout)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.stdout), &body)
	if body == nil {
		_ = json.Unmarshal([]byte(res.stderr), &body)
	}
	if body == nil {
		t.Fatalf("maildo %s: no JSON error\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.stdout, res.stderr)
	}
	return body
}

type row struct {
	ID                string `json:"id"`
	InternetMessageID string `json:"internet_message_id"`
	Subject           string `json:"subject"`
	Preview           string `json:"preview"`
	IsRead            bool   `json:"is_read"`
	Flagged           bool   `json:"flagged"`
	From              struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"from"`
	Folder struct {
		Path string `json:"path"`
	} `json:"folder"`
}

type page struct {
	Messages []row    `json:"messages"`
	Total    *int     `json:"total"`
	HasMore  bool     `json:"has_more"`
	Warnings []string `json:"warnings"`
}

func subjects(rows []row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Subject
	}
	return out
}

func runSuite(t *testing.T, srv server) {
	caFile := env(t, "MAILDO_IT_CA_FILE")
	user := fmt.Sprintf("it-%d@example.test", time.Now().UnixNano())
	seed := seedMailbox(t, srv, caFile, user)
	c := newCLI(t, srv.password)
	login := []string{"login", "--provider", "imap", "--name", "it", "--email", user,
		"--imap-host", "localhost", "--imap-port", srv.imapPort, "--imap-tls", "implicit",
		"--smtp-host", "localhost", "--smtp-port", srv.smtpPort, "--smtp-tls", "implicit"}

	t.Run("untrusted certificate says so", func(t *testing.T) {
		body := c.fails(t, login...)
		if body["reason"] != "tls_verification_failed" {
			t.Fatalf("error = %v", body)
		}
	})
	if srv.plainPort != "" {
		t.Run("implicit TLS on a plain port says so", func(t *testing.T) {
			args := append([]string(nil), login...)
			args[10] = srv.plainPort
			body := c.fails(t, append(args, "--ca-file", caFile)...)
			if body["reason"] != "tls_not_offered" {
				t.Fatalf("error = %v", body)
			}
		})
		t.Run("wrong password is rejected", func(t *testing.T) {
			bad := newCLI(t, "wrong")
			body := bad.fails(t, append(login, "--ca-file", caFile)...)
			if body["reason"] != "credential_rejected" {
				t.Fatalf("error = %v", body)
			}
		})
		t.Run("STARTTLS login works", func(t *testing.T) {
			other := newCLI(t, srv.password)
			args := append([]string(nil), login...)
			args[10], args[12] = srv.plainPort, "starttls"
			other.ok(t, nil, append(args, "--ca-file", caFile)...)
			var p page
			other.ok(t, &p, "list", "--limit", "1")
		})
	}
	t.Run("login", func(t *testing.T) {
		c.ok(t, nil, append(login, "--ca-file", caFile)...)
		c.ok(t, nil, "auth", "status", "--refresh")
	})

	t.Run("folders", func(t *testing.T) {
		var out struct {
			Folders []struct {
				Path          string `json:"path"`
				WellKnownName string `json:"well_known_name"`
				TotalCount    int    `json:"total_count"`
			} `json:"folders"`
		}
		c.ok(t, &out, "folders", "list")
		roles := map[string]string{}
		counts := map[string]int{}
		for _, f := range out.Folders {
			roles[f.Path] = f.WellKnownName
			counts[f.Path] = f.TotalCount
		}
		if roles["INBOX"] != "inbox" || roles["Trash"] != "deleted" || roles["Archive"] != "archive" || roles["Sent"] != "sent" {
			t.Fatalf("roles = %v", roles)
		}
		if counts["Bulk"] != bulkCount {
			t.Fatalf("Bulk count = %d", counts["Bulk"])
		}
		c.ok(t, nil, "folders", "create", "Projects")
		c.ok(t, nil, "folders", "rename", "Projects", "Proposals")
		c.ok(t, nil, "folders", "delete", "Proposals", "--confirm", "Proposals")
		// In this beta maildo deletes only empty folders without subfolders.
		if body := c.fails(t, "folders", "delete", "Bulk", "--confirm", "Bulk"); body["code"] != "protected_folder" || body["reason"] != "not_empty" {
			t.Fatalf("delete of a full folder = %v", body)
		}
		if body := c.fails(t, "folders", "delete", "Clients", "--confirm", "Clients"); body["code"] != "protected_folder" || body["reason"] != "has_children" {
			t.Fatalf("delete of a folder with a subfolder = %v", body)
		}
	})

	t.Run("list newest first with previews and paging", func(t *testing.T) {
		var p page
		c.ok(t, &p, "list", "--limit", "3")
		newest := "Screenshot of the bug"
		if !srv.keepsDates {
			newest = p.Messages[0].Subject
		}
		if len(p.Messages) != 3 || p.Messages[0].Subject != newest || p.Total == nil || *p.Total != seed.inboxCount || !p.HasMore {
			t.Fatalf("list = %v total %v more %v", subjects(p.Messages), p.Total, p.HasMore)
		}
		var second page
		c.ok(t, &second, "list", "--limit", "3", "--page", "2")
		if second.Messages[0].Subject == p.Messages[0].Subject {
			t.Fatalf("page 2 repeats page 1: %v", subjects(second.Messages))
		}
		var news page
		c.ok(t, &news, "list", "Newsletters", "--limit", "100")
		for _, m := range news.Messages {
			if m.Subject == "Your weekly digest" && !strings.Contains(m.Preview, "top stories") {
				t.Fatalf("HTML preview = %q", m.Preview)
			}
		}
	})

	t.Run("search", func(t *testing.T) {
		cases := []struct {
			query string
			want  []string
		}{
			{"kickoff", []string{"Re: Project kickoff", "Re: Project kickoff", "Project kickoff"}},
			{"from:clara subject:kickoff", []string{"Re: Project kickoff", "Project kickoff"}},
			{`"weekly digest"`, []string{"Your weekly digest"}},
			{"has:attachment from:billing@vendor.example", []string{"Invoice 2026-104"}},
			{"is:flagged", []string{"Site visit on Friday"}},
			{"from:billing@vendor.example from:bob@builder.example", []string{"Invoice 2026-104", "Site visit on Friday"}},
			{"from:billing@vendor.example OR from:bob@builder.example", []string{"Invoice 2026-104", "Site visit on Friday"}},
			{"Grüße", []string{"Grüße aus München – Angebot für Straße 5 🚀"}},
			{"payment after:2026-10-01 before:2026-10-02", []string{"Contract draft for October"}},
		}
		for _, tc := range cases {
			if strings.Contains(tc.query, "from:clara") && !srv.namesInFrom {
				continue
			}
			var p page
			c.ok(t, &p, "search", tc.query)
			got, want := subjects(p.Messages), tc.want
			if !srv.keepsDates {
				sort.Strings(got)
				want = append([]string(nil), want...)
				sort.Strings(want)
			}
			if strings.Join(got, " | ") != strings.Join(want, " | ") {
				t.Errorf("search %q = %q, want %q", tc.query, got, want)
			}
		}
		// A display name with a space: GreenMail refuses FROM "Clara Client"
		// with BAD, so maildo retries with one FROM per word.
		var named page
		c.ok(t, &named, "search", `from:"Clara Client" subject:kickoff`)
		if srv.namesInFrom && len(named.Messages) != 2 {
			t.Errorf("from:\"Clara Client\" = %v", subjects(named.Messages))
		}
		var trash page
		c.ok(t, &trash, "search", "trashed order")
		if len(trash.Messages) != 0 {
			t.Fatalf("Trash leaked into search: %v", subjects(trash.Messages))
		}
		c.ok(t, &trash, "search", `subject:"Trashed order 2"`, "--folder", "Trash")
		if len(trash.Messages) != 1 {
			t.Fatalf("explicit Trash search = %v", subjects(trash.Messages))
		}
	})

	t.Run("search a large folder", func(t *testing.T) {
		start := time.Now()
		var p page
		c.ok(t, &p, "search", "keyword42")
		if p.Total == nil || *p.Total != bulkCount/100 || len(p.Messages) != 25 {
			t.Fatalf("bulk search total %v rows %d", p.Total, len(p.Messages))
		}
		if d := time.Since(start); d > 20*time.Second {
			t.Fatalf("bulk search took %s", d)
		}
		var all page
		c.ok(t, &all, "list", "Bulk", "--limit", "1000", "--page", "3")
		if len(all.Messages) != bulkCount-2000 {
			t.Fatalf("third page of Bulk = %d rows", len(all.Messages))
		}
	})

	t.Run("broken mail", func(t *testing.T) {
		var p page
		c.ok(t, &p, "search", "invoice", "--folder", "Dirty")
		if len(p.Messages) != 1 || p.Messages[0].Subject != "Clean invoice" {
			t.Fatalf("dirty search = %v", subjects(p.Messages))
		}
		c.ok(t, &p, "list", "Dirty", "--limit", "100")
		if len(p.Messages) != dirtyCount+1 {
			t.Fatalf("dirty list = %d rows, warnings %v", len(p.Messages), p.Warnings)
		}
		for _, m := range p.Messages {
			c.ok(t, nil, "read", "--id", m.ID)
		}
	})

	t.Run("read by Message-ID reads the right body", func(t *testing.T) {
		var out struct {
			Message struct {
				Subject string `json:"subject"`
				Body    struct {
					Content string `json:"content"`
				} `json:"body"`
			} `json:"message"`
		}
		c.ok(t, &out, "read", "--internet-id", seed.kickoffID)
		if out.Message.Subject != "Project kickoff" || !strings.Contains(out.Message.Body.Content, "kick off next week") {
			t.Fatalf("read = %q %q", out.Message.Subject, out.Message.Body.Content)
		}
	})

	t.Run("thread across folders", func(t *testing.T) {
		var out struct {
			Thread struct {
				Messages []row `json:"messages"`
				Meta     struct {
					Complete bool `json:"complete"`
				} `json:"provider_metadata"`
			} `json:"thread"`
		}
		c.ok(t, &out, "thread", "--internet-id", seed.lastReplyID)
		if len(out.Thread.Messages) != 3 || !out.Thread.Meta.Complete {
			t.Fatalf("thread = %v complete %v", subjects(out.Thread.Messages), out.Thread.Meta.Complete)
		}
	})

	t.Run("attachments", func(t *testing.T) {
		var list page
		c.ok(t, &list, "search", "subject:invoice from:billing@vendor.example")
		var att struct {
			Attachments []struct {
				ID          string  `json:"id"`
				Name        string  `json:"name"`
				Size        int64   `json:"size"`
				DecodedSize *uint64 `json:"decoded_size"`
			} `json:"attachments"`
		}
		c.ok(t, &att, "attachments", "list", "--id", list.Messages[0].ID)
		if len(att.Attachments) != 2 || att.Attachments[0].Name != "invoice-2026-104.pdf" || att.Attachments[0].Size < 2000 {
			t.Fatalf("attachments = %+v", att.Attachments)
		}
		// IMAP knows only the encoded size, so the file size is an estimate
		// and no decoded_size is listed.
		if att.Attachments[0].DecodedSize != nil {
			t.Fatalf("decoded_size = %d, but IMAP does not know it", *att.Attachments[0].DecodedSize)
		}
		dir := t.TempDir()
		c.ok(t, nil, "attachments", "download", "--id", list.Messages[0].ID, "--all", "--to", dir)
		data, err := os.ReadFile(filepath.Join(dir, "invoice-2026-104.pdf"))
		if err != nil || !bytes.HasPrefix(data, []byte("%PDF-1.4")) {
			t.Fatalf("downloaded PDF: %v %q", err, data[:min(10, len(data))])
		}
	})

	// Servers hand RFC 2231 names over in BODYSTRUCTURE as they are, which
	// once showed up as "(no name)" and attachment-001.
	t.Run("non-ASCII attachment names", func(t *testing.T) {
		const pdfName, txtName = "Übersicht Q3 – final.pdf", "Grüße.txt"
		admin := dialAdmin(t, srv, caFile, user)
		when := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
		wait(t, []*imapclient.AppendCommand{appendRaw(t, admin, "INBOX", msg("Clara Client <client@client.test>", "Attachment names", when.Format(time.RFC1123Z), "<names@client.test>", "",
			"Content-Type: multipart/mixed; boundary=\"n\"\n\n--n\nContent-Type: text/plain\n\nTwo files.\n"+
				"--n\nContent-Type: application/pdf\nContent-Disposition: attachment;\n filename*0*=utf-8''%C3%9Cbersicht%20Q3;\n filename*1*=%20%E2%80%93%20final.pdf\nContent-Transfer-Encoding: base64\n\nJVBERi0xLjQK\n"+
				"--n\nContent-Type: text/plain; name=\"=?iso-8859-1?Q?Gr=FC=DFe.txt?=\"\nContent-Disposition: attachment\n\nhello\n--n--\n"), when)})
		admin.Close()
		var p page
		c.ok(t, &p, "search", `subject:"Attachment names"`)
		if len(p.Messages) != 1 {
			t.Fatalf("search = %v", subjects(p.Messages))
		}
		id := p.Messages[0].ID
		var att struct {
			Attachments []struct {
				Name string `json:"name"`
			} `json:"attachments"`
		}
		c.ok(t, &att, "attachments", "list", "--id", id)
		if len(att.Attachments) != 2 || att.Attachments[0].Name != pdfName || att.Attachments[1].Name != txtName {
			t.Fatalf("attachments = %+v", att.Attachments)
		}
		dir := t.TempDir()
		c.ok(t, nil, "attachments", "download", "--id", id, "--all", "--to", dir)
		for _, name := range []string{pdfName, txtName} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("download lost the name %q: %v", name, err)
			}
		}
		rcpt := fmt.Sprintf("names-%d@example.test", time.Now().UnixNano())
		c.approved(t, "forward", "--id", id, "--to", rcpt, "--body", "Forwarding both files.", "--send")
		got := waitForMessage(t, srv, caFile, rcpt, "Forwarding both files.")
		if names := attachmentNames(t, got); strings.Join(names, "|") != pdfName+"|"+txtName {
			t.Fatalf("forwarded names = %q\n%s", names, got)
		}
	})

	t.Run("changes", func(t *testing.T) {
		var p page
		c.ok(t, &p, "search", "subject:\"Contract draft\"")
		id := p.Messages[0].ID
		c.ok(t, nil, "mark", "read", "--id", id)
		c.ok(t, nil, "flag", "set", "--id", id)
		c.ok(t, &p, "search", "subject:\"Contract draft\" is:flagged is:read")
		if len(p.Messages) != 1 {
			t.Fatalf("after mark and flag: %v", subjects(p.Messages))
		}
		c.ok(t, nil, "flag", "clear", "--id", id)
		c.ok(t, nil, "copy", "--id", id, "--to", "Clients")
		// The undo command of an archive puts the message back in INBOX.
		var archived struct {
			NewID        string   `json:"new_id"`
			UndoCommands []string `json:"undo_commands"`
		}
		c.ok(t, &archived, "archive", "--id", id)
		if archived.NewID == "" || archived.NewID == id || len(archived.UndoCommands) != 1 || !strings.Contains(archived.UndoCommands[0], "--to inbox --id "+archived.NewID) {
			t.Fatalf("archive result = %+v", archived)
		}
		var back struct {
			NewID string `json:"new_id"`
		}
		c.ok(t, &back, strings.Fields(archived.UndoCommands[0])[1:]...)
		c.ok(t, &p, "search", "subject:\"Contract draft\"", "--folder", "INBOX")
		if len(p.Messages) != 1 || p.Messages[0].ID != back.NewID {
			t.Fatalf("after the undo, INBOX has %v", subjects(p.Messages))
		}
		c.ok(t, nil, "archive", "--id", back.NewID)
		c.ok(t, &p, "search", "subject:\"Contract draft\"", "--folder", "Archive")
		if len(p.Messages) != 1 {
			t.Fatalf("archived copy not in Archive: %v", subjects(p.Messages))
		}
		c.ok(t, nil, "delete", "--id", p.Messages[0].ID)
		c.ok(t, &p, "search", "subject:\"Contract draft\"", "--folder", "Trash")
		if len(p.Messages) != 1 {
			t.Fatalf("deleted message not in Trash: %v", subjects(p.Messages))
		}
		c.ok(t, nil, "restore", "--id", p.Messages[0].ID, "--to", "INBOX")
		c.ok(t, nil, "archive", "--query", "from:deals@promo.example", "--all", "--apply")
		c.ok(t, &p, "search", "from:deals@promo.example", "--folder", "Archive")
		if len(p.Messages) != promoCount {
			t.Fatalf("bulk archive moved %d of %d", len(p.Messages), promoCount)
		}
	})

	t.Run("short ids", func(t *testing.T) {
		res := c.run(t, "search", "kickoff")
		short := regexp.MustCompile(`(?m)^\S*\s+([0-9a-f]{7})\s+.*Project kickoff`).FindStringSubmatch(res.stdout)
		if res.exit != 0 || short == nil || !strings.Contains(res.stdout, "ID       FROM") {
			t.Fatalf("search table:\n%s%s", res.stdout, res.stderr)
		}
		read := c.run(t, "read", short[1])
		if read.exit != 0 || !strings.Contains(read.stdout, "kickoff") {
			t.Fatalf("read %s:\n%s%s", short[1], read.stdout, read.stderr)
		}
		flag := c.run(t, "flag", "set", short[1])
		if flag.exit != 0 || !strings.HasPrefix(flag.stdout, "Flagged ") {
			t.Fatalf("flag set %s:\n%s%s", short[1], flag.stdout, flag.stderr)
		}
		c.run(t, "flag", "clear", short[1])
	})

	t.Run("apply commands change only what the preview showed", func(t *testing.T) {
		admin := dialAdmin(t, srv, caFile, user)
		defer admin.Close()
		stamp := fmt.Sprintf("bound%d", time.Now().UnixNano())
		now := time.Now()
		add := func(n string) *imapclient.AppendCommand {
			return appendRaw(t, admin, "INBOX", msg("Sender <bound@example.test>", stamp+" "+n, now.Format(time.RFC1123Z), "<"+stamp+n+"@example.test>", "", plainBody(n)), now)
		}
		wait(t, []*imapclient.AppendCommand{add("one"), add("two")})
		var preview struct {
			Count        int    `json:"count"`
			ApplyCommand string `json:"apply_command"`
		}
		c.ok(t, &preview, "archive", "--query", "subject:"+stamp, "--all")
		if preview.Count != 2 || strings.Contains(preview.ApplyCommand, "--query") || !strings.Contains(preview.ApplyCommand, "--account it") {
			t.Fatalf("preview = %+v", preview)
		}
		// Mail that matches arrives between preview and apply.
		wait(t, []*imapclient.AppendCommand{add("late")})
		args := strings.Fields(preview.ApplyCommand)
		if res := c.run(t, args[1:]...); res.exit != 0 {
			t.Fatalf("apply: exit %d\n%s%s", res.exit, res.stdout, res.stderr)
		}
		var inbox page
		c.ok(t, &inbox, "search", "subject:"+stamp, "--folder", "INBOX")
		if len(inbox.Messages) != 1 || inbox.Messages[0].Subject != stamp+" late" {
			t.Fatalf("left in INBOX: %v", subjects(inbox.Messages))
		}

		// A short id of a message that moved is refused, not pointed at
		// another message.
		res := c.run(t, "search", "subject:"+stamp+" late")
		short := regexp.MustCompile(`(?m)^\S*\s+([0-9a-f]{7,11})\s+`).FindStringSubmatch(res.stdout)
		if short == nil {
			t.Fatalf("search table:\n%s%s", res.stdout, res.stderr)
		}
		if res := c.run(t, "archive", short[1]); res.exit != 0 {
			t.Fatalf("archive %s: %s%s", short[1], res.stdout, res.stderr)
		}
		if res := c.run(t, "archive", short[1]); res.exit == 0 {
			t.Fatalf("archive of a moved message by its old short id succeeded:\n%s", res.stdout)
		}

		// restore --query looks in Trash.
		c.ok(t, nil, "delete", "--query", "subject:"+stamp, "--folder", "Archive", "--latest")
		var restore struct {
			Targets []row `json:"targets"`
		}
		c.ok(t, &restore, "restore", "--query", "subject:"+stamp, "--latest", "--dry-run")
		if len(restore.Targets) != 1 || restore.Targets[0].Folder.Path != "Trash" {
			t.Fatalf("restore preview = %+v", restore.Targets)
		}
	})

	t.Run("queries that change mail stay out of Sent", func(t *testing.T) {
		var preview struct {
			Targets []row `json:"targets"`
		}
		c.ok(t, &preview, "archive", "--query", "kickoff", "--all")
		if len(preview.Targets) == 0 {
			t.Fatal("no targets")
		}
		for _, target := range preview.Targets {
			if target.Folder.Path == "Sent" || target.Folder.Path == "Drafts" {
				t.Fatalf("archive --query reached %s: %+v", target.Folder.Path, preview.Targets)
			}
		}
	})

	t.Run("drafts, replies and forwards", func(t *testing.T) {
		rcpt := fmt.Sprintf("draft-%d@example.test", time.Now().UnixNano())
		var created, updated struct {
			DraftID string `json:"draft_id"`
		}
		c.ok(t, &created, "draft", "create", "--to", rcpt, "--subject", "Draft subject", "--body", "First draft body")
		var first struct {
			ApprovalCode string `json:"approval_code"`
		}
		c.ok(t, &first, "draft", "send", "--id", created.DraftID, "--dry-run")
		c.ok(t, &updated, "draft", "update", "--id", created.DraftID, "--body", "Updated draft body")
		if updated.DraftID == "" || updated.DraftID == created.DraftID {
			t.Fatalf("draft ids: created %q, updated %q", created.DraftID, updated.DraftID)
		}
		// The approval of the first version never sends the update: the
		// update is a new message with a new id.
		if old := c.run(t, "draft", "send", "--id", created.DraftID, "--approve", first.ApprovalCode, "--yes", "--json"); old.exit == 0 && strings.Contains(old.stdout, "Updated") {
			t.Fatalf("the old approval sent the update: %s", old.stdout)
		}
		var current struct {
			ApprovalCode string `json:"approval_code"`
		}
		c.ok(t, &current, "draft", "send", "--id", updated.DraftID, "--dry-run")
		if body := c.fails(t, "send", "--to", rcpt, "--subject", "Other", "--body", "x", "--approve", current.ApprovalCode, "--yes"); body["code"] != "approval_mismatch" {
			t.Fatalf("another message with this code: %v", body)
		}
		c.approved(t, "draft", "send", "--id", updated.DraftID)
		waitForMessage(t, srv, caFile, rcpt, "Updated draft body")
		var drafts, sent page
		c.ok(t, &drafts, "list", "Drafts")
		c.ok(t, &sent, "list", "Sent")
		if containsSubject(drafts.Messages, "Draft subject") || !containsSubject(sent.Messages, "Draft subject") {
			t.Fatalf("after draft send: Drafts %v, Sent %v", subjects(drafts.Messages), subjects(sent.Messages))
		}

		var reply struct {
			DraftID string `json:"draft_id"`
			Subject string `json:"subject"`
			To      []struct {
				Address string `json:"address"`
			} `json:"to"`
		}
		c.ok(t, &reply, "reply", "--internet-id", seed.kickoffID, "--body", "Tuesday works.")
		if reply.DraftID == "" || reply.Subject != "Re: Project kickoff" || len(reply.To) != 1 || reply.To[0].Address != "client@client.test" {
			t.Fatalf("reply draft = %+v", reply)
		}

		fwd := fmt.Sprintf("forward-%d@example.test", time.Now().UnixNano())
		c.approved(t, "forward", "--query", "has:attachment from:billing@vendor.example", "--latest", "--to", fwd, "--body", "Forwarding the invoice.", "--include-attachments", "all", "--send")
		got := waitForMessage(t, srv, caFile, fwd, "Forwarding the invoice.")
		if !strings.Contains(got, "invoice-2026-104.pdf") || !strings.Contains(got, "Subject: Fwd: Invoice 2026-104") {
			t.Fatalf("forwarded message:\n%s", got)
		}
	})

	// The dogfood case: reply to the latest mail of a conversation that
	// already has your own reply in Sent. The preview shows the thread
	// headers, and thread finds the new reply too.
	t.Run("a sent reply joins its thread", func(t *testing.T) {
		var preview struct {
			Message struct {
				CC          []any    `json:"cc"`
				BCC         []any    `json:"bcc"`
				ReplyTo     []any    `json:"reply_to"`
				Attachments []any    `json:"attachments"`
				InReplyTo   string   `json:"in_reply_to"`
				References  []string `json:"references"`
			} `json:"message"`
		}
		args := []string{"reply", "--internet-id", seed.lastReplyID, "--body", "See you on Tuesday.", "--send"}
		c.ok(t, &preview, append(args, "--dry-run")...)
		m := preview.Message
		if m.CC == nil || m.BCC == nil || m.ReplyTo == nil || m.Attachments == nil || m.InReplyTo != seed.lastReplyID ||
			strings.Join(m.References, " ") != "<kickoff@client.test> <reply1@example.test> "+seed.lastReplyID {
			t.Fatalf("reply preview = %+v", m)
		}
		c.approved(t, args...)
		var thread struct {
			Thread struct {
				Messages []row `json:"messages"`
			} `json:"thread"`
		}
		c.ok(t, &thread, "thread", "--internet-id", seed.kickoffID)
		found := map[string]bool{}
		for _, m := range thread.Thread.Messages {
			found[m.InternetMessageID] = true
			if m.Folder.Path == "Sent" && strings.Contains(m.Preview, "See you on Tuesday") {
				found["new reply"] = true
			}
		}
		if !found[seed.kickoffID] || !found["<reply1@example.test>"] || !found[seed.lastReplyID] || !found["new reply"] {
			t.Fatalf("thread = %v", subjects(thread.Thread.Messages))
		}
	})

	t.Run("send", func(t *testing.T) {
		rcpt := fmt.Sprintf("rcpt-%d@example.test", time.Now().UnixNano())
		if body := c.fails(t, "send", "--yes", "--to", rcpt, "--subject", "Status update", "--body", "x"); body["code"] != "confirmation_required" {
			t.Fatalf("--yes without --approve: %v", body)
		}
		args := []string{"send", "--to", rcpt, "--subject", "Status update – Grüße", "--body", "Hello from the integration test."}
		code := c.approved(t, args...)
		got := waitForMessage(t, srv, caFile, rcpt, "Hello from the integration test.")
		if !strings.Contains(got, "Hello from the integration test.") || strings.Contains(strings.ToLower(got), "\nbcc:") {
			t.Fatalf("delivered message:\n%s", got)
		}
		// The same approved command a second time sends nothing.
		if body := c.fails(t, append(args, "--approve", code, "--yes")...); body["code"] != "approval_used" {
			t.Fatalf("second run of the approved command: %v", body)
		}
	})
}

const (
	bulkCount  = 2500
	dirtyCount = 6
	promoCount = 15
)

type seeded struct {
	inboxCount  int
	kickoffID   string
	lastReplyID string
}

func dialAdmin(t *testing.T, srv server, caFile, user string) *imapclient.Client {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	conn, err := tls.Dial("tcp", "localhost:"+srv.imapPort, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	if err := c.Login(user, srv.password).Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

// appendRaw sends an APPEND without waiting for the answer, so many appends
// travel together. Call wait to collect the answers.
func appendRaw(t *testing.T, c *imapclient.Client, mailbox, raw string, when time.Time, flags ...goimap.Flag) *imapclient.AppendCommand {
	t.Helper()
	data := []byte(strings.ReplaceAll(raw, "\n", "\r\n"))
	cmd := c.Append(mailbox, int64(len(data)), &goimap.AppendOptions{Flags: flags, Time: when})
	if _, err := io.Copy(cmd, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func wait(t *testing.T, cmds []*imapclient.AppendCommand) {
	t.Helper()
	for _, cmd := range cmds {
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func msg(from, subject, date, id, extra, body string) string {
	return fmt.Sprintf("From: %s\nTo: Me <me@example.test>\nSubject: %s\nDate: %s\nMessage-ID: %s\n%sMIME-Version: 1.0\n", from, subject, date, id, extra) + body
}

func plainBody(text string) string {
	return "Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: 8bit\n\n" + text + "\n"
}

// seedMailbox fills a fresh mailbox with the kinds of mail real inboxes have.
func seedMailbox(t *testing.T, srv server, caFile, user string) seeded {
	t.Helper()
	c := dialAdmin(t, srv, caFile, user)
	defer c.Close()
	delim := "/"
	if list, err := c.List("", "", nil).Collect(); err == nil && len(list) > 0 && list[0].Delim != 0 {
		delim = string(list[0].Delim)
	}
	existing := map[string]bool{}
	if list, err := c.List("", "*", nil).Collect(); err == nil {
		for _, item := range list {
			existing[item.Mailbox] = true
		}
	}
	for _, name := range []string{"Archive", "Trash", "Junk", "Sent", "Drafts", "Clients", "Clients" + delim + "ACME", "Newsletters", "Receipts 2026", "Dirty", "Bulk"} {
		if !existing[name] {
			if err := c.Create(name, nil).Wait(); err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}
	}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) (time.Time, string) {
		ts := base.Add(d)
		return ts, ts.Format(time.RFC1123Z)
	}
	inbox := 0
	// UIDs follow the order of appends, and the Message-ID test needs the
	// Trash messages to come first, so each append is waited for here. Only
	// the bulk folder below is pipelined.
	add := func(mailbox, raw string, when time.Time, flags ...goimap.Flag) {
		wait(t, []*imapclient.AppendCommand{appendRaw(t, c, mailbox, raw, when, flags...)})
		if mailbox == "INBOX" {
			inbox++
		}
	}
	clara := "Clara Client <client@client.test>"

	// Trash first, so the kickoff message's UID in INBOX also exists in Trash.
	for i := 0; i < 4; i++ {
		when, date := at(time.Duration(i) * time.Minute)
		add("Trash", msg("x@example.net", fmt.Sprintf("Trashed order %d", i), date, fmt.Sprintf("<trash%d@example.net>", i), "", plainBody("WRONG BODY FROM TRASH: trashed order")), when)
	}
	when, date := at(0)
	add("INBOX", msg(clara, "Contract draft for October", date, "<contract@client.test>", "", plainBody("Hi,\n\nplease find my notes on the contract.\n\n1. Payment terms\n2. Delivery\n\nThanks,\nClara")), when)
	when, date = at(time.Hour)
	add("Newsletters", msg("Weekly News <news@news.example.com>", "Your weekly digest", date, "<digest@news.example.com>", "",
		"Content-Type: text/html; charset=utf-8\n\n<html><body><h1>Weekly Digest</h1><p>Hello <b>reader</b>, here are this week's <a href=\"https://news.example.com/a\">top stories</a>.</p></body></html>\n"), when)
	when, date = at(2 * time.Hour)
	add("INBOX", msg("Bob Builder <bob@builder.example>", "Site visit on Friday", date, "<site@builder.example>", "",
		"Content-Type: multipart/alternative; boundary=\"alt\"\n\n--alt\nContent-Type: text/plain; charset=utf-8\n\nCan we meet at the site on Friday at 10?\n--alt\nContent-Type: text/html; charset=utf-8\n\n<p>Can we meet at the site on <b>Friday at 10</b>?</p>\n--alt--\n"), when, goimap.FlagSeen, goimap.FlagFlagged)
	when, date = at(24 * time.Hour)
	pdf := "%PDF-1.4\n%fake pdf for testing\n" + strings.Repeat("0", 2048)
	add("INBOX", msg("Vendor Billing <billing@vendor.example>", "Invoice 2026-104", date, "<invoice@vendor.example>", "",
		"Content-Type: multipart/mixed; boundary=\"mix\"\n\n--mix\nContent-Type: text/plain\n\nYour invoice is attached.\n--mix\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=\"invoice-2026-104.pdf\"\nContent-Transfer-Encoding: base64\n\n"+
			base64Lines(pdf)+"--mix\nContent-Type: text/csv\nContent-Disposition: attachment; filename=\"line-items.csv\"\n\ndate,amount\n2026-10-02,119.00\n--mix--\n"), when)
	when, date = at(48 * time.Hour)
	add("INBOX", msg(clara, "Project kickoff", date, "<kickoff@client.test>", "", plainBody("Shall we kick off next week?")), when, goimap.FlagSeen)
	when, date = at(49 * time.Hour)
	add("Sent", msg("Me <me@example.test>", "Re: Project kickoff", date, "<reply1@example.test>", "References: <kickoff@client.test>\nIn-Reply-To: <kickoff@client.test>\n", plainBody("Yes, Tuesday works.\n\n> Shall we kick off next week?")), when, goimap.FlagSeen)
	when, date = at(51 * time.Hour)
	add("INBOX", msg(clara, "Re: Project kickoff", date, "<reply2@client.test>", "References: <kickoff@client.test> <reply1@example.test>\nIn-Reply-To: <reply1@example.test>\n", plainBody("Great, Tuesday 10:00 then.")), when)
	when, date = at(72 * time.Hour)
	add("INBOX", msg("Jürgen Müller <juergen@beispiel.de>", "=?UTF-8?Q?Gr=C3=BC=C3=9Fe_aus_M=C3=BCnchen_=E2=80=93_Angebot_f=C3=BCr_Stra=C3=9Fe_5_=F0=9F=9A=80?=", date, "<angebot@beispiel.de>", "",
		plainBody("Hallo,\n\nanbei das Angebot. Größe: 5 m². Preis: 1.200 €.\n日本語のテキスト\n\nViele Grüße\nJürgen")), when)
	when, date = at(96 * time.Hour)
	add("INBOX", msg(clara, "Screenshot of the bug", date, "<shot@client.test>", "",
		"Content-Type: multipart/mixed; boundary=\"m\"\n\n--m\nContent-Type: text/plain\n\nSee screenshot.\n--m\nContent-Type: text/plain\nContent-Disposition: attachment; filename=\"../../etc/passwd\"\n\nnot really a passwd\n--m--\n"), when)
	when, date = at(6 * 24 * time.Hour)
	add("Clients"+delim+"ACME", msg("Ann Acme <ann@acme.example>", "Quarterly review: "+strings.Repeat("very long subject line that keeps going ", 4), date, "<review@acme.example>", "Cc: "+clara+"\n", plainBody("Agenda attached later.")), when)
	when, date = at(7 * 24 * time.Hour)
	add("Receipts 2026", msg("Shop <orders@shop.example>", "Your order #4411", date, "<order@shop.example>", "", plainBody("Order total 42.00 EUR")), when, goimap.FlagSeen)
	for i := 0; i < promoCount; i++ {
		when, date := at(8*24*time.Hour + time.Duration(i)*time.Minute)
		add("Newsletters", msg("Promo Deals <deals@promo.example>", fmt.Sprintf("Deal of the day #%d", i+1), date, fmt.Sprintf("<deal%d@promo.example>", i), "", plainBody(fmt.Sprintf("Deal %d: 50%% off.", i+1))), when)
	}
	for i, raw := range dirtyMessages() {
		when, _ := at(time.Duration(i) * time.Hour)
		add("Dirty", raw, when)
	}
	when, date = at(10 * 24 * time.Hour)
	add("Dirty", msg("clean@example.net", "Clean invoice", date, "<clean@example.net>", "", plainBody("invoice inside")), when)
	var pending []*imapclient.AppendCommand
	for i := 0; i < bulkCount; i++ {
		when, date := at(time.Duration(i) * time.Minute)
		if len(pending) == 200 {
			wait(t, pending)
			pending = nil
		}
		pending = append(pending, appendRaw(t, c, "Bulk", fmt.Sprintf("From: sender%d@bulk.example\nTo: me@example.test\nSubject: Bulk message %d about topic%d\nDate: %s\nMessage-ID: <bulk%d@bulk.example>\n\nBody of bulk message %d. keyword%d\n", i%50, i, i%7, date, i, i, i%100), when))
	}
	wait(t, pending)
	return seeded{inboxCount: inbox, kickoffID: "<kickoff@client.test>", lastReplyID: "<reply2@client.test>"}
}

func dirtyMessages() []string {
	out := []string{
		"From: shop@old.example\nTo: me@example.test\nSubject: Gr\xfc\xdfe raw latin1\nDate: Mon, 12 Oct 2026 10:00:00 +0000\nMessage-ID: <d1@old.example>\n\nbody\n",
		"From: a@b.example\nTo: me@example.test\nThis line has no colon\nSubject: garbage header line\nDate: Mon, 12 Oct 2026 11:00:00 +0000\nMessage-ID: <d3@b.example>\n\nbody\n",
		"From: a@b.example\nTo: me@example.test\nSubject: bad date\nDate: Mon, 32 Foo 2026 25:61:00 +9999\nMessage-ID: <d4@b.example>\n\nbody\n",
		"From: =?x-foo?Q?Weird?= <w@b.example>\nTo: me@example.test\nSubject: =?x-unknown?B?SGVsbG8=?= charset\nDate: Mon, 12 Oct 2026 12:00:00 +0000\nMessage-ID: <d5@b.example>\nContent-Type: text/plain; charset=x-unknown\n\nbody bytes \xe9\n",
		"From: a@b.example\nTo: me@example.test\nSubject: broken base64\nDate: Mon, 12 Oct 2026 13:00:00 +0000\nMessage-ID: <d6@b.example>\nMIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\n@@@not base64!!!\n",
	}
	var many []string
	for i := 0; i < 250; i++ {
		many = append(many, fmt.Sprintf("u%d@b.example", i))
	}
	out = append(out, "From: a@b.example\nTo: "+strings.Join(many, ", ")+"\nSubject: 250 recipients\nDate: Mon, 12 Oct 2026 17:00:00 +0000\nMessage-ID: <d10@b.example>\n\nbody\n")
	return out
}

func base64Lines(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var out strings.Builder
	for len(enc) > 76 {
		out.WriteString(enc[:76] + "\n")
		enc = enc[76:]
	}
	out.WriteString(enc + "\n")
	return out.String()
}

// waitForMessage polls GreenMail, which delivers all mail sent through its
// SMTP server, until rcpt has a message containing text.
func waitForMessage(t *testing.T, srv server, caFile, rcpt, text string) string {
	t.Helper()
	greenmail := server{imapPort: os.Getenv("MAILDO_IT_GREENMAIL_IMAPS"), password: "any"}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c := dialAdmin(t, greenmail, caFile, rcpt)
		data, err := c.Select("INBOX", nil).Wait()
		if err == nil && data.NumMessages > 0 {
			section := &goimap.FetchItemBodySection{Peek: true}
			msgs, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
			c.Close()
			if err == nil && len(msgs) == 1 {
				raw := string(msgs[0].FindBodySection(section))
				if strings.Contains(raw, text) {
					return raw
				}
			}
		} else {
			c.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no message for %s arrived", rcpt)
	return ""
}

func containsSubject(rows []row, subject string) bool {
	for _, r := range rows {
		if r.Subject == subject {
			return true
		}
	}
	return false
}

// attachmentNames reads the file names of a delivered message's
// attachments the way other mail programs read them.
func attachmentNames(t *testing.T, raw string) []string {
	t.Helper()
	m, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	var names []string
	reader := multipart.NewReader(m.Body, params["boundary"])
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
