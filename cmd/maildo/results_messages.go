package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/search"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// searchResult is the JSON result of maildo search.
type searchResult struct {
	provider.AccountSelection
	Query mail.Query `json:"query"`
	MessagePage
}

// MessagePage is the paging part of list and search output. total is left
// out when an account could not count its matches.
type MessagePage struct {
	Messages []mail.MessageSummary `json:"messages"`
	Count    int                   `json:"count"`
	Total    *int                  `json:"total,omitempty"`
	Page     int                   `json:"page"`
	HasMore  bool                  `json:"has_more"`
	Warnings []string              `json:"warnings,omitempty"`
}

func newMessagePage(page search.Page, offset, limit int) MessagePage {
	out := MessagePage{
		Messages: page.Messages,
		Count:    len(page.Messages),
		Page:     offset/limit + 1,
		HasMore:  page.HasMore,
		Warnings: page.Warnings,
	}
	if page.TotalKnown {
		total := page.Total
		out.Total = &total
	}
	return out
}

// listMessagesResult is the JSON result of maildo list.
type listMessagesResult struct {
	provider.AccountSelection
	Filter mail.ListFilter `json:"filter"`
	MessagePage
}

// messagesMeta is the --include-meta data for list and search, such as the
// accounts and the number of messages.
func messagesMeta(commandPath string, sel provider.AccountSelection, count int) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path":  commandPath,
		"scope":         sel.Scope,
		"accounts":      accounts,
		"providers":     providers,
		"message_count": count,
	}}
}

// messageView says what a message table shows, for its summary line.
type messageView struct {
	empty    string // "No messages in INBOX (work)."
	showing  string // "messages in INBOX (work)"
	folders  bool   // add a FOLDER column (search spans folders)
	nextPage string // command for the next page, when there is one
}

// humanMessageList prints messages as an aligned table with short ids,
// sender names, subjects and local relative dates. The short ids are saved
// so later commands accept them.
func humanMessageList(f *rungrad.Factory, page MessagePage, offset int, view messageView) func(io.Writer) {
	rows := page.Messages
	refs := make([]shortIDEntry, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, messageRefWithSubject(row.Account, row.ID, row.Subject))
	}
	ids := rememberShortIDs(f, refs)
	quiet := f.Flags != nil && f.Flags.Quiet
	return func(w io.Writer) {
		if len(rows) == 0 {
			if !quiet {
				fmt.Fprintln(w, view.empty)
			}
			return
		}
		accounts := map[string]bool{}
		for _, row := range rows {
			accounts[row.Account] = true
		}
		header := []string{"", "ID"}
		if len(accounts) > 1 {
			header = append(header, "ACCOUNT")
		}
		header = append(header, "FROM", "SUBJECT")
		subjectCol := len(header) - 1
		if view.folders {
			header = append(header, "FOLDER")
		}
		header = append(header, "DATE")
		t := newTable(header...)
		t.flex = subjectCol
		marks := false
		for _, row := range rows {
			mark := ""
			if !row.IsRead {
				mark += "*"
			}
			if row.Flagged {
				mark += "!"
			}
			if row.HasAttachments {
				mark += "+"
			}
			if mark != "" {
				marks = true
			}
			cells := []string{mark, ids.of(row.Account, row.ID)}
			if len(accounts) > 1 {
				cells = append(cells, row.Account)
			}
			subject := strings.TrimSpace(row.Subject)
			if subject == "" {
				subject = "(no subject)"
			}
			cells = append(cells, truncate(senderName(row.From), 24), subject)
			if view.folders {
				cells = append(cells, truncate(folderSummaryText(row.Folder), 20))
			}
			cells = append(cells, relativeTime(row.Time()))
			t.add(cells...)
		}
		if quiet {
			// --quiet prints the table only.
			t.render(w, screenWidth(f), boldStyle(f))
			return
		}
		first := offset + 1
		last := offset + len(rows)
		switch {
		case page.Total != nil && offset == 0 && len(rows) == *page.Total:
			fmt.Fprintf(w, "%s\n\n", plainField(pluralizeView(*page.Total, view.showing)))
		case page.Total == nil && offset == 0 && !page.HasMore:
			// Gmail gives no total, but the whole list fits on this page.
			fmt.Fprintf(w, "%s\n\n", plainField(pluralizeView(len(rows), view.showing)))
		case page.Total != nil:
			fmt.Fprintf(w, "Showing %d-%d of %s\n\n", first, last, plainField(pluralizeView(*page.Total, view.showing)))
		default:
			fmt.Fprintf(w, "Showing %d-%d %s\n\n", first, last, plainField(view.showing))
		}
		t.render(w, screenWidth(f), boldStyle(f))
		if marks {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "* unread  ! flagged  + attachment")
		}
		switch {
		case view.nextPage == "":
		case terminalSafe(view.nextPage):
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Next page: "+view.nextPage)
		default:
			fmt.Fprintln(w)
			fmt.Fprintln(w, "There are more messages. Add --page to the same command to see them.")
		}
	}
}

// pluralizeView turns "messages in INBOX" into "1 message in INBOX" or
// "2,512 messages in INBOX".
func pluralizeView(total int, showing string) string {
	if total == 1 {
		return "1 " + strings.Replace(showing, "messages", "message", 1)
	}
	return thousands(total) + " " + showing
}

// plainMessageList is the --plain output: one tab-separated line per message
// with its ids, for scripts that don't want JSON.
func plainMessageList(rows []mail.MessageSummary) func(io.Writer) {
	return func(w io.Writer) {
		for _, row := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				plainField(row.Account),
				plainField(row.Provider),
				plainField(row.ID),
				plainField(row.InternetMessageID),
				plainField(row.Subject),
			)
		}
	}
}

// accountSuffix names the account, or counts them: " (work)", " (3 accounts)".
func accountSuffix(sel provider.AccountSelection) string {
	switch len(sel.Accounts) {
	case 0:
		return ""
	case 1:
		return " (" + sel.Accounts[0].Name + ")"
	default:
		return fmt.Sprintf(" (%d accounts)", len(sel.Accounts))
	}
}

// nextPageCommand is the command for the next page, or "" on the last one.
func nextPageCommand(cmd *cobra.Command, args []string, page MessagePage) string {
	if !page.HasMore {
		return ""
	}
	return commandLine(cmd, args, nil, [][2]string{{"page", strconv.Itoa(page.Page + 1)}})
}
