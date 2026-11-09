//go:build conformance

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	runconfig "github.com/vincentsch/rungrad/config"
	"github.com/vincentsch/rungrad/update"
)

const (
	conformanceProviderName = "conformance"
	conformanceStateDir     = ".maildo-conformance"
	conformanceStateFile    = "mailbox-state.json"
	conformanceAuditFile    = "write-audit.ndjson"
	conformanceSecretEnv    = "MAILDO_CONFORMANCE_TOKEN"
)

var conformanceProviderIdentities = []string{"outlook", "gmail", "imap", "fastmail"}

func main() {
	if err := seedConformanceIfNeeded(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "maildo: conformance fixture state unavailable:", err)
		os.Exit(rungrad.ExitAPI)
	}
	deps, err := conformanceDepsE()
	if err != nil {
		fmt.Fprintln(os.Stderr, "maildo: invalid application wiring")
		os.Exit(rungrad.ExitAPI)
	}
	app, err := newAppE(deps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "maildo: invalid application wiring")
		os.Exit(rungrad.ExitAPI)
	}
	os.Exit(app.RunIO(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func conformanceDeps() Deps {
	deps, err := conformanceDepsE()
	if err != nil {
		deps.wiringErr = err
	}
	return deps
}

func conformanceDepsE() (Deps, error) {
	// This test build must never use a real provider, even when a test
	// passes an account or provider it does not expect.
	deps := Deps{
		Providers:           provider.NewRegistry(),
		SetupParsers:        defaultSetupParsers(append(append([]string(nil), conformanceProviderIdentities...), conformanceProviderName)),
		DirectSendPreparers: provider.NewDirectSendPreparerRegistry(),
	}
	home, _ := conformanceHome()
	for _, name := range conformanceProviderIdentities {
		if err := deps.Providers.Register(provider.ProviderRegistration{
			Name:      name,
			Lifecycle: conformanceProviderAlias{conformanceProvider: conformanceProvider{home: home}, name: name},
			Codec:     conformanceCodecAlias{name: name},
		}); err != nil {
			return Deps{}, err
		}
	}
	if err := deps.Providers.Register(provider.ProviderRegistration{
		Name:      conformanceProviderName,
		Lifecycle: conformanceProvider{home: home},
		Codec:     conformanceCredentialCodec{},
	}); err != nil {
		return Deps{}, err
	}
	deps.UpdateFetcher = conformanceUpdateFetcher{}
	return deps, nil
}

// The aliases keep the real providers' names and login flags, but run on
// test code only. None of them can open a real connection.
type conformanceProviderAlias struct {
	conformanceProvider
	name string
}

func (p conformanceProviderAlias) Name() string { return p.name }

type conformanceCodecAlias struct {
	conformanceCredentialCodec
	name string
}

func (c conformanceCodecAlias) ProviderName() string { return c.name }

type conformanceUpdateFetcher struct{}

func (conformanceUpdateFetcher) Latest() (update.Release, error) {
	return update.Release{Version: version}, nil
}

func seedConformanceIfNeeded(args []string) error {
	if !conformanceNeedsSeed(args) {
		return nil
	}
	home, err := conformanceHome()
	if err != nil {
		return err
	}
	if _, err := ensureConformanceState(home); err != nil {
		return err
	}
	configPath, err := conformanceConfigPath(args, home)
	if err != nil {
		return err
	}
	if err := ensureConformanceRegistry(configPath); err != nil {
		return err
	}
	return ensureConformanceCredentials(home, filepath.Dir(configPath))
}

func conformanceNeedsSeed(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "__rungrad_manifest", "completion", "help", "update", "version", "--help", "-h", "--version", "this-subcommand-does-not-exist":
			return false
		}
		if strings.HasPrefix(arg, "--help=") || strings.HasPrefix(arg, "--version=") {
			return false
		}
	}
	tokens := conformanceCommandTokens(args)
	if len(tokens) == 0 {
		return false
	}
	switch tokens[0] {
	case "move", "read":
		return true
	case "folders":
		return len(tokens) >= 2 && (tokens[1] == "list" || tokens[1] == "view" || tokens[1] == "delete")
	default:
		return false
	}
}

