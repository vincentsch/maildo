package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func FuzzRuleMutationPreviewSeparatesMachineAndHumanValues(f *testing.F) {
	for _, seed := range []string{
		"ordinary localized value Ä规则",
		"line\ncarriage\r\x1b[31mcolor\x1b[0m",
		"zero\u200bwidth\u202eoverride\u2029paragraph",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !utf8.ValidString(value) || len(value) > 256 {
			t.Skip()
		}
		raw := "value-" + value
		account := provider.ResolvedAccount{Name: "work", Provider: "fake"}
		rule := mail.Rule{
			ID: raw, DisplayName: raw, Sequence: 10, IsEnabled: true,
			Conditions: mail.RulePredicates{SubjectContains: []string{raw}},
		}
		change := mail.RuleChangeSet{
			Operation: mail.RuleOperationUpdate,
			SubjectContains: &mail.RuleStringCollectionChange{
				Set: true, Before: []string{raw}, After: []string{raw + "-after"}, Count: 1,
			},
		}
		preview := previewRuleMutation(account, rule, change, true, false, false)
		encoded, err := preview.JSON()
		if err != nil {
			t.Fatal(err)
		}
		var machine map[string]any
		if err := json.Unmarshal(encoded, &machine); err != nil {
			t.Fatal(err)
		}
		body := machine["body"].(map[string]any)
		if body["rule_id"] != raw || body["display_name"] != raw ||
			body["subject_contains_before_1"] != raw || body["subject_contains_after_1"] != raw+"-after" {
			t.Fatalf("machine preview changed values: %#v", body)
		}

		var human bytes.Buffer
		humanRulePreview(account, rule, change, "maildo rules update "+raw)(&human)
		assertNoTerminalControls(t, human.String())
	})
}

func FuzzRuleDeleteConfirmationNeverEchoesRejectedTokens(f *testing.F) {
	for _, seed := range [][]byte{{0}, []byte("Mixed-Case"), []byte(" spaces "), {0, 1, 2, 255}} {
		for variant := uint8(0); variant < 4; variant++ {
			f.Add(seed, variant)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte, variant uint8) {
		if len(raw) > 64 {
			t.Skip()
		}
		token := hex.EncodeToString(raw)
		if token == "" {
			token = "empty"
		}
		target := "target_token_" + token
		supplied := "supplied_token_" + token
		switch variant % 4 {
		case 1:
			supplied = strings.ToUpper(target)
		case 2:
			supplied = " " + target + " "
		case 3:
			supplied = target + "_suffix"
		}
		if ruleDeleteConfirmationMatches(target, supplied) {
			t.Fatalf("non-exact confirmation matched target=%q supplied=%q", target, supplied)
		}
		product := ruleConfirmationRequiredError(provider.ResolvedAccount{Name: "work", Provider: "fake"}).(*errs.Error)
		details, err := json.Marshal(product.ErrorDetails())
		if err != nil {
			t.Fatal(err)
		}
		rendered := product.Error() + string(details)
		for _, forbidden := range []string{target, supplied, strings.TrimSpace(supplied), strings.ToLower(target), strings.ToLower(strings.TrimSpace(supplied))} {
			if forbidden != "" && strings.Contains(rendered, forbidden) {
				t.Fatalf("confirmation error exposed %q: %s", forbidden, rendered)
			}
		}
	})
}
