// Package errs defines the errors maildo shows to people and scripts.
//
// It does not import rungrad. rungrad finds the ExitCode and ErrorDetails
// methods on its own, so the provider packages can return these errors
// without depending on the CLI framework.
package errs

import "fmt"

const (
	CodeMissingActiveAccount             = "missing_active_account"
	CodeUnknownAccount                   = "unknown_account"
	CodeInvalidAccountName               = "invalid_account_name"
	CodeAccountAlreadyExists             = "account_already_exists"
	CodeInvalidActiveAccount             = "invalid_active_account"
	CodeInvalidConfig                    = "invalid_config"
	CodeUnsupportedConfigVersion         = "unsupported_config_version"
	CodeProviderNotAvailable             = "provider_not_available"
	CodeProviderConflict                 = "provider_conflict"
	CodeAuthAppConfigMissing             = "auth_app_config_missing"
	CodeProviderIdentityUnavailable      = "provider_identity_unavailable"
	CodeAuthRepairRequired               = "auth_repair_required"
	CodeAccountIdentityMismatch          = "account_identity_mismatch"
	CodeInvalidAccountScope              = "invalid_account_scope"
	CodeUnsupportedCapability            = "unsupported_capability"
	CodeCapabilityNotImplemented         = "capability_not_implemented"
	CodeFolderNotFound                   = "folder_not_found"
	CodeAmbiguousFolder                  = "ambiguous_folder"
	CodeInvalidFolderName                = "invalid_folder_name"
	CodeProtectedFolder                  = "protected_folder"
	CodeConfirmationRequired             = "confirmation_required"
	CodeApprovalMismatch                 = "approval_mismatch"
	CodeApprovalUsed                     = "approval_used"
	CodeInvalidQuery                     = "invalid_query"
	CodeUnsupportedQueryOperator         = "unsupported_query_operator"
	CodeInvalidSelector                  = "invalid_selector"
	CodeAmbiguousMessageSelector         = "ambiguous_message_selector"
	CodeMessageNotFound                  = "message_not_found"
	CodeThreadNotFound                   = "thread_not_found"
	CodeRuleNotFound                     = "rule_not_found"
	CodeInvalidRuleInput                 = "invalid_rule_input"
	CodeVerificationFailed               = "verification_failed"
	CodeUnsupportedBodyMode              = "unsupported_body_mode"
	CodeInvalidComposeInput              = "invalid_compose_input"
	CodeMessageTooLarge                  = "message_too_large"
	CodeRecipientRejected                = "recipient_rejected"
	CodeDraftNotFound                    = "draft_not_found"
	CodeAttachmentNotFound               = "attachment_not_found"
	CodeAmbiguousAttachment              = "ambiguous_attachment"
	CodeUnsupportedAttachment            = "unsupported_attachment"
	CodeAttachmentTooLarge               = "attachment_too_large"
	CodeAttachmentDownloadPartialFailure = "attachment_download_partial_failure"
	CodeBulkPartialFailure               = "bulk_partial_failure"
	CodeBulkTargetLimitExceeded          = "bulk_target_limit_exceeded"
	CodeActionLogWriteFailed             = "action_log_write_failed"
	CodeSharedProviderContractInvalid    = "shared_provider_contract_invalid"
	CodeProviderClientContractInvalid    = "provider_client_contract_invalid"
	CodeProviderClientClosed             = "provider_client_closed"
	CodeReadLimitExceeded                = "read_limit_exceeded"
	CodeResourceLimitExceeded            = "resource_limit_exceeded"
	CodeProviderForbidden                = "provider_forbidden"
	CodeProviderRateLimited              = "provider_rate_limited"
	CodeProviderConnectionError          = "provider_connection_error"
	CodeProviderDataError                = "provider_data_error"
	CodeDoctorCheckFailed                = "doctor_check_failed"
	CodePolicyDenied                     = "policy_denied"
	CodeUnsupportedPlatform              = "unsupported_platform"
	CodeOutputFileExists                 = "output_file_exists"
)