func conformanceCommandTokens(args []string) []string {
	valueFlags := map[string]bool{
		"--account":  true,
		"--config":   true,
		"--confirm":  true,
		"--id":       true,
		"--jq":       true,
		"--template": true,
		"--to":       true,
	}
	var tokens []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--") {
			if strings.Contains(arg, "=") {
				continue
			}
			if valueFlags[arg] && i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		tokens = append(tokens, arg)
	}
	return tokens
}

func conformanceHome() (string, error) {
	home := os.Getenv("HOME")
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if home == "" || xdg == "" || home != xdg {
		return "", errors.New("invalid conformance home")
	}
	info, err := os.Lstat(home)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("invalid conformance home")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("invalid conformance home")
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(filepath.Base(realHome), "rungrad-conformance-") {
		return "", errors.New("conformance home has unexpected name")
	}
	return realHome, nil
}

func conformanceConfigPath(args []string, home string) (string, error) {
	override := ""
	for i, arg := range args {
		switch {
		case arg == "--config" && i+1 < len(args):
			override = args[i+1]
		case strings.HasPrefix(arg, "--config="):
			override = strings.TrimPrefix(arg, "--config=")
		}
	}
	store := runconfig.Store{Tool: "maildo", Override: override}
	path, err := store.ConfigPath()
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, rel, err := conformanceContainedPath(home, path)
	if err != nil || rel == "." {
		return "", errors.New("config path outside conformance home")
	}
	if err := rejectConformanceSymlinkComponents(home, rel); err != nil {
		return "", err
	}
	return path, nil
}

func conformanceContainedPath(home, path string) (string, string, error) {
	home = filepath.Clean(home)
	path = filepath.Clean(path)
	// home is resolved, but paths built from $HOME may not be: on macOS the
	// temp folder /var/folders is a link to /private/var/folders. Move such a
	// path onto the resolved home before checking that it stays inside.
	if raw := filepath.Clean(os.Getenv("HOME")); raw != home {
		if rel, err := filepath.Rel(raw, path); err == nil && !relEscapes(rel) {
			path = filepath.Join(home, rel)
		}
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || relEscapes(rel) {
		return "", "", errors.New("path outside conformance home")
	}
	return path, rel, nil
}

func relEscapes(rel string) bool {
	slash := filepath.ToSlash(rel)
	return rel == "" || slash == ".." || strings.HasPrefix(slash, "../") || filepath.IsAbs(rel)
}

func rejectConformanceSymlinkComponents(home, rel string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	current := home
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("conformance path traverses symlink")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("conformance path parent is not a directory")
		}
	}
	return nil
}

func ensureConformanceRegistry(configPath string) error {
	if reg, err := maildoconfig.Load(configPath, []string{conformanceProviderName}); err == nil && conformanceRegistryReady(reg) {
		return nil
	}
	reg := &maildoconfig.Registry{
		Version:       1,
		ActiveAccount: "outlook",
	}
	for _, name := range conformanceAccountNames() {
		reg.Accounts = append(reg.Accounts, conformanceAccount(name))
	}
	return maildoconfig.Save(configPath, reg, []string{conformanceProviderName})
}

func conformanceRegistryReady(reg *maildoconfig.Registry) bool {
	if reg == nil || reg.ActiveAccount == "" {
		return false
	}
	want := map[string]bool{}
	for _, name := range conformanceAccountNames() {
		want[name] = false
	}
	for _, acct := range reg.Accounts {
		if _, ok := want[acct.Name]; ok && acct.Provider == conformanceProviderName {
			want[acct.Name] = true
		}
	}
	for _, ok := range want {
		if !ok {
			return false
		}
	}
	return true
}

func conformanceAccountNames() []string {
	names := []string{"fixture", "logged-out", "ambiguous", "api-error", "forbidden", "rate-limited"}
	for _, providerName := range conformanceProviderIdentities {
		names = append(names,
			providerName,
			providerName+"-logged-out",
			providerName+"-ambiguous",
			providerName+"-api-error",
			providerName+"-forbidden",
			providerName+"-rate-limited",
		)
	}
	return names
}

func conformanceAccount(name string) maildoconfig.Account {
	return maildoconfig.Account{
		Name:              name,
		Provider:          conformanceProviderName,
		Email:             name + "@example.invalid",
		DisplayName:       "Conformance " + name,
		ProviderAccountID: "provider-" + name,
		AuthRef:           maildoconfig.DeriveAuthRef(conformanceProviderName, name),
	}
}

