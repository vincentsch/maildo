package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// folderListResult is the result of `folders list`, with the accounts it
// covers.
type folderListResult struct {
	provider.AccountSelection
	Folders []mail.Folder `json:"folders"`
}

// folderViewResult is the result of `folders view`. It has the account
// selection that other results have, and also plain account and provider
// fields, since it is about one folder in one account.
type folderViewResult struct {
	provider.AccountSelection
	Account   string         `json:"account"`
	Provider  string         `json:"provider"`
	Ref       mail.FolderRef `json:"ref"`
	MatchedBy string         `json:"matched_by,omitempty"`
	Folder    mail.Folder    `json:"folder"`
}

// folderMutationResult is the result of creating, renaming or deleting a
// folder.
type folderMutationResult struct {
	provider.AccountSelection
	Operation             string                      `json:"operation"`
	Applied               bool                        `json:"applied"`
	ParentFolder          *mail.FolderSummary         `json:"parent_folder,omitempty"`
	TargetFolder          *mail.FolderSummary         `json:"target_folder,omitempty"`
	CreatedFolder         *mail.FolderSummary         `json:"created_folder,omitempty"`
	ResultFolder          *mail.FolderSummary         `json:"result_folder,omitempty"`
	DeletedFolder         *mail.FolderSummary         `json:"deleted_folder,omitempty"`
	OldDisplayName        string                      `json:"old_display_name,omitempty"`
	NewDisplayName        string                      `json:"new_display_name,omitempty"`
	TotalItemCount        int                         `json:"total_item_count,omitempty"`
	TotalItemCountKnown   bool                        `json:"total_item_count_known,omitempty"`
	ChildFolderCount      int                         `json:"child_folder_count,omitempty"`
	ChildFolderCountKnown bool                        `json:"child_folder_count_known,omitempty"`
	CountsSource          string                      `json:"counts_source,omitempty"`
	DescendantFacts       *mail.FolderDescendantFacts `json:"descendant_facts,omitempty"`
	ConfirmationRequired  bool                        `json:"confirmation_required,omitempty"`
	ConfirmationMatched   bool                        `json:"confirmation_matched,omitempty"`
	VerificationStatus    string                      `json:"verification_status,omitempty"`
	*mail.WriteOutcomeFacts
	ProviderMeta    *mail.FolderProviderMetadata `json:"provider_metadata,omitempty"`
	ActionLogStatus mail.ActionLogStatus         `json:"action_log_status"`
}

// MarshalJSON always writes the counts of a delete, also when they are zero
// or false, and leaves them out for create and rename.
func (r folderMutationResult) MarshalJSON() ([]byte, error) {
	type resultJSON folderMutationResult
	if r.Operation != string(mail.FolderOperationDelete) {
		return json.Marshal(resultJSON(r))
	}
	// The outer fields override the embedded omitempty fields for deletes, so
	// zero and false stay visible without adding delete-only fields elsewhere.
	return json.Marshal(struct {
		resultJSON
		TotalItemCount        int  `json:"total_item_count"`
		TotalItemCountKnown   bool `json:"total_item_count_known"`
		ChildFolderCount      int  `json:"child_folder_count"`
		ChildFolderCountKnown bool `json:"child_folder_count_known"`
	}{
		resultJSON:            resultJSON(r),
		TotalItemCount:        r.TotalItemCount,
		TotalItemCountKnown:   r.TotalItemCountKnown,
		ChildFolderCount:      r.ChildFolderCount,
		ChildFolderCountKnown: r.ChildFolderCountKnown,
	})
}

// folderMutationPreviewPlan is what a folder change would do. Its JSON is
// the flat field list of a dry-run preview.
type folderMutationPreviewPlan struct {
	Account                   provider.ResolvedAccount
	Operation                 mail.FolderMutationOperation
	DisplayName               string
	NewDisplayName            string
	Parent                    *mail.FolderSummary
	Target                    *mail.FolderSummary
	Preflight                 mail.FolderMutationPreflight
	ConfirmationRequired      bool
	PathConfirmationAvailable bool
	// Command makes the change: the same command for the same account,
	// without --dry-run.
	Command string
}

// writeFolderPreview shows a folder change that --dry-run stopped.
func writeFolderPreview(f *rungrad.Factory, plan folderMutationPreviewPlan) error {
	return writeReadablePreview(f, previewFolderMutation(plan), humanFolderPreview(plan))
}

