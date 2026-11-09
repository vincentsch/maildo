package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/htmltext"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// Sending mail is the one action that cannot be undone, so every way to send
// (send, draft send, reply --send, forward --send) goes through the same
// steps here:
//
//  1. --dry-run shows the whole message as it would go out and prints an
//     approval code, a short hash of that message and the account. The
//     printed command carries the code in --approve.
//  2. The account's policy.allow_recipients is checked.
//  3. A send needs one of these: --approve with the code of exactly this
//     message plus --yes (scripts and agents), a yes from a person in a
//     terminal, or the account's policy.unattended_send.
//  4. A code works once: a command with a code that already sent its
//     message is refused (sent_approvals.go).
//  5. Right before the provider hands the message over, it shows maildo
//     what it is about to send, and maildo compares the code once more. A
//     message that changed in between is not sent.
//  6. The send is written to the action log, whether it worked or not.

// outgoing is a message about to be sent or saved, as previews show it.
// The recipient lists and attachments are always in the JSON, as [] when
// they are empty, so a person or a script can check each of them.
type outgoing struct {
	From    *mail.Recipient  `json:"from,omitempty"`
	To      []mail.Recipient `json:"to"`
	CC      []mail.Recipient `json:"cc"`
	BCC     []mail.Recipient `json:"bcc"`
	ReplyTo []mail.Recipient `json:"reply_to"`
	Subject string           `json:"subject"`
	// InReplyTo and References keep a reply in its conversation. Both are
	// empty for a new message.
	InReplyTo   string               `json:"in_reply_to"`
	References  []string             `json:"references"`
	Body        string               `json:"body"`
	BodyHTML    string               `json:"body_html,omitempty"`
	Attachments []outgoingAttachment `json:"attachments"`
	// AttachmentsLeftOut are attachments of a forwarded message that do
	// not go along.
	AttachmentsLeftOut []string `json:"attachments_left_out,omitempty"`
	DraftID            string   `json:"draft_id,omitempty"`
	// content is what the approval code covers. It is nil when the
	// provider builds the message itself and maildo only knows parts of it.
	content *mail.SendContent
	// recipientsKnown is false when the provider picks the recipients
	// itself, as Outlook does for replies before the draft exists.
	recipientsKnown bool
}

type outgoingAttachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline,omitempty"`
}

// MarshalJSON writes the empty lists as [] instead of null.
func (o outgoing) MarshalJSON() ([]byte, error) {
	type plain outgoing
	p := plain(o)
	for _, list := range []*[]mail.Recipient{&p.To, &p.CC, &p.BCC, &p.ReplyTo} {
		if *list == nil {
			*list = []mail.Recipient{}
		}
	}
	if p.References == nil {
		p.References = []string{}
	}
	if p.Attachments == nil {
		p.Attachments = []outgoingAttachment{}
	}
	// Keep < and > as they are, like the rest of the output.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (o outgoing) recipients() []mail.Recipient {
	out := append([]mail.Recipient(nil), o.To...)
	out = append(out, o.CC...)
	return append(out, o.BCC...)
}

// newOutgoing describes the content a provider is going to send.
func newOutgoing(c mail.SendContent) outgoing {
	msg := outgoing{
		To:              c.To,
		CC:              c.CC,
		BCC:             c.BCC,
		ReplyTo:         c.ReplyTo,
		Subject:         c.Subject,
		InReplyTo:       c.InReplyTo,
		References:      c.References,
		Body:            readableBody(c.Text, c.HTML),
		BodyHTML:        c.HTML,
		content:         &c,
		recipientsKnown: true,
	}
	if c.From.Address != "" || c.From.Name != "" {
		from := c.From
		msg.From = &from
	}
	for _, a := range c.Attachments {
		msg.Attachments = append(msg.Attachments, outgoingAttachment{Name: a.Name, ContentType: a.ContentType, Size: a.Size, Inline: a.Inline})
	}
	return msg
}

// readableBody is the body as a person reads it: the text part, or the
// HTML part turned into text when there is no text part.
func readableBody(text, html string) string {
	body := text
	if strings.TrimSpace(body) == "" && strings.TrimSpace(html) != "" {
		body = htmltext.ToReadable(html)
	}
	return strings.ReplaceAll(body, "\r\n", "\n")
}

