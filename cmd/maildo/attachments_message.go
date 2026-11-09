package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

const (
	attachmentSelectionAll          = "all"
	attachmentSelectionName         = "name"
	attachmentSelectionAttachmentID = "attachment_id"

	attachmentStatusPlanned    = "planned"
	attachmentStatusDownloaded = "downloaded"
	attachmentStatusSkipped    = "skipped"
	attachmentStatusFailed     = "failed"

	attachmentReasonUnsupportedKind = "unsupported_kind"
	attachmentReasonNotAttempted    = "not_attempted"
)

type attachmentListTarget struct {
	DraftID  string
	Selector mail.MessageSelector
}

// downloadAttachmentSelection says which attachments to save: all of them,
// some by name, or some by id. Only one of these is set.
type downloadAttachmentSelection struct {
	Kind  string
	Names []string
	IDs   []string
}

// attachmentMessageTarget is the message whose attachments a command works
// on, with the accounts that were searched to find it. By the time the server
// is asked for attachments, a search or Message-ID has become a message id.
type attachmentMessageTarget struct {
	Selection         provider.AccountSelection
	Searched          *provider.AccountSelection
	Account           provider.ResolvedAccount
	Selector          mail.MessageSelector
	MessageID         string
	InternetMessageID string
}

// selectedMessageAttachment follows one attachment from picking it, through
// choosing its file name and saving it, to the result.
type selectedMessageAttachment struct {
	Attachment mail.Attachment
	Index      int
	Download   bool
	Row        attachmentDownloadRow
}

// parseAttachmentListTarget works out whether `attachments list` is about a
// draft or a message. A draft belongs to one account, so --draft-id takes
// at most one --account.
func parseAttachmentListTarget(cmd *cobra.Command) (attachmentListTarget, error) {
	flags := cmd.Flags()
	hasDraft := flags.Changed("draft-id")
	hasMessage := flags.Changed("id") || flags.Changed("internet-id") || flags.Changed("query") || flags.Changed("latest") || flags.Changed("first")
	if hasDraft && hasMessage {
		return attachmentListTarget{}, errs.New(errs.CodeInvalidSelector, "give either --draft-id or a message (--id, --internet-id or --query), not both")
	}
	if hasDraft {
		allAccounts, _ := flags.GetBool("all-accounts")
		if allAccounts {
			return attachmentListTarget{}, errs.New(errs.CodeInvalidSelector, "a draft belongs to one account, so --draft-id cannot be used with --all-accounts")
		}
		accounts, _ := flags.GetStringArray("account")
		if len(accounts) > 1 {
			return attachmentListTarget{}, errs.New(errs.CodeInvalidSelector, "a draft belongs to one account, so give only one --account with --draft-id")
		}
		id, err := requiredDraftIDFlag(cmd)
		if err != nil {
			return attachmentListTarget{}, err
		}
		return attachmentListTarget{DraftID: id}, nil
	}
	selector, err := parseCommandSelector(cmd, false)
	if err != nil {
		return attachmentListTarget{}, err
	}
	return attachmentListTarget{Selector: selector}, nil
}

// attachmentsListMessage finds the one message the flags point at and lists
// its attachments.
func (h *handlers) attachmentsListMessage(f *rungrad.Factory, cmd *cobra.Command, selector mail.MessageSelector) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		target, err := h.resolveMessageAttachmentListTarget(f, cmd, clientScope, selector)
		if err != nil {
			return err
		}
		var list mail.MessageAttachmentList
		if err := provider.WithAccountClient(cmd.Context(), clientScope, target.Account, provider.ClientAccessMessageAttachmentsRead, func(reader provider.MessageAttachmentReader) error {
			var err error
			list, err = reader.ListMessageAttachments(cmd.Context(), mail.MessageAttachmentListRequest{MessageID: target.MessageID})
			return err
		}); err != nil {
			return err
		}
		sourceInternetID := target.InternetMessageID
		if sourceInternetID == "" {
			sourceInternetID = list.InternetMessageID
		}
		result := messageAttachmentsResult{
			AccountSelection: target.Selection,
			Operation:        "attachments_list",
			SourceID:         target.MessageID,
			SourceInternetID: sourceInternetID,
			SelectorKind:     string(selector.Kind),
			Attachments:      list.Attachments,
			Count:            len(list.Attachments),
			ProviderMeta:     cloneProviderMetadataForResult(list.ProviderMeta),
		}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  messageAttachmentsMeta(cmd.CommandPath(), target.Selection, result.Operation, selector, target.MessageID, result.Count, target.Searched),
			Human: humanMessageAttachments(result),
			Plain: plainMessageAttachments(result),
		})
	})
}

