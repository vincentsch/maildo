package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	runmanifest "github.com/vincentsch/rungrad/manifest"
	"github.com/vincentsch/rungrad/testutil"
)

func TestRuleMutationValidationOrder(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		usage  bool
		field  string
		reason string
	}{
		{name: "dry run apply before false presence", args: []string{"rules", "create", "--dry-run", "--apply", "--mark-read=false", "--json"}, usage: true},
		{name: "false presence before content", args: []string{"rules", "create", "--mark-read=false", "--json"}, usage: true},
		{name: "boolean conflict before content", args: []string{"rules", "update", "rule-1", "--mark-read", "--no-mark-read", "--json"}, usage: true},
		{name: "parse before content", args: []string{"rules", "create", "--from", "not an address", "--json"}, field: "from", reason: "malformed_address"},
		{name: "content before blank update id", args: []string{"rules", "update", " ", "--json"}, field: "changes", reason: "no_changes"},
		{name: "blank update id after valid content", args: []string{"rules", "update", " ", "--subject-contains", "Receipt", "--json"}, field: "rule_id", reason: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run(tt.args...)
			if tt.usage {
				assertFrameworkUsage(t, res)
			} else {
				assertRuleInputReason(t, res, tt.field, tt.reason)
			}
			assertNoRuleProviderWork(t, fake)
		})
	}
}

func TestRuleMutationRejectsBlankRuleIDsBeforeProviderWork(t *testing.T) {
	for _, command := range []struct {
		name string
		args []string
	}{
		{name: "update", args: []string{"rules", "update", " \t ", "--subject-contains", "Receipt", "--json"}},
		{name: "enable", args: []string{"rules", "enable", " \t ", "--json"}},
		{name: "disable", args: []string{"rules", "disable", " \t ", "--json"}},
		{name: "delete", args: []string{"rules", "delete", " \t ", "--json"}},
	} {
		t.Run(command.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			h := seededRulesHarnessWithProvider(t, fake, "work")
			assertRuleInputReason(t, h.run(command.args...), "rule_id", "empty")
			assertNoRuleProviderWork(t, fake)
		})
	}
}

