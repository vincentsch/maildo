package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/docsgen"
	"github.com/vincentsch/rungrad/testutil"
)

func resolvedArchivePolicyReport() mail.ArchivePolicyReport {
	return mail.ArchivePolicyReport{
		Status: mail.ArchivePolicyReportPartial, MailboxPurpose: "user", ArchiveFolderID: "AAMk...",
		ArchiveFolder: &mail.ArchivePolicyFolder{
			ID: "AAMk...", DisplayName: "Archive",
			TotalItemCountKnown: true, UnreadItemCountKnown: true, ChildFolderCountKnown: true,
		},
		OnlineArchive:    mail.ArchivePolicyAvailability{Status: mail.ArchivePolicyAvailabilityUnavailable, Reason: mail.ArchivePolicyReasonNotExposedByGraphMailAPI},
		RetentionPolicy:  mail.ArchivePolicyAvailability{Status: mail.ArchivePolicyAvailabilityUnavailable, Reason: mail.ArchivePolicyReasonRequiresExchangeAdminContext},
		Warnings:         []mail.ArchivePolicyWarning{},
		ProviderMetadata: mail.ArchivePolicyProviderMetadata{Source: mail.ArchivePolicySourceMicrosoftGraph},
	}
}

func unavailableArchivePolicyReport() mail.ArchivePolicyReport {
	report := resolvedArchivePolicyReport()
	report.Status = mail.ArchivePolicyReportUnavailable
	report.MailboxPurpose = ""
	report.ArchiveFolderID = ""
	report.ArchiveFolder = nil
	report.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningMailboxSettingsUnavailable}
	return report
}

func seededArchivePolicyHarness(t *testing.T, fake *testkit.FakeProvider, active string, names ...string) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	accounts := make([]maildoconfig.Account, 0, len(names))
	for _, name := range names {
		account := maildoconfig.Account{Name: name, Provider: fake.Name(), Email: name + "@example.test", AuthRef: fake.Name() + "/" + name}
		accounts = append(accounts, account)
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
	}
	seedRegistry(t, h.configPath, active, accounts)
	return h
}

func TestArchivePolicyCheckStableOutputTransformsMetadataAndDryRun(t *testing.T) {
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{
		"work": resolvedArchivePolicyReport(),
	})
	h := seededArchivePolicyHarness(t, fake, "work", "work")

	for _, tt := range []struct {
		args   []string
		golden string
	}{
		{args: []string{"outlook", "archive-policy", "check"}, golden: "testdata/archive_policy_resolved_human.golden"},
		{args: []string{"outlook", "archive-policy", "check", "--json"}, golden: "testdata/archive_policy_resolved_json.golden"},
	} {
		want, err := os.ReadFile(tt.golden)
		if err != nil {
			t.Fatal(err)
		}
		res := h.run(tt.args...)
		decodeOK(t, res, nil)
		if res.Stdout != string(want) {
			t.Fatalf("output differs from %s\nwant=%q\ngot =%q", tt.golden, string(want), res.Stdout)
		}
	}

	normal := h.run("outlook", "archive-policy", "check", "--json")
	dry := h.run("outlook", "archive-policy", "check", "--dry-run", "--json")
	decodeOK(t, normal, nil)
	decodeOK(t, dry, nil)
	if normal.Stdout != dry.Stdout {
		t.Fatalf("dry-run JSON differs\nnormal=%s\ndry=%s", normal.Stdout, dry.Stdout)
	}
	jq := h.run("outlook", "archive-policy", "check", "--json", "--jq", ".online_archive.reason")
	decodeOK(t, jq, nil)
	if strings.TrimSpace(jq.Stdout) != "not_exposed_by_graph_mail_api" {
		t.Fatalf("jq output = %q", jq.Stdout)
	}
	template := h.run("outlook", "archive-policy", "check", "--template", "{{.account}}:{{.status}}")
	decodeOK(t, template, nil)
	if strings.TrimSpace(template.Stdout) != "work:partial" {
		t.Fatalf("template output = %q", template.Stdout)
	}

	meta := h.run("outlook", "archive-policy", "check", "--include-meta", "--json")
	decodeOK(t, meta, nil)
	var envelope map[string]any
	if err := json.Unmarshal([]byte(meta.Stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 2 || envelope["data"] == nil || envelope["meta"] == nil {
		t.Fatalf("metadata envelope = %#v", envelope)
	}
	dataObject := envelope["data"].(map[string]any)
	accounts := dataObject["accounts"].([]any)
	if dataObject["scope"] != "single" || dataObject["account"] != "work" || dataObject["provider"] != "outlook" || len(accounts) != 1 {
		t.Fatalf("metadata data selection = %#v", dataObject)
	}
	metaObject := envelope["meta"].(map[string]any)
	if len(metaObject) != 1 {
		t.Fatalf("meta = %#v", metaObject)
	}
	extra := metaObject["extra"].(map[string]any)
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"account", "command_path", "provider", "report_status", "scope", "warning_codes"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("meta keys = %#v", keys)
	}
	if extra["account"] != "work" || extra["provider"] != "outlook" || extra["report_status"] != "partial" || extra["scope"] != "single" || extra["command_path"] != "maildo outlook archive-policy check" || len(extra["warning_codes"].([]any)) != 0 {
		t.Fatalf("meta extra = %#v", extra)
	}
	if _, ok := extra["accounts"]; ok {
		t.Fatalf("legacy accounts metadata present: %#v", extra)
	}
	if _, ok := extra["providers"]; ok {
		t.Fatalf("legacy providers metadata present: %#v", extra)
	}
	if fake.CapabilityCalls.Load() != 7 || fake.AccountClientCalls.Load() != 7 || fake.ArchiveReportCalls.Load() != 7 {
		t.Fatalf("counters capability=%d client=%d report=%d", fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.ArchiveReportCalls.Load())
	}
}

func TestArchivePolicyNestedHumanPresentationControls(t *testing.T) {
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{
		"work": resolvedArchivePolicyReport(),
	})
	h := seededArchivePolicyHarness(t, fake, "work", "work")
	baseline := h.run("outlook", "archive-policy", "check")
	decodeOK(t, baseline, nil)
	for _, flag := range []string{"--no-color", "--no-ansi", "--no-pager"} {
		res := h.run("outlook", "archive-policy", "check", flag)
		decodeOK(t, res, nil)
		if res.Stdout != baseline.Stdout {
			t.Fatalf("%s output differs\nbase=%q\ngot =%q", flag, baseline.Stdout, res.Stdout)
		}
	}
}

func TestArchivePolicyCheckUnavailableAndHumanWarningVariants(t *testing.T) {
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": unavailableArchivePolicyReport()})
	h := seededArchivePolicyHarness(t, fake, "work", "work")
	for _, tt := range []struct {
		args   []string
		golden string
	}{
		{[]string{"outlook", "archive-policy", "check"}, "testdata/archive_policy_unavailable_human.golden"},
		{[]string{"outlook", "archive-policy", "check", "--json"}, "testdata/archive_policy_unavailable_json.golden"},
	} {
		want, err := os.ReadFile(tt.golden)
		if err != nil {
			t.Fatal(err)
		}
		res := h.run(tt.args...)
		decodeOK(t, res, nil)
		if res.Stdout != string(want) {
			t.Fatalf("output differs from %s\nwant=%q\ngot =%q", tt.golden, string(want), res.Stdout)
		}
	}

	missing := resolvedArchivePolicyReport()
	missing.ArchiveFolderID = ""
	missing.ArchiveFolder = nil
	missing.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}
	fake.ArchiveReportsByAccount["work"] = missing
	res := h.run("outlook", "archive-policy", "check")
	decodeOK(t, res, nil)
	if !strings.Contains(res.Stdout, "maildo could not read the folder's details.\n  ID:          not reported") || !strings.Contains(res.Stdout, "Warning: the mailbox settings do not name an Archive folder.") {
		t.Fatalf("missing-id human output = %s", res.Stdout)
	}

	unresolved := missing
	unresolved.ArchiveFolderID = "opaque-id"
	unresolved.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}
	fake.ArchiveReportsByAccount["work"] = unresolved
	res = h.run("outlook", "archive-policy", "check")
	decodeOK(t, res, nil)
	if !strings.Contains(res.Stdout, "maildo could not read the folder's details.\n  ID:          opaque-id") || !strings.Contains(res.Stdout, "Warning: maildo could not find the folder that the mailbox settings name as the Archive folder.") {
		t.Fatalf("unresolved human output = %s", res.Stdout)
	}
}

