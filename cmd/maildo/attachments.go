package main

import (
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// attachmentsModule holds the commands for attachments of messages and drafts.
type attachmentsModule struct {
	h    *handlers
	defs []cmdDef
}

func newAttachmentsModule(h *handlers) attachmentsModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	listModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	draftIDFlag := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		cmd.Flags().String("draft-id", "", "Draft id, or a short id from maildo")
	}
	messageSelectorFlags := func(cmd *cobra.Command) {
		cmd.Flags().String("id", "", "Message id, or a short id from list or search")
		cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
		cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
		cmd.Flags().Bool("latest", false, "With --query, use the newest match")
		cmd.Flags().Bool("first", false, "With --query, use the first match; matches come newest first, so this is the same as --latest")
	}
	defs := []cmdDef{
		{
			path: "attachments", use: "attachments", short: "Work with the attachments of messages and drafts", groupID: "attachments",
			examples: []string{"maildo attachments list 3c1f2a7", "maildo attachments download 3c1f2a7 --attachment-id 1 --to ./attachments", "maildo attachments add --draft-id 5d2e8b1 ./quote.pdf"},
			related:  []string{"read", "draft read", "draft update"},
		},
		{
			path: "attachments list", use: "list [id]", short: "List the attachments of a message or draft",
			examples: []string{
				`maildo attachments list 3c1f2a7`,
				`maildo attachments list --id 3c1f2a7`,
				`maildo attachments list --id 3c1f2a7 --account work --json`,
				`maildo attachments list --internet-id "<message-id@example.com>" --account work --json`,
				`maildo attachments list --query "invoice has:attachment" --latest --all-accounts --plain`,
				`maildo attachments list --draft-id 5d2e8b1`,
			},
			related:      []string{"attachments download", "attachments add", "attachments remove", "read", "draft read"},
			outputModes:  listModes,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
				cmd.Flags().String("draft-id", "", "Draft id, or a short id from maildo")
				messageSelectorFlags(cmd)
			},
			run: h.attachmentsList,
		},
		{
			path: "attachments download", use: "download [id]", short: "Save a message's attachments to a folder",
			examples: []string{
				`maildo attachments download 3c1f2a7 --all --to ./attachments`,
				`maildo attachments download --id 3c1f2a7 --account work --all --to ./attachments`,
				`maildo attachments download --id 3c1f2a7 --attachment-id 1 --to ./attachments --json`,
				`maildo attachments download --id 3c1f2a7 --name invoice.pdf --to ./attachments`,
				`maildo attachments download --query "invoice has:attachment" --latest --attachment-id 2 --to ./attachments --json`,
				`maildo attachments download --id 3c1f2a7 --all --to ./attachments --dry-run`,
			},
			related:      []string{"attachments list", "read", "search"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				messageSelectorFlags(cmd)
				cmd.Flags().String("to", "", "Folder to save the attachments in (created if missing)")
				cmd.Flags().Bool("all", false, "Save every attached file")
				cmd.Flags().StringArray("name", nil, "Name of an attachment to save (repeatable)")
				cmd.Flags().StringArray("attachment-id", nil, "Id of an attachment to save, from attachments list (repeatable)")
				cmd.Flags().Bool("overwrite", false, "Replace files that already exist in the folder")
			},
			run: h.attachmentsDownload,
		},
		{
			path: "attachments add", use: "add <path...>", short: "Add files to a draft",
			examples: []string{
				`maildo attachments add --draft-id 5d2e8b1 ./quote.pdf`,
				`maildo attachments add --draft-id 5d2e8b1 --account work ./quote.pdf --json`,
				`maildo attachments add --draft-id 5d2e8b1 ./quote.pdf --dry-run`,
			},
			related:      []string{"attachments list", "attachments remove", "draft read"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MinimumNArgs(1),
			configure:    draftIDFlag,
			run:          h.attachmentsAdd,
		},
		{
			path: "attachments remove", use: "remove", short: "Remove an attached file from a draft",
			examples: []string{
				`maildo attachments remove --draft-id 5d2e8b1 --account work --name quote.pdf`,
				`maildo attachments remove --draft-id 5d2e8b1 --attachment-id 2 --json`,
				`maildo attachments remove --draft-id 5d2e8b1 --name quote.pdf --dry-run`,
			},
			related:      []string{"attachments list", "attachments add", "draft read"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				draftIDFlag(cmd)
				cmd.Flags().String("name", "", "Name of the attachment to remove")
				cmd.Flags().String("attachment-id", "", "Id of the attachment to remove, from attachments list")
			},
			run: h.attachmentsRemove,
		},
	}
	defs = markMailboxAuth(defs,
		"attachments list",
		"attachments download",
		"attachments add",
		"attachments remove",
	)
	return attachmentsModule{h: h, defs: defs}
}

