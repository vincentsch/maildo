package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/docsgen"
	runmanifest "github.com/vincentsch/rungrad/manifest"
	"github.com/vincentsch/rungrad/output"
	"github.com/vincentsch/rungrad/testutil"
)

// cliHarness runs commands with a per-test config root and injectable providers.
// It keeps tests close to production command execution while avoiding real
// environment, browser, and filesystem state.
type cliHarness struct {
	t            *testing.T
	root         string
	configPath   string
	providers    *provider.Registry
	preparers    *provider.DirectSendPreparerRegistry
	browserCalls atomic.Int64
}

type credentialHarness interface {
	credentialRoot() string
}

func (h *cliHarness) credentialRoot() string {
	return h.root
}

func newFakeHarness(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
	t.Helper()
	root := t.TempDir()
	return &cliHarness{
		t:          t,
		root:       root,
		configPath: filepath.Join(root, "config.yaml"),
		providers:  testkit.NewProviderRegistry(fake),
	}
}

func (h *cliHarness) app() *rungrad.App {
	preparers := h.preparers
	if preparers == nil {
		preparers = testkit.NewDirectSendPreparerRegistry(providerLifecycles(h.providers)...)
	}
	return newApp(Deps{
		Providers:           h.providers,
		DirectSendPreparers: preparers,
		SetupParsers:        defaultSetupParsers(h.providers.Names()),
	})
}

func providerLifecycles(registry *provider.Registry) []provider.Provider {
	var out []provider.Provider
	for _, name := range registry.Names() {
		p, _, ok := registry.Get(name)
		if ok {
			out = append(out, p)
		}
	}
	return out
}

func (h *cliHarness) store() auth.Store {
	return auth.NewStore(h.root)
}

func credentialExists(t *testing.T, h credentialHarness, providerName, account string) bool {
	t.Helper()
	present, err := testkit.CredentialExists(h.credentialRoot(), providerName, account)
	if err != nil {
		return false
	}
	return present
}

func loadCredentialCache(h credentialHarness, providerName, account string) (*testkit.TokenCache, error) {
	return testkit.LoadCredential(h.credentialRoot(), providerName, account)
}

func (h *cliHarness) run(args ...string) testutil.Result {
	return h.runWith(testutil.Options{}, args...)
}

func (h *cliHarness) runWith(opts testutil.Options, args ...string) testutil.Result {
	h.t.Helper()
	fullArgs := append([]string{"--config", h.configPath}, args...)
	if opts.LookupEnv == nil {
		opts.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if opts.BrowserOpener == nil {
		opts.BrowserOpener = func(context.Context, string) error {
			h.browserCalls.Add(1)
			return nil
		}
	}
	return testutil.RunWith(h.app(), opts, fullArgs...)
}

func TestInjectedProviderWiringRequiresSetupParsers(t *testing.T) {
	registry := testkit.NewProviderRegistry(testkit.NewFakeProvider("fake"))
	if _, err := newAppE(Deps{Providers: registry}); err == nil {
		t.Fatal("newAppE accepted providers without setup parsers")
	}
	if _, err := newAppE(Deps{SetupParsers: defaultSetupParsers([]string{"fake"})}); err == nil {
		t.Fatal("newAppE accepted setup parsers without providers")
	}
}

func TestProductionMailServerProviderWiring(t *testing.T) {
	deps, err := productionDepsE()
	if err != nil {
		t.Fatalf("productionDepsE: %v", err)
	}
	if got, want := deps.Providers.Names(), []string{"outlook", "gmail", "imap", "fastmail"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider order = %#v, want %#v", got, want)
	}
	for _, name := range deps.Providers.Names() {
		caps, err := deps.Providers.Capabilities(context.Background(), name)
		if err != nil {
			t.Fatalf("%s capabilities: %v", name, err)
		}
		for _, cap := range provider.AllCapabilities() {
			if caps.State(cap) == provider.CapSupportedNotImpl {
				t.Fatalf("%s leaves %s as %s", name, cap, provider.CapSupportedNotImpl)
			}
		}
	}
	if got, want := deps.SetupParsers.IDs(), []string{"outlook", "gmail", "mailserver"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parser ids = %#v, want %#v", got, want)
	}
	caps, err := deps.Providers.Capabilities(context.Background(), "fastmail")
	if err != nil {
		t.Fatalf("fastmail capabilities: %v", err)
	}
	if caps.State(provider.CapSendDirect) != provider.CapImplemented || caps.State(provider.CapDraftCreate) != provider.CapImplemented {
		t.Fatalf("fastmail capability states: send=%s draft=%s", caps.State(provider.CapSendDirect), caps.State(provider.CapDraftCreate))
	}
	help := testutil.RunWith(newApp(deps), testutil.Options{}, "login", "--help")
	if help.Exit != 0 {
		t.Fatalf("login help failed: %s", help.Stderr)
	}
	for _, flag := range []string{"--email", "--username", "--imap-host", "--imap-port", "--imap-tls", "--smtp-host", "--smtp-port", "--smtp-tls"} {
		if !strings.Contains(help.Stdout, flag) {
			t.Fatalf("login help missing %s\n%s", flag, help.Stdout)
		}
	}
	if strings.Contains(help.Stdout, "--password") {
		t.Fatalf("login help exposed password flag\n%s", help.Stdout)
	}
}

func TestLoginShortcutAndAccountLifecycle(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)

	var login loginResult
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), &login)
	if !login.Created || login.Reauthenticated || !login.Active || deref(login.ActiveAccount) != "work" {
		t.Fatalf("login result = %#v", login)
	}
	if got := fake.LoginCalls.Load(); got != 1 {
		t.Fatalf("login calls = %d, want 1", got)
	}
	if got := h.browserCalls.Load(); got != 1 {
		t.Fatalf("browser calls = %d, want 1", got)
	}

	fake.Ident.Email = "changed@example.test"
	var reauth loginResult
	decodeOK(t, h.run("auth", "login", "--name", "work", "--json"), &reauth)
	if reauth.Created || !reauth.Reauthenticated || reauth.Email != "changed@example.test" {
		t.Fatalf("reauth result = %#v", reauth)
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"fake", "outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	account, _ := reg.Find("work")
	if account.DefaultFrom != "fake@example.test" || account.Email != "changed@example.test" {
		t.Fatalf("reauth did not preserve default_from/update email: %#v", account)
	}

	var second loginResult
	decodeOK(t, h.run("auth", "login", "--provider", "fake", "--name", "personal", "--no-set-active", "--json"), &second)
	if !second.Created || second.Active || deref(second.ActiveAccount) != "work" {
		t.Fatalf("second login result = %#v", second)
	}

	var switched switchResult
	decodeOK(t, h.run("auth", "switch", "personal", "--json"), &switched)
	if deref(switched.ActiveAccount) != "personal" {
		t.Fatalf("switch active = %#v", switched.ActiveAccount)
	}

	var list listResult
	decodeOK(t, h.run("auth", "list", "--json"), &list)
	if len(list.Accounts) != 2 {
		t.Fatalf("accounts = %#v", list.Accounts)
	}
	if list.Accounts[0].Name != "work" || list.Accounts[0].Active {
		t.Fatalf("work account = %#v", list.Accounts[0])
	}
	if list.Accounts[1].Name != "personal" || !list.Accounts[1].Active {
		t.Fatalf("personal account = %#v", list.Accounts[1])
	}
}

func TestConfiguredSenderIdentityUpdateChangesDefaultFrom(t *testing.T) {
	account := maildoconfig.Account{
		Name:        "work",
		Provider:    "imap",
		Email:       "old@example.test",
		DefaultFrom: "old@example.test",
		AuthRef:     "imap/work",
	}
	ident := provider.Identity{Email: "new@example.test", Assurance: provider.IdentityConfigured}

	if err := enforceAccountIdentity(account, ident); err != nil {
		t.Fatalf("enforceAccountIdentity: %v", err)
	}
	applyInteractiveIdentity(&account, ident)
	if account.Email != "new@example.test" || account.DefaultFrom != "new@example.test" {
		t.Fatalf("identity update = %#v", account)
	}
}

func TestProviderVerifiedIdentityStillRejectsEmailChangeWithoutID(t *testing.T) {
	account := maildoconfig.Account{
		Name:     "work",
		Provider: "gmail",
		Email:    "old@example.test",
		AuthRef:  "gmail/work",
	}
	ident := provider.Identity{Email: "new@example.test", Assurance: provider.IdentityProviderVerified}

	if err := enforceAccountIdentity(account, ident); err == nil {
		t.Fatal("enforceAccountIdentity allowed provider-verified email change")
	}
}

func TestExistingLoginCredentialSourceRequiresUnchangedSetup(t *testing.T) {
	setup := maildoconfig.ProviderSetup{MailServer: &maildoconfig.MailServerSetup{
		Email:    "me@example.test",
		Username: "me@example.test",
		IMAP:     maildoconfig.ServerEndpoint{Host: "imap.example.test", Port: 993, TLS: maildoconfig.TLSImplicit},
		SMTP:     maildoconfig.ServerEndpoint{Host: "smtp.example.test", Port: 465, TLS: maildoconfig.TLSImplicit},
	}}
	reg := &maildoconfig.Registry{Version: 1, Accounts: []maildoconfig.Account{{
		Name:        "work",
		Provider:    "imap",
		Email:       setup.MailServer.Email,
		Username:    setup.MailServer.Username,
		AuthRef:     "imap/work",
		MailServer:  &maildoconfig.MailServerConfig{IMAP: setup.MailServer.IMAP, SMTP: setup.MailServer.SMTP},
		DefaultFrom: setup.MailServer.Email,
	}}}
	account, _ := reg.Find("work")
	h := handlers{}

	source, providerSource, err := h.existingLoginCredentialSource(reg, account, setup, []byte("cache"), true)
	if err != nil {
		t.Fatal(err)
	}
	presence, err := providerSource.WithBytes(context.Background(), func(raw []byte) error {
		if string(raw) != "cache" {
			t.Fatalf("raw cache = %q", raw)
		}
		return nil
	})
	source.close()
	if err != nil || presence != provider.CredentialPresent {
		t.Fatalf("unchanged setup presence = %s, err = %v", presence, err)
	}

	changed := setup.Clone()
	changed.MailServer.IMAP.Host = "imap2.example.test"
	source, providerSource, err = h.existingLoginCredentialSource(reg, account, changed, []byte("cache"), true)
	if err != nil {
		t.Fatal(err)
	}
	presence, err = providerSource.WithBytes(context.Background(), func([]byte) error {
		t.Fatal("changed setup exposed cached credential")
		return nil
	})
	source.close()
	if err != nil || presence != provider.CredentialAbsent {
		t.Fatalf("changed setup presence = %s, err = %v", presence, err)
	}
}

