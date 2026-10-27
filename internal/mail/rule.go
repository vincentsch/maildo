package mail

import "errors"

// RuleScope names the set of rules a command works on. Only Inbox rules
// exist so far.
type RuleScope string

const (
	RuleScopeInbox RuleScope = "inbox"
)

// RuleMutationOperation names a rule change, such as rule_create. The names
// appear in output and do not change.
type RuleMutationOperation string

const (
	RuleOperationCreate  RuleMutationOperation = "rule_create"
	RuleOperationUpdate  RuleMutationOperation = "rule_update"
	RuleOperationEnable  RuleMutationOperation = "rule_enable"
	RuleOperationDisable RuleMutationOperation = "rule_disable"
	RuleOperationDelete  RuleMutationOperation = "rule_delete"
)

// RuleSequenceSource says where a rule's position in the rule order came
// from: maildo's default, the user, or the rule as it was.
type RuleSequenceSource string

const (
	RuleSequenceDefaulted RuleSequenceSource = "defaulted"
	RuleSequenceExplicit  RuleSequenceSource = "explicit"
	RuleSequencePreserved RuleSequenceSource = "preserved"
)

// RuleMutationResultStatus says whether a rule command only previewed,
// changed the rule, or found nothing to change.
type RuleMutationResultStatus string

const (
	RuleMutationStatusPreview   RuleMutationResultStatus = "preview"
	RuleMutationStatusApplied   RuleMutationResultStatus = "applied"
	RuleMutationStatusUnchanged RuleMutationResultStatus = "unchanged"
)

// RuleVerificationCause says why the check after a rule change failed. It
// holds no provider response data.
type RuleVerificationCause string

const (
	RuleVerificationCauseMismatch              RuleVerificationCause = "mismatch"
	RuleVerificationCauseNotFound              RuleVerificationCause = "not_found"
	RuleVerificationCauseAuthRepairRequired    RuleVerificationCause = "auth_repair_required"
	RuleVerificationCauseRateLimited           RuleVerificationCause = "rate_limited"
	RuleVerificationCauseProviderAPI           RuleVerificationCause = "provider_api"
	RuleVerificationCauseMalformedResponse     RuleVerificationCause = "malformed_response"
	RuleVerificationCauseOversizedResponse     RuleVerificationCause = "oversized_response"
	RuleVerificationCauseTransport             RuleVerificationCause = "transport"
	RuleVerificationCauseProviderReportedError RuleVerificationCause = "provider_reported_error"
)

const (
	RuleVerificationVerified = "verified"
	RuleVerificationFailed   = "failed"
)

// RuleListRequest asks a provider for the rules in one scope.
type RuleListRequest struct {
	Scope RuleScope `json:"scope,omitempty"`
}

// RuleListResult is all rules of one account.
type RuleListResult struct {
	Rules []Rule `json:"rules"`
}

// RuleGetRequest asks a provider for one rule.
type RuleGetRequest struct {
	Scope  RuleScope `json:"scope,omitempty"`
	RuleID string    `json:"rule_id"`
}

// Rule is a server-side rule, the same for every provider. The fields that
// appear in output hold only checked data. What the provider needs to keep
// the rest of the rule intact on a later change is kept apart and never
// printed.
type Rule struct {
	Account           string                 `json:"account"`
	Provider          string                 `json:"provider"`
	Scope             RuleScope              `json:"scope"`
	ID                string                 `json:"id"`
	DisplayName       string                 `json:"display_name"`
	Sequence          int                    `json:"sequence"`
	IsEnabled         bool                   `json:"is_enabled"`
	IsReadOnly        bool                   `json:"is_read_only"`
	HasError          bool                   `json:"has_error"`
	Conditions        RulePredicates         `json:"conditions"`
	Exceptions        RulePredicates         `json:"exceptions"`
	Actions           RuleActions            `json:"actions"`
	HealthStatus      RuleHealthStatus       `json:"health_status,omitempty"`
	HealthFindings    []RuleHealthFinding    `json:"health_findings,omitempty"`
	UnsupportedFields []RuleUnsupportedField `json:"unsupported_fields,omitempty"`
	ProviderMeta      map[string]any         `json:"provider_metadata,omitempty"`
	// ProviderFieldPresence records whether the three yes/no fields maildo
	// can edit were present in Graph's answer at all. It is never printed
	// or sent to the provider.
	ProviderFieldPresence RuleProviderFieldPresence `json:"-"`
	// ProviderState is what the provider needs to keep the rest of the rule
	// intact on a later change. It never appears in output.
	ProviderState map[string]any `json:"-"`
}

