package provider

import (
	"context"
	"errors"

	"github.com/vincentsch/maildo/internal/mail"
)

// RuleMutationOutcomeUnknownReason says why maildo cannot tell whether a rule
// change that went out was accepted or rejected. Only the values below exist.
type RuleMutationOutcomeUnknownReason string

const (
	RuleMutationOutcomeMissingID        RuleMutationOutcomeUnknownReason = "missing_rule_id"
	RuleMutationOutcomeMalformed        RuleMutationOutcomeUnknownReason = "malformed_success_response"
	RuleMutationOutcomeOversized        RuleMutationOutcomeUnknownReason = "oversized_success_response"
	RuleMutationOutcomeUnreadable       RuleMutationOutcomeUnknownReason = "unreadable_success_response"
	RuleMutationOutcomeTransportUnknown RuleMutationOutcomeUnknownReason = "transport_outcome_unknown"
	RuleMutationOutcomeUnexpected2xx    RuleMutationOutcomeUnknownReason = "unexpected_success_status"
	RuleMutationOutcomeServerUnknown    RuleMutationOutcomeUnknownReason = "server_outcome_unknown"
)

// RuleMutationOutcomeUnknownError carries only the reason. The command adds
// the operation and the rule it knows about.
type RuleMutationOutcomeUnknownError struct {
	reason RuleMutationOutcomeUnknownReason
}

// NewRuleMutationOutcomeUnknownError refuses unknown reasons, so callers can
// rely on the reason and never have to parse provider error text.
func NewRuleMutationOutcomeUnknownError(reason RuleMutationOutcomeUnknownReason) error {
	switch reason {
	case RuleMutationOutcomeMissingID,
		RuleMutationOutcomeMalformed,
		RuleMutationOutcomeOversized,
		RuleMutationOutcomeUnreadable,
		RuleMutationOutcomeTransportUnknown,
		RuleMutationOutcomeUnexpected2xx,
		RuleMutationOutcomeServerUnknown:
		return &RuleMutationOutcomeUnknownError{reason: reason}
	default:
		return errors.New("invalid provider rule mutation outcome reason")
	}
}

func (e *RuleMutationOutcomeUnknownError) Error() string {
	return "maildo could not tell whether the rule change happened"
}

func (e *RuleMutationOutcomeUnknownError) Reason() RuleMutationOutcomeUnknownReason {
	if e == nil {
		return ""
	}
	return e.reason
}

// RuleReadFailureReason says why reading a rule back after a change failed,
// so the check after a write never has to parse provider error text.
type RuleReadFailureReason string

const (
	RuleReadFailureNotFound           RuleReadFailureReason = "not_found"
	RuleReadFailureAuthRepairRequired RuleReadFailureReason = "auth_repair_required"
	RuleReadFailureRateLimited        RuleReadFailureReason = "rate_limited"
	RuleReadFailureMalformedResponse  RuleReadFailureReason = "malformed_response"
	RuleReadFailureOversizedResponse  RuleReadFailureReason = "oversized_response"
	RuleReadFailureTransport          RuleReadFailureReason = "transport"
	RuleReadFailureProviderAPI        RuleReadFailureReason = "provider_api"
)

// RuleReadFailureError pairs the reason with the error that commands show to
// people.
type RuleReadFailureError struct {
	reason  RuleReadFailureReason
	outward error
}

// NewRuleReadFailureError turns an unknown reason into provider_api and
// fills in a plain error when none is given.
func NewRuleReadFailureError(reason RuleReadFailureReason, outward error) *RuleReadFailureError {
	switch reason {
	case RuleReadFailureNotFound,
		RuleReadFailureAuthRepairRequired,
		RuleReadFailureRateLimited,
		RuleReadFailureMalformedResponse,
		RuleReadFailureOversizedResponse,
		RuleReadFailureTransport,
		RuleReadFailureProviderAPI:
	default:
		reason = RuleReadFailureProviderAPI
	}
	if outward == nil {
		outward = errors.New("could not read the rule from the provider")
	}
	return &RuleReadFailureError{reason: reason, outward: outward}
}

func (e *RuleReadFailureError) Reason() RuleReadFailureReason {
	if e == nil {
		return RuleReadFailureProviderAPI
	}
	return e.reason
}

func (e *RuleReadFailureError) Error() string {
	if e == nil || e.outward == nil {
		return "could not read the rule from the provider"
	}
	return e.outward.Error()
}

func (e *RuleReadFailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.outward
}

// RuleReader lists and reads Inbox rules.
type RuleReader interface {
	ListRules(ctx context.Context, req mail.RuleListRequest) (mail.RuleListResult, error)
	GetRule(ctx context.Context, req mail.RuleGetRequest) (mail.Rule, error)
}

// RuleManager creates, changes and deletes Inbox rules.
type RuleManager interface {
	CreateRule(ctx context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error)
	UpdateRule(ctx context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error)
	DeleteRule(ctx context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error)
}