// senderOf is the From that maildo writes for an account: the account's
// sender_name (or the name the provider knows) and its address.
func senderOf(acct provider.ResolvedAccount) *mail.Recipient {
	from := &mail.Recipient{Address: acct.Email}
	if identity, err := provider.ComposeIdentityForAccount(acct); err == nil {
		from.Name = identity.DisplayName
	}
	return from
}

// outgoingFromMessage describes a new message that maildo composed.
func outgoingFromMessage(acct provider.ResolvedAccount, msg mail.OutboundMessage) outgoing {
	from := senderOf(acct)
	if msg.From != nil && msg.From.Name != "" {
		from.Name = msg.From.Name
	}
	return newOutgoing(provider.SendContentOfOutbound(*from, msg))
}

// outgoingFromDraft describes a stored draft as sending it would hand it
// over.
func outgoingFromDraft(d mail.Draft) outgoing {
	var msg outgoing
	if d.Send != nil {
		msg = newOutgoing(*d.Send)
	} else {
		msg = outgoing{To: d.To, CC: d.CC, BCC: d.BCC, ReplyTo: d.ReplyTo, Subject: d.Subject, Body: d.Body.Content, recipientsKnown: true}
	}
	msg.DraftID = d.ID
	return msg
}

// approvalCode is a short hash of everything a send hands over, and of the
// account that sends. Two messages that differ anywhere, down to the
// content of an attachment, get different codes.
func approvalCode(account string, c mail.SendContent) string {
	h := sha256.New()
	field := func(values ...string) {
		for _, v := range values {
			// The length goes first, so no two lists of values hash alike.
			fmt.Fprintf(h, "%d:%s\n", len(v), v)
		}
	}
	people := func(label string, rs []mail.Recipient) {
		field(label, strconv.Itoa(len(rs)))
		for _, r := range rs {
			field(r.Name, strings.ToLower(r.Address))
		}
	}
	field("maildo approval 1", account)
	people("from", []mail.Recipient{c.From})
	people("to", c.To)
	people("cc", c.CC)
	people("bcc", c.BCC)
	people("reply-to", c.ReplyTo)
	field("subject", c.Subject)
	field("in-reply-to", c.InReplyTo)
	field("references", strconv.Itoa(len(c.References)))
	field(c.References...)
	field("text", strings.ReplaceAll(c.Text, "\r\n", "\n"))
	field("html", strings.ReplaceAll(c.HTML, "\r\n", "\n"))
	field("attachments", strconv.Itoa(len(c.Attachments)))
	for _, a := range c.Attachments {
		field(a.Name, strconv.FormatInt(a.Size, 10), strconv.FormatBool(a.Inline), hex.EncodeToString(a.SHA256[:]))
	}
	sum := h.Sum(nil)
	return strings.ToLower(base32.StdEncoding.EncodeToString(sum[:5]))
}

// sendPreview is the --dry-run output of every send, and of replies and
// forwards that would only save a draft.
type sendPreview struct {
	DryRun    bool   `json:"dry_run"`
	Operation string `json:"operation"`
	// Send is true when the command sends, false when it saves a draft.
	Send     bool     `json:"send"`
	Account  string   `json:"account"`
	Provider string   `json:"provider"`
	Message  outgoing `json:"message"`
	// ApprovalCode is the code --approve needs to send exactly this
	// message.
	ApprovalCode string `json:"approval_code,omitempty"`
	// DraftSaved is true when maildo had to save the message as a draft to
	// show it, as for Outlook replies. The draft id is in the message.
	DraftSaved bool `json:"draft_saved,omitempty"`
	// SentBefore is set when maildo sent this exact message before.
	SentBefore  *sentBefore `json:"sent_before,omitempty"`
	SendCommand string      `json:"send_command,omitempty"`
}

