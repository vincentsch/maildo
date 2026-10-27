package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/discover"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// handlers holds what the command handlers share.
//
// maildo looks up accounts and saved logins itself instead of using
// rungrad's auth hook, because the auth commands must also work when a
// login is missing or broken.
type handlers struct {
	deps Deps
}

// coordinator opens the store of accounts and saved logins at the config
// path.
func (h *handlers) coordinator(f *rungrad.Factory) (*auth.Coordinator, error) {
	return auth.NewCoordinator(f.ConfigPath(), h.deps.Providers.Names())
}

func (h *handlers) registryLoader(f *rungrad.Factory) provider.RegistrySnapshotLoader {
	return func(ctx context.Context) (*maildoconfig.Registry, error) {
		coord, err := h.coordinator(f)
		if err != nil {
			return nil, err
		}
		return coord.LoadRegistry(ctx, f.DryRun())
	}
}

// prepare loads the accounts. With cleanup, it first removes files left
// over from an interrupted login.
func (h *handlers) prepare(ctx context.Context, f *rungrad.Factory, cleanup bool) (*maildoconfig.Registry, *auth.Coordinator, error) {
	coord, err := h.coordinator(f)
	if err != nil {
		return nil, nil, err
	}
	if cleanup {
		if err := coord.CleanupStale(ctx); err != nil {
			return nil, nil, err
		}
	}
	reg, err := coord.LoadRegistry(ctx, f.DryRun())
	return reg, coord, err
}