// attachmentsDownload goes in this order: check the --to folder, find the
// message, list its attachments, choose every file name, and only then
// download the files. A bad name stops the command before anything is saved.
func (h *handlers) attachmentsDownload(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		outputDir, err := requiredDownloadDirectory(cmd)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("draft-id") {
			return errs.New(errs.CodeInvalidSelector, "attachments download saves attachments of messages; it does not take --draft-id")
		}
		selector, err := parseCommandSelector(cmd, false)
		if err != nil {
			return err
		}
		selection, err := parseDownloadAttachmentSelection(cmd)
		if err != nil {
			return err
		}
		target, err := h.resolveMessageAttachmentDownloadTarget(f, cmd, clientScope, selector)
		if err != nil {
			return err
		}
		var list mail.MessageAttachmentList
		err = provider.WithAccountClient(cmd.Context(), clientScope, target.Account, provider.ClientAccessMessageAttachmentsRead, func(reader provider.MessageAttachmentReader) error {
			var err error
			list, err = reader.ListMessageAttachments(cmd.Context(), mail.MessageAttachmentListRequest{MessageID: target.MessageID})
			if err != nil {
				return err
			}
			sourceInternetID := target.InternetMessageID
			if sourceInternetID == "" {
				sourceInternetID = list.InternetMessageID
			}
			selected, err := resolveDownloadAttachments(list.Attachments, selection)
			if err != nil {
				return err
			}
			if err := planAttachmentFilenames(selected, outputDir, overwriteFlag(cmd)); err != nil {
				return err
			}
			result := downloadResultFromSelection(target, sourceInternetID, selection.Kind, outputDir, selected, list.ProviderMeta)
			if f.DryRun() {
				result.DownloadedCount = 0
				command := changeCommand(cmd, nil, [][2]string{{"account", target.Account.Name}})
				return writeReadablePreview(f, previewMessageAttachmentDownload(target.Account, result), humanDownloadPreview(result, command))
			}
			if err := ensureDownloadDir(outputDir); err != nil {
				return err
			}
			for i := range selected {
				if !selected[i].Download {
					continue
				}
				selected[i].Row.Status = attachmentStatusFailed
				content, err := reader.DownloadMessageAttachment(cmd.Context(), mail.MessageAttachmentDownloadRequest{
					MessageID:    target.MessageID,
					AttachmentID: selected[i].Attachment.ID,
				})
				if err == nil {
					err = writeDownloadedAttachment(selected[i].Row.LocalPath, selected[i].Row.SafeBasename, overwriteFlag(cmd), content)
				} else if content.Content != nil {
					_ = content.Content.Close()
				}
				if err != nil {
					selected[i].Row.ErrorCode = errorCode(err)
					result = downloadResultFromSelection(target, sourceInternetID, selection.Kind, outputDir, selected, list.ProviderMeta)
					// Earlier attachments may already be saved, so this is a
					// partial failure, not a plain error.
					markDownloadFailureRows(&result, i)
					if result.DownloadedCount > 0 {
						return partialAttachmentDownloadError{err: err, result: result}
					}
					return err
				}
				selected[i].Row.Status = attachmentStatusDownloaded
				if info, err := os.Stat(selected[i].Row.LocalPath); err == nil {
					selected[i].Row.SavedSize = info.Size()
				}
			}
			result = downloadResultFromSelection(target, sourceInternetID, selection.Kind, outputDir, selected, list.ProviderMeta)
			return f.WriteResultWithMeta(result, messageAttachmentDownloadMeta(cmd.CommandPath(), result), humanMessageAttachmentDownload(result))
		})
		return err
	})
}

