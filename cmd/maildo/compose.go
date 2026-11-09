package main

import (
	"context"
	"fmt"
	"io"
	netmail "net/mail"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	composepkg "github.com/vincentsch/maildo/internal/compose"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/search"
	"github.com/vincentsch/rungrad"
)

const replyLong = `Reply to a message (saves a draft unless you add --send)

reply saves your reply as a draft. To send it instead, add --send, and run
the command with --dry-run first to see the whole message.

Below your text, maildo quotes the whole original message, after a line
such as "On 2026-11-02 12:15 CET, Maria Klein <maria@example.com> wrote:".
With --quote short it quotes only the start, and with --quote none it adds
no quote. --quote-language sets the language of that line, en or de; auto
picks it from your locale.

Pick the message by its id, or find it with --query and --latest, which
takes the newest match. --first takes the first match, which is the same
message, because matches come newest first. --internet-id picks it by its
Message-ID. --reply-all also writes to everyone else on the message.`

const draftDeleteLong = `Discard a draft for good

This works like the discard button of a mail program. The draft is removed
from Drafts and does not go to Trash. The id must name a draft, so this
cannot delete other mail. To move a message to Trash, use maildo delete.

Run it with --dry-run first. The preview shows the draft and prints the
command that discards it, with --confirm and the draft's id.`

// composeModule holds the draft commands and send, reply and forward.
type composeModule struct {
	h    *handlers
	defs []cmdDef
}