// humanFolderPreview says what a folder change would do and how to run it.
func humanFolderPreview(plan folderMutationPreviewPlan) func(io.Writer) {
	return func(w io.Writer) {
		in := " in " + plainField(plan.Account.Name)
		verb := ""
		switch plan.Operation {
		case mail.FolderOperationCreate:
			verb = "create it"
			if plan.Parent != nil {
				in = " under " + folderQuoted(plan.Parent, "") + in
			}
			fmt.Fprintf(w, "Would create the folder %s%s.\n", quote(plainField(plan.DisplayName)), in)
		case mail.FolderOperationRename:
			verb = "rename it"
			fmt.Fprintf(w, "Would rename the folder %s to %s%s.\n", folderQuoted(plan.Target, ""), quote(plainField(plan.NewDisplayName)), in)
		case mail.FolderOperationDelete:
			verb = "delete it"
			fmt.Fprintf(w, "Would delete the folder %s%s.\n", folderQuoted(plan.Target, ""), in)
			fmt.Fprintln(w, folderDeleteReach(plan.Account.Provider))
		}
		fmt.Fprintln(w)
		if plan.Command == "" {
			fmt.Fprintln(w, "Nothing was changed.")
			return
		}
		fmt.Fprintf(w, "Nothing was changed. To %s, run:\n", verb)
		fmt.Fprintln(w, "  "+plan.Command)
	}
}

// folderDeleteReach says what a folder delete reaches. maildo previews a
// delete only for an empty folder without subfolders, so this is always
// nothing.
func folderDeleteReach(providerName string) string {
	if providerName == "gmail" {
		return "No message has this label, and it has no nested labels."
	}
	return "It is empty and has no subfolders."
}

// folderMutationResultFromProvider builds the result from the server's
// answer and adds the account selection.
func folderMutationResultFromProvider(sel provider.AccountSelection, result mail.FolderMutationResult, applied bool) folderMutationResult {
	verification := result.VerificationStatus
	if applied && verification == "" {
		verification = mail.FolderVerificationProviderResponse
	}
	out := folderMutationResult{
		AccountSelection:      sel,
		Operation:             string(result.Operation),
		Applied:               applied,
		ParentFolder:          result.ParentFolder,
		TargetFolder:          result.TargetFolder,
		CreatedFolder:         result.CreatedFolder,
		DeletedFolder:         result.DeletedFolder,
		OldDisplayName:        result.OldDisplayName,
		NewDisplayName:        result.NewDisplayName,
		TotalItemCount:        result.TotalItemCount,
		TotalItemCountKnown:   result.TotalItemCountKnown,
		ChildFolderCount:      result.ChildFolderCount,
		ChildFolderCountKnown: result.ChildFolderKnown,
		CountsSource:          result.CountsSource,
		VerificationStatus:    verification,
		WriteOutcomeFacts:     mail.CloneWriteOutcomeFacts(result.WriteOutcomeFacts),
		ProviderMeta:          cloneProviderMetadataForResult(result.ProviderMeta),
	}
	if result.DescendantFactsPresent {
		facts := result.DescendantFacts
		facts.Sample = append([]mail.FolderDescendant(nil), result.DescendantFacts.Sample...)
		out.DescendantFacts = &facts
	}
	switch {
	case result.RenamedFolder != nil:
		out.ResultFolder = result.RenamedFolder
	case result.CreatedFolder != nil:
		out.ResultFolder = result.CreatedFolder
	case result.DeletedFolder != nil:
		out.ResultFolder = result.DeletedFolder
	}
	return out
}

// humanFolderList prints folders as a table of paths, the names other
// commands accept, with their message counts.
func humanFolderList(f *rungrad.Factory, r folderListResult) func(io.Writer) {
	return func(w io.Writer) {
		if len(r.Folders) == 0 {
			fmt.Fprintln(w, "No folders.")
			return
		}
		fmt.Fprintf(w, "%s%s\n\n", upperFirst(pluralize(len(r.Folders), "folder", "folders")), accountSuffix(r.AccountSelection))
		many := len(r.Accounts) > 1
		header := []string{"FOLDER", "UNREAD", "TOTAL"}
		if many {
			header = append([]string{"ACCOUNT"}, header...)
		}
		t := newTable(header...)
		for _, folder := range r.Folders {
			row := []string{folderListName(folder), countCell(folder.UnreadCount, folder.UnreadCountKnown), countCell(folder.TotalCount, folder.TotalCountKnown)}
			if many {
				row = append([]string{folder.Account}, row...)
			}
			t.add(row...)
		}
		t.render(w, screenWidth(f), boldStyle(f))
	}
}