func ensureConformanceCredentials(home, root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	_, rel, err := conformanceContainedPath(home, root)
	if err != nil {
		return err
	}
	if err := rejectConformanceSymlinkComponents(home, rel); err != nil {
		return err
	}
	store := auth.NewStore(root)
	for _, name := range conformanceAccountNames() {
		if strings.HasSuffix(name, "-logged-out") || name == "logged-out" {
			continue
		}
		account := conformanceAccount(name)
		present, err := store.Exists(conformanceProviderName, name)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		cache := outlook.NewTokenCache(outlook.TokenCacheData{
			Version:           1,
			Provider:          conformanceProviderName,
			AccountName:       name,
			TokenType:         "Bearer",
			AccessToken:       "fixture-access-token-" + name,
			RefreshToken:      "fixture-refresh-token-" + name,
			ExpiresAt:         time.Unix(1893456000, 0).UTC(),
			VerifiedEmail:     account.Email,
			DisplayName:       account.DisplayName,
			ProviderAccountID: account.ProviderAccountID,
		})
		data, err := conformanceCredentialCodec{}.Encode(auth.CredentialBinding{Provider: conformanceProviderName, Account: name}, cache)
		cache.Destroy()
		if err != nil {
			return err
		}
		if err := store.SaveBytes(conformanceProviderName, name, data); err != nil {
			return err
		}
	}
	for _, name := range conformanceAccountNames() {
		if strings.HasSuffix(name, "-logged-out") || name == "logged-out" {
			if _, err := store.Delete(conformanceProviderName, name); err != nil {
				return err
			}
		}
	}
	return nil
}

type conformanceMailboxState struct {
	Version  int                       `json:"version"`
	Folders  []conformanceFolderState  `json:"folders"`
	Messages []conformanceMessageState `json:"messages"`
}

type conformanceFolderState struct {
	Account          string `json:"account"`
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	Path             string `json:"path"`
	WellKnownName    string `json:"well_known_name,omitempty"`
	TotalItemCount   int    `json:"total_item_count"`
	UnreadItemCount  int    `json:"unread_item_count"`
	ChildFolderCount int    `json:"child_folder_count"`
}

type conformanceMessageState struct {
	Account  string `json:"account"`
	ID       string `json:"id"`
	FolderID string `json:"folder_id"`
	Subject  string `json:"subject"`
	IsRead   bool   `json:"is_read"`
}

type conformanceWriteAuditRecord struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Account   string `json:"account"`
	Target    string `json:"target"`
}

func ensureConformanceState(home string) (conformanceMailboxState, error) {
	dir := filepath.Join(home, conformanceStateDir)
	if err := ensurePrivateDir(dir); err != nil {
		return conformanceMailboxState{}, err
	}
	statePath := filepath.Join(dir, conformanceStateFile)
	info, err := os.Lstat(statePath)
	if errors.Is(err, os.ErrNotExist) {
		state := seedConformanceState()
		data, err := encodeConformanceState(state)
		if err != nil {
			return conformanceMailboxState{}, err
		}
		f, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return conformanceMailboxState{}, err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return conformanceMailboxState{}, err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return conformanceMailboxState{}, err
		}
		if err := f.Close(); err != nil {
			return conformanceMailboxState{}, err
		}
		return state, nil
	}
	if err != nil {
		return conformanceMailboxState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return conformanceMailboxState{}, errors.New("invalid state file")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return conformanceMailboxState{}, err
	}
	state, err := decodeConformanceState(data)
	if err != nil {
		return conformanceMailboxState{}, err
	}
	if err := validateConformanceAudit(filepath.Join(dir, conformanceAuditFile)); err != nil {
		return conformanceMailboxState{}, err
	}
	return state, nil
}

func ensurePrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("invalid conformance state directory")
	}
	return nil
}

