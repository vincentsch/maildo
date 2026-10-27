package main

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	composepkg "github.com/vincentsch/maildo/internal/compose"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// forward has the provider create a forward draft, puts your text above the
// forwarded message, and sends it only with --send.
func (h *handlers) forward(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		ctx := cmd.Context()
		selector, err := parseCommandSelector(cmd, false)
		if err != nil {
			return err
		}
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		sourceAccess, err := provider.ComposeSourceAccessForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessComposeSourceRead); err != nil {
			return err
		}
		draft, _ := cmd.Flags().GetBool("draft")
		send, _ := cmd.Flags().GetBool("send")
		if draft && send {
			return invalidComposeInput("mode", "--draft and --send cannot be used together")
		}
		action := mail.ComposeExecutionDraft
		if send {
			action = mail.ComposeExecutionSend
		}
		include := forwardIncludeFlag(cmd)
		names, _ := cmd.Flags().GetStringArray("include-attachment")
		ids, _ := cmd.Flags().GetStringArray("include-attachment-id")
		attachments, err := composepkg.BuildForwardAttachmentSelection(include, names, ids)
		if err != nil {
			return err
		}
		policy, err := provider.ComposeIngressPolicyForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		writeAccess := forwardWriteAccess(action, attachments)
		access := writeAccess
		usePreview := f.DryRun() && policy == provider.ComposeIngressMIMEBounded && attachments.Mode != mail.ForwardAttachmentsNone
		if usePreview {
			access = forwardPreviewAccess(action, attachments)
		}
		if err := provider.CheckClientAccess(acct, access); err != nil {
			return err
		}
		opts, err := readForwardOptions(f, cmd, acct, policy)
		if err != nil {
			return err
		}
		plan, err := composepkg.BuildForward(opts)
		if err != nil {
			return err
		}
		var resolved mail.ResolvedMessageSelector
		sourceInternetID := selector.InternetID
		if selector.Kind == mail.SelectorQuery {
			selected, err := h.resolveReplySource(ctx, clientScope, sel, selector.Query)
			if err != nil {
				return err
			}
			resolved = mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: selected.Message.ID}
			sourceInternetID = selected.Message.InternetMessageID
		} else {
			resolved = selector.Resolved()
		}
		var source *provider.ComposeSourceSnapshot
		if err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessComposeSourceRead, func(reader provider.ComposeSourceReader) error {
			var err error
			source, err = reader.ReadComposeSource(ctx, sourceAccess, resolved)
			return err
		}); err != nil {
			return err
		}
		defer source.Destroy()
		var finalBody mail.OutboundBody
		var subject, sourceID string
		if err := source.Use(func(material provider.ComposeSourceMaterial) error {
			var err error
			finalBody, err = composepkg.BuildForwardBody(plan, composeSourceForBody(material))
			subject, sourceID = material.Subject, material.SourceID
			return err
		}); err != nil {
			return err
		}
		sending := plan.Action == mail.ComposeExecutionSend
		pin := [][2]string{{"id", sourceID}}
		forwardReq := provider.ForwardDraftRequest{
			To:          plan.To,
			CC:          plan.CC,
			BCC:         plan.BCC,
			Body:        finalBody,
			Action:      plan.Action,
			Attachments: plan.Attachments,
		}
		// Gmail and IMAP build the forward in maildo and say exactly what
		// would go out. Outlook writes it on its side, so maildo sees it
		// only once the draft exists.
		var validated mail.ForwardDraftResult
		if f.DryRun() || sending {
			validationSource, err := source.Clone()
			if err != nil {
				return err
			}
			defer validationSource.Destroy()
			forwardReq.Source = validationSource
			if usePreview {
				err = provider.WithAccountClient(ctx, clientScope, acct, access, func(previewer provider.ForwardDraftPreviewer) error {
					var err error
					validated, err = previewer.ValidateForwardDraftPreview(ctx, forwardReq)
					return err
				})
			} else {
				err = provider.WithAccountClient(ctx, clientScope, acct, access, func(creator provider.ForwardDraftCreator) error {
					var err error
					validated, err = creator.ValidateForwardDraft(ctx, forwardReq)
					return err
				})
			}
			if err != nil {
				return err
			}
			forwardReq.Source = nil
		}
		native := validated.Send == nil
		if f.DryRun() && !(sending && native) {
			var msg outgoing
			if native {
				text, html := provider.MIMEBodies(finalBody)
				msg = outgoing{From: senderOf(acct), To: plan.To, CC: plan.CC, BCC: plan.BCC, Subject: prefixSubject("Fwd: ", subject),
					Body: readableBody(text, html), BodyHTML: html, recipientsKnown: true}
				msg.Attachments, msg.AttachmentsLeftOut = h.forwardAttachmentNames(ctx, clientScope, acct, sourceID, plan.Attachments)
			} else {
				msg = newOutgoing(*validated.Send)
				_, msg.AttachmentsLeftOut = h.forwardAttachmentNames(ctx, clientScope, acct, sourceID, plan.Attachments)
			}
			if sending {
				return h.writeSendPreview(f, cmd, acct, "forward", msg, pin)
			}
			return writeComposePreview(f, cmd, acct, sendPreview{Operation: "forward", Message: msg, SendCommand: changeCommand(cmd, nil, append([][2]string{{"account", acct.Name}}, pin...))})
		}
		if sending && !f.DryRun() {
			// Refuse a send that cannot be approved before anything is
			// written.
			if err := checkSendFlags(f, cmd, acct); err != nil {
				return err
			}
		}
		writeSource, err := source.Transfer()
		if err != nil {
			return err
		}
		defer writeSource.Destroy()
		forwardReq.Source = writeSource
		var created mail.ForwardDraftResult
		var logStatus mail.ActionLogStatus
		if sending && !native {
			msg := newOutgoing(*validated.Send)
			approved, err := h.approveSend(f, cmd, acct, msg)
			if err != nil {
				return err
			}
			forwardReq.Check = sendCheck(acct, approved)
			createErr := provider.WithAccountClient(ctx, clientScope, acct, writeAccess, func(creator provider.ForwardDraftCreator) error {
				var err error
				created, err = creator.CreateForwardDraft(ctx, forwardReq)
				return err
			})
			msg.DraftID = created.DraftID
			logStatus = h.logSend(f, cmd, acct, "forward", msg, approved, created.SentID, createErr)
			if createErr != nil {
				return forwardDraftError(createErr, created.DraftID)
			}
		} else {
			// Save the forward as a draft: a plain forward, or Outlook's
			// forward that maildo checks and sends afterwards.
			forwardReq.Action = mail.ComposeExecutionDraft
			createErr := provider.WithAccountClient(ctx, clientScope, acct, forwardWriteAccess(mail.ComposeExecutionDraft, plan.Attachments), func(creator provider.ForwardDraftCreator) error {
				var err error
				created, err = creator.CreateForwardDraft(ctx, forwardReq)
				return err
			})
			if created.DraftID != "" || createErr != nil {
				logStatus = h.logChange(f, cmd, acct, "draft_create", audit.Change{Kind: "draft", ID: created.DraftID, Name: firstNonEmpty(created.Subject, prefixSubject("Fwd: ", subject))}, createErr)
			}
			if createErr != nil {
				return forwardDraftError(createErr, created.DraftID)
			}
			if sending {
				sent, status, err := h.sendSavedDraft(f, cmd, clientScope, acct, "forward", created.DraftID)
				if err != nil || sent == nil {
					return err
				}
				logStatus = status
				created.To, created.CC, created.BCC = sent.draft.To, sent.draft.CC, sent.draft.BCC
				created.SentID = sent.outcome.MessageID
				created.SentCopy = sent.outcome.SentCopy
				created.Warnings = append(created.Warnings, sent.outcome.Warnings...)
				created.WriteOutcomeFacts = sent.outcome.WriteOutcomeFacts
			}
		}
		if sourceInternetID == "" {
			sourceInternetID = created.SourceInternetID
		}
		writeFacts := mail.CloneWriteOutcomeFacts(created.WriteOutcomeFacts)
		result := forwardResult{
			AccountSelection:           sel,
			SourceID:                   sourceIDForOutput(selector, resolved, created.SourceID),
			SourceInternetID:           sourceInternetID,
			ActionMode:                 string(plan.Action),
			DraftID:                    created.DraftID,
			SentID:                     created.SentID,
			To:                         created.To,
			CC:                         created.CC,
			BCC:                        created.BCC,
			Subject:                    created.Subject,
			BodyFormat:                 finalBody.Format,
			AttachmentMode:             plan.Attachments.Mode,
			SelectedSourceAttachments:  created.SelectedSourceAttachments,
			FinalAttachments:           created.FinalAttachments,
			UnsupportedSourceExclusion: created.ExcludedUnsupported,
			AttachmentCount:            created.AttachmentCount,
			WriteOutcomeFacts:          writeFacts,
			ProviderMeta:               cloneProviderMetadataForResult(created.ProviderMeta),
			SentCopy:                   created.SentCopy,
			Warnings:                   created.Warnings,
			ActionLogStatus:            logStatus,
		}
		warnAll(f, created.Warnings)
		return f.WriteResultWithMeta(result, forwardMeta(cmd.CommandPath(), sel, selector, plan, result), humanForward(f, result))
	})
}

