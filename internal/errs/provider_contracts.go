package errs

// Write outcomes
//
// A change to a mailbox can fail in three ways that matter to the user:
//
//   - rejected: the provider said no, so nothing changed and trying again
//     is safe.
//   - not applied: the request never left maildo.
//   - unknown: the request went out, but maildo cannot tell whether it
//     landed, for example because the connection dropped before the answer.
//     Trying again might do it twice.
//
// Providers report this through a ProviderWriteAttempt. They create one
// before a write, call CrossProviderWriteTransmissionBoundary just before
// the request leaves, and finish it with exactly one of
// VerifiedWriteSuccessFacts, NewWriteRejected, NewWriteNotApplied or
// NewOutcomeUnknown. The error then carries "outcome", "retry_safe" and
// "write_may_have_landed", which the CLI turns into its advice.
//
// The other constructors in this file build the read errors providers
// share (limits, not found, connection and data errors) with the same
// detail keys everywhere.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vincentsch/maildo/internal/limits"
)

type ProviderName string

const (
	ProviderOutlook  ProviderName = "outlook"
	ProviderGmail    ProviderName = "gmail"
	ProviderIMAP     ProviderName = "imap"
	ProviderFastmail ProviderName = "fastmail"
)

func NewSharedProviderContractInvalid() error {
	return New(CodeSharedProviderContractInvalid, "shared provider contract is invalid")
}

func NewProviderClientContractInvalid() error {
	return New(CodeProviderClientContractInvalid, "provider client contract is invalid")
}

// NewProviderClientClosed reports a call on an account client after the
// command finished with it.
func NewProviderClientClosed(account string, provider ProviderName) error {
	if !safeAccount(account) || !validProviderName(provider) {
		return NewProviderClientContractInvalid()
	}
	return NewWithDetails(CodeProviderClientClosed, "provider client is closed", map[string]any{
		"account":  account,
		"provider": string(provider),
	})
}

type WriteOutcome string

const (
	WriteOutcomeNotApplied   WriteOutcome = "not_applied"
	WriteOutcomeRejected     WriteOutcome = "rejected"
	WriteOutcomeSucceeded    WriteOutcome = "succeeded"
	WriteOutcomeNoOp         WriteOutcome = "no_op"
	WriteOutcomeKnownPartial WriteOutcome = "known_partial"
	WriteOutcomeUnknown      WriteOutcome = "unknown"
)

// WriteOutcomeFacts say how a write ended. A result that worked shows only
// the outcome and what the provider confirmed. WriteMayHaveLanded and
// RetrySafe matter when a write failed, so they appear in the error's
// details (and in error_details for one message of a bulk change), never
// next to a success, where they would read like a warning.
type WriteOutcomeFacts struct {
	Outcome             WriteOutcome `json:"outcome"`
	WriteMayHaveLanded  bool         `json:"-"`
	RetrySafe           bool         `json:"-"`
	AcceptedByProvider  bool         `json:"accepted_by_provider"`
	ConfirmedAfterWrite bool         `json:"confirmed_after_write"`
	Succeeded           bool         `json:"succeeded"`
}

type ProviderWriteOperation string

const (
	WriteOutlookDirectSend     ProviderWriteOperation = "outlook_direct_send"
	WriteOutlookDraftSend      ProviderWriteOperation = "outlook_draft_send"
	WriteOutlookCompose        ProviderWriteOperation = "outlook_compose"
	WriteOutlookMessageChange  ProviderWriteOperation = "outlook_message_change"
	WriteGmailDraftCreate      ProviderWriteOperation = "gmail_draft_create"
	WriteGmailDraftUpdate      ProviderWriteOperation = "gmail_draft_update"
	WriteGmailDraftDelete      ProviderWriteOperation = "gmail_draft_delete"
	WriteGmailDraftSend        ProviderWriteOperation = "gmail_draft_send"
	WriteGmailDirectSend       ProviderWriteOperation = "gmail_direct_send"
	WriteGmailReply            ProviderWriteOperation = "gmail_reply"
	WriteGmailForward          ProviderWriteOperation = "gmail_forward"
	WriteGmailAttachmentAdd    ProviderWriteOperation = "gmail_attachment_add"
	WriteGmailAttachmentRemove ProviderWriteOperation = "gmail_attachment_remove"
	WriteGmailLabelCreate      ProviderWriteOperation = "gmail_label_create"
	WriteGmailLabelRename      ProviderWriteOperation = "gmail_label_rename"
	WriteGmailLabelDelete      ProviderWriteOperation = "gmail_label_delete"
	WriteGmailMove             ProviderWriteOperation = "gmail_move"
	WriteGmailCopy             ProviderWriteOperation = "gmail_copy"
	WriteGmailArchive          ProviderWriteOperation = "gmail_archive"
	WriteGmailDelete           ProviderWriteOperation = "gmail_delete"
	WriteGmailRestore          ProviderWriteOperation = "gmail_restore"
	WriteGmailJunk             ProviderWriteOperation = "gmail_junk"
	WriteGmailMarkRead         ProviderWriteOperation = "gmail_mark_read"
	WriteGmailMarkUnread       ProviderWriteOperation = "gmail_mark_unread"
	WriteGmailFlagSet          ProviderWriteOperation = "gmail_flag_set"
	WriteGmailFlagClear        ProviderWriteOperation = "gmail_flag_clear"
	WriteGmailCategoriesSet    ProviderWriteOperation = "gmail_categories_set"
	WriteGmailCategoriesClear  ProviderWriteOperation = "gmail_categories_clear"
	WriteSMTPDirectSend        ProviderWriteOperation = "smtp_direct_send"
	WriteIMAPFolderCreate      ProviderWriteOperation = "imap_folder_create"
	WriteIMAPFolderRename      ProviderWriteOperation = "imap_folder_rename"
	WriteIMAPFolderDelete      ProviderWriteOperation = "imap_folder_delete"
	WriteIMAPCopy              ProviderWriteOperation = "imap_copy"
	WriteIMAPMove              ProviderWriteOperation = "imap_move"
	WriteIMAPArchive           ProviderWriteOperation = "imap_archive"
	WriteIMAPDelete            ProviderWriteOperation = "imap_delete"
	WriteIMAPRestore           ProviderWriteOperation = "imap_restore"
	WriteIMAPJunk              ProviderWriteOperation = "imap_junk"
	WriteIMAPMarkRead          ProviderWriteOperation = "imap_mark_read"
	WriteIMAPMarkUnread        ProviderWriteOperation = "imap_mark_unread"
	WriteIMAPFlagSet           ProviderWriteOperation = "imap_flag_set"
	WriteIMAPFlagClear         ProviderWriteOperation = "imap_flag_clear"
	WriteIMAPDraftCreate       ProviderWriteOperation = "imap_draft_create"
	WriteIMAPDraftUpdate       ProviderWriteOperation = "imap_draft_update"
	WriteIMAPDraftDelete       ProviderWriteOperation = "imap_draft_delete"
	WriteIMAPReply             ProviderWriteOperation = "imap_reply"
	WriteIMAPForward           ProviderWriteOperation = "imap_forward"
)

type ProviderWriteAttempt interface {
	providerWriteAttempt()
}

type writeAttemptState int

const (
	attemptPreTransmission writeAttemptState = iota
	attemptTransmitted
	attemptConsumed
)

type writeAttempt struct {
	mu        sync.Mutex
	account   string
	provider  ProviderName
	operation ProviderWriteOperation
	state     writeAttemptState
}

func (*writeAttempt) providerWriteAttempt() {}

func NewProviderWriteAttempt(account string, provider ProviderName, operation ProviderWriteOperation) (ProviderWriteAttempt, error) {
	if !safeAccount(account) || !operationAllowedForProvider(provider, operation) {
		return nil, NewSharedProviderContractInvalid()
	}
	return &writeAttempt{account: account, provider: provider, operation: operation}, nil
}

func CrossProviderWriteTransmissionBoundary(attempt ProviderWriteAttempt) error {
	a, ok := attempt.(*writeAttempt)
	if !ok || a == nil {
		return NewSharedProviderContractInvalid()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != attemptPreTransmission {
		return NewSharedProviderContractInvalid()
	}
	a.state = attemptTransmitted
	return nil
}

func NewWriteNotApplied(attempt ProviderWriteAttempt, cause error) error {
	a, err := consumeAttempt(attempt, attemptPreTransmission)
	if err != nil {
		return err
	}
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		return cause
	}
	product, ok := cause.(*Error)
	if !ok || product == nil {
		return NewSharedProviderContractInvalid()
	}
	return product.withWriteFacts(a, WriteOutcomeFacts{Outcome: WriteOutcomeNotApplied, RetrySafe: true})
}

type DefinitiveProviderRejection interface {
	definitiveProviderRejection()
}

type definitiveRejection struct {
	err       *Error
	account   string
	provider  ProviderName
	operation ProviderWriteOperation
	preData   bool
}

func (definitiveRejection) definitiveProviderRejection() {}

