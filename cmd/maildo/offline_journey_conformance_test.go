//go:build conformance

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/audit"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
)

// Each child is a test-built executable driving the existing RunGrad app and
// fake-provider injection seams. No production provider lifecycle is registered.
// Protocol adapter behavior is tested separately in the provider packages.
func TestOfflineFourProviderJourneys(t *testing.T) {
	if name := os.Getenv("MAILDO_OFFLINE_JOURNEY_CHILD"); name != "" {
		runOfflineJourney(t, name)
		return
	}
	for _, name := range []string{"outlook", "gmail", "imap", "fastmail"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestOfflineFourProviderJourneys$", "-test.v")
			cmd.Env = append(os.Environ(), "MAILDO_OFFLINE_JOURNEY_CHILD="+name)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("offline journey: %v\n%s", err, output)
			}
		})
	}
}

func TestConformanceWiringContainsOnlyFixtureLifecycle(t *testing.T) {
	deps, err := conformanceDepsE()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range deps.Providers.Names() {
		lifecycle, _, ok := deps.Providers.Get(name)
		if !ok {
			t.Fatal("fixture lifecycle missing")
		}
		switch lifecycle.(type) {
		case conformanceProvider, conformanceProviderAlias:
		default:
			t.Fatalf("tagged fixture registered production lifecycle %T", lifecycle)
		}
	}
	if _, err := newAppE(deps); err != nil {
		t.Fatalf("fixture-only app wiring: %v", err)
	}
}

type journeyProvider struct{ *testkit.FakeProvider }

func (p journeyProvider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	caps, err := p.FakeProvider.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if p.Name() == "imap" || p.Name() == "fastmail" {
		for _, cap := range []provider.Capability{
			provider.CapDraftCreate, provider.CapDraftRead, provider.CapDraftUpdate, provider.CapDraftSend,
			provider.CapReplyDraftCreate, provider.CapForwardDraftCreate, provider.CapAttachmentsMutate,
			provider.CapCategoriesRead, provider.CapCategoriesMutate,
		} {
			caps[cap] = provider.CapUnsupported
		}
	}
	return caps, err
}

