package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/manifest"
	"github.com/vincentsch/rungrad/testutil"
)

const handlerOwnedAuthChildEnv = "MAILDO_HANDLER_OWNED_AUTH_CHILD"
const handlerOwnedAuthChildCaseEnv = "MAILDO_HANDLER_OWNED_AUTH_CASE"
const handlerOwnedAuthHelperFailureExit = 86

type countingRunGradResolver struct {
	calls int
	token string
}

func (r *countingRunGradResolver) ResolveCredential(*rungrad.AuthContext) (rungrad.Credential, error) {
	r.calls++
	return rungrad.Credential{Token: r.token, Source: "framework"}, nil
}

type literalEchoModule struct {
	value string
}

func (m literalEchoModule) Groups() []rungrad.Group { return nil }

func (m literalEchoModule) Commands() []*rungrad.Command {
	return []*rungrad.Command{{
		Use:         "redaction-probe",
		Short:       "Render a literal value",
		OutputModes: literalEchoModes(),
		Run: func(f *rungrad.Factory, _ *cobra.Command, _ []string) error {
			return f.WriteResult(map[string]string{"value": m.value}, func(w io.Writer) {
				fmt.Fprintln(w, m.value)
			})
		},
	}}
}

func (m literalEchoModule) Catalog() []rungrad.CommandSpec {
	return []rungrad.CommandSpec{{
		Path:        "redaction-probe",
		Summary:     "Render a literal value",
		OutputModes: literalEchoModes(),
		Examples:    []string{"maildo redaction-probe"},
	}}
}

func literalEchoModes() []string {
	return []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
}

func (h *cliHarness) scopeApp() *rungrad.App {
	deps := Deps{Providers: h.providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(h.providers)...), SetupParsers: defaultSetupParsers(h.providers.Names())}
	return newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
}

func (h *cliHarness) runScope(args ...string) testutil.Result {
	return h.runScopeWith(testutil.Options{}, args...)
}

func (h *cliHarness) runScopeWith(opts testutil.Options, args ...string) testutil.Result {
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
	return testutil.RunWith(h.scopeApp(), opts, fullArgs...)
}

func TestScopeAccountSelection(t *testing.T) {
	t.Run("active default", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-one", "--json"), &sel)
		assertSelection(t, sel, "single", "work")
	})

	t.Run("explicit override", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-one", "--account", "personal", "--json"), &sel)
		assertSelection(t, sel, "single", "personal")
	})

	t.Run("read many", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-many", "--account", "work", "--account", "personal", "--json"), &sel)
		assertSelection(t, sel, "read_many", "work", "personal")
	})

	t.Run("read many one explicit", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-many", "--account", "work", "--json"), &sel)
		assertSelection(t, sel, "single", "work")
	})

	t.Run("read many active fallback", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-many", "--json"), &sel)
		assertSelection(t, sel, "single", "work")
	})

	t.Run("all accounts", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "read-all", "--all-accounts", "--json"), &sel)
		assertSelection(t, sel, "all", "work", "personal")
	})

	t.Run("unknown account", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "read-one", "--account", "missing", "--json"), errs.CodeUnknownAccount)
	})

	t.Run("repeated account", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "read-many", "--account", "work", "--account", "work", "--json"), errs.CodeInvalidAccountScope)
	})

	t.Run("missing active", func(t *testing.T) {
		h := seededScopeHarness(t, "")
		res := h.runScope("scopekit", "read-one", "--json")
		assertErrorCode(t, res, errs.CodeMissingActiveAccount)
		body := errorJSON(t, res)
		msg := body["error"].(string)
		for _, want := range []string{"maildo login", "maildo auth switch <name>", "--account <name>"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("missing-active message %q does not contain %q", msg, want)
			}
		}
	})

	t.Run("no accounts", func(t *testing.T) {
		h := newFakeHarness(t, testkit.NewFakeProvider("fake"))
		assertErrorCode(t, h.runScope("scopekit", "read-one", "--json"), errs.CodeMissingActiveAccount)
		assertErrorCode(t, h.runScope("scopekit", "read-all", "--all-accounts", "--json"), errs.CodeMissingActiveAccount)
	})

	t.Run("mutation single account", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "mutate-one", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidAccountScope)
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "mutate-one", "--account", "work", "--json"), &sel)
		assertSelection(t, sel, "single", "work")
	})

	t.Run("framework mutual exclusion", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		res := h.runScope("scopekit", "read-all", "--account", "work", "--all-accounts", "--json")
		assertFrameworkUsage(t, res)
	})

	t.Run("unregistered all accounts flag", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		res := h.runScope("scopekit", "read-many", "--account", "work", "--all-accounts", "--json")
		assertFrameworkUsage(t, res)
	})
}

func TestScopeBulkQualified(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var sel provider.AccountSelection
		decodeOK(t, h.runScope("scopekit", "bulk", "--target", "work:AAA", "--target", "personal:BBB", "--target", "work:CCC", "--json"), &sel)
		assertSelection(t, sel, "bulk_qualified", "work", "personal")
	})

	t.Run("empty", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "bulk", "--json"), errs.CodeInvalidAccountScope)
	})

	t.Run("unqualified", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "bulk", "--target", "plain-id", "--json"), errs.CodeInvalidAccountScope)
	})

	t.Run("unknown account", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "bulk", "--target", "missing:AAA", "--json"), errs.CodeUnknownAccount)
	})

	t.Run("conflicting account flag", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		assertErrorCode(t, h.runScope("scopekit", "bulk", "--account", "work", "--target", "work:AAA", "--json"), errs.CodeInvalidAccountScope)
	})
}