// newComposeModule declares each command with its help and handler in one
// place, so help and behavior cannot drift apart.
func newComposeModule(h *handlers) composeModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	readModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	composeFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		addComposeFlags(cmd)
	}
	updateFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		cmd.Flags().String("id", "", "Draft id, or the short id maildo printed")
		addComposeFlags(cmd)
		cmd.Flags().Bool("clear-to", false, "Remove all To recipients")
		cmd.Flags().Bool("clear-cc", false, "Remove all Cc recipients")
		cmd.Flags().Bool("clear-bcc", false, "Remove all Bcc recipients")
		cmd.Flags().Bool("clear-reply-to", false, "Remove all Reply-To addresses")
	}
	replyFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		cmd.Flags().String("id", "", "Message id, or a short id from list or search")
		cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
		cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
		cmd.Flags().Bool("latest", false, "With --query, use the newest match")
		cmd.Flags().Bool("first", false, "With --query, use the first match; matches come newest first, so this is the same as --latest")
		cmd.Flags().Bool("reply-all", false, "Reply to the sender and all other recipients")
		cmd.Flags().Bool("draft", false, "Only save a draft; reply does this anyway unless you add --send")
		cmd.Flags().Bool("send", false, "Send the reply instead of saving a draft")
		cmd.Flags().String("quote", "full", "How much of the original to quote: full, short or none")
		cmd.Flags().String("quote-language", "auto", "Language of the \"On ... wrote:\" line: en, de or auto (from your locale)")
		addReplyBodyFlags(cmd)
		addSendFlags(cmd)
	}
	forwardFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		cmd.Flags().String("id", "", "Message id, or a short id from list or search")
		cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
		cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
		cmd.Flags().Bool("latest", false, "With --query, use the newest match")
		cmd.Flags().Bool("first", false, "With --query, use the first match; matches come newest first, so this is the same as --latest")
		cmd.Flags().Bool("draft", false, "Only save a draft; forward does this anyway unless you add --send")
		cmd.Flags().Bool("send", false, "Send the forward instead of saving a draft")
		cmd.Flags().StringArray("to", nil, "To recipient; repeat the flag or separate addresses with commas")
		cmd.Flags().StringArray("cc", nil, "Cc recipient; repeat the flag or separate addresses with commas")
		cmd.Flags().StringArray("bcc", nil, "Bcc recipient; repeat the flag or separate addresses with commas")
		cmd.Flags().String("include-attachments", "all", "Which attachments of the original go along: all or none")
		cmd.Flags().StringArray("include-attachment", nil, "Name of an attachment of the original to send along (repeatable)")
		cmd.Flags().StringArray("include-attachment-id", nil, "Id of an attachment of the original to send along (repeatable)")
		addReplyBodyFlags(cmd)
		addSendFlags(cmd)
	}
	defs := []cmdDef{
		{
			path: "draft", use: "draft", short: "Write, change and send drafts", groupID: "compose",
			examples: []string{"maildo draft create --to client@example.com --subject Quote", "maildo draft read --id 3c1f2a7"},
			related:  []string{"send", "read"},
		},
		{
			path: "draft create", use: "create", short: "Save a new draft",
			examples: []string{
				`maildo draft create --to client@example.com --subject "Quote" --body "Draft body"`,
				`maildo draft create --account work --to client@example.com --body-file draft.md --markdown`,
				`maildo draft create --to client@example.com --subject "Quote" --json`,
				`maildo draft create --to client@example.com --body "Preview" --dry-run`,
			},
			related:      []string{"draft read", "draft update", "draft send", "send"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    composeFlags,
			run:          h.draftCreate,
		},
		{
			path: "draft read", use: "read [id]", short: "Show a draft",
			examples: []string{
				`maildo draft read 3c1f2a7`,
				`maildo draft read --id 3c1f2a7`,
				`maildo draft read --id 3c1f2a7 --account work --json`,
				`maildo draft read --id 3c1f2a7 --body-mode html`,
				`maildo draft read --id 3c1f2a7 --plain`,
			},
			related:      []string{"draft update", "draft send", "read"},
			outputModes:  readModes,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("id", "", "Draft id, or the short id maildo printed")
				cmd.Flags().String("body-mode", string(mail.BodyModeReadable), "How to show the body: readable, text, html or raw")
			},
			run: h.draftRead,
		},
		{
			path: "draft update", use: "update [id]", short: "Change a draft",
			examples: []string{
				`maildo draft update --id 3c1f2a7 --subject "Updated subject"`,
				`maildo draft update --id 3c1f2a7 --account work --body-file draft.html --html`,
				`maildo draft update --id 3c1f2a7 --clear-cc --json`,
				`maildo draft update --id 3c1f2a7 --body "Preview" --dry-run`,
			},
			related:      []string{"draft read", "draft send", "draft create"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure:    updateFlags,
			run:          h.draftUpdate,
		},
		{
			path: "draft delete", use: "delete [id]", short: "Discard a draft for good",
			long: draftDeleteLong,
			examples: []string{
				`maildo draft delete 3c1f2a7 --dry-run`,
				`maildo draft delete 3c1f2a7 --confirm 3c1f2a7`,
				`maildo draft delete --id 3c1f2a7 --account work --dry-run --json`,
			},
			related:      []string{"draft read", "delete"},
			outputModes:  resultModes,
			mutates:      true,
			destructive:  true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("id", "", "Draft id, or the short id maildo printed")
				cmd.Flags().String("confirm", "", "The draft's id again, to confirm; --dry-run prints the command")
				cmd.Flags().Bool("full", false, "Show the whole body in the preview, also when it is long")
			},
			run: h.draftDelete,
		},
		{
			path: "draft send", use: "send [id]", short: "Send a draft",
			examples: []string{
				`maildo draft send 3c1f2a7`,
				`maildo draft send --id 3c1f2a7 --dry-run`,
				`maildo draft send --account work --id 3c1f2a7 --approve k7q2m9xd --yes`,
				`maildo draft send --id 3c1f2a7 --dry-run --full`,
			},
			related:      []string{"draft read", "send"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("id", "", "Draft id, or the short id maildo printed")
				addSendFlags(cmd)
			},
			run: h.draftSend,
		},
		{
			path: "send", use: "send", short: "Send a new message", groupID: "compose",
			examples: []string{
				`maildo send --to client@example.com --subject "Quote" --body "Message body"`,
				`maildo send --to client@example.com --subject "Quote" --body "Message body" --dry-run`,
				`maildo send --account work --to client@example.com --subject "Quote" --body "Message body" --approve k7q2m9xd --yes`,
				`maildo send --to client@example.com --subject "Quote" --body-file quote.txt --attach quote.pdf --dry-run --json`,
			},
			related:      []string{"draft create", "draft send"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				composeFlags(cmd)
				cmd.Flags().StringArray("attach", nil, "File to attach (repeatable)")
				addSendFlags(cmd)
			},
			run: h.sendDirect,
		},
		{
			path: "reply", use: "reply [id]", short: "Reply to a message (saves a draft unless you add --send)", groupID: "compose", long: replyLong,
			examples: []string{
				`maildo reply 9eac029 --body "Thanks, paid today."`,
				`maildo reply --id 3c1f2a7 --account work --body-file reply.md --markdown`,
				`maildo reply --reply-all --id 3c1f2a7 --body "On it" --quote short`,
				`maildo reply --query "from:client@example.com subject:quote" --latest --body "Thanks" --send --dry-run`,
				`maildo reply --id 3c1f2a7 --body "Preview" --dry-run --json`,
			},
			related:      []string{"draft create", "send", "read"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure:    replyFlags,
			run:          h.reply,
		},
		{
			path: "forward", use: "forward [id]", short: "Forward a message (saves a draft unless you add --send)", groupID: "compose",
			examples: []string{
				`maildo forward 9eac029 --to anna@example.com --body "FYI"`,
				`maildo forward --id 3c1f2a7 --account work --to client@example.com --body-file note.md --markdown`,
				`maildo forward --query "from:client@example.com subject:quote" --latest --to client@example.com --body "FYI" --send --dry-run`,
				`maildo forward 9eac029 --to anna@example.com --body "FYI" --include-attachments none`,
				`maildo forward --id 3c1f2a7 --body "Preview" --dry-run --json`,
			},
			related:      []string{"reply", "draft update", "send", "read"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure:    forwardFlags,
			run:          h.forward,
		},
	}
	defs = markMailboxAuth(defs,
		"draft create",
		"draft read",
		"draft update",
		"draft delete",
		"draft send",
		"send",
		"reply",
		"forward",
	)
	return composeModule{h: h, defs: defs}
}

func (m composeModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "compose", Title: "Write and send"}}
}

func (m composeModule) Commands() []*rungrad.Command {
	draft := m.defs[0].command()
	// defs[1:6] are the draft subcommands; defs[6:] are top-level compose commands.
	for _, def := range m.defs[1:6] {
		draft.AddCommand(def.command())
	}
	return []*rungrad.Command{draft, m.defs[6].command(), m.defs[7].command(), m.defs[8].command()}
}

