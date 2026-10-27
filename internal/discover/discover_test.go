package discover

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/config"
)

type fakeResolver struct {
	srv map[string]*net.SRV
	mx  map[string]string
}

func (r fakeResolver) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	if rec, ok := r.srv["_"+service+"._"+proto+"."+name]; ok {
		return "", []*net.SRV{rec}, nil
	}
	return "", nil, errors.New("no such host")
}

func (r fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if host, ok := r.mx[name]; ok {
		return []*net.MX{{Host: host, Pref: 10}}, nil
	}
	return nil, errors.New("no such host")
}

// offline routes every HTTP request to handler and makes every probe fail,
// so a test only sees the network it sets up.
func offline(handler http.HandlerFunc, resolver fakeResolver) Options {
	srv := httptest.NewServer(handler)
	client := srv.Client()
	client.Transport = rewrite{target: srv.URL, base: http.DefaultTransport}
	return Options{
		HTTP:     client,
		Resolver: resolver,
		Probe:    func(context.Context, string, uint16, bool) error { return errors.New("refused") },
		ISPDBURL: "https://ispdb.test/v1.1/",
	}
}

// rewrite sends every request to the test server, keeping the original
// host in a header so the handler can tell requests apart.
type rewrite struct {
	target string
	base   http.RoundTripper
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Original-Host", req.URL.Host)
	u := *req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(r.target, "http://")
	out.URL = &u
	return r.base.RoundTrip(out)
}

const sampleConfig = `<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.org">
    <displayName>Example Mail</displayName>
    <incomingServer type="pop3"><hostname>pop.example.org</hostname><port>995</port><socketType>SSL</socketType></incomingServer>
    <incomingServer type="imap"><hostname>imap.example.org</hostname><port>143</port><socketType>plain</socketType><username>%EMAILADDRESS%</username></incomingServer>
    <incomingServer type="imap"><hostname>imap.%EMAILDOMAIN%</hostname><port>993</port><socketType>SSL</socketType><username>%EMAILLOCALPART%</username></incomingServer>
    <outgoingServer type="smtp"><hostname>smtp.example.org</hostname><port>587</port><socketType>STARTTLS</socketType></outgoingServer>
  </emailProvider>
</clientConfig>`

func TestPresetsNeedNoNetwork(t *testing.T) {
	for email, want := range map[string]string{
		"me@gmail.com":     "imap.gmail.com",
		"me@ICLOUD.com":    "imap.mail.me.com",
		"me@yahoo.co.uk":   "imap.mail.yahoo.com",
		"me@posteo.de":     "posteo.de",
		"me@gmx.de":        "imap.gmx.net",
		"me@web.de":        "imap.web.de",
		"me@mailbox.org":   "imap.mailbox.org",
		"me@fastmail.com":  "imap.fastmail.com",
		"me@hotmail.com":   "",
		"me@example.local": "-",
	} {
		s, err := Discover(context.Background(), email, Options{SkipNetwork: true})
		if want == "-" {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: err = %v, want not found", email, err)
			}
			continue
		}
		if err != nil || s.IMAP.Host != want || s.Source != "built-in" || s.PasswordHint == "" {
			t.Fatalf("%s: %+v, %v", email, s, err)
		}
	}
	if s, _ := PresetFor("me@hotmail.com"); s.Provider != "outlook" {
		t.Fatalf("hotmail provider = %q, want outlook", s.Provider)
	}
	if s, _ := PresetFor("me@fastmail.com"); s.Provider != "fastmail" {
		t.Fatalf("fastmail provider = %q", s.Provider)
	}
}

