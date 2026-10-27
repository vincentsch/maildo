package main

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	rulespkg "github.com/vincentsch/maildo/internal/rules"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

type rulesModule struct {
	h    *handlers
	defs []cmdDef
}

const (
	ruleMutationPredicateFamilies       = "conjunctive"
	ruleMutationPredicateRepeatedValues = "disjunctive"
)

type ruleMutationSemantics struct {
	Families       string
	RepeatedValues string
}

// ruleMutationPredicateSemantics is how conditions combine when maildo writes
// a rule: different conditions must all match, and several values for one
// condition match when any of them does. Previews report it in JSON with
// these words; rule reads use "and" and "or".
func ruleMutationPredicateSemantics() ruleMutationSemantics {
	return ruleMutationSemantics{
		Families:       ruleMutationPredicateFamilies,
		RepeatedValues: ruleMutationPredicateRepeatedValues,
	}
}

func newRulesModule(h *handlers) rulesModule {
	listModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	viewModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	createFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		configureRuleIntentFlags(cmd, false)
		cmd.Flags().Bool("apply", false, "Create the rule; without --apply maildo only shows what it would create")
	}
	updateFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		configureRuleIntentFlags(cmd, true)
		cmd.Flags().Bool("apply", false, "Change the rule; without --apply maildo only shows what would change")
	}
	defs := []cmdDef{
		{
			path: "rules", use: "rules", short: "List and change Inbox rules (Outlook)", groupID: "rules",
			examples: []string{"maildo rules list", "maildo rules view rule-id", "maildo rules create --subject-contains invoice --move-to Archive"},
		},
		{
			path: "rules list", use: "list", short: "List Inbox rules",
			examples: []string{
				"maildo rules list",
				"maildo rules list --account work",
				"maildo rules list --all-accounts",
				"maildo rules list --json",
				"maildo rules list --plain",
			},
			related:      []string{"rules view", "rules health", "folders list"},
			outputModes:  listModes,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
			},
			run: h.rulesList,
		},
		{
			path: "rules view", use: "view <rule-id>", short: "Show one Inbox rule",
			examples: []string{
				"maildo rules view rule-id",
				"maildo rules view rule-id --account work",
				"maildo rules view rule-id --json",
			},
			related:      []string{"rules list", "rules health"},
			outputModes:  viewModes,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.rulesView,
		},
		{
			path: "rules health", use: "health", short: "Check Inbox rules for problems",
			examples: []string{
				"maildo rules health",
				"maildo rules health --account work",
				"maildo rules health --all-accounts",
				"maildo rules health --json",
				"maildo rules health --plain",
			},
			related:      []string{"rules list", "rules view", "folders list"},
			outputModes:  listModes,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
			},
			run: h.rulesHealth,
		},
		{
			path: "rules create", use: "create", short: "Create an Inbox rule",
			examples: []string{
				`maildo rules create --account work --subject-contains invoice --move-to Archive`,
				`maildo rules create --from "Client <client@example.com>" --move-to Clients --mark-read --apply`,
				`maildo rules create --sender-contains alerts --has-attachments --stop-processing --json`,
			},
			related:      []string{"rules list", "rules view", "folders list"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    createFlags,
			run:          h.rulesCreate,
		},
		{
			path: "rules update", use: "update <rule-id>", short: "Change an Inbox rule; a condition you give replaces its old values",
			examples: []string{
				`maildo rules update rule-id --account work --subject-contains invoice --subject-contains receipt`,
				`maildo rules update rule-id --move-to Archive --apply`,
				`maildo rules update rule-id --no-mark-read --apply --json`,
			},
			related:      []string{"rules view", "rules list", "folders list"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure:    updateFlags,
			run:          h.rulesUpdate,
		},
		{
			path: "rules enable", use: "enable <rule-id>", short: "Turn an Inbox rule on",
			examples: []string{
				`maildo rules enable rule-id --account work`,
				`maildo rules enable rule-id --dry-run --json`,
			},
			related:      []string{"rules disable", "rules view"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.rulesEnable,
		},
		{
			path: "rules disable", use: "disable <rule-id>", short: "Turn an Inbox rule off",
			examples: []string{
				`maildo rules disable rule-id --account work`,
				`maildo rules disable rule-id --dry-run --json`,
			},
			related:      []string{"rules enable", "rules view"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.rulesDisable,
		},
		{
			path: "rules delete", use: "delete <rule-id>", short: "Delete an Inbox rule",
			examples: []string{
				`maildo rules delete rule-id --account work --dry-run`,
				`maildo rules delete rule-id --confirm rule-id`,
				`maildo rules delete rule-id --confirm rule-id --json`,
			},
			related:      []string{"rules list", "rules view"},
			outputModes:  viewModes,
			mutates:      true,
			destructive:  true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("confirm", "", "The rule's id again, to confirm the delete")
			},
			run: h.rulesDelete,
		},
	}
	defs = markMailboxAuth(defs,
		"rules list",
		"rules view",
		"rules health",
		"rules create",
		"rules update",
		"rules enable",
		"rules disable",
		"rules delete",
	)
	return rulesModule{h: h, defs: defs}
}

func configureRuleIntentFlags(cmd *cobra.Command, update bool) {
	cmd.Flags().String("name", "", "Name of the rule")
	cmd.Flags().StringArray("from", nil, "Match mail from this address (repeatable; comma lists accepted)")
	cmd.Flags().StringArray("sender-contains", nil, "Match a sender address that contains this text (repeatable; any value matches)")
	cmd.Flags().StringArray("subject-contains", nil, "Match a subject that contains this text (repeatable; any value matches, but different conditions must all match)")
	cmd.Flags().StringArray("recipient-contains", nil, "Match a recipient address that contains this text (repeatable; any value matches)")
	cmd.Flags().StringArray("header-contains", nil, "Match a message header that contains this text (repeatable; any value matches)")
	cmd.Flags().Bool("has-attachments", false, "Match messages with attachments")
	cmd.Flags().String("move-to", "", "Folder to move matching messages to")
	cmd.Flags().Bool("mark-read", false, "Mark matching messages as read")
	cmd.Flags().Bool("stop-processing", false, "Run no later rules on a message this rule matched")
	sequenceHelp := "Place in the order rules run, from 1 to 2147483647; no other rule may use it (default: after the last rule)"
	if update {
		sequenceHelp = "Place in the order rules run, from 1 to 2147483647; no other rule may use it"
	}
	cmd.Flags().Int64("sequence", 0, sequenceHelp)
	if update {
		cmd.Flags().Bool("no-has-attachments", false, "Remove the has-attachments condition")
		cmd.Flags().Bool("no-mark-read", false, "Remove the mark-read action")
		cmd.Flags().Bool("no-stop-processing", false, "Remove the stop-processing action")
	}
}

func (m rulesModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "rules", Title: "Rules"}}
}

