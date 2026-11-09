package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// maxDescribedTargets limits how many messages a preview looks up by id to
// show their subject and sender.
const maxDescribedTargets = 100

// describeTargets looks up the subject, sender and folder of targets that
// were given by id, so previews and results can name them. A target that
// cannot be found keeps the reason in Problem; the change itself would fail
// for it too.
func (h *handlers) describeTargets(ctx context.Context, scope provider.AccountClientScope, targets []mutationTarget, limit int) {
	described := 0
	for i := range targets {
		t := &targets[i]
		if t.Summary != nil || (t.MessageID == "" && t.InternetMessageID == "") {
			continue
		}
		if described >= limit {
			return
		}
		described++
		err := provider.WithAccountClient(ctx, scope, t.Account, provider.ClientAccessMessagesRead, func(reader provider.MessageReader) error {
			selector := mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: t.MessageID}
			if t.MessageID == "" {
				selector = mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorInternetID, InternetID: t.InternetMessageID}
			}
			msg, err := reader.ReadMessage(ctx, mail.ReadMessageRequest{Selector: selector, MetadataOnly: true})
			if err != nil {
				return err
			}
			if msg.Subject == "" && msg.From.Address == "" && msg.From.Name == "" {
				// Some providers return only ids from a metadata read. A
				// text read has the subject and sender.
				full, err := reader.ReadMessage(ctx, mail.ReadMessageRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: msg.ID}, BodyMode: mail.BodyModeText})
				if err == nil {
					msg = full
				}
			}
			if t.MessageID == "" {
				t.MessageID = msg.ID
			}
			t.Summary = &mail.MessageSummary{
				Account: msg.Account, Provider: msg.Provider, ID: t.MessageID,
				InternetMessageID: msg.InternetMessageID, Folder: msg.Folder,
				From: msg.From, Subject: msg.Subject, ReceivedAt: msg.ReceivedAt, SentAt: msg.SentAt,
			}
			t.Described = true
			if t.InternetMessageID == "" {
				t.InternetMessageID = msg.InternetMessageID
			}
			return nil
		})
		if err != nil {
			t.Problem = explain(err).message
		}
	}
}

// targetLabel names a message for a sentence: "Project kickoff" from Clara
// Client. Without details it falls back to the short id.
func targetLabel(t mutationTarget) string {
	if t.Summary == nil {
		return "message " + shortID(t.Account.Name, t.MessageID)
	}
	// A draft has no sender yet; "from -" would read like a folder name.
	if strings.TrimSpace(t.Summary.From.Name) == "" && t.Summary.From.Address == "" {
		return subjectText(t.Summary.Subject)
	}
	return subjectText(t.Summary.Subject) + " from " + plainField(senderName(t.Summary.From))
}

// opPhrase says what an operation does to object, in the present tense for
// previews ("archive 3 messages") or the past tense for results. cats are
// the category names a categories command sets or removes.
func opPhrase(op mail.MessageMutationOperation, object, folder string, cats []string, past bool) string {
	pick := func(present, done string) string {
		if past {
			return done
		}
		return present
	}
	switch op {
	case mail.MutationArchive:
		return pick("archive ", "Archived ") + object
	case mail.MutationMove:
		return pick("move ", "Moved ") + object + " to " + folder
	case mail.MutationCopy:
		return pick("copy ", "Copied ") + object + " to " + folder
	case mail.MutationDelete:
		return pick("move ", "Moved ") + object + " to " + firstNonEmpty(folder, "Trash")
	case mail.MutationRestore:
		return pick("restore ", "Restored ") + object + " to " + folder
	case mail.MutationJunk:
		return pick("move ", "Moved ") + object + " to " + firstNonEmpty(folder, "Junk")
	case mail.MutationMarkRead:
		return pick("mark ", "Marked ") + object + " as read"
	case mail.MutationMarkUnread:
		return pick("mark ", "Marked ") + object + " as unread"
	case mail.MutationFlagSet:
		return pick("flag ", "Flagged ") + object
	case mail.MutationFlagClear:
		return pick("remove the flag from ", "Removed the flag from ") + object
	case mail.MutationCategoriesSet:
		return pick("set the categories of ", "Set the categories of ") + object + " to " + strings.Join(cats, ", ")
	case mail.MutationCategoriesClear:
		if len(cats) > 0 {
			return pick("remove the categories ", "Removed the categories ") + strings.Join(cats, ", ") + " from " + object
		}
		return pick("clear the categories of ", "Cleared the categories of ") + object
	default:
		return pick(string(op)+" ", "Changed ") + object
	}
}