func AsDefinitiveProviderRejection(err error) (DefinitiveProviderRejection, bool) {
	var product *Error
	if !errors.As(err, &product) || product == nil || product.definitive == nil {
		return nil, false
	}
	return *product.definitive, true
}

func NewDefinitiveProviderRejectionError(account string, provider ProviderName, operation ProviderWriteOperation, cause error) error {
	return newDefinitiveProviderRejectionError(account, provider, operation, cause, false)
}

func NewPreTransmissionDefinitiveProviderRejectionError(account string, provider ProviderName, operation ProviderWriteOperation, cause error) error {
	if (provider != ProviderIMAP && provider != ProviderFastmail) || operation != WriteSMTPDirectSend {
		return NewSharedProviderContractInvalid()
	}
	return newDefinitiveProviderRejectionError(account, provider, operation, cause, true)
}

func newDefinitiveProviderRejectionError(account string, provider ProviderName, operation ProviderWriteOperation, cause error, preData bool) error {
	if !safeAccount(account) || !operationAllowedForProvider(provider, operation) {
		return NewSharedProviderContractInvalid()
	}
	var product *Error
	if !errors.As(cause, &product) || product == nil || !definitiveProviderRejectionCodeAllowed(product.code) || product.outcome != nil || product.definitive != nil {
		return NewSharedProviderContractInvalid()
	}
	out := NewWithDetails(product.code, product.message, product.Details())
	rejection := definitiveRejection{err: out, account: account, provider: provider, operation: operation, preData: preData}
	out.definitive = &rejection
	return out
}

func definitiveProviderRejectionCodeAllowed(code string) bool {
	switch code {
	case CodeProviderIdentityUnavailable, CodeAuthRepairRequired, CodeProviderForbidden, CodeProviderRateLimited,
		CodeProviderConnectionError, CodeProviderDataError, CodeProviderConflict, CodeInvalidComposeInput,
		CodeMessageTooLarge, CodeRecipientRejected, CodeInvalidFolderName, CodeFolderNotFound, CodeProtectedFolder,
		CodeMessageNotFound, CodeDraftNotFound:
		return true
	default:
		return false
	}
}

func NewWriteRejected(attempt ProviderWriteAttempt, rejection DefinitiveProviderRejection) error {
	r, ok := rejection.(definitiveRejection)
	if !ok || r.err == nil {
		return NewSharedProviderContractInvalid()
	}
	wantState := attemptTransmitted
	if r.preData {
		wantState = attemptPreTransmission
	}
	a, err := consumeAttempt(attempt, wantState)
	if err != nil {
		return err
	}
	if a.account != r.account || a.provider != r.provider || a.operation != r.operation {
		return NewSharedProviderContractInvalid()
	}
	return r.err.withWriteFacts(a, WriteOutcomeFacts{Outcome: WriteOutcomeRejected, RetrySafe: true})
}

func VerifiedWriteSuccessFacts(attempt ProviderWriteAttempt) (*WriteOutcomeFacts, error) {
	if _, err := consumeAttempt(attempt, attemptTransmitted); err != nil {
		return nil, err
	}
	return &WriteOutcomeFacts{
		Outcome:             WriteOutcomeSucceeded,
		WriteMayHaveLanded:  true,
		RetrySafe:           false,
		AcceptedByProvider:  true,
		ConfirmedAfterWrite: true,
		Succeeded:           true,
	}, nil
}

func VerifiedWriteNoOpFacts() *WriteOutcomeFacts {
	return &WriteOutcomeFacts{Outcome: WriteOutcomeNoOp, RetrySafe: true, Succeeded: true}
}

type KnownPartialOperation string

const (
	KnownGmailReplySend       KnownPartialOperation = "gmail_reply_send"
	KnownGmailForwardSend     KnownPartialOperation = "gmail_forward_send"
	KnownGmailMove            KnownPartialOperation = "gmail_move"
	KnownGmailCopy            KnownPartialOperation = "gmail_copy"
	KnownGmailArchive         KnownPartialOperation = "gmail_archive"
	KnownGmailDelete          KnownPartialOperation = "gmail_delete"
	KnownGmailRestore         KnownPartialOperation = "gmail_restore"
	KnownGmailJunk            KnownPartialOperation = "gmail_junk"
	KnownGmailMarkRead        KnownPartialOperation = "gmail_mark_read"
	KnownGmailMarkUnread      KnownPartialOperation = "gmail_mark_unread"
	KnownGmailFlagSet         KnownPartialOperation = "gmail_flag_set"
	KnownGmailFlagClear       KnownPartialOperation = "gmail_flag_clear"
	KnownGmailCategoriesSet   KnownPartialOperation = "gmail_categories_set"
	KnownGmailCategoriesClear KnownPartialOperation = "gmail_categories_clear"
)

type KnownPartialPhase string

const (
	KnownPhaseDraftVerified KnownPartialPhase = "draft_verified"
	KnownPhaseSend          KnownPartialPhase = "send"
	KnownPhaseUntrash       KnownPartialPhase = "untrash"
	KnownPhaseTrash         KnownPartialPhase = "trash"
	KnownPhaseModifyChunk   KnownPartialPhase = "modify_chunk"
)

type KnownPartialObservedState string

const (
	KnownObservedLastVerified          KnownPartialObservedState = "last_verified_intermediate"
	KnownObservedIntendedFinal         KnownPartialObservedState = "intended_final"
	KnownObservedOther                 KnownPartialObservedState = "other"
	KnownObservedDiagnosticUnavailable KnownPartialObservedState = "diagnostic_unavailable"
)

type KnownPartialPlan interface {
	knownPartialPlan()
}

type knownPartialPlan struct {
	operation KnownPartialOperation
	phases    []KnownPartialPhase
}

func (*knownPartialPlan) knownPartialPlan() {}

type KnownPartialDetails struct {
	PlannedPhases                  []KnownPartialPhase       `json:"planned_phases"`
	CompletedPhaseIndices          []uint16                  `json:"completed_phase_indices"`
	FailedPhaseIndex               uint16                    `json:"failed_phase_index"`
	FailedPhase                    KnownPartialPhase         `json:"failed_phase"`
	FailedPhaseTransmissionCrossed bool                      `json:"failed_phase_transmission_crossed"`
	ObservedState                  KnownPartialObservedState `json:"observed_state"`
	VerifiedDraftID                string                    `json:"verified_draft_id,omitempty"`
}

func NewKnownPartialPlan(operation KnownPartialOperation, phases []KnownPartialPhase) (KnownPartialPlan, error) {
	if !validKnownPartialPlan(operation, phases) {
		return nil, NewSharedProviderContractInvalid()
	}
	return &knownPartialPlan{operation: operation, phases: append([]KnownPartialPhase(nil), phases...)}, nil
}

func AdvanceVerifiedWritePhase(attempt ProviderWriteAttempt, plan KnownPartialPlan, completedPrefixLength uint16) (ProviderWriteAttempt, error) {
	a, err := consumeAttempt(attempt, attemptTransmitted)
	if err != nil {
		return nil, err
	}
	p, ok := plan.(*knownPartialPlan)
	if !ok || p == nil || !knownPartialPrefixValid(p, completedPrefixLength) || !knownPartialAttemptMatchesPlan(a, p) {
		return nil, NewSharedProviderContractInvalid()
	}
	next, err := NewProviderWriteAttempt(a.account, a.provider, knownPartialNextOperation(p, completedPrefixLength))
	if err != nil {
		return nil, err
	}
	return next, nil
}

func NewKnownPartial(attempt ProviderWriteAttempt, rejection DefinitiveProviderRejection, plan KnownPartialPlan, completedPrefixLength uint16, observed KnownPartialObservedState, verifiedDraftID string) error {
	r, ok := rejection.(definitiveRejection)
	if !ok || r.err == nil || r.preData || !validKnownPartialObservedState(observed) {
		return NewSharedProviderContractInvalid()
	}
	a, details, err := knownPartialDetails(attempt, plan, completedPrefixLength, true, observed, verifiedDraftID)
	if err != nil {
		return err
	}
	if a.account != r.account || a.provider != r.provider || a.operation != r.operation {
		return NewSharedProviderContractInvalid()
	}
	return r.err.withKnownPartialFacts(a, details, "the provider refused a later step after the earlier steps were done")
}

func NewKnownPartialBeforeNextPhase(attempt ProviderWriteAttempt, cause error, plan KnownPartialPlan, completedPrefixLength uint16, verifiedDraftID string) error {
	a, details, err := knownPartialDetails(attempt, plan, completedPrefixLength, false, KnownObservedLastVerified, verifiedDraftID)
	if err != nil {
		return err
	}
	reason := "failed_before_next_phase"
	productMessage := "a later step failed after the earlier steps were done"
	var product *Error
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "deadline_exceeded_before_next_phase"
		product = New(CodeVerificationFailed, "maildo ran out of time after the earlier steps were done")
	case errors.Is(cause, context.Canceled):
		reason = "canceled_before_next_phase"
		product = New(CodeVerificationFailed, "the command was cancelled after the earlier steps were done")
	case errors.As(cause, &product) && product != nil && product.outcome == nil && product.definitive == nil:
		product = NewWithDetails(product.code, product.message, product.Details())
	default:
		return NewSharedProviderContractInvalid()
	}
	if product.message == "" {
		product.message = productMessage
	}
	details["reason"] = reason
	return product.withKnownPartialFacts(a, details, product.message)
}

