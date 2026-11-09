package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	composepkg "github.com/vincentsch/maildo/internal/compose"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// draftWriteResult is the JSON result of draft create, update and send. It
// never has the body.
type draftWriteResult struct {
	provider.AccountSelection
	Operation       string               `json:"operation"`
	DraftID         string               `json:"draft_id,omitempty"`
	MessageID       string               `json:"message_id,omitempty"`
	RequestedFrom   string               `json:"requested_from,omitempty"`
	To              []mail.Recipient     `json:"to,omitempty"`
	CC              []mail.Recipient     `json:"cc,omitempty"`
	BCC             []mail.Recipient     `json:"bcc,omitempty"`
	ReplyTo         []mail.Recipient     `json:"reply_to,omitempty"`
	Subject         string               `json:"subject"`
	BodyFormat      mail.BodyFormat      `json:"body_format,omitempty"`
	AttachmentCount int                  `json:"attachment_count"`
	ActionLogStatus mail.ActionLogStatus `json:"action_log_status,omitempty"`
	*mail.WriteOutcomeFacts
	ProviderMeta *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
	Warnings     []string                    `json:"warnings,omitempty"`
	// previousID and subjectSet let the human view of an update say
	// whether the id changed and name the draft only when the subject is
	// known.
	previousID string
	subjectSet bool
}

// draftReadResult has the body, because draft read is how you ask for it.
type draftReadResult struct {
	provider.AccountSelection
	Draft mail.Draft `json:"draft"`
}

// directSendResult is the JSON result of maildo send. It has the recipients
// and the subject, but never the body, file paths or the raw message.
type directSendResult struct {
	Account   string                   `json:"account"`
	Provider  string                   `json:"provider"`
	Operation string                   `json:"operation"`
	MessageID string                   `json:"message_id,omitempty"`
	To        []mail.Recipient         `json:"to"`
	CC        []mail.Recipient         `json:"cc,omitempty"`
	BCC       []mail.Recipient         `json:"bcc,omitempty"`
	Subject   string                   `json:"subject"`
	Summary   mail.PreparedSendSummary `json:"summary"`
	*mail.WriteOutcomeFacts
	ProviderMeta    *mail.SMTPDirectSendProviderMetadata `json:"provider_metadata,omitempty"`
	SentCopy        string                               `json:"sent_copy,omitempty"`
	Warnings        []string                             `json:"warnings,omitempty"`
	ActionLogStatus mail.ActionLogStatus                 `json:"action_log_status"`
}

// composeMeta is the --include-meta data for compose commands: which command
// and account, but no message content.
func composeMeta(commandPath string, sel provider.AccountSelection, operation, draftID, messageID string) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"operation":    operation,
	}
	if draftID != "" {
		extra["draft_id"] = draftID
	}
	if messageID != "" {
		extra["message_id"] = messageID
	}
	return output.Meta{Extra: extra}
}

// directSendMeta is the --include-meta data for maildo send.
func directSendMeta(commandPath string, sel provider.AccountSelection, messageID string) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"operation":    "send",
	}
	if messageID != "" {
		extra["message_id"] = messageID
	}
	return output.Meta{Extra: extra}
}

// draftReadMeta records the --body-mode that was used, without the body.
func draftReadMeta(commandPath string, sel provider.AccountSelection, draftID string, bodyMode mail.BodyMode) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"draft_id":     draftID,
		"body_mode":    string(bodyMode),
	}}
}

// humanDraftWrite prints one short line for a saved draft, without the
// body.
func humanDraftWrite(f *rungrad.Factory, r draftWriteResult) func(io.Writer) {
	acct := ""
	if len(r.Accounts) > 0 {
		acct = r.Accounts[0].Name
	}
	if r.Operation == "draft_update" {
		return humanDraftUpdated(f, acct, r)
	}
	return humanDraftSaved(f, acct, r.DraftID, r.Subject, append(append([]mail.Recipient(nil), r.To...), r.CC...), canSend(r.AccountSelection))
}

// humanDraftUpdated says a draft was updated. An update that kept the
// subject does not know it, so the draft is then named by its id.
func humanDraftUpdated(f *rungrad.Factory, acct string, r draftWriteResult) func(io.Writer) {
	short := ""
	if r.DraftID != "" {
		short = shortID(acct, r.DraftID)
		rememberShortIDs(f, []shortIDEntry{messageRef(acct, r.DraftID)})
	}
	return func(w io.Writer) {
		name := "the draft"
		switch {
		case r.subjectSet:
			name += " " + subjectText(r.Subject)
		case r.previousID != "":
			name += " " + shortID(acct, r.previousID)
		}
		fmt.Fprintf(w, "Updated %s in %s.\n", name, plainField(acct))
		if short == "" {
			return
		}
		next := fmt.Sprintf("Send it with: maildo draft send %s", short)
		if !canSend(r.AccountSelection) {
			next = fmt.Sprintf("Read it with: maildo draft read %s", short)
		}
		if r.previousID != "" && r.previousID != r.DraftID {
			fmt.Fprintf(w, "Its id is now %s. %s\n", short, next)
			return
		}
		fmt.Fprintln(w, next)
	}
}

