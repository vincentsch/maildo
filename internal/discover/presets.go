package discover

import (
	"strings"

	"github.com/vincentsch/maildo/internal/config"
)

// preset holds the settings of one large mail service. Server names and
// ports come from each service's own help pages.
type preset struct {
	service  string
	provider string
	imap     config.ServerEndpoint
	smtp     config.ServerEndpoint
	hint     string
}

func implicit(host string, port uint16) config.ServerEndpoint {
	return config.ServerEndpoint{Host: host, Port: port, TLS: config.TLSImplicit}
}

func starttls(host string, port uint16) config.ServerEndpoint {
	return config.ServerEndpoint{Host: host, Port: port, TLS: config.TLSStartTLS}
}

const microsoftHint = "Microsoft no longer accepts passwords over IMAP for Outlook.com and Microsoft 365 mailboxes. Sign in with Microsoft instead: maildo login --provider outlook. That needs an app registration, which https://github.com/vincentsch/maildo/blob/main/docs/providers/outlook.md walks through."

var (
	gmail = preset{
		service: "Gmail", provider: "imap",
		imap: implicit("imap.gmail.com", 993), smtp: implicit("smtp.gmail.com", 465),
		hint: "Gmail wants an app password, not your Google password. Turn on 2-Step Verification, then create one at https://myaccount.google.com/apppasswords",
	}
	icloud = preset{
		service: "iCloud Mail", provider: "imap",
		imap: implicit("imap.mail.me.com", 993), smtp: starttls("smtp.mail.me.com", 587),
		hint: "iCloud wants an app-specific password. Create one at https://account.apple.com under Sign-In and Security.",
	}
	yahoo = preset{
		service: "Yahoo Mail", provider: "imap",
		imap: implicit("imap.mail.yahoo.com", 993), smtp: implicit("smtp.mail.yahoo.com", 465),
		hint: "Yahoo wants an app password. Create one at https://login.yahoo.com/account/security",
	}
	aol = preset{
		service: "AOL Mail", provider: "imap",
		imap: implicit("imap.aol.com", 993), smtp: implicit("smtp.aol.com", 465),
		hint: "AOL wants an app password. Create one at https://login.aol.com/account/security",
	}
	fastmail = preset{
		service: "Fastmail", provider: "fastmail",
		imap: config.FastmailIMAPEndpoint(), smtp: config.FastmailSMTPEndpoint(),
		hint: "Fastmail wants an app password with IMAP and SMTP access. Create one in Fastmail under Settings > Privacy & Security > App passwords.",
	}
	gmxNet = preset{
		service: "GMX", provider: "imap",
		imap: implicit("imap.gmx.net", 993), smtp: implicit("mail.gmx.net", 465),
		hint: "Use your GMX password. Turn on IMAP first, in GMX under Settings > POP3 & IMAP.",
	}
	gmxCom = preset{
		service: "GMX", provider: "imap",
		imap: implicit("imap.gmx.com", 993), smtp: implicit("mail.gmx.com", 465),
		hint: "Use your GMX password. Turn on IMAP first, in GMX under Settings > POP3 & IMAP.",
	}
	webde = preset{
		service: "WEB.DE", provider: "imap",
		imap: implicit("imap.web.de", 993), smtp: starttls("smtp.web.de", 587),
		hint: "Use your WEB.DE password. Turn on IMAP first, in WEB.DE under Settings > POP3/IMAP.",
	}
	posteo = preset{
		service: "Posteo", provider: "imap",
		imap: implicit("posteo.de", 993), smtp: implicit("posteo.de", 465),
		hint: "Use your Posteo password.",
	}
	mailboxOrg = preset{
		service: "mailbox.org", provider: "imap",
		imap: implicit("imap.mailbox.org", 993), smtp: implicit("smtp.mailbox.org", 465),
		hint: "Use your mailbox.org password, or an app password if you turned on two-factor login.",
	}
	microsoft = preset{service: "Outlook.com", provider: "outlook", hint: microsoftHint}
)

var presetsByDomain = map[string]preset{
	"gmail.com": gmail, "googlemail.com": gmail,
	"icloud.com": icloud, "me.com": icloud, "mac.com": icloud,
	"ymail.com": yahoo, "rocketmail.com": yahoo,
	"aol.com": aol, "aim.com": aol,
	"fastmail.com": fastmail, "fastmail.fm": fastmail, "fastmail.net": fastmail, "fastmail.org": fastmail,
	"fastmail.us": fastmail, "fastmail.co.uk": fastmail, "fastmail.de": fastmail, "fastmail.to": fastmail,
	"gmx.de": gmxNet, "gmx.net": gmxNet, "gmx.at": gmxNet, "gmx.ch": gmxNet, "gmx.eu": gmxNet, "gmx.org": gmxNet, "gmx.info": gmxNet, "gmx.biz": gmxNet,
	"gmx.com": gmxCom, "gmx.co.uk": gmxCom, "gmx.fr": gmxCom, "gmx.es": gmxCom, "gmx.us": gmxCom,
	"web.de":    webde,
	"posteo.de": posteo, "posteo.net": posteo, "posteo.org": posteo, "posteo.eu": posteo, "posteo.at": posteo, "posteo.ch": posteo, "posteo.me": posteo,
	"mailbox.org": mailboxOrg,
	"outlook.com": microsoft, "hotmail.com": microsoft, "live.com": microsoft, "msn.com": microsoft, "passport.com": microsoft,
	"outlook.de": microsoft, "hotmail.de": microsoft, "live.de": microsoft, "hotmail.co.uk": microsoft, "live.co.uk": microsoft,
	"hotmail.fr": microsoft, "outlook.fr": microsoft, "live.fr": microsoft,
}

// PresetFor returns built-in settings for addresses at large mail services,
// without any network access.
func PresetFor(email string) (Settings, bool) {
	_, domain, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok {
		return Settings{}, false
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	p, found := presetsByDomain[domain]
	if !found && (strings.HasPrefix(domain, "yahoo.") || strings.HasPrefix(domain, "ymail.")) {
		p, found = yahoo, true
	}
	if !found {
		return Settings{}, false
	}
	return p.settings(email, "built-in"), true
}

// presetForMX recognises a large mail service that hosts the mail of a
// custom domain, by the domain's MX host.
func presetForMX(email, mxHost string) (Settings, bool) {
	for suffix, p := range map[string]preset{
		".google.com":          gmail,
		".googlemail.com":      gmail,
		".outlook.com":         {service: "Microsoft 365", provider: "outlook", hint: microsoftHint},
		".messagingengine.com": fastmail,
		".mail.icloud.com":     icloud,
		".yahoodns.net":        yahoo,
		".gmx.net":             gmxNet,
		".web.de":              webde,
		".posteo.de":           posteo,
		".mailbox.org":         mailboxOrg,
	} {
		if strings.HasSuffix(mxHost, suffix) {
			return p.settings(email, "mx"), true
		}
	}
	return Settings{}, false
}

func (p preset) settings(email, source string) Settings {
	return Settings{
		Provider:     p.provider,
		Service:      p.service,
		IMAP:         p.imap,
		SMTP:         p.smtp,
		Username:     email,
		Source:       source,
		PasswordHint: p.hint,
	}
}