func (m rulesModule) Commands() []*rungrad.Command {
	parent := m.defs[0].command()
	for _, def := range m.defs[1:] {
		parent.AddCommand(def.command())
	}
	return []*rungrad.Command{parent}
}

func (m rulesModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

func (h *handlers) rulesList(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		// Read every account before printing anything, so a failure in a later
		// account prints no half list. A non-nil slice keeps empty JSON as [].
		rows := make([]mail.Rule, 0)
		for _, acct := range sel.Accounts {
			rules, err := h.listAnnotatedRules(cmd.Context(), clientScope, acct)
			if err != nil {
				return err
			}
			rows = append(rows, rules...)
		}
		result := ruleListResult{AccountSelection: sel, Rules: rows, Count: len(rows)}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  rulesMeta(cmd.CommandPath(), sel, len(rows), nil),
			Human: humanRuleList(result),
			Plain: plainRuleList(result),
		})
	})
}

func (h *handlers) rulesView(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		rule, err := h.getAnnotatedRule(cmd.Context(), clientScope, acct, args[0])
		if err != nil {
			return err
		}
		result := ruleViewResult{
			AccountSelection: sel,
			Account:          acct.Name,
			Provider:         acct.Provider,
			RequestedRuleID:  args[0],
			Rule:             rule,
		}
		return f.WriteResultWithMeta(result, rulesViewMeta(cmd.CommandPath(), acct, args[0]), humanRuleView(result))
	})
}

func (h *handlers) rulesHealth(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		// Like list: read every account first, and keep empty JSON as [].
		rows := make([]mail.Rule, 0)
		for _, acct := range sel.Accounts {
			rules, err := h.listAnnotatedRules(cmd.Context(), clientScope, acct)
			if err != nil {
				return err
			}
			rows = append(rows, rules...)
		}
		counts := findingCounts(rows)
		result := ruleHealthResult{AccountSelection: sel, Rules: rows, Count: len(rows), FindingCounts: counts}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  rulesMeta(cmd.CommandPath(), sel, len(rows), counts),
			Human: humanRuleHealth(result),
			Plain: plainRuleHealth(result),
		})
	})
}

// rulesCreate checks the flags before it contacts the server, then shows
// the new rule or, with --apply, creates it.
func (h *handlers) rulesCreate(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	if err := rejectCreateUpdateUsage(f, cmd); err != nil {
		return err
	}
	if err := rejectRuleFalsePresenceFlags(cmd); err != nil {
		return err
	}
	if err := rejectRuleBoolConflicts(cmd); err != nil {
		return err
	}
	intent, moveRaw, err := parseRuleIntentFlags(cmd, false)
	if err != nil {
		return err
	}
	// Count --move-to as an action for this first check. The folder itself is
	// looked up later, in the same folder list the write uses.
	localIntent := intent
	if moveRaw != "" {
		localIntent.MoveToFolderSet = true
	}
	if err := rulespkg.ValidateCreateIntent(localIntent); err != nil {
		return err
	}
	if err := rejectEmptyRuleAccountSelection(cmd); err != nil {
		return err
	}
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		ruleCtx, err := h.ruleMutationContext(cmd.Context(), clientScope, acct)
		if err != nil {
			return err
		}
		if moveRaw != "" {
			folder, err := resolveRuleMoveFolder(acct, *intent.MoveToFolderRef, ruleCtx.Folders.Folders)
			if err != nil {
				return err
			}
			intent.MoveToFolder = &folder
			intent.MoveToFolderSet = true
		}
		req, err := rulespkg.BuildCreate(intent, ruleCtx.Rules.Rules)
		if err != nil {
			return err
		}
		req.Rule = stampRuleForAccount(req.Rule, acct)
		// The preview, the write and the check afterwards all use this one
		// folder list, so they name the same folders.
		expected := annotateExpectedRule(acct, req.Rule, ruleCtx.Folders.Folders, append(ruleCtx.Rules.Rules, req.Rule))
		req.Rule = expected
		applyFlag, _ := cmd.Flags().GetBool("apply")
		apply := !f.DryRun() && applyFlag
		if !apply {
			return writeRulePreview(f, cmd, nil, acct, expected, req.ChangeSet, true, false)
		}
		attempted := false
		out, err := func() (ruleMutationResult, error) {
			var applied mail.RuleMutationResult
			if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessRulesMutate, func(manager provider.RuleManager) error {
				attempted = true
				var err error
				applied, err = manager.CreateRule(cmd.Context(), req)
				return err
			}); err != nil {
				if reason, ok := ruleMutationOutcomeReason(err); ok {
					return ruleMutationResult{}, ruleVerificationFailure(acct, mail.RuleOperationCreate, "", outcomeReasonCause(reason), 0, &reason)
				}
				return ruleMutationResult{}, err
			}
			if strings.TrimSpace(applied.RuleID) == "" {
				reason := provider.RuleMutationOutcomeMissingID
				return ruleMutationResult{}, ruleVerificationFailure(acct, mail.RuleOperationCreate, "", mail.RuleVerificationCauseMalformedResponse, 0, &reason)
			}
			expected.ID = applied.RuleID
			observed, verification, err := h.verifyRuleMutation(cmd.Context(), clientScope, acct, ruleCtx.Folders.Folders, expected, req.ChangeSet, nil)
			if err != nil {
				return ruleMutationResult{}, err
			}
			return ruleMutationResultFromObservation(sel, observed, req.ChangeSet, ruleCtx.ProviderMetadata, []string{}, &verification, mail.RuleMutationStatusApplied), nil
		}()
		return h.finishRuleChange(f, cmd, acct, mail.RuleOperationCreate, expected, attempted, out, err)
	})
}