func TestAutoconfigFromTheDomainWins(t *testing.T) {
	var asked []string
	opts := offline(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Header.Get("X-Original-Host")+r.URL.Path)
		if r.Header.Get("X-Original-Host") == "autoconfig.example.org" && r.URL.Query().Get("emailaddress") == "ann@example.org" {
			_, _ = w.Write([]byte(sampleConfig))
			return
		}
		http.NotFound(w, r)
	}, fakeResolver{})
	s, err := Discover(context.Background(), "ann@example.org", opts)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		Provider: "imap", Service: "Example Mail", Username: "ann", Source: "autoconfig",
		IMAP: config.ServerEndpoint{Host: "imap.example.org", Port: 993, TLS: config.TLSImplicit},
		SMTP: config.ServerEndpoint{Host: "smtp.example.org", Port: 587, TLS: config.TLSStartTLS},
	}
	if s != want {
		t.Fatalf("settings = %+v\nwant %+v (asked %v)", s, want, asked)
	}
}

func TestISPDBThenSRVThenMX(t *testing.T) {
	ispdb := offline(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Original-Host") == "ispdb.test" && strings.HasSuffix(r.URL.Path, "/example.net") {
			_, _ = w.Write([]byte(strings.ReplaceAll(sampleConfig, "Example Mail", "From ISPDB")))
			return
		}
		http.NotFound(w, r)
	}, fakeResolver{})
	if s, err := Discover(context.Background(), "bo@example.net", ispdb); err != nil || s.Source != "ispdb" || s.Service != "From ISPDB" {
		t.Fatalf("ispdb = %+v, %v", s, err)
	}

	srv := offline(http.NotFound, fakeResolver{srv: map[string]*net.SRV{
		"_imaps._tcp.example.com":       {Target: "mail.example.com.", Port: 993},
		"_submissions._tcp.example.com": {Target: ".", Port: 0},
		"_submission._tcp.example.com":  {Target: "mail.example.com.", Port: 587},
	}})
	s, err := Discover(context.Background(), "cy@example.com", srv)
	if err != nil || s.Source != "dns-srv" || s.IMAP != (config.ServerEndpoint{Host: "mail.example.com", Port: 993, TLS: config.TLSImplicit}) ||
		s.SMTP != (config.ServerEndpoint{Host: "mail.example.com", Port: 587, TLS: config.TLSStartTLS}) {
		t.Fatalf("srv = %+v, %v", s, err)
	}

	mx := offline(http.NotFound, fakeResolver{mx: map[string]string{"acme.example": "acme-example.mail.protection.outlook.com."}})
	if s, err := Discover(context.Background(), "di@acme.example", mx); err != nil || s.Provider != "outlook" || s.Source != "mx" {
		t.Fatalf("mx = %+v, %v", s, err)
	}
	workspace := offline(http.NotFound, fakeResolver{mx: map[string]string{"shop.example": "aspmx.l.google.com."}})
	if s, err := Discover(context.Background(), "ed@shop.example", workspace); err != nil || s.IMAP.Host != "imap.gmail.com" || s.Username != "ed@shop.example" {
		t.Fatalf("workspace mx = %+v, %v", s, err)
	}
}

func TestGuessOnlyWhenTheServerAnswers(t *testing.T) {
	opts := offline(http.NotFound, fakeResolver{})
	if _, err := Discover(context.Background(), "fi@quiet.example", opts); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	opts.Probe = func(_ context.Context, host string, port uint16, _ bool) error {
		if (host == "mail.quiet.example" && port == 993) || (host == "smtp.quiet.example" && port == 587) {
			return nil
		}
		return errors.New("refused")
	}
	s, err := Discover(context.Background(), "fi@quiet.example", opts)
	if err != nil || s.Source != "guess" || s.IMAP.Host != "mail.quiet.example" || s.SMTP.Port != 587 || s.SMTP.TLS != config.TLSStartTLS {
		t.Fatalf("guess = %+v, %v", s, err)
	}
}

func TestPlainOnlyServersAreIgnored(t *testing.T) {
	plainOnly := strings.ReplaceAll(sampleConfig, "<socketType>SSL</socketType><username>%EMAILLOCALPART%", "<socketType>plain</socketType><username>%EMAILLOCALPART%")
	if _, ok := parseClientConfig([]byte(plainOnly), "a@example.org"); ok {
		t.Fatal("a configuration with only plain IMAP was accepted")
	}
}
