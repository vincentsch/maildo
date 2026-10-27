package rules

import (
	"errors"
	"math"
	"net/mail"
	"reflect"
	"sort"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	domain "github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

const (
	FindingDisabledRule       = "disabled_rule"
	FindingReadOnlyRule       = "read_only_rule"
	FindingProviderError      = "provider_error"
	FindingMissingFolder      = "missing_folder_target"
	FindingDuplicateSequence  = "duplicate_sequence"
	FindingUnsupportedField   = "unsupported_field"
	FindingForwardingAction   = "forwarding_action"
	FindingExternalForwarding = "external_forwarding"
)

// AnnotateList adds folder names and health findings to one account's rules.
func AnnotateList(in []domain.Rule, folders []domain.Folder, mailboxEmail string) []domain.Rule {
	out := make([]domain.Rule, len(in))
	sequenceCounts := map[int]int{}
	for _, rule := range in {
		sequenceCounts[rule.Sequence]++
	}
	for i, rule := range in {
		out[i] = AnnotateRule(rule, folders, mailboxEmail, sequenceCounts)
	}
	return out
}

// AnnotateRule returns a copy of rule with folder names, the way conditions
// combine, and health findings filled in. The rule passed in stays as it
// was, so callers can use it again.
func AnnotateRule(rule domain.Rule, folders []domain.Folder, mailboxEmail string, sequenceCounts map[int]int) domain.Rule {
	rule = cloneRule(rule)
	if sequenceCounts == nil {
		sequenceCounts = map[int]int{rule.Sequence: 1}
	}
	folderIndex := indexFolders(folders)
	resolveTargets(&rule, folderIndex)
	rule.UnsupportedFields = sortedUnsupported(rule.UnsupportedFields)
	rule.Conditions.Semantics = defaultSemantics(rule.Conditions.Semantics)
	rule.Exceptions.Semantics = defaultSemantics(rule.Exceptions.Semantics)
	rule.HealthFindings = findings(rule, mailboxEmail, sequenceCounts)
	rule.HealthStatus = status(rule.HealthFindings)
	return rule
}

// cloneRule deep-copies everything AnnotateRule may change or print.
func cloneRule(in domain.Rule) domain.Rule {
	rule := in
	rule.Conditions = clonePredicates(in.Conditions)
	rule.Exceptions = clonePredicates(in.Exceptions)
	rule.Actions = cloneActions(in.Actions)
	rule.HealthFindings = cloneHealthFindings(in.HealthFindings)
	rule.UnsupportedFields = append([]domain.RuleUnsupportedField(nil), in.UnsupportedFields...)
	rule.ProviderMeta = cloneAnyMap(in.ProviderMeta)
	rule.ProviderState = cloneAnyMap(in.ProviderState)
	return rule
}

func cloneHealthFindings(in []domain.RuleHealthFinding) []domain.RuleHealthFinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.RuleHealthFinding, len(in))
	for i, finding := range in {
		out[i] = finding
		out[i].Details = cloneAnyMap(finding.Details)
	}
	return out
}

func clonePredicates(in domain.RulePredicates) domain.RulePredicates {
	p := in
	p.FromAddresses = append([]domain.Recipient(nil), in.FromAddresses...)
	p.SenderContains = append([]string(nil), in.SenderContains...)
	p.RecipientContains = append([]string(nil), in.RecipientContains...)
	p.SubjectContains = append([]string(nil), in.SubjectContains...)
	p.HeaderContains = append([]string(nil), in.HeaderContains...)
	p.HasAttachments = cloneBool(in.HasAttachments)
	p.BodyContains = append([]string(nil), in.BodyContains...)
	p.BodyOrSubjectContains = append([]string(nil), in.BodyOrSubjectContains...)
	p.Categories = append([]string(nil), in.Categories...)
	p.WithinSizeRange = cloneSizeRange(in.WithinSizeRange)
	p.SentToAddresses = append([]domain.Recipient(nil), in.SentToAddresses...)
	p.IsApprovalRequest = cloneBool(in.IsApprovalRequest)
	p.IsAutomaticForward = cloneBool(in.IsAutomaticForward)
	p.IsAutomaticReply = cloneBool(in.IsAutomaticReply)
	p.IsEncrypted = cloneBool(in.IsEncrypted)
	p.IsMeetingRequest = cloneBool(in.IsMeetingRequest)
	p.IsMeetingResponse = cloneBool(in.IsMeetingResponse)
	p.IsNonDeliveryReport = cloneBool(in.IsNonDeliveryReport)
	p.IsPermissionControlled = cloneBool(in.IsPermissionControlled)
	p.IsReadReceipt = cloneBool(in.IsReadReceipt)
	p.IsSigned = cloneBool(in.IsSigned)
	p.IsVoicemail = cloneBool(in.IsVoicemail)
	p.NotSentToMe = cloneBool(in.NotSentToMe)
	p.SentCCMe = cloneBool(in.SentCCMe)
	p.SentOnlyToMe = cloneBool(in.SentOnlyToMe)
	p.SentToMe = cloneBool(in.SentToMe)
	p.SentToOrCCMe = cloneBool(in.SentToOrCCMe)
	return p
}

func cloneActions(in domain.RuleActions) domain.RuleActions {
	a := in
	a.MoveToFolder = cloneTarget(in.MoveToFolder)
	a.CopyToFolder = cloneTarget(in.CopyToFolder)
	a.MarkAsRead = cloneBool(in.MarkAsRead)
	a.AssignCategories = append([]string(nil), in.AssignCategories...)
	a.ForwardTo = append([]domain.Recipient(nil), in.ForwardTo...)
	a.ForwardAsAttachment = append([]domain.Recipient(nil), in.ForwardAsAttachment...)
	a.RedirectTo = append([]domain.Recipient(nil), in.RedirectTo...)
	return a
}

func cloneTarget(in *domain.RuleFolderTarget) *domain.RuleFolderTarget {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneSizeRange(in *domain.RuleSizeRange) *domain.RuleSizeRange {
	if in == nil {
		return nil
	}
	out := *in
	out.Minimum = cloneInt(in.Minimum)
	out.Maximum = cloneInt(in.Maximum)
	return &out
}

func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func indexFolders(folders []domain.Folder) map[string]domain.Folder {
	out := make(map[string]domain.Folder, len(folders))
	for _, folder := range folders {
		if folder.ID != "" {
			out[folder.ID] = folder
		}
	}
	return out
}

func resolveTargets(rule *domain.Rule, folders map[string]domain.Folder) {
	if rule.Actions.MoveToFolder != nil {
		resolveTarget(rule.Actions.MoveToFolder, folders)
	}
	if rule.Actions.CopyToFolder != nil {
		resolveTarget(rule.Actions.CopyToFolder, folders)
	}
}

func resolveTarget(target *domain.RuleFolderTarget, folders map[string]domain.Folder) {
	if target == nil || target.ProviderFolderID == "" {
		return
	}
	folder, ok := folders[target.ProviderFolderID]
	if !ok {
		target.Missing = true
		return
	}
	target.DisplayName = folder.DisplayName
	target.Path = folder.Path
	target.WellKnownName = folder.WellKnownName
	target.Missing = false
}

// findings checks a rule against what maildo already knows about the
// account, without calling the provider. The order is fixed, so text and
// JSON output list the findings the same way.
func findings(rule domain.Rule, mailboxEmail string, sequenceCounts map[int]int) []domain.RuleHealthFinding {
	var out []domain.RuleHealthFinding
	if rule.HasError {
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingProviderError,
			Severity: domain.RuleHealthError,
			Field:    "has_error",
			Message:  "Provider reports an error for this rule.",
		})
	}
	if !rule.IsEnabled {
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingDisabledRule,
			Severity: domain.RuleHealthInfo,
			Field:    "is_enabled",
			Message:  "Rule is disabled.",
		})
	}
	if rule.IsReadOnly {
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingReadOnlyRule,
			Severity: domain.RuleHealthInfo,
			Field:    "is_read_only",
			Message:  "Rule is read-only or provider-managed.",
		})
	}
	if sequenceCounts[rule.Sequence] > 1 {
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingDuplicateSequence,
			Severity: domain.RuleHealthWarning,
			Field:    "sequence",
			Message:  "Multiple rules use this sequence value.",
			Details:  map[string]any{"sequence": rule.Sequence, "count": sequenceCounts[rule.Sequence]},
		})
	}
	if target := rule.Actions.MoveToFolder; target != nil && target.Missing {
		out = append(out, missingFolderFinding("actions.move_to_folder", target.ProviderFolderID))
	}
	if target := rule.Actions.CopyToFolder; target != nil && target.Missing {
		out = append(out, missingFolderFinding("actions.copy_to_folder", target.ProviderFolderID))
	}
	for _, unsupported := range sortedUnsupported(rule.UnsupportedFields) {
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingUnsupportedField,
			Severity: domain.RuleHealthInfo,
			Field:    unsupported.Area + "." + unsupported.Field,
			Message:  "Rule uses a provider field that maildo preserves but does not edit.",
			Details: map[string]any{
				"area":       unsupported.Area,
				"field":      unsupported.Field,
				"value_kind": unsupported.ValueKind,
				"count":      unsupported.Count,
				"summary":    unsupported.Summary,
			},
		})
	}
	out = appendForwardingFindings(out, "actions.forward_to", "Rule forwards messages.", rule.Actions.ForwardTo, mailboxEmail)
	out = appendForwardingFindings(out, "actions.forward_as_attachment_to", "Rule forwards messages as attachments.", rule.Actions.ForwardAsAttachment, mailboxEmail)
	out = appendForwardingFindings(out, "actions.redirect_to", "Rule redirects messages.", rule.Actions.RedirectTo, mailboxEmail)
	return out
}