// previewCommand is the command a send preview prints: the command the user
// ran, for the account the preview showed, with the approval code and
// --yes. pin replaces flags that picked a message (for a reply or forward),
// so the command answers the message shown even if newer mail matches the
// same search.
func previewCommand(cmd *cobra.Command, acct provider.ResolvedAccount, code string, pin [][2]string) string {
	set := append([][2]string{{"account", acct.Name}}, pin...)
	drop := []string{"dry-run", "full"}
	if len(pin) > 0 {
		drop = append(drop, "id", "internet-id", "query", "latest", "first")
	}
	if code != "" {
		set = append(set, [2]string{"approve", code}, [2]string{"yes", ""})
	}
	command := commandLine(cmd, nil, drop, set)
	if len(command) > 400 && len(pin) == 0 {
		return ""
	}
	return command
}

// draftSendCommand sends one draft with its approval code. It keeps the
// --config and --json the user gave.
func draftSendCommand(cmd *cobra.Command, acct provider.ResolvedAccount, draftID, code string) string {
	parts := []string{"maildo", "draft", "send"}
	if flag := cmd.Flags().Lookup("config"); flag != nil && flag.Changed {
		parts = append(parts, "--config", shellQuote(flag.Value.String()))
	}
	if flag := cmd.Flags().Lookup("json"); flag != nil && flag.Changed && flag.Value.String() == "true" {
		parts = append(parts, "--json")
	}
	parts = append(parts, "--account", shellQuote(acct.Name), "--id", shellQuote(draftID), "--approve", code, "--yes")
	return strings.Join(parts, " ")
}

// writeSendPreview shows a message that would be sent, with its approval
// code and the command that sends it.
func (h *handlers) writeSendPreview(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, operation string, msg outgoing, pin [][2]string) error {
	if err := checkSendRecipients(acct, msg); err != nil {
		return err
	}
	code := approvalCode(acct.Name, *msg.content)
	command := previewCommand(cmd, acct, code, pin)
	if operation == "draft_send" {
		command = draftSendCommand(cmd, acct, msg.DraftID, code)
	}
	return writeComposePreview(f, cmd, acct, sendPreview{Operation: operation, Send: true, Message: msg, ApprovalCode: code, SendCommand: command})
}

// writeComposePreview prints a send or draft preview. The policy applies
// to previews too, so a preview never promises a send that would be
// refused.
func writeComposePreview(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, p sendPreview) error {
	if p.Message.recipientsKnown {
		if err := provider.CheckRecipients(acct, p.Message.recipients()); err != nil {
			return err
		}
	}
	p.DryRun = true
	p.Account = acct.Name
	p.Provider = acct.Provider
	if p.Send && p.ApprovalCode != "" {
		p.SentBefore = newSentBefore(previewApproval(sentApprovalsPath(f), p.ApprovalCode, true))
	}
	full, _ := cmd.Flags().GetBool("full")
	return f.WriteResult(p, func(w io.Writer) {
		switch {
		case p.DraftSaved:
			fmt.Fprintf(w, "Outlook writes replies and forwards itself, so maildo saved this one as a draft to show it to you. Nothing was sent.\n\n")
			fmt.Fprintf(w, "Would send this draft from %s:\n\n", plainField(acct.Name))
		case p.Send:
			fmt.Fprintf(w, "Would send this message from %s:\n\n", plainField(acct.Name))
		default:
			fmt.Fprintf(w, "Would save this draft in %s:\n\n", plainField(acct.Name))
		}
		writeOutgoing(w, p.Message, full)
		fmt.Fprintln(w)
		writeSenderNote(w, acct, p.Message)
		writeSentBefore(w, acct.Name, p.SentBefore)
		switch {
		case !p.Send && p.SendCommand == "":
			fmt.Fprintln(w, "Nothing was saved. To save it, run the same command without --dry-run.")
		case !p.Send:
			fmt.Fprintln(w, "Nothing was saved. To save it, run:")
			fmt.Fprintln(w, "  "+p.SendCommand)
		case p.SendCommand == "":
			fmt.Fprintf(w, "Nothing was sent. The approval code for this message is %s.\n", p.ApprovalCode)
			fmt.Fprintf(w, "To send it, run the same command with --approve %s --yes instead of --dry-run.\n", p.ApprovalCode)
		default:
			if !p.DraftSaved {
				fmt.Fprint(w, "Nothing was sent. ")
			}
			fmt.Fprintf(w, "The approval code for this message is %s.\n", p.ApprovalCode)
			fmt.Fprintln(w, "To send it, run:")
			fmt.Fprintln(w, "  "+p.SendCommand)
		}
		if bodyFile, _ := cmd.Flags().GetString("body-file"); bodyFile == "-" && !p.DraftSaved {
			fmt.Fprintln(w, "The command reads the body from stdin, so pipe in the same text again.")
		}
		if p.DraftSaved {
			fmt.Fprintf(w, "If you do not want to send it, delete the draft with: maildo delete --account %s --id %s\n", shellQuote(acct.Name), shellQuote(p.Message.DraftID))
		}
	})
}