func seedConformanceState() conformanceMailboxState {
	state := conformanceMailboxState{
		Version: 1,
		Folders: []conformanceFolderState{
			{Account: "fixture", ID: "zeta-id", DisplayName: "Zeta", Path: "Zeta"},
			{Account: "fixture", ID: "inbox-id", DisplayName: "Inbox", Path: "Inbox", WellKnownName: mail.WellKnownInbox, TotalItemCount: 1, UnreadItemCount: 1},
			{Account: "fixture", ID: "archive-id", DisplayName: "Archive", Path: "Archive", WellKnownName: mail.WellKnownArchive},
			{Account: "fixture", ID: "disposable-id", DisplayName: "Disposable", Path: "Disposable"},
			{Account: "ambiguous", ID: "dup-b", DisplayName: "Dup", Path: "B/Dup"},
			{Account: "ambiguous", ID: "dup-a", DisplayName: "Dup", Path: "A/Dup"},
		},
		Messages: []conformanceMessageState{
			{Account: "fixture", ID: "fixture-message", FolderID: "inbox-id", Subject: "Fixture message", IsRead: false},
		},
	}
	for _, providerName := range conformanceProviderIdentities {
		state.addProviderFixtures(providerName)
	}
	return state
}

func (s *conformanceMailboxState) addProviderFixtures(account string) {
	archiveName := "Archive"
	archiveWellKnown := mail.WellKnownArchive
	presentID := account + "-message"
	switch account {
	case "gmail":
		archiveName = "ConformanceArchive"
		archiveWellKnown = ""
		presentID = "18f0000000000001"
	case "outlook":
		presentID = "AAMkAGZpeHR1cmUtMQ=="
	case "imap":
		presentID = "imap-message/v1/SU5CT1g/77/1"
	case "fastmail":
		presentID = "imap-message/v1/SU5CT1g/77/2"
	}
	s.Folders = append(s.Folders,
		conformanceFolderState{Account: account, ID: account + "-zeta-id", DisplayName: "Zeta", Path: "Zeta"},
		conformanceFolderState{Account: account, ID: account + "-inbox-id", DisplayName: "Inbox", Path: "Inbox", WellKnownName: mail.WellKnownInbox, TotalItemCount: 1, UnreadItemCount: 1},
		conformanceFolderState{Account: account, ID: account + "-archive-id", DisplayName: archiveName, Path: archiveName, WellKnownName: archiveWellKnown},
		conformanceFolderState{Account: account, ID: account + "-disposable-id", DisplayName: "Disposable", Path: "Disposable"},
		conformanceFolderState{Account: account + "-ambiguous", ID: account + "-dup-b", DisplayName: "Dup", Path: "B/Dup"},
		conformanceFolderState{Account: account + "-ambiguous", ID: account + "-dup-a", DisplayName: "Dup", Path: "A/Dup"},
	)
	s.Messages = append(s.Messages, conformanceMessageState{
		Account: account, ID: presentID, FolderID: account + "-inbox-id", Subject: "Fixture message", IsRead: false,
	})
}