func (m composeModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

func addComposeFlags(cmd *cobra.Command) {
	cmd.Flags().StringArray("to", nil, "To recipient; repeat the flag or separate addresses with commas")
	cmd.Flags().StringArray("cc", nil, "Cc recipient; repeat the flag or separate addresses with commas")
	cmd.Flags().StringArray("bcc", nil, "Bcc recipient; repeat the flag or separate addresses with commas")
	cmd.Flags().StringArray("reply-to", nil, "Reply-To address; repeat the flag or separate addresses with commas")
	cmd.Flags().String("from", "", "Sender, such as \"Ann <ann@example.com>\"; the address must be the account's own")
	cmd.Flags().String("subject", "", "Message subject")
	addReplyBodyFlags(cmd)
}

// addSendFlags adds the flags that approve a send and show the whole
// message in previews.
func addSendFlags(cmd *cobra.Command) {
	cmd.Flags().String("approve", "", "Approval code from the --dry-run preview; maildo sends only if the message still matches it, and only once")
	cmd.Flags().Bool("yes", false, "Send without asking; needs --approve unless the account allows unattended sends")
	cmd.Flags().Bool("full", false, "Show the whole body in the preview, also when it is long")
}

func addReplyBodyFlags(cmd *cobra.Command) {
	cmd.Flags().String("body", "", "Message body as text")
	cmd.Flags().String("body-file", "", "Read the message body from a file, or from stdin with -")
	cmd.Flags().Bool("text", false, "Write the body as plain text")
	cmd.Flags().Bool("markdown", false, "Write the body in Markdown; maildo turns it into HTML")
	cmd.Flags().Bool("html", false, "Write the body in HTML; maildo removes unsafe parts")
}

// draftCreate checks and builds the message before it connects to the
// account. With --dry-run it stops after checking that the account can save
// drafts, and shows the preview.
func (h *handlers) draftCreate(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftCreate); err != nil {
			return err
		}
		policy, err := provider.ComposeIngressPolicyForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		input, err := readCreateInput(f, cmd, acct, policy)
		if err != nil {
			return err
		}
		req, summary, err := composepkg.BuildCreate(input, acct.Email)
		if err != nil {
			return err
		}
		if f.DryRun() {
			msg := outgoingFromMessage(acct, req.Message)
			return writeReadablePreview(f, previewCompose("draft-create", acct, summary), humanDraftPreview(acct, msg, changeCommand(cmd, nil, [][2]string{{"account", acct.Name}})))
		}
		var ref mail.DraftRef
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftCreate, func(creator provider.DraftCreator) error {
			var err error
			ref, err = creator.CreateDraft(cmd.Context(), req)
			return err
		})
		logStatus := h.logChange(f, cmd, acct, "draft_create", audit.Change{Kind: "draft", ID: ref.ID, Name: req.Message.Subject}, err)
		if err != nil {
			return err
		}
		warnAll(f, ref.Warnings)
		result := draftWriteResult{
			ActionLogStatus:   logStatus,
			Warnings:          ref.Warnings,
			AccountSelection:  sel,
			Operation:         "draft_create",
			DraftID:           ref.ID,
			RequestedFrom:     summary.RequestedFrom,
			To:                req.Message.To,
			CC:                req.Message.CC,
			BCC:               req.Message.BCC,
			ReplyTo:           req.Message.ReplyTo,
			Subject:           req.Message.Subject,
			BodyFormat:        resultBodyFormat(req.Message.Body),
			AttachmentCount:   0,
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(ref.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(ref.ProviderMeta),
		}
		return f.WriteResultWithMeta(result, composeMeta(cmd.CommandPath(), sel, result.Operation, result.DraftID, result.MessageID), humanDraftWrite(f, result))
	})
}

// sendDirect sends a new message without saving a draft first.
func (h *handlers) sendDirect(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	if err := validateDirectSendLocalInput(cmd); err != nil {
		return err
	}
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDirectSend); err != nil {
			return err
		}
		policy, err := provider.ComposeIngressPolicyForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		input, err := readSendInput(f, cmd, acct, policy)
		if err != nil {
			return err
		}
		req, _, err := composepkg.BuildSend(input, acct.Email)
		clearSendInput(&input)
		if err != nil {
			return err
		}
		if paths, _ := cmd.Flags().GetStringArray("attach"); len(paths) > 0 {
			uploads, err := prevalidateAttachmentUploads(paths)
			if err != nil {
				return err
			}
			req.Message.Attachments = uploads
		}
		// Describe the message for the preview and the approval code now,
		// because preparing the send takes the message over.
		msg := outgoingFromMessage(acct, req.Message)
		summary, err := provider.PrepareDirectSend(cmd.Context(), h.deps.DirectSendPreparers, acct, &req)
		if err != nil {
			return err
		}
		if req.Prepared != nil {
			defer req.Prepared.Destroy()
		}
		if f.DryRun() {
			return h.writeSendPreview(f, cmd, acct, "send", msg, nil)
		}
		approved, err := h.approveSend(f, cmd, acct, msg)
		if err != nil {
			return err
		}
		var outcome mail.DirectSendOutcome
		sendErr := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDirectSend, func(sender provider.DirectSender) error {
			var err error
			outcome, err = sender.SendMessage(cmd.Context(), req)
			return err
		})
		logStatus := h.logSend(f, cmd, acct, "send", msg, approved, outcome.MessageID, sendErr)
		if sendErr != nil {
			return sendErr
		}
		result := directSendResult{
			Account:           acct.Name,
			Provider:          acct.Provider,
			Operation:         "send",
			MessageID:         outcome.MessageID,
			To:                msg.To,
			CC:                msg.CC,
			BCC:               msg.BCC,
			Subject:           msg.Subject,
			Summary:           summary,
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(outcome.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(outcome.ProviderMeta),
			SentCopy:          outcome.SentCopy,
			Warnings:          outcome.Warnings,
			ActionLogStatus:   logStatus,
		}
		warnAll(f, outcome.Warnings)
		return f.WriteResultWithMeta(result, directSendMeta(cmd.CommandPath(), sel, result.MessageID), humanSent(acct.Name, msg, outcome.SentCopy))
	})
}