func missingFolderFinding(field, id string) domain.RuleHealthFinding {
	return domain.RuleHealthFinding{
		Code:     FindingMissingFolder,
		Severity: domain.RuleHealthError,
		Field:    field,
		Message:  "Rule targets a folder that is missing from the current folder snapshot.",
		Details:  map[string]any{"provider_folder_id": id},
	}
}

// appendForwardingFindings always reports that a rule forwards mail. It calls
// the forward external only when it can read both addresses for sure.
func appendForwardingFindings(out []domain.RuleHealthFinding, field, message string, recipients []domain.Recipient, mailboxEmail string) []domain.RuleHealthFinding {
	if len(recipients) == 0 {
		return out
	}
	out = append(out, domain.RuleHealthFinding{
		Code:     FindingForwardingAction,
		Severity: domain.RuleHealthWarning,
		Field:    field,
		Message:  message,
		Details:  map[string]any{"recipient_count": len(recipients)},
	})
	domainName := emailDomain(mailboxEmail)
	if domainName == "" {
		// Without the account's own domain, maildo cannot tell whether the
		// forward leaves the organization, so it reports only that the rule
		// forwards.
		return out
	}
	for _, recipient := range recipients {
		rd := emailDomain(recipient.Address)
		if rd == "" || strings.EqualFold(rd, domainName) {
			continue
		}
		out = append(out, domain.RuleHealthFinding{
			Code:     FindingExternalForwarding,
			Severity: domain.RuleHealthWarning,
			Field:    field,
			Message:  "Rule targets a recipient outside the account email domain.",
			Details: map[string]any{
				"recipient_domain": rd,
				"account_domain":   domainName,
			},
		})
	}
	return out
}

// emailDomain needs a valid address. Taking whatever follows the last "@"
// would turn a broken address into a wrong "forwards outside" finding.
func emailDomain(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil {
		return ""
	}
	address = parsed.Address
	at := strings.LastIndex(address, "@")
	if at <= 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(address[at+1:])
}

func status(findings []domain.RuleHealthFinding) domain.RuleHealthStatus {
	if len(findings) == 0 {
		return domain.RuleHealthHealthy
	}
	out := domain.RuleHealthInfo
	for _, finding := range findings {
		switch finding.Severity {
		case domain.RuleHealthError:
			return domain.RuleHealthError
		case domain.RuleHealthWarning:
			out = domain.RuleHealthWarning
		}
	}
	return out
}

func sortedUnsupported(in []domain.RuleUnsupportedField) []domain.RuleUnsupportedField {
	out := append([]domain.RuleUnsupportedField(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Area == out[j].Area {
			return out[i].Field < out[j].Field
		}
		return out[i].Area < out[j].Area
	})
	return out
}

// defaultSemantics fills in each missing part on its own, because a provider
// may report how one part combines and leave out the other.
func defaultSemantics(in domain.RulePredicateSemantics) domain.RulePredicateSemantics {
	defaults := domain.DefaultRulePredicateSemantics()
	if in.Families == "" && in.RepeatedValues == "" {
		return defaults
	}
	if in.Families == "" {
		in.Families = defaults.Families
	}
	if in.RepeatedValues == "" {
		in.RepeatedValues = defaults.RepeatedValues
	}
	return in
}

// BoolIntent tells a yes/no flag that was not given apart from one that sets
// or clears the field.
type BoolIntent struct {
	Set   bool
	Clear bool
	Value bool
}

// RuleMutationIntent is what the rule create and rule update flags asked
// for, before maildo checks it.
type RuleMutationIntent struct {
	Name                 string
	NameSet              bool
	FromAddresses        []domain.Recipient
	FromSet              bool
	SenderContains       []string
	SenderContainsSet    bool
	RecipientContains    []string
	RecipientContainsSet bool
	SubjectContains      []string
	SubjectContainsSet   bool
	HeaderContains       []string
	HeaderContainsSet    bool
	HasAttachments       BoolIntent
	MoveToFolder         *domain.Folder
	MoveToFolderRef      *domain.FolderRef
	MoveToFolderSet      bool
	MarkAsRead           BoolIntent
	StopProcessingRules  BoolIntent
	Sequence             int64
	SequenceSet          bool
}