func TestRuleMutationUsesOneCapabilitySnapshotAndClient(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	decodeOK(t, h.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json"), nil)
	if fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 1 {
		t.Fatalf("capabilities=%d clients=%d, want 1/1", fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load())
	}
}

func TestRuleMutationRendererValidationPrecedesProviderWork(t *testing.T) {
	operations := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
	}{
		{name: "create", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply"}},
		{name: "update", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply"}},
		{name: "enable", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1"}},
		{name: "disable", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1"}},
		{name: "delete", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1"}},
	}
	renderers := []struct {
		name string
		args []string
	}{
		{name: "invalid jq", args: []string{"--jq", ".broken["}},
		{name: "invalid template", args: []string{"--template", "{{"}},
	}

	for _, operation := range operations {
		for _, renderer := range renderers {
			t.Run(operation.name+"/"+renderer.name, func(t *testing.T) {
				fake := testkit.NewFakeRuleMutationProvider("fake", operation.rules())
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run(append(append([]string(nil), operation.args...), renderer.args...)...)
				assertFrameworkUsage(t, res)
				if res.Stdout != "" {
					t.Fatalf("renderer error wrote stdout %q", res.Stdout)
				}
				assertNoRuleProviderWork(t, fake)
			})
		}
	}

	for _, args := range [][]string{
		{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--jq", ".status", "--template", "{{.status}}"},
		{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--plain"},
		{"rules", "enable", "rule-1", "--dry-run", "--include-meta", "--json"},
	} {
		fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
		h := seededRulesHarnessWithProvider(t, fake, "work")
		assertFrameworkUsage(t, h.run(args...))
		assertNoRuleProviderWork(t, fake)
	}
}

func TestRuleMutationValidTransformsWriteOnce(t *testing.T) {
	operations := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
	}{
		{name: "create", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply"}},
		{name: "update", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply"}},
		{name: "enable", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1"}},
		{name: "disable", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1"}},
		{name: "delete", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1"}},
	}
	renderers := []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{name: "jq null", args: []string{"--jq", "null"}, wantStdout: "null\n"},
		{name: "jq false", args: []string{"--jq", "false"}, wantStdout: "false\n"},
		{name: "jq empty string", args: []string{"--jq", `""`}, wantStdout: "\n"},
		{name: "jq no rows", args: []string{"--jq", "empty"}},
		{name: "template empty", args: []string{"--template", "{{if false}}unreachable{{end}}"}, wantStdout: "\n"},
	}

	for _, operation := range operations {
		for _, renderer := range renderers {
			t.Run(operation.name+"/"+renderer.name, func(t *testing.T) {
				fake := testkit.NewFakeRuleMutationProvider("fake", operation.rules())
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run(append(append([]string(nil), operation.args...), renderer.args...)...)
				if res.Exit != rungrad.ExitSuccess || res.Stdout != renderer.wantStdout || res.Stderr != "" {
					t.Fatalf("valid transform result = %#v", res)
				}
				writes := fake.RuleCreateCalls.Load() + fake.RuleUpdateCalls.Load() + fake.RuleDeleteCalls.Load()
				if writes != 1 || fake.RuleGetCalls.Load() != 1 {
					t.Fatalf("valid transform calls writes=%d get=%d, want 1/1", writes, fake.RuleGetCalls.Load())
				}
			})
		}
	}
}

func TestRuleMutationGlobalOutputControls(t *testing.T) {
	tests := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
	}{
		{name: "create preview", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read"}},
		{name: "create applied", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply"}},
		{name: "update preview", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt"}},
		{name: "update applied", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply"}},
		{name: "enable preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1", "--dry-run"}},
		{name: "enable applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1"}},
		{name: "disable preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1", "--dry-run"}},
		{name: "disable applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1"}},
		{name: "delete preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--dry-run"}},
		{name: "delete applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1"}},
	}
	outputs := []struct {
		name string
		args []string
	}{
		{name: "human"},
		{name: "json", args: []string{"--json"}},
	}
	flags := []string{"--no-color", "--no-ansi", "--no-pager", "--quiet", "--no-prompt"}

	for _, tt := range tests {
		for _, output := range outputs {
			for _, flag := range flags {
				t.Run(tt.name+"/"+output.name+"/"+strings.TrimPrefix(flag, "--"), func(t *testing.T) {
					baselineFake := testkit.NewFakeRuleMutationProvider("fake", tt.rules())
					baselineHarness := seededRulesHarnessWithProvider(t, baselineFake, "work")
					baseline := baselineHarness.run(append(append([]string(nil), tt.args...), output.args...)...)
					decodeOK(t, baseline, nil)
					baselineCounters := snapshotRuleMutationCounters(baselineFake)

					flaggedFake := testkit.NewFakeRuleMutationProvider("fake", tt.rules())
					flaggedHarness := seededRulesHarnessWithProvider(t, flaggedFake, "work")
					var pagerCalls int
					stdin := &failOnRead{}
					opts := testutil.Options{}
					switch flag {
					case "--no-color", "--no-ansi":
						opts.OutputTerminalSet = true
						opts.OutputTerminal = true
					case "--no-pager":
						opts.OutputTerminalSet = true
						opts.OutputTerminal = true
						opts.TerminalHeight = func() (int, bool) { return 1, true }
						opts.Pager = rungrad.PagerFunc(func(_ []string, _ io.Reader, _, _ io.Writer) error {
							pagerCalls++
							return nil
						})
					case "--no-prompt":
						opts.Stdin = stdin
						opts.TerminalSet = true
						opts.Terminal = true
					}
					args := append(append([]string(nil), tt.args...), output.args...)
					if flag == "--no-pager" {
						args = append(args, "--no-color", flag)
					} else {
						args = append(args, flag)
					}
					got := flaggedHarness.runWith(opts, args...)
					decodeOK(t, got, nil)
					// Printed commands repeat each harness's own --config.
					baseline.Stdout = strings.ReplaceAll(baseline.Stdout, baselineHarness.configPath, "<config>")
					got.Stdout = strings.ReplaceAll(got.Stdout, flaggedHarness.configPath, "<config>")
					if got != baseline {
						t.Fatalf("%s changed output\nbaseline=%#v\ngot=%#v", flag, baseline, got)
					}
					if counters := snapshotRuleMutationCounters(flaggedFake); counters != baselineCounters {
						t.Fatalf("%s changed provider calls: baseline=%#v got=%#v", flag, baselineCounters, counters)
					}
					if pagerCalls != 0 || stdin.reads.Load() != 0 {
						t.Fatalf("%s used pager/stdin: pager=%d reads=%d", flag, pagerCalls, stdin.reads.Load())
					}
					if output.name == "human" {
						assertNoTerminalControls(t, got.Stdout)
					}
				})
			}
		}
	}
}

func TestCreatedRuleSupportsCompleteMutationLifecycle(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")

	var initialFolders folderListResult
	decodeOK(t, h.run("folders", "list", "--json"), &initialFolders)
	if len(initialFolders.Folders) == 0 {
		t.Fatal("initial folder list was empty")
	}
	var initialRules ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &initialRules)
	if initialRules.Count != 2 {
		t.Fatalf("initial rule count = %d, want 2", initialRules.Count)
	}

	createArgs := []string{
		"rules", "create", "--name", "Vendor receipts", "--sequence", "30",
		"--sender-contains", "legacy-vendor", "--subject-contains", "Receipt",
		"--has-attachments", "--move-to", "Archive", "--mark-read", "--stop-processing",
	}
	createWrites := fake.RuleCreateCalls.Load()
	decodePreview(t, h.run(append(append([]string(nil), createArgs...), "--json")...))
	if fake.RuleCreateCalls.Load() != createWrites || fake.RuleGetCalls.Load() != 0 {
		t.Fatalf("create preview wrote/read directly: create=%d get=%d", fake.RuleCreateCalls.Load(), fake.RuleGetCalls.Load())
	}
	var created ruleMutationResult
	decodeOK(t, h.run(append(append([]string(nil), createArgs...), "--apply", "--json")...), &created)
	if created.RuleID == "" || created.Verification == nil || created.Verification.Status != mail.RuleVerificationVerified || fake.RuleCreateCalls.Load() != createWrites+1 {
		t.Fatalf("created=%#v create calls=%d", created, fake.RuleCreateCalls.Load())
	}

	var createdView ruleViewResult
	decodeOK(t, h.run("rules", "view", created.RuleID, "--json"), &createdView)
	if createdView.Rule.ID != created.RuleID || createdView.Rule.Actions.MoveToFolder == nil || createdView.Rule.Actions.MoveToFolder.Path != "Archive" {
		t.Fatalf("created view=%#v", createdView)
	}

	updateArgs := []string{
		"rules", "update", created.RuleID, "--name", "Current vendor receipts", "--sequence", "31",
		"--sender-contains", "current-vendor", "--no-has-attachments", "--no-mark-read", "--no-stop-processing",
	}
	updateWrites := fake.RuleUpdateCalls.Load()
	decodePreview(t, h.run(append(append([]string(nil), updateArgs...), "--json")...))
	if fake.RuleUpdateCalls.Load() != updateWrites {
		t.Fatalf("update preview wrote: before=%d after=%d", updateWrites, fake.RuleUpdateCalls.Load())
	}
	var updated ruleMutationResult
	decodeOK(t, h.run(append(append([]string(nil), updateArgs...), "--apply", "--json")...), &updated)
	if updated.RuleID != created.RuleID || updated.Verification == nil || updated.Verification.Status != mail.RuleVerificationVerified || fake.RuleUpdateCalls.Load() != updateWrites+1 {
		t.Fatalf("updated=%#v update calls=%d", updated, fake.RuleUpdateCalls.Load())
	}
	var updatedView ruleViewResult
	decodeOK(t, h.run("rules", "view", created.RuleID, "--json"), &updatedView)
	rule := updatedView.Rule
	if rule.DisplayName != "Current vendor receipts" || rule.Sequence != 31 || !reflect.DeepEqual(rule.Conditions.SenderContains, []string{"current-vendor"}) ||
		!reflect.DeepEqual(rule.Conditions.SubjectContains, []string{"Receipt"}) || rule.Conditions.HasAttachments != nil ||
		rule.Actions.MoveToFolder == nil || rule.Actions.MoveToFolder.Path != "Archive" || rule.Actions.MarkAsRead != nil || rule.Actions.StopProcessingRules {
		t.Fatalf("updated rule state=%#v", rule)
	}

	beforeDisable := fake.RuleUpdateCalls.Load()
	decodePreview(t, h.run("rules", "disable", created.RuleID, "--dry-run", "--json"))
	if fake.RuleUpdateCalls.Load() != beforeDisable {
		t.Fatal("disable preview wrote")
	}
	var disabled ruleMutationResult
	decodeOK(t, h.run("rules", "disable", created.RuleID, "--json"), &disabled)
	if disabled.RuleID != created.RuleID || fake.RuleUpdateCalls.Load() != beforeDisable+1 {
		t.Fatalf("disabled=%#v update calls=%d", disabled, fake.RuleUpdateCalls.Load())
	}
	getBeforeUnchanged := fake.RuleGetCalls.Load()
	var disableUnchanged ruleMutationResult
	decodeOK(t, h.run("rules", "disable", created.RuleID, "--json"), &disableUnchanged)
	if disableUnchanged.Status != mail.RuleMutationStatusUnchanged || fake.RuleUpdateCalls.Load() != beforeDisable+1 || fake.RuleGetCalls.Load() != getBeforeUnchanged {
		t.Fatalf("unchanged disable=%#v update/get=%d/%d", disableUnchanged, fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
	}

	beforeEnable := fake.RuleUpdateCalls.Load()
	decodePreview(t, h.run("rules", "enable", created.RuleID, "--dry-run", "--json"))
	if fake.RuleUpdateCalls.Load() != beforeEnable {
		t.Fatal("enable preview wrote")
	}
	var enabled ruleMutationResult
	decodeOK(t, h.run("rules", "enable", created.RuleID, "--json"), &enabled)
	if enabled.RuleID != created.RuleID || fake.RuleUpdateCalls.Load() != beforeEnable+1 {
		t.Fatalf("enabled=%#v update calls=%d", enabled, fake.RuleUpdateCalls.Load())
	}
	getBeforeUnchanged = fake.RuleGetCalls.Load()
	var enableUnchanged ruleMutationResult
	decodeOK(t, h.run("rules", "enable", created.RuleID, "--json"), &enableUnchanged)
	if enableUnchanged.Status != mail.RuleMutationStatusUnchanged || fake.RuleUpdateCalls.Load() != beforeEnable+1 || fake.RuleGetCalls.Load() != getBeforeUnchanged {
		t.Fatalf("unchanged enable=%#v update/get=%d/%d", enableUnchanged, fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
	}

	deleteWrites := fake.RuleDeleteCalls.Load()
	decodePreview(t, h.run("rules", "delete", created.RuleID, "--dry-run", "--json"))
	missing := h.run("rules", "delete", created.RuleID, "--json")
	assertErrorCode(t, missing, errs.CodeConfirmationRequired)
	assertRuleConfirmationDoesNotLeak(t, missing, created.RuleID)
	wrong := h.run("rules", "delete", created.RuleID, "--confirm", "wrong", "--json")
	assertErrorCode(t, wrong, errs.CodeConfirmationRequired)
	assertRuleConfirmationDoesNotLeak(t, wrong, created.RuleID)
	if fake.RuleDeleteCalls.Load() != deleteWrites {
		t.Fatalf("delete guard wrote: before=%d after=%d", deleteWrites, fake.RuleDeleteCalls.Load())
	}
	var deleted ruleMutationResult
	decodeOK(t, h.run("rules", "delete", created.RuleID, "--confirm", created.RuleID, "--json"), &deleted)
	if deleted.RuleID != created.RuleID || deleted.Verification == nil || deleted.Verification.Status != mail.RuleVerificationVerified || fake.RuleDeleteCalls.Load() != deleteWrites+1 {
		t.Fatalf("deleted=%#v delete calls=%d", deleted, fake.RuleDeleteCalls.Load())
	}
	assertErrorCodeExit(t, h.run("rules", "view", created.RuleID, "--json"), errs.CodeRuleNotFound, 5)
	var ops []string
	for _, entry := range readActionLog(t, h) {
		ops = append(ops, string(entry.Operation))
	}
	if strings.Join(ops, " ") != "rule_create rule_update rule_disable rule_enable rule_delete" {
		t.Fatalf("complete lifecycle logged %v", ops)
	}
}

func TestRuleMutationsLeaveLocalFilesUnchanged(t *testing.T) {
	tests := []struct {
		name      string
		rules     func() map[string][]mail.Rule
		configure func(*testkit.FakeProvider)
		args      []string
		wantError bool
	}{
		{name: "create preview", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json"}},
		{name: "create applied", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"}},
		{name: "update preview", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "update applied", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "enable applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "enable unchanged", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "disable applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1", "--json"}},
		{name: "disable unchanged", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "disable", "rule-1", "--json"}},
		{name: "delete preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--dry-run", "--json"}},
		{name: "delete applied", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1", "--json"}},
		{name: "local validation failure", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--json"}, wantError: true},
		{name: "provider failure", rules: mutationUpdateRules, configure: func(fake *testkit.FakeProvider) {
			fake.RuleMutationErrByOperation = map[mail.RuleMutationOperation]error{mail.RuleOperationUpdate: errors.New("fixture write failure")}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", tt.rules())
			if tt.configure != nil {
				tt.configure(fake)
			}
			h := seededRulesHarnessWithProvider(t, fake, "work")
			before := snapshotRuleTestFiles(t, h.root)
			res := h.run(tt.args...)
			if tt.wantError {
				if res.Exit == rungrad.ExitSuccess || res.Stdout != "" {
					t.Fatalf("failure result = %#v", res)
				}
			} else {
				decodeOK(t, res, nil)
			}
			after := snapshotRuleTestFiles(t, h.root)
			// Changes that reach the provider add to the action log; nothing
			// else on disk may change.
			_, logged := after["audit/actions.jsonl"]
			wantLog := strings.HasSuffix(tt.name, "applied") || tt.name == "provider failure"
			if logged != wantLog {
				t.Fatalf("action log written = %t, want %t", logged, wantLog)
			}
			for _, name := range []string{".", "audit", "audit/actions.jsonl"} {
				delete(before, name)
				delete(after, name)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("local files changed\nbefore=%#v\nafter =%#v", before, after)
			}
		})
	}
}

func TestRuleMutationRejectsMismatchedClientIdentityBeforeProviderReads(t *testing.T) {
	for _, tt := range []struct {
		name         string
		account      string
		providerName string
	}{
		{name: "blank account", account: "", providerName: "fake"},
		{name: "different account", account: "personal-client-identity", providerName: "fake"},
		{name: "blank provider", account: "work", providerName: ""},
		{name: "different provider", account: "work", providerName: "other-provider-identity"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			wrapped := &reportedRuleMutationClientProvider{
				FakeProvider: fake,
				account:      tt.account,
				providerName: tt.providerName,
			}
			h := newFakeHarness(t, fake)
			h.providers = testkit.NewProviderRegistry(wrapped)
			accounts := []maildoconfig.Account{folderWorkAccount(), folderPersonalAccount()}
			seedRegistry(t, h.configPath, "work", accounts)
			for _, account := range accounts {
				if err := testkit.SeedCredential(h.root, account, nil); err != nil {
					t.Fatalf("seed credential %s: %v", account.Name, err)
				}
			}

			res := h.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json")
			if res.Exit != rungrad.ExitAPI || res.Stdout != "" {
				t.Fatalf("mismatched client result = %#v", res)
			}
			if strings.Contains(res.Stderr, tt.account) && tt.account != "" && tt.account != "work" {
				t.Fatalf("error exposed mismatched account identity: %q", res.Stderr)
			}
			if strings.Contains(res.Stderr, tt.providerName) && tt.providerName != "" && tt.providerName != "fake" {
				t.Fatalf("error exposed mismatched provider identity: %q", res.Stderr)
			}
			if fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 1 || fake.FolderListCalls.Load() != 0 || fake.RuleListCalls.Load() != 0 ||
				fake.RuleGetCalls.Load()+fake.RuleCreateCalls.Load()+fake.RuleUpdateCalls.Load()+fake.RuleDeleteCalls.Load() != 0 {
				t.Fatalf("mismatched client reached provider reads/writes caps=%d clients=%d folders=%d list=%d get=%d create=%d update=%d delete=%d",
					fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.FolderListCalls.Load(), fake.RuleListCalls.Load(), fake.RuleGetCalls.Load(),
					fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load(), fake.RuleDeleteCalls.Load())
			}
		})
	}

	fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	decodeOK(t, h.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json"), nil)
	if fake.FolderListCalls.Load() != 1 || fake.RuleListCalls.Load() != 1 {
		t.Fatalf("matching client reads folders=%d rules=%d, want 1/1", fake.FolderListCalls.Load(), fake.RuleListCalls.Load())
	}
}

func TestRuleMutationSelectsOnlyRequestedAccount(t *testing.T) {
	rules := mutationScalarRules(false, false)
	personalRule := rules["work"][0]
	personalRule.DisplayName = "Personal invoice filing"
	rules["personal"] = []mail.Rule{personalRule}

	for _, tt := range []struct {
		name        string
		args        []string
		wantAccount string
	}{
		{name: "active account fallback", args: []string{"rules", "enable", "rule-1", "--json"}, wantAccount: "work"},
		{name: "explicit account override", args: []string{"rules", "enable", "rule-1", "--account", "personal", "--json"}, wantAccount: "personal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			var got ruleMutationResult
			decodeOK(t, h.run(tt.args...), &got)
			assertSelection(t, got.AccountSelection, "single", tt.wantAccount)
			if got.Account != tt.wantAccount || got.Provider != "fake" {
				t.Fatalf("selected result account/provider = %q/%q", got.Account, got.Provider)
			}
			wantLog := []string{"rule_enable:" + tt.wantAccount + ":rule-1"}
			if !sameStrings(fake.RuleMutationCallLog, wantLog) || fake.RuleUpdateCalls.Load() != 1 || fake.RuleGetCalls.Load() != 1 {
				t.Fatalf("selected mutation log=%#v update/get=%d/%d", fake.RuleMutationCallLog, fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
			}
		})
	}

	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "repeated account", args: []string{"rules", "enable", "rule-1", "--account", "work", "--account", "work", "--json"}},
		{name: "all accounts", args: []string{"rules", "enable", "rule-1", "--all-accounts", "--json"}},
		{name: "unknown account", args: []string{"rules", "enable", "rule-1", "--account", "missing", "--json"}},
		{name: "empty account create", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--account", "", "--json"}},
		{name: "empty account update", args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--account", "", "--json"}},
		{name: "empty account enable", args: []string{"rules", "enable", "rule-1", "--account", "", "--json"}},
		{name: "empty account disable", args: []string{"rules", "disable", "rule-1", "--account", "", "--json"}},
		{name: "empty account delete", args: []string{"rules", "delete", "rule-1", "--account", "", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run(tt.args...)
			if res.Exit == rungrad.ExitSuccess || res.Stdout != "" {
				t.Fatalf("invalid selection result = %#v", res)
			}
			assertNoRuleProviderWork(t, fake)
		})
	}
}

func TestRuleCreateNeverUpdatesByDisplayName(t *testing.T) {
	rules := mutationUpdateRules()
	rules["work"][0].DisplayName = "Duplicate display name"
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")

	preview := decodePreview(t, h.run("rules", "create", "--name", "Duplicate display name", "--subject-contains", "Receipt", "--mark-read", "--json"))
	body := preview["body"].(map[string]any)
	if body["display_name"] != "Duplicate display name" || fake.RuleCreateCalls.Load() != 0 || fake.RuleUpdateCalls.Load() != 0 {
		t.Fatalf("duplicate-name preview=%#v create/update=%d/%d", body, fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load())
	}

	var created ruleMutationResult
	decodeOK(t, h.run("rules", "create", "--name", "Duplicate display name", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"), &created)
	if created.RuleID == "" || created.RuleID == "rule-1" || fake.RuleCreateCalls.Load() != 1 || fake.RuleUpdateCalls.Load() != 0 {
		t.Fatalf("duplicate-name create=%#v create/update=%d/%d", created, fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load())
	}

	var listed ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &listed)
	matchingIDs := []string{}
	for _, rule := range listed.Rules {
		if rule.DisplayName == "Duplicate display name" {
			matchingIDs = append(matchingIDs, rule.ID)
		}
	}
	sort.Strings(matchingIDs)
	wantIDs := []string{created.RuleID, "rule-1"}
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(matchingIDs, wantIDs) {
		t.Fatalf("duplicate display-name ids=%#v, want %#v", matchingIDs, wantIDs)
	}
}

func TestRuleMutationRejectsDuplicateTargetRows(t *testing.T) {
	acct := provider.ResolvedAccount{Name: "work", Provider: "fake", Email: "work@example.test"}
	rules := mutationUpdateRules()["work"]
	duplicate := rules[0]
	duplicate.DisplayName = "Conflicting duplicate"
	duplicate.Sequence = 99

	for _, rows := range [][]mail.Rule{
		{rules[0], duplicate},
		{duplicate, rules[0]},
	} {
		got, err := ruleFromSnapshot(acct, "rule-1", rows, nil)
		if err == nil || got.ID != "" {
			t.Fatalf("duplicate target returned %#v, err=%v", got, err)
		}
		var exitErr interface{ ExitCode() int }
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != rungrad.ExitAPI || strings.Contains(err.Error(), "rule-1") || strings.Contains(err.Error(), "Conflicting duplicate") {
			t.Fatalf("duplicate target error=%#v", err)
		}
	}

	unrelated := rules[0]
	unrelated.ID = "unrelated-rule"
	unrelated.DisplayName = "Unrelated duplicate"
	got, err := ruleFromSnapshot(acct, "rule-1", []mail.Rule{rules[0], unrelated, unrelated}, nil)
	if err != nil || got.ID != "rule-1" {
		t.Fatalf("unrelated duplicate affected target: got=%#v err=%v", got, err)
	}
}

type reportedRuleMutationClientProvider struct {
	*testkit.FakeProvider
	account      string
	providerName string
}

func (p *reportedRuleMutationClientProvider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	client, err := p.FakeProvider.NewAccountClient(ctx, ac)
	if err != nil {
		return nil, err
	}
	return reportedRuleMutationClient{
		AccountClient: client,
		RuleManager:   client.(provider.RuleManager),
		RuleReader:    client.(provider.RuleReader),
		FolderReader:  client.(provider.FolderReader),
		account:       p.account,
		providerName:  p.providerName,
	}, nil
}

type reportedRuleMutationClient struct {
	provider.AccountClient
	provider.RuleManager
	provider.RuleReader
	provider.FolderReader
	account      string
	providerName string
}

func (c reportedRuleMutationClient) AccountName() string  { return c.account }
func (c reportedRuleMutationClient) ProviderName() string { return c.providerName }

func TestRuleMutationUsesSingleProviderSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		rules      map[string][]mail.Rule
		args       []string
		wantGet    int64
		wantCreate int64
		wantUpdate int64
		wantDelete int64
	}{
		{name: "create preview", rules: mutationUpdateRules(), args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json"}},
		{name: "create applied", rules: mutationUpdateRules(), args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"}, wantGet: 1, wantCreate: 1},
		{name: "update preview", rules: mutationUpdateRules(), args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "update applied", rules: mutationUpdateRules(), args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}, wantGet: 1, wantUpdate: 1},
		{name: "scalar applied", rules: mutationScalarRules(false, false), args: []string{"rules", "enable", "rule-1", "--json"}, wantGet: 1, wantUpdate: 1},
		{name: "scalar unchanged", rules: mutationScalarRules(true, false), args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "delete preview", rules: mutationScalarRules(true, false), args: []string{"rules", "delete", "rule-1", "--dry-run", "--json"}},
		{name: "delete applied", rules: mutationScalarRules(true, false), args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1", "--json"}, wantGet: 1, wantDelete: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", tt.rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			decodeOK(t, h.run(tt.args...), nil)
			if fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 1 || fake.FolderListCalls.Load() != 1 || fake.RuleListCalls.Load() != 1 ||
				fake.RuleGetCalls.Load() != tt.wantGet || fake.RuleCreateCalls.Load() != tt.wantCreate || fake.RuleUpdateCalls.Load() != tt.wantUpdate || fake.RuleDeleteCalls.Load() != tt.wantDelete {
				t.Fatalf("counters caps=%d clients=%d folders=%d list=%d get=%d create=%d update=%d delete=%d",
					fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.FolderListCalls.Load(), fake.RuleListCalls.Load(), fake.RuleGetCalls.Load(),
					fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load(), fake.RuleDeleteCalls.Load())
			}
		})
	}
}

func TestRuleMutationStaleInterfaces(t *testing.T) {
	for _, tt := range []struct {
		iface       string
		wantFolders int64
		wantRules   int64
	}{
		{iface: "RuleManager", wantFolders: 1, wantRules: 1},
		{iface: "RuleReader", wantFolders: 1},
		{iface: "FolderReader"},
	} {
		t.Run(tt.iface, func(t *testing.T) {
			fake := testkit.NewFakeStaleRuleMutationInterfaceProvider("fake", tt.iface)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json")
			assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
			body := errorJSON(t, res)
			if body["capability"] == "" {
				t.Fatalf("stale interface body = %#v", body)
			}
			if fake.AccountClientCalls.Load() != 1 || fake.FolderListCalls.Load() != tt.wantFolders || fake.RuleListCalls.Load() != tt.wantRules {
				t.Fatalf("stale counters clients=%d folders=%d rules=%d", fake.AccountClientCalls.Load(), fake.FolderListCalls.Load(), fake.RuleListCalls.Load())
			}
		})
	}
}

func TestRuleMutationCapabilityMatrix(t *testing.T) {
	for _, tt := range []struct {
		name       string
		capability provider.Capability
		pending    bool
	}{
		{name: "rules read unsupported", capability: provider.CapRulesRead},
		{name: "rules read pending", capability: provider.CapRulesRead, pending: true},
		{name: "rules mutate unsupported", capability: provider.CapRulesMutate},
		{name: "rules mutate pending", capability: provider.CapRulesMutate, pending: true},
		{name: "folders read unsupported", capability: provider.CapFoldersRead},
		{name: "folders read pending", capability: provider.CapFoldersRead, pending: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			switch tt.capability {
			case provider.CapRulesRead:
				fake.RulesReadImplemented = false
				fake.RulesReadSupportedNotImpl = tt.pending
			case provider.CapRulesMutate:
				fake.RulesMutateImplemented = false
				fake.RulesMutateSupportedNotImpl = tt.pending
			case provider.CapFoldersRead:
				fake.FoldersImplemented = false
				fake.FoldersSupportedNotImpl = tt.pending
			}
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json")
			want := errs.CodeUnsupportedCapability
			if tt.pending {
				want = errs.CodeCapabilityNotImplemented
			}
			assertErrorCode(t, res, want)
			if fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 0 {
				t.Fatalf("capability counters caps=%d clients=%d", fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load())
			}
		})
	}
}

func TestRuleMutationScopesSnapshotSequenceValidation(t *testing.T) {
	rules := mutationScalarRules(true, false)
	bad := rules["work"][0]
	bad.ID = "bad-sequence"
	bad.DisplayName = "Malformed sequence"
	bad.Sequence = 0
	rules["work"] = append(rules["work"], bad)
	tests := []struct {
		name    string
		args    []string
		wantAPI bool
	}{
		{name: "explicit create ignores unrelated invalid sequence", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--sequence", "3", "--json"}},
		{name: "default create consumes full snapshot", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json"}, wantAPI: true},
		{name: "preserving update checks only target", args: []string{"rules", "update", "rule-1", "--name", "Renamed", "--json"}},
		{name: "current sequence remains preserving", args: []string{"rules", "update", "rule-1", "--name", "Renamed", "--sequence", "10", "--json"}},
		{name: "resequencing consumes full snapshot", args: []string{"rules", "update", "rule-1", "--sequence", "3", "--json"}, wantAPI: true},
		{name: "scalar checks only target", args: []string{"rules", "disable", "rule-1", "--dry-run", "--json"}},
		{name: "delete skips sequence validation", args: []string{"rules", "delete", "bad-sequence", "--dry-run", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run(tt.args...)
			if !tt.wantAPI {
				decodeOK(t, res, nil)
				return
			}
			if res.Exit != 2 || !strings.Contains(res.Stderr, "provider returned an invalid rule sequence") || strings.Contains(res.Stderr, "bad-sequence") || strings.Contains(res.Stderr, "sequence\":0") {
				t.Fatalf("invalid provider sequence result = %#v", res)
			}
			assertNoCode(t, res)
		})
	}
}

func TestRuleMutationRejectsInvalidAuthoritativeSequence(t *testing.T) {
	operations := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
	}{
		{name: "create", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"}},
		{name: "update", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "enable", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "disable", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1", "--json"}},
	}
	sequences := []int{0}
	if strconv.IntSize > 32 {
		aboveGraphRange := int64(math.MaxInt32) + 1
		sequences = append(sequences, int(aboveGraphRange))
	}
	for _, operation := range operations {
		for _, sequence := range sequences {
			t.Run(operation.name+"/"+fmt.Sprint(sequence), func(t *testing.T) {
				invalidSequence := sequence
				fake := testkit.NewFakeRuleMutationProvider("fake", operation.rules())
				fake.RuleMutationPostWriteSequence = &invalidSequence
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run(operation.args...)
				if res.Exit != 2 || res.Stdout != "" || !strings.Contains(res.Stderr, "provider returned an invalid rule sequence") {
					t.Fatalf("invalid read-back sequence result = %#v", res)
				}
				assertNoCode(t, res)
				writes := fake.RuleCreateCalls.Load() + fake.RuleUpdateCalls.Load() + fake.RuleDeleteCalls.Load()
				if writes != 1 || fake.RuleGetCalls.Load() != 1 {
					t.Fatalf("invalid read-back calls writes=%d get=%d", writes, fake.RuleGetCalls.Load())
				}
			})
		}
	}
}

func TestRuleMutationCLIInvalidInputReasons(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		field  string
		reason string
	}{
		{name: "blank name", args: []string{"rules", "create", "--name", " ", "--subject-contains", "x", "--mark-read", "--json"}, field: "name", reason: "empty"},
		{name: "blank from", args: []string{"rules", "create", "--from", " ", "--mark-read", "--json"}, field: "from", reason: "empty_value"},
		{name: "malformed from", args: []string{"rules", "create", "--from", "not an address", "--mark-read", "--json"}, field: "from", reason: "malformed_address"},
		{name: "duplicate from", args: []string{"rules", "create", "--from", "A@example.test", "--from", "a@example.test", "--mark-read", "--json"}, field: "from", reason: "duplicate_value"},
		{name: "duplicate repeatable", args: []string{"rules", "create", "--subject-contains", "Receipt", "--subject-contains", "receipt", "--mark-read", "--json"}, field: "subject_contains", reason: "duplicate_value"},
		{name: "missing condition", args: []string{"rules", "create", "--mark-read", "--json"}, field: "conditions", reason: "required"},
		{name: "missing action", args: []string{"rules", "create", "--subject-contains", "Receipt", "--json"}, field: "actions", reason: "required"},
		{name: "no update fields", args: []string{"rules", "update", "rule-1", "--json"}, field: "changes", reason: "no_changes"},
		{name: "empty move", args: []string{"rules", "create", "--subject-contains", "Receipt", "--move-to", " ", "--json"}, field: "move_to", reason: "empty"},
		{name: "sequence zero", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--sequence", "0", "--json"}, field: "sequence", reason: "out_of_range"},
		{name: "sequence overflow", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--sequence", "2147483648", "--json"}, field: "sequence", reason: "out_of_range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			h := seededRulesHarnessWithProvider(t, fake, "work")
			assertRuleInputReason(t, h.run(tt.args...), tt.field, tt.reason)
			assertNoRuleProviderWork(t, fake)
		})
	}
}

func TestRuleMutationRejectsExplicitFalsePresenceFlags(t *testing.T) {
	for _, flag := range []string{"has-attachments", "mark-read", "stop-processing", "no-has-attachments", "no-mark-read", "no-stop-processing"} {
		t.Run(flag, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
			h := seededRulesHarnessWithProvider(t, fake, "work")
			args := []string{"rules", "update", "rule-1", "--" + flag + "=false", "--json"}
			res := h.run(args...)
			assertFrameworkUsage(t, res)
			if !strings.Contains(res.Stderr, "takes no value") {
				t.Fatalf("explicit false stderr = %q", res.Stderr)
			}
			assertNoRuleProviderWork(t, fake)
		})
	}

	createFake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
	createHarness := seededRulesHarnessWithProvider(t, createFake, "work")
	decodePreview(t, createHarness.run("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply=false", "--json"))
	if createFake.RuleCreateCalls.Load() != 0 {
		t.Fatalf("--apply=false wrote %d rules", createFake.RuleCreateCalls.Load())
	}

	enableFake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(false, false))
	enableHarness := seededRulesHarnessWithProvider(t, enableFake, "work")
	var enabled ruleMutationResult
	decodeOK(t, enableHarness.run("rules", "enable", "rule-1", "--dry-run=false", "--json"), &enabled)
	if !enabled.Applied || enableFake.RuleUpdateCalls.Load() != 1 {
		t.Fatalf("--dry-run=false result=%#v updates=%d", enabled, enableFake.RuleUpdateCalls.Load())
	}
}

func TestRuleMutationRejectsReadOnlyTargets(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "update before folder resolution", args: []string{"rules", "update", "rule-1", "--move-to", "missing-target", "--json"}},
		{name: "enable", args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "disable", args: []string{"rules", "disable", "rule-1", "--json"}},
		{name: "delete before confirmation", args: []string{"rules", "delete", "rule-1", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rules := mutationScalarRules(false, false)
			rules["work"][0].IsReadOnly = true
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			assertRuleInputReason(t, h.run(tt.args...), "rule_id", "rule_read_only")
			if fake.RuleCreateCalls.Load()+fake.RuleUpdateCalls.Load()+fake.RuleDeleteCalls.Load()+fake.RuleGetCalls.Load() != 0 {
				t.Fatalf("read-only path wrote or direct-read: create=%d update=%d delete=%d get=%d", fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load(), fake.RuleDeleteCalls.Load(), fake.RuleGetCalls.Load())
			}
		})
	}
}

func TestRuleMutationAlreadyInStateDoesNotWrite(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		command string
	}{
		{name: "already enabled", enabled: true, command: "enable"},
		{name: "already disabled", enabled: false, command: "disable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(tt.enabled, false))
			h := seededRulesHarnessWithProvider(t, fake, "work")
			var got ruleMutationResult
			decodeOK(t, h.run("rules", tt.command, "rule-1", "--json"), &got)
			if got.Status != mail.RuleMutationStatusUnchanged || got.Applied || got.Changed || got.Verification != nil || got.PreservedFields == nil || len(got.PreservedFields) != 0 {
				t.Fatalf("unchanged result = %#v", got)
			}
			if fake.RuleUpdateCalls.Load() != 0 || fake.RuleGetCalls.Load() != 0 {
				t.Fatalf("unchanged calls update=%d get=%d", fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
			}
		})
	}
}

func TestRuleMutationOutcomeUnknownUsesCommandOperation(t *testing.T) {
	for _, tt := range []struct {
		name string
		op   mail.RuleMutationOperation
		args []string
	}{
		{name: "create", op: mail.RuleOperationCreate, args: []string{"rules", "create", "--subject-contains", "x", "--mark-read", "--apply", "--json"}},
		{name: "update", op: mail.RuleOperationUpdate, args: []string{"rules", "update", "rule-1", "--subject-contains", "changed", "--apply", "--json"}},
		{name: "enable", op: mail.RuleOperationEnable, args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "disable", op: mail.RuleOperationDisable, args: []string{"rules", "disable", "rule-1", "--json"}},
		{name: "delete", op: mail.RuleOperationDelete, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enabled := tt.op == mail.RuleOperationDisable || tt.op == mail.RuleOperationDelete
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(enabled, false))
			fake.RuleMutationErrByOperation = map[mail.RuleMutationOperation]error{
				tt.op: provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeServerUnknown),
			}
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run(tt.args...)
			assertErrorCode(t, res, errs.CodeVerificationFailed)
			if res.Stdout != "" {
				t.Fatalf("outcome failure stdout = %q", res.Stdout)
			}
			body := errorJSON(t, res)
			if body["operation"] != string(tt.op) || body["reason"] != string(provider.RuleMutationOutcomeServerUnknown) {
				t.Fatalf("outcome body = %#v", body)
			}
			wantGet := int64(1)
			if tt.op == mail.RuleOperationCreate {
				wantGet = 0
			}
			if fake.RuleGetCalls.Load() != wantGet || fake.RuleCreateCalls.Load()+fake.RuleUpdateCalls.Load()+fake.RuleDeleteCalls.Load() != 1 {
				t.Fatalf("outcome calls get=%d writes=%d", fake.RuleGetCalls.Load(), fake.RuleCreateCalls.Load()+fake.RuleUpdateCalls.Load()+fake.RuleDeleteCalls.Load())
			}
		})
	}
}

func TestRuleMutationPreviewGoldens(t *testing.T) {
	create := runMutationPreview(t, mutationUpdateRules(), "create", "--name", "Receipts", "--subject-contains", "Receipt", "--mark-read", "--json")
	assertPreviewBody(t, create, map[string]any{
		"account": "work", "applied": "false", "apply_required": "true", "changed": "true", "display_name": "Receipts", "generated_name": "false",
		"mark_as_read_after": "true", "mark_as_read_before": "absent", "operation": "rule_create", "predicate_families": "conjunctive", "predicate_repeated_values": "disjunctive",
		"preserved_fields_count": "0", "provider": "fake", "rule_scope": "inbox", "sequence": "11", "sequence_source": "defaulted", "source_rule_count": "1", "status": "preview",
		"subject_contains_after_1": "Receipt", "subject_contains_after_count": "1", "subject_contains_before_count": "0",
	})

	update := runMutationPreview(t, mutationUpdateRules(), "update", "rule-1", "--subject-contains", "Receipt", "--json")
	assertPreviewBody(t, update, map[string]any{
		"account": "work", "applied": "false", "apply_required": "true", "changed": "true", "display_name": "Invoice filing", "operation": "rule_update",
		"predicate_families": "conjunctive", "predicate_repeated_values": "disjunctive", "preserved_fields_1": "actions.mark_as_read", "preserved_fields_2": "conditions.has_attachments",
		"preserved_fields_3": "display_name", "preserved_fields_4": "exceptions", "preserved_fields_5": "is_enabled", "preserved_fields_6": "sequence", "preserved_fields_7": "unsupported_fields",
		"preserved_fields_count": "7", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "sequence": "10", "sequence_source": "preserved", "status": "preview",
		"subject_contains_after_1": "Receipt", "subject_contains_after_count": "1", "subject_contains_before_1": "Invoice", "subject_contains_before_count": "1",
	})

	mixed := runMutationPreview(t, mutationMixedRules(), "update", "rule-1", "--from", "New Sender <new@example.test>", "--no-mark-read", "--move-to", "Archive", "--json")
	assertPreviewBody(t, mixed, mutationMixedPreviewGolden())

	changed := runMutationPreview(t, mutationScalarRules(false, false), "enable", "rule-1", "--dry-run", "--json")
	assertPreviewBody(t, changed, map[string]any{
		"account": "work", "applied": "false", "changed": "true", "display_name": "Invoice filing", "is_enabled_after": "true", "is_enabled_before": "false", "operation": "rule_enable",
		"preserved_fields_1": "actions.mark_as_read", "preserved_fields_2": "conditions.subject_contains", "preserved_fields_3": "display_name", "preserved_fields_4": "sequence", "preserved_fields_count": "4",
		"provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "sequence": "10", "sequence_source": "preserved", "status": "preview",
	})

	unchanged := runMutationPreview(t, mutationScalarRules(true, false), "enable", "rule-1", "--dry-run", "--json")
	assertPreviewBody(t, unchanged, map[string]any{
		"account": "work", "applied": "false", "changed": "false", "display_name": "Invoice filing", "is_enabled_after": "true", "is_enabled_before": "true", "operation": "rule_enable",
		"preserved_fields_count": "0", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "sequence": "10", "sequence_source": "preserved", "status": "preview",
	})

	deleted := runMutationPreview(t, mutationScalarRules(true, false), "delete", "rule-1", "--dry-run", "--json")
	assertPreviewBody(t, deleted, map[string]any{
		"account": "work", "applied": "false", "changed": "true", "confirmation_required": "true", "display_name": "Invoice filing", "operation": "rule_delete",
		"preserved_fields_count": "0", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "status": "preview",
	})

	assertPreviewHumanGolden(t, mutationUpdateRules(), []string{"rules", "create", "--name", "Receipts", "--subject-contains", "Receipt", "--mark-read"}, "Would create the rule \"Receipts\" in work:\n\n  When:    subject contains \"Receipt\"\n  Then:    mark as read\n\nNothing was changed. To create it, run:\n  maildo rules create --config <config> --name Receipts --subject-contains Receipt --mark-read --account work --apply\n")
	assertPreviewHumanGolden(t, mutationUpdateRules(), []string{"rules", "update", "rule-1", "--subject-contains", "Receipt"}, "Would change the rule \"Invoice filing\" in work:\n\n  Subject contains:    \"Receipt\" (was \"Invoice\")\n\nThe rest of the rule would stay the same.\nNothing was changed. To change it, run:\n  maildo rules update rule-1 --config <config> --subject-contains Receipt --account work --apply\n")
	assertPreviewHumanGolden(t, mutationScalarRules(false, false), []string{"rules", "enable", "rule-1", "--dry-run"}, "Would turn on the rule \"Invoice filing\" in work.\n\nNothing was changed. To turn it on, run:\n  maildo rules enable rule-1 --config <config> --account work\n")
	assertPreviewHumanGolden(t, mutationScalarRules(true, false), []string{"rules", "enable", "rule-1", "--dry-run"}, "The rule \"Invoice filing\" in work is already on, so nothing would change.\n")
	assertPreviewHumanGolden(t, mutationScalarRules(true, false), []string{"rules", "delete", "rule-1", "--dry-run"}, "Would delete the rule \"Invoice filing\" in work.\n\nNothing was changed. To delete it, run:\n  maildo rules delete rule-1 --config <config> --account work --confirm rule-1\n")
}

func TestRuleMutationPreviewPreservesMachineValuesAndRejectsHumanControls(t *testing.T) {
	unsafe := "value\nSPOOF\r\x1b[31mred\x1b[0m\u202eoverride"
	yes := true
	acct := provider.ResolvedAccount{Name: "account-" + unsafe, Provider: "provider-" + unsafe}
	rule := mail.Rule{
		ID: "rule-" + unsafe, DisplayName: "name-" + unsafe, Sequence: 10, IsEnabled: true,
		Conditions: mail.RulePredicates{SubjectContains: []string{"before-" + unsafe}},
		Actions:    mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "old-id-" + unsafe, DisplayName: "old-" + unsafe}, MarkAsRead: &yes},
	}
	change := mail.RuleChangeSet{
		Operation:      mail.RuleOperationUpdate,
		SequenceSource: mail.RuleSequencePreserved,
		SubjectContains: &mail.RuleStringCollectionChange{
			Set: true, Before: []string{"before-" + unsafe}, After: []string{"after-" + unsafe}, Count: 1,
		},
		MoveToFolder: &mail.RuleFolderTargetChange{
			Set:    true,
			Before: &mail.RuleFolderTarget{ProviderFolderID: "old-id-" + unsafe, DisplayName: "old-" + unsafe},
			After:  &mail.RuleFolderTarget{ProviderFolderID: "new-id-" + unsafe, Path: "new-" + unsafe},
		},
	}
	preview := previewRuleMutation(acct, rule, change, true, false, false)
	machineJSON, err := preview.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var machine map[string]any
	if err := json.Unmarshal(machineJSON, &machine); err != nil {
		t.Fatal(err)
	}
	body := machine["body"].(map[string]any)
	if machine["path"] != acct.Name || body["display_name"] != rule.DisplayName || body["rule_id"] != rule.ID ||
		body["subject_contains_before_1"] != "before-"+unsafe || body["subject_contains_after_1"] != "after-"+unsafe ||
		body["move_to_folder_before_id"] != "old-id-"+unsafe || body["move_to_folder_after"] != "new-"+unsafe {
		t.Fatalf("machine preview changed raw values: %#v", machine)
	}

	var human bytes.Buffer
	humanRulePreview(acct, rule, change, "maildo rules update "+rule.ID)(&human)
	text := human.String()
	assertNoTerminalControls(t, text)
	for _, forbidden := range []string{"\x1b", "\r", "\u202e", "\nSPOOF"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("preview retained terminal-control boundary %q: %q", forbidden, text)
		}
	}
	if !strings.Contains(text, fmt.Sprintf("Would change the rule %q in %s:", plainField(rule.DisplayName), plainField(acct.Name))) ||
		!strings.Contains(text, "  maildo rules update "+plainField(rule.ID)+"\n") {
		t.Fatalf("preview did not retain sanitized values: %q", text)
	}

	ruleID := "rule-" + unsafe
	rules := mutationScalarRules(true, false)
	rules["work"][0].ID = ruleID
	rules["work"][0].DisplayName = "name-" + unsafe
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")
	deleteArgs := []string{"rules", "delete", ruleID, "--dry-run"}
	deleteJSON := decodePreview(t, h.run(append(append([]string(nil), deleteArgs...), "--json")...))
	deleteBody := deleteJSON["body"].(map[string]any)
	if deleteBody["rule_id"] != ruleID || deleteBody["display_name"] != "name-"+unsafe {
		t.Fatalf("delete JSON preview changed raw values: %#v", deleteBody)
	}
	quotedID, err := json.Marshal(ruleID)
	if err != nil {
		t.Fatal(err)
	}
	jq := h.run(append(append([]string(nil), deleteArgs...), "--jq", ".body.rule_id == "+string(quotedID))...)
	decodeOK(t, jq, nil)
	if strings.TrimSpace(jq.Stdout) != "true" {
		t.Fatalf("jq did not receive the raw rule id: %q", jq.Stdout)
	}
	identity := decodePreview(t, h.run(append(append([]string(nil), deleteArgs...), "--jq=")...))
	identityBody := identity["body"].(map[string]any)
	if identityBody["rule_id"] != ruleID || identityBody["display_name"] != "name-"+unsafe {
		t.Fatalf("empty jq identity changed raw values: %#v", identityBody)
	}
	templateExpr := "{{if eq .body.rule_id " + string(quotedID) + "}}exact{{else}}changed{{end}}"
	template := h.run(append(append([]string(nil), deleteArgs...), "--template", templateExpr)...)
	decodeOK(t, template, nil)
	if strings.TrimSpace(template.Stdout) != "exact" {
		t.Fatalf("template did not receive the raw rule id: %q", template.Stdout)
	}
	deleteHuman := h.run(deleteArgs...)
	decodeOK(t, deleteHuman, nil)
	assertNoTerminalControls(t, deleteHuman.Stdout)
	if strings.Contains(deleteHuman.Stdout, "\nSPOOF") || strings.Contains(deleteHuman.Stdout, "\r") || strings.Contains(deleteHuman.Stdout, "\u202e") {
		t.Fatalf("delete human preview retained control boundaries: %q", deleteHuman.Stdout)
	}

	update := decodePreview(t, h.run("rules", "update", ruleID, "--subject-contains", unsafe, "--json"))
	if update["body"].(map[string]any)["subject_contains_after_1"] != unsafe {
		t.Fatalf("update JSON preview changed user input: %#v", update)
	}
}

