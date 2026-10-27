package errs_test

import (
	"errors"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestAuthCoordinatorErrorShapes(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		code    string
		message string
		details map[string]any
	}{
		{
			name:    "state changed",
			err:     errs.NewAuthStateChanged("work", "outlook", errs.AuthStateClientOpen),
			code:    errs.CodeProviderConflict,
			message: "the saved login changed while this command ran, probably because of another maildo command; try again",
			details: map[string]any{
				"account":   "work",
				"provider":  "outlook",
				"operation": "client_open",
				"reason":    "auth_state_changed",
			},
		},
		{
			name:    "durability",
			err:     errs.NewAuthStateDurabilityFailure(),
			code:    errs.CodeInvalidConfig,
			message: "maildo could not confirm that the login change was written to disk",
			details: map[string]any{
				"reason":            "auth_state_durability_unverified",
				"recovery_required": false,
			},
		},
		{
			name:    "recovery",
			err:     errs.NewAuthRecoveryRequired(errs.AuthTransactionRename, errs.RecoveryRollForward),
			code:    errs.CodeInvalidConfig,
			message: "maildo could not finish an interrupted login change; run maildo auth list, which tries again and shows your accounts, before you run this command again",
			details: map[string]any{
				"operation":                   "rename",
				"recovery_required":           true,
				"recovery_direction":          "roll_forward",
				"desired_state_authoritative": true,
			},
		},
		{
			name:    "invalid evidence",
			err:     errs.NewAuthRecoveryEvidenceInvalid(),
			code:    errs.CodeInvalidConfig,
			message: "the record of an interrupted login change is damaged; make a copy of maildo's config folder, then restore the folder from a backup you trust before you try again",
			details: map[string]any{
				"reason":                      "transaction_evidence_invalid",
				"recovery_required":           true,
				"recovery_direction":          "indeterminate",
				"desired_state_authoritative": nil,
			},
		},
		{
			name:    "start",
			err:     errs.NewAuthTransactionStartFailure(errs.AuthTransactionLogin),
			code:    errs.CodeInvalidConfig,
			message: "maildo could not start the login change; try again",
			details: map[string]any{
				"operation":         "login",
				"reason":            "transaction_id_unavailable",
				"recovery_required": false,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var product *errs.Error
			if !errors.As(tt.err, &product) {
				t.Fatalf("error = %T, want *errs.Error", tt.err)
			}
			if product.Code() != tt.code || product.Message() != tt.message || product.ExitCode() != 1 {
				t.Fatalf("error shape = (%q, %q, %d)", product.Code(), product.Message(), product.ExitCode())
			}
			got := product.ErrorDetails().(map[string]any)
			for key, want := range tt.details {
				if got[key] != want {
					t.Fatalf("detail %s = %#v, want %#v; all=%#v", key, got[key], want, got)
				}
			}
		})
	}
}
