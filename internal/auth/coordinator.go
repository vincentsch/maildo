package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/fsutil"
)

const (
	maxJournalBytes               = 64 * 1024
	maxDetachedSnapshotBytes      = 64 * 1024 * 1024
	maxConfigParentEntries        = 4096
	maxConfigRootEntries          = 4096
	maxAuthTreeEntries            = 4096
	maxCoordinatorEvidenceEntries = 1024
	maxPreparationRemnants        = 128
)

var transactionPhaseHook func(TransactionPhase) error

// Coordinator does all reads and writes of the config file and the saved
// logins for one command. A change to both happens under a lock and goes
// through a journal, so an interrupted change can be finished or undone on
// the next run.
type Coordinator struct {
	configFile          string
	root                string
	registryName        string
	recognizedProviders []string
}

func NewCoordinator(configFile string, recognizedProviders []string) (*Coordinator, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	if configFile == "" {
		return nil, coordinatorConfigError()
	}
	if len(recognizedProviders) == 0 || len(recognizedProviders) > maildoconfig.MaxRecognizedProviders {
		return nil, coordinatorConfigError()
	}
	seen := map[string]bool{}
	providers := make([]string, len(recognizedProviders))
	for i, name := range recognizedProviders {
		if !maildoconfig.IsValidProviderName(name) || seen[name] {
			return nil, coordinatorConfigError()
		}
		seen[name] = true
		providers[i] = name
	}
	clean := filepath.Clean(configFile)
	base := filepath.Base(clean)
	if !isPortableComponent(base) || strings.EqualFold(base, "auth") || !fitsDerivedBasenameBudget(base) {
		return nil, coordinatorConfigError()
	}
	root := filepath.Dir(clean)
	if rootBase := filepath.Base(root); rootBase != "." && rootBase != string(filepath.Separator) && !isPortableComponent(rootBase) {
		return nil, coordinatorConfigError()
	}
	return &Coordinator{
		configFile:          clean,
		root:                root,
		registryName:        base,
		recognizedProviders: providers,
	}, nil
}

func (c *Coordinator) LoadRegistry(ctx context.Context, dryRun bool) (*maildoconfig.Registry, error) {
	if dryRun && c.locklessDryRunEligible() {
		return maildoconfig.Load(c.configFile, c.recognizedProviders)
	}
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer locked.unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := locked.recover(); err != nil {
		return nil, err
	}
	reg, _, _, err := locked.readRegistry()
	return reg, err
}

func (c *Coordinator) CleanupStale(ctx context.Context) error {
	locked, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return err
	}
	if err := NewStore(c.root).CleanupStale(c.configFile); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) CredentialExists(ctx context.Context, provider, account string, dryRun bool) (bool, error) {
	if dryRun && c.locklessDryRunEligible() {
		return NewStore(c.root).Exists(provider, account)
	}
	locked, err := c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return false, err
	}
	return NewStore(c.root).Exists(provider, account)
}

func (c *Coordinator) Snapshot(ctx context.Context, provider, account string) (*maildoconfig.Registry, StateRevision, []byte, bool, error) {
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, StateRevision{}, nil, false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, StateRevision{}, nil, false, err
	}
	reg, raw, exists, err := locked.readRegistry()
	if err != nil {
		return nil, StateRevision{}, nil, false, err
	}
	data, present, err := NewStore(c.root).LoadBytes(provider, account)
	if err != nil {
		return nil, StateRevision{}, nil, false, err
	}
	if err := checkedSnapshotBytes(raw, data); err != nil {
		zero(data)
		return nil, StateRevision{}, nil, false, err
	}
	rev := newStateRevision(exists, raw, reg, provider, account, data, present)
	return reg, rev, data, present, nil
}

func (c *Coordinator) LoadCredentialBytes(ctx context.Context, provider, account string) ([]byte, bool, error) {
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, false, err
	}
	return NewStore(c.root).LoadBytes(provider, account)
}

func (c *Coordinator) SaveCredentialBytes(ctx context.Context, rev StateRevision, provider, account string, data []byte, operation errs.AuthStateOperation) error {
	locked, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return err
	}
	if err := locked.compareRevision(rev, operation); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return NewStore(c.root).SaveBytes(provider, account, data)
}

func (c *Coordinator) DeleteCredential(ctx context.Context, provider, account string) (bool, error) {
	locked, err := c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return NewStore(c.root).Delete(provider, account)
}

func (c *Coordinator) SaveRegistry(ctx context.Context, reg *maildoconfig.Registry) error {
	locked, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return maildoconfig.Save(c.configFile, reg, c.recognizedProviders)
}

// UpdateRegistry changes the registry under the lock: it reads the current
// file, lets fn change it, and saves the result.
func (c *Coordinator) UpdateRegistry(ctx context.Context, fn func(*maildoconfig.Registry) error) (*maildoconfig.Registry, error) {
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, err
	}
	reg, _, _, err := locked.readRegistry()
	if err != nil {
		return nil, err
	}
	if err := fn(reg); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := maildoconfig.Save(c.configFile, reg, c.recognizedProviders); err != nil {
		return nil, err
	}
	return reg, nil
}

