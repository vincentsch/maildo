package mail

import (
	"sort"

	"github.com/vincentsch/maildo/internal/errs"
)

type ProviderMetadataSource string
type ProtectedFolderAlias string
type ProviderLabelType string
type ProviderSpecialUse string
type ProviderCountsSource string
type ProviderDeleteSemantics string
type SummaryFolderBasis string
type ThreadBasis string
type MutationBasis string

const (
	MetadataSourceMicrosoftGraph ProviderMetadataSource = "microsoft_graph"
	MetadataSourceGmailAPI       ProviderMetadataSource = "gmail_api"
	MetadataSourceIMAP           ProviderMetadataSource = "imap"
	MetadataSourceFakeForTest    ProviderMetadataSource = "fake"
)

const (
	ProtectedAliasMsgFolderRoot             ProtectedFolderAlias = "msgfolderroot"
	ProtectedAliasInbox                     ProtectedFolderAlias = "inbox"
	ProtectedAliasDrafts                    ProtectedFolderAlias = "drafts"
	ProtectedAliasSentItems                 ProtectedFolderAlias = "sentitems"
	ProtectedAliasDeletedItems              ProtectedFolderAlias = "deleteditems"
	ProtectedAliasArchive                   ProtectedFolderAlias = "archive"
	ProtectedAliasJunkEmail                 ProtectedFolderAlias = "junkemail"
	ProtectedAliasOutbox                    ProtectedFolderAlias = "outbox"
	ProtectedAliasConversationHistory       ProtectedFolderAlias = "conversationhistory"
	ProtectedAliasSearchFolders             ProtectedFolderAlias = "searchfolders"
	ProtectedAliasRecoverableItemsDeletions ProtectedFolderAlias = "recoverableitemsdeletions"
	ProtectedAliasClutter                   ProtectedFolderAlias = "clutter"
	ProtectedAliasScheduled                 ProtectedFolderAlias = "scheduled"
	ProtectedAliasSyncIssues                ProtectedFolderAlias = "syncissues"
	ProtectedAliasConflicts                 ProtectedFolderAlias = "conflicts"
	ProtectedAliasLocalFailures             ProtectedFolderAlias = "localfailures"
	ProtectedAliasServerFailures            ProtectedFolderAlias = "serverfailures"
)

const (
	LabelTypeSystem ProviderLabelType = "system"
	LabelTypeUser   ProviderLabelType = "user"

	SpecialUseAll     ProviderSpecialUse = "all"
	SpecialUseArchive ProviderSpecialUse = "archive"
	SpecialUseDrafts  ProviderSpecialUse = "drafts"
	SpecialUseFlagged ProviderSpecialUse = "flagged"
	SpecialUseJunk    ProviderSpecialUse = "junk"
	SpecialUseSent    ProviderSpecialUse = "sent"
	SpecialUseTrash   ProviderSpecialUse = "trash"

	CountsSourceStatus  ProviderCountsSource = "status"
	CountsSourceUnknown ProviderCountsSource = "unknown"

	DeleteSemanticsLabelOnly ProviderDeleteSemantics = "label_only"

	SummaryBasisSystem    SummaryFolderBasis = "system"
	SummaryBasisUserLabel SummaryFolderBasis = "user_label"
	SummaryBasisFallback  SummaryFolderBasis = "fallback"

	ThreadBasisGmailNative    ThreadBasis = "gmail_native"
	ThreadBasisIMAPReferences ThreadBasis = "imap_references"
	ThreadBasisIMAPSingleton  ThreadBasis = "imap_singleton"

	MutationBasisGmailLabels MutationBasis = "gmail_labels"
)

type ProviderMetadataUse string

const (
	MetadataFolder                  ProviderMetadataUse = "folder"
	MetadataFolderMutationPreflight ProviderMetadataUse = "folder_mutation_preflight"
	MetadataFolderMutationResult    ProviderMetadataUse = "folder_mutation_result"
	MetadataMessageSummary          ProviderMetadataUse = "message_summary"
	MetadataMessage                 ProviderMetadataUse = "message"
	MetadataThread                  ProviderMetadataUse = "thread"
	MetadataAttachment              ProviderMetadataUse = "attachment"
	MetadataAttachmentList          ProviderMetadataUse = "attachment_list"
	MetadataMessageMutation         ProviderMetadataUse = "message_mutation"
	MetadataDraft                   ProviderMetadataUse = "draft"
	MetadataDraftWrite              ProviderMetadataUse = "draft_write"
	MetadataDraftSend               ProviderMetadataUse = "draft_send"
	MetadataSMTPDirectSend          ProviderMetadataUse = "smtp_direct_send"
)

