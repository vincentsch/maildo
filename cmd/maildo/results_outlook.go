package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad/output"
)

// archivePolicyCheckResult puts the account selection first, so its JSON
// starts like the other commands' JSON.
type archivePolicyCheckResult struct {
	provider.AccountSelection
	mail.ArchivePolicyReport
}

// humanArchivePolicy shows the three things people mix up as separate
// sections: the Archive folder that Outlook's Archive button uses, the
// Exchange Online Archive mailbox, and the Exchange retention policy.
// Text from the server is made safe for the terminal only here; JSON keeps
// the exact values.
func humanArchivePolicy(result archivePolicyCheckResult) func(io.Writer) {
	return func(w io.Writer) {
		report := result.ArchivePolicyReport
		field := func(label, value string) {
			fmt.Fprintf(w, "  %-12s %s\n", label+":", value)
		}
		fmt.Fprintf(w, "Archive settings for %s (%s)\n", plainField(report.Account), plainField(report.Provider))
		fmt.Fprintf(w, "Report: %s. Mailbox type: %s.\n\n", plainField(string(report.Status)), plainField(firstNonEmpty(report.MailboxPurpose, "not reported")))

		fmt.Fprintln(w, "Archive folder")
		fmt.Fprintln(w, "  The folder Outlook's Archive button moves mail to. It is not the Exchange Online Archive mailbox.")
		if report.ArchiveFolder == nil {
			fmt.Fprintln(w, "  maildo could not read the folder's details.")
		} else {
			folder := report.ArchiveFolder
			field("Name", plainField(folder.DisplayName))
			field("Messages", archivePolicyCount(folder.TotalItemCount, folder.TotalItemCountKnown))
			field("Unread", archivePolicyCount(folder.UnreadItemCount, folder.UnreadItemCountKnown))
			field("Subfolders", archivePolicyCount(folder.ChildFolderCount, folder.ChildFolderCountKnown))
		}
		field("ID", plainField(firstNonEmpty(report.ArchiveFolderID, "not reported")))

		fmt.Fprintln(w, "\nExchange Online Archive")
		fmt.Fprintln(w, "  "+archivePolicyAvailabilityText(report.OnlineArchive, "maildo cannot tell whether this mailbox has one"))

		fmt.Fprintln(w, "\nRetention policy (Exchange MRM)")
		fmt.Fprintln(w, "  "+archivePolicyAvailabilityText(report.RetentionPolicy, "maildo cannot tell which retention policy applies"))

		if len(report.Warnings) > 0 {
			fmt.Fprintln(w)
			for _, warning := range report.Warnings {
				fmt.Fprintln(w, "Warning: "+archivePolicyWarningText(warning))
			}
		}
	}
}

// archivePolicyAvailabilityText says why maildo cannot see an Exchange
// setting. It never says the feature is off, because maildo does not know.
func archivePolicyAvailabilityText(a mail.ArchivePolicyAvailability, unknown string) string {
	if a.Status != mail.ArchivePolicyAvailabilityUnavailable {
		return fmt.Sprintf("Status: %s (%s).", plainField(string(a.Status)), plainField(string(a.Reason)))
	}
	switch a.Reason {
	case mail.ArchivePolicyReasonNotExposedByGraphMailAPI:
		return unknown + ", because Microsoft Graph's mail API does not show it."
	case mail.ArchivePolicyReasonRequiresExchangeAdminContext:
		return unknown + ", because only an Exchange Online administrator can see it."
	default:
		return unknown + " (" + plainField(string(a.Reason)) + ")."
	}
}

func archivePolicyCount(value int, known bool) string {
	if !known {
		return "unknown"
	}
	return strconv.Itoa(value)
}

func archivePolicyWarningText(warning mail.ArchivePolicyWarning) string {
	switch warning {
	case mail.ArchivePolicyWarningMailboxSettingsUnavailable:
		return "maildo could not read the mailbox settings, so it does not know which folder is the Archive folder."
	case mail.ArchivePolicyWarningFolderIDMissing:
		return "the mailbox settings do not name an Archive folder."
	case mail.ArchivePolicyWarningFolderUnresolved:
		return "maildo could not find the folder that the mailbox settings name as the Archive folder."
	default:
		return plainField(string(warning))
	}
}

// archivePolicyMeta holds the account and the report state. The folder and
// mailbox facts are in the result itself.
func archivePolicyMeta(commandPath string, result archivePolicyCheckResult) output.Meta {
	warnings := make([]string, len(result.Warnings))
	for i, warning := range result.Warnings {
		warnings[i] = string(warning)
	}
	return output.Meta{Extra: map[string]any{
		"account":       result.Account,
		"command_path":  commandPath,
		"provider":      result.Provider,
		"report_status": result.Status,
		"scope":         result.Scope,
		"warning_codes": warnings,
	}}
}

func strconvQuote(value string) string {
	return strconv.Quote(value)
}