func requiredDownloadDirectory(cmd *cobra.Command) (string, error) {
	if !cmd.Flags().Changed("to") {
		return "", rungrad.NewError(rungrad.ExitUsage, "say where to save the attachments with --to <folder>")
	}
	dir, _ := cmd.Flags().GetString("to")
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", rungrad.NewError(rungrad.ExitUsage, "--to needs a folder")
	}
	return filepath.Clean(dir), nil
}

func overwriteFlag(cmd *cobra.Command) bool {
	overwrite, _ := cmd.Flags().GetBool("overwrite")
	return overwrite
}

func parseDownloadAttachmentSelection(cmd *cobra.Command) (downloadAttachmentSelection, error) {
	flags := cmd.Flags()
	all, _ := flags.GetBool("all")
	names, _ := flags.GetStringArray("name")
	ids, _ := flags.GetStringArray("attachment-id")
	families := 0
	if all {
		families++
	}
	if len(names) > 0 {
		families++
	}
	if len(ids) > 0 {
		families++
	}
	if families != 1 {
		return downloadAttachmentSelection{}, errs.New(errs.CodeInvalidSelector, "pick the attachments with one of --all, --name or --attachment-id")
	}
	if all {
		return downloadAttachmentSelection{Kind: attachmentSelectionAll}, nil
	}
	if len(names) > 0 {
		clean, err := cleanUniqueSelectionValues(names, "name")
		if err != nil {
			return downloadAttachmentSelection{}, err
		}
		return downloadAttachmentSelection{Kind: attachmentSelectionName, Names: clean}, nil
	}
	clean, err := cleanUniqueSelectionValues(ids, "attachment_id")
	if err != nil {
		return downloadAttachmentSelection{}, err
	}
	return downloadAttachmentSelection{Kind: attachmentSelectionAttachmentID, IDs: clean}, nil
}

func cleanUniqueSelectionValues(values []string, field string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, errs.NewWithDetails(errs.CodeInvalidSelector, "--"+strings.ReplaceAll(field, "_", "-")+" requires a value", map[string]any{"field": field})
		}
		if _, ok := seen[value]; ok {
			return nil, errs.NewWithDetails(errs.CodeInvalidSelector, "the same attachment is named twice", map[string]any{"field": field, "value": value})
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

// resolveMessageAttachmentListTarget lets --query search several accounts,
// and returns the one account the message belongs to.
func (h *handlers) resolveMessageAttachmentListTarget(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, selector mail.MessageSelector) (attachmentMessageTarget, error) {
	if selector.Kind == mail.SelectorQuery {
		scope, err := h.resolveSelectorScope(f, cmd, clientScope, selector)
		if err != nil {
			return attachmentMessageTarget{}, err
		}
		return attachmentMessageTarget{
			Selection:         scope.Selection,
			Searched:          scope.Searched,
			Account:           scope.Owner,
			Selector:          selector,
			MessageID:         scope.Selected.Message.ID,
			InternetMessageID: scope.Selected.Message.InternetMessageID,
		}, nil
	}
	scope, err := h.resolveSelectorScope(f, cmd, clientScope, selector)
	if err != nil {
		return attachmentMessageTarget{}, err
	}
	target := attachmentMessageTarget{Selection: scope.Selection, Account: scope.Owner, Selector: selector}
	if selector.Kind == mail.SelectorID {
		target.MessageID = selector.ID
		return target, nil
	}
	msg, err := h.resolveInternetMessageAttachmentTarget(cmd.Context(), clientScope, scope.Owner, selector.InternetID)
	if err != nil {
		return attachmentMessageTarget{}, err
	}
	target.MessageID = msg.ID
	target.InternetMessageID = msg.InternetMessageID
	return target, nil
}

// resolveMessageAttachmentDownloadTarget works in one account only. A
// --query is searched now, right before the download.
func (h *handlers) resolveMessageAttachmentDownloadTarget(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, selector mail.MessageSelector) (attachmentMessageTarget, error) {
	sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
	if err != nil {
		return attachmentMessageTarget{}, err
	}
	acct := sel.Accounts[0]
	target := attachmentMessageTarget{Selection: sel, Account: acct, Selector: selector}
	switch selector.Kind {
	case mail.SelectorQuery:
		selected, err := h.resolveReplySource(cmd.Context(), clientScope, sel, selector.Query)
		if err != nil {
			return attachmentMessageTarget{}, err
		}
		target.MessageID = selected.Message.ID
		target.InternetMessageID = selected.Message.InternetMessageID
	case mail.SelectorID:
		target.MessageID = selector.ID
	case mail.SelectorInternetID:
		msg, err := h.resolveInternetMessageAttachmentTarget(cmd.Context(), clientScope, acct, selector.InternetID)
		if err != nil {
			return attachmentMessageTarget{}, err
		}
		target.MessageID = msg.ID
		target.InternetMessageID = msg.InternetMessageID
	default:
		return attachmentMessageTarget{}, errs.New(errs.CodeInvalidSelector, "attachments download requires --id, --internet-id, or --query")
	}
	return target, nil
}

// resolveInternetMessageAttachmentTarget finds the message for a Message-ID
// header. It reads only the headers, not the body.
func (h *handlers) resolveInternetMessageAttachmentTarget(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, internetID string) (mail.Message, error) {
	var msg mail.Message
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesRead, func(reader provider.MessageReader) error {
		var err error
		msg, err = reader.ReadMessage(ctx, mail.ReadMessageRequest{
			Selector:     mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorInternetID, InternetID: internetID},
			MetadataOnly: true,
		})
		return err
	})
	return msg, err
}

