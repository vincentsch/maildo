package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	rulespkg "github.com/vincentsch/maildo/internal/rules"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

type ruleListResult struct {
	provider.AccountSelection
	Rules []mail.Rule `json:"rules"`
	Count int         `json:"count"`
}

type ruleViewResult struct {
	provider.AccountSelection
	Account         string    `json:"account"`
	Provider        string    `json:"provider"`
	RequestedRuleID string    `json:"requested_rule_id"`
	Rule            mail.Rule `json:"rule"`
}

type ruleHealthResult struct {
	provider.AccountSelection
	Rules         []mail.Rule    `json:"rules"`
	Count         int            `json:"count"`
	FindingCounts map[string]int `json:"finding_counts"`
}

// ruleMutationResult is the result of a rule change: the rule as maildo read
// it back after the write, and how the check of that read went.
type ruleMutationResult struct {
	provider.AccountSelection
	Operation            string                            `json:"operation"`
	Status               mail.RuleMutationResultStatus     `json:"status"`
	Applied              bool                              `json:"applied"`
	Changed              bool                              `json:"changed"`
	Account              string                            `json:"account"`
	Provider             string                            `json:"provider"`
	RuleScope            string                            `json:"rule_scope"`
	RuleID               string                            `json:"rule_id,omitempty"`
	DisplayName          string                            `json:"display_name,omitempty"`
	GeneratedName        *bool                             `json:"generated_name,omitempty"`
	Sequence             *int                              `json:"sequence,omitempty"`
	SequenceSource       mail.RuleSequenceSource           `json:"sequence_source,omitempty"`
	SourceRuleCount      *int                              `json:"source_rule_count,omitempty"`
	ChangeSet            *mail.RuleChangeSet               `json:"change_set,omitempty"`
	ResolvedMoveTarget   *mail.FolderSummary               `json:"resolved_move_target,omitempty"`
	PreservedFields      []string                          `json:"preserved_fields"`
	ConfirmationRequired bool                              `json:"confirmation_required,omitempty"`
	ConfirmationMatched  bool                              `json:"confirmation_matched,omitempty"`
	Verification         *mail.RuleVerification            `json:"verification,omitempty"`
	ProviderMetadata     mail.RuleMutationProviderMetadata `json:"provider_metadata"`
	ProviderStatus       *mail.RuleMutationProviderStatus  `json:"provider_status,omitempty"`
	// ActionLogStatus is set when the command wrote to the provider.
	ActionLogStatus *mail.ActionLogStatus `json:"action_log_status,omitempty"`
}

// ruleMutationResultFromObservation builds the result from the rule as maildo
// read it back after the write, the same read that passed the check, and not
// from the server's answer to the write.
func ruleMutationResultFromObservation(sel provider.AccountSelection, observed mail.Rule, change mail.RuleChangeSet, metadata mail.RuleMutationProviderMetadata, preserved []string, verification *mail.RuleVerification, status mail.RuleMutationResultStatus) ruleMutationResult {
	sequence := observed.Sequence
	out := ruleMutationResult{
		AccountSelection:   sel,
		Operation:          string(change.Operation),
		Status:             status,
		Applied:            status == mail.RuleMutationStatusApplied,
		Changed:            status == mail.RuleMutationStatusApplied,
		Account:            observed.Account,
		Provider:           observed.Provider,
		RuleScope:          string(observed.Scope),
		RuleID:             observed.ID,
		DisplayName:        observed.DisplayName,
		Sequence:           &sequence,
		SequenceSource:     change.SequenceSource,
		SourceRuleCount:    change.SourceRuleCount,
		ChangeSet:          &change,
		ResolvedMoveTarget: ruleMoveSummary(observed.Actions.MoveToFolder),
		PreservedFields:    append([]string(nil), preserved...),
		Verification:       verification,
		ProviderMetadata:   metadata,
	}
	if out.PreservedFields == nil {
		out.PreservedFields = []string{}
	}
	if change.Operation == mail.RuleOperationCreate {
		generated := change.GeneratedName
		out.GeneratedName = &generated
	}
	statusValue, _ := mail.NewRuleMutationProviderStatus(observed.HasError, observed.IsReadOnly)
	if observed.IsReadOnly && status == mail.RuleMutationStatusApplied {
		statusValue, _ = mail.NewRuleMutationProviderStatus(observed.HasError, observed.IsReadOnly, mail.RuleMutationWarningProviderMarkedReadOnly)
	}
	out.ProviderStatus = &statusValue
	return out
}

// ruleDeleteResultFromObservation builds the result of a delete. It shows the
// rule as it was, since after the delete there is nothing to read back.
func ruleDeleteResultFromObservation(sel provider.AccountSelection, deleted mail.Rule, metadata mail.RuleMutationProviderMetadata, required, matched bool, verification mail.RuleVerification) ruleMutationResult {
	out := ruleMutationResult{
		AccountSelection:     sel,
		Operation:            string(mail.RuleOperationDelete),
		Status:               mail.RuleMutationStatusApplied,
		Applied:              true,
		Changed:              true,
		Account:              deleted.Account,
		Provider:             deleted.Provider,
		RuleScope:            string(deleted.Scope),
		RuleID:               deleted.ID,
		DisplayName:          deleted.DisplayName,
		PreservedFields:      []string{},
		ConfirmationRequired: required,
		ConfirmationMatched:  matched,
		Verification:         &verification,
		ProviderMetadata:     metadata,
	}
	return out
}