func forwardWriteAccess(action mail.ComposeExecutionAction, attachments mail.ForwardAttachmentSelection) provider.ClientAccessKind {
	withAttachments := attachments.Mode != mail.ForwardAttachmentsNone
	switch {
	case action == mail.ComposeExecutionSend && withAttachments:
		return provider.ClientAccessForwardDraftSendWithAttachments
	case action == mail.ComposeExecutionSend:
		return provider.ClientAccessForwardDraftSend
	case withAttachments:
		return provider.ClientAccessForwardDraftCreateWithAttachments
	default:
		return provider.ClientAccessForwardDraftCreate
	}
}

func forwardPreviewAccess(action mail.ComposeExecutionAction, attachments mail.ForwardAttachmentSelection) provider.ClientAccessKind {
	if attachments.Mode == mail.ForwardAttachmentsNone {
		return forwardWriteAccess(action, attachments)
	}
	if action == mail.ComposeExecutionSend {
		return provider.ClientAccessForwardDraftSendPreviewWithAttachments
	}
	return provider.ClientAccessForwardDraftCreatePreviewWithAttachments
}

func readForwardOptions(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.ForwardOptions, error) {
	body, err := readBodyInput(f, cmd, acct, policy)
	if err != nil {
		return composepkg.ForwardOptions{}, err
	}
	flags := cmd.Flags()
	to, _ := flags.GetStringArray("to")
	cc, _ := flags.GetStringArray("cc")
	bcc, _ := flags.GetStringArray("bcc")
	draft, _ := flags.GetBool("draft")
	send, _ := flags.GetBool("send")
	include := forwardIncludeFlag(cmd)
	names, _ := flags.GetStringArray("include-attachment")
	ids, _ := flags.GetStringArray("include-attachment-id")
	return composepkg.ForwardOptions{
		Draft: draft,
		Send:  send,
		Recipients: composepkg.RecipientInput{
			To: to, CC: cc, BCC: bcc,
			ToSet: flags.Changed("to"), CCSet: flags.Changed("cc"), BCCSet: flags.Changed("bcc"),
		},
		Body:                   body,
		IncludeAttachments:     include,
		IncludeAttachmentNames: names,
		IncludeAttachmentIDs:   ids,
	}, nil
}

