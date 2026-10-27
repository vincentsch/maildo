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

// forwardResult never has the body. The recipients, the subject and the
// attachments come from reading the draft back after maildo added your
// text.
type forwardResult struct {
	provider.AccountSelection
	SourceID                   string                     `json:"source_id,omitempty"`
	SourceInternetID           string                     `json:"source_internet_id,omitempty"`
	ActionMode                 string                     `json:"action_mode"`
	DraftID                    string                     `json:"draft_id,omitempty"`
	SentID                     string                     `json:"sent_id,omitempty"`
	To                         []mail.Recipient           `json:"to,omitempty"`
	CC                         []mail.Recipient           `json:"cc,omitempty"`
	BCC                        []mail.Recipient           `json:"bcc,omitempty"`
	Subject                    string                     `json:"subject"`
	BodyFormat                 mail.BodyFormat            `json:"body_format,omitempty"`
	AttachmentMode             mail.ForwardAttachmentMode `json:"attachment_mode"`
	SelectedSourceAttachments  []mail.Attachment          `json:"selected_source_attachments,omitempty"`
	FinalAttachments           []mail.Attachment          `json:"final_attachments,omitempty"`
	UnsupportedSourceExclusion []mail.AttachmentExclusion `json:"excluded_unsupported,omitempty"`
	AttachmentCount            int                        `json:"attachment_count"`
	*mail.WriteOutcomeFacts
	ProviderMeta *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy     string                      `json:"sent_copy,omitempty"`
	Warnings     []string                    `json:"warnings,omitempty"`
	// ActionLogStatus is set for forwards that were sent.
	ActionLogStatus mail.ActionLogStatus `json:"action_log_status,omitempty"`
}

func humanForward(f *rungrad.Factory, r forwardResult) func(io.Writer) {
	acct := ""
	if len(r.Accounts) > 0 {
		acct = r.Accounts[0].Name
	}
	if r.ActionMode != string(mail.ComposeExecutionSend) && r.DraftID != "" {
		return humanDraftSaved(f, acct, r.DraftID, r.Subject, append(append([]mail.Recipient(nil), r.To...), r.CC...), canSend(r.AccountSelection))
	}
	msg := outgoing{To: r.To, CC: r.CC, BCC: r.BCC, Subject: r.Subject, recipientsKnown: true}
	return humanSent(acct, msg, r.SentCopy)
}

func forwardMeta(commandPath string, sel provider.AccountSelection, selector mail.MessageSelector, plan composepkg.ForwardPlan, result forwardResult) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path":           commandPath,
		"scope":                  sel.Scope,
		"accounts":               accounts,
		"providers":              providers,
		"selector_kind":          string(selector.Kind),
		"action_mode":            string(plan.Action),
		"draft_id":               result.DraftID,
		"attachment_mode":        string(plan.Attachments.Mode),
		"attachment_count":       result.AttachmentCount,
		"selected_count":         len(result.SelectedSourceAttachments),
		"unsupported_count":      len(result.UnsupportedSourceExclusion),
		"final_attachment_count": len(result.FinalAttachments),
	}}
}

func forwardDraftError(err error, draftID string) error {
	if err == nil || draftID == "" {
		return err
	}
	if _, ok := errs.WriteOutcomeFactsFromError(err); ok {
		// Keep the outcome facts on top, and still name the draft.
		return errs.WithDetails(err, map[string]any{"draft_id": draftID, "draft_created": true})
	}
	return forwardDraftCreatedError{err: err, draftID: draftID}
}

// forwardDraftCreatedError keeps the original error and adds the id of the
// draft that was already created, so the user can find it.
type forwardDraftCreatedError struct {
	err     error
	draftID string
}

func (e forwardDraftCreatedError) Error() string {
	return e.err.Error() + fmt.Sprintf(" (forward draft %s was created; inspect or remove it manually)", e.draftID)
}

func (e forwardDraftCreatedError) ExitCode() int {
	if c, ok := e.err.(interface{ ExitCode() int }); ok {
		return c.ExitCode()
	}
	return 1
}

func (e forwardDraftCreatedError) ErrorDetails() any {
	details := map[string]any{"draft_id": e.draftID, "draft_created": true}
	if d, ok := e.err.(interface{ ErrorDetails() any }); ok {
		if m, ok := d.ErrorDetails().(map[string]any); ok {
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
