package main

import (
	"fmt"
	"io"

	composepkg "github.com/vincentsch/maildo/internal/compose"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// replyResult never has the body. The recipients and the subject come from
// reading back the reply draft the provider created.
type replyResult struct {
	provider.AccountSelection
	SourceID         string             `json:"source_id,omitempty"`
	SourceInternetID string             `json:"source_internet_id,omitempty"`
	ReplyMode        mail.ReplyMode     `json:"reply_mode"`
	ActionMode       string             `json:"action_mode"`
	QuoteMode        mail.QuoteMode     `json:"quote_mode"`
	QuoteLanguage    mail.QuoteLanguage `json:"quote_language"`
	DraftID          string             `json:"draft_id,omitempty"`
	SentID           string             `json:"sent_id,omitempty"`
	To               []mail.Recipient   `json:"to,omitempty"`
	CC               []mail.Recipient   `json:"cc,omitempty"`
	BCC              []mail.Recipient   `json:"bcc,omitempty"`
	Subject          string             `json:"subject"`
	AttachmentCount  int                `json:"attachment_count"`
	*mail.WriteOutcomeFacts
	ProviderMeta *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy     string                      `json:"sent_copy,omitempty"`
	Warnings     []string                    `json:"warnings,omitempty"`
	// ActionLogStatus is set for replies that were sent.
	ActionLogStatus mail.ActionLogStatus `json:"action_log_status,omitempty"`
}

func humanReply(f *rungrad.Factory, r replyResult) func(io.Writer) {
	acct := ""
	if len(r.Accounts) > 0 {
		acct = r.Accounts[0].Name
	}
	if r.ActionMode != string(mail.ComposeExecutionSend) && r.DraftID != "" {
		return humanDraftSaved(f, acct, r.DraftID, r.Subject, append(append([]mail.Recipient(nil), r.To...), r.CC...), canSend(r.AccountSelection))
	}
	msg := outgoing{To: r.To, CC: r.CC, BCC: r.BCC, Subject: r.Subject, recipientsKnown: len(r.To)+len(r.CC) > 0}
	return humanSent(acct, msg, r.SentCopy)
}

func replyMeta(commandPath string, sel provider.AccountSelection, selector mail.MessageSelector, plan composepkg.ReplyPlan) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path":  commandPath,
		"scope":         sel.Scope,
		"accounts":      accounts,
		"providers":     providers,
		"selector_kind": string(selector.Kind),
		"reply_mode":    string(plan.Mode),
		"quote_mode":    string(plan.Quote),
		"action_mode":   string(plan.Action),
	}}
}

func replyDraftError(err error, draftID string) error {
	if err == nil || draftID == "" {
		return err
	}
	if _, ok := errs.WriteOutcomeFactsFromError(err); ok {
		// Keep the outcome facts on top, and still name the draft.
		return errs.WithDetails(err, map[string]any{"draft_id": draftID, "draft_created": true})
	}
	return replyDraftCreatedError{err: err, draftID: draftID}
}

// replyDraftCreatedError keeps the original error with its exit code and
// details, and adds the id of the reply draft that was already created. Then
// the user can find the draft when a later step failed.
type replyDraftCreatedError struct {
	err     error
	draftID string
}

func (e replyDraftCreatedError) Error() string {
	return e.err.Error() + fmt.Sprintf(" (reply draft %s was created; inspect or remove it manually)", e.draftID)
}

func (e replyDraftCreatedError) ExitCode() int {
	if c, ok := e.err.(interface{ ExitCode() int }); ok {
		return c.ExitCode()
	}
	return 1
}

func (e replyDraftCreatedError) ErrorDetails() any {
	details := map[string]any{"draft_id": e.draftID, "draft_created": true}
	if d, ok := e.err.(interface{ ErrorDetails() any }); ok {
		if m, ok := d.ErrorDetails().(map[string]any); ok {
			// Keep the provider's error details, so the caller can still tell
			// what kind of error it was.
			for k, v := range m {
				if k == "code" && v == "" {
					continue
				}
				details[k] = v
			}
		}
	}
	return details
}
