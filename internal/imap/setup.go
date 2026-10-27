package imap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/discover"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// SetupParser reads the login flags of IMAP and SMTP accounts, everything
// except the password. Fastmail accounts use it too, with Fastmail's servers
// filled in.
//
// When only the email address is given, Discover finds the servers. When
// Discover is nil, only maildo's built-in settings for large mail services
// are used.
type SetupParser struct {
	Discover func(ctx context.Context, email string) (discover.Settings, error)
}

func (p SetupParser) ConfigureFlags(cmd *cobra.Command) error {
	cmd.Flags().String("email", "", "Your email address; maildo looks up the servers from it")
	cmd.Flags().String("username", "", "Username for the IMAP and SMTP servers (default: the email address)")
	cmd.Flags().String("imap-host", "", "IMAP server name, such as imap.example.com")
	cmd.Flags().String("imap-port", "", "IMAP server port (default 993, or 143 with --imap-tls starttls)")
	cmd.Flags().String("imap-tls", "", "IMAP TLS mode: implicit or starttls (default: from the port)")
	cmd.Flags().String("smtp-host", "", "SMTP server name, such as smtp.example.com")
	cmd.Flags().String("smtp-port", "", "SMTP server port (default 465, or 587 with --smtp-tls starttls)")
	cmd.Flags().String("smtp-tls", "", "SMTP TLS mode: implicit or starttls (default: from the port)")
	cmd.Flags().String("ca-file", "", "Certificate authority (PEM file) to trust in addition to the system's")
	return nil
}

func (p SetupParser) Resolve(runCtx context.Context, ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	switch ctx.Provider {
	case "imap":
		res, err := resolveGenericSetup(ctx)
		if err != nil {
			return res, err
		}
		return p.discoverMissing(runCtx, ctx, res)
	case "fastmail":
		return resolveFastmailSetup(ctx)
	default:
		return provider.SetupResolution{}, provider.ErrProviderRuntimeDenied
	}
}

func (p SetupParser) PromptMissing(ctx context.Context, prompt provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	switch prompt.Provider {
	case "imap":
		return promptGenericSetup(ctx, prompt, resolution)
	case "fastmail":
		return promptFastmailSetup(ctx, prompt, resolution)
	default:
		return provider.SetupResolution{}, provider.ErrProviderRuntimeDenied
	}
}

func resolveGenericSetup(ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	setup := maildoconfig.MailServerSetup{}
	if ctx.Existing.MailServer != nil {
		setup = *ctx.Existing.MailServer
	}
	var missing []string
	var err error
	if setup.Email, err = resolveEmailFlag(ctx.Flags, "email", setup.Email); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.Username, err = resolveUsernameFlag(ctx.Flags, "username", setup.Username); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.IMAP, err = resolveEndpointFlags(ctx.Flags, "imap", setup.IMAP); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.SMTP, err = resolveEndpointFlags(ctx.Flags, "smtp", setup.SMTP); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.CAFile, err = resolveCAFileFlag(ctx.Flags, setup.CAFile); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.Email == "" {
		missing = append(missing, "email")
	}
	if setup.Username == "" && setup.Email != "" && !flagChanged(ctx.Flags, "username") {
		setup.Username = setup.Email
	}
	if setup.Username == "" {
		missing = append(missing, "username")
	}
	setup.IMAP = withEndpointDefaults(setup.IMAP, 993, 143)
	setup.SMTP = withEndpointDefaults(setup.SMTP, 465, 587)
	missing = appendEndpointMissing(missing, "imap", setup.IMAP)
	missing = appendEndpointMissing(missing, "smtp", setup.SMTP)
	return provider.SetupResolution{
		Setup:   maildoconfig.ProviderSetup{MailServer: &setup},
		Missing: missing,
		Preview: mailServerPreview(setup),
	}, nil
}

// discoverMissing fills in the servers when the email address is known but
// no server flag was given.
func (p SetupParser) discoverMissing(ctx context.Context, setupCtx provider.SetupResolveContext, res provider.SetupResolution) (provider.SetupResolution, error) {
	setup := res.Setup.MailServer
	if setup == nil || setup.Email == "" || setup.IMAP.Host != "" || setup.SMTP.Host != "" {
		return res, nil
	}
	for _, name := range []string{"imap-host", "imap-port", "imap-tls", "smtp-host", "smtp-port", "smtp-tls"} {
		if flagChanged(setupCtx.Flags, name) {
			return res, nil
		}
	}
	find := p.Discover
	if find == nil {
		find = func(ctx context.Context, email string) (discover.Settings, error) {
			return discover.Discover(ctx, email, discover.Options{SkipNetwork: true})
		}
	}
	found, err := find(ctx, setup.Email)
	if err != nil {
		res.Notes = append(res.Notes, "maildo could not find the mail servers for "+setup.Email+"; enter them below or pass --imap-host and --smtp-host.")
		return res, nil
	}
	if found.Provider == "outlook" {
		return res, rungrad.NewError(rungrad.ExitUsage, found.PasswordHint)
	}
	setup.IMAP, setup.SMTP = found.IMAP, found.SMTP
	if !flagChanged(setupCtx.Flags, "username") && found.Username != "" {
		setup.Username = found.Username
	}
	res.Missing = withoutEndpointFields(res.Missing)
	res.Preview = mailServerPreview(*setup)
	res.Notes = append(res.Notes,
		fmt.Sprintf("Found the servers for %s (%s):", found.Service, sourceText(found.Source)),
		"  IMAP "+endpointText(found.IMAP),
		"  SMTP "+endpointText(found.SMTP))
	return res, nil
}