var exits = map[string]int{
	CodeMissingActiveAccount:             1,
	CodeDoctorCheckFailed:                1,
	CodePolicyDenied:                     4,
	CodeUnsupportedPlatform:              1,
	CodeOutputFileExists:                 1,
	CodeUnknownAccount:                   1,
	CodeInvalidAccountName:               1,
	CodeAccountAlreadyExists:             1,
	CodeInvalidActiveAccount:             1,
	CodeInvalidConfig:                    1,
	CodeUnsupportedConfigVersion:         1,
	CodeProviderNotAvailable:             1,
	CodeProviderConflict:                 1,
	CodeAuthAppConfigMissing:             1,
	CodeProviderIdentityUnavailable:      3,
	CodeAuthRepairRequired:               3,
	CodeAccountIdentityMismatch:          3,
	CodeInvalidAccountScope:              1,
	CodeUnsupportedCapability:            1,
	CodeCapabilityNotImplemented:         1,
	CodeFolderNotFound:                   5,
	CodeAmbiguousFolder:                  1,
	CodeInvalidFolderName:                1,
	CodeProtectedFolder:                  1,
	CodeConfirmationRequired:             1,
	CodeApprovalMismatch:                 1,
	CodeApprovalUsed:                     1,
	CodeInvalidQuery:                     1,
	CodeUnsupportedQueryOperator:         1,
	CodeInvalidSelector:                  1,
	CodeAmbiguousMessageSelector:         1,
	CodeMessageNotFound:                  5,
	CodeThreadNotFound:                   5,
	CodeRuleNotFound:                     5,
	CodeInvalidRuleInput:                 1,
	CodeVerificationFailed:               1,
	CodeUnsupportedBodyMode:              1,
	CodeInvalidComposeInput:              1,
	CodeMessageTooLarge:                  1,
	CodeRecipientRejected:                1,
	CodeDraftNotFound:                    5,
	CodeAttachmentNotFound:               5,
	CodeAmbiguousAttachment:              1,
	CodeUnsupportedAttachment:            1,
	CodeAttachmentTooLarge:               1,
	CodeAttachmentDownloadPartialFailure: 1,
	CodeBulkPartialFailure:               1,
	CodeBulkTargetLimitExceeded:          1,
	CodeActionLogWriteFailed:             1,
	CodeSharedProviderContractInvalid:    2,
	CodeProviderClientContractInvalid:    2,
	CodeProviderClientClosed:             2,
	CodeReadLimitExceeded:                1,
	CodeResourceLimitExceeded:            1,
	CodeProviderForbidden:                4,
	CodeProviderRateLimited:              6,
	CodeProviderConnectionError:          1,
	CodeProviderDataError:                1,
}

// Error is an error for people and scripts.
//
// The code never changes, so scripts can check it. The message is the text
// people read; it never contains file paths. exit is the process exit code.
type Error struct {
	code       string
	message    string
	exit       int
	details    map[string]any
	outcome    *WriteOutcomeFacts
	definitive *definitiveRejection
}

// New returns an Error for code. A code missing from the exits table gets
// exit code 1.
func New(code, message string) *Error {
	exit, ok := exits[code]
	if !ok {
		exit = 1
	}
	return &Error{code: code, message: message, exit: exit}
}

// NewWithDetails returns an Error with details for the JSON output. Details
// must not hold secrets or message content.
func NewWithDetails(code, message string, details map[string]any) *Error {
	e := New(code, message)
	if len(details) > 0 {
		e.details = cloneDetails(details)
	}
	return e
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *Error) Details() map[string]any {
	if e == nil {
		return map[string]any{}
	}
	return cloneDetails(e.details)
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.message != "" {
		return e.message
	}
	return fmt.Sprintf("maildo error: %s", e.code)
}

func (e *Error) ExitCode() int {
	if e == nil || e.exit == 0 {
		return 1
	}
	return e.exit
}

// ErrorDetails returns the fields rungrad adds to the JSON error.
func (e *Error) ErrorDetails() any {
	if e == nil {
		return map[string]any{"code": ""}
	}
	out := cloneDetails(e.details)
	out["code"] = e.code
	return out
}

// WithDetails returns a copy of err with extra machine-readable details, such
// as the server host or the server's own error text. Other errors pass through.
func WithDetails(err error, extra map[string]any) error {
	e, ok := err.(*Error)
	if !ok || e == nil || len(extra) == 0 {
		return err
	}
	out := *e
	out.details = cloneDetails(e.details)
	for k, v := range extra {
		if v == nil || v == "" {
			continue
		}
		out.details[k] = cloneDetailValue(v)
	}
	return &out
}

func cloneDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneDetailValue(v)
	}
	return out
}

func cloneDetailValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneDetails(x)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneDetailValue(v)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []uint16:
		return append([]uint16(nil), x...)
	case []int:
		return append([]int(nil), x...)
	default:
		return x
	}
}