func TestRuleMutationOutputGoldens(t *testing.T) {
	t.Run("create JSON and human", func(t *testing.T) {
		args := []string{"rules", "create", "--name", "Receipts", "--subject-contains", "Receipt", "--mark-read", "--apply"}
		jsonResult := runAppliedRuleMutation(t, mutationUpdateRules(), append(args, "--json")...)
		assertJSONGolden(t, jsonResult.Stdout, `{
			"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],
			"operation":"rule_create","status":"applied","applied":true,"changed":true,"account":"work","provider":"fake","rule_scope":"inbox",
			"rule_id":"fake-rule-1","display_name":"Receipts","generated_name":false,"sequence":11,"sequence_source":"defaulted","source_rule_count":1,
			"change_set":{"operation":"rule_create","sequence_source":"defaulted","display_name":{"after":"Receipts"},"sequence":{"after":11},"is_enabled":{"after":true},"subject_contains":{"after":["Receipt"],"count":1},"mark_as_read":{"after":true}},
			"preserved_fields":[],"verification":{"status":"verified","checked_fields":["actions.mark_as_read","conditions.subject_contains","display_name","is_enabled","presence.actions.mark_as_read","presence.actions.stop_processing_rules","presence.conditions.has_attachments","rule_id","sequence"]},
			"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]},"action_log_status":{"written":true,"entry_count":1,"relative_path":"audit/actions.jsonl"}
		}`)
		human := runAppliedRuleMutation(t, mutationUpdateRules(), args...)
		wantHuman := "Created the rule \"Receipts\" in work. Its id is fake-rule-1.\n"
		if human.Stdout != wantHuman {
			t.Fatalf("create human\nwant=%q\ngot =%q", wantHuman, human.Stdout)
		}
	})

	t.Run("update JSON and human", func(t *testing.T) {
		args := []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply"}
		jsonResult := runAppliedRuleMutation(t, mutationUpdateRules(), append(args, "--json")...)
		assertJSONGolden(t, jsonResult.Stdout, `{
			"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],
			"operation":"rule_update","status":"applied","applied":true,"changed":true,"account":"work","provider":"fake","rule_scope":"inbox",
			"rule_id":"rule-1","display_name":"Invoice filing","sequence":10,"sequence_source":"preserved",
			"change_set":{"operation":"rule_update","sequence_source":"preserved","subject_contains":{"before":["Invoice"],"after":["Receipt"],"removed":["Invoice"],"count":1}},
			"preserved_fields":["actions.mark_as_read","conditions.has_attachments","display_name","exceptions","is_enabled","sequence","unsupported_fields"],
			"verification":{"status":"verified","checked_fields":["actions.mark_as_read","conditions.has_attachments","conditions.subject_contains","display_name","exceptions","is_enabled","presence.actions.mark_as_read","presence.actions.stop_processing_rules","presence.conditions.has_attachments","rule_id","sequence","unsupported_fields"]},
			"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]},"action_log_status":{"written":true,"entry_count":1,"relative_path":"audit/actions.jsonl"}
		}`)
		human := runAppliedRuleMutation(t, mutationUpdateRules(), args...)
		wantHuman := "Changed the rule \"Invoice filing\" in work:\n\n  Subject contains:    \"Receipt\" (was \"Invoice\")\n"
		if human.Stdout != wantHuman {
			t.Fatalf("update human\nwant=%q\ngot =%q", wantHuman, human.Stdout)
		}
	})

	for _, tt := range []struct {
		name      string
		enabled   bool
		command   string
		status    string
		applied   bool
		changed   bool
		before    bool
		after     bool
		wantHuman string
		wantJSON  string
	}{
		{name: "enable changed", enabled: false, command: "enable", status: "applied", applied: true, changed: true, before: false, after: true,
			wantJSON:  `{"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],"operation":"rule_enable","status":"applied","applied":true,"changed":true,"account":"work","provider":"fake","rule_scope":"inbox","rule_id":"rule-1","display_name":"Invoice filing","sequence":10,"sequence_source":"preserved","change_set":{"operation":"rule_enable","sequence_source":"preserved","is_enabled":{"before":false,"after":true}},"preserved_fields":["actions.mark_as_read","conditions.subject_contains","display_name","sequence"],"verification":{"status":"verified","checked_fields":["actions.mark_as_read","conditions.subject_contains","display_name","is_enabled","presence.actions.mark_as_read","presence.actions.stop_processing_rules","presence.conditions.has_attachments","rule_id","sequence"]},"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]},"action_log_status":{"written":true,"entry_count":1,"relative_path":"audit/actions.jsonl"}}`,
			wantHuman: "Turned on the rule \"Invoice filing\" in work.\n"},
		{name: "disable changed", enabled: true, command: "disable", status: "applied", applied: true, changed: true, before: true, after: false,
			wantJSON:  `{"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],"operation":"rule_disable","status":"applied","applied":true,"changed":true,"account":"work","provider":"fake","rule_scope":"inbox","rule_id":"rule-1","display_name":"Invoice filing","sequence":10,"sequence_source":"preserved","change_set":{"operation":"rule_disable","sequence_source":"preserved","is_enabled":{"before":true,"after":false}},"preserved_fields":["actions.mark_as_read","conditions.subject_contains","display_name","sequence"],"verification":{"status":"verified","checked_fields":["actions.mark_as_read","conditions.subject_contains","display_name","is_enabled","presence.actions.mark_as_read","presence.actions.stop_processing_rules","presence.conditions.has_attachments","rule_id","sequence"]},"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]},"action_log_status":{"written":true,"entry_count":1,"relative_path":"audit/actions.jsonl"}}`,
			wantHuman: "Turned off the rule \"Invoice filing\" in work.\n"},
		{name: "enable unchanged", enabled: true, command: "enable", status: "unchanged", before: true, after: true,
			wantJSON:  `{"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],"operation":"rule_enable","status":"unchanged","applied":false,"changed":false,"account":"work","provider":"fake","rule_scope":"inbox","rule_id":"rule-1","display_name":"Invoice filing","sequence":10,"sequence_source":"preserved","change_set":{"operation":"rule_enable","sequence_source":"preserved"},"preserved_fields":[],"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]}}`,
			wantHuman: "The rule \"Invoice filing\" in work was already on.\n"},
		{name: "disable unchanged", enabled: false, command: "disable", status: "unchanged", before: false, after: false,
			wantJSON:  `{"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],"operation":"rule_disable","status":"unchanged","applied":false,"changed":false,"account":"work","provider":"fake","rule_scope":"inbox","rule_id":"rule-1","display_name":"Invoice filing","sequence":10,"sequence_source":"preserved","change_set":{"operation":"rule_disable","sequence_source":"preserved"},"preserved_fields":[],"provider_metadata":{"source":"fake"},"provider_status":{"has_error":false,"is_read_only":false,"warnings":[]}}`,
			wantHuman: "The rule \"Invoice filing\" in work was already off.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			jsonResult := runAppliedRuleMutation(t, mutationScalarRules(tt.enabled, false), "rules", tt.command, "rule-1", "--json")
			assertJSONGolden(t, jsonResult.Stdout, tt.wantJSON)
			human := runAppliedRuleMutation(t, mutationScalarRules(tt.enabled, false), "rules", tt.command, "rule-1")
			if human.Stdout != tt.wantHuman {
				t.Fatalf("scalar human\nwant=%q\ngot =%q", tt.wantHuman, human.Stdout)
			}
		})
	}

	t.Run("delete JSON and human", func(t *testing.T) {
		args := []string{"rules", "delete", "rule-1", "--confirm", "rule-1"}
		jsonResult := runAppliedRuleMutation(t, mutationScalarRules(true, false), append(args, "--json")...)
		assertJSONGolden(t, jsonResult.Stdout, `{
			"scope":"single","accounts":[{"name":"work","provider":"fake","email":"work@example.test","display_name":"Work Account","active":true}],
			"operation":"rule_delete","status":"applied","applied":true,"changed":true,"account":"work","provider":"fake","rule_scope":"inbox","rule_id":"rule-1","display_name":"Invoice filing",
			"preserved_fields":[],"confirmation_required":true,"confirmation_matched":true,"verification":{"status":"verified","checked_fields":["absence"]},"provider_metadata":{"source":"fake"},"action_log_status":{"written":true,"entry_count":1,"relative_path":"audit/actions.jsonl"}
		}`)
		human := runAppliedRuleMutation(t, mutationScalarRules(true, false), args...)
		wantHuman := "Deleted the rule \"Invoice filing\" in work.\n"
		if human.Stdout != wantHuman {
			t.Fatalf("delete human\nwant=%q\ngot =%q", wantHuman, human.Stdout)
		}
	})
}

