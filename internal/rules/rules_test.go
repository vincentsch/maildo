package rules

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestAnnotateListResolvesTargetsAndFindings(t *testing.T) {
	yes := true
	rules := []mail.Rule{
		{
			Account:     "work",
			Provider:    "fake",
			Scope:       mail.RuleScopeInbox,
			ID:          "a",
			DisplayName: "Move external",
			Sequence:    7,
			IsEnabled:   false,
			IsReadOnly:  true,
			HasError:    true,
			Conditions:  mail.RulePredicates{SubjectContains: []string{"invoice"}},
			Actions: mail.RuleActions{
				MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "missing-id"},
				ForwardTo:    []mail.Recipient{{Address: "ops@external.test"}},
				MarkAsRead:   &yes,
			},
			UnsupportedFields: []mail.RuleUnsupportedField{
				{Area: "actions", Field: "forwardTo", ValueKind: "array", Count: 1},
				{Area: "conditions", Field: "bodyContains", ValueKind: "array", Count: 1},
			},
		},
		{
			Account:     "work",
			Provider:    "fake",
			Scope:       mail.RuleScopeInbox,
			ID:          "b",
			DisplayName: "Archive",
			Sequence:    7,
			IsEnabled:   true,
			Actions:     mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "archive-id"}},
		},
	}
	folders := []mail.Folder{{ID: "archive-id", DisplayName: "Archive", Path: "Archive", WellKnownName: mail.WellKnownArchive}}

	got := AnnotateList(rules, folders, "work@example.test")
	if got[0].Actions.MoveToFolder == nil || !got[0].Actions.MoveToFolder.Missing {
		t.Fatalf("missing target not marked: %#v", got[0].Actions.MoveToFolder)
	}
	if got[1].Actions.MoveToFolder == nil || got[1].Actions.MoveToFolder.Path != "Archive" || got[1].Actions.MoveToFolder.Missing {
		t.Fatalf("archive target not resolved: %#v", got[1].Actions.MoveToFolder)
	}
	if got[0].HealthStatus != mail.RuleHealthError || got[1].HealthStatus != mail.RuleHealthWarning {
		t.Fatalf("statuses = %s/%s", got[0].HealthStatus, got[1].HealthStatus)
	}
	assertFindingCodes(t, got[0], []string{
		FindingProviderError,
		FindingDisabledRule,
		FindingReadOnlyRule,
		FindingDuplicateSequence,
		FindingMissingFolder,
		FindingUnsupportedField,
		FindingUnsupportedField,
		FindingForwardingAction,
		FindingExternalForwarding,
	})
	if got[0].Conditions.Semantics.Families != "and" || got[0].Conditions.Semantics.RepeatedValues != "or" {
		t.Fatalf("semantics = %#v", got[0].Conditions.Semantics)
	}
	if got[0].Exceptions.Semantics.Families != "and" || got[0].Exceptions.Semantics.RepeatedValues != "or" {
		t.Fatalf("exception semantics = %#v", got[0].Exceptions.Semantics)
	}
}

func TestAnnotateRuleDefaultsPredicateSemanticsIndependently(t *testing.T) {
	tests := []struct {
		name           string
		conditions     mail.RulePredicateSemantics
		exceptions     mail.RulePredicateSemantics
		wantConditions mail.RulePredicateSemantics
		wantExceptions mail.RulePredicateSemantics
	}{
		{
			name:           "conditions family only is missing",
			conditions:     mail.RulePredicateSemantics{RepeatedValues: "provider-repeated"},
			exceptions:     mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			wantConditions: mail.RulePredicateSemantics{Families: "and", RepeatedValues: "provider-repeated"},
			wantExceptions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
		},
		{
			name:           "conditions repeated values only is missing",
			conditions:     mail.RulePredicateSemantics{Families: "provider-family"},
			exceptions:     mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			wantConditions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "or"},
			wantExceptions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
		},
		{
			name:           "exceptions family only is missing",
			conditions:     mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			exceptions:     mail.RulePredicateSemantics{RepeatedValues: "provider-repeated"},
			wantConditions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			wantExceptions: mail.RulePredicateSemantics{Families: "and", RepeatedValues: "provider-repeated"},
		},
		{
			name:           "exceptions repeated values only is missing",
			conditions:     mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			exceptions:     mail.RulePredicateSemantics{Families: "provider-family"},
			wantConditions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "provider-repeated"},
			wantExceptions: mail.RulePredicateSemantics{Families: "provider-family", RepeatedValues: "or"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnnotateRule(mail.Rule{
				ID:         "rule",
				Conditions: mail.RulePredicates{Semantics: tt.conditions},
				Exceptions: mail.RulePredicates{Semantics: tt.exceptions},
			}, nil, "", nil)
			if got.Conditions.Semantics != tt.wantConditions {
				t.Fatalf("conditions semantics = %#v, want %#v", got.Conditions.Semantics, tt.wantConditions)
			}
			if got.Exceptions.Semantics != tt.wantExceptions {
				t.Fatalf("exceptions semantics = %#v, want %#v", got.Exceptions.Semantics, tt.wantExceptions)
			}
		})
	}
}

func FuzzAnnotateRulePreservesPredicateSemantics(f *testing.F) {
	for _, seed := range []struct {
		families string
		repeated string
	}{
		{},
		{families: "and", repeated: "or"},
		{families: "provider-family"},
		{repeated: "provider-repeated"},
		{families: "\x1b[31mfamily", repeated: "值"},
	} {
		f.Add(seed.families, seed.repeated)
	}
	f.Fuzz(func(t *testing.T, families, repeated string) {
		input := mail.RulePredicateSemantics{Families: families, RepeatedValues: repeated}
		rule := mail.Rule{ID: "rule", Conditions: mail.RulePredicates{Semantics: input}, Exceptions: mail.RulePredicates{Semantics: input}}
		got := AnnotateRule(rule, nil, "", nil)
		wantFamilies := families
		if wantFamilies == "" {
			wantFamilies = "and"
		}
		wantRepeated := repeated
		if wantRepeated == "" {
			wantRepeated = "or"
		}
		for name, semantics := range map[string]mail.RulePredicateSemantics{
			"conditions": got.Conditions.Semantics,
			"exceptions": got.Exceptions.Semantics,
		} {
			if semantics.Families != wantFamilies || semantics.RepeatedValues != wantRepeated {
				t.Fatalf("%s semantics = %#v, want families=%q repeated=%q", name, semantics, wantFamilies, wantRepeated)
			}
		}
		if rule.Conditions.Semantics != input || rule.Exceptions.Semantics != input {
			t.Fatalf("annotation mutated input semantics: conditions=%#v exceptions=%#v input=%#v", rule.Conditions.Semantics, rule.Exceptions.Semantics, input)
		}
	})
}