// resolveDownloadAttachments picks the attachments to save. With --all, an
// attachment that is not a file is listed as left out; asking for such an
// attachment by name or id is an error.
func resolveDownloadAttachments(attachments []mail.Attachment, selection downloadAttachmentSelection) ([]selectedMessageAttachment, error) {
	switch selection.Kind {
	case attachmentSelectionAll:
		out := make([]selectedMessageAttachment, 0, len(attachments))
		for i, a := range attachments {
			item := selectedMessageAttachment{Attachment: a, Index: i, Download: downloadableAttachment(a)}
			item.Row = rowForAttachment(a, attachmentStatusPlanned)
			if !item.Download {
				item.Row.Status = attachmentStatusSkipped
				item.Row.Reason = attachmentReasonUnsupportedKind
			}
			out = append(out, item)
		}
		return out, nil
	case attachmentSelectionName:
		return resolveNamedDownloadAttachments(attachments, selection.Names)
	case attachmentSelectionAttachmentID:
		return resolveIDDownloadAttachments(attachments, selection.IDs)
	default:
		return nil, errs.New(errs.CodeInvalidSelector, "unsupported attachment selection")
	}
}

func resolveNamedDownloadAttachments(attachments []mail.Attachment, names []string) ([]selectedMessageAttachment, error) {
	selected := map[int]struct{}{}
	for _, name := range names {
		var matches []int
		for i, a := range attachments {
			if a.Name == name {
				matches = append(matches, i)
			}
		}
		switch len(matches) {
		case 0:
			return nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"name": name})
		case 1:
			i := matches[0]
			if !downloadableAttachment(attachments[i]) {
				return nil, unsupportedAttachment(attachments[i])
			}
			selected[i] = struct{}{}
		default:
			ids := make([]string, 0, len(matches))
			for _, i := range matches {
				ids = append(ids, attachments[i].ID)
			}
			return nil, errs.NewWithDetails(errs.CodeAmbiguousAttachment, "the message has more than one attachment with this name; pick one with --attachment-id", map[string]any{
				"name":          name,
				"candidate_ids": ids,
			})
		}
	}
	return selectedInProviderOrder(attachments, selected), nil
}