func TestRuleMutationAffectedRuleSummary(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", map[string][]mail.Rule{"work": {}})
	h := seededRulesHarnessWithProvider(t, fake, "work")
	preview := decodePreview(t, h.run("rules", "create", "--name", "Receipts", "--subject-contains", "Receipt", "--mark-read", "--json"))
	body := preview["body"].(map[string]any)
	if body["source_rule_count"] != "0" || body["generated_name"] != "false" || body["subject_contains_before_count"] != "0" || body["subject_contains_after_1"] != "Receipt" {
		t.Fatalf("create summary = %#v", body)
	}

	preview = runMutationPreview(t, mutationMixedRules(), "update", "rule-1", "--from", "New Sender <new@example.test>", "--no-mark-read", "--move-to", "Archive", "--json")
	assertPreviewBody(t, preview, mutationMixedPreviewGolden())
	/*
		The complete mixed preview is intentionally asserted in both the golden
		and affected-summary cases: one protects the exact renderer contract and
		the other keeps source/preservation accounting traceable.
	*/
}

func mutationMixedPreviewGolden() map[string]any {
	return map[string]any{
		"account": "work", "applied": "false", "apply_required": "true", "changed": "true", "display_name": "Invoice filing",
		"from_addresses_after_1": "New Sender <new@example.test>", "from_addresses_after_count": "1", "from_addresses_before_1": "Old Sender <old@example.test>", "from_addresses_before_count": "1",
		"mark_as_read_after": "absent", "mark_as_read_before": "true", "move_to_folder_after": "Archive", "move_to_folder_after_id": "archive-id", "move_to_folder_before": "Old", "move_to_folder_before_id": "old-folder",
		"operation": "rule_update", "predicate_families": "conjunctive", "predicate_repeated_values": "disjunctive",
		"preserved_fields_1": "conditions.has_attachments", "preserved_fields_2": "conditions.subject_contains", "preserved_fields_3": "display_name", "preserved_fields_4": "exceptions", "preserved_fields_5": "is_enabled", "preserved_fields_6": "sequence", "preserved_fields_7": "unsupported_fields", "preserved_fields_count": "7",
		"provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "sequence": "10", "sequence_source": "preserved", "status": "preview",
	}
}