func TestExternalForwardingSkippedWithoutMailboxEmail(t *testing.T) {
	got := AnnotateList([]mail.Rule{{
		ID:        "forward",
		Sequence:  1,
		IsEnabled: true,
		Actions:   mail.RuleActions{RedirectTo: []mail.Recipient{{Address: "person@external.test"}}},
	}}, nil, "")
	assertFindingCodes(t, got[0], []string{FindingForwardingAction})
}

func TestForwardingFindingFamiliesAndRecipientDomains(t *testing.T) {
	tests := []struct {
		name       string
		actions    mail.RuleActions
		mailbox    string
		wantCodes  []string
		wantFields []string
	}{
		{
			name:       "internal forward",
			actions:    mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "person@EXAMPLE.TEST"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction},
			wantFields: []string{"actions.forward_to"},
		},
		{
			name:       "external forward",
			actions:    mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "person@external.test"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction, FindingExternalForwarding},
			wantFields: []string{"actions.forward_to", "actions.forward_to"},
		},
		{
			name:       "unknown recipient domain",
			actions:    mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "not-an-address"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction},
			wantFields: []string{"actions.forward_to"},
		},
		{
			name:       "malformed recipient domain",
			actions:    mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "person@@external.test"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction},
			wantFields: []string{"actions.forward_to"},
		},
		{
			name:       "malformed mailbox domain",
			actions:    mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "person@external.test"}}},
			mailbox:    "owner@@example.test",
			wantCodes:  []string{FindingForwardingAction},
			wantFields: []string{"actions.forward_to"},
		},
		{
			name:       "forward as attachment",
			actions:    mail.RuleActions{ForwardAsAttachment: []mail.Recipient{{Address: "person@example.test"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction},
			wantFields: []string{"actions.forward_as_attachment_to"},
		},
		{
			name:       "external redirect",
			actions:    mail.RuleActions{RedirectTo: []mail.Recipient{{Address: "person@external.test"}}},
			mailbox:    "owner@example.test",
			wantCodes:  []string{FindingForwardingAction, FindingExternalForwarding},
			wantFields: []string{"actions.redirect_to", "actions.redirect_to"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnnotateRule(mail.Rule{ID: "forward", Sequence: 1, IsEnabled: true, Actions: tt.actions}, nil, tt.mailbox, nil)
			assertFindingCodes(t, got, tt.wantCodes)
			fields := make([]string, 0, len(got.HealthFindings))
			for _, finding := range got.HealthFindings {
				fields = append(fields, finding.Field)
			}
			if !reflect.DeepEqual(fields, tt.wantFields) {
				t.Fatalf("finding fields = %#v, want %#v", fields, tt.wantFields)
			}
			if got.HealthStatus != mail.RuleHealthWarning {
				t.Fatalf("health status = %s, want warning", got.HealthStatus)
			}
		})
	}
}

