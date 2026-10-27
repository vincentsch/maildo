package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// draftAttachmentsResult lists the attachments a draft has now, without
// their contents.
type draftAttachmentsResult struct {
	provider.AccountSelection
	Operation    string                      `json:"operation"`
	DraftID      string                      `json:"draft_id"`
	Attachments  []mail.Attachment           `json:"attachments"`
	Count        int                         `json:"count"`
	ProviderMeta *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
}

// draftAttachmentAddResult is the result of adding files to a draft: the
// files added, without their contents, and the draft's attachments after it.
type draftAttachmentAddResult struct {
	provider.AccountSelection
	Operation        string                  `json:"operation"`
	DraftID          string                  `json:"draft_id"`
	Added            []mail.Attachment       `json:"added_attachments,omitempty"`
	FinalAttachments []mail.Attachment       `json:"final_attachments,omitempty"`
	AttachmentCount  int                     `json:"attachment_count"`
	Uploads          []mail.AttachmentUpload `json:"uploads,omitempty"`
	*mail.WriteOutcomeFacts
	ProviderMeta    *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
	Warnings        []string                    `json:"warnings,omitempty"`
	ActionLogStatus mail.ActionLogStatus        `json:"action_log_status"`
}

// draftAttachmentRemoveResult is the result of removing an attachment: the
// attachment removed and the draft's attachments after it.
type draftAttachmentRemoveResult struct {
	provider.AccountSelection
	Operation        string            `json:"operation"`
	DraftID          string            `json:"draft_id"`
	Removed          mail.Attachment   `json:"removed_attachment"`
	FinalAttachments []mail.Attachment `json:"final_attachments,omitempty"`
	AttachmentCount  int               `json:"attachment_count"`
	*mail.WriteOutcomeFacts
	ProviderMeta    *mail.DraftProviderMetadata `json:"provider_metadata,omitempty"`
	Warnings        []string                    `json:"warnings,omitempty"`
	ActionLogStatus mail.ActionLogStatus        `json:"action_log_status"`
}

type messageAttachmentsResult struct {
	provider.AccountSelection
	Operation        string                           `json:"operation"`
	SourceID         string                           `json:"source_message_id"`
	SourceInternetID string                           `json:"source_internet_message_id,omitempty"`
	SelectorKind     string                           `json:"selector_kind"`
	Attachments      []mail.Attachment                `json:"attachments"`
	Count            int                              `json:"count"`
	ProviderMeta     *mail.AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
}

// messageAttachmentDownloadResult is the result of saving a message's
// attachments. Only rows with status `downloaded` have a local path.
type messageAttachmentDownloadResult struct {
	provider.AccountSelection
	Operation        string                           `json:"operation"`
	SourceID         string                           `json:"source_message_id"`
	SourceInternetID string                           `json:"source_internet_message_id,omitempty"`
	SelectorKind     string                           `json:"selector_kind"`
	SelectionKind    string                           `json:"selection_kind"`
	OutputDirectory  string                           `json:"output_directory"`
	SelectedCount    int                              `json:"selected_count"`
	DownloadedCount  int                              `json:"downloaded_count"`
	SkippedCount     int                              `json:"skipped_count"`
	FailedCount      int                              `json:"failed_count"`
	Attachments      []attachmentDownloadRow          `json:"attachments"`
	ProviderMeta     *mail.AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
}

