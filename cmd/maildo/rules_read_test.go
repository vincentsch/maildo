package main

// This file covers the read-only Inbox rule command surface.

import (
	"context"
	"encoding/json"
	"fmt"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestRulesListViewAndHealthFakeProvider(t *testing.T) {
	h := seededRulesHarness(t, "work")

	var active ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &active)
	assertSelection(t, active.AccountSelection, "single", "work")
	if active.Count != 2 || len(active.Rules) != 2 {
		t.Fatalf("active rules = %#v", active)
	}
	assertRuleReadSemantics(t, active.Rules[0])
	if active.Rules[0].Account != "work" || active.Rules[0].Provider != "fake" || active.Rules[0].Scope != mail.RuleScopeInbox {
		t.Fatalf("rule labels = %#v", active.Rules[0])
	}
	if active.Rules[0].Actions.MoveToFolder == nil || active.Rules[0].Actions.MoveToFolder.Path != "Clients/ACME" {
		t.Fatalf("folder target not resolved: %#v", active.Rules[0].Actions.MoveToFolder)
	}

	var explicit ruleListResult
	decodeOK(t, h.run("rules", "list", "--account", "work", "--json"), &explicit)
	assertSelection(t, explicit.AccountSelection, "single", "work")

	var many ruleListResult
	decodeOK(t, h.run("rules", "list", "--account", "work", "--account", "personal", "--json"), &many)
	assertSelection(t, many.AccountSelection, "read_many", "work", "personal")
	if many.Count != 3 {
		t.Fatalf("many count = %d, want 3", many.Count)
	}

	var all ruleListResult
	decodeOK(t, h.run("rules", "list", "--all-accounts", "--json"), &all)
	assertSelection(t, all.AccountSelection, "all", "work", "personal")
	if all.Count != 3 {
		t.Fatalf("all count = %d, want 3", all.Count)
	}

	var view ruleViewResult
	decodeOK(t, h.run("rules", "view", "rule-move-client", "--json"), &view)
	assertSelection(t, view.AccountSelection, "single", "work")
	if view.RequestedRuleID != "rule-move-client" || view.Rule.ID != "rule-move-client" {
		t.Fatalf("view = %#v", view)
	}
	assertRuleReadSemantics(t, view.Rule)

	var health ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--json"), &health)
	assertSelection(t, health.AccountSelection, "single", "work")
	if health.Count != 2 || health.FindingCounts[string(mail.RuleHealthWarning)] == 0 || health.Rules[1].HealthStatus != mail.RuleHealthWarning {
		t.Fatalf("health = %#v", health)
	}
	assertRuleReadSemantics(t, health.Rules[0])

	var explicitHealth ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--account", "work", "--json"), &explicitHealth)
	assertSelection(t, explicitHealth.AccountSelection, "single", "work")

	var manyHealth ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--account", "work", "--account", "personal", "--json"), &manyHealth)
	assertSelection(t, manyHealth.AccountSelection, "read_many", "work", "personal")
	if manyHealth.Count != 3 {
		t.Fatalf("many health count = %d, want 3", manyHealth.Count)
	}

	var allHealth ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--all-accounts", "--json"), &allHealth)
	assertSelection(t, allHealth.AccountSelection, "all", "work", "personal")
	if allHealth.Count != 3 {
		t.Fatalf("all health count = %d, want 3", allHealth.Count)
	}
}

func TestRulesOutputModesMetaAndDryRun(t *testing.T) {
	h := seededRulesHarness(t, "work")

	human := h.run("rules", "list")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "Move client mail") || !strings.Contains(human.Stdout, "HEALTH") {
		t.Fatalf("human stdout = %q", human.Stdout)
	}
	healthHuman := h.run("rules", "health")
	decodeOK(t, healthHuman, nil)
	if !strings.Contains(healthHuman.Stdout, `"Forward alerts" (order 2, id rule-forward-alert): warning`) || !strings.Contains(healthHuman.Stdout, "(actions.forward_to)") {
		t.Fatalf("health human stdout = %q", healthHuman.Stdout)
	}
	viewHuman := h.run("rules", "view", "rule-forward-alert")
	decodeOK(t, viewHuman, nil)
	if !strings.Contains(viewHuman.Stdout, "Conditions (all must match; a list matches when any value does):") ||
		!strings.Contains(viewHuman.Stdout, "  Has attachments: yes") ||
		!strings.Contains(viewHuman.Stdout, "Fields maildo cannot edit (they stay as they are):") ||
		!strings.Contains(viewHuman.Stdout, "Health:\n") {
		t.Fatalf("view human stdout = %q", viewHuman.Stdout)
	}

	jq := h.run("rules", "list", "--json", "--jq", ".count")
	if strings.TrimSpace(jq.Stdout) != "2" {
		t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
	}
	healthJQ := h.run("rules", "health", "--json", "--jq", ".count")
	if strings.TrimSpace(healthJQ.Stdout) != "2" {
		t.Fatalf("health jq stdout = %q stderr=%s", healthJQ.Stdout, healthJQ.Stderr)
	}
	listTemplate := h.run("rules", "list", "--template", "{{.count}}")
	if strings.TrimSpace(listTemplate.Stdout) != "2" {
		t.Fatalf("list template stdout = %q stderr=%s", listTemplate.Stdout, listTemplate.Stderr)
	}
	viewJQ := h.run("rules", "view", "rule-move-client", "--json", "--jq", ".rule.id")
	if strings.TrimSpace(viewJQ.Stdout) != "rule-move-client" {
		t.Fatalf("view jq stdout = %q stderr=%s", viewJQ.Stdout, viewJQ.Stderr)
	}
	tmpl := h.run("rules", "view", "rule-move-client", "--template", "{{.rule.id}}")
	if strings.TrimSpace(tmpl.Stdout) != "rule-move-client" {
		t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	healthTemplate := h.run("rules", "health", "--template", "{{.count}}")
	if strings.TrimSpace(healthTemplate.Stdout) != "2" {
		t.Fatalf("health template stdout = %q stderr=%s", healthTemplate.Stdout, healthTemplate.Stderr)
	}
	plain := h.run("rules", "list", "--plain")
	decodeOK(t, plain, nil)
	assertTabSeparatedColumns(t, plain.Stdout, 7)
	if !strings.Contains(plain.Stdout, "work\tfake\tinbox\trule-move-client\t1\tenabled\tMove client mail\n") {
		t.Fatalf("plain stdout = %q", plain.Stdout)
	}
	healthPlain := h.run("rules", "health", "--plain")
	decodeOK(t, healthPlain, nil)
	assertTabSeparatedColumns(t, healthPlain.Stdout, 7)
	if !strings.Contains(healthPlain.Stdout, "work\tfake\tinbox\trule-forward-alert\twarning\tdisabled_rule,unsupported_field,forwarding_action,external_forwarding\tForward alerts\n") {
		t.Fatalf("health plain stdout = %q", healthPlain.Stdout)
	}

	var envelope struct {
		Data ruleListResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("rules", "list", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["command_path"] != "maildo rules list" ||
		envelope.Meta.Extra["scope"] != "single" ||
		envelope.Meta.Extra["rule_scope"] != string(mail.RuleScopeInbox) ||
		envelope.Meta.Extra["rule_count"] != float64(2) ||
		len(envelope.Meta.Extra) != 6 {
		t.Fatalf("meta = %#v", envelope.Meta.Extra)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"work"}) {
		t.Fatalf("meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake"}) {
		t.Fatalf("meta providers = %#v", got)
	}

	var healthEnvelope struct {
		Data ruleHealthResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("rules", "health", "--json", "--include-meta"), &healthEnvelope)
	counts, ok := healthEnvelope.Meta.Extra["finding_counts"].(map[string]any)
	if healthEnvelope.Meta.Extra["command_path"] != "maildo rules health" ||
		healthEnvelope.Meta.Extra["scope"] != "single" ||
		healthEnvelope.Meta.Extra["rule_scope"] != string(mail.RuleScopeInbox) ||
		healthEnvelope.Meta.Extra["rule_count"] != float64(2) ||
		len(healthEnvelope.Meta.Extra) != 7 || !ok || len(counts) != 3 ||
		counts[string(mail.RuleHealthInfo)] != float64(2) ||
		counts[string(mail.RuleHealthWarning)] != float64(2) ||
		counts[string(mail.RuleHealthError)] != float64(0) {
		t.Fatalf("health meta = %#v", healthEnvelope.Meta.Extra)
	}
	if _, ok := counts[string(mail.RuleHealthHealthy)]; ok {
		t.Fatalf("health meta contains healthy finding count: %#v", counts)
	}
	if got := metaStringSlice(t, healthEnvelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"work"}) {
		t.Fatalf("health meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, healthEnvelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake"}) {
		t.Fatalf("health meta providers = %#v", got)
	}

	var viewEnvelope struct {
		Data ruleViewResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("rules", "view", "rule-move-client", "--json", "--include-meta"), &viewEnvelope)
	if viewEnvelope.Meta.Extra["command_path"] != "maildo rules view" ||
		viewEnvelope.Meta.Extra["scope"] != "single" ||
		viewEnvelope.Meta.Extra["account"] != "work" ||
		viewEnvelope.Meta.Extra["provider"] != "fake" ||
		viewEnvelope.Meta.Extra["rule_scope"] != string(mail.RuleScopeInbox) ||
		viewEnvelope.Meta.Extra["requested_rule_id"] != "rule-move-client" ||
		viewEnvelope.Meta.Extra["rule_count"] != float64(1) ||
		len(viewEnvelope.Meta.Extra) != 9 {
		t.Fatalf("view meta = %#v", viewEnvelope.Meta.Extra)
	}
	if got := metaStringSlice(t, viewEnvelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"work"}) {
		t.Fatalf("view meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, viewEnvelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake"}) {
		t.Fatalf("view meta providers = %#v", got)
	}

	bad := h.run("rules", "list", "--include-meta", "--dry-run", "--json")
	assertFrameworkUsage(t, bad)
	if !strings.Contains(bad.Stderr, "--include-meta cannot be combined with --dry-run") {
		t.Fatalf("dry-run meta stderr = %s", bad.Stderr)
	}
	var dryList ruleListResult
	decodeOK(t, h.run("rules", "list", "--dry-run", "--json"), &dryList)
	if dryList.Count != 2 {
		t.Fatalf("dry list = %#v", dryList)
	}
	var dryView ruleViewResult
	decodeOK(t, h.run("rules", "view", "rule-move-client", "--dry-run", "--json"), &dryView)
	if dryView.Rule.ID != "rule-move-client" {
		t.Fatalf("dry view = %#v", dryView)
	}
	var dryHealth ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--dry-run", "--json"), &dryHealth)
	if dryHealth.Count != 2 {
		t.Fatalf("dry health = %#v", dryHealth)
	}
}