// wouldChange says whether a read or flag change would do anything, when
// the target came from a search and its state is known.
func wouldChange(t mutationTarget, op mail.MessageMutationOperation) *bool {
	if t.Summary == nil || t.Described {
		return nil
	}
	var v bool
	switch op {
	case mail.MutationMarkRead:
		v = !t.Summary.IsRead
	case mail.MutationMarkUnread:
		v = t.Summary.IsRead
	case mail.MutationFlagSet:
		v = !t.Summary.Flagged
	case mail.MutationFlagClear:
		v = t.Summary.Flagged
	default:
		return nil
	}
	return &v
}

// alreadyState names the state that makes a read or flag change do
// nothing ("read", "flagged"), or "" for other operations.
func alreadyState(op mail.MessageMutationOperation) string {
	switch op {
	case mail.MutationMarkRead:
		return "read"
	case mail.MutationMarkUnread:
		return "unread"
	case mail.MutationFlagSet:
		return "flagged"
	case mail.MutationFlagClear:
		return "unflagged"
	}
	return ""
}

// mutationPreview is what a change would do. Bulk changes show it until you
// add --apply; single changes show it with --dry-run.
type mutationPreview struct {
	provider.AccountSelection
	DryRun       bool                     `json:"dry_run"`
	Operation    string                   `json:"operation"`
	Applied      bool                     `json:"applied"`
	Selector     mail.BulkSelectorSummary `json:"selector"`
	TargetFolder *mail.FolderSummary      `json:"target_folder,omitempty"`
	Categories   *mail.CategoryChange     `json:"categories,omitempty"`
	Count        int                      `json:"count"`
	Targets      []previewTarget          `json:"targets"`
	ApplyCommand string                   `json:"apply_command,omitempty"`
}

type previewTarget struct {
	Account           string               `json:"account"`
	Provider          string               `json:"provider"`
	ID                string               `json:"id"`
	InternetMessageID string               `json:"internet_message_id,omitempty"`
	Subject           *string              `json:"subject,omitempty"`
	From              *mail.Recipient      `json:"from,omitempty"`
	Folder            *mail.FolderSummary  `json:"folder,omitempty"`
	ReceivedAt        *time.Time           `json:"received_at,omitempty"`
	SentAt            *time.Time           `json:"sent_at,omitempty"`
	TargetFolder      *mail.FolderSummary  `json:"target_folder,omitempty"`
	Categories        *mail.CategoryChange `json:"categories,omitempty"`
	WouldChange       *bool                `json:"would_change,omitempty"`
	Problem           string               `json:"problem,omitempty"`
}

