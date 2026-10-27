package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

var _ provider.Diagnoser = (*accountClient)(nil)

// Diagnose checks the account step by step: DNS, TLS and the certificate,
// IMAP login, the server's extensions, the folder roles and the SMTP login.
// It ends with what maildo can and cannot do with this server.
func (c *accountClient) Diagnose(ctx context.Context) mail.Diagnosis {
	var d mail.Diagnosis
	add := func(name, status, detail string) {
		d.Checks = append(d.Checks, mail.DiagnosisCheck{Name: name, Status: status, Detail: detail})
	}
	fail := func(name string, err error) {
		d.Checks = append(d.Checks, mail.DiagnosisCheck{Name: name, Status: "failed", Detail: errorDetail(err), Err: err})
	}
	p := &Provider{name: c.provider, opts: c.opts}
	imapEP, smtpEP := c.setup.IMAP, c.setup.SMTP

	// DNS. A test dialer stands in for the network, so there is nothing to
	// look up.
	if c.opts.DialContext == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, imapEP.Host)
		if err != nil {
			add("DNS", "failed", fmt.Sprintf("%s does not resolve: %v", imapEP.Host, err))
			return d
		}
		add("DNS", "ok", fmt.Sprintf("%s is %s", imapEP.Host, addrs[0]))
	}

	// TLS and the certificate, on a connection of its own.
	if detail, err := p.tlsCheck(ctx, c.account, imapEP, c.setup.CAFile); err != nil {
		fail("TLS", err)
		return d
	} else {
		add("TLS", "ok", detail)
	}

	// Login, and what the server can do.
	var caps goimap.CapSet
	var snapshot imapFolderSnapshot
	loginErr := c.withIMAPSession(ctx, func(session *imapSession) error {
		state, err := session.activeState(ctx)
		if err != nil {
			return err
		}
		caps = state.capabilities
		snapshot, err = c.folderSnapshot(ctx, session, false)
		return err
	})
	if loginErr != nil {
		fail("Login", loginErr)
		return d
	}
	add("Login", "ok", "the IMAP server accepted the password")
	add("IMAP", "ok", capabilityList(caps))

	roles := map[mail.ProviderSpecialUse]string{}
	for _, record := range snapshot.selectable {
		for _, role := range record.specialUse {
			if _, seen := roles[role]; !seen {
				roles[role] = folderLabel(record)
			}
		}
	}
	var found, missing []string
	for _, role := range []mail.ProviderSpecialUse{mail.SpecialUseSent, mail.SpecialUseDrafts, mail.SpecialUseArchive, mail.SpecialUseTrash, mail.SpecialUseJunk} {
		if name, ok := roles[role]; ok {
			found = append(found, fmt.Sprintf("%s=%s", roleName(role), name))
		} else {
			missing = append(missing, roleName(role))
		}
	}
	folderStatus := "ok"
	folderDetail := fmt.Sprintf("%d %s", len(snapshot.folders), plural(len(snapshot.folders), "folder", "folders"))
	if len(found) > 0 {
		how := "marked by the server (SPECIAL-USE)"
		if snapshot.specialUseAuthority == specialUseNames {
			how = "found by name, since the server does not mark them"
		}
		folderDetail += "; " + strings.Join(found, ", ") + ", " + how
	}
	if len(missing) > 0 {
		folderStatus = "warning"
		folderDetail += "; no folder for " + strings.Join(missing, ", ")
	}
	add("Folders", folderStatus, folderDetail)

	// SMTP.
	smtpOK := false
	smtpErr := c.withSMTPSession(ctx, func(*smtpSession) (smtpSessionOutcome, error) {
		return smtpSessionOutcomeAcceptedSubmission, nil
	})
	if smtpErr != nil {
		fail("SMTP", smtpErr)
	} else {
		smtpOK = true
		add("SMTP", "ok", fmt.Sprintf("%s:%d accepted the login", smtpEP.Host, smtpEP.Port))
	}

	// What works.
	rev2 := caps.Has(goimap.CapIMAP4rev2)
	hasMove := rev2 || caps.Has(goimap.CapMove)
	hasUIDPlus := rev2 || caps.Has(goimap.CapUIDPlus)
	d.Works = []string{"search", "list", "read", "thread", "attachments", "mark", "flag", "folders"}
	if hasUIDPlus {
		d.Works = append(d.Works, "copy")
	} else {
		d.Limits = append(d.Limits, "copy needs the UIDPLUS extension, which this server lacks")
	}
	if hasMove && hasUIDPlus {
		d.Works = append(d.Works, "move", "restore")
		for _, role := range []struct {
			command string
			role    mail.ProviderSpecialUse
		}{{"archive", mail.SpecialUseArchive}, {"delete", mail.SpecialUseTrash}, {"junk", mail.SpecialUseJunk}} {
			if _, ok := roles[role.role]; ok {
				d.Works = append(d.Works, role.command)
			} else {
				d.Limits = append(d.Limits, fmt.Sprintf("%s needs a folder named %s; create it with: maildo folders create %s", role.command, roleName(role.role), roleName(role.role)))
			}
		}
	} else {
		d.Limits = append(d.Limits, "move, archive, delete and junk need the MOVE and UIDPLUS extensions, which this server lacks")
	}
	if smtpOK {
		d.Works = append(d.Works, "send", "reply", "forward")
		if _, ok := roles[mail.SpecialUseSent]; !ok {
			d.Limits = append(d.Limits, "maildo cannot save a copy of sent mail, because there is no Sent folder; create it with: maildo folders create Sent")
		}
	} else {
		d.Limits = append(d.Limits, "send, reply and forward need a working SMTP login")
	}
	if _, ok := roles[mail.SpecialUseDrafts]; ok {
		d.Works = append(d.Works, "drafts")
	} else {
		d.Limits = append(d.Limits, "drafts need a folder named Drafts; create it with: maildo folders create Drafts")
	}
	if !caps.Has(goimap.CapCondStore) && !rev2 {
		d.Limits = append(d.Limits, "the server lacks CONDSTORE, so maildo cannot notice when another mail program changes a message while maildo marks or flags it")
	}
	return d
}

