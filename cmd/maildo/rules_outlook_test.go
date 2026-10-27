package main

import (
	"encoding/json"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOutlookRuleReadCommands(t *testing.T) {
	h := seededOutlookFoldersHarness(t)

	h.server.resetRuleRequests()
	var list ruleListResult
	decodeOK(t, h.runNoBrowser("rules", "list", "--account", "work", "--json"), &list)
	if list.Count != 2 || len(list.Rules) != 2 || list.Rules[0].Provider != "outlook" || list.Rules[0].Scope != mail.RuleScopeInbox {
		t.Fatalf("list = %#v", list)
	}
	assertOutlookRuleReadsOnly(t, h.server.ruleRequestRecordsSnapshot(), 1)

	h.server.resetRuleRequests()
	var view ruleViewResult
	decodeOK(t, h.runNoBrowser("rules", "view", "rule-move-client", "--account", "work", "--json"), &view)
	if view.Rule.ID != "rule-move-client" || view.Rule.Actions.MoveToFolder == nil || view.Rule.Actions.MoveToFolder.Path != "Clients/ACME" {
		t.Fatalf("view = %#v", view)
	}
	assertOutlookRuleReadsOnly(t, h.server.ruleRequestRecordsSnapshot(), 2)

	h.server.resetRuleRequests()
	var health ruleHealthResult
	decodeOK(t, h.runNoBrowser("rules", "health", "--account", "work", "--json"), &health)
	if health.Count != 2 || health.FindingCounts[string(mail.RuleHealthWarning)] == 0 {
		t.Fatalf("health = %#v", health)
	}
	assertOutlookRuleReadsOnly(t, h.server.ruleRequestRecordsSnapshot(), 1)

	if h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("normal reads used auth UI/refresh token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
}

func TestRuleReadIndependentNormalizedFixtureMatchesOutlook(t *testing.T) {
	outlookHarness := seededOutlookFoldersHarness(t)
	fake := testkit.NewFakeRuleProvider("outlook", map[string][]mail.Rule{"work": independentOutlookRuleFixtures()})
	neutralHarness := newFakeHarness(t, fake)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(neutralHarness.configPath, "work", account); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedCredential(neutralHarness.root, account, nil); err != nil {
		t.Fatal(err)
	}
	var outlookList ruleListResult
	var neutralList ruleListResult
	decodeOK(t, outlookHarness.runNoBrowser("rules", "list", "--json"), &outlookList)
	decodeOK(t, neutralHarness.run("rules", "list", "--json"), &neutralList)
	if !reflect.DeepEqual(outlookList, neutralList) {
		t.Fatalf("independent normalized fixture differs from Outlook\noutlook=%#v\nneutral=%#v", outlookList, neutralList)
	}
	richView := outlookHarness.runNoBrowser("rules", "view", "rule-move-client")
	decodeOK(t, richView, nil)
	checkHumanGolden(t, "testdata/rules_view_rich_human.golden", richView.Stdout)

	commands := [][]string{
		{"rules", "list"},
		{"rules", "list", "--json"},
		{"rules", "list", "--plain"},
		{"rules", "view", "rule-move-client"},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "health"},
		{"rules", "health", "--json"},
		{"rules", "health", "--plain"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			outlookResult := outlookHarness.runNoBrowser(args...)
			neutralResult := neutralHarness.run(args...)
			decodeOK(t, outlookResult, nil)
			decodeOK(t, neutralResult, nil)
			if outlookResult.Stdout != neutralResult.Stdout || outlookResult.Stderr != neutralResult.Stderr {
				t.Fatalf("normalized output differs for %v\noutlook stdout=%q\nneutral stdout=%q\noutlook stderr=%q\nneutral stderr=%q", args, outlookResult.Stdout, neutralResult.Stdout, outlookResult.Stderr, neutralResult.Stderr)
			}
		})
	}
}

func independentOutlookRuleFixtures() []mail.Rule {
	yes := true
	return []mail.Rule{
		{
			ID:          "rule-move-client",
			DisplayName: "Move client mail",
			Sequence:    1,
			IsEnabled:   true,
			Conditions: mail.RulePredicates{
				Semantics:       mail.DefaultRulePredicateSemantics(),
				FromAddresses:   []mail.Recipient{{Name: "Client", Address: "client@example.test"}},
				BodyContains:    []string{"body"},
				SubjectContains: []string{"invoice", "contract"},
			},
			Exceptions: mail.RulePredicates{
				Semantics:       mail.DefaultRulePredicateSemantics(),
				SubjectContains: []string{"skip"},
			},
			Actions: mail.RuleActions{
				MoveToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "acme-id"},
				CopyToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "archive-id"},
				MarkAsRead:          &yes,
				StopProcessingRules: true,
				AssignCategories:    []string{"Review"},
				MarkImportance:      "low",
				ForwardTo:           []mail.Recipient{{Address: "ops@external.test"}},
			},
			UnsupportedFields: []mail.RuleUnsupportedField{
				{Area: "actions", Field: "assignCategories", ValueKind: "array", Count: 1, Summary: "preserved documented provider field"},
				{Area: "actions", Field: "copyToFolder", ValueKind: "string", Count: 1, Summary: "preserved documented provider field"},
				{Area: "actions", Field: "forwardTo", ValueKind: "array", Count: 1, Summary: "preserved documented provider field"},
				{Area: "actions", Field: "markImportance", ValueKind: "string", Count: 1, Summary: "preserved documented provider field"},
				{Area: "actions", Field: "providerAction", ValueKind: "object", Count: 1, Summary: "unknown provider object with 1 field(s)"},
				{Area: "conditions", Field: "bodyContains", ValueKind: "array", Count: 1, Summary: "preserved documented provider field"},
				{Area: "conditions", Field: "providerPredicate", ValueKind: "array", Count: 1, Summary: "unknown provider array with 1 item(s)"},
				{Area: "exceptions", Field: "exceptionFuture", ValueKind: "string", Count: 1, Summary: "unknown provider string"},
			},
			ProviderMeta: map[string]any{"source": "microsoft_graph"},
		},
		{
			ID:          "rule-forward-alert",
			DisplayName: "Forward alerts",
			Sequence:    2,
			IsEnabled:   false,
			Conditions: mail.RulePredicates{
				Semantics:      mail.DefaultRulePredicateSemantics(),
				SenderContains: []string{"alerts"},
				HasAttachments: &yes,
			},
			Exceptions: mail.RulePredicates{Semantics: mail.DefaultRulePredicateSemantics()},
			Actions: mail.RuleActions{
				ForwardTo: []mail.Recipient{{Address: "ops@external.test"}},
			},
			UnsupportedFields: []mail.RuleUnsupportedField{
				{Area: "actions", Field: "forwardTo", ValueKind: "array", Count: 1, Summary: "preserved documented provider field"},
			},
			ProviderMeta: map[string]any{"source": "microsoft_graph"},
		},
	}
}