// reply has the provider create a reply draft to the original message, and
// sends it only with --send.
func (h *handlers) reply(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
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
		replyDraft, _ := cmd.Flags().GetBool("draft")
		replySend, _ := cmd.Flags().GetBool("send")
		if replyDraft && replySend {
			return invalidComposeInput("mode", "--draft and --send cannot be used together")
		}
		writeAccess := provider.ClientAccessReplyDraftCreate
		if replySend {
			writeAccess = provider.ClientAccessReplyDraftSend
		}
		if err := provider.CheckClientAccess(acct, writeAccess); err != nil {
			return err
		}
		policy, err := provider.ComposeIngressPolicyForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		opts, err := readReplyOptions(f, cmd, acct, policy)
		if err != nil {
			return err
		}
		plan, err := composepkg.BuildReply(opts)
		if err != nil {
			return err
		}
		var resolved mail.ResolvedMessageSelector
		sourceInternetID := selector.InternetID
		if selector.Kind == mail.SelectorQuery {
			// Show the Message-ID of the message the search found, when the
			// provider sent one. The provider still gets the message id.
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
		if err := source.Use(func(m provider.ComposeSourceMaterial) error {
			var err error
			subject, sourceID = m.Subject, m.SourceID
			finalBody, err = composepkg.BuildReplyBody(plan, composeSourceForBody(m))
			return err
		}); err != nil {
			return err
		}
		replyReq := provider.ReplyDraftRequest{
			Mode:     plan.Mode,
			Quote:    plan.Quote,
			Language: plan.Language,
			Action:   plan.Action,
			Body:     finalBody,
		}
		pin := [][2]string{{"id", sourceID}}
		// Ask the provider for the reply it would write. Gmail and IMAP
		// build it in maildo and say exactly what would go out. Outlook
		// writes it on its side, so its recipients and quote are only known
		// once the draft exists.
		var validated mail.ReplyDraftResult
		if f.DryRun() || replySend {
			validationSource, err := source.Clone()
			if err != nil {
				return err
			}
			defer validationSource.Destroy()
			replyReq.Source = validationSource
			if err := provider.WithAccountClient(ctx, clientScope, acct, writeAccess, func(creator provider.ReplyDraftCreator) error {
				var err error
				validated, err = creator.ValidateReplyDraft(ctx, replyReq)
				return err
			}); err != nil {
				return err
			}
			replyReq.Source = nil
		}
		native := validated.Send == nil
		if f.DryRun() && !(replySend && native) {
			msg := outgoingForReply(acct, subject, firstNonEmpty(validated.SourceInternetID, sourceInternetID), finalBody)
			if !native {
				msg = newOutgoing(*validated.Send)
			}
			if replySend {
				return h.writeSendPreview(f, cmd, acct, "reply", msg, pin)
			}
			return writeComposePreview(f, cmd, acct, sendPreview{Operation: "reply", Message: msg, SendCommand: changeCommand(cmd, nil, append([][2]string{{"account", acct.Name}}, pin...))})
		}
		if replySend && !f.DryRun() {
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
		replyReq.Source = writeSource
		var created mail.ReplyDraftResult
		var logStatus mail.ActionLogStatus
		if replySend && !native {
			msg := newOutgoing(*validated.Send)
			approved, err := h.approveSend(f, cmd, acct, msg)
			if err != nil {
				return err
			}
			replyReq.Check = sendCheck(acct, approved)
			createErr := provider.WithAccountClient(ctx, clientScope, acct, writeAccess, func(creator provider.ReplyDraftCreator) error {
				var err error
				created, err = creator.CreateReplyDraft(ctx, replyReq)
				return err
			})
			msg.DraftID = created.DraftID
			logStatus = h.logSend(f, cmd, acct, "reply", msg, approved, created.SentID, createErr)
			if createErr != nil {
				return replyDraftError(createErr, created.DraftID)
			}
		} else {
			// Save the reply as a draft: a plain reply, or Outlook's reply
			// that maildo checks and sends afterwards.
			replyReq.Action = mail.ComposeExecutionDraft
			createErr := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessReplyDraftCreate, func(creator provider.ReplyDraftCreator) error {
				var err error
				created, err = creator.CreateReplyDraft(ctx, replyReq)
				return err
			})
			if created.DraftID != "" || createErr != nil {
				logStatus = h.logChange(f, cmd, acct, "draft_create", audit.Change{Kind: "draft", ID: created.DraftID, Name: created.Subject}, createErr)
			}
			if createErr != nil {
				return replyDraftError(createErr, created.DraftID)
			}
			if replySend {
				sent, status, err := h.sendSavedDraft(f, cmd, clientScope, acct, "reply", created.DraftID)
				if err != nil || sent == nil {
					return err
				}
				logStatus = status
				created.To, created.CC, created.BCC, created.Subject = sent.draft.To, sent.draft.CC, sent.draft.BCC, sent.draft.Subject
				created.SentID = sent.outcome.MessageID
				created.SentCopy = sent.outcome.SentCopy
				created.Warnings = append(created.Warnings, sent.outcome.Warnings...)
				created.WriteOutcomeFacts = sent.outcome.WriteOutcomeFacts
				created.ProviderMeta = sent.outcome.ProviderMeta
			}
		}
		writeFacts := mail.CloneWriteOutcomeFacts(created.WriteOutcomeFacts)
		if sourceInternetID == "" {
			sourceInternetID = created.SourceInternetID
		}
		result := replyResult{
			AccountSelection:  sel,
			SourceID:          sourceIDForOutput(selector, resolved, created.SourceID),
			SourceInternetID:  sourceInternetID,
			ReplyMode:         plan.Mode,
			ActionMode:        string(plan.Action),
			QuoteMode:         plan.Quote,
			QuoteLanguage:     plan.Language,
			DraftID:           created.DraftID,
			SentID:            created.SentID,
			To:                created.To,
			CC:                created.CC,
			BCC:               created.BCC,
			Subject:           created.Subject,
			AttachmentCount:   created.AttachmentCount,
			WriteOutcomeFacts: writeFacts,
			ProviderMeta:      cloneProviderMetadataForResult(created.ProviderMeta),
			SentCopy:          created.SentCopy,
			Warnings:          created.Warnings,
			ActionLogStatus:   logStatus,
		}
		warnAll(f, created.Warnings)
		return f.WriteResultWithMeta(result, replyMeta(cmd.CommandPath(), sel, selector, plan), humanReply(f, result))
	})
}

