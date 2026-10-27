package main

import (
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// logChange writes a change to a draft, folder or rule to the action log.
// A change that failed is logged too, since it may still have reached the
// server. The change has happened by the time this runs, so a log that
// cannot be written becomes a warning and a status in the result, not an
// error.
func (h *handlers) logChange(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, operation string, change audit.Change, changeErr error) mail.ActionLogStatus {
	status := mail.ActionLogStatus{RelativePath: audit.RelativePath}
	change.Account = acct.Name
	change.Provider = acct.Provider
	change.Outcome = "done"
	if changeErr != nil {
		change.Outcome = "failed"
		change.ErrorCode = productErrorCode(changeErr)
	}
	entry := audit.Entry{
		Version:   1,
		CreatedAt: h.deps.Now().UTC(),
		Command:   actionLogCommandPath(cmd),
		Operation: mail.MessageMutationOperation(operation),
		Change:    &change,
	}
	if err := audit.Append(filepath.Dir(f.ConfigPath()), entry); err != nil {
		status.Reason = "append_failed"
		status.ErrorCode = errs.CodeActionLogWriteFailed
		f.Infof("Warning: the change was not written to the action log: %v", err)
		return status
	}
	status.Written = true
	status.EntryCount = 1
	return status
}