// humanRuleList prints one row per rule, in the order the rules run. The
// ID column is what rules view and the other rule commands take.
func humanRuleList(r ruleListResult) func(io.Writer) {
	return func(w io.Writer) {
		if len(r.Rules) == 0 {
			fmt.Fprintln(w, "No rules.")
			return
		}
		fmt.Fprintf(w, "%s%s\n\n", upperFirst(pluralize(len(r.Rules), "rule", "rules")), accountSuffix(r.AccountSelection))
		many := len(r.Accounts) > 1
		header := []string{"ORDER", "NAME", "STATE", "ACTIONS", "HEALTH", "ID"}
		if many {
			header = append([]string{"ACCOUNT"}, header...)
		}
		t := newTable(header...)
		for _, rule := range r.Rules {
			row := []string{
				strconv.Itoa(rule.Sequence),
				firstNonEmpty(rule.DisplayName, "(no name)"),
				ruleStateText(rule),
				ruleActionSummary(rule.Actions),
				ruleHealthText(rule.HealthStatus),
				rule.ID,
			}
			if many {
				row = append([]string{rule.Account}, row...)
			}
			t.add(row...)
		}
		t.render(w, 0, nil)
	}
}

// humanRuleMutation says in a sentence what happened to the rule, with the
// changed fields for an update.
func humanRuleMutation(r ruleMutationResult) func(io.Writer) {
	return func(w io.Writer) {
		name := fmt.Sprintf("the rule %s in %s", quote(plainField(r.DisplayName)), plainField(r.Account))
		switch r.Operation {
		case string(mail.RuleOperationCreate):
			fmt.Fprintf(w, "Created %s. Its id is %s.\n", name, plainField(r.RuleID))
		case string(mail.RuleOperationUpdate):
			lines := []string{}
			if r.ChangeSet != nil {
				lines = ruleChangeLines(*r.ChangeSet)
			}
			if len(lines) == 0 {
				fmt.Fprintf(w, "Changed %s.\n", name)
				break
			}
			fmt.Fprintf(w, "Changed %s:\n\n", name)
			for _, line := range lines {
				fmt.Fprintln(w, line)
			}
		case string(mail.RuleOperationEnable), string(mail.RuleOperationDisable):
			state := "on"
			if r.Operation == string(mail.RuleOperationDisable) {
				state = "off"
			}
			if r.Status == mail.RuleMutationStatusUnchanged {
				fmt.Fprintf(w, "%s was already %s.\n", upperFirst(name), state)
			} else {
				fmt.Fprintf(w, "Turned %s %s.\n", state, name)
			}
		case string(mail.RuleOperationDelete):
			fmt.Fprintf(w, "Deleted %s.\n", name)
		default:
			fmt.Fprintf(w, "Changed %s.\n", name)
		}
		if r.ProviderStatus == nil {
			return
		}
		switch {
		case len(r.ProviderStatus.Warnings) > 0:
			fmt.Fprintln(w, "Warning: the mail service marked this rule read-only after the change.")
		case r.ProviderStatus.HasError:
			fmt.Fprintln(w, "Warning: the mail service reports an error for this rule, so it may not run.")
		}
	}
}

// writeRulePreview shows a rule change that was not applied, with the
// command that applies it. The JSON is the flat dry-run field list.
func writeRulePreview(f *rungrad.Factory, cmd *cobra.Command, args []string, acct provider.ResolvedAccount, rule mail.Rule, change mail.RuleChangeSet, applyRequired, confirmationRequired bool) error {
	set := [][2]string{{"account", acct.Name}}
	if applyRequired {
		set = append(set, [2]string{"apply", ""})
	}
	if confirmationRequired {
		set = append(set, [2]string{"confirm", rule.ID})
	}
	command := changeCommand(cmd, args, set)
	preview := previewRuleMutation(acct, rule, change, applyRequired, confirmationRequired, false)
	preview.Body = append(preview.Body, output.Field{Name: "command", Value: command})
	return writeReadablePreview(f, preview, humanRulePreview(acct, rule, change, command))
}