// previewBodyLines is how many body lines a preview shows before it asks for
// --full. The interactive confirmation and JSON always have the whole body.
const previewBodyLines = 40

func writeOutgoing(w io.Writer, msg outgoing, full bool) {
	field := func(label, value string) {
		if value != "" {
			fmt.Fprintf(w, "  %-9s %s\n", label+":", value)
		}
	}
	if msg.From != nil {
		field("From", recipientText(*msg.From))
	}
	if msg.recipientsKnown {
		field("To", recipientsText(msg.To))
	} else {
		field("To", "chosen by the provider from the original message")
	}
	// Empty fields still get a line, so the preview says when there are
	// none.
	field("Cc", firstNonEmpty(recipientsText(msg.CC), "(none)"))
	field("Bcc", firstNonEmpty(recipientsText(msg.BCC), "(none)"))
	field("Reply-To", firstNonEmpty(recipientsText(msg.ReplyTo), "(none)"))
	field("Subject", firstNonEmpty(plainField(msg.Subject), "(no subject)"))
	if msg.InReplyTo != "" {
		field("Thread", "replies to "+plainField(msg.InReplyTo))
	}
	names := []string{"(none)"}
	if len(msg.Attachments) > 0 {
		names = nil
		for _, a := range msg.Attachments {
			size := byteSize(a.Size)
			if a.Inline {
				size += ", inline"
			}
			names = append(names, fmt.Sprintf("%s (%s)", firstNonEmpty(a.Name, "(no name)"), size))
		}
	}
	field("Attached", plainField(strings.Join(names, ", ")))
	if len(msg.AttachmentsLeftOut) > 0 {
		field("Left out", plainField(strings.Join(msg.AttachmentsLeftOut, ", ")))
	}
	fmt.Fprintln(w)
	lines := strings.Split(strings.TrimRight(msg.Body, "\n"), "\n")
	if strings.TrimSpace(msg.Body) == "" {
		fmt.Fprintln(w, "  (no text)")
		return
	}
	hidden := 0
	if !full && len(lines) > previewBodyLines {
		hidden = len(lines) - previewBodyLines
		lines = lines[:previewBodyLines]
	}
	for _, line := range lines {
		fmt.Fprintln(w, strings.TrimRight("  "+plainField(line), " "))
	}
	if hidden > 0 {
		fmt.Fprintf(w, "\n  (%s of the body are not shown. Add --full to see all of it.)\n", pluralize(hidden, "more line", "more lines"))
	}
}

// writeSenderNote says when From has only the address, and how to add a
// name. Outlook takes the name from the mailbox, so it gets no note.
func writeSenderNote(w io.Writer, acct provider.ResolvedAccount, msg outgoing) {
	if msg.From == nil || strings.TrimSpace(msg.From.Name) != "" || acct.Provider == "outlook" {
		return
	}
	fmt.Fprintln(w, "The From line has only your address and no name. To add your name, run:")
	fmt.Fprintf(w, "  maildo config set sender_name \"Your Name\" --account %s\n", shellQuote(acct.Name))
}

// outgoingForReply describes a reply that Outlook writes itself, for the
// preview of a reply draft. Outlook picks the recipients and adds the quote
// when it saves the draft, so the preview shows only maildo's part and the
// Message-ID of the message it answers.
func outgoingForReply(acct provider.ResolvedAccount, subject, sourceInternetID string, body mail.OutboundBody) outgoing {
	text, html := provider.MIMEBodies(body)
	return outgoing{
		From:      senderOf(acct),
		Subject:   prefixSubject("Re: ", subject),
		InReplyTo: sourceInternetID,
		Body:      readableBody(text, html),
		BodyHTML:  html,
	}
}