type providerMetadataSeal struct {
	provider errs.ProviderName
	use      ProviderMetadataUse
	fake     bool
}

type FolderProviderMetadata struct {
	providerMetadataSeal
	Source          ProviderMetadataSource  `json:"source,omitempty"`
	ProtectedAlias  ProtectedFolderAlias    `json:"protected_alias,omitempty"`
	LabelType       ProviderLabelType       `json:"label_type,omitempty"`
	Selectable      *bool                   `json:"selectable,omitempty"`
	ColorText       string                  `json:"color_text,omitempty"`
	ColorBackground string                  `json:"color_background,omitempty"`
	SpecialUse      []ProviderSpecialUse    `json:"special_use,omitempty"`
	CountsSource    ProviderCountsSource    `json:"counts_source,omitempty"`
	OrphanParent    *bool                   `json:"orphan_parent,omitempty"`
	DeleteSemantics ProviderDeleteSemantics `json:"delete_semantics,omitempty"`
}

type SummaryProviderMetadata struct {
	providerMetadataSeal
	Source             ProviderMetadataSource `json:"source,omitempty"`
	UserLabels         []string               `json:"user_labels,omitempty"`
	Starred            *bool                  `json:"starred,omitempty"`
	SummaryFolderBasis SummaryFolderBasis     `json:"summary_folder_basis,omitempty"`
	Flagged            *bool                  `json:"flagged,omitempty"`
}

type MessageProviderMetadata struct {
	providerMetadataSeal
	Source  ProviderMetadataSource `json:"source,omitempty"`
	Starred *bool                  `json:"starred,omitempty"`
	Flagged *bool                  `json:"flagged,omitempty"`
}

type ThreadProviderMetadata struct {
	providerMetadataSeal
	Basis          ThreadBasis `json:"basis,omitempty"`
	Complete       *bool       `json:"complete,omitempty"`
	CandidateLimit *uint64     `json:"candidate_limit,omitempty"`
	CandidateCount *uint64     `json:"candidate_count,omitempty"`
}

type AttachmentProviderMetadata struct {
	providerMetadataSeal
	Source                      ProviderMetadataSource `json:"source,omitempty"`
	SnapshotMessageID           string                 `json:"snapshot_message_id,omitempty"`
	ExternalAttachmentIDPresent *bool                  `json:"external_attachment_id_present,omitempty"`
}

type MutationProviderMetadata struct {
	providerMetadataSeal
	Source             ProviderMetadataSource `json:"source,omitempty"`
	MutationBasis      MutationBasis          `json:"mutation_basis,omitempty"`
	FinalLabelIDs      *[]string              `json:"final_label_ids,omitempty"`
	ConcurrencyGuarded *bool                  `json:"concurrency_guarded,omitempty"`
}

type DraftProviderMetadata struct {
	providerMetadataSeal
	Source             ProviderMetadataSource `json:"source,omitempty"`
	SnapshotMessageID  string                 `json:"snapshot_message_id,omitempty"`
	ConcurrencyGuarded *bool                  `json:"concurrency_guarded,omitempty"`
}

type SMTPDirectSendProviderMetadata struct {
	providerMetadataSeal
	AcceptanceKnown        bool `json:"acceptance_known"`
	ServerMessageIDPresent bool `json:"server_message_id_present"`
	SMTPUTF8Used           bool `json:"smtp_utf8_used"`
}

type ProviderMetadataValue interface {
	FolderProviderMetadata | SummaryProviderMetadata | MessageProviderMetadata |
		ThreadProviderMetadata | AttachmentProviderMetadata | MutationProviderMetadata |
		DraftProviderMetadata | SMTPDirectSendProviderMetadata
}

func NewProviderMetadata[T ProviderMetadataValue](provider errs.ProviderName, use ProviderMetadataUse, value T) (*T, error) {
	return newProviderMetadata(provider, use, false, value)
}

func NewFakeProviderMetadataForTest[T ProviderMetadataValue](use ProviderMetadataUse, value T) (*T, error) {
	return newProviderMetadata(errs.ProviderName("fake"), use, true, value)
}