func TestScopeOutputModesAndMeta(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "read-one", args: []string{"scopekit", "read-one"}, want: []string{"work"}},
		{name: "read-many", args: []string{"scopekit", "read-many", "--account", "work", "--account", "personal"}, want: []string{"work", "personal"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededScopeHarness(t, "work")
			human := h.runScope(tt.args...)
			decodeOK(t, human, nil)
			if !strings.Contains(human.Stdout, tt.want[0]) || !strings.Contains(human.Stdout, "fake") {
				t.Fatalf("human stdout = %q", human.Stdout)
			}

			var sel provider.AccountSelection
			decodeOK(t, h.runScope(argsWith(tt.args, "--json")...), &sel)
			wantScope := "single"
			if len(tt.want) > 1 {
				wantScope = "read_many"
			}
			assertSelection(t, sel, wantScope, tt.want...)
			assertSelectionProviders(t, sel, "fake")

			jq := h.runScope(argsWith(tt.args, "--json", "--jq", ".scope")...)
			if strings.TrimSpace(jq.Stdout) != wantScope {
				t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
			}
			tmpl := h.runScope(argsWith(tt.args, "--template", "{{.scope}}")...)
			if strings.TrimSpace(tmpl.Stdout) != wantScope {
				t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
			}

			var envelope struct {
				Data provider.AccountSelection `json:"data"`
				Meta struct {
					Extra map[string]any `json:"extra"`
				} `json:"meta"`
			}
			decodeOK(t, h.runScope(argsWith(tt.args, "--json", "--include-meta")...), &envelope)
			assertSelection(t, envelope.Data, wantScope, tt.want...)
			assertSelectionProviders(t, envelope.Data, "fake")
			if envelope.Meta.Extra["scope"] != wantScope {
				t.Fatalf("meta scope = %#v, want %q", envelope.Meta.Extra["scope"], wantScope)
			}
			if wantPath := "maildo " + strings.Join(tt.args[:2], " "); envelope.Meta.Extra["command_path"] != wantPath {
				t.Fatalf("meta command_path = %#v, want %q", envelope.Meta.Extra["command_path"], wantPath)
			}
			if got := metaStringSlice(t, envelope.Meta.Extra["accounts"]); !sameStrings(got, tt.want) {
				t.Fatalf("meta accounts = %#v, want %#v", got, tt.want)
			}
			wantProviders := make([]string, len(tt.want))
			for i := range wantProviders {
				wantProviders[i] = "fake"
			}
			if got := metaStringSlice(t, envelope.Meta.Extra["providers"]); !sameStrings(got, wantProviders) {
				t.Fatalf("meta providers = %#v, want %#v", got, wantProviders)
			}
		})
	}

	h := seededScopeHarness(t, "work")
	var raw map[string]any
	decodeOK(t, h.runScope("scopekit", "read-many", "--account", "work", "--account", "personal", "--json"), &raw)
	accounts := raw["accounts"].([]any)
	work := accounts[0].(map[string]any)
	personal := accounts[1].(map[string]any)
	if work["email"] == "" {
		t.Fatalf("known email missing: %#v", work)
	}
	if work["provider"] != "fake" || personal["provider"] != "fake" {
		t.Fatalf("provider missing from raw JSON: work=%#v personal=%#v", work, personal)
	}
	if _, ok := work["auth_ref"]; ok {
		t.Fatalf("auth_ref leaked in raw JSON: %#v", work)
	}
	if _, ok := personal["auth_ref"]; ok {
		t.Fatalf("auth_ref leaked in raw JSON: %#v", personal)
	}
	if _, ok := personal["email"]; ok {
		t.Fatalf("empty email was not omitted: %#v", personal)
	}
}