func buildMutationPreview(set mutationTargetSet, plan mutationPlan, folders map[string]mail.Folder, categoryChanges map[int]*mail.CategoryChange, applyCommand string) mutationPreview {
	out := mutationPreview{
		AccountSelection: set.Selection,
		DryRun:           true,
		Operation:        string(plan.Operation),
		Selector:         set.Summary,
		Categories:       plan.CategoryChange,
		Count:            len(set.Targets),
		Targets:          make([]previewTarget, 0, len(set.Targets)),
		ApplyCommand:     applyCommand,
	}
	if len(folders) == 1 {
		for _, folder := range folders {
			summary := mail.SummaryFrom(folder)
			out.TargetFolder = &summary
		}
	}
	for i, t := range set.Targets {
		row := previewTarget{
			Account:           t.Account.Name,
			Provider:          t.Account.Provider,
			ID:                t.MessageID,
			InternetMessageID: t.InternetMessageID,
			WouldChange:       wouldChange(t, plan.Operation),
			Problem:           t.Problem,
		}
		if s := t.Summary; s != nil {
			subject := s.Subject
			from := s.From
			folder := s.Folder
			row.Subject, row.From, row.Folder = &subject, &from, &folder
			row.ReceivedAt, row.SentAt = s.ReceivedAt, s.SentAt
		}
		if folder, ok := folders[accountKey(t.Account.Name, t.Account.Provider)]; ok && len(folders) > 1 {
			summary := mail.SummaryFrom(folder)
			row.TargetFolder = &summary
		}
		if change := categoryChanges[i]; change != nil {
			row.Categories = change
		}
		out.Targets = append(out.Targets, row)
	}
	return out
}

// humanMutationPreview prints the preview: one sentence for one message, a
// table for several, and the command that makes the change.
func humanMutationPreview(f *rungrad.Factory, p mutationPreview, set mutationTargetSet, plan mutationPlan) func(io.Writer) {
	refs := make([]shortIDEntry, 0, len(set.Targets))
	for _, t := range set.Targets {
		subject := ""
		if t.Summary != nil {
			subject = t.Summary.Subject
		}
		refs = append(refs, messageRefWithSubject(t.Account.Name, t.MessageID, subject))
	}
	ids := rememberShortIDs(f, refs)
	return func(w io.Writer) {
		folder := ""
		if p.TargetFolder != nil {
			folder = folderSummaryText(*p.TargetFolder)
		}
		if len(set.Targets) == 0 {
			fmt.Fprintf(w, "No messages%s match, so there is nothing to change.\n", accountIn(p.AccountSelection))
			if set.SkipsOwnMail {
				fmt.Fprintln(w, "A change with --query leaves out Sent, Drafts, Trash and Junk. To change mail there, name the folder with --folder.")
			}
			return
		}
		problems := 0
		unchanged := 0
		for i, t := range set.Targets {
			if t.Problem != "" {
				problems++
			}
			if wc := p.Targets[i].WouldChange; wc != nil && !*wc {
				unchanged++
			}
		}
		if !set.Bulk && len(set.Targets) == 1 {
			t := set.Targets[0]
			if t.Problem != "" {
				fmt.Fprintf(w, "Cannot %s: %s.\n", opPhrase(plan.Operation, targetLabel(t), folder, plan.Categories, false), plainField(t.Problem))
				return
			}
			// One parenthesis names the folder and the account:
			// "Project kickoff" from Clara (INBOX, work).
			var place []string
			if t.Summary != nil && folderSummaryText(t.Summary.Folder) != "-" {
				place = append(place, plainField(folderSummaryText(t.Summary.Folder)))
			}
			if len(p.Accounts) == 1 {
				place = append(place, p.Accounts[0].Name)
			}
			label := targetLabel(t)
			if len(place) > 0 {
				label += " (" + strings.Join(place, ", ") + ")"
			}
			fmt.Fprintf(w, "Would %s.\n", opPhrase(plan.Operation, label, folder, plan.Categories, false))
			if unchanged == 1 {
				fmt.Fprintf(w, "It is already %s, so nothing would change.\n", alreadyState(plan.Operation))
			}
			todo := opPhrase(plan.Operation, "it", folder, plan.Categories, false)
			if plan.Operation == mail.MutationCategoriesSet || plan.Operation == mail.MutationCategoriesClear {
				todo = "change its categories"
			}
			fmt.Fprintf(w, "Nothing was changed. To %s, run:\n", todo)
			writeCommand(w, p.ApplyCommand, commandInJSON)
			return
		}
		object := pluralize(len(set.Targets), "message", "messages") + accountIn(p.AccountSelection)
		fmt.Fprintf(w, "Would %s:\n\n", opPhrase(plan.Operation, object, folder, plan.Categories, false))
		t := newTable("ID", "FROM", "SUBJECT", "FOLDER", "DATE")
		t.indent = "  "
		t.flex = 2
		for _, target := range set.Targets {
			subject, from, where, date := "", "", "", ""
			if s := target.Summary; s != nil {
				subject = firstNonEmpty(strings.TrimSpace(s.Subject), "(no subject)")
				from = truncate(senderName(s.From), 24)
				where = truncate(folderSummaryText(s.Folder), 24)
				date = relativeTime(s.Time())
			}
			if target.Problem != "" {
				subject = "(" + target.Problem + ")"
			}
			t.add(ids.of(target.Account.Name, target.MessageID), from, subject, where, date)
		}
		t.render(w, screenWidth(f), boldStyle(f))
		fmt.Fprintln(w)
		if unchanged > 0 {
			fmt.Fprintf(w, "%s of them %s already %s, so they would not change.\n", thousands(unchanged), isAre(unchanged), alreadyState(plan.Operation))
		}
		if problems > 0 {
			fmt.Fprintf(w, "%s could not be found and would fail.\n", pluralize(problems, "message", "messages"))
		}
		if problems == len(set.Targets) {
			fmt.Fprintln(w, "Nothing was changed. Search again to get current ids.")
			return
		}
		if p.ApplyCommand == "" {
			fmt.Fprintln(w, "Nothing was changed.")
			return
		}
		fmt.Fprintf(w, "Nothing was changed. To %s, run:\n", opPhrase(plan.Operation, "these messages", folder, plan.Categories, false))
		writeCommand(w, p.ApplyCommand, commandInJSON)
	}
}