func TestRuleReadsUseSilentAccountCredentials(t *testing.T) {
	commands := [][]string{
		{"rules", "list", "--account", "work", "--json"},
		{"rules", "view", "rule-move-client", "--account", "work", "--json"},
		{"rules", "health", "--account", "work", "--json"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args[1:2], " "), func(t *testing.T) {
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

			res := h.runNoBrowser(args...)
			decodeOK(t, res, nil)
			if h.server.tokenCalls.Load() != 1 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
				t.Fatalf("silent refresh counters token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
			cache, err := loadCredentialCache(h, "outlook", "work")
			if err != nil {
				t.Fatal(err)
			}
			if cache.AccessToken != "cli-refreshed-access-secret-12345" || cache.RefreshToken != "cli-rotated-refresh-secret-12345" || !cache.ExpiresAt.After(h.now) {
				t.Fatalf("rotated cache was not persisted: %#v", cache)
			}
			info, err := os.Stat(filepath.Join(h.root, "auth", "outlook", "work.json"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("cache mode=%v err=%v", info, err)
			}
			for _, secret := range []string{"seed-outlook-access-token-work", "seed-outlook-refresh-token-work", "cli-refreshed-access-secret-12345", "cli-rotated-refresh-secret-12345"} {
				if strings.Contains(res.Stdout+res.Stderr, secret) {
					t.Fatalf("%v exposed credential sentinel %q", args, secret)
				}
			}
			assertOutlookRuleReadsOnly(t, h.server.ruleRequestRecordsSnapshot(), map[bool]int{true: 2, false: 1}[args[1] == "view"])
		})
	}
}

func TestRuleReadsRejectBrokenCredentialsBeforeProviderCalls(t *testing.T) {
	tests := []struct {
		name      string
		seed      bool
		mutate    func(*testkit.TokenCache)
		configure func(*outlookHarness) error
	}{
		{name: "missing cache"},
		{name: "corrupt cache", seed: true, configure: func(h *outlookHarness) error {
			return os.WriteFile(filepath.Join(h.root, "auth", "outlook", "work.json"), []byte("{"), 0o600)
		}},
		{name: "missing refresh token", seed: true, mutate: func(c *testkit.TokenCache) {
			c.RefreshToken = ""
			c.ExpiresAt = time.Unix(1, 0)
		}},
		{name: "under scoped", seed: true, mutate: func(c *testkit.TokenCache) {
			c.Scopes = []string{"User.Read"}
		}},
		{name: "identity mismatch", seed: true, mutate: func(c *testkit.TokenCache) {
			c.ProviderAccountID = "different-provider-account"
		}},
		{name: "email mismatch fallback", seed: true, mutate: func(c *testkit.TokenCache) {
			c.ProviderAccountID = ""
			c.VerifiedEmail = "different@example.test"
		}},
		{name: "revoked refresh token", seed: true, mutate: func(c *testkit.TokenCache) {
			c.ExpiresAt = time.Unix(1, 0)
		}, configure: func(h *outlookHarness) error {
			h.server.invalidGrant.Store(true)
			return nil
		}},
	}
	commands := [][]string{
		{"rules", "list", "--json"},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "health", "--json"},
	}
	for _, tt := range tests {
		for _, args := range commands {
			t.Run(tt.name+"/"+args[1], func(t *testing.T) {
				h := newOutlookHarness(t)
				account := outlookFoldersAccount()
				if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
					t.Fatal(err)
				}
				if tt.seed {
					if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
						c.VerifiedEmail = account.Email
						c.ProviderAccountID = account.ProviderAccountID
						if tt.mutate != nil {
							tt.mutate(c)
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
				if tt.configure != nil {
					if err := tt.configure(h); err != nil {
						t.Fatal(err)
					}
				}
				res := h.runNoBrowser(args...)
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if strings.TrimSpace(res.Stdout) != "" || h.server.mailFolderCalls.Load() != 0 || h.server.ruleCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
					t.Fatalf("provider/UI work occurred stdout=%q folders=%d rules=%d device=%d browser=%d", res.Stdout, h.server.mailFolderCalls.Load(), h.server.ruleCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
				}
			})
		}
	}
}

func TestRuleReadsMapFolderAndRuleAuthorizationFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, command := range []string{"list", "view", "health"} {
			t.Run(http.StatusText(status)+"/folder/"+command, func(t *testing.T) {
				h := seededOutlookFoldersHarness(t)
				h.server.mailFolderStatus.Store(int64(status))
				args := []string{"rules", command}
				if command == "view" {
					args = append(args, "rule-move-client")
				}
				args = append(args, "--json")
				res := h.runNoBrowser(args...)
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if res.Stdout != "" || h.server.ruleCalls.Load() != 0 || h.browserCalls.Load() != 0 {
					t.Fatalf("folder auth failure leaked output or continued stdout=%q rules=%d browser=%d", res.Stdout, h.server.ruleCalls.Load(), h.browserCalls.Load())
				}
			})
		}

		for _, command := range []string{"list", "health"} {
			t.Run(http.StatusText(status)+"/rule-list/"+command, func(t *testing.T) {
				h := seededOutlookFoldersHarness(t)
				h.server.ruleCollectionStatus.Store(int64(status))
				res := h.runNoBrowser("rules", command, "--json")
				assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
				if res.Stdout != "" || h.server.mailFolderCalls.Load() == 0 || h.server.ruleCalls.Load() != 1 || h.browserCalls.Load() != 0 {
					t.Fatalf("rule auth ordering stdout=%q folders=%d rules=%d browser=%d", res.Stdout, h.server.mailFolderCalls.Load(), h.server.ruleCalls.Load(), h.browserCalls.Load())
				}
			})
		}

		t.Run(http.StatusText(status)+"/rule-get/view", func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			h.server.setRuleReadStatus("rule-move-client", status)
			res := h.runNoBrowser("rules", "view", "rule-move-client", "--json")
			assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
			if res.Stdout != "" || h.server.mailFolderCalls.Load() == 0 || h.server.ruleCalls.Load() != 1 || h.browserCalls.Load() != 0 {
				t.Fatalf("rule get auth ordering stdout=%q folders=%d rules=%d browser=%d", res.Stdout, h.server.mailFolderCalls.Load(), h.server.ruleCalls.Load(), h.browserCalls.Load())
			}
		})
	}
}