func TestRuleMutationVerificationFailureContract(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
	fake.RuleMutationSkipStateUpdate = true
	h := seededRulesHarnessWithProvider(t, fake, "work")
	res := h.run("rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json")
	assertErrorCode(t, res, errs.CodeVerificationFailed)
	if res.Stdout != "" {
		t.Fatalf("verification failure stdout = %q", res.Stdout)
	}
	body := errorJSON(t, res)
	if body["verification_cause"] != string(mail.RuleVerificationCauseMismatch) || body["mismatch_count"] != float64(1) || body["operation"] != string(mail.RuleOperationUpdate) {
		t.Fatalf("verification body = %#v", body)
	}
	for _, forbidden := range []string{"expected", "actual", "ProviderState", "Receipt", "Invoice"} {
		if strings.Contains(res.Stderr, forbidden) {
			t.Fatalf("verification error exposed %q: %s", forbidden, res.Stderr)
		}
	}
	if fake.RuleUpdateCalls.Load() != 1 || fake.RuleGetCalls.Load() != 1 {
		t.Fatalf("verification calls update=%d get=%d", fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
	}
}

func TestRuleMutationVerificationCauseAndProviderStatusContract(t *testing.T) {
	operations := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
	}{
		{name: "create", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"}},
		{name: "update", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "enable", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1", "--json"}},
		{name: "disable", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1", "--json"}},
	}
	for _, operation := range operations {
		for _, status := range []string{"error", "read_only"} {
			t.Run(operation.name+"/"+status, func(t *testing.T) {
				fake := testkit.NewFakeRuleMutationProvider("fake", operation.rules())
				fake.RuleMutationPostWriteHasError = status == "error"
				fake.RuleMutationPostWriteReadOnly = status == "read_only"
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run(operation.args...)
				if status == "error" {
					assertErrorCode(t, res, errs.CodeVerificationFailed)
					if res.Stdout != "" {
						t.Fatalf("provider status failure stdout = %q", res.Stdout)
					}
					body := errorJSON(t, res)
					if body["verification_cause"] != string(mail.RuleVerificationCauseProviderReportedError) {
						t.Fatalf("provider error body = %#v", body)
					}
					if _, ok := body["mismatch_count"]; ok {
						t.Fatalf("provider error has mismatch count: %#v", body)
					}
					return
				}
				var got ruleMutationResult
				decodeOK(t, res, &got)
				if got.ProviderStatus == nil || !got.ProviderStatus.IsReadOnly || !reflect.DeepEqual(got.ProviderStatus.Warnings, []mail.RuleMutationProviderWarning{mail.RuleMutationWarningProviderMarkedReadOnly}) {
					t.Fatalf("read-only success = %#v", got)
				}
			})
		}
	}
}

func TestRuleMutationUnchangedProviderErrorStatus(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(true, true))
	h := seededRulesHarnessWithProvider(t, fake, "work")
	var got ruleMutationResult
	res := h.run("rules", "enable", "rule-1", "--json")
	decodeOK(t, res, &got)
	if got.Status != mail.RuleMutationStatusUnchanged || got.ProviderStatus == nil || !got.ProviderStatus.HasError || got.Verification != nil {
		t.Fatalf("unchanged provider error = %#v", got)
	}
	if fake.RuleUpdateCalls.Load() != 0 || fake.RuleGetCalls.Load() != 0 {
		t.Fatalf("unchanged provider error calls update=%d get=%d", fake.RuleUpdateCalls.Load(), fake.RuleGetCalls.Load())
	}
	humanFake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(true, true))
	humanHarness := seededRulesHarnessWithProvider(t, humanFake, "work")
	human := humanHarness.run("rules", "enable", "rule-1")
	decodeOK(t, human, nil)
	if human.Stdout != "The rule \"Invoice filing\" in work was already on.\nWarning: the mail service reports an error for this rule, so it may not run.\n" {
		t.Fatalf("unchanged human = %q", human.Stdout)
	}
}

func TestRuleDeleteConfirmationUsesRawProviderID(t *testing.T) {
	const rawID = " Raw-ID-XyZ "
	rules := mutationScalarRules(true, false)
	rules["work"][0].ID = rawID
	for _, supplied := range []string{"Raw-ID-XyZ", " raw-id-xyz ", " Wrong-Token-AbC "} {
		t.Run(supplied, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run("rules", "delete", rawID, "--confirm", supplied, "--json")
			assertErrorCode(t, res, errs.CodeConfirmationRequired)
			for _, forbidden := range []string{rawID, strings.TrimSpace(rawID), strings.ToLower(strings.TrimSpace(rawID)), supplied, strings.TrimSpace(supplied), strings.ToLower(strings.TrimSpace(supplied))} {
				if forbidden != "" && strings.Contains(res.Stderr, forbidden) {
					t.Fatalf("confirmation error exposed %q: %s", forbidden, res.Stderr)
				}
			}
			if fake.RuleDeleteCalls.Load() != 0 || fake.RuleGetCalls.Load() != 0 {
				t.Fatalf("mismatch wrote delete=%d get=%d", fake.RuleDeleteCalls.Load(), fake.RuleGetCalls.Load())
			}
		})
	}
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")
	var got ruleMutationResult
	decodeOK(t, h.run("rules", "delete", rawID, "--confirm", rawID, "--json"), &got)
	if got.RuleID != rawID || !got.ConfirmationMatched {
		t.Fatalf("raw confirmation result = %#v", got)
	}
}

func TestRuleDeleteNeverReadsStdinForConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		wantError  bool
		wantDelete int64
		wantGet    int64
	}{
		{name: "dry run", args: []string{"rules", "delete", "rule-1", "--dry-run", "--json"}},
		{name: "missing confirmation", args: []string{"rules", "delete", "rule-1", "--json"}, wantError: true},
		{name: "mismatched confirmation", args: []string{"rules", "delete", "rule-1", "--confirm", "different", "--json"}, wantError: true},
		{name: "no prompt does not bypass confirmation", args: []string{"rules", "delete", "rule-1", "--no-prompt", "--json"}, wantError: true},
		{name: "exact confirmation", args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1", "--json"}, wantDelete: 1, wantGet: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(true, false))
			h := seededRulesHarnessWithProvider(t, fake, "work")
			stdin := &observedStringReader{reader: strings.NewReader("rule-1\n")}
			res := h.runWith(testutil.Options{Stdin: stdin, TerminalSet: true, Terminal: true}, tt.args...)
			if tt.wantError {
				assertErrorCode(t, res, errs.CodeConfirmationRequired)
			} else {
				decodeOK(t, res, nil)
			}
			if stdin.reads != 0 {
				t.Fatalf("delete read stdin %d times", stdin.reads)
			}
			if fake.RuleDeleteCalls.Load() != tt.wantDelete || fake.RuleGetCalls.Load() != tt.wantGet {
				t.Fatalf("delete/get calls = %d/%d, want %d/%d", fake.RuleDeleteCalls.Load(), fake.RuleGetCalls.Load(), tt.wantDelete, tt.wantGet)
			}
		})
	}
}