func TestArchivePolicyCheckSanitizesHumanFieldsAndPreservesJSON(t *testing.T) {
	report := resolvedArchivePolicyReport()
	report.MailboxPurpose = "user\r\nForged: purpose"
	report.ArchiveFolderID = "id\tvalue"
	report.ArchiveFolder.ID = report.ArchiveFolderID
	report.ArchiveFolder.DisplayName = "Archive\x1b[31m\nForged: name"
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": report})
	h := seededArchivePolicyHarness(t, fake, "work", "work")
	human := h.run("outlook", "archive-policy", "check")
	decodeOK(t, human, nil)
	if strings.Contains(human.Stdout, "\x1b") || strings.Contains(human.Stdout, "\r") || strings.Contains(human.Stdout, "\t") || strings.Contains(human.Stdout, "\nForged:") {
		t.Fatalf("human output retained presentation controls: %q", human.Stdout)
	}
	machine := h.run("outlook", "archive-policy", "check", "--json")
	decodeOK(t, machine, nil)
	var got archivePolicyCheckResult
	if err := json.Unmarshal([]byte(machine.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.MailboxPurpose != report.MailboxPurpose || got.ArchiveFolderID != report.ArchiveFolderID || got.ArchiveFolder.DisplayName != report.ArchiveFolder.DisplayName {
		t.Fatalf("machine strings changed: %#v", got)
	}
}

func TestArchivePolicyGuardOrderAndCapabilityFailures(t *testing.T) {
	t.Run("provider mismatch precedes capabilities and credentials", func(t *testing.T) {
		fake := testkit.NewFakeProvider("fake")
		h := newFakeHarness(t, fake)
		seedRegistry(t, h.configPath, "work", []maildoconfig.Account{{Name: "work", Provider: "fake", AuthRef: "fake/work"}})
		res := h.run("outlook", "archive-policy", "check", "--json")
		assertErrorCode(t, res, errs.CodeUnsupportedCapability)
		if fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 0 || res.Stdout != "" {
			t.Fatalf("provider guard counters/output = %d/%d %q", fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), res.Stdout)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"account", "provider", "required_provider", "capability", "code"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("provider mismatch detail %q missing: %#v", key, body)
			}
		}
	})

	tests := []struct {
		name       string
		fake       *testkit.FakeProvider
		wantCode   string
		wantMarker string
	}{
		{"missing archive capability", testkit.NewFakeProvider("outlook"), errs.CodeUnsupportedCapability, string(provider.CapArchivePolicyReport)},
		{"unsupported archive capability", func() *testkit.FakeProvider {
			f := testkit.NewFakeProvider("outlook")
			f.ArchiveReportUnsupported = true
			return f
		}(), errs.CodeUnsupportedCapability, string(provider.CapArchivePolicyReport)},
		{"pending archive capability", testkit.NewFakePendingArchivePolicyProvider("outlook"), errs.CodeCapabilityNotImplemented, string(provider.CapArchivePolicyReport)},
		{"missing folders prerequisite", func() *testkit.FakeProvider {
			f := testkit.NewFakeArchivePolicyProvider("outlook", nil)
			f.FoldersImplemented = false
			return f
		}(), errs.CodeUnsupportedCapability, string(provider.CapFoldersRead)},
		{"unsupported folders prerequisite", func() *testkit.FakeProvider {
			f := testkit.NewFakeArchivePolicyProvider("outlook", nil)
			f.FoldersImplemented = false
			f.FoldersUnsupported = true
			return f
		}(), errs.CodeUnsupportedCapability, string(provider.CapFoldersRead)},
		{"pending folders prerequisite", func() *testkit.FakeProvider {
			f := testkit.NewFakeArchivePolicyProvider("outlook", nil)
			f.FoldersImplemented = false
			f.FoldersSupportedNotImpl = true
			return f
		}(), errs.CodeCapabilityNotImplemented, string(provider.CapFoldersRead)},
		{"stale reporter", testkit.NewFakeStaleArchivePolicyProvider("outlook"), errs.CodeCapabilityNotImplemented, "archive_policy_report"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := seededArchivePolicyHarness(t, tt.fake, "work", "work")
			res := h.run("outlook", "archive-policy", "check", "--json")
			assertErrorCode(t, res, tt.wantCode)
			if tt.fake.CapabilityCalls.Load() != 1 || res.Stdout != "" || !strings.Contains(res.Stderr, tt.wantMarker) {
				t.Fatalf("capability calls/output = %d %q", tt.fake.CapabilityCalls.Load(), res.Stdout)
			}
			if tt.name == "stale reporter" && tt.fake.AccountClientCalls.Load() != 1 {
				t.Fatalf("stale reporter client calls = %d", tt.fake.AccountClientCalls.Load())
			}
			if tt.name != "stale reporter" && tt.fake.AccountClientCalls.Load() != 0 {
				t.Fatalf("capability failure loaded credentials %d times", tt.fake.AccountClientCalls.Load())
			}
			if tt.fake.ArchiveReportCalls.Load() != 0 {
				t.Fatalf("guard failure called report %d times", tt.fake.ArchiveReportCalls.Load())
			}
		})
	}

	capErr := testkit.NewFakeArchivePolicyProvider("outlook", nil)
	capErr.CapabilityErr = errors.New("capability fixture failure")
	h := seededArchivePolicyHarness(t, capErr, "work", "work")
	res := h.run("outlook", "archive-policy", "check", "--json")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "capability fixture failure") || capErr.CapabilityCalls.Load() != 1 || capErr.AccountClientCalls.Load() != 0 {
		t.Fatalf("capability error result=%#v counters=%d/%d", res, capErr.CapabilityCalls.Load(), capErr.AccountClientCalls.Load())
	}

	clientErr := testkit.NewFakeArchivePolicyProvider("outlook", nil)
	clientErr.AccountClientErr = errors.New("client fixture failure")
	h = seededArchivePolicyHarness(t, clientErr, "work", "work")
	res = h.run("outlook", "archive-policy", "check", "--json")
	if res.Exit == 0 || res.Stdout != "" || !strings.Contains(res.Stderr, "client fixture failure") || clientErr.CapabilityCalls.Load() != 1 || clientErr.AccountClientCalls.Load() != 1 || clientErr.ArchiveReportCalls.Load() != 0 {
		t.Fatalf("client error result=%#v counters=%d/%d/%d", res, clientErr.CapabilityCalls.Load(), clientErr.AccountClientCalls.Load(), clientErr.ArchiveReportCalls.Load())
	}

	reportErr := testkit.NewFakeArchivePolicyProvider("outlook", nil)
	reportErr.ArchiveReportErrByAccount = map[string]error{"work": errors.New("report fixture failure")}
	h = seededArchivePolicyHarness(t, reportErr, "work", "work")
	res = h.run("outlook", "archive-policy", "check", "--json")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "report fixture failure") || res.Stdout != "" || reportErr.ArchiveReportCalls.Load() != 1 {
		t.Fatalf("report error result=%#v calls=%d", res, reportErr.ArchiveReportCalls.Load())
	}

	repeated := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
	h = seededArchivePolicyHarness(t, repeated, "work", "work")
	assertErrorCode(t, h.run("outlook", "archive-policy", "check", "--account", "work", "--account", "work", "--json"), errs.CodeInvalidAccountScope)
	if repeated.CapabilityCalls.Load() != 0 {
		t.Fatal("repeated account reached capability load")
	}
	all := h.run("outlook", "archive-policy", "check", "--all-accounts", "--json")
	if all.Exit == 0 || !strings.Contains(all.Stderr, "unknown flag") {
		t.Fatalf("--all-accounts result = %#v", all)
	}
}

