package main

import (
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestAdministrationCommandsRejectExplicitEmptyAccount(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		fake func() *testkit.FakeProvider
		run  func(*testing.T, *testkit.FakeProvider) *cliHarness
	}{
		{
			name: "folder read",
			args: []string{"folders", "list", "--account=", "--json"},
			fake: func() *testkit.FakeProvider { return testkit.NewFakeFolderProvider("fake", testkit.SampleFolders()) },
			run: func(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
				return seededFoldersHarnessWithProvider(t, fake, "work")
			},
		},
		{
			name: "folder mutation",
			args: []string{"folders", "create", "Clients", "--account", "", "--json"},
			fake: func() *testkit.FakeProvider {
				return testkit.NewFakeFolderMutationProvider("fake", testkit.SampleFolders())
			},
			run: func(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
				return seededFoldersHarnessWithProvider(t, fake, "work")
			},
		},
		{
			name: "rule read",
			args: []string{"rules", "list", "--account=", "--json"},
			fake: func() *testkit.FakeProvider { return testkit.NewFakeRuleProvider("fake", testkit.SampleRules()) },
			run: func(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
				return seededRulesHarnessWithProvider(t, fake, "work")
			},
		},
		{
			name: "rule mutation",
			args: []string{"rules", "enable", "rule-1", "--account", "", "--json"},
			fake: func() *testkit.FakeProvider {
				return testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			},
			run: func(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
				return seededRulesHarnessWithProvider(t, fake, "work")
			},
		},
		{
			name: "archive report",
			args: []string{"outlook", "archive-policy", "check", "--account=", "--json"},
			fake: func() *testkit.FakeProvider {
				return testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
			},
			run: func(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
				return seededArchivePolicyHarness(t, fake, "work", "work")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := tt.fake()
			h := tt.run(t, fake)
			res := h.run(tt.args...)
			assertErrorCode(t, res, errs.CodeInvalidAccountScope)
			if res.Stdout != "" {
				t.Fatalf("stdout = %q, want empty", res.Stdout)
			}
			if fake.CapabilityCalls.Load() != 0 || fake.AccountClientCalls.Load() != 0 ||
				fake.FolderListCalls.Load() != 0 || fake.FolderCreateCalls.Load() != 0 ||
				fake.RuleListCalls.Load() != 0 || fake.RuleGetCalls.Load() != 0 ||
				fake.RuleCreateCalls.Load() != 0 || fake.RuleUpdateCalls.Load() != 0 || fake.RuleDeleteCalls.Load() != 0 ||
				fake.ArchiveReportCalls.Load() != 0 {
				t.Fatalf("provider work occurred: capability=%d client=%d folders=%d/%d rules=%d/%d/%d/%d/%d archive=%d",
					fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.FolderListCalls.Load(), fake.FolderCreateCalls.Load(),
					fake.RuleListCalls.Load(), fake.RuleGetCalls.Load(), fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load(), fake.RuleDeleteCalls.Load(),
					fake.ArchiveReportCalls.Load())
			}
		})
	}
}