// finishRuleChange logs a rule change that reached the server, then
// writes the result or returns the error.
func (h *handlers) finishRuleChange(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, op mail.RuleMutationOperation, rule mail.Rule, attempted bool, out ruleMutationResult, err error) error {
	if attempted {
		change := audit.Change{Kind: "rule", ID: firstNonEmpty(out.RuleID, rule.ID), Name: firstNonEmpty(out.DisplayName, rule.DisplayName)}
		status := h.logChange(f, cmd, acct, string(op), change, err)
		out.ActionLogStatus = &status
	}
	if err != nil {
		return err
	}
	return f.WriteResultWithMeta(out, ruleMutationMeta(cmd.CommandPath(), out), humanRuleMutation(out))
}

// rulesUpdate checks the flags before it contacts the server, then finds
// the rule and the folder in one read of the account's rules and folders.
func (h *handlers) rulesUpdate(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	if err := rejectCreateUpdateUsage(f, cmd); err != nil {
		return err
	}
	if err := rejectRuleFalsePresenceFlags(cmd); err != nil {
		return err
	}
	if err := rejectRuleBoolConflicts(cmd); err != nil {
		return err
	}
	intent, moveRaw, err := parseRuleIntentFlags(cmd, true)
	if err != nil {
		return err
	}
	// Count --move-to as an action for this first check. The folder itself is
	// looked up later, in the same folder list the write uses.
	localIntent := intent
	if moveRaw != "" {
		localIntent.MoveToFolderSet = true
	}
	if err := rulespkg.ValidateUpdateIntent(localIntent); err != nil {
		return err
	}
	if err := rejectBlankRuleID(args[0]); err != nil {
		return err
	}
	if err := rejectEmptyRuleAccountSelection(cmd); err != nil {
		return err
	}
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		ruleCtx, err := h.ruleMutationContext(cmd.Context(), clientScope, acct)
		if err != nil {
			return err
		}
		existing, err := ruleFromSnapshot(acct, args[0], ruleCtx.Rules.Rules, ruleCtx.Folders.Folders)
		if err != nil {
			return err
		}
		if err := rejectReadOnlyRule(acct, existing); err != nil {
			return err
		}
		if err := rulespkg.ValidateUpdateSequence(existing, ruleCtx.Rules.Rules, intent); err != nil {
			return err
		}
		if moveRaw != "" {
			folder, err := resolveRuleMoveFolder(acct, *intent.MoveToFolderRef, ruleCtx.Folders.Folders)
			if err != nil {
				return err
			}
			intent.MoveToFolder = &folder
			intent.MoveToFolderSet = true
		}
		req, err := rulespkg.BuildUpdate(existing, ruleCtx.Rules.Rules, intent)
		if err != nil {
			return err
		}
		// The rules package knows nothing about the account's folders, so
		// the folder names and health findings are added here.
		req.Expected = annotateExpectedRule(acct, req.Expected, ruleCtx.Folders.Folders, replaceRule(ruleCtx.Rules.Rules, req.Expected))
		applyFlag, _ := cmd.Flags().GetBool("apply")
		apply := !f.DryRun() && applyFlag
		if !apply {
			return writeRulePreview(f, cmd, args, acct, req.Expected, req.ChangeSet, true, false)
		}
		attempted := false
		out, err := h.applyRuleUpdate(cmd.Context(), clientScope, sel, acct, ruleCtx, existing, req, &attempted)
		return h.finishRuleChange(f, cmd, acct, mail.RuleOperationUpdate, existing, attempted, out, err)
	})
}

