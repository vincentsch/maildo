package discover

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vincentsch/maildo/internal/config"
)

// Thunderbird's autoconfig format, as served by mail domains themselves and
// by the ISPDB. Only the parts maildo needs are read.
type clientConfig struct {
	Providers []struct {
		DisplayName string      `xml:"displayName"`
		Incoming    []xmlServer `xml:"incomingServer"`
		Outgoing    []xmlServer `xml:"outgoingServer"`
	} `xml:"emailProvider"`
}

type xmlServer struct {
	Type       string `xml:"type,attr"`
	Hostname   string `xml:"hostname"`
	Port       string `xml:"port"`
	SocketType string `xml:"socketType"`
	Username   string `xml:"username"`
}

// fromAutoconfig asks the domain itself, at the two places Thunderbird
// looks.
func fromAutoconfig(ctx context.Context, email, domain string, opts Options) (Settings, bool) {
	query := "?emailaddress=" + url.QueryEscape(email)
	for _, u := range []string{
		"https://autoconfig." + domain + "/mail/config-v1.1.xml" + query,
		"https://" + domain + "/.well-known/autoconfig/mail/config-v1.1.xml" + query,
	} {
		if s, ok := fetchConfig(ctx, opts.HTTP, u, email); ok {
			s.Source = "autoconfig"
			return s, true
		}
	}
	return Settings{}, false
}

// fromISPDB asks Thunderbird's public database of mail providers.
func fromISPDB(ctx context.Context, email, domain string, opts Options) (Settings, bool) {
	s, ok := fetchConfig(ctx, opts.HTTP, strings.TrimSuffix(opts.ISPDBURL, "/")+"/"+url.PathEscape(domain), email)
	if ok {
		s.Source = "ispdb"
	}
	return s, ok
}

func fetchConfig(ctx context.Context, client *http.Client, u, email string) (Settings, bool) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Settings{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return Settings{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Settings{}, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return Settings{}, false
	}
	return parseClientConfig(data, email)
}

// parseClientConfig picks the first IMAP and SMTP server that use TLS.
// Servers offering only plain connections are ignored: maildo never sends a
// password unencrypted.
func parseClientConfig(data []byte, email string) (Settings, bool) {
	var cfg clientConfig
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return Settings{}, false
	}
	for _, p := range cfg.Providers {
		imap, imapUser, ok := pickServer(p.Incoming, "imap", email)
		if !ok {
			continue
		}
		smtp, _, ok := pickServer(p.Outgoing, "smtp", email)
		if !ok {
			continue
		}
		service := strings.TrimSpace(p.DisplayName)
		if service == "" {
			service = imap.Host
		}
		return Settings{Provider: "imap", Service: service, IMAP: imap, SMTP: smtp, Username: imapUser}, true
	}
	return Settings{}, false
}

func pickServer(servers []xmlServer, kind, email string) (config.ServerEndpoint, string, bool) {
	local, domain, _ := strings.Cut(email, "@")
	for _, s := range servers {
		if !strings.EqualFold(s.Type, kind) {
			continue
		}
		port, err := strconv.ParseUint(strings.TrimSpace(s.Port), 10, 16)
		host := strings.ToLower(strings.TrimSpace(s.Hostname))
		if err != nil || port == 0 || host == "" {
			continue
		}
		host = strings.ReplaceAll(host, "%emaildomain%", strings.ToLower(domain))
		var mode config.TLSMode
		switch strings.ToUpper(strings.TrimSpace(s.SocketType)) {
		case "SSL", "TLS":
			mode = config.TLSImplicit
		case "STARTTLS":
			mode = config.TLSStartTLS
		default:
			continue
		}
		user := strings.TrimSpace(s.Username)
		user = strings.ReplaceAll(user, "%EMAILADDRESS%", email)
		user = strings.ReplaceAll(user, "%EMAILLOCALPART%", local)
		user = strings.ReplaceAll(user, "%EMAILDOMAIN%", domain)
		if user == "" || strings.Contains(user, "%") {
			user = email
		}
		return config.ServerEndpoint{Host: host, Port: uint16(port), TLS: mode}, user, true
	}
	return config.ServerEndpoint{}, "", false
}