func TestRuleReadOutputCrossProduct(t *testing.T) {
	h := seededRulesHarness(t, "work")
	dryRunModes := []struct {
		name string
		args []string
	}{
		{name: "list human", args: []string{"rules", "list"}},
		{name: "list json", args: []string{"rules", "list", "--json"}},
		{name: "list plain", args: []string{"rules", "list", "--plain"}},
		{name: "list jq", args: []string{"rules", "list", "--json", "--jq", ".count"}},
		{name: "list template", args: []string{"rules", "list", "--template", "{{.count}}"}},
		{name: "view human", args: []string{"rules", "view", "rule-move-client"}},
		{name: "view json", args: []string{"rules", "view", "rule-move-client", "--json"}},
		{name: "view jq", args: []string{"rules", "view", "rule-move-client", "--json", "--jq", ".rule.id"}},
		{name: "view template", args: []string{"rules", "view", "rule-move-client", "--template", "{{.rule.id}}"}},
		{name: "health human", args: []string{"rules", "health"}},
		{name: "health json", args: []string{"rules", "health", "--json"}},
		{name: "health plain", args: []string{"rules", "health", "--plain"}},
		{name: "health jq", args: []string{"rules", "health", "--json", "--jq", ".count"}},
		{name: "health template", args: []string{"rules", "health", "--template", "{{.count}}"}},
	}
	for _, tt := range dryRunModes {
		t.Run("dry-run/"+tt.name, func(t *testing.T) {
			normal := h.run(tt.args...)
			decodeOK(t, normal, nil)
			dryArgs := append(append([]string(nil), tt.args...), "--dry-run")
			dry := h.run(dryArgs...)
			decodeOK(t, dry, nil)
			if dry.Stdout != normal.Stdout || dry.Stderr != normal.Stderr {
				t.Fatalf("dry-run changed %s output\nnormal stdout=%q\ndry stdout=%q\nnormal stderr=%q\ndry stderr=%q", tt.name, normal.Stdout, dry.Stdout, normal.Stderr, dry.Stderr)
			}
		})
	}

	metaTransforms := []struct {
		name string
		args []string
		want string
	}{
		{name: "list jq data", args: []string{"rules", "list", "--json", "--include-meta", "--jq", ".data.count"}, want: "2"},
		{name: "list jq meta", args: []string{"rules", "list", "--json", "--include-meta", "--jq", ".meta.extra.rule_scope"}, want: "inbox"},
		{name: "list template", args: []string{"rules", "list", "--include-meta", "--template", "{{.data.count}}/{{.meta.extra.rule_scope}}"}, want: "2/inbox"},
		{name: "view jq data", args: []string{"rules", "view", "rule-move-client", "--json", "--include-meta", "--jq", ".data.rule.id"}, want: "rule-move-client"},
		{name: "view jq meta", args: []string{"rules", "view", "rule-move-client", "--json", "--include-meta", "--jq", ".meta.extra.requested_rule_id"}, want: "rule-move-client"},
		{name: "view template", args: []string{"rules", "view", "rule-move-client", "--include-meta", "--template", "{{.data.rule.id}}/{{.meta.extra.rule_scope}}"}, want: "rule-move-client/inbox"},
		{name: "health jq data", args: []string{"rules", "health", "--json", "--include-meta", "--jq", ".data.count"}, want: "2"},
		{name: "health jq meta", args: []string{"rules", "health", "--json", "--include-meta", "--jq", ".meta.extra.finding_counts.warning"}, want: "2"},
		{name: "health template", args: []string{"rules", "health", "--include-meta", "--template", "{{.data.count}}/{{.meta.extra.finding_counts.warning}}"}, want: "2/2"},
	}
	for _, tt := range metaTransforms {
		t.Run("metadata/"+tt.name, func(t *testing.T) {
			res := h.run(tt.args...)
			decodeOK(t, res, nil)
			if strings.TrimSpace(res.Stdout) != tt.want || res.Stderr != "" {
				t.Fatalf("metadata transform stdout=%q stderr=%q, want %q", res.Stdout, res.Stderr, tt.want)
			}
		})
	}

	empty := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", map[string][]mail.Rule{"work": {}}), "work")
	for _, command := range []string{"list", "health"} {
		t.Run("empty/"+command+" human", func(t *testing.T) {
			res := empty.run("rules", command)
			decodeOK(t, res, nil)
			if res.Stdout != "No rules.\n" || res.Stderr != "" {
				t.Fatalf("empty human = %#v", res)
			}
		})
		t.Run("empty/"+command+" plain", func(t *testing.T) {
			res := empty.run("rules", command, "--plain")
			decodeOK(t, res, nil)
			if res.Stdout != "" || res.Stderr != "" {
				t.Fatalf("empty plain = %#v", res)
			}
		})
		for _, transform := range []struct {
			name string
			args []string
		}{
			{name: "jq", args: []string{"--json", "--jq", ".count"}},
			{name: "template", args: []string{"--template", "{{.count}}"}},
			{name: "metadata jq", args: []string{"--json", "--include-meta", "--jq", ".data.count"}},
			{name: "metadata template", args: []string{"--include-meta", "--template", "{{.data.count}}"}},
		} {
			t.Run("empty/"+command+" "+transform.name, func(t *testing.T) {
				args := append([]string{"rules", command}, transform.args...)
				res := empty.run(args...)
				decodeOK(t, res, nil)
				if strings.TrimSpace(res.Stdout) != "0" || res.Stderr != "" {
					t.Fatalf("empty transform = %#v", res)
				}
			})
		}
		var result map[string]any
		decodeOK(t, empty.run("rules", command, "--json", "--include-meta"), &result)
		data := result["data"].(map[string]any)
		if data["count"] != float64(0) || len(data["rules"].([]any)) != 0 {
			t.Fatalf("empty metadata data = %#v", data)
		}
		extra := result["meta"].(map[string]any)["extra"].(map[string]any)
		if extra["rule_count"] != float64(0) {
			t.Fatalf("empty metadata extra = %#v", extra)
		}
		if command == "health" {
			counts := extra["finding_counts"].(map[string]any)
			if len(counts) != 3 || counts["info"] != float64(0) || counts["warning"] != float64(0) || counts["error"] != float64(0) {
				t.Fatalf("empty finding counts = %#v", counts)
			}
			if _, ok := counts["healthy"]; ok {
				t.Fatalf("empty finding counts invented healthy: %#v", counts)
			}
		}
	}

	invalidFake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
	invalid := seededRulesHarnessWithProvider(t, invalidFake, "work")
	for _, args := range [][]string{
		{"rules", "list", "--plain", "--include-meta"},
		{"rules", "view", "rule-move-client", "--plain"},
		{"rules", "health", "--json", "--plain"},
		{"rules", "list", "--json", "--jq", ".count", "--template", "{{.count}}"},
		{"rules", "health", "--json", "--include-meta", "--dry-run"},
	} {
		res := invalid.run(args...)
		assertFrameworkUsage(t, res)
		if res.Stdout != "" {
			t.Fatalf("invalid combination %v wrote stdout %q", args, res.Stdout)
		}
	}
	if invalidFake.AccountClientCalls.Load() != 0 || invalidFake.FolderListCalls.Load() != 0 || invalidFake.RuleListCalls.Load() != 0 || invalidFake.RuleGetCalls.Load() != 0 {
		t.Fatalf("invalid combinations reached provider clients=%d folders=%d list=%d get=%d", invalidFake.AccountClientCalls.Load(), invalidFake.FolderListCalls.Load(), invalidFake.RuleListCalls.Load(), invalidFake.RuleGetCalls.Load())
	}
}