// login handles both `maildo login` and `maildo auth login`.
//
// It saves the account and its login together at the end, so a failed login
// leaves no saved password for an account that does not exist.
func (h *handlers) login(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	setActive, _ := cmd.Flags().GetBool("set-active")
	noSetActive, _ := cmd.Flags().GetBool("no-set-active")
	if setActive && noSetActive {
		return rungrad.NewError(rungrad.ExitUsage, "--set-active and --no-set-active cannot be combined")
	}
	nameFlag, _ := cmd.Flags().GetString("name")
	nameSet := cmd.Flags().Changed("name")
	providerFlag, _ := cmd.Flags().GetString("provider")
	providerSet := cmd.Flags().Changed("provider")
	prompts := newPromptSession(f, cmd)
	defer forgetStdinReader(f)

	// --name with an existing account logs in to it again, with its saved
	// provider, so `maildo auth login --name work` from the logged-out hint
	// works.
	name := nameFlag
	var account *maildoconfig.Account
	existing := false
	providerName := ""
	if nameSet {
		if err := maildoconfig.ValidateAccountName(name); err != nil {
			return err
		}
		account, existing = reg.Find(name)
		if existing {
			providerName = account.Provider
			if providerSet && providerFlag != providerName {
				return rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("the account %q uses %s; maildo cannot change an account's provider, so pick another name with --name", name, providerName))
			}
		}
	}
	// Without --provider, the email address decides: maildo knows which
	// large services need what, and finds other servers itself.
	email, _ := cmd.Flags().GetString("email")
	if providerName == "" {
		switch {
		case providerSet:
			providerName = providerFlag
			if _, _, ok := h.deps.Providers.Get(providerName); !ok {
				return errs.NewWithDetails(errs.CodeProviderNotAvailable, fmt.Sprintf("maildo has no provider %q", providerName), map[string]any{
					"provider": providerName,
					"hint":     "leave out --provider and give your address with --email; maildo picks the provider from it. Otherwise use imap or fastmail for a password login, gmail or outlook for a sign-in in the browser",
				})
			}
		case email == "" && canPrompt(f, cmd):
			email = prompts.line("Email address:", "")
			if email == "" {
				return rungrad.NewError(rungrad.ExitUsage, "an email address is needed to log in")
			}
			if err := cmd.Flags().Set("email", email); err != nil {
				return rungrad.NewError(rungrad.ExitUsage, "--email is invalid")
			}
			fallthrough
		case email != "":
			providerName, err = providerForEmail(email, reg)
			if err != nil {
				return err
			}
			if !providerUsesConfiguredSender(providerName) {
				// OAuth providers learn the address from the sign-in itself.
				if flag := cmd.Flags().Lookup("email"); flag != nil {
					flag.Changed = false
				}
			}
		default:
			return rungrad.NewError(rungrad.ExitUsage, "give the address to log in to, like: maildo login --email you@example.com (or choose a provider with --provider)")
		}
	}
	if !nameSet {
		suggested := suggestAccountName(reg, providerName, email)
		if canPrompt(f, cmd) {
			name = prompts.line(fmt.Sprintf("Account name [%s]:", suggested), suggested)
		} else if suggested != "" {
			name = suggested
		} else {
			return rungrad.NewError(rungrad.ExitUsage, "give the account a name with --name")
		}
		if err := maildoconfig.ValidateAccountName(name); err != nil {
			return err
		}
		account, existing = reg.Find(name)
		if existing {
			if providerName != account.Provider {
				return rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("the account %q uses %s; maildo cannot change an account's provider, so pick another name", name, account.Provider))
			}
			providerName = account.Provider
		}
	}
	delete(cmd.Annotations, annotationNewAccount)
	if !existing {
		setAnnotation(cmd, annotationNewAccount, "set")
	}
	p, codec, err := h.lookupProviderCodec(providerName)
	if err != nil {
		return err
	}
	setup, setupResolution, err := h.resolveAuthSetup(cmd.Context(), f, cmd, reg, providerName, name, account, provider.InteractiveLogin)
	if err != nil {
		return err
	}
	for _, note := range setupResolution.Notes {
		f.Infof("%s", note)
	}
	willSetActive := setActive || (!existing && len(reg.Accounts) == 0)
	if f.DryRun() {
		return writeAuthPreview(f, previewLogin(providerName, name, willSetActive, existing, previewFields(setupResolution.Preview)...))
	}

	snapshotReg, rev, existingBytes, existingCache, err := coord.Snapshot(cmd.Context(), providerName, name)
	if err != nil {
		return err
	}
	defer zeroBytes(existingBytes)
	reg = snapshotReg
	account, existing = reg.Find(name)
	runtime := newProviderRuntime(f, providerName, provider.InteractiveLogin)
	existingSource, existingForProvider, err := h.existingLoginCredentialSource(reg, account, setup, existingBytes, existingCache)
	if err != nil {
		return err
	}
	secretSource := h.newInteractiveSecretSource(f)
	authResult, err := p.InteractiveAuth(cmd.Context(), provider.InteractiveAuthContext{
		Runtime:   runtime,
		Operation: provider.InteractiveLogin,
		Flow:      setupResolution.Flow,
		Provider:  providerName,
		Account:   name,
		Setup:     setup,
		Existing:  existingForProvider,
		Secrets:   secretSource,
	})
	runtime.close()
	if existingSource != nil {
		existingSource.close()
	}
	secretSource.close()
	if err != nil {
		destroyInteractiveResult(authResult)
		return mapProviderContractError(err)
	}
	if err := validateInteractiveResult(providerName, name, setup, authResult); err != nil {
		destroyInteractiveResult(authResult)
		return mapProviderContractError(err)
	}
	if err := cmd.Context().Err(); err != nil {
		destroyInteractiveResult(authResult)
		return err
	}
	defer authResult.Credential.Destroy()
	credentialBytes, err := codec.Encode(auth.CredentialBinding{Provider: providerName, Account: name}, authResult.Credential)
	if err != nil {
		return mapProviderContractError(err)
	}
	defer zeroBytes(credentialBytes)
	ident := authResult.Identity
	created := !existing
	if existing {
		if err := enforceAccountIdentity(*account, ident); err != nil {
			return err
		}
		applyInteractiveIdentity(account, ident)
		if err := h.applyAuthSetup(reg, account, authResult.Setup); err != nil {
			return err
		}
	} else {
		newAccount := maildoconfig.Account{
			Name:              name,
			Provider:          providerName,
			Email:             ident.Email,
			DisplayName:       ident.DisplayName,
			ProviderAccountID: ident.ProviderAccountID,
			AuthRef:           maildoconfig.DeriveAuthRef(providerName, name),
			DefaultFrom:       ident.Email,
		}
		if err := h.applyAuthSetup(reg, &newAccount, authResult.Setup); err != nil {
			return err
		}
		if err := reg.Add(newAccount); err != nil {
			return err
		}
	}
	if willSetActive {
		reg.SetActive(name)
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	binding := auth.CredentialBinding{Provider: providerName, Account: name}
	if err := coord.CommitTransaction(cmd.Context(), auth.TransactionRequest{
		Operation:       errs.AuthTransactionLogin,
		Revision:        rev,
		Registry:        reg,
		OldBinding:      binding,
		NewBinding:      binding,
		DesiredCache:    credentialBytes,
		DesiredCacheSet: true,
	}, errs.AuthStateLogin); err != nil {
		return err
	}
	result := loginResult{
		Account:         name,
		Provider:        providerName,
		Operation:       "login",
		Created:         created,
		Reauthenticated: !created,
		Email:           ident.Email,
		DisplayName:     ident.DisplayName,
		Active:          reg.ActiveAccount == name,
		AuthRef:         maildoconfig.DeriveAuthRef(providerName, name),
		AuthStatus:      authStatusValid,
		ActiveAccount:   activePtr(reg.ActiveAccount),
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "login", name, providerName, reg.ActiveAccount), humanLogin(result))
}