// humanRulePreview says what a rule change would do.
func humanRulePreview(acct provider.ResolvedAccount, rule mail.Rule, change mail.RuleChangeSet, command string) func(io.Writer) {
	return func(w io.Writer) {
		name := fmt.Sprintf("the rule %s in %s", quote(plainField(rule.DisplayName)), plainField(acct.Name))
		verb := "do it"
		switch change.Operation {
		case mail.RuleOperationCreate:
			verb = "create it"
			fmt.Fprintf(w, "Would create %s:\n\n", name)
			field := func(label, value string) {
				if value != "" {
					fmt.Fprintf(w, "  %-8s %s\n", label+":", value)
				}
			}
			field("When", ruleWhenText(rule.Conditions))
			field("Unless", ruleWhenText(rule.Exceptions))
			field("Then", ruleThenText(rule.Actions))
			if !rule.IsEnabled {
				field("State", "off")
			}
			fmt.Fprintln(w)
		case mail.RuleOperationUpdate:
			verb = "change it"
			lines := ruleChangeLines(change)
			if len(lines) == 0 {
				fmt.Fprintf(w, "%s already looks like this, so nothing would change.\n", upperFirst(name))
				return
			}
			fmt.Fprintf(w, "Would change %s:\n\n", name)
			for _, line := range lines {
				fmt.Fprintln(w, line)
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "The rest of the rule would stay the same.")
		case mail.RuleOperationEnable, mail.RuleOperationDisable:
			state := "on"
			if change.Operation == mail.RuleOperationDisable {
				state = "off"
			}
			if change.IsEnabled == nil {
				fmt.Fprintf(w, "%s is already %s, so nothing would change.\n", upperFirst(name), state)
				return
			}
			verb = "turn it " + state
			fmt.Fprintf(w, "Would turn %s %s.\n\n", state, name)
		case mail.RuleOperationDelete:
			verb = "delete it"
			fmt.Fprintf(w, "Would delete %s.\n\n", name)
		}
		fmt.Fprintf(w, "Nothing was changed. To %s, run:\n", verb)
		writeCommand(w, command, commandWithoutDry)
	}
}

// ruleChangeLines lists the fields an update changes, one per line, with
// the old value.
func ruleChangeLines(c mail.RuleChangeSet) []string {
	var out []string
	add := func(label, after, before string) {
		if after == before {
			return
		}
		out = append(out, fmt.Sprintf("  %-20s %s (was %s)", label+":", after, before))
	}
	if c.DisplayName != nil {
		add("Name", quotedOrNone([]string{c.DisplayName.After}), quotedOrNone([]string{c.DisplayName.Before}))
	}
	if c.Sequence != nil {
		add("Order", strconv.Itoa(c.Sequence.After), strconv.Itoa(c.Sequence.Before))
	}
	if c.IsEnabled != nil {
		add("On", ruleBoolText(c.IsEnabled.After), ruleBoolText(c.IsEnabled.Before))
	}
	if c.FromAddresses != nil {
		add("From", recipientsOrNone(c.FromAddresses.After), recipientsOrNone(c.FromAddresses.Before))
	}
	for _, s := range []struct {
		label  string
		change *mail.RuleStringCollectionChange
	}{
		{"Sender contains", c.SenderContains},
		{"Recipient contains", c.RecipientContains},
		{"Subject contains", c.SubjectContains},
		{"Header contains", c.HeaderContains},
	} {
		if s.change != nil {
			add(s.label, quotedOrNone(s.change.After), quotedOrNone(s.change.Before))
		}
	}
	if c.HasAttachments != nil {
		add("Has attachments", ruleBoolText(c.HasAttachments.After), ruleBoolText(c.HasAttachments.Before))
	}
	if c.MoveToFolder != nil {
		add("Move to", ruleFolderText(c.MoveToFolder.After), ruleFolderText(c.MoveToFolder.Before))
	}
	if c.MarkAsRead != nil {
		add("Mark as read", ruleBoolText(c.MarkAsRead.After), ruleBoolText(c.MarkAsRead.Before))
	}
	if c.StopProcessingRules != nil {
		add("Stop other rules", ruleBoolText(c.StopProcessingRules.After), ruleBoolText(c.StopProcessingRules.Before))
	}
	return out
}

// ruleWhenText describes the conditions maildo can set, joined with "and".
func ruleWhenText(p mail.RulePredicates) string {
	var parts []string
	if len(p.FromAddresses) > 0 {
		parts = append(parts, "from "+recipientsOrNone(p.FromAddresses))
	}
	for _, s := range []struct {
		label  string
		values []string
	}{
		{"sender contains", p.SenderContains},
		{"recipient contains", p.RecipientContains},
		{"subject contains", p.SubjectContains},
		{"a header contains", p.HeaderContains},
	} {
		if len(s.values) > 0 {
			parts = append(parts, s.label+" "+quotedOrNone(s.values))
		}
	}
	if p.HasAttachments != nil {
		if *p.HasAttachments {
			parts = append(parts, "has attachments")
		} else {
			parts = append(parts, "has no attachments")
		}
	}
	return strings.Join(parts, " and ")
}

// ruleThenText describes a rule's actions.
func ruleThenText(a mail.RuleActions) string {
	var parts []string
	if a.MoveToFolder != nil {
		parts = append(parts, "move to "+ruleFolderText(a.MoveToFolder))
	}
	if a.CopyToFolder != nil {
		parts = append(parts, "copy to "+ruleFolderText(a.CopyToFolder))
	}
	if a.MarkAsRead != nil && *a.MarkAsRead {
		parts = append(parts, "mark as read")
	}
	if a.Delete || a.PermanentDelete {
		parts = append(parts, "delete")
	}
	if len(a.ForwardTo)+len(a.ForwardAsAttachment)+len(a.RedirectTo) > 0 {
		parts = append(parts, "forward")
	}
	if a.StopProcessingRules {
		parts = append(parts, "stop other rules")
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", then ")
}

