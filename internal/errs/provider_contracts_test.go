package errs_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
)

func TestProviderWriteAttemptRejectsForgedOperationNames(t *testing.T) {
	for _, tt := range []struct {
		provider  errs.ProviderName
		operation errs.ProviderWriteOperation
	}{
		{errs.ProviderGmail, errs.ProviderWriteOperation("gmail_private")},
		{errs.ProviderGmail, errs.WriteSMTPDirectSend},
		{errs.ProviderIMAP, errs.WriteGmailMove},
		{errs.ProviderOutlook, errs.WriteGmailMove},
		{errs.ProviderGmail, errs.WriteOutlookDirectSend},
		{errs.ProviderIMAP, errs.WriteOutlookDirectSend},
		{errs.ProviderFastmail, errs.WriteOutlookDirectSend},
	} {
		if _, err := errs.NewProviderWriteAttempt("work", tt.provider, tt.operation); productCode(err) != errs.CodeSharedProviderContractInvalid {
			t.Fatalf("NewProviderWriteAttempt(%s, %s) error = %v", tt.provider, tt.operation, err)
		}
	}
}

func TestOutlookFolderReadLimitContract(t *testing.T) {
	err := errs.NewReadLimitExceeded("work", errs.ProviderOutlook, errs.ReadLimitFolders, limits.OutlookMaxFolders+1)
	assertProductCode(t, err, errs.CodeReadLimitExceeded)
	d := productDetails(err)
	if d["dimension"] != "folders" || d["limit"] != uint64(limits.OutlookMaxFolders) || d["observed_at_least"] != uint64(limits.OutlookMaxFolders+1) {
		t.Fatalf("folder limit facts: %v", d)
	}
	for _, p := range []errs.ProviderName{errs.ProviderGmail, errs.ProviderIMAP, errs.ProviderFastmail} {
		assertProductCode(t, errs.NewReadLimitExceeded("work", p, errs.ReadLimitFolders, limits.OutlookMaxFolders+1), errs.CodeSharedProviderContractInvalid)
	}
	assertProductCode(t, errs.NewReadLimitExceeded("work", errs.ProviderOutlook, errs.ReadLimitFolders, limits.OutlookMaxFolders), errs.CodeSharedProviderContractInvalid)
}

func TestDefinitiveProviderRejectionMarkerControlsWriteRejected(t *testing.T) {
	unmarked := errs.New(errs.CodeProviderForbidden, "provider rejected request")
	if _, ok := errs.AsDefinitiveProviderRejection(unmarked); ok {
		t.Fatal("unmarked product error converted to definitive rejection")
	}
	attempt, err := errs.NewProviderWriteAttempt("work", errs.ProviderGmail, errs.WriteGmailMove)
	if err != nil {
		t.Fatal(err)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		t.Fatal(err)
	}
	if got := errs.NewWriteRejected(attempt, nil); productCode(got) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("unmarked rejection error = %v", got)
	}

	marked := errs.NewDefinitiveProviderRejectionError("work", errs.ProviderGmail, errs.WriteGmailMove, unmarked)
	rejection, ok := errs.AsDefinitiveProviderRejection(marked)
	if !ok {
		t.Fatalf("marked error did not convert: %v", marked)
	}
	attempt, err = errs.NewProviderWriteAttempt("work", errs.ProviderGmail, errs.WriteGmailMove)
	if err != nil {
		t.Fatal(err)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		t.Fatal(err)
	}
	out := errs.NewWriteRejected(attempt, rejection)
	assertProductCode(t, out, errs.CodeProviderForbidden)
	facts, ok := errs.WriteOutcomeFactsFromError(out)
	if !ok || facts.Outcome != errs.WriteOutcomeRejected || !facts.RetrySafe {
		t.Fatalf("write facts = %#v, %v", facts, ok)
	}
}