func (c *Coordinator) SwitchActive(ctx context.Context, name string) (*maildoconfig.Registry, maildoconfig.Account, error) {
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, maildoconfig.Account{}, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, maildoconfig.Account{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, maildoconfig.Account{}, err
	}
	reg, _, _, err := locked.readRegistry()
	if err != nil {
		return nil, maildoconfig.Account{}, err
	}
	account, ok := reg.Find(name)
	if !ok {
		return nil, maildoconfig.Account{}, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", name))
	}
	selected := *account
	reg.SetActive(name)
	if err := maildoconfig.Save(c.configFile, reg, c.recognizedProviders); err != nil {
		return nil, maildoconfig.Account{}, err
	}
	return reg, selected, nil
}

func (c *Coordinator) LogoutAccount(ctx context.Context, name string, logout LocalLogoutFunc) (*maildoconfig.Registry, maildoconfig.Account, bool, error) {
	if logout == nil {
		return nil, maildoconfig.Account{}, false, errs.New(errs.CodeInvalidConfig, "auth coordinator configuration is invalid")
	}
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	reg, _, _, err := locked.readRegistry()
	if err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	account, ok := reg.Find(name)
	if !ok {
		return nil, maildoconfig.Account{}, false, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", name))
	}
	data, present, err := NewStore(c.root).LoadBytes(account.Provider, account.Name)
	if err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	defer zero(data)
	deleted, err := logout(*account, reg, data, present)
	if err != nil {
		return nil, maildoconfig.Account{}, false, err
	}
	if deleted {
		if _, err := NewStore(c.root).Delete(account.Provider, account.Name); err != nil {
			return nil, maildoconfig.Account{}, false, err
		}
	}
	return reg, *account, deleted, nil
}

type TransactionRequest struct {
	Operation       errs.AuthTransactionOperation
	Revision        StateRevision
	Registry        *maildoconfig.Registry
	OldBinding      CredentialBinding
	NewBinding      CredentialBinding
	DesiredCache    []byte
	DesiredCacheSet bool
}

type CredentialRebindFunc func(source, dest CredentialBinding, data []byte, present bool) (bool, []byte, error)

type LocalLogoutFunc func(account maildoconfig.Account, reg *maildoconfig.Registry, data []byte, present bool) (bool, error)

func (c *Coordinator) CommitTransaction(ctx context.Context, req TransactionRequest, conflict errs.AuthStateOperation) error {
	locked, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return err
	}
	if err := locked.compareRevision(req.Revision, conflict); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return locked.commitTransaction(req)
}

func (c *Coordinator) RenameAccount(ctx context.Context, oldName, newName string, rebind CredentialRebindFunc) (*maildoconfig.Registry, bool, error) {
	if rebind == nil {
		return nil, false, errs.New(errs.CodeInvalidConfig, "auth coordinator configuration is invalid")
	}
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	reg, _, _, err := locked.readRegistry()
	if err != nil {
		return nil, false, err
	}
	account, ok := reg.Find(oldName)
	if !ok {
		return nil, false, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", oldName))
	}
	if _, exists := reg.Find(newName); exists {
		return nil, false, errs.New(errs.CodeAccountAlreadyExists, fmt.Sprintf("account %q already exists", newName))
	}
	if exists, err := NewStore(c.root).Exists(account.Provider, newName); err != nil {
		return nil, false, err
	} else if exists {
		return nil, false, errs.New(errs.CodeAccountAlreadyExists, fmt.Sprintf("a saved login for %q already exists", newName))
	}
	data, present, err := NewStore(c.root).LoadBytes(account.Provider, oldName)
	if err != nil {
		return nil, false, err
	}
	defer zero(data)
	source := CredentialBinding{Provider: account.Provider, Account: oldName}
	dest := CredentialBinding{Provider: account.Provider, Account: newName}
	migrated, desired, err := rebind(source, dest, data, present)
	if err != nil {
		zero(desired)
		return nil, false, err
	}
	defer zero(desired)
	if !migrated {
		desired = nil
	}
	providerName := account.Provider
	reg.Rename(oldName, newName)
	tx := TransactionRequest{
		Operation:       errs.AuthTransactionRename,
		Registry:        reg,
		OldBinding:      source,
		NewBinding:      dest,
		DesiredCache:    desired,
		DesiredCacheSet: migrated,
	}
	if err := locked.commitTransaction(tx); err != nil {
		return nil, false, err
	}
	if renamed, ok := reg.Find(newName); ok {
		renamed.Provider = providerName
	}
	return reg, migrated, nil
}

