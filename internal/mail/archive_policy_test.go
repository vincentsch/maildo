package mail

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestArchivePolicyReportJSONContractPreservesOrderOptionalityAndKnownCounts(t *testing.T) {
	report := ArchivePolicyReport{
		Account: "work", Provider: "outlook", Status: ArchivePolicyReportPartial,
		MailboxPurpose: "future-purpose", ArchiveFolderID: "archive-id",
		ArchiveFolder: &ArchivePolicyFolder{
			ID: "archive-id", DisplayName: "Archive",
			TotalItemCount: 0, TotalItemCountKnown: true,
			UnreadItemCount: 0, UnreadItemCountKnown: false,
			ChildFolderCount: 2, ChildFolderCountKnown: true,
		},
		OnlineArchive:    ArchivePolicyAvailability{Status: ArchivePolicyAvailabilityUnavailable, Reason: ArchivePolicyReasonNotExposedByGraphMailAPI},
		RetentionPolicy:  ArchivePolicyAvailability{Status: ArchivePolicyAvailabilityUnavailable, Reason: ArchivePolicyReasonRequiresExchangeAdminContext},
		Warnings:         []ArchivePolicyWarning{},
		ProviderMetadata: ArchivePolicyProviderMetadata{Source: ArchivePolicySourceMicrosoftGraph},
	}
	got, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"account":"work","provider":"outlook","status":"partial","mailbox_purpose":"future-purpose","archive_folder_id":"archive-id","archive_folder":{"id":"archive-id","display_name":"Archive","total_item_count":0,"total_item_count_known":true,"unread_item_count":0,"unread_item_count_known":false,"child_folder_count":2,"child_folder_count_known":true},"online_archive":{"status":"unavailable","reason":"not_exposed_by_graph_mail_api"},"retention_policy":{"status":"unavailable","reason":"requires_exchange_online_admin_context"},"warnings":[],"provider_metadata":{"source":"microsoft_graph"}}`
	if string(got) != want {
		t.Fatalf("archive report JSON\n got: %s\nwant: %s", got, want)
	}

	report.Status = ArchivePolicyReportUnavailable
	report.MailboxPurpose = ""
	report.ArchiveFolderID = ""
	report.ArchiveFolder = nil
	report.Warnings = []ArchivePolicyWarning{ArchivePolicyWarningMailboxSettingsUnavailable}
	got, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"account":"work","provider":"outlook","status":"unavailable","online_archive":{"status":"unavailable","reason":"not_exposed_by_graph_mail_api"},"retention_policy":{"status":"unavailable","reason":"requires_exchange_online_admin_context"},"warnings":["mailbox_settings_unavailable"],"provider_metadata":{"source":"microsoft_graph"}}`
	if string(got) != want {
		t.Fatalf("unavailable report JSON\n got: %s\nwant: %s", got, want)
	}
}

func TestArchivePolicyNamedValueVocabularies(t *testing.T) {
	if got, want := []ArchivePolicyReportStatus{ArchivePolicyReportPartial, ArchivePolicyReportUnavailable}, []ArchivePolicyReportStatus{"partial", "unavailable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("report statuses = %#v", got)
	}
	if got, want := []ArchivePolicyReason{ArchivePolicyReasonNotExposedByGraphMailAPI, ArchivePolicyReasonRequiresExchangeAdminContext}, []ArchivePolicyReason{"not_exposed_by_graph_mail_api", "requires_exchange_online_admin_context"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons = %#v", got)
	}
	if got, want := []ArchivePolicyWarning{ArchivePolicyWarningMailboxSettingsUnavailable, ArchivePolicyWarningFolderIDMissing, ArchivePolicyWarningFolderUnresolved}, []ArchivePolicyWarning{"mailbox_settings_unavailable", "archive_folder_id_missing", "archive_folder_unresolved"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings = %#v", got)
	}
	if ArchivePolicyAvailabilityUnavailable != "unavailable" || ArchivePolicySourceMicrosoftGraph != "microsoft_graph" {
		t.Fatalf("availability/source vocabulary differs")
	}
}