func TestRuleReadsKeepMultiAccountRefreshAllOrNothing(t *testing.T) {
	newHarness := func(t *testing.T, failRefresh bool) *outlookHarness {
		h := newOutlookHarness(t)
		work := outlookFoldersAccount()
		personal := maildoconfig.Account{
			Name: "personal", Provider: "outlook", Email: "personal@example.test", DisplayName: "Personal",
			ProviderAccountID: "outlook-personal", AuthRef: "outlook/personal",
		}
		if err := testkit.SeedRegistry(h.configPath, "work", work, personal); err != nil {
			t.Fatal(err)
		}
		if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
			c.VerifiedEmail = work.Email
			c.ProviderAccountID = work.ProviderAccountID
		}); err != nil {
			t.Fatal(err)
		}
		if err := testkit.SeedOutlookCache(h.root, "personal", func(c *testkit.TokenCache) {
			c.VerifiedEmail = personal.Email
			c.ProviderAccountID = personal.ProviderAccountID
			c.ExpiresAt = h.now.Add(-time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
		h.server.invalidGrant.Store(failRefresh)
		return h
	}

	t.Run("second account refresh succeeds", func(t *testing.T) {
		h := newHarness(t, false)
		var list ruleListResult
		decodeOK(t, h.runNoBrowser("rules", "list", "--account", "work", "--account", "personal", "--json"), &list)
		if list.Count != 4 || h.server.tokenCalls.Load() != 1 || h.browserCalls.Load() != 0 {
			t.Fatalf("list=%#v token=%d browser=%d", list, h.server.tokenCalls.Load(), h.browserCalls.Load())
		}
		for i, account := range []string{"work", "work", "personal", "personal"} {
			if list.Rules[i].Account != account {
				t.Fatalf("row %d account=%q, want %q", i, list.Rules[i].Account, account)
			}
		}
	})

	t.Run("second account refresh fails without partial output", func(t *testing.T) {
		h := newHarness(t, true)
		res := h.runNoBrowser("rules", "list", "--account", "work", "--account", "personal", "--json")
		assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
		if res.Stdout != "" || h.server.tokenCalls.Load() != 1 || h.browserCalls.Load() != 0 {
			t.Fatalf("stdout=%q token=%d browser=%d", res.Stdout, h.server.tokenCalls.Load(), h.browserCalls.Load())
		}
	})
}

func TestOutlookRuleMutationCommands(t *testing.T) {
	h := seededOutlookFoldersHarness(t)

	h.server.resetRuleRequests()
	preview := decodePreview(t, h.runNoBrowser("rules", "create",
		"--subject-contains", "Receipt",
		"--move-to", "Archive",
		"--json",
	))
	previewBody := preview["body"].(map[string]any)
	if previewBody["operation"] != string(mail.RuleOperationCreate) ||
		previewBody["move_to_folder_after_id"] != "archive-id" ||
		previewBody["move_to_folder_before"] != "absent" ||
		hasRuleWriteRequest(h.server.ruleRequestRecordsSnapshot()) {
		t.Fatalf("preview body=%#v requests=%#v", previewBody, h.server.ruleRequestRecordsSnapshot())
	}

	h.server.resetRuleRequests()
	var created ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "create",
		"--from", "Client <new-client@example.test>",
		"--subject-contains", "Receipt",
		"--move-to", "Archive",
		"--mark-read",
		"--stop-processing",
		"--apply",
		"--json",
	), &created)
	if created.Provider != "outlook" || created.RuleID == "" ||
		created.Verification == nil || created.Verification.Status != mail.RuleVerificationVerified {
		t.Fatalf("created = %#v", created)
	}
	post := firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPost, "/me/mailFolders/inbox/messageRules")
	postBody := ruleRequestBody(t, post)
	conditions := postBody["conditions"].(map[string]any)
	actions := postBody["actions"].(map[string]any)
	from := conditions["fromAddresses"].([]any)[0].(map[string]any)["emailAddress"].(map[string]any)
	if from["address"] != "new-client@example.test" ||
		conditions["subjectContains"].([]any)[0] != "Receipt" ||
		actions["moveToFolder"] != "archive-id" ||
		actions["markAsRead"] != true ||
		actions["stopProcessingRules"] != true {
		t.Fatalf("create payload = %#v", postBody)
	}

	h.server.resetRuleRequests()
	var updated ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "update", "rule-move-client",
		"--subject-contains", "updated",
		"--move-to", "Archive",
		"--no-mark-read",
		"--apply",
		"--json",
	), &updated)
	if updated.Operation != string(mail.RuleOperationUpdate) ||
		updated.Verification == nil || updated.Verification.Status != mail.RuleVerificationVerified ||
		updated.ChangeSet == nil || updated.ChangeSet.MarkAsRead == nil || !updated.ChangeSet.MarkAsRead.Cleared {
		t.Fatalf("updated = %#v", updated)
	}
	patch := firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client")
	patchBody := ruleRequestBody(t, patch)
	patchedConditions := patchBody["conditions"].(map[string]any)
	patchedActions := patchBody["actions"].(map[string]any)
	if patchedConditions["providerPredicate"] == nil ||
		patchedConditions["bodyContains"] == nil ||
		patchedConditions["subjectContains"].([]any)[0] != "updated" ||
		patchedActions["providerAction"] == nil ||
		patchedActions["assignCategories"] == nil ||
		patchedActions["copyToFolder"] == nil ||
		patchedActions["forwardTo"] == nil ||
		patchedActions["markImportance"] == nil ||
		patchedActions["moveToFolder"] != "archive-id" {
		t.Fatalf("patch payload did not preserve provider state: %#v", patchBody)
	}
	if _, ok := patchedActions["markAsRead"]; ok {
		t.Fatalf("patch payload kept cleared markAsRead: %#v", patchBody)
	}

	h.server.resetRuleRequests()
	var disabled ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "disable", "rule-move-client", "--json"), &disabled)
	disablePatch := ruleRequestBody(t, firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client"))
	if disabled.Operation != string(mail.RuleOperationDisable) ||
		disabled.Verification == nil || disabled.Verification.Status != mail.RuleVerificationVerified ||
		disablePatch["isEnabled"] != false ||
		disablePatch["conditions"] != nil || disablePatch["actions"] != nil {
		t.Fatalf("disable result=%#v payload=%#v", disabled, disablePatch)
	}

	h.server.resetRuleRequests()
	var enabled ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "enable", "rule-move-client", "--json"), &enabled)
	enablePatch := ruleRequestBody(t, firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client"))
	if enabled.Operation != string(mail.RuleOperationEnable) ||
		enabled.Verification == nil || enabled.Verification.Status != mail.RuleVerificationVerified ||
		enablePatch["isEnabled"] != true {
		t.Fatalf("enable result=%#v payload=%#v", enabled, enablePatch)
	}

	assertErrorCode(t, h.runNoBrowser("rules", "delete", "rule-forward-alert", "--json"), errs.CodeConfirmationRequired)
	var deleted ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "delete", "rule-forward-alert", "--confirm", "rule-forward-alert", "--json"), &deleted)
	if deleted.Operation != string(mail.RuleOperationDelete) ||
		deleted.Verification == nil || deleted.Verification.Status != mail.RuleVerificationVerified ||
		!deleted.ConfirmationRequired || !deleted.ConfirmationMatched {
		t.Fatalf("deleted = %#v", deleted)
	}
	assertErrorCodeExit(t, h.runNoBrowser("rules", "view", "rule-forward-alert", "--account", "work", "--json"), errs.CodeRuleNotFound, 5)
}