// list shows the accounts. It checks whether each has a saved login, but
// does not use it.
func (h *handlers) list(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	result := listResult{Accounts: make([]listAccount, 0, len(reg.Accounts))}
	for _, account := range reg.Accounts {
		present, err := coord.CredentialExists(cmd.Context(), account.Provider, account.Name, f.DryRun())
		if err != nil {
			return err
		}
		item := listAccount{
			Name:        account.Name,
			Provider:    account.Provider,
			Email:       account.Email,
			DisplayName: account.DisplayName,
			Active:      account.Name == reg.ActiveAccount,
			AuthRef:     account.AuthRef,
			AuthStatus:  statusFromCache(present),
		}
		if _, _, ok := h.deps.Providers.Get(account.Provider); ok {
			status, err := h.localStatus(cmd.Context(), f, coord, account, reg)
			if err != nil {
				return err
			}
			applyLocalStatus(status, account.Name, &item.AuthStatus, &item.LoginCommand, &item.RepairCommand)
		} else if !present {
			item.LoginCommand = loginCommand(account.Name)
		}
		result.Accounts = append(result.Accounts, item)
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "list", "", "", reg.ActiveAccount), humanList(result))
}

// status shows whether an account's login works. Only with --refresh, and
// only when a login is saved, does it renew the login with the server.
func (h *handlers) status(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	name := reg.ActiveAccount
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		return errs.New(errs.CodeMissingActiveAccount, "there is no active account; name one, as in maildo auth status <name>, or pick one with maildo auth switch <name>")
	}
	account, err := lookupAccount(reg, name)
	if err != nil {
		return err
	}
	refresh, _ := cmd.Flags().GetBool("refresh")
	present, err := coord.CredentialExists(cmd.Context(), account.Provider, account.Name, f.DryRun())
	if err != nil {
		return err
	}
	if refresh && f.DryRun() {
		return writeAuthPreview(f, previewRefresh(*account))
	}
	authStatus := statusFromCache(present)
	refreshChecked := false
	credentialChanged := false
	rotatedRefreshToken := false
	repair := ""
	if !refresh {
		if _, _, ok := h.deps.Providers.Get(account.Provider); ok {
			status, err := h.localStatus(cmd.Context(), f, coord, *account, reg)
			if err != nil {
				return err
			}
			login := ""
			applyLocalStatus(status, account.Name, &authStatus, &login, &repair)
		}
	} else if refresh && present {
		refreshed, err := h.forceRefresh(cmd.Context(), f, coord, *account, reg, errs.AuthStateStatusRefresh)
		if err != nil {
			if isAuthRepairRequired(err) {
				// Unlike `auth refresh`, this reports a broken login as the
				// account's state and exits with 0, which suits scripts.
				authStatus = authStatusRepairRequired
				refreshChecked = true
				repair = repairCommand(account.Name)
				goto renderStatus
			}
			return err
		}
		authStatus = authStatusRefreshed
		refreshChecked = refreshed.RefreshChecked
		credentialChanged = refreshed.CredentialChanged
		rotatedRefreshToken = refreshed.RotatedRefreshToken
	}
renderStatus:
	result := statusResult{
		Account:             account.Name,
		Provider:            account.Provider,
		Email:               account.Email,
		DisplayName:         account.DisplayName,
		Active:              account.Name == reg.ActiveAccount,
		AuthStatus:          authStatus,
		CredentialCache:     credentialState(present),
		RefreshChecked:      refreshChecked,
		CredentialChanged:   credentialChanged,
		RotatedRefreshToken: rotatedRefreshToken,
		RepairCommand:       repair,
	}
	if authStatus == authStatusLoggedOut {
		result.LoginCommand = loginCommand(account.Name)
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "status", account.Name, account.Provider, reg.ActiveAccount), humanStatus(result))
}

