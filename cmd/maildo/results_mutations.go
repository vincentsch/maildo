package main

import (
	"fmt"
	"io"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad/output"
)

type mutationSelectorResult struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	InternetID string `json:"internet_id,omitempty"`
}

// mutationResult is the JSON result of a change to one message. What the
// provider reports is kept apart in ProviderMeta.
type mutationResult struct {
	provider.AccountSelection
	Operation         string                 `json:"operation"`
	Applied           bool                   `json:"applied"`
	Selector          mutationSelectorResult `json:"selector"`
	MessageID         string                 `json:"message_id,omitempty"`
	InternetMessageID string                 `json:"internet_message_id,omitempty"`
	ResultMessageID   string                 `json:"new_id,omitempty"`
	SourceFolder      *mail.FolderSummary    `json:"source_folder,omitempty"`
	TargetFolder      *mail.FolderSummary    `json:"target_folder,omitempty"`
	ReadState         *mail.BoolChange       `json:"read_state,omitempty"`
	FlagState         *mail.BoolChange       `json:"flag_state,omitempty"`
	Categories        *mail.CategoryChange   `json:"categories,omitempty"`
	Changed           *bool                  `json:"changed,omitempty"`
	*mail.WriteOutcomeFacts
	ProviderMeta    *mail.MutationProviderMetadata `json:"provider_metadata,omitempty"`
	ActionLogStatus mail.ActionLogStatus           `json:"action_log_status,omitempty"`
	// UndoCommands move the message back where it was, after a change
	// that moved it to another folder.
	UndoCommands []string `json:"undo_commands,omitempty"`
}

// categoryListResult is the JSON result of categories list. It picks the
// message like the commands that change messages, but it only reads.
type categoryListResult struct {
	provider.AccountSelection
	Operation         string                         `json:"operation"`
	Applied           bool                           `json:"applied"`
	Selector          mutationSelectorResult         `json:"selector"`
	MessageID         string                         `json:"message_id"`
	InternetMessageID string                         `json:"internet_message_id,omitempty"`
	Categories        []string                       `json:"categories"`
	Count             int                            `json:"count"`
	ProviderMeta      *mail.MutationProviderMetadata `json:"provider_metadata,omitempty"`
}

// bulkMutationResult is the JSON result of a change to several messages that
// all worked. When some fail, the error details have the same fields.
type bulkMutationResult struct {
	provider.AccountSelection
	Operation       string                       `json:"operation"`
	Applied         bool                         `json:"applied"`
	Selector        mail.BulkSelectorSummary     `json:"selector"`
	Counts          mail.BulkMutationCounts      `json:"counts"`
	Targets         []mail.AppliedMutationTarget `json:"targets"`
	ActionLogStatus mail.ActionLogStatus         `json:"action_log_status"`
	// UndoCommands move the messages back where they were, one command
	// for each folder they came from.
	UndoCommands []string `json:"undo_commands,omitempty"`
}

// mutationResultFromProvider combines what maildo knows about the message
// with the provider's result, which may leave out the Message-ID.
func mutationResultFromProvider(target mutationTarget, op mail.MessageMutationOperation, applied bool, result mail.MessageMutationResult) mutationResult {
	messageID := result.MessageID
	if messageID == "" {
		messageID = target.MessageID
	}
	internetID := result.InternetMessageID
	if internetID == "" {
		internetID = target.InternetMessageID
	}
	return mutationResult{
		AccountSelection:  target.Selection,
		Operation:         string(op),
		Applied:           applied,
		Selector:          selectorResult(target),
		MessageID:         messageID,
		InternetMessageID: internetID,
		ResultMessageID:   result.ResultMessageID,
		SourceFolder:      result.SourceFolder,
		TargetFolder:      result.TargetFolder,
		ReadState:         result.ReadState,
		FlagState:         result.FlagState,
		Categories:        result.Categories,
		Changed:           cloneBoolPtr(result.Changed),
		WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(result.WriteOutcomeFacts),
		ProviderMeta:      cloneProviderMetadataForResult(result.ProviderMeta),
	}
}