// humanMutationResult names the message that changed and, when the change
// gave it a new id (moves do), prints the new short id.
func humanMutationResult(f *rungrad.Factory, r mutationResult, target mutationTarget, plan mutationPlan) func(io.Writer) {
	newID := ""
	ids := shortIDs{}
	if r.ResultMessageID != "" && r.ResultMessageID != r.MessageID {
		newID = r.ResultMessageID
		subject := ""
		if target.Summary != nil {
			subject = target.Summary.Subject
		}
		ids = rememberShortIDs(f, []shortIDEntry{messageRefWithSubject(target.Account.Name, newID, subject)})
	}
	return func(w io.Writer) {
		folder := ""
		if r.TargetFolder != nil {
			folder = folderSummaryText(*r.TargetFolder)
		}
		label := targetLabel(target)
		if r.Changed != nil && !*r.Changed {
			switch mail.MessageMutationOperation(r.Operation) {
			case mail.MutationMarkRead:
				fmt.Fprintf(w, "%s was already read.\n", upperFirst(label))
				return
			case mail.MutationMarkUnread:
				fmt.Fprintf(w, "%s was already unread.\n", upperFirst(label))
				return
			case mail.MutationFlagSet:
				fmt.Fprintf(w, "%s was already flagged.\n", upperFirst(label))
				return
			case mail.MutationFlagClear:
				fmt.Fprintf(w, "%s had no flag.\n", upperFirst(label))
				return
			}
			if folder != "" {
				fmt.Fprintf(w, "%s is already in %s, so nothing changed.\n", upperFirst(label), folder)
			} else {
				fmt.Fprintf(w, "%s did not change.\n", upperFirst(label))
			}
			return
		}
		fmt.Fprintf(w, "%s.\n", opPhrase(plan.Operation, label, folder, plan.Categories, true))
		if newID != "" {
			fmt.Fprintf(w, "Its id is now %s.\n", ids.of(target.Account.Name, newID))
		}
		from := ""
		if r.SourceFolder != nil {
			from = folderSummaryText(*r.SourceFolder)
		}
		writeUndo(w, "it", from, r.UndoCommands)
	}
}