func (c *Coordinator) RemoveAccount(ctx context.Context, name string, force bool, setActive string, logout LocalLogoutFunc) (*maildoconfig.Registry, maildoconfig.Account, string, bool, error) {
	if logout == nil {
		return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeInvalidConfig, "auth coordinator configuration is invalid")
	}
	locked, err := c.lock(ctx)
	if err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	defer locked.unlock()
	if err := locked.recover(); err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	reg, _, _, err := locked.readRegistry()
	if err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	account, ok := reg.Find(name)
	if !ok {
		return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", name))
	}
	if force && setActive != "" {
		return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeInvalidActiveAccount, "--force and --set-active cannot be used together")
	}
	removingActive := reg.ActiveAccount == name
	if removingActive && !force && setActive == "" {
		return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeInvalidActiveAccount, fmt.Sprintf("%q is the active account; name the next active account with --set-active <name>, or pass --force to leave no account active", name))
	}
	activeAfter := reg.ActiveAccount
	activeEffect := "unchanged"
	if setActive != "" {
		if setActive == name {
			return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeInvalidActiveAccount, "--set-active cannot name the account you are removing")
		}
		if _, ok := reg.Find(setActive); !ok {
			return nil, maildoconfig.Account{}, "", false, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", setActive))
		}
		activeAfter = setActive
		activeEffect = "set:" + setActive
	} else if force && removingActive {
		activeAfter = ""
		activeEffect = "cleared"
	}
	data, present, err := NewStore(c.root).LoadBytes(account.Provider, account.Name)
	if err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	defer zero(data)
	deleted, err := logout(*account, reg, data, present)
	if err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	removed := *account
	reg.Remove(name)
	reg.ActiveAccount = activeAfter
	if err := locked.commitTransaction(TransactionRequest{
		Operation:       errs.AuthTransactionRemove,
		Registry:        reg,
		OldBinding:      CredentialBinding{Provider: removed.Provider, Account: removed.Name},
		NewBinding:      CredentialBinding{},
		DesiredCacheSet: false,
	}); err != nil {
		return nil, maildoconfig.Account{}, "", false, err
	}
	return reg, removed, activeEffect, deleted, nil
}

type lockedCoordinator struct {
	c    *Coordinator
	file *os.File
}

func (c *Coordinator) lock(ctx context.Context) (*lockedCoordinator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Refuse a folder that is not private before EnsureDir can change its
	// mode, so the user learns that others could open it.
	if info, err := os.Lstat(filepath.Join(c.root, "auth")); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || managedDirectoryModeInvalid(info.Mode()) {
			return nil, errs.New(errs.CodeInvalidConfig, "the auth folder in maildo's config folder must be a real folder (not a link) that only you can open; make it private with chmod 700 and check who could read it")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not check the auth folder in its config folder")
	}
	if err := fsutil.EnsureDir(filepath.Join(c.root, "auth"), 0o700); err != nil {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not create the auth folder in its config folder")
	}
	lockPath := filepath.Join(c.root, "auth", ".transaction.lock")
	file, err := openLockFile(lockPath)
	if err != nil {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not open its lock file in the auth folder")
	}
	delays := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	attempt := 0
	for {
		ok, err := fsutil.TryLockFile(file)
		if err != nil {
			_ = file.Close()
			return nil, errs.New(errs.CodeInvalidConfig, "maildo could not lock its auth folder")
		}
		if ok {
			return &lockedCoordinator{c: c, file: file}, nil
		}
		delay := 50 * time.Millisecond
		if attempt < len(delays) {
			delay = delays[attempt]
		}
		attempt++
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func openLockFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid lock file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) || lockFileModeInvalid(fileInfo.Mode()) {
		return nil, fmt.Errorf("invalid lock file")
	}
	closeOnError = false
	return file, nil
}

func (l *lockedCoordinator) unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := fsutil.UnlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not unlock its auth folder")
	}
	if closeErr != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not unlock its auth folder")
	}
	return nil
}

func (l *lockedCoordinator) readRegistry() (*maildoconfig.Registry, []byte, bool, error) {
	raw, err := readConfigLimited(l.c.configFile, maildoconfig.MaxRegistryBytes)
	if errors.Is(err, os.ErrNotExist) {
		return &maildoconfig.Registry{Version: 1}, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, errs.New(errs.CodeInvalidConfig, "maildo could not read its config file")
	}
	reg, err := maildoconfig.Decode(raw, l.c.recognizedProviders)
	if err != nil {
		zero(raw)
		return nil, nil, false, err
	}
	return reg, raw, true, nil
}

func (l *lockedCoordinator) compareRevision(rev StateRevision, operation errs.AuthStateOperation) error {
	_, raw, exists, err := l.readRegistry()
	if err != nil {
		return err
	}
	defer zero(raw)
	cache, present, err := NewStore(l.c.root).LoadBytes(rev.provider, rev.account)
	if err != nil {
		return err
	}
	defer zero(cache)
	reg := &maildoconfig.Registry{Version: 1}
	if exists {
		var err error
		reg, err = maildoconfig.Decode(raw, l.c.recognizedProviders)
		if err != nil {
			return err
		}
	}
	current := newStateRevision(exists, raw, reg, rev.provider, rev.account, cache, present)
	if !rev.equal(current) {
		return errs.NewAuthStateChanged(rev.account, rev.provider, operation)
	}
	return nil
}