func TestArchivePolicyAccountSelectionAndFrameworkOutputGuards(t *testing.T) {
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{
		"work": resolvedArchivePolicyReport(), "personal": resolvedArchivePolicyReport(),
	})
	h := seededArchivePolicyHarness(t, fake, "work", "work", "personal")
	var selected archivePolicyCheckResult
	decodeOK(t, h.run("outlook", "archive-policy", "check", "--account", "personal", "--json"), &selected)
	if selected.Account != "personal" || len(selected.Accounts) != 1 || selected.Accounts[0].Name != "personal" || selected.Accounts[0].Active {
		t.Fatalf("explicit selection = %#v", selected)
	}
	beforeCaps, beforeReports := fake.CapabilityCalls.Load(), fake.ArchiveReportCalls.Load()
	assertErrorCode(t, h.run("outlook", "archive-policy", "check", "--account", "missing", "--json"), errs.CodeUnknownAccount)
	if fake.CapabilityCalls.Load() != beforeCaps || fake.ArchiveReportCalls.Load() != beforeReports {
		t.Fatal("unknown account reached provider work")
	}

	for _, args := range [][]string{
		{"outlook", "archive-policy", "check", "--plain"},
		{"outlook", "archive-policy", "check", "--dry-run", "--include-meta", "--json"},
	} {
		res := h.run(args...)
		if res.Exit == 0 || res.Stdout != "" {
			t.Fatalf("framework guard %v = %#v", args, res)
		}
	}
	if fake.CapabilityCalls.Load() != beforeCaps || fake.ArchiveReportCalls.Load() != beforeReports {
		t.Fatal("framework output guard reached handler")
	}

	missingActive := seededArchivePolicyHarness(t, fake, "", "work")
	assertErrorCode(t, missingActive.run("outlook", "archive-policy", "check", "--json"), errs.CodeMissingActiveAccount)
}

// archiveIdentityProvider wraps the normal archive fake but can replace the
// authenticated client's identity, including returning a typed nil client.
type archiveIdentityProvider struct {
	*testkit.FakeProvider
	clientAccount  string
	clientProvider string
	nilClient      bool
	reportAccount  string
	reportProvider string
}

func (p *archiveIdentityProvider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	client, err := p.FakeProvider.NewAccountClient(ctx, ac)
	if err != nil {
		return nil, err
	}
	if p.nilClient {
		return nil, nil
	}
	reporter := client.(provider.ArchivePolicyReporter)
	account := ac.Account
	providerName := ac.Provider
	if p.clientAccount != "" || p.clientProvider != "" {
		account = p.clientAccount
		providerName = p.clientProvider
	}
	return archiveIdentityClient{
		ArchivePolicyReporter: reporter,
		client:                client,
		account:               account, providerName: providerName,
		reportAccount: p.reportAccount, reportProvider: p.reportProvider,
	}, nil
}

// archiveIdentityClient lets tests vary client and report identities
// independently while preserving the reporter interface.
type archiveIdentityClient struct {
	provider.ArchivePolicyReporter
	client         provider.AccountClient
	account        string
	providerName   string
	reportAccount  string
	reportProvider string
}

func (c archiveIdentityClient) AccountName() string  { return c.account }
func (c archiveIdentityClient) ProviderName() string { return c.providerName }
func (c archiveIdentityClient) Destroy() {
	if c.client != nil {
		c.client.Destroy()
	}
}
func (c archiveIdentityClient) ArchivePolicyReport(ctx context.Context, req mail.ArchivePolicyReportRequest) (mail.ArchivePolicyReport, error) {
	report, err := c.ArchivePolicyReporter.ArchivePolicyReport(ctx, req)
	if c.reportAccount != "" {
		report.Account = c.reportAccount
	}
	if c.reportProvider != "" {
		report.Provider = c.reportProvider
	}
	return report, err
}

func TestArchivePolicyRejectsReportSelectionIdentityMismatch(t *testing.T) {
	secret := "returned-secret\nForged: identity"
	for _, tt := range []struct {
		name           string
		clientAccount  string
		clientProvider string
		nilClient      bool
		reportAccount  string
		reportProvider string
		wantReports    int64
	}{
		{name: "nil client", nilClient: true},
		{name: "blank client account", clientAccount: "", clientProvider: "outlook"},
		{name: "blank client provider", clientAccount: "work", clientProvider: ""},
		{name: "different client account", clientAccount: secret, clientProvider: "outlook"},
		{name: "different client provider", clientAccount: "work", clientProvider: secret},
		{name: "different report account", reportAccount: secret, wantReports: 1},
		{name: "different report provider", reportProvider: secret, wantReports: 1},
		{name: "different report account and provider", reportAccount: secret, reportProvider: secret, wantReports: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
			wrapped := &archiveIdentityProvider{FakeProvider: base, clientAccount: tt.clientAccount, clientProvider: tt.clientProvider, nilClient: tt.nilClient, reportAccount: tt.reportAccount, reportProvider: tt.reportProvider}
			h := newFakeHarness(t, base)
			h.providers = testkit.NewProviderRegistry(wrapped)
			account := maildoconfig.Account{Name: "work", Provider: "outlook", Email: "work@example.test", AuthRef: "outlook/work"}
			seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
			if err := testkit.SeedCredential(h.root, account, nil); err != nil {
				t.Fatal(err)
			}
			res := h.run("outlook", "archive-policy", "check", "--json")
			if res.Exit != 2 || res.Stdout != "" ||
				(!strings.Contains(res.Stderr, "account_client_invalid") && !strings.Contains(res.Stderr, `could not read archive policy for account \"work\" (outlook)`)) ||
				strings.Contains(res.Stderr, "returned-secret") || strings.Contains(res.Stderr, "Forged") {
				t.Fatalf("identity mismatch result = %#v", res)
			}
			if base.CapabilityCalls.Load() != 1 || base.AccountClientCalls.Load() != 1 || base.ArchiveReportCalls.Load() != tt.wantReports {
				t.Fatalf("identity counters = %d/%d/%d", base.CapabilityCalls.Load(), base.AccountClientCalls.Load(), base.ArchiveReportCalls.Load())
			}
		})
	}
}