func TestRuleUpdateConcurrentProviderEditCharacterization(t *testing.T) {
	t.Run("surviving scalar sibling is detected", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.setRuleBeforePatch(func(_ *cliOutlookServer, _ string, rule map[string]any) {
			conditions := rule["conditions"].(map[string]any)
			conditions["providerConcurrent"] = []any{"survives"}
		})
		h.server.resetRuleRequests()
		res := h.runNoBrowser("rules", "disable", "rule-move-client", "--json")
		assertErrorCode(t, res, errs.CodeVerificationFailed)
		if res.Stdout != "" {
			t.Fatalf("concurrent scalar mismatch wrote stdout %q", res.Stdout)
		}
		patch := ruleRequestBody(t, firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client"))
		if patch["isEnabled"] != false || patch["conditions"] != nil || patch["actions"] != nil {
			t.Fatalf("scalar patch carried complex state: %#v", patch)
		}
		stored, ok := h.server.graphRuleByID("rule-move-client")
		conditions, _ := stored["conditions"].(map[string]any)
		if !ok || conditions["providerConcurrent"] == nil {
			t.Fatalf("concurrent scalar sibling did not survive: %#v", stored)
		}
	})

	t.Run("stale complex replacement can lose an edit", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.setRuleBeforePatch(func(_ *cliOutlookServer, _ string, rule map[string]any) {
			conditions := rule["conditions"].(map[string]any)
			conditions["bodyContains"] = []any{"concurrent-provider-edit"}
		})
		h.server.resetRuleRequests()
		var updated ruleMutationResult
		decodeOK(t, h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "maildo-edit", "--apply", "--json"), &updated)
		if updated.Verification == nil || updated.Verification.Status != mail.RuleVerificationVerified {
			t.Fatalf("complex update result=%#v", updated)
		}
		patch := ruleRequestBody(t, firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client"))
		conditions := patch["conditions"].(map[string]any)
		if !reflect.DeepEqual(conditions["bodyContains"], []any{"body"}) {
			t.Fatalf("patch did not carry stale reconstructed sibling: %#v", conditions)
		}
		stored, ok := h.server.graphRuleByID("rule-move-client")
		storedConditions, _ := stored["conditions"].(map[string]any)
		if !ok || !reflect.DeepEqual(storedConditions["bodyContains"], []any{"body"}) || reflect.DeepEqual(storedConditions["bodyContains"], []any{"concurrent-provider-edit"}) {
			t.Fatalf("complex replacement did not characterize lost edit: %#v", stored)
		}
	})

	t.Run("folder rename remains snapshot derived", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.setRuleBeforePatch(func(server *cliOutlookServer, _ string, _ map[string]any) {
			server.archiveDisplayName = "Archive renamed concurrently"
		})
		var updated ruleMutationResult
		decodeOK(t, h.runNoBrowser("rules", "update", "rule-move-client", "--move-to", "Archive", "--apply", "--json"), &updated)
		if updated.ResolvedMoveTarget == nil || updated.ResolvedMoveTarget.ID != "archive-id" || updated.ResolvedMoveTarget.DisplayName != "Archive" || updated.ResolvedMoveTarget.Path != "Archive" {
			t.Fatalf("mutation output did not retain folder snapshot: %#v", updated.ResolvedMoveTarget)
		}

		var folders folderListResult
		decodeOK(t, h.runNoBrowser("folders", "list", "--json"), &folders)
		var renamed *mail.Folder
		for i := range folders.Folders {
			if folders.Folders[i].ID == "archive-id" {
				renamed = &folders.Folders[i]
				break
			}
		}
		if renamed == nil || renamed.DisplayName != "Archive renamed concurrently" {
			t.Fatalf("fresh folder snapshot did not observe rename: %#v", renamed)
		}
	})
}