func CloneWriteOutcomeFacts(in *WriteOutcomeFacts) *WriteOutcomeFacts {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func WriteOutcomeFactsFromError(err error) (*WriteOutcomeFacts, bool) {
	var product *Error
	if !errors.As(err, &product) || product == nil || product.outcome == nil {
		return nil, false
	}
	return CloneWriteOutcomeFacts(product.outcome), true
}

func consumeAttempt(attempt ProviderWriteAttempt, state writeAttemptState) (*writeAttempt, error) {
	a, ok := attempt.(*writeAttempt)
	if !ok || a == nil {
		return nil, NewSharedProviderContractInvalid()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != state {
		return nil, NewSharedProviderContractInvalid()
	}
	a.state = attemptConsumed
	return a, nil
}

func (e *Error) withWriteFacts(a *writeAttempt, facts WriteOutcomeFacts) error {
	if e == nil || a == nil {
		return NewSharedProviderContractInvalid()
	}
	details := e.Details()
	for _, key := range []string{"outcome", "write_may_have_landed", "retry_safe", "accepted_by_provider", "confirmed_after_write", "succeeded"} {
		if _, exists := details[key]; exists {
			return NewSharedProviderContractInvalid()
		}
	}
	details["account"] = a.account
	details["provider"] = string(a.provider)
	details["operation"] = string(a.operation)
	addWriteFacts(details, facts)
	out := NewWithDetails(e.code, e.message, details)
	out.outcome = CloneWriteOutcomeFacts(&facts)
	return out
}

func (e *Error) withKnownPartialFacts(a *writeAttempt, known map[string]any, message string) error {
	if e == nil || a == nil {
		return NewSharedProviderContractInvalid()
	}
	details := e.Details()
	for _, key := range []string{"outcome", "write_may_have_landed", "retry_safe", "accepted_by_provider", "confirmed_after_write", "succeeded", "compound_result", "state_reread_required"} {
		if _, exists := details[key]; exists {
			return NewSharedProviderContractInvalid()
		}
	}
	for key, value := range known {
		if _, exists := details[key]; exists {
			return NewSharedProviderContractInvalid()
		}
		details[key] = value
	}
	details["account"] = a.account
	details["provider"] = string(a.provider)
	details["operation"] = string(a.operation)
	details["compound_result"] = "known_partial"
	details["state_reread_required"] = true
	facts := WriteOutcomeFacts{
		Outcome:            WriteOutcomeKnownPartial,
		WriteMayHaveLanded: true,
		RetrySafe:          false,
		AcceptedByProvider: true,
	}
	addWriteFacts(details, facts)
	if message == "" {
		message = e.message
	}
	out := NewWithDetails(e.code, message, details)
	out.outcome = CloneWriteOutcomeFacts(&facts)
	return out
}

func addWriteFacts(details map[string]any, facts WriteOutcomeFacts) {
	details["outcome"] = string(facts.Outcome)
	details["write_may_have_landed"] = facts.WriteMayHaveLanded
	details["retry_safe"] = facts.RetrySafe
	details["accepted_by_provider"] = facts.AcceptedByProvider
	details["confirmed_after_write"] = facts.ConfirmedAfterWrite
	details["succeeded"] = facts.Succeeded
}

func validKnownPartialPlan(operation KnownPartialOperation, phases []KnownPartialPhase) bool {
	if len(phases) < 2 {
		return false
	}
	switch operation {
	case KnownGmailReplySend, KnownGmailForwardSend:
		return len(phases) == 2 && phases[0] == KnownPhaseDraftVerified && phases[1] == KnownPhaseSend
	case KnownGmailMove, KnownGmailRestore, KnownGmailJunk:
		return allKnownPartialPhases(phases, KnownPhaseModifyChunk) || knownPartialStartsWithUntrashThenModify(phases)
	case KnownGmailCopy, KnownGmailArchive, KnownGmailMarkRead, KnownGmailMarkUnread, KnownGmailFlagSet, KnownGmailFlagClear, KnownGmailCategoriesSet, KnownGmailCategoriesClear:
		return allKnownPartialPhases(phases, KnownPhaseModifyChunk)
	default:
		return false
	}
}

func allKnownPartialPhases(phases []KnownPartialPhase, expected KnownPartialPhase) bool {
	for _, phase := range phases {
		if phase != expected {
			return false
		}
	}
	return true
}

func knownPartialStartsWithUntrashThenModify(phases []KnownPartialPhase) bool {
	if len(phases) < 2 || phases[0] != KnownPhaseUntrash {
		return false
	}
	for _, phase := range phases[1:] {
		if phase != KnownPhaseModifyChunk {
			return false
		}
	}
	return true
}

func knownPartialPrefixValid(plan *knownPartialPlan, completedPrefixLength uint16) bool {
	if plan == nil || completedPrefixLength == 0 {
		return false
	}
	return int(completedPrefixLength) < len(plan.phases)
}

func knownPartialDetails(attempt ProviderWriteAttempt, plan KnownPartialPlan, completedPrefixLength uint16, failedTransmitted bool, observed KnownPartialObservedState, verifiedDraftID string) (*writeAttempt, map[string]any, error) {
	a, err := consumeAttempt(attempt, attemptTransmitted)
	if err != nil {
		return nil, nil, err
	}
	p, ok := plan.(*knownPartialPlan)
	if !ok || p == nil || !knownPartialPrefixValid(p, completedPrefixLength) || !knownPartialAttemptMatchesPlan(a, p) || !validKnownPartialObservedState(observed) {
		return nil, nil, NewSharedProviderContractInvalid()
	}
	completed := make([]uint16, 0, completedPrefixLength)
	for i := uint16(1); i <= completedPrefixLength; i++ {
		completed = append(completed, i)
	}
	failedIndex := completedPrefixLength + 1
	details := map[string]any{
		"planned_phases":                    knownPartialPhaseStrings(p.phases),
		"completed_phase_indices":           completed,
		"failed_phase_index":                failedIndex,
		"failed_phase":                      string(p.phases[int(failedIndex)-1]),
		"failed_phase_transmission_crossed": failedTransmitted,
		"observed_state":                    string(observed),
	}
	if verifiedDraftID != "" {
		details["verified_draft_id"] = verifiedDraftID
	}
	return a, details, nil
}

func knownPartialPhaseStrings(phases []KnownPartialPhase) []string {
	out := make([]string, len(phases))
	for i, phase := range phases {
		out[i] = string(phase)
	}
	return out
}

func validKnownPartialObservedState(observed KnownPartialObservedState) bool {
	switch observed {
	case KnownObservedLastVerified, KnownObservedIntendedFinal, KnownObservedOther, KnownObservedDiagnosticUnavailable:
		return true
	default:
		return false
	}
}

func knownPartialAttemptMatchesPlan(a *writeAttempt, plan *knownPartialPlan) bool {
	if a == nil || plan == nil || a.provider != ProviderGmail {
		return false
	}
	return a.operation == knownPartialInitialOperation(plan.operation)
}

func knownPartialInitialOperation(operation KnownPartialOperation) ProviderWriteOperation {
	switch operation {
	case KnownGmailReplySend:
		return WriteGmailReply
	case KnownGmailForwardSend:
		return WriteGmailForward
	case KnownGmailMove:
		return WriteGmailMove
	case KnownGmailCopy:
		return WriteGmailCopy
	case KnownGmailArchive:
		return WriteGmailArchive
	case KnownGmailDelete:
		return WriteGmailDelete
	case KnownGmailRestore:
		return WriteGmailRestore
	case KnownGmailJunk:
		return WriteGmailJunk
	case KnownGmailMarkRead:
		return WriteGmailMarkRead
	case KnownGmailMarkUnread:
		return WriteGmailMarkUnread
	case KnownGmailFlagSet:
		return WriteGmailFlagSet
	case KnownGmailFlagClear:
		return WriteGmailFlagClear
	case KnownGmailCategoriesSet:
		return WriteGmailCategoriesSet
	case KnownGmailCategoriesClear:
		return WriteGmailCategoriesClear
	default:
		return ""
	}
}

func knownPartialNextOperation(plan *knownPartialPlan, completedPrefixLength uint16) ProviderWriteOperation {
	if plan == nil {
		return ""
	}
	if (plan.operation == KnownGmailReplySend || plan.operation == KnownGmailForwardSend) && completedPrefixLength == 1 {
		if plan.operation == KnownGmailReplySend {
			return WriteGmailReply
		}
		return WriteGmailForward
	}
	return knownPartialInitialOperation(plan.operation)
}

type OutcomeUnknownReason string

const (
	OutcomeAcceptanceResponseLost        OutcomeUnknownReason = "acceptance_response_lost"
	OutcomeProviderResultMissing         OutcomeUnknownReason = "provider_result_missing"
	OutcomeProviderResultMalformed       OutcomeUnknownReason = "provider_result_malformed"
	OutcomePostWriteReadFailed           OutcomeUnknownReason = "post_write_read_failed"
	OutcomePostWriteMismatch             OutcomeUnknownReason = "post_write_mismatch"
	OutcomeProviderRejectionStateUnknown OutcomeUnknownReason = "provider_rejection_state_unknown"
)

func NewOutcomeUnknown(attempt ProviderWriteAttempt, reason OutcomeUnknownReason) error {
	if reason == OutcomeProviderRejectionStateUnknown || !validOutcomeUnknownReason(reason) {
		return NewSharedProviderContractInvalid()
	}
	a, err := consumeAttempt(attempt, attemptTransmitted)
	if err != nil {
		return err
	}
	accepted := reason != OutcomeAcceptanceResponseLost
	return newOutcomeError(a, string(reason), WriteOutcomeFacts{
		Outcome:            WriteOutcomeUnknown,
		WriteMayHaveLanded: true,
		RetrySafe:          false,
		AcceptedByProvider: accepted,
	})
}

func NewIMAPMoveRejectionStateUnknown(attempt ProviderWriteAttempt) error {
	a, err := consumeAttempt(attempt, attemptTransmitted)
	if err != nil {
		return err
	}
	if a.provider != ProviderIMAP && a.provider != ProviderFastmail {
		return NewSharedProviderContractInvalid()
	}
	switch a.operation {
	case WriteIMAPMove, WriteIMAPArchive, WriteIMAPDelete, WriteIMAPRestore, WriteIMAPJunk:
	default:
		return NewSharedProviderContractInvalid()
	}
	return newOutcomeError(a, string(OutcomeProviderRejectionStateUnknown), WriteOutcomeFacts{Outcome: WriteOutcomeUnknown, WriteMayHaveLanded: true})
}

func NewIMAPContradictoryMutationStateUnknown(attempt ProviderWriteAttempt) error {
	a, err := consumeAttempt(attempt, attemptTransmitted)
	if err != nil {
		return err
	}
	if a.provider != ProviderIMAP && a.provider != ProviderFastmail {
		return NewSharedProviderContractInvalid()
	}
	switch a.operation {
	case WriteIMAPCopy, WriteIMAPMove, WriteIMAPArchive, WriteIMAPDelete, WriteIMAPRestore, WriteIMAPJunk:
	default:
		return NewSharedProviderContractInvalid()
	}
	return newOutcomeError(a, string(OutcomeProviderRejectionStateUnknown), WriteOutcomeFacts{Outcome: WriteOutcomeUnknown, WriteMayHaveLanded: true})
}

func newOutcomeError(a *writeAttempt, reason string, facts WriteOutcomeFacts) error {
	if a == nil {
		return NewSharedProviderContractInvalid()
	}
	facts.RetrySafe = false
	facts.ConfirmedAfterWrite = false
	facts.Succeeded = false
	details := map[string]any{
		"account":   a.account,
		"provider":  string(a.provider),
		"operation": string(a.operation),
		"reason":    reason,
	}
	addWriteFacts(details, facts)
	out := NewWithDetails(CodeVerificationFailed, "the request went out, but maildo could not confirm the result", details)
	out.outcome = CloneWriteOutcomeFacts(&facts)
	return out
}

func validOutcomeUnknownReason(reason OutcomeUnknownReason) bool {
	switch reason {
	case OutcomeAcceptanceResponseLost, OutcomeProviderResultMissing, OutcomeProviderResultMalformed, OutcomePostWriteReadFailed, OutcomePostWriteMismatch, OutcomeProviderRejectionStateUnknown:
		return true
	default:
		return false
	}
}

func operationAllowedForProvider(provider ProviderName, operation ProviderWriteOperation) bool {
	switch provider {
	case ProviderOutlook:
		switch operation {
		case WriteOutlookDirectSend, WriteOutlookDraftSend, WriteOutlookCompose, WriteOutlookMessageChange:
			return true
		default:
			return false
		}
	case ProviderGmail:
		switch operation {
		case WriteGmailDraftCreate, WriteGmailDraftUpdate, WriteGmailDraftDelete, WriteGmailDraftSend, WriteGmailDirectSend,
			WriteGmailReply, WriteGmailForward, WriteGmailAttachmentAdd, WriteGmailAttachmentRemove,
			WriteGmailLabelCreate, WriteGmailLabelRename, WriteGmailLabelDelete, WriteGmailMove,
			WriteGmailCopy, WriteGmailArchive, WriteGmailDelete, WriteGmailRestore, WriteGmailJunk,
			WriteGmailMarkRead, WriteGmailMarkUnread, WriteGmailFlagSet, WriteGmailFlagClear,
			WriteGmailCategoriesSet, WriteGmailCategoriesClear:
			return true
		default:
			return false
		}
	case ProviderIMAP, ProviderFastmail:
		switch operation {
		case WriteSMTPDirectSend, WriteIMAPFolderCreate, WriteIMAPFolderRename, WriteIMAPFolderDelete,
			WriteIMAPCopy, WriteIMAPMove, WriteIMAPArchive, WriteIMAPDelete, WriteIMAPRestore,
			WriteIMAPJunk, WriteIMAPMarkRead, WriteIMAPMarkUnread, WriteIMAPFlagSet, WriteIMAPFlagClear,
			WriteIMAPDraftCreate, WriteIMAPDraftUpdate, WriteIMAPDraftDelete, WriteIMAPReply, WriteIMAPForward:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

type ReadLimitDimension string

const (
	ReadLimitLabels                                  ReadLimitDimension = "labels"
	ReadLimitPages                                   ReadLimitDimension = "pages"
	ReadLimitFolders                                 ReadLimitDimension = "folders"
	ReadLimitHydratedMessages                        ReadLimitDimension = "hydrated_messages"
	ReadLimitHeaders                                 ReadLimitDimension = "headers"
	ReadLimitHeaderBytes                             ReadLimitDimension = "header_bytes"
	ReadLimitAddresses                               ReadLimitDimension = "addresses"
	ReadLimitMIMEDepth                               ReadLimitDimension = "mime_depth"
	ReadLimitMIMEParts                               ReadLimitDimension = "mime_parts"
	ReadLimitMailboxes                               ReadLimitDimension = "mailboxes"
	ReadLimitHierarchyDepth                          ReadLimitDimension = "hierarchy_depth"
	ReadLimitMailboxNameBytes                        ReadLimitDimension = "mailbox_name_bytes"
	ReadLimitListStatusBytes                         ReadLimitDimension = "list_status_bytes"
	ReadLimitCandidatesPerMailbox                    ReadLimitDimension = "candidates_per_mailbox"
	ReadLimitCandidatesTotal                         ReadLimitDimension = "candidates_total"
	ReadLimitBodyBytes                               ReadLimitDimension = "body_bytes"
	ReadLimitThreadCandidates                        ReadLimitDimension = "thread_candidates"
	ReadLimitThreadRelationIDs                       ReadLimitDimension = "thread_relation_ids"
	ReadLimitThreadSearchBatches                     ReadLimitDimension = "thread_search_batches"
	ReadLimitThreadRelationTokens                    ReadLimitDimension = "thread_relation_tokens"
	ReadLimitComposeVerificationResponseBytes        ReadLimitDimension = "compose_verification_response_bytes"
	ReadLimitComposeVerificationEncodedBytes         ReadLimitDimension = "compose_verification_encoded_bytes"
	ReadLimitComposeVerificationRawBytes             ReadLimitDimension = "compose_verification_raw_bytes"
	ReadLimitComposeVerificationDiscardedHeaders     ReadLimitDimension = "compose_verification_discarded_headers"
	ReadLimitComposeVerificationDiscardedHeaderBytes ReadLimitDimension = "compose_verification_discarded_header_bytes"
	ReadLimitInvalidJSON                             ReadLimitDimension = "invalid_json"
)

type ResourceLimitDimension string

const ResourceLimitLabels ResourceLimitDimension = "labels"

type MessageSizeLimitSource string

const (
	MessageSizeComposeSource   MessageSizeLimitSource = "compose_source"
	MessageSizeOutboundEncoded MessageSizeLimitSource = "outbound_encoded"
	MessageSizeSMTPAdvertised  MessageSizeLimitSource = "smtp_size"
)

type GmailAttachmentSizeDimension string

const (
	GmailAttachmentSizeEncoded GmailAttachmentSizeDimension = "encoded_bytes"
	GmailAttachmentSizeDecoded GmailAttachmentSizeDimension = "decoded_bytes"
)

type IMAPAttachmentSizeDimension string

const (
	IMAPAttachmentSizeEncoded IMAPAttachmentSizeDimension = "encoded_bytes"
	IMAPAttachmentSizeDecoded IMAPAttachmentSizeDimension = "decoded_bytes"
)

type IMAPMessageNotFoundReason string

const (
	IMAPMessageNotFoundMissing            IMAPMessageNotFoundReason = "missing"
	IMAPMessageNotFoundUIDValidityChanged IMAPMessageNotFoundReason = "uidvalidity_changed"
)

type limitRow struct {
	limit       uint64
	remediation string
}

func NewReadLimitExceeded(account string, provider ProviderName, dimension ReadLimitDimension, observedAtLeast uint64) error {
	row, ok := readLimitRows()[limitKey{provider: provider, dimension: dimension}]
	if !safeAccount(account) || !ok || dimension == ReadLimitInvalidJSON || observedAtLeast <= row.limit {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeReadLimitExceeded, "this is more than maildo reads in one command", map[string]any{
		"account":           account,
		"provider":          string(provider),
		"dimension":         string(dimension),
		"limit":             row.limit,
		"observed_at_least": observedAtLeast,
		"remediation":       row.remediation,
	})
}

func NewGmailAttachmentStructureReadLimitExceeded(account string, limit uint64, observedAtLeast uint64) error {
	switch limit {
	case uint64(limits.GmailMaxAttachmentStructureResponseBytes), uint64(limits.GmailMaxJSONResponseBytes), uint64(limits.GmailMaxAttachmentEncodedBytes):
	default:
		return NewSharedProviderContractInvalid()
	}
	if !safeAccount(account) || observedAtLeast <= limit {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeReadLimitExceeded, "this is more than maildo reads in one command", map[string]any{
		"account":           account,
		"provider":          string(ProviderGmail),
		"dimension":         string(ReadLimitInvalidJSON),
		"limit":             limit,
		"observed_at_least": observedAtLeast,
		"remediation":       "open this message in Gmail instead",
	})
}

func NewGmailAttachmentStaleSnapshot(account string, snapshotMessageID string, attachmentID string) error {
	if !safeAccount(account) || !safeDetailScalar(snapshotMessageID) || !safeDetailScalar(attachmentID) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeAttachmentNotFound, "attachment not found", map[string]any{
		"account":             account,
		"provider":            string(ProviderGmail),
		"snapshot_message_id": snapshotMessageID,
		"selector_kind":       "mime_part_id",
		"attachment_id":       attachmentID,
		"reason":              "stale_snapshot_part",
	})
}

func NewGmailAttachmentTooLarge(account string, snapshotMessageID string, attachmentID string, dimension GmailAttachmentSizeDimension, limit uint64, observedAtLeast uint64) error {
	const maxDecodedAttachment = uint64(25 * 1024 * 1024)
	validPair := false
	switch dimension {
	case GmailAttachmentSizeEncoded:
		validPair = limit == uint64(limits.GmailMaxAttachmentEncodedBytes)
	case GmailAttachmentSizeDecoded:
		validPair = limit == maxDecodedAttachment
	}
	if !safeAccount(account) || !safeDetailScalar(snapshotMessageID) || !safeDetailScalar(attachmentID) || !validPair || observedAtLeast <= limit {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeAttachmentTooLarge, "the attachment is larger than maildo can download", map[string]any{
		"account":             account,
		"provider":            string(ProviderGmail),
		"snapshot_message_id": snapshotMessageID,
		"selector_kind":       "mime_part_id",
		"attachment_id":       attachmentID,
		"dimension":           string(dimension),
		"limit":               limit,
		"observed_at_least":   observedAtLeast,
	})
}

func NewRequiredCIDAttachment() error {
	return NewWithDetails(CodeInvalidComposeInput, "the original message shows some of its attachments inside the text, so the forward has to include them", map[string]any{
		"field":  "include_attachments",
		"reason": "required_cid_attachment",
	})
}

func NewIMAPAttachmentTooLarge(account string, provider ProviderName, messageID string, attachmentID string, dimension IMAPAttachmentSizeDimension, limit uint64, observedAtLeast uint64) error {
	const maxDecodedAttachment = uint64(25 * 1024 * 1024)
	validPair := false
	switch dimension {
	case IMAPAttachmentSizeEncoded:
		validPair = limit == uint64(limits.IMAPMaxEncodedAttachmentBytes)
	case IMAPAttachmentSizeDecoded:
		validPair = limit == maxDecodedAttachment
	}
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !safeDetailScalar(messageID) || !safeDetailScalar(attachmentID) || !validPair || observedAtLeast <= limit {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeAttachmentTooLarge, "attachment is too large to download", map[string]any{
		"account":           account,
		"provider":          string(provider),
		"message_id":        messageID,
		"attachment_id":     attachmentID,
		"dimension":         string(dimension),
		"limit":             limit,
		"observed_at_least": observedAtLeast,
	})
}

func NewIMAPMessageNotFound(account string, provider ProviderName, selectorKind string, selectorValue string, reason IMAPMessageNotFoundReason) error {
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !safeDetailScalar(selectorKind) || !safeDetailScalar(selectorValue) || !validIMAPMessageNotFoundReason(reason) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeMessageNotFound, "message not found", map[string]any{
		"account":        account,
		"provider":       string(provider),
		"selector_kind":  selectorKind,
		"selector_value": selectorValue,
		"reason":         string(reason),
	})
}

func NewIMAPThreadNotFound(account string, provider ProviderName, selectorKind string, selectorValue string) error {
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !safeDetailScalar(selectorKind) || !safeDetailScalar(selectorValue) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeThreadNotFound, "thread not found", map[string]any{
		"account":        account,
		"provider":       string(provider),
		"selector_kind":  selectorKind,
		"selector_value": selectorValue,
	})
}

func NewIMAPAmbiguousInternetMessageID(account string, provider ProviderName, internetMessageID string, count uint64) error {
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !safeDetailScalar(internetMessageID) || count < 2 {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeAmbiguousMessageSelector, "more than one message has this Message-ID", map[string]any{
		"account":             account,
		"provider":            string(provider),
		"selector_kind":       "internet_id",
		"internet_message_id": internetMessageID,
		"matches":             count,
	})
}

func NewIMAPSourceMailboxNotSelectable(account string, provider ProviderName, capability string, folderID string) error {
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !safeDetailScalar(capability) || !safeDetailScalar(folderID) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeUnsupportedCapability, "the folder cannot be opened, because the server marks it as a folder that holds only other folders", map[string]any{
		"account":    account,
		"provider":   string(provider),
		"capability": capability,
		"folder_id":  folderID,
		"reason":     "source_mailbox_not_selectable",
	})
}

func NewResourceLimitExceeded(account string, provider ProviderName, dimension ResourceLimitDimension, prospectiveCount uint64) error {
	if !safeAccount(account) || provider != ProviderGmail || dimension != ResourceLimitLabels || prospectiveCount <= limits.GmailMaxLabels {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeResourceLimitExceeded, "Gmail allows at most 5,000 labels", map[string]any{
		"account":           account,
		"provider":          string(provider),
		"dimension":         string(dimension),
		"limit":             uint64(limits.GmailMaxLabels),
		"prospective_count": prospectiveCount,
		"remediation":       "delete Gmail labels you no longer need",
	})
}

// NewTooManyAddresses reports a message with more addresses than maildo
// puts in one message.
func NewTooManyAddresses(limit uint64) error {
	return NewWithDetails(CodeInvalidComposeInput,
		fmt.Sprintf("this message has too many addresses; maildo puts at most %d in one message, sender and recipients together", limit),
		map[string]any{"limit": limit, "hint": "send it as more than one message, each to a part of the recipients"})
}

func NewMessageTooLarge(account string, provider ProviderName, source MessageSizeLimitSource, limit uint64, observedAtLeast uint64) error {
	remediation := ""
	const outboundLimit = uint64(25 * 1024 * 1024)
	if !safeAccount(account) {
		return NewSharedProviderContractInvalid()
	}
	switch source {
	case MessageSizeComposeSource:
		if provider != ProviderGmail && provider != ProviderIMAP && provider != ProviderFastmail {
			return NewSharedProviderContractInvalid()
		}
		if limit != outboundLimit || observedAtLeast != outboundLimit+1 {
			return NewSharedProviderContractInvalid()
		}
		remediation = "make the message shorter"
	case MessageSizeOutboundEncoded:
		if provider != ProviderGmail && provider != ProviderIMAP && provider != ProviderFastmail {
			return NewSharedProviderContractInvalid()
		}
		if limit != outboundLimit || observedAtLeast != outboundLimit+1 {
			return NewSharedProviderContractInvalid()
		}
		remediation = "make the message smaller, for example with fewer or smaller attachments"
	case MessageSizeSMTPAdvertised:
		if provider != ProviderIMAP && provider != ProviderFastmail {
			return NewSharedProviderContractInvalid()
		}
		if limit == 0 || limit > outboundLimit || observedAtLeast <= limit || observedAtLeast > outboundLimit {
			return NewSharedProviderContractInvalid()
		}
		remediation = "make the message smaller, so it fits the size limit the server announced"
	default:
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeMessageTooLarge, "the message is too large", map[string]any{
		"account":           account,
		"provider":          string(provider),
		"limit_source":      string(source),
		"limit":             limit,
		"observed_at_least": observedAtLeast,
		"remediation":       remediation,
	})
}

type limitKey struct {
	provider  ProviderName
	dimension ReadLimitDimension
}

func readLimitRows() map[limitKey]limitRow {
	gmailComplex := "open this message in Gmail instead"
	imapComplex := "open this message in your usual mail program instead"
	imapNarrow := "search one folder with --folder, or add a date such as newer:30d"
	gmailCompose := "use a smaller message, or use Gmail directly"
	imapFolders := "delete folders you no longer need, or use your usual mail program for this account"
	imapThread := "read the messages one at a time with maildo read instead"
	return map[limitKey]limitRow{
		{ProviderOutlook, ReadLimitPages}:                                 {limits.OutlookMaxPages, "narrow the search, for example with --folder or a date such as newer:30d"},
		{ProviderOutlook, ReadLimitFolders}:                               {limits.OutlookMaxFolders, "delete folders you no longer need, or use Outlook directly"},
		{ProviderGmail, ReadLimitLabels}:                                  {limits.GmailMaxLabels, "delete Gmail labels until no more than 5,000 are left, or use Gmail directly"},
		{ProviderGmail, ReadLimitPages}:                                   {limits.GmailMaxPages, "narrow the search, for example with --folder or a date such as newer:30d"},
		{ProviderGmail, ReadLimitHydratedMessages}:                        {limits.GmailMaxHydratedMessages, "narrow the search, for example with --folder or a date such as newer:30d"},
		{ProviderGmail, ReadLimitHeaders}:                                 {limits.GmailMaxHeaderCount, gmailComplex},
		{ProviderGmail, ReadLimitHeaderBytes}:                             {uint64(limits.GmailMaxHeaderBytes), gmailComplex},
		{ProviderGmail, ReadLimitAddresses}:                               {limits.GmailMaxAddressCount, gmailComplex},
		{ProviderGmail, ReadLimitMIMEDepth}:                               {limits.GmailMaxMIMEDepth, gmailComplex},
		{ProviderGmail, ReadLimitMIMEParts}:                               {limits.GmailMaxMIMEParts, gmailComplex},
		{ProviderGmail, ReadLimitBodyBytes}:                               {1 * 1024 * 1024, gmailComplex},
		{ProviderGmail, ReadLimitComposeVerificationResponseBytes}:        {uint64(limits.GmailMaxComposeVerificationResponseBytes), gmailCompose},
		{ProviderGmail, ReadLimitComposeVerificationEncodedBytes}:         {uint64(limits.GmailMaxComposeVerificationEncodedBytes), gmailCompose},
		{ProviderGmail, ReadLimitComposeVerificationRawBytes}:             {uint64(limits.GmailMaxComposeVerificationDecodedRFCBytes), gmailCompose},
		{ProviderGmail, ReadLimitComposeVerificationDiscardedHeaders}:     {limits.GmailMaxComposeVerificationDiscardedRootHeaderCount, gmailCompose},
		{ProviderGmail, ReadLimitComposeVerificationDiscardedHeaderBytes}: {uint64(limits.GmailMaxComposeVerificationDiscardedRootHeaderBytes), gmailCompose},
		{ProviderIMAP, ReadLimitMailboxes}:                                {limits.IMAPMaxMailboxes, imapFolders},
		{ProviderFastmail, ReadLimitMailboxes}:                            {limits.IMAPMaxMailboxes, imapFolders},
		{ProviderIMAP, ReadLimitHierarchyDepth}:                           {limits.IMAPMaxHierarchyDepth, imapFolders},
		{ProviderFastmail, ReadLimitHierarchyDepth}:                       {limits.IMAPMaxHierarchyDepth, imapFolders},
		{ProviderIMAP, ReadLimitMailboxNameBytes}:                         {uint64(limits.IMAPMaxMailboxNameBytes), "rename folders that have very long names, or use your usual mail program for this account"},
		{ProviderFastmail, ReadLimitMailboxNameBytes}:                     {uint64(limits.IMAPMaxMailboxNameBytes), "rename folders that have very long names, or use your usual mail program for this account"},
		{ProviderIMAP, ReadLimitListStatusBytes}:                          {uint64(limits.IMAPMaxListStatusBytes), imapFolders},
		{ProviderFastmail, ReadLimitListStatusBytes}:                      {uint64(limits.IMAPMaxListStatusBytes), imapFolders},
		{ProviderIMAP, ReadLimitCandidatesPerMailbox}:                     {limits.IMAPMaxCandidatesPerMailbox, imapNarrow},
		{ProviderFastmail, ReadLimitCandidatesPerMailbox}:                 {limits.IMAPMaxCandidatesPerMailbox, imapNarrow},
		{ProviderIMAP, ReadLimitCandidatesTotal}:                          {limits.IMAPMaxCandidatesTotal, imapNarrow},
		{ProviderFastmail, ReadLimitCandidatesTotal}:                      {limits.IMAPMaxCandidatesTotal, imapNarrow},
		{ProviderIMAP, ReadLimitHeaders}:                                  {limits.IMAPMaxHeaderCount, imapComplex},
		{ProviderFastmail, ReadLimitHeaders}:                              {limits.IMAPMaxHeaderCount, imapComplex},
		{ProviderIMAP, ReadLimitHeaderBytes}:                              {uint64(limits.IMAPMaxHeaderBytes), imapComplex},
		{ProviderFastmail, ReadLimitHeaderBytes}:                          {uint64(limits.IMAPMaxHeaderBytes), imapComplex},
		{ProviderIMAP, ReadLimitAddresses}:                                {limits.IMAPMaxAddressCount, imapComplex},
		{ProviderFastmail, ReadLimitAddresses}:                            {limits.IMAPMaxAddressCount, imapComplex},
		{ProviderIMAP, ReadLimitMIMEDepth}:                                {limits.IMAPMaxMIMEDepth, imapComplex},
		{ProviderFastmail, ReadLimitMIMEDepth}:                            {limits.IMAPMaxMIMEDepth, imapComplex},
		{ProviderIMAP, ReadLimitMIMEParts}:                                {limits.IMAPMaxMIMEParts, imapComplex},
		{ProviderFastmail, ReadLimitMIMEParts}:                            {limits.IMAPMaxMIMEParts, imapComplex},
		{ProviderIMAP, ReadLimitBodyBytes}:                                {1 * 1024 * 1024, imapComplex},
		{ProviderFastmail, ReadLimitBodyBytes}:                            {1 * 1024 * 1024, imapComplex},
		{ProviderIMAP, ReadLimitThreadCandidates}:                         {limits.IMAPMaxThreadCandidates, imapThread},
		{ProviderFastmail, ReadLimitThreadCandidates}:                     {limits.IMAPMaxThreadCandidates, imapThread},
		{ProviderIMAP, ReadLimitThreadRelationIDs}:                        {limits.IMAPMaxThreadRelationIDs, imapThread},
		{ProviderFastmail, ReadLimitThreadRelationIDs}:                    {limits.IMAPMaxThreadRelationIDs, imapThread},
		{ProviderIMAP, ReadLimitThreadSearchBatches}:                      {limits.IMAPMaxThreadSearchBatches, imapThread},
		{ProviderFastmail, ReadLimitThreadSearchBatches}:                  {limits.IMAPMaxThreadSearchBatches, imapThread},
		{ProviderIMAP, ReadLimitThreadRelationTokens}:                     {limits.IMAPMaxRelationTokensPerMessage, imapThread},
		{ProviderFastmail, ReadLimitThreadRelationTokens}:                 {limits.IMAPMaxRelationTokensPerMessage, imapThread},
	}
}

type IMAPDefaultRootCondition string

const (
	IMAPDefaultRootMissing           IMAPDefaultRootCondition = "missing"
	IMAPDefaultRootAmbiguous         IMAPDefaultRootCondition = "ambiguous"
	IMAPDefaultRootDelimiterInvalid  IMAPDefaultRootCondition = "delimiter_invalid"
	IMAPDefaultRootDelimiterConflict IMAPDefaultRootCondition = "delimiter_conflict"
	IMAPDefaultRootSnapshotConflict  IMAPDefaultRootCondition = "snapshot_conflict"
)

func NewIMAPDefaultRootInvalid(account string, provider ProviderName, condition IMAPDefaultRootCondition) error {
	if !safeAccount(account) || (provider != ProviderIMAP && provider != ProviderFastmail) || !validIMAPDefaultRootCondition(condition) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeProviderDataError, "provider returned invalid protocol data", map[string]any{
		"account":        account,
		"provider":       string(provider),
		"operation":      "folder_create",
		"reason":         "invalid_protocol",
		"root_condition": string(condition),
	})
}