func TestOutlookArchivePolicyCommandTreeHelpAndInvalidPath(t *testing.T) {
	app := newApp(productionDeps())
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"--help"}, want: "outlook"},
		{args: []string{"outlook"}, want: "archive-policy"},
		{args: []string{"outlook", "--help"}, want: "archive-policy"},
		{args: []string{"outlook", "archive-policy"}, want: "check"},
		{args: []string{"outlook", "archive-policy", "--help"}, want: "check"},
		{args: []string{"outlook", "archive-policy", "check", "--help"}, want: "Show which archive and retention settings maildo can see"},
	} {
		res := testutil.Run(app, tt.args...)
		if res.Exit != 0 || !strings.Contains(res.Stdout, tt.want) {
			t.Fatalf("help %v = %#v, want %q", tt.args, res, tt.want)
		}
	}
	for _, args := range [][]string{
		{"outlook", "check"},
		{"outlook", "archive-policy", "unexpected"},
	} {
		res := testutil.Run(app, args...)
		if res.Exit != 1 || res.Stdout != "" || res.Stderr == "" {
			t.Fatalf("invalid path %v = %#v", args, res)
		}
	}
	manifest := testutil.CaptureManifest(t, app)
	var subtree []string
	for _, command := range manifest.Commands {
		path := strings.Join(command.Path, " ")
		if strings.HasPrefix(path, "outlook") {
			subtree = append(subtree, path)
		}
		if path == "outlook check" {
			t.Fatalf("manifest exposed invalid outlook check path: %#v", command)
		}
	}
	if want := []string{"outlook", "outlook archive-policy", "outlook archive-policy check"}; !reflect.DeepEqual(subtree, want) {
		t.Fatalf("outlook subtree = %#v, want %#v", subtree, want)
	}
}