func TestKnownPartialBeforeNextPhaseFacts(t *testing.T) {
	plan, err := errs.NewKnownPartialPlan(errs.KnownGmailReplySend, []errs.KnownPartialPhase{errs.KnownPhaseDraftVerified, errs.KnownPhaseSend})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := errs.NewProviderWriteAttempt("gmail-home", errs.ProviderGmail, errs.WriteGmailReply)
	if err != nil {
		t.Fatal(err)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		t.Fatal(err)
	}
	out := errs.NewKnownPartialBeforeNextPhase(attempt, context.Canceled, plan, 1, "d-verified")
	assertProductCode(t, out, errs.CodeVerificationFailed)
	facts, ok := errs.WriteOutcomeFactsFromError(out)
	if !ok || facts.Outcome != errs.WriteOutcomeKnownPartial || !facts.WriteMayHaveLanded || facts.RetrySafe || !facts.AcceptedByProvider || facts.ConfirmedAfterWrite || facts.Succeeded {
		t.Fatalf("known partial facts = %#v, %v", facts, ok)
	}
	details := productDetails(out)
	if details["reason"] != "canceled_before_next_phase" || details["compound_result"] != "known_partial" || details["state_reread_required"] != true || details["verified_draft_id"] != "d-verified" {
		t.Fatalf("details = %#v", details)
	}
	if !reflect.DeepEqual(details["planned_phases"], []string{"draft_verified", "send"}) {
		t.Fatalf("planned phases = %#v", details["planned_phases"])
	}
	if !reflect.DeepEqual(details["completed_phase_indices"], []uint16{1}) {
		t.Fatalf("completed phases = %#v", details["completed_phase_indices"])
	}
}

func TestKnownPartialRejectedPhaseFacts(t *testing.T) {
	plan, err := errs.NewKnownPartialPlan(errs.KnownGmailMove, []errs.KnownPartialPhase{errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk})
	if err != nil {
		t.Fatal(err)
	}
	marked := errs.NewDefinitiveProviderRejectionError("work", errs.ProviderGmail, errs.WriteGmailMove, errs.New(errs.CodeProviderConflict, "conflict"))
	rejection, ok := errs.AsDefinitiveProviderRejection(marked)
	if !ok {
		t.Fatalf("marked error did not convert: %v", marked)
	}
	attempt, err := errs.NewProviderWriteAttempt("work", errs.ProviderGmail, errs.WriteGmailMove)
	if err != nil {
		t.Fatal(err)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		t.Fatal(err)
	}
	out := errs.NewKnownPartial(attempt, rejection, plan, 1, errs.KnownObservedDiagnosticUnavailable, "")
	assertProductCode(t, out, errs.CodeProviderConflict)
	facts, ok := errs.WriteOutcomeFactsFromError(out)
	if !ok || facts.Outcome != errs.WriteOutcomeKnownPartial {
		t.Fatalf("known partial facts = %#v, %v", facts, ok)
	}
	if details := productDetails(out); details["failed_phase_transmission_crossed"] != true || details["observed_state"] != "diagnostic_unavailable" {
		t.Fatalf("details = %#v", details)
	}
}

func TestKnownPartialPlanAcceptsGmailMailboxLabelChunks(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation errs.KnownPartialOperation
		phases    []errs.KnownPartialPhase
	}{
		{
			name:      "copy multi modify chunks",
			operation: errs.KnownGmailCopy,
			phases:    []errs.KnownPartialPhase{errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk},
		},
		{
			name:      "move untrash then modify chunks",
			operation: errs.KnownGmailMove,
			phases:    []errs.KnownPartialPhase{errs.KnownPhaseUntrash, errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk},
		},
		{
			name:      "categories clear multi modify chunks",
			operation: errs.KnownGmailCategoriesClear,
			phases:    []errs.KnownPartialPhase{errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk, errs.KnownPhaseModifyChunk},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := errs.NewKnownPartialPlan(tt.operation, tt.phases); err != nil {
				t.Fatalf("NewKnownPartialPlan() error = %v", err)
			}
		})
	}
}

func TestKnownPartialPlanRejectsGmailDeleteCompoundPlan(t *testing.T) {
	_, err := errs.NewKnownPartialPlan(errs.KnownGmailDelete, []errs.KnownPartialPhase{errs.KnownPhaseModifyChunk, errs.KnownPhaseTrash})
	assertProductCode(t, err, errs.CodeSharedProviderContractInvalid)
}