func TestRuleReadGlobalOutputControls(t *testing.T) {
	h := seededRulesHarness(t, "work")
	commands := [][]string{
		{"rules", "list"},
		{"rules", "view", "rule-move-client"},
		{"rules", "health"},
	}
	withFlags := func(args []string, flags ...string) []string {
		return append(append([]string(nil), args...), flags...)
	}

	for _, args := range commands {
		t.Run(args[1], func(t *testing.T) {
			baseline := h.run(args...)
			decodeOK(t, baseline, nil)
			if baseline.Stderr != "" || baseline.Stdout == "" {
				t.Fatalf("baseline = %#v", baseline)
			}

			quiet := h.run(withFlags(args, "--quiet")...)
			decodeOK(t, quiet, nil)
			if quiet.Stdout != baseline.Stdout || quiet.Stderr != "" {
				t.Fatalf("quiet changed primary output\nbaseline=%q\nquiet=%q\nstderr=%q", baseline.Stdout, quiet.Stdout, quiet.Stderr)
			}

			promptReader := &failOnRead{}
			noPrompt := h.runWith(testutil.Options{
				Stdin:       promptReader,
				TerminalSet: true,
				Terminal:    true,
			}, withFlags(args, "--no-prompt")...)
			decodeOK(t, noPrompt, nil)
			if promptReader.reads.Load() != 0 || noPrompt.Stdout != baseline.Stdout || noPrompt.Stderr != "" {
				t.Fatalf("no-prompt reads=%d stdout=%q stderr=%q", promptReader.reads.Load(), noPrompt.Stdout, noPrompt.Stderr)
			}

			terminal := testutil.Options{OutputTerminalSet: true, OutputTerminal: true}
			noANSI := h.runWith(terminal, withFlags(args, "--no-ansi")...)
			decodeOK(t, noANSI, nil)
			assertNoTerminalControls(t, noANSI.Stdout)
			if noANSI.Stdout != baseline.Stdout {
				t.Fatalf("no-ansi changed content\nbaseline=%q\nno-ansi=%q", baseline.Stdout, noANSI.Stdout)
			}

			noColor := h.runWith(terminal, withFlags(args, "--no-color")...)
			decodeOK(t, noColor, nil)
			assertNoTerminalControls(t, noColor.Stdout)
			if noColor.Stdout != baseline.Stdout {
				t.Fatalf("no-color changed content\nbaseline=%q\nno-color=%q", baseline.Stdout, noColor.Stdout)
			}

			pagerCalls := 0
			noPager := h.runWith(testutil.Options{
				OutputTerminalSet: true,
				OutputTerminal:    true,
				TerminalHeight:    func() (int, bool) { return 1, true },
				Pager: rungrad.PagerFunc(func(_ []string, _ io.Reader, _, _ io.Writer) error {
					pagerCalls++
					return nil
				}),
			}, withFlags(args, "--no-color", "--no-pager")...)
			decodeOK(t, noPager, nil)
			if pagerCalls != 0 || noPager.Stdout != baseline.Stdout || noPager.Stderr != "" {
				t.Fatalf("no-pager calls=%d stdout=%q stderr=%q", pagerCalls, noPager.Stdout, noPager.Stderr)
			}

			jsonBaseline := h.run(withFlags(args, "--json")...)
			decodeOK(t, jsonBaseline, nil)
			for _, flag := range []string{"--no-color", "--no-ansi", "--no-pager", "--quiet", "--no-prompt"} {
				got := h.run(withFlags(args, "--json", flag)...)
				decodeOK(t, got, nil)
				if got.Stdout != jsonBaseline.Stdout || got.Stderr != "" {
					t.Fatalf("%s changed JSON\nbaseline=%q\ngot=%q\nstderr=%q", flag, jsonBaseline.Stdout, got.Stdout, got.Stderr)
				}
			}
		})
	}

	quietError := h.run("rules", "view", "missing", "--json", "--quiet")
	assertErrorCodeExit(t, quietError, errs.CodeRuleNotFound, 5)
	if quietError.Stdout != "" || !json.Valid([]byte(quietError.Stderr)) {
		t.Fatalf("quiet structured error = %#v", quietError)
	}

	for _, args := range [][]string{
		{"rules", "list", "--json", "--jq", "["},
		{"rules", "view", "rule-move-client", "--template", "{{"},
		{"rules", "health", "--json", "--jq", ".. | invalid("},
	} {
		res := h.run(args...)
		if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stderr, "rule-move-client") || strings.Contains(res.Stderr, "provider_metadata") {
			t.Fatalf("invalid transform leaked output for %v: %#v", args, res)
		}
	}
}

func TestRuleReadJSONContractKeysAndOmissions(t *testing.T) {
	h := seededRulesHarness(t, "work")
	list := decodeRuleJSONObject(t, h.run("rules", "list", "--json"))
	assertRuleJSONKeys(t, list, "accounts", "count", "rules", "scope")
	accounts := ruleJSONArray(t, list["accounts"])
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", accounts)
	}
	assertRuleJSONKeys(t, accounts[0], "active", "display_name", "email", "name", "provider")
	rules := ruleJSONArray(t, list["rules"])
	if len(rules) != 2 || list["count"] != json.Number("2") {
		t.Fatalf("list = %#v", list)
	}

	move := rules[0]
	assertRuleJSONKeys(t, move,
		"account", "actions", "conditions", "display_name", "exceptions",
		"has_error", "health_status", "id", "is_enabled", "is_read_only",
		"provider", "provider_metadata", "scope", "sequence",
	)
	if move["sequence"] != json.Number("1") || move["health_status"] != string(mail.RuleHealthHealthy) {
		t.Fatalf("move scalar contract = %#v", move)
	}
	assertRuleJSONKeys(t, ruleJSONObject(t, move["conditions"]), "from_addresses", "semantics", "subject_contains")
	assertRuleJSONKeys(t, ruleJSONObject(t, move["exceptions"]), "semantics")
	assertRuleJSONKeys(t, ruleJSONObject(t, ruleJSONObject(t, move["conditions"])["semantics"]), "families", "repeated_values")
	from := ruleJSONArray(t, ruleJSONObject(t, move["conditions"])["from_addresses"])
	assertRuleJSONKeys(t, from[0], "address", "name")
	actions := ruleJSONObject(t, move["actions"])
	assertRuleJSONKeys(t, actions, "mark_as_read", "move_to_folder", "stop_processing_rules")
	assertRuleJSONKeys(t, ruleJSONObject(t, actions["move_to_folder"]), "display_name", "path", "provider_folder_id")
	assertRuleJSONKeys(t, ruleJSONObject(t, move["provider_metadata"]), "source")
	for _, omitted := range []string{"health_findings", "unsupported_fields", "provider_state", "ProviderState"} {
		if _, ok := move[omitted]; ok {
			t.Fatalf("healthy rule unexpectedly rendered %q: %#v", omitted, move)
		}
	}

	forward := rules[1]
	assertRuleJSONKeys(t, forward,
		"account", "actions", "conditions", "display_name", "exceptions",
		"has_error", "health_findings", "health_status", "id", "is_enabled",
		"is_read_only", "provider", "provider_metadata", "scope", "sequence",
		"unsupported_fields",
	)
	assertRuleJSONKeys(t, ruleJSONObject(t, forward["conditions"]), "has_attachments", "semantics", "sender_contains")
	forwardActions := ruleJSONObject(t, forward["actions"])
	assertRuleJSONKeys(t, forwardActions, "forward_to")
	forwardRecipients := ruleJSONArray(t, forwardActions["forward_to"])
	assertRuleJSONKeys(t, forwardRecipients[0], "address")
	unsupported := ruleJSONArray(t, forward["unsupported_fields"])
	assertRuleJSONKeys(t, unsupported[0], "area", "count", "field", "summary", "value_kind")
	findings := ruleJSONArray(t, forward["health_findings"])
	if len(findings) != 4 {
		t.Fatalf("findings = %#v", findings)
	}
	assertRuleJSONKeys(t, findings[0], "code", "field", "message", "severity")
	for _, index := range []int{1, 2, 3} {
		assertRuleJSONKeys(t, findings[index], "code", "details", "field", "message", "severity")
	}
	assertRuleJSONKeys(t, ruleJSONObject(t, findings[1]["details"]), "area", "count", "field", "summary", "value_kind")
	assertRuleJSONKeys(t, ruleJSONObject(t, findings[2]["details"]), "recipient_count")
	assertRuleJSONKeys(t, ruleJSONObject(t, findings[3]["details"]), "account_domain", "recipient_domain")

	view := decodeRuleJSONObject(t, h.run("rules", "view", "rule-move-client", "--json"))
	assertRuleJSONKeys(t, view, "account", "accounts", "provider", "requested_rule_id", "rule", "scope")
	if !reflect.DeepEqual(ruleJSONObject(t, view["rule"]), move) {
		t.Fatalf("view rule projection differs from list\nlist=%#v\nview=%#v", move, view["rule"])
	}

	health := decodeRuleJSONObject(t, h.run("rules", "health", "--json"))
	assertRuleJSONKeys(t, health, "accounts", "count", "finding_counts", "rules", "scope")
	assertRuleJSONKeys(t, ruleJSONObject(t, health["finding_counts"]), "error", "info", "warning")
	if !reflect.DeepEqual(ruleJSONArray(t, health["rules"]), rules) {
		t.Fatalf("health rule projection differs from list\nlist=%#v\nhealth=%#v", rules, health["rules"])
	}
}

func TestRuleReadHumanGoldenOutput(t *testing.T) {
	h := seededRulesHarness(t, "work")
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "rules_list_human.golden", args: []string{"rules", "list"}},
		{name: "rules_list_plain.golden", args: []string{"rules", "list", "--plain"}},
		{name: "rules_view_human.golden", args: []string{"rules", "view", "rule-move-client"}},
		{name: "rules_health_human.golden", args: []string{"rules", "health"}},
		{name: "rules_health_plain.golden", args: []string{"rules", "health", "--plain"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := h.run(tt.args...)
			decodeOK(t, res, nil)
			checkHumanGolden(t, "testdata/"+tt.name, res.Stdout)
		})
	}
}

func TestRuleHealthSeverityGoldenOutput(t *testing.T) {
	rules := map[string][]mail.Rule{"work": {
		{ID: "healthy", DisplayName: "Healthy", Sequence: 1, IsEnabled: true},
		{ID: "disabled", DisplayName: "Disabled", Sequence: 2, IsEnabled: false},
		{ID: "forward", DisplayName: "Forward", Sequence: 3, IsEnabled: true, Actions: mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "ops@external.test"}}}},
		{ID: "provider-error", DisplayName: "Provider error", Sequence: 4, IsEnabled: true, HasError: true},
	}}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", rules), "work")
	res := h.run("rules", "health")
	decodeOK(t, res, nil)
	checkHumanGolden(t, "testdata/rules_health_severities_human.golden", res.Stdout)
}

// checkHumanGolden compares human output with a reviewed golden file. With
// MAILDO_UPDATE_GOLDENS=1 it rewrites the file instead; review the diff.
func checkHumanGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv(updateGeneratedSurfaceEnv) == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s\nwant=%q\ngot =%q", path, string(want), got)
	}
}