func quotedOrNone(values []string) string {
	var quoted []string
	for _, v := range values {
		if v != "" {
			quoted = append(quoted, quote(plainField(v)))
		}
	}
	if len(quoted) == 0 {
		return "(none)"
	}
	return strings.Join(quoted, " or ")
}

func recipientsOrNone(rs []mail.Recipient) string {
	if len(rs) == 0 {
		return "(none)"
	}
	return recipientList(rs)
}

func ruleBoolText(v *bool) string {
	if v == nil {
		return "(not set)"
	}
	return yesNo(*v)
}

func ruleFolderText(target *mail.RuleFolderTarget) string {
	if target == nil {
		return "(none)"
	}
	text := quote(plainField(folderTargetShort(*target)))
	if target.Missing {
		text += " (missing)"
	}
	return text
}

func ruleMutationMeta(commandPath string, r ruleMutationResult) output.Meta {
	extra := map[string]any{
		"command_path": commandPath,
		"scope":        r.Scope,
		"account":      r.Account,
		"provider":     r.Provider,
		"operation":    r.Operation,
		"status":       r.Status,
		"applied":      r.Applied,
		"changed":      r.Changed,
		"rule_scope":   r.RuleScope,
	}
	if r.RuleID != "" {
		extra["rule_id"] = r.RuleID
	}
	if r.Sequence != nil {
		extra["sequence"] = *r.Sequence
	}
	if r.SequenceSource != "" {
		extra["sequence_source"] = r.SequenceSource
	}
	if r.Verification != nil && r.Verification.Status != "" {
		extra["verification_status"] = r.Verification.Status
	}
	if r.ConfirmationMatched {
		extra["confirmation"] = "matched"
	}
	return output.Meta{Extra: extra}
}

// previewRuleMutation builds the JSON of a rule preview as a flat list of
// fields in a fixed order. Values stay exact here; text for people is made
// safe for the terminal later.
func previewRuleMutation(acct provider.ResolvedAccount, rule mail.Rule, change mail.RuleChangeSet, applyRequired, confirmationRequired, confirmationMatched bool) output.DryRunPreview {
	changed := changeSetChanged(change)
	fields := []output.Field{
		{Name: "account", Value: acct.Name},
		{Name: "applied", Value: "false"},
	}
	if applyRequired {
		fields = append(fields, output.Field{Name: "apply_required", Value: "true"})
	}
	fields = append(fields, output.Field{Name: "changed", Value: strconv.FormatBool(changed)})
	if confirmationRequired {
		fields = append(fields, output.Field{Name: "confirmation_required", Value: "true"})
	}
	if rule.DisplayName != "" {
		fields = append(fields, output.Field{Name: "display_name", Value: rule.DisplayName})
	}
	if change.Operation == mail.RuleOperationCreate {
		fields = append(fields, output.Field{Name: "generated_name", Value: strconv.FormatBool(change.GeneratedName)})
	}
	if rule.ID != "" {
		fields = append(fields, output.Field{Name: "rule_id", Value: rule.ID})
	}
	fields = append(fields,
		output.Field{Name: "operation", Value: string(change.Operation)},
	)
	if change.Operation == mail.RuleOperationCreate || change.Operation == mail.RuleOperationUpdate {
		semantics := ruleMutationPredicateSemantics()
		fields = append(fields,
			output.Field{Name: "predicate_families", Value: semantics.Families},
			output.Field{Name: "predicate_repeated_values", Value: semantics.RepeatedValues},
		)
	}
	preserved := rulespkg.PreservedFields(rule, change)
	fields = appendPreservedPreviewFields(fields, preserved)
	fields = append(fields,
		output.Field{Name: "provider", Value: acct.Provider},
	)
	fields = append(fields, output.Field{Name: "rule_scope", Value: string(mail.RuleScopeInbox)})
	if change.Operation != mail.RuleOperationDelete && rule.Sequence > 0 {
		fields = append(fields,
			output.Field{Name: "sequence", Value: strconv.Itoa(rule.Sequence)},
			output.Field{Name: "sequence_source", Value: string(change.SequenceSource)},
		)
	}
	if change.SourceRuleCount != nil {
		fields = append(fields, output.Field{Name: "source_rule_count", Value: strconv.Itoa(*change.SourceRuleCount)})
	}
	fields = append(fields, output.Field{Name: "status", Value: string(mail.RuleMutationStatusPreview)})
	fields = appendRuleChangePreviewFields(fields, change)
	_ = confirmationMatched
	return output.DryRunPreview{Method: string(change.Operation), Path: acct.Name, Body: fields}
}