func encodeConformanceState(state conformanceMailboxState) ([]byte, error) {
	if state.Folders == nil {
		state.Folders = []conformanceFolderState{}
	}
	if state.Messages == nil {
		state.Messages = []conformanceMessageState{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func decodeConformanceState(data []byte) (conformanceMailboxState, error) {
	if len(data) < 2 || data[len(data)-1] != '\n' || data[len(data)-2] == '\n' {
		return conformanceMailboxState{}, errors.New("state must end with one newline")
	}
	var state conformanceMailboxState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return conformanceMailboxState{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return conformanceMailboxState{}, errors.New("state has trailing data")
	}
	if err := validateConformanceState(state); err != nil {
		return conformanceMailboxState{}, err
	}
	return state, nil
}

func validateConformanceState(state conformanceMailboxState) error {
	if state.Version != 1 || state.Folders == nil || state.Messages == nil {
		return errors.New("invalid state header")
	}
	folders := map[string]bool{}
	for _, f := range state.Folders {
		if f.Account == "" || f.ID == "" {
			return errors.New("folder identity is required")
		}
		key := f.Account + "\x00" + f.ID
		if folders[key] {
			return errors.New("duplicate folder identity")
		}
		folders[key] = true
	}
	messages := map[string]bool{}
	for _, m := range state.Messages {
		if m.Account == "" || m.ID == "" || m.FolderID == "" {
			return errors.New("message identity is required")
		}
		key := m.Account + "\x00" + m.ID
		if messages[key] {
			return errors.New("duplicate message identity")
		}
		if !folders[m.Account+"\x00"+m.FolderID] {
			return errors.New("message references missing folder")
		}
		messages[key] = true
	}
	return nil
}

func validateConformanceAudit(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("invalid audit file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec conformanceWriteAuditRecord
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return err
		}
		if rec.Version != 1 || rec.Operation == "" || rec.Account == "" || rec.Target == "" {
			return errors.New("invalid audit record")
		}
	}
	return nil
}

type conformanceProvider struct {
	home string
}

func (p conformanceProvider) Name() string { return conformanceProviderName }

func (p conformanceProvider) Capabilities(context.Context) (provider.Capabilities, error) {
	caps := make(provider.Capabilities, len(provider.AllCapabilities()))
	for _, cap := range provider.AllCapabilities() {
		caps[cap] = provider.CapUnsupported
	}
	caps[provider.CapFoldersRead] = provider.CapImplemented
	caps[provider.CapFoldersMutate] = provider.CapImplemented
	caps[provider.CapMessagesRead] = provider.CapImplemented
	caps[provider.CapMessagesMove] = provider.CapImplemented
	return caps, nil
}

func (p conformanceProvider) InteractiveAuth(context.Context, provider.InteractiveAuthContext) (provider.InteractiveAuthResult, error) {
	return provider.InteractiveAuthResult{}, rungrad.NewError(rungrad.ExitAPI, "conformance provider does not support login")
}

func (p conformanceProvider) LocalHealth(context.Context, provider.StoredAuthContext) (provider.LocalStatus, error) {
	return provider.LocalUsable, nil
}

func (p conformanceProvider) ForcedRefresh(context.Context, provider.StoredAuthContext) (provider.RefreshResult, error) {
	return provider.RefreshResult{}, nil
}

func (p conformanceProvider) Logout(context.Context, provider.StoredAuthContext) error {
	return nil
}

func (p conformanceProvider) NewAccountClient(ctx context.Context, ac provider.StoredAuthContext) (provider.AccountClient, error) {
	cache, err := conformanceCredential(ctx, ac)
	if err != nil {
		return nil, errs.New(errs.CodeProviderIdentityUnavailable, fmt.Sprintf("credential cache for account %q is missing", ac.Account))
	}
	defer cache.Destroy()
	home := p.home
	if home == "" {
		var err error
		home, err = conformanceHome()
		if err != nil {
			return nil, rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
		}
	}
	return &conformanceClient{home: home, account: ac.Account, accessToken: cache.AccessTokenValue()}, nil
}

func conformanceCredential(ctx context.Context, ac provider.StoredAuthContext) (*outlook.TokenCache, error) {
	if secret := os.Getenv(conformanceSecretEnv); secret != "" {
		cache := conformanceEnvCache(auth.CredentialBinding{Provider: ac.Provider, Account: ac.Account}, secret)
		if err := conformanceRegisterSecrets(ac.Runtime, cache); err != nil {
			cache.Destroy()
			return nil, err
		}
		return cache, nil
	}
	var cache *outlook.TokenCache
	err := ac.Credential.WithCurrent(ctx, func(lease provider.CredentialLease) error {
		current, ok := lease.Current().(*outlook.TokenCache)
		if !ok || current == nil || current.Destroyed() {
			return provider.ErrCredentialCapabilityClosed
		}
		cache = conformanceCloneCache(current)
		return nil
	})
	return cache, err
}

func conformanceEnvCache(binding auth.CredentialBinding, secret string) *outlook.TokenCache {
	account := conformanceAccount(binding.Account)
	return outlook.NewTokenCache(outlook.TokenCacheData{
		Version:           1,
		Provider:          binding.Provider,
		AccountName:       binding.Account,
		TokenType:         "Bearer",
		AccessToken:       secret,
		RefreshToken:      "fixture-refresh-token-" + binding.Account,
		ExpiresAt:         time.Unix(1893456000, 0).UTC(),
		VerifiedEmail:     account.Email,
		DisplayName:       account.DisplayName,
		ProviderAccountID: account.ProviderAccountID,
	})
}

type conformanceCredentialCodec struct{}

func (conformanceCredentialCodec) ProviderName() string { return conformanceProviderName }

func (conformanceCredentialCodec) Decode(registrar provider.SecretRegistrar, binding auth.CredentialBinding, data []byte) (provider.Credential, error) {
	var raw outlook.TokenCacheData
	if err := json.Unmarshal(data, &raw); err != nil {
		if secret := os.Getenv(conformanceSecretEnv); secret != "" {
			cache := conformanceEnvCache(binding, secret)
			if err := conformanceRegisterSecrets(registrar, cache); err != nil {
				cache.Destroy()
				return nil, err
			}
			return cache, nil
		}
		return nil, provider.ErrCredentialCapabilityClosed
	}
	cache := outlook.NewTokenCache(raw)
	if err := conformanceRegisterSecrets(registrar, cache); err != nil {
		cache.Destroy()
		return nil, err
	}
	if cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		cache.Destroy()
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return cache, nil
}

func (conformanceCredentialCodec) Encode(binding auth.CredentialBinding, credential provider.Credential) ([]byte, error) {
	cache, ok := credential.(*outlook.TokenCache)
	if !ok || cache == nil || cache.Destroyed() || cache.Provider != binding.Provider || cache.AccountName != binding.Account {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	data, err := json.MarshalIndent(cache.Snapshot(), "", "  ")
	if err != nil {
		return nil, provider.ErrCredentialCapabilityClosed
	}
	return append(data, '\n'), nil
}

func (conformanceCredentialCodec) RegisterSecrets(registrar provider.SecretRegistrar, credential provider.Credential) error {
	cache, ok := credential.(*outlook.TokenCache)
	if !ok || cache == nil || cache.Destroyed() {
		return provider.ErrCredentialCapabilityClosed
	}
	return conformanceRegisterSecrets(registrar, cache)
}

func (conformanceCredentialCodec) LocalHealth(maildoconfig.ProviderSetup, auth.CredentialBinding, provider.Credential) (provider.LocalStatus, error) {
	return provider.LocalUsable, nil
}

func (conformanceCredentialCodec) RefreshTokenRotated(binding auth.CredentialBinding, before, after provider.Credential) (bool, error) {
	oldCache, ok := before.(*outlook.TokenCache)
	if !ok || oldCache == nil || oldCache.Destroyed() || oldCache.Provider != binding.Provider || oldCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	newCache, ok := after.(*outlook.TokenCache)
	if !ok || newCache == nil || newCache.Destroyed() || newCache.Provider != binding.Provider || newCache.AccountName != binding.Account {
		return false, provider.ErrCredentialCapabilityClosed
	}
	return newCache.RefreshTokenValue() != "" && newCache.RefreshTokenValue() != oldCache.RefreshTokenValue(), nil
}

func (conformanceCredentialCodec) RebindCredential(registrar provider.SecretRegistrar, raw []byte, oldBinding, newBinding auth.CredentialBinding) (auth.CredentialRebindResult, error) {
	var decoded outlook.TokenCacheData
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Provider != oldBinding.Provider || decoded.AccountName != oldBinding.Account {
		return auth.CredentialRebindResult{Bytes: append([]byte(nil), raw...), Disposition: auth.CredentialPreservedNeedsRepair}, nil
	}
	cache := outlook.NewTokenCache(decoded)
	if err := conformanceRegisterSecrets(registrar, cache); err != nil {
		cache.Destroy()
		return auth.CredentialRebindResult{}, err
	}
	cache.Provider = newBinding.Provider
	cache.AccountName = newBinding.Account
	data, err := conformanceCredentialCodec{}.Encode(newBinding, cache)
	cache.Destroy()
	if err != nil {
		return auth.CredentialRebindResult{}, err
	}
	return auth.CredentialRebindResult{Bytes: data, Disposition: auth.CredentialReboundUsable}, nil
}

func conformanceRegisterSecrets(registrar provider.SecretRegistrar, cache *outlook.TokenCache) error {
	if registrar == nil || cache == nil {
		return nil
	}
	accessToken := cache.AccessTokenValue()
	refreshToken := cache.RefreshTokenValue()
	if err := registrar.RegisterSecret(accessToken); err != nil {
		return err
	}
	if err := registrar.RegisterSecret(refreshToken); err != nil {
		return err
	}
	if accessToken != "" {
		if err := registrar.RegisterSecret(cache.TokenType + " " + accessToken); err != nil {
			return err
		}
		if err := registrar.RegisterSecret("Bearer " + accessToken); err != nil {
			return err
		}
	}
	return nil
}

func conformanceCloneCache(cache *outlook.TokenCache) *outlook.TokenCache {
	return outlook.NewTokenCache(cache.Snapshot())
}

type conformanceClient struct {
	home        string
	account     string
	accessToken string
}

func (c conformanceClient) ProviderName() string { return conformanceProviderName }
func (c conformanceClient) AccountName() string  { return c.account }
func (c *conformanceClient) Destroy()            { c.accessToken = "" }

func (c conformanceClient) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	if c.accessToken == "" {
		return mail.FolderList{}, rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	switch {
	case c.account == "api-error" || strings.HasSuffix(c.account, "-api-error"):
		return mail.FolderList{}, rungrad.NewError(rungrad.ExitAPI, "provider fixture error")
	case c.account == "forbidden" || strings.HasSuffix(c.account, "-forbidden"):
		return mail.FolderList{}, rungrad.NewError(rungrad.ExitForbidden, "provider fixture forbidden")
	case c.account == "rate-limited" || strings.HasSuffix(c.account, "-rate-limited"):
		return mail.FolderList{}, rungrad.NewError(rungrad.ExitRateLimited, "provider fixture rate limited")
	}
	state, err := c.state()
	if err != nil {
		return mail.FolderList{}, err
	}
	out := mail.FolderList{Folders: []mail.Folder{}}
	for _, f := range state.Folders {
		if f.Account != c.account {
			continue
		}
		out.Folders = append(out.Folders, mail.Folder{
			Account:       f.Account,
			Provider:      conformanceProviderName,
			ID:            f.ID,
			DisplayName:   f.DisplayName,
			WellKnownName: f.WellKnownName,
			Path:          f.Path,
			TotalCount:    f.TotalItemCount,
			UnreadCount:   f.UnreadItemCount,
			ChildCount:    f.ChildFolderCount,
		})
	}
	return out, nil
}

func (c conformanceClient) ReadMessage(ctx context.Context, req mail.ReadMessageRequest) (mail.Message, error) {
	switch req.Selector.Kind {
	case mail.ResolvedSelectorInternetID:
		if _, err := mail.NormalizeMessageIDScalar(req.Selector.InternetID); err != nil {
			return mail.Message{}, errs.New(errs.CodeInvalidSelector, "invalid message selector")
		}
	case mail.ResolvedSelectorID:
		if strings.Contains(req.Selector.ID, "not-base64") {
			return mail.Message{}, errs.New(errs.CodeInvalidSelector, "invalid message selector")
		}
	}
	state, err := c.state()
	if err != nil {
		return mail.Message{}, err
	}
	for _, m := range state.Messages {
		if m.Account != c.account || req.Selector.Kind != mail.ResolvedSelectorID || req.Selector.ID != m.ID {
			continue
		}
		folder, ok := state.folder(m.Account, m.FolderID)
		if !ok {
			return mail.Message{}, rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
		}
		return mail.Message{
			Account:  c.account,
			Provider: conformanceProviderName,
			ID:       m.ID,
			Folder:   mail.SummaryFrom(folder.mailFolder()),
			From:     mail.Recipient{Address: "sender@example.invalid"},
			To:       []mail.Recipient{{Address: "fixture@example.invalid"}},
			Subject:  m.Subject,
			IsRead:   m.IsRead,
			Body:     mail.MessageBody{ContentType: "text/plain", Mode: req.BodyMode, Content: "Fixture body"},
		}, nil
	}
	return mail.Message{}, errs.New(errs.CodeMessageNotFound, "message not found")
}

func (c conformanceClient) PrepareFolderRename(context.Context, mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error) {
	return mail.FolderMutationPreflight{}, nil
}

func (c conformanceClient) PrepareFolderDelete(ctx context.Context, req mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error) {
	return mail.FolderMutationPreflight{
		Operation:           mail.FolderOperationDelete,
		Target:              mail.SummaryFrom(req.Target),
		Protected:           false,
		TotalItemCount:      req.Target.TotalCount,
		TotalItemCountKnown: true,
		ChildFolderCount:    req.Target.ChildCount,
		ChildFolderKnown:    true,
		CountsSource:        mail.FolderCountsSourceRefreshed,
	}, nil
}

func (c conformanceClient) CreateFolder(ctx context.Context, req mail.FolderCreateRequest) (mail.FolderMutationResult, error) {
	return mail.FolderMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "folder create is not configured for conformance")
}

func (c conformanceClient) RenameFolder(ctx context.Context, req mail.FolderRenameRequest) (mail.FolderMutationResult, error) {
	return mail.FolderMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "folder rename is not configured for conformance")
}

func (c conformanceClient) DeleteFolder(ctx context.Context, req mail.FolderDeleteRequest) (mail.FolderMutationResult, error) {
	if err := c.audit("folder_delete", req.Target.ID); err != nil {
		return mail.FolderMutationResult{}, err
	}
	folder := mail.SummaryFrom(req.Target)
	return mail.FolderMutationResult{Operation: mail.FolderOperationDelete, DeletedFolder: &folder, VerificationStatus: mail.FolderVerificationProviderResponse}, nil
}

func (c conformanceClient) MoveMessage(ctx context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error) {
	if err := c.audit("move", req.MessageID); err != nil {
		return mail.MessageMutationResult{}, err
	}
	target := mail.SummaryFrom(req.Folder)
	return mail.MessageMutationResult{MessageID: req.MessageID, ResultMessageID: req.MessageID, TargetFolder: &target}, nil
}

func (c conformanceClient) CopyMessage(ctx context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "copy is not configured for conformance")
}