func validIMAPDefaultRootCondition(condition IMAPDefaultRootCondition) bool {
	switch condition {
	case IMAPDefaultRootMissing, IMAPDefaultRootAmbiguous, IMAPDefaultRootDelimiterInvalid, IMAPDefaultRootDelimiterConflict, IMAPDefaultRootSnapshotConflict:
		return true
	default:
		return false
	}
}

type PreCommitOAuthFailureReason string

const (
	PreCommitOAuthAuthorizationDenied PreCommitOAuthFailureReason = "authorization_denied"
	PreCommitOAuthCallbackInvalid     PreCommitOAuthFailureReason = "callback_invalid"
	PreCommitOAuthCallbackTimeout     PreCommitOAuthFailureReason = "callback_timeout"
	PreCommitOAuthLoopbackUnavailable PreCommitOAuthFailureReason = "loopback_unavailable"
	PreCommitOAuthBrowserUnavailable  PreCommitOAuthFailureReason = "browser_unavailable"
)

func NewPreCommitCredentialRejected(account string, provider ProviderName) error {
	if !safeAccount(account) || (provider != ProviderGmail && provider != ProviderIMAP && provider != ProviderFastmail) {
		return NewSharedProviderContractInvalid()
	}
	return newPreCommitIdentityFailure(account, provider, "credential_rejected")
}