// writeUndo prints the commands that move the messages back. from names
// the folder when they all came from one.
func writeUndo(w io.Writer, object, from string, commands []string) {
	if len(commands) == 0 {
		return
	}
	if from != "" && len(commands) == 1 {
		fmt.Fprintf(w, "To move %s back to %s, run:\n", object, plainField(from))
	} else {
		fmt.Fprintf(w, "To move %s back, run:\n", object)
	}
	for _, c := range commands {
		writeCommand(w, c, commandInJSON)
	}
}

// maxListedResults is how many changed messages a bulk result lists.
const maxListedResults = 20

// humanBulkResult says how many messages changed and lists the first ones
// with their (possibly new) short ids.
func humanBulkResult(f *rungrad.Factory, r bulkMutationResult, set mutationTargetSet, plan mutationPlan) func(io.Writer) {
	refs := make([]shortIDEntry, 0, len(r.Targets))
	for i, row := range r.Targets {
		subject := ""
		if i < len(set.Targets) && set.Targets[i].Summary != nil {
			subject = set.Targets[i].Summary.Subject
		}
		refs = append(refs, messageRefWithSubject(row.Account, firstNonEmpty(row.ResultMessageID, row.ID), subject))
	}
	ids := rememberShortIDs(f, refs)
	return func(w io.Writer) {
		if len(r.Targets) == 0 {
			fmt.Fprintln(w, "No messages matched, so nothing was changed.")
			return
		}
		folder := ""
		if len(r.Targets) > 0 && r.Targets[0].TargetFolder != nil {
			folder = folderSummaryText(*r.Targets[0].TargetFolder)
		}
		unchanged := 0
		for _, row := range r.Targets {
			if row.Changed != nil && !*row.Changed {
				unchanged++
			}
		}
		object := pluralize(r.Counts.Succeeded, "message", "messages") + accountIn(r.AccountSelection)
		fmt.Fprintf(w, "%s.", opPhrase(plan.Operation, object, folder, plan.Categories, true))
		state := alreadyState(plan.Operation)
		if state == "" && folder != "" {
			state = "in " + folder
		}
		switch {
		case unchanged > 0 && state != "":
			fmt.Fprintf(w, " %s of them %s already %s.", thousands(unchanged), wasWere(unchanged), state)
		case unchanged > 0:
			fmt.Fprintf(w, " %s of them did not change.", thousands(unchanged))
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w)
		t := newTable("ID", "FROM", "SUBJECT")
		t.indent = "  "
		t.flex = 2
		for i, row := range r.Targets {
			if i == maxListedResults {
				break
			}
			subject, from := "", ""
			if i < len(set.Targets) && set.Targets[i].Summary != nil {
				s := set.Targets[i].Summary
				subject = firstNonEmpty(strings.TrimSpace(s.Subject), "(no subject)")
				from = truncate(senderName(s.From), 24)
			}
			t.add(ids.of(row.Account, firstNonEmpty(row.ResultMessageID, row.ID)), from, subject)
		}
		t.render(w, screenWidth(f), boldStyle(f))
		if extra := len(r.Targets) - maxListedResults; extra > 0 {
			fmt.Fprintf(w, "  and %s more\n", thousands(extra))
		}
		from := map[string]bool{}
		for _, row := range r.Targets {
			if row.SourceFolder != nil {
				from[folderSummaryText(*row.SourceFolder)] = true
			}
		}
		source := ""
		if len(from) == 1 {
			for name := range from {
				source = name
			}
		}
		if len(r.UndoCommands) > 0 {
			fmt.Fprintln(w)
		}
		writeUndo(w, "them", source, r.UndoCommands)
	}
}