// forwardIncludeFlag returns --include-attachments only when the user gave
// it, so that naming attachments with --include-attachment works without
// also writing --include-attachments none.
func forwardIncludeFlag(cmd *cobra.Command) string {
	if !cmd.Flags().Changed("include-attachments") {
		return ""
	}
	value, _ := cmd.Flags().GetString("include-attachments")
	return value
}

// forwardAttachmentNames lists, for the preview and the confirmation, the
// original's attachments that go along with a forward and the ones that stay
// behind. Inline images belong to the body and are not listed. If the list
// cannot be read, the preview falls back to what the user asked for.
func (h *handlers) forwardAttachmentNames(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, sourceID string, sel mail.ForwardAttachmentSelection) (included []outgoingAttachment, leftOut []string) {
	var rows []mail.Attachment
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessageAttachmentsRead, func(reader provider.MessageAttachmentReader) error {
		list, err := reader.ListMessageAttachments(ctx, mail.MessageAttachmentListRequest{MessageID: sourceID})
		rows = list.Attachments
		return err
	})
	if err != nil {
		switch sel.Mode {
		case mail.ForwardAttachmentsAll:
			return []outgoingAttachment{{Name: "all attachments of the original"}}, nil
		case mail.ForwardAttachmentsSelected:
			for _, name := range append(append([]string(nil), sel.Names...), sel.IDs...) {
				included = append(included, outgoingAttachment{Name: name})
			}
			return included, nil
		}
		return nil, nil
	}
	names := map[string]bool{}
	for _, n := range sel.Names {
		names[n] = true
	}
	ids := map[string]bool{}
	for _, id := range sel.IDs {
		ids[id] = true
	}
	for _, row := range rows {
		if row.IsInline {
			continue
		}
		var goes bool
		switch sel.Mode {
		case mail.ForwardAttachmentsAll:
			// IMAP forwards attached emails too; Outlook and Gmail copy
			// files only.
			goes = row.Kind == mail.AttachmentKindFile || (row.Kind == mail.AttachmentKindItem && (acct.Provider == "imap" || acct.Provider == "fastmail"))
		case mail.ForwardAttachmentsSelected:
			goes = names[row.Name] || ids[row.ID]
		}
		name := firstNonEmpty(row.Name, "(no name)")
		if goes {
			included = append(included, outgoingAttachment{Name: name, ContentType: row.ContentType, Size: row.Size})
		} else {
			leftOut = append(leftOut, name)
		}
	}
	return included, leftOut
}