func (m attachmentsModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "attachments", Title: "Attachments"}}
}

func (m attachmentsModule) Commands() []*rungrad.Command {
	root := m.defs[0].command()
	for _, def := range m.defs[1:] {
		root.AddCommand(def.command())
	}
	return []*rungrad.Command{root}
}

func (m attachmentsModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

func (h *handlers) attachmentsList(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	target, err := parseAttachmentListTarget(cmd)
	if err != nil {
		return err
	}
	if target.DraftID != "" {
		return h.attachmentsListDraft(f, cmd, target.DraftID)
	}
	return h.attachmentsListMessage(f, cmd, target.Selector)
}

func (h *handlers) attachmentsListDraft(f *rungrad.Factory, cmd *cobra.Command, id string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftAttachmentsMutate); err != nil {
			return err
		}
		var draft mail.Draft
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftRead, func(reader provider.DraftReader) error {
			var err error
			draft, err = reader.ReadDraft(cmd.Context(), mail.ReadDraftRequest{DraftID: id, MetadataOnly: true})
			return err
		}); err != nil {
			return err
		}
		defer destroyAttachmentTokens(draft.Attachments)
		attachments := publicAttachments(draft.Attachments)
		result := draftAttachmentsResult{
			AccountSelection: sel,
			Operation:        "attachments_list",
			DraftID:          id,
			Attachments:      attachments,
			Count:            len(attachments),
			ProviderMeta:     cloneProviderMetadataForResult(draft.ProviderMeta),
		}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  attachmentsMeta(cmd.CommandPath(), sel, result.Operation, id, result.Count),
			Human: humanDraftAttachments(result),
			Plain: plainDraftAttachments(result),
		})
	})
}

func (h *handlers) attachmentsAdd(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftIDFlag(cmd)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftAttachmentsMutate); err != nil {
			return err
		}
		uploads, err := prevalidateAttachmentUploads(args)
		if err != nil {
			return err
		}
		defer destroyAttachmentUploads(uploads)
		var subject string
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftRead, func(reader provider.DraftReader) error {
			var draft mail.Draft
			draft, err = reader.ReadDraft(cmd.Context(), mail.ReadDraftRequest{DraftID: id, MetadataOnly: true})
			destroyAttachmentTokens(draft.Attachments)
			subject = draft.Subject
			return err
		}); err != nil {
			return err
		}
		if f.DryRun() {
			command := changeCommand(cmd, args, [][2]string{{"account", acct.Name}})
			return writeReadablePreview(f, previewAttachmentsAdd(acct, id, uploads), humanAttachmentChangePreview(acct, subject, uploadNames(uploads), true, command))
		}
		var added mail.AddDraftAttachmentsResult
		attempted := false
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftAttachmentsMutate, func(manager provider.DraftAttachmentManager) error {
			attempted = true
			var err error
			added, err = manager.AddDraftAttachments(cmd.Context(), mail.AddDraftAttachmentsRequest{DraftID: id, Uploads: uploads})
			return err
		})
		var logStatus mail.ActionLogStatus
		if attempted {
			change := audit.Change{Kind: "attachment", ID: id, Name: strings.Join(uploadNames(uploads), ", ")}
			if added.DraftID != "" && added.DraftID != id {
				change.NewID = added.DraftID
			}
			logStatus = h.logChange(f, cmd, acct, "attachments_add", change, err)
		}
		if err != nil {
			publicAdded := publicAttachments(added.Added)
			destroyAttachmentTokens(added.Added)
			destroyAttachmentTokens(added.FinalAttachments)
			// Some uploads may have gone through before one failed. Call it a
			// partial failure only then; when every upload went through and the
			// read afterwards failed, that is a different error.
			if len(publicAdded) > 0 && len(publicAdded) < len(uploads) {
				return partialAttachmentAddError{err: err, draftID: id, uploads: uploads, added: publicAdded}
			}
			return err
		}
		defer destroyAttachmentTokens(added.Added)
		defer destroyAttachmentTokens(added.FinalAttachments)
		finalAttachments := publicAttachments(added.FinalAttachments)
		result := draftAttachmentAddResult{
			AccountSelection:  sel,
			Operation:         "attachments_add",
			DraftID:           firstNonEmpty(added.DraftID, id),
			Added:             publicAttachments(added.Added),
			FinalAttachments:  finalAttachments,
			AttachmentCount:   len(finalAttachments),
			Uploads:           safeUploads(uploads),
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(added.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(added.ProviderMeta),
			Warnings:          added.Warnings,
			ActionLogStatus:   logStatus,
		}
		warnAll(f, added.Warnings)
		human := humanAttachmentChange(f, acct.Name, id, result.DraftID, subject, uploadNames(uploads), true, result.AttachmentCount)
		return f.WriteResultWithMeta(result, attachmentsMeta(cmd.CommandPath(), sel, result.Operation, id, result.AttachmentCount), human)
	})
}