func (c conformanceClient) ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "archive is not configured for conformance")
}

func (c conformanceClient) DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "delete is not configured for conformance")
}

func (c conformanceClient) RestoreMessage(ctx context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "restore is not configured for conformance")
}

func (c conformanceClient) JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "junk is not configured for conformance")
}

func (c conformanceClient) SetMessageReadState(ctx context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "read-state mutation is not configured for conformance")
}

func (c conformanceClient) SetMessageFlag(ctx context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error) {
	return mail.MessageMutationResult{}, rungrad.NewError(rungrad.ExitAPI, "flag mutation is not configured for conformance")
}

func (c conformanceClient) state() (conformanceMailboxState, error) {
	state, err := ensureConformanceState(c.home)
	if err != nil {
		return conformanceMailboxState{}, rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	return state, nil
}

func (c conformanceClient) audit(operation, target string) error {
	home, err := conformanceHome()
	if err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	if home != c.home {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	path := filepath.Join(c.home, conformanceStateDir, conformanceAuditFile)
	if err := validateConformanceAuditTarget(c.home, path); err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	rec := conformanceWriteAuditRecord{Version: 1, Operation: operation, Account: c.account, Target: target}
	data, err := json.Marshal(rec)
	if err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	if err := f.Close(); err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "conformance fixture state unavailable")
	}
	return nil
}

