package provider

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
)

// ScopeMode says how a command picks its accounts. ResolveAccounts applies
// the account rules from this, never from the command's name.
type ScopeMode int

const (
	// ModeSingle picks exactly one account: the one in --account, or else
	// the active account.
	ModeSingle ScopeMode = iota
	// ModeMulti picks the accounts in --account (one or more), all accounts
	// with --all-accounts when AllowAll is set, or else the active account.
	ModeMulti
	// ModeBulkQualified takes the accounts from the targets, each of which
	// names its account (the rows of an ids file), and refuses --account.
	ModeBulkQualified
)

// AccountQualifiedTarget is a message id together with the account it
// belongs to, as in an ids file row "account<TAB>id".
type AccountQualifiedTarget struct {
	Account string
	ID      string
}

// ScopeRequest says how a command picks accounts. ResolveAccounts does all
// the checks of the flags and the account list.
type ScopeRequest struct {
	Mode                    ScopeMode
	AllowAll                bool
	AccountQualifiedTargets []AccountQualifiedTarget
}

// ResolvedAccount is an account as commands use and print it. It holds no
// secrets. The client behind it opens only when a command first needs it, so
// picking accounts never reads saved logins or signs in.
type ResolvedAccount struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AuthRef     string `json:"-"`
	Active      bool   `json:"active"`
	// Policy is what the user allows on this account (maildo config set
	// policy...). WithAccountClient enforces it.
	Policy maildoconfig.Policy `json:"-"`

	token                      *resolvedAccountToken
	allowIdentityAccessForTest bool
}

func AllowAccountIdentityAccessForTest(account ResolvedAccount) ResolvedAccount {
	account.allowIdentityAccessForTest = true
	return account
}

// AccountSelection is the accounts a command picked, in the form commands
// put into their JSON.
type AccountSelection struct {
	Scope    string            `json:"scope"`
	Accounts []ResolvedAccount `json:"accounts"`
}

type AccountClientRequest struct {
	Binding                   auth.CredentialBinding
	AuthRef                   string
	Setup                     maildoconfig.ProviderSetup
	ExpectedEmail             string
	ExpectedDisplayName       string
	ExpectedProviderAccountID string
}

type AccountClientOpenFunc func(context.Context) (AccountClient, error)

type AccountClientBinder func(AccountClientRequest) (AccountClientOpenFunc, error)

type RegistrySnapshotLoader func(context.Context) (*maildoconfig.Registry, error)

const (
	accountFlagUsage      = "Account to use (default: the active account)"
	multiAccountFlagUsage = "Account to use; repeat it for more than one (default: the active account)"
	allAccountsFlagUsage  = "Use all your accounts"
)

// ConfigureAccountFlags adds --account, and --all-accounts where the command
// can work on all accounts.
func ConfigureAccountFlags(cmd *cobra.Command, req ScopeRequest) {
	usage := accountFlagUsage
	if req.Mode == ModeMulti {
		usage = multiAccountFlagUsage
	}
	cmd.Flags().StringArray("account", nil, usage)
	if req.Mode == ModeMulti && req.AllowAll {
		cmd.Flags().Bool("all-accounts", false, allAccountsFlagUsage)
		cmd.MarkFlagsMutuallyExclusive("account", "all-accounts")
	}
}