func TestScopeCapabilities(t *testing.T) {
	t.Run("fake report covers canonical capabilities", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		var report provider.CapabilityReport
		decodeOK(t, h.runScope("scopekit", "capabilities", "--account", "work", "--json"), &report)
		if len(report.Capabilities) != len(provider.AllCapabilities()) {
			t.Fatalf("capabilities = %d, want %d", len(report.Capabilities), len(provider.AllCapabilities()))
		}
		if capState(report, provider.CapMessagesList) != string(provider.CapImplemented) ||
			capState(report, provider.CapMessagesRead) != string(provider.CapImplemented) ||
			capState(report, provider.CapMessagesSearch) != string(provider.CapSupportedNotImpl) ||
			capState(report, provider.CapRulesRead) != string(provider.CapUnsupported) ||
			capState(report, provider.CapRulesMutate) != string(provider.CapUnsupported) ||
			capState(report, provider.CapFoldersRead) != string(provider.CapUnsupported) {
			t.Fatalf("unexpected fake capability report: %#v", report.Capabilities)
		}
	})

	t.Run("outlook reports implemented message and folder reads and not implemented for the rest", func(t *testing.T) {
		h := newFakeHarness(t, testkit.NewFakeProvider("fake"))
		outlookAccount := maildoconfig.Account{Name: "work-ms", Provider: "outlook", Email: "work-ms@example.test", AuthRef: "outlook/work-ms"}
		seedRegistry(t, h.configPath, "work-ms", []maildoconfig.Account{outlookAccount})
		var report provider.CapabilityReport
		decodeOK(t, h.runScope("scopekit", "capabilities", "--account", "work-ms", "--json"), &report)
		if len(report.Capabilities) != len(provider.AllCapabilities()) {
			t.Fatalf("capabilities = %d, want %d", len(report.Capabilities), len(provider.AllCapabilities()))
		}
		implemented := map[string]bool{
			string(provider.CapFoldersRead):         true,
			string(provider.CapFoldersMutate):       true,
			string(provider.CapMessagesSearch):      true,
			string(provider.CapMessagesList):        true,
			string(provider.CapMessagesRead):        true,
			string(provider.CapThreadsRead):         true,
			string(provider.CapDraftCreate):         true,
			string(provider.CapDraftRead):           true,
			string(provider.CapDraftUpdate):         true,
			string(provider.CapDraftSend):           true,
			string(provider.CapSendDirect):          true,
			string(provider.CapReplyDraftCreate):    true,
			string(provider.CapForwardDraftCreate):  true,
			string(provider.CapAttachmentsRead):     true,
			string(provider.CapAttachmentsMutate):   true,
			string(provider.CapMessagesMove):        true,
			string(provider.CapMessagesCopy):        true,
			string(provider.CapMessagesArchive):     true,
			string(provider.CapMessagesDelete):      true,
			string(provider.CapMessagesRestore):     true,
			string(provider.CapMessagesJunk):        true,
			string(provider.CapMessagesMarkRead):    true,
			string(provider.CapMessagesFlag):        true,
			string(provider.CapCategoriesRead):      true,
			string(provider.CapCategoriesMutate):    true,
			string(provider.CapRulesRead):           true,
			string(provider.CapRulesMutate):         true,
			string(provider.CapArchivePolicyReport): true,
		}
		for _, c := range report.Capabilities {
			if implemented[c.Name] {
				if c.State != string(provider.CapImplemented) {
					t.Fatalf("outlook capability %s state = %s", c.Name, c.State)
				}
				continue
			}
			if c.State != string(provider.CapSupportedNotImpl) {
				t.Fatalf("outlook capability %s state = %s", c.Name, c.State)
			}
		}
	})

	t.Run("capability check mapping", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		secrets := []string{"cap-check-access-token-12345", "cap-check-refresh-token-12345"}
		if err := testkit.SeedCredential(h.root, scopeWorkAccount(), &testkit.TokenCache{
			Version:      1,
			Provider:     "fake",
			AccountName:  "work",
			AccessToken:  secrets[0],
			RefreshToken: secrets[1],
			ExpiresAt:    time.Unix(1893456000, 0),
		}); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		var ok capCheckResult
		decodeOK(t, h.runScope("scopekit", "cap-check", "--account", "work", "--capability", "messages_list", "--json"), &ok)
		if ok.State != string(provider.CapImplemented) {
			t.Fatalf("cap-check success = %#v", ok)
		}
		for _, tt := range []struct {
			args []string
			code string
		}{
			{args: []string{"scopekit", "cap-check", "--account", "work", "--capability", "messages_search", "--json"}, code: errs.CodeCapabilityNotImplemented},
			{args: []string{"scopekit", "cap-check", "--account", "work", "--capability", "rules_read", "--json"}, code: errs.CodeUnsupportedCapability},
			{args: []string{"scopekit", "cap-check", "--account", "work", "--capability", "folders_read", "--json"}, code: errs.CodeUnsupportedCapability},
			{args: []string{"scopekit", "cap-check", "--account", "work", "--capability", "rules_read", "--json", "--jq", ".state"}, code: errs.CodeUnsupportedCapability},
			{args: []string{"scopekit", "cap-check", "--account", "work", "--capability", "rules_read", "--template", "{{.state}}"}, code: errs.CodeUnsupportedCapability},
		} {
			res := h.runScope(tt.args...)
			assertErrorCode(t, res, tt.code)
			testkit.AssertNoSecretLeak(t, res, secrets...)
		}
	})
}

func TestScopeOpenAccountClient(t *testing.T) {
	t.Run("healthy cache", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		account := scopeWorkAccount()
		access := "scope-open-access-token-12345"
		refresh := "scope-open-refresh-token-12345"
		if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{
			Version:           1,
			Provider:          account.Provider,
			AccountName:       account.Name,
			AccessToken:       access,
			RefreshToken:      refresh,
			ExpiresAt:         time.Unix(1893456000, 0),
			VerifiedEmail:     account.Email,
			DisplayName:       account.DisplayName,
			ProviderAccountID: account.ProviderAccountID,
		}); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		var opened openResult
		res := h.runScope("scopekit", "open", "--account", "work", "--json")
		decodeOK(t, res, &opened)
		if !opened.Ready || opened.Account != "work" || opened.Provider != "fake" {
			t.Fatalf("open result = %#v", opened)
		}
		if h.browserCalls.Load() != 0 {
			t.Fatalf("open used browser %d times", h.browserCalls.Load())
		}
		testkit.AssertNoSecretLeak(t, res, access, refresh)
	})

	t.Run("broken cache", func(t *testing.T) {
		h := seededScopeHarness(t, "work")
		account := scopeWorkAccount()
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		path := filepath.Join(h.root, "auth", "fake", "work.json")
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatalf("write corrupt cache: %v", err)
		}
		assertErrorCodeExit(t, h.runScope("scopekit", "open", "--account", "work", "--json"), errs.CodeAuthRepairRequired, 3)
		if h.browserCalls.Load() != 0 {
			t.Fatalf("broken open used browser %d times", h.browserCalls.Load())
		}
	})
}