// tlsCheck connects, starts TLS (right away or after STARTTLS) and
// describes the TLS version and the certificate.
func (p *Provider) tlsCheck(ctx context.Context, account string, ep maildoconfig.ServerEndpoint, caFile string) (string, error) {
	cfg, err := p.tlsConfig(ep.Host, caFile)
	if err != nil {
		return "", err
	}
	conn, err := p.dial(ctx, ep)
	if err != nil {
		return "", withEndpoint(mapDialError(account, p.name, errs.ProviderProtocolIMAP, err), ep)
	}
	defer conn.Close()
	if ep.TLS == maildoconfig.TLSStartTLS {
		_ = conn.SetDeadline(time.Now().Add(greetingTimeout))
		reader := bufio.NewReader(conn)
		if _, err := imapReadLine(reader); err != nil {
			return "", fmt.Errorf("the server sent no IMAP greeting on port %d; this port probably expects TLS from the start (--imap-tls implicit)", ep.Port)
		}
		if _, err := fmt.Fprintf(conn, "MDT1 STARTTLS\r\n"); err != nil {
			return "", err
		}
		for {
			line, err := imapReadLine(reader)
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(line, "MDT1 ") {
				if !strings.HasPrefix(strings.ToUpper(line), "MDT1 OK") {
					return "", fmt.Errorf("the server refused STARTTLS: %s", strings.TrimPrefix(line, "MDT1 "))
				}
				break
			}
		}
	}
	tlsConn := tls.Client(conn, cfg)
	if err := handshake(ctx, tlsConn); err != nil {
		return "", withEndpoint(mapTLSFailure(account, p.name, errs.ProviderProtocolIMAP, err), ep)
	}
	state := tlsConn.ConnectionState()
	detail := fmt.Sprintf("%s on %s:%d", tls.VersionName(state.Version), ep.Host, ep.Port)
	if len(state.PeerCertificates) > 0 {
		cert := state.PeerCertificates[0]
		issuer := cert.Issuer.CommonName
		if issuer == "" && len(cert.Issuer.Organization) > 0 {
			issuer = cert.Issuer.Organization[0]
		}
		detail += fmt.Sprintf(", certificate valid until %s", cert.NotAfter.Format("2 Jan 2006"))
		if issuer != "" {
			detail += " (issued by " + issuer + ")"
		}
		if time.Until(cert.NotAfter) < 14*24*time.Hour {
			detail += "; it expires soon"
		}
	}
	return detail, nil
}

func roleName(role mail.ProviderSpecialUse) string {
	name := string(role)
	return strings.ToUpper(name[:1]) + name[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func capabilityList(caps goimap.CapSet) string {
	interesting := []goimap.Cap{goimap.CapIMAP4rev2, goimap.CapIMAP4rev1, goimap.CapSpecialUse, goimap.CapMove, goimap.CapUIDPlus,
		goimap.CapCondStore, goimap.CapESearch, goimap.CapListStatus, goimap.CapUTF8Accept, goimap.CapIdle}
	var have []string
	for _, c := range interesting {
		if caps.Has(c) {
			have = append(have, string(c))
		}
	}
	if caps.Has(goimap.Cap("SORT")) {
		have = append(have, "SORT")
	}
	return strings.Join(have, ", ")
}

// errorDetail puts a maildo error on one line, with its reason and the
// server's own words.
func errorDetail(err error) string {
	var product *errs.Error
	if !errors.As(err, &product) {
		return err.Error()
	}
	d := product.Details()
	parts := []string{product.Message()}
	if reason, _ := d["reason"].(string); reason != "" {
		parts = append(parts, "reason: "+strings.ReplaceAll(reason, "_", " "))
	}
	for _, key := range []string{"cause", "server_message"} {
		if v, _ := d[key].(string); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "; ")
}