func validateConformanceAuditTarget(home, path string) error {
	clean, rel, err := conformanceContainedPath(home, path)
	if err != nil {
		return err
	}
	if err := rejectConformanceSymlinkComponents(home, rel); err != nil {
		return err
	}
	dir := filepath.Join(home, conformanceStateDir)
	if filepath.Clean(filepath.Dir(clean)) != filepath.Clean(dir) {
		return errors.New("invalid audit path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("invalid conformance state directory")
	}
	info, err = os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("invalid audit file")
	}
	return nil
}

func (s conformanceMailboxState) folder(account, id string) (conformanceFolderState, bool) {
	for _, f := range s.Folders {
		if f.Account == account && f.ID == id {
			return f, true
		}
	}
	return conformanceFolderState{}, false
}

func (f conformanceFolderState) mailFolder() mail.Folder {
	return mail.Folder{
		Account:       f.Account,
		Provider:      conformanceProviderName,
		ID:            f.ID,
		DisplayName:   f.DisplayName,
		WellKnownName: f.WellKnownName,
		Path:          f.Path,
		TotalCount:    f.TotalItemCount,
		UnreadCount:   f.UnreadItemCount,
		ChildCount:    f.ChildFolderCount,
	}
}

func conformanceSortedFixtureFolders() []string {
	state := seedConformanceState()
	var folders []mail.Folder
	for _, f := range state.Folders {
		if f.Account == "fixture" {
			folders = append(folders, f.mailFolder())
		}
	}
	sort.SliceStable(folders, func(i, j int) bool {
		if folders[i].Path != folders[j].Path {
			return folders[i].Path < folders[j].Path
		}
		if folders[i].DisplayName != folders[j].DisplayName {
			return folders[i].DisplayName < folders[j].DisplayName
		}
		return folders[i].ID < folders[j].ID
	})
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.Path+"/"+f.ID)
	}
	return out
}