func withoutEndpointFields(missing []string) []string {
	var out []string
	for _, field := range missing {
		if strings.HasPrefix(field, "imap_") || strings.HasPrefix(field, "smtp_") {
			continue
		}
		out = append(out, field)
	}
	return out
}

func sourceText(source string) string {
	switch source {
	case "built-in":
		return "built into maildo"
	case "autoconfig":
		return "from the domain's autoconfig file"
	case "ispdb":
		return "from Thunderbird's provider database"
	case "dns-srv":
		return "from DNS SRV records"
	case "mx":
		return "from the domain's MX records"
	default:
		return "a guess that answered"
	}
}

func endpointText(ep maildoconfig.ServerEndpoint) string {
	mode := "TLS"
	if ep.TLS == maildoconfig.TLSStartTLS {
		mode = "STARTTLS"
	}
	return fmt.Sprintf("%s:%d (%s)", ep.Host, ep.Port, mode)
}

func resolveFastmailSetup(ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	for _, name := range []string{"imap-host", "imap-port", "imap-tls", "smtp-host", "smtp-port", "smtp-tls", "ca-file"} {
		if flagChanged(ctx.Flags, name) {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "--"+name+" does not work with --provider fastmail, because maildo already knows Fastmail's servers")
		}
	}
	email := ""
	if ctx.Existing.MailServer != nil {
		email = ctx.Existing.MailServer.Email
	}
	var err error
	email, err = resolveEmailFlag(ctx.Flags, "email", email)
	if err != nil {
		return provider.SetupResolution{}, err
	}
	if email == "" {
		return provider.SetupResolution{
			Setup: maildoconfig.ProviderSetup{MailServer: &maildoconfig.MailServerSetup{
				IMAP: maildoconfig.FastmailIMAPEndpoint(),
				SMTP: maildoconfig.FastmailSMTPEndpoint(),
			}},
			Missing: []string{"email"},
			Preview: mailServerPreview(maildoconfig.MailServerSetup{
				IMAP: maildoconfig.FastmailIMAPEndpoint(),
				SMTP: maildoconfig.FastmailSMTPEndpoint(),
			}),
		}, nil
	}
	if flagChanged(ctx.Flags, "username") {
		username, err := resolveEmailLikeUsernameFlag(ctx.Flags, email)
		if err != nil {
			return provider.SetupResolution{}, err
		}
		if !strings.EqualFold(username, email) {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "a Fastmail username must be the same as --email")
		}
	}
	setup := maildoconfig.MailServerSetup{
		Email:    email,
		Username: email,
		IMAP:     maildoconfig.FastmailIMAPEndpoint(),
		SMTP:     maildoconfig.FastmailSMTPEndpoint(),
	}
	return provider.SetupResolution{
		Setup:   maildoconfig.ProviderSetup{MailServer: &setup},
		Preview: mailServerPreview(setup),
	}, nil
}