func TestScopeHandlerOwnedAuthResolution(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{scopeWorkAccount(), scopePersonalAccount()})
	selectedAccess := "handler-selected-access-token-12345"
	selectedRefresh := "handler-selected-refresh-token-12345"
	if err := testkit.SeedCredential(h.root, scopePersonalAccount(), &testkit.TokenCache{
		Version:      1,
		Provider:     "fake",
		AccountName:  "personal",
		AccessToken:  selectedAccess,
		RefreshToken: selectedRefresh,
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed selected credential: %v", err)
	}
	resolver := &countingRunGradResolver{token: "framework-token-should-not-load-12345"}
	deps := Deps{Providers: h.providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(h.providers)...), SetupParsers: defaultSetupParsers(h.providers.Names()), RunGradAuth: resolver}
	app := newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
	if err := app.ValidateCatalog(); err != nil {
		t.Fatalf("ValidateCatalog() = %v", err)
	}
	manifest := testutil.CaptureManifest(t, app)
	if open := findManifestCommand(t, manifest, "scopekit open"); !open.RequiresAuth {
		t.Fatalf("scopekit open requires_auth = false: %#v", open)
	}
	catalogAuth := false
	for _, spec := range app.Catalog() {
		if spec.Path == "scopekit open" {
			catalogAuth = spec.RequiresAuth
			break
		}
	}
	if !catalogAuth {
		t.Fatal("scopekit open catalog RequiresAuth = false")
	}

	var opened openResult
	res := runExistingScopeApp(t, app, h.configPath, testutil.Options{
		LookupEnv: emptyLookupEnv,
		BrowserOpener: func(context.Context, string) error {
			h.browserCalls.Add(1)
			return nil
		},
	}, "scopekit", "open", "--account", "personal", "--json")
	decodeOK(t, res, &opened)
	if opened.Account != "personal" || opened.Provider != "fake" || !opened.Ready {
		t.Fatalf("open result = %#v", opened)
	}
	if resolver.calls != 0 {
		t.Fatalf("RunGrad resolver calls = %d, want 0", resolver.calls)
	}
	if fake.AccountClientCalls.Load() != 1 {
		t.Fatalf("account client calls = %d, want 1", fake.AccountClientCalls.Load())
	}
	assertAccounts(t, fake.AccountClientAccounts(), "personal")
	if h.browserCalls.Load() != 0 {
		t.Fatalf("browser calls = %d, want 0", h.browserCalls.Load())
	}
	if app.Factory().Token != "" || app.Factory().Credential() != (rungrad.Credential{}) {
		t.Fatalf("framework credential state = token %q credential %+v", app.Factory().Token, app.Factory().Credential())
	}
	testkit.AssertNoSecretLeak(t, res, selectedAccess, selectedRefresh, resolver.token)
}

func TestScopeHandlerOwnedAuthSelectedLoggedOutAccount(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{scopeWorkAccount(), scopePersonalAccount()})
	activeAccess := "handler-active-access-token-12345"
	activeRefresh := "handler-active-refresh-token-12345"
	if err := testkit.SeedCredential(h.root, scopeWorkAccount(), &testkit.TokenCache{
		Version:      1,
		Provider:     "fake",
		AccountName:  "work",
		AccessToken:  activeAccess,
		RefreshToken: activeRefresh,
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed active credential: %v", err)
	}
	activePath := scopeCredentialPath(h.root, scopeWorkAccount())
	beforeActive := readFileBytes(t, activePath)
	assertMissingPath(t, scopeCredentialPath(h.root, scopePersonalAccount()))

	resolver := &countingRunGradResolver{token: "framework-token-should-not-load-67890"}
	deps := Deps{Providers: h.providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(h.providers)...), SetupParsers: defaultSetupParsers(h.providers.Names()), RunGradAuth: resolver}
	app := newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
	stdin := &failOnRead{}
	res := runExistingScopeApp(t, app, h.configPath, testutil.Options{
		Stdin:       stdin,
		TerminalSet: true,
		Terminal:    true,
		LookupEnv:   emptyLookupEnv,
		BrowserOpener: func(context.Context, string) error {
			h.browserCalls.Add(1)
			return nil
		},
	}, "scopekit", "open", "--account", "personal", "--json")
	assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
	if res.Stdout != "" {
		t.Fatalf("stdout = %q, want empty", res.Stdout)
	}
	body := errorJSON(t, res)
	if !strings.Contains(fmt.Sprint(body["error"]), "maildo auth repair personal") {
		t.Fatalf("error does not name selected repair command: %#v", body)
	}
	if strings.Contains(fmt.Sprint(body["error"]), "maildo auth repair work") {
		t.Fatalf("error used active account repair command: %#v", body)
	}
	if resolver.calls != 0 {
		t.Fatalf("RunGrad resolver calls = %d, want 0", resolver.calls)
	}
	if h.browserCalls.Load() != 0 {
		t.Fatalf("browser calls = %d, want 0", h.browserCalls.Load())
	}
	if stdin.reads.Load() != 0 {
		t.Fatalf("stdin reads = %d, want 0", stdin.reads.Load())
	}
	if fake.AccountClientCalls.Load() != 0 {
		t.Fatalf("account client calls = %d, want 0", fake.AccountClientCalls.Load())
	}
	assertAccounts(t, fake.AccountClientAccounts())
	if app.Factory().Token != "" || app.Factory().Credential() != (rungrad.Credential{}) {
		t.Fatalf("framework credential state = token %q credential %+v", app.Factory().Token, app.Factory().Credential())
	}
	afterActive := readFileBytes(t, activePath)
	if !bytes.Equal(beforeActive, afterActive) {
		t.Fatal("active credential cache changed during selected logged-out failure")
	}
	assertMissingPath(t, scopeCredentialPath(h.root, scopePersonalAccount()))
	testkit.AssertNoSecretLeak(t, res, activeAccess, activeRefresh, resolver.token)
}