// ResolveAccounts turns the account flags, or the accounts named by the
// targets, into accounts from the config. It never reads saved logins, opens
// a browser or calls a provider.
func ResolveAccounts(scope AccountClientScope, cmd *cobra.Command, providers *Registry, loadRegistry RegistrySnapshotLoader, bindClient AccountClientBinder, req ScopeRequest) (AccountSelection, error) {
	realScope, ok := scope.(*commandAccountClientScope)
	if !ok || realScope == nil || !realScope.active() {
		return AccountSelection{}, errs.NewProviderClientContractInvalid()
	}
	if bindClient == nil {
		return AccountSelection{}, ErrAccountClientInvalid
	}
	if loadRegistry == nil {
		return AccountSelection{}, ErrAccountClientInvalid
	}
	accounts, _ := cmd.Flags().GetStringArray("account")
	if cmd.Flags().Changed("account") && len(accounts) == 0 {
		return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "--account needs an account name")
	}

	if providers == nil {
		return AccountSelection{}, ErrAccountClientInvalid
	}
	reg, err := loadRegistry(cmd.Context())
	if err != nil {
		return AccountSelection{}, err
	}
	if reg == nil {
		return AccountSelection{}, ErrAccountClientInvalid
	}

	if dup := duplicateAccount(accounts); dup != "" {
		return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, fmt.Sprintf("account %q is given more than once", dup))
	}

	// One lookup for all modes, so an unknown account gives the same error
	// everywhere.
	capabilitySnapshots := map[string]Capabilities{}
	resolve := func(name string) (ResolvedAccount, error) {
		account, ok := reg.Find(name)
		if !ok {
			return ResolvedAccount{}, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", name))
		}
		return newResolved(cmd.Context(), realScope, providers, capabilitySnapshots, bindClient, reg, *account)
	}
	missingActive := func() error {
		return errs.New(errs.CodeMissingActiveAccount, "there is no active account; run maildo login to add one, or maildo auth switch <name> to pick one, or pass --account <name>")
	}

	switch req.Mode {
	case ModeSingle:
		if len(accounts) > 1 {
			return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "this command takes only one --account")
		}
		if len(accounts) == 1 {
			account, err := resolve(accounts[0])
			if err != nil {
				return AccountSelection{}, err
			}
			return AccountSelection{Scope: "single", Accounts: []ResolvedAccount{account}}, nil
		}
		if reg.ActiveAccount == "" {
			return AccountSelection{}, missingActive()
		}
		account, err := resolve(reg.ActiveAccount)
		if err != nil {
			return AccountSelection{}, err
		}
		return AccountSelection{Scope: "single", Accounts: []ResolvedAccount{account}}, nil

	case ModeMulti:
		allAccounts := false
		if req.AllowAll {
			allAccounts, _ = cmd.Flags().GetBool("all-accounts")
		}
		if allAccounts && len(accounts) > 0 {
			return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "--account and --all-accounts cannot be used together")
		}
		if allAccounts {
			if len(reg.Accounts) == 0 {
				return AccountSelection{}, missingActive()
			}
			resolved := make([]ResolvedAccount, 0, len(reg.Accounts))
			for _, account := range reg.Accounts {
				acct, err := newResolved(cmd.Context(), realScope, providers, capabilitySnapshots, bindClient, reg, account)
				if err != nil {
					return AccountSelection{}, err
				}
				resolved = append(resolved, acct)
			}
			return AccountSelection{Scope: "all", Accounts: resolved}, nil
		}
		if len(accounts) > 0 {
			resolved := make([]ResolvedAccount, 0, len(accounts))
			for _, name := range accounts {
				account, err := resolve(name)
				if err != nil {
					return AccountSelection{}, err
				}
				resolved = append(resolved, account)
			}
			scope := "single"
			if len(resolved) >= 2 {
				scope = "read_many"
			}
			return AccountSelection{Scope: scope, Accounts: resolved}, nil
		}
		if reg.ActiveAccount == "" {
			return AccountSelection{}, missingActive()
		}
		account, err := resolve(reg.ActiveAccount)
		if err != nil {
			return AccountSelection{}, err
		}
		return AccountSelection{Scope: "single", Accounts: []ResolvedAccount{account}}, nil

	case ModeBulkQualified:
		// Each target names its account, so an --account flag would leave it
		// unclear which account a target belongs to.
		if len(accounts) > 0 {
			return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "--account cannot be used with an ids file whose rows name their account")
		}
		if len(req.AccountQualifiedTargets) == 0 {
			return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "no message ids were given")
		}
		seen := map[string]bool{}
		resolved := make([]ResolvedAccount, 0, len(req.AccountQualifiedTargets))
		for _, target := range req.AccountQualifiedTargets {
			// Check every target, also when its account was seen before, so a
			// target without an account never slips through.
			if target.Account == "" {
				return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, fmt.Sprintf("id %q does not name its account", target.ID))
			}
			if seen[target.Account] {
				continue
			}
			account, err := resolve(target.Account)
			if err != nil {
				return AccountSelection{}, err
			}
			seen[target.Account] = true
			resolved = append(resolved, account)
		}
		return AccountSelection{Scope: "bulk_qualified", Accounts: resolved}, nil

	default:
		return AccountSelection{}, errs.New(errs.CodeInvalidAccountScope, "unknown account scope mode")
	}
}

// duplicateAccount returns the first account name given twice, in the order
// the user typed them.
func duplicateAccount(accounts []string) string {
	seen := map[string]bool{}
	for _, account := range accounts {
		if seen[account] {
			return account
		}
		seen[account] = true
	}
	return ""
}

// newResolved builds a ResolvedAccount from the config entry and attaches the
// function that opens its client on first use.
func newResolved(ctx context.Context, scope *commandAccountClientScope, providers *Registry, snapshots map[string]Capabilities, bindClient AccountClientBinder, reg *maildoconfig.Registry, account maildoconfig.Account) (ResolvedAccount, error) {
	acct := account
	caps, ok := snapshots[acct.Provider]
	if !ok {
		var err error
		caps, err = providers.Capabilities(ctx, acct.Provider)
		if err != nil {
			return ResolvedAccount{}, err
		}
		snapshots[acct.Provider] = caps
	}
	setup, err := maildoconfig.SetupFor(reg, acct)
	if err != nil && maildoconfig.ProviderHasSetup(acct.Provider) {
		return ResolvedAccount{}, err
	}
	if err != nil {
		setup = maildoconfig.ProviderSetup{}
	}
	before := setup.Clone()
	req := AccountClientRequest{
		Binding: auth.CredentialBinding{
			Provider: acct.Provider,
			Account:  acct.Name,
		},
		AuthRef:                   maildoconfig.DeriveAuthRef(acct.Provider, acct.Name),
		Setup:                     setup.Clone(),
		ExpectedEmail:             acct.Email,
		ExpectedDisplayName:       acct.DisplayName,
		ExpectedProviderAccountID: acct.ProviderAccountID,
	}
	open, err := bindClient(req)
	if err != nil || open == nil {
		return ResolvedAccount{}, ErrAccountClientInvalid
	}
	if !req.Setup.Equal(before) {
		return ResolvedAccount{}, ErrAccountClientInvalid
	}
	token := &resolvedAccountToken{
		scope:      scope,
		provider:   acct.Provider,
		account:    acct.Name,
		email:      acct.Email,
		display:    acct.DisplayName,
		senderName: acct.SenderName,
		open:       open,
		caps:       caps,
	}
	return ResolvedAccount{
		Name:        acct.Name,
		Provider:    acct.Provider,
		Email:       acct.Email,
		DisplayName: acct.DisplayName,
		AuthRef:     req.AuthRef,
		Active:      acct.Name == reg.ActiveAccount,
		Policy:      clonePolicy(acct.Policy),
		token:       token,
	}, nil
}