func TestProviderFailureContextMatrix(t *testing.T) {
	ctx, err := errs.NewProviderFailureContext("work", errs.ProviderIMAP, errs.ProviderProtocolIMAP, errs.ProviderOperationIMAPAuth)
	if err != nil {
		t.Fatal(err)
	}
	assertProductCode(t, errs.NewProviderDataFailure(ctx, errs.ProviderDataReasonLiteralLimitExceeded), errs.CodeProviderDataError)
	assertProductCode(t, errs.NewProviderCapabilityUnavailable(ctx, errs.ProviderCapabilityReasonIMAPVersion, errs.ProviderRequirementIMAP4rev1OrRev2), errs.CodeUnsupportedCapability)
	assertProductCode(t, errs.NewStoredCredentialRejected(ctx), errs.CodeAuthRepairRequired)

	smtpCtx, err := errs.NewProviderFailureContext("work", errs.ProviderFastmail, errs.ProviderProtocolSMTP, errs.ProviderOperationSMTPAuth)
	if err != nil {
		t.Fatal(err)
	}
	if productCode(errs.NewProviderDataFailure(smtpCtx, errs.ProviderDataReasonLiteralLimitExceeded)) != errs.CodeSharedProviderContractInvalid {
		t.Fatal("SMTP literal-limit data failure was accepted")
	}
	if _, err := errs.NewProviderFailureContext("work", errs.ProviderGmail, errs.ProviderProtocolIMAP, errs.ProviderOperationIMAPAuth); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("invalid provider context error = %v", err)
	}
}

func TestSMTPSealedProviderFailures(t *testing.T) {
	smtpCtx, err := errs.NewProviderFailureContext("work", errs.ProviderIMAP, errs.ProviderProtocolSMTP, errs.ProviderOperationSMTPDirectSend)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := errs.NewProviderCapabilityUnavailable(smtpCtx, errs.ProviderCapabilityReasonSMTPUTF8Required, errs.ProviderRequirementSMTPUTF8)
	assertProductCode(t, unavailable, errs.CodeUnsupportedCapability)
	if details := productDetails(unavailable); details["reason"] != "smtputf8_required" || details["protocol"] != "smtp" || details["operation"] != "smtp_direct_send" {
		t.Fatalf("SMTPUTF8 details = %#v", details)
	}

	rejected := errs.NewSMTPRecipientRejected("work", errs.ProviderFastmail, 2, 1, 3)
	assertProductCode(t, rejected, errs.CodeRecipientRejected)
	if details := productDetails(rejected); details["rejected_index"] != 2 || details["accepted_count"] != 1 || details["total_count"] != 3 {
		t.Fatalf("recipient rejection details = %#v", details)
	}

	temporary := errs.NewSMTPRecipientTemporarilyRejected("work", errs.ProviderIMAP, 1, 0, 1)
	assertProductCode(t, temporary, errs.CodeProviderConnectionError)
	if details := productDetails(temporary); details["reason"] != "recipient_temporarily_rejected" || details["protocol"] != "smtp" {
		t.Fatalf("temporary recipient details = %#v", details)
	}

	if productCode(errs.NewSMTPRecipientRejected("work", errs.ProviderGmail, 1, 0, 1)) != errs.CodeSharedProviderContractInvalid {
		t.Fatal("Gmail recipient rejection was accepted for SMTP constructor")
	}
	if productCode(errs.NewSMTPRecipientRejected("work", errs.ProviderIMAP, 2, 0, 2)) != errs.CodeSharedProviderContractInvalid {
		t.Fatal("invalid recipient counters were accepted")
	}

	final := errs.NewSMTPClassifiedReply("work", errs.ProviderIMAP, errs.SMTPReplyFinal, errs.SMTPReplyProviderPolicy)
	if _, ok := errs.AsDefinitiveProviderRejection(final); !ok {
		t.Fatalf("final SMTP classified reply was not definitive: %v", final)
	}
	mailReply := errs.NewSMTPClassifiedReply("work", errs.ProviderIMAP, errs.SMTPReplyMAIL, errs.SMTPReplyInvalidProtocol)
	if _, ok := errs.AsDefinitiveProviderRejection(mailReply); ok {
		t.Fatalf("MAIL SMTP classified reply should be a typed cause, not a rejection marker: %v", mailReply)
	}
}