// folderListName is a folder's path, with its role added when the name
// does not already say it, as in "Gesendete Elemente (Sent)".
func folderListName(folder mail.Folder) string {
	name := folderSummaryText(mail.SummaryFrom(folder))
	role := folderRoleText(folder.WellKnownName)
	if role == "" || strings.EqualFold(folder.DisplayName, role) {
		return name
	}
	return name + " (" + role + ")"
}

// folderRoleText names a well-known folder role for people.
func folderRoleText(wellKnown string) string {
	switch wellKnown {
	case "":
		return ""
	case mail.WellKnownInbox:
		return "Inbox"
	case mail.WellKnownDeleted:
		return "Trash"
	default:
		return upperFirst(wellKnown)
	}
}

// plainFolderList prints tab-separated lines for scripts: account,
// provider, id, display name.
func plainFolderList(r folderListResult) func(io.Writer) {
	return func(w io.Writer) {
		for _, folder := range r.Folders {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", plainField(folder.Account), plainField(folder.Provider), plainField(folder.ID), plainField(folder.DisplayName))
		}
	}
}

func plainField(s string) string {
	// Text from the server ends up in human and plain output. Remove
	// terminal escape sequences first, then replace control and format
	// characters that could hide text, reorder it, or break a line.
	if strings.IndexByte(s, 0x1b) >= 0 || strings.IndexByte(s, 0x9b) >= 0 || strings.IndexByte(s, 0x9d) >= 0 {
		s = string(output.SanitizeControlBytes([]byte(s)))
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, s)
}

// humanFolderMutation says in one sentence which folder changed.
func humanFolderMutation(r folderMutationResult) func(io.Writer) {
	return func(w io.Writer) {
		acct := ""
		if len(r.Accounts) > 0 {
			acct = r.Accounts[0].Name
		}
		in := " in " + plainField(acct)
		switch r.Operation {
		case string(mail.FolderOperationCreate):
			name := folderQuoted(r.ResultFolder, r.NewDisplayName)
			if r.ParentFolder != nil {
				in = " under " + folderQuoted(r.ParentFolder, "") + in
			}
			fmt.Fprintf(w, "Created the folder %s%s.\n", name, in)
		case string(mail.FolderOperationRename):
			fmt.Fprintf(w, "Renamed the folder %s to %s%s.\n", folderQuoted(r.TargetFolder, r.OldDisplayName), folderQuoted(r.ResultFolder, r.NewDisplayName), in)
		case string(mail.FolderOperationDelete):
			fmt.Fprintf(w, "Deleted the folder %s%s.\n", folderQuoted(r.TargetFolder, ""), in)
		default:
			fmt.Fprintf(w, "Changed the folder %s%s.\n", folderQuoted(r.ResultFolder, ""), in)
		}
	}
}

// folderQuoted is a folder's path in quotes, or fallback when the result
// has no folder.
func folderQuoted(folder *mail.FolderSummary, fallback string) string {
	if folder != nil {
		return quote(plainField(folderSummaryText(*folder)))
	}
	return quote(plainField(fallback))
}

// humanFolderView shows one folder with its counts and id.
func humanFolderView(r folderViewResult) func(io.Writer) {
	return func(w io.Writer) {
		folder := r.Folder
		field := func(label, value string) {
			fmt.Fprintf(w, "  %-12s %s\n", label+":", plainField(value))
		}
		fmt.Fprintf(w, "%s (%s)\n\n", plainField(folderSummaryText(mail.SummaryFrom(folder))), plainField(r.Account))
		field("Name", folder.DisplayName)
		if role := folderRoleText(folder.WellKnownName); role != "" {
			field("Role", role)
		}
		field("Parent", firstNonEmpty(folder.ParentPath, "(top level)"))
		field("Subfolders", strconv.Itoa(folder.ChildCount))
		messages := knownCountText(folder.TotalCount, folder.TotalCountKnown)
		if folder.UnreadCountKnown {
			messages += ", " + strconv.Itoa(folder.UnreadCount) + " unread"
		}
		field("Messages", messages)
		field("ID", folder.ID)
	}
}

