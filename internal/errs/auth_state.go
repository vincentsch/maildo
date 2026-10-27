package errs

type AuthStateOperation string

const (
	AuthStateLogin         AuthStateOperation = "login"
	AuthStateRepair        AuthStateOperation = "repair"
	AuthStateRefresh       AuthStateOperation = "refresh"
	AuthStateStatusRefresh AuthStateOperation = "status_refresh"
	AuthStateClientOpen    AuthStateOperation = "client_open"
	AuthStateClientRefresh AuthStateOperation = "client_refresh"
)

type RecoveryDirection string

const (
	RecoveryRollBack    RecoveryDirection = "roll_back"
	RecoveryRollForward RecoveryDirection = "roll_forward"
)

type AuthTransactionOperation string

const (
	AuthTransactionLogin  AuthTransactionOperation = "login"
	AuthTransactionRepair AuthTransactionOperation = "repair"
	AuthTransactionRename AuthTransactionOperation = "rename"
	AuthTransactionRemove AuthTransactionOperation = "remove"
)

func NewAuthStateChanged(account, provider string, operation AuthStateOperation) error {
	if !validAuthStateOperation(operation) {
		return New(CodeInvalidConfig, "auth state operation is invalid")
	}
	return NewWithDetails(CodeProviderConflict, "the saved login changed while this command ran, probably because of another maildo command; try again", map[string]any{
		"account":   account,
		"provider":  provider,
		"operation": string(operation),
		"reason":    "auth_state_changed",
	})
}

func NewAuthStateDurabilityFailure() error {
	return NewWithDetails(CodeInvalidConfig, "maildo could not confirm that the login change was written to disk", map[string]any{
		"reason":            "auth_state_durability_unverified",
		"recovery_required": false,
	})
}

func NewAuthRecoveryRequired(operation AuthTransactionOperation, direction RecoveryDirection) error {
	if !validAuthTransactionOperation(operation) || !validRecoveryDirection(direction) {
		return NewAuthRecoveryEvidenceInvalid()
	}
	desired := direction == RecoveryRollForward
	return NewWithDetails(CodeInvalidConfig, "maildo could not finish an interrupted login change; run maildo auth list, which tries again and shows your accounts, before you run this command again", map[string]any{
		"operation":                   string(operation),
		"recovery_required":           true,
		"recovery_direction":          string(direction),
		"desired_state_authoritative": desired,
	})
}

func NewAuthRecoveryEvidenceInvalid() error {
	return NewWithDetails(CodeInvalidConfig, "the record of an interrupted login change is damaged; make a copy of maildo's config folder, then restore the folder from a backup you trust before you try again", map[string]any{
		"reason":                      "transaction_evidence_invalid",
		"recovery_required":           true,
		"recovery_direction":          "indeterminate",
		"desired_state_authoritative": nil,
	})
}

func NewAuthTransactionStartFailure(operation AuthTransactionOperation) error {
	if !validAuthTransactionOperation(operation) {
		return New(CodeInvalidConfig, "auth transaction operation is invalid")
	}
	return NewWithDetails(CodeInvalidConfig, "maildo could not start the login change; try again", map[string]any{
		"operation":         string(operation),
		"reason":            "transaction_id_unavailable",
		"recovery_required": false,
	})
}

func validAuthStateOperation(operation AuthStateOperation) bool {
	switch operation {
	case AuthStateLogin, AuthStateRepair, AuthStateRefresh, AuthStateStatusRefresh, AuthStateClientOpen, AuthStateClientRefresh:
		return true
	default:
		return false
	}
}

func validAuthTransactionOperation(operation AuthTransactionOperation) bool {
	switch operation {
	case AuthTransactionLogin, AuthTransactionRepair, AuthTransactionRename, AuthTransactionRemove:
		return true
	default:
		return false
	}
}

func validRecoveryDirection(direction RecoveryDirection) bool {
	switch direction {
	case RecoveryRollBack, RecoveryRollForward:
		return true
	default:
		return false
	}
}
