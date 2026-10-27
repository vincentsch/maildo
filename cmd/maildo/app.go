package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	runconfig "github.com/vincentsch/rungrad/config"
	"github.com/vincentsch/rungrad/update"

	"github.com/vincentsch/maildo/internal/discover"
	"github.com/vincentsch/maildo/internal/fastmail"
	"github.com/vincentsch/maildo/internal/gmail"
	"github.com/vincentsch/maildo/internal/imap"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

type Deps struct {
	Providers           *provider.Registry
	DirectSendPreparers *provider.DirectSendPreparerRegistry
	SetupParsers        *provider.SetupParserRegistry
	NewVisibleInput     VisibleInputFactory
	SecretReader        provider.SecretReader
	Now                 func() time.Time
	RunGradAuth         rungrad.CredentialResolver
	RunGradLoadConfig   func(string) (runconfig.Config, error)
	UpdateFetcher       update.Fetcher
	wiringErr           error
}

type VisibleInputFactory func(*rungrad.Factory) provider.VisibleInput

// productionDeps sets up the real providers. Only tests add fake ones.
func productionDeps() Deps {
	deps, err := productionDepsE()
	if err != nil {
		deps.wiringErr = err
	}
	return deps
}

func productionDepsE() (Deps, error) {
	return depsWithMailServerOptions(imap.Options{})
}

// depsWithMailServerOptions wires the production providers. Tests pass
// options that route IMAP and SMTP to servers running in the test.
func depsWithMailServerOptions(mailServer imap.Options) (Deps, error) {
	r := provider.NewRegistry()
	outlookProvider := outlook.New(outlook.Options{})
	if err := r.Register(provider.ProviderRegistration{Name: "outlook", Lifecycle: outlookProvider, Codec: outlook.CredentialCodec{}}); err != nil {
		return Deps{}, err
	}
	gmailProvider := gmail.New(gmail.Options{})
	if err := r.Register(provider.ProviderRegistration{Name: "gmail", Lifecycle: gmailProvider, Codec: gmail.CredentialCodec{}}); err != nil {
		return Deps{}, err
	}
	imapProvider := imap.New(mailServer)
	if err := r.Register(provider.ProviderRegistration{Name: "imap", Lifecycle: imapProvider, Codec: imap.NewCredentialCodec("imap")}); err != nil {
		return Deps{}, err
	}
	fastmailProvider := fastmail.New(mailServer)
	if err := r.Register(provider.ProviderRegistration{Name: "fastmail", Lifecycle: fastmailProvider, Codec: fastmail.CredentialCodec()}); err != nil {
		return Deps{}, err
	}
	preparers := provider.NewDirectSendPreparerRegistry()
	if err := preparers.Register("outlook", provider.ComposeIngressDelivered, outlook.DirectSendPreparer{}); err != nil {
		return Deps{}, err
	}
	if err := preparers.Register("gmail", provider.ComposeIngressMIMEBounded, gmail.DirectSendPreparer{}); err != nil {
		return Deps{}, err
	}
	if err := preparers.Register("imap", provider.ComposeIngressMIMEBounded, imap.NewDirectSendPreparer("imap")); err != nil {
		return Deps{}, err
	}
	if err := preparers.Register("fastmail", provider.ComposeIngressMIMEBounded, imap.NewDirectSendPreparer("fastmail")); err != nil {
		return Deps{}, err
	}
	parsers := provider.NewSetupParserRegistry()
	if err := parsers.Register(provider.SetupParserRegistration{ID: "outlook", Aliases: []string{"outlook"}, Parser: outlook.SetupParser{}}); err != nil {
		return Deps{}, err
	}
	if err := parsers.Register(provider.SetupParserRegistration{ID: "gmail", Aliases: []string{"gmail"}, Parser: gmail.SetupParser{}}); err != nil {
		return Deps{}, err
	}
	// Tests route mail servers through DialContext; they also get no network
	// discovery, only the built-in settings.
	mailServerParser := imap.SetupParser{}
	if mailServer.DialContext == nil {
		mailServerParser.Discover = func(ctx context.Context, email string) (discover.Settings, error) {
			return discover.Discover(ctx, email, discover.Options{})
		}
	}
	if err := parsers.Register(provider.SetupParserRegistration{ID: "mailserver", Aliases: []string{"imap", "fastmail"}, Parser: &mailServerParser}); err != nil {
		return Deps{}, err
	}
	return Deps{
		Providers:           r,
		DirectSendPreparers: preparers,
		SetupParsers:        parsers,
		UpdateFetcher:       update.GitHubFetcher{Owner: "vincentsch", Repo: "maildo"},
	}, nil
}

// newApp builds the rungrad app with maildo's commands. Tests can pass extra
// command modules; main passes none.
//
// rungrad parses flags, handles output modes, help and docs, and finds the
// config path. maildo reads its own accounts file at that path, so rungrad's
// LoadConfig hook does not read it.
func newApp(deps Deps, extra ...rungrad.FeatureModule) *rungrad.App {
	app, err := newAppE(deps, extra...)
	if err != nil {
		panic("maildo: invalid application wiring")
	}
	return app
}

