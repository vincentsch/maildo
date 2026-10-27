package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

type narrowAccountClient struct{}

func (narrowAccountClient) ProviderName() string { return "fake" }
func (narrowAccountClient) AccountName() string  { return "work" }
func (narrowAccountClient) Destroy()             {}

func TestRuleMutationOutcomeUnknownErrorContract(t *testing.T) {
	reasons := []RuleMutationOutcomeUnknownReason{
		RuleMutationOutcomeMissingID, RuleMutationOutcomeMalformed,
		RuleMutationOutcomeOversized, RuleMutationOutcomeUnreadable,
		RuleMutationOutcomeTransportUnknown, RuleMutationOutcomeUnexpected2xx,
		RuleMutationOutcomeServerUnknown,
	}
	for _, reason := range reasons {
		err := NewRuleMutationOutcomeUnknownError(reason)
		var typed *RuleMutationOutcomeUnknownError
		if !errors.As(err, &typed) || typed.Reason() != reason || err.Error() != "maildo could not tell whether the rule change happened" {
			t.Fatalf("reason %q produced %#v", reason, err)
		}
	}
}

func TestRuleMutationOutcomeUnknownRejectsInvalidReason(t *testing.T) {
	for _, reason := range []RuleMutationOutcomeUnknownReason{"", "unknown", "bad\r\nreason", "secret-token-sentinel"} {
		err := NewRuleMutationOutcomeUnknownError(reason)
		var typed *RuleMutationOutcomeUnknownError
		if err == nil || err.Error() != "invalid provider rule mutation outcome reason" || errors.As(err, &typed) {
			t.Fatalf("invalid reason %q produced %#v", reason, err)
		}
	}
}

func TestRuleReadFailureErrorContract(t *testing.T) {
	outward := errors.New("safe outward error")
	for _, reason := range []RuleReadFailureReason{
		RuleReadFailureNotFound, RuleReadFailureAuthRepairRequired,
		RuleReadFailureRateLimited, RuleReadFailureMalformedResponse,
		RuleReadFailureOversizedResponse, RuleReadFailureTransport,
		RuleReadFailureProviderAPI,
	} {
		err := NewRuleReadFailureError(reason, outward)
		if err.Reason() != reason || err.Error() != outward.Error() || !errors.Is(err, outward) {
			t.Fatalf("reason %q produced %#v", reason, err)
		}
	}
	invalid := NewRuleReadFailureError("secret-invalid", nil)
	if invalid.Reason() != RuleReadFailureProviderAPI || invalid.Error() != "could not read the rule from the provider" {
		t.Fatalf("invalid reason did not fail closed: %#v", invalid)
	}
}

func TestWithAccountClientUsesScopedDescriptorAndReportsStaleInterface(t *testing.T) {
	ctx := context.Background()
	err := WithAccountClientScope(ctx, func(scope AccountClientScope) error {
		real := scope.(*commandAccountClientScope)
		acct := ResolvedAccount{
			Name:        "work",
			Provider:    "fake",
			Email:       "work@example.test",
			DisplayName: "Work Account",
			token: &resolvedAccountToken{
				scope:    real,
				provider: "fake",
				account:  "work",
				email:    "work@example.test",
				display:  "Work Account",
				open: func(context.Context) (AccountClient, error) {
					return narrowAccountClient{}, nil
				},
				caps: implementedCaps(),
			},
			allowIdentityAccessForTest: true,
		}
		var got AccountClient
		if err := WithAccountClient[AccountClient](ctx, scope, acct, ClientAccessAccountIdentityForTest, func(client AccountClient) error {
			got = client
			return nil
		}); err != nil {
			return err
		}
		if got.ProviderName() != "fake" || got.AccountName() != "work" {
			t.Fatalf("identity facade = %s/%s", got.ProviderName(), got.AccountName())
		}
		err := WithAccountClient[RuleReader](ctx, scope, acct, ClientAccessRulesRead, func(RuleReader) error {
			t.Fatal("callback called for stale interface")
			return nil
		})
		var productErr *errs.Error
		if !errors.As(err, &productErr) || productErr.Code() != errs.CodeCapabilityNotImplemented {
			t.Fatalf("stale interface error = %#v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func implementedCaps() Capabilities {
	out := Capabilities{}
	for _, cap := range allCapabilities {
		out[cap] = CapImplemented
	}
	return out
}