func (l *lockedCoordinator) commitTransaction(req TransactionRequest) error {
	txid, err := newTxID(filepath.Join(l.c.root, "auth", ".transactions"))
	if err != nil {
		return errs.NewAuthTransactionStartFailure(req.Operation)
	}
	txRoot := filepath.Join(l.c.root, "auth", ".transactions")
	if err := fsutil.EnsureDir(txRoot, 0o700); err != nil {
		return errs.NewAuthTransactionStartFailure(req.Operation)
	}
	txDir := filepath.Join(txRoot, txid)
	prepareDir := filepath.Join(txRoot, ".prepare-"+txid)
	if err := os.Mkdir(prepareDir, 0o700); err != nil {
		return errs.NewAuthTransactionStartFailure(req.Operation)
	}
	cleanupTX := true
	evidenceDurable := false
	cleanupPath := prepareDir
	defer func() {
		if cleanupTX && !evidenceDurable {
			_ = os.RemoveAll(cleanupPath)
		}
	}()
	priorReg, _, priorRegExists, err := l.readRegistry()
	if err != nil {
		return err
	}
	_ = priorReg
	priorRegBytes, err := os.ReadFile(l.c.configFile)
	if errors.Is(err, os.ErrNotExist) {
		priorRegBytes = nil
	} else if err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not read its config file")
	}
	desiredRegBytes, err := maildoconfig.Encode(req.Registry, l.c.recognizedProviders)
	if err != nil {
		return err
	}
	oldProvider, oldAccount := req.OldBinding.Provider, req.OldBinding.Account
	newProvider, newAccount := req.NewBinding.Provider, req.NewBinding.Account
	if newProvider == "" {
		newProvider, newAccount = oldProvider, oldAccount
	}
	priorCache, priorCachePresent, err := NewStore(l.c.root).LoadBytes(oldProvider, oldAccount)
	if err != nil {
		return err
	}
	defer zero(priorRegBytes)
	defer zero(desiredRegBytes)
	defer zero(priorCache)

	journal := Journal{
		Version:      1,
		TxID:         txid,
		Operation:    req.Operation,
		Phase:        TransactionPrepared,
		RegistryName: l.c.registryName,
		OldBinding:   req.OldBinding,
		NewBinding:   req.NewBinding,
	}
	if priorRegExists {
		if err := writeMaterial(prepareDir, "registry.prior", priorRegBytes); err != nil {
			return err
		}
		journal.Registry.Prior = presentImage("registry.prior", priorRegBytes)
	} else {
		journal.Registry.Prior = absentImage()
	}
	if err := writeMaterial(prepareDir, "registry.desired", desiredRegBytes); err != nil {
		return err
	}
	journal.Registry.Desired = presentImage("registry.desired", desiredRegBytes)
	journal.Cache.OldPath = cacheRelPath(oldProvider, oldAccount)
	if req.NewBinding.Provider != "" {
		journal.Cache.NewPath = cacheRelPath(newProvider, newAccount)
	}
	if priorCachePresent {
		if err := writeMaterial(prepareDir, "cache.prior", priorCache); err != nil {
			return err
		}
		journal.Cache.Prior = presentImage("cache.prior", priorCache)
	} else {
		journal.Cache.Prior = absentImage()
	}
	if req.DesiredCacheSet {
		if len(req.DesiredCache) > maxCredentialBytes {
			return credentialError("save", newAccount)
		}
		if err := writeMaterial(prepareDir, "cache.desired", req.DesiredCache); err != nil {
			return err
		}
		journal.Cache.Desired = presentImage("cache.desired", req.DesiredCache)
	} else {
		journal.Cache.Desired = absentImage()
	}
	if err := writeJournal(prepareDir, journal); err != nil {
		return err
	}
	if err := os.Rename(prepareDir, txDir); err != nil {
		return errs.NewAuthTransactionStartFailure(req.Operation)
	}
	if err := fsutil.SyncDir(txRoot); err != nil {
		return errs.NewAuthTransactionStartFailure(req.Operation)
	}
	cleanupPath = txDir
	evidenceDurable = true
	if req.DesiredCacheSet {
		if err := NewStore(l.c.root).SaveBytes(newProvider, newAccount, req.DesiredCache); err != nil {
			return l.failTransaction(txDir, err)
		}
	} else {
		if _, err := NewStore(l.c.root).Delete(oldProvider, oldAccount); err != nil {
			return l.failTransaction(txDir, err)
		}
	}
	journal.Phase = TransactionCacheInstalled
	if err := runTransactionPhaseHook(journal.Phase); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := writeJournal(txDir, journal); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := maildoconfig.Save(l.c.configFile, req.Registry, l.c.recognizedProviders); err != nil {
		return l.failTransaction(txDir, err)
	}
	journal.Phase = TransactionRegistryInstalled
	if err := runTransactionPhaseHook(journal.Phase); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := writeJournal(txDir, journal); err != nil {
		return l.failTransaction(txDir, err)
	}
	if req.DesiredCacheSet && (oldProvider != newProvider || oldAccount != newAccount) {
		if _, err := NewStore(l.c.root).Delete(oldProvider, oldAccount); err != nil {
			return l.failTransaction(txDir, err)
		}
	}
	journal.Phase = TransactionObsoleteCacheRemoved
	if err := runTransactionPhaseHook(journal.Phase); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := writeJournal(txDir, journal); err != nil {
		return l.failTransaction(txDir, err)
	}
	journal.Phase = TransactionCommitted
	if err := runTransactionPhaseHook(journal.Phase); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := writeJournal(txDir, journal); err != nil {
		return l.failTransaction(txDir, err)
	}
	if err := os.RemoveAll(txDir); err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not remove leftover files from an earlier login change")
	}
	cleanupTX = false
	return nil
}