func TestProductionOutlookWiringProvidesArchiveReporter(t *testing.T) {
	deps := productionDeps()
	registered, _, ok := deps.Providers.Get("outlook")
	if !ok {
		t.Fatal("production dependencies did not register Outlook")
	}
	caps, err := registered.Capabilities(context.Background())
	if err != nil || caps.State(provider.CapArchivePolicyReport) != provider.CapImplemented {
		t.Fatalf("archive report capability=%q err=%v", caps.State(provider.CapArchivePolicyReport), err)
	}
	root := t.TempDir()
	if err := testkit.SeedOutlookCache(root, "work", nil); err != nil {
		t.Fatal(err)
	}
	binding := auth.CredentialBinding{Provider: "outlook", Account: "work"}
	data, present, err := auth.NewStore(root).LoadBytes(binding.Provider, binding.Account)
	if err != nil || !present {
		t.Fatalf("load seeded cache: present=%t err=%v", present, err)
	}
	cred, err := outlook.CredentialCodec{}.Decode(rungradRegistrar{}, binding, data)
	if err != nil {
		t.Fatal(err)
	}
	session := newCredentialSession(binding, outlook.CredentialCodec{}, rungradRegistrar{}, cred, true)
	defer session.destroy()
	client, err := registered.NewAccountClient(context.Background(), provider.StoredAuthContext{
		Runtime:                   rungradRegistrar{},
		Provider:                  "outlook",
		Account:                   "work",
		Setup:                     maildoconfig.ProviderSetup{Outlook: &maildoconfig.OutlookSetup{ClientID: "test-client-id"}},
		ExpectedEmail:             "work@example.test",
		ExpectedProviderAccountID: "outlook-work",
		Credential:                session,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Destroy()
	if _, ok := client.(provider.ArchivePolicyReporter); !ok {
		t.Fatalf("production Outlook account client %T does not implement archive reporting", client)
	}
}

func TestOutlookArchivePolicyRealAdapterFreshCredentialsDryRunAndRequestContract(t *testing.T) {
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(h.root, "auth", "outlook", "work.json")
	configBefore, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	credentialBefore, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoBefore, err := os.Stat(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	credentialInfoBefore, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}

	normal := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
	decodeOK(t, normal, nil)
	var result archivePolicyCheckResult
	if err := json.Unmarshal([]byte(normal.Stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != mail.ArchivePolicyReportPartial || result.MailboxPurpose != "user" || result.ArchiveFolderID != "archive-id" || result.ArchiveFolder == nil || result.ArchiveFolder.DisplayName != "Archive" ||
		!result.ArchiveFolder.TotalItemCountKnown || !result.ArchiveFolder.UnreadItemCountKnown || !result.ArchiveFolder.ChildFolderCountKnown ||
		result.OnlineArchive.Status != mail.ArchivePolicyAvailabilityUnavailable || result.OnlineArchive.Reason != mail.ArchivePolicyReasonNotExposedByGraphMailAPI ||
		result.RetentionPolicy.Status != mail.ArchivePolicyAvailabilityUnavailable || result.RetentionPolicy.Reason != mail.ArchivePolicyReasonRequiresExchangeAdminContext ||
		result.ProviderMetadata.Source != mail.ArchivePolicySourceMicrosoftGraph || len(result.Warnings) != 0 {
		t.Fatalf("real adapter result = %#v", result)
	}
	if h.server.tokenCalls.Load() != 0 || h.server.archiveSettingsCalls.Load() != 1 || h.server.archiveFolderCalls.Load() != 1 || h.server.archivePolicyCalls.Load() != 2 || h.server.mailFolderCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("fresh calls token=%d settings=%d folder=%d report=%d genericFolders=%d device=%d browser=%d",
			h.server.tokenCalls.Load(), h.server.archiveSettingsCalls.Load(), h.server.archiveFolderCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.mailFolderCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
	records := h.server.archivePolicyRequestRecordsSnapshot()
	if len(records) != 2 || records[0].Method != http.MethodGet || records[0].Path != "/me/mailboxSettings" || records[0].EscapedPath != "/me/mailboxSettings" || records[0].Query.Get("$select") != "archiveFolder,userPurpose" || records[0].Prefer != "" || !records[0].AuthorizationPresent ||
		records[1].Method != http.MethodGet || records[1].Path != "/me/mailFolders/archive-id" || records[1].EscapedPath != "/me/mailFolders/archive-id" || records[1].Query.Get("$select") != "id,displayName,totalItemCount,unreadItemCount,childFolderCount" || records[1].Prefer != "" || !records[1].AuthorizationPresent {
		t.Fatalf("archive request records = %#v", records)
	}
	if !strings.Contains(records[0].RequestURI, "%24select=archiveFolder%2CuserPurpose") || !strings.Contains(records[1].RequestURI, "%24select=id%2CdisplayName%2CtotalItemCount%2CunreadItemCount%2CchildFolderCount") {
		t.Fatalf("archive request URIs = %#v", records)
	}
	for _, forbidden := range []string{"ignored-reply-body-sentinel", "folder-sentinel", "seed-outlook-access-token-work", "seed-outlook-refresh-token-work", h.server.srv.URL, h.root} {
		if strings.Contains(normal.Stdout+normal.Stderr, forbidden) {
			t.Fatalf("real adapter output leaked %q: %#v", forbidden, normal)
		}
	}
	assertArchiveReadFilesUnchanged(t, h.configPath, credentialPath, configBefore, credentialBefore, configInfoBefore, credentialInfoBefore)
	if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("archive report wrote action log or stat failed: %v", err)
	}

	dry := h.runNoBrowser("outlook", "archive-policy", "check", "--dry-run", "--json")
	decodeOK(t, dry, nil)
	if dry.Stdout != normal.Stdout || h.server.archivePolicyCalls.Load() != 4 {
		t.Fatalf("dry-run differs or skipped reads: normal=%s dry=%s calls=%d", normal.Stdout, dry.Stdout, h.server.archivePolicyCalls.Load())
	}
	assertArchiveReadFilesUnchanged(t, h.configPath, credentialPath, configBefore, credentialBefore, configInfoBefore, credentialInfoBefore)
}

func TestArchivePolicyUnrelatedProviderDataNeverEntersOutputMetadataOrStaticSurfaces(t *testing.T) {
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatal(err)
	}
	h.server.archiveSettingsBody.Store([]byte(`{"archiveFolder":"archive-id","userPurpose":"user","automaticRepliesSetting":{"internalReplyMessage":"automatic-reply-secret-sentinel"},"ignored":"settings-secret-sentinel"}`))
	h.server.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":"Archive","totalItemCount":0,"unreadItemCount":0,"childFolderCount":0,"ignored":"folder-secret-sentinel"}`))

	res := h.runNoBrowser("outlook", "archive-policy", "check", "--include-meta", "--json")
	decodeOK(t, res, nil)
	app := newApp(productionDeps())
	help := testutil.CaptureAllHelp(app)
	docs := docsgen.Generate(newApp(productionDeps()))
	manifestBytes, err := json.Marshal(testutil.CaptureManifest(t, newApp(productionDeps())))
	if err != nil {
		t.Fatal(err)
	}
	combined := res.Stdout + res.Stderr + string(manifestBytes)
	for _, value := range help {
		combined += value
	}
	for _, value := range docs {
		combined += value
	}
	for _, forbidden := range []string{
		"automatic-reply-secret-sentinel", "settings-secret-sentinel", "folder-secret-sentinel",
		"seed-outlook-access-token-work", "seed-outlook-refresh-token-work", "Authorization: Bearer",
		h.server.srv.URL, h.root,
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("archive output, metadata, help, docs, or manifest leaked %q", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("metadata report wrote action log: %v", err)
	}
}

// assertArchiveReadFilesUnchanged proves a fresh read did not rewrite either
// local file by comparing content, metadata, and file identity.
func assertArchiveReadFilesUnchanged(t *testing.T, configPath, credentialPath string, configBefore, credentialBefore []byte, configInfoBefore, credentialInfoBefore os.FileInfo) {
	t.Helper()
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	credentialAfter, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoAfter, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	credentialInfoAfter, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) || !bytes.Equal(credentialBefore, credentialAfter) ||
		sha256.Sum256(configBefore) != sha256.Sum256(configAfter) || sha256.Sum256(credentialBefore) != sha256.Sum256(credentialAfter) ||
		configInfoBefore.Mode() != configInfoAfter.Mode() || configInfoBefore.Size() != configInfoAfter.Size() || !configInfoBefore.ModTime().Equal(configInfoAfter.ModTime()) || !os.SameFile(configInfoBefore, configInfoAfter) ||
		credentialInfoBefore.Mode() != credentialInfoAfter.Mode() || credentialInfoBefore.Size() != credentialInfoAfter.Size() || !credentialInfoBefore.ModTime().Equal(credentialInfoAfter.ModTime()) || !os.SameFile(credentialInfoBefore, credentialInfoAfter) {
		t.Fatalf("read-only archive report changed config or credential cache")
	}
}

func TestOutlookArchivePolicyRealAdapterSilentRefreshAndBrokenCredentials(t *testing.T) {
	t.Run("near expiry refresh rotates only credentials", func(t *testing.T) {
		h := newOutlookHarness(t)
		account := outlookFoldersAccount()
		if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
			t.Fatal(err)
		}
		if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
			c.VerifiedEmail = account.Email
			c.ProviderAccountID = account.ProviderAccountID
			c.ExpiresAt = h.now.Add(-time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
		configBefore, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatal(err)
		}
		configInfoBefore, err := os.Stat(h.configPath)
		if err != nil {
			t.Fatal(err)
		}
		credentialPath := filepath.Join(h.root, "auth", "outlook", "work.json")
		credentialBefore, err := os.ReadFile(credentialPath)
		if err != nil {
			t.Fatal(err)
		}
		credentialInfoBefore, err := os.Stat(credentialPath)
		if err != nil {
			t.Fatal(err)
		}
		treeBefore := hashTree(t, h.root)
		res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
		decodeOK(t, res, nil)
		if h.server.tokenCalls.Load() != 1 || h.server.archivePolicyCalls.Load() != 2 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
			t.Fatalf("refresh calls token=%d report=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
		}
		cache, err := loadCredentialCache(h, "outlook", "work")
		if err != nil {
			t.Fatal(err)
		}
		if cache.AccessToken != "cli-refreshed-access-secret-12345" || cache.RefreshToken != "cli-rotated-refresh-secret-12345" || !cache.ExpiresAt.After(h.now) {
			t.Fatalf("rotated cache = %#v", cache)
		}
		configAfter, err := os.ReadFile(h.configPath)
		configInfoAfter, statErr := os.Stat(h.configPath)
		if err != nil || statErr != nil || !bytes.Equal(configBefore, configAfter) || sha256.Sum256(configBefore) != sha256.Sum256(configAfter) || configInfoBefore.Mode() != configInfoAfter.Mode() || configInfoBefore.Size() != configInfoAfter.Size() || !configInfoBefore.ModTime().Equal(configInfoAfter.ModTime()) || !os.SameFile(configInfoBefore, configInfoAfter) {
			t.Fatalf("silent refresh changed config: %v", err)
		}
		credentialAfter, err := os.ReadFile(credentialPath)
		if err != nil {
			t.Fatal(err)
		}
		credentialInfoAfter, err := os.Stat(credentialPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(credentialBefore, credentialAfter) || sha256.Sum256(credentialBefore) == sha256.Sum256(credentialAfter) || os.SameFile(credentialInfoBefore, credentialInfoAfter) || credentialInfoAfter.Mode().Perm() != 0o600 {
			t.Fatalf("silent refresh did not atomically replace the credential cache")
		}
		credentialEntries, err := os.ReadDir(filepath.Dir(credentialPath))
		if err != nil {
			t.Fatal(err)
		}
		if len(credentialEntries) != 1 || credentialEntries[0].Name() != "work.json" || credentialEntries[0].IsDir() {
			t.Fatalf("silent refresh left credential remnants: %#v", credentialEntries)
		}
		credentialRel := filepath.Join("auth", "outlook", "work.json")
		lockRel := filepath.Join("auth", ".transaction.lock")
		if treeAfter := hashTree(t, h.root); !sameStrings(treeWithoutFiles(treeBefore, credentialRel, lockRel), treeWithoutFiles(treeAfter, credentialRel, lockRel)) {
			t.Fatalf("silent refresh changed files outside %q\nbefore=%#v\nafter=%#v", credentialRel, treeBefore, treeAfter)
		}
		if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("silent archive report wrote action log: %v", err)
		}
	})

	for _, tt := range []struct {
		name      string
		seed      bool
		corrupt   bool
		configure func(*outlookHarness, *testkit.TokenCache)
		wantToken int64
	}{
		{name: "missing cache"},
		{name: "corrupt cache", seed: true, corrupt: true},
		{name: "unsupported cache version", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.Version = 99 }},
		{name: "wrong cache provider", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.Provider = "fake" }},
		{name: "wrong cache account", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.AccountName = "other" }},
		{name: "under scoped", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.Scopes = []string{"User.Read"} }},
		{name: "provider account mismatch", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.ProviderAccountID = "different-provider-account" }},
		{name: "verified email fallback mismatch", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) {
			c.ProviderAccountID = ""
			c.VerifiedEmail = "different@example.test"
		}},
		{name: "missing refresh", seed: true, configure: func(_ *outlookHarness, c *testkit.TokenCache) { c.RefreshToken = ""; c.ExpiresAt = time.Unix(1, 0) }},
		{name: "revoked refresh", seed: true, wantToken: 1, configure: func(h *outlookHarness, c *testkit.TokenCache) {
			c.ExpiresAt = time.Unix(1, 0)
			h.server.invalidGrant.Store(true)
		}},
		{name: "malformed refresh response", seed: true, wantToken: 1, configure: func(h *outlookHarness, c *testkit.TokenCache) {
			c.ExpiresAt = time.Unix(1, 0)
			h.server.refreshMalformed.Store(true)
		}},
		{name: "refresh response missing access token", seed: true, wantToken: 1, configure: func(h *outlookHarness, c *testkit.TokenCache) {
			c.ExpiresAt = time.Unix(1, 0)
			h.server.refreshMissingAccess.Store(true)
		}},
		{name: "under-scoped refresh response", seed: true, wantToken: 1, configure: func(h *outlookHarness, c *testkit.TokenCache) {
			c.ExpiresAt = time.Unix(1, 0)
			h.server.refreshUnderScoped.Store(true)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if tt.seed {
				if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
					c.VerifiedEmail = account.Email
					c.ProviderAccountID = account.ProviderAccountID
					if tt.configure != nil {
						tt.configure(h, c)
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.corrupt {
				if err := os.WriteFile(filepath.Join(h.root, "auth", "outlook", "work.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			configBefore, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatal(err)
			}
			configInfoBefore, err := os.Stat(h.configPath)
			if err != nil {
				t.Fatal(err)
			}
			credentialPath := filepath.Join(h.root, "auth", "outlook", "work.json")
			credentialBefore, credentialErr := os.ReadFile(credentialPath)
			var credentialInfoBefore os.FileInfo
			if credentialErr == nil {
				credentialInfoBefore, err = os.Stat(credentialPath)
				if err != nil {
					t.Fatal(err)
				}
			} else if !os.IsNotExist(credentialErr) {
				t.Fatal(credentialErr)
			}
			treeBefore := hashTree(t, h.root)
			res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
			assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
			if res.Stdout != "" || h.server.archivePolicyCalls.Load() != 0 || h.server.tokenCalls.Load() != tt.wantToken || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 || !strings.Contains(res.Stderr, "maildo auth repair work") {
				t.Fatalf("broken credential result=%#v report=%d token=%d device=%d browser=%d", res, h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
			if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("failed archive report wrote action log: %v", err)
			}
			configAfter, err := os.ReadFile(h.configPath)
			configInfoAfter, statErr := os.Stat(h.configPath)
			if err != nil || statErr != nil || !bytes.Equal(configBefore, configAfter) || sha256.Sum256(configBefore) != sha256.Sum256(configAfter) || configInfoBefore.Mode() != configInfoAfter.Mode() || configInfoBefore.Size() != configInfoAfter.Size() || !configInfoBefore.ModTime().Equal(configInfoAfter.ModTime()) || !os.SameFile(configInfoBefore, configInfoAfter) {
				t.Fatalf("failed authentication changed config: %v", err)
			}
			credentialAfter, afterErr := os.ReadFile(credentialPath)
			if credentialErr == nil {
				credentialInfoAfter, statErr := os.Stat(credentialPath)
				if afterErr != nil || statErr != nil || !bytes.Equal(credentialBefore, credentialAfter) || sha256.Sum256(credentialBefore) != sha256.Sum256(credentialAfter) || !os.SameFile(credentialInfoBefore, credentialInfoAfter) || credentialInfoBefore.Mode() != credentialInfoAfter.Mode() || credentialInfoBefore.Size() != credentialInfoAfter.Size() || !credentialInfoBefore.ModTime().Equal(credentialInfoAfter.ModTime()) {
					t.Fatalf("failed authentication replaced the credential cache: read=%v stat=%v", afterErr, statErr)
				}
			} else if !os.IsNotExist(afterErr) {
				t.Fatalf("missing credential cache was created: %v", afterErr)
			}
			if treeAfter := hashTree(t, h.root); !sameStrings(treeBefore, treeAfter) {
				t.Fatalf("failed authentication changed runtime tree\nbefore=%#v\nafter=%#v", treeBefore, treeAfter)
			}
		})
	}
}

func TestOutlookArchivePolicyMalformedRefreshClassificationFollowsHTTPStatus(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		wantExit   int
		wantStderr string
	}{
		{name: "successful response is repair required", status: http.StatusOK, wantExit: rungrad.ExitAuth, wantStderr: "maildo auth repair work"},
		{name: "internal server error is transient", status: http.StatusInternalServerError, wantExit: rungrad.ExitAPI, wantStderr: "could not renew the Outlook login right now"},
		{name: "service unavailable is transient", status: http.StatusServiceUnavailable, wantExit: rungrad.ExitAPI, wantStderr: "could not renew the Outlook login right now"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if err := testkit.SeedOutlookCache(h.root, "work", func(cache *testkit.TokenCache) {
				cache.VerifiedEmail = account.Email
				cache.ProviderAccountID = account.ProviderAccountID
				cache.ExpiresAt = time.Unix(1, 0)
			}); err != nil {
				t.Fatal(err)
			}
			h.server.refreshMalformed.Store(true)
			h.server.refreshMalformedStatus.Store(int64(tt.status))
			before := hashTree(t, h.root)
			res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
			if res.Exit != tt.wantExit || res.Stdout != "" || !strings.Contains(res.Stderr, tt.wantStderr) {
				t.Fatalf("malformed refresh status=%d result=%#v", tt.status, res)
			}
			if h.server.tokenCalls.Load() != 1 || h.server.archivePolicyCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("malformed refresh status=%d token=%d archive=%d device=%d browser=%d", tt.status, h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
			if after := hashTree(t, h.root); !sameStrings(before, after) {
				t.Fatalf("malformed refresh status=%d mutated runtime tree\nbefore=%#v\nafter=%#v", tt.status, before, after)
			}
		})
	}
}

type outlookSiblingCacheSnapshot struct {
	path string
	data []byte
	info os.FileInfo
}

func seedOutlookArchiveAccountsWithSibling(t *testing.T, h *outlookHarness, workExpired bool) outlookSiblingCacheSnapshot {
	t.Helper()
	work := outlookFoldersAccount()
	personal := maildoconfig.Account{
		Name: "personal", Provider: "outlook", Email: "personal@example.test", DisplayName: "Personal",
		ProviderAccountID: "outlook-personal", AuthRef: "outlook/personal",
	}
	if err := testkit.SeedRegistry(h.configPath, "work", work, personal); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(cache *testkit.TokenCache) {
		cache.VerifiedEmail = work.Email
		cache.ProviderAccountID = work.ProviderAccountID
		if workExpired {
			cache.ExpiresAt = time.Unix(1, 0)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "personal", func(cache *testkit.TokenCache) {
		cache.VerifiedEmail = personal.Email
		cache.ProviderAccountID = personal.ProviderAccountID
		cache.AccessToken = "sibling-access-sentinel"
		cache.RefreshToken = "sibling-refresh-sentinel"
		cache.ExpiresAt = time.Unix(1, 0)
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.root, "auth", "outlook", "personal.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return outlookSiblingCacheSnapshot{path: path, data: data, info: info}
}

func assertOutlookSiblingCacheUnchanged(t *testing.T, before outlookSiblingCacheSnapshot) {
	t.Helper()
	after, err := os.ReadFile(before.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(before.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.data, after) || sha256.Sum256(before.data) != sha256.Sum256(after) || !os.SameFile(before.info, info) || before.info.Mode() != info.Mode() || before.info.Size() != info.Size() || !before.info.ModTime().Equal(info.ModTime()) {
		t.Fatal("selected Outlook archive report changed or replaced the sibling credential cache")
	}
}

func TestOutlookArchivePolicySiblingCacheIsolationAcrossFailures(t *testing.T) {
	for _, tt := range []struct {
		name        string
		workExpired bool
		configure   func(*cliOutlookServer)
		wantExit    int
		wantToken   int64
		wantReads   int64
		wantStderr  string
	}{
		{name: "malformed successful refresh", workExpired: true, configure: func(server *cliOutlookServer) { server.refreshMalformed.Store(true) }, wantExit: rungrad.ExitAuth, wantToken: 1, wantStderr: "maildo auth repair work"},
		{name: "malformed unavailable refresh", workExpired: true, configure: func(server *cliOutlookServer) {
			server.refreshMalformed.Store(true)
			server.refreshMalformedStatus.Store(http.StatusServiceUnavailable)
		}, wantExit: rungrad.ExitAPI, wantToken: 1, wantStderr: "could not renew the Outlook login right now"},
		{name: "settings status failure", configure: func(server *cliOutlookServer) { server.archiveSettingsStatus.Store(http.StatusInternalServerError) }, wantExit: rungrad.ExitAPI, wantReads: 1, wantStderr: "could not read archive policy"},
		{name: "malformed settings", configure: func(server *cliOutlookServer) { server.archiveSettingsBody.Store([]byte(`{"archiveFolder":`)) }, wantExit: rungrad.ExitAPI, wantReads: 1, wantStderr: "could not read archive policy"},
		{name: "folder status failure", configure: func(server *cliOutlookServer) { server.archiveFolderStatus.Store(http.StatusInternalServerError) }, wantExit: rungrad.ExitAPI, wantReads: 2, wantStderr: "could not read archive policy"},
		{name: "malformed folder", configure: func(server *cliOutlookServer) { server.archiveFolderBody.Store([]byte(`{"id":`)) }, wantExit: rungrad.ExitAPI, wantReads: 2, wantStderr: "could not read archive policy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			sibling := seedOutlookArchiveAccountsWithSibling(t, h, tt.workExpired)
			tt.configure(h.server)
			before := hashTree(t, h.root)
			res := h.runNoBrowser("outlook", "archive-policy", "check", "--account", "work", "--json")
			if res.Exit != tt.wantExit || res.Stdout != "" || !strings.Contains(res.Stderr, tt.wantStderr) {
				t.Fatalf("failure result=%#v", res)
			}
			if h.server.tokenCalls.Load() != tt.wantToken || h.server.archivePolicyCalls.Load() != tt.wantReads || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("failure token=%d archive=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
			assertOutlookSiblingCacheUnchanged(t, sibling)
			if after := hashTree(t, h.root); !sameStrings(before, after) {
				t.Fatalf("selected failure changed runtime tree\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestOutlookArchivePolicySelectedAccountDoesNotReadSiblingCache(t *testing.T) {
	h := newOutlookHarness(t)
	sibling := seedOutlookArchiveAccountsWithSibling(t, h, false)
	before := hashTree(t, h.root)
	if err := os.Chmod(sibling.path, 0); err != nil {
		t.Fatal(err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.Chmod(sibling.path, sibling.info.Mode().Perm())
		}
	}()
	res := h.runNoBrowser("outlook", "archive-policy", "check", "--account", "work", "--json")
	if err := os.Chmod(sibling.path, sibling.info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	restored = true
	decodeOK(t, res, nil)
	if h.server.tokenCalls.Load() != 0 || h.server.archivePolicyCalls.Load() != 2 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("selected report token=%d archive=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
	assertOutlookSiblingCacheUnchanged(t, sibling)
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("selected report with inaccessible sibling changed runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestOutlookArchivePolicySiblingCacheIsolationDuringCancellation(t *testing.T) {
	h := newOutlookHarness(t)
	sibling := seedOutlookArchiveAccountsWithSibling(t, h, false)
	before := hashTree(t, h.root)
	arrived := make(chan struct{})
	release := make(chan struct{})
	h.server.mu.Lock()
	h.server.archiveFolderArrived = arrived
	h.server.archiveFolderRelease = release
	h.server.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	app := h.app()
	app.Root().SetContext(ctx)
	result := make(chan testutil.Result, 1)
	go func() {
		result <- testutil.RunWith(app, h.options(func(context.Context, string) error {
			h.browserCalls.Add(1)
			return nil
		}), "--config", h.configPath, "outlook", "archive-policy", "check", "--account", "work", "--json")
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		cancel()
		close(release)
		t.Fatal("timed out waiting for the selected account folder read")
	}
	cancel()
	var res testutil.Result
	select {
	case res = <-result:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("canceled archive report did not return")
	}
	close(release)
	if res.Exit != rungrad.ExitAPI || res.Stdout != "" || !strings.Contains(res.Stderr, "could not read archive policy") {
		t.Fatalf("canceled result=%#v", res)
	}
	if h.server.tokenCalls.Load() != 0 || h.server.archivePolicyCalls.Load() != 2 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("canceled token=%d archive=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
	assertOutlookSiblingCacheUnchanged(t, sibling)
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("canceled selected read changed runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestOutlookArchivePolicyRealAdapterWarningsFailuresAndNoPartialOutput(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*cliOutlookServer)
		wantOK    bool
		warning   mail.ArchivePolicyWarning
		wantReads int64
	}{
		{name: "settings unavailable", configure: func(s *cliOutlookServer) {
			s.archiveSettingsStatus.Store(http.StatusNotFound)
			s.archiveSettingsBody.Store([]byte(strings.Repeat("secret-error-body", 100000)))
		}, wantOK: true, warning: mail.ArchivePolicyWarningMailboxSettingsUnavailable, wantReads: 1},
		{name: "missing folder id", configure: func(s *cliOutlookServer) {
			s.archiveSettingsBody.Store([]byte(`{"archiveFolder":null,"userPurpose":"user"}`))
		}, wantOK: true, warning: mail.ArchivePolicyWarningFolderIDMissing, wantReads: 1},
		{name: "folder unresolved", configure: func(s *cliOutlookServer) {
			s.archiveFolderStatus.Store(http.StatusGone)
			s.archiveFolderBody.Store([]byte(strings.Repeat("secret-error-body", 100000)))
		}, wantOK: true, warning: mail.ArchivePolicyWarningFolderUnresolved, wantReads: 2},
		{name: "settings failure", configure: func(s *cliOutlookServer) { s.archiveSettingsStatus.Store(http.StatusInternalServerError) }, wantReads: 1},
		{name: "folder failure", configure: func(s *cliOutlookServer) { s.archiveFolderStatus.Store(http.StatusInternalServerError) }, wantReads: 2},
		{name: "malformed settings", configure: func(s *cliOutlookServer) { s.archiveSettingsBody.Store([]byte(`{"archiveFolder":`)) }, wantReads: 1},
		{name: "duplicate settings field", configure: func(s *cliOutlookServer) {
			s.archiveSettingsBody.Store([]byte(`{"archiveFolder":"archive-id","ArchiveFolder":"other-id"}`))
		}, wantReads: 1},
		{name: "malformed settings text", configure: func(s *cliOutlookServer) {
			s.archiveSettingsBody.Store(append([]byte(`{"archiveFolder":"archive-id","userPurpose":"`), 0xff, '"', '}'))
		}, wantReads: 1},
		{name: "unpaired surrogate in settings", configure: func(s *cliOutlookServer) {
			s.archiveSettingsBody.Store([]byte(`{"archiveFolder":"archive-id","userPurpose":"\ud800"}`))
		}, wantReads: 1},
		{name: "null settings object", configure: func(s *cliOutlookServer) { s.archiveSettingsBody.Store([]byte(`null`)) }, wantReads: 1},
		{name: "invalid archive folder shape", configure: func(s *cliOutlookServer) { s.archiveSettingsBody.Store([]byte(`{"archiveFolder":[]}`)) }, wantReads: 1},
		{name: "invalid mailbox purpose", configure: func(s *cliOutlookServer) {
			s.archiveSettingsBody.Store([]byte(`{"archiveFolder":null,"userPurpose":{"value":false}}`))
		}, wantReads: 1},
		{name: "malformed folder", configure: func(s *cliOutlookServer) { s.archiveFolderBody.Store([]byte(`{"id":`)) }, wantReads: 2},
		{name: "duplicate folder field", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":"Archive","DisplayName":"Other"}`))
		}, wantReads: 2},
		{name: "malformed folder text", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store(append([]byte(`{"id":"archive-id","displayName":"`), 0xff, '"', '}'))
		}, wantReads: 2},
		{name: "unpaired surrogate in folder", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":"\udc00"}`))
		}, wantReads: 2},
		{name: "invalid folder data", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store([]byte(`{"id":"different","displayName":"Archive"}`))
		}, wantReads: 2},
		{name: "blank folder name", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":" "}`))
		}, wantReads: 2},
		{name: "negative folder count", configure: func(s *cliOutlookServer) {
			s.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":"Archive","totalItemCount":-1}`))
		}, wantReads: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
			}); err != nil {
				t.Fatal(err)
			}
			tt.configure(h.server)
			res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
			if tt.wantOK {
				decodeOK(t, res, nil)
				var report archivePolicyCheckResult
				if err := json.Unmarshal([]byte(res.Stdout), &report); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(report.Warnings, []mail.ArchivePolicyWarning{tt.warning}) {
					t.Fatalf("warnings = %#v", report.Warnings)
				}
			} else if res.Exit != 2 || res.Stdout != "" || !strings.Contains(res.Stderr, `could not read archive policy for account \"work\" (outlook)`) {
				t.Fatalf("failure result = %#v", res)
			}
			if h.server.archivePolicyCalls.Load() != tt.wantReads || strings.Contains(res.Stdout+res.Stderr, "secret-error-body") || h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("warning/failure calls or leak result=%#v reads=%d token=%d", res, h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load())
			}
			if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("archive warning/failure wrote action log: %v", err)
			}
		})
	}
}

func TestOutlookArchivePolicyEndpointStatusPrecedenceAndActionLogAbsence(t *testing.T) {
	statuses := []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusGone,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	}
	for _, endpoint := range []string{"settings", "folder"} {
		for _, status := range statuses {
			t.Run(endpoint+"/"+http.StatusText(status), func(t *testing.T) {
				h := newOutlookHarness(t)
				account := outlookFoldersAccount()
				if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
					t.Fatal(err)
				}
				if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
					c.VerifiedEmail = account.Email
					c.ProviderAccountID = account.ProviderAccountID
				}); err != nil {
					t.Fatal(err)
				}
				oversized := []byte(strings.Repeat("secret-status-body", 70000))
				wantReads := int64(1)
				if endpoint == "settings" {
					h.server.archiveSettingsStatus.Store(int64(status))
					h.server.archiveSettingsBody.Store(oversized)
				} else {
					wantReads = 2
					h.server.archiveFolderStatus.Store(int64(status))
					h.server.archiveFolderBody.Store(oversized)
				}

				res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
				successfulMissing := status == http.StatusNotFound || status == http.StatusGone
				if successfulMissing {
					decodeOK(t, res, nil)
					var got archivePolicyCheckResult
					if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
						t.Fatal(err)
					}
					wantWarning := mail.ArchivePolicyWarningMailboxSettingsUnavailable
					if endpoint == "folder" {
						wantWarning = mail.ArchivePolicyWarningFolderUnresolved
					}
					if !reflect.DeepEqual(got.Warnings, []mail.ArchivePolicyWarning{wantWarning}) {
						t.Fatalf("warnings = %#v", got.Warnings)
					}
				} else {
					if res.Stdout != "" {
						t.Fatalf("status failure wrote stdout: %q", res.Stdout)
					}
					switch status {
					case http.StatusUnauthorized, http.StatusForbidden:
						assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
						if !strings.Contains(res.Stderr, "maildo auth repair work") {
							t.Fatalf("auth guidance = %q", res.Stderr)
						}
					case http.StatusTooManyRequests:
						if res.Exit != rungrad.ExitRateLimited || !strings.Contains(res.Stderr, "Outlook archive-policy request was rate limited") {
							t.Fatalf("rate-limited result = %#v", res)
						}
					case http.StatusOK:
						if res.Exit != rungrad.ExitAPI || !strings.Contains(res.Stderr, "Outlook archive-policy response exceeds the read size limit") {
							t.Fatalf("oversized result = %#v", res)
						}
					default:
						if res.Exit != rungrad.ExitAPI || !strings.Contains(res.Stderr, `could not read archive policy for account \"work\" (outlook)`) {
							t.Fatalf("provider status result = %#v", res)
						}
					}
				}
				if h.server.archivePolicyCalls.Load() != wantReads || h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 || strings.Contains(res.Stdout+res.Stderr, "secret-status-body") {
					t.Fatalf("status calls/leak result=%#v reads=%d token=%d", res, h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load())
				}
				if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
					t.Fatalf("status branch wrote action log: %v", err)
				}
			})
		}
	}
}