// applyRuleUpdate writes a rule update and reads the rule back to check
// it. attempted reports whether the write was sent.
func (h *handlers) applyRuleUpdate(ctx context.Context, clientScope provider.AccountClientScope, sel provider.AccountSelection, acct provider.ResolvedAccount, ruleCtx ruleMutationContext, existing mail.Rule, req mail.RuleUpdateRequest, attempted *bool) (ruleMutationResult, error) {
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesMutate, func(manager provider.RuleManager) error {
		*attempted = true
		_, err := manager.UpdateRule(ctx, req)
		return err
	}); err != nil {
		reason, ok := ruleMutationOutcomeReason(err)
		if !ok {
			return ruleMutationResult{}, err
		}
		observed, verification, verifyErr := h.verifyRuleMutation(ctx, clientScope, acct, ruleCtx.Folders.Folders, req.Expected, req.ChangeSet, &reason)
		if verifyErr != nil {
			return ruleMutationResult{}, verifyErr
		}
		return ruleMutationResultFromObservation(sel, observed, req.ChangeSet, ruleCtx.ProviderMetadata, rulespkg.PreservedFields(existing, req.ChangeSet), &verification, mail.RuleMutationStatusApplied), nil
	}
	observed, verification, err := h.verifyRuleMutation(ctx, clientScope, acct, ruleCtx.Folders.Folders, req.Expected, req.ChangeSet, nil)
	if err != nil {
		return ruleMutationResult{}, err
	}
	return ruleMutationResultFromObservation(sel, observed, req.ChangeSet, ruleCtx.ProviderMetadata, rulespkg.PreservedFields(existing, req.ChangeSet), &verification, mail.RuleMutationStatusApplied), nil
}

func (h *handlers) rulesEnable(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return h.rulesSetEnabled(f, cmd, args[0], true)
}

func (h *handlers) rulesDisable(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return h.rulesSetEnabled(f, cmd, args[0], false)
}

// rulesSetEnabled turns a rule on or off. Both are the same write with a
// different is_enabled value.
func (h *handlers) rulesSetEnabled(f *rungrad.Factory, cmd *cobra.Command, ruleID string, enabled bool) error {
	if err := rejectBlankRuleID(ruleID); err != nil {
		return err
	}
	if err := rejectEmptyRuleAccountSelection(cmd); err != nil {
		return err
	}
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		ruleCtx, err := h.ruleMutationContext(cmd.Context(), clientScope, acct)
		if err != nil {
			return err
		}
		existing, err := ruleFromSnapshot(acct, ruleID, ruleCtx.Rules.Rules, ruleCtx.Folders.Folders)
		if err != nil {
			return err
		}
		if err := rejectReadOnlyRule(acct, existing); err != nil {
			return err
		}
		req, err := rulespkg.BuildEnableDisable(existing, enabled)
		if err != nil {
			return err
		}
		// The rules package knows nothing about the account's folders, so
		// the folder names and health findings are added here.
		req.Expected = annotateExpectedRule(acct, req.Expected, ruleCtx.Folders.Folders, replaceRule(ruleCtx.Rules.Rules, req.Expected))
		if f.DryRun() {
			return writeRulePreview(f, cmd, []string{ruleID}, acct, req.Expected, req.ChangeSet, false, false)
		}
		if req.ChangeSet.IsEnabled == nil {
			out := ruleMutationResultFromObservation(sel, existing, req.ChangeSet, ruleCtx.ProviderMetadata, []string{}, nil, mail.RuleMutationStatusUnchanged)
			out.Applied = false
			out.Changed = false
			return f.WriteResultWithMeta(out, ruleMutationMeta(cmd.CommandPath(), out), humanRuleMutation(out))
		}
		attempted := false
		out, err := h.applyRuleUpdate(cmd.Context(), clientScope, sel, acct, ruleCtx, existing, req, &attempted)
		return h.finishRuleChange(f, cmd, acct, req.ChangeSet.Operation, existing, attempted, out, err)
	})
}

// rulesDelete needs the rule's exact id in --confirm. After the delete it
// reads the rule again and expects the server to say it is gone.
func (h *handlers) rulesDelete(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	if err := rejectBlankRuleID(args[0]); err != nil {
		return err
	}
	if err := rejectEmptyRuleAccountSelection(cmd); err != nil {
		return err
	}
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		ruleCtx, err := h.ruleMutationContext(cmd.Context(), clientScope, acct)
		if err != nil {
			return err
		}
		target, err := ruleFromSnapshot(acct, args[0], ruleCtx.Rules.Rules, ruleCtx.Folders.Folders)
		if err != nil {
			return err
		}
		if err := rejectReadOnlyRule(acct, target); err != nil {
			return err
		}
		change := mail.RuleChangeSet{Operation: mail.RuleOperationDelete}
		if f.DryRun() {
			return writeRulePreview(f, cmd, args, acct, target, change, false, true)
		}
		confirm, _ := cmd.Flags().GetString("confirm")
		if !ruleDeleteConfirmationMatches(target.ID, confirm) {
			return ruleConfirmationRequiredError(acct)
		}
		attempted := false
		out, err := func() (ruleMutationResult, error) {
			if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessRulesMutate, func(manager provider.RuleManager) error {
				attempted = true
				_, err := manager.DeleteRule(cmd.Context(), mail.RuleDeleteRequest{Scope: mail.RuleScopeInbox, RuleID: target.ID})
				return err
			}); err != nil {
				reason, ok := ruleMutationOutcomeReason(err)
				if !ok {
					return ruleMutationResult{}, err
				}
				verification, verifyErr := h.verifyRuleDeleted(cmd.Context(), clientScope, acct, target.ID, &reason)
				if verifyErr != nil {
					return ruleMutationResult{}, verifyErr
				}
				return ruleDeleteResultFromObservation(sel, target, ruleCtx.ProviderMetadata, true, true, verification), nil
			}
			verification, err := h.verifyRuleDeleted(cmd.Context(), clientScope, acct, target.ID, nil)
			if err != nil {
				return ruleMutationResult{}, err
			}
			return ruleDeleteResultFromObservation(sel, target, ruleCtx.ProviderMetadata, true, true, verification), nil
		}()
		return h.finishRuleChange(f, cmd, acct, mail.RuleOperationDelete, target, attempted, out, err)
	})
}