type observedStringReader struct {
	reader *strings.Reader
	reads  int
}

func (r *observedStringReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func TestRuleMutationMetadataEnvelopeUsesSemanticDataEquality(t *testing.T) {
	tests := []struct {
		name  string
		rules func() map[string][]mail.Rule
		args  []string
		extra map[string]any
	}{
		{
			name: "applied create", rules: mutationUpdateRules,
			args:  []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"},
			extra: map[string]any{"account": "work", "applied": true, "changed": true, "command_path": "maildo rules create", "operation": "rule_create", "provider": "fake", "rule_id": "fake-rule-1", "rule_scope": "inbox", "scope": "single", "sequence": float64(11), "sequence_source": "defaulted", "status": "applied", "verification_status": "verified"},
		},
		{
			name: "applied update", rules: mutationUpdateRules,
			args:  []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"},
			extra: map[string]any{"account": "work", "applied": true, "changed": true, "command_path": "maildo rules update", "operation": "rule_update", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "scope": "single", "sequence": float64(10), "sequence_source": "preserved", "status": "applied", "verification_status": "verified"},
		},
		{
			name: "changed scalar", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) },
			args:  []string{"rules", "enable", "rule-1", "--json"},
			extra: map[string]any{"account": "work", "applied": true, "changed": true, "command_path": "maildo rules enable", "operation": "rule_enable", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "scope": "single", "sequence": float64(10), "sequence_source": "preserved", "status": "applied", "verification_status": "verified"},
		},
		{
			name: "unchanged scalar", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) },
			args:  []string{"rules", "enable", "rule-1", "--json"},
			extra: map[string]any{"account": "work", "applied": false, "changed": false, "command_path": "maildo rules enable", "operation": "rule_enable", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "scope": "single", "sequence": float64(10), "sequence_source": "preserved", "status": "unchanged"},
		},
		{
			name: "applied delete", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) },
			args:  []string{"rules", "delete", "rule-1", "--confirm", "rule-1", "--json"},
			extra: map[string]any{"account": "work", "applied": true, "changed": true, "command_path": "maildo rules delete", "confirmation": "matched", "operation": "rule_delete", "provider": "fake", "rule_id": "rule-1", "rule_scope": "inbox", "scope": "single", "status": "applied", "verification_status": "verified"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			standaloneFake := testkit.NewFakeRuleMutationProvider("fake", tt.rules())
			standaloneHarness := seededRulesHarnessWithProvider(t, standaloneFake, "work")
			standalone := standaloneHarness.run(tt.args...)
			decodeOK(t, standalone, nil)
			var standaloneData any
			if err := json.Unmarshal([]byte(standalone.Stdout), &standaloneData); err != nil {
				t.Fatal(err)
			}

			envelopeFake := testkit.NewFakeRuleMutationProvider("fake", tt.rules())
			envelopeHarness := seededRulesHarnessWithProvider(t, envelopeFake, "work")
			var envelope map[string]any
			args := append(append([]string(nil), tt.args...), "--include-meta")
			decodeOK(t, envelopeHarness.run(args...), &envelope)
			standaloneCanonical, err := json.Marshal(standaloneData)
			if err != nil {
				t.Fatal(err)
			}
			envelopeCanonical, err := json.Marshal(envelope["data"])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(envelopeCanonical, standaloneCanonical) {
				t.Fatalf("envelope data differs\nstandalone=%s\nenvelope=%s", standaloneCanonical, envelopeCanonical)
			}
			extra := envelope["meta"].(map[string]any)["extra"].(map[string]any)
			if !reflect.DeepEqual(extra, tt.extra) {
				t.Fatalf("metadata differs\nwant=%#v\ngot =%#v", tt.extra, extra)
			}
		})
	}
}