func TestOutlookRuleMutationVerificationFailures(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.skipRuleStateUpdate.Store(true)
		res := h.runNoBrowser("rules", "update", "rule-move-client",
			"--subject-contains", "drift",
			"--apply",
			"--json",
		)
		assertErrorCode(t, res, errs.CodeVerificationFailed)
	})

	t.Run("provider read error", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.setRuleReadStatus("rule-move-client", http.StatusInternalServerError)
		res := h.runNoBrowser("rules", "disable", "rule-move-client", "--json")
		assertErrorCode(t, res, errs.CodeVerificationFailed)
	})

	t.Run("delete gone verifies", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.setRuleReadStatus("rule-forward-alert", http.StatusGone)
		var deleted ruleMutationResult
		decodeOK(t, h.runNoBrowser("rules", "delete", "rule-forward-alert", "--confirm", "rule-forward-alert", "--json"), &deleted)
		if deleted.Verification == nil || deleted.Verification.Status != mail.RuleVerificationVerified {
			t.Fatalf("deleted = %#v", deleted)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		h.server.ruleWriteStatus.Store(http.StatusInternalServerError)
		res := h.runNoBrowser("rules", "update", "rule-move-client",
			"--subject-contains", "write-fails",
			"--apply",
			"--json",
		)
		assertErrorCode(t, res, errs.CodeVerificationFailed)
		body := errorJSON(t, res)
		if body["verification_cause"] != string(mail.RuleVerificationCauseMismatch) ||
			body["reason"] != string(provider.RuleMutationOutcomeServerUnknown) ||
			body["operation"] != string(mail.RuleOperationUpdate) {
			t.Fatalf("write failure body = %#v", body)
		}
		_ = firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client")
	})
}

func TestOutlookRuleMutationEmptyConditionStateThroughHandler(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.mu.Lock()
	h.server.ruleState["rule-no-conditions"] = graphRuleFixture("rule-no-conditions", "No conditions", 3, true,
		map[string]any{},
		map[string]any{"moveToFolder": "archive-id"},
	)
	h.server.mu.Unlock()

	h.server.resetRuleRequests()
	var updated ruleMutationResult
	decodeOK(t, h.runNoBrowser("rules", "update", "rule-no-conditions",
		"--subject-contains", "receipt",
		"--apply",
		"--json",
	), &updated)
	if updated.Verification == nil || updated.Verification.Status != mail.RuleVerificationVerified {
		t.Fatalf("updated = %#v", updated)
	}
	patch := firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-no-conditions")
	body := ruleRequestBody(t, patch)
	conditions := body["conditions"].(map[string]any)
	if got := conditions["subjectContains"].([]any); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("conditions payload = %#v", body)
	}
	if _, ok := body["actions"]; ok {
		t.Fatalf("condition-only update sent actions: %#v", body)
	}
}

func TestOutlookRulePresenceOnlyClearPreviewAndPatch(t *testing.T) {
	for _, tt := range []struct {
		name       string
		flag       string
		previewKey string
		object     string
		graphKey   string
		value      any
		absent     bool
	}{
		{name: "has attachments false", flag: "--no-has-attachments", previewKey: "has_attachments", object: "conditions", graphKey: "hasAttachments", value: false},
		{name: "has attachments null", flag: "--no-has-attachments", previewKey: "has_attachments", object: "conditions", graphKey: "hasAttachments", value: nil},
		{name: "has attachments absent", flag: "--no-has-attachments", previewKey: "has_attachments", object: "conditions", graphKey: "hasAttachments", absent: true},
		{name: "mark as read false", flag: "--no-mark-read", previewKey: "mark_as_read", object: "actions", graphKey: "markAsRead", value: false},
		{name: "mark as read null", flag: "--no-mark-read", previewKey: "mark_as_read", object: "actions", graphKey: "markAsRead", value: nil},
		{name: "mark as read absent", flag: "--no-mark-read", previewKey: "mark_as_read", object: "actions", graphKey: "markAsRead", absent: true},
		{name: "stop processing false", flag: "--no-stop-processing", previewKey: "stop_processing_rules", object: "actions", graphKey: "stopProcessingRules", value: false},
		{name: "stop processing null", flag: "--no-stop-processing", previewKey: "stop_processing_rules", object: "actions", graphKey: "stopProcessingRules", value: nil},
		{name: "stop processing absent", flag: "--no-stop-processing", previewKey: "stop_processing_rules", object: "actions", graphKey: "stopProcessingRules", absent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			conditions := map[string]any{"subjectContains": []any{"fixture"}}
			actions := map[string]any{"moveToFolder": "archive-id"}
			if !tt.absent {
				if tt.object == "conditions" {
					conditions[tt.graphKey] = tt.value
				} else {
					actions[tt.graphKey] = tt.value
				}
			}
			h.server.mu.Lock()
			h.server.ruleState["presence-rule"] = graphRuleFixture("presence-rule", "Presence", 3, true, conditions, actions)
			h.server.mu.Unlock()

			previewResult := h.runNoBrowser("rules", "update", "presence-rule", tt.flag, "--json")
			if tt.absent {
				assertRuleInputReason(t, previewResult, "changes", "no_changes")
				if hasRuleWriteRequest(h.server.ruleRequestRecordsSnapshot()) {
					t.Fatalf("absent clear wrote: %#v", h.server.ruleRequestRecordsSnapshot())
				}
				return
			}
			preview := decodePreview(t, previewResult)
			body := preview["body"].(map[string]any)
			wantBefore := "false"
			if tt.value == nil {
				wantBefore = "present"
			}
			if body[tt.previewKey+"_before"] != wantBefore || body[tt.previewKey+"_after"] != "absent" {
				t.Fatalf("presence preview = %#v", body)
			}

			h.server.resetRuleRequests()
			var applied ruleMutationResult
			decodeOK(t, h.runNoBrowser("rules", "update", "presence-rule", tt.flag, "--apply", "--json"), &applied)
			if applied.Verification == nil || applied.Verification.Status != mail.RuleVerificationVerified {
				t.Fatalf("presence result = %#v", applied)
			}
			patch := ruleRequestBody(t, firstRuleRequest(t, h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/presence-rule"))
			object := patch[tt.object].(map[string]any)
			if _, exists := object[tt.graphKey]; exists {
				t.Fatalf("clear retained %s: %#v", tt.graphKey, patch)
			}
		})
	}
}

func TestOutlookRuleCreateUnusableAcceptedResponseVerificationFailure(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*cliOutlookServer)
		reason    provider.RuleMutationOutcomeUnknownReason
		cause     mail.RuleVerificationCause
	}{
		{name: "missing id", configure: func(s *cliOutlookServer) { s.missingRuleMutationID.Store(true) }, reason: provider.RuleMutationOutcomeMissingID, cause: mail.RuleVerificationCauseMalformedResponse},
		{name: "malformed", configure: func(s *cliOutlookServer) { s.malformedRules.Store(true) }, reason: provider.RuleMutationOutcomeMalformed, cause: mail.RuleVerificationCauseMalformedResponse},
		{name: "oversized", configure: func(s *cliOutlookServer) { s.oversizedRules.Store(true) }, reason: provider.RuleMutationOutcomeOversized, cause: mail.RuleVerificationCauseOversizedResponse},
		{name: "unreadable", configure: func(s *cliOutlookServer) { s.unreadableRuleMutationBody.Store(true) }, reason: provider.RuleMutationOutcomeUnreadable, cause: mail.RuleVerificationCauseTransport},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			tt.configure(h.server)
			res := h.runNoBrowser("rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json")
			assertErrorCode(t, res, errs.CodeVerificationFailed)
			body := errorJSON(t, res)
			if body["reason"] != string(tt.reason) || body["verification_cause"] != string(tt.cause) || body["operation"] != string(mail.RuleOperationCreate) {
				t.Fatalf("create outcome body = %#v", body)
			}
			if _, ok := body["rule_id"]; ok {
				t.Fatalf("create outcome exposed id: %#v", body)
			}
			requests := h.server.ruleRequestRecordsSnapshot()
			if countRuleRequests(requests, http.MethodPost, "/me/mailFolders/inbox/messageRules") != 1 || countRuleTargetGets(requests) != 0 {
				t.Fatalf("create outcome requests = %#v", requests)
			}
		})
	}
}