func TestMissingCopyTargetAndHealthStatusReduction(t *testing.T) {
	copyRule := AnnotateRule(mail.Rule{
		ID:        "copy",
		Sequence:  1,
		IsEnabled: true,
		Actions:   mail.RuleActions{CopyToFolder: &mail.RuleFolderTarget{ProviderFolderID: "missing-copy"}},
	}, nil, "", nil)
	assertFindingCodes(t, copyRule, []string{FindingMissingFolder})
	if copyRule.Actions.CopyToFolder == nil || !copyRule.Actions.CopyToFolder.Missing ||
		copyRule.HealthFindings[0].Field != "actions.copy_to_folder" || copyRule.HealthStatus != mail.RuleHealthError {
		t.Fatalf("copy rule = %#v", copyRule)
	}

	tests := []struct {
		name string
		rule mail.Rule
		want mail.RuleHealthStatus
	}{
		{name: "healthy", rule: mail.Rule{ID: "healthy", Sequence: 1, IsEnabled: true}, want: mail.RuleHealthHealthy},
		{name: "info", rule: mail.Rule{ID: "disabled", Sequence: 1}, want: mail.RuleHealthInfo},
		{name: "warning", rule: mail.Rule{ID: "forward", Sequence: 1, IsEnabled: true, Actions: mail.RuleActions{ForwardTo: []mail.Recipient{{Address: "person@example.test"}}}}, want: mail.RuleHealthWarning},
		{name: "error", rule: mail.Rule{ID: "error", Sequence: 1, IsEnabled: true, HasError: true}, want: mail.RuleHealthError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AnnotateRule(tt.rule, nil, "owner@example.test", nil).HealthStatus; got != tt.want {
				t.Fatalf("health status = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAnnotateListDoesNotMutateInputTargets(t *testing.T) {
	min := 10
	max := 20
	in := []mail.Rule{{
		ID:        "move",
		Sequence:  1,
		IsEnabled: true,
		Conditions: mail.RulePredicates{
			WithinSizeRange: &mail.RuleSizeRange{Minimum: &min, Maximum: &max},
		},
		Actions: mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "archive-id"}},
	}}
	got := AnnotateList(in, []mail.Folder{{ID: "archive-id", DisplayName: "Archive", Path: "Archive"}}, "")
	if got[0].Actions.MoveToFolder == nil || got[0].Actions.MoveToFolder.Path != "Archive" {
		t.Fatalf("annotated target = %#v", got[0].Actions.MoveToFolder)
	}
	if got[0].Conditions.WithinSizeRange == nil || got[0].Conditions.WithinSizeRange.Minimum == nil || *got[0].Conditions.WithinSizeRange.Minimum != 10 ||
		got[0].Conditions.WithinSizeRange.Maximum == nil || *got[0].Conditions.WithinSizeRange.Maximum != 20 {
		t.Fatalf("annotated size range = %#v", got[0].Conditions.WithinSizeRange)
	}
	if in[0].Actions.MoveToFolder.Path != "" || in[0].Actions.MoveToFolder.DisplayName != "" {
		t.Fatalf("input target was mutated: %#v", in[0].Actions.MoveToFolder)
	}
	*got[0].Conditions.WithinSizeRange.Minimum = 99
	if *in[0].Conditions.WithinSizeRange.Minimum != 10 {
		t.Fatalf("input size range was mutated: %#v", in[0].Conditions.WithinSizeRange)
	}
}

func TestAnnotateRuleDeepClonesNestedCollections(t *testing.T) {
	yes := true
	min := 10
	max := 20
	in := mail.Rule{
		ID:        "nested",
		Sequence:  1,
		IsEnabled: true,
		Conditions: mail.RulePredicates{
			FromAddresses:   []mail.Recipient{{Address: "sender@example.test"}},
			SubjectContains: []string{"invoice"},
			HasAttachments:  &yes,
			WithinSizeRange: &mail.RuleSizeRange{Minimum: &min, Maximum: &max},
		},
		Exceptions: mail.RulePredicates{SubjectContains: []string{"ignore"}},
		Actions: mail.RuleActions{
			CopyToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "archive-id"},
			MarkAsRead:          &yes,
			AssignCategories:    []string{"Review"},
			ForwardAsAttachment: []mail.Recipient{{Address: "archive@example.test"}},
		},
		ProviderMeta:  map[string]any{"nested": map[string]any{"values": []any{"meta"}}},
		ProviderState: map[string]any{"conditions": map[string]any{"values": []any{"state"}}},
	}
	got := AnnotateRule(in, []mail.Folder{{ID: "archive-id", DisplayName: "Archive", Path: "Archive", WellKnownName: mail.WellKnownArchive}}, "owner@example.test", nil)
	if got.Actions.CopyToFolder == nil || got.Actions.CopyToFolder.DisplayName != "Archive" ||
		got.Actions.CopyToFolder.Path != "Archive" || got.Actions.CopyToFolder.WellKnownName != mail.WellKnownArchive {
		t.Fatalf("copy target = %#v", got.Actions.CopyToFolder)
	}
	got.Conditions.FromAddresses[0].Address = "changed@example.test"
	got.Conditions.SubjectContains[0] = "changed"
	*got.Conditions.HasAttachments = false
	*got.Conditions.WithinSizeRange.Minimum = 99
	got.Exceptions.SubjectContains[0] = "changed"
	got.Actions.CopyToFolder.Path = "Changed"
	*got.Actions.MarkAsRead = false
	got.Actions.AssignCategories[0] = "Changed"
	got.Actions.ForwardAsAttachment[0].Address = "changed@example.test"
	got.ProviderMeta["nested"].(map[string]any)["values"].([]any)[0] = "changed"
	got.ProviderState["conditions"].(map[string]any)["values"].([]any)[0] = "changed"

	if in.Conditions.FromAddresses[0].Address != "sender@example.test" ||
		in.Conditions.SubjectContains[0] != "invoice" || !*in.Conditions.HasAttachments ||
		*in.Conditions.WithinSizeRange.Minimum != 10 || in.Exceptions.SubjectContains[0] != "ignore" ||
		in.Actions.CopyToFolder.Path != "" || !*in.Actions.MarkAsRead ||
		in.Actions.AssignCategories[0] != "Review" || in.Actions.ForwardAsAttachment[0].Address != "archive@example.test" ||
		in.ProviderMeta["nested"].(map[string]any)["values"].([]any)[0] != "meta" ||
		in.ProviderState["conditions"].(map[string]any)["values"].([]any)[0] != "state" {
		t.Fatalf("annotation mutated input: %#v", in)
	}
}

func TestUnsupportedFieldOrderingIsDeterministic(t *testing.T) {
	got := AnnotateRule(mail.Rule{
		ID:        "ordered",
		Sequence:  1,
		IsEnabled: true,
		UnsupportedFields: []mail.RuleUnsupportedField{
			{Area: "rule", Field: "z"},
			{Area: "actions", Field: "b"},
			{Area: "actions", Field: "a"},
		},
	}, nil, "", nil)
	var fields []string
	for _, field := range got.UnsupportedFields {
		fields = append(fields, field.Area+"."+field.Field)
	}
	want := []string{"actions.a", "actions.b", "rule.z"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
	assertFindingCodes(t, got, []string{FindingUnsupportedField, FindingUnsupportedField, FindingUnsupportedField})
}

func TestBuildCreateDefaultsValidationAndFromParsing(t *testing.T) {
	from, err := ParseFromAddresses([]string{`Client <client@example.test>`})
	if err != nil {
		t.Fatalf("ParseFromAddresses: %v", err)
	}
	req, err := BuildCreate(RuleMutationIntent{
		FromAddresses:   from,
		FromSet:         true,
		MoveToFolder:    &mail.Folder{ID: "archive-id", DisplayName: "Archive", Path: "Archive"},
		MoveToFolderSet: true,
	}, []mail.Rule{{ID: "existing", Sequence: 4}})
	if err != nil {
		t.Fatalf("BuildCreate: %v", err)
	}
	if req.Rule.Sequence != 5 || req.ChangeSet.SequenceSource != mail.RuleSequenceDefaulted ||
		!req.ChangeSet.GeneratedName || req.Rule.DisplayName != "from client@example.test -> move to Archive" ||
		req.Rule.Actions.MoveToFolder == nil || req.Rule.Actions.MoveToFolder.ProviderFolderID != "archive-id" {
		t.Fatalf("create request = %#v", req)
	}
	firstReq, err := BuildCreate(RuleMutationIntent{
		SubjectContains:    []string{"invoice"},
		SubjectContainsSet: true,
		MarkAsRead:         BoolIntent{Set: true, Value: true},
	}, nil)
	if err != nil {
		t.Fatalf("BuildCreate empty list: %v", err)
	}
	if firstReq.Rule.Sequence != 1 || firstReq.Rule.DisplayName != "subject contains invoice -> mark read" {
		t.Fatalf("first create request = %#v", firstReq)
	}
	priorityReq, err := BuildCreate(RuleMutationIntent{
		FromAddresses:        from,
		FromSet:              true,
		SenderContains:       []string{"sender"},
		SenderContainsSet:    true,
		SubjectContains:      []string{"subject"},
		SubjectContainsSet:   true,
		RecipientContains:    []string{"recipient"},
		RecipientContainsSet: true,
		HeaderContains:       []string{"header"},
		HeaderContainsSet:    true,
		HasAttachments:       BoolIntent{Set: true, Value: true},
		MoveToFolder:         &mail.Folder{ID: "archive-id", DisplayName: "Archive"},
		MoveToFolderSet:      true,
		MarkAsRead:           BoolIntent{Set: true, Value: true},
		StopProcessingRules:  BoolIntent{Set: true, Value: true},
	}, nil)
	if err != nil {
		t.Fatalf("BuildCreate priority: %v", err)
	}
	if priorityReq.Rule.DisplayName != "from client@example.test -> move to Archive" {
		t.Fatalf("priority display name = %q", priorityReq.Rule.DisplayName)
	}
	if _, err := BuildCreate(RuleMutationIntent{SubjectContains: []string{"x"}, SubjectContainsSet: true}, nil); err == nil {
		t.Fatal("create without action succeeded")
	}
	if _, err := ParseFromAddresses([]string{`A <same@example.test>`, `same@example.test`}); err == nil {
		t.Fatal("duplicate from address succeeded")
	}
	if _, err := BuildCreate(RuleMutationIntent{
		SubjectContains:    []string{"x"},
		SubjectContainsSet: true,
		MoveToFolder:       &mail.Folder{ID: "archive-id"},
		MoveToFolderSet:    true,
		Sequence:           4,
		SequenceSet:        true,
	}, []mail.Rule{{ID: "existing", Sequence: 4}}); err == nil {
		t.Fatal("duplicate sequence succeeded")
	}
}

func TestBuildCreateLargeSnapshotRemainsDeterministic(t *testing.T) {
	const ruleCount = 10000
	existing := make([]mail.Rule, ruleCount)
	for i := range existing {
		existing[i] = mail.Rule{Sequence: i + 1}
	}
	reversed := append([]mail.Rule(nil), existing...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	intent := RuleMutationIntent{
		SubjectContains:    []string{"invoice"},
		SubjectContainsSet: true,
		MarkAsRead:         BoolIntent{Set: true, Value: true},
	}

	forward, err := BuildCreate(intent, existing)
	if err != nil {
		t.Fatalf("forward snapshot: %v", err)
	}
	backward, err := BuildCreate(intent, reversed)
	if err != nil {
		t.Fatalf("reversed snapshot: %v", err)
	}
	if !reflect.DeepEqual(forward, backward) || forward.Rule.Sequence != ruleCount+1 ||
		forward.ChangeSet.SourceRuleCount == nil || *forward.ChangeSet.SourceRuleCount != ruleCount {
		t.Fatalf("large snapshot forward=%#v backward=%#v", forward, backward)
	}
}

func TestValidateRuleIntentPreflight(t *testing.T) {
	if err := ValidateCreateIntent(RuleMutationIntent{
		SubjectContains:    []string{"invoice"},
		SubjectContainsSet: true,
		MoveToFolderSet:    true,
	}); err != nil {
		t.Fatalf("valid unresolved move target rejected: %v", err)
	}
	for _, intent := range []RuleMutationIntent{
		{SubjectContains: []string{"invoice"}, SubjectContainsSet: true},
		{MoveToFolderSet: true},
		{SubjectContains: []string{""}, SubjectContainsSet: true, MoveToFolderSet: true},
		{SubjectContains: []string{"invoice"}, SubjectContainsSet: true, MoveToFolderSet: true, SequenceSet: true},
	} {
		if err := ValidateCreateIntent(intent); err == nil {
			t.Fatalf("invalid create intent succeeded: %#v", intent)
		}
	}
	if err := ValidateUpdateIntent(RuleMutationIntent{Name: "Renamed", NameSet: true}); err != nil {
		t.Fatalf("valid update intent rejected: %v", err)
	}
	for _, intent := range []RuleMutationIntent{
		{},
		{SubjectContains: []string{""}, SubjectContainsSet: true},
		{SequenceSet: true},
	} {
		if err := ValidateUpdateIntent(intent); err == nil {
			t.Fatalf("invalid update intent succeeded: %#v", intent)
		}
	}
}

func TestBuildUpdateReplacementClearsAndVerification(t *testing.T) {
	yes := true
	existing := mail.Rule{
		ID:          "rule-1",
		DisplayName: "Existing",
		Sequence:    2,
		IsEnabled:   true,
		Conditions: mail.RulePredicates{
			SubjectContains: []string{"invoice", "contract"},
			HasAttachments:  &yes,
			BodyContains:    []string{"preserved"},
		},
		Actions: mail.RuleActions{
			MoveToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "archive-id"},
			MarkAsRead:          &yes,
			StopProcessingRules: true,
			ForwardTo:           []mail.Recipient{{Address: "ops@example.test"}},
		},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{
			ConditionsHasAttachments: true, ActionsMarkAsRead: true,
			ActionsStopProcessingRules: true, ActionsStopProcessingRulesValueAvailable: true,
		},
	}
	req, err := BuildUpdate(existing, []mail.Rule{existing}, RuleMutationIntent{
		SubjectContains:     []string{"receipt"},
		SubjectContainsSet:  true,
		HasAttachments:      BoolIntent{Clear: true},
		MarkAsRead:          BoolIntent{Clear: true},
		StopProcessingRules: BoolIntent{Clear: true},
	})
	if err != nil {
		t.Fatalf("BuildUpdate: %v", err)
	}
	if req.Expected.Conditions.HasAttachments != nil ||
		req.Expected.Actions.MarkAsRead != nil ||
		req.Expected.Actions.StopProcessingRules ||
		len(req.Expected.Conditions.BodyContains) != 1 ||
		len(req.Expected.Actions.ForwardTo) != 1 ||
		req.ChangeSet.SubjectContains.Count != 1 ||
		len(req.ChangeSet.SubjectContains.Removed) != 2 {
		t.Fatalf("update request = %#v", req)
	}
	observed := req.Expected
	observed.Actions.ForwardTo = nil
	verification := VerifyRuleMutation(req.Expected, observed, req.ChangeSet)
	if verification.Status != mail.RuleVerificationFailed || len(verification.Mismatches) == 0 {
		t.Fatalf("verification = %#v", verification)
	}
	if _, err := BuildUpdate(existing, []mail.Rule{existing}, RuleMutationIntent{}); err == nil {
		t.Fatal("empty update succeeded")
	}
	if _, err := BuildUpdate(existing, []mail.Rule{existing}, RuleMutationIntent{
		SubjectContains: []string{""}, SubjectContainsSet: true,
	}); err == nil {
		t.Fatal("empty subject value succeeded")
	}
}

func TestRuleSequenceInt32Boundaries(t *testing.T) {
	base := RuleMutationIntent{SubjectContains: []string{"invoice"}, SubjectContainsSet: true, MarkAsRead: BoolIntent{Set: true, Value: true}, SequenceSet: true}
	for _, sequence := range []int64{1, math.MaxInt32} {
		intent := base
		intent.Sequence = sequence
		if _, err := BuildCreate(intent, nil); err != nil {
			t.Fatalf("sequence %d rejected: %v", sequence, err)
		}
	}
	for _, sequence := range []int64{-1, 0, math.MaxInt32 + 1} {
		intent := base
		intent.Sequence = sequence
		_, err := BuildCreate(intent, nil)
		assertInvalidRuleReason(t, err, "sequence", "out_of_range")
	}
	intent := base
	intent.SequenceSet = false
	_, err := BuildCreate(intent, []mail.Rule{{ID: "last", Sequence: math.MaxInt32}})
	assertInvalidRuleReason(t, err, "sequence", "sequence_exhausted")
	intent.SequenceSet = true
	intent.Sequence = 1
	if _, err := BuildCreate(intent, []mail.Rule{{ID: "broken", Sequence: 0}}); err != nil {
		t.Fatalf("explicit create consulted unrelated invalid sequence: %v", err)
	}
}

func TestRuleUpdateCurrentSequenceIsNoChange(t *testing.T) {
	rule := mutationDefinitionFixture()
	broken := rule
	broken.ID = "unrelated"
	broken.Sequence = 0
	_, err := BuildUpdate(rule, []mail.Rule{rule, broken}, RuleMutationIntent{Sequence: int64(rule.Sequence), SequenceSet: true})
	assertInvalidRuleReason(t, err, "changes", "no_changes")
}

func TestRuleMutationInvalidInputReasonCatalog(t *testing.T) {
	tests := []struct {
		name, field, reason string
		err                 error
	}{
		{"blank name", "name", "empty", ValidateUpdateIntent(RuleMutationIntent{NameSet: true})},
		{"empty from", "from", "empty_value", func() error { _, err := ParseFromAddresses([]string{" "}); return err }()},
		{"malformed from", "from", "malformed_address", func() error { _, err := ParseFromAddresses([]string{"<bad"}); return err }()},
		{"duplicate from", "from", "duplicate_value", func() error { _, err := ParseFromAddresses([]string{"A@example.test", "a@example.test"}); return err }()},
		{"missing conditions", "conditions", "required", ValidateCreateIntent(RuleMutationIntent{MarkAsRead: BoolIntent{Set: true, Value: true}})},
		{"missing actions", "actions", "required", ValidateCreateIntent(RuleMutationIntent{SubjectContains: []string{"x"}, SubjectContainsSet: true})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertInvalidRuleReason(t, tt.err, tt.field, tt.reason) })
	}
	rule := mutationDefinitionFixture()
	other := rule
	other.ID, other.Sequence = "other", 9
	_, err := BuildUpdate(rule, []mail.Rule{rule, other}, RuleMutationIntent{Sequence: 9, SequenceSet: true})
	assertInvalidRuleReason(t, err, "sequence", "duplicate")
	var productErr *errs.Error
	if !errors.As(err, &productErr) || productErr.Details()["rule_id"] != nil {
		t.Fatalf("duplicate sequence exposed another rule: %#v", err)
	}
}

func TestRuleUpdatePresenceOnlyClearIsEffectiveChange(t *testing.T) {
	for _, field := range []string{"has_attachments", "mark_as_read", "stop_processing_rules"} {
		t.Run(field, func(t *testing.T) {
			rule := mutationDefinitionFixture()
			intent := RuleMutationIntent{}
			switch field {
			case "has_attachments":
				no := false
				rule.Conditions.HasAttachments = &no
				rule.ProviderFieldPresence.ConditionsHasAttachments = true
				intent.HasAttachments.Clear = true
			case "mark_as_read":
				no := false
				rule.Actions.MarkAsRead = &no
				rule.ProviderFieldPresence.ActionsMarkAsRead = true
				intent.MarkAsRead.Clear = true
			case "stop_processing_rules":
				rule.Actions.StopProcessingRules = false
				rule.ProviderFieldPresence.ActionsStopProcessingRules = true
				rule.ProviderFieldPresence.ActionsStopProcessingRulesValueAvailable = true
				intent.StopProcessingRules.Clear = true
			}
			req, err := BuildUpdate(rule, []mail.Rule{rule}, intent)
			if err != nil {
				t.Fatalf("presence-only clear failed: %v", err)
			}
			change := map[string]*mail.RuleBoolFieldChange{"has_attachments": req.ChangeSet.HasAttachments, "mark_as_read": req.ChangeSet.MarkAsRead, "stop_processing_rules": req.ChangeSet.StopProcessingRules}[field]
			if change == nil || !change.Cleared || change.Before == nil || *change.Before || change.AfterPresent {
				t.Fatalf("clear change = %#v", change)
			}
			observed := req.Expected
			switch field {
			case "has_attachments":
				observed.ProviderFieldPresence.ConditionsHasAttachments = true
			case "mark_as_read":
				observed.ProviderFieldPresence.ActionsMarkAsRead = true
			default:
				observed.ProviderFieldPresence.ActionsStopProcessingRules = true
			}
			if VerifyRuleMutation(req.Expected, observed, req.ChangeSet).Status != mail.RuleVerificationFailed {
				t.Fatal("retained cleared key verified")
			}
		})
	}
	rule := mutationDefinitionFixture()
	_, err := BuildUpdate(rule, []mail.Rule{rule}, RuleMutationIntent{HasAttachments: BoolIntent{Clear: true}})
	assertInvalidRuleReason(t, err, "changes", "no_changes")

	nullRule := mutationDefinitionFixture()
	nullRule.Actions.MarkAsRead = nil
	nullRule.ProviderFieldPresence.ActionsMarkAsRead = true
	req, err := BuildUpdate(nullRule, []mail.Rule{nullRule}, RuleMutationIntent{MarkAsRead: BoolIntent{Clear: true}})
	if err != nil || req.ChangeSet.MarkAsRead == nil || req.ChangeSet.MarkAsRead.Before != nil || !req.ChangeSet.MarkAsRead.BeforePresent {
		t.Fatalf("accepted-null clear = %#v err=%v", req.ChangeSet.MarkAsRead, err)
	}

	for _, field := range []string{"has_attachments", "mark_as_read", "stop_processing_rules"} {
		t.Run(field+" set repairs absent presence", func(t *testing.T) {
			rule := mutationDefinitionFixture()
			intent := RuleMutationIntent{}
			switch field {
			case "has_attachments":
				rule.Conditions.HasAttachments = boolPtr(true)
				intent.HasAttachments = BoolIntent{Set: true, Value: true}
			case "mark_as_read":
				rule.ProviderFieldPresence.ActionsMarkAsRead = false
				intent.MarkAsRead = BoolIntent{Set: true, Value: true}
			default:
				rule.Actions.StopProcessingRules = true
				intent.StopProcessingRules = BoolIntent{Set: true, Value: true}
			}
			req, err := BuildUpdate(rule, []mail.Rule{rule}, intent)
			if err != nil {
				t.Fatal(err)
			}
			change := map[string]*mail.RuleBoolFieldChange{"has_attachments": req.ChangeSet.HasAttachments, "mark_as_read": req.ChangeSet.MarkAsRead, "stop_processing_rules": req.ChangeSet.StopProcessingRules}[field]
			if change == nil || change.BeforePresent || !change.AfterPresent || change.After == nil || !*change.After {
				t.Fatalf("presence repair change = %#v", change)
			}
		})

		t.Run(field+" matching presence is unchanged", func(t *testing.T) {
			rule := mutationDefinitionFixture()
			intent := RuleMutationIntent{}
			switch field {
			case "has_attachments":
				rule.Conditions.HasAttachments = boolPtr(true)
				rule.ProviderFieldPresence.ConditionsHasAttachments = true
				intent.HasAttachments = BoolIntent{Set: true, Value: true}
			case "mark_as_read":
				rule.ProviderFieldPresence.ActionsMarkAsRead = true
				intent.MarkAsRead = BoolIntent{Set: true, Value: true}
			default:
				rule.Actions.StopProcessingRules = true
				rule.ProviderFieldPresence.ActionsStopProcessingRules = true
				rule.ProviderFieldPresence.ActionsStopProcessingRulesValueAvailable = true
				intent.StopProcessingRules = BoolIntent{Set: true, Value: true}
			}
			_, err := BuildUpdate(rule, []mail.Rule{rule}, intent)
			assertInvalidRuleReason(t, err, "changes", "no_changes")
		})
	}
}

func TestRuleUpdateRejectsEmptyResultingObjects(t *testing.T) {
	no := false
	conditionOnly := mutationDefinitionFixture()
	conditionOnly.Conditions = mail.RulePredicates{HasAttachments: &no}
	conditionOnly.ProviderFieldPresence.ConditionsHasAttachments = true
	_, err := BuildUpdate(conditionOnly, []mail.Rule{conditionOnly}, RuleMutationIntent{HasAttachments: BoolIntent{Clear: true}})
	assertInvalidRuleReason(t, err, "conditions", "empty_result")

	actionOnly := mutationDefinitionFixture()
	actionOnly.Actions = mail.RuleActions{MarkAsRead: &no}
	actionOnly.ProviderFieldPresence.ActionsMarkAsRead = true
	_, err = BuildUpdate(actionOnly, []mail.Rule{actionOnly}, RuleMutationIntent{MarkAsRead: BoolIntent{Clear: true}})
	assertInvalidRuleReason(t, err, "actions", "empty_result")
}

func TestRuleMutationPlanningDoesNotMutateSnapshot(t *testing.T) {
	rule := mutationDefinitionFixture()
	rule.ProviderState = map[string]any{"actions": map[string]any{"nested": []any{"kept"}}}
	rule.ProviderMeta = map[string]any{"nested": map[string]any{"value": "kept"}}
	rule.HealthFindings = []mail.RuleHealthFinding{{Code: "fixture", Details: map[string]any{"nested": []any{"kept"}}}}
	req, err := BuildUpdate(rule, []mail.Rule{rule}, RuleMutationIntent{Name: "Renamed", NameSet: true})
	if err != nil {
		t.Fatal(err)
	}
	req.Existing.ProviderState["actions"].(map[string]any)["nested"].([]any)[0] = "changed"
	req.Existing.ProviderMeta["nested"].(map[string]any)["value"] = "changed"
	req.Expected.HealthFindings[0].Details["nested"].([]any)[0] = "changed"
	req.Expected.Conditions.SubjectContains[0] = "changed"
	if rule.ProviderState["actions"].(map[string]any)["nested"].([]any)[0] != "kept" ||
		rule.ProviderMeta["nested"].(map[string]any)["value"] != "kept" ||
		rule.HealthFindings[0].Details["nested"].([]any)[0] != "kept" ||
		rule.Conditions.SubjectContains[0] != "invoice" {
		t.Fatalf("snapshot mutated: %#v", rule)
	}
}

func TestBuildCreateInitializesProviderFieldPresence(t *testing.T) {
	req, err := BuildCreate(RuleMutationIntent{
		HasAttachments:      BoolIntent{Set: true, Value: true},
		MarkAsRead:          BoolIntent{Set: true, Value: true},
		StopProcessingRules: BoolIntent{Set: true, Value: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := req.Rule.ProviderFieldPresence
	if !p.ConditionsHasAttachments || !p.ActionsMarkAsRead || !p.ActionsStopProcessingRules || !p.ActionsStopProcessingRulesValueAvailable {
		t.Fatalf("presence = %#v", p)
	}
	if req.ChangeSet.SourceRuleCount == nil || *req.ChangeSet.SourceRuleCount != 0 {
		t.Fatalf("source count = %#v", req.ChangeSet.SourceRuleCount)
	}
}

func TestVerifyRuleCreateCompleteDefinitionState(t *testing.T) {
	wantConditions := []string{
		"conditions.body_contains", "conditions.body_or_subject_contains", "conditions.categories", "conditions.from_addresses",
		"conditions.has_attachments", "conditions.header_contains", "conditions.importance", "conditions.is_approval_request",
		"conditions.is_automatic_forward", "conditions.is_automatic_reply", "conditions.is_encrypted", "conditions.is_meeting_request",
		"conditions.is_meeting_response", "conditions.is_non_delivery_report", "conditions.is_permission_controlled", "conditions.is_read_receipt",
		"conditions.is_signed", "conditions.is_voicemail", "conditions.message_action_flag", "conditions.not_sent_to_me",
		"conditions.recipient_contains", "conditions.sender_contains", "conditions.sensitivity", "conditions.sent_cc_me",
		"conditions.sent_only_to_me", "conditions.sent_to_addresses", "conditions.sent_to_me", "conditions.sent_to_or_cc_me",
		"conditions.subject_contains", "conditions.within_size_range",
	}
	wantActions := []string{
		"actions.assign_categories", "actions.copy_to_folder", "actions.delete", "actions.forward_as_attachment_to",
		"actions.forward_to", "actions.mark_as_read", "actions.mark_importance", "actions.move_to_folder",
		"actions.permanent_delete", "actions.redirect_to", "actions.stop_processing_rules",
	}
	if !reflect.DeepEqual(ruleConditionFieldCatalog, wantConditions) || !reflect.DeepEqual(ruleActionFieldCatalog, wantActions) {
		t.Fatalf("observable catalog conditions=%#v actions=%#v", ruleConditionFieldCatalog, ruleActionFieldCatalog)
	}

	expected := completeMutationDefinitionFixture()
	expected.ID = "created"
	change := mail.RuleChangeSet{Operation: mail.RuleOperationCreate, SubjectContains: &mail.RuleStringCollectionChange{Set: true, After: []string{"invoice"}}}
	wantChecked := append([]string{"rule_id", "display_name", "sequence", "is_enabled"}, wantConditions...)
	wantChecked = append(wantChecked, wantActions...)
	wantChecked = append(wantChecked, "exceptions", "unsupported_fields", "presence.actions.mark_as_read", "presence.actions.stop_processing_rules", "presence.conditions.has_attachments")
	sort.Strings(wantChecked)
	if got := VerifyRuleMutation(expected, expected, change); got.Status != mail.RuleVerificationVerified || !reflect.DeepEqual(got.CheckedFields, wantChecked) {
		t.Fatalf("complete verification = %#v", got)
	}

	minimal := mutationDefinitionFixture()
	minimal.ID = "created"
	observed := minimal
	observed.Actions.Delete = true
	if VerifyRuleMutation(minimal, observed, change).Status != mail.RuleVerificationFailed {
		t.Fatal("unexpected action addition verified")
	}
}

func TestVerifyRuleUpdatePreservesCompleteDefinitionState(t *testing.T) {
	expected := mutationDefinitionFixture()
	expected.Exceptions.SubjectContains = []string{"ignore"}
	expected.UnsupportedFields = []mail.RuleUnsupportedField{{Area: "actions", Field: "future", ValueKind: "array", Count: 1, Summary: "one"}}
	change := mail.RuleChangeSet{Operation: mail.RuleOperationUpdate, DisplayName: &mail.RuleStringFieldChange{Set: true, Before: "Rule", After: "Renamed"}}
	observed := expected
	observed.Exceptions.SubjectContains = nil
	observed.UnsupportedFields = []mail.RuleUnsupportedField{{Area: "actions", Field: "future", ValueKind: "object", Count: 1, Summary: "different presentation"}}
	got := VerifyRuleMutation(expected, observed, change)
	if got.Status != mail.RuleVerificationFailed || len(got.Mismatches) != 2 || got.Mismatches[0].Field != "exceptions" || got.Mismatches[1].Field != "unsupported_fields" {
		t.Fatalf("verification = %#v", got)
	}
}

func TestVerifyRuleScalarMutationPreservesCompleteDefinitionState(t *testing.T) {
	expected := mutationDefinitionFixture()
	change := mail.RuleChangeSet{Operation: mail.RuleOperationDisable, IsEnabled: &mail.RuleBoolFieldChange{Set: true, Before: boolPtr(true), After: boolPtr(false)}}
	expected.IsEnabled = false
	observed := expected
	observed.Conditions.SubjectContains = []string{"drift"}
	got := VerifyRuleMutation(expected, observed, change)
	if got.Status != mail.RuleVerificationFailed || len(got.Mismatches) != 1 || got.Mismatches[0].Field != "conditions.subject_contains" {
		t.Fatalf("scalar verification = %#v", got)
	}
}

func TestRuleMutationDescriptorsAndGeneratedNames(t *testing.T) {
	from, _ := ParseFromAddresses([]string{"Sender <sender@example.test>"})
	conditions := []struct {
		intent RuleMutationIntent
		want   string
	}{
		{RuleMutationIntent{FromAddresses: from, FromSet: true}, "from sender@example.test"},
		{RuleMutationIntent{SenderContains: []string{"sender"}, SenderContainsSet: true}, "sender contains sender"},
		{RuleMutationIntent{SubjectContains: []string{"subject"}, SubjectContainsSet: true}, "subject contains subject"},
		{RuleMutationIntent{RecipientContains: []string{"recipient"}, RecipientContainsSet: true}, "recipient contains recipient"},
		{RuleMutationIntent{HeaderContains: []string{"header"}, HeaderContainsSet: true}, "header contains header"},
		{RuleMutationIntent{HasAttachments: BoolIntent{Set: true, Value: true}}, "messages with attachments"},
	}
	for _, condition := range conditions {
		intent := condition.intent
		intent.MarkAsRead = BoolIntent{Set: true, Value: true}
		req, err := BuildCreate(intent, nil)
		if err != nil || req.Rule.DisplayName != condition.want+" -> mark read" {
			t.Fatalf("condition %#v generated %q err=%v", condition.intent, req.Rule.DisplayName, err)
		}
	}
	for _, action := range []struct {
		folder     *mail.Folder
		mark, stop bool
		want       string
	}{
		{folder: &mail.Folder{ID: "id", DisplayName: "Display", Path: "Path"}, want: "move to Display"},
		{folder: &mail.Folder{ID: "id", Path: "Path"}, want: "move to Path"},
		{folder: &mail.Folder{ID: "id"}, want: "move to id"},
		{mark: true, want: "mark read"},
		{stop: true, want: "stop processing"},
	} {
		intent := RuleMutationIntent{SubjectContains: []string{"x"}, SubjectContainsSet: true}
		if action.folder != nil {
			intent.MoveToFolder, intent.MoveToFolderSet = action.folder, true
		}
		if action.mark {
			intent.MarkAsRead = BoolIntent{Set: true, Value: true}
		}
		if action.stop {
			intent.StopProcessingRules = BoolIntent{Set: true, Value: true}
		}
		req, err := BuildCreate(intent, nil)
		if err != nil || req.Rule.DisplayName != "subject contains x -> "+action.want {
			t.Fatalf("action %#v generated %q err=%v", action, req.Rule.DisplayName, err)
		}
	}

	priorityIntent := RuleMutationIntent{
		FromAddresses:        from,
		FromSet:              true,
		SenderContains:       []string{"lower"},
		SenderContainsSet:    true,
		SubjectContains:      []string{"lower"},
		SubjectContainsSet:   true,
		RecipientContains:    []string{"lower"},
		RecipientContainsSet: true,
		HeaderContains:       []string{"lower"},
		HeaderContainsSet:    true,
		HasAttachments:       BoolIntent{Set: true, Value: true},
		MoveToFolder:         &mail.Folder{ID: "archive-id", DisplayName: "Archive"},
		MoveToFolderSet:      true,
		MarkAsRead:           BoolIntent{Set: true, Value: true},
		StopProcessingRules:  BoolIntent{Set: true, Value: true},
	}
	priority, err := BuildCreate(priorityIntent, nil)
	if err != nil || priority.Rule.DisplayName != "from sender@example.test -> move to Archive" {
		t.Fatalf("priority generated %q err=%v", priority.Rule.DisplayName, err)
	}
	if got := defaultRuleName(mail.Rule{}); got != "messages -> apply rule" {
		t.Fatalf("fallback generated name = %q", got)
	}
}

func completeMutationDefinitionFixture() mail.Rule {
	yes, no := true, false
	minimum, maximum := 10, 20
	rule := mail.Rule{
		ID: "rule-1", DisplayName: "Complete rule", Sequence: 10, IsEnabled: true,
		Conditions: mail.RulePredicates{
			Semantics:              mail.DefaultRulePredicateSemantics(),
			FromAddresses:          []mail.Recipient{{Name: "Sender", Address: "sender@example.test"}},
			SenderContains:         []string{"sender"},
			RecipientContains:      []string{"recipient"},
			SubjectContains:        []string{"subject"},
			HeaderContains:         []string{"header"},
			HasAttachments:         &yes,
			BodyContains:           []string{"body"},
			BodyOrSubjectContains:  []string{"body-or-subject"},
			Categories:             []string{"category"},
			Importance:             "high",
			Sensitivity:            "private",
			MessageActionFlag:      "any",
			WithinSizeRange:        &mail.RuleSizeRange{Minimum: &minimum, Maximum: &maximum},
			SentToAddresses:        []mail.Recipient{{Address: "recipient@example.test"}},
			IsApprovalRequest:      &no,
			IsAutomaticForward:     &yes,
			IsAutomaticReply:       &no,
			IsEncrypted:            &yes,
			IsMeetingRequest:       &yes,
			IsMeetingResponse:      &no,
			IsNonDeliveryReport:    &yes,
			IsPermissionControlled: &no,
			IsReadReceipt:          &yes,
			IsSigned:               &no,
			IsVoicemail:            &yes,
			NotSentToMe:            &no,
			SentCCMe:               &yes,
			SentOnlyToMe:           &no,
			SentToMe:               &yes,
			SentToOrCCMe:           &no,
		},
		Exceptions: mail.RulePredicates{SubjectContains: []string{"except"}},
		Actions: mail.RuleActions{
			MoveToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "move"},
			CopyToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "copy"},
			MarkAsRead:          &yes,
			StopProcessingRules: true,
			AssignCategories:    []string{"category"},
			Delete:              true,
			PermanentDelete:     true,
			MarkImportance:      "high",
			ForwardTo:           []mail.Recipient{{Address: "forward@example.test"}},
			ForwardAsAttachment: []mail.Recipient{{Address: "attachment@example.test"}},
			RedirectTo:          []mail.Recipient{{Address: "redirect@example.test"}},
		},
		UnsupportedFields: []mail.RuleUnsupportedField{{Area: "actions", Field: "future", ValueKind: "array", Count: 1, Summary: "safe"}},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{
			ConditionsHasAttachments: true, ActionsMarkAsRead: true,
			ActionsStopProcessingRules: true, ActionsStopProcessingRulesValueAvailable: true,
		},
	}
	return rule
}

func mutationDefinitionFixture() mail.Rule {
	yes := true
	return mail.Rule{
		ID: "rule-1", DisplayName: "Rule", Sequence: 10, IsEnabled: true,
		Conditions:            mail.RulePredicates{SubjectContains: []string{"invoice"}},
		Actions:               mail.RuleActions{MoveToFolder: &mail.RuleFolderTarget{ProviderFolderID: "archive-id"}, MarkAsRead: &yes},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{ActionsMarkAsRead: true},
	}
}

func assertInvalidRuleReason(t *testing.T, err error, field, reason string) {
	t.Helper()
	var productErr *errs.Error
	if !errors.As(err, &productErr) {
		t.Fatalf("error = %#v, want %s/%s", err, field, reason)
	}
	details := productErr.Details()
	if productErr.Code() != errs.CodeInvalidRuleInput || details["field"] != field || details["reason"] != reason {
		t.Fatalf("error = %#v, want %s/%s", err, field, reason)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertFindingCodes(t *testing.T, rule mail.Rule, want []string) {
	t.Helper()
	got := make([]string, 0, len(rule.HealthFindings))
	for _, finding := range rule.HealthFindings {
		got = append(got, finding.Code)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finding codes for %s = %#v, want %#v; findings=%#v", rule.ID, got, want, rule.HealthFindings)
	}
}