func TestScopeHandlerOwnedAuthReuseAcrossSelectedCacheState(t *testing.T) {
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{scopeWorkAccount(), scopePersonalAccount()})
	workAccess := "reuse-work-access-token-12345"
	workRefresh := "reuse-work-refresh-token-12345"
	personalAccess := "reuse-personal-access-token-12345"
	personalRefresh := "reuse-personal-refresh-token-12345"
	seedScopeCredential(t, h.root, scopeWorkAccount(), workAccess, workRefresh)
	seedScopeCredential(t, h.root, scopePersonalAccount(), personalAccess, personalRefresh)

	resolver := &countingRunGradResolver{token: "framework-token-should-not-load-reuse"}
	deps := Deps{Providers: h.providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(h.providers)...), SetupParsers: defaultSetupParsers(h.providers.Names()), RunGradAuth: resolver}
	app := newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
	browserCalls := 0
	run := func(args ...string) testutil.Result {
		return runExistingScopeApp(t, app, h.configPath, testutil.Options{
			LookupEnv: emptyLookupEnv,
			BrowserOpener: func(context.Context, string) error {
				browserCalls++
				return nil
			},
		}, args...)
	}

	start := len(fake.AccountClientAccounts())
	jq := run("scopekit", "open", "--account", "personal", "--jq", ".account")
	if jq.Exit != 0 || strings.TrimSpace(jq.Stdout) != "personal" || jq.Stderr != "" {
		t.Fatalf("jq selected run = %#v", jq)
	}
	assertAccounts(t, accountLogDelta(fake, start), "personal")
	assertNoFrameworkAuthState(t, app)

	if err := os.Remove(scopeCredentialPath(h.root, scopePersonalAccount())); err != nil {
		t.Fatalf("remove selected credential: %v", err)
	}
	start = len(fake.AccountClientAccounts())
	loggedOut := run("scopekit", "open", "--account", "personal", "--json")
	assertErrorCodeExit(t, loggedOut, errs.CodeAuthRepairRequired, 3)
	assertAccounts(t, accountLogDelta(fake, start))
	assertNoFrameworkAuthState(t, app)

	seedScopeCredential(t, h.root, scopePersonalAccount(), personalAccess, personalRefresh)
	start = len(fake.AccountClientAccounts())
	var reopened openResult
	reopenedResult := run("scopekit", "open", "--account", "personal", "--json")
	decodeOK(t, reopenedResult, &reopened)
	if reopened.Account != "personal" {
		t.Fatalf("reopened account = %#v", reopened)
	}
	assertAccounts(t, accountLogDelta(fake, start), "personal")
	assertNoFrameworkAuthState(t, app)

	start = len(fake.AccountClientAccounts())
	var active openResult
	activeResult := run("scopekit", "open", "--json")
	decodeOK(t, activeResult, &active)
	if active.Account != "work" {
		t.Fatalf("active fallback account = %#v", active)
	}
	assertAccounts(t, accountLogDelta(fake, start), "work")
	assertNoFrameworkAuthState(t, app)

	if resolver.calls != 0 {
		t.Fatalf("RunGrad resolver calls = %d, want 0", resolver.calls)
	}
	if browserCalls != 0 {
		t.Fatalf("browser calls = %d, want 0", browserCalls)
	}
	for _, res := range []testutil.Result{jq, loggedOut, reopenedResult, activeResult} {
		testkit.AssertNoSecretLeak(t, res, workAccess, workRefresh, personalAccess, personalRefresh, resolver.token)
	}
}

func TestHandlerOwnedAuthSelectedAccountSubprocess(t *testing.T) {
	if os.Getenv(handlerOwnedAuthChildEnv) == "1" {
		os.Exit(runHandlerOwnedAuthChild(os.Getenv(handlerOwnedAuthChildCaseEnv)))
	}
	for _, tt := range []struct {
		name     string
		wantExit int
	}{
		{name: "success", wantExit: 0},
		{name: "logged_out", wantExit: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHandlerOwnedAuthSelectedAccountSubprocess$", "--", tt.name)
			cmd.Env = append(os.Environ(),
				handlerOwnedAuthChildEnv+"=1",
				handlerOwnedAuthChildCaseEnv+"="+tt.name,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			exit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("child run: %v", err)
				}
				exit = exitErr.ExitCode()
			}
			if exit == handlerOwnedAuthHelperFailureExit {
				t.Fatalf("child helper failed\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
			}
			if exit != tt.wantExit {
				t.Fatalf("child exit = %d, want %d\nstdout=%s\nstderr=%s", exit, tt.wantExit, stdout.String(), stderr.String())
			}
			out, errText := stdout.String(), stderr.String()
			switch tt.name {
			case "success":
				if errText != "" {
					t.Fatalf("success stderr = %q", errText)
				}
				var opened openResult
				if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
					t.Fatalf("success stdout JSON: %v\n%s", err, out)
				}
				if opened.Account != "personal" || opened.Provider != "fake" || !opened.Ready {
					t.Fatalf("success opened = %#v", opened)
				}
			case "logged_out":
				if out != "" {
					t.Fatalf("logged-out stdout = %q", out)
				}
				var body map[string]any
				if err := json.Unmarshal(stderr.Bytes(), &body); err != nil {
					t.Fatalf("logged-out stderr JSON: %v\n%s", err, errText)
				}
				if body["code"] != errs.CodeAuthRepairRequired || body["exit_code"] != float64(3) ||
					!strings.Contains(fmt.Sprint(body["error"]), "maildo auth repair personal") {
					t.Fatalf("logged-out body = %#v", body)
				}
			}
			for _, secret := range []string{
				"child-selected-access-token-12345",
				"child-selected-refresh-token-12345",
				"child-active-access-token-12345",
				"child-active-refresh-token-12345",
				"child-framework-token-12345",
			} {
				if strings.Contains(out, secret) || strings.Contains(errText, secret) {
					t.Fatalf("child leaked secret %q\nstdout=%s\nstderr=%s", secret, out, errText)
				}
			}
		})
	}
}