// appendRuleChangePreviewFields adds each changed field's old and new value,
// always in the same order.
func appendRuleChangePreviewFields(fields []output.Field, change mail.RuleChangeSet) []output.Field {
	if change.FromAddresses != nil {
		fields = append(fields, output.Field{Name: "from_addresses_before_count", Value: strconv.Itoa(len(change.FromAddresses.Before))})
		for i, r := range change.FromAddresses.Before {
			fields = append(fields, output.Field{Name: fmt.Sprintf("from_addresses_before_%d", i+1), Value: recipientText(r)})
		}
		fields = append(fields, output.Field{Name: "from_addresses_after_count", Value: strconv.Itoa(len(change.FromAddresses.After))})
		for i, r := range change.FromAddresses.After {
			fields = append(fields, output.Field{Name: fmt.Sprintf("from_addresses_after_%d", i+1), Value: recipientText(r)})
		}
	}
	if change.DisplayName != nil && change.Operation != mail.RuleOperationCreate {
		fields = append(fields,
			output.Field{Name: "display_name_before", Value: change.DisplayName.Before},
			output.Field{Name: "display_name_after", Value: change.DisplayName.After},
		)
	}
	if change.Sequence != nil && change.Operation != mail.RuleOperationCreate {
		fields = append(fields,
			output.Field{Name: "sequence_before", Value: strconv.Itoa(change.Sequence.Before)},
			output.Field{Name: "sequence_after", Value: strconv.Itoa(change.Sequence.After)},
		)
	}
	if change.Operation != mail.RuleOperationCreate {
		fields = appendBoolRuleChangeField(fields, "is_enabled", change.IsEnabled)
	}
	if (change.Operation == mail.RuleOperationEnable || change.Operation == mail.RuleOperationDisable) && change.IsEnabled == nil && change.RequestedEnabled != nil {
		value := strconv.FormatBool(*change.RequestedEnabled)
		fields = append(fields,
			output.Field{Name: "is_enabled_before", Value: value},
			output.Field{Name: "is_enabled_after", Value: value},
		)
	}
	fields = appendStringRuleChangeFields(fields, "sender_contains", change.SenderContains)
	fields = appendStringRuleChangeFields(fields, "recipient_contains", change.RecipientContains)
	fields = appendStringRuleChangeFields(fields, "subject_contains", change.SubjectContains)
	fields = appendStringRuleChangeFields(fields, "header_contains", change.HeaderContains)
	fields = appendBoolRuleChangeField(fields, "has_attachments", change.HasAttachments)
	fields = appendBoolRuleChangeField(fields, "mark_as_read", change.MarkAsRead)
	fields = appendBoolRuleChangeField(fields, "stop_processing_rules", change.StopProcessingRules)
	if change.MoveToFolder != nil {
		fields = appendFolderChangePreviewFields(fields, change.MoveToFolder)
	}
	return fields
}

func appendStringRuleChangeFields(fields []output.Field, name string, change *mail.RuleStringCollectionChange) []output.Field {
	if change == nil {
		return fields
	}
	fields = append(fields, output.Field{Name: name + "_before_count", Value: strconv.Itoa(len(change.Before))})
	for i, value := range change.Before {
		fields = append(fields, output.Field{Name: fmt.Sprintf("%s_before_%d", name, i+1), Value: value})
	}
	fields = append(fields, output.Field{Name: name + "_after_count", Value: strconv.Itoa(len(change.After))})
	for i, value := range change.After {
		fields = append(fields, output.Field{Name: fmt.Sprintf("%s_after_%d", name, i+1), Value: value})
	}
	return fields
}

func appendBoolRuleChangeField(fields []output.Field, name string, change *mail.RuleBoolFieldChange) []output.Field {
	if change == nil {
		return fields
	}
	return append(fields,
		output.Field{Name: name + "_before", Value: boolChangeValue(change.Before, change.BeforePresent)},
		output.Field{Name: name + "_after", Value: boolChangeValue(change.After, change.AfterPresent)},
	)
}

func appendPreservedPreviewFields(fields []output.Field, preserved []string) []output.Field {
	fields = append(fields, output.Field{Name: "preserved_fields_count", Value: strconv.Itoa(len(preserved))})
	for i, field := range preserved {
		fields = append(fields, output.Field{Name: fmt.Sprintf("preserved_fields_%d", i+1), Value: field})
	}
	return fields
}

func appendFolderChangePreviewFields(fields []output.Field, change *mail.RuleFolderTargetChange) []output.Field {
	beforeID, beforeText := "", "absent"
	if change.Before != nil {
		beforeID = change.Before.ProviderFolderID
		beforeText = folderSummaryText(*ruleMoveSummary(change.Before))
	}
	afterID, afterText := "", "absent"
	if change.After != nil {
		afterID = change.After.ProviderFolderID
		afterText = folderSummaryText(*ruleMoveSummary(change.After))
	}
	return append(fields,
		output.Field{Name: "move_to_folder_before_id", Value: beforeID},
		output.Field{Name: "move_to_folder_before", Value: beforeText},
		output.Field{Name: "move_to_folder_after_id", Value: afterID},
		output.Field{Name: "move_to_folder_after", Value: afterText},
	)
}

// boolChangeValue tells "the server did not send this field" ("absent") apart
// from "it sent the field, but with no usable value" ("present").
func boolChangeValue(value *bool, present bool) string {
	if value != nil {
		return strconv.FormatBool(*value)
	}
	if present {
		return "present"
	}
	return "absent"
}