// categoryListResultFromProvider builds the result like
// mutationResultFromProvider and keeps the provider's order of categories.
func categoryListResultFromProvider(target mutationTarget, result mail.MessageCategoryListResult) categoryListResult {
	messageID := result.MessageID
	if messageID == "" {
		messageID = target.MessageID
	}
	internetID := result.InternetMessageID
	if internetID == "" {
		internetID = target.InternetMessageID
	}
	return categoryListResult{
		AccountSelection:  target.Selection,
		Operation:         string(mail.MutationCategoriesList),
		Applied:           false,
		Selector:          selectorResult(target),
		MessageID:         messageID,
		InternetMessageID: internetID,
		Categories:        append([]string(nil), result.Categories...),
		Count:             result.Count,
		ProviderMeta:      cloneProviderMetadataForResult(result.ProviderMeta),
	}
}

// selectorResult records how the message was picked (an id, a Message-ID or
// a search), but not the search text.
func selectorResult(target mutationTarget) mutationSelectorResult {
	out := mutationSelectorResult{Kind: target.SelectorKind}
	switch target.Selector.Kind {
	case mail.SelectorID:
		out.ID = target.Selector.ID
	case mail.SelectorInternetID:
		out.InternetID = target.Selector.InternetID
	}
	return out
}

func mutationMeta(commandPath string, r mutationResult) output.Meta {
	accounts, providers := accountProviderNames(r.AccountSelection)
	extra := map[string]any{
		"command_path":  commandPath,
		"scope":         r.Scope,
		"accounts":      accounts,
		"providers":     providers,
		"operation":     r.Operation,
		"applied":       r.Applied,
		"selector_kind": r.Selector.Kind,
	}
	if r.MessageID != "" {
		extra["message_id"] = r.MessageID
	}
	if r.ResultMessageID != "" {
		extra["new_id"] = r.ResultMessageID
	}
	if r.TargetFolder != nil && r.TargetFolder.ID != "" {
		extra["target_folder_id"] = r.TargetFolder.ID
	}
	if r.Categories != nil {
		extra["category_count"] = r.Categories.Count
		extra["removed_category_count"] = len(r.Categories.Removed)
		extra["skipped_category_count"] = len(r.Categories.Skipped)
	}
	if r.ActionLogStatus.RelativePath != "" {
		extra["action_log_written"] = r.ActionLogStatus.Written
		extra["action_log_entry_count"] = r.ActionLogStatus.EntryCount
		extra["action_log_relative_path"] = r.ActionLogStatus.RelativePath
	}
	return output.Meta{Extra: extra}
}

func bulkMutationMeta(commandPath string, r bulkMutationResult) output.Meta {
	accounts, providers := accountProviderNames(r.AccountSelection)
	return output.Meta{Extra: map[string]any{
		"command_path":             commandPath,
		"scope":                    r.Scope,
		"accounts":                 accounts,
		"providers":                providers,
		"operation":                r.Operation,
		"applied":                  r.Applied,
		"selector_kind":            r.Selector.Kind,
		"target_count":             r.Counts.Total,
		"succeeded_count":          r.Counts.Succeeded,
		"failed_count":             r.Counts.Failed,
		"skipped_count":            r.Counts.Skipped,
		"action_log_written":       r.ActionLogStatus.Written,
		"action_log_entry_count":   r.ActionLogStatus.EntryCount,
		"action_log_relative_path": r.ActionLogStatus.RelativePath,
	}}
}

func categoryListMeta(commandPath string, r categoryListResult) output.Meta {
	accounts, providers := accountProviderNames(r.AccountSelection)
	return output.Meta{Extra: map[string]any{
		"command_path":   commandPath,
		"scope":          r.Scope,
		"accounts":       accounts,
		"providers":      providers,
		"operation":      r.Operation,
		"applied":        false,
		"selector_kind":  r.Selector.Kind,
		"message_id":     r.MessageID,
		"category_count": r.Count,
	}}
}