// refresh renews a saved login without asking anything. For a logged-out
// account it contacts no server and returns logged_out with the login
// command.
func (h *handlers) refresh(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	account, err := lookupAccount(reg, args[0])
	if err != nil {
		return err
	}
	if f.DryRun() {
		return writeAuthPreview(f, previewRefresh(*account))
	}
	present, err := coord.CredentialExists(cmd.Context(), account.Provider, account.Name, f.DryRun())
	if err != nil {
		return err
	}
	result := refreshResult{
		Account:             account.Name,
		Provider:            account.Provider,
		Operation:           "refresh",
		AuthStatus:          authStatusLoggedOut,
		CredentialCache:     credentialAbsent,
		RefreshChecked:      false,
		CredentialChanged:   false,
		RotatedRefreshToken: false,
		LoginCommand:        loginCommand(account.Name),
	}
	if present {
		refreshed, err := h.forceRefresh(cmd.Context(), f, coord, *account, reg, errs.AuthStateRefresh)
		if err != nil {
			return err
		}
		result.AuthStatus = authStatusRefreshed
		result.CredentialCache = credentialPresent
		result.RefreshChecked = refreshed.RefreshChecked
		result.CredentialChanged = refreshed.CredentialChanged
		result.RotatedRefreshToken = refreshed.RotatedRefreshToken
		result.LoginCommand = ""
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "refresh", account.Name, account.Provider, reg.ActiveAccount), humanRefresh(result))
}

// repair logs in to an existing account again. The old login stays saved
// until the new one works.
func (h *handlers) repair(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	account, err := lookupAccount(reg, args[0])
	if err != nil {
		return err
	}
	if f.DryRun() {
		setup, setupResolution, err := h.resolveAuthSetup(cmd.Context(), f, cmd, reg, account.Provider, account.Name, account, provider.InteractiveRepair)
		if err != nil {
			return err
		}
		_ = setup
		return writeAuthPreview(f, previewRepair(*account, previewFields(setupResolution.Preview)...))
	}
	p, codec, err := h.lookupProviderCodec(account.Provider)
	if err != nil {
		return err
	}
	setup, setupResolution, err := h.resolveAuthSetup(cmd.Context(), f, cmd, reg, account.Provider, account.Name, account, provider.InteractiveRepair)
	if err != nil {
		return err
	}
	_, rev, existingBytes, existingCache, err := coord.Snapshot(cmd.Context(), account.Provider, account.Name)
	if err != nil {
		return err
	}
	defer zeroBytes(existingBytes)
	if refreshedAccount, ok := reg.Find(account.Name); ok {
		account = refreshedAccount
	}
	runtime := newProviderRuntime(f, account.Provider, provider.InteractiveRepair)
	existingSource := newCredentialSource(existingBytes, existingCache)
	secretSource := h.newInteractiveSecretSource(f)
	authResult, err := p.InteractiveAuth(cmd.Context(), provider.InteractiveAuthContext{
		Runtime:   runtime,
		Operation: provider.InteractiveRepair,
		Flow:      setupResolution.Flow,
		Provider:  account.Provider,
		Account:   account.Name,
		Setup:     setup,
		Existing:  existingSource,
		Secrets:   secretSource,
	})
	runtime.close()
	existingSource.close()
	secretSource.close()
	if err != nil {
		destroyInteractiveResult(authResult)
		return mapProviderContractError(err)
	}
	if err := validateInteractiveResult(account.Provider, account.Name, setup, authResult); err != nil {
		destroyInteractiveResult(authResult)
		return mapProviderContractError(err)
	}
	if err := cmd.Context().Err(); err != nil {
		destroyInteractiveResult(authResult)
		return err
	}
	defer authResult.Credential.Destroy()
	credentialBytes, err := codec.Encode(auth.CredentialBinding{Provider: account.Provider, Account: account.Name}, authResult.Credential)
	if err != nil {
		return mapProviderContractError(err)
	}
	defer zeroBytes(credentialBytes)
	ident := authResult.Identity
	if err := enforceAccountIdentity(*account, ident); err != nil {
		return err
	}
	applyInteractiveIdentity(account, ident)
	if err := h.applyAuthSetup(reg, account, authResult.Setup); err != nil {
		return err
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	binding := auth.CredentialBinding{Provider: account.Provider, Account: account.Name}
	if err := coord.CommitTransaction(cmd.Context(), auth.TransactionRequest{
		Operation:       errs.AuthTransactionRepair,
		Revision:        rev,
		Registry:        reg,
		OldBinding:      binding,
		NewBinding:      binding,
		DesiredCache:    credentialBytes,
		DesiredCacheSet: true,
	}, errs.AuthStateRepair); err != nil {
		return err
	}
	result := repairResult{
		Account:             account.Name,
		Provider:            account.Provider,
		Operation:           "repair",
		Repaired:            true,
		AuthStatus:          authStatusValid,
		CredentialCache:     credentialPresent,
		RefreshChecked:      true,
		CredentialChanged:   true,
		RotatedRefreshToken: false,
		Active:              account.Name == reg.ActiveAccount,
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "repair", account.Name, account.Provider, reg.ActiveAccount), humanRepair(result))
}

