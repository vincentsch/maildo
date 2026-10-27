package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/mail"
)

// memServer is a real IMAP server from go-imap, running in the test process
// over TLS. Unlike scripted fakes it answers SEARCH, FETCH and SELECT the way
// a server does, including "no mailbox selected" errors. It has no
// SPECIAL-USE, like GreenMail, so maildo finds Trash and Sent by name.
type memServer struct {
	t        *testing.T
	listener net.Listener
	server   *imapserver.Server
	commands *commandLog
}

// commandLog records the IMAP commands clients send, for tests that check
// how many round trips an operation costs.
type commandLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *commandLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] != "*" && fields[0] != "+" && !strings.HasPrefix(fields[1], "OK") && fields[1] != "NO" && fields[1] != "BAD" {
			l.lines = append(l.lines, strings.TrimSpace(line))
		}
	}
	return len(p), nil
}

func (l *commandLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		fields := strings.SplitN(line, " ", 2)
		if len(fields) == 2 && strings.HasPrefix(strings.ToUpper(fields[1]), prefix) {
			n++
		}
	}
	return n
}

func (l *commandLog) reset() {
	l.mu.Lock()
	l.lines = nil
	l.mu.Unlock()
}

func startMemServer(t *testing.T, mailboxes ...string) *memServer {
	t.Helper()
	return startMemServerWith(t, nil, mailboxes...)
}

// defaultMemCaps are the extensions the in-process server advertises.
var defaultMemCaps = goimap.CapSet{
	goimap.CapIMAP4rev1: {}, goimap.CapUIDPlus: {}, goimap.CapMove: {},
	goimap.CapESearch: {}, goimap.CapListStatus: {}, goimap.CapNamespace: {},
}

// startMemServerWith starts a memServer whose sessions pass through wrap,
// so a test can make the server misbehave the way some real servers do.
func startMemServerWith(t *testing.T, wrap func(imapserver.Session) imapserver.Session, mailboxes ...string) *memServer {
	return startMemServerFull(t, wrap, defaultMemCaps, mailboxes...)
}

// startMemServerWithCaps starts a server that advertises only caps, for
// tests of servers without an extension such as UIDPLUS.
func startMemServerWithCaps(t *testing.T, caps goimap.CapSet, mailboxes ...string) *memServer {
	return startMemServerFull(t, nil, caps, mailboxes...)
}

func startMemServerFull(t *testing.T, wrap func(imapserver.Session) imapserver.Session, caps goimap.CapSet, mailboxes ...string) *memServer {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me@example.com", "app-password")
	for _, name := range append([]string{"INBOX"}, mailboxes...) {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	log := &commandLog{}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			if wrap != nil {
				return wrap(mem.NewSession()), nil, nil
			}
			return mem.NewSession(), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
		DebugWriter:  log,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, serverTLSConfig(t))
	go func() { _ = server.Serve(tlsLn) }()
	s := &memServer{t: t, listener: tlsLn, server: server, commands: log}
	t.Cleanup(func() { _ = server.Close() })
	return s
}

func (s *memServer) options() Options {
	addr := s.listener.Addr().String()
	return Options{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSConfig: insecureTLSConfig,
	}
}

// client returns a maildo account client logged in to this server.
func (s *memServer) client() *accountClient {
	s.t.Helper()
	c := newTestAccountClient(s.t, NewProvider("imap", s.options()))
	s.t.Cleanup(c.Destroy)
	return c
}

// admin opens a plain go-imap connection for seeding and checking.
func (s *memServer) admin() *imapclient.Client {
	s.t.Helper()
	conn, err := tls.Dial("tcp", s.listener.Addr().String(), insecureTLSConfig(""))
	if err != nil {
		s.t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	if err := c.Login("me@example.com", "app-password").Wait(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = c.Close() })
	return c
}

// add appends a raw message and returns its UID.
func (s *memServer) add(mailbox string, raw string, received time.Time, flags ...goimap.Flag) uint32 {
	s.t.Helper()
	c := s.admin()
	defer c.Logout()
	data := []byte(strings.ReplaceAll(raw, "\n", "\r\n"))
	cmd := c.Append(mailbox, int64(len(data)), &goimap.AppendOptions{Flags: flags, Time: received})
	if _, err := io.Copy(cmd, bytes.NewReader(data)); err != nil {
		s.t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		s.t.Fatal(err)
	}
	result, err := cmd.Wait()
	if err != nil {
		s.t.Fatal(err)
	}
	return uint32(result.UID)
}

// testMessage builds a small RFC 5322 message.
type testMessage struct {
	from, to, cc, subject, date, messageID, references, inReplyTo string
	contentType, body, extraHeaders                               string
}

func (m testMessage) raw() string {
	var b strings.Builder
	write := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, value)
		}
	}
	write("From", m.from)
	write("To", orDefault(m.to, "Me <me@example.com>"))
	write("Cc", m.cc)
	write("Subject", m.subject)
	write("Date", orDefault(m.date, "Mon, 05 Oct 2026 09:00:00 +0000"))
	write("Message-ID", m.messageID)
	write("References", m.references)
	write("In-Reply-To", m.inReplyTo)
	b.WriteString(m.extraHeaders)
	write("MIME-Version", "1.0")
	write("Content-Type", orDefault(m.contentType, "text/plain; charset=utf-8"))
	b.WriteString("\n")
	b.WriteString(m.body)
	b.WriteString("\n")
	return b.String()
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func day(n int) time.Time {
	return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

// folderFor builds the resolved-folder value commands pass to the provider.
func folderFor(t *testing.T, name string) mail.Folder {
	t.Helper()
	id, err := formatFolderID(name)
	if err != nil {
		t.Fatal(err)
	}
	return mail.Folder{ID: id, DisplayName: name, Path: name}
}

func tlsServer(t *testing.T, conn net.Conn) net.Conn {
	t.Helper()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("imap handshake: %v", err)
	}
	return tlsConn
}

var _ = maildoconfig.TLSImplicit