func TestRulesScopeCapabilitiesAndErrors(t *testing.T) {
	t.Run("list and health reject positional arguments", func(t *testing.T) {
		h := seededRulesHarness(t, "work")
		for _, command := range []string{"list", "health"} {
			res := h.run("rules", command, "unexpected", "--json")
			assertFrameworkUsage(t, res)
			if !strings.Contains(res.Stderr, "unexpected") {
				t.Fatalf("%s stderr = %s", command, res.Stderr)
			}
		}
	})

	t.Run("missing active and unknown account", func(t *testing.T) {
		h := seededRulesHarness(t, "")
		assertErrorCode(t, h.run("rules", "list", "--json"), errs.CodeMissingActiveAccount)
		h = seededRulesHarness(t, "work")
		assertErrorCode(t, h.run("rules", "list", "--account", "missing", "--json"), errs.CodeUnknownAccount)
		assertErrorCode(t, h.run("rules", "list", "--account", "work", "--account", "work", "--json"), errs.CodeInvalidAccountScope)
		assertErrorCode(t, h.run("rules", "view", "rule-move-client", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidAccountScope)
		viewAll := h.run("rules", "view", "rule-move-client", "--all-accounts", "--json")
		assertFrameworkUsage(t, viewAll)
		missingID := h.run("rules", "view", "--json")
		assertFrameworkUsage(t, missingID)
	})

	t.Run("folder scope is unavailable", func(t *testing.T) {
		h := seededRulesHarness(t, "work")
		for _, args := range [][]string{
			{"rules", "list", "--folder", "inbox", "--json"},
			{"rules", "view", "rule-move-client", "--folder", "inbox", "--json"},
			{"rules", "health", "--folder", "inbox", "--json"},
		} {
			res := h.run(args...)
			assertFrameworkUsage(t, res)
			if !strings.Contains(res.Stderr, "unknown flag: --folder") {
				t.Fatalf("%v stderr = %s", args, res.Stderr)
			}
		}
	})

	t.Run("missing rule and folder capabilities", func(t *testing.T) {
		noRules := testkit.NewFakeFolderProvider("fake", testkit.SampleFolders())
		h := seededRulesHarnessWithProvider(t, noRules, "work")
		assertErrorCode(t, h.run("rules", "list", "--json"), errs.CodeUnsupportedCapability)

		noFolders := testkit.NewFakeProvider("fake")
		noFolders.RulesReadImplemented = true
		noFolders.RulesByAccount = testkit.SampleRules()
		h = seededRulesHarnessWithProvider(t, noFolders, "work")
		assertErrorCode(t, h.run("rules", "list", "--json"), errs.CodeUnsupportedCapability)
	})

	t.Run("stale rule and folder clients", func(t *testing.T) {
		h := seededRulesHarnessWithProvider(t, testkit.NewFakeStaleRuleProvider("fake"), "work")
		res := h.run("rules", "list", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body := errorJSON(t, res)
		if body["capability"] != string(provider.CapRulesRead) {
			t.Fatalf("stale rule body = %#v", body)
		}

		staleFolder := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
		staleFolder.RuleClientMissing = true
		staleFolder.FolderClientMissing = true
		h = seededRulesHarnessWithProvider(t, staleFolder, "work")
		res = h.run("rules", "list", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body = errorJSON(t, res)
		if body["capability"] != string(provider.CapFoldersRead) {
			t.Fatalf("stale folder body = %#v", body)
		}
	})

	t.Run("rule not found", func(t *testing.T) {
		h := seededRulesHarness(t, "work")
		res := h.run("rules", "view", "missing-rule", "--json")
		assertErrorCodeExit(t, res, errs.CodeRuleNotFound, 5)
		body := errorJSON(t, res)
		if body["rule_id"] != "missing-rule" || body["scope"] != string(mail.RuleScopeInbox) {
			t.Fatalf("rule not found body = %#v", body)
		}
	})

	t.Run("view plain unsupported", func(t *testing.T) {
		h := seededRulesHarness(t, "work")
		res := h.run("rules", "view", "rule-move-client", "--plain")
		assertFrameworkUsage(t, res)
		if !strings.Contains(res.Stderr, "does not support --plain") {
			t.Fatalf("plain stderr = %s", res.Stderr)
		}
	})
}

func TestRulesAllAccountsFailureDoesNotWritePartialOutput(t *testing.T) {
	fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
	fake.RuleErrByAccount = map[string]error{"personal": fmt.Errorf("fake rule failure")}
	h := seededRulesHarnessWithProvider(t, fake, "work")

	res := h.run("rules", "list", "--all-accounts", "--json")
	if res.Exit == 0 {
		t.Fatalf("list succeeded, want failure")
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("partial stdout = %s", res.Stdout)
	}
	res = h.run("rules", "health", "--all-accounts", "--json")
	if res.Exit == 0 {
		t.Fatalf("health succeeded, want failure")
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("partial health stdout = %s", res.Stdout)
	}
}

func TestRulesEmptyCollectionsSerializeAsArrays(t *testing.T) {
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", map[string][]mail.Rule{
		"work": {},
	}), "work")

	var list ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &list)
	if list.Rules == nil || list.Count != 0 {
		t.Fatalf("empty list = %#v, want non-nil empty rules", list)
	}

	var health ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--json"), &health)
	if health.Rules == nil || health.Count != 0 {
		t.Fatalf("empty health = %#v, want non-nil empty rules", health)
	}
}

func TestRulesViewUsesAccountLocalDuplicateSequences(t *testing.T) {
	rules := map[string][]mail.Rule{"work": {
		{ID: "first", DisplayName: "First", Sequence: 4, IsEnabled: true},
		{ID: "second", DisplayName: "Second", Sequence: 4, IsEnabled: true},
	}}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", rules), "work")

	var view ruleViewResult
	decodeOK(t, h.run("rules", "view", "first", "--json"), &view)
	if !hasFinding(view.Rule, "duplicate_sequence") {
		t.Fatalf("view findings = %#v", view.Rule.HealthFindings)
	}
}

func TestRuleSequenceCountsForViewIncludesOmittedRule(t *testing.T) {
	listed := []mail.Rule{{ID: "listed", Sequence: 4}}
	viewed := mail.Rule{ID: "viewed", Sequence: 4}
	counts := ruleSequenceCountsForView(listed, viewed)
	if counts[4] != 2 {
		t.Fatalf("sequence 4 count = %d, want 2", counts[4])
	}
}

func TestRuleReadFailureOrderingAndNoPartialOutput(t *testing.T) {
	type failureStage struct {
		name        string
		provider    func() *testkit.FakeProvider
		wantCalls   []string
		wantClients int64
	}
	stages := []failureStage{
		{
			name: "folder capability rejected",
			provider: func() *testkit.FakeProvider {
				fake := testkit.NewFakeProvider("fake")
				fake.RulesReadImplemented = true
				fake.RulesByAccount = testkit.SampleRules()
				return fake
			},
		},
		{
			name: "folder client missing",
			provider: func() *testkit.FakeProvider {
				fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
				fake.FolderClientMissing = true
				fake.RuleClientMissing = true
				return fake
			},
			wantClients: 1,
		},
		{
			name: "folder list fails",
			provider: func() *testkit.FakeProvider {
				fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
				fake.FolderErrByAccount = map[string]error{"work": fmt.Errorf("folder read failed")}
				return fake
			},
			wantCalls:   []string{"work:folder_list"},
			wantClients: 1,
		},
		{
			name: "rule capability rejected after folders",
			provider: func() *testkit.FakeProvider {
				return testkit.NewFakeFolderProvider("fake", testkit.SampleFolders())
			},
			wantCalls:   []string{"work:folder_list"},
			wantClients: 1,
		},
		{
			name: "rule client missing after folders",
			provider: func() *testkit.FakeProvider {
				return testkit.NewFakeStaleRuleProvider("fake")
			},
			wantCalls:   []string{"work:folder_list"},
			wantClients: 1,
		},
		{
			name: "rule list fails after folders",
			provider: func() *testkit.FakeProvider {
				fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
				fake.RuleListErrByAccount = map[string]error{"work": fmt.Errorf("rule list failed")}
				return fake
			},
			wantCalls:   []string{"work:folder_list", "work:rule_list"},
			wantClients: 1,
		},
	}

	for _, command := range []string{"list", "health"} {
		for _, stage := range stages {
			t.Run(command+"/"+stage.name, func(t *testing.T) {
				fake := stage.provider()
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run("rules", command, "--json")
				if res.Exit == 0 {
					t.Fatalf("%s succeeded, want failure", command)
				}
				if strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("%s wrote partial stdout: %q", command, res.Stdout)
				}
				if got := fake.RuleReadCalls(); !sameStrings(got, stage.wantCalls) {
					t.Fatalf("read calls = %#v, want %#v", got, stage.wantCalls)
				}
				if got := fake.AccountClientCalls.Load(); got != stage.wantClients {
					t.Fatalf("account client calls = %d, want %d", got, stage.wantClients)
				}
			})
		}
	}

	for _, command := range []string{"list", "health"} {
		for _, stage := range []string{"folder", "rule"} {
			t.Run(command+"/later account "+stage+" failure", func(t *testing.T) {
				fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
				wantCalls := []string{"work:folder_list", "work:rule_list", "personal:folder_list"}
				if stage == "folder" {
					fake.FolderErrByAccount = map[string]error{"personal": fmt.Errorf("personal folder read failed")}
				} else {
					fake.RuleListErrByAccount = map[string]error{"personal": fmt.Errorf("personal rule list failed")}
					wantCalls = append(wantCalls, "personal:rule_list")
				}
				h := seededRulesHarnessWithProvider(t, fake, "work")
				res := h.run("rules", command, "--account", "work", "--account", "personal", "--json")
				if res.Exit == 0 || strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("exit=%d partial stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
				}
				if got := fake.RuleReadCalls(); !sameStrings(got, wantCalls) {
					t.Fatalf("read calls = %#v, want %#v", got, wantCalls)
				}
			})
		}
	}
}

