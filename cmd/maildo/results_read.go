package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// readResult is the JSON result of maildo read.
type readResult struct {
	provider.AccountSelection
	Message mail.Message `json:"message"`
}

// threadResult is the JSON result of maildo thread.
type threadResult struct {
	provider.AccountSelection
	Thread mail.Thread `json:"thread"`
}

// readMeta is the --include-meta data for read: the command, the account
// and the body mode. It never has message content or credentials.
func readMeta(commandPath string, sel provider.AccountSelection, selector mail.MessageSelector, bodyMode mail.BodyMode, searched *provider.AccountSelection) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path":  commandPath,
		"scope":         sel.Scope,
		"accounts":      accounts,
		"providers":     providers,
		"selector_kind": string(selector.Kind),
		"body_mode":     string(bodyMode),
	}
	addSearchedMeta(extra, searched)
	return output.Meta{Extra: extra}
}

// threadMeta is the --include-meta data for thread, with the number of
// messages.
func threadMeta(commandPath string, sel provider.AccountSelection, selector mail.MessageSelector, count int, searched *provider.AccountSelection) output.Meta {
	accounts, providers := accountProviderNames(sel)
	extra := map[string]any{
		"command_path":  commandPath,
		"scope":         sel.Scope,
		"accounts":      accounts,
		"providers":     providers,
		"selector_kind": string(selector.Kind),
		"message_count": count,
	}
	addSearchedMeta(extra, searched)
	return output.Meta{Extra: extra}
}

// addSearchedMeta adds the accounts that --query searched to --include-meta.
// The result itself names only the account the message is in.
func addSearchedMeta(extra map[string]any, searched *provider.AccountSelection) {
	if searched == nil {
		return
	}
	accounts, _ := accountProviderNames(*searched)
	extra["searched_scope"] = searched.Scope
	extra["searched_accounts"] = accounts
}

// humanReadMessage shows the headers people read (subject, sender,
// recipients, date, folder, attachments) and then the body. The full header
// block, the Message-ID and the thread id are in --json.
func humanReadMessage(f *rungrad.Factory, r readResult) func(io.Writer) {
	m := r.Message
	ids := rememberShortIDs(f, []shortIDEntry{messageRefWithSubject(m.Account, m.ID, m.Subject)})
	return func(w io.Writer) {
		field := func(label, value string) {
			fmt.Fprintf(w, "%-9s %s\n", label+":", value)
		}
		field("Subject", firstNonEmpty(strings.TrimSpace(plainField(m.Subject)), "(no subject)"))
		field("From", recipientText(m.From))
		if m.Sender != nil && !sameAddress(*m.Sender, m.From) {
			field("Sender", recipientText(*m.Sender))
		}
		if len(m.ReplyTo) > 0 && !(len(m.ReplyTo) == 1 && sameAddress(m.ReplyTo[0], m.From)) {
			field("Reply-To", recipientsText(m.ReplyTo))
		}
		if len(m.To) > 0 {
			field("To", recipientsText(m.To))
		}
		if len(m.CC) > 0 {
			field("Cc", recipientsText(m.CC))
		}
		if len(m.BCC) > 0 {
			field("Bcc", recipientsText(m.BCC))
		}
		if t := firstTime(m.ReceivedAt, m.SentAt); t != nil {
			date := t.Local().Format("Mon, 2 Jan 2006 15:04")
			if rel := relativeTime(*t); strings.HasSuffix(rel, "ago") || rel == "yesterday" || rel == "just now" {
				date += " (" + rel + ")"
			}
			field("Date", date)
		}
		folder := plainField(folderSummaryText(m.Folder))
		if !m.IsRead {
			folder += ", unread"
		}
		field("Folder", folder+" ("+plainField(m.Account)+")")
		if len(m.Categories) > 0 {
			field("Labels", plainField(strings.Join(m.Categories, ", ")))
		}
		field("ID", ids.of(m.Account, m.ID))
		if len(m.Attachments) > 0 {
			fmt.Fprintln(w, "Attachments:")
			t := newTable()
			t.indent = "  "
			for _, a := range m.Attachments {
				t.add(firstNonEmpty(a.Name, "(no name)"), attachmentSize(a))
			}
			t.render(w, 0, nil)
		}
		for _, warning := range m.Warnings {
			fmt.Fprintf(w, "Warning:  %s\n", plainField(warning))
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimRight(m.Body.Content, "\n"))
	}
}