func resolveIDDownloadAttachments(attachments []mail.Attachment, ids []string) ([]selectedMessageAttachment, error) {
	selected := map[int]struct{}{}
	for _, id := range ids {
		found := -1
		for i, a := range attachments {
			if a.ID == id {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"attachment_id": id})
		}
		if !downloadableAttachment(attachments[found]) {
			return nil, unsupportedAttachment(attachments[found])
		}
		selected[found] = struct{}{}
	}
	return selectedInProviderOrder(attachments, selected), nil
}

// selectedInProviderOrder puts the picked attachments back in the message's
// order, so the order of the flags does not matter.
func selectedInProviderOrder(attachments []mail.Attachment, selected map[int]struct{}) []selectedMessageAttachment {
	out := make([]selectedMessageAttachment, 0, len(selected))
	for i, a := range attachments {
		if _, ok := selected[i]; !ok {
			continue
		}
		item := selectedMessageAttachment{Attachment: a, Index: i, Download: true}
		item.Row = rowForAttachment(a, attachmentStatusPlanned)
		out = append(out, item)
	}
	return out
}

func downloadableAttachment(a mail.Attachment) bool {
	return a.Kind == mail.AttachmentKindFile
}

func unsupportedAttachment(a mail.Attachment) error {
	return errs.NewWithDetails(errs.CodeUnsupportedAttachment, "this attachment is not a file, for example an attached Outlook item or a link, so maildo cannot save it", map[string]any{
		"attachment_id": a.ID,
		"name":          a.Name,
		"kind":          string(a.Kind),
	})
}

func rowForAttachment(a mail.Attachment, status string) attachmentDownloadRow {
	row := attachmentDownloadRow{
		ID:           a.ID,
		Name:         a.Name,
		ContentType:  a.ContentType,
		Size:         a.Size,
		IsInline:     a.IsInline,
		Kind:         a.Kind,
		ProviderMeta: cloneProviderMetadataForResult(a.ProviderMeta),
		Status:       status,
	}
	row.EncodedSize, row.DecodedSize = a.KnownSizes()
	return row
}

// planAttachmentFilenames chooses and checks every file name before anything
// is downloaded, so a bad name later in the list cannot leave earlier files
// behind.
func planAttachmentFilenames(selected []selectedMessageAttachment, dir string, overwrite bool) error {
	used := map[string]struct{}{}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return rungrad.NewError(rungrad.ExitUsage, "the --to folder is not a valid path")
	}
	for i := range selected {
		if !selected[i].Download {
			continue
		}
		base, err := safeAttachmentBasename(selected[i].Attachment.Name, selected[i].Index+1)
		if err != nil {
			return err
		}
		finalBase := uniqueAttachmentBasename(base, used)
		finalPath := filepath.Join(dir, finalBase)
		if err := verifyInsideDirectory(absDir, finalPath); err != nil {
			return err
		}
		if err := checkFinalPath(finalPath, overwrite); err != nil {
			return err
		}
		selected[i].Row.SafeBasename = finalBase
		selected[i].Row.LocalPath = finalPath
	}
	return nil
}

// safeAttachmentBasename turns an attachment name from a message into a
// file name inside the download folder. A name such as "../../etc/passwd"
// becomes "passwd": only the last path element is kept, control characters
// and characters Windows forbids become "_", and leading dots go, so no
// hidden file appears. Empty or reserved names become attachment-NNN.
func safeAttachmentBasename(name string, index int) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	parts := strings.Split(strings.ReplaceAll(cleaned, "\\", "/"), "/")
	base := ""
	for i := len(parts) - 1; i >= 0; i-- {
		if part := strings.TrimSpace(parts[i]); part != "" && part != "." && part != ".." {
			base = part
			break
		}
	}
	base = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, base)
	base = strings.TrimLeft(strings.TrimRight(base, " ."), ". ")
	if base == "" || windowsDeviceBasename(base) {
		base = fmt.Sprintf("attachment-%03d", index)
	}
	return base, nil
}

func windowsDeviceBasename(base string) bool {
	stem := base
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return true
	}
	return false
}