func changeSetChanged(change mail.RuleChangeSet) bool {
	return len(rulespkg.ChangedFields(change)) > 0 || change.Operation == mail.RuleOperationDelete
}

func rulesChangedFields(change *mail.RuleChangeSet) []string {
	if change == nil {
		return nil
	}
	return rulespkg.ChangedFields(*change)
}

func joinedFields(fields []string) string {
	if len(fields) == 0 {
		return "none"
	}
	return strings.Join(fields, ", ")
}

func boolValue(v *bool) bool { return v != nil && *v }

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func ruleMoveSummary(target *mail.RuleFolderTarget) *mail.FolderSummary {
	if target == nil {
		return nil
	}
	return &mail.FolderSummary{
		ID:            target.ProviderFolderID,
		DisplayName:   target.DisplayName,
		WellKnownName: target.WellKnownName,
		Path:          target.Path,
	}
}

func plainRuleList(r ruleListResult) func(io.Writer) {
	return func(w io.Writer) {
		for _, rule := range r.Rules {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				plainField(rule.Account),
				plainField(rule.Provider),
				plainField(string(rule.Scope)),
				plainField(rule.ID),
				rule.Sequence,
				enabledText(rule.IsEnabled),
				plainField(rule.DisplayName),
			)
		}
	}
}

// humanRuleView shows one rule: its conditions, exceptions and actions,
// the fields maildo cannot edit, and what the health check found.
func humanRuleView(r ruleViewResult) func(io.Writer) {
	return func(w io.Writer) {
		rule := r.Rule
		fmt.Fprintf(w, "Rule %s in %s (%s)\n\n", quote(plainField(rule.DisplayName)), plainField(rule.Account), plainField(rule.Provider))
		fmt.Fprintf(w, "Order: %d\n", rule.Sequence)
		fmt.Fprintf(w, "State: %s\n", ruleStateText(rule))
		writeRulePredicates(w, "Conditions", rule.Conditions)
		writeRulePredicates(w, "Exceptions", rule.Exceptions)
		writeRuleActions(w, rule.Actions)
		writeUnsupportedFields(w, rule.UnsupportedFields)
		writeHealthFindings(w, rule.HealthFindings)
		fmt.Fprintf(w, "ID: %s\n", plainField(rule.ID))
	}
}

// humanRuleHealth lists each rule with what the health check found.
func humanRuleHealth(r ruleHealthResult) func(io.Writer) {
	return func(w io.Writer) {
		if len(r.Rules) == 0 {
			fmt.Fprintln(w, "No rules.")
			return
		}
		fmt.Fprintf(w, "%s%s\n\n", upperFirst(pluralize(len(r.Rules), "rule", "rules")), accountSuffix(r.AccountSelection))
		many := len(r.Accounts) > 1
		for _, rule := range r.Rules {
			prefix := ""
			if many {
				prefix = plainField(rule.Account) + ": "
			}
			fmt.Fprintf(w, "%s%s (order %d, id %s): %s\n", prefix, quote(plainField(rule.DisplayName)), rule.Sequence, plainField(rule.ID), ruleHealthText(rule.HealthStatus))
			for _, finding := range rule.HealthFindings {
				fmt.Fprintln(w, "  "+findingText(finding))
			}
		}
	}
}

func plainRuleHealth(r ruleHealthResult) func(io.Writer) {
	return func(w io.Writer) {
		for _, rule := range r.Rules {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				plainField(rule.Account),
				plainField(rule.Provider),
				plainField(string(rule.Scope)),
				plainField(rule.ID),
				plainField(string(rule.HealthStatus)),
				plainField(findingCodes(rule.HealthFindings)),
				plainField(rule.DisplayName),
			)
		}
	}
}