func TestScopeSecretSweep(t *testing.T) {
	h := seededScopeHarness(t, "work")
	account := scopeWorkAccount()
	secrets := []string{"sweep-access-token-12345", "sweep-refresh-token-12345"}
	if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	for _, base := range [][]string{
		{"scopekit", "read-one", "--account", "work", "--json", "--include-meta"},
		{"scopekit", "read-many", "--account", "work", "--account", "personal", "--json", "--include-meta"},
		{"scopekit", "capabilities", "--account", "work", "--json", "--include-meta"},
		{"scopekit", "open", "--account", "work", "--json", "--include-meta"},
	} {
		res := h.runScope(base...)
		decodeOK(t, res, nil)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
	}
	for _, base := range [][]string{
		{"scopekit", "read-one", "--account", "work"},
		{"scopekit", "read-many", "--account", "work", "--account", "personal"},
		{"scopekit", "capabilities", "--account", "work"},
		{"scopekit", "open", "--account", "work"},
	} {
		jsonRes := h.runScope(argsWith(base, "--json")...)
		decodeOK(t, jsonRes, nil)
		testkit.AssertNoSecretLeak(t, jsonRes, secrets...)
		assertNoPathLeak(t, jsonRes, h.root, h.configPath)

		humanRes := h.runScope(base...)
		decodeOK(t, humanRes, nil)
		testkit.AssertNoSecretLeak(t, humanRes, secrets...)
		assertNoPathLeak(t, humanRes, h.root, h.configPath)
	}
}

func TestScopeManifestHelpAndConsistency(t *testing.T) {
	factory := func() *rungrad.App {
		fake := testkit.NewFakeProvider("fake")
		providers := testkit.NewProviderRegistry(fake)
		deps := Deps{Providers: providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(providers)...), SetupParsers: defaultSetupParsers(providers.Names())}
		return newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
	}
	testutil.AssertConsistent(t, factory)

	m := testutil.CaptureManifest(t, factory())
	assertFlagSet(t, findManifestCommand(t, m, "scopekit read-one"), map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	assertFlagSet(t, findManifestCommand(t, m, "scopekit read-many"), map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	assertFlagSet(t, findManifestCommand(t, m, "scopekit mutate-one"), map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	assertFlagSet(t, findManifestCommand(t, m, "scopekit capabilities"), map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	scopeOpen := findManifestCommand(t, m, "scopekit open")
	assertFlagSet(t, scopeOpen, map[string]string{"account": "stringArray"}, []string{"all-accounts"})
	if !scopeOpen.RequiresAuth {
		t.Fatalf("scopekit open requires_auth = false")
	}
	for _, path := range []string{"scopekit read-one", "scopekit read-many", "scopekit read-all", "scopekit mutate-one", "scopekit bulk", "scopekit capabilities", "scopekit cap-check"} {
		if cmd := findManifestCommand(t, m, path); cmd.RequiresAuth {
			t.Fatalf("%s unexpectedly requires auth", path)
		}
	}
	assertFlagSet(t, findManifestCommand(t, m, "scopekit read-all"), map[string]string{"account": "stringArray", "all-accounts": "bool"}, nil)
	assertFlagSet(t, findManifestCommand(t, m, "scopekit bulk"), map[string]string{"account": "stringArray", "target": "stringArray"}, []string{"all-accounts"})
	assertFlagSet(t, findManifestCommand(t, m, "scopekit cap-check"), map[string]string{"account": "stringArray", "capability": "string"}, []string{"all-accounts"})

	help := testutil.Run(factory(), "scopekit", "read-all", "--help")
	if help.Exit != 0 || !strings.Contains(help.Stdout, "--account") || !strings.Contains(help.Stdout, "--all-accounts") {
		t.Fatalf("read-all help exit=%d stdout=%s stderr=%s", help.Exit, help.Stdout, help.Stderr)
	}
}

func seededScopeHarness(t *testing.T, active string) *cliHarness {
	t.Helper()
	fake := testkit.NewFakeProvider("fake")
	h := newFakeHarness(t, fake)
	seedRegistry(t, h.configPath, active, []maildoconfig.Account{scopeWorkAccount(), scopePersonalAccount()})
	return h
}

func scopeWorkAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:              "work",
		Provider:          "fake",
		Email:             "work@example.test",
		DisplayName:       "Work Account",
		ProviderAccountID: "fake-work",
		AuthRef:           "fake/work",
		DefaultFrom:       "work@example.test",
	}
}

func scopePersonalAccount() maildoconfig.Account {
	return maildoconfig.Account{
		Name:     "personal",
		Provider: "fake",
		AuthRef:  "fake/personal",
	}
}

func assertSelection(t *testing.T, sel provider.AccountSelection, scope string, accounts ...string) {
	t.Helper()
	if sel.Scope != scope {
		t.Fatalf("scope = %q, want %q; selection=%#v", sel.Scope, scope, sel)
	}
	if len(sel.Accounts) != len(accounts) {
		t.Fatalf("accounts = %d, want %d; selection=%#v", len(sel.Accounts), len(accounts), sel)
	}
	for i, want := range accounts {
		if sel.Accounts[i].Name != want {
			t.Fatalf("account[%d] = %q, want %q; selection=%#v", i, sel.Accounts[i].Name, want, sel)
		}
	}
}

func assertSelectionProviders(t *testing.T, sel provider.AccountSelection, providerName string) {
	t.Helper()
	for i, account := range sel.Accounts {
		if account.Provider != providerName {
			t.Fatalf("account[%d] provider = %q, want %q; selection=%#v", i, account.Provider, providerName, sel)
		}
	}
}

func argsWith(base []string, extra ...string) []string {
	out := append([]string(nil), base...)
	return append(out, extra...)
}

func metaStringSlice(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("meta value = %#v, want array", value)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("meta item = %#v, want string", item)
		}
		out = append(out, s)
	}
	return out
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func errorJSON(t *testing.T, res testutil.Result) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("decode stderr: %v\n%s", err, res.Stderr)
	}
	return body
}