// ParseFromAddresses reads the --from values like other address flags. It
// refuses the same address twice, in any case, and keeps display names.
func ParseFromAddresses(values []string) ([]domain.Recipient, error) {
	if len(values) == 0 {
		return nil, invalidRuleInput("from", "empty_value", "--from needs at least one sender address")
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, invalidRuleInput("from", "empty_value", "sender address values must not be empty")
		}
	}
	recipients, err := domain.ParseMailboxListValues(values)
	if err != nil {
		return nil, invalidRuleInput("from", "malformed_address", "--from has an address that is not valid")
	}
	seen := map[string]struct{}{}
	out := make([]domain.Recipient, 0, len(recipients))
	for _, r := range recipients {
		r.Address = strings.TrimSpace(r.Address)
		key := strings.ToLower(r.Address)
		if key == "" {
			return nil, invalidRuleInput("from", "empty_value", "sender address values must not be empty")
		}
		if _, ok := seen[key]; ok {
			return nil, invalidRuleInput("from", "duplicate_value", "--from has the same address twice")
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out, nil
}

// ValidateCreateIntent finds the mistakes in a rule create that the flags
// alone show, before maildo opens the account or calls the provider.
func ValidateCreateIntent(intent RuleMutationIntent) error {
	normalized, err := normalizeIntent(intent)
	if err != nil {
		return err
	}
	if normalized.SequenceSet && !validRuleSequence64(normalized.Sequence) {
		return invalidRuleInput("sequence", "out_of_range", "sequence must be between 1 and 2147483647")
	}
	if !intentHasCondition(normalized) {
		return invalidRuleInput("conditions", "required", "a rule needs at least one condition, such as --from or --subject-contains")
	}
	if !intentHasAction(normalized) {
		return invalidRuleInput("actions", "required", "a rule needs at least one action, such as --move-to or --mark-read")
	}
	return nil
}

// ValidateUpdateIntent finds the mistakes in a rule update that the flags
// alone show, before maildo opens the account or calls the provider.
func ValidateUpdateIntent(intent RuleMutationIntent) error {
	normalized, err := normalizeIntent(intent)
	if err != nil {
		return err
	}
	if normalized.SequenceSet && !validRuleSequence64(normalized.Sequence) {
		return invalidRuleInput("sequence", "out_of_range", "sequence must be between 1 and 2147483647")
	}
	if !intentHasUpdateField(normalized) {
		return invalidRuleInput("changes", "no_changes", "nothing to change; give at least one field to change, such as --name")
	}
	return nil
}

// BuildCreate checks a new rule and returns the request for the provider and
// the rule maildo expects to read back afterwards.
func BuildCreate(intent RuleMutationIntent, existing []domain.Rule) (domain.RuleCreateRequest, error) {
	normalized, err := normalizeIntent(intent)
	if err != nil {
		return domain.RuleCreateRequest{}, err
	}
	seq, source, err := createSequence(normalized, existing)
	if err != nil {
		return domain.RuleCreateRequest{}, err
	}
	rule := domain.Rule{
		Scope:        domain.RuleScopeInbox,
		DisplayName:  strings.TrimSpace(normalized.Name),
		Sequence:     seq,
		IsEnabled:    true,
		Conditions:   createPredicates(normalized),
		Actions:      createActions(normalized),
		ProviderMeta: map[string]any{},
		ProviderFieldPresence: domain.RuleProviderFieldPresence{
			ConditionsHasAttachments:                 normalized.HasAttachments.Set,
			ActionsMarkAsRead:                        normalized.MarkAsRead.Set,
			ActionsStopProcessingRules:               normalized.StopProcessingRules.Set,
			ActionsStopProcessingRulesValueAvailable: normalized.StopProcessingRules.Set,
		},
	}
	generated := false
	if !normalized.NameSet {
		rule.DisplayName = defaultRuleName(rule)
		generated = true
	}
	if !hasAnyRuleCondition(rule) {
		return domain.RuleCreateRequest{}, invalidRuleInput("conditions", "required", "a rule needs at least one condition, such as --from or --subject-contains")
	}
	if !hasAnyRuleAction(rule) {
		return domain.RuleCreateRequest{}, invalidRuleInput("actions", "required", "a rule needs at least one action, such as --move-to or --mark-read")
	}
	change := domain.RuleChangeSet{
		Operation:      domain.RuleOperationCreate,
		GeneratedName:  generated,
		SequenceSource: source,
		DisplayName:    &domain.RuleStringFieldChange{Set: true, After: rule.DisplayName},
		Sequence:       &domain.RuleIntFieldChange{Set: true, After: rule.Sequence},
		IsEnabled:      &domain.RuleBoolFieldChange{Set: true, After: boolPtr(true)},
	}
	fillCreateConditionChanges(&change, rule.Conditions)
	fillCreateActionChanges(&change, rule.Actions)
	if source == domain.RuleSequenceDefaulted {
		count := len(existing)
		change.SourceRuleCount = &count
	}
	return domain.RuleCreateRequest{Scope: domain.RuleScopeInbox, Rule: rule, ChangeSet: change}, nil
}

// BuildUpdate checks an update and returns the request for the provider. The
// request carries the rule as it is now, so the provider can keep the fields
// maildo cannot edit.
func BuildUpdate(existing domain.Rule, all []domain.Rule, intent RuleMutationIntent) (domain.RuleUpdateRequest, error) {
	if existing.IsReadOnly {
		return domain.RuleUpdateRequest{}, invalidRuleInput("rule_id", "rule_read_only", "the provider marks this rule as read-only, so maildo cannot change it")
	}
	normalized, err := normalizeIntent(intent)
	if err != nil {
		return domain.RuleUpdateRequest{}, err
	}
	if err := ValidateUpdateSequence(existing, all, normalized); err != nil {
		return domain.RuleUpdateRequest{}, err
	}
	change := domain.RuleChangeSet{
		Operation:      domain.RuleOperationUpdate,
		SequenceSource: domain.RuleSequencePreserved,
	}
	if normalized.NameSet {
		name := strings.TrimSpace(normalized.Name)
		if name == "" {
			return domain.RuleUpdateRequest{}, invalidRuleInput("name", "empty", "rule name must not be empty")
		}
		if name != existing.DisplayName {
			change.DisplayName = &domain.RuleStringFieldChange{Set: true, Before: existing.DisplayName, After: name}
		}
	}
	if normalized.SequenceSet {
		sequence := int(normalized.Sequence)
		change.SequenceSource = domain.RuleSequenceExplicit
		if sequence != existing.Sequence {
			change.Sequence = &domain.RuleIntFieldChange{Set: true, Before: existing.Sequence, After: sequence}
		}
	}
	fillUpdateConditionChanges(&change, existing, normalized)
	fillUpdateActionChanges(&change, existing, normalized)
	if !changeSetHasChanges(change) {
		return domain.RuleUpdateRequest{}, invalidRuleInput("changes", "no_changes", "the rule already looks like this, so there is nothing to change")
	}
	// Apply all changes before checking what is left: an update can clear
	// one condition while another one, which maildo cannot edit, stays.
	expected := ApplyChangeSet(existing, change)
	if !hasAnyRuleCondition(expected) {
		return domain.RuleUpdateRequest{}, invalidRuleInput("conditions", "empty_result", "after this update the rule would have no conditions left; a rule needs at least one")
	}
	if !hasAnyRuleAction(expected) {
		return domain.RuleUpdateRequest{}, invalidRuleInput("actions", "empty_result", "after this update the rule would have no actions left; a rule needs at least one")
	}
	return domain.RuleUpdateRequest{
		Scope:     domain.RuleScopeInbox,
		RuleID:    existing.ID,
		Existing:  cloneRule(existing),
		Expected:  expected,
		ChangeSet: change,
	}, nil
}

// ValidateUpdateSequence checks the position (sequence) only when the update
// changes it. Giving the same position again changes nothing, so problems in
// other rules' positions do not matter then.
func ValidateUpdateSequence(existing domain.Rule, all []domain.Rule, intent RuleMutationIntent) error {
	if !intent.SequenceSet {
		return validateRuleSequence(existing.Sequence)
	}
	if !validRuleSequence64(intent.Sequence) {
		return invalidRuleInput("sequence", "out_of_range", "sequence must be between 1 and 2147483647")
	}
	sequence := int(intent.Sequence)
	if sequence == existing.Sequence {
		return validateRuleSequence(existing.Sequence)
	}
	if err := validateRuleSequences(all); err != nil {
		return err
	}
	return ensureSequenceAvailable(sequence, all, existing.ID)
}

// BuildEnableDisable builds an update that only turns a rule on or off.
func BuildEnableDisable(existing domain.Rule, enabled bool) (domain.RuleUpdateRequest, error) {
	if existing.IsReadOnly {
		return domain.RuleUpdateRequest{}, invalidRuleInput("rule_id", "rule_read_only", "the provider marks this rule as read-only, so maildo cannot change it")
	}
	if err := validateRuleSequence(existing.Sequence); err != nil {
		return domain.RuleUpdateRequest{}, err
	}
	op := domain.RuleOperationDisable
	if enabled {
		op = domain.RuleOperationEnable
	}
	change := domain.RuleChangeSet{
		Operation:        op,
		SequenceSource:   domain.RuleSequencePreserved,
		RequestedEnabled: boolPtr(enabled),
	}
	if existing.IsEnabled != enabled {
		before := existing.IsEnabled
		change.IsEnabled = &domain.RuleBoolFieldChange{Set: true, Before: &before, After: boolPtr(enabled), BeforePresent: true, AfterPresent: true}
	}
	expected := ApplyChangeSet(existing, change)
	return domain.RuleUpdateRequest{
		Scope:     domain.RuleScopeInbox,
		RuleID:    existing.ID,
		Existing:  cloneRule(existing),
		Expected:  expected,
		ChangeSet: change,
	}, nil
}

// ApplyChangeSet returns a copy of rule with the changes applied.
func ApplyChangeSet(existing domain.Rule, change domain.RuleChangeSet) domain.Rule {
	out := cloneRule(existing)
	if change.DisplayName != nil && change.DisplayName.Set {
		out.DisplayName = change.DisplayName.After
	}
	if change.Sequence != nil && change.Sequence.Set {
		out.Sequence = change.Sequence.After
	}
	if change.IsEnabled != nil && change.IsEnabled.Set && change.IsEnabled.After != nil {
		out.IsEnabled = *change.IsEnabled.After
	}
	if change.FromAddresses != nil && change.FromAddresses.Set {
		out.Conditions.FromAddresses = append([]domain.Recipient(nil), change.FromAddresses.After...)
	}
	if change.SenderContains != nil && change.SenderContains.Set {
		out.Conditions.SenderContains = append([]string(nil), change.SenderContains.After...)
	}
	if change.RecipientContains != nil && change.RecipientContains.Set {
		out.Conditions.RecipientContains = append([]string(nil), change.RecipientContains.After...)
	}
	if change.SubjectContains != nil && change.SubjectContains.Set {
		out.Conditions.SubjectContains = append([]string(nil), change.SubjectContains.After...)
	}
	if change.HeaderContains != nil && change.HeaderContains.Set {
		out.Conditions.HeaderContains = append([]string(nil), change.HeaderContains.After...)
	}
	if change.HasAttachments != nil && change.HasAttachments.Set {
		if change.HasAttachments.Cleared {
			// Nil represents "absent" for tri-state provider booleans. False is
			// a deliberate value and must remain distinguishable.
			out.Conditions.HasAttachments = nil
			out.ProviderFieldPresence.ConditionsHasAttachments = false
		} else {
			out.Conditions.HasAttachments = cloneBool(change.HasAttachments.After)
			out.ProviderFieldPresence.ConditionsHasAttachments = change.HasAttachments.AfterPresent
		}
	}
	if change.MoveToFolder != nil && change.MoveToFolder.Set {
		out.Actions.MoveToFolder = cloneTarget(change.MoveToFolder.After)
	}
	if change.MarkAsRead != nil && change.MarkAsRead.Set {
		if change.MarkAsRead.Cleared {
			// Nil represents "absent" for tri-state provider booleans. False is
			// a deliberate value and must remain distinguishable.
			out.Actions.MarkAsRead = nil
			out.ProviderFieldPresence.ActionsMarkAsRead = false
		} else {
			out.Actions.MarkAsRead = cloneBool(change.MarkAsRead.After)
			out.ProviderFieldPresence.ActionsMarkAsRead = change.MarkAsRead.AfterPresent
		}
	}
	if change.StopProcessingRules != nil && change.StopProcessingRules.Set {
		if change.StopProcessingRules.Cleared || change.StopProcessingRules.After == nil {
			out.Actions.StopProcessingRules = false
			out.ProviderFieldPresence.ActionsStopProcessingRules = false
			out.ProviderFieldPresence.ActionsStopProcessingRulesValueAvailable = false
		} else {
			out.Actions.StopProcessingRules = *change.StopProcessingRules.After
			out.ProviderFieldPresence.ActionsStopProcessingRules = change.StopProcessingRules.AfterPresent
			out.ProviderFieldPresence.ActionsStopProcessingRulesValueAvailable = change.StopProcessingRules.After != nil
		}
	}
	out.Conditions.Semantics = defaultSemantics(out.Conditions.Semantics)
	out.Exceptions.Semantics = defaultSemantics(out.Exceptions.Semantics)
	return out
}

// VerifyRuleMutation compares the rule maildo read back with the rule it
// expected, field by field. It uses the same field list as the changed and
// preserved fields in the output, so the check and the output always cover
// the same fields.
func VerifyRuleMutation(expected, observed domain.Rule, change domain.RuleChangeSet) domain.RuleVerification {
	touched := changedFieldSet(change)
	fields := []string{"rule_id", "display_name", "sequence", "is_enabled"}
	for _, field := range ruleConditionFieldCatalog {
		if predicateFieldPopulated(expected.Conditions, field) || predicateFieldPopulated(observed.Conditions, field) || touched[field] {
			fields = append(fields, field)
		}
	}
	for _, field := range ruleActionFieldCatalog {
		if actionFieldPopulated(expected.Actions, field) || actionFieldPopulated(observed.Actions, field) || touched[field] {
			fields = append(fields, field)
		}
	}
	if predicatesPopulated(expected.Exceptions) || predicatesPopulated(observed.Exceptions) {
		fields = append(fields, "exceptions")
	}
	if len(expected.UnsupportedFields) > 0 || len(observed.UnsupportedFields) > 0 {
		fields = append(fields, "unsupported_fields")
	}
	fields = append(fields,
		"presence.actions.mark_as_read",
		"presence.actions.stop_processing_rules",
		"presence.conditions.has_attachments",
	)
	fields = dedupeStrings(fields)
	sort.Strings(fields)
	var mismatches []domain.RuleVerificationMismatch
	for _, field := range fields {
		if mismatch, ok := compareRuleField(field, expected, observed); ok {
			mismatches = append(mismatches, mismatch)
		}
	}
	status := domain.RuleVerificationVerified
	if len(mismatches) > 0 {
		status = domain.RuleVerificationFailed
	}
	return domain.RuleVerification{Status: status, CheckedFields: fields, Mismatches: mismatches}
}

// VerifyRuleDelete counts a rule that can no longer be read as deleted.
func VerifyRuleDelete(err error) domain.RuleVerification {
	if err == nil {
		return domain.RuleVerification{
			Status:        domain.RuleVerificationFailed,
			CheckedFields: []string{"absence"},
			Mismatches: []domain.RuleVerificationMismatch{{
				Field:    "absence",
				Expected: true,
				Actual:   false,
			}},
		}
	}
	var productErr *errs.Error
	if errors.As(err, &productErr) && productErr.Code() == errs.CodeRuleNotFound {
		return domain.RuleVerification{Status: domain.RuleVerificationVerified, CheckedFields: []string{"absence"}}
	}
	return domain.RuleVerification{
		Status:        domain.RuleVerificationFailed,
		CheckedFields: []string{"absence"},
		Mismatches: []domain.RuleVerificationMismatch{{
			Field:  "absence",
			Actual: "read_error",
		}},
	}
}

// VerificationFailedError turns a failed check after a rule change into the
// error the commands show.
func VerificationFailedError(account, providerName string, op domain.RuleMutationOperation, ruleID string, v domain.RuleVerification) error {
	details := map[string]any{
		"account":             account,
		"provider":            providerName,
		"operation":           string(op),
		"verification_status": v.Status,
		"verification_cause":  string(domain.RuleVerificationCauseMismatch),
		"mismatch_count":      len(v.Mismatches),
	}
	if ruleID != "" {
		details["rule_id"] = ruleID
	}
	return errs.NewWithDetails(errs.CodeVerificationFailed, verificationGuidance(op), details)
}

func verificationGuidance(op domain.RuleMutationOperation) string {
	switch op {
	case domain.RuleOperationCreate:
		return "maildo could not confirm the new rule; run maildo rules list before you try again, because the rule may already exist"
	case domain.RuleOperationDelete:
		return "maildo could not confirm the delete; run maildo rules list and check that the rule is gone before you try again"
	default:
		return "maildo could not confirm the change; look at the rule with maildo rules view before you try again, because the change may already have happened"
	}
}

// normalizeIntent trims the flag values and refuses empty ones and repeats.
// Checks that need the existing rules or folders come later.
func normalizeIntent(in RuleMutationIntent) (RuleMutationIntent, error) {
	out := in
	var err error
	if out.NameSet {
		out.Name = strings.TrimSpace(out.Name)
		if out.Name == "" {
			return RuleMutationIntent{}, invalidRuleInput("name", "empty", "rule name must not be empty")
		}
	}
	if out.SenderContainsSet {
		out.SenderContains, err = normalizeStringFamily("sender_contains", out.SenderContains)
		if err != nil {
			return RuleMutationIntent{}, err
		}
	}
	if out.RecipientContainsSet {
		out.RecipientContains, err = normalizeStringFamily("recipient_contains", out.RecipientContains)
		if err != nil {
			return RuleMutationIntent{}, err
		}
	}
	if out.SubjectContainsSet {
		out.SubjectContains, err = normalizeStringFamily("subject_contains", out.SubjectContains)
		if err != nil {
			return RuleMutationIntent{}, err
		}
	}
	if out.HeaderContainsSet {
		out.HeaderContains, err = normalizeStringFamily("header_contains", out.HeaderContains)
		if err != nil {
			return RuleMutationIntent{}, err
		}
	}
	return out, nil
}

func normalizeStringFamily(field string, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, invalidRuleInput(field, "empty_value", "--"+strings.ReplaceAll(field, "_", "-")+" needs a value")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, invalidRuleInput(field, "empty_value", "a --"+strings.ReplaceAll(field, "_", "-")+" value is empty")
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return nil, invalidRuleInput(field, "duplicate_value", "--"+strings.ReplaceAll(field, "_", "-")+" has the same value twice")
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

// createSequence takes the position (sequence) the caller gave if no rule
// has it yet. Otherwise it puts the new rule after the last one, or at 1 when
// there are no rules.
func createSequence(intent RuleMutationIntent, existing []domain.Rule) (int, domain.RuleSequenceSource, error) {
	if intent.SequenceSet {
		if !validRuleSequence64(intent.Sequence) {
			return 0, "", invalidRuleInput("sequence", "out_of_range", "sequence must be between 1 and 2147483647")
		}
		sequence := int(intent.Sequence)
		if err := ensureSequenceAvailable(sequence, existing, ""); err != nil {
			return 0, "", err
		}
		return sequence, domain.RuleSequenceExplicit, nil
	}
	if err := validateRuleSequences(existing); err != nil {
		return 0, "", err
	}
	maxSeq := 0
	for _, rule := range existing {
		if rule.Sequence > maxSeq {
			maxSeq = rule.Sequence
		}
	}
	if maxSeq == math.MaxInt32 {
		return 0, "", invalidRuleInput("sequence", "sequence_exhausted", "there is no free position (sequence) left after the current rules")
	}
	return maxSeq + 1, domain.RuleSequenceDefaulted, nil
}

func validRuleSequence64(sequence int64) bool {
	return sequence >= 1 && sequence <= math.MaxInt32
}

func validateRuleSequence(sequence int) error {
	if sequence < 1 || int64(sequence) > math.MaxInt32 {
		return rungrad.NewError(rungrad.ExitAPI, "provider returned an invalid rule sequence")
	}
	return nil
}

// ValidateProviderSequence checks the positions the provider reported before
// maildo uses them in the output or in the check after a change.
func ValidateProviderSequence(sequence int) error {
	return validateRuleSequence(sequence)
}

func validateRuleSequences(existing []domain.Rule) error {
	for _, rule := range existing {
		if err := validateRuleSequence(rule.Sequence); err != nil {
			return err
		}
	}
	return nil
}

func ensureSequenceAvailable(sequence int, existing []domain.Rule, exceptID string) error {
	for _, rule := range existing {
		if exceptID != "" && rule.ID == exceptID {
			continue
		}
		if rule.Sequence == sequence {
			return invalidRuleInput("sequence", "duplicate", "another rule already has this position (sequence)")
		}
	}
	return nil
}

func createPredicates(intent RuleMutationIntent) domain.RulePredicates {
	p := domain.RulePredicates{Semantics: domain.DefaultRulePredicateSemantics()}
	if intent.FromSet {
		p.FromAddresses = append([]domain.Recipient(nil), intent.FromAddresses...)
	}
	if intent.SenderContainsSet {
		p.SenderContains = append([]string(nil), intent.SenderContains...)
	}
	if intent.RecipientContainsSet {
		p.RecipientContains = append([]string(nil), intent.RecipientContains...)
	}
	if intent.SubjectContainsSet {
		p.SubjectContains = append([]string(nil), intent.SubjectContains...)
	}
	if intent.HeaderContainsSet {
		p.HeaderContains = append([]string(nil), intent.HeaderContains...)
	}
	if intent.HasAttachments.Set {
		p.HasAttachments = boolPtr(intent.HasAttachments.Value)
	}
	return p
}

func createActions(intent RuleMutationIntent) domain.RuleActions {
	var a domain.RuleActions
	if intent.MoveToFolderSet && intent.MoveToFolder != nil {
		a.MoveToFolder = targetFromFolder(*intent.MoveToFolder)
	}
	if intent.MarkAsRead.Set {
		a.MarkAsRead = boolPtr(intent.MarkAsRead.Value)
	}
	if intent.StopProcessingRules.Set {
		a.StopProcessingRules = intent.StopProcessingRules.Value
	}
	return a
}

func fillCreateConditionChanges(change *domain.RuleChangeSet, p domain.RulePredicates) {
	if len(p.FromAddresses) > 0 {
		change.FromAddresses = &domain.RuleRecipientCollectionChange{Set: true, After: append([]domain.Recipient(nil), p.FromAddresses...), Count: len(p.FromAddresses)}
	}
	if len(p.SenderContains) > 0 {
		change.SenderContains = stringCollectionChange(nil, p.SenderContains)
	}
	if len(p.RecipientContains) > 0 {
		change.RecipientContains = stringCollectionChange(nil, p.RecipientContains)
	}
	if len(p.SubjectContains) > 0 {
		change.SubjectContains = stringCollectionChange(nil, p.SubjectContains)
	}
	if len(p.HeaderContains) > 0 {
		change.HeaderContains = stringCollectionChange(nil, p.HeaderContains)
	}
	if p.HasAttachments != nil {
		change.HasAttachments = &domain.RuleBoolFieldChange{Set: true, After: cloneBool(p.HasAttachments), AfterPresent: true}
	}
}

func fillCreateActionChanges(change *domain.RuleChangeSet, a domain.RuleActions) {
	if a.MoveToFolder != nil {
		change.MoveToFolder = &domain.RuleFolderTargetChange{Set: true, After: cloneTarget(a.MoveToFolder)}
	}
	if a.MarkAsRead != nil {
		change.MarkAsRead = &domain.RuleBoolFieldChange{Set: true, After: cloneBool(a.MarkAsRead), AfterPresent: true}
	}
	if a.StopProcessingRules {
		change.StopProcessingRules = &domain.RuleBoolFieldChange{Set: true, After: boolPtr(true), AfterPresent: true}
	}
}

// fillUpdateConditionChanges compares the values and whether each field was
// present at all, so setting a field to false is not the same as clearing it
// or leaving it out.
func fillUpdateConditionChanges(change *domain.RuleChangeSet, existing domain.Rule, intent RuleMutationIntent) {
	before := existing.Conditions
	if intent.FromSet && !reflect.DeepEqual(before.FromAddresses, intent.FromAddresses) {
		change.FromAddresses = recipientCollectionChange(before.FromAddresses, intent.FromAddresses)
	}
	if intent.SenderContainsSet && !sameStringSlice(before.SenderContains, intent.SenderContains) {
		change.SenderContains = stringCollectionChange(before.SenderContains, intent.SenderContains)
	}
	if intent.RecipientContainsSet && !sameStringSlice(before.RecipientContains, intent.RecipientContains) {
		change.RecipientContains = stringCollectionChange(before.RecipientContains, intent.RecipientContains)
	}
	if intent.SubjectContainsSet && !sameStringSlice(before.SubjectContains, intent.SubjectContains) {
		change.SubjectContains = stringCollectionChange(before.SubjectContains, intent.SubjectContains)
	}
	if intent.HeaderContainsSet && !sameStringSlice(before.HeaderContains, intent.HeaderContains) {
		change.HeaderContains = stringCollectionChange(before.HeaderContains, intent.HeaderContains)
	}
	if intent.HasAttachments.Set {
		after := boolPtr(intent.HasAttachments.Value)
		present := existing.ProviderFieldPresence.ConditionsHasAttachments
		if !present || !boolPtrEqual(before.HasAttachments, after) {
			change.HasAttachments = &domain.RuleBoolFieldChange{Set: true, Before: cloneBool(before.HasAttachments), After: after, BeforePresent: present, AfterPresent: true}
		}
	}
	if intent.HasAttachments.Clear && existing.ProviderFieldPresence.ConditionsHasAttachments {
		change.HasAttachments = &domain.RuleBoolFieldChange{Set: true, Cleared: true, Before: cloneBool(before.HasAttachments), BeforePresent: true}
	}
}

// fillUpdateActionChanges does the same for the yes/no actions, and compares
// target folders by their provider id.
func fillUpdateActionChanges(change *domain.RuleChangeSet, existing domain.Rule, intent RuleMutationIntent) {
	before := existing.Actions
	if intent.MoveToFolderSet && intent.MoveToFolder != nil {
		after := targetFromFolder(*intent.MoveToFolder)
		if before.MoveToFolder == nil || before.MoveToFolder.ProviderFolderID != after.ProviderFolderID {
			change.MoveToFolder = &domain.RuleFolderTargetChange{Set: true, Before: cloneTarget(before.MoveToFolder), After: after}
		}
	}
	if intent.MarkAsRead.Set {
		after := boolPtr(intent.MarkAsRead.Value)
		present := existing.ProviderFieldPresence.ActionsMarkAsRead
		if !present || !boolPtrEqual(before.MarkAsRead, after) {
			change.MarkAsRead = &domain.RuleBoolFieldChange{Set: true, Before: cloneBool(before.MarkAsRead), After: after, BeforePresent: present, AfterPresent: true}
		}
	}
	if intent.MarkAsRead.Clear && existing.ProviderFieldPresence.ActionsMarkAsRead {
		change.MarkAsRead = &domain.RuleBoolFieldChange{Set: true, Cleared: true, Before: cloneBool(before.MarkAsRead), BeforePresent: true}
	}
	if intent.StopProcessingRules.Set {
		after := boolPtr(intent.StopProcessingRules.Value)
		present := existing.ProviderFieldPresence.ActionsStopProcessingRules
		if !present || before.StopProcessingRules != intent.StopProcessingRules.Value {
			beforePtr := boolPtr(before.StopProcessingRules)
			change.StopProcessingRules = &domain.RuleBoolFieldChange{Set: true, Before: beforePtr, After: after, BeforePresent: present, AfterPresent: true}
		}
	}
	if intent.StopProcessingRules.Clear && existing.ProviderFieldPresence.ActionsStopProcessingRules {
		var beforePtr *bool
		if existing.ProviderFieldPresence.ActionsStopProcessingRulesValueAvailable {
			beforePtr = boolPtr(existing.Actions.StopProcessingRules)
		}
		change.StopProcessingRules = &domain.RuleBoolFieldChange{Set: true, Cleared: true, Before: beforePtr, BeforePresent: true}
	}
}

func stringCollectionChange(before, after []string) *domain.RuleStringCollectionChange {
	return &domain.RuleStringCollectionChange{
		Set:     true,
		Before:  append([]string(nil), before...),
		After:   append([]string(nil), after...),
		Removed: removedStrings(before, after),
		Count:   len(after),
	}
}

func recipientCollectionChange(before, after []domain.Recipient) *domain.RuleRecipientCollectionChange {
	return &domain.RuleRecipientCollectionChange{
		Set:     true,
		Before:  append([]domain.Recipient(nil), before...),
		After:   append([]domain.Recipient(nil), after...),
		Removed: removedRecipients(before, after),
		Count:   len(after),
	}
}

func removedStrings(before, after []string) []string {
	present := map[string]struct{}{}
	for _, v := range after {
		present[v] = struct{}{}
	}
	var out []string
	for _, v := range before {
		if _, ok := present[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

func removedRecipients(before, after []domain.Recipient) []domain.Recipient {
	present := map[string]struct{}{}
	for _, v := range after {
		present[strings.ToLower(v.Address)] = struct{}{}
	}
	var out []domain.Recipient
	for _, v := range before {
		if _, ok := present[strings.ToLower(v.Address)]; !ok {
			out = append(out, v)
		}
	}
	return out
}

// defaultRuleName names a rule after one condition and one action, picked in
// a fixed order, so the same rule always gets the same name.
func defaultRuleName(rule domain.Rule) string {
	condition := "messages"
	switch {
	case len(rule.Conditions.FromAddresses) > 0:
		condition = "from " + recipientName(rule.Conditions.FromAddresses[0])
	case len(rule.Conditions.SenderContains) > 0:
		condition = "sender contains " + rule.Conditions.SenderContains[0]
	case len(rule.Conditions.SubjectContains) > 0:
		condition = "subject contains " + rule.Conditions.SubjectContains[0]
	case len(rule.Conditions.RecipientContains) > 0:
		condition = "recipient contains " + rule.Conditions.RecipientContains[0]
	case len(rule.Conditions.HeaderContains) > 0:
		condition = "header contains " + rule.Conditions.HeaderContains[0]
	case rule.Conditions.HasAttachments != nil:
		condition = "messages with attachments"
	}
	action := "apply rule"
	switch {
	case rule.Actions.MoveToFolder != nil:
		target := rule.Actions.MoveToFolder.DisplayName
		if target == "" {
			target = rule.Actions.MoveToFolder.Path
		}
		if target == "" {
			target = rule.Actions.MoveToFolder.ProviderFolderID
		}
		action = "move to " + target
	case rule.Actions.MarkAsRead != nil:
		action = "mark read"
	case rule.Actions.StopProcessingRules:
		action = "stop processing"
	}
	return condition + " -> " + action
}

func recipientName(r domain.Recipient) string {
	if r.Address != "" {
		return r.Address
	}
	return r.Name
}

// These checks look only at the flags, before maildo calls the provider.
// Conflicts with the existing rules are found later.
func intentHasCondition(intent RuleMutationIntent) bool {
	return (intent.FromSet && len(intent.FromAddresses) > 0) ||
		(intent.SenderContainsSet && len(intent.SenderContains) > 0) ||
		(intent.RecipientContainsSet && len(intent.RecipientContains) > 0) ||
		(intent.SubjectContainsSet && len(intent.SubjectContains) > 0) ||
		(intent.HeaderContainsSet && len(intent.HeaderContains) > 0) ||
		intent.HasAttachments.Set
}

func intentHasAction(intent RuleMutationIntent) bool {
	return intent.MoveToFolderSet ||
		intent.MarkAsRead.Set ||
		(intent.StopProcessingRules.Set && intent.StopProcessingRules.Value)
}

func intentHasUpdateField(intent RuleMutationIntent) bool {
	return intent.NameSet ||
		intent.FromSet ||
		intent.SenderContainsSet ||
		intent.RecipientContainsSet ||
		intent.SubjectContainsSet ||
		intent.HeaderContainsSet ||
		intent.HasAttachments.Set ||
		intent.HasAttachments.Clear ||
		intent.MoveToFolderSet ||
		intent.MarkAsRead.Set ||
		intent.MarkAsRead.Clear ||
		intent.StopProcessingRules.Set ||
		intent.StopProcessingRules.Clear ||
		intent.SequenceSet
}

// hasAnyCondition also counts the conditions maildo cannot edit, so clearing
// one field is fine while the rule still has another condition.
func hasAnyCondition(p domain.RulePredicates) bool {
	return len(p.FromAddresses) > 0 ||
		len(p.SenderContains) > 0 ||
		len(p.RecipientContains) > 0 ||
		len(p.SubjectContains) > 0 ||
		len(p.HeaderContains) > 0 ||
		p.HasAttachments != nil ||
		len(p.BodyContains) > 0 ||
		len(p.BodyOrSubjectContains) > 0 ||
		len(p.Categories) > 0 ||
		p.Importance != "" ||
		p.Sensitivity != "" ||
		p.MessageActionFlag != "" ||
		p.WithinSizeRange != nil ||
		len(p.SentToAddresses) > 0 ||
		p.IsApprovalRequest != nil ||
		p.IsAutomaticForward != nil ||
		p.IsAutomaticReply != nil ||
		p.IsEncrypted != nil ||
		p.IsMeetingRequest != nil ||
		p.IsMeetingResponse != nil ||
		p.IsNonDeliveryReport != nil ||
		p.IsPermissionControlled != nil ||
		p.IsReadReceipt != nil ||
		p.IsSigned != nil ||
		p.IsVoicemail != nil ||
		p.NotSentToMe != nil ||
		p.SentCCMe != nil ||
		p.SentOnlyToMe != nil ||
		p.SentToMe != nil ||
		p.SentToOrCCMe != nil
}

// hasAnyRuleCondition also counts a field the provider sent as null.
func hasAnyRuleCondition(rule domain.Rule) bool {
	return hasAnyCondition(rule.Conditions) || rule.ProviderFieldPresence.ConditionsHasAttachments
}

// hasAnyAction also counts the actions maildo cannot edit, when it checks
// whether an update would leave the rule with nothing to do.
func hasAnyAction(a domain.RuleActions) bool {
	return a.MoveToFolder != nil ||
		a.CopyToFolder != nil ||
		a.MarkAsRead != nil ||
		a.StopProcessingRules ||
		len(a.AssignCategories) > 0 ||
		a.Delete ||
		a.PermanentDelete ||
		a.MarkImportance != "" ||
		len(a.ForwardTo) > 0 ||
		len(a.ForwardAsAttachment) > 0 ||
		len(a.RedirectTo) > 0
}

// hasAnyRuleAction also counts fields the provider sent with a zero value,
// which the value alone would not show.
func hasAnyRuleAction(rule domain.Rule) bool {
	return hasAnyAction(rule.Actions) ||
		rule.ProviderFieldPresence.ActionsMarkAsRead ||
		rule.ProviderFieldPresence.ActionsStopProcessingRules
}

func changeSetHasChanges(c domain.RuleChangeSet) bool {
	return c.DisplayName != nil ||
		c.Sequence != nil ||
		c.IsEnabled != nil ||
		changeTouchesConditions(c) ||
		changeTouchesActions(c)
}

func changeTouchesConditions(c domain.RuleChangeSet) bool {
	return c.FromAddresses != nil ||
		c.SenderContains != nil ||
		c.RecipientContains != nil ||
		c.SubjectContains != nil ||
		c.HeaderContains != nil ||
		c.HasAttachments != nil
}

func changeTouchesActions(c domain.RuleChangeSet) bool {
	return c.MoveToFolder != nil ||
		c.MarkAsRead != nil ||
		c.StopProcessingRules != nil
}

// These lists name every condition and action field. The changed and
// preserved fields in the output and the check after a change all use them.
var ruleConditionFieldCatalog = []string{
	"conditions.body_contains",
	"conditions.body_or_subject_contains",
	"conditions.categories",
	"conditions.from_addresses",
	"conditions.has_attachments",
	"conditions.header_contains",
	"conditions.importance",
	"conditions.is_approval_request",
	"conditions.is_automatic_forward",
	"conditions.is_automatic_reply",
	"conditions.is_encrypted",
	"conditions.is_meeting_request",
	"conditions.is_meeting_response",
	"conditions.is_non_delivery_report",
	"conditions.is_permission_controlled",
	"conditions.is_read_receipt",
	"conditions.is_signed",
	"conditions.is_voicemail",
	"conditions.message_action_flag",
	"conditions.not_sent_to_me",
	"conditions.recipient_contains",
	"conditions.sender_contains",
	"conditions.sensitivity",
	"conditions.sent_cc_me",
	"conditions.sent_only_to_me",
	"conditions.sent_to_addresses",
	"conditions.sent_to_me",
	"conditions.sent_to_or_cc_me",
	"conditions.subject_contains",
	"conditions.within_size_range",
}

var ruleActionFieldCatalog = []string{
	"actions.assign_categories",
	"actions.copy_to_folder",
	"actions.delete",
	"actions.forward_as_attachment_to",
	"actions.forward_to",
	"actions.mark_as_read",
	"actions.mark_importance",
	"actions.move_to_folder",
	"actions.permanent_delete",
	"actions.redirect_to",
	"actions.stop_processing_rules",
}

// ChangedFields returns the fields a change touches, sorted.
func ChangedFields(change domain.RuleChangeSet) []string {
	set := changedFieldSet(change)
	out := make([]string, 0, len(set))
	for field := range set {
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

func changedFieldSet(change domain.RuleChangeSet) map[string]bool {
	out := map[string]bool{}
	if change.DisplayName != nil {
		out["display_name"] = true
	}
	if change.Sequence != nil {
		out["sequence"] = true
	}
	if change.IsEnabled != nil {
		out["is_enabled"] = true
	}
	if change.FromAddresses != nil {
		out["conditions.from_addresses"] = true
	}
	if change.SenderContains != nil {
		out["conditions.sender_contains"] = true
	}
	if change.RecipientContains != nil {
		out["conditions.recipient_contains"] = true
	}
	if change.SubjectContains != nil {
		out["conditions.subject_contains"] = true
	}
	if change.HeaderContains != nil {
		out["conditions.header_contains"] = true
	}
	if change.HasAttachments != nil {
		out["conditions.has_attachments"] = true
	}
	if change.MoveToFolder != nil {
		out["actions.move_to_folder"] = true
	}
	if change.MarkAsRead != nil {
		out["actions.mark_as_read"] = true
	}
	if change.StopProcessingRules != nil {
		out["actions.stop_processing_rules"] = true
	}
	return out
}

// PreservedFields returns the fields an update leaves as they were, sorted.
func PreservedFields(rule domain.Rule, change domain.RuleChangeSet) []string {
	if change.Operation == domain.RuleOperationCreate || change.Operation == domain.RuleOperationDelete {
		return []string{}
	}
	if (change.Operation == domain.RuleOperationEnable || change.Operation == domain.RuleOperationDisable) && change.IsEnabled == nil {
		return []string{}
	}
	changed := changedFieldSet(change)
	var out []string
	if rule.DisplayName != "" && !changed["display_name"] {
		out = append(out, "display_name")
	}
	if rule.Sequence != 0 && !changed["sequence"] {
		out = append(out, "sequence")
	}
	if !changed["is_enabled"] {
		out = append(out, "is_enabled")
	}
	for _, field := range ruleConditionFieldCatalog {
		if !changed[field] && predicateFieldPopulated(rule.Conditions, field) {
			out = append(out, field)
		}
	}
	for _, field := range ruleActionFieldCatalog {
		if !changed[field] && actionFieldPopulated(rule.Actions, field) {
			out = append(out, field)
		}
	}
	if predicatesPopulated(rule.Exceptions) {
		out = append(out, "exceptions")
	}
	if len(rule.UnsupportedFields) > 0 {
		out = append(out, "unsupported_fields")
	}
	sort.Strings(out)
	return out
}

// compareRuleField returns the expected and the observed value of one field.
// Errors show only the field name and the number of mismatches, never the
// values.
func compareRuleField(field string, expected, observed domain.Rule) (domain.RuleVerificationMismatch, bool) {
	var want, got any
	switch field {
	case "rule_id":
		want, got = expected.ID, observed.ID
	case "display_name":
		want, got = expected.DisplayName, observed.DisplayName
	case "sequence":
		want, got = expected.Sequence, observed.Sequence
	case "is_enabled":
		want, got = expected.IsEnabled, observed.IsEnabled
	case "exceptions":
		want, got = comparablePredicates(expected.Exceptions), comparablePredicates(observed.Exceptions)
	case "unsupported_fields":
		want, got = comparableUnsupported(expected.UnsupportedFields), comparableUnsupported(observed.UnsupportedFields)
	case "presence.conditions.has_attachments":
		want, got = expected.ProviderFieldPresence.ConditionsHasAttachments, observed.ProviderFieldPresence.ConditionsHasAttachments
	case "presence.actions.mark_as_read":
		want, got = expected.ProviderFieldPresence.ActionsMarkAsRead, observed.ProviderFieldPresence.ActionsMarkAsRead
	case "presence.actions.stop_processing_rules":
		want, got = expected.ProviderFieldPresence.ActionsStopProcessingRules, observed.ProviderFieldPresence.ActionsStopProcessingRules
	default:
		if strings.HasPrefix(field, "conditions.") {
			want, got = predicateFieldValue(expected.Conditions, field), predicateFieldValue(observed.Conditions, field)
		} else if strings.HasPrefix(field, "actions.") {
			want, got = actionFieldValue(expected.Actions, field), actionFieldValue(observed.Actions, field)
		} else {
			return domain.RuleVerificationMismatch{}, false
		}
	}
	if reflect.DeepEqual(want, got) {
		return domain.RuleVerificationMismatch{}, false
	}
	return domain.RuleVerificationMismatch{Field: field, Expected: want, Actual: got}, true
}

func predicateFieldValue(p domain.RulePredicates, field string) any {
	switch field {
	case "conditions.body_contains":
		return p.BodyContains
	case "conditions.body_or_subject_contains":
		return p.BodyOrSubjectContains
	case "conditions.categories":
		return p.Categories
	case "conditions.from_addresses":
		return p.FromAddresses
	case "conditions.has_attachments":
		return p.HasAttachments
	case "conditions.header_contains":
		return p.HeaderContains
	case "conditions.importance":
		return p.Importance
	case "conditions.is_approval_request":
		return p.IsApprovalRequest
	case "conditions.is_automatic_forward":
		return p.IsAutomaticForward
	case "conditions.is_automatic_reply":
		return p.IsAutomaticReply
	case "conditions.is_encrypted":
		return p.IsEncrypted
	case "conditions.is_meeting_request":
		return p.IsMeetingRequest
	case "conditions.is_meeting_response":
		return p.IsMeetingResponse
	case "conditions.is_non_delivery_report":
		return p.IsNonDeliveryReport
	case "conditions.is_permission_controlled":
		return p.IsPermissionControlled
	case "conditions.is_read_receipt":
		return p.IsReadReceipt
	case "conditions.is_signed":
		return p.IsSigned
	case "conditions.is_voicemail":
		return p.IsVoicemail
	case "conditions.message_action_flag":
		return p.MessageActionFlag
	case "conditions.not_sent_to_me":
		return p.NotSentToMe
	case "conditions.recipient_contains":
		return p.RecipientContains
	case "conditions.sender_contains":
		return p.SenderContains
	case "conditions.sensitivity":
		return p.Sensitivity
	case "conditions.sent_cc_me":
		return p.SentCCMe
	case "conditions.sent_only_to_me":
		return p.SentOnlyToMe
	case "conditions.sent_to_addresses":
		return p.SentToAddresses
	case "conditions.sent_to_me":
		return p.SentToMe
	case "conditions.sent_to_or_cc_me":
		return p.SentToOrCCMe
	case "conditions.subject_contains":
		return p.SubjectContains
	case "conditions.within_size_range":
		return p.WithinSizeRange
	default:
		return nil
	}
}

func actionFieldValue(a domain.RuleActions, field string) any {
	switch field {
	case "actions.assign_categories":
		return a.AssignCategories
	case "actions.copy_to_folder":
		return targetID(a.CopyToFolder)
	case "actions.delete":
		return a.Delete
	case "actions.forward_as_attachment_to":
		return a.ForwardAsAttachment
	case "actions.forward_to":
		return a.ForwardTo
	case "actions.mark_as_read":
		return a.MarkAsRead
	case "actions.mark_importance":
		return a.MarkImportance
	case "actions.move_to_folder":
		return targetID(a.MoveToFolder)
	case "actions.permanent_delete":
		return a.PermanentDelete
	case "actions.redirect_to":
		return a.RedirectTo
	case "actions.stop_processing_rules":
		return a.StopProcessingRules
	default:
		return nil
	}
}

func predicateFieldPopulated(p domain.RulePredicates, field string) bool {
	v := predicateFieldValue(p, field)
	return valuePopulated(v)
}

func actionFieldPopulated(a domain.RuleActions, field string) bool {
	v := actionFieldValue(a, field)
	return valuePopulated(v)
}

// valuePopulated looks only at the value. Whether the provider sent the
// field at all is compared separately and does not show in
// preserved_fields.
func valuePopulated(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !rv.IsNil()
	case reflect.Slice, reflect.Map, reflect.String:
		return rv.Len() > 0
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	default:
		return !rv.IsZero()
	}
}

func predicatesPopulated(p domain.RulePredicates) bool {
	for _, field := range ruleConditionFieldCatalog {
		if predicateFieldPopulated(p, field) {
			return true
		}
	}
	return false
}

func comparablePredicates(p domain.RulePredicates) domain.RulePredicates {
	p = clonePredicates(p)
	p.Semantics = domain.RulePredicateSemantics{}
	return p
}

// unsupportedTuple keeps only what is needed to check that the fields
// maildo cannot edit survived a change, without the descriptions.
type unsupportedTuple struct {
	Area      string
	Field     string
	ValueKind string
	Count     int
}

// comparableUnsupported sorts those fields before comparing, so a different
// order from the provider does not look like a change.
func comparableUnsupported(in []domain.RuleUnsupportedField) []unsupportedTuple {
	out := make([]unsupportedTuple, 0, len(in))
	for _, field := range in {
		out = append(out, unsupportedTuple{Area: field.Area, Field: field.Field, ValueKind: field.ValueKind, Count: field.Count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		if out[i].ValueKind != out[j].ValueKind {
			return out[i].ValueKind < out[j].ValueKind
		}
		return out[i].Count < out[j].Count
	})
	return out
}

func targetID(target *domain.RuleFolderTarget) string {
	if target == nil {
		return ""
	}
	return target.ProviderFolderID
}

func boolPtr(v bool) *bool {
	return &v
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameStringSlice(a, b []string) bool {
	return reflect.DeepEqual(a, b)
}

func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func invalidRuleInput(field, reason, message string) error {
	return errs.NewWithDetails(errs.CodeInvalidRuleInput, message, map[string]any{"field": field, "reason": reason})
}

func targetFromFolder(folder domain.Folder) *domain.RuleFolderTarget {
	return &domain.RuleFolderTarget{
		ProviderFolderID: folder.ID,
		DisplayName:      folder.DisplayName,
		Path:             folder.Path,
		WellKnownName:    folder.WellKnownName,
	}
}

// cloneAnyMap deep-copies the provider's extra data, so planning a change
// can never alter the rule maildo read.
func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneAnyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneAny(item)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []domain.Recipient:
		return append([]domain.Recipient(nil), x...)
	default:
		return x
	}
}