func TestRuleViewStopsAtFailedReadStage(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testkit.FakeProvider)
		wantCalls []string
		wantCode  string
	}{
		{
			name: "folder list fails before direct get",
			configure: func(fake *testkit.FakeProvider) {
				fake.FolderErrByAccount = map[string]error{"work": fmt.Errorf("folder read failed")}
			},
			wantCalls: []string{"work:folder_list"},
		},
		{
			name: "direct get not found stops before list snapshot",
			configure: func(fake *testkit.FakeProvider) {
				fake.RuleGetByAccount = map[string][]mail.Rule{"work": {}}
			},
			wantCalls: []string{"work:folder_list", "work:rule_get:rule-move-client"},
			wantCode:  errs.CodeRuleNotFound,
		},
		{
			name: "direct get provider error stops before list snapshot",
			configure: func(fake *testkit.FakeProvider) {
				fake.RuleGetErrByAccount = map[string]error{"work": fmt.Errorf("direct get failed")}
			},
			wantCalls: []string{"work:folder_list", "work:rule_get:rule-move-client"},
		},
		{
			name: "list snapshot fails after direct get",
			configure: func(fake *testkit.FakeProvider) {
				fake.RuleListErrByAccount = map[string]error{"work": fmt.Errorf("snapshot failed")}
			},
			wantCalls: []string{"work:folder_list", "work:rule_get:rule-move-client", "work:rule_list"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
			tt.configure(fake)
			h := seededRulesHarnessWithProvider(t, fake, "work")
			res := h.run("rules", "view", "rule-move-client", "--json")
			if res.Exit == 0 || strings.TrimSpace(res.Stdout) != "" {
				t.Fatalf("exit=%d partial stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
			}
			if tt.wantCode != "" {
				assertErrorCodeExit(t, res, tt.wantCode, 5)
			}
			if got := fake.RuleReadCalls(); !sameStrings(got, tt.wantCalls) {
				t.Fatalf("read calls = %#v, want %#v", got, tt.wantCalls)
			}
		})
	}
}

func TestRuleViewIncludesDirectRuleOmittedFromListSnapshot(t *testing.T) {
	fake := testkit.NewFakeRuleProvider("fake", map[string][]mail.Rule{"work": {}})
	fake.RuleGetByAccount = map[string][]mail.Rule{"work": {
		{ID: "viewed", DisplayName: "Viewed", Sequence: 4, IsEnabled: true},
	}}
	fake.RuleListByAccount = map[string][]mail.Rule{"work": {
		{ID: "listed", DisplayName: "Listed", Sequence: 4, IsEnabled: true},
	}}
	h := seededRulesHarnessWithProvider(t, fake, "work")

	var view ruleViewResult
	decodeOK(t, h.run("rules", "view", "viewed", "--json"), &view)
	if view.Rule.ID != "viewed" || !hasFinding(view.Rule, "duplicate_sequence") {
		t.Fatalf("view = %#v", view.Rule)
	}
	wantCalls := []string{"work:folder_list", "work:rule_get:viewed", "work:rule_list"}
	if got := fake.RuleReadCalls(); !sameStrings(got, wantCalls) {
		t.Fatalf("read calls = %#v, want %#v", got, wantCalls)
	}
}

func TestRuleReadsKeepAccountHealthAndFolderResolutionIsolated(t *testing.T) {
	rules := map[string][]mail.Rule{
		"work": {
			{ID: "work-first", DisplayName: "Work first", Sequence: 8, IsEnabled: true, Actions: mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "shared-folder"}}},
			{ID: "work-collision", DisplayName: "Work collision", Sequence: 7, IsEnabled: true},
			{ID: "work-last", DisplayName: "Work last", Sequence: 2, IsEnabled: true},
		},
		"personal": {
			{ID: "personal-first", DisplayName: "Personal first", Sequence: 9, IsEnabled: true, Actions: mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "shared-folder"}}},
			{ID: "personal-collision", DisplayName: "Personal collision", Sequence: 7, IsEnabled: true},
			{ID: "personal-last", DisplayName: "Personal last", Sequence: 1, IsEnabled: true},
		},
	}
	fake := testkit.NewFakeRuleProvider("fake", rules)
	fake.FoldersByAccount = map[string]mail.FolderList{
		"work":     {Folders: []mail.Folder{{ID: "shared-folder", DisplayName: "Work Archive", Path: "Work/Archive"}}},
		"personal": {Folders: []mail.Folder{{ID: "shared-folder", DisplayName: "Personal Archive", Path: "Personal/Archive"}}},
	}
	h := seededRulesHarnessWithProvider(t, fake, "work")
	args := []string{"rules", "list", "--account", "personal", "--account", "work", "--json"}

	var list ruleListResult
	decodeOK(t, h.run(args...), &list)
	wantIDs := []string{"personal-first", "personal-collision", "personal-last", "work-first", "work-collision", "work-last"}
	if got := ruleIDs(list.Rules); !sameStrings(got, wantIDs) {
		t.Fatalf("list ids = %#v, want %#v", got, wantIDs)
	}
	if list.Rules[0].Actions.MoveToFolder == nil || list.Rules[0].Actions.MoveToFolder.Path != "Personal/Archive" {
		t.Fatalf("personal folder target = %#v", list.Rules[0].Actions.MoveToFolder)
	}
	if list.Rules[3].Actions.MoveToFolder == nil || list.Rules[3].Actions.MoveToFolder.Path != "Work/Archive" {
		t.Fatalf("work folder target = %#v", list.Rules[3].Actions.MoveToFolder)
	}
	for _, rule := range list.Rules {
		if hasFinding(rule, "duplicate_sequence") {
			t.Fatalf("cross-account duplicate leaked into %s: %#v", rule.ID, rule.HealthFindings)
		}
	}

	var health ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--account", "personal", "--account", "work", "--json"), &health)
	if got := ruleIDs(health.Rules); !sameStrings(got, wantIDs) {
		t.Fatalf("health ids = %#v, want %#v", got, wantIDs)
	}
	for _, rule := range health.Rules {
		if rule.HealthStatus != mail.RuleHealthHealthy || len(rule.HealthFindings) != 0 {
			t.Fatalf("health for %s = %s %#v", rule.ID, rule.HealthStatus, rule.HealthFindings)
		}
	}

	for _, mode := range [][]string{{}, {"--plain"}} {
		command := append([]string{"rules", "list", "--account", "personal", "--account", "work"}, mode...)
		res := h.run(command...)
		decodeOK(t, res, nil)
		assertTextOrder(t, res.Stdout, wantIDs)
	}

	var envelope struct {
		Data ruleHealthResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("rules", "health", "--account", "personal", "--account", "work", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["rule_count"] != float64(len(wantIDs)) {
		t.Fatalf("meta rule count = %#v", envelope.Meta.Extra["rule_count"])
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"personal", "work"}) {
		t.Fatalf("meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake", "fake"}) {
		t.Fatalf("meta providers = %#v", got)
	}
	counts, ok := envelope.Meta.Extra["finding_counts"].(map[string]any)
	if !ok || counts[string(mail.RuleHealthInfo)] != float64(0) || counts[string(mail.RuleHealthWarning)] != float64(0) || counts[string(mail.RuleHealthError)] != float64(0) {
		t.Fatalf("meta finding counts = %#v", envelope.Meta.Extra["finding_counts"])
	}
	if _, ok := counts[string(mail.RuleHealthHealthy)]; ok {
		t.Fatalf("meta invented healthy finding bucket: %#v", counts)
	}
}

func TestRuleReadOutputIsDeterministic(t *testing.T) {
	h := seededRulesHarness(t, "work")
	commands := [][]string{
		{"rules", "list", "--account", "personal", "--account", "work"},
		{"rules", "list", "--account", "personal", "--account", "work", "--json"},
		{"rules", "list", "--account", "personal", "--account", "work", "--plain"},
		{"rules", "list", "--account", "personal", "--account", "work", "--json", "--jq", ".rules[].id"},
		{"rules", "list", "--account", "personal", "--account", "work", "--template", "{{range .rules}}{{.id}}\\n{{end}}"},
		{"rules", "list", "--account", "personal", "--account", "work", "--json", "--include-meta"},
		{"rules", "view", "rule-move-client"},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "view", "rule-move-client", "--json", "--jq", ".rule.id"},
		{"rules", "view", "rule-move-client", "--template", "{{.rule.id}}"},
		{"rules", "view", "rule-move-client", "--json", "--include-meta"},
		{"rules", "health", "--account", "personal", "--account", "work"},
		{"rules", "health", "--account", "personal", "--account", "work", "--json"},
		{"rules", "health", "--account", "personal", "--account", "work", "--plain"},
		{"rules", "health", "--account", "personal", "--account", "work", "--json", "--jq", ".rules[].id"},
		{"rules", "health", "--account", "personal", "--account", "work", "--template", "{{range .rules}}{{.id}}\\n{{end}}"},
		{"rules", "health", "--account", "personal", "--account", "work", "--json", "--include-meta"},
	}
	for _, args := range commands {
		first := h.run(args...)
		decodeOK(t, first, nil)
		for iteration := 1; iteration < 20; iteration++ {
			got := h.run(args...)
			if got.Exit != first.Exit || got.Stdout != first.Stdout || got.Stderr != first.Stderr {
				t.Fatalf("%v changed at iteration %d\nfirst stdout=%q\ngot stdout=%q\nfirst stderr=%q\ngot stderr=%q", args, iteration, first.Stdout, got.Stdout, first.Stderr, got.Stderr)
			}
		}
	}
}

