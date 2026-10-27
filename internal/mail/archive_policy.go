package mail

// ArchivePolicyReportRequest is empty: the account says everything the
// provider needs for the report.
type ArchivePolicyReportRequest struct{}

// ArchivePolicyReportStatus says whether the mail API gave some of the facts
// or none of the mailbox settings.
type ArchivePolicyReportStatus string

const (
	ArchivePolicyReportPartial     ArchivePolicyReportStatus = "partial"
	ArchivePolicyReportUnavailable ArchivePolicyReportStatus = "unavailable"
)

// ArchivePolicyAvailabilityStatus marks admin settings that maildo cannot see
// through the mail API.
type ArchivePolicyAvailabilityStatus string

const ArchivePolicyAvailabilityUnavailable ArchivePolicyAvailabilityStatus = "unavailable"

// ArchivePolicyReason says why maildo cannot see an admin setting.
type ArchivePolicyReason string

const (
	ArchivePolicyReasonNotExposedByGraphMailAPI     ArchivePolicyReason = "not_exposed_by_graph_mail_api"
	ArchivePolicyReasonRequiresExchangeAdminContext ArchivePolicyReason = "requires_exchange_online_admin_context"
)

// ArchivePolicyWarning names a gap in a report that otherwise worked.
type ArchivePolicyWarning string

const (
	ArchivePolicyWarningMailboxSettingsUnavailable ArchivePolicyWarning = "mailbox_settings_unavailable"
	ArchivePolicyWarningFolderIDMissing            ArchivePolicyWarning = "archive_folder_id_missing"
	ArchivePolicyWarningFolderUnresolved           ArchivePolicyWarning = "archive_folder_unresolved"
)

// ArchivePolicyProviderSource names the API the report's facts come from.
type ArchivePolicyProviderSource string

const ArchivePolicySourceMicrosoftGraph ArchivePolicyProviderSource = "microsoft_graph"

// ArchivePolicyProviderMetadata names where the report's facts come from,
// without any request or response data.
type ArchivePolicyProviderMetadata struct {
	Source ArchivePolicyProviderSource `json:"source"`
}

// ArchivePolicyAvailability marks a setting as unavailable and says why.
type ArchivePolicyAvailability struct {
	Status ArchivePolicyAvailabilityStatus `json:"status"`
	Reason ArchivePolicyReason             `json:"reason"`
}

// ArchivePolicyFolder is the folder that Outlook's One-Click Archive uses.
// The Known flags tell a count of zero apart from a missing count.
type ArchivePolicyFolder struct {
	ID                    string `json:"id"`
	DisplayName           string `json:"display_name"`
	TotalItemCount        int    `json:"total_item_count"`
	TotalItemCountKnown   bool   `json:"total_item_count_known"`
	UnreadItemCount       int    `json:"unread_item_count"`
	UnreadItemCountKnown  bool   `json:"unread_item_count_known"`
	ChildFolderCount      int    `json:"child_folder_count"`
	ChildFolderCountKnown bool   `json:"child_folder_count_known"`
}

// ArchivePolicyReport holds only what the mail API shows to the signed-in
// user. Online Archive and retention policies always appear, marked as
// unavailable, so a missing fact is never read as a feature that is off.
// Warnings is never nil, so the JSON always has the same shape.
type ArchivePolicyReport struct {
	Account          string                        `json:"account"`
	Provider         string                        `json:"provider"`
	Status           ArchivePolicyReportStatus     `json:"status"`
	MailboxPurpose   string                        `json:"mailbox_purpose,omitempty"`
	ArchiveFolderID  string                        `json:"archive_folder_id,omitempty"`
	ArchiveFolder    *ArchivePolicyFolder          `json:"archive_folder,omitempty"`
	OnlineArchive    ArchivePolicyAvailability     `json:"online_archive"`
	RetentionPolicy  ArchivePolicyAvailability     `json:"retention_policy"`
	Warnings         []ArchivePolicyWarning        `json:"warnings"`
	ProviderMetadata ArchivePolicyProviderMetadata `json:"provider_metadata"`
}