func newAppE(deps Deps, extra ...rungrad.FeatureModule) (app *rungrad.App, err error) {
	if deps.wiringErr != nil {
		return nil, errInvalidAppWiring()
	}
	defer func() {
		if recover() != nil {
			app = nil
			err = errInvalidAppWiring()
		}
	}()
	if deps.Providers == nil {
		if deps.SetupParsers != nil || deps.DirectSendPreparers != nil {
			return nil, errInvalidAppWiring()
		}
		prod, err := productionDepsE()
		if err != nil {
			return nil, errInvalidAppWiring()
		}
		deps.Providers = prod.Providers
		deps.DirectSendPreparers = prod.DirectSendPreparers
		deps.SetupParsers = prod.SetupParsers
	} else if deps.SetupParsers == nil || deps.DirectSendPreparers == nil {
		return nil, errInvalidAppWiring()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.RunGradLoadConfig == nil {
		deps.RunGradLoadConfig = func(string) (runconfig.Config, error) {
			return runconfig.Config{Version: 1}, nil
		}
	}
	if deps.NewVisibleInput == nil {
		deps.NewVisibleInput = newVisibleInput
	}
	app = rungrad.New(rungrad.AppConfig{
		Name:           "maildo",
		Short:          "Handle your email from the terminal",
		Long:           "maildo handles your mail from the terminal. People get readable tables, and scripts and coding agents get stable JSON. A change to many messages shows a preview first, and limits you set for each account decide what a script or agent may do.",
		Version:        versionString(),
		AdvancedOutput: true,
		ErrorPolicy:    &rungrad.ErrorPolicy{Render: renderError},
		Auth:           deps.RunGradAuth,
		Resolution: &rungrad.ResolutionConfig{
			// rungrad gets an empty config, so help and the other built-in
			// commands work even when maildo's config file is missing or
			// broken. The handlers read that file themselves.
			LoadConfig: deps.RunGradLoadConfig,
		},
	})
	app.Root().Example = strings.Join([]string{
		"maildo login",
		"maildo list",
		`maildo search "from:anna invoice newer:30d"`,
		"maildo read 4d1a2f3",
		`maildo archive --query "from:deals@shop.example" --all`,
	}, "\n")
	h := &handlers{deps: deps}
	app.AddModule(newAuthModule(h))
	app.AddModule(newDoctorModule(h))
	app.AddModule(newConfigModule(h))
	app.AddModule(newFoldersModule(h))
	app.AddModule(newMessagesModule(h))
	app.AddModule(newComposeModule(h))
	app.AddModule(newAttachmentsModule(h))
	app.AddModule(newMutationsModule(h))
	app.AddModule(newRulesModule(h))
	app.AddModule(newOutlookModule(h))
	app.AddModule(updateModule{fetcher: deps.UpdateFetcher})
	for _, m := range extra {
		app.AddModule(m)
	}
	if err := deps.Providers.Validate(); err != nil {
		return nil, errInvalidAppWiring()
	}
	if err := deps.DirectSendPreparers.Validate(context.Background(), deps.Providers); err != nil {
		return nil, errInvalidAppWiring()
	}
	if err := deps.SetupParsers.Validate(); err != nil {
		return nil, errInvalidAppWiring()
	}
	if err := provider.ValidateProviderParserPairs(deps.Providers, deps.SetupParsers); err != nil {
		return nil, errInvalidAppWiring()
	}
	if err := app.ValidateCatalog(); err != nil {
		return nil, errInvalidAppWiring()
	}
	return app, nil
}

func defaultSetupParsers(names []string) *provider.SetupParserRegistry {
	r := provider.NewSetupParserRegistry()
	var mailServerAliases []string
	for _, name := range names {
		if name == "imap" || name == "fastmail" {
			mailServerAliases = append(mailServerAliases, name)
			continue
		}
		parser := provider.SetupParser(noopSetupParser{providerName: name})
		if name == "outlook" {
			parser = outlook.SetupParser{}
		} else if name == "gmail" {
			parser = gmail.SetupParser{}
		}
		_ = r.Register(provider.SetupParserRegistration{ID: name, Aliases: []string{name}, Parser: parser})
	}
	if len(mailServerAliases) > 0 {
		_ = r.Register(provider.SetupParserRegistration{ID: "mailserver", Aliases: mailServerAliases, Parser: &imap.SetupParser{}})
	}
	return r
}

type noopSetupParser struct {
	providerName string
}

func (noopSetupParser) ConfigureFlags(*cobra.Command) error { return nil }

func (p noopSetupParser) Resolve(_ context.Context, ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	return provider.SetupResolution{Setup: maildoconfig.ProviderSetup{}}, nil
}

func (noopSetupParser) PromptMissing(_ context.Context, _ provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	return resolution, nil
}

func errInvalidAppWiring() error {
	return errors.New("maildo: invalid application wiring")
}

type updateModule struct {
	fetcher update.Fetcher
}

func (updateModule) Groups() []rungrad.Group { return nil }

func (m updateModule) Commands() []*rungrad.Command {
	return []*rungrad.Command{update.Command(update.CommandConfig{
		CurrentVersion: version,
		ToolName:       "maildo",
		Fetcher:        m.fetcher,
		Apply:          nil,
	})}
}

func (updateModule) Catalog() []rungrad.CommandSpec {
	return []rungrad.CommandSpec{{
		Path:        "update",
		Summary:     "Check for and install the latest version",
		OutputModes: []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON},
		Examples: []string{
			"maildo update --check",
			"maildo update --check --json",
			"maildo update",
		},
		Mutates: true,
	}}
}