func (l *lockedCoordinator) failTransaction(txDir string, original error) error {
	if original == nil {
		return nil
	}
	if err := l.recoverTransaction(txDir); err != nil {
		return err
	}
	return original
}

func runTransactionPhaseHook(phase TransactionPhase) error {
	if transactionPhaseHook == nil {
		return nil
	}
	return transactionPhaseHook(phase)
}

func (l *lockedCoordinator) recover() error {
	if err := l.rejectReservedRemnants(); err != nil {
		return err
	}
	txRoot := filepath.Join(l.c.root, "auth", ".transactions")
	entries, err := os.ReadDir(txRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errs.New(errs.CodeInvalidConfig, "config could not be read")
	}
	active := 0
	prepares := 0
	evidence := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".prepare-") {
			prepares++
			evidence++
			if prepares > maxPreparationRemnants || evidence > maxCoordinatorEvidenceEntries {
				return errs.NewAuthRecoveryEvidenceInvalid()
			}
			if !validTxID(strings.TrimPrefix(name, ".prepare-")) || !entry.IsDir() {
				return errs.NewAuthRecoveryEvidenceInvalid()
			}
			if err := os.RemoveAll(filepath.Join(txRoot, name)); err != nil {
				return errs.New(errs.CodeInvalidConfig, "maildo could not remove leftover files from an earlier login change")
			}
			continue
		}
		if strings.HasPrefix(name, ".garbage-") {
			evidence++
			if evidence > maxCoordinatorEvidenceEntries {
				return errs.NewAuthRecoveryEvidenceInvalid()
			}
			if !entry.IsDir() {
				return errs.NewAuthRecoveryEvidenceInvalid()
			}
			if err := os.RemoveAll(filepath.Join(txRoot, name)); err != nil {
				return errs.New(errs.CodeInvalidConfig, "maildo could not remove leftover files from an earlier login change")
			}
			continue
		}
		if validTxID(name) && entry.IsDir() {
			active++
			evidence++
			if active > 1 || evidence > maxCoordinatorEvidenceEntries {
				return errs.NewAuthRecoveryEvidenceInvalid()
			}
			if err := l.recoverTransaction(filepath.Join(txRoot, name)); err != nil {
				return err
			}
			continue
		}
		return errs.NewAuthRecoveryEvidenceInvalid()
	}
	return nil
}