// explain tells which messages did not change and why.
func (e bulkPartialFailureError) explain() explanation {
	failed := e.counts.Failed + e.counts.Skipped
	ex := explanation{message: fmt.Sprintf("%d of %d messages could not be changed", failed, e.counts.Total)}
	notFound := false
	for _, row := range e.targets {
		if row.Status == "succeeded" {
			continue
		}
		label := e.labels[row.ID]
		if label == "" {
			label = "message " + shortID(row.Account, row.ID)
		}
		reason := row.Reason
		switch row.ErrorCode {
		case errs.CodeMessageNotFound:
			reason = "not found"
			notFound = true
		case errs.CodeUnsupportedCapability:
			reason = "the account or its server does not allow this"
		}
		status := "Failed"
		if row.Status == "skipped" {
			// Not tried, because an earlier error stopped the account.
			status = "Not tried"
		}
		ex.lines = append(ex.lines, fmt.Sprintf("%s: %s (%s)", status, label, plainField(reason)))
		if len(ex.lines) == maxListedResults {
			break
		}
	}
	switch e.counts.Succeeded {
	case 0:
	case 1:
		ex.next = "The other message was changed. "
	default:
		ex.next = fmt.Sprintf("The other %d messages were changed. ", e.counts.Succeeded)
	}
	switch {
	case notFound:
		ex.next += "Ids change when mail moves to another folder; search again to get current ids."
	case e.counts.Succeeded > 0:
		ex.next += "Search again for current ids before you try the others again."
	default:
		ex.next = "Nothing was changed."
	}
	return ex
}