func TestGmailClientIDOverrideConflictFailsBeforeOAuth(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	reg := &maildoconfig.Registry{
		Version:       1,
		ActiveAccount: "personal",
		Gmail:         &maildoconfig.GmailConfig{ClientID: "stored-client"},
		Accounts: []maildoconfig.Account{{
			Name:     "personal",
			Provider: "gmail",
			Email:    "personal@example.test",
			AuthRef:  "gmail/personal",
		}},
	}
	if err := maildoconfig.Save(configPath, reg, []string{"outlook", "gmail"}); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	res := testutil.RunWith(newApp(productionDeps()), testutil.Options{
		LookupEnv: emptyLookupEnv,
		BrowserOpener: func(context.Context, string) error {
			t.Fatal("browser opened before Gmail client-id conflict validation")
			return nil
		},
	}, "--config", configPath, "login", "--provider", "gmail", "--name", "work", "--gmail-client-id", "other-client", "--json")
	if res.Exit == 0 {
		t.Fatalf("login succeeded, want client-id conflict\nstdout=%s\nstderr=%s", res.Stdout, res.Stderr)
	}
	assertNoCode(t, res)
	if !strings.Contains(res.Stderr, "the Gmail client id from --gmail-client-id differs from gmail.client_id") {
		t.Fatalf("stderr = %s", res.Stderr)
	}
}

func TestInteractiveLoginAndMachineModesDoNotPrompt(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	res := h.runWith(testutil.Options{
		Stdin:       strings.NewReader("work\n"),
		Terminal:    true,
		TerminalSet: true,
	}, "login", "--provider", "fake")
	decodeOK(t, res, nil)
	if !credentialExists(t, h, "fake", "work") {
		t.Fatal("interactive login did not create credential cache")
	}

	reader := &failOnRead{}
	machine := h.runWith(testutil.Options{Stdin: reader}, "--json", "login")
	if machine.Exit == 0 {
		t.Fatalf("machine login succeeded, want usage error")
	}
	if reader.reads.Load() != 0 {
		t.Fatal("machine mode attempted to read stdin")
	}
	assertNoCode(t, machine)
}

func TestAuthRefreshRepairAndLoggedOutPaths(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), &loginResult{})
	h.browserCalls.Store(0)

	var refresh refreshResult
	decodeOK(t, h.run("auth", "refresh", "work", "--json"), &refresh)
	if refresh.AuthStatus != authStatusRefreshed || refresh.RotatedRefreshToken {
		t.Fatalf("refresh result = %#v", refresh)
	}
	if fake.SilentRefreshCalls.Load() != 1 || fake.RepairCalls.Load() != 0 || fake.LoginCalls.Load() != 1 {
		t.Fatalf("provider counters: login=%d repair=%d refresh=%d", fake.LoginCalls.Load(), fake.RepairCalls.Load(), fake.SilentRefreshCalls.Load())
	}
	if h.browserCalls.Load() != 0 {
		t.Fatalf("refresh opened browser %d times", h.browserCalls.Load())
	}

	fake.RotateOnRefresh = true
	var rotated refreshResult
	decodeOK(t, h.run("auth", "refresh", "work", "--json"), &rotated)
	if !rotated.RotatedRefreshToken {
		t.Fatalf("rotated refresh result = %#v", rotated)
	}

	var logout logoutResult
	decodeOK(t, h.run("auth", "logout", "work", "--json"), &logout)
	if !logout.CredentialCacheDeleted || fake.LogoutCalls.Load() != 1 {
		t.Fatalf("logout = %#v calls=%d", logout, fake.LogoutCalls.Load())
	}
	before := fake.SilentRefreshCalls.Load()
	var loggedOut refreshResult
	decodeOK(t, h.run("auth", "refresh", "work", "--json"), &loggedOut)
	if loggedOut.AuthStatus != authStatusLoggedOut || loggedOut.RefreshChecked || loggedOut.LoginCommand == "" {
		t.Fatalf("logged out refresh = %#v", loggedOut)
	}
	if fake.SilentRefreshCalls.Load() != before {
		t.Fatal("logged-out refresh called provider")
	}

	h.browserCalls.Store(0)
	var repair repairResult
	decodeOK(t, h.run("auth", "repair", "work", "--json"), &repair)
	if !repair.Repaired || repair.AuthStatus != authStatusValid || h.browserCalls.Load() != 1 {
		t.Fatalf("repair = %#v browser=%d", repair, h.browserCalls.Load())
	}
}

func TestRenameRemoveAndValidation(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), &loginResult{})
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "office", "--no-set-active", "--json"), &loginResult{})

	assertErrorCode(t, h.run("auth", "remove", "work", "--json"), errs.CodeInvalidActiveAccount)
	assertErrorCode(t, h.run("auth", "remove", "work", "--force", "--set-active", "office", "--json"), errs.CodeInvalidActiveAccount)
	assertErrorCode(t, h.run("auth", "remove", "work", "--set-active", "missing", "--json"), errs.CodeUnknownAccount)
	assertErrorCode(t, h.run("auth", "remove", "work", "--set-active", "work", "--json"), errs.CodeInvalidActiveAccount)

	var renamed renameResult
	decodeOK(t, h.run("auth", "rename", "office", "personal", "--json"), &renamed)
	if renamed.OldName != "office" || renamed.NewName != "personal" || !renamed.CredentialCacheMigrated {
		t.Fatalf("rename = %#v", renamed)
	}
	assertErrorCode(t, h.run("auth", "rename", "personal", "work", "--json"), errs.CodeAccountAlreadyExists)

	seedAccount(t, h, maildoconfig.Account{Name: "loggedout", Provider: "fake", AuthRef: "fake/loggedout"}, false)
	var renamedLoggedOut renameResult
	decodeOK(t, h.run("auth", "rename", "loggedout", "nocache", "--json"), &renamedLoggedOut)
	if renamedLoggedOut.CredentialCacheMigrated {
		t.Fatalf("logged-out rename migrated cache: %#v", renamedLoggedOut)
	}

	var removed removeResult
	decodeOK(t, h.run("auth", "remove", "work", "--set-active", "personal", "--json"), &removed)
	if !removed.Removed || deref(removed.ActiveAccount) != "personal" {
		t.Fatalf("remove active with set-active = %#v", removed)
	}
	var forced removeResult
	decodeOK(t, h.run("auth", "remove", "personal", "--force", "--json"), &forced)
	if !forced.Removed || forced.ActiveAccount != nil {
		t.Fatalf("force remove = %#v", forced)
	}
}