// switchAccount changes which account is active, and nothing else.
func (h *handlers) switchAccount(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	account, err := lookupAccount(reg, args[0])
	if err != nil {
		return err
	}
	if f.DryRun() {
		return writeAuthPreview(f, previewSwitch(*account, reg.ActiveAccount))
	}
	reg, switched, err := coord.SwitchActive(cmd.Context(), account.Name)
	if err != nil {
		return err
	}
	result := switchResult{
		Account:       switched.Name,
		Provider:      switched.Provider,
		Operation:     "switch",
		ActiveAccount: activePtr(reg.ActiveAccount),
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "switch", switched.Name, switched.Provider, reg.ActiveAccount), humanSwitch(result))
}

// logout deletes the saved login, but keeps the account and leaves the
// active account as it is.
func (h *handlers) logout(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	account, err := lookupAccount(reg, args[0])
	if err != nil {
		return err
	}
	present, err := coord.CredentialExists(cmd.Context(), account.Provider, account.Name, f.DryRun())
	if err != nil {
		return err
	}
	if f.DryRun() {
		return writeAuthPreview(f, previewLogout(*account, present))
	}
	reg, loggedOut, deleted, err := coord.LogoutAccount(cmd.Context(), account.Name, func(account maildoconfig.Account, lockedReg *maildoconfig.Registry, data []byte, present bool) (bool, error) {
		return h.localLogoutIntentFromBytes(cmd.Context(), f, account, lockedReg, data, present)
	})
	if err != nil {
		return err
	}
	result := logoutResult{
		Account:                loggedOut.Name,
		Provider:               loggedOut.Provider,
		Operation:              "logout",
		CredentialCacheDeleted: deleted,
		ActiveAccount:          activePtr(reg.ActiveAccount),
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "logout", loggedOut.Name, loggedOut.Provider, reg.ActiveAccount), humanLogout(result))
}

// rename renames an account and moves its saved login to the new name. If
// saving the accounts fails after the login moved, it moves the login back.
func (h *handlers) rename(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	oldName, newName := args[0], args[1]
	if err := maildoconfig.ValidateAccountName(newName); err != nil {
		return err
	}
	account, err := lookupAccount(reg, oldName)
	if err != nil {
		return err
	}
	if _, exists := reg.Find(newName); exists {
		return errs.New(errs.CodeAccountAlreadyExists, fmt.Sprintf("account %q already exists", newName))
	}
	activeChange := "unchanged"
	if reg.ActiveAccount == oldName {
		activeChange = newName
	}
	if f.DryRun() {
		return writeAuthPreview(f, previewRename(account, newName, activeChange))
	}
	_, codec, err := h.lookupProviderCodec(account.Provider)
	if err != nil {
		return err
	}
	providerName := account.Provider
	reg, migrated, err := coord.RenameAccount(cmd.Context(), oldName, newName, func(source, dest auth.CredentialBinding, data []byte, present bool) (bool, []byte, error) {
		if !present {
			return false, nil, nil
		}
		registrar := newSecretRegistrar(f)
		defer registrar.close()
		result, err := codec.RebindCredential(registrar, data, source, dest)
		if err != nil {
			return false, nil, mapProviderContractError(err)
		}
		return true, result.Bytes, nil
	})
	if err != nil {
		return err
	}
	result := renameResult{
		Account:                 newName,
		Provider:                providerName,
		Operation:               "rename",
		OldName:                 oldName,
		NewName:                 newName,
		AuthRef:                 maildoconfig.DeriveAuthRef(providerName, newName),
		ActiveAccount:           activePtr(reg.ActiveAccount),
		CredentialCacheMigrated: migrated,
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "rename", newName, providerName, reg.ActiveAccount), humanRename(result))
}