func sameAddress(a, b mail.Recipient) bool {
	return strings.EqualFold(a.Address, b.Address)
}

func recipientsText(recipients []mail.Recipient) string {
	items := make([]string, 0, len(recipients))
	for _, r := range recipients {
		items = append(items, recipientText(r))
	}
	return strings.Join(items, ", ")
}

// attachmentSize prints a size people read: "2.1 KB". When only the
// encoded size is known, the decoded size is an estimate ("about 2.0 KB").
func attachmentSize(a mail.Attachment) string {
	if a.Size <= 0 {
		return "size unknown"
	}
	text := byteSize(a.Size)
	if a.SizeFactsPresent && !a.SizeFacts.DecodedSizeKnown {
		text = "about " + text
	}
	return text
}

func byteSize(n int64) string {
	switch {
	case n == 1:
		return "1 byte"
	case n < 1000:
		return fmt.Sprintf("%d bytes", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f KB", float64(n)/1000)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/(1000*1000))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1000*1000*1000))
	}
}

func plainReadBody(m mail.Message) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintln(w, m.Body.Content)
	}
}

// humanThread lists the messages of a thread, oldest first, with a short
// preview of each.
func humanThread(f *rungrad.Factory, r threadResult) func(io.Writer) {
	refs := make([]shortIDEntry, 0, len(r.Thread.Messages))
	for _, row := range r.Thread.Messages {
		refs = append(refs, messageRefWithSubject(row.Account, row.ID, row.Subject))
	}
	ids := rememberShortIDs(f, refs)
	return func(w io.Writer) {
		rows := r.Thread.Messages
		if len(rows) == 0 {
			fmt.Fprintln(w, "No messages.")
			return
		}
		subject := firstNonEmpty(strings.TrimSpace(plainField(rows[0].Subject)), "(no subject)")
		fmt.Fprintf(w, "%s, %s\n\n", subject, pluralize(len(rows), "message", "messages"))
		width := screenWidth(f)
		for i, row := range rows {
			if i > 0 {
				fmt.Fprintln(w)
			}
			mark := ""
			if !row.IsRead {
				mark = ", unread"
			}
			if row.HasAttachments {
				mark += ", with attachments"
			}
			fmt.Fprintf(w, "%s  %s  %s (%s%s)\n", ids.of(row.Account, row.ID), plainField(senderName(row.From)),
				relativeTime(threadRowTime(row)), plainField(folderSummaryText(row.Folder)), mark)
			// Outlook's previews keep the line breaks of the body; one
			// line per message reads better.
			if preview := strings.Join(strings.Fields(plainField(row.Preview)), " "); preview != "" {
				fmt.Fprintln(w, "  "+truncate(preview, max(width-2, 40)))
			}
		}
		last := rows[len(rows)-1]
		fmt.Fprintf(w, "\nTo read a whole message, run: maildo read %s\n", ids.of(last.Account, last.ID))
	}
}

func recipientText(r mail.Recipient) string {
	switch {
	// Outlook fills in the address as the name of a recipient who has none.
	case r.Name != "" && r.Address != "" && !strings.EqualFold(strings.TrimSpace(r.Name), r.Address):
		return plainField(r.Name) + " <" + plainField(r.Address) + ">"
	case r.Address != "":
		return plainField(r.Address)
	case r.Name != "":
		return plainField(r.Name)
	default:
		return "-"
	}
}

func folderSummaryText(f mail.FolderSummary) string {
	if f.Path != "" {
		return f.Path
	}
	if f.DisplayName != "" {
		return f.DisplayName
	}
	if f.ID != "" {
		return f.ID
	}
	return "-"
}

func threadRowTime(row mail.ThreadMessage) time.Time {
	if row.SentAt != nil {
		return *row.SentAt
	}
	if row.ReceivedAt != nil {
		return *row.ReceivedAt
	}
	return time.Time{}
}

func firstTime(times ...*time.Time) *time.Time {
	for _, t := range times {
		if t != nil {
			return t
		}
	}
	return nil
}