// sentDraft is a draft that sendSavedDraft sent.
type sentDraft struct {
	draft   mail.Draft
	outcome mail.SendOutcome
}

// sendSavedDraft checks and sends a reply or forward that the provider
// wrote as a draft (Outlook). With --dry-run it shows the draft with its
// approval code and the draft send command, and returns no sentDraft. A
// refused or failed send names the draft, which stays in Drafts.
func (h *handlers) sendSavedDraft(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, operation, draftID string) (*sentDraft, mail.ActionLogStatus, error) {
	ctx := cmd.Context()
	keep := func(err error) error {
		if operation == "forward" {
			return forwardDraftError(err, draftID)
		}
		return replyDraftError(err, draftID)
	}
	draft, err := h.readDraftForSend(ctx, clientScope, acct, draftID)
	if err != nil {
		return nil, mail.ActionLogStatus{}, keep(err)
	}
	msg := outgoingFromDraft(draft)
	if f.DryRun() {
		if err := checkSendRecipients(acct, msg); err != nil {
			return nil, mail.ActionLogStatus{}, keep(err)
		}
		code := approvalCode(acct.Name, *msg.content)
		p := sendPreview{Operation: operation, Send: true, Message: msg, ApprovalCode: code, DraftSaved: true, SendCommand: draftSendCommand(cmd, acct, draftID, code)}
		return nil, mail.ActionLogStatus{}, writeComposePreview(f, cmd, acct, p)
	}
	approved, err := h.approveSend(f, cmd, acct, msg)
	if err != nil {
		return nil, mail.ActionLogStatus{}, keep(err)
	}
	outcome, status, err := h.sendDraft(f, cmd, clientScope, acct, operation, draft, approved)
	if err != nil {
		return nil, status, keep(err)
	}
	return &sentDraft{draft: draft, outcome: outcome}, status, nil
}

// draftRead is the only command here that prints the body.
func (h *handlers) draftRead(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftID(cmd)
		if err != nil {
			return err
		}
		modeFlag, _ := cmd.Flags().GetString("body-mode")
		bodyMode, err := mail.ParseBodyMode(modeFlag)
		if err != nil {
			return err
		}
		var draft mail.Draft
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftRead, func(reader provider.DraftReader) error {
			var err error
			draft, err = reader.ReadDraft(cmd.Context(), mail.ReadDraftRequest{DraftID: id, BodyMode: bodyMode})
			return err
		}); err != nil {
			return err
		}
		result := draftReadResult{AccountSelection: sel, Draft: draft}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  draftReadMeta(cmd.CommandPath(), sel, id, bodyMode),
			Human: humanDraftRead(f, result),
			Plain: plainDraftBody(draft),
		})
	})
}

// draftUpdate checks the requested changes first, so --dry-run and the real
// update treat recipients, subject and body the same way.
func (h *handlers) draftUpdate(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftID(cmd)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftUpdate); err != nil {
			return err
		}
		policy, err := provider.ComposeIngressPolicyForAccount(h.deps.DirectSendPreparers, acct)
		if err != nil {
			return err
		}
		input, err := readUpdateInput(f, cmd, acct, policy)
		if err != nil {
			return err
		}
		req, summary, err := composepkg.BuildUpdate(id, input, acct.Email)
		if err != nil {
			return err
		}
		if f.DryRun() {
			var validation mail.DraftUpdateValidation
			if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftUpdate, func(updater provider.DraftUpdater) error {
				var err error
				validation, err = updater.ValidateDraftUpdate(cmd.Context(), req)
				return err
			}); err != nil {
				return err
			}
			command := changeCommand(cmd, nil, [][2]string{{"account", acct.Name}})
			return writeReadablePreview(f, previewDraftUpdate(acct, req, summary, validation), humanDraftUpdatePreview(acct, id, req.Patch, command))
		}
		var ref mail.DraftRef
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftUpdate, func(updater provider.DraftUpdater) error {
			var err error
			ref, err = updater.UpdateDraft(cmd.Context(), req)
			return err
		})
		change := audit.Change{Kind: "draft", ID: id, Name: req.Patch.Subject.Value}
		if ref.ID != "" && ref.ID != id {
			change.NewID = ref.ID
		}
		logStatus := h.logChange(f, cmd, acct, "draft_update", change, err)
		if err != nil {
			return err
		}
		warnAll(f, ref.Warnings)
		result := draftWriteResult{
			ActionLogStatus:   logStatus,
			Warnings:          ref.Warnings,
			previousID:        id,
			subjectSet:        req.Patch.Subject.Set,
			AccountSelection:  sel,
			Operation:         "draft_update",
			DraftID:           ref.ID,
			RequestedFrom:     summary.RequestedFrom,
			To:                req.Patch.To.Values,
			CC:                req.Patch.CC.Values,
			BCC:               req.Patch.BCC.Values,
			ReplyTo:           req.Patch.ReplyTo.Values,
			Subject:           req.Patch.Subject.Value,
			BodyFormat:        resultBodyFormat(req.Patch.Body.Body),
			AttachmentCount:   0,
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(ref.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(ref.ProviderMeta),
		}
		return f.WriteResultWithMeta(result, composeMeta(cmd.CommandPath(), sel, result.Operation, result.DraftID, result.MessageID), humanDraftWrite(f, result))
	})
}