// remove deletes an account and its saved login. The login goes first, so
// when that fails the account is still there.
func (h *handlers) remove(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	reg, coord, err := h.prepare(cmd.Context(), f, !f.DryRun())
	if err != nil {
		return err
	}
	name := args[0]
	account, err := lookupAccount(reg, name)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	setActive, _ := cmd.Flags().GetString("set-active")
	if force && setActive != "" {
		return errs.New(errs.CodeInvalidActiveAccount, "--force and --set-active cannot be combined")
	}
	removingActive := reg.ActiveAccount == name
	if removingActive && !force && setActive == "" {
		return errs.New(errs.CodeInvalidActiveAccount, fmt.Sprintf("%q is the active account; pick the next one with --set-active <name>, or add --force to leave no account active", name))
	}
	activeEffect := "unchanged"
	// --set-active also works when the removed account is not the active
	// one.
	if setActive != "" {
		if setActive == name {
			return errs.New(errs.CodeInvalidActiveAccount, "the account you remove cannot become the active account")
		}
		if _, ok := reg.Find(setActive); !ok {
			return errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", setActive))
		}
		activeEffect = "set:" + setActive
	} else if force && removingActive {
		activeEffect = "cleared"
	}
	present, err := coord.CredentialExists(cmd.Context(), account.Provider, account.Name, f.DryRun())
	if err != nil {
		return err
	}
	if f.DryRun() {
		return writeAuthPreview(f, previewRemove(*account, activeEffect, present))
	}
	reg, removed, activeEffect, deleted, err := coord.RemoveAccount(cmd.Context(), name, force, setActive, func(account maildoconfig.Account, lockedReg *maildoconfig.Registry, data []byte, present bool) (bool, error) {
		return h.localLogoutIntentFromBytes(cmd.Context(), f, account, lockedReg, data, present)
	})
	if err != nil {
		return err
	}
	result := removeResult{
		Account:                name,
		Provider:               removed.Provider,
		Operation:              "remove",
		Removed:                true,
		CredentialCacheDeleted: deleted,
		ActiveAccount:          activePtr(reg.ActiveAccount),
	}
	return f.WriteResultWithMeta(result, safeMeta(cmd.CommandPath(), "remove", name, removed.Provider, reg.ActiveAccount), humanRemove(result))
}

// providerForEmail picks the provider for an address: Fastmail's own profile
// for Fastmail addresses, Outlook (OAuth) for Microsoft addresses once an
// Outlook client id is set up, and IMAP for everything else.
func providerForEmail(email string, reg *maildoconfig.Registry) (string, error) {
	if _, _, ok := strings.Cut(email, "@"); !ok {
		return "", rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("%q is not an email address", email))
	}
	preset, ok := discover.PresetFor(email)
	switch {
	case !ok:
		return "imap", nil
	case preset.Provider == "outlook":
		if reg != nil && reg.Outlook != nil && reg.Outlook.ClientID != "" {
			return "outlook", nil
		}
		return "", rungrad.NewError(rungrad.ExitUsage, preset.PasswordHint)
	default:
		return preset.Provider, nil
	}
}