func (l *lockedCoordinator) rejectReservedRemnants() error {
	txRoot := filepath.Join(l.c.root, "auth", ".transactions")
	err := filepath.WalkDir(l.c.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == l.c.root {
			return nil
		}
		if path == txRoot {
			return filepath.SkipDir
		}
		name := entry.Name()
		if name == ".transaction.lock" {
			return nil
		}
		if name == ".transactions" && path == txRoot {
			return filepath.SkipDir
		}
		if reservedCoordinatorRemnant(name) {
			return errs.NewAuthRecoveryEvidenceInvalid()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (l *lockedCoordinator) recoverTransaction(dir string) error {
	journal, err := readJournal(l.c.root, dir)
	if err != nil {
		return errs.NewAuthRecoveryEvidenceInvalid()
	}
	if journal.RegistryName != l.c.registryName {
		return errs.NewAuthRecoveryEvidenceInvalid()
	}
	rollForward := journal.Phase == TransactionRegistryInstalled || journal.Phase == TransactionObsoleteCacheRemoved || journal.Phase == TransactionCommitted
	if rollForward {
		if err := l.applyJournalDesired(dir, journal); err != nil {
			return errs.NewAuthRecoveryRequired(journal.Operation, errs.RecoveryRollForward)
		}
	} else {
		if err := l.applyJournalPrior(dir, journal); err != nil {
			return errs.NewAuthRecoveryRequired(journal.Operation, errs.RecoveryRollBack)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not remove leftover files from an earlier login change")
	}
	return nil
}

func (l *lockedCoordinator) applyJournalPrior(dir string, journal Journal) error {
	if err := restoreRegistryImage(filepath.Join(l.c.root, journal.RegistryName), dir, journal.Registry.Prior, l.c.recognizedProviders); err != nil {
		return err
	}
	if journal.Cache.OldPath != "" {
		if err := restoreCacheImage(l.c.root, dir, journal.Cache.OldPath, journal.Cache.Prior); err != nil {
			return err
		}
	}
	if journal.Cache.NewPath != "" && journal.Cache.NewPath != journal.Cache.OldPath {
		if err := removeCacheRel(l.c.root, journal.Cache.NewPath); err != nil {
			return err
		}
	}
	return nil
}

func (l *lockedCoordinator) applyJournalDesired(dir string, journal Journal) error {
	if err := restoreRegistryImage(filepath.Join(l.c.root, journal.RegistryName), dir, journal.Registry.Desired, l.c.recognizedProviders); err != nil {
		return err
	}
	target := journal.Cache.NewPath
	if target == "" {
		target = journal.Cache.OldPath
	}
	if target != "" {
		if err := restoreCacheImage(l.c.root, dir, target, journal.Cache.Desired); err != nil {
			return err
		}
	}
	if journal.Cache.OldPath != "" && journal.Cache.NewPath != "" && journal.Cache.OldPath != journal.Cache.NewPath {
		if err := removeCacheRel(l.c.root, journal.Cache.OldPath); err != nil {
			return err
		}
	}
	return nil
}

type StateRevision struct {
	registryExists bool
	registryHash   [32]byte
	account        string
	provider       string
	authRef        string
	setupHash      [32]byte
	email          string
	providerID     string
	active         string
	cacheExists    bool
	cacheHash      [32]byte
}

func newStateRevision(registryExists bool, raw []byte, reg *maildoconfig.Registry, providerName, accountName string, cache []byte, cacheExists bool) StateRevision {
	var rev StateRevision
	rev.registryExists = registryExists
	rev.registryHash = sha256.Sum256(raw)
	rev.provider = providerName
	rev.account = accountName
	rev.cacheExists = cacheExists
	rev.cacheHash = sha256.Sum256(cache)
	if reg != nil {
		rev.active = reg.ActiveAccount
		if account, ok := reg.Find(accountName); ok {
			rev.authRef = account.AuthRef
			rev.email = account.Email
			rev.providerID = account.ProviderAccountID
			setup, _ := maildoconfig.SetupFor(reg, *account)
			encoded, _ := json.Marshal(setup.Clone())
			rev.setupHash = sha256.Sum256(encoded)
			rev.provider = account.Provider
		}
	}
	return rev
}

func (r StateRevision) equal(other StateRevision) bool {
	return r.registryExists == other.registryExists &&
		r.registryHash == other.registryHash &&
		r.account == other.account &&
		r.provider == other.provider &&
		r.authRef == other.authRef &&
		r.setupHash == other.setupHash &&
		r.email == other.email &&
		r.providerID == other.providerID &&
		r.active == other.active &&
		r.cacheExists == other.cacheExists &&
		r.cacheHash == other.cacheHash
}

type Journal struct {
	Version      int                           `json:"version"`
	TxID         string                        `json:"txid"`
	Operation    errs.AuthTransactionOperation `json:"operation"`
	Phase        TransactionPhase              `json:"phase"`
	RegistryName string                        `json:"registry_name"`
	Registry     RegistryImages                `json:"registry"`
	Cache        CacheImages                   `json:"cache"`
	OldBinding   CredentialBinding             `json:"old_binding"`
	NewBinding   CredentialBinding             `json:"new_binding"`
}

type FileImage struct {
	Present  bool         `json:"present"`
	Material string       `json:"material"`
	SHA256   string       `json:"sha256"`
	Length   uint64       `json:"length"`
	Security FileSecurity `json:"security"`
}

type FileSecurity struct {
	Kind     FileSecurityKind `json:"kind"`
	UnixMode uint32           `json:"unix_mode,omitempty"`
}

type FileSecurityKind string

const (
	FileSecurityAbsent   FileSecurityKind = "absent"
	FileSecurityUnixMode FileSecurityKind = "unix_mode"
)

type RegistryImages struct {
	Prior   FileImage `json:"prior"`
	Desired FileImage `json:"desired"`
}

type CacheImages struct {
	OldPath           string                      `json:"old_path,omitempty"`
	NewPath           string                      `json:"new_path,omitempty"`
	Prior             FileImage                   `json:"prior"`
	Desired           FileImage                   `json:"desired"`
	RebindDisposition CredentialRebindDisposition `json:"rebind_disposition,omitempty"`
}

type TransactionPhase string

const (
	TransactionPrepared             TransactionPhase = "prepared"
	TransactionCacheInstalled       TransactionPhase = "cache_installed"
	TransactionRegistryInstalled    TransactionPhase = "registry_installed"
	TransactionObsoleteCacheRemoved TransactionPhase = "obsolete_cache_removed"
	TransactionCommitted            TransactionPhase = "committed"
)

func writeMaterial(dir, name string, data []byte) error {
	if !validMaterialName(name) {
		return errs.NewAuthRecoveryEvidenceInvalid()
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, name), data, 0o600); err != nil {
		return errs.New(errs.CodeInvalidConfig, "auth transaction could not be saved")
	}
	return nil
}

func writeJournal(dir string, journal Journal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxJournalBytes {
		return errs.NewAuthRecoveryEvidenceInvalid()
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "journal.json"), append(data, '\n'), 0o600); err != nil {
		return errs.New(errs.CodeInvalidConfig, "auth transaction could not be saved")
	}
	return nil
}

func readJournal(root, dir string) (Journal, error) {
	dirs, err := transactionMaterialDirs(root, dir)
	if err != nil {
		return Journal{}, err
	}
	data, err := readLimited(root, dirs, "journal.json", maxJournalBytes)
	if err != nil {
		return Journal{}, err
	}
	defer zero(data)
	var journal Journal
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&journal); err != nil {
		return Journal{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Journal{}, fmt.Errorf("trailing journal data")
	}
	if journal.Version != 1 || !validTxID(journal.TxID) || journal.TxID != filepath.Base(dir) || !validTransactionPhase(journal.Phase) || !validTransactionOperation(journal.Operation) {
		return Journal{}, fmt.Errorf("invalid journal")
	}
	if err := validateJournal(journal); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

func validateJournal(journal Journal) error {
	if journal.RegistryName == "" || !isPortableComponent(journal.RegistryName) {
		return fmt.Errorf("invalid journal registry")
	}
	if err := validateBinding(journal.OldBinding, true); err != nil {
		return err
	}
	if err := validateBinding(journal.NewBinding, false); err != nil {
		return err
	}
	if journal.Cache.OldPath != cacheRelPath(journal.OldBinding.Provider, journal.OldBinding.Account) {
		return fmt.Errorf("invalid old cache path")
	}
	if journal.NewBinding.Provider == "" {
		if journal.Cache.NewPath != "" {
			return fmt.Errorf("invalid new cache path")
		}
	} else if journal.Cache.NewPath != cacheRelPath(journal.NewBinding.Provider, journal.NewBinding.Account) {
		return fmt.Errorf("invalid new cache path")
	}
	if err := validateFileImage(journal.Registry.Prior, maildoconfig.MaxRegistryBytes, false); err != nil {
		return err
	}
	if err := validateFileImage(journal.Registry.Desired, maildoconfig.MaxRegistryBytes, true); err != nil {
		return err
	}
	if err := validateFileImage(journal.Cache.Prior, maxCredentialBytes, false); err != nil {
		return err
	}
	if err := validateFileImage(journal.Cache.Desired, maxCredentialBytes, false); err != nil {
		return err
	}
	return nil
}

func validateBinding(binding CredentialBinding, required bool) error {
	if binding.Provider == "" && binding.Account == "" && !required {
		return nil
	}
	if !maildoconfig.IsValidProviderName(binding.Provider) || !maildoconfig.IsValidAccountName(binding.Account) {
		return fmt.Errorf("invalid credential binding")
	}
	return nil
}

func validateFileImage(image FileImage, limit int64, required bool) error {
	if !image.Present {
		if required || image.Material != "" || image.SHA256 != "" || image.Length != 0 || image.Security.Kind != FileSecurityAbsent {
			return fmt.Errorf("invalid absent image")
		}
		return nil
	}
	if image.Material == "" || !validMaterialName(image.Material) || image.Length > uint64(limit) {
		return fmt.Errorf("invalid present image")
	}
	if len(image.SHA256) != sha256.Size*2 {
		return fmt.Errorf("invalid image hash")
	}
	if _, err := hex.DecodeString(image.SHA256); err != nil {
		return fmt.Errorf("invalid image hash")
	}
	if image.Security.Kind != FileSecurityUnixMode || image.Security.UnixMode != 0o600 {
		return fmt.Errorf("invalid image security")
	}
	return nil
}

func validMaterialName(name string) bool {
	switch name {
	case "registry.prior", "registry.desired", "cache.prior", "cache.desired":
		return true
	default:
		return false
	}
}

func presentImage(material string, data []byte) FileImage {
	sum := sha256.Sum256(data)
	return FileImage{
		Present:  true,
		Material: material,
		SHA256:   hex.EncodeToString(sum[:]),
		Length:   uint64(len(data)),
		Security: FileSecurity{Kind: FileSecurityUnixMode, UnixMode: 0o600},
	}
}

func absentImage() FileImage {
	return FileImage{Security: FileSecurity{Kind: FileSecurityAbsent}}
}

func restoreRegistryImage(path, dir string, image FileImage, recognized []string) error {
	if !image.Present {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := readImageMaterial(filepath.Dir(path), dir, image, maildoconfig.MaxRegistryBytes)
	if err != nil {
		return err
	}
	defer zero(data)
	if _, err := maildoconfig.Decode(data, recognized); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

func restoreCacheImage(root, dir, rel string, image FileImage) error {
	if !image.Present {
		return removeCacheRel(root, rel)
	}
	data, err := readImageMaterial(root, dir, image, maxCredentialBytes)
	if err != nil {
		return err
	}
	defer zero(data)
	provider, account, err := parseCacheRel(rel)
	if err != nil {
		return err
	}
	return NewStore(root).SaveBytes(provider, account, data)
}

func readImageMaterial(root, dir string, image FileImage, limit int64) ([]byte, error) {
	if image.Material == "" || strings.Contains(image.Material, "/") || strings.Contains(image.Material, `\`) {
		return nil, fmt.Errorf("invalid material")
	}
	dirs, err := transactionMaterialDirs(root, dir)
	if err != nil {
		return nil, err
	}
	data, err := readLimited(root, dirs, image.Material, limit)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if image.Length != uint64(len(data)) || image.SHA256 != hex.EncodeToString(sum[:]) {
		zero(data)
		return nil, fmt.Errorf("material mismatch")
	}
	return data, nil
}

func transactionMaterialDirs(root, dir string) ([]string, error) {
	dirs := []string{"auth", ".transactions", filepath.Base(dir)}
	if filepath.Clean(dir) != filepath.Join(root, dirs[0], dirs[1], dirs[2]) {
		return nil, fmt.Errorf("invalid material directory")
	}
	return dirs, nil
}

func cacheRelPath(provider, account string) string {
	return provider + "/" + account + ".json"
}

func parseCacheRel(rel string) (string, string, error) {
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
		return "", "", fmt.Errorf("invalid cache path")
	}
	provider := parts[0]
	account := strings.TrimSuffix(parts[1], ".json")
	if !maildoconfig.IsValidProviderName(provider) || !maildoconfig.IsValidAccountName(account) {
		return "", "", fmt.Errorf("invalid cache path")
	}
	return provider, account, nil
}

func removeCacheRel(root, rel string) error {
	provider, account, err := parseCacheRel(rel)
	if err != nil {
		return err
	}
	_, err = NewStore(root).Delete(provider, account)
	return err
}

func checkedSnapshotBytes(registry, credential []byte) error {
	total := len(registry)
	if len(credential) > maxDetachedSnapshotBytes-total {
		zero(credential)
		return errs.New(errs.CodeInvalidConfig, "config could not be read")
	}
	return nil
}

func newTxID(txRoot string) (string, error) {
	for i := 0; i < 8; i++ {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		if _, err := os.Stat(filepath.Join(txRoot, id)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := os.Stat(filepath.Join(txRoot, ".prepare-"+id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
	}
	return "", fmt.Errorf("transaction id collision")
}

var txIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validTxID(id string) bool {
	return txIDRE.MatchString(id)
}

func validTransactionPhase(phase TransactionPhase) bool {
	switch phase {
	case TransactionPrepared, TransactionCacheInstalled, TransactionRegistryInstalled, TransactionObsoleteCacheRemoved, TransactionCommitted:
		return true
	default:
		return false
	}
}

func validTransactionOperation(operation errs.AuthTransactionOperation) bool {
	switch operation {
	case errs.AuthTransactionLogin, errs.AuthTransactionRepair, errs.AuthTransactionRename, errs.AuthTransactionRemove:
		return true
	default:
		return false
	}
}

func (c *Coordinator) locklessDryRunEligible() bool {
	if hasCoordinatorEvidence(c.root) {
		return false
	}
	return true
}

func hasCoordinatorEvidence(root string) bool {
	authRoot := filepath.Join(root, "auth")
	for _, path := range []string{
		filepath.Join(authRoot, ".transaction.lock"),
		filepath.Join(authRoot, ".transactions"),
	} {
		if _, err := os.Lstat(path); err == nil {
			return true
		}
	}
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if reservedCoordinatorRemnant(name) {
			found = true
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

func reservedCoordinatorRemnant(name string) bool {
	return strings.HasPrefix(name, ".maildo-gc-") ||
		strings.HasPrefix(name, ".prepare-") ||
		strings.HasPrefix(name, ".garbage-") ||
		strings.HasSuffix(name, ".next") ||
		strings.HasSuffix(name, ".removed")
}

func isPortableComponent(name string) bool {
	if name == "." || name == ".." || name == "" || !utf8.ValidString(name) {
		return false
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || maildoconfig.IsWin32DeviceBasename(name) {
		return false
	}
	for _, r := range name {
		switch r {
		case 0, '/', '\\', ':', '<', '>', '"', '|', '?', '*':
			return false
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func fitsDerivedBasenameBudget(name string) bool {
	longest := "." + name + ".maildo-single-0123456789abcdef0123456789abcdef-registry.removed"
	return len(longest) <= 255 && len(utf16.Encode([]rune(longest))) <= 255
}

func coordinatorConfigError() error {
	return errs.New(errs.CodeInvalidConfig, "auth coordinator configuration is invalid")
}