func promptGenericSetup(ctx context.Context, prompt provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	setup := maildoconfig.MailServerSetup{}
	if resolution.Setup.MailServer != nil {
		setup = *resolution.Setup.MailServer
	}
	var err error
	if setup.Email == "" {
		value, readErr := prompt.Visible.ReadLine(ctx, "Email:", "")
		if readErr != nil {
			return provider.SetupResolution{}, readErr
		}
		if setup.Email, err = maildoconfig.NormalizeConfiguredEmail(value); err != nil {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "that is not a valid email address")
		}
	}
	if setup.Username == "" {
		value, readErr := prompt.Visible.ReadLine(ctx, "Username ["+setup.Email+"]:", setup.Email)
		if readErr != nil {
			return provider.SetupResolution{}, readErr
		}
		if setup.Username, err = maildoconfig.NormalizeMailServerUsername(value); err != nil {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "that username cannot be used")
		}
	}
	if setup.IMAP.Host == "" {
		value, readErr := prompt.Visible.ReadLine(ctx, "IMAP host:", "")
		if readErr != nil {
			return provider.SetupResolution{}, readErr
		}
		if setup.IMAP.Host, err = maildoconfig.NormalizeMailServerHost(value); err != nil {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "that is not a valid IMAP host name")
		}
	}
	if setup.IMAP, err = promptEndpointDetails(ctx, prompt.Visible, "IMAP", setup.IMAP, 993, 143); err != nil {
		return provider.SetupResolution{}, err
	}
	if setup.SMTP.Host == "" {
		value, readErr := prompt.Visible.ReadLine(ctx, "SMTP host:", "")
		if readErr != nil {
			return provider.SetupResolution{}, readErr
		}
		if setup.SMTP.Host, err = maildoconfig.NormalizeMailServerHost(value); err != nil {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "that is not a valid SMTP host name")
		}
	}
	if setup.SMTP, err = promptEndpointDetails(ctx, prompt.Visible, "SMTP", setup.SMTP, 465, 587); err != nil {
		return provider.SetupResolution{}, err
	}
	return provider.SetupResolution{Setup: maildoconfig.ProviderSetup{MailServer: &setup}, Preview: mailServerPreview(setup)}, nil
}

func promptFastmailSetup(ctx context.Context, prompt provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	setup := maildoconfig.MailServerSetup{}
	if resolution.Setup.MailServer != nil {
		setup = *resolution.Setup.MailServer
	}
	if setup.Email == "" {
		value, err := prompt.Visible.ReadLine(ctx, "Email:", "")
		if err != nil {
			return provider.SetupResolution{}, err
		}
		email, err := maildoconfig.NormalizeConfiguredEmail(value)
		if err != nil {
			return provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "that is not a valid email address")
		}
		setup.Email = email
	}
	setup.Username = setup.Email
	setup.IMAP = maildoconfig.FastmailIMAPEndpoint()
	setup.SMTP = maildoconfig.FastmailSMTPEndpoint()
	return provider.SetupResolution{Setup: maildoconfig.ProviderSetup{MailServer: &setup}, Preview: mailServerPreview(setup)}, nil
}

func resolveEmailFlag(flags provider.SetupFlagValues, name, current string) (string, error) {
	flag, ok := flags.Lookup(name)
	if !ok || !flag.Changed {
		return current, nil
	}
	value, err := maildoconfig.NormalizeConfiguredEmail(flag.Value)
	if err != nil {
		return "", rungrad.NewError(rungrad.ExitUsage, "--"+name+" is not a valid email address")
	}
	return value, nil
}

func resolveUsernameFlag(flags provider.SetupFlagValues, name, current string) (string, error) {
	flag, ok := flags.Lookup(name)
	if !ok || !flag.Changed {
		return current, nil
	}
	value, err := maildoconfig.NormalizeMailServerUsername(flag.Value)
	if err != nil {
		return "", rungrad.NewError(rungrad.ExitUsage, "--"+name+" cannot be used as a username")
	}
	return value, nil
}

func resolveEmailLikeUsernameFlag(flags provider.SetupFlagValues, email string) (string, error) {
	flag, ok := flags.Lookup("username")
	if !ok || !flag.Changed {
		return email, nil
	}
	value, err := maildoconfig.NormalizeConfiguredEmail(flag.Value)
	if err != nil {
		return "", rungrad.NewError(rungrad.ExitUsage, "--username must be your Fastmail email address")
	}
	return value, nil
}

func resolveEndpointFlags(flags provider.SetupFlagValues, prefix string, current maildoconfig.ServerEndpoint) (maildoconfig.ServerEndpoint, error) {
	out := current
	if flag, ok := flags.Lookup(prefix + "-host"); ok && flag.Changed {
		value, err := maildoconfig.NormalizeMailServerHost(flag.Value)
		if err != nil {
			return out, rungrad.NewError(rungrad.ExitUsage, "--"+prefix+"-host is not a valid host name")
		}
		out.Host = value
	}
	if flag, ok := flags.Lookup(prefix + "-port"); ok && flag.Changed {
		port, err := parsePort(flag.Value)
		if err != nil {
			return out, rungrad.NewError(rungrad.ExitUsage, "--"+prefix+"-port must be a number from 1 to 65535")
		}
		out.Port = port
	}
	if flag, ok := flags.Lookup(prefix + "-tls"); ok && flag.Changed {
		mode, err := parseTLSMode(flag.Value)
		if err != nil {
			return out, rungrad.NewError(rungrad.ExitUsage, "--"+prefix+"-tls must be implicit or starttls")
		}
		out.TLS = mode
	}
	return out, nil
}