func TestRuleMutationOutputModeMatrix(t *testing.T) {
	operations := []struct {
		name    string
		rules   func() map[string][]mail.Rule
		args    []string
		preview bool
	}{
		{name: "create", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply"}},
		{name: "update", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply"}},
		{name: "enable changed", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1"}},
		{name: "disable changed", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1"}},
		{name: "enable unchanged", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "enable", "rule-1"}},
		{name: "disable unchanged", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "disable", "rule-1"}},
		{name: "delete", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--confirm", "rule-1"}},
		{name: "create preview", rules: mutationUpdateRules, args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read"}, preview: true},
		{name: "update preview", rules: mutationUpdateRules, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt"}, preview: true},
		{name: "enable changed preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "enable", "rule-1", "--dry-run"}, preview: true},
		{name: "enable unchanged preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "enable", "rule-1", "--dry-run"}, preview: true},
		{name: "disable changed preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "disable", "rule-1", "--dry-run"}, preview: true},
		{name: "disable unchanged preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(false, false) }, args: []string{"rules", "disable", "rule-1", "--dry-run"}, preview: true},
		{name: "delete preview", rules: func() map[string][]mail.Rule { return mutationScalarRules(true, false) }, args: []string{"rules", "delete", "rule-1", "--dry-run"}, preview: true},
	}
	for _, operation := range operations {
		jqExpression := ".status"
		templateExpression := "{{.status}}"
		if operation.preview {
			jqExpression = ".body.status"
			templateExpression = "{{.body.status}}"
		}
		for _, mode := range []struct {
			name string
			args []string
		}{
			{name: "human"},
			{name: "json", args: []string{"--json"}},
			{name: "jq", args: []string{"--json", "--jq", jqExpression}},
			{name: "template", args: []string{"--template", templateExpression}},
		} {
			t.Run(operation.name+"/"+mode.name, func(t *testing.T) {
				fake := testkit.NewFakeRuleMutationProvider("fake", operation.rules())
				h := seededRulesHarnessWithProvider(t, fake, "work")
				args := append(append([]string(nil), operation.args...), mode.args...)
				res := h.run(args...)
				decodeOK(t, res, nil)
				if res.Stderr != "" || res.Stdout == "" || strings.Contains(res.Stdout, "\x1b[") {
					t.Fatalf("mode result = %#v", res)
				}
				if operation.preview && mode.name == "human" && !strings.Contains(res.Stdout, "nothing would change") && !strings.Contains(res.Stdout, "Nothing was changed. To ") {
					t.Fatalf("preview human = %q", res.Stdout)
				}
			})
		}
	}

	for _, args := range [][]string{
		{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--json", "--include-meta"},
		{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json", "--include-meta"},
		{"rules", "enable", "rule-1", "--dry-run", "--json", "--include-meta"},
		{"rules", "delete", "rule-1", "--dry-run", "--json", "--include-meta"},
	} {
		fake := testkit.NewFakeRuleMutationProvider("fake", mutationScalarRules(false, false))
		h := seededRulesHarnessWithProvider(t, fake, "work")
		assertFrameworkUsage(t, h.run(args...))
	}
}

func TestRuleMutationDescriptorsAndGeneratedNames(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	manifest := testutil.CaptureManifest(t, h.app())
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	for _, tt := range []struct {
		path        string
		use         string
		short       string
		examples    []string
		related     []string
		flags       []string
		destructive bool
	}{
		{
			path: "rules create", use: "create", short: "Create an Inbox rule",
			examples: []string{`maildo rules create --account work --subject-contains invoice --move-to Archive`, `maildo rules create --from "Client <client@example.com>" --move-to Clients --mark-read --apply`, `maildo rules create --sender-contains alerts --has-attachments --stop-processing --json`},
			related:  []string{"rules list", "rules view", "folders list"},
			flags:    []string{"account", "apply", "from", "has-attachments", "header-contains", "mark-read", "move-to", "name", "recipient-contains", "sender-contains", "sequence", "stop-processing", "subject-contains"},
		},
		{
			path: "rules update", use: "update <rule-id>", short: "Change an Inbox rule; a condition you give replaces its old values",
			examples: []string{`maildo rules update rule-id --account work --subject-contains invoice --subject-contains receipt`, `maildo rules update rule-id --move-to Archive --apply`, `maildo rules update rule-id --no-mark-read --apply --json`},
			related:  []string{"rules view", "rules list", "folders list"},
			flags:    []string{"account", "apply", "from", "has-attachments", "header-contains", "mark-read", "move-to", "name", "no-has-attachments", "no-mark-read", "no-stop-processing", "recipient-contains", "sender-contains", "sequence", "stop-processing", "subject-contains"},
		},
		{
			path: "rules enable", use: "enable <rule-id>", short: "Turn an Inbox rule on",
			examples: []string{`maildo rules enable rule-id --account work`, `maildo rules enable rule-id --dry-run --json`},
			related:  []string{"rules disable", "rules view"}, flags: []string{"account"},
		},
		{
			path: "rules disable", use: "disable <rule-id>", short: "Turn an Inbox rule off",
			examples: []string{`maildo rules disable rule-id --account work`, `maildo rules disable rule-id --dry-run --json`},
			related:  []string{"rules enable", "rules view"}, flags: []string{"account"},
		},
		{
			path: "rules delete", use: "delete <rule-id>", short: "Delete an Inbox rule",
			examples: []string{`maildo rules delete rule-id --account work --dry-run`, `maildo rules delete rule-id --confirm rule-id`, `maildo rules delete rule-id --confirm rule-id --json`},
			related:  []string{"rules list", "rules view"}, flags: []string{"account", "confirm"}, destructive: true,
		},
	} {
		command := findManifestCommand(t, manifest, tt.path)
		if command.Use != tt.use || command.Short != tt.short || !reflect.DeepEqual(command.Examples, tt.examples) || !reflect.DeepEqual(command.Related, tt.related) ||
			!reflect.DeepEqual(command.OutputModes, resultModes) || !reflect.DeepEqual(ruleMutationManifestFlagNames(command), tt.flags) ||
			!command.Mutates || !command.SupportsDryRun || command.Destructive != tt.destructive || command.RequiresConfirmation != tt.destructive || !command.SupportsMeta {
			t.Fatalf("descriptor %s differs: %#v", tt.path, command)
		}
	}
	for _, tt := range []struct {
		command string
		need    []string
	}{
		{command: "create", need: []string{"only shows what it would create", "--apply", "any value matches", "different conditions must all match"}},
		{command: "update", need: []string{"only shows what would change", "--apply", "replaces its old values"}},
		{command: "enable", need: []string{"Turn an Inbox rule on", "--dry-run"}},
		{command: "disable", need: []string{"Turn an Inbox rule off", "--dry-run"}},
		{command: "delete", need: []string{"Delete an Inbox rule", "--confirm", "--dry-run"}},
	} {
		res := h.run("rules", tt.command, "--help")
		decodeOK(t, res, nil)
		for _, fragment := range tt.need {
			if !strings.Contains(res.Stdout, fragment) {
				t.Fatalf("%s help missing %q: %s", tt.command, fragment, res.Stdout)
			}
		}
	}
}

func ruleMutationManifestFlagNames(command runmanifest.Command) []string {
	names := make([]string, 0, len(command.LocalFlags))
	for _, flag := range command.LocalFlags {
		names = append(names, flag.Name)
	}
	return names
}

// A rule change that reaches the provider is logged, whether it worked or
// not. Previews and failures before the write leave no entry.
func TestRuleMutationActionLog(t *testing.T) {
	writes := map[string]string{
		"success":                     "done",
		"write failure":               "failed",
		"outcome unknown failure":     "",
		"post write read failure":     "failed",
		"invalid post write sequence": "failed",
		"verification failure":        "failed",
		"provider status failure":     "",
	}
	tests := []struct {
		name      string
		rules     func() map[string][]mail.Rule
		configure func(*testkit.FakeProvider)
		args      []string
	}{
		{name: "success", args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "preview", args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "local validation failure", args: []string{"rules", "create", "--mark-read", "--json"}},
		{name: "capability failure", configure: func(f *testkit.FakeProvider) { f.RulesMutateImplemented = false }, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "stale interface failure", configure: func(f *testkit.FakeProvider) { f.RuleMutateClientMissing = true }, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "folder snapshot failure", configure: func(f *testkit.FakeProvider) {
			f.FolderErrByAccount = map[string]error{"work": errors.New("fixture folder failure")}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "rule snapshot failure", configure: func(f *testkit.FakeProvider) {
			f.RuleListErrByAccount = map[string]error{"work": errors.New("fixture rule failure")}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--json"}},
		{name: "not found failure", args: []string{"rules", "update", "missing-rule", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "read only failure", rules: func() map[string][]mail.Rule {
			rules := mutationUpdateRules()
			rules["work"][0].IsReadOnly = true
			return rules
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "confirmation failure", args: []string{"rules", "delete", "rule-1", "--confirm", "wrong", "--json"}},
		{name: "write failure", configure: func(f *testkit.FakeProvider) {
			f.RuleMutationErrByOperation = map[mail.RuleMutationOperation]error{mail.RuleOperationUpdate: errors.New("fixture write failure")}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "outcome unknown failure", configure: func(f *testkit.FakeProvider) {
			f.RuleMutationErrByOperation = map[mail.RuleMutationOperation]error{mail.RuleOperationUpdate: provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeServerUnknown)}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "post write read failure", configure: func(f *testkit.FakeProvider) {
			f.RuleMutationGetErrByAccount = map[string]error{"work": provider.NewRuleReadFailureError(provider.RuleReadFailureTransport, errors.New("safe read failure"))}
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "invalid post write sequence", configure: func(f *testkit.FakeProvider) {
			invalid := 0
			f.RuleMutationPostWriteSequence = &invalid
		}, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "verification failure", configure: func(f *testkit.FakeProvider) { f.RuleMutationSkipStateUpdate = true }, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
		{name: "provider status failure", configure: func(f *testkit.FakeProvider) { f.RuleMutationPostWriteHasError = true }, args: []string{"rules", "update", "rule-1", "--subject-contains", "Receipt", "--apply", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := mutationUpdateRules()
			if tt.rules != nil {
				rules = tt.rules()
			}
			fake := testkit.NewFakeRuleMutationProvider("fake", rules)
			if tt.configure != nil {
				tt.configure(fake)
			}
			h := seededRulesHarnessWithProvider(t, fake, "work")
			_ = h.run(tt.args...)
			outcome, wrote := writes[tt.name]
			if !wrote {
				if actionLogExists(t, h) {
					t.Fatal("rule mutation wrote action log without a write")
				}
				return
			}
			entries := readActionLog(t, h)
			if len(entries) != 1 || entries[0].Change == nil || entries[0].Change.Kind != "rule" || entries[0].Change.ID != "rule-1" || entries[0].Operation != "rule_update" {
				t.Fatalf("entries = %#v", entries)
			}
			if outcome != "" && entries[0].Change.Outcome != outcome {
				t.Fatalf("outcome = %q, want %q", entries[0].Change.Outcome, outcome)
			}
		})
	}
}

func mutationUpdateRules() map[string][]mail.Rule {
	yes := true
	return map[string][]mail.Rule{"work": {{
		ID: "rule-1", DisplayName: "Invoice filing", Sequence: 10, IsEnabled: true,
		Conditions: mail.RulePredicates{
			Semantics: mail.DefaultRulePredicateSemantics(), SubjectContains: []string{"Invoice"}, HasAttachments: &yes,
		},
		Exceptions:            mail.RulePredicates{Semantics: mail.DefaultRulePredicateSemantics(), SubjectContains: []string{"Skip"}},
		Actions:               mail.RuleActions{MarkAsRead: &yes},
		UnsupportedFields:     []mail.RuleUnsupportedField{{Area: "actions", Field: "futureAction", ValueKind: "object", Count: 1, Summary: "safe summary"}},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{ConditionsHasAttachments: true, ActionsMarkAsRead: true},
		ProviderState:         map[string]any{"conditions": map[string]any{"subjectContains": []any{"Invoice"}, "hasAttachments": true}, "actions": map[string]any{"markAsRead": true}},
	}}}
}

func mutationMixedRules() map[string][]mail.Rule {
	rules := mutationUpdateRules()
	rule := rules["work"][0]
	rule.Conditions.FromAddresses = []mail.Recipient{{Name: "Old Sender", Address: "old@example.test"}}
	rule.Actions.MoveToFolder = &mail.RuleFolderTarget{ProviderFolderID: "old-folder", DisplayName: "Old", Path: "Old"}
	rule.ProviderState["conditions"].(map[string]any)["fromAddresses"] = []any{}
	rule.ProviderState["actions"].(map[string]any)["moveToFolder"] = "old-folder"
	rules["work"][0] = rule
	return rules
}

func mutationScalarRules(enabled, hasError bool) map[string][]mail.Rule {
	yes := true
	return map[string][]mail.Rule{"work": {{
		ID: "rule-1", DisplayName: "Invoice filing", Sequence: 10, IsEnabled: enabled, HasError: hasError,
		Conditions:            mail.RulePredicates{Semantics: mail.DefaultRulePredicateSemantics(), SubjectContains: []string{"Invoice"}},
		Exceptions:            mail.RulePredicates{Semantics: mail.DefaultRulePredicateSemantics()},
		Actions:               mail.RuleActions{MarkAsRead: &yes},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{ActionsMarkAsRead: true},
		ProviderState:         map[string]any{"conditions": map[string]any{"subjectContains": []any{"Invoice"}}, "actions": map[string]any{"markAsRead": true}},
	}}}
}

func runMutationPreview(t *testing.T, rules map[string][]mail.Rule, args ...string) map[string]any {
	t.Helper()
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")
	return decodePreview(t, h.run(append([]string{"rules"}, args...)...))
}

func runAppliedRuleMutation(t *testing.T, rules map[string][]mail.Rule, args ...string) testutil.Result {
	t.Helper()
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")
	res := h.run(args...)
	decodeOK(t, res, nil)
	return res
}

func assertJSONGolden(t *testing.T, gotJSON, wantJSON string) {
	t.Helper()
	var got, want bytes.Buffer
	if err := json.Compact(&got, []byte(gotJSON)); err != nil {
		t.Fatalf("compact actual JSON: %v\n%s", err, gotJSON)
	}
	if err := json.Compact(&want, []byte(wantJSON)); err != nil {
		t.Fatalf("compact golden JSON: %v\n%s", err, wantJSON)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("JSON golden mismatch\nwant=%s\ngot =%s", want.Bytes(), got.Bytes())
	}
}

func assertPreviewBody(t *testing.T, preview map[string]any, want map[string]any) {
	t.Helper()
	body, ok := preview["body"].(map[string]any)
	// The command that applies the preview carries the test's --config
	// path, so it is checked for its shape only.
	command, _ := body["command"].(string)
	if !strings.HasPrefix(command, "maildo rules ") || !strings.Contains(command, " --account work") {
		t.Fatalf("preview command = %q", command)
	}
	body = maps.Clone(body)
	delete(body, "command")
	if preview["dry_run"] != true || !ok || !reflect.DeepEqual(body, want) {
		t.Fatalf("preview mismatch\nwant=%#v\ngot =%#v", want, preview)
	}
}

func assertPreviewHumanGolden(t *testing.T, rules map[string][]mail.Rule, args []string, want string) {
	t.Helper()
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	h := seededRulesHarnessWithProvider(t, fake, "work")
	res := h.run(args...)
	decodeOK(t, res, nil)
	if got := strings.ReplaceAll(res.Stdout, h.configPath, "<config>"); got != want {
		t.Fatalf("preview human golden mismatch\nwant=%q\ngot =%q", want, got)
	}
}

func assertRuleInputReason(t *testing.T, res testutil.Result, field, reason string) {
	t.Helper()
	assertErrorCode(t, res, errs.CodeInvalidRuleInput)
	body := errorJSON(t, res)
	if body["field"] != field || body["reason"] != reason {
		t.Fatalf("invalid rule input = %#v, want field=%q reason=%q", body, field, reason)
	}
}

func assertNoRuleProviderWork(t *testing.T, fake *testkit.FakeProvider) {
	t.Helper()
	if fake.CapabilityCalls.Load()+fake.AccountClientCalls.Load()+fake.FolderListCalls.Load()+fake.RuleListCalls.Load()+fake.RuleGetCalls.Load()+
		fake.RuleCreateCalls.Load()+fake.RuleUpdateCalls.Load()+fake.RuleDeleteCalls.Load() != 0 {
		t.Fatalf("provider work caps=%d clients=%d folders=%d list=%d get=%d create=%d update=%d delete=%d",
			fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.FolderListCalls.Load(), fake.RuleListCalls.Load(), fake.RuleGetCalls.Load(),
			fake.RuleCreateCalls.Load(), fake.RuleUpdateCalls.Load(), fake.RuleDeleteCalls.Load())
	}
}

type ruleTestFileFingerprint struct {
	Mode             fs.FileMode
	Size             int64
	ModifiedUnixNano int64
	SHA256           [sha256.Size]byte
}

type ruleMutationCounters struct {
	Capabilities int64
	Clients      int64
	Folders      int64
	RuleLists    int64
	RuleGets     int64
	Creates      int64
	Updates      int64
	Deletes      int64
}

func snapshotRuleMutationCounters(fake *testkit.FakeProvider) ruleMutationCounters {
	return ruleMutationCounters{
		Capabilities: fake.CapabilityCalls.Load(),
		Clients:      fake.AccountClientCalls.Load(),
		Folders:      fake.FolderListCalls.Load(),
		RuleLists:    fake.RuleListCalls.Load(),
		RuleGets:     fake.RuleGetCalls.Load(),
		Creates:      fake.RuleCreateCalls.Load(),
		Updates:      fake.RuleUpdateCalls.Load(),
		Deletes:      fake.RuleDeleteCalls.Load(),
	}
}

func snapshotRuleTestFiles(t *testing.T, root string) map[string]ruleTestFileFingerprint {
	t.Helper()
	out := map[string]ruleTestFileFingerprint{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "auth/.transaction.lock" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fingerprint := ruleTestFileFingerprint{
			Mode:             info.Mode(),
			Size:             info.Size(),
			ModifiedUnixNano: info.ModTime().UnixNano(),
		}
		if relative == "auth" {
			fingerprint.Size = 0
			fingerprint.ModifiedUnixNano = 0
		}
		if info.Mode().IsRegular() {
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fingerprint.SHA256 = sha256.Sum256(contents)
		}
		out[relative] = fingerprint
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot local files: %v", err)
	}
	return out
}

func sortedMapKeys(in map[string]any) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