// humanDraftPreview shows a draft that --dry-run did not save.
// bodyStart is the start of a body on one line, for the draft update
// preview.
func bodyStart(text, html string) string {
	body := strings.Join(strings.Fields(readableBody(text, html)), " ")
	if len([]rune(body)) > 200 {
		body = string([]rune(body)[:200]) + "..."
	}
	return body
}

func humanDraftPreview(acct provider.ResolvedAccount, msg outgoing, command string) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Would save this draft in %s:\n\n", plainField(acct.Name))
		writeOutgoing(w, msg, true)
		fmt.Fprintln(w)
		writeSenderNote(w, acct, msg)
		fmt.Fprintln(w, "Nothing was saved. To save it, run:")
		writeCommand(w, command, commandWithoutDry)
	}
}

// humanDraftUpdatePreview shows what a draft update would change.
func humanDraftUpdatePreview(acct provider.ResolvedAccount, draftID string, p mail.DraftPatch, command string) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Would change the draft %s in %s:\n\n", shortID(acct.Name, draftID), plainField(acct.Name))
		field := func(label, value string) {
			fmt.Fprintf(w, "  %-9s %s\n", label+":", value)
		}
		recipients := func(label string, patch mail.RecipientPatch) {
			switch {
			case patch.Clear:
				field(label, "(removed)")
			case patch.Set:
				field(label, recipientsText(patch.Values))
			}
		}
		recipients("To", p.To)
		recipients("Cc", p.CC)
		recipients("Bcc", p.BCC)
		recipients("Reply-To", p.ReplyTo)
		if p.Subject.Set {
			field("Subject", firstNonEmpty(plainField(p.Subject.Value), "(none)"))
		}
		if p.Body.Set {
			field("Body", firstNonEmpty(plainField(bodyStart(p.Body.Body.Text, p.Body.Body.HTML)), "(empty)"))
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The rest of the draft would stay the same.")
		fmt.Fprintln(w, "Nothing was changed. To update the draft, run:")
		writeCommand(w, command, commandWithoutDry)
	}
}

// humanDraftSaved says a draft was saved, under which short id, and how to
// send it.
func humanDraftSaved(f *rungrad.Factory, acct, draftID, subject string, to []mail.Recipient, sendable bool) func(io.Writer) {
	short := ""
	if draftID != "" {
		short = shortID(acct, draftID)
		rememberShortIDs(f, []shortIDEntry{messageRef(acct, draftID)})
	}
	return func(w io.Writer) {
		fmt.Fprintf(w, "Saved the draft %s", subjectText(subject))
		if len(to) > 0 {
			fmt.Fprintf(w, " to %s", recipientsText(to))
		}
		fmt.Fprintf(w, " in %s.\n", plainField(acct))
		switch {
		case short == "":
		case sendable:
			fmt.Fprintf(w, "Draft id: %s. Send it with: maildo draft send %s\n", short, short)
		default:
			fmt.Fprintf(w, "Draft id: %s. Read it with: maildo draft read %s\n", short, short)
		}
	}
}

// canSend reports whether the account's policy lets maildo send, so
// results suggest a send command only when it would work.
func canSend(sel provider.AccountSelection) bool {
	if len(sel.Accounts) == 0 {
		return true
	}
	p := sel.Accounts[0].Policy
	return !p.DraftsOnly && !p.ReadOnly
}

// humanDraftRead shows a draft the way read shows a message: the fields a
// person checks before sending, then the body.
func humanDraftRead(f *rungrad.Factory, r draftReadResult) func(io.Writer) {
	d := r.Draft
	ids := rememberShortIDs(f, []shortIDEntry{messageRefWithSubject(d.Account, d.ID, d.Subject)})
	return func(w io.Writer) {
		field := func(label, value string) {
			if value != "" {
				fmt.Fprintf(w, "%-9s %s\n", label+":", value)
			}
		}
		field("Subject", firstNonEmpty(strings.TrimSpace(plainField(d.Subject)), "(no subject)"))
		if d.From.Address != "" {
			field("From", recipientText(d.From))
		}
		field("To", recipientsText(d.To))
		field("Cc", recipientsText(d.CC))
		field("Bcc", recipientsText(d.BCC))
		field("Reply-To", recipientsText(d.ReplyTo))
		if t := firstTime(d.LastModifiedAt, d.CreatedAt); t != nil {
			field("Saved", t.Local().Format("Mon, 2 Jan 2006 15:04")+" ("+relativeTime(*t)+")")
		}
		field("Folder", plainField(folderSummaryText(d.Folder))+" ("+plainField(d.Account)+")")
		field("ID", ids.of(d.Account, d.ID))
		if len(d.Attachments) > 0 {
			fmt.Fprintln(w, "Attachments:")
			t := newTable()
			t.indent = "  "
			for _, a := range d.Attachments {
				t.add(firstNonEmpty(a.Name, "(no name)"), attachmentSize(a))
			}
			t.render(w, 0, nil)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimRight(d.Body.Content, "\n"))
	}
}