// RuleProviderFieldPresence tells a missing field apart from a field that is
// set to false or null.
type RuleProviderFieldPresence struct {
	ConditionsHasAttachments                 bool
	ActionsMarkAsRead                        bool
	ActionsStopProcessingRules               bool
	ActionsStopProcessingRulesValueAvailable bool
}

// RulePredicateSemantics shows in the output how conditions combine:
// different kinds of condition must all match (and), and several values of
// one kind match when any of them does (or).
type RulePredicateSemantics struct {
	Families       string `json:"families"`
	RepeatedValues string `json:"repeated_values"`
}

// DefaultRulePredicateSemantics is how Microsoft Graph combines conditions.
func DefaultRulePredicateSemantics() RulePredicateSemantics {
	return RulePredicateSemantics{Families: "and", RepeatedValues: "or"}
}

// RulePredicates holds a rule's conditions, also those maildo cannot edit
// yet.
type RulePredicates struct {
	Semantics              RulePredicateSemantics `json:"semantics"`
	FromAddresses          []Recipient            `json:"from_addresses,omitempty"`
	SenderContains         []string               `json:"sender_contains,omitempty"`
	RecipientContains      []string               `json:"recipient_contains,omitempty"`
	SubjectContains        []string               `json:"subject_contains,omitempty"`
	HeaderContains         []string               `json:"header_contains,omitempty"`
	HasAttachments         *bool                  `json:"has_attachments,omitempty"`
	BodyContains           []string               `json:"body_contains,omitempty"`
	BodyOrSubjectContains  []string               `json:"body_or_subject_contains,omitempty"`
	Categories             []string               `json:"categories,omitempty"`
	Importance             string                 `json:"importance,omitempty"`
	Sensitivity            string                 `json:"sensitivity,omitempty"`
	MessageActionFlag      string                 `json:"message_action_flag,omitempty"`
	WithinSizeRange        *RuleSizeRange         `json:"within_size_range,omitempty"`
	SentToAddresses        []Recipient            `json:"sent_to_addresses,omitempty"`
	IsApprovalRequest      *bool                  `json:"is_approval_request,omitempty"`
	IsAutomaticForward     *bool                  `json:"is_automatic_forward,omitempty"`
	IsAutomaticReply       *bool                  `json:"is_automatic_reply,omitempty"`
	IsEncrypted            *bool                  `json:"is_encrypted,omitempty"`
	IsMeetingRequest       *bool                  `json:"is_meeting_request,omitempty"`
	IsMeetingResponse      *bool                  `json:"is_meeting_response,omitempty"`
	IsNonDeliveryReport    *bool                  `json:"is_non_delivery_report,omitempty"`
	IsPermissionControlled *bool                  `json:"is_permission_controlled,omitempty"`
	IsReadReceipt          *bool                  `json:"is_read_receipt,omitempty"`
	IsSigned               *bool                  `json:"is_signed,omitempty"`
	IsVoicemail            *bool                  `json:"is_voicemail,omitempty"`
	NotSentToMe            *bool                  `json:"not_sent_to_me,omitempty"`
	SentCCMe               *bool                  `json:"sent_cc_me,omitempty"`
	SentOnlyToMe           *bool                  `json:"sent_only_to_me,omitempty"`
	SentToMe               *bool                  `json:"sent_to_me,omitempty"`
	SentToOrCCMe           *bool                  `json:"sent_to_or_cc_me,omitempty"`
}

// RuleSizeRange is a condition on the message size.
type RuleSizeRange struct {
	Minimum *int `json:"minimum,omitempty"`
	Maximum *int `json:"maximum,omitempty"`
}

// RuleActions holds a rule's actions, also those maildo cannot edit yet.
type RuleActions struct {
	MoveToFolder        *RuleFolderTarget `json:"move_to_folder,omitempty"`
	CopyToFolder        *RuleFolderTarget `json:"copy_to_folder,omitempty"`
	MarkAsRead          *bool             `json:"mark_as_read,omitempty"`
	StopProcessingRules bool              `json:"stop_processing_rules,omitempty"`
	AssignCategories    []string          `json:"assign_categories,omitempty"`
	Delete              bool              `json:"delete,omitempty"`
	PermanentDelete     bool              `json:"permanent_delete,omitempty"`
	MarkImportance      string            `json:"mark_importance,omitempty"`
	ForwardTo           []Recipient       `json:"forward_to,omitempty"`
	ForwardAsAttachment []Recipient       `json:"forward_as_attachment_to,omitempty"`
	RedirectTo          []Recipient       `json:"redirect_to,omitempty"`
}