func TestRulesDataShapeEdgeCases(t *testing.T) {
	yes := true
	no := false
	min := 10
	max := 20
	edgeID := "rule\twith\ncontrols-" + strings.Repeat("id", 80)
	edgeName := "Lokalisiert Ä\tName\nLine " + strings.Repeat("SehrLang", 30)
	rules := map[string][]mail.Rule{"work": {
		{
			ID:          edgeID,
			DisplayName: edgeName,
			Sequence:    1,
			IsEnabled:   true,
			Conditions: mail.RulePredicates{
				Semantics:         mail.DefaultRulePredicateSemantics(),
				SubjectContains:   []string{"one", "two"},
				RecipientContains: []string{"team"},
				MessageActionFlag: "any",
				WithinSizeRange:   &mail.RuleSizeRange{Minimum: &min, Maximum: &max},
				IsMeetingRequest:  &yes,
				SentToMe:          &no,
			},
			Exceptions: mail.RulePredicates{
				Semantics: mail.RulePredicateSemantics{
					Families:       "and\tfamily\nline",
					RepeatedValues: "or\rvalue\nline",
				},
				SubjectContains: []string{"skip"},
			},
			Actions: mail.RuleActions{
				MoveToFolder:   &mail.RuleFolderTarget{ProviderFolderID: "missing\tfolder\nid"},
				MarkAsRead:     &yes,
				MarkImportance: "high\tpriority\nline",
			},
			UnsupportedFields: []mail.RuleUnsupportedField{{Area: "rule", Field: "provider\tFuture\nName", ValueKind: "object", Count: 1, Summary: "unknown provider object with 1 field"}},
		},
	}}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", rules), "work")
	var list ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &list)
	if list.Rules[0].Conditions.Semantics.Families != "and" || list.Rules[0].Conditions.Semantics.RepeatedValues != "or" {
		t.Fatalf("semantics = %#v", list.Rules[0].Conditions.Semantics)
	}
	if list.Rules[0].Actions.MoveToFolder == nil || !list.Rules[0].Actions.MoveToFolder.Missing {
		t.Fatalf("missing target = %#v", list.Rules[0].Actions.MoveToFolder)
	}
	if !hasFinding(list.Rules[0], "missing_folder_target") || !hasFinding(list.Rules[0], "unsupported_field") {
		t.Fatalf("findings = %#v", list.Rules[0].HealthFindings)
	}
	plain := h.run("rules", "list", "--plain")
	decodeOK(t, plain, nil)
	if strings.Contains(plain.Stdout, "\trule\twith\ncontrols\t") || !strings.Contains(plain.Stdout, "rule with controls") || !strings.Contains(plain.Stdout, "Lokalisiert Ä Name Line") {
		t.Fatalf("plain did not sanitize controls: %q", plain.Stdout)
	}
	var view ruleViewResult
	decodeOK(t, h.run("rules", "view", edgeID, "--json"), &view)
	if len(view.Rule.Exceptions.SubjectContains) != 1 {
		t.Fatalf("exceptions = %#v", view.Rule.Exceptions)
	}
	human := h.run("rules", "view", edgeID)
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "  Message action flag: any") ||
		!strings.Contains(human.Stdout, "  Size: 10 to 20 KB") ||
		!strings.Contains(human.Stdout, "  Is meeting request: yes") ||
		!strings.Contains(human.Stdout, "  Sent to me: no") {
		t.Fatalf("human predicate fields = %q", human.Stdout)
	}
	if strings.Contains(human.Stdout, "rule\twith\ncontrols") || !strings.Contains(human.Stdout, "rule with controls") || !strings.Contains(human.Stdout, "Lokalisiert Ä Name Line") {
		t.Fatalf("human did not sanitize controls: %q", human.Stdout)
	}
	if strings.Contains(human.Stdout, "and\tfamily\nline") ||
		strings.Contains(human.Stdout, "or\rvalue\nline") ||
		!strings.Contains(human.Stdout, "Exceptions (conditions combine with and family line, values with or value line):") {
		t.Fatalf("human did not sanitize predicate semantics: %q", human.Stdout)
	}
	if strings.Contains(human.Stdout, "missing\tfolder\nid") ||
		strings.Contains(human.Stdout, "high\tpriority\nline") ||
		!strings.Contains(human.Stdout, "  Move to folder: missing folder id (missing)") ||
		!strings.Contains(human.Stdout, "  Set importance: high priority line") {
		t.Fatalf("human did not sanitize action summary: %q", human.Stdout)
	}
	health := h.run("rules", "health")
	decodeOK(t, health, nil)
	if strings.Contains(health.Stdout, "provider\tFuture\nName") || !strings.Contains(health.Stdout, "rule.provider Future Name") {
		t.Fatalf("health did not sanitize finding fields: %q", health.Stdout)
	}

	mutation := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", rules), "work")
	mutationHuman := mutation.run("rules", "create", "--name", edgeName, "--subject-contains", "Receipt", "--move-to", "Archive", "--apply")
	decodeOK(t, mutationHuman, nil)
	if strings.Contains(mutationHuman.Stdout, "Lokalisiert\tÄ\nName") || !strings.Contains(mutationHuman.Stdout, "Lokalisiert Ä Name Line") {
		t.Fatalf("mutation human did not sanitize controls: %q", mutationHuman.Stdout)
	}
}

func TestRuleHumanAndPlainOutputRejectProviderTerminalControls(t *testing.T) {
	unsafeText := "before\x00\x07\b\x1b[31mred\x1b[0m\x1b]0;title\x07after\x7f\u0085\u009b31m\u200bzero\u2028line\u2029paragraph\u202eoverride\u2066isolate"
	rules := map[string][]mail.Rule{"work": {
		{
			ID:          "unsafe-" + unsafeText,
			DisplayName: "Unsafe " + unsafeText,
			Sequence:    1,
			IsEnabled:   true,
			Conditions:  mail.RulePredicates{SubjectContains: []string{unsafeText}},
			Actions: mail.RuleActions{
				ForwardTo: []mail.Recipient{{Name: unsafeText, Address: "safe@example.test"}},
			},
			UnsupportedFields: []mail.RuleUnsupportedField{{Area: "rule", Field: unsafeText, ValueKind: "string", Count: 1, Summary: unsafeText}},
		},
	}}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", rules), "work")

	for _, args := range [][]string{
		{"rules", "list"},
		{"rules", "list", "--no-ansi"},
		{"rules", "list", "--plain"},
		{"rules", "view", "unsafe-" + unsafeText},
		{"rules", "view", "unsafe-" + unsafeText, "--no-ansi"},
		{"rules", "health"},
		{"rules", "health", "--no-ansi"},
		{"rules", "health", "--plain"},
	} {
		res := h.run(args...)
		decodeOK(t, res, nil)
		assertNoTerminalControls(t, res.Stdout)
		if args[len(args)-1] == "--plain" {
			assertTabSeparatedColumns(t, res.Stdout, 7)
		}
	}

	jsonResult := h.run("rules", "list", "--json")
	decodeOK(t, jsonResult, &ruleListResult{})
	if !json.Valid([]byte(jsonResult.Stdout)) {
		t.Fatalf("JSON output is invalid: %q", jsonResult.Stdout)
	}
}

func TestRulesReadOnlyCommandsDoNotMutateConfigRoot(t *testing.T) {
	h := seededRulesHarness(t, "work")
	before := hashTree(t, h.root)

	decodeOK(t, h.run("rules", "list", "--json"), &ruleListResult{})
	decodeOK(t, h.run("rules", "view", "rule-move-client", "--json"), &ruleViewResult{})
	decodeOK(t, h.run("rules", "health", "--json"), &ruleHealthResult{})
	decodeOK(t, h.run("rules", "list", "--dry-run", "--json"), &ruleListResult{})
	decodeOK(t, h.run("rules", "view", "rule-move-client", "--dry-run", "--json"), &ruleViewResult{})
	decodeOK(t, h.run("rules", "health", "--dry-run", "--json"), &ruleHealthResult{})

	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("rules reads mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRulesSecretSweep(t *testing.T) {
	h := seededRulesHarness(t, "work")
	account := folderWorkAccount()
	secrets := []string{"rules-access-token-12345", "rules-refresh-token-12345"}
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
	for _, args := range [][]string{
		{"rules", "list"},
		{"rules", "list", "--json"},
		{"rules", "list", "--json", "--jq", ".count"},
		{"rules", "list", "--template", "{{.count}}"},
		{"rules", "list", "--plain"},
		{"rules", "list", "--json", "--include-meta"},
		{"rules", "view", "rule-move-client"},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "view", "rule-move-client", "--json", "--jq", ".rule.id"},
		{"rules", "view", "rule-move-client", "--template", "{{.rule.id}}"},
		{"rules", "view", "rule-move-client", "--json", "--include-meta"},
		{"rules", "view", "missing", "--json"},
		{"rules", "health"},
		{"rules", "health", "--json"},
		{"rules", "health", "--plain"},
		{"rules", "health", "--json", "--include-meta"},
	} {
		res := h.run(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
	}

	mutating := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules()), "work")
	if err := testkit.SeedCredential(mutating.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed mutation credential: %v", err)
	}
	for _, args := range [][]string{
		{"rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--json"},
		{"rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--apply", "--json", "--include-meta"},
		{"rules", "update", "rule-move-client", "--subject-contains", "Receipt", "--json"},
		{"rules", "update", "rule-move-client", "--subject-contains", "Receipt", "--apply", "--json"},
		{"rules", "disable", "rule-move-client", "--dry-run", "--json"},
		{"rules", "disable", "rule-move-client", "--json"},
		{"rules", "delete", "rule-forward-alert", "--dry-run", "--json"},
		{"rules", "delete", "rule-forward-alert", "--json"},
	} {
		res := mutating.run(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, mutating.root, mutating.configPath)
	}
}