// writeRulePredicates prints a rule's conditions or exceptions, also the
// ones maildo cannot edit, so the view shows everything the JSON has.
func writeRulePredicates(w io.Writer, title string, p mail.RulePredicates) {
	var lines []string
	add := func(label, value string) {
		lines = append(lines, "  "+label+": "+value)
	}
	addBool := func(label string, value *bool) {
		if value != nil {
			add(label, yesNo(*value))
		}
	}
	if len(p.FromAddresses) > 0 {
		add("From", recipientList(p.FromAddresses))
	}
	if len(p.SenderContains) > 0 {
		add("Sender contains", ruleStringList(p.SenderContains))
	}
	if len(p.RecipientContains) > 0 {
		add("Recipient contains", ruleStringList(p.RecipientContains))
	}
	if len(p.SubjectContains) > 0 {
		add("Subject contains", ruleStringList(p.SubjectContains))
	}
	if len(p.HeaderContains) > 0 {
		add("Header contains", ruleStringList(p.HeaderContains))
	}
	addBool("Has attachments", p.HasAttachments)
	if len(p.BodyContains) > 0 {
		add("Body contains", ruleStringList(p.BodyContains))
	}
	if len(p.BodyOrSubjectContains) > 0 {
		add("Body or subject contains", ruleStringList(p.BodyOrSubjectContains))
	}
	if len(p.Categories) > 0 {
		add("Categories", ruleStringList(p.Categories))
	}
	if p.Importance != "" {
		add("Importance", plainField(p.Importance))
	}
	if p.Sensitivity != "" {
		add("Sensitivity", plainField(p.Sensitivity))
	}
	if p.MessageActionFlag != "" {
		add("Message action flag", plainField(p.MessageActionFlag))
	}
	if p.WithinSizeRange != nil {
		add("Size", sizeRangeText(*p.WithinSizeRange))
	}
	if len(p.SentToAddresses) > 0 {
		add("Sent to", recipientList(p.SentToAddresses))
	}
	addBool("Is approval request", p.IsApprovalRequest)
	addBool("Is automatic forward", p.IsAutomaticForward)
	addBool("Is automatic reply", p.IsAutomaticReply)
	addBool("Is encrypted", p.IsEncrypted)
	addBool("Is meeting request", p.IsMeetingRequest)
	addBool("Is meeting response", p.IsMeetingResponse)
	addBool("Is non-delivery report", p.IsNonDeliveryReport)
	addBool("Is permission controlled", p.IsPermissionControlled)
	addBool("Is read receipt", p.IsReadReceipt)
	addBool("Is signed", p.IsSigned)
	addBool("Is voicemail", p.IsVoicemail)
	addBool("Not sent to me", p.NotSentToMe)
	addBool("Sent cc me", p.SentCCMe)
	addBool("Sent only to me", p.SentOnlyToMe)
	addBool("Sent to me", p.SentToMe)
	addBool("Sent to or cc me", p.SentToOrCCMe)
	if len(lines) == 0 {
		fmt.Fprintf(w, "%s: none\n", title)
		return
	}
	fmt.Fprintf(w, "%s (%s):\n", title, predicateSemantics(p))
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// writeRuleActions prints what a rule does with the mail it matches.
func writeRuleActions(w io.Writer, a mail.RuleActions) {
	var lines []string
	add := func(label, value string) {
		lines = append(lines, "  "+label+": "+value)
	}
	if a.MoveToFolder != nil {
		add("Move to folder", folderTargetText(*a.MoveToFolder))
	}
	if a.CopyToFolder != nil {
		add("Copy to folder", folderTargetText(*a.CopyToFolder))
	}
	if a.MarkAsRead != nil {
		add("Mark as read", yesNo(*a.MarkAsRead))
	}
	if a.StopProcessingRules {
		add("Stop other rules", "yes")
	}
	if len(a.AssignCategories) > 0 {
		add("Add categories", ruleStringList(a.AssignCategories))
	}
	if a.Delete {
		add("Delete", "yes")
	}
	if a.PermanentDelete {
		add("Delete for good", "yes")
	}
	if a.MarkImportance != "" {
		add("Set importance", plainField(a.MarkImportance))
	}
	if len(a.ForwardTo) > 0 {
		add("Forward to", recipientList(a.ForwardTo))
	}
	if len(a.ForwardAsAttachment) > 0 {
		add("Forward as attachment to", recipientList(a.ForwardAsAttachment))
	}
	if len(a.RedirectTo) > 0 {
		add("Redirect to", recipientList(a.RedirectTo))
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "Actions: none")
		return
	}
	fmt.Fprintln(w, "Actions:")
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// writeUnsupportedFields lists rule fields that maildo keeps as they are
// when it changes the rule, but cannot edit or show in full.
func writeUnsupportedFields(w io.Writer, fields []mail.RuleUnsupportedField) {
	if len(fields) == 0 {
		fmt.Fprintln(w, "Fields maildo cannot edit: none")
		return
	}
	fmt.Fprintln(w, "Fields maildo cannot edit (they stay as they are):")
	for _, field := range fields {
		line := "  " + plainField(field.Area) + "." + plainField(field.Field)
		if field.Summary != "" {
			line += ": " + plainField(field.Summary)
		}
		fmt.Fprintln(w, line)
	}
}

func writeHealthFindings(w io.Writer, findings []mail.RuleHealthFinding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "Health: ok")
		return
	}
	fmt.Fprintln(w, "Health:")
	for _, finding := range findings {
		fmt.Fprintln(w, "  "+findingText(finding))
	}
}

// findingText is one health finding: its severity, what it says, and the
// rule field it is about.
func findingText(finding mail.RuleHealthFinding) string {
	text := fmt.Sprintf("%-7s  %s", plainField(string(finding.Severity)), plainField(finding.Message))
	if finding.Field != "" {
		text += " (" + plainField(finding.Field) + ")"
	}
	return text
}

// ruleHealthText is a rule's health for people: "ok" when the check found
// nothing, otherwise the worst severity it found.
func ruleHealthText(status mail.RuleHealthStatus) string {
	if status == "" || status == mail.RuleHealthHealthy {
		return "ok"
	}
	return plainField(string(status))
}