func NewPreCommitScopeMissing(account string, provider ProviderName) error {
	if !safeAccount(account) || provider != ProviderGmail {
		return NewSharedProviderContractInvalid()
	}
	return newPreCommitIdentityFailure(account, provider, "scope_missing")
}

func NewPreCommitIdentityMismatch(account string, provider ProviderName) error {
	if !safeAccount(account) || (provider != ProviderGmail && provider != ProviderIMAP && provider != ProviderFastmail) {
		return NewSharedProviderContractInvalid()
	}
	return newPreCommitIdentityFailure(account, provider, "identity_mismatch")
}

func NewPreCommitOAuthFailure(account string, reason PreCommitOAuthFailureReason) error {
	if !safeAccount(account) || !validPreCommitOAuthFailureReason(reason) {
		return NewSharedProviderContractInvalid()
	}
	return newPreCommitIdentityFailure(account, ProviderGmail, string(reason))
}

func newPreCommitIdentityFailure(account string, provider ProviderName, reason string) error {
	return NewWithDetails(CodeProviderIdentityUnavailable, "provider identity is unavailable", map[string]any{
		"account":   account,
		"provider":  string(provider),
		"operation": "login",
		"reason":    reason,
	})
}

func validPreCommitOAuthFailureReason(reason PreCommitOAuthFailureReason) bool {
	switch reason {
	case PreCommitOAuthAuthorizationDenied, PreCommitOAuthCallbackInvalid, PreCommitOAuthCallbackTimeout, PreCommitOAuthLoopbackUnavailable, PreCommitOAuthBrowserUnavailable:
		return true
	default:
		return false
	}
}