func assertFrameworkUsage(t *testing.T, res testutil.Result) {
	t.Helper()
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1\nstdout=%s\nstderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	assertNoCode(t, res)
}

func capState(report provider.CapabilityReport, cap provider.Capability) string {
	for _, c := range report.Capabilities {
		if c.Name == string(cap) {
			return c.State
		}
	}
	return ""
}

func findManifestCommand(t *testing.T, m manifest.Manifest, path string) manifest.Command {
	t.Helper()
	for _, c := range m.Commands {
		if strings.Join(c.Path, " ") == path {
			return c
		}
	}
	t.Fatalf("manifest command %q not found", path)
	return manifest.Command{}
}

func assertFlagSet(t *testing.T, c manifest.Command, want map[string]string, absent []string) {
	t.Helper()
	flags := map[string]manifest.Flag{}
	for _, f := range c.LocalFlags {
		flags[f.Name] = f
	}
	for name, typ := range want {
		flag, ok := flags[name]
		if !ok {
			t.Fatalf("%s missing flag %q; flags=%#v", strings.Join(c.Path, " "), name, c.LocalFlags)
		}
		if flag.Type != typ {
			t.Fatalf("%s flag %q type = %q, want %q", strings.Join(c.Path, " "), name, flag.Type, typ)
		}
	}
	for _, name := range absent {
		if _, ok := flags[name]; ok {
			t.Fatalf("%s unexpectedly has flag %q; flags=%#v", strings.Join(c.Path, " "), name, c.LocalFlags)
		}
	}
}

func assertCommandModes(t *testing.T, c manifest.Command, want []string) {
	t.Helper()
	if !sameStrings(c.OutputModes, want) {
		t.Fatalf("%s output modes = %#v, want %#v", strings.Join(c.Path, " "), c.OutputModes, want)
	}
}

func emptyLookupEnv(string) (string, bool) { return "", false }

func runExistingScopeApp(t *testing.T, app *rungrad.App, configPath string, opts testutil.Options, args ...string) testutil.Result {
	t.Helper()
	fullArgs := append([]string{"--config", configPath}, args...)
	if opts.LookupEnv == nil {
		opts.LookupEnv = emptyLookupEnv
	}
	return testutil.RunWith(app, opts, fullArgs...)
}

func scopeCredentialPath(root string, account maildoconfig.Account) string {
	return filepath.Join(root, "auth", account.Provider, account.Name+".json")
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertMissingPath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s existence err = %v, want missing", path, err)
	}
}

func seedScopeCredential(t *testing.T, root string, account maildoconfig.Account, access, refresh string) {
	t.Helper()
	if err := testkit.SeedCredential(root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed credential for %s: %v", account.Name, err)
	}
}

func assertAccounts(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !sameStrings(got, want) {
		t.Fatalf("account-client accounts = %#v, want %#v", got, want)
	}
}

func accountLogDelta(fake *testkit.FakeProvider, start int) []string {
	accounts := fake.AccountClientAccounts()
	if start > len(accounts) {
		return nil
	}
	return append([]string(nil), accounts[start:]...)
}

func assertNoFrameworkAuthState(t *testing.T, app *rungrad.App) {
	t.Helper()
	if app.Factory().Token != "" || app.Factory().Credential() != (rungrad.Credential{}) {
		t.Fatalf("framework credential state = token %q credential %+v", app.Factory().Token, app.Factory().Credential())
	}
}

type childReadCounter struct {
	reads int
}

func (r *childReadCounter) Read([]byte) (int, error) {
	r.reads++
	return 0, fmt.Errorf("unexpected child stdin read")
}