func TestOutlookRuleMutationTransportOutcomeVerification(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		wantError bool
	}{
		{name: "create has no usable id", args: []string{"rules", "create", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json"}, wantError: true},
		{name: "update verifies known id", args: []string{"rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json"}},
		{name: "enable verifies known id", args: []string{"rules", "enable", "rule-forward-alert", "--json"}},
		{name: "disable verifies known id", args: []string{"rules", "disable", "rule-move-client", "--json"}},
		{name: "delete verifies absence", args: []string{"rules", "delete", "rule-forward-alert", "--confirm", "rule-forward-alert", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			h.server.ruleWriteDisconnect.Store(true)
			res := h.runNoBrowser(tt.args...)
			if tt.wantError {
				assertErrorCode(t, res, errs.CodeVerificationFailed)
				body := errorJSON(t, res)
				if body["reason"] != string(provider.RuleMutationOutcomeTransportUnknown) || body["verification_cause"] != string(mail.RuleVerificationCauseTransport) {
					t.Fatalf("transport create body = %#v", body)
				}
			} else {
				var got ruleMutationResult
				decodeOK(t, res, &got)
				if got.Status != mail.RuleMutationStatusApplied || got.Verification == nil || got.Verification.Status != mail.RuleVerificationVerified {
					t.Fatalf("transport verified result = %#v", got)
				}
			}
			requests := h.server.ruleRequestRecordsSnapshot()
			writes := 0
			for _, req := range requests {
				if req.Method != http.MethodGet {
					writes++
				}
			}
			if writes != 1 {
				t.Fatalf("transport writes=%d requests=%#v", writes, requests)
			}
		})
	}
}

func TestOutlookRulePatchResponseNeverOverridesVerifiedState(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.conflictingRuleResponse.Store(true)
	res := h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json")
	var got ruleMutationResult
	decodeOK(t, res, &got)
	if got.RuleID != "rule-move-client" || got.DisplayName != "Move client mail" || got.Sequence == nil || *got.Sequence != 1 {
		t.Fatalf("response overrode verified state: %#v", got)
	}
	for _, forbidden := range []string{"response-only-id", "response-only-name", "response-secret-source", "2147483647"} {
		if strings.Contains(res.Stdout+res.Stderr, forbidden) {
			t.Fatalf("response field %q rendered: %#v", forbidden, res)
		}
	}
	if countRuleTargetGets(h.server.ruleRequestRecordsSnapshot()) != 1 {
		t.Fatalf("patch target GETs = %#v", h.server.ruleRequestRecordsSnapshot())
	}
}

func TestOutlookRuleMutationVerificationDetectsDroppedState(t *testing.T) {
	for _, path := range []string{"conditions.bodyContains", "actions.assignCategories", "exceptions.subjectContains", "actions.providerAction"} {
		t.Run(path, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			h.server.rulePostWriteDrop.Store(path)
			res := h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json")
			assertErrorCode(t, res, errs.CodeVerificationFailed)
			body := errorJSON(t, res)
			if body["verification_cause"] != string(mail.RuleVerificationCauseMismatch) || body["mismatch_count"].(float64) < 1 {
				t.Fatalf("dropped state body = %#v", body)
			}
			if countRuleRequests(h.server.ruleRequestRecordsSnapshot(), http.MethodPatch, "/me/mailFolders/inbox/messageRules/rule-move-client") != 1 || countRuleTargetGets(h.server.ruleRequestRecordsSnapshot()) != 1 {
				t.Fatalf("dropped state requests = %#v", h.server.ruleRequestRecordsSnapshot())
			}
		})
	}
}

func TestOutlookRuleClearVerificationRequiresKeyAbsence(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.mu.Lock()
	h.server.ruleState["clear-rule"] = graphRuleFixture("clear-rule", "Clear", 3, true,
		map[string]any{"subjectContains": []any{"fixture"}},
		map[string]any{"moveToFolder": "archive-id", "markAsRead": false},
	)
	h.server.mu.Unlock()
	h.server.retainClearedRuleKeys.Store(true)
	res := h.runNoBrowser("rules", "update", "clear-rule", "--no-mark-read", "--apply", "--json")
	assertErrorCode(t, res, errs.CodeVerificationFailed)
	body := errorJSON(t, res)
	if body["verification_cause"] != string(mail.RuleVerificationCauseMismatch) || body["mismatch_count"].(float64) < 1 {
		t.Fatalf("retained clear body = %#v", body)
	}
}

func TestOutlookRuleMutationReadBackFailurePrecedence(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.ruleWriteStatus.Store(http.StatusInternalServerError)
	h.server.setRuleReadStatus("rule-move-client", http.StatusUnauthorized)
	res := h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json")
	assertErrorCode(t, res, errs.CodeVerificationFailed)
	body := errorJSON(t, res)
	if body["reason"] != string(provider.RuleMutationOutcomeServerUnknown) || body["verification_cause"] != string(mail.RuleVerificationCauseAuthRepairRequired) || body["repair_command"] != "maildo auth repair work" {
		t.Fatalf("read-back precedence body = %#v", body)
	}
	if _, ok := body["mismatch_count"]; ok {
		t.Fatalf("read-back error has mismatch count: %#v", body)
	}
}