func TestOutputModesDryRunsAndSecretRedaction(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)

	preview := decodePreview(t, h.run("login", "--provider", "fake", "--name", "work", "--dry-run", "--json"))
	if preview["dry_run"] != true || fake.LoginCalls.Load() != 0 {
		t.Fatalf("dry-run preview = %#v calls=%d", preview, fake.LoginCalls.Load())
	}
	body := preview["body"].(map[string]any)
	if body["auth_ref"] != "fake/work" || body["set_active"] != "true" {
		t.Fatalf("dry-run body = %#v", body)
	}

	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), &loginResult{})
	jq := h.run("auth", "status", "work", "--json", "--jq", ".account")
	if strings.TrimSpace(jq.Stdout) != "work" {
		t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("auth", "status", "work", "--template", "{{.account}}:{{.auth_status}}")
	if strings.TrimSpace(tmpl.Stdout) != "work:valid" {
		t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	var envelope struct {
		Data listResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("auth", "list", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["command_path"] != "maildo auth list" {
		t.Fatalf("meta = %#v", envelope.Meta.Extra)
	}
	if plain := h.run("auth", "list", "--plain"); plain.Exit == 0 {
		t.Fatalf("--plain unexpectedly succeeded: %s", plain.Stdout)
	}
	if bad := h.run("auth", "refresh", "work", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("--include-meta --dry-run succeeded")
	}

	draftFake := testkit.NewFakeDraftProvider("fake")
	draftHarness := newFakeHarness(t, draftFake)
	account := workAccount()
	seedRegistry(t, draftHarness.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(draftHarness.root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	composePreview := decodePreview(t, draftHarness.run("draft", "create", "--to", "client@example.test", "--subject", "Preview", "--body", "compose secret body", "--dry-run", "--json"))
	if draftFake.Drafts.CreateCalls.Load() != 0 {
		t.Fatalf("compose dry-run mutated: %d", draftFake.Drafts.CreateCalls.Load())
	}
	if strings.Contains(previewString(composePreview), "compose secret body") {
		t.Fatalf("compose preview leaked body: %#v", composePreview)
	}
	replyPreview := decodePreview(t, draftHarness.run("reply", "--id", "src1", "--body", "reply secret body", "--dry-run", "--json"))
	if draftFake.Drafts.ReplyCreateCalls.Load() != 0 {
		t.Fatalf("reply dry-run mutated: %d", draftFake.Drafts.ReplyCreateCalls.Load())
	}
	// A reply preview shows who gets the reply and the whole body, so a
	// person or agent can check it before it goes out.
	replyMessage, _ := replyPreview["message"].(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(replyMessage["body"]), "reply secret body") || !strings.Contains(fmt.Sprint(replyMessage["body"]), "> original source line two") || !strings.HasSuffix(fmt.Sprint(replyMessage["subject"]), ": fake") {
		t.Fatalf("reply preview = %#v", replyPreview)
	}
	if bad := draftHarness.run("send", "--yes", "--to", "client@example.test", "--subject", "Preview", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("compose --include-meta --dry-run succeeded")
	}
	if bad := draftHarness.run("reply", "--id", "src1", "--body", "x", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("reply --include-meta --dry-run succeeded")
	}

	secret := "secret-error-token-12345"
	secretProvider := &secretErrorProvider{name: "boom", secret: secret}
	secretHarness := newFakeHarness(t, testkit.NewFakeProvider("fake"))
	secretHarness.providers = testkit.NewProviderRegistry(secretProvider)
	res := secretHarness.run("login", "--provider", "boom", "--name", "err", "--json")
	if res.Exit == 0 {
		t.Fatal("secret error login succeeded")
	}
	testutil.AssertRedacted(t, res, secret)
	if !strings.Contains(res.Stderr, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %s", res.Stderr)
	}
}

func TestDryRunPreviewMasksSecretFields(t *testing.T) {
	var out bytes.Buffer
	f := &rungrad.Factory{Flags: &rungrad.GlobalFlags{JSON: true}, Stdout: &out, Stderr: &bytes.Buffer{}}
	err := f.WritePreview(output.DryRunPreview{
		Method: "test",
		Path:   "fake/work",
		Body:   []output.Field{{Name: "token", Value: "secret-preview-token-12345", Secret: true}},
	})
	if err != nil {
		t.Fatalf("WritePreview: %v", err)
	}
	if strings.Contains(out.String(), "secret-preview-token-12345") || !strings.Contains(out.String(), `"token": "***"`) {
		t.Fatalf("preview did not mask secret: %s", out.String())
	}
	out.Reset()
	err = f.WritePreview(output.DryRunPreview{
		Method: "compose",
		Path:   "work",
		Body:   []output.Field{{Name: "body_source", Value: "inline"}},
	})
	if err != nil {
		t.Fatalf("WritePreview compose: %v", err)
	}
	if strings.Contains(out.String(), "body text") || !strings.Contains(out.String(), `"body_source": "inline"`) {
		t.Fatalf("compose preview shape = %s", out.String())
	}
	out.Reset()
	err = f.WritePreview(output.DryRunPreview{
		Method: "reply",
		Path:   "work",
		Body: []output.Field{
			{Name: "body_source", Value: "inline"},
			{Name: "token", Value: "reply-preview-token-12345", Secret: true},
		},
	})
	if err != nil {
		t.Fatalf("WritePreview reply: %v", err)
	}
	if strings.Contains(out.String(), "reply-preview-token-12345") || !strings.Contains(out.String(), `"token": "***"`) || !strings.Contains(out.String(), `"body_source": "inline"`) {
		t.Fatalf("reply preview masking = %s", out.String())
	}
}

func TestProductionOutlookFakeExclusionCatalogAndMalformedConfig(t *testing.T) {
	testutil.AssertConsistent(t, func() *rungrad.App { return newApp(productionDeps()) })

	manifest := testutil.CaptureManifest(t, newApp(productionDeps()))
	if len(manifest.Commands) < 60 {
		t.Fatalf("manifest command entries = %d, want the whole command tree", len(manifest.Commands))
	}
	if len(manifest.Commands[0].Path) != 0 {
		t.Fatalf("first manifest command path = %#v, want root", manifest.Commands[0].Path)
	}
	if manifest.SchemaVersion != runmanifest.SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", manifest.SchemaVersion, runmanifest.SchemaVersion)
	}
	if got := newApp(productionDeps()).Root().Example; !strings.HasPrefix(got, "maildo login\nmaildo list\n") {
		t.Fatalf("root examples = %q", got)
	}
	var paths []string
	var accountFlagPaths []string
	for _, spec := range manifest.Commands {
		if len(spec.Path) == 0 {
			continue
		}
		joined := strings.Join(spec.Path, " ")
		if strings.HasPrefix(strings.Join(spec.Path, " "), "scopekit") {
			t.Fatalf("production manifest exposed scopekit command: %#v", spec.Path)
		}
		for _, flag := range spec.LocalFlags {
			if flag.Name == "account" {
				accountFlagPaths = append(accountFlagPaths, joined)
			}
		}
		paths = append(paths, joined)
	}
	sort.Strings(paths)
	want := paths
	assertCatalogPathSet(t, newApp(productionDeps()), want)
	assertRelatedTargets(t, manifest)
	assertHandlerAuthSet(t, newApp(productionDeps()), manifest)
	if len(accountFlagPaths) < 40 {
		t.Fatalf("account flag paths = %#v, want every mailbox command", accountFlagPaths)
	}
	foldersList := findManifestCommand(t, manifest, "folders list")
	foldersView := findManifestCommand(t, manifest, "folders view")
	foldersCreate := findManifestCommand(t, manifest, "folders create")
	foldersRename := findManifestCommand(t, manifest, "folders rename")
	foldersDelete := findManifestCommand(t, manifest, "folders delete")
	for _, tt := range []struct {
		command runmanifest.Command
		want    []string
	}{
		{command: foldersCreate, want: []string{
			"maildo folders create Clients --account work",
			"maildo folders create ACME --parent Clients",
			"maildo folders create Receipts --dry-run --json",
		}},
		{command: foldersRename, want: []string{
			"maildo folders rename Clients Accounts",
			"maildo folders rename clients-id Accounts --account work",
			"maildo folders rename Clients Accounts --dry-run --json",
		}},
		{command: foldersDelete, want: []string{
			"maildo folders delete \"Old Projects\" --account work --dry-run",
			"maildo folders delete old-projects-id --confirm old-projects-id",
			"maildo folders delete \"Clients/2024\" --confirm \"Clients/2024\" --json",
		}},
	} {
		if !reflect.DeepEqual(tt.command.Examples, tt.want) {
			t.Fatalf("folder command examples for %q = %#v, want %#v", strings.Join(tt.command.Path, " "), tt.command.Examples, tt.want)
		}
	}
	rulesList := findManifestCommand(t, manifest, "rules list")
	rulesView := findManifestCommand(t, manifest, "rules view")
	rulesHealth := findManifestCommand(t, manifest, "rules health")
	rulesCreate := findManifestCommand(t, manifest, "rules create")
	rulesUpdate := findManifestCommand(t, manifest, "rules update")
	rulesEnable := findManifestCommand(t, manifest, "rules enable")
	rulesDisable := findManifestCommand(t, manifest, "rules disable")
	rulesDelete := findManifestCommand(t, manifest, "rules delete")
	outlookRoot := findManifestCommand(t, manifest, "outlook")
	outlookArchivePolicy := findManifestCommand(t, manifest, "outlook archive-policy")
	outlookArchivePolicyCheck := findManifestCommand(t, manifest, "outlook archive-policy check")
	searchCmd := findManifestCommand(t, manifest, "search")
	listCmd := findManifestCommand(t, manifest, "list")
	readCmd := findManifestCommand(t, manifest, "read")
	threadCmd := findManifestCommand(t, manifest, "thread")
	draftCreate := findManifestCommand(t, manifest, "draft create")
	draftRead := findManifestCommand(t, manifest, "draft read")
	draftUpdate := findManifestCommand(t, manifest, "draft update")
	draftSend := findManifestCommand(t, manifest, "draft send")
	sendCmd := findManifestCommand(t, manifest, "send")
	replyCmd := findManifestCommand(t, manifest, "reply")
	forwardCmd := findManifestCommand(t, manifest, "forward")
	attachmentsList := findManifestCommand(t, manifest, "attachments list")
	attachmentsDownload := findManifestCommand(t, manifest, "attachments download")
	attachmentsAdd := findManifestCommand(t, manifest, "attachments add")
	attachmentsRemove := findManifestCommand(t, manifest, "attachments remove")
	moveCmd := findManifestCommand(t, manifest, "move")
	copyCmd := findManifestCommand(t, manifest, "copy")
	archiveCmd := findManifestCommand(t, manifest, "archive")
	deleteCmd := findManifestCommand(t, manifest, "delete")
	restoreCmd := findManifestCommand(t, manifest, "restore")
	junkCmd := findManifestCommand(t, manifest, "junk")
	markReadCmd := findManifestCommand(t, manifest, "mark read")
	markUnreadCmd := findManifestCommand(t, manifest, "mark unread")
	flagSetCmd := findManifestCommand(t, manifest, "flag set")
	flagClearCmd := findManifestCommand(t, manifest, "flag clear")
	categoriesListCmd := findManifestCommand(t, manifest, "categories list")
	categoriesSetCmd := findManifestCommand(t, manifest, "categories set")
	categoriesClearCmd := findManifestCommand(t, manifest, "categories clear")
	updateCmd := findManifestCommand(t, manifest, "update")
	assertFlagSet(t, foldersList, map[string]string{"account": "stringArray", "all-accounts": "bool"}, nil)
	assertFlagSet(t, foldersView, map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	assertFlagSet(t, foldersCreate, map[string]string{"account": "stringArray", "parent": "string"}, []string{"all-accounts", "apply", "confirm"})
	assertFlagSet(t, foldersRename, map[string]string{"account": "stringArray"}, []string{"all-accounts", "apply", "confirm", "parent"})
	assertFlagSet(t, foldersDelete, map[string]string{"account": "stringArray", "confirm": "string"}, []string{"all-accounts", "apply", "parent"})
	assertFlagSet(t, rulesList, map[string]string{"account": "stringArray", "all-accounts": "bool"}, []string{"folder"})
	assertFlagSet(t, rulesView, map[string]string{"account": "stringArray"}, []string{"all-accounts", "folder"})
	assertFlagSet(t, rulesHealth, map[string]string{"account": "stringArray", "all-accounts": "bool"}, []string{"folder"})
	ruleCreateFlags := map[string]string{"account": "stringArray", "name": "string", "from": "stringArray", "sender-contains": "stringArray", "subject-contains": "stringArray", "recipient-contains": "stringArray", "header-contains": "stringArray", "has-attachments": "bool", "move-to": "string", "mark-read": "bool", "stop-processing": "bool", "sequence": "int64", "apply": "bool"}
	assertFlagSet(t, rulesCreate, ruleCreateFlags, []string{"all-accounts", "folder", "plain", "confirm", "no-has-attachments", "no-mark-read", "no-stop-processing"})
	ruleUpdateFlags := map[string]string{"account": "stringArray", "name": "string", "from": "stringArray", "sender-contains": "stringArray", "subject-contains": "stringArray", "recipient-contains": "stringArray", "header-contains": "stringArray", "has-attachments": "bool", "move-to": "string", "mark-read": "bool", "stop-processing": "bool", "sequence": "int64", "apply": "bool", "no-has-attachments": "bool", "no-mark-read": "bool", "no-stop-processing": "bool"}
	assertFlagSet(t, rulesUpdate, ruleUpdateFlags, []string{"all-accounts", "folder", "plain", "confirm"})
	assertFlagSet(t, rulesEnable, map[string]string{"account": "stringArray"}, []string{"all-accounts", "folder", "plain", "apply", "confirm"})
	assertFlagSet(t, rulesDisable, map[string]string{"account": "stringArray"}, []string{"all-accounts", "folder", "plain", "apply", "confirm"})
	assertFlagSet(t, rulesDelete, map[string]string{"account": "stringArray", "confirm": "string"}, []string{"all-accounts", "folder", "plain", "apply"})
	assertFlagSet(t, outlookRoot, map[string]string{}, []string{"account", "all-accounts", "plain"})
	assertFlagSet(t, outlookArchivePolicy, map[string]string{}, []string{"account", "all-accounts", "plain"})
	assertFlagSet(t, outlookArchivePolicyCheck, map[string]string{"account": "stringArray"}, []string{"all-accounts", "plain"})
	assertFlagSet(t, searchCmd, map[string]string{"account": "stringArray", "all-accounts": "bool", "folder": "string", "limit": "int"}, nil)
	assertFlagSet(t, listCmd, map[string]string{"account": "stringArray", "all-accounts": "bool", "unread": "bool", "limit": "int"}, []string{"from", "subject"})
	assertFlagSet(t, readCmd, map[string]string{"account": "stringArray", "all-accounts": "bool", "id": "string", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool", "body-mode": "string"}, []string{"thread-id", "all"})
	assertFlagSet(t, threadCmd, map[string]string{"account": "stringArray", "all-accounts": "bool", "id": "string", "internet-id": "string", "thread-id": "string", "query": "string", "latest": "bool", "first": "bool"}, []string{"body-mode", "all"})
	composeFlagTypes := map[string]string{"account": "stringArray", "to": "stringArray", "cc": "stringArray", "bcc": "stringArray", "reply-to": "stringArray", "from": "string", "subject": "string", "body": "string", "body-file": "string", "text": "bool", "markdown": "bool", "html": "bool"}
	assertFlagSet(t, draftCreate, composeFlagTypes, []string{"all-accounts", "id", "clear-to"})
	assertFlagSet(t, sendCmd, composeFlagTypes, []string{"all-accounts", "id", "clear-to"})
	updateFlagTypes := map[string]string{"account": "stringArray", "id": "string", "to": "stringArray", "cc": "stringArray", "bcc": "stringArray", "reply-to": "stringArray", "from": "string", "subject": "string", "body": "string", "body-file": "string", "text": "bool", "markdown": "bool", "html": "bool", "clear-to": "bool", "clear-cc": "bool", "clear-bcc": "bool", "clear-reply-to": "bool"}
	assertFlagSet(t, draftUpdate, updateFlagTypes, []string{"all-accounts"})
	assertFlagSet(t, draftRead, map[string]string{"account": "stringArray", "id": "string", "body-mode": "string"}, []string{"all-accounts", "to", "body"})
	assertFlagSet(t, draftSend, map[string]string{"account": "stringArray", "id": "string"}, []string{"all-accounts", "to", "body"})
	assertFlagSet(t, replyCmd, map[string]string{
		"account": "stringArray", "id": "string", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool",
		"reply-all": "bool", "draft": "bool", "send": "bool", "quote": "string", "quote-language": "string",
		"body": "string", "body-file": "string", "text": "bool", "markdown": "bool", "html": "bool",
	}, []string{"all-accounts", "thread-id", "all", "to", "cc", "bcc", "reply-to", "from", "subject", "clear-to"})
	assertFlagSet(t, forwardCmd, map[string]string{
		"account": "stringArray", "id": "string", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool",
		"draft": "bool", "send": "bool", "to": "stringArray", "cc": "stringArray", "bcc": "stringArray",
		"include-attachments": "string", "include-attachment": "stringArray", "include-attachment-id": "stringArray",
		"body": "string", "body-file": "string", "text": "bool", "markdown": "bool", "html": "bool",
	}, []string{"all-accounts", "thread-id", "all", "reply-all", "reply-to", "from", "subject", "quote", "clear-to"})
	assertFlagSet(t, attachmentsList, map[string]string{"account": "stringArray", "all-accounts": "bool", "draft-id": "string", "id": "string", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool"}, []string{"to", "body"})
	assertFlagSet(t, attachmentsDownload, map[string]string{"account": "stringArray", "id": "string", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool", "to": "string", "all": "bool", "name": "stringArray", "attachment-id": "stringArray", "overwrite": "bool"}, []string{"all-accounts", "draft-id", "body"})
	assertFlagSet(t, attachmentsAdd, map[string]string{"account": "stringArray", "draft-id": "string"}, []string{"all-accounts", "id", "to", "body"})
	assertFlagSet(t, attachmentsRemove, map[string]string{"account": "stringArray", "draft-id": "string", "name": "string", "attachment-id": "string"}, []string{"all-accounts", "id", "to", "body"})
	mutationSelectorFlags := map[string]string{"account": "stringArray", "id": "stringArray", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool", "ids-file": "string", "all": "bool", "max-targets": "int", "apply": "bool"}
	mutationFolderFlags := map[string]string{"account": "stringArray", "id": "stringArray", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool", "ids-file": "string", "all": "bool", "max-targets": "int", "apply": "bool", "to": "string"}
	mutationSingleSelectorFlags := map[string]string{"account": "stringArray", "id": "stringArray", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool"}
	for _, c := range []runmanifest.Command{moveCmd, copyCmd, restoreCmd} {
		assertFlagSet(t, c, mutationFolderFlags, []string{"all-accounts", "category"})
	}
	for _, c := range []runmanifest.Command{archiveCmd, deleteCmd, junkCmd, markReadCmd, markUnreadCmd, flagSetCmd, flagClearCmd} {
		assertFlagSet(t, c, mutationSelectorFlags, []string{"all-accounts", "to", "category"})
	}
	assertFlagSet(t, categoriesListCmd, mutationSingleSelectorFlags, []string{"all-accounts", "to", "category", "ids-file", "all", "max-targets", "apply"})
	categoryFlags := map[string]string{"account": "stringArray", "id": "stringArray", "internet-id": "string", "query": "string", "latest": "bool", "first": "bool", "ids-file": "string", "all": "bool", "max-targets": "int", "apply": "bool", "category": "stringArray"}
	assertFlagSet(t, categoriesSetCmd, categoryFlags, []string{"all-accounts", "to"})
	assertFlagSet(t, categoriesClearCmd, categoryFlags, []string{"all-accounts", "to"})
	assertFlagSet(t, updateCmd, map[string]string{"check": "bool"}, []string{"account", "all-accounts"})
	assertCommandModes(t, foldersList, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, foldersView, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, foldersCreate, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, foldersRename, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, foldersDelete, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, rulesList, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, rulesView, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, rulesHealth, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	for _, c := range []runmanifest.Command{rulesCreate, rulesUpdate, rulesEnable, rulesDisable, rulesDelete} {
		assertCommandModes(t, c, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	}
	assertCommandModes(t, outlookArchivePolicyCheck, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	if len(outlookRoot.OutputModes) != 0 || len(outlookArchivePolicy.OutputModes) != 0 || outlookRoot.SupportsMeta || outlookArchivePolicy.SupportsMeta ||
		outlookRoot.Mutates || outlookArchivePolicy.Mutates || outlookArchivePolicyCheck.Mutates || outlookRoot.Destructive || outlookArchivePolicy.Destructive || outlookArchivePolicyCheck.Destructive ||
		outlookRoot.RequiresConfirmation || outlookArchivePolicy.RequiresConfirmation || outlookArchivePolicyCheck.RequiresConfirmation ||
		outlookRoot.Short != "Commands that only work with Outlook accounts" ||
		!reflect.DeepEqual(outlookRoot.Examples, []string{"maildo outlook archive-policy check"}) ||
		!reflect.DeepEqual(outlookRoot.Related, []string{"rules health", "folders list"}) ||
		outlookArchivePolicy.Short != "Check Outlook archive and retention settings" ||
		!reflect.DeepEqual(outlookArchivePolicy.Examples, []string{"maildo outlook archive-policy check"}) ||
		!reflect.DeepEqual(outlookArchivePolicy.Related, []string{"folders list", "rules health"}) ||
		!outlookArchivePolicyCheck.SupportsMeta || outlookArchivePolicyCheck.Short != "Show which archive and retention settings maildo can see" ||
		!reflect.DeepEqual(outlookArchivePolicyCheck.Examples, []string{"maildo outlook archive-policy check", "maildo outlook archive-policy check --account work", "maildo outlook archive-policy check --json"}) ||
		!reflect.DeepEqual(outlookArchivePolicyCheck.Related, []string{"folders list", "rules health"}) {
		t.Fatalf("outlook archive-policy manifest contract differs: root=%#v intermediate=%#v leaf=%#v", outlookRoot, outlookArchivePolicy, outlookArchivePolicyCheck)
	}
	outlookModule := newOutlookModule(nil)
	if groups := outlookModule.Groups(); !reflect.DeepEqual(groups, []rungrad.Group{{ID: "outlook", Title: "Outlook"}}) || outlookModule.defs[0].groupID != "outlook" || outlookModule.defs[1].groupID != "" || outlookModule.defs[2].groupID != "" {
		t.Fatalf("outlook group contract differs: groups=%#v defs=%#v", groups, outlookModule.defs)
	}
	assertCommandModes(t, searchCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, listCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, readCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, threadCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, draftCreate, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, draftRead, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, draftUpdate, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, draftSend, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, sendCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, replyCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, forwardCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, attachmentsList, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, attachmentsDownload, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, attachmentsAdd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	assertCommandModes(t, attachmentsRemove, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate})
	mutationModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	for _, c := range []runmanifest.Command{moveCmd, copyCmd, archiveCmd, deleteCmd, restoreCmd, junkCmd, markReadCmd, markUnreadCmd, flagSetCmd, flagClearCmd, categoriesListCmd, categoriesSetCmd, categoriesClearCmd} {
		assertCommandModes(t, c, mutationModes)
	}
	assertCommandModes(t, updateCmd, []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON})
	if !foldersCreate.Mutates || !foldersRename.Mutates || !foldersDelete.Mutates ||
		!draftCreate.Mutates || draftRead.Mutates || !draftUpdate.Mutates || !draftSend.Mutates || !sendCmd.Mutates || !replyCmd.Mutates || !forwardCmd.Mutates || attachmentsList.Mutates || !attachmentsDownload.Mutates || !attachmentsAdd.Mutates || !attachmentsRemove.Mutates ||
		!moveCmd.Mutates || !copyCmd.Mutates || !archiveCmd.Mutates || !deleteCmd.Mutates || !restoreCmd.Mutates || !junkCmd.Mutates || !markReadCmd.Mutates || !markUnreadCmd.Mutates || !flagSetCmd.Mutates || !flagClearCmd.Mutates || categoriesListCmd.Mutates || !categoriesSetCmd.Mutates || !categoriesClearCmd.Mutates {
		t.Fatalf("compose mutates flags: create=%t read=%t update=%t draftSend=%t send=%t reply=%t forward=%t attachmentsList=%t attachmentsDownload=%t attachmentsAdd=%t attachmentsRemove=%t", draftCreate.Mutates, draftRead.Mutates, draftUpdate.Mutates, draftSend.Mutates, sendCmd.Mutates, replyCmd.Mutates, forwardCmd.Mutates, attachmentsList.Mutates, attachmentsDownload.Mutates, attachmentsAdd.Mutates, attachmentsRemove.Mutates)
	}
	if foldersCreate.Destructive || foldersRename.Destructive || !foldersDelete.Destructive || foldersCreate.RequiresConfirmation || foldersRename.RequiresConfirmation || !foldersDelete.RequiresConfirmation {
		t.Fatalf("folder destructive metadata: create=%t/%t rename=%t/%t delete=%t/%t", foldersCreate.Destructive, foldersCreate.RequiresConfirmation, foldersRename.Destructive, foldersRename.RequiresConfirmation, foldersDelete.Destructive, foldersDelete.RequiresConfirmation)
	}
	if !updateCmd.Mutates || updateCmd.RequiresAuth || updateCmd.SupportsMeta || updateCmd.Destructive || updateCmd.RequiresConfirmation ||
		!reflect.DeepEqual(updateCmd.Examples, []string{"maildo update --check", "maildo update --check --json", "maildo update"}) ||
		len(updateCmd.Related) != 0 {
		t.Fatalf("update manifest contract differs: %#v", updateCmd)
	}
	if rulesList.Mutates || rulesView.Mutates || rulesHealth.Mutates ||
		!rulesCreate.Mutates || !rulesUpdate.Mutates || !rulesEnable.Mutates || !rulesDisable.Mutates || !rulesDelete.Mutates {
		t.Fatalf("rules mutates flags: list=%t view=%t health=%t create=%t update=%t enable=%t disable=%t delete=%t", rulesList.Mutates, rulesView.Mutates, rulesHealth.Mutates, rulesCreate.Mutates, rulesUpdate.Mutates, rulesEnable.Mutates, rulesDisable.Mutates, rulesDelete.Mutates)
	}
	if rulesCreate.Destructive || rulesUpdate.Destructive || rulesEnable.Destructive || rulesDisable.Destructive || !rulesDelete.Destructive ||
		rulesCreate.RequiresConfirmation || rulesUpdate.RequiresConfirmation || rulesEnable.RequiresConfirmation || rulesDisable.RequiresConfirmation || !rulesDelete.RequiresConfirmation {
		t.Fatalf("rule destructive metadata: create=%t/%t update=%t/%t enable=%t/%t disable=%t/%t delete=%t/%t", rulesCreate.Destructive, rulesCreate.RequiresConfirmation, rulesUpdate.Destructive, rulesUpdate.RequiresConfirmation, rulesEnable.Destructive, rulesEnable.RequiresConfirmation, rulesDisable.Destructive, rulesDisable.RequiresConfirmation, rulesDelete.Destructive, rulesDelete.RequiresConfirmation)
	}
	if !foldersList.SupportsMeta || !foldersView.SupportsMeta || !foldersCreate.SupportsMeta || !foldersRename.SupportsMeta || !foldersDelete.SupportsMeta || !searchCmd.SupportsMeta || !listCmd.SupportsMeta || !readCmd.SupportsMeta || !threadCmd.SupportsMeta ||
		!draftCreate.SupportsMeta || !draftRead.SupportsMeta || !draftUpdate.SupportsMeta || !draftSend.SupportsMeta || !sendCmd.SupportsMeta || !replyCmd.SupportsMeta || !forwardCmd.SupportsMeta || !attachmentsList.SupportsMeta || !attachmentsDownload.SupportsMeta || !attachmentsAdd.SupportsMeta || !attachmentsRemove.SupportsMeta ||
		!moveCmd.SupportsMeta || !copyCmd.SupportsMeta || !archiveCmd.SupportsMeta || !deleteCmd.SupportsMeta || !restoreCmd.SupportsMeta || !junkCmd.SupportsMeta || !markReadCmd.SupportsMeta || !markUnreadCmd.SupportsMeta || !flagSetCmd.SupportsMeta || !flagClearCmd.SupportsMeta || !categoriesListCmd.SupportsMeta || !categoriesSetCmd.SupportsMeta || !categoriesClearCmd.SupportsMeta ||
		len(foldersList.Examples) == 0 || len(foldersView.Examples) == 0 || len(foldersCreate.Examples) == 0 || len(foldersRename.Examples) == 0 || len(foldersDelete.Examples) == 0 || len(searchCmd.Examples) == 0 || len(listCmd.Examples) == 0 || len(readCmd.Examples) == 0 || len(threadCmd.Examples) == 0 ||
		len(draftCreate.Examples) == 0 || len(draftRead.Examples) == 0 || len(draftUpdate.Examples) == 0 || len(draftSend.Examples) == 0 || len(sendCmd.Examples) == 0 || len(replyCmd.Examples) == 0 || len(forwardCmd.Examples) == 0 || len(attachmentsList.Examples) == 0 || len(attachmentsDownload.Examples) == 0 || len(attachmentsAdd.Examples) == 0 || len(attachmentsRemove.Examples) == 0 ||
		len(moveCmd.Examples) == 0 || len(copyCmd.Examples) == 0 || len(archiveCmd.Examples) == 0 || len(deleteCmd.Examples) == 0 || len(restoreCmd.Examples) == 0 || len(junkCmd.Examples) == 0 || len(markReadCmd.Examples) == 0 || len(markUnreadCmd.Examples) == 0 || len(flagSetCmd.Examples) == 0 || len(flagClearCmd.Examples) == 0 || len(categoriesListCmd.Examples) == 0 || len(categoriesSetCmd.Examples) == 0 || len(categoriesClearCmd.Examples) == 0 {
		t.Fatalf("manifest entries missing meta/examples: foldersList=%#v foldersView=%#v search=%#v list=%#v read=%#v thread=%#v draftCreate=%#v draftRead=%#v draftUpdate=%#v draftSend=%#v send=%#v reply=%#v forward=%#v attachmentsList=%#v attachmentsDownload=%#v attachmentsAdd=%#v attachmentsRemove=%#v", foldersList, foldersView, searchCmd, listCmd, readCmd, threadCmd, draftCreate, draftRead, draftUpdate, draftSend, sendCmd, replyCmd, forwardCmd, attachmentsList, attachmentsDownload, attachmentsAdd, attachmentsRemove)
	}
	if !rulesList.SupportsMeta || !rulesView.SupportsMeta || !rulesHealth.SupportsMeta ||
		!rulesCreate.SupportsMeta || !rulesUpdate.SupportsMeta || !rulesEnable.SupportsMeta || !rulesDisable.SupportsMeta || !rulesDelete.SupportsMeta ||
		len(rulesList.Examples) == 0 || len(rulesView.Examples) == 0 || len(rulesHealth.Examples) == 0 ||
		len(rulesCreate.Examples) == 0 || len(rulesUpdate.Examples) == 0 || len(rulesEnable.Examples) == 0 || len(rulesDisable.Examples) == 0 || len(rulesDelete.Examples) == 0 {
		t.Fatalf("rules manifest entries missing meta/examples: list=%#v view=%#v health=%#v create=%#v update=%#v enable=%#v disable=%#v delete=%#v", rulesList, rulesView, rulesHealth, rulesCreate, rulesUpdate, rulesEnable, rulesDisable, rulesDelete)
	}
	generatedDocs := docsgen.Generate(newApp(productionDeps()))
	for _, path := range []string{"maildo_outlook.md", "maildo_outlook_archive-policy.md", "maildo_outlook_archive-policy_check.md"} {
		if _, ok := generatedDocs[path]; !ok {
			t.Fatalf("generated docs missing %q", path)
		}
	}
	if _, ok := generatedDocs["maildo_outlook_check.md"]; ok {
		t.Fatal("generated docs exposed invalid outlook check page")
	}
	allDocs := ""
	for _, page := range generatedDocs {
		allDocs += page
	}
	help := testutil.Run(newApp(productionDeps()), "--help")
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if strings.Contains(allDocs, "fake") || strings.Contains(help.Stdout, "fake") ||
		strings.Contains(allDocs, "scopekit") || strings.Contains(help.Stdout, "scopekit") {
		t.Fatalf("production surfaces mention test-only providers or commands\nhelp=%s", help.Stdout)
	}
	for _, forbidden := range []string{"ProviderState", "provider_state"} {
		if strings.Contains(allDocs, forbidden) || strings.Contains(help.Stdout, forbidden) || strings.Contains(string(manifestBytes), forbidden) {
			t.Fatalf("production docs/help/manifest mention provider state marker %q", forbidden)
		}
	}
	for path, content := range testutil.CaptureAllHelp(newApp(productionDeps())) {
		if strings.Contains(content, "fake") || strings.Contains(content, "scopekit") {
			t.Fatalf("production help for %q mentions test-only providers or commands\n%s", path, content)
		}
		for _, forbidden := range []string{"ProviderState", "provider_state"} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("production help for %q mentions provider state marker %q\n%s", path, forbidden, content)
			}
		}
	}

	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\naccounts:\n  - name: work\n    provider: fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prod := testutil.Run(newApp(productionDeps()), "--config", configPath, "auth", "list", "--json")
	assertErrorCode(t, prod, errs.CodeInvalidConfig)

	prodRoot := t.TempDir()
	outlook := testutil.RunWith(newApp(productionDeps()), testutil.Options{
		LookupEnv: func(string) (string, bool) { return "", false },
	}, "--config", filepath.Join(prodRoot, "config.yaml"), "login", "--provider", "outlook", "--name", "work", "--json")
	assertErrorCode(t, outlook, errs.CodeAuthAppConfigMissing)

	if err := os.WriteFile(configPath, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp(productionDeps())
	for _, args := range [][]string{
		{"--config", configPath, "--help"},
		{"--config", configPath, "--version"},
		{"--config", configPath, "__rungrad_manifest"},
	} {
		res := testutil.Run(app, args...)
		if res.Exit != 0 {
			t.Fatalf("%v exit = %d stdout=%s stderr=%s", args, res.Exit, res.Stdout, res.Stderr)
		}
	}
	res := testutil.Run(app, "--config", configPath, "auth", "list", "--json")
	assertErrorCode(t, res, errs.CodeInvalidConfig)
}

func assertCatalogPathSet(t *testing.T, app *rungrad.App, want []string) {
	t.Helper()
	if err := app.ValidateCatalog(); err != nil {
		t.Fatalf("ValidateCatalog() = %v", err)
	}
	specs := app.Catalog()
	if len(specs) != len(want) {
		t.Fatalf("catalog row count = %d, want %d", len(specs), len(want))
	}
	got := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Path == "" {
			t.Fatal("catalog contains root path")
		}
		got = append(got, spec.Path)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog rows = %#v, want %#v", got, want)
	}
}

func assertRelatedTargets(t *testing.T, manifest runmanifest.Manifest) {
	t.Helper()
	visible := map[string]bool{}
	for _, cmd := range manifest.Commands {
		if len(cmd.Path) == 0 {
			continue
		}
		visible[strings.Join(cmd.Path, " ")] = true
	}
	for _, cmd := range manifest.Commands {
		if len(cmd.Path) == 0 {
			continue
		}
		path := strings.Join(cmd.Path, " ")
		seen := map[string]bool{}
		for _, related := range cmd.Related {
			if related == "" {
				t.Fatalf("%s has empty related target", path)
			}
			if related == path {
				t.Fatalf("%s has self related target", path)
			}
			if seen[related] {
				t.Fatalf("%s has duplicate related target %q", path, related)
			}
			seen[related] = true
			if !visible[related] {
				t.Fatalf("%s related target %q is not a visible non-root command", path, related)
			}
			if strings.HasPrefix(related, "maildo ") {
				t.Fatalf("%s related target %q includes tool prefix", path, related)
			}
		}
	}
}

func assertHandlerAuthSet(t *testing.T, app *rungrad.App, manifest runmanifest.Manifest) {
	t.Helper()
	want := []string{
		"archive",
		"attachments add",
		"attachments download",
		"attachments list",
		"attachments remove",
		"categories clear",
		"categories list",
		"categories set",
		"copy",
		"delete",
		"doctor",
		"draft create",
		"draft read",
		"draft send",
		"draft update",
		"flag clear",
		"flag set",
		"folders create",
		"folders delete",
		"folders list",
		"folders rename",
		"folders view",
		"forward",
		"junk",
		"list",
		"mark read",
		"mark unread",
		"move",
		"outlook archive-policy check",
		"read",
		"reply",
		"restore",
		"rules create",
		"rules delete",
		"rules disable",
		"rules enable",
		"rules health",
		"rules list",
		"rules update",
		"rules view",
		"search",
		"send",
		"thread",
	}
	if len(want) != 43 {
		t.Fatalf("test auth expectation has %d rows, want 43", len(want))
	}
	wantSet := map[string]bool{}
	for _, path := range want {
		wantSet[path] = true
	}
	var got []string
	for _, cmd := range manifest.Commands {
		if len(cmd.Path) == 0 {
			if cmd.RequiresAuth {
				t.Fatal("root requires auth")
			}
			continue
		}
		path := strings.Join(cmd.Path, " ")
		if cmd.RequiresAuth {
			got = append(got, path)
		}
		if cmd.RequiresAuth != wantSet[path] {
			t.Fatalf("%s requires_auth = %t, want %t", path, cmd.RequiresAuth, wantSet[path])
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requires_auth paths = %#v, want %#v", got, want)
	}

	live := liveCommandMap(app.Root())
	parentPaths := map[string]bool{
		"auth":                   true,
		"attachments":            true,
		"categories":             true,
		"draft":                  true,
		"flag":                   true,
		"folders":                true,
		"mark":                   true,
		"outlook":                true,
		"outlook archive-policy": true,
		"rules":                  true,
		"config":                 true,
	}
	var runPaths, nonRunnableParents, outputOwners []string
	for path, cmd := range live {
		if path == "" {
			continue
		}
		manifestCmd := findManifestCommand(t, manifest, path)
		if cmd.RunE != nil {
			runPaths = append(runPaths, path)
			if len(manifestCmd.OutputModes) > 0 {
				outputOwners = append(outputOwners, path)
			}
		} else if parentPaths[path] {
			nonRunnableParents = append(nonRunnableParents, path)
		}
		if parentPaths[path] {
			if len(manifestCmd.OutputModes) != 0 || manifestCmd.RequiresAuth || manifestCmd.Mutates || manifestCmd.Destructive {
				t.Fatalf("parent %s publishes leaf metadata: %#v", path, manifestCmd)
			}
		}
		if wantSet[path] {
			if cmd.RunE == nil {
				t.Fatalf("%s is marked auth but has no RunE", path)
			}
			if got := cmd.Annotations["rungrad.authResolution"]; got != string(rungrad.AuthResolutionHandler) {
				t.Fatalf("%s authResolution = %q, want handler", path, got)
			}
		} else if got := cmd.Annotations["rungrad.authResolution"]; got == string(rungrad.AuthResolutionHandler) {
			t.Fatalf("%s unexpectedly uses handler auth", path)
		}
	}
	sort.Strings(runPaths)
	sort.Strings(nonRunnableParents)
	sort.Strings(outputOwners)
	if len(outputOwners) != len(runPaths)-2 {
		t.Fatalf("output-owning paths = %d, want every runnable command except the two help-only outlook parents (%d)", len(outputOwners), len(runPaths)-2)
	}
	for _, path := range []string{"outlook", "outlook archive-policy"} {
		if live[path].RunE == nil {
			t.Fatalf("%s lost help-only handler", path)
		}
	}
}

func liveCommandMap(root *cobra.Command) map[string]*cobra.Command {
	out := map[string]*cobra.Command{"": root}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			if child.Hidden || !child.IsAvailableCommand() {
				continue
			}
			segs := strings.Fields(child.CommandPath())
			path := ""
			if len(segs) > 1 {
				path = strings.Join(segs[1:], " ")
			}
			out[path] = child
			walk(child)
		}
	}
	walk(root)
	return out
}

func TestLoginRollbackOnRegistrySaveAndCredentialCommitFailure(t *testing.T) {
	t.Run("registry save failure", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		blocker := &blockRootLoginProvider{FakeProvider: fake, backup: t.TempDir()}
		h := newFakeHarness(t, fake)
		blocker.backup = filepath.Join(filepath.Dir(h.root), filepath.Base(h.root)+"-blocked")
		blocker.root = h.root
		h.providers = testkit.NewProviderRegistry(blocker)
		res := h.run("login", "--provider", "fake", "--name", "work", "--json")
		_ = os.Remove(h.root)
		_ = os.Rename(blocker.backup, h.root)
		_ = auth.Cleanup(h.root, h.configPath)
		if res.Exit == 0 {
			t.Fatalf("login succeeded, want registry save failure")
		}
		assertErrorCode(t, res, errs.CodeInvalidConfig)
		assertNoPathLeak(t, res, h.root, h.configPath, blocker.backup)
		if credentialExists(t, h, "fake", "work") {
			t.Fatal("final credential exists after registry save failure")
		}
		if _, err := os.Stat(h.configPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("registry stat after save failure = %v, want not exist", err)
		}
	})

	t.Run("credential commit failure restores registry", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		deleter := &deleteStageLoginProvider{FakeProvider: fake}
		h := newFakeHarness(t, fake)
		deleter.root = h.root
		h.providers = testkit.NewProviderRegistry(deleter)
		seedRegistry(t, h.configPath, "old", []maildoconfig.Account{{Name: "old", Provider: "fake", AuthRef: "fake/old"}})
		res := h.run("login", "--provider", "fake", "--name", "work", "--json")
		_ = auth.Cleanup(h.root, h.configPath)
		if res.Exit == 0 {
			t.Fatalf("login succeeded, want credential commit failure")
		}
		assertErrorCode(t, res, errs.CodeInvalidConfig)
		assertNoPathLeak(t, res, h.root, h.configPath)
		reg, err := maildoconfig.Load(h.configPath, []string{"fake", "outlook"})
		if err != nil {
			t.Fatalf("load restored registry: %v", err)
		}
		if _, ok := reg.Find("work"); ok {
			t.Fatal("failed login left account in registry")
		}
		if _, ok := reg.Find("old"); !ok {
			t.Fatal("old registry entry missing after rollback")
		}
		if credentialExists(t, h, "fake", "work") {
			t.Fatal("final credential exists after commit failure")
		}
	})

	t.Run("credential commit failure removes new registry", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		deleter := &deleteStageLoginProvider{FakeProvider: fake}
		h := newFakeHarness(t, fake)
		deleter.root = h.root
		h.providers = testkit.NewProviderRegistry(deleter)
		res := h.run("login", "--provider", "fake", "--name", "work", "--json")
		_ = auth.Cleanup(h.root, h.configPath)
		if res.Exit == 0 {
			t.Fatalf("login succeeded, want credential commit failure")
		}
		assertErrorCode(t, res, errs.CodeInvalidConfig)
		assertNoPathLeak(t, res, h.root, h.configPath)
		if _, err := os.Stat(h.configPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("registry stat after failed first login = %v, want not exist", err)
		}
		if credentialExists(t, h, "fake", "work") {
			t.Fatal("final credential exists after failed first login")
		}
	})

	t.Run("repair commit failure keeps previous final cache", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		deleter := &deleteStageRepairProvider{FakeProvider: fake}
		h := newFakeHarness(t, fake)
		deleter.root = h.root
		deleter.backup = filepath.Join(filepath.Dir(h.root), filepath.Base(h.root)+"-auth-fake-backup")
		h.providers = testkit.NewProviderRegistry(deleter)
		account := workAccount()
		seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
		oldCache := &testkit.TokenCache{
			Version:      1,
			Provider:     account.Provider,
			AccountName:  account.Name,
			AccessToken:  "old-access-token-12345",
			RefreshToken: "old-refresh-token-12345",
			ExpiresAt:    time.Unix(1893456000, 0),
		}
		if err := testkit.SeedCredential(h.root, account, oldCache); err != nil {
			t.Fatalf("seed old cache: %v", err)
		}
		res := h.run("auth", "repair", "work", "--json")
		_ = os.Remove(filepath.Join(h.root, "auth", account.Provider))
		_ = os.Rename(deleter.backup, filepath.Join(h.root, "auth", account.Provider))
		_ = auth.Cleanup(h.root, h.configPath)
		if res.Exit == 0 {
			t.Fatalf("repair succeeded, want credential commit failure")
		}
		assertErrorCode(t, res, errs.CodeInvalidConfig)
		assertNoPathLeak(t, res, h.root, h.configPath)
		loaded, err := testkit.LoadCredential(h.root, account.Provider, account.Name)
		if err != nil {
			t.Fatalf("load final cache after failed repair: %v", err)
		}
		if loaded.AccessToken != oldCache.AccessToken || loaded.RefreshToken != oldCache.RefreshToken {
			t.Fatalf("final cache changed after failed repair: %#v", loaded)
		}
	})
}

func TestInteractiveCapabilitiesCloseAfterProviderReturn(t *testing.T) {
	retained := &retainedInteractiveProvider{FakeProvider: testkit.NewFakeProvider("fake")}
	secretReader := &countingSecretReader{}
	registry := testkit.NewProviderRegistry(retained)
	app := newApp(Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names()), SecretReader: secretReader})
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	res := testutil.RunWith(app, testutil.Options{
		LookupEnv: emptyLookupEnv,
		BrowserOpener: func(context.Context, string) error {
			return nil
		},
	}, "--config", configPath, "login", "--provider", "fake", "--name", "work", "--json")
	if res.Exit != 0 {
		t.Fatalf("login failed: %s", res.Stderr)
	}
	if err := retained.runtime.RegisterSecret("late-secret"); !errors.Is(err, provider.ErrProviderRuntimeClosed) {
		t.Fatalf("retained runtime RegisterSecret err = %v, want closed", err)
	}
	if _, _, err := retained.runtime.LookupSecretEnv("MAILDO_GMAIL_CLIENT_SECRET"); !errors.Is(err, provider.ErrProviderRuntimeClosed) {
		t.Fatalf("retained runtime LookupSecretEnv err = %v, want closed", err)
	}
	if retained.existing != nil {
		t.Fatal("new login received an existing credential source")
	}
	if err := retained.secrets.WithSecret(context.Background(), "Secret:", func([]byte) error { return nil }); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("retained secret source err = %v, want closed", err)
	}
	if calls := secretReader.calls.Load(); calls != 0 {
		t.Fatalf("retained secret source read count = %d, want 0", calls)
	}

	res = testutil.RunWith(app, testutil.Options{
		LookupEnv: emptyLookupEnv,
		BrowserOpener: func(context.Context, string) error {
			return nil
		},
	}, "--config", configPath, "auth", "login", "--name", "work", "--json")
	if res.Exit != 0 {
		t.Fatalf("reauth failed: %s", res.Stderr)
	}
	if _, err := retained.existing.WithBytes(context.Background(), nil); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("retained existing source err = %v, want closed", err)
	}
}

func TestCanceledRefreshDoesNotPersistAcceptedReplacement(t *testing.T) {
	base := testkit.NewFakeProvider("fake")
	base.RotateOnRefresh = true
	ctx, cancel := context.WithCancel(context.Background())
	retained := &cancelingRefreshProvider{FakeProvider: base, cancel: cancel}
	registry := testkit.NewProviderRegistry(retained)
	deps := Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())}
	h := handlers{deps: deps}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	account := maildoconfig.Account{Name: "work", Provider: "fake", Email: "work@example.test", ProviderAccountID: "fake-work", AuthRef: "fake/work"}
	reg := &maildoconfig.Registry{Version: 1, Accounts: []maildoconfig.Account{account}, ActiveAccount: "work"}
	if err := maildoconfig.Save(configPath, reg, registry.Names()); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	coord, err := auth.NewCoordinator(configPath, registry.Names())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	original := &testkit.TokenCache{
		Version:           1,
		Provider:          "fake",
		AccountName:       "work",
		TokenType:         "Bearer",
		AccessToken:       "old-access-token-12345",
		RefreshToken:      "old-refresh-token-12345",
		ExpiresAt:         time.Unix(1, 0),
		VerifiedEmail:     account.Email,
		ProviderAccountID: account.ProviderAccountID,
	}
	if err := testkit.SeedCredential(root, account, original); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	result, err := h.forceRefresh(ctx, &rungrad.Factory{}, coord, account, reg, errs.AuthStateRefresh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("forceRefresh = (%#v, %v), want context canceled", result, err)
	}
	loaded, err := testkit.LoadCredential(root, "fake", "work")
	if err != nil {
		t.Fatalf("load credential after canceled refresh: %v", err)
	}
	if loaded.AccessToken != original.AccessToken || loaded.RefreshToken != original.RefreshToken {
		t.Fatalf("credential changed after canceled refresh: %#v", loaded)
	}
	if err := retained.runtime.RegisterSecret("late-secret"); !errors.Is(err, provider.ErrProviderRuntimeClosed) {
		t.Fatalf("retained stored runtime err = %v, want closed", err)
	}
	if err := retained.credential.WithCurrent(context.Background(), func(provider.CredentialLease) error { return nil }); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("retained credential session err = %v, want closed", err)
	}
}

func TestClientPlusErrorIsDestroyed(t *testing.T) {
	base := testkit.NewFakeProvider("fake")
	tracker := &trackingAccountClient{Provider: "fake", Account: "work"}
	wrapped := &clientPlusErrorProvider{FakeProvider: base, client: tracker}
	registry := testkit.NewProviderRegistry(wrapped)
	deps := Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())}
	h := handlers{deps: deps}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	account := maildoconfig.Account{Name: "work", Provider: "fake", Email: "work@example.test", ProviderAccountID: "fake-work", AuthRef: "fake/work"}
	if err := maildoconfig.Save(configPath, &maildoconfig.Registry{Version: 1, Accounts: []maildoconfig.Account{account}, ActiveAccount: "work"}, registry.Names()); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	coord, err := auth.NewCoordinator(configPath, registry.Names())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := testkit.SeedCredential(root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	_, err = h.openAccountClient(context.Background(), &rungrad.Factory{}, coord, provider.AccountClientRequest{
		Binding:                   auth.CredentialBinding{Provider: "fake", Account: "work"},
		AuthRef:                   "fake/work",
		ExpectedEmail:             account.Email,
		ExpectedProviderAccountID: account.ProviderAccountID,
	})
	if err == nil {
		t.Fatal("openAccountClient succeeded, want provider error")
	}
	if !tracker.destroyed.Load() {
		t.Fatal("client returned with error was not destroyed")
	}
}

// blockRootLoginProvider makes the config root unwritable after provider login
// succeeds, forcing the registry-save rollback path.
type blockRootLoginProvider struct {
	*testkit.FakeProvider
	backup string
	root   string
}

func (p *blockRootLoginProvider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	result, err := p.FakeProvider.InteractiveAuth(ctx, ac)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	if p.backup != "" && p.root != "" {
		_ = os.RemoveAll(p.backup)
		if err := os.Rename(p.root, p.backup); err != nil {
			return provider.InteractiveAuthResult{}, err
		}
		if err := os.WriteFile(p.root, []byte("not a directory"), 0o600); err != nil {
			return provider.InteractiveAuthResult{}, err
		}
	}
	return result, nil
}

// deleteStageLoginProvider deletes the staged credential after login writes it,
// forcing commit failure after the registry save.
type deleteStageLoginProvider struct {
	*testkit.FakeProvider
	root string
}

func (p *deleteStageLoginProvider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	result, err := p.FakeProvider.InteractiveAuth(ctx, ac)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	if p.root != "" {
		if err := os.WriteFile(filepath.Join(p.root, "auth", ac.Provider), []byte("not a directory"), 0o600); err != nil {
			result.Credential.Destroy()
			return provider.InteractiveAuthResult{}, err
		}
	}
	return result, nil
}

// deleteStageRepairProvider deletes the staged credential after repair writes
// it, proving repair keeps the previous final cache on commit failure.
type deleteStageRepairProvider struct {
	*testkit.FakeProvider
	root   string
	backup string
}

func (p *deleteStageRepairProvider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	result, err := p.FakeProvider.InteractiveAuth(ctx, ac)
	if err != nil {
		return provider.InteractiveAuthResult{}, err
	}
	if p.root != "" {
		providerDir := filepath.Join(p.root, "auth", ac.Provider)
		if p.backup == "" {
			p.backup = providerDir + ".backup"
		}
		_ = os.RemoveAll(p.backup)
		if err := os.Rename(providerDir, p.backup); err != nil {
			result.Credential.Destroy()
			return provider.InteractiveAuthResult{}, err
		}
		if err := os.WriteFile(providerDir, []byte("not a directory"), 0o600); err != nil {
			result.Credential.Destroy()
			return provider.InteractiveAuthResult{}, err
		}
	}
	return result, nil
}

type retainedInteractiveProvider struct {
	*testkit.FakeProvider
	runtime  provider.InteractiveProviderRuntime
	existing provider.CredentialSource
	secrets  provider.InteractiveSecretSource
}

func (p *retainedInteractiveProvider) InteractiveAuth(ctx context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	p.runtime = ac.Runtime
	p.existing = ac.Existing
	p.secrets = ac.Secrets
	return p.FakeProvider.InteractiveAuth(ctx, ac)
}

type countingSecretReader struct {
	calls atomic.Int64
}

func (r *countingSecretReader) ReadHidden(context.Context, *rungrad.Factory, string) ([]byte, error) {
	r.calls.Add(1)
	return []byte("late-secret"), nil
}

type cancelingRefreshProvider struct {
	*testkit.FakeProvider
	cancel     context.CancelFunc
	runtime    provider.ProviderRuntime
	credential provider.CredentialSession
}

func (p *cancelingRefreshProvider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	p.runtime = ac.Runtime
	p.credential = ac.Credential
	result, err := p.FakeProvider.ForcedRefresh(ctx, ac)
	if p.cancel != nil {
		p.cancel()
	}
	return result, err
}

type clientPlusErrorProvider struct {
	*testkit.FakeProvider
	client provider.AccountClient
}

func (p *clientPlusErrorProvider) NewAccountClient(context.Context, provider.StoredAuthContext) (provider.AccountClient, error) {
	return p.client, fmt.Errorf("client construction failed after allocation")
}

type trackingAccountClient struct {
	Provider  string
	Account   string
	destroyed atomic.Bool
}

func (c *trackingAccountClient) ProviderName() string { return c.Provider }
func (c *trackingAccountClient) AccountName() string  { return c.Account }
func (c *trackingAccountClient) Destroy()             { c.destroyed.Store(true) }

// secretErrorProvider returns an error containing a registered secret so tests
// can assert RunGrad redaction at the error boundary.
type secretErrorProvider struct {
	name   string
	secret string
}

func (p *secretErrorProvider) Name() string { return p.name }

func (p *secretErrorProvider) Capabilities(context.Context) (provider.Capabilities, error) {
	caps := make(provider.Capabilities, len(provider.AllCapabilities()))
	for _, cap := range provider.AllCapabilities() {
		caps[cap] = provider.CapUnsupported
	}
	return caps, nil
}

func (p *secretErrorProvider) InteractiveAuth(_ context.Context, ac provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	_ = ac.Runtime.RegisterSecret(p.secret)
	return provider.InteractiveAuthResult{}, fmt.Errorf("provider failed with %s", p.secret)
}

func (p *secretErrorProvider) LocalHealth(context.Context, provider.StoredAuthContext) (provider.LocalStatus, error) {
	return provider.LocalRepairRequired, nil
}

func (p *secretErrorProvider) ForcedRefresh(context.Context, provider.StoredAuthContext) (provider.RefreshResult, error) {
	return provider.RefreshResult{}, fmt.Errorf("not implemented")
}

func (p *secretErrorProvider) Logout(context.Context, provider.StoredAuthContext) error {
	return nil
}

func (p *secretErrorProvider) NewAccountClient(context.Context, provider.StoredAuthContext) (provider.AccountClient, error) {
	return nil, fmt.Errorf("not implemented")
}

// failOnRead proves machine-mode commands do not attempt interactive prompts.
type failOnRead struct {
	reads atomic.Int64
}

func (r *failOnRead) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, fmt.Errorf("unexpected read")
}

func workAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:              "work",
		Provider:          "fake",
		Email:             "fake@example.test",
		DisplayName:       "Fake fake",
		ProviderAccountID: "fake-fake",
		AuthRef:           "fake/work",
		DefaultFrom:       "fake@example.test",
	}
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func decodeOK(t *testing.T, res testutil.Result, v any) {
	t.Helper()
	if res.Exit != 0 {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	if v != nil {
		if err := res.JSON(v); err != nil {
			t.Fatalf("decode stdout: %v\n%s", err, res.Stdout)
		}
	}
}

func decodePreview(t *testing.T, res testutil.Result) map[string]any {
	t.Helper()
	var preview map[string]any
	decodeOK(t, res, &preview)
	return preview
}

func assertErrorCode(t *testing.T, res testutil.Result, code string) {
	t.Helper()
	assertErrorCodeExit(t, res, code, 1)
}

func assertErrorCodeExit(t *testing.T, res testutil.Result, code string, exit int) {
	t.Helper()
	if res.Exit != exit {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", res.Exit, exit, res.Stdout, res.Stderr)
	}
	// A browser sign-in prints its address on stderr before any error, even
	// with --json, so the error is the JSON object that follows.
	errJSON := res.Stderr
	if at := strings.Index(errJSON, "\n{\n"); at >= 0 && !strings.HasPrefix(errJSON, "{") {
		errJSON = errJSON[at+1:]
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(errJSON), &body); err != nil {
		t.Fatalf("decode stderr: %v\n%s", err, res.Stderr)
	}
	if body["code"] != code {
		t.Fatalf("code = %#v, want %q\nstderr=%s", body["code"], code, res.Stderr)
	}
	if body["exit_code"] != float64(exit) {
		t.Fatalf("exit_code = %#v, want %d", body["exit_code"], exit)
	}
}

func assertNoCode(t *testing.T, res testutil.Result) {
	t.Helper()
	if res.Stderr == "" || !strings.HasPrefix(strings.TrimSpace(res.Stderr), "{") {
		return
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("decode stderr: %v\n%s", err, res.Stderr)
	}
	if _, ok := body["code"]; ok {
		t.Fatalf("unexpected product code in stderr: %s", res.Stderr)
	}
}

func assertNoPathLeak(t *testing.T, res testutil.Result, paths ...string) {
	t.Helper()
	output := res.Stdout + res.Stderr
	// A printed command repeats the --config the user typed; that is not
	// a leak.
	for _, path := range paths {
		if path != "" {
			output = strings.ReplaceAll(output, "--config "+path, "--config <config>")
			output = strings.ReplaceAll(output, "--config '"+path+"'", "--config <config>")
		}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if strings.Contains(output, path) {
			t.Fatalf("output leaked path %q\nstdout=%s\nstderr=%s", path, res.Stdout, res.Stderr)
		}
	}
}

func seedRegistry(t *testing.T, path, active string, accounts []maildoconfig.Account) {
	t.Helper()
	reg := &maildoconfig.Registry{Version: 1, ActiveAccount: active, Accounts: accounts}
	if err := maildoconfig.Save(path, reg, recognizedProvidersForAccounts(accounts)); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
}

func seedAccount(t *testing.T, h *cliHarness, account maildoconfig.Account, withCache bool) {
	t.Helper()
	reg, err := maildoconfig.Load(h.configPath, []string{"fake", "outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if _, ok := reg.Find(account.Name); !ok {
		if err := reg.Add(account); err != nil {
			t.Fatalf("add account: %v", err)
		}
	}
	if err := maildoconfig.Save(h.configPath, reg, h.providers.Names()); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if withCache {
		if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{Version: 1, Provider: account.Provider, AccountName: account.Name, AccessToken: "seed-access-token-" + account.Name, RefreshToken: "seed-refresh-token-" + account.Name, ExpiresAt: time.Unix(1893456000, 0)}); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
	}
}

func recognizedProvidersForAccounts(accounts []maildoconfig.Account) []string {
	seen := map[string]bool{"outlook": true}
	providers := []string{"outlook"}
	for _, account := range accounts {
		if account.Provider == "" || seen[account.Provider] {
			continue
		}
		seen[account.Provider] = true
		providers = append(providers, account.Provider)
	}
	return providers
}