func plainDraftBody(d mail.Draft) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintln(w, d.Body.Content)
	}
}

// previewCompose builds the --dry-run JSON from the summary only. It says
// where the body came from and how long it is, but never shows the body or a
// file path.
func previewCompose(method string, acct provider.ResolvedAccount, summary composepkg.Summary) output.DryRunPreview {
	fields := []output.Field{
		{Name: "account", Value: acct.Name},
		{Name: "provider", Value: acct.Provider},
		{Name: "to_count", Value: strconv.Itoa(summary.ToCount)},
		{Name: "cc_count", Value: strconv.Itoa(summary.CCCount)},
		{Name: "bcc_count", Value: strconv.Itoa(summary.BCCCount)},
		{Name: "reply_to_count", Value: strconv.Itoa(summary.ReplyToCount)},
		{Name: "subject_present", Value: strconv.FormatBool(summary.SubjectSet)},
		{Name: "body_present", Value: strconv.FormatBool(summary.BodyPresent)},
	}
	if summary.BodySource != "" {
		fields = append(fields,
			output.Field{Name: "body_format", Value: string(summary.BodyFormat)},
			output.Field{Name: "body_source", Value: string(summary.BodySource)},
			output.Field{Name: "body_bytes", Value: strconv.Itoa(summary.BodyBytes)},
		)
	}
	if summary.RequestedFrom != "" {
		fields = append(fields, output.Field{Name: "requested_from", Value: summary.RequestedFrom})
	}
	return output.DryRunPreview{Method: method, Path: acct.Name, Body: fields}
}

// previewDraftUpdate shows the requested changes, not the draft as it is
// now.
func previewDraftUpdate(acct provider.ResolvedAccount, req mail.UpdateDraftRequest, summary composepkg.Summary, validation mail.DraftUpdateValidation) output.DryRunPreview {
	fields := []output.Field{
		{Name: "account", Value: acct.Name},
		{Name: "provider", Value: acct.Provider},
		{Name: "draft_id", Value: req.DraftID},
		{Name: "to", Value: recipientPatchChange(req.Patch.To)},
		{Name: "cc", Value: recipientPatchChange(req.Patch.CC)},
		{Name: "bcc", Value: recipientPatchChange(req.Patch.BCC)},
		{Name: "reply_to", Value: recipientPatchChange(req.Patch.ReplyTo)},
		{Name: "subject_change", Value: subjectPatchChange(req.Patch.Subject)},
	}
	if validation.ThreadTransition != "" {
		fields = append(fields, output.Field{Name: "thread_transition", Value: validation.ThreadTransition})
	}
	fields = append(fields, output.Field{Name: "body_change", Value: bodyPatchChange(req.Patch.Body)})
	if req.Patch.Body.Set {
		fields = append(fields,
			output.Field{Name: "body_format", Value: string(summary.BodyFormat)},
			output.Field{Name: "body_source", Value: string(summary.BodySource)},
			output.Field{Name: "body_bytes", Value: strconv.Itoa(summary.BodyBytes)},
		)
	}
	if summary.RequestedFrom != "" {
		fields = append(fields, output.Field{Name: "requested_from", Value: summary.RequestedFrom})
	}
	return output.DryRunPreview{Method: "draft-update", Path: acct.Name, Body: fields}
}

func recipientPatchChange(p mail.RecipientPatch) string {
	switch {
	case p.Clear:
		return "clear"
	case p.Set:
		return "replace"
	default:
		return "unchanged"
	}
}

func subjectPatchChange(p mail.SubjectPatch) string {
	if !p.Set {
		return "unchanged"
	}
	if p.Value == "" {
		return "clear"
	}
	return "replace"
}

func bodyPatchChange(p mail.BodyPatch) string {
	if !p.Set {
		return "unchanged"
	}
	if !p.Body.Present {
		return "clear"
	}
	return "replace"
}