func (h *handlers) attachmentsRemove(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftIDFlag(cmd)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftAttachmentsMutate); err != nil {
			return err
		}
		name, attachmentID, err := removeAttachmentTarget(cmd)
		if err != nil {
			return err
		}
		var draft mail.Draft
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftRead, func(reader provider.DraftReader) error {
			var err error
			draft, err = reader.ReadDraft(cmd.Context(), mail.ReadDraftRequest{DraftID: id, MetadataOnly: true})
			return err
		}); err != nil {
			return err
		}
		defer destroyAttachmentTokens(draft.Attachments)
		// Find the attachment in the draft as it is now. Inline attachments and
		// anything that is not a plain file are refused here.
		target, err := resolveDraftAttachment(draft.Attachments, name, attachmentID)
		if err != nil {
			return err
		}
		if err := validateRemovableAttachment(target); err != nil {
			return err
		}
		publicTarget := publicAttachment(target)
		if f.DryRun() {
			command := changeCommand(cmd, nil, [][2]string{{"account", acct.Name}})
			return writeReadablePreview(f, previewAttachmentsRemove(acct, id, publicTarget), humanAttachmentChangePreview(acct, draft.Subject, []string{publicTarget.Name}, false, command))
		}
		var requestToken mail.AttachmentExecutionToken
		if target.ExecutionToken != nil {
			requestToken = target.ExecutionToken.Clone()
		}
		if requestToken != nil {
			defer requestToken.Destroy()
		}
		var removed mail.RemoveDraftAttachmentResult
		attempted := false
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftAttachmentsMutate, func(manager provider.DraftAttachmentManager) error {
			attempted = true
			var err error
			removed, err = manager.RemoveDraftAttachment(cmd.Context(), mail.RemoveDraftAttachmentRequest{DraftID: id, AttachmentID: target.ID, ExecutionToken: requestToken})
			return err
		})
		var logStatus mail.ActionLogStatus
		if attempted {
			change := audit.Change{Kind: "attachment", ID: id, Name: publicTarget.Name}
			if removed.DraftID != "" && removed.DraftID != id {
				change.NewID = removed.DraftID
			}
			logStatus = h.logChange(f, cmd, acct, "attachments_remove", change, err)
		}
		if err != nil {
			destroyAttachmentTokens([]mail.Attachment{removed.Removed})
			destroyAttachmentTokens(removed.FinalAttachments)
			return err
		}
		defer destroyAttachmentTokens([]mail.Attachment{removed.Removed})
		defer destroyAttachmentTokens(removed.FinalAttachments)
		finalAttachments := publicAttachments(removed.FinalAttachments)
		result := draftAttachmentRemoveResult{
			AccountSelection:  sel,
			Operation:         "attachments_remove",
			DraftID:           firstNonEmpty(removed.DraftID, id),
			Removed:           mergeRemovedAttachment(publicTarget, publicAttachment(removed.Removed)),
			FinalAttachments:  finalAttachments,
			AttachmentCount:   len(finalAttachments),
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(removed.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(removed.ProviderMeta),
			Warnings:          removed.Warnings,
			ActionLogStatus:   logStatus,
		}
		warnAll(f, removed.Warnings)
		human := humanAttachmentChange(f, acct.Name, id, result.DraftID, draft.Subject, []string{result.Removed.Name}, false, result.AttachmentCount)
		return f.WriteResultWithMeta(result, attachmentsMeta(cmd.CommandPath(), sel, result.Operation, id, result.AttachmentCount), human)
	})
}