// draftDelete discards a draft. It reads the draft first, so the preview
// shows what goes, and so only a draft can be deleted this way.
func (h *handlers) draftDelete(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftID(cmd)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftDelete); err != nil {
			return err
		}
		var draft mail.Draft
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftRead, func(reader provider.DraftReader) error {
			var err error
			draft, err = reader.ReadDraft(cmd.Context(), mail.ReadDraftRequest{DraftID: id, BodyMode: mail.BodyModeReadable})
			return err
		}); err != nil {
			return err
		}
		if f.DryRun() {
			return writeDraftDeletePreview(f, cmd, acct, draft, id)
		}
		if confirm, _ := cmd.Flags().GetString("confirm"); !draftDeleteConfirmed(cmd, id, confirm) {
			return errs.NewWithDetails(errs.CodeConfirmationRequired,
				"discarding a draft needs --confirm with the draft's id; run the command with --dry-run to see the draft and the full command",
				map[string]any{"account": acct.Name, "provider": acct.Provider, "draft_id": id})
		}
		var ref mail.DraftRef
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftDelete, func(deleter provider.DraftDeleter) error {
			var err error
			ref, err = deleter.DeleteDraft(cmd.Context(), mail.DeleteDraftRequest{DraftID: id})
			return err
		})
		logStatus := h.logChange(f, cmd, acct, "draft_delete", audit.Change{Kind: "draft", ID: id, Name: draft.Subject}, err)
		if err != nil {
			return err
		}
		warnAll(f, ref.Warnings)
		result := draftWriteResult{
			AccountSelection:  sel,
			Operation:         "draft_delete",
			DraftID:           id,
			To:                draft.To,
			CC:                draft.CC,
			BCC:               draft.BCC,
			ReplyTo:           draft.ReplyTo,
			Subject:           draft.Subject,
			AttachmentCount:   len(draft.Attachments),
			ActionLogStatus:   logStatus,
			Warnings:          ref.Warnings,
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(ref.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(ref.ProviderMeta),
		}
		return f.WriteResultWithMeta(result, composeMeta(cmd.CommandPath(), sel, result.Operation, result.DraftID, ""), humanDraftDeleted(acct.Name, draft.Subject, len(ref.Warnings) > 0))
	})
}

// draftSend sends a stored draft. It reads the draft, shows and checks it,
// and the provider sends it only if what it is about to send still has the
// approval code of what was checked.
func (h *handlers) draftSend(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, acct, err := h.composeScope(f, cmd, clientScope)
		if err != nil {
			return err
		}
		id, err := requiredDraftID(cmd)
		if err != nil {
			return err
		}
		if err := provider.CheckClientAccess(acct, provider.ClientAccessDraftSend); err != nil {
			return err
		}
		draft, err := h.readDraftForSend(cmd.Context(), clientScope, acct, id)
		if err != nil {
			return err
		}
		msg := outgoingFromDraft(draft)
		if f.DryRun() {
			return h.writeSendPreview(f, cmd, acct, "draft_send", msg, nil)
		}
		approved, err := h.approveSend(f, cmd, acct, msg)
		if err != nil {
			return err
		}
		outcome, logStatus, err := h.sendDraft(f, cmd, clientScope, acct, "draft_send", draft, approved)
		if err != nil {
			return err
		}
		result := draftWriteResult{
			AccountSelection:  sel,
			Operation:         "draft_send",
			DraftID:           id,
			MessageID:         outcome.MessageID,
			To:                msg.To,
			CC:                msg.CC,
			BCC:               msg.BCC,
			Subject:           msg.Subject,
			AttachmentCount:   len(msg.Attachments),
			ActionLogStatus:   logStatus,
			Warnings:          outcome.Warnings,
			WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(outcome.WriteOutcomeFacts),
			ProviderMeta:      cloneProviderMetadataForResult(outcome.ProviderMeta),
		}
		warnAll(f, outcome.Warnings)
		return f.WriteResultWithMeta(result, composeMeta(cmd.CommandPath(), sel, result.Operation, result.DraftID, result.MessageID), humanSent(acct.Name, msg, outcome.SentCopy))
	})
}

// readDraftForSend reads a draft with everything the preview and the
// approval code need: what sending it would hand over.
func (h *handlers) readDraftForSend(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, id string) (mail.Draft, error) {
	var draft mail.Draft
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessDraftSend, func(sender provider.DraftSender) error {
		var err error
		draft, err = sender.ValidateDraftSend(ctx, mail.SendDraftRequest{DraftID: id})
		return err
	})
	if err == nil && draft.ID == "" {
		draft.ID = id
	}
	if err == nil && draft.Send == nil {
		return mail.Draft{}, errs.NewProviderClientContractInvalid()
	}
	return draft, err
}

// sendDraft has the provider send an approved draft. The provider reads
// the draft again and sends it only when it still has the approval's code.
// The send is written to the action log whether it worked or not.
func (h *handlers) sendDraft(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, operation string, draft mail.Draft, approved approval) (mail.SendOutcome, mail.ActionLogStatus, error) {
	var outcome mail.SendOutcome
	sendErr := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessDraftSend, func(sender provider.DraftSender) error {
		var err error
		outcome, err = sender.SendDraft(cmd.Context(), mail.SendDraftRequest{DraftID: draft.ID, Check: sendCheck(acct, approved)})
		return err
	})
	logStatus := h.logSend(f, cmd, acct, operation, outgoingFromDraft(draft), approved, outcome.MessageID, sendErr)
	return outcome, logStatus, sendErr
}

func (h *handlers) composeScope(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope) (provider.AccountSelection, provider.ResolvedAccount, error) {
	sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
	if err != nil {
		return provider.AccountSelection{}, provider.ResolvedAccount{}, err
	}
	return sel, sel.Accounts[0], nil
}

func (h *handlers) checkCapability(ctx context.Context, acct provider.ResolvedAccount, cap provider.Capability) error {
	caps, err := h.deps.Providers.Capabilities(ctx, acct.Provider)
	if err != nil {
		return err
	}
	return provider.CheckCapability(acct, caps, cap)
}

