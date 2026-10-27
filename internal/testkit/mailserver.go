package testkit

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// MailServer runs a real IMAP server (go-imap's in-memory server) and an
// SMTP submission server in the test process, both over TLS. CLI tests point
// the imap provider at it through Dial and TLSConfig, so they exercise the
// same protocol code as a real mailbox, offline.
type MailServer struct {
	t        testing.TB
	imapAddr string
	smtpAddr string
	user     string
	password string

	mu   sync.Mutex
	sent []SentMail
}

// SentMail is one message the SMTP server accepted.
type SentMail struct {
	From string
	To   []string
	Data []byte
}

// StartMailServer starts both servers with one user and the given extra
// mailboxes (INBOX always exists). They stop when the test ends.
func StartMailServer(t testing.TB, user, password string, mailboxes ...string) *MailServer {
	t.Helper()
	cert := selfSignedCert(t)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s := &MailServer{t: t, user: user, password: password}

	mem := imapmemserver.New()
	u := imapmemserver.NewUser(user, password)
	for _, name := range append([]string{"INBOX"}, mailboxes...) {
		if err := u.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(u)
	imapSrv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1: {}, goimap.CapUIDPlus: {}, goimap.CapMove: {},
			goimap.CapESearch: {}, goimap.CapListStatus: {}, goimap.CapNamespace: {},
		},
	})
	imapLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = imapSrv.Serve(tls.NewListener(imapLn, tlsConfig)) }()
	s.imapAddr = imapLn.Addr().String()

	smtpSrv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &smtpSession{server: s}, nil
	}))
	smtpSrv.Domain = "localhost"
	smtpSrv.ReadTimeout = 10 * time.Second
	smtpSrv.WriteTimeout = 10 * time.Second
	smtpSrv.MaxMessageBytes = 30 << 20
	smtpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = smtpSrv.Serve(tls.NewListener(smtpLn, tlsConfig)) }()
	s.smtpAddr = smtpLn.Addr().String()

	t.Cleanup(func() {
		_ = imapSrv.Close()
		_ = smtpSrv.Close()
	})
	return s
}

// Dial connects to the IMAP server for ports 993 and 143 and to the SMTP
// server for everything else, whatever host was asked for.
func (s *MailServer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, _ := net.SplitHostPort(address)
	target := s.smtpAddr
	if port == "993" || port == "143" {
		target = s.imapAddr
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}

// TLSConfig trusts the servers' throwaway certificate.
func (s *MailServer) TLSConfig(serverName string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // test server
}

// Sent returns the messages accepted over SMTP so far.
func (s *MailServer) Sent() []SentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SentMail(nil), s.sent...)
}

// Append stores a raw message (LF line ends are fine) and returns its UID.
func (s *MailServer) Append(mailbox, raw string, received time.Time, flags ...goimap.Flag) uint32 {
	s.t.Helper()
	c := s.admin()
	defer c.Close()
	data := []byte(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n"))
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

// Fetch returns the raw messages in a mailbox, oldest first.
func (s *MailServer) Fetch(mailbox string) [][]byte {
	s.t.Helper()
	c := s.admin()
	defer c.Close()
	data, err := c.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		s.t.Fatal(err)
	}
	if data.NumMessages == 0 {
		return nil
	}
	section := &goimap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(goimap.SeqSetNum(seqRange(data.NumMessages)...), &goimap.FetchOptions{BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		s.t.Fatal(err)
	}
	out := make([][]byte, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, msg.FindBodySection(section))
	}
	return out
}

func seqRange(n uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = uint32(i + 1)
	}
	return out
}

func (s *MailServer) admin() *imapclient.Client {
	s.t.Helper()
	conn, err := tls.Dial("tcp", s.imapAddr, s.TLSConfig(""))
	if err != nil {
		s.t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	if err := c.Login(s.user, s.password).Wait(); err != nil {
		s.t.Fatal(err)
	}
	return c
}

type smtpSession struct {
	server *MailServer
	authed bool
	from   string
	to     []string
}

func (ss *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (ss *smtpSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != ss.server.user || password != ss.server.password {
			return errors.New("invalid credentials")
		}
		ss.authed = true
		return nil
	}), nil
}

func (ss *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if !ss.authed {
		return smtp.ErrAuthRequired
	}
	ss.from = from
	return nil
}

func (ss *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	ss.to = append(ss.to, to)
	return nil
}

func (ss *smtpSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	ss.server.mu.Lock()
	ss.server.sent = append(ss.server.sent, SentMail{From: ss.from, To: append([]string(nil), ss.to...), Data: data})
	ss.server.mu.Unlock()
	return nil
}

func (ss *smtpSession) Reset() {
	ss.from = ""
	ss.to = nil
}

func (ss *smtpSession) Logout() error { return nil }

func selfSignedCert(t testing.TB) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