func requiredDraftIDFlag(cmd *cobra.Command) (string, error) {
	id, _ := cmd.Flags().GetString("draft-id")
	if strings.TrimSpace(id) == "" {
		return "", invalidComposeInput("draft_id", "--draft-id needs a draft id")
	}
	return strings.TrimSpace(id), nil
}

func prevalidateAttachmentUploads(paths []string) ([]mail.AttachmentUpload, error) {
	seenCanonical := map[string]string{}
	seenNames := map[string]struct{}{}
	uploads := make([]mail.AttachmentUpload, 0, len(paths))
	for _, raw := range paths {
		name := filepath.Base(filepath.Clean(raw))
		if strings.TrimSpace(raw) == "" || name == "." || name == string(filepath.Separator) {
			return nil, invalidComposeInput("path", "give the path of a file to attach")
		}
		canonical, err := filepath.EvalSymlinks(raw)
		if err != nil {
			return nil, invalidAttachmentFile(name, "could not open the file to attach")
		}
		// Refuse the same file twice, and two files with the same name: the
		// draft only gets the file name, never the local path.
		if prev, ok := seenCanonical[canonical]; ok {
			return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "the same file is given twice", map[string]any{
				"field":     "path",
				"file_name": name,
				"duplicate": prev,
			})
		}
		seenCanonical[canonical] = name
		if _, ok := seenNames[name]; ok {
			return nil, errs.NewWithDetails(errs.CodeInvalidComposeInput, "two files to attach have the same name", map[string]any{
				"field":     "path",
				"file_name": name,
			})
		}
		seenNames[name] = struct{}{}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, invalidAttachmentFile(name, "could not open the file to attach")
		}
		if !info.Mode().IsRegular() {
			return nil, invalidAttachmentFile(name, "only a regular file can be attached, not a folder or device")
		}
		if info.Size() >= mail.MaxSimpleAttachmentBytes {
			return nil, errs.NewWithDetails(errs.CodeAttachmentTooLarge, "the file is too large to attach; maildo attaches files smaller than 3 MB", map[string]any{
				"file_name": name,
				"size":      info.Size(),
				"max_size":  mail.MaxSimpleAttachmentBytes - 1,
			})
		}
		data, err := readBoundedAttachmentFile(canonical)
		if err != nil {
			return nil, invalidAttachmentFile(name, "could not read the file to attach")
		}
		if int64(len(data)) >= mail.MaxSimpleAttachmentBytes {
			return nil, errs.NewWithDetails(errs.CodeAttachmentTooLarge, "the file is too large to attach; maildo attaches files smaller than 3 MB", map[string]any{
				"file_name": name,
				"size":      len(data),
				"max_size":  mail.MaxSimpleAttachmentBytes - 1,
			})
		}
		contentType := mime.TypeByExtension(filepath.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		uploads = append(uploads, mail.AttachmentUpload{Name: name, ContentType: contentType, Size: int64(len(data)), Bytes: data})
	}
	return uploads, nil
}

func readBoundedAttachmentFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, mail.MaxSimpleAttachmentBytes+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func invalidAttachmentFile(name, message string) error {
	return errs.NewWithDetails(errs.CodeInvalidComposeInput, message, map[string]any{
		"field":     "path",
		"file_name": name,
	})
}

func safeUploads(in []mail.AttachmentUpload) []mail.AttachmentUpload {
	out := make([]mail.AttachmentUpload, len(in))
	for i, upload := range in {
		upload.Bytes = nil
		out[i] = upload
	}
	return out
}

func removeAttachmentTarget(cmd *cobra.Command) (string, string, error) {
	name, _ := cmd.Flags().GetString("name")
	attachmentID, _ := cmd.Flags().GetString("attachment-id")
	hasName := cmd.Flags().Changed("name")
	hasID := cmd.Flags().Changed("attachment-id")
	if hasName == hasID {
		return "", "", invalidComposeInput("attachment", "give either --name or --attachment-id")
	}
	name = strings.TrimSpace(name)
	attachmentID = strings.TrimSpace(attachmentID)
	if hasName && name == "" {
		return "", "", invalidComposeInput("name", "--name needs an attachment name")
	}
	if hasID && attachmentID == "" {
		return "", "", invalidComposeInput("attachment_id", "--attachment-id needs an attachment id")
	}
	return name, attachmentID, nil
}

func resolveDraftAttachment(attachments []mail.Attachment, name, id string) (mail.Attachment, error) {
	if id != "" {
		for _, a := range attachments {
			if a.ID == id {
				return a, nil
			}
		}
		return mail.Attachment{}, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{
			"attachment_id": id,
		})
	}
	matches := make([]mail.Attachment, 0, 1)
	for _, a := range attachments {
		if a.Name == name {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return mail.Attachment{}, errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{
			"name": name,
		})
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return mail.Attachment{}, errs.NewWithDetails(errs.CodeAmbiguousAttachment, "the draft has more than one attachment with this name; pick one with --attachment-id", map[string]any{
			"name":          name,
			"candidate_ids": ids,
		})
	}
}

func validateRemovableAttachment(a mail.Attachment) error {
	if a.IsInline {
		return invalidComposeInput("attachment", "maildo cannot remove an inline attachment, such as a picture inside the text")
	}
	if a.Kind != mail.AttachmentKindFile {
		return invalidComposeInput("attachment", "maildo can only remove attached files, and this attachment is not a file")
	}
	return nil
}

func mergeRemovedAttachment(target, providerRemoved mail.Attachment) mail.Attachment {
	if providerRemoved.ID == "" {
		return target
	}
	out := providerRemoved
	// Use what the server reported, and fill in the gaps from the attachment
	// as maildo saw it before the delete. Some servers return only the id.
	if out.Name == "" {
		out.Name = target.Name
	}
	if out.ContentType == "" {
		out.ContentType = target.ContentType
	}
	if out.Size == 0 {
		out.Size = target.Size
	}
	if out.ContentID == "" {
		out.ContentID = target.ContentID
	}
	if out.Kind == "" {
		out.Kind = target.Kind
	}
	if out.ProviderType == "" {
		out.ProviderType = target.ProviderType
	}
	return out
}

func destroyAttachmentUploads(uploads []mail.AttachmentUpload) {
	for i := range uploads {
		zeroBytes(uploads[i].Bytes)
		uploads[i].Bytes = nil
	}
}

func destroyAttachmentTokens(attachments []mail.Attachment) {
	for i := range attachments {
		if attachments[i].ExecutionToken != nil {
			attachments[i].ExecutionToken.Destroy()
			attachments[i].ExecutionToken = nil
		}
	}
}

func publicAttachments(in []mail.Attachment) []mail.Attachment {
	out := make([]mail.Attachment, len(in))
	for i, attachment := range in {
		out[i] = publicAttachment(attachment)
	}
	return out
}

func publicAttachment(attachment mail.Attachment) mail.Attachment {
	out := attachment
	out.ExecutionToken = nil
	out.ProviderMeta, _ = mail.CloneProviderMetadata(attachment.ProviderMeta)
	return out
}