func targetLabels(targets []mutationTarget) map[string]string {
	out := make(map[string]string, len(targets))
	for _, t := range targets {
		if t.Summary != nil {
			out[t.MessageID] = targetLabel(t)
		}
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] == '"' && len(s) > 1 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Flags that pick messages. The apply command drops them and names the
// exact messages the preview showed instead.
var selectorFlagNames = []string{"id", "ids-file", "internet-id", "query", "all", "latest", "first", "max-targets", "folder", "account", "all-accounts", "dry-run", "apply"}

// maxInlineApplyCommand is how long an apply command with the ids inline
// may get before maildo puts the ids in a file instead.
const maxInlineApplyCommand = 400

// applyCommandFor is the command that makes a previewed change: the same
// command with the resolved account and the full ids of the messages the
// preview showed, never the search again. Mail that arrives later is not
// touched, and switching the active account does not change the target.
// Many ids go into a file in previews/ next to the config.
func applyCommandFor(f *rungrad.Factory, cmd *cobra.Command, set mutationTargetSet) (string, error) {
	if len(set.Targets) == 0 {
		return "", nil
	}
	accounts := map[string]bool{}
	for _, t := range set.Targets {
		accounts[t.Account.Name] = true
	}
	var base [][2]string
	if len(accounts) == 1 {
		base = append(base, [2]string{"account", set.Targets[0].Account.Name})
	}
	if len(set.Targets) == 1 {
		return commandLine(cmd, nil, selectorFlagNames, append(base, [2]string{"id", set.Targets[0].MessageID})), nil
	}
	if len(accounts) == 1 {
		inline := append([][2]string(nil), base...)
		for _, t := range set.Targets {
			inline = append(inline, [2]string{"id", t.MessageID})
		}
		inline = append(inline, [2]string{"apply", ""})
		if line := commandLine(cmd, nil, selectorFlagNames, inline); len(line) <= maxInlineApplyCommand {
			return line, nil
		}
	}
	qualified := len(accounts) > 1
	lines := make([]string, 0, len(set.Targets))
	for _, t := range set.Targets {
		if qualified {
			lines = append(lines, t.Account.Name+"\t"+t.MessageID)
		} else {
			lines = append(lines, t.MessageID)
		}
	}
	path, err := writePreviewIDs(f, previewIDsFilePrefix(cmd), lines)
	if err != nil {
		return "", err
	}
	return commandLine(cmd, nil, selectorFlagNames, append(base, [2]string{"ids-file", path}, [2]string{"apply", ""})), nil
}

// undoable reports whether op takes messages out of their folder, so its
// result can say how to move them back.
func undoable(op mail.MessageMutationOperation) bool {
	switch op {
	case mail.MutationArchive, mail.MutationMove, mail.MutationDelete, mail.MutationJunk, mail.MutationRestore:
		return true
	}
	return false
}

// undoCommands are the commands that move changed messages back to the
// folders they came from: one maildo move for each account and folder.
// Messages that did not move, and messages whose folder maildo does not
// know, are left out.
func undoCommands(f *rungrad.Factory, cmd *cobra.Command, op mail.MessageMutationOperation, rows []mail.AppliedMutationTarget) []string {
	if !undoable(op) {
		return nil
	}
	type group struct {
		account, folder string
		ids             []string
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, row := range rows {
		if row.Status != "succeeded" || (row.Changed != nil && !*row.Changed) || row.SourceFolder == nil {
			continue
		}
		folder := folderArg(*row.SourceFolder)
		if folder == "" {
			continue
		}
		key := row.Account + "\x00" + folder
		g := byKey[key]
		if g == nil {
			g = &group{account: row.Account, folder: folder}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.ids = append(g.ids, firstNonEmpty(row.ResultMessageID, row.ID))
	}
	var out []string
	for _, g := range groups {
		line, err := undoCommand(f, cmd, g.account, g.folder, g.ids)
		if err != nil {
			f.Infof("Warning: maildo could not write the undo command for %s: %v", g.folder, err)
			continue
		}
		out = append(out, line)
	}
	return out
}

// undoCommand moves ids back to folder. Like an apply command, it names
// the account and the exact messages, and many ids go into a file.
func undoCommand(f *rungrad.Factory, cmd *cobra.Command, account, folder string, ids []string) (string, error) {
	head := []string{"maildo", "move"}
	if flag := cmd.Flags().Lookup("config"); flag != nil && flag.Changed {
		head = append(head, "--config", shellQuote(flag.Value.String()))
	}
	head = append(head, "--account", shellQuote(account), "--to", shellQuote(folder))
	var tail []string
	if len(ids) > 1 {
		tail = append(tail, "--apply")
	}
	if f.Flags.JSON || f.Flags.JQ != "" || f.Flags.Template != "" {
		tail = append(tail, "--json")
	}
	parts := append([]string(nil), head...)
	for _, id := range ids {
		parts = append(parts, "--id", shellQuote(id))
	}
	if line := strings.Join(append(parts, tail...), " "); len(ids) == 1 || len(line) <= maxInlineApplyCommand {
		return line, nil
	}
	path, err := writePreviewIDs(f, "move", ids)
	if err != nil {
		return "", err
	}
	return strings.Join(append(append(head, "--ids-file", shellQuote(path)), tail...), " "), nil
}

// folderArg names a folder for --to: by its standard name when it has one,
// else by its path, else by its id.
func folderArg(folder mail.FolderSummary) string {
	return firstNonEmpty(folder.WellKnownName, folder.Path, folder.ID)
}

const (
	previewsDir   = "previews"
	previewMaxAge = 7 * 24 * time.Hour
)

// writePreviewIDs saves the ids for an apply or undo command, one line
// each (account, tab, id when several accounts are involved). prefix names
// the command that may read the file. It also clears out files older than
// a week. This happens even with --dry-run, since the printed command
// needs the file.
func writePreviewIDs(f *rungrad.Factory, prefix string, lines []string) (string, error) {
	dir := filepath.Join(filepath.Dir(f.ConfigPath()), previewsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errs.NewWithDetails(errs.CodeResourceLimitExceeded, "could not save the ids for the apply command", map[string]any{"cause": err.Error()})
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > previewMaxAge {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	name := fmt.Sprintf("%s-%s-%s.txt", prefix, time.Now().Format("20060102-150405"), hex.EncodeToString(sum[:])[:8])
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", errs.NewWithDetails(errs.CodeResourceLimitExceeded, "could not save the ids for the apply command", map[string]any{"cause": err.Error()})
	}
	return path, nil
}

// accountIn names where messages are: " in work", " in 3 accounts".
func accountIn(sel provider.AccountSelection) string {
	switch len(sel.Accounts) {
	case 0:
		return ""
	case 1:
		return " in " + sel.Accounts[0].Name
	default:
		return fmt.Sprintf(" in %d accounts", len(sel.Accounts))
	}
}