func (h *handlers) resolveReplySource(ctx context.Context, clientScope provider.AccountClientScope, sel provider.AccountSelection, query *mail.Query) (search.SelectedMessage, error) {
	run := func(ctx context.Context, acct provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error) {
		var out mail.MessageSummaryList
		err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesSearch, func(searcher provider.MessageSearcher) error {
			var err error
			out, err = searcher.SearchMessages(ctx, mail.SearchRequest{Query: *query, Limit: limit})
			return err
		})
		return out, err
	}
	return search.ResolveSelected(ctx, sel, run)
}

func readCreateInput(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.CreateInput, error) {
	recipients := readRecipientInput(cmd)
	sender := readSenderInput(cmd)
	subject := readSubjectInput(cmd)
	body, err := readBodyInput(f, cmd, acct, policy)
	if err != nil {
		return composepkg.CreateInput{}, err
	}
	return composepkg.CreateInput{Recipients: recipients, Sender: sender, Subject: subject, Body: body}, nil
}

func readSendInput(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.SendInput, error) {
	recipients := readRecipientInput(cmd)
	sender := readSenderInput(cmd)
	subject := readSubjectInput(cmd)
	body, err := readBodyInput(f, cmd, acct, policy)
	if err != nil {
		return composepkg.SendInput{}, err
	}
	return composepkg.SendInput{Recipients: recipients, Sender: sender, Subject: subject, Body: body}, nil
}

func validateDirectSendLocalInput(cmd *cobra.Command) error {
	flags := cmd.Flags()
	inlineSet := flags.Changed("body")
	fileSet := flags.Changed("body-file")
	if inlineSet && fileSet {
		return invalidComposeInput("body", "--body and --body-file cannot be used together")
	}
	textSet := flags.Changed("text")
	markdownSet := flags.Changed("markdown")
	htmlSet := flags.Changed("html")
	formatCount := 0
	for _, set := range []bool{textSet, markdownSet, htmlSet} {
		if set {
			formatCount++
		}
	}
	if formatCount > 1 {
		return invalidComposeInput("body_format", "use only one of --text, --markdown and --html")
	}

	recipients := readRecipientInput(cmd)
	to, err := validateRecipientFlag("to", recipients.To)
	if err != nil {
		return err
	}
	cc, err := validateRecipientFlag("cc", recipients.CC)
	if err != nil {
		return err
	}
	bcc, err := validateRecipientFlag("bcc", recipients.BCC)
	if err != nil {
		return err
	}
	if _, err := validateRecipientFlag("reply_to", recipients.ReplyTo); err != nil {
		return err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return invalidComposeInput("recipients", "the message has no recipients; add at least one To, Cc or Bcc address")
	}

	sender := readSenderInput(cmd)
	if sender.Provided {
		addrs, err := netmail.ParseAddressList(sender.Raw)
		if err != nil {
			return invalidComposeInput("from", "--from is not a valid address")
		}
		if len(addrs) != 1 {
			return invalidComposeInput("from", "--from takes one address")
		}
	}
	return nil
}

func validateRecipientFlag(field string, values []string) ([]mail.Recipient, error) {
	recipients, err := mail.ParseMailboxListValues(values)
	if err != nil {
		return nil, invalidComposeInput(field, mail.InvalidAddressMessage(strings.ReplaceAll(field, "_", "-"), err))
	}
	return recipients, nil
}

func clearSendInput(in *composepkg.SendInput) {
	if in == nil {
		return
	}
	zeroBytes(in.Body.Bytes)
	*in = composepkg.SendInput{}
}

func readUpdateInput(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.UpdateInput, error) {
	recipients := readRecipientInput(cmd)
	recipients.ClearTo, _ = cmd.Flags().GetBool("clear-to")
	recipients.ClearCC, _ = cmd.Flags().GetBool("clear-cc")
	recipients.ClearBCC, _ = cmd.Flags().GetBool("clear-bcc")
	recipients.ClearReplyTo, _ = cmd.Flags().GetBool("clear-reply-to")
	sender := readSenderInput(cmd)
	subject := readSubjectInput(cmd)
	body, err := readBodyInput(f, cmd, acct, policy)
	if err != nil {
		return composepkg.UpdateInput{}, err
	}
	return composepkg.UpdateInput{Recipients: recipients, Sender: sender, Subject: subject, Body: body}, nil
}

func readReplyOptions(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.ReplyOptions, error) {
	body, err := readBodyInput(f, cmd, acct, policy)
	if err != nil {
		return composepkg.ReplyOptions{}, err
	}
	replyAll, _ := cmd.Flags().GetBool("reply-all")
	draft, _ := cmd.Flags().GetBool("draft")
	send, _ := cmd.Flags().GetBool("send")
	quote, _ := cmd.Flags().GetString("quote")
	quoteLanguage, _ := cmd.Flags().GetString("quote-language")
	lookup := f.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return composepkg.ReplyOptions{
		ReplyAll:      replyAll,
		Draft:         draft,
		Send:          send,
		Quote:         quote,
		QuoteLanguage: quoteLanguage,
		Body:          body,
		EnvLookup:     lookup,
	}, nil
}

func readRecipientInput(cmd *cobra.Command) composepkg.RecipientInput {
	flags := cmd.Flags()
	to, _ := flags.GetStringArray("to")
	cc, _ := flags.GetStringArray("cc")
	bcc, _ := flags.GetStringArray("bcc")
	replyTo, _ := flags.GetStringArray("reply-to")
	return composepkg.RecipientInput{
		To: to, CC: cc, BCC: bcc, ReplyTo: replyTo,
		ToSet: flags.Changed("to"), CCSet: flags.Changed("cc"), BCCSet: flags.Changed("bcc"), ReplyToSet: flags.Changed("reply-to"),
	}
}