// prefixSubject adds "Re: " or "Fwd: " unless the subject has it already
// (also in German: "AW:", "WG:").
func prefixSubject(prefix, subject string) string {
	trimmed := strings.TrimSpace(subject)
	lower := strings.ToLower(trimmed)
	same := []string{"re:", "aw:"}
	if strings.HasPrefix(strings.ToLower(prefix), "f") {
		same = []string{"fwd:", "fw:", "wg:"}
	}
	for _, p := range same {
		if strings.HasPrefix(lower, p) {
			return trimmed
		}
	}
	return prefix + trimmed
}

// checkSendRecipients applies policy.allow_recipients to a message. When
// the provider picks the recipients itself, maildo cannot check them yet,
// so an allow list refuses the send.
func checkSendRecipients(acct provider.ResolvedAccount, msg outgoing) error {
	if msg.recipientsKnown {
		return provider.CheckRecipients(acct, msg.recipients())
	}
	if len(acct.Policy.AllowRecipients) > 0 {
		return errs.NewWithDetails(errs.CodePolicyDenied,
			"maildo cannot check this message's recipients against the account's allow list before sending",
			map[string]any{"account": acct.Name, "provider": acct.Provider, "policy": "allow_recipients",
				"hint": "create a draft without --send, check it with maildo draft read, then send it with maildo draft send"})
	}
	return nil
}

// checkSendFlags refuses a send that cannot be approved, before maildo
// writes anything: --yes without --approve, or no --yes and no terminal to
// ask. policy.unattended_send lifts both.
func checkSendFlags(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount) error {
	if acct.Policy.UnattendedSend {
		return nil
	}
	yes, _ := cmd.Flags().GetBool("yes")
	approve, _ := cmd.Flags().GetString("approve")
	switch {
	case yes && strings.TrimSpace(approve) == "":
		return errs.NewWithDetails(errs.CodeConfirmationRequired, "--yes needs --approve with the approval code from the preview, so maildo sends only the message that was approved",
			map[string]any{
				"account": acct.Name,
				"hint":    "run the command with --dry-run, show the preview to whoever approves it, then run the command it prints",
			})
	case !yes && !f.CanPrompt():
		return errs.NewWithDetails(errs.CodeConfirmationRequired, "maildo sends mail only after a confirmation, and there is no terminal to ask",
			map[string]any{
				"account": acct.Name,
				"hint":    "run the command with --dry-run, show the preview to whoever approves it, then run the command it prints",
			})
	}
	return nil
}

// approveSend runs every check before a send and writes the send down as
// started. The caller passes the approval's code to the provider through
// sendCheck, so the provider sends only content with that code, and then
// hands the approval to logSend.
func (h *handlers) approveSend(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, msg outgoing) (approval, error) {
	if err := provider.CheckPolicy(acct, provider.ClientAccessDirectSend); err != nil {
		return approval{}, err
	}
	if err := checkSendRecipients(acct, msg); err != nil {
		return approval{}, err
	}
	if msg.content == nil {
		return approval{}, errs.NewProviderClientContractInvalid()
	}
	code := approvalCode(acct.Name, *msg.content)
	approve, _ := cmd.Flags().GetString("approve")
	approve = strings.ToLower(strings.TrimSpace(approve))
	if approve != "" && approve != code {
		return approval{}, approvalMismatch(acct, "this message is not the one that was approved; it changed after the preview, so nothing was sent")
	}
	if err := checkSendFlags(f, cmd, acct); err != nil {
		return approval{}, err
	}
	if approve != "" {
		if err := checkApprovalUnused(sentApprovalsPath(f), acct, code); err != nil {
			return approval{}, err
		}
	}
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes && !acct.Policy.UnattendedSend {
		fmt.Fprintf(f.Stderr, "About to send this message from %s:\n\n", acct.Name)
		writeOutgoing(f.Stderr, msg, true)
		fmt.Fprintln(f.Stderr)
		writeSenderNote(f.Stderr, acct, msg)
		writeSentBefore(f.Stderr, acct.Name, newSentBefore(previewApproval(sentApprovalsPath(f), code, false)))
		ok, err := f.Chooser().Confirm("Send this message?", "Yes, send it", "No, cancel")
		if err != nil || !ok {
			return approval{}, errs.NewWithDetails(errs.CodeConfirmationRequired, "you cancelled the send, so nothing was sent",
				map[string]any{"account": acct.Name})
		}
	}
	return claimApproval(sentApprovalsPath(f), h.deps.Now(), acct, code, approve != "")
}

