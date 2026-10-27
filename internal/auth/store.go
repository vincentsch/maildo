// Package auth saves logins (passwords and tokens) in private files and
// changes them together with the config file.
//
// It treats a saved login as plain bytes and never decodes it; each provider
// does that. File paths come only from checked provider and account names,
// never from auth_ref in the config.
package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/fsutil"
)

const maxCredentialBytes = 1 * 1024 * 1024

// MaxCredentialBytes is the largest saved login maildo accepts.
const MaxCredentialBytes = maxCredentialBytes

type CredentialBinding struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
}

type CredentialRebindDisposition string

const (
	CredentialReboundUsable        CredentialRebindDisposition = "rebound_usable"
	CredentialReboundNeedsRepair   CredentialRebindDisposition = "rebound_repair_required"
	CredentialPreservedNeedsRepair CredentialRebindDisposition = "preserved_repair_required"
)

type CredentialRebindResult struct {
	Bytes       []byte
	Disposition CredentialRebindDisposition
}

// Store reads and writes saved logins in the auth folder next to the config
// file.
type Store struct {
	root string
}

func NewStore(root string) Store {
	return Store{root: root}
}

// Exists reports whether the account has a saved login.
func (s Store) Exists(provider, account string) (bool, error) {
	if err := validatePathNames(provider, account); err != nil {
		return false, err
	}
	file, err := openManagedRead(s.root, []string{"auth", provider}, account+".json")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, credentialError("stat", account)
	}
	defer file.close()
	if err := file.validate(); err != nil {
		return false, credentialError("stat", account)
	}
	return true, nil
}

// LoadBytes returns a copy of the saved login that the caller may change and
// should wipe.
func (s Store) LoadBytes(provider, account string) ([]byte, bool, error) {
	if err := validatePathNames(provider, account); err != nil {
		return nil, false, err
	}
	data, err := readLimited(s.root, []string{"auth", provider}, account+".json", maxCredentialBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, credentialError("read", account)
	}
	owned := append([]byte(nil), data...)
	zero(data)
	return owned, true, nil
}

// SaveBytes replaces the saved login in one step, in a file only you can
// read. It writes a copy, so data stays as the caller passed it.
func (s Store) SaveBytes(provider, account string, data []byte) error {
	if err := validatePathNames(provider, account); err != nil {
		return err
	}
	if len(data) > maxCredentialBytes {
		return credentialError("save", account)
	}
	owned := append([]byte(nil), data...)
	defer zero(owned)
	if err := fsutil.WriteFileAtomic(s.cachePath(provider, account), owned, 0o600); err != nil {
		return credentialError("save", account)
	}
	return nil
}

// Delete removes a saved login. A login that is not there is not an error.
func (s Store) Delete(provider, account string) (bool, error) {
	if err := validatePathNames(provider, account); err != nil {
		return false, err
	}
	path := s.cachePath(provider, account)
	dir := filepath.Dir(path)
	removed := filepath.Join(dir, "."+filepath.Base(path)+".removed")
	if _, err := os.Lstat(removed); err == nil {
		return false, credentialError("remove", account)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, credentialError("remove", account)
	}
	err := os.Rename(path, removed)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, credentialError("remove", account)
	}
	if err := fsutil.SyncDir(dir); err != nil {
		return false, credentialError("remove", account)
	}
	if err := os.Remove(removed); err != nil {
		return false, credentialError("remove", account)
	}
	if err := fsutil.SyncDir(dir); err != nil {
		return false, credentialError("remove", account)
	}
	return true, nil
}

// CleanupStale removes leftover temporary files maildo wrote in the config
// folder.
func (s Store) CleanupStale(configPath string) error {
	if err := fsutil.RemoveStale(s.root, configPath); err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not remove leftover files from an earlier login change")
	}
	return nil
}

// Cleanup is CleanupStale for callers that have no Store.
func Cleanup(root, configPath string) error {
	return NewStore(root).CleanupStale(configPath)
}

func (s Store) cachePath(provider, account string) string {
	return filepath.Join(s.root, "auth", provider, account+".json")
}

func validatePathNames(provider, account string) error {
	if !maildoconfig.IsValidProviderName(provider) || !maildoconfig.IsValidAccountName(account) {
		return errs.New(errs.CodeInvalidConfig, "the account or provider name is not valid for a saved login")
	}
	return nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// credentialError says what failed without file paths, which stay out of
// all output.
func credentialError(operation, account string) error {
	action := map[string]string{
		"read":   "read",
		"remove": "removed",
		"save":   "saved",
		"stat":   "checked",
	}[operation]
	if action == "" {
		action = operation
	}
	if account == "" {
		return errs.New(errs.CodeInvalidConfig, fmt.Sprintf("the saved login could not be %s", action))
	}
	return errs.New(errs.CodeInvalidConfig, fmt.Sprintf("the saved login for account %q could not be %s; run maildo auth repair %s", account, action, account))
}

func readLimited(root string, dirs []string, name string, limit int64) ([]byte, error) {
	file, err := openManagedRead(root, dirs, name)
	if err != nil {
		return nil, err
	}
	defer file.close()
	return file.read(limit)
}

// readConfigLimited reads the config file. It holds no secrets, so the user
// may choose its place and permissions; saved logins go through the stricter
// private reader instead.
func readConfigLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("config file exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		zero(data)
		return nil, err
	}
	if int64(len(data)) > limit {
		zero(data)
		return nil, fmt.Errorf("config file exceeds limit")
	}
	return data, nil
}