func readSenderInput(cmd *cobra.Command) composepkg.SenderInput {
	value, _ := cmd.Flags().GetString("from")
	return composepkg.SenderInput{Provided: cmd.Flags().Changed("from"), Raw: value}
}

func readSubjectInput(cmd *cobra.Command) composepkg.SubjectInput {
	value, _ := cmd.Flags().GetString("subject")
	return composepkg.SubjectInput{Provided: cmd.Flags().Changed("subject"), Value: value}
}

func readBodyInput(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) (composepkg.BodyInput, error) {
	flags := cmd.Flags()
	inline, _ := flags.GetString("body")
	file, _ := flags.GetString("body-file")
	in := composepkg.BodyInput{
		InlineSet:   flags.Changed("body"),
		Inline:      inline,
		FileSet:     flags.Changed("body-file"),
		TextSet:     flags.Changed("text"),
		MarkdownSet: flags.Changed("markdown"),
		HTMLSet:     flags.Changed("html"),
	}
	if !in.FileSet || in.InlineSet {
		// If --body and --body-file are both given, let internal/compose
		// report that, without reading the file first.
		if in.InlineSet && !in.FileSet && policy == provider.ComposeIngressMIMEBounded && int64(len(inline)) > mail.MaxOutboundEncodedMessageBytes {
			return composepkg.BodyInput{}, messageTooLargeForComposeSource(acct)
		}
		return in, nil
	}
	if file == "-" {
		data, err := readBodyBytes(f.Stdin, acct, policy, "could not read body from stdin")
		if err != nil {
			return composepkg.BodyInput{}, err
		}
		in.Bytes = data
		in.FromStdin = true
		return in, nil
	}
	data, err := readBodyFile(file, acct, policy)
	if err != nil {
		return composepkg.BodyInput{}, err
	}
	in.Bytes = data
	return in, nil
}

func readBodyFile(file string, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy) ([]byte, error) {
	if policy == provider.ComposeIngressDelivered {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, invalidComposeInput("body_file", "could not read body file")
		}
		return data, nil
	}
	handle, err := os.Open(file)
	if err != nil {
		return nil, invalidComposeInput("body_file", "could not read body file")
	}
	defer handle.Close()
	return readBodyBytes(handle, acct, policy, "could not read body file")
}

func readBodyBytes(src io.Reader, acct provider.ResolvedAccount, policy provider.ComposeIngressPolicy, readErrMessage string) ([]byte, error) {
	if policy == provider.ComposeIngressDelivered {
		data, err := io.ReadAll(src)
		if err != nil {
			return nil, invalidComposeInput("body_file", readErrMessage)
		}
		return data, nil
	}
	if policy != provider.ComposeIngressMIMEBounded {
		return nil, errs.NewProviderClientContractInvalid()
	}
	limit := mail.MaxOutboundEncodedMessageBytes
	data, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		return nil, invalidComposeInput("body_file", readErrMessage)
	}
	if int64(len(data)) > limit {
		return nil, messageTooLargeForComposeSource(acct)
	}
	return data, nil
}

func messageTooLargeForComposeSource(acct provider.ResolvedAccount) error {
	limit := uint64(mail.MaxOutboundEncodedMessageBytes)
	return errs.NewMessageTooLarge(acct.Name, errs.ProviderName(acct.Provider), errs.MessageSizeComposeSource, limit, limit+1)
}

func requiredDraftID(cmd *cobra.Command) (string, error) {
	id, _ := cmd.Flags().GetString("id")
	if strings.TrimSpace(id) == "" {
		return "", invalidComposeInput("id", "--id requires a value")
	}
	return strings.TrimSpace(id), nil
}

func invalidComposeInput(field, message string) error {
	return errs.NewWithDetails(errs.CodeInvalidComposeInput, message, map[string]any{"field": field})
}

func resultBodyFormat(body mail.OutboundBody) mail.BodyFormat {
	if body.Present || body.Bytes > 0 {
		return body.Format
	}
	return ""
}

func formatRecipientCounts(to, cc, bcc, replyTo int) string {
	parts := []string{fmt.Sprintf("to:%d", to), fmt.Sprintf("cc:%d", cc), fmt.Sprintf("bcc:%d", bcc)}
	if replyTo > 0 {
		parts = append(parts, fmt.Sprintf("reply-to:%d", replyTo))
	}
	return strings.Join(parts, " ")
}

func sourceIDForOutput(selector mail.MessageSelector, resolved mail.ResolvedMessageSelector, providerSourceID string) string {
	switch selector.Kind {
	case mail.SelectorID:
		return selector.ID
	case mail.SelectorQuery:
		return resolved.ID
	case mail.SelectorInternetID:
		return providerSourceID
	default:
		return providerSourceID
	}
}

func composeSourceForBody(material provider.ComposeSourceMaterial) composepkg.SourceMaterial {
	return composepkg.SourceMaterial{
		SourceID:             material.SourceID,
		InternetMessageID:    material.InternetMessageID,
		ThreadID:             material.ThreadID,
		From:                 material.From,
		ReplyTo:              append([]mail.Recipient(nil), material.ReplyTo...),
		To:                   append([]mail.Recipient(nil), material.To...),
		CC:                   append([]mail.Recipient(nil), material.CC...),
		Subject:              material.Subject,
		SentAt:               material.SentAt,
		InReplyTo:            append([]string(nil), material.InReplyTo...),
		References:           append([]string(nil), material.References...),
		Text:                 append([]byte(nil), material.Text...),
		HTML:                 append([]byte(nil), material.HTML...),
		HTMLHasCIDReferences: material.HTMLHasCIDReferences,
	}
}