// uniqueAttachmentBasename gives two attachments with the same name different
// file names. It also checks the names it makes: for `report.txt`,
// `report.txt` and `report-2.txt`, the third does not get the second's name.
func uniqueAttachmentBasename(base string, used map[string]struct{}) string {
	candidate := base
	for suffix := 2; ; suffix++ {
		key := strings.ToLower(candidate)
		if _, ok := used[key]; !ok {
			used[key] = struct{}{}
			return candidate
		}
		candidate = suffixedAttachmentBasename(base, suffix)
	}
}

func suffixedAttachmentBasename(base string, suffix int) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = base
		ext = ""
	}
	return stem + "-" + strconv.Itoa(suffix) + ext
}

func verifyInsideDirectory(absDir, finalPath string) error {
	absFinal, err := filepath.Abs(finalPath)
	if err != nil {
		return rungrad.NewError(rungrad.ExitUsage, "could not work out where to save the attachment")
	}
	rel, err := filepath.Rel(absDir, absFinal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rungrad.NewError(rungrad.ExitUsage, "an attachment would be saved outside the --to folder, so maildo stopped")
	}
	return nil
}

func checkFinalPath(path string, overwrite bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return rungrad.NewError(rungrad.ExitUsage, "could not check the file the attachment would be saved as")
	}
	if info.IsDir() {
		return rungrad.NewError(rungrad.ExitUsage, "a folder with the attachment's name is in the --to folder")
	}
	if !overwrite {
		return outputFileExists(path)
	}
	return nil
}

// outputFileExists names the file a download would overwrite. Downloads
// check every file name before saving any, so nothing was saved.
func outputFileExists(path string) error {
	name := filepath.Base(path)
	return errs.NewWithDetails(errs.CodeOutputFileExists, fmt.Sprintf("the file %s already exists", name), map[string]any{
		"file": name,
		"hint": "add --overwrite to replace it, or save to another folder with --to",
	})
}

func ensureDownloadDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return rungrad.NewError(rungrad.ExitUsage, "could not create the --to folder")
		}
		return nil
	}
	if err != nil {
		return rungrad.NewError(rungrad.ExitUsage, "could not check the --to folder")
	}
	if !info.IsDir() {
		return rungrad.NewError(rungrad.ExitUsage, "--to names a file, not a folder")
	}
	return nil
}

// writeDownloadedAttachment writes one attachment to a temporary file in the
// --to folder, stops one byte past the size limit, and moves the file into
// place only when it is complete and synced. Without --overwrite it links the
// file into place, which fails if a file with that name appeared meanwhile.
func writeDownloadedAttachment(path, safeBase string, overwrite bool, content mail.MessageAttachmentContent) error {
	if content.Content == nil {
		return rungrad.NewError(rungrad.ExitAPI, "the server sent no content for the attachment")
	}
	if content.DeclaredSize > mail.MaxMessageAttachmentDownloadBytes {
		_ = content.Content.Close()
		return errs.NewWithDetails(errs.CodeAttachmentTooLarge, "the attachment is larger than 25 MB, the most maildo downloads", map[string]any{
			"attachment_id": content.AttachmentID,
			"file_name":     safeBase,
			"size":          content.DeclaredSize,
			"max_size":      mail.MaxMessageAttachmentDownloadBytes,
		})
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		_ = content.Content.Close()
		return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	limit := mail.MaxMessageAttachmentDownloadBytes + 1
	written, copyErr := io.Copy(tmp, io.LimitReader(content.Content, limit))
	closeContentErr := content.Content.Close()
	if copyErr != nil {
		_ = tmp.Close()
		return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
	}
	if closeContentErr != nil {
		_ = tmp.Close()
		if _, ok := closeContentErr.(interface{ ExitCode() int }); ok {
			return closeContentErr
		}
		return rungrad.NewError(rungrad.ExitAPI, "the attachment download stopped before the end")
	}
	if written > mail.MaxMessageAttachmentDownloadBytes {
		_ = tmp.Close()
		return errs.NewWithDetails(errs.CodeAttachmentTooLarge, "the attachment is larger than 25 MB, the most maildo downloads", map[string]any{
			"attachment_id": content.AttachmentID,
			"file_name":     safeBase,
			"size":          written,
			"max_size":      mail.MaxMessageAttachmentDownloadBytes,
		})
	}
	if content.DeclaredSize > 0 && written < content.DeclaredSize {
		_ = tmp.Close()
		return rungrad.NewError(rungrad.ExitAPI, "the attachment download stopped before the end")
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
	}
	if err := tmp.Close(); err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return rungrad.NewError(rungrad.ExitUsage, "a folder with the attachment's name is in the --to folder")
		}
		if !overwrite {
			return outputFileExists(path)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rungrad.NewError(rungrad.ExitUsage, "could not check the file the attachment would be saved as")
	}
	if overwrite {
		if err := os.Rename(tmpPath, path); err != nil {
			return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
		}
		removeTmp = false
	} else {
		// Checking first and renaming after would miss a file that another
		// download saves in between; a link fails when the name is taken.
		if err := os.Link(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return outputFileExists(path)
			}
			return rungrad.NewError(rungrad.ExitAPI, "could not save the attachment file")
		}
	}
	syncLocalDir(filepath.Dir(path))
	return nil
}

func syncLocalDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}

// downloadResultFromSelection builds the result, or the details of an error,
// from the picked attachments. Only saved attachments show a local path.
func downloadResultFromSelection(target attachmentMessageTarget, internetID, selectionKind, outputDir string, selected []selectedMessageAttachment, meta *mail.AttachmentProviderMetadata) messageAttachmentDownloadResult {
	rows := make([]attachmentDownloadRow, len(selected))
	downloaded, skipped, failed := 0, 0, 0
	for i, item := range selected {
		row := item.Row
		switch row.Status {
		case attachmentStatusDownloaded:
			downloaded++
		case attachmentStatusSkipped:
			skipped++
		case attachmentStatusFailed:
			failed++
		}
		if row.Status != attachmentStatusDownloaded {
			row.LocalPath = ""
		}
		rows[i] = row
	}
	return messageAttachmentDownloadResult{
		AccountSelection: target.Selection,
		Operation:        "attachments_download",
		SourceID:         target.MessageID,
		SourceInternetID: internetID,
		SelectorKind:     string(target.Selector.Kind),
		SelectionKind:    selectionKind,
		OutputDirectory:  outputDir,
		SelectedCount:    len(rows),
		DownloadedCount:  downloaded,
		SkippedCount:     skipped,
		FailedCount:      failed,
		Attachments:      rows,
		ProviderMeta:     cloneProviderMetadataForResult(meta),
	}
}

// markDownloadFailureRows marks the attachments after a failed one as not
// tried, and counts again.
func markDownloadFailureRows(result *messageAttachmentDownloadResult, failedIndex int) {
	for i := range result.Attachments {
		switch {
		case i == failedIndex:
			result.Attachments[i].Status = attachmentStatusFailed
			if result.Attachments[i].ErrorCode == "" {
				result.Attachments[i].ErrorCode = resultErrorCode(result.Attachments[i])
			}
		case i > failedIndex && result.Attachments[i].Status == attachmentStatusPlanned:
			result.Attachments[i].Status = attachmentStatusSkipped
			result.Attachments[i].Reason = attachmentReasonNotAttempted
		}
	}
	downloaded, skipped, failed := 0, 0, 0
	for _, row := range result.Attachments {
		switch row.Status {
		case attachmentStatusDownloaded:
			downloaded++
		case attachmentStatusSkipped:
			skipped++
		case attachmentStatusFailed:
			failed++
		}
	}
	result.DownloadedCount = downloaded
	result.SkippedCount = skipped
	result.FailedCount = failed
}

func resultErrorCode(row attachmentDownloadRow) string {
	return row.ErrorCode
}

func errorCode(err error) string {
	if d, ok := err.(interface{ ErrorDetails() any }); ok {
		if m, ok := d.ErrorDetails().(map[string]any); ok {
			if code, ok := m["code"].(string); ok {
				return code
			}
		}
	}
	return ""
}