// ruleStateText says whether a rule is on, and adds when the server
// reports an error for it or marks it read-only.
func ruleStateText(rule mail.Rule) string {
	state := "off"
	if rule.IsEnabled {
		state = "on"
	}
	if rule.HasError {
		state += ", has an error"
	}
	if rule.IsReadOnly {
		state += ", read-only"
	}
	return state
}

// ruleActionSummary is a short list of what a rule does, for the rules
// list. Callers must still pass it through plainField, because folder names
// and server values can hold tabs or line breaks.
func ruleActionSummary(a mail.RuleActions) string {
	var parts []string
	if a.MoveToFolder != nil {
		parts = append(parts, "move to "+folderTargetShort(*a.MoveToFolder))
	}
	if a.CopyToFolder != nil {
		parts = append(parts, "copy to "+folderTargetShort(*a.CopyToFolder))
	}
	if a.MarkAsRead != nil && *a.MarkAsRead {
		parts = append(parts, "mark as read")
	}
	if a.StopProcessingRules {
		parts = append(parts, "stop other rules")
	}
	if len(a.AssignCategories) > 0 {
		parts = append(parts, "add "+pluralize(len(a.AssignCategories), "category", "categories"))
	}
	if a.Delete {
		parts = append(parts, "delete")
	}
	if a.PermanentDelete {
		parts = append(parts, "delete for good")
	}
	if a.MarkImportance != "" {
		parts = append(parts, "set importance to "+a.MarkImportance)
	}
	if len(a.ForwardTo) > 0 {
		parts = append(parts, "forward to "+pluralize(len(a.ForwardTo), "address", "addresses"))
	}
	if len(a.ForwardAsAttachment) > 0 {
		parts = append(parts, "forward as attachment to "+pluralize(len(a.ForwardAsAttachment), "address", "addresses"))
	}
	if len(a.RedirectTo) > 0 {
		parts = append(parts, "redirect to "+pluralize(len(a.RedirectTo), "address", "addresses"))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func folderTargetShort(target mail.RuleFolderTarget) string {
	if target.Path != "" {
		return target.Path
	}
	if target.DisplayName != "" {
		return target.DisplayName
	}
	return target.ProviderFolderID
}

func folderTargetText(target mail.RuleFolderTarget) string {
	short := folderTargetShort(target)
	text := plainField(short)
	var extra []string
	if target.Missing {
		extra = append(extra, "missing")
	}
	if role := folderRoleText(target.WellKnownName); role != "" && !strings.EqualFold(target.DisplayName, role) {
		extra = append(extra, "the "+plainField(role)+" folder")
	}
	if target.ProviderFolderID != "" && target.ProviderFolderID != short {
		extra = append(extra, "id "+plainField(target.ProviderFolderID))
	}
	if len(extra) > 0 {
		text += " (" + strings.Join(extra, ", ") + ")"
	}
	return text
}

func ruleStringList(values []string) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, plainField(value))
	}
	return strings.Join(out, ", ")
}

func enabledText(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

// predicateSemantics says how a rule's conditions combine. Outlook's way
// gets a sentence; anything else is shown as the server reports it.
func predicateSemantics(p mail.RulePredicates) string {
	families := p.Semantics.Families
	if families == "" {
		families = mail.DefaultRulePredicateSemantics().Families
	}
	repeated := p.Semantics.RepeatedValues
	if repeated == "" {
		repeated = mail.DefaultRulePredicateSemantics().RepeatedValues
	}
	if families == "and" && repeated == "or" {
		return "all must match; a list matches when any value does"
	}
	return "conditions combine with " + plainField(families) + ", values with " + plainField(repeated)
}

func recipientList(recipients []mail.Recipient) string {
	out := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient.Address != "" {
			out = append(out, plainField(recipient.Address))
			continue
		}
		if recipient.Name != "" {
			out = append(out, plainField(recipient.Name))
		}
	}
	return strings.Join(out, ", ")
}

// sizeRangeText is a size condition. Outlook gives the sizes in kilobytes.
func sizeRangeText(r mail.RuleSizeRange) string {
	switch {
	case r.Minimum != nil && r.Maximum != nil:
		return fmt.Sprintf("%d to %d KB", *r.Minimum, *r.Maximum)
	case r.Minimum != nil:
		return fmt.Sprintf("at least %d KB", *r.Minimum)
	case r.Maximum != nil:
		return fmt.Sprintf("at most %d KB", *r.Maximum)
	default:
		return "any"
	}
}

func findingCodes(findings []mail.RuleHealthFinding) string {
	if len(findings) == 0 {
		return ""
	}
	out := make([]string, 0, len(findings))
	for _, finding := range findings {
		out = append(out, finding.Code)
	}
	return strings.Join(out, ",")
}

// findingCounts counts the findings per severity, with a zero for every
// severity that has none, so scripts always see all three keys.
func findingCounts(rules []mail.Rule) map[string]int {
	counts := map[string]int{
		string(mail.RuleHealthInfo):    0,
		string(mail.RuleHealthWarning): 0,
		string(mail.RuleHealthError):   0,
	}
	for _, rule := range rules {
		for _, finding := range rule.HealthFindings {
			counts[string(finding.Severity)]++
		}
	}
	return counts
}