func TestGraphReadRefactorPreservesRuleVerificationFailureReasons(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*cliOutlookServer)
		cause     mail.RuleVerificationCause
	}{
		{name: "not found", configure: func(s *cliOutlookServer) { s.setRuleReadStatus("rule-move-client", http.StatusNotFound) }, cause: mail.RuleVerificationCauseNotFound},
		{name: "auth repair", configure: func(s *cliOutlookServer) { s.setRuleReadStatus("rule-move-client", http.StatusForbidden) }, cause: mail.RuleVerificationCauseAuthRepairRequired},
		{name: "rate limited", configure: func(s *cliOutlookServer) { s.setRuleReadStatus("rule-move-client", http.StatusTooManyRequests) }, cause: mail.RuleVerificationCauseRateLimited},
		{name: "provider api", configure: func(s *cliOutlookServer) { s.setRuleReadStatus("rule-move-client", http.StatusBadGateway) }, cause: mail.RuleVerificationCauseProviderAPI},
		{name: "identity mismatch", configure: func(s *cliOutlookServer) { s.conflictingRuleReadID.Store(true) }, cause: mail.RuleVerificationCauseProviderAPI},
		{name: "malformed", configure: func(s *cliOutlookServer) { s.malformedRuleRead.Store(true) }, cause: mail.RuleVerificationCauseMalformedResponse},
		{name: "oversized", configure: func(s *cliOutlookServer) { s.oversizedRuleRead.Store(true) }, cause: mail.RuleVerificationCauseOversizedResponse},
		{name: "transport", configure: func(s *cliOutlookServer) { s.ruleReadDisconnect.Store(true) }, cause: mail.RuleVerificationCauseTransport},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			tt.configure(h.server)
			res := h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json")
			assertErrorCode(t, res, errs.CodeVerificationFailed)
			if res.Stdout != "" {
				t.Fatalf("read verification failure wrote stdout: %q", res.Stdout)
			}
			body := errorJSON(t, res)
			if body["verification_cause"] != string(tt.cause) || body["operation"] != string(mail.RuleOperationUpdate) {
				t.Fatalf("read reason body = %#v", body)
			}
			if _, ok := body["mismatch_count"]; ok {
				t.Fatalf("read reason has mismatch count: %#v", body)
			}
		})
	}
}

func TestRuleMutationVerificationAndProviderStatusPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*cliOutlookServer)
		wantCause mail.RuleVerificationCause
		wantOK    bool
		readOnly  bool
	}{
		{name: "matching provider error", configure: func(s *cliOutlookServer) { s.rulePostWriteHasError.Store(true) }, wantCause: mail.RuleVerificationCauseProviderReportedError},
		{name: "matching read only", configure: func(s *cliOutlookServer) { s.rulePostWriteReadOnly.Store(true) }, wantOK: true, readOnly: true},
		{name: "identity failure outranks provider status", configure: func(s *cliOutlookServer) {
			s.conflictingRuleReadID.Store(true)
			s.rulePostWriteHasError.Store(true)
		}, wantCause: mail.RuleVerificationCauseProviderAPI},
		{name: "decode failure outranks provider status", configure: func(s *cliOutlookServer) {
			s.malformedRuleRead.Store(true)
			s.rulePostWriteReadOnly.Store(true)
		}, wantCause: mail.RuleVerificationCauseMalformedResponse},
		{name: "mismatch outranks provider error", configure: func(s *cliOutlookServer) {
			s.rulePostWriteDrop.Store("exceptions.subjectContains")
			s.rulePostWriteHasError.Store(true)
		}, wantCause: mail.RuleVerificationCauseMismatch},
		{name: "mismatch outranks read only", configure: func(s *cliOutlookServer) {
			s.rulePostWriteDrop.Store("exceptions.subjectContains")
			s.rulePostWriteReadOnly.Store(true)
		}, wantCause: mail.RuleVerificationCauseMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookFoldersHarness(t)
			tt.configure(h.server)
			res := h.runNoBrowser("rules", "update", "rule-move-client", "--subject-contains", "changed", "--apply", "--json")
			if tt.wantOK {
				var got ruleMutationResult
				decodeOK(t, res, &got)
				if got.ProviderStatus == nil || got.ProviderStatus.IsReadOnly != tt.readOnly || len(got.ProviderStatus.Warnings) != 1 {
					t.Fatalf("provider status success = %#v", got)
				}
				return
			}
			assertErrorCode(t, res, errs.CodeVerificationFailed)
			if res.Stdout != "" {
				t.Fatalf("precedence failure wrote stdout: %q", res.Stdout)
			}
			if body := errorJSON(t, res); body["verification_cause"] != string(tt.wantCause) {
				t.Fatalf("precedence body = %#v", body)
			}
		})
	}
}

func TestOutlookRuleMutationMetadataWhitelist(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	h.server.conflictingRuleResponse.Store(true)
	res := h.runNoBrowser("rules", "create", "--name", "Safe name", "--subject-contains", "Receipt", "--mark-read", "--apply", "--json")
	var got map[string]any
	decodeOK(t, res, &got)
	metadata := got["provider_metadata"].(map[string]any)
	if !reflect.DeepEqual(metadata, map[string]any{"source": "microsoft_graph"}) {
		t.Fatalf("metadata = %#v", metadata)
	}
	if strings.Contains(res.Stdout+res.Stderr, "response-secret-source") || strings.Contains(res.Stdout+res.Stderr, "provider_metadata.extra") {
		t.Fatalf("response metadata rendered: %#v", res)
	}
}

func TestRuleMutationProviderSourceContract(t *testing.T) {
	outlookHarness := seededOutlookFoldersHarness(t)
	var outlookResult ruleMutationResult
	decodeOK(t, outlookHarness.runNoBrowser("rules", "enable", "rule-forward-alert", "--json"), &outlookResult)
	if outlookResult.ProviderMetadata.Source != mail.RuleMutationSourceMicrosoftGraph {
		t.Fatalf("Outlook source = %#v", outlookResult.ProviderMetadata)
	}

	rules := mutationScalarRules(false, false)
	rules["work"][0].ProviderMeta = map[string]any{"source": "unknown\r\nsecret-token", "extra": "secret-token"}
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	fake.RuleMutationReceiptOperation = mail.RuleOperationDelete
	fake.RuleMutationReceiptMetadata = mail.RuleMutationProviderMetadata{Source: mail.RuleMutationSourceMicrosoftGraph}
	fakeHarness := seededRulesHarnessWithProvider(t, fake, "work")
	res := fakeHarness.run("rules", "enable", "rule-1", "--json")
	var fakeResult ruleMutationResult
	decodeOK(t, res, &fakeResult)
	if fakeResult.Operation != string(mail.RuleOperationEnable) || fakeResult.ProviderMetadata.Source != mail.RuleMutationSourceFake || strings.Contains(res.Stdout+res.Stderr, "secret-token") {
		t.Fatalf("fake source/result = %#v / %#v", fakeResult.ProviderMetadata, res)
	}
}