// humanCategoryList names the message and lists its categories, one per
// line. label is the message as targetLabel names it.
func humanCategoryList(r categoryListResult, label string) func(io.Writer) {
	return func(w io.Writer) {
		label := upperFirst(label) + accountSuffix(r.AccountSelection)
		if len(r.Categories) == 0 {
			fmt.Fprintf(w, "%s has no categories.\n", label)
			return
		}
		fmt.Fprintf(w, "%s has %s:\n", label, pluralize(len(r.Categories), "category", "categories"))
		for _, category := range r.Categories {
			fmt.Fprintf(w, "  %s\n", plainField(category))
		}
	}
}

// bulkPartialFailureError is a change to several messages where some failed.
// Nothing goes to stdout; the JSON error has the outcome of every message.
type bulkPartialFailureError struct {
	operation         string
	selector          mail.BulkSelectorSummary
	counts            mail.BulkMutationCounts
	targets           []mail.AppliedMutationTarget
	status            mail.ActionLogStatus
	mutationAttempted bool
	mutationApplied   bool
	exit              int
	labels            map[string]string // id to "Subject" from Sender, for people
	undo              []string          // moves the messages that did change back
}

func (e bulkPartialFailureError) Error() string {
	return "bulk mutation partially failed"
}

func (e bulkPartialFailureError) ExitCode() int {
	if e.exit != 0 {
		return e.exit
	}
	return 1
}

func (e bulkPartialFailureError) ErrorDetails() any {
	details := map[string]any{
		"code":               errs.CodeBulkPartialFailure,
		"operation":          e.operation,
		"selector":           e.selector,
		"counts":             e.counts,
		"targets":            e.targets,
		"action_log_status":  e.status,
		"partial_failure":    true,
		"mutation_attempted": e.mutationAttempted,
		"mutation_applied":   e.mutationApplied,
	}
	if code := firstTargetErrorCode(e.targets); code != "" {
		details["underlying_error_code"] = code
	}
	if len(e.undo) > 0 {
		details["undo_commands"] = e.undo
	}
	return details
}

// actionLogWriteFailedError wins over the errors of single messages: when the
// action log could not be written, the command must not report success. It
// still lists each message, so the caller can see what was tried.
type actionLogWriteFailedError struct {
	operation         string
	selector          mail.BulkSelectorSummary
	counts            mail.BulkMutationCounts
	targets           []mail.AppliedMutationTarget
	status            mail.ActionLogStatus
	mutationAttempted bool
	mutationApplied   bool
}

func (e actionLogWriteFailedError) Error() string {
	return "action log write failed after mutation attempts"
}

func (e actionLogWriteFailedError) ExitCode() int {
	return 1
}

func (e actionLogWriteFailedError) ErrorDetails() any {
	details := map[string]any{
		"code":               errs.CodeActionLogWriteFailed,
		"operation":          e.operation,
		"selector":           e.selector,
		"counts":             e.counts,
		"targets":            e.targets,
		"action_log_status":  e.status,
		"mutation_attempted": e.mutationAttempted,
		"mutation_applied":   e.mutationApplied,
	}
	if hasPartialMutationOutcome(e.counts) {
		details["partial_failure"] = true
	}
	if code := firstTargetErrorCode(e.targets); code != "" {
		details["underlying_error_code"] = code
	}
	return details
}

// hasPartialMutationOutcome reports whether some messages had already failed
// or were not tried when the action log could not be written.
func hasPartialMutationOutcome(counts mail.BulkMutationCounts) bool {
	return counts.Failed > 0 || counts.Skipped > 0
}

// firstTargetErrorCode is the error code of the first message that failed.
// It goes into the details, next to the error's own code.
func firstTargetErrorCode(targets []mail.AppliedMutationTarget) string {
	for _, target := range targets {
		if target.ErrorCode != "" {
			return target.ErrorCode
		}
	}
	return ""
}