func CloneProviderMetadata[T ProviderMetadataValue](value *T) (*T, error) {
	if value == nil {
		return nil, nil
	}
	provider, use, fake, ok := metadataSeal(*value)
	if !ok {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	clone := cloneMetadataValue(*value)
	return newProviderMetadata(provider, use, fake, clone)
}

func newProviderMetadata[T ProviderMetadataValue](provider errs.ProviderName, use ProviderMetadataUse, fake bool, value T) (*T, error) {
	if !fake && !validMetadataProvider(provider) {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	if fake && provider != "fake" {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	if err := validateMetadata(provider, use, fake, value); err != nil {
		return nil, err
	}
	out := cloneMetadataValue(value)
	sealMetadata(&out, provider, use, fake)
	return &out, nil
}

func metadataSeal[T ProviderMetadataValue](value T) (errs.ProviderName, ProviderMetadataUse, bool, bool) {
	switch v := any(value).(type) {
	case FolderProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case SummaryProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case MessageProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case ThreadProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case AttachmentProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case MutationProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case DraftProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	case SMTPDirectSendProviderMetadata:
		return v.provider, v.use, v.fake, v.use != ""
	default:
		return "", "", false, false
	}
}

func sealMetadata[T ProviderMetadataValue](value *T, provider errs.ProviderName, use ProviderMetadataUse, fake bool) {
	seal := providerMetadataSeal{provider: provider, use: use, fake: fake}
	switch v := any(value).(type) {
	case *FolderProviderMetadata:
		v.providerMetadataSeal = seal
	case *SummaryProviderMetadata:
		v.providerMetadataSeal = seal
	case *MessageProviderMetadata:
		v.providerMetadataSeal = seal
	case *ThreadProviderMetadata:
		v.providerMetadataSeal = seal
	case *AttachmentProviderMetadata:
		v.providerMetadataSeal = seal
	case *MutationProviderMetadata:
		v.providerMetadataSeal = seal
	case *DraftProviderMetadata:
		v.providerMetadataSeal = seal
	case *SMTPDirectSendProviderMetadata:
		v.providerMetadataSeal = seal
	}
}

func validateMetadata[T ProviderMetadataValue](provider errs.ProviderName, use ProviderMetadataUse, fake bool, value T) error {
	switch v := any(value).(type) {
	case FolderProviderMetadata:
		if use != MetadataFolder && use != MetadataFolderMutationPreflight && use != MetadataFolderMutationResult {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateFolderMetadata(provider, use, fake, v)
	case SummaryProviderMetadata:
		if use != MetadataMessageSummary {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateSummaryMetadata(provider, fake, v)
	case MessageProviderMetadata:
		if use != MetadataMessage {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateMessageMetadata(provider, fake, v)
	case ThreadProviderMetadata:
		if use != MetadataThread {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateThreadMetadata(provider, fake, v)
	case AttachmentProviderMetadata:
		if use != MetadataAttachment && use != MetadataAttachmentList {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateAttachmentMetadata(provider, fake, v)
	case MutationProviderMetadata:
		if use != MetadataMessageMutation {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateMutationMetadata(provider, fake, v)
	case DraftProviderMetadata:
		if use != MetadataDraft && use != MetadataDraftWrite && use != MetadataDraftSend {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateDraftMetadata(provider, fake, v)
	case SMTPDirectSendProviderMetadata:
		if use != MetadataSMTPDirectSend {
			return errs.NewSharedProviderContractInvalid()
		}
		return validateSMTPDirectSendMetadata(provider, fake, v)
	default:
		return errs.NewSharedProviderContractInvalid()
	}
}

func validateFolderMetadata(provider errs.ProviderName, use ProviderMetadataUse, fake bool, v FolderProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if v.ProtectedAlias != "" && (!validProtectedAlias(v.ProtectedAlias) || use == MetadataFolderMutationResult) {
		return errs.NewSharedProviderContractInvalid()
	}
	if v.LabelType != "" && !validProviderLabelType(v.LabelType) {
		return errs.NewSharedProviderContractInvalid()
	}
	if v.CountsSource != "" && !validProviderCountsSource(v.CountsSource) {
		return errs.NewSharedProviderContractInvalid()
	}
	if v.DeleteSemantics != "" && v.DeleteSemantics != DeleteSemanticsLabelOnly {
		return errs.NewSharedProviderContractInvalid()
	}
	if len(v.SpecialUse) > 1 && !sort.SliceIsSorted(v.SpecialUse, func(i, j int) bool { return v.SpecialUse[i] < v.SpecialUse[j] }) {
		return errs.NewSharedProviderContractInvalid()
	}
	for i, special := range v.SpecialUse {
		if !validProviderSpecialUse(special) {
			return errs.NewSharedProviderContractInvalid()
		}
		if i > 0 && special == v.SpecialUse[i-1] {
			return errs.NewSharedProviderContractInvalid()
		}
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook:
		if v.LabelType != "" || v.Selectable != nil || v.ColorText != "" || v.ColorBackground != "" || len(v.SpecialUse) != 0 || v.CountsSource != "" || v.OrphanParent != nil || v.DeleteSemantics != "" {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		if v.ProtectedAlias != "" || v.CountsSource != "" || v.OrphanParent != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if v.ProtectedAlias != "" || v.LabelType != "" || v.ColorText != "" || v.ColorBackground != "" || v.DeleteSemantics != "" {
			return errs.NewSharedProviderContractInvalid()
		}
		if use == MetadataFolder && (v.Selectable == nil || v.CountsSource == "") {
			return errs.NewSharedProviderContractInvalid()
		}
		if v.OrphanParent != nil && !*v.OrphanParent {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateSummaryMetadata(provider errs.ProviderName, fake bool, v SummaryProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if v.SummaryFolderBasis != "" && !validSummaryFolderBasis(v.SummaryFolderBasis) {
		return errs.NewSharedProviderContractInvalid()
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook:
		if len(v.UserLabels) != 0 || v.Starred != nil || v.SummaryFolderBasis != "" || v.Flagged != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		if v.UserLabels == nil || v.SummaryFolderBasis == "" || v.Flagged != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if len(v.UserLabels) != 0 || v.Starred != nil || v.SummaryFolderBasis != "" || v.Flagged == nil {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateMessageMetadata(provider errs.ProviderName, fake bool, v MessageProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook:
		if v.Starred != nil || v.Flagged != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		if v.Flagged != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if v.Starred != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateThreadMetadata(provider errs.ProviderName, fake bool, v ThreadProviderMetadata) error {
	if !validThreadBasis(v.Basis) {
		return errs.NewSharedProviderContractInvalid()
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderGmail:
		if v.Basis != ThreadBasisGmailNative || v.CandidateLimit != nil || v.CandidateCount != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if v.Basis != ThreadBasisIMAPReferences && v.Basis != ThreadBasisIMAPSingleton {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderOutlook:
		return errs.NewSharedProviderContractInvalid()
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateAttachmentMetadata(provider errs.ProviderName, fake bool, v AttachmentProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook, errs.ProviderIMAP, errs.ProviderFastmail:
		if v.SnapshotMessageID != "" || v.ExternalAttachmentIDPresent != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		if v.SnapshotMessageID == "" {
			return errs.NewSharedProviderContractInvalid()
		}
		if v.ExternalAttachmentIDPresent != nil && !*v.ExternalAttachmentIDPresent {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateMutationMetadata(provider errs.ProviderName, fake bool, v MutationProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if v.MutationBasis != "" && v.MutationBasis != MutationBasisGmailLabels {
		return errs.NewSharedProviderContractInvalid()
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook:
		if v.MutationBasis != "" || v.FinalLabelIDs != nil || v.ConcurrencyGuarded != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		if v.MutationBasis == "" {
			return errs.NewSharedProviderContractInvalid()
		}
		if v.MutationBasis == MutationBasisGmailLabels && v.FinalLabelIDs == nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if v.MutationBasis != "" || v.FinalLabelIDs != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateDraftMetadata(provider errs.ProviderName, fake bool, v DraftProviderMetadata) error {
	if err := requireSource(provider, fake, v.Source); err != nil {
		return err
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderOutlook:
		if v.SnapshotMessageID != "" || v.ConcurrencyGuarded != nil {
			return errs.NewSharedProviderContractInvalid()
		}
	case errs.ProviderGmail:
		return nil
	case errs.ProviderIMAP, errs.ProviderFastmail:
		if v.SnapshotMessageID != "" {
			return errs.NewSharedProviderContractInvalid()
		}
	default:
		return errs.NewSharedProviderContractInvalid()
	}
	return nil
}

func validateSMTPDirectSendMetadata(provider errs.ProviderName, fake bool, v SMTPDirectSendProviderMetadata) error {
	if !v.AcceptanceKnown || v.ServerMessageIDPresent {
		return errs.NewSharedProviderContractInvalid()
	}
	if fake {
		return nil
	}
	switch provider {
	case errs.ProviderIMAP, errs.ProviderFastmail:
		return nil
	default:
		return errs.NewSharedProviderContractInvalid()
	}
}

func requireSource(provider errs.ProviderName, fake bool, source ProviderMetadataSource) error {
	if source == "" {
		return errs.NewSharedProviderContractInvalid()
	}
	return validateSource(provider, fake, source)
}

func validateSource(provider errs.ProviderName, fake bool, source ProviderMetadataSource) error {
	switch {
	case fake:
		if source == MetadataSourceFakeForTest {
			return nil
		}
	case provider == errs.ProviderOutlook:
		if source == MetadataSourceMicrosoftGraph {
			return nil
		}
	case provider == errs.ProviderGmail:
		if source == MetadataSourceGmailAPI {
			return nil
		}
	case provider == errs.ProviderIMAP || provider == errs.ProviderFastmail:
		if source == MetadataSourceIMAP {
			return nil
		}
	}
	return errs.NewSharedProviderContractInvalid()
}

func validProtectedAlias(alias ProtectedFolderAlias) bool {
	switch alias {
	case ProtectedAliasMsgFolderRoot, ProtectedAliasInbox, ProtectedAliasDrafts, ProtectedAliasSentItems, ProtectedAliasDeletedItems,
		ProtectedAliasArchive, ProtectedAliasJunkEmail, ProtectedAliasOutbox, ProtectedAliasConversationHistory, ProtectedAliasSearchFolders,
		ProtectedAliasRecoverableItemsDeletions, ProtectedAliasClutter, ProtectedAliasScheduled, ProtectedAliasSyncIssues,
		ProtectedAliasConflicts, ProtectedAliasLocalFailures, ProtectedAliasServerFailures:
		return true
	default:
		return false
	}
}

func validProviderLabelType(labelType ProviderLabelType) bool {
	switch labelType {
	case LabelTypeSystem, LabelTypeUser:
		return true
	default:
		return false
	}
}

func validProviderSpecialUse(special ProviderSpecialUse) bool {
	switch special {
	case SpecialUseAll, SpecialUseArchive, SpecialUseDrafts, SpecialUseFlagged, SpecialUseJunk, SpecialUseSent, SpecialUseTrash:
		return true
	default:
		return false
	}
}

func validProviderCountsSource(source ProviderCountsSource) bool {
	switch source {
	case CountsSourceStatus, CountsSourceUnknown:
		return true
	default:
		return false
	}
}

func validSummaryFolderBasis(basis SummaryFolderBasis) bool {
	switch basis {
	case SummaryBasisSystem, SummaryBasisUserLabel, SummaryBasisFallback:
		return true
	default:
		return false
	}
}

func validThreadBasis(basis ThreadBasis) bool {
	switch basis {
	case ThreadBasisGmailNative, ThreadBasisIMAPReferences, ThreadBasisIMAPSingleton:
		return true
	default:
		return false
	}
}

func validMetadataProvider(provider errs.ProviderName) bool {
	switch provider {
	case errs.ProviderOutlook, errs.ProviderGmail, errs.ProviderIMAP, errs.ProviderFastmail:
		return true
	default:
		return false
	}
}

func cloneMetadataValue[T ProviderMetadataValue](value T) T {
	switch v := any(value).(type) {
	case FolderProviderMetadata:
		v.Selectable = cloneBoolPtr(v.Selectable)
		v.SpecialUse = append([]ProviderSpecialUse(nil), v.SpecialUse...)
		v.OrphanParent = cloneBoolPtr(v.OrphanParent)
		return any(v).(T)
	case SummaryProviderMetadata:
		v.UserLabels = append([]string(nil), v.UserLabels...)
		v.Starred = cloneBoolPtr(v.Starred)
		v.Flagged = cloneBoolPtr(v.Flagged)
		return any(v).(T)
	case MessageProviderMetadata:
		v.Starred = cloneBoolPtr(v.Starred)
		v.Flagged = cloneBoolPtr(v.Flagged)
		return any(v).(T)
	case ThreadProviderMetadata:
		v.Complete = cloneBoolPtr(v.Complete)
		v.CandidateLimit = cloneUint64Ptr(v.CandidateLimit)
		v.CandidateCount = cloneUint64Ptr(v.CandidateCount)
		return any(v).(T)
	case AttachmentProviderMetadata:
		v.ExternalAttachmentIDPresent = cloneBoolPtr(v.ExternalAttachmentIDPresent)
		return any(v).(T)
	case MutationProviderMetadata:
		if v.FinalLabelIDs != nil {
			labels := append([]string(nil), (*v.FinalLabelIDs)...)
			v.FinalLabelIDs = &labels
		}
		v.ConcurrencyGuarded = cloneBoolPtr(v.ConcurrencyGuarded)
		return any(v).(T)
	case DraftProviderMetadata:
		v.ConcurrencyGuarded = cloneBoolPtr(v.ConcurrencyGuarded)
		return any(v).(T)
	case SMTPDirectSendProviderMetadata:
		return any(v).(T)
	default:
		return value
	}
}

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneUint64Ptr(in *uint64) *uint64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