// ruleMutationContext is the one read of folders and rules that a rule
// change works from. The preview, the write and the check afterwards all use
// it, so they never see different folders.
type ruleMutationContext struct {
	Folders          mail.FolderList
	Rules            mail.RuleListResult
	ProviderMetadata mail.RuleMutationProviderMetadata
}

// ruleMutationContext reads folders and rules once, and checks that the
// account can change rules at all.
func (h *handlers) ruleMutationContext(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount) (ruleMutationContext, error) {
	for _, access := range []provider.ClientAccessKind{
		provider.ClientAccessFoldersRead,
		provider.ClientAccessRulesRead,
		provider.ClientAccessRulesMutate,
	} {
		if err := provider.CheckClientAccess(acct, access); err != nil {
			return ruleMutationContext{}, err
		}
	}
	var folders mail.FolderList
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		var err error
		folders, err = reader.ListFolders(ctx, folderListRequest(mail.FolderListDetailIdentity))
		return err
	}); err != nil {
		return ruleMutationContext{}, err
	}
	var rules mail.RuleListResult
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesRead, func(reader provider.RuleReader) error {
		var err error
		rules, err = reader.ListRules(ctx, mail.RuleListRequest{Scope: mail.RuleScopeInbox})
		return err
	}); err != nil {
		return ruleMutationContext{}, err
	}
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesMutate, func(provider.RuleManager) error { return nil }); err != nil {
		return ruleMutationContext{}, err
	}
	metadata, err := localRuleMutationProviderMetadata(acct)
	if err != nil {
		return ruleMutationContext{}, err
	}
	return ruleMutationContext{Folders: folders, Rules: rules, ProviderMetadata: metadata}, nil
}

// localRuleMutationProviderMetadata names the API behind a rule change from
// the selected account, never from what the server answered.
func localRuleMutationProviderMetadata(acct provider.ResolvedAccount) (mail.RuleMutationProviderMetadata, error) {
	source := mail.RuleMutationSourceFake
	if acct.Provider == "outlook" {
		source = mail.RuleMutationSourceMicrosoftGraph
	}
	metadata, err := mail.NewRuleMutationProviderMetadata(source)
	if err != nil {
		return mail.RuleMutationProviderMetadata{}, rungrad.NewError(rungrad.ExitAPI, "maildo does not know which API changes rules for this account")
	}
	return metadata, nil
}

// rejectCreateUpdateUsage refuses flag mixes that make no sense for a rule
// preview.
func rejectCreateUpdateUsage(f *rungrad.Factory, cmd *cobra.Command) error {
	apply, _ := cmd.Flags().GetBool("apply")
	if f.DryRun() && apply {
		return rungrad.NewError(rungrad.ExitUsage, "--dry-run and --apply cannot be combined")
	}
	if !apply && f != nil && f.Flags != nil && f.Flags.IncludeMeta {
		return rungrad.NewError(rungrad.ExitUsage, "for rules create and rules update, --include-meta needs --apply")
	}
	return nil
}

// rejectRuleBoolConflicts refuses a flag together with its --no- form.
func rejectRuleBoolConflicts(cmd *cobra.Command) error {
	for _, pair := range []struct {
		set   string
		clear string
	}{
		{"has-attachments", "no-has-attachments"},
		{"mark-read", "no-mark-read"},
		{"stop-processing", "no-stop-processing"},
	} {
		if cmd.Flags().Changed(pair.set) && cmd.Flags().Changed(pair.clear) {
			return rungrad.NewError(rungrad.ExitUsage, "--"+pair.set+" and --"+pair.clear+" cannot be combined")
		}
	}
	return nil
}

// rejectRuleFalsePresenceFlags refuses forms such as --mark-read=false.
// These flags work by being there; to turn one off, use its --no- form.
func rejectRuleFalsePresenceFlags(cmd *cobra.Command) error {
	for _, name := range []string{
		"has-attachments", "no-has-attachments",
		"mark-read", "no-mark-read",
		"stop-processing", "no-stop-processing",
	} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || !flag.Changed {
			continue
		}
		value, err := cmd.Flags().GetBool(name)
		if err != nil || !value {
			return rungrad.NewError(rungrad.ExitUsage, "--"+name+" takes no value; leave it out, or use its --no- form where there is one")
		}
	}
	return nil
}

func rejectBlankRuleID(ruleID string) error {
	if strings.TrimSpace(ruleID) != "" {
		return nil
	}
	return errs.NewWithDetails(errs.CodeInvalidRuleInput, "the rule id is empty", map[string]any{"field": "rule_id", "reason": "empty"})
}

// rejectEmptyRuleAccountSelection refuses an empty --account. Leaving the
// flag out is fine: then the active account is used.
func rejectEmptyRuleAccountSelection(cmd *cobra.Command) error {
	if cmd.Flags().Changed("account") {
		accounts, _ := cmd.Flags().GetStringArray("account")
		if len(accounts) == 0 {
			return errs.New(errs.CodeInvalidAccountScope, "--account needs an account name")
		}
	}
	return nil
}