type ProviderProtocol string

const (
	ProviderProtocolIMAP ProviderProtocol = "imap"
	ProviderProtocolSMTP ProviderProtocol = "smtp"
)

type ProviderOperation string

const (
	ProviderOperationIMAPAuth            ProviderOperation = "imap_auth"
	ProviderOperationIMAPFoldersRead     ProviderOperation = "imap_folders_read"
	ProviderOperationIMAPMessagesSearch  ProviderOperation = "imap_messages_search"
	ProviderOperationIMAPMessagesList    ProviderOperation = "imap_messages_list"
	ProviderOperationIMAPMessagesRead    ProviderOperation = "imap_messages_read"
	ProviderOperationIMAPThreadsRead     ProviderOperation = "imap_threads_read"
	ProviderOperationIMAPAttachmentsRead ProviderOperation = "imap_attachments_read"
	ProviderOperationSMTPAuth            ProviderOperation = "smtp_auth"
	ProviderOperationSMTPDirectSend      ProviderOperation = "smtp_direct_send"
)

type ProviderAuthRequirement string

const ProviderAuthRequirementPLAIN ProviderAuthRequirement = "PLAIN"

type ProviderCapabilityReason string

const (
	ProviderCapabilityReasonStaged             ProviderCapabilityReason = "capability_staged"
	ProviderCapabilityReasonMappingUnsupported ProviderCapabilityReason = "provider_mapping_unsupported"
	ProviderCapabilityReasonIMAPVersion        ProviderCapabilityReason = "imap_version_unsupported"
	ProviderCapabilityReasonSMTPUTF8Required   ProviderCapabilityReason = "smtputf8_required"
)