// attachmentDownloadRow is one picked attachment and what happened to it.
// With --all, attachments that are not files are listed too, with the reason
// they were left out.
type attachmentDownloadRow struct {
	ID               string                           `json:"id"`
	Name             string                           `json:"name,omitempty"`
	ContentType      string                           `json:"content_type,omitempty"`
	Size             int64                            `json:"size,omitempty"`
	IsInline         bool                             `json:"is_inline"`
	Kind             mail.AttachmentKind              `json:"kind,omitempty"`
	ProviderMeta     *mail.AttachmentProviderMetadata `json:"provider_metadata,omitempty"`
	EncodedSize      *uint64                          `json:"encoded_size,omitempty"`
	EncodedSizeKnown *bool                            `json:"encoded_size_known,omitempty"`
	DecodedSize      *uint64                          `json:"decoded_size,omitempty"`
	DecodedSizeKnown *bool                            `json:"decoded_size_known,omitempty"`
	SafeBasename     string                           `json:"safe_basename,omitempty"`
	LocalPath        string                           `json:"local_path,omitempty"`
	// SavedSize is the size of the saved file. Size is what the server
	// said before the download, which can differ a little.
	SavedSize int64  `json:"saved_size,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

func attachmentsMeta(commandPath string, sel provider.AccountSelection, operation, draftID string, count int) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path":     commandPath,
		"scope":            sel.Scope,
		"accounts":         accounts,
		"providers":        providers,
		"operation":        operation,
		"draft_id":         draftID,
		"attachment_count": count,
	}}
}

func messageAttachmentsMeta(commandPath string, sel provider.AccountSelection, operation string, selector mail.MessageSelector, messageID string, count int, searched *provider.AccountSelection) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path":      commandPath,
		"scope":             sel.Scope,
		"accounts":          accounts,
		"providers":         providers,
		"operation":         operation,
		"selector_kind":     string(selector.Kind),
		"source_message_id": messageID,
		"attachment_count":  count,
	}
	addSearchedMeta(extra, searched)
	return output.Meta{Extra: extra}
}

func messageAttachmentDownloadMeta(commandPath string, r messageAttachmentDownloadResult) output.Meta {
	accounts, providers := accountProviderNames(r.AccountSelection)
	return output.Meta{Extra: map[string]any{
		"command_path":      commandPath,
		"scope":             r.Scope,
		"accounts":          accounts,
		"providers":         providers,
		"operation":         r.Operation,
		"selector_kind":     r.SelectorKind,
		"selection_kind":    r.SelectionKind,
		"source_message_id": r.SourceID,
		"selected_count":    r.SelectedCount,
		"downloaded_count":  r.DownloadedCount,
		"skipped_count":     r.SkippedCount,
		"failed_count":      r.FailedCount,
	}}
}

func humanDraftAttachments(r draftAttachmentsResult) func(io.Writer) {
	return func(w io.Writer) {
		acct, providerName := "-", "-"
		if len(r.Accounts) > 0 {
			acct = r.Accounts[0].Name
			providerName = r.Accounts[0].Provider
		}
		_ = providerName
		if len(r.Attachments) == 0 {
			fmt.Fprintf(w, "The draft has no attachments (%s).\n", plainField(acct))
			return
		}
		fmt.Fprintf(w, "%s on the draft (%s):\n\n", upperFirst(pluralize(r.Count, "attachment", "attachments")), plainField(acct))
		attachmentTable(w, r.Attachments)
	}
}

func humanMessageAttachments(r messageAttachmentsResult) func(io.Writer) {
	return func(w io.Writer) {
		acct, providerName := "-", "-"
		if len(r.Accounts) > 0 {
			acct = r.Accounts[0].Name
			providerName = r.Accounts[0].Provider
		}
		_ = providerName
		if len(r.Attachments) == 0 {
			fmt.Fprintf(w, "The message has no attachments (%s).\n", plainField(acct))
			return
		}
		fmt.Fprintf(w, "%s (%s):\n\n", upperFirst(pluralize(r.Count, "attachment", "attachments")), plainField(acct))
		attachmentTable(w, r.Attachments)
	}
}

func plainMessageAttachments(r messageAttachmentsResult) func(io.Writer) {
	return func(w io.Writer) {
		for _, a := range r.Attachments {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%t\t%s\t%s\t%s\n",
				plainField(a.ID),
				plainField(a.Name),
				plainField(a.ContentType),
				a.Size,
				a.IsInline,
				plainField(a.ContentID),
				plainField(string(a.Kind)),
				plainField(a.ProviderType),
			)
		}
	}
}

// humanMessageAttachmentDownload says how many files were saved where, with
// a row for each attachment.
func humanMessageAttachmentDownload(r messageAttachmentDownloadResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Saved %s to %s.\n", pluralize(r.DownloadedCount, "attachment", "attachments"), plainField(r.OutputDirectory))
		downloadTable(w, r.Attachments, false)
	}
}

// downloadTable lists the attachments of a download, with what happened to
// each, or for a preview what would happen.
func downloadTable(w io.Writer, rows []attachmentDownloadRow, preview bool) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w)
	t := newTable("FILE", "SIZE", "RESULT")
	t.indent = "  "
	for _, row := range rows {
		file := firstNonEmpty(row.SafeBasename, row.Name, "(no name)")
		result := row.Status
		switch row.Status {
		case attachmentStatusDownloaded:
			result = "saved"
		case attachmentStatusPlanned:
			result = "would be saved"
		case attachmentStatusSkipped:
			result = "left out"
			switch row.Reason {
			case attachmentReasonUnsupportedKind:
				result = "left out: not a file"
			case attachmentReasonNotAttempted:
				result = "left out: an earlier one failed"
			case "":
			default:
				result += ": " + strings.ReplaceAll(row.Reason, "_", " ")
			}
		case attachmentStatusFailed:
			if row.Reason != "" {
				result += ": " + strings.ReplaceAll(row.Reason, "_", " ")
			}
		}
		if preview && row.Status == attachmentStatusDownloaded {
			result = "would be saved"
		}
		size := "-"
		switch {
		case row.SavedSize > 0 && !preview:
			size = byteSize(row.SavedSize)
		case row.Size > 0:
			size = byteSize(row.Size)
		}
		t.add(file, size, result)
	}
	t.render(w, 0, nil)
}

// humanDownloadPreview says which files a download would save where.
func humanDownloadPreview(r messageAttachmentDownloadResult, command string) func(io.Writer) {
	return func(w io.Writer) {
		planned := 0
		for _, row := range r.Attachments {
			if row.Status != attachmentStatusSkipped {
				planned++
			}
		}
		fmt.Fprintf(w, "Would save %s to %s.\n", pluralize(planned, "attachment", "attachments"), plainField(r.OutputDirectory))
		downloadTable(w, r.Attachments, true)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Nothing was saved. To save them, run:")
		fmt.Fprintln(w, "  "+plainField(command))
	}
}

// humanAttachmentChangePreview says which files a dry run would add to or
// remove from a draft.
func humanAttachmentChangePreview(acct provider.ResolvedAccount, subject string, names []string, add bool, command string) func(io.Writer) {
	return func(w io.Writer) {
		draft := "the draft"
		if subject != "" {
			draft += " " + subjectText(subject)
		}
		files := attachmentNamesText(names)
		if add {
			fmt.Fprintf(w, "Would add %s to %s in %s.\n\n", files, draft, plainField(acct.Name))
		} else {
			fmt.Fprintf(w, "Would remove %s from %s in %s.\n\n", files, draft, plainField(acct.Name))
		}
		fmt.Fprintln(w, "Nothing was changed. To do it, run:")
		fmt.Fprintln(w, "  "+plainField(command))
	}
}

// humanAttachmentChange says which files were added to or removed from a
// draft, and the draft's new short id when the change gave it one.
func humanAttachmentChange(f *rungrad.Factory, acct, oldID, newID, subject string, names []string, add bool, count int) func(io.Writer) {
	changed := newID != "" && newID != oldID
	if changed {
		rememberShortIDs(f, []shortIDEntry{messageRef(acct, newID)})
	}
	return func(w io.Writer) {
		draft := "the draft"
		if subject != "" {
			draft += " " + subjectText(subject)
		}
		files := attachmentNamesText(names)
		if add {
			fmt.Fprintf(w, "Added %s to %s in %s.", files, draft, plainField(acct))
		} else {
			fmt.Fprintf(w, "Removed %s from %s in %s.", files, draft, plainField(acct))
		}
		fmt.Fprintf(w, " It now has %s.\n", pluralize(count, "attachment", "attachments"))
		if changed {
			short := shortID(acct, newID)
			fmt.Fprintf(w, "Draft id: %s. Send it with: maildo draft send %s\n", short, short)
		}
	}
}

// attachmentNamesText names one or two files, or counts more.
func attachmentNamesText(names []string) string {
	switch len(names) {
	case 0:
		return "no files"
	case 1:
		return plainField(names[0])
	case 2:
		return plainField(names[0]) + " and " + plainField(names[1])
	default:
		return fmt.Sprintf("%d files", len(names))
	}
}

func uploadNames(uploads []mail.AttachmentUpload) []string {
	out := make([]string, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, u.Name)
	}
	return out
}

func plainDraftAttachments(r draftAttachmentsResult) func(io.Writer) {
	return func(w io.Writer) {
		for _, a := range r.Attachments {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%t\t%s\n",
				plainField(a.ID),
				plainField(a.Name),
				plainField(a.ContentType),
				a.Size,
				a.IsInline,
				plainField(a.ContentID),
			)
		}
	}
}

func previewAttachmentsAdd(acct provider.ResolvedAccount, draftID string, uploads []mail.AttachmentUpload) output.DryRunPreview {
	fields := []output.Field{
		{Name: "account", Value: acct.Name},
		{Name: "provider", Value: acct.Provider},
		{Name: "draft_id", Value: draftID},
		{Name: "upload_count", Value: strconv.Itoa(len(uploads))},
	}
	var total int64
	for _, upload := range uploads {
		total += upload.Size
	}
	fields = append(fields, output.Field{Name: "upload_bytes", Value: strconv.FormatInt(total, 10)})
	for i, upload := range uploads {
		fields = append(fields,
			output.Field{Name: fmt.Sprintf("upload_%d_name", i+1), Value: upload.Name},
			output.Field{Name: fmt.Sprintf("upload_%d_content_type", i+1), Value: upload.ContentType},
			output.Field{Name: fmt.Sprintf("upload_%d_size", i+1), Value: strconv.FormatInt(upload.Size, 10)},
		)
	}
	return output.DryRunPreview{Method: "attachments-add", Path: acct.Name, Body: fields}
}

func previewAttachmentsRemove(acct provider.ResolvedAccount, draftID string, target mail.Attachment) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "attachments-remove",
		Path:   acct.Name,
		Body: []output.Field{
			{Name: "account", Value: acct.Name},
			{Name: "provider", Value: acct.Provider},
			{Name: "draft_id", Value: draftID},
			{Name: "attachment_id", Value: target.ID},
			{Name: "attachment_name", Value: target.Name},
			{Name: "content_type", Value: target.ContentType},
			{Name: "size", Value: strconv.FormatInt(target.Size, 10)},
		},
	}
}

func previewMessageAttachmentDownload(acct provider.ResolvedAccount, r messageAttachmentDownloadResult) output.DryRunPreview {
	fields := []output.Field{
		{Name: "account", Value: acct.Name},
		{Name: "provider", Value: acct.Provider},
		{Name: "selector_kind", Value: r.SelectorKind},
		{Name: "selection_kind", Value: r.SelectionKind},
		{Name: "source_message_id", Value: r.SourceID},
		{Name: "target_directory", Value: r.OutputDirectory},
		{Name: "selected_count", Value: strconv.Itoa(r.SelectedCount)},
		{Name: "downloaded_count", Value: strconv.Itoa(r.DownloadedCount)},
		{Name: "skipped_count", Value: strconv.Itoa(r.SkippedCount)},
	}
	var total int64
	for _, row := range r.Attachments {
		if row.Status != "skipped" {
			total += row.Size
		}
	}
	fields = append(fields, output.Field{Name: "planned_bytes", Value: strconv.FormatInt(total, 10)})
	for i, row := range r.Attachments {
		prefix := fmt.Sprintf("attachment_%d_", i+1)
		fields = append(fields,
			output.Field{Name: prefix + "id", Value: row.ID},
			output.Field{Name: prefix + "name", Value: row.Name},
			output.Field{Name: prefix + "safe_basename", Value: row.SafeBasename},
			output.Field{Name: prefix + "status", Value: row.Status},
			output.Field{Name: prefix + "reason", Value: row.Reason},
			output.Field{Name: prefix + "size", Value: strconv.FormatInt(row.Size, 10)},
		)
	}
	return output.DryRunPreview{Method: "attachments-download", Path: acct.Name, Body: fields}
}

// partialAttachmentAddError is a failed upload after earlier files were
// already added to the draft. It keeps the server's error and says which
// file failed.
type partialAttachmentAddError struct {
	err     error
	draftID string
	uploads []mail.AttachmentUpload
	added   []mail.Attachment
}

func (e partialAttachmentAddError) Error() string {
	return e.err.Error() + fmt.Sprintf(" (%s added to the draft before the failure)", pluralize(len(e.added), "file was", "files were"))
}

func (e partialAttachmentAddError) ExitCode() int {
	if c, ok := e.err.(interface{ ExitCode() int }); ok {
		return c.ExitCode()
	}
	return 1
}

func (e partialAttachmentAddError) ErrorDetails() any {
	failedName := ""
	var failedSize int64
	if len(e.added) < len(e.uploads) {
		failed := e.uploads[len(e.added)]
		failedName = failed.Name
		failedSize = failed.Size
	}
	details := map[string]any{
		"draft_id":          e.draftID,
		"added_attachments": e.added,
		"partial":           true,
	}
	if failedName != "" {
		details["failed_file_name"] = failedName
		details["failed_file_size"] = failedSize
	}
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

// partialAttachmentDownloadError is a failed download after at least one
// earlier attachment was already saved.
type partialAttachmentDownloadError struct {
	err    error
	result messageAttachmentDownloadResult
}

func (e partialAttachmentDownloadError) Error() string {
	return e.err.Error() + fmt.Sprintf(" (%s saved before the failure)", pluralize(e.result.DownloadedCount, "attachment was", "attachments were"))
}

func (e partialAttachmentDownloadError) ExitCode() int {
	if c, ok := e.err.(interface{ ExitCode() int }); ok {
		return c.ExitCode()
	}
	return 1
}

func (e partialAttachmentDownloadError) ErrorDetails() any {
	details := map[string]any{
		"code":              errs.CodeAttachmentDownloadPartialFailure,
		"partial":           true,
		"source_message_id": e.result.SourceID,
		"selector_kind":     e.result.SelectorKind,
		"selection_kind":    e.result.SelectionKind,
		"selected_count":    e.result.SelectedCount,
		"downloaded_count":  e.result.DownloadedCount,
		"skipped_count":     e.result.SkippedCount,
		"failed_count":      e.result.FailedCount,
		"attachments":       e.result.Attachments,
	}
	if e.result.SourceInternetID != "" {
		details["source_internet_message_id"] = e.result.SourceInternetID
	}
	underlying := safeErrorDetails(e.err)
	if code, ok := underlying["code"].(string); ok && code != "" {
		details["underlying_error_code"] = code
	}
	for k, v := range underlying {
		if k == "code" {
			continue
		}
		// The partial-download code stays on top, also when the
		// underlying error has details of its own.
		if _, exists := details[k]; !exists {
			details[k] = v
		}
	}
	for _, row := range e.result.Attachments {
		if row.Status == "failed" {
			details["failed_attachment_id"] = row.ID
			details["failed_safe_basename"] = row.SafeBasename
			break
		}
	}
	return details
}

func safeErrorDetails(err error) map[string]any {
	if d, ok := err.(interface{ ErrorDetails() any }); ok {
		if m, ok := d.ErrorDetails().(map[string]any); ok {
			out := make(map[string]any, len(m))
			for k, v := range m {
				out[k] = v
			}
			return out
		}
	}
	return nil
}

// attachmentTable lists attachments with their type and a readable size.
// The ATTACHMENT ID column is what --attachment-id takes.
func attachmentTable(w io.Writer, attachments []mail.Attachment) {
	t := newTable("NAME", "TYPE", "SIZE", "ATTACHMENT ID")
	t.indent = "  "
	for _, a := range attachments {
		name := firstNonEmpty(a.Name, "(no name)")
		if a.IsInline {
			name += " (inline)"
		}
		t.add(name, firstNonEmpty(a.ContentType, "-"), attachmentSize(a), a.ID)
	}
	t.render(w, 0, nil)
}