func runHandlerOwnedAuthChild(which string) int {
	root, err := os.MkdirTemp("", "maildo-handler-owned-auth-child-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "child setup temp dir: %v\n", err)
		return handlerOwnedAuthHelperFailureExit
	}
	defer os.RemoveAll(root)
	configPath := filepath.Join(root, "config.yaml")
	work := scopeWorkAccount()
	personal := scopePersonalAccount()
	if err := maildoconfig.Save(configPath, &maildoconfig.Registry{
		Version:       1,
		ActiveAccount: "work",
		Accounts:      []maildoconfig.Account{work, personal},
	}, []string{"fake", "outlook"}); err != nil {
		fmt.Fprintf(os.Stderr, "child seed registry: %v\n", err)
		return handlerOwnedAuthHelperFailureExit
	}
	if err := testkit.SeedCredential(root, work, &testkit.TokenCache{
		Version:      1,
		Provider:     work.Provider,
		AccountName:  work.Name,
		AccessToken:  "child-active-access-token-12345",
		RefreshToken: "child-active-refresh-token-12345",
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "child seed active credential: %v\n", err)
		return handlerOwnedAuthHelperFailureExit
	}
	if which == "success" {
		if err := testkit.SeedCredential(root, personal, &testkit.TokenCache{
			Version:      1,
			Provider:     personal.Provider,
			AccountName:  personal.Name,
			AccessToken:  "child-selected-access-token-12345",
			RefreshToken: "child-selected-refresh-token-12345",
			ExpiresAt:    time.Unix(1893456000, 0),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "child seed selected credential: %v\n", err)
			return handlerOwnedAuthHelperFailureExit
		}
	}
	activeBefore, err := os.ReadFile(scopeCredentialPath(root, work))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child read active before: %v\n", err)
		return handlerOwnedAuthHelperFailureExit
	}
	if which == "logged_out" {
		if _, err := os.Stat(scopeCredentialPath(root, personal)); !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "child selected cache exists err=%v\n", err)
			return handlerOwnedAuthHelperFailureExit
		}
	}
	fake := testkit.NewFakeProvider("fake")
	providers := testkit.NewProviderRegistry(fake)
	resolver := &countingRunGradResolver{token: "child-framework-token-12345"}
	deps := Deps{Providers: providers, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(providers)...), SetupParsers: defaultSetupParsers(providers.Names()), RunGradAuth: resolver}
	app := newApp(deps, newScopeModule(&scopeHandlers{deps: deps}))
	browserCalls := 0
	app.Factory().PromptTerminalSet = true
	app.Factory().PromptTerminal = true
	app.Factory().BrowserOpener = func(context.Context, string) error {
		browserCalls++
		return nil
	}
	stdin := &childReadCounter{}
	code := app.RunIO([]string{"--config", configPath, "scopekit", "open", "--account", "personal", "--json"}, stdin, os.Stdout, os.Stderr)
	if resolver.calls != 0 {
		fmt.Fprintf(os.Stderr, "child resolver calls = %d\n", resolver.calls)
		return handlerOwnedAuthHelperFailureExit
	}
	if browserCalls != 0 {
		fmt.Fprintf(os.Stderr, "child browser calls = %d\n", browserCalls)
		return handlerOwnedAuthHelperFailureExit
	}
	if stdin.reads != 0 {
		fmt.Fprintf(os.Stderr, "child stdin reads = %d\n", stdin.reads)
		return handlerOwnedAuthHelperFailureExit
	}
	wantClientCalls := int64(1)
	wantAccounts := []string{"personal"}
	if which == "logged_out" {
		wantClientCalls = 0
		wantAccounts = nil
	}
	if fake.AccountClientCalls.Load() != wantClientCalls {
		fmt.Fprintf(os.Stderr, "child account client calls = %d\n", fake.AccountClientCalls.Load())
		return handlerOwnedAuthHelperFailureExit
	}
	if !sameStrings(fake.AccountClientAccounts(), wantAccounts) {
		fmt.Fprintf(os.Stderr, "child account-client accounts = %#v\n", fake.AccountClientAccounts())
		return handlerOwnedAuthHelperFailureExit
	}
	if app.Factory().Token != "" || app.Factory().Credential() != (rungrad.Credential{}) {
		fmt.Fprintf(os.Stderr, "child framework auth state = token %q credential %+v\n", app.Factory().Token, app.Factory().Credential())
		return handlerOwnedAuthHelperFailureExit
	}
	activeAfter, err := os.ReadFile(scopeCredentialPath(root, work))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child read active after: %v\n", err)
		return handlerOwnedAuthHelperFailureExit
	}
	if !bytes.Equal(activeBefore, activeAfter) {
		fmt.Fprintln(os.Stderr, "child active cache changed")
		return handlerOwnedAuthHelperFailureExit
	}
	if which == "logged_out" {
		if _, err := os.Stat(scopeCredentialPath(root, personal)); !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "child selected cache after exists err=%v\n", err)
			return handlerOwnedAuthHelperFailureExit
		}
	}
	switch which {
	case "success":
		if code != 0 {
			fmt.Fprintf(os.Stderr, "child success code = %d\n", code)
			return handlerOwnedAuthHelperFailureExit
		}
	case "logged_out":
		if code != 3 {
			fmt.Fprintf(os.Stderr, "child logged-out code = %d\n", code)
			return handlerOwnedAuthHelperFailureExit
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown child case %q\n", which)
		return handlerOwnedAuthHelperFailureExit
	}
	return code
}