type ProviderRequirement string

const (
	ProviderRequirementIMAP4rev1OrRev2 ProviderRequirement = "IMAP4rev1_or_IMAP4rev2"
	ProviderRequirementSMTPUTF8        ProviderRequirement = "SMTPUTF8"
)

type ProviderConnectionReason string

const (
	ProviderConnectionReasonDNS                   ProviderConnectionReason = "dns"
	ProviderConnectionReasonDial                  ProviderConnectionReason = "dial"
	ProviderConnectionReasonTimeout               ProviderConnectionReason = "timeout"
	ProviderConnectionReasonTLSRequired           ProviderConnectionReason = "tls_required"
	ProviderConnectionReasonTLSVerificationFailed ProviderConnectionReason = "tls_verification_failed"
	ProviderConnectionReasonTLSNotOffered         ProviderConnectionReason = "tls_not_offered"
	ProviderConnectionReasonTLSHandshakeFailed    ProviderConnectionReason = "tls_handshake_failed"
	ProviderConnectionReasonRefused               ProviderConnectionReason = "refused"
	ProviderConnectionReasonProtocolGreeting      ProviderConnectionReason = "protocol_greeting"
	ProviderConnectionReasonConnectionLost        ProviderConnectionReason = "connection_lost"
	ProviderConnectionReasonRecipientTemporary    ProviderConnectionReason = "recipient_temporarily_rejected"
)

type ProviderForbiddenReason string

const (
	ProviderForbiddenReasonPolicy  ProviderForbiddenReason = "provider_policy"
	ProviderForbiddenReasonUnknown ProviderForbiddenReason = "unknown_forbidden"
)

type ProviderDataReason string

const (
	ProviderDataReasonInvalidProtocol        ProviderDataReason = "invalid_protocol"
	ProviderDataReasonLineLimitExceeded      ProviderDataReason = "line_limit_exceeded"
	ProviderDataReasonLiteralLimitExceeded   ProviderDataReason = "literal_limit_exceeded"
	ProviderDataReasonInvalidMailboxEncoding ProviderDataReason = "invalid_mailbox_encoding"
	ProviderDataReasonInvalidMIME            ProviderDataReason = "invalid_mime"
	ProviderDataReasonInvalidHeader          ProviderDataReason = "invalid_header"
	ProviderDataReasonThreadReferenceCycle   ProviderDataReason = "thread_reference_cycle"
)

type ProviderFailureContext interface {
	providerFailureContext()
}

type providerFailureContext struct {
	account   string
	provider  ProviderName
	protocol  ProviderProtocol
	operation ProviderOperation
}

func (*providerFailureContext) providerFailureContext() {}

func NewProviderFailureContext(account string, provider ProviderName, protocol ProviderProtocol, operation ProviderOperation) (ProviderFailureContext, error) {
	if !safeAccount(account) || !validProviderProtocolOperation(provider, protocol, operation) {
		return nil, NewSharedProviderContractInvalid()
	}
	return &providerFailureContext{account: account, provider: provider, protocol: protocol, operation: operation}, nil
}

func NewStoredCredentialRejected(ctx ProviderFailureContext) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeAuthRepairRequired, "stored credential was rejected by provider", providerFailureDetails(c, "credential_rejected"))
}

func NewUnsupportedAuthentication(ctx ProviderFailureContext, requirement ProviderAuthRequirement) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil || requirement != ProviderAuthRequirementPLAIN {
		return NewSharedProviderContractInvalid()
	}
	details := providerFailureDetails(c, "unsupported_authentication")
	details["authentication_requirement"] = string(requirement)
	return NewWithDetails(CodeAuthRepairRequired, "provider authentication is unsupported", details)
}

func NewProviderCapabilityUnavailable(ctx ProviderFailureContext, reason ProviderCapabilityReason, requirement ProviderRequirement) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil || !validProviderCapabilityRequirement(c.protocol, reason, requirement) {
		return NewSharedProviderContractInvalid()
	}
	details := providerFailureDetails(c, string(reason))
	details["requirement"] = string(requirement)
	return NewWithDetails(CodeUnsupportedCapability, "provider capability is unavailable", details)
}

func NewProviderConnectionFailure(ctx ProviderFailureContext, reason ProviderConnectionReason) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil || !validProviderConnectionReason(reason) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeProviderConnectionError, "provider connection failed", providerFailureDetails(c, string(reason)))
}

func NewProviderForbiddenFailure(ctx ProviderFailureContext, reason ProviderForbiddenReason) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil || !validProviderForbiddenReason(reason) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeProviderForbidden, "provider rejected the operation", providerFailureDetails(c, string(reason)))
}

