package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
)

func TestAccountClientSelectedAccountAttribution(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T) (*cliHarness, *testkit.FakeProvider)
		args  func(t *testing.T, h *cliHarness) []string
		want  []string
	}{
		{
			name: "search",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeMailboxProvider("fake", testkit.SampleMessages())
				return seededMessagesHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("search", "anything", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "list",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeMailboxProvider("fake", testkit.SampleMessages())
				return seededMessagesHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("list", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "read query",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads())
				return seededReadHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("read", "--query", "anything", "--first", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "thread query",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads())
				return seededReadHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("thread", "--query", "anything", "--first", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "folders view",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
				return seededFoldersHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("folders", "view", "inbox", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "folders delete",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
				return seededFoldersHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("folders", "delete", "spare-id", "--confirm", "spare-id", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "message attachments",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				return seededMessageAttachmentHarness(t)
			},
			args: fixedAttributionArgs("attachments", "list", "--id", "personal-new", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "draft attachment add",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				h, fake := seededAttachmentHarness(t)
				seedPersonalAccount(t, h)
				return h, fake
			},
			args: func(t *testing.T, h *cliHarness) []string {
				path := filepath.Join(t.TempDir(), "upload.txt")
				if err := os.WriteFile(path, []byte("upload"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"attachments", "add", path, "--draft-id", "seed-draft", "--account", "personal", "--dry-run", "--json"}
			},
			want: []string{"personal"},
		},
		{
			name: "draft attachment remove",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				h, fake := seededAttachmentHarness(t)
				seedPersonalAccount(t, h)
				return h, fake
			},
			args: fixedAttributionArgs("attachments", "remove", "--draft-id", "seed-draft", "--name", "contract.pdf", "--account", "personal", "--dry-run", "--json"),
			want: []string{"personal"},
		},
		{
			name:  "draft create",
			setup: attributionDraftHarness,
			args:  fixedAttributionArgs("draft", "create", "--to", "client@example.test", "--subject", "Subject", "--body", "Body", "--account", "personal", "--json"),
			want:  []string{"personal"},
		},
		{
			name:  "draft read",
			setup: attributionDraftHarness,
			args:  fixedAttributionArgs("draft", "read", "--id", "seed-draft", "--account", "personal", "--json"),
			want:  []string{"personal"},
		},
		{
			name:  "draft update",
			setup: attributionDraftHarness,
			args:  fixedAttributionArgs("draft", "update", "--id", "seed-draft", "--subject", "Updated", "--account", "personal", "--json"),
			want:  []string{"personal"},
		},
		{
			name:  "draft send",
			setup: attributionDraftHarness,
			args:  fixedAttributionArgs("draft", "send", "--yes", "--id", "seed-draft", "--account", "personal", "--json"),
			// The preview reads the draft and the send reads it again.
			want: []string{"personal", "personal"},
		},
		{
			name:  "send",
			setup: attributionDraftHarness,
			args:  fixedAttributionArgs("send", "--yes", "--to", "client@example.test", "--subject", "Subject", "--body", "Body", "--account", "personal", "--json"),
			want:  []string{"personal"},
		},
		{
			name: "reply",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				h, fake := seededReplyHarness(t)
				seedPersonalAccount(t, h)
				return h, fake
			},
			args: fixedAttributionArgs("reply", "--id", "src1", "--body", "Body", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "forward",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				h, fake := seededForwardHarness(t)
				seedPersonalAccount(t, h)
				return h, fake
			},
			args: fixedAttributionArgs("forward", "--id", "src1", "--to", "client@example.test", "--body", "Body", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "categories list",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				return seededMutationHarness(t)
			},
			args: fixedAttributionArgs("categories", "list", "--id", "personal-new", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "move",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				return seededMutationHarness(t)
			},
			args: fixedAttributionArgs("move", "--id", "personal-new", "--to", "Archive", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "rules list",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
				return seededRulesHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("rules", "list", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "rules mutation",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
				return seededRulesHarnessWithProvider(t, fake, "work"), fake
			},
			args: fixedAttributionArgs("rules", "disable", "rule-personal-news", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
		{
			name: "archive policy",
			setup: func(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
				fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{
					"personal": resolvedArchivePolicyReport(),
				})
				return seededArchivePolicyHarness(t, fake, "work", "work", "personal"), fake
			},
			args: fixedAttributionArgs("outlook", "archive-policy", "check", "--account", "personal", "--json"),
			want: []string{"personal"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fake := tt.setup(t)
			before := fake.AccountClientAccounts()
			args := tt.args(t, h)
			run := h.run
			if slices.Contains(args, "--yes") {
				run = func(a ...string) testutil.Result { return approvedRun(t, h.run, a...) }
			}
			decodeOK(t, run(args...), nil)
			assertAccountClientDelta(t, fake, before, tt.want)
		})
	}
}

func fixedAttributionArgs(args ...string) func(*testing.T, *cliHarness) []string {
	return func(*testing.T, *cliHarness) []string { return append([]string(nil), args...) }
}

func attributionDraftHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	h, fake := seededDraftHarness(t)
	seedPersonalAccount(t, h)
	return h, fake
}

func seedPersonalAccount(t *testing.T, h *cliHarness) {
	t.Helper()
	account := workAccount()
	account.Name = "personal"
	account.Email = "personal@example.test"
	account.DisplayName = "Personal"
	account.ProviderAccountID = "fake-personal"
	account.AuthRef = "fake/personal"
	account.DefaultFrom = "personal@example.test"
	seedAccount(t, h, account, true)
}

func assertAccountClientDelta(t *testing.T, fake *testkit.FakeProvider, before, want []string) {
	t.Helper()
	after := fake.AccountClientAccounts()
	if len(after) < len(before) {
		t.Fatalf("client account log shrank: before=%#v after=%#v", before, after)
	}
	got := append([]string(nil), after[len(before):]...)
	if len(got) == 0 {
		t.Fatalf("no account client was constructed; full log=%#v", after)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("client account delta = %#v, want %#v; full log=%#v", got, want, after)
	}
}