// parseRuleIntentFlags turns the flags into the change maildo wants to make.
// It returns the --move-to folder as typed, because finding that folder needs
// the account's folder list.
func parseRuleIntentFlags(cmd *cobra.Command, update bool) (rulespkg.RuleMutationIntent, string, error) {
	flags := cmd.Flags()
	var intent rulespkg.RuleMutationIntent
	if flags.Changed("name") {
		intent.Name, _ = flags.GetString("name")
		intent.NameSet = true
	}
	if flags.Changed("from") {
		values, _ := flags.GetStringArray("from")
		parsed, err := rulespkg.ParseFromAddresses(values)
		if err != nil {
			return rulespkg.RuleMutationIntent{}, "", err
		}
		intent.FromAddresses = parsed
		intent.FromSet = true
	}
	if flags.Changed("sender-contains") {
		intent.SenderContains, _ = flags.GetStringArray("sender-contains")
		intent.SenderContainsSet = true
	}
	if flags.Changed("subject-contains") {
		intent.SubjectContains, _ = flags.GetStringArray("subject-contains")
		intent.SubjectContainsSet = true
	}
	if flags.Changed("recipient-contains") {
		intent.RecipientContains, _ = flags.GetStringArray("recipient-contains")
		intent.RecipientContainsSet = true
	}
	if flags.Changed("header-contains") {
		intent.HeaderContains, _ = flags.GetStringArray("header-contains")
		intent.HeaderContainsSet = true
	}
	if flags.Changed("has-attachments") {
		value, _ := flags.GetBool("has-attachments")
		intent.HasAttachments = rulespkg.BoolIntent{Set: true, Value: value}
	}
	if update && flags.Changed("no-has-attachments") {
		value, _ := flags.GetBool("no-has-attachments")
		if value {
			intent.HasAttachments = rulespkg.BoolIntent{Clear: true}
		}
	}
	if flags.Changed("mark-read") {
		value, _ := flags.GetBool("mark-read")
		intent.MarkAsRead = rulespkg.BoolIntent{Set: true, Value: value}
	}
	if update && flags.Changed("no-mark-read") {
		value, _ := flags.GetBool("no-mark-read")
		if value {
			intent.MarkAsRead = rulespkg.BoolIntent{Clear: true}
		}
	}
	if flags.Changed("stop-processing") {
		value, _ := flags.GetBool("stop-processing")
		intent.StopProcessingRules = rulespkg.BoolIntent{Set: true, Value: value}
	}
	if update && flags.Changed("no-stop-processing") {
		value, _ := flags.GetBool("no-stop-processing")
		if value {
			intent.StopProcessingRules = rulespkg.BoolIntent{Clear: true}
		}
	}
	if flags.Changed("sequence") {
		intent.Sequence, _ = flags.GetInt64("sequence")
		intent.SequenceSet = true
	}
	moveRaw := ""
	if flags.Changed("move-to") {
		moveRaw, _ = flags.GetString("move-to")
		if strings.TrimSpace(moveRaw) == "" {
			return rulespkg.RuleMutationIntent{}, "", errs.NewWithDetails(errs.CodeInvalidRuleInput, "--move-to needs a folder name", map[string]any{"field": "move_to", "reason": "empty"})
		}
		ref, err := mail.ParseFolderRef(moveRaw)
		if err != nil {
			return rulespkg.RuleMutationIntent{}, "", err
		}
		intent.MoveToFolderRef = &ref
		intent.MoveToFolderSet = true
	}
	return intent, moveRaw, nil
}

// resolveRuleMoveFolder finds the --move-to folder the same way the folders
// commands do, in the folder list that was already read.
func resolveRuleMoveFolder(acct provider.ResolvedAccount, ref mail.FolderRef, folders []mail.Folder) (mail.Folder, error) {
	res, err := mail.ResolveFolderRef(mail.FolderResolveScope{Account: acct.Name, Provider: acct.Provider}, ref, folders)
	if err != nil {
		return mail.Folder{}, err
	}
	return res.Folder, nil
}

// ruleFromSnapshot finds the rule in the list that was already read. Two
// rules with the same id are an error: maildo will not let the order of the
// server's list pick which one it changes.
func ruleFromSnapshot(acct provider.ResolvedAccount, ruleID string, rules []mail.Rule, folders []mail.Folder) (mail.Rule, error) {
	var matched *mail.Rule
	for _, rule := range rules {
		if rule.ID != ruleID {
			continue
		}
		if matched != nil {
			return mail.Rule{}, rungrad.NewError(rungrad.ExitAPI, "the server listed two rules with the same id")
		}
		candidate := rule
		matched = &candidate
	}
	if matched != nil {
		rule := stampRuleForAccount(*matched, acct)
		return rulespkg.AnnotateRule(rule, folders, acct.Email, ruleSequenceCounts(rules)), nil
	}
	return mail.Rule{}, errs.NewWithDetails(errs.CodeRuleNotFound, "rule not found", map[string]any{
		"account":  acct.Name,
		"provider": acct.Provider,
		"scope":    string(mail.RuleScopeInbox),
		"rule_id":  ruleID,
	})
}

func stampRuleForAccount(rule mail.Rule, acct provider.ResolvedAccount) mail.Rule {
	rule.Account = acct.Name
	rule.Provider = acct.Provider
	if rule.Scope == "" {
		rule.Scope = mail.RuleScopeInbox
	}
	return rule
}

func annotateExpectedRule(acct provider.ResolvedAccount, rule mail.Rule, folders []mail.Folder, rules []mail.Rule) mail.Rule {
	rule = stampRuleForAccount(rule, acct)
	return rulespkg.AnnotateRule(rule, folders, acct.Email, ruleSequenceCounts(rules))
}

func replaceRule(rules []mail.Rule, replacement mail.Rule) []mail.Rule {
	out := append([]mail.Rule(nil), rules...)
	for i, rule := range out {
		if rule.ID == replacement.ID {
			out[i] = replacement
			return out
		}
	}
	return append(out, replacement)
}