func TestRuleProviderStateNeverRenders(t *testing.T) {
	const sentinel = "provider-state-sentinel"
	rules := testkit.SampleRules()
	rule := rules["work"][0]
	rule.ProviderState = map[string]any{
		"conditions": map[string]any{"raw": sentinel},
		"actions":    map[string]any{"raw": sentinel},
	}
	rules["work"][0] = rule
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", rules), "work")

	for _, args := range [][]string{
		{"rules", "--help"},
		{"rules", "create", "--help"},
		{"rules", "update", "--help"},
		{"rules", "enable", "--help"},
		{"rules", "disable", "--help"},
		{"rules", "delete", "--help"},
		{"__rungrad_manifest"},
		{"rules", "list", "--json"},
		{"rules", "list", "--json", "--jq", ".rules[0].provider_state"},
		{"rules", "list", "--template", `{{printf "%#v" .rules}}`},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "view", "rule-move-client", "--json", "--jq", ".rule.provider_state"},
		{"rules", "view", "rule-move-client", "--template", `{{printf "%#v" .rule}}`},
		{"rules", "health", "--json"},
		{"rules", "health", "--json", "--jq", ".rules[0].provider_state"},
		{"rules", "health", "--template", `{{printf "%#v" .rules}}`},
		{"rules", "update", "rule-move-client", "--subject-contains", "preview", "--json"},
		{"rules", "disable", "rule-move-client", "--dry-run", "--json"},
		{"rules", "update", "rule-move-client", "--subject-contains", "applied", "--apply", "--json", "--include-meta"},
	} {
		res := h.run(args...)
		if res.Exit != 0 {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, res.Exit, res.Stdout, res.Stderr)
		}
		if strings.Contains(res.Stdout+res.Stderr, sentinel) || strings.Contains(res.Stdout+res.Stderr, "provider_state") {
			t.Fatalf("%v rendered provider state\nstdout=%s\nstderr=%s", args, res.Stdout, res.Stderr)
		}
	}
}

func TestFakeRuleProviderDoesNotAdvertiseRuleMutation(t *testing.T) {
	fake := testkit.NewFakeRuleProvider("fake", testkit.SampleRules())
	caps, err := fake.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.State(provider.CapRulesRead) != provider.CapImplemented || caps.State(provider.CapRulesMutate) != provider.CapUnsupported {
		t.Fatalf("rule capabilities = read:%s mutate:%s", caps.State(provider.CapRulesRead), caps.State(provider.CapRulesMutate))
	}
}

func TestRuleReadsTrackStatefulProviderWithoutWriting(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	readCommands := [][]string{
		{"rules", "list", "--json"},
		{"rules", "view", "rule-move-client", "--json"},
		{"rules", "health", "--json"},
	}
	before := make([]string, len(readCommands))
	for i, args := range readCommands {
		res := h.run(args...)
		decodeOK(t, res, nil)
		before[i] = res.Stdout
	}

	decodePreview(t, h.run("rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--json"))
	if fake.RuleCreateCalls.Load() != 0 || len(fake.RuleMutationCallLog) != 0 {
		t.Fatalf("preview wrote provider state calls=%d log=%#v", fake.RuleCreateCalls.Load(), fake.RuleMutationCallLog)
	}
	for i, args := range readCommands {
		res := h.run(args...)
		decodeOK(t, res, nil)
		if res.Stdout != before[i] {
			t.Fatalf("%v changed after preview\nbefore=%s\nafter=%s", args, before[i], res.Stdout)
		}
	}

	var created ruleMutationResult
	decodeOK(t, h.run("rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--apply", "--json"), &created)
	if created.RuleID == "" || fake.RuleCreateCalls.Load() != 1 {
		t.Fatalf("created=%#v calls=%d", created, fake.RuleCreateCalls.Load())
	}
	writesBeforeReads := len(fake.RuleMutationCallLog)
	createCalls := fake.RuleCreateCalls.Load()
	updateCalls := fake.RuleUpdateCalls.Load()
	deleteCalls := fake.RuleDeleteCalls.Load()

	var list ruleListResult
	decodeOK(t, h.run("rules", "list", "--json"), &list)
	if list.Count != 3 {
		t.Fatalf("list after create = %#v", list)
	}
	foundCreated := false
	for _, rule := range list.Rules {
		assertRuleReadSemantics(t, rule)
		if rule.ID == created.RuleID {
			foundCreated = true
			if rule.Actions.MoveToFolder == nil || rule.Actions.MoveToFolder.Path != "Archive" {
				t.Fatalf("created rule target = %#v", rule.Actions.MoveToFolder)
			}
		}
	}
	if !foundCreated {
		t.Fatalf("created rule %q absent from %#v", created.RuleID, ruleIDs(list.Rules))
	}
	var view ruleViewResult
	decodeOK(t, h.run("rules", "view", created.RuleID, "--json"), &view)
	assertRuleReadSemantics(t, view.Rule)
	var health ruleHealthResult
	decodeOK(t, h.run("rules", "health", "--json"), &health)
	if health.Count != 3 {
		t.Fatalf("health after create = %#v", health)
	}
	if fake.RuleCreateCalls.Load() != createCalls || fake.RuleUpdateCalls.Load() != updateCalls || fake.RuleDeleteCalls.Load() != deleteCalls || len(fake.RuleMutationCallLog) != writesBeforeReads {
		t.Fatalf("reads wrote provider state create=%d/%d update=%d/%d delete=%d/%d log=%d/%d",
			fake.RuleCreateCalls.Load(), createCalls, fake.RuleUpdateCalls.Load(), updateCalls,
			fake.RuleDeleteCalls.Load(), deleteCalls, len(fake.RuleMutationCallLog), writesBeforeReads)
	}
}

func TestRuleMutationPreviewApplyAndVerificationFakeProvider(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	help := h.run("rules", "create", "--help")
	decodeOK(t, help, nil)
	if !strings.Contains(help.Stdout, "any value matches") ||
		!strings.Contains(help.Stdout, "different conditions must all match") {
		t.Fatalf("create help predicate semantics = %q", help.Stdout)
	}

	fake.FolderListCalls.Store(0)
	preview := decodePreview(t, h.run("rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--json"))
	body := preview["body"].(map[string]any)
	if body["operation"] != string(mail.RuleOperationCreate) ||
		body["apply_required"] != "true" ||
		body["predicate_families"] != "conjunctive" ||
		body["predicate_repeated_values"] != "disjunctive" ||
		body["move_to_folder_after_id"] != "archive-id" ||
		body["move_to_folder_before"] != "absent" {
		t.Fatalf("create preview body = %#v", body)
	}
	if fake.RuleCreateCalls.Load() != 0 {
		t.Fatalf("dry-run create calls = %d", fake.RuleCreateCalls.Load())
	}
	if fake.FolderListCalls.Load() != 1 {
		t.Fatalf("folder reads for create preview = %d, want 1", fake.FolderListCalls.Load())
	}

	var createEnvelope struct {
		Data ruleMutationResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("rules", "create", "--from", "Client <new-client@example.test>", "--move-to", "Archive", "--mark-read", "--apply", "--json", "--include-meta"), &createEnvelope)
	created := createEnvelope.Data
	if fake.RuleCreateCalls.Load() != 1 || created.RuleID == "" || created.Verification == nil || created.Verification.Status != mail.RuleVerificationVerified {
		t.Fatalf("created = %#v calls=%d", created, fake.RuleCreateCalls.Load())
	}
	if createEnvelope.Meta.Extra["verification_status"] != mail.RuleVerificationVerified ||
		createEnvelope.Meta.Extra["operation"] != string(mail.RuleOperationCreate) {
		t.Fatalf("create meta = %#v", createEnvelope.Meta.Extra)
	}

	var update ruleMutationResult
	decodeOK(t, h.run("rules", "update", "rule-move-client", "--subject-contains", "invoice", "--subject-contains", "receipt", "--no-mark-read", "--apply", "--json"), &update)
	if fake.RuleUpdateCalls.Load() != 1 || update.Operation != string(mail.RuleOperationUpdate) ||
		update.ChangeSet == nil || update.ChangeSet.SubjectContains == nil || update.ChangeSet.SubjectContains.Count != 2 ||
		update.ChangeSet.MarkAsRead == nil || !update.ChangeSet.MarkAsRead.Cleared ||
		update.Verification == nil || update.Verification.Status != mail.RuleVerificationVerified {
		t.Fatalf("update = %#v calls=%d", update, fake.RuleUpdateCalls.Load())
	}

	disableCalls := fake.RuleUpdateCalls.Load()
	decodeOK(t, h.run("rules", "disable", "rule-move-client", "--dry-run", "--json"), nil)
	if fake.RuleUpdateCalls.Load() != disableCalls {
		t.Fatalf("dry-run disable mutated: before=%d after=%d", disableCalls, fake.RuleUpdateCalls.Load())
	}
	var disable ruleMutationResult
	decodeOK(t, h.run("rules", "disable", "rule-move-client", "--json"), &disable)
	if disable.Operation != string(mail.RuleOperationDisable) || disable.Verification == nil || disable.Verification.Status != mail.RuleVerificationVerified {
		t.Fatalf("disable = %#v", disable)
	}

	deletePreview := decodePreview(t, h.run("rules", "delete", "rule-forward-alert", "--dry-run", "--json"))
	deleteBody := deletePreview["body"].(map[string]any)
	if deleteBody["confirmation_required"] != "true" || fake.RuleDeleteCalls.Load() != 0 {
		t.Fatalf("delete preview = %#v calls=%d", deleteBody, fake.RuleDeleteCalls.Load())
	}
	missingConfirm := h.run("rules", "delete", "rule-forward-alert", "--json")
	assertErrorCode(t, missingConfirm, errs.CodeConfirmationRequired)
	assertRuleConfirmationDoesNotLeak(t, missingConfirm, "rule-forward-alert")
	wrongConfirm := h.run("rules", "delete", "rule-forward-alert", "--confirm", "wrong", "--json")
	assertErrorCode(t, wrongConfirm, errs.CodeConfirmationRequired)
	assertRuleConfirmationDoesNotLeak(t, wrongConfirm, "rule-forward-alert")
	var deleted ruleMutationResult
	decodeOK(t, h.run("rules", "delete", "rule-forward-alert", "--confirm", "rule-forward-alert", "--json"), &deleted)
	if fake.RuleDeleteCalls.Load() != 1 || deleted.Operation != string(mail.RuleOperationDelete) ||
		deleted.Verification == nil || deleted.Verification.Status != mail.RuleVerificationVerified ||
		!deleted.ConfirmationRequired || !deleted.ConfirmationMatched {
		t.Fatalf("deleted = %#v calls=%d", deleted, fake.RuleDeleteCalls.Load())
	}
	wantLog := []string{"create:work:", "rule_update:work:rule-move-client", "rule_disable:work:rule-move-client", "delete:work:rule-forward-alert"}
	if !sameStrings(fake.RuleMutationCallLog, wantLog) {
		t.Fatalf("rule mutation call log = %#v, want %#v", fake.RuleMutationCallLog, wantLog)
	}
	if entries := readActionLog(t, h); len(entries) != len(wantLog) {
		t.Fatalf("rule mutations logged %d entries, want %d", len(entries), len(wantLog))
	}
}

func TestRuleMutationUsesSingleFolderSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "create preview", args: []string{"rules", "create", "--subject-contains", "Receipt", "--move-to", "Archive", "--json"}},
		{name: "update preview", args: []string{"rules", "update", "rule-move-client", "--subject-contains", "Receipt", "--json"}},
		{name: "enable dry-run", args: []string{"rules", "enable", "rule-forward-alert", "--dry-run", "--json"}},
		{name: "disable dry-run", args: []string{"rules", "disable", "rule-move-client", "--dry-run", "--json"}},
		{name: "delete dry-run", args: []string{"rules", "delete", "rule-forward-alert", "--dry-run", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
			h := seededRulesHarnessWithProvider(t, fake, "work")
			fake.FolderListCalls.Store(0)
			decodeOK(t, h.run(tt.args...), nil)
			if fake.FolderListCalls.Load() != 1 {
				t.Fatalf("%s folder reads = %d, want 1", tt.name, fake.FolderListCalls.Load())
			}
			if len(fake.RuleMutationCallLog) != 0 {
				t.Fatalf("%s wrote during dry-run/preview: %#v", tt.name, fake.RuleMutationCallLog)
			}
		})
	}
}

func TestRuleMutationUsageAndCapabilityFailures(t *testing.T) {
	fake := testkit.NewFakeRuleMutationProvider("fake", testkit.SampleRules())
	h := seededRulesHarnessWithProvider(t, fake, "work")
	for _, args := range [][]string{
		{"rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--dry-run", "--apply", "--json"},
		{"rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--json", "--include-meta"},
		{"rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--plain"},
		{"rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--all-accounts", "--json"},
		{"rules", "delete", "rule-forward-alert", "--all-accounts", "--json"},
		{"rules", "update", "rule-move-client", "--has-attachments", "--no-has-attachments", "--json"},
		{"rules", "update", "rule-move-client", "--mark-read", "--no-mark-read", "--json"},
		{"rules", "update", "rule-move-client", "--stop-processing", "--no-stop-processing", "--json"},
	} {
		res := h.run(args...)
		assertFrameworkUsage(t, res)
	}
	noPromptDelete := h.run("rules", "delete", "rule-forward-alert", "--no-prompt", "--json")
	assertErrorCode(t, noPromptDelete, errs.CodeConfirmationRequired)
	assertRuleConfirmationDoesNotLeak(t, noPromptDelete, "rule-forward-alert")
	beforeFolders := fake.FolderListCalls.Load()
	beforeLog := len(fake.RuleMutationCallLog)
	res := h.run("rules", "create", "--subject-contains", "x", "--move-to", "Missing", "--json")
	assertErrorCodeExit(t, res, errs.CodeFolderNotFound, 5)
	if fake.FolderListCalls.Load() != beforeFolders+1 || len(fake.RuleMutationCallLog) != beforeLog {
		t.Fatalf("invalid move target reads/log = folders:%d->%d log:%d->%d", beforeFolders, fake.FolderListCalls.Load(), beforeLog, len(fake.RuleMutationCallLog))
	}
	for _, args := range [][]string{
		{"rules", "create", "--subject-contains", "x", "--json"},
		{"rules", "create", "--move-to", "Archive", "--json"},
		{"rules", "create", "--subject-contains", "", "--move-to", "Archive", "--json"},
		{"rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--sequence", "0", "--json"},
		{"rules", "update", "rule-move-client", "--json"},
	} {
		beforeFolders := fake.FolderListCalls.Load()
		beforeCreate := fake.RuleCreateCalls.Load()
		beforeUpdate := fake.RuleUpdateCalls.Load()
		beforeLog := len(fake.RuleMutationCallLog)
		res := h.run(args...)
		assertErrorCode(t, res, errs.CodeInvalidRuleInput)
		if fake.FolderListCalls.Load() != beforeFolders ||
			fake.RuleCreateCalls.Load() != beforeCreate ||
			fake.RuleUpdateCalls.Load() != beforeUpdate ||
			len(fake.RuleMutationCallLog) != beforeLog {
			t.Fatalf("%v touched provider before local validation: folders=%d/%d create=%d/%d update=%d/%d log=%d/%d",
				args,
				beforeFolders, fake.FolderListCalls.Load(),
				beforeCreate, fake.RuleCreateCalls.Load(),
				beforeUpdate, fake.RuleUpdateCalls.Load(),
				beforeLog, len(fake.RuleMutationCallLog))
		}
	}

	pending := testkit.NewFakePendingRuleMutationProvider("fake")
	h = seededRulesHarnessWithProvider(t, pending, "work")
	assertErrorCode(t, h.run("rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--json"), errs.CodeCapabilityNotImplemented)

	stale := testkit.NewFakeStaleRuleMutationProvider("fake")
	h = seededRulesHarnessWithProvider(t, stale, "work")
	res = h.run("rules", "create", "--subject-contains", "x", "--move-to", "Archive", "--json")
	assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
	body := errorJSON(t, res)
	if body["capability"] != string(provider.CapRulesMutate) {
		t.Fatalf("stale rule manager body = %#v", body)
	}
}

func assertRuleConfirmationDoesNotLeak(t *testing.T, res testutil.Result, forbidden ...string) {
	t.Helper()
	body := errorJSON(t, res)
	if _, ok := body["rule_id"]; ok {
		t.Fatalf("confirmation error leaked rule id field: %#v", body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal confirmation body: %v", err)
	}
	for _, token := range forbidden {
		if strings.Contains(string(encoded), token) {
			t.Fatalf("confirmation error leaked token %q: %s", token, encoded)
		}
	}
}

func TestRulesErrorDetailsJSON(t *testing.T) {
	h := seededRulesHarness(t, "work")
	res := h.run("rules", "view", "missing", "--json")
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("decode stderr: %v\n%s", err, res.Stderr)
	}
	if body["exit_code"] != float64(5) || body["code"] != errs.CodeRuleNotFound {
		t.Fatalf("body = %#v", body)
	}
}

func seededRulesHarness(t *testing.T, active string) *cliHarness {
	t.Helper()
	return seededRulesHarnessWithProvider(t, testkit.NewFakeRuleProvider("fake", testkit.SampleRules()), active)
}

func seededRulesHarnessWithProvider(t *testing.T, fake *testkit.FakeProvider, active string) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	accounts := []maildoconfig.Account{folderWorkAccount(), folderPersonalAccount()}
	seedRegistry(t, h.configPath, active, accounts)
	for _, account := range accounts {
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential %s: %v", account.Name, err)
		}
	}
	return h
}

func hasFinding(rule mail.Rule, code string) bool {
	for _, finding := range rule.HealthFindings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func ruleIDs(rules []mail.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.ID)
	}
	return out
}

func decodeRuleJSONObject(t *testing.T, res testutil.Result) map[string]any {
	t.Helper()
	decodeOK(t, res, nil)
	decoder := json.NewDecoder(strings.NewReader(res.Stdout))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("decode JSON object: %v\n%s", err, res.Stdout)
	}
	return out
}

func ruleJSONObject(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want JSON object: %#v", value, value)
	}
	return out
}

func ruleJSONArray(t *testing.T, value any) []map[string]any {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("value is %T, want JSON array: %#v", value, value)
	}
	out := make([]map[string]any, len(values))
	for i, value := range values {
		out[i] = ruleJSONObject(t, value)
	}
	return out
}

func assertRuleJSONKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !sameStrings(got, want) {
		t.Fatalf("JSON keys = %#v, want %#v in %#v", got, want, object)
	}
}

func assertTextOrder(t *testing.T, output string, values []string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(output[position+1:], value)
		if next < 0 {
			t.Fatalf("output does not contain %q after byte %d: %q", value, position, output)
		}
		position += next + 1
	}
}

func assertRuleReadSemantics(t *testing.T, rule mail.Rule) {
	t.Helper()
	if rule.Conditions.Semantics.Families != "and" || rule.Conditions.Semantics.RepeatedValues != "or" {
		t.Fatalf("conditions semantics = %#v", rule.Conditions.Semantics)
	}
}

func assertTabSeparatedColumns(t *testing.T, output string, want int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if got := strings.Count(line, "\t") + 1; got != want {
			t.Fatalf("plain row has %d columns, want %d: %q", got, want, line)
		}
	}
}

func assertNoTerminalControls(t *testing.T, text string) {
	t.Helper()
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("output contains terminal/format control U+%04X: %q", r, text)
		}
	}
}

func FuzzPlainFieldRejectsControlBoundaries(f *testing.F) {
	for _, seed := range []string{
		"ordinary localized text Ä规则",
		"tabs\tlines\r\n",
		"\x00\x07\x1b[31mcolor\x1b[0m\x7f",
		"zero\u200bwidth\u202eline\u2029paragraph\u2066isolate",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := plainField(input)
		for _, r := range got {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
				t.Fatalf("plainField retained U+%04X in %q", r, got)
			}
		}
	})
}