// RuleFolderTarget is the folder a rule moves or copies mail to, with the
// name maildo found for its id.
type RuleFolderTarget struct {
	ProviderFolderID string `json:"provider_folder_id"`
	DisplayName      string `json:"display_name,omitempty"`
	Path             string `json:"path,omitempty"`
	WellKnownName    string `json:"well_known_name,omitempty"`
	Missing          bool   `json:"missing,omitempty"`
}

// RuleUnsupportedField names a rule field that maildo cannot edit or does
// not know, without copying the provider's JSON.
type RuleUnsupportedField struct {
	Area      string `json:"area"`
	Field     string `json:"field"`
	ValueKind string `json:"value_kind"`
	Count     int    `json:"count,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// RuleHealthStatus is the most serious finding of rules health.
type RuleHealthStatus string

const (
	RuleHealthHealthy RuleHealthStatus = "healthy"
	RuleHealthInfo    RuleHealthStatus = "info"
	RuleHealthWarning RuleHealthStatus = "warning"
	RuleHealthError   RuleHealthStatus = "error"
)

// RuleHealthFinding is one problem rules health found in an account.
type RuleHealthFinding struct {
	Code     string           `json:"code"`
	Severity RuleHealthStatus `json:"severity"`
	Field    string           `json:"field,omitempty"`
	Message  string           `json:"message"`
	Details  map[string]any   `json:"details,omitempty"`
}

// RuleStringCollectionChange is a text condition before and after a rule
// update. The new values replace the old ones.
type RuleStringCollectionChange struct {
	Set     bool     `json:"-"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after"`
	Removed []string `json:"removed,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
	Count   int      `json:"count"`
}

// RuleRecipientCollectionChange is an address condition before and after a
// rule update. The new addresses replace the old ones.
type RuleRecipientCollectionChange struct {
	Set     bool        `json:"-"`
	Before  []Recipient `json:"before,omitempty"`
	After   []Recipient `json:"after"`
	Removed []Recipient `json:"removed,omitempty"`
	Skipped []Recipient `json:"skipped,omitempty"`
	Count   int         `json:"count"`
}

// RuleBoolFieldChange sets or clears a yes/no rule field, which may also be
// unset.
type RuleBoolFieldChange struct {
	Set           bool  `json:"-"`
	Cleared       bool  `json:"cleared,omitempty"`
	Before        *bool `json:"before,omitempty"`
	After         *bool `json:"after,omitempty"`
	BeforePresent bool  `json:"-"`
	AfterPresent  bool  `json:"-"`
}

// RuleStringFieldChange replaces one text field.
type RuleStringFieldChange struct {
	Set    bool   `json:"-"`
	Before string `json:"before,omitempty"`
	After  string `json:"after"`
}

// RuleIntFieldChange replaces one number field.
type RuleIntFieldChange struct {
	Set    bool `json:"-"`
	Before int  `json:"before,omitempty"`
	After  int  `json:"after"`
}

// RuleFolderTargetChange replaces the folder a rule moves mail to.
type RuleFolderTargetChange struct {
	Set    bool              `json:"-"`
	Before *RuleFolderTarget `json:"before,omitempty"`
	After  *RuleFolderTarget `json:"after,omitempty"`
}

// RuleChangeSet is what a rule update changes. A nil field means the user
// did not ask to change it.
type RuleChangeSet struct {
	Operation           RuleMutationOperation          `json:"operation"`
	GeneratedName       bool                           `json:"generated_name,omitempty"`
	SequenceSource      RuleSequenceSource             `json:"sequence_source,omitempty"`
	DisplayName         *RuleStringFieldChange         `json:"display_name,omitempty"`
	Sequence            *RuleIntFieldChange            `json:"sequence,omitempty"`
	IsEnabled           *RuleBoolFieldChange           `json:"is_enabled,omitempty"`
	FromAddresses       *RuleRecipientCollectionChange `json:"from_addresses,omitempty"`
	SenderContains      *RuleStringCollectionChange    `json:"sender_contains,omitempty"`
	RecipientContains   *RuleStringCollectionChange    `json:"recipient_contains,omitempty"`
	SubjectContains     *RuleStringCollectionChange    `json:"subject_contains,omitempty"`
	HeaderContains      *RuleStringCollectionChange    `json:"header_contains,omitempty"`
	HasAttachments      *RuleBoolFieldChange           `json:"has_attachments,omitempty"`
	MoveToFolder        *RuleFolderTargetChange        `json:"move_to_folder,omitempty"`
	MarkAsRead          *RuleBoolFieldChange           `json:"mark_as_read,omitempty"`
	StopProcessingRules *RuleBoolFieldChange           `json:"stop_processing_rules,omitempty"`
	SourceRuleCount     *int                           `json:"-"`
	RequestedEnabled    *bool                          `json:"-"`
}

// RuleCreateRequest asks a provider to create a checked rule.
type RuleCreateRequest struct {
	Scope     RuleScope     `json:"scope,omitempty"`
	Rule      Rule          `json:"rule"`
	ChangeSet RuleChangeSet `json:"change_set"`
}

// RuleUpdateRequest holds the rule as it is now and the changes. Providers
// use Existing.ProviderState to keep the fields maildo cannot edit.
type RuleUpdateRequest struct {
	Scope     RuleScope     `json:"scope,omitempty"`
	RuleID    string        `json:"rule_id"`
	Existing  Rule          `json:"existing_rule"`
	Expected  Rule          `json:"expected_rule"`
	ChangeSet RuleChangeSet `json:"change_set"`
}

// RuleDeleteRequest names the rule to delete.
type RuleDeleteRequest struct {
	Scope  RuleScope `json:"scope,omitempty"`
	RuleID string    `json:"rule_id"`
}

// RuleMutationResult is what a provider reports after a rule change, before
// the command reads the rule back to check it.
type RuleMutationResult struct {
	Operation        RuleMutationOperation        `json:"operation"`
	RuleID           string                       `json:"rule_id"`
	ProviderMetadata RuleMutationProviderMetadata `json:"provider_metadata"`
}

// RuleDeleteResult is what a provider reports after deleting a rule.
type RuleDeleteResult struct {
	Operation        RuleMutationOperation        `json:"operation"`
	RuleID           string                       `json:"rule_id"`
	ProviderMetadata RuleMutationProviderMetadata `json:"provider_metadata"`
}

// RuleMutationProviderSource names the provider that made a rule change.
type RuleMutationProviderSource string

const (
	RuleMutationSourceMicrosoftGraph RuleMutationProviderSource = "microsoft_graph"
	RuleMutationSourceFake           RuleMutationProviderSource = "fake"
)

// RuleMutationProviderMetadata names the provider that made a rule change.
// maildo fills it in itself, never from the provider's answer.
type RuleMutationProviderMetadata struct {
	Source RuleMutationProviderSource `json:"source"`
}

// NewRuleMutationProviderMetadata refuses sources that are not listed above.
func NewRuleMutationProviderMetadata(source RuleMutationProviderSource) (RuleMutationProviderMetadata, error) {
	switch source {
	case RuleMutationSourceMicrosoftGraph, RuleMutationSourceFake:
		return RuleMutationProviderMetadata{Source: source}, nil
	default:
		return RuleMutationProviderMetadata{}, errors.New("invalid rule mutation provider source")
	}
}

// RuleMutationProviderWarning is a warning about what the provider showed
// after a rule change. Only the values below exist.
type RuleMutationProviderWarning string

const RuleMutationWarningProviderMarkedReadOnly RuleMutationProviderWarning = "provider_marked_read_only"

// RuleMutationProviderStatus is the state of the rule as the provider showed
// it after the change.
type RuleMutationProviderStatus struct {
	HasError   bool                          `json:"has_error"`
	IsReadOnly bool                          `json:"is_read_only"`
	Warnings   []RuleMutationProviderWarning `json:"warnings"`
}

// NewRuleMutationProviderStatus checks the warnings and drops repeats before
// the status goes into output.
func NewRuleMutationProviderStatus(hasError, isReadOnly bool, warnings ...RuleMutationProviderWarning) (RuleMutationProviderStatus, error) {
	out := RuleMutationProviderStatus{HasError: hasError, IsReadOnly: isReadOnly, Warnings: make([]RuleMutationProviderWarning, 0, len(warnings))}
	seen := map[RuleMutationProviderWarning]bool{}
	for _, warning := range warnings {
		if warning != RuleMutationWarningProviderMarkedReadOnly || seen[warning] {
			return RuleMutationProviderStatus{}, errors.New("invalid rule mutation provider warning")
		}
		seen[warning] = true
		out.Warnings = append(out.Warnings, warning)
	}
	return out, nil
}

// RuleVerificationMismatch is one field that did not match when maildo read
// the rule back after a change.
type RuleVerificationMismatch struct {
	Field    string `json:"field"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

// RuleVerification is the result of reading a rule back after a change.
type RuleVerification struct {
	Status        string                     `json:"status"`
	CheckedFields []string                   `json:"checked_fields,omitempty"`
	Mismatches    []RuleVerificationMismatch `json:"mismatches,omitempty"`
}