// resolveCAFileFlag stores the CA file as an absolute path, so the account
// keeps working when maildo runs from another directory.
func resolveCAFileFlag(flags provider.SetupFlagValues, current string) (string, error) {
	flag, ok := flags.Lookup("ca-file")
	if !ok || !flag.Changed {
		return current, nil
	}
	if strings.TrimSpace(flag.Value) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(flag.Value)
	if err != nil || strings.ContainsAny(abs, "\x00\r\n") {
		return "", rungrad.NewError(rungrad.ExitUsage, "--ca-file is not a usable file path")
	}
	if _, err := os.Stat(abs); err != nil {
		return "", rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("--ca-file %s cannot be read", abs))
	}
	return abs, nil
}

// appendEndpointMissing reports only a missing host. The port and TLS mode
// get defaults once the host is known.
func appendEndpointMissing(missing []string, prefix string, ep maildoconfig.ServerEndpoint) []string {
	if ep.Host == "" {
		missing = append(missing, prefix+"_host")
	}
	return missing
}

// promptEndpointDetails asks for the port and the TLS mode of a server
// whose host is known. Both questions offer the usual answer, so pressing
// Enter twice gives port 993 (IMAP) or 465 (SMTP) with TLS.
func promptEndpointDetails(ctx context.Context, input provider.VisibleInput, name string, ep maildoconfig.ServerEndpoint, tlsPort, startTLSPort uint16) (maildoconfig.ServerEndpoint, error) {
	if ep.Port == 0 {
		def := tlsPort
		if ep.TLS == maildoconfig.TLSStartTLS {
			def = startTLSPort
		}
		value, err := input.ReadLine(ctx, fmt.Sprintf("%s port [%d]:", name, def), strconv.Itoa(int(def)))
		if err != nil {
			return ep, err
		}
		if ep.Port, err = parsePort(value); err != nil {
			return ep, rungrad.NewError(rungrad.ExitUsage, name+" port must be a number from 1 to 65535")
		}
	}
	if ep.TLS == "" {
		def := withEndpointDefaults(maildoconfig.ServerEndpoint{Host: ep.Host, Port: ep.Port}, tlsPort, startTLSPort).TLS
		value, err := input.ReadLine(ctx, fmt.Sprintf("%s TLS mode, implicit or starttls [%s]:", name, def), string(def))
		if err != nil {
			return ep, err
		}
		if ep.TLS, err = parseTLSMode(value); err != nil {
			return ep, rungrad.NewError(rungrad.ExitUsage, name+" TLS mode must be implicit or starttls")
		}
	}
	return ep, nil
}

func parsePort(value string) (uint16, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, fmt.Errorf("invalid port")
	}
	port, err := strconv.ParseUint(value, 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("invalid port")
	}
	return uint16(port), nil
}

func parseTLSMode(value string) (maildoconfig.TLSMode, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("invalid tls mode")
	}
	switch strings.ToLower(value) {
	case string(maildoconfig.TLSImplicit):
		return maildoconfig.TLSImplicit, nil
	case string(maildoconfig.TLSStartTLS):
		return maildoconfig.TLSStartTLS, nil
	default:
		return "", fmt.Errorf("invalid tls mode")
	}
}

func flagChanged(flags provider.SetupFlagValues, name string) bool {
	flag, ok := flags.Lookup(name)
	return ok && flag.Changed
}

func mailServerPreview(setup maildoconfig.MailServerSetup) []provider.SetupPreviewField {
	return []provider.SetupPreviewField{
		{Name: "email", Value: setup.Email},
		{Name: "username", Value: setup.Username},
		{Name: "imap_host", Value: setup.IMAP.Host},
		{Name: "imap_port", Value: portString(setup.IMAP.Port)},
		{Name: "imap_tls", Value: string(setup.IMAP.TLS)},
		{Name: "smtp_host", Value: setup.SMTP.Host},
		{Name: "smtp_port", Value: portString(setup.SMTP.Port)},
		{Name: "smtp_tls", Value: string(setup.SMTP.TLS)},
		{Name: "ca_file", Value: setup.CAFile},
	}
}

func portString(port uint16) string {
	if port == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(port), 10)
}

var _ provider.SetupParser = SetupParser{}

// withEndpointDefaults fills in the usual port and TLS mode once the host is
// known: TLS from the start on 993 and 465, STARTTLS on 143, 587 and 25.
func withEndpointDefaults(ep maildoconfig.ServerEndpoint, tlsPort, startTLSPort uint16) maildoconfig.ServerEndpoint {
	if ep.Host == "" {
		return ep
	}
	if ep.Port == 0 {
		ep.Port = tlsPort
		if ep.TLS == maildoconfig.TLSStartTLS {
			ep.Port = startTLSPort
		}
	}
	if ep.TLS == "" {
		ep.TLS = maildoconfig.TLSImplicit
		switch ep.Port {
		case 143, 587, 25:
			ep.TLS = maildoconfig.TLSStartTLS
		}
	}
	return ep
}