func runOfflineJourney(t *testing.T, name string) {
	if name != "outlook" && name != "gmail" && name != "imap" && name != "fastmail" {
		t.Fatal("invalid fixture provider")
	}
	h := &cliHarness{t: t, root: t.TempDir()}
	h.configPath = filepath.Join(h.root, "config.yaml")
	accounts := []maildoconfig.Account{}
	for _, accountName := range []string{"work", "personal"} {
		account := maildoconfig.Account{Name: accountName, Provider: name, Email: accountName + "@example.test", AuthRef: name + "/" + accountName}
		if name == "imap" || name == "fastmail" {
			account.Username = account.Email
			account.MailServer = &maildoconfig.MailServerConfig{
				IMAP: maildoconfig.ServerEndpoint{Host: "imap.example.test", Port: 993, TLS: maildoconfig.TLSImplicit},
				SMTP: maildoconfig.ServerEndpoint{Host: "smtp.example.test", Port: 465, TLS: maildoconfig.TLSImplicit},
			}
			if name == "fastmail" {
				account.MailServer.IMAP = maildoconfig.FastmailIMAPEndpoint()
				account.MailServer.SMTP = maildoconfig.FastmailSMTPEndpoint()
			}
		}
		accounts = append(accounts, account)
	}
	seedRegistry(t, h.configPath, "work", accounts)
	for _, account := range accounts {
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatal(err)
		}
	}
	use := func(fake *testkit.FakeProvider) {
		var lifecycles []provider.Provider
		for _, candidate := range []string{"outlook", "gmail", "imap", "fastmail"} {
			if candidate == name {
				lifecycles = append(lifecycles, journeyProvider{fake})
			} else {
				lifecycles = append(lifecycles, testkit.NewFakeProvider(candidate))
			}
		}
		h.providers = testkit.NewProviderRegistry(lifecycles...)
		h.preparers = provider.NewDirectSendPreparerRegistry()
		for _, lifecycle := range lifecycles {
			caps, err := lifecycle.Capabilities(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if caps.State(provider.CapSendDirect) == provider.CapImplemented {
				policy := provider.ComposeIngressMIMEBounded
				if lifecycle.Name() == "outlook" {
					policy = provider.ComposeIngressDelivered
				}
				if err := h.preparers.Register(lifecycle.Name(), policy, testkit.FakeDirectSendPreparer{Provider: lifecycle.Name()}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	ok := func(args ...string) {
		t.Helper()
		dryRun := slices.Contains(args, "--dry-run")
		var res testutil.Result
		if slices.Contains(args, "--yes") && !dryRun {
			res = approvedRun(t, h.run, args...)
		} else {
			res = h.run(args...)
		}
		decodeOK(t, res, nil)
		testkit.AssertNoSecretLeak(t, res, "seed-access-token-work", "seed-refresh-token-work")
		// Only draft read and previews show the body; previews show it so
		// a person can approve what goes out.
		if !(args[0] == "draft" && args[1] == "read") && !dryRun && strings.Contains(res.Stdout+res.Stderr, "private compose body") {
			t.Fatal("ordinary output contains compose body")
		}
	}

	read := testkit.NewFakeReadProvider(name, testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads())
	read.FoldersImplemented = true
	read.Folders = testkit.SampleFolders()
	use(read)
	for _, args := range [][]string{
		{"auth", "list", "--json"}, {"auth", "status", "work", "--json"},
		{"folders", "list", "--json"}, {"folders", "view", "inbox", "--json"},
		{"search", "invoice", "--account", "work", "--account", "personal", "--json"},
		{"list", "inbox", "--all-accounts", "--plain"},
		{"read", "--id", "work-new", "--json"},
		{"read", "--internet-id", "<work-new@example.test>", "--plain"},
		{"read", "--query", "anything", "--latest", "--json"},
		{"thread", "--query", "anything", "--first", "--json"},
		{"folders", "list", "--jq="}, {"folders", "list", "--template="},
		{"folders", "list", "--json", "--include-meta"},
	} {
		ok(args...)
	}
	if res := h.run("read", "--id", "work-new", "--query", "anything", "--latest", "--json"); res.Exit != 1 || res.Stdout != "" {
		t.Fatalf("ambiguous selector result: %#v", res)
	}
	folders := testkit.NewFakeFolderMutationProvider(name, testkit.SampleFolders())
	use(folders)
	ok("folders", "create", "Disposable", "--dry-run", "--json")
	var createdFolder folderMutationResult
	decodeOK(t, h.run("folders", "create", "Disposable", "--json"), &createdFolder)
	if createdFolder.CreatedFolder == nil || createdFolder.CreatedFolder.ID == "" {
		t.Fatal("folder create omitted identity")
	}
	// The shared folder fake is deliberately stateless; rename/delete probe an
	// existing fixture folder rather than pretending creation changed its tree.
	// Only an empty folder without subfolders may be deleted.
	assertErrorCode(t, h.run("folders", "delete", "clients-id", "--confirm", "clients-id", "--json"), errs.CodeProtectedFolder)
	folderID := "spare-id"
	ok("folders", "rename", folderID, "Renamed", "--json")
	assertErrorCode(t, h.run("folders", "delete", folderID, "--json"), errs.CodeConfirmationRequired)
	ok("folders", "delete", folderID, "--confirm", folderID, "--json")
	assertErrorCodeExit(t, h.run("folders", "view", "missing-folder", "--json"), errs.CodeFolderNotFound, 5)

	mutations := testkit.NewFakeMutationProvider(name, testkit.SampleMessages(), testkit.SampleFullMessages())
	mutations.Folders = testkit.SampleFolders()
	use(mutations)
	before := mutations.MessageMutationCalls.Load()
	// The folder changes above are in the action log already; a preview
	// must not add a line.
	logLines := len(readActionLog(t, h))
	ok("mark", "read", "--id", "work-new", "--id", "work-old", "--json")
	if mutations.MessageMutationCalls.Load() != before || len(readActionLog(t, h)) != logLines {
		t.Fatal("bulk preview wrote")
	}
	ok("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json")
	ok("mark", "unread", "--id", "work-new", "--json")
	ok("copy", "--internet-id", "<work-new@example.test>", "--to", "inbox", "--json")
	ok("move", "--query", "anything", "--latest", "--to", "archive", "--json")
	for _, args := range [][]string{
		{"flag", "set", "--id", "work-old", "--json"}, {"flag", "clear", "--id", "work-old", "--json"},
		{"archive", "--id", "work-old", "--json"}, {"delete", "--id", "work-old", "--json"},
		{"restore", "--id", "work-old", "--to", "inbox", "--json"}, {"junk", "--id", "work-old", "--json"},
	} {
		ok(args...)
	}
	assertErrorCodeExit(t, h.run("mark", "read", "--id", "personal-new", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)
	if name == "outlook" || name == "gmail" {
		ok("categories", "set", "--id", "work-new", "--category", "Client", "--json")
		ok("categories", "list", "--id", "work-new", "--json")
		ok("categories", "clear", "--id", "work-new", "--json")
	} else {
		assertErrorCode(t, h.run("categories", "list", "--id", "work-new", "--json"), errs.CodeUnsupportedCapability)
	}

	attachment := mail.Attachment{ID: "part-1", Name: "../report.txt", ContentType: "text/plain", Size: 7, Kind: mail.AttachmentKindFile}
	attachments := testkit.NewFakeMessageAttachmentProvider(name, testkit.SampleMessages(), testkit.SampleFullMessages(),
		map[string]map[string][]mail.Attachment{"work": {"work-new": {attachment}}},
		map[string]map[string]map[string]testkit.FakeAttachmentContent{"work": {"work-new": {"part-1": {Bytes: []byte("fixture"), ContentType: "text/plain"}}}})
	use(attachments)
	ok("attachments", "list", "--id", "work-new", "--plain")
	// A name that points out of the folder is saved under a safe name
	// inside it.
	destination := filepath.Join(h.root, "downloads")
	ok("attachments", "download", "--id", "work-new", "--all", "--to", destination, "--json")
	if _, err := os.Stat(filepath.Join(h.root, "report.txt")); err == nil {
		t.Fatal("an attachment was saved outside the download folder")
	}
	if err := os.Rename(filepath.Join(destination, "report.txt"), filepath.Join(h.root, "first-report.txt")); err != nil {
		t.Fatalf("the unsafe name was not saved as report.txt: %v", err)
	}
	attachments.MessageAttachmentsByAccount["work"]["work-new"][0].Name = "report.txt"
	ok("attachments", "download", "--id", "work-new", "--all", "--to", destination, "--dry-run", "--json")
	ok("attachments", "download", "--id", "work-new", "--all", "--to", destination, "--json")
	if data, err := os.ReadFile(filepath.Join(destination, "report.txt")); err != nil || string(data) != "fixture" {
		t.Fatalf("download bytes: %q, %v", data, err)
	}

	drafts := testkit.NewFakeForwardProvider(name)
	use(drafts)
	ok("send", "--yes", "--to", "client@example.test", "--body", "private compose body", "--dry-run", "--json")
	ok("send", "--yes", "--to", "client@example.test", "--bcc", "hidden@example.test", "--body", "private compose body", "--json")
	if name == "outlook" || name == "gmail" {
		var created draftWriteResult
		decodeOK(t, h.run("draft", "create", "--to", "client@example.test", "--body", "private compose body", "--json"), &created)
		ok("draft", "update", "--id", created.DraftID, "--subject", "Updated", "--json")
		ok("attachments", "add", "--draft-id", created.DraftID, filepath.Join(destination, "report.txt"), "--json")
		ok("attachments", "list", "--draft-id", created.DraftID, "--json")
		ok("attachments", "remove", "--draft-id", created.DraftID, "--name", "report.txt", "--json")
		ok("draft", "read", "--id", created.DraftID, "--plain")
		ok("draft", "send", "--yes", "--id", created.DraftID, "--json")
		ok("reply", "--id", "work-new", "--body", "private compose body", "--dry-run", "--json")
		ok("reply", "--id", "work-new", "--body", "private compose body", "--json")
		ok("forward", "--id", "work-new", "--to", "client@example.test", "--body", "private compose body", "--json")
	} else {
		for _, args := range [][]string{
			{"draft", "create", "--to", "client@example.test", "--body", "body", "--json"},
			{"reply", "--id", "work-new", "--body", "body", "--json"},
			{"forward", "--id", "work-new", "--to", "client@example.test", "--body", "body", "--json"},
		} {
			assertErrorCode(t, h.run(args...), errs.CodeUnsupportedCapability)
		}
	}

	rules := testkit.NewFakeRuleMutationProvider(name, testkit.SampleRules())
	if name != "outlook" {
		rules.RulesReadImplemented, rules.RulesMutateImplemented = false, false
	}
	use(rules)
	if name == "outlook" {
		ok("rules", "list", "--plain")
		var created ruleMutationResult
		decodeOK(t, h.run("rules", "create", "--subject-contains", "fixture", "--mark-read", "--apply", "--json"), &created)
		ok("rules", "view", created.RuleID, "--json")
		ok("rules", "update", created.RuleID, "--subject-contains", "changed", "--apply", "--json")
		ok("rules", "disable", created.RuleID, "--json")
		ok("rules", "enable", created.RuleID, "--json")
		assertErrorCode(t, h.run("rules", "delete", created.RuleID, "--json"), errs.CodeConfirmationRequired)
		ok("rules", "delete", created.RuleID, "--confirm", created.RuleID, "--json")
		assertErrorCodeExit(t, h.run("rules", "view", created.RuleID, "--json"), errs.CodeRuleNotFound, 5)
	} else {
		assertErrorCode(t, h.run("rules", "list", "--json"), errs.CodeUnsupportedCapability)
	}
	ok("auth", "refresh", "work", "--json")
	ok("auth", "switch", "personal", "--json")
	ok("auth", "rename", "work", "renamed", "--json")
	ok("auth", "remove", "renamed", "--json")
	if h.browserCalls.Load() != 0 {
		t.Fatal("ordinary command opened a browser")
	}
	if data, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath)); err != nil || strings.Contains(string(data), "private compose body") || strings.Contains(string(data), "seed-access-token") {
		t.Fatalf("action log missing or unsafe: %v", err)
	}
}