func TestGmailAttachmentErrorsUsePublicSelectorsAndClosedLimits(t *testing.T) {
	stale := errs.NewGmailAttachmentStaleSnapshot("work", "m1", "part-1")
	assertProductCode(t, stale, errs.CodeAttachmentNotFound)
	if got := productDetails(stale); !reflect.DeepEqual(got, map[string]any{
		"account":             "work",
		"provider":            "gmail",
		"snapshot_message_id": "m1",
		"selector_kind":       "mime_part_id",
		"attachment_id":       "part-1",
		"reason":              "stale_snapshot_part",
	}) {
		t.Fatalf("stale details = %#v", got)
	}

	encoded := errs.NewGmailAttachmentTooLarge("work", "m1", "part-1", errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), uint64(limits.GmailMaxAttachmentEncodedBytes)+1)
	assertProductCode(t, encoded, errs.CodeAttachmentTooLarge)
	if details := productDetails(encoded); details["dimension"] != string(errs.GmailAttachmentSizeEncoded) ||
		details["limit"] != uint64(limits.GmailMaxAttachmentEncodedBytes) ||
		details["observed_at_least"] != uint64(limits.GmailMaxAttachmentEncodedBytes)+1 ||
		details["snapshot_message_id"] != "m1" ||
		details["attachment_id"] != "part-1" {
		t.Fatalf("encoded details = %#v", details)
	}

	const decodedLimit = uint64(25 * 1024 * 1024)
	decoded := errs.NewGmailAttachmentTooLarge("work", "m1", "part-1", errs.GmailAttachmentSizeDecoded, decodedLimit, decodedLimit+1)
	assertProductCode(t, decoded, errs.CodeAttachmentTooLarge)
	if details := productDetails(decoded); details["dimension"] != string(errs.GmailAttachmentSizeDecoded) ||
		details["limit"] != decodedLimit ||
		details["observed_at_least"] != decodedLimit+1 {
		t.Fatalf("decoded details = %#v", details)
	}

	for _, err := range []error{
		errs.NewGmailAttachmentStaleSnapshot("", "m1", "part-1"),
		errs.NewGmailAttachmentStaleSnapshot("work", "", "part-1"),
		errs.NewGmailAttachmentStaleSnapshot("work", "m1", ""),
		errs.NewGmailAttachmentTooLarge("work", "m1", "part-1", errs.GmailAttachmentSizeDimension("bad"), uint64(limits.GmailMaxAttachmentEncodedBytes), uint64(limits.GmailMaxAttachmentEncodedBytes)+1),
		errs.NewGmailAttachmentTooLarge("work", "m1", "part-1", errs.GmailAttachmentSizeEncoded, decodedLimit, decodedLimit+1),
		errs.NewGmailAttachmentTooLarge("work", "m1", "part-1", errs.GmailAttachmentSizeEncoded, uint64(limits.GmailMaxAttachmentEncodedBytes), uint64(limits.GmailMaxAttachmentEncodedBytes)),
	} {
		assertProductCode(t, err, errs.CodeSharedProviderContractInvalid)
	}
}

func TestRequiredCIDAttachmentConstructor(t *testing.T) {
	err := errs.NewRequiredCIDAttachment()
	assertProductCode(t, err, errs.CodeInvalidComposeInput)
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v, want product error", err)
	}
	if product.Message() != "the original message shows some of its attachments inside the text, so the forward has to include them" {
		t.Fatalf("message = %q", product.Message())
	}
	if got := product.Details(); !reflect.DeepEqual(got, map[string]any{
		"field":  "include_attachments",
		"reason": "required_cid_attachment",
	}) {
		t.Fatalf("details = %#v", got)
	}
}

func TestFreshLoginConstructors(t *testing.T) {
	assertProductCode(t, errs.NewPreCommitScopeMissing("home", errs.ProviderGmail), errs.CodeProviderIdentityUnavailable)
	assertProductCode(t, errs.NewPreCommitCredentialRejected("home", errs.ProviderFastmail), errs.CodeProviderIdentityUnavailable)
	assertProductCode(t, errs.NewPreCommitIdentityMismatch("home", errs.ProviderIMAP), errs.CodeProviderIdentityUnavailable)
	assertProductCode(t, errs.NewPreCommitOAuthFailure("home", errs.PreCommitOAuthCallbackTimeout), errs.CodeProviderIdentityUnavailable)

	if productCode(errs.NewPreCommitScopeMissing("home", errs.ProviderOutlook)) != errs.CodeSharedProviderContractInvalid {
		t.Fatal("outlook scope-missing pre-commit constructor was accepted")
	}
	if productCode(errs.NewPreCommitOAuthFailure("home", errs.PreCommitOAuthFailureReason("bad"))) != errs.CodeSharedProviderContractInvalid {
		t.Fatal("invalid OAuth reason was accepted")
	}
}

func assertProductCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := productCode(err); got != code {
		t.Fatalf("code = %q, want %q; err=%v", got, code, err)
	}
}

func productCode(err error) string {
	var product *errs.Error
	if !errors.As(err, &product) {
		return ""
	}
	return product.Code()
}

func productDetails(err error) map[string]any {
	var product *errs.Error
	if !errors.As(err, &product) {
		return nil
	}
	return product.Details()
}