// suggestAccountName proposes a short account name: the existing account
// for this address if there is one, otherwise the mail domain's first label
// ("posteo", "gmail"), made unique.
func suggestAccountName(reg *maildoconfig.Registry, providerName, email string) string {
	if reg != nil && email != "" {
		for _, account := range reg.Accounts {
			if sameEmail(account.Email, email) && account.Provider == providerName {
				return account.Name
			}
		}
	}
	base := providerName
	if _, domain, ok := strings.Cut(strings.ToLower(email), "@"); ok {
		label, _, _ := strings.Cut(domain, ".")
		var b strings.Builder
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 && b.String()[0] >= 'a' && b.String()[0] <= 'z' {
			base = b.String()
		}
	}
	if len(base) > 40 {
		base = base[:40]
	}
	if base == "" || maildoconfig.ValidateAccountName(base) != nil {
		base = "mail"
	}
	name := base
	for i := 2; reg != nil; i++ {
		if _, taken := reg.Find(name); !taken {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// lookupAccount finds an account by name, or returns the unknown-account
// error that all auth commands use.
func lookupAccount(reg *maildoconfig.Registry, name string) (*maildoconfig.Account, error) {
	account, ok := reg.Find(name)
	if !ok {
		return nil, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", name))
	}
	return account, nil
}

// lookupProviderCodec returns a provider and the code that reads and writes
// its saved logins.
func (h *handlers) lookupProviderCodec(name string) (provider.Provider, provider.CredentialCodec, error) {
	p, codec, ok := h.deps.Providers.Get(name)
	if !ok {
		return nil, nil, errs.New(errs.CodeProviderNotAvailable, fmt.Sprintf("provider %q is not available", name))
	}
	return p, codec, nil
}

func (h *handlers) resolveAuthSetup(ctx context.Context, f *rungrad.Factory, cmd *cobra.Command, reg *maildoconfig.Registry, providerName, accountName string, account *maildoconfig.Account, operation provider.InteractiveAuthOperation) (maildoconfig.ProviderSetup, provider.SetupResolution, error) {
	if h.deps.SetupParsers == nil {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, provider.ErrProviderRuntimeDenied
	}
	if err := h.deps.SetupParsers.RejectForeignChangedFlags(cmd, providerName); err != nil {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, err
	}
	_, parser, ok := h.deps.SetupParsers.Lookup(providerName)
	if !ok {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, errs.New(errs.CodeProviderNotAvailable, fmt.Sprintf("provider %q is not available", providerName))
	}
	// A new IMAP or Fastmail account has no server settings yet; they come
	// from the flags and prompts below. Outlook and Gmail accounts start from
	// the client id stored for all accounts of that provider.
	var existing maildoconfig.ProviderSetup
	if account != nil || !providerUsesConfiguredSender(providerName) {
		setupAccount := maildoconfig.Account{Name: accountName, Provider: providerName, AuthRef: maildoconfig.DeriveAuthRef(providerName, accountName)}
		if account != nil {
			setupAccount = *account
		}
		var err error
		existing, err = h.setupForRegisteredProvider(reg, setupAccount)
		if err != nil {
			return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, err
		}
	}
	flags, err := h.deps.SetupParsers.Snapshot(cmd, providerName)
	if err != nil {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, err
	}
	env := &setupEnvLookup{f: f, provider: providerName}
	resolution, err := parser.Resolve(ctx, provider.SetupResolveContext{
		Flags:     flags,
		Operation: operation,
		Provider:  providerName,
		Account:   accountName,
		Existing:  existing.Clone(),
		LookupEnv: env.lookup,
	})
	env.close()
	if err != nil {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, mapProviderContractError(err)
	}
	if len(resolution.Missing) > 0 {
		defer forgetStdinReader(f)
		if !canPrompt(f, cmd) {
			return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, rungrad.NewError(rungrad.ExitUsage, "maildo needs more settings to log in; give "+missingFlags(resolution.Missing))
		}
		// Say why maildo asks (for example, that it could not find the
		// servers) before the questions, not after them.
		for _, note := range resolution.Notes {
			f.Infof("%s", note)
		}
		resolution, err = parser.PromptMissing(ctx, provider.SetupPromptContext{
			Operation: operation,
			Provider:  providerName,
			Account:   accountName,
			Visible:   h.newVisibleInput(f),
		}, resolution)
		if err != nil {
			return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, mapProviderContractError(err)
		}
		resolution.Notes = nil
	}
	setup := resolution.Setup.Clone()
	if maildoconfig.ProviderHasSetup(providerName) || !setup.Equal(maildoconfig.ProviderSetup{}) {
		if err := setup.ValidateFor(providerName); err != nil {
			return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, err
		}
	}
	if err := validateProviderGlobalSetup(reg, providerName, accountName, setup, resolution); err != nil {
		return maildoconfig.ProviderSetup{}, provider.SetupResolution{}, err
	}
	return setup, resolution, nil
}

func (h *handlers) existingLoginCredentialSource(reg *maildoconfig.Registry, account *maildoconfig.Account, setup maildoconfig.ProviderSetup, existingBytes []byte, existingCache bool) (*credentialSource, provider.CredentialSource, error) {
	if account == nil {
		return nil, nil, nil
	}
	currentSetup, err := h.setupForRegisteredProvider(reg, *account)
	if err != nil {
		return nil, nil, err
	}
	if currentSetup.Equal(setup) {
		source := newCredentialSource(existingBytes, existingCache)
		return source, source, nil
	}
	source := newCredentialSource(nil, false)
	return source, source, nil
}

func validateProviderGlobalSetup(reg *maildoconfig.Registry, providerName, accountName string, setup maildoconfig.ProviderSetup, resolution provider.SetupResolution) error {
	if providerName != "gmail" || setup.Gmail == nil || reg == nil || reg.Gmail == nil || reg.Gmail.ClientID == "" || setup.Gmail.ClientID == reg.Gmail.ClientID {
		return nil
	}
	source := ""
	for _, field := range resolution.Preview {
		if field.Name == "client_id_source" {
			source = field.Value
			break
		}
	}
	if source != "flag" && source != "environment" {
		return nil
	}
	for _, account := range reg.Accounts {
		if account.Provider == "gmail" && account.Name != accountName {
			return rungrad.NewError(rungrad.ExitUsage, "the Gmail client id from "+gmailClientIDSourceText(source)+" differs from gmail.client_id, which your other Gmail accounts use; leave it out, or change gmail.client_id with maildo config set")
		}
	}
	return nil
}

// gmailClientIDSourceText names where a Gmail client id came from.
func gmailClientIDSourceText(source string) string {
	switch source {
	case "flag":
		return "--gmail-client-id"
	case "environment":
		return "MAILDO_GMAIL_CLIENT_ID"
	default:
		return source
	}
}

func (h *handlers) newVisibleInput(f *rungrad.Factory) provider.VisibleInput {
	if h.deps.NewVisibleInput != nil {
		return h.deps.NewVisibleInput(f)
	}
	return newVisibleInput(f)
}

func validateInteractiveResult(providerName, accountName string, setup maildoconfig.ProviderSetup, result provider.InteractiveAuthResult) error {
	if result.Credential == nil || isTypedNil(result.Credential) || result.Credential.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	if !result.Setup.Equal(setup) {
		return provider.ErrCredentialCapabilityClosed
	}
	ident := result.Identity
	switch providerName {
	case "outlook":
		if ident.Assurance != provider.IdentityProviderVerified || ident.Email == "" || ident.ProviderAccountID == "" {
			return provider.ErrCredentialCapabilityClosed
		}
	case "gmail":
		if ident.Assurance != provider.IdentityProviderVerified || ident.Email == "" || ident.ProviderAccountID != "" {
			return provider.ErrCredentialCapabilityClosed
		}
	case "imap", "fastmail":
		if ident.Assurance != provider.IdentityConfigured || setup.MailServer == nil || !sameEmail(ident.Email, setup.MailServer.Email) || ident.DisplayName != "" || ident.ProviderAccountID != "" {
			return provider.ErrCredentialCapabilityClosed
		}
	default:
		if ident.Assurance == "" || ident.Email == "" {
			return provider.ErrCredentialCapabilityClosed
		}
	}
	_ = accountName
	return nil
}

func applyInteractiveIdentity(account *maildoconfig.Account, ident provider.Identity) {
	if account == nil {
		return
	}
	account.Email = ident.Email
	account.DisplayName = ident.DisplayName
	account.ProviderAccountID = ident.ProviderAccountID
	account.AuthRef = maildoconfig.DeriveAuthRef(account.Provider, account.Name)
	if providerUsesConfiguredSender(account.Provider) {
		account.DefaultFrom = ident.Email
	}
}

func providerUsesConfiguredSender(providerName string) bool {
	return providerName == "imap" || providerName == "fastmail"
}

// enforceAccountIdentity stops a login or repair from pointing an existing
// account at a different mailbox. It compares the provider's account id, or
// the address when no id is saved yet.
func enforceAccountIdentity(account maildoconfig.Account, ident provider.Identity) error {
	if providerUsesConfiguredSender(account.Provider) {
		return nil
	}
	if account.ProviderAccountID != "" {
		if account.ProviderAccountID != ident.ProviderAccountID {
			return identityMismatch(account.Name)
		}
		return nil
	}
	if account.Email != "" && !sameEmail(account.Email, ident.Email) {
		if account.Provider == "gmail" {
			return errs.NewWithDetails(errs.CodeAuthRepairRequired, fmt.Sprintf("you signed in as another Google address than the one account %q uses; run %s and sign in as %s", account.Name, repairCommand(account.Name), account.Email), map[string]any{
				"account":        account.Name,
				"provider":       account.Provider,
				"reason":         "identity_mismatch",
				"repair_command": repairCommand(account.Name),
			})
		}
		return identityMismatch(account.Name)
	}
	return nil
}

func identityMismatch(name string) error {
	return errs.New(errs.CodeAccountIdentityMismatch, fmt.Sprintf("you signed in to a different mailbox than account %q uses; to use another mailbox, remove the account with maildo auth remove and log in again", name))
}

func sameEmail(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func applyLocalStatus(status provider.LocalStatus, name string, authStatus, login, repair *string) {
	switch status {
	case provider.LocalAbsent:
		*authStatus = authStatusLoggedOut
		*login = loginCommand(name)
		*repair = ""
	case provider.LocalRepairRequired:
		*authStatus = authStatusRepairRequired
		*login = ""
		*repair = repairCommand(name)
	default:
		*authStatus = authStatusValid
		*login = ""
		*repair = ""
	}
}

func isAuthRepairRequired(err error) bool {
	var productErr *errs.Error
	return errors.As(err, &productErr) && productErr.Code() == errs.CodeAuthRepairRequired
}

// missingFlags names the flags for missing setup fields: "--imap-host and
// --smtp-host".
func missingFlags(missing []string) string {
	flags := make([]string, 0, len(missing))
	for _, field := range missing {
		flags = append(flags, "--"+strings.ReplaceAll(field, "_", "-"))
	}
	switch len(flags) {
	case 0:
		return "none"
	case 1:
		return flags[0]
	default:
		return strings.Join(flags[:len(flags)-1], ", ") + " and " + flags[len(flags)-1]
	}
}
