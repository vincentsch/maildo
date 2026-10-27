package rules

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

func FuzzRuleProviderStateClonePreservesJSONNumbers(f *testing.F) {
	for _, seed := range []struct {
		value int64
		shape uint8
	}{
		{0, 0},
		{9007199254740993, 0},
		{-9007199254740993, 1},
		{123456789, 2},
	} {
		f.Add(seed.value, seed.shape)
	}
	f.Fuzz(func(t *testing.T, value int64, shape uint8) {
		lexeme := strconv.FormatInt(value, 10)
		switch shape % 3 {
		case 1:
			lexeme += ".125"
		case 2:
			lexeme += "e+17"
		}
		number := json.Number(lexeme)
		source := map[string]any{
			"direct": number,
			"nested": []any{map[string]any{"value": number}, []any{number}},
		}
		cloned := cloneAnyMap(source)
		if got, ok := cloned["direct"].(json.Number); !ok || got.String() != lexeme {
			t.Fatalf("direct number = %#v, want json.Number(%q)", cloned["direct"], lexeme)
		}
		nested := cloned["nested"].([]any)
		if got, ok := nested[0].(map[string]any)["value"].(json.Number); !ok || got.String() != lexeme {
			t.Fatalf("nested number = %#v, want json.Number(%q)", nested[0], lexeme)
		}
		if got, ok := nested[1].([]any)[0].(json.Number); !ok || got.String() != lexeme {
			t.Fatalf("array number = %#v, want json.Number(%q)", nested[1], lexeme)
		}

		nested[0].(map[string]any)["value"] = json.Number("1")
		nested[1].([]any)[0] = json.Number("2")
		original := source["nested"].([]any)
		if original[0].(map[string]any)["value"].(json.Number).String() != lexeme || original[1].([]any)[0].(json.Number).String() != lexeme {
			t.Fatalf("clone mutation changed source: %#v", source)
		}
	})
}

func FuzzRuleVerificationDetectsSingleFieldDrift(f *testing.F) {
	paths := []string{
		"rule_id",
		"display_name",
		"sequence",
		"is_enabled",
		"conditions.subject_contains",
		"actions.mark_as_read",
		"exceptions",
		"unsupported_fields",
		"presence.conditions.has_attachments",
		"presence.actions.mark_as_read",
		"presence.actions.stop_processing_rules",
	}
	for i := range paths {
		f.Add(uint8(i), "seed")
	}
	f.Fuzz(func(t *testing.T, selector uint8, value string) {
		if len(value) > 128 {
			t.Skip()
		}
		path := paths[int(selector)%len(paths)]
		expected := completeMutationDefinitionFixture()
		observed := cloneRule(expected)
		drift := "drift-" + value
		switch path {
		case "rule_id":
			observed.ID = drift
		case "display_name":
			observed.DisplayName = drift
		case "sequence":
			observed.Sequence++
		case "is_enabled":
			observed.IsEnabled = !observed.IsEnabled
		case "conditions.subject_contains":
			observed.Conditions.SubjectContains = []string{drift}
		case "actions.mark_as_read":
			value := observed.Actions.MarkAsRead == nil || !*observed.Actions.MarkAsRead
			observed.Actions.MarkAsRead = &value
		case "exceptions":
			observed.Exceptions.SubjectContains = []string{drift}
		case "unsupported_fields":
			observed.UnsupportedFields = append(observed.UnsupportedFields, mail.RuleUnsupportedField{Area: "actions", Field: drift, ValueKind: "string", Count: 1})
		case "presence.conditions.has_attachments":
			observed.ProviderFieldPresence.ConditionsHasAttachments = !observed.ProviderFieldPresence.ConditionsHasAttachments
		case "presence.actions.mark_as_read":
			observed.ProviderFieldPresence.ActionsMarkAsRead = !observed.ProviderFieldPresence.ActionsMarkAsRead
		case "presence.actions.stop_processing_rules":
			observed.ProviderFieldPresence.ActionsStopProcessingRules = !observed.ProviderFieldPresence.ActionsStopProcessingRules
		}

		change := mail.RuleChangeSet{Operation: mail.RuleOperationUpdate}
		verification := VerifyRuleMutation(expected, observed, change)
		if verification.Status != mail.RuleVerificationFailed || len(verification.Mismatches) != 1 || verification.Mismatches[0].Field != path {
			t.Fatalf("path %q verification = %#v", path, verification)
		}
	})
}