func NewProviderDataFailure(ctx ProviderFailureContext, reason ProviderDataReason) error {
	c, ok := ctx.(*providerFailureContext)
	if !ok || c == nil || !validProviderDataReason(c.protocol, reason) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeProviderDataError, "provider returned invalid protocol data", providerFailureDetails(c, string(reason)))
}

func NewSMTPRecipientRejected(account string, provider ProviderName, rejectedIndex, acceptedCount, totalCount int) error {
	if !validSMTPDirectProvider(account, provider) || !validSMTPRecipientCounts(rejectedIndex, acceptedCount, totalCount) {
		return NewSharedProviderContractInvalid()
	}
	return NewWithDetails(CodeRecipientRejected, "recipient was rejected by provider", smtpRecipientDetails(account, provider, rejectedIndex, acceptedCount, totalCount))
}

func NewSMTPRecipientTemporarilyRejected(account string, provider ProviderName, rejectedIndex, acceptedCount, totalCount int) error {
	if !validSMTPDirectProvider(account, provider) || !validSMTPRecipientCounts(rejectedIndex, acceptedCount, totalCount) {
		return NewSharedProviderContractInvalid()
	}
	details := providerFailureDetails(&providerFailureContext{
		account:   account,
		provider:  provider,
		protocol:  ProviderProtocolSMTP,
		operation: ProviderOperationSMTPDirectSend,
	}, string(ProviderConnectionReasonRecipientTemporary))
	for key, value := range smtpRecipientDetails(account, provider, rejectedIndex, acceptedCount, totalCount) {
		details[key] = value
	}
	return NewWithDetails(CodeProviderConnectionError, "provider connection failed", details)
}

type SMTPReplyPhase string

const (
	SMTPReplyMAIL        SMTPReplyPhase = "mail"
	SMTPReplyRCPT        SMTPReplyPhase = "rcpt"
	SMTPReplyDATACommand SMTPReplyPhase = "data_command"
	SMTPReplyFinal       SMTPReplyPhase = "final"
)

type SMTPReplyClass string

const (
	SMTPReplyConnectionLost   SMTPReplyClass = "connection_lost"
	SMTPReplyProviderPolicy   SMTPReplyClass = "provider_policy"
	SMTPReplyInvalidProtocol  SMTPReplyClass = "invalid_protocol"
	SMTPReplyUnknownForbidden SMTPReplyClass = "unknown_forbidden"
)

func NewSMTPClassifiedReply(account string, provider ProviderName, phase SMTPReplyPhase, class SMTPReplyClass) error {
	if !validSMTPDirectProvider(account, provider) || !validSMTPReplyPhase(phase) || !validSMTPReplyClass(class) {
		return NewSharedProviderContractInvalid()
	}
	ctx := &providerFailureContext{
		account:   account,
		provider:  provider,
		protocol:  ProviderProtocolSMTP,
		operation: ProviderOperationSMTPDirectSend,
	}
	var cause error
	switch class {
	case SMTPReplyConnectionLost:
		cause = NewProviderConnectionFailure(ctx, ProviderConnectionReasonConnectionLost)
	case SMTPReplyProviderPolicy:
		cause = NewProviderForbiddenFailure(ctx, ProviderForbiddenReasonPolicy)
	case SMTPReplyInvalidProtocol:
		cause = NewProviderDataFailure(ctx, ProviderDataReasonInvalidProtocol)
	case SMTPReplyUnknownForbidden:
		cause = NewProviderForbiddenFailure(ctx, ProviderForbiddenReasonUnknown)
	default:
		return NewSharedProviderContractInvalid()
	}
	switch phase {
	case SMTPReplyDATACommand:
		return NewPreTransmissionDefinitiveProviderRejectionError(account, provider, WriteSMTPDirectSend, cause)
	case SMTPReplyFinal:
		return NewDefinitiveProviderRejectionError(account, provider, WriteSMTPDirectSend, cause)
	default:
		return cause
	}
}

func providerFailureDetails(c *providerFailureContext, reason string) map[string]any {
	return map[string]any{
		"account":   c.account,
		"provider":  string(c.provider),
		"protocol":  string(c.protocol),
		"operation": string(c.operation),
		"reason":    reason,
	}
}

func smtpRecipientDetails(account string, provider ProviderName, rejectedIndex, acceptedCount, totalCount int) map[string]any {
	return map[string]any{
		"account":        account,
		"provider":       string(provider),
		"rejected_index": rejectedIndex,
		"accepted_count": acceptedCount,
		"total_count":    totalCount,
	}
}

func validSMTPRecipientCounts(rejectedIndex, acceptedCount, totalCount int) bool {
	return rejectedIndex >= 1 && acceptedCount == rejectedIndex-1 && totalCount >= rejectedIndex
}

func validSMTPDirectProvider(account string, provider ProviderName) bool {
	return safeAccount(account) && (provider == ProviderIMAP || provider == ProviderFastmail)
}

func validSMTPReplyPhase(phase SMTPReplyPhase) bool {
	switch phase {
	case SMTPReplyMAIL, SMTPReplyRCPT, SMTPReplyDATACommand, SMTPReplyFinal:
		return true
	default:
		return false
	}
}

func validSMTPReplyClass(class SMTPReplyClass) bool {
	switch class {
	case SMTPReplyConnectionLost, SMTPReplyProviderPolicy, SMTPReplyInvalidProtocol, SMTPReplyUnknownForbidden:
		return true
	default:
		return false
	}
}

func validProviderProtocolOperation(provider ProviderName, protocol ProviderProtocol, operation ProviderOperation) bool {
	if provider != ProviderIMAP && provider != ProviderFastmail {
		return false
	}
	switch protocol {
	case ProviderProtocolIMAP:
		switch operation {
		case ProviderOperationIMAPAuth, ProviderOperationIMAPFoldersRead, ProviderOperationIMAPMessagesSearch,
			ProviderOperationIMAPMessagesList, ProviderOperationIMAPMessagesRead, ProviderOperationIMAPThreadsRead,
			ProviderOperationIMAPAttachmentsRead:
			return true
		default:
			return false
		}
	case ProviderProtocolSMTP:
		return operation == ProviderOperationSMTPAuth || operation == ProviderOperationSMTPDirectSend
	default:
		return false
	}
}

func validProviderCapabilityRequirement(protocol ProviderProtocol, reason ProviderCapabilityReason, requirement ProviderRequirement) bool {
	switch {
	case protocol == ProviderProtocolIMAP && reason == ProviderCapabilityReasonIMAPVersion && requirement == ProviderRequirementIMAP4rev1OrRev2:
		return true
	case protocol == ProviderProtocolSMTP && reason == ProviderCapabilityReasonSMTPUTF8Required && requirement == ProviderRequirementSMTPUTF8:
		return true
	default:
		return false
	}
}

func validProviderConnectionReason(reason ProviderConnectionReason) bool {
	switch reason {
	case ProviderConnectionReasonDNS, ProviderConnectionReasonDial, ProviderConnectionReasonTimeout, ProviderConnectionReasonTLSRequired,
		ProviderConnectionReasonTLSVerificationFailed, ProviderConnectionReasonTLSNotOffered, ProviderConnectionReasonTLSHandshakeFailed,
		ProviderConnectionReasonRefused, ProviderConnectionReasonProtocolGreeting, ProviderConnectionReasonConnectionLost,
		ProviderConnectionReasonRecipientTemporary:
		return true
	default:
		return false
	}
}

func validProviderForbiddenReason(reason ProviderForbiddenReason) bool {
	switch reason {
	case ProviderForbiddenReasonPolicy, ProviderForbiddenReasonUnknown:
		return true
	default:
		return false
	}
}

func validProviderDataReason(protocol ProviderProtocol, reason ProviderDataReason) bool {
	switch reason {
	case ProviderDataReasonInvalidProtocol, ProviderDataReasonLineLimitExceeded:
		return true
	case ProviderDataReasonLiteralLimitExceeded:
		return protocol == ProviderProtocolIMAP
	case ProviderDataReasonInvalidMailboxEncoding, ProviderDataReasonInvalidMIME, ProviderDataReasonInvalidHeader, ProviderDataReasonThreadReferenceCycle:
		return protocol == ProviderProtocolIMAP
	default:
		return false
	}
}

func validIMAPMessageNotFoundReason(reason IMAPMessageNotFoundReason) bool {
	switch reason {
	case IMAPMessageNotFoundMissing, IMAPMessageNotFoundUIDValidityChanged:
		return true
	default:
		return false
	}
}

func validProviderName(provider ProviderName) bool {
	switch provider {
	case ProviderOutlook, ProviderGmail, ProviderIMAP, ProviderFastmail:
		return true
	default:
		return false
	}
}

func safeAccount(account string) bool {
	if account == "" {
		return false
	}
	for _, r := range account {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func safeDetailScalar(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func contractInvalidf(format string, args ...any) error {
	_ = fmt.Sprintf(format, args...)
	return NewSharedProviderContractInvalid()
}