func TestOutlookArchivePolicyRealAdapterKeepsOpaqueFolderIDsAsOneSegment(t *testing.T) {
	for _, id := range []string{"a/b", "%", "%2F", "?", "#", ".", "..", "Grüße", " archive "} {
		t.Run(id, func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
			}); err != nil {
				t.Fatal(err)
			}
			settingsBody, _ := json.Marshal(map[string]any{"archiveFolder": id, "userPurpose": "user"})
			folderBody, _ := json.Marshal(map[string]any{"id": id, "displayName": "Archive", "totalItemCount": 0, "unreadItemCount": 0, "childFolderCount": 0})
			h.server.archiveSettingsBody.Store(settingsBody)
			h.server.archiveFolderBody.Store(folderBody)
			res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
			decodeOK(t, res, nil)
			var result archivePolicyCheckResult
			if err := json.Unmarshal([]byte(res.Stdout), &result); err != nil {
				t.Fatal(err)
			}
			if result.ArchiveFolderID != id || result.ArchiveFolder == nil || result.ArchiveFolder.ID != id {
				t.Fatalf("opaque id result = %#v", result)
			}
			records := h.server.archivePolicyRequestRecordsSnapshot()
			if len(records) != 2 {
				t.Fatalf("records = %#v", records)
			}
			escapedID := url.PathEscape(id)
			if id == "." {
				escapedID = "%2E"
			} else if id == ".." {
				escapedID = "%2E%2E"
			}
			if records[1].EscapedPath != "/me/mailFolders/"+escapedID || !strings.HasPrefix(records[1].RequestURI, "/me/mailFolders/"+escapedID+"?") || h.server.archiveFolderCalls.Load() != 1 {
				t.Fatalf("opaque id request = %#v", records[1])
			}
		})
	}
}