// verifyRuleMutation reads the rule back after a write and turns any
// difference from what maildo wrote into an error.
func (h *handlers) verifyRuleMutation(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, folders []mail.Folder, expected mail.Rule, change mail.RuleChangeSet, outcomeReason *provider.RuleMutationOutcomeUnknownReason) (mail.Rule, mail.RuleVerification, error) {
	var observed mail.Rule
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesRead, func(reader provider.RuleReader) error {
		var err error
		observed, err = reader.GetRule(ctx, mail.RuleGetRequest{Scope: mail.RuleScopeInbox, RuleID: expected.ID})
		return err
	})
	if err != nil {
		cause := verificationCauseForReadError(err)
		return mail.Rule{}, mail.RuleVerification{}, ruleVerificationFailure(acct, change.Operation, expected.ID, cause, 0, outcomeReason)
	}
	if err := rulespkg.ValidateProviderSequence(observed.Sequence); err != nil {
		return mail.Rule{}, mail.RuleVerification{}, err
	}
	observed = annotateExpectedRule(acct, observed, folders, []mail.Rule{observed})
	v := rulespkg.VerifyRuleMutation(expected, observed, change)
	if v.Status != mail.RuleVerificationVerified {
		return mail.Rule{}, v, ruleVerificationFailure(acct, change.Operation, expected.ID, mail.RuleVerificationCauseMismatch, len(v.Mismatches), outcomeReason)
	}
	if observed.HasError {
		return mail.Rule{}, v, ruleVerificationFailure(acct, change.Operation, expected.ID, mail.RuleVerificationCauseProviderReportedError, 0, outcomeReason)
	}
	return observed, v, nil
}

// verifyRuleDeleted counts the delete as done only when the server says the
// rule is not found. Any other read error leaves the result unconfirmed.
func (h *handlers) verifyRuleDeleted(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, ruleID string, outcomeReason *provider.RuleMutationOutcomeUnknownReason) (mail.RuleVerification, error) {
	var readErr error
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesRead, func(reader provider.RuleReader) error {
		_, readErr = reader.GetRule(ctx, mail.RuleGetRequest{Scope: mail.RuleScopeInbox, RuleID: ruleID})
		return nil
	})
	if err != nil {
		return mail.RuleVerification{}, err
	}
	err = readErr
	if err == nil {
		v := mail.RuleVerification{Status: mail.RuleVerificationFailed, CheckedFields: []string{"absence"}, Mismatches: []mail.RuleVerificationMismatch{{Field: "absence"}}}
		return v, ruleVerificationFailure(acct, mail.RuleOperationDelete, ruleID, mail.RuleVerificationCauseMismatch, 1, outcomeReason)
	}
	if verificationCauseForReadError(err) == mail.RuleVerificationCauseNotFound {
		return mail.RuleVerification{Status: mail.RuleVerificationVerified, CheckedFields: []string{"absence"}}, nil
	}
	return mail.RuleVerification{}, ruleVerificationFailure(acct, mail.RuleOperationDelete, ruleID, verificationCauseForReadError(err), 0, outcomeReason)
}

func rejectReadOnlyRule(acct provider.ResolvedAccount, rule mail.Rule) error {
	if !rule.IsReadOnly {
		return nil
	}
	return errs.NewWithDetails(errs.CodeInvalidRuleInput, "the mail service marks this rule read-only, so maildo cannot change it", map[string]any{
		"field": "rule_id", "reason": "rule_read_only", "rule_id": rule.ID,
		"account": acct.Name, "provider": acct.Provider,
	})
}

func ruleMutationOutcomeReason(err error) (provider.RuleMutationOutcomeUnknownReason, bool) {
	var outcome *provider.RuleMutationOutcomeUnknownError
	if !errors.As(err, &outcome) {
		return "", false
	}
	return outcome.Reason(), true
}

// outcomeReasonCause maps the reason a write is unconfirmed to the shorter
// list of causes that errors report.
func outcomeReasonCause(reason provider.RuleMutationOutcomeUnknownReason) mail.RuleVerificationCause {
	switch reason {
	case provider.RuleMutationOutcomeMissingID, provider.RuleMutationOutcomeMalformed:
		return mail.RuleVerificationCauseMalformedResponse
	case provider.RuleMutationOutcomeOversized:
		return mail.RuleVerificationCauseOversizedResponse
	case provider.RuleMutationOutcomeUnreadable, provider.RuleMutationOutcomeTransportUnknown:
		return mail.RuleVerificationCauseTransport
	default:
		return mail.RuleVerificationCauseProviderAPI
	}
}

// verificationCauseForReadError maps a failed read-back to a cause, by its
// error type and code, never by its text.
func verificationCauseForReadError(err error) mail.RuleVerificationCause {
	var readErr *provider.RuleReadFailureError
	if errors.As(err, &readErr) {
		switch readErr.Reason() {
		case provider.RuleReadFailureNotFound:
			return mail.RuleVerificationCauseNotFound
		case provider.RuleReadFailureAuthRepairRequired:
			return mail.RuleVerificationCauseAuthRepairRequired
		case provider.RuleReadFailureRateLimited:
			return mail.RuleVerificationCauseRateLimited
		case provider.RuleReadFailureMalformedResponse:
			return mail.RuleVerificationCauseMalformedResponse
		case provider.RuleReadFailureOversizedResponse:
			return mail.RuleVerificationCauseOversizedResponse
		case provider.RuleReadFailureTransport:
			return mail.RuleVerificationCauseTransport
		default:
			return mail.RuleVerificationCauseProviderAPI
		}
	}
	var productErr *errs.Error
	if errors.As(err, &productErr) {
		switch productErr.Code() {
		case errs.CodeRuleNotFound:
			return mail.RuleVerificationCauseNotFound
		case errs.CodeAuthRepairRequired:
			return mail.RuleVerificationCauseAuthRepairRequired
		}
	}
	return mail.RuleVerificationCauseProviderAPI
}