// sendCheck is the last check, run by the provider with exactly what it is
// about to send.
func sendCheck(acct provider.ResolvedAccount, a approval) mail.SendCheck {
	return func(c mail.SendContent) error {
		if approvalCode(acct.Name, c) != a.code {
			return approvalMismatch(acct, "the message changed after maildo checked it, so nothing was sent")
		}
		return nil
	}
}

func approvalMismatch(acct provider.ResolvedAccount, message string) error {
	return errs.NewWithDetails(errs.CodeApprovalMismatch, message, map[string]any{
		"account": acct.Name,
		"hint":    "run the command with --dry-run again, show the new preview to whoever approves it, and use its code",
	})
}

// logSend writes down how a send went: in the record of used approval
// codes, and in the action log. A send that failed is logged too, with the
// error code, since it may still have reached the server.
func (h *handlers) logSend(f *rungrad.Factory, cmd *cobra.Command, acct provider.ResolvedAccount, operation string, msg outgoing, a approval, messageID string, sendErr error) mail.ActionLogStatus {
	finishApproval(f, a, sendErr)
	status := mail.ActionLogStatus{RelativePath: audit.RelativePath}
	record := &audit.SentMessage{
		Account:   acct.Name,
		Provider:  acct.Provider,
		MessageID: messageID,
		DraftID:   msg.DraftID,
		To:        addresses(msg.To),
		CC:        addresses(msg.CC),
		BCC:       addresses(msg.BCC),
		Subject:   msg.Subject,
		Outcome:   "sent",
	}
	if sendErr != nil {
		record.Outcome = "failed"
		record.ErrorCode = productErrorCode(sendErr)
	}
	entry := audit.Entry{
		Version:   1,
		CreatedAt: h.deps.Now().UTC(),
		Command:   actionLogCommandPath(cmd),
		Operation: mail.MessageMutationOperation(operation),
		Sent:      record,
	}
	if err := audit.Append(filepath.Dir(f.ConfigPath()), entry); err != nil {
		status.Reason = "append_failed"
		status.ErrorCode = errs.CodeActionLogWriteFailed
		f.Infof("Warning: the send was not written to the action log: %v", err)
		return status
	}
	status.Written = true
	status.EntryCount = 1
	return status
}

func addresses(rs []mail.Recipient) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Address)
	}
	return out
}

// humanSent says what was sent, to whom.
func humanSent(acct string, msg outgoing, sentCopy string) func(io.Writer) {
	return func(w io.Writer) {
		to := "the original sender"
		if msg.recipientsKnown {
			to = shortRecipientsText(append(append([]mail.Recipient(nil), msg.To...), msg.CC...))
			switch {
			case to == "":
				to = "Bcc " + shortRecipientsText(msg.BCC)
			case len(msg.BCC) > 0:
				to += " (Bcc: " + shortRecipientsText(msg.BCC) + ")"
			}
		}
		fmt.Fprintf(w, "Sent %s to %s from %s.", subjectText(msg.Subject), to, plainField(acct))
		if sentCopy != "" {
			fmt.Fprintf(w, " A copy is in %s.", plainField(sentCopy))
		}
		fmt.Fprintln(w)
	}
}

// shortRecipientsText names up to five recipients, and counts the rest.
func shortRecipientsText(rs []mail.Recipient) string {
	if len(rs) <= 5 {
		return recipientsText(rs)
	}
	return recipientsText(rs[:3]) + fmt.Sprintf(" and %d more", len(rs)-3)
}

// warnAll prints warnings for people on stderr; JSON has them in
// "warnings".
func warnAll(f *rungrad.Factory, warnings []string) {
	for _, w := range warnings {
		f.Infof("Warning: %s", w)
	}
}
