// Package discover finds the IMAP and SMTP servers for an email address, so
// logging in only needs the address and a password.
//
// It tries these in order and stops at the first that works:
//
//  1. settings built into maildo for large mail services
//  2. the domain's own autoconfig file, in Thunderbird's format
//  3. Thunderbird's public database of mail providers (the ISPDB)
//  4. DNS SRV records (RFC 6186 and RFC 8314)
//  5. the domain's MX records, which often name a known mail host
//  6. the usual host names, such as imap.<domain> and smtp.<domain>, but
//     only when a connection to them works
package discover

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/config"
)

// Settings is what discovery found for one address.
type Settings struct {
	// Provider is the maildo provider to use: "imap", "fastmail", or
	// "outlook" for Microsoft accounts, which need OAuth.
	Provider string
	// Service names the mail service for people, such as "Posteo".
	Service  string
	IMAP     config.ServerEndpoint
	SMTP     config.ServerEndpoint
	Username string
	// Source says where the settings came from: built-in, autoconfig,
	// ispdb, dns-srv, mx or guess.
	Source string
	// PasswordHint tells the user which password the service wants, for
	// example an app password, and where to create it.
	PasswordHint string
}

// Resolver is the DNS part of net.Resolver that discovery uses.
type Resolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Options replaces the network for tests. The zero value uses the real one.
type Options struct {
	HTTP     *http.Client
	Resolver Resolver
	// Probe checks that a server answers on host:port, with TLS from the
	// start when tls is true, or with a plain connection for STARTTLS ports.
	Probe func(ctx context.Context, host string, port uint16, tls bool) error
	// ISPDBURL is the base URL of Thunderbird's ISPDB.
	ISPDBURL string
	// SkipNetwork limits discovery to the built-in settings.
	SkipNetwork bool
}

// ErrNotFound means no step found settings maildo can use.
var ErrNotFound = errors.New("no mail server settings found")

const stepTimeout = 6 * time.Second

// Discover finds settings for email.
func Discover(ctx context.Context, email string, opts Options) (Settings, error) {
	local, domain, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok || local == "" || domain == "" {
		return Settings{}, fmt.Errorf("%q is not an email address", email)
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if s, ok := PresetFor(email); ok {
		return s, nil
	}
	if opts.SkipNetwork {
		return Settings{}, ErrNotFound
	}
	opts = withDefaults(opts)
	for _, step := range []func(context.Context, string, string, Options) (Settings, bool){
		fromAutoconfig, fromISPDB, fromSRV, fromMX, fromGuess,
	} {
		if err := ctx.Err(); err != nil {
			return Settings{}, err
		}
		if s, ok := step(ctx, email, domain, opts); ok {
			return s, nil
		}
	}
	return Settings{}, ErrNotFound
}

func withDefaults(opts Options) Options {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: stepTimeout}
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Probe == nil {
		opts.Probe = probe
	}
	if opts.ISPDBURL == "" {
		opts.ISPDBURL = "https://autoconfig.thunderbird.net/v1.1/"
	}
	return opts
}

// fromSRV reads the SRV records from RFC 6186 and RFC 8314: _imaps and
// _submissions for TLS from the start, _imap and _submission for STARTTLS.
func fromSRV(ctx context.Context, email, domain string, opts Options) (Settings, bool) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	imap, ok := srvEndpoint(ctx, opts.Resolver, domain, "imaps", "imap")
	if !ok {
		return Settings{}, false
	}
	smtp, ok := srvEndpoint(ctx, opts.Resolver, domain, "submissions", "submission")
	if !ok {
		return Settings{}, false
	}
	return Settings{Provider: "imap", Service: domain, IMAP: imap, SMTP: smtp, Username: email, Source: "dns-srv"}, true
}

func srvEndpoint(ctx context.Context, r Resolver, domain, implicitService, starttlsService string) (config.ServerEndpoint, bool) {
	for _, svc := range []struct {
		name string
		mode config.TLSMode
	}{{implicitService, config.TLSImplicit}, {starttlsService, config.TLSStartTLS}} {
		_, records, err := r.LookupSRV(ctx, svc.name, "tcp", domain)
		if err != nil || len(records) == 0 {
			continue
		}
		rec := records[0]
		host := strings.TrimSuffix(rec.Target, ".")
		// A target of "." says the service is not offered.
		if host == "" || rec.Port == 0 {
			continue
		}
		return config.ServerEndpoint{Host: strings.ToLower(host), Port: rec.Port, TLS: svc.mode}, true
	}
	return config.ServerEndpoint{}, false
}

// fromMX looks at the domain's MX host. It first checks it against the
// large services maildo knows, then asks the ISPDB about the MX host's
// domain.
func fromMX(ctx context.Context, email, domain string, opts Options) (Settings, bool) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	records, err := opts.Resolver.LookupMX(ctx, domain)
	if err != nil || len(records) == 0 {
		return Settings{}, false
	}
	host := strings.ToLower(strings.TrimSuffix(records[0].Host, "."))
	if s, ok := presetForMX(email, host); ok {
		s.Source = "mx"
		return s, true
	}
	mxDomain := registrableDomain(host)
	if mxDomain == "" || mxDomain == domain {
		return Settings{}, false
	}
	if s, ok := fromISPDB(ctx, email, mxDomain, opts); ok {
		s.Source = "mx"
		return s, true
	}
	return Settings{}, false
}

// registrableDomain keeps the last two labels of a host name, which is
// right for the mail hosts this is used for (mx.example.com → example.com).
func registrableDomain(host string) string {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return ""
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// fromGuess tries imap.<domain> or mail.<domain> for IMAP, and smtp.<domain>
// or mail.<domain> for SMTP. It takes a host only if a connection works.
// Login then checks the guessed settings with the password.
func fromGuess(ctx context.Context, email, domain string, opts Options) (Settings, bool) {
	var imap, smtp config.ServerEndpoint
	for _, ep := range []config.ServerEndpoint{
		{Host: "imap." + domain, Port: 993, TLS: config.TLSImplicit},
		{Host: "mail." + domain, Port: 993, TLS: config.TLSImplicit},
	} {
		if opts.Probe(ctx, ep.Host, ep.Port, true) == nil {
			imap = ep
			break
		}
	}
	if imap.Host == "" {
		return Settings{}, false
	}
	for _, ep := range []config.ServerEndpoint{
		{Host: "smtp." + domain, Port: 465, TLS: config.TLSImplicit},
		{Host: "smtp." + domain, Port: 587, TLS: config.TLSStartTLS},
		{Host: "mail." + domain, Port: 465, TLS: config.TLSImplicit},
		{Host: "mail." + domain, Port: 587, TLS: config.TLSStartTLS},
	} {
		if opts.Probe(ctx, ep.Host, ep.Port, ep.TLS == config.TLSImplicit) == nil {
			smtp = ep
			break
		}
	}
	if smtp.Host == "" {
		return Settings{}, false
	}
	return Settings{Provider: "imap", Service: domain, IMAP: imap, SMTP: smtp, Username: email, Source: "guess"}, true
}

// probe opens a connection and, when useTLS is true, does the TLS handshake.
func probe(ctx context.Context, host string, port uint16, useTLS bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer conn.Close()
	if !useTLS {
		return nil
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	return tlsConn.HandshakeContext(ctx)
}