// folderMutationMeta holds the folder ids and, for a delete, the counts and
// the confirmation, for --include-meta.
func folderMutationMeta(commandPath string, r folderMutationResult) output.Meta {
	accounts, providers := accountProviderNames(r.AccountSelection)
	extra := map[string]any{
		"command_path": commandPath,
		"scope":        r.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"operation":    r.Operation,
		"applied":      r.Applied,
	}
	if r.ParentFolder != nil && r.ParentFolder.ID != "" {
		extra["parent_folder_id"] = r.ParentFolder.ID
	}
	if r.TargetFolder != nil && r.TargetFolder.ID != "" {
		extra["target_folder_id"] = r.TargetFolder.ID
	}
	if r.ResultFolder != nil && r.ResultFolder.ID != "" {
		extra["result_folder_id"] = r.ResultFolder.ID
	}
	if r.Operation == string(mail.FolderOperationDelete) {
		extra["total_item_count"] = r.TotalItemCount
		extra["total_item_count_known"] = r.TotalItemCountKnown
		extra["child_folder_count"] = r.ChildFolderCount
		extra["child_folder_count_known"] = r.ChildFolderCountKnown
		extra["counts_source"] = r.CountsSource
		extra["confirmation_required"] = r.ConfirmationRequired
		extra["confirmation_matched"] = r.ConfirmationMatched
	}
	if r.VerificationStatus != "" {
		extra["verification_status"] = r.VerificationStatus
	}
	return output.Meta{Extra: extra}
}

// previewFolderMutation builds the JSON of a folder preview as a flat list of
// fields. A delete preview has the fresh counts and says whether its path
// can be used in --confirm.
func previewFolderMutation(plan folderMutationPreviewPlan) output.DryRunPreview {
	fields := []output.Field{
		{Name: "account", Value: plan.Account.Name},
		{Name: "provider", Value: plan.Account.Provider},
		{Name: "operation", Value: string(plan.Operation)},
		{Name: "applied", Value: "false"},
	}
	if plan.DisplayName != "" {
		fields = append(fields, output.Field{Name: "display_name", Value: plan.DisplayName})
	}
	if plan.NewDisplayName != "" {
		fields = append(fields, output.Field{Name: "new_display_name", Value: plan.NewDisplayName})
	}
	if plan.Parent != nil {
		fields = append(fields,
			output.Field{Name: "parent_folder_id", Value: plan.Parent.ID},
			output.Field{Name: "parent_folder", Value: folderSummaryText(*plan.Parent)},
		)
	}
	if plan.Target != nil {
		fields = append(fields,
			output.Field{Name: "target_folder_id", Value: plan.Target.ID},
			output.Field{Name: "target_folder", Value: folderSummaryText(*plan.Target)},
		)
	}
	if plan.Operation == mail.FolderOperationDelete {
		fields = append(fields,
			output.Field{Name: "total_item_count", Value: strconv.Itoa(plan.Preflight.TotalItemCount)},
			output.Field{Name: "total_item_count_known", Value: strconv.FormatBool(plan.Preflight.TotalItemCountKnown)},
			output.Field{Name: "child_folder_count", Value: strconv.Itoa(plan.Preflight.ChildFolderCount)},
			output.Field{Name: "child_folder_count_known", Value: strconv.FormatBool(plan.Preflight.ChildFolderKnown)},
			output.Field{Name: "counts_source", Value: plan.Preflight.CountsSource},
			output.Field{Name: "confirmation_required", Value: strconv.FormatBool(plan.ConfirmationRequired)},
			output.Field{Name: "path_confirmation_available", Value: strconv.FormatBool(plan.PathConfirmationAvailable)},
		)
	}
	if plan.Command != "" {
		fields = append(fields, output.Field{Name: "command", Value: plan.Command})
	}
	return output.DryRunPreview{Method: string(plan.Operation), Path: plan.Account.Name, Body: fields}
}

// writeReadablePreview writes a dry-run preview with its usual JSON and a
// human view in sentences instead of a list of fields.
func writeReadablePreview(f *rungrad.Factory, p output.DryRunPreview, human func(io.Writer)) error {
	return f.WriteResult(p, human)
}

// countCell is a count for a table cell, "-" when unknown.
func countCell(count int, known bool) string {
	if !known {
		return "-"
	}
	return thousands(count)
}

func knownCountText(count int, known bool) string {
	if !known {
		return "unknown"
	}
	return strconv.Itoa(count)
}