func TestOutlookRuleMutationAuthPersistence(t *testing.T) {
	t.Run("fresh credentials unchanged", func(t *testing.T) {
		h := seededOutlookFoldersHarness(t)
		configBefore, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatal(err)
		}
		cachePath := filepath.Join(h.root, "auth", "outlook", "work.json")
		cacheBefore, err := os.ReadFile(cachePath)
		if err != nil {
			t.Fatal(err)
		}
		decodeOK(t, h.runNoBrowser("rules", "enable", "rule-forward-alert", "--json"), nil)
		configAfter, _ := os.ReadFile(h.configPath)
		cacheAfter, _ := os.ReadFile(cachePath)
		if !reflect.DeepEqual(configBefore, configAfter) || !reflect.DeepEqual(cacheBefore, cacheAfter) || h.server.tokenCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
			t.Fatalf("fresh auth changed config/cache or used interaction token=%d device=%d browser=%d", h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
		}
		if entries := readActionLog(t, h.cliHarness); len(entries) != 1 || entries[0].Change == nil || entries[0].Change.ID != "rule-forward-alert" {
			t.Fatalf("fresh auth mutation log = %#v", entries)
		}
	})

	t.Run("near expiry silently refreshes once", func(t *testing.T) {
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
		configBefore, _ := os.ReadFile(h.configPath)
		decodeOK(t, h.runNoBrowser("rules", "enable", "rule-forward-alert", "--json"), nil)
		configAfter, _ := os.ReadFile(h.configPath)
		cache, err := loadCredentialCache(h, "outlook", "work")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(h.root, "auth", "outlook", "work.json"))
		if err != nil || info.Mode().Perm() != 0o600 || !reflect.DeepEqual(configBefore, configAfter) || h.server.tokenCalls.Load() != 1 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 || cache.RefreshToken != "cli-rotated-refresh-secret-12345" {
			t.Fatalf("refresh persistence cache=%#v info=%v err=%v token=%d device=%d browser=%d", cache, info, err, h.server.tokenCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
		}
	})

	for _, tt := range []struct {
		name      string
		configure func(*outlookHarness, maildoconfig.Account)
	}{
		{name: "missing", configure: func(*outlookHarness, maildoconfig.Account) {}},
		{name: "corrupt", configure: func(h *outlookHarness, _ maildoconfig.Account) {
			path := filepath.Join(h.root, "auth", "outlook")
			if err := os.MkdirAll(path, 0o700); err != nil {
				h.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "work.json"), []byte("{"), 0o600); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "under scoped", configure: func(h *outlookHarness, account maildoconfig.Account) {
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
				c.Scopes = []string{"User.Read"}
			}); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "failed refresh", configure: func(h *outlookHarness, account maildoconfig.Account) {
			if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
				c.VerifiedEmail = account.Email
				c.ProviderAccountID = account.ProviderAccountID
				c.ExpiresAt = h.now.Add(-time.Minute)
			}); err != nil {
				h.t.Fatal(err)
			}
			h.server.invalidGrant.Store(true)
		}},
	} {
		t.Run(tt.name+" fails before write", func(t *testing.T) {
			h := newOutlookHarness(t)
			account := outlookFoldersAccount()
			if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
				t.Fatal(err)
			}
			tt.configure(h, account)
			res := h.runNoBrowser("rules", "enable", "rule-forward-alert", "--json")
			assertErrorCodeExit(t, res, errs.CodeAuthRepairRequired, 3)
			if hasRuleWriteRequest(h.server.ruleRequestRecordsSnapshot()) || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 || actionLogExists(t, h.cliHarness) {
				t.Fatalf("broken auth reached write/interaction requests=%#v device=%d browser=%d", h.server.ruleRequestRecordsSnapshot(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
			}
		})
	}
}

func countRuleRequests(requests []ruleRequest, method, path string) int {
	count := 0
	for _, req := range requests {
		if req.Method == method && req.Path == path {
			count++
		}
	}
	return count
}

func countRuleTargetGets(requests []ruleRequest) int {
	count := 0
	for _, req := range requests {
		if req.Method == http.MethodGet && strings.Contains(req.Path, "/messageRules/") {
			count++
		}
	}
	return count
}

func firstRuleRequest(t *testing.T, requests []ruleRequest, method, path string) ruleRequest {
	t.Helper()
	for _, req := range requests {
		if req.Method == method && req.Path == path {
			return req
		}
	}
	t.Fatalf("request %s %s not found in %#v", method, path, requests)
	return ruleRequest{}
}

func hasRuleWriteRequest(requests []ruleRequest) bool {
	for _, req := range requests {
		if req.Method == http.MethodPost || req.Method == http.MethodPatch || req.Method == http.MethodDelete {
			return true
		}
	}
	return false
}

func assertOutlookRuleReadsOnly(t *testing.T, requests []ruleRequest, want int) {
	t.Helper()
	if len(requests) != want {
		t.Fatalf("rule requests = %#v, want %d", requests, want)
	}
	for _, request := range requests {
		if request.Method != http.MethodGet || len(request.Body) != 0 {
			t.Fatalf("rule read issued a write/body request: %#v", request)
		}
		if request.Path == "/me/mailFolders/inbox/messageRules" {
			if request.Query.Get("$top") != "100" || len(request.Query) != 1 {
				t.Fatalf("rule collection query = %#v", request.Query)
			}
		} else if len(request.Query) != 0 {
			t.Fatalf("direct rule query = %#v", request.Query)
		}
	}
}

func ruleRequestBody(t *testing.T, req ruleRequest) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode rule request body: %v\n%s", err, string(req.Body))
	}
	return body
}