// ruleVerificationFailure reports a rule change maildo could not confirm.
// It names the rule and the cause, but copies no rule contents into the
// error.
func ruleVerificationFailure(acct provider.ResolvedAccount, op mail.RuleMutationOperation, ruleID string, cause mail.RuleVerificationCause, mismatchCount int, outcomeReason *provider.RuleMutationOutcomeUnknownReason) error {
	details := map[string]any{
		"account": acct.Name, "provider": acct.Provider, "operation": string(op),
		"verification_status": mail.RuleVerificationFailed, "verification_cause": string(cause),
	}
	if ruleID != "" {
		details["rule_id"] = ruleID
	}
	if cause == mail.RuleVerificationCauseMismatch {
		details["mismatch_count"] = mismatchCount
	}
	if cause == mail.RuleVerificationCauseAuthRepairRequired {
		details["repair_command"] = "maildo auth repair " + acct.Name
	}
	if outcomeReason != nil {
		details["reason"] = string(*outcomeReason)
	}
	message := "maildo could not confirm the rule change, and it may have happened; check the rule with maildo rules view before you try again"
	if op == mail.RuleOperationCreate {
		message = "maildo could not confirm that the rule was created, and it may exist already; check maildo rules list before you try again"
	} else if op == mail.RuleOperationDelete {
		message = "maildo could not confirm that the rule was deleted; check maildo rules list before you try again"
	}
	return errs.NewWithDetails(errs.CodeVerificationFailed, message, details)
}

func ruleDeleteConfirmationMatches(targetID, supplied string) bool {
	return supplied == targetID
}

// ruleConfirmationRequiredError says what --confirm needs without printing
// the rule id, because the id is the confirmation itself.
func ruleConfirmationRequiredError(acct provider.ResolvedAccount) error {
	return errs.NewWithDetails(errs.CodeConfirmationRequired,
		"deleting a rule needs --confirm with the rule's id; run the command with --dry-run to see the rule and the full command",
		map[string]any{
			"account":                  acct.Name,
			"provider":                 acct.Provider,
			"confirmation_required":    true,
			"confirmation_match_shape": "provider_rule_id",
		})
}

// listAnnotatedRules reads the folders first, so a folder error always wins,
// and names each rule's folders from that one list.
func (h *handlers) listAnnotatedRules(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount) ([]mail.Rule, error) {
	var folders mail.FolderList
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		var err error
		folders, err = reader.ListFolders(ctx, folderListRequest(mail.FolderListDetailIdentity))
		return err
	}); err != nil {
		return nil, err
	}
	var list mail.RuleListResult
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesRead, func(reader provider.RuleReader) error {
		var err error
		list, err = reader.ListRules(ctx, mail.RuleListRequest{Scope: mail.RuleScopeInbox})
		return err
	}); err != nil {
		return nil, err
	}
	return rulespkg.AnnotateList(list.Rules, folders.Folders, acct.Email), nil
}

// getAnnotatedRule reads the rule by id, so an unknown id gets the server's
// "not found". It also lists all rules, because the check for two rules in
// the same place needs all of them.
func (h *handlers) getAnnotatedRule(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, ruleID string) (mail.Rule, error) {
	var folders mail.FolderList
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		var err error
		folders, err = reader.ListFolders(ctx, folderListRequest(mail.FolderListDetailIdentity))
		return err
	}); err != nil {
		return mail.Rule{}, err
	}
	var rule mail.Rule
	var list mail.RuleListResult
	if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessRulesRead, func(reader provider.RuleReader) error {
		var err error
		rule, err = reader.GetRule(ctx, mail.RuleGetRequest{Scope: mail.RuleScopeInbox, RuleID: ruleID})
		if err != nil {
			return err
		}
		list, err = reader.ListRules(ctx, mail.RuleListRequest{Scope: mail.RuleScopeInbox})
		return err
	}); err != nil {
		return mail.Rule{}, err
	}
	sequenceCounts := ruleSequenceCountsForView(list.Rules, rule)
	return rulespkg.AnnotateRule(rule, folders.Folders, acct.Email, sequenceCounts), nil
}

func rulesMeta(commandPath string, sel provider.AccountSelection, count int, findingCounts map[string]int) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"rule_scope":   string(mail.RuleScopeInbox),
		"rule_count":   count,
	}
	if findingCounts != nil {
		extra["finding_counts"] = findingCounts
	}
	return output.Meta{Extra: extra}
}

func rulesViewMeta(commandPath string, acct provider.ResolvedAccount, ruleID string) output.Meta {
	return output.Meta{Extra: map[string]any{
		"command_path":      commandPath,
		"scope":             "single",
		"account":           acct.Name,
		"provider":          acct.Provider,
		"accounts":          []string{acct.Name},
		"providers":         []string{acct.Provider},
		"rule_scope":        string(mail.RuleScopeInbox),
		"requested_rule_id": ruleID,
		"rule_count":        1,
	}}
}

// ruleSequenceCounts counts how many rules use each place in the order.
func ruleSequenceCounts(rules []mail.Rule) map[int]int {
	counts := make(map[int]int, len(rules))
	for _, rule := range rules {
		counts[rule.Sequence]++
	}
	return counts
}

// ruleSequenceCountsForView adds the viewed rule when the server's list
// left it out, and does not count it twice when it is there.
func ruleSequenceCountsForView(rules []mail.Rule, viewed mail.Rule) map[int]int {
	counts := ruleSequenceCounts(rules)
	for _, rule := range rules {
		if rule.ID == viewed.ID {
			return counts
		}
	}
	counts[viewed.Sequence]++
	return counts
}
