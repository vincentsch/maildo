// Package config reads and writes maildo's config file: the accounts, their
// settings and policies. It holds no secrets.
//
// rungrad finds the file, but this package reads and checks the YAML itself,
// so maildo decides what the errors say and keeps file paths out of them.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/fsutil"
	"golang.org/x/net/idna"
	"gopkg.in/yaml.v3"
)

const (
	// MaxRegistryBytes is the largest config file maildo reads or writes.
	MaxRegistryBytes = 4 * 1024 * 1024
	// MaxRegistryAccounts is the most accounts one config file may hold.
	MaxRegistryAccounts = 2048
	// MaxRecognizedProviders is the most provider names a caller may pass in
	// as the known providers.
	MaxRecognizedProviders = 128
)

// Registry is the config file, version 1.
//
// It holds the accounts and provider settings only. Passwords, tokens and
// anything a provider sent back never go into this file; saved logins live
// in their own private files.
type Registry struct {
	Version       int            `yaml:"version"`
	ActiveAccount string         `yaml:"active_account,omitempty"`
	Outlook       *OutlookConfig `yaml:"outlook,omitempty"`
	Gmail         *GmailConfig   `yaml:"gmail,omitempty"`
	Accounts      []Account      `yaml:"accounts,omitempty"`
}

// OutlookConfig holds the Outlook app registration's client id, which is not
// a secret.
type OutlookConfig struct {
	ClientID string `yaml:"client_id,omitempty"`
}

// GmailConfig holds the Gmail OAuth client id. The client secret is saved
// with the login, not here.
type GmailConfig struct {
	ClientID string `yaml:"client_id,omitempty"`
}

// Account is one mail account under the name the user gave it.
//
// AuthRef is built from Provider and Name and checked on read. maildo never
// uses it as a file path.
type Account struct {
	Name              string            `yaml:"name"`
	Provider          string            `yaml:"provider"`
	Email             string            `yaml:"email,omitempty"`
	DisplayName       string            `yaml:"display_name,omitempty"`
	ProviderAccountID string            `yaml:"provider_account_id,omitempty"`
	AuthRef           string            `yaml:"auth_ref"`
	DefaultFrom       string            `yaml:"default_from,omitempty"`
	Username          string            `yaml:"username,omitempty"`
	MailServer        *MailServerConfig `yaml:"mail_server,omitempty"`
	// SenderName is the display name used in From when sending.
	SenderName string `yaml:"sender_name,omitempty"`
	Policy     Policy `yaml:"policy,omitempty"`
}

// Policy limits what maildo may do with one account. It is meant for
// handing an account to a script or a coding agent: the limits hold no matter
// which flags the caller passes.
type Policy struct {
	// ReadOnly allows reading only: nothing is sent, drafted, moved,
	// flagged or deleted.
	ReadOnly bool `yaml:"read_only,omitempty" json:"read_only,omitempty"`
	// DraftsOnly allows reading and writing drafts. Nothing is sent, and
	// mail is not moved, flagged or deleted.
	DraftsOnly bool `yaml:"drafts_only,omitempty" json:"drafts_only,omitempty"`
	// AllowRecipients limits sending to these addresses and domains. An entry
	// is an address (ann@example.com) or a domain (@example.com).
	AllowRecipients []string `yaml:"allow_recipients,omitempty" json:"allow_recipients,omitempty"`
	// MaxBulk caps how many messages one command may change.
	MaxBulk int `yaml:"max_bulk,omitempty" json:"max_bulk,omitempty"`
	// UnattendedSend lets maildo send from the account without an approval
	// code and without asking, for scripts that send on purpose.
	UnattendedSend bool `yaml:"unattended_send,omitempty" json:"unattended_send,omitempty"`
}

// IsZero reports whether the policy sets no limit.
func (p Policy) IsZero() bool {
	return !p.ReadOnly && !p.DraftsOnly && len(p.AllowRecipients) == 0 && p.MaxBulk == 0 && !p.UnattendedSend
}

// MailServerConfig holds an account's IMAP and SMTP servers.
type MailServerConfig struct {
	IMAP ServerEndpoint `yaml:"imap"`
	SMTP ServerEndpoint `yaml:"smtp"`
	// CAFile is an extra PEM certificate bundle to trust, for self-hosted
	// servers with their own certificate authority.
	CAFile string `yaml:"ca_file,omitempty"`
}

// ServerEndpoint is a mail server's host, port and TLS mode.
type ServerEndpoint struct {
	Host string  `yaml:"host"`
	Port uint16  `yaml:"port"`
	TLS  TLSMode `yaml:"tls"`
}

// TLSMode says how maildo starts TLS: right away (implicit) or after a plain
// greeting (starttls).
type TLSMode string

const (
	TLSImplicit TLSMode = "implicit"
	TLSStartTLS TLSMode = "starttls"
)

// OutlookSetup is the setup an Outlook account needs at runtime.
type OutlookSetup struct {
	ClientID string
}

// GmailSetup is the setup a Gmail API account needs at runtime.
type GmailSetup struct {
	ClientID string
}

// MailServerSetup is the setup an IMAP or Fastmail account needs at runtime.
type MailServerSetup struct {
	Email    string
	Username string
	IMAP     ServerEndpoint
	SMTP     ServerEndpoint
	CAFile   string
}

// ProviderSetup holds the setup of one account. Exactly one field is set,
// the one that matches the account's provider.
type ProviderSetup struct {
	Outlook    *OutlookSetup
	Gmail      *GmailSetup
	MailServer *MailServerSetup
}

var accountNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
var providerNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var reservedNames = map[string]bool{
	"all":     true,
	"active":  true,
	"current": true,
	"default": true,
	"none":    true,
	"null":    true,
}

// Load reads and checks the config file.
//
// A missing file means no accounts yet. A file that cannot be read or is not
// valid gives an invalid_config error without the file path.
func Load(path string, recognizedProviders []string) (*Registry, error) {
	data, err := readLimited(path, MaxRegistryBytes)
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Version: 1}, nil
	}
	if err != nil {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not read its config file")
	}
	return Decode(data, recognizedProviders)
}

// Decode reads a config file's content. Unknown keys and more than one YAML
// document are errors.
func Decode(data []byte, recognizedProviders []string) (*Registry, error) {
	if len(data) > MaxRegistryBytes {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not read its config file")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, invalidConfig("the config file needs a version line, version: 1")
	}
	doc, err := decodeOneDocument(data)
	if err != nil {
		return nil, err
	}
	root := documentRoot(doc)
	if root == nil {
		return nil, invalidConfig("the config file needs a version line, version: 1")
	}
	version, err := readVersion(root)
	if err != nil {
		return nil, err
	}
	if version > 1 {
		return nil, errs.New(errs.CodeUnsupportedConfigVersion, fmt.Sprintf("the config file has version %d, which this maildo does not understand; update maildo", version))
	}
	if version <= 0 {
		return nil, invalidConfig("the config file needs a version line, version: 1")
	}
	// KnownFields rejects misspelled keys, and the YAML library's message
	// names the line, which helps someone who edited the file by hand.
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, invalidConfig("the config file is not valid: " + strings.TrimPrefix(err.Error(), "yaml: "))
	}
	migrated, err := Migrate(&r)
	if err != nil {
		return nil, err
	}
	for i := range migrated.Accounts {
		if migrated.Accounts[i].AuthRef == "" {
			migrated.Accounts[i].AuthRef = DeriveAuthRef(migrated.Accounts[i].Provider, migrated.Accounts[i].Name)
		}
	}
	normalizeRegistry(migrated)
	if err := migrated.validate(recognizedProviders); err != nil {
		return nil, err
	}
	return migrated, nil
}

// Encode returns the YAML for r, in the form maildo always writes. It does
// not change r.
func Encode(r *Registry, recognizedProviders []string) ([]byte, error) {
	clone := r.Clone()
	if clone == nil {
		clone = &Registry{}
	}
	clone.Version = 1
	for i := range clone.Accounts {
		clone.Accounts[i].AuthRef = DeriveAuthRef(clone.Accounts[i].Provider, clone.Accounts[i].Name)
	}
	normalizeRegistry(clone)
	if err := clone.validate(recognizedProviders); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(clone)
	if err != nil {
		return nil, invalidConfig("maildo could not write the config")
	}
	if len(data) > MaxRegistryBytes {
		return nil, errs.New(errs.CodeInvalidConfig, "maildo could not save its config file")
	}
	return data, nil
}

// Save writes the config file so only you can read it, with auth_ref values
// rebuilt from the names. Its errors leave out the file path.
func Save(path string, r *Registry, recognizedProviders []string) error {
	data, err := Encode(r, recognizedProviders)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return errs.New(errs.CodeInvalidConfig, "maildo could not save its config file")
	}
	return nil
}

// Migrate checks the config version. Only version 1 exists so far, so there
// is nothing to convert yet.
func Migrate(r *Registry) (*Registry, error) {
	if r == nil {
		return &Registry{Version: 1}, nil
	}
	if r.Version == 1 {
		return r, nil
	}
	if r.Version > 1 {
		return nil, errs.New(errs.CodeUnsupportedConfigVersion, fmt.Sprintf("the config file has version %d, which this maildo does not understand; update maildo", r.Version))
	}
	return nil, invalidConfig("the config file needs a version line, version: 1")
}

// ValidateAccountName checks an account name the user typed.
func ValidateAccountName(name string) error {
	if !IsValidAccountName(name) {
		return errs.New(errs.CodeInvalidAccountName, fmt.Sprintf("%q cannot be an account name; start with a lowercase letter and use only lowercase letters, digits, - and _ (at most 63 characters)", name))
	}
	return nil
}

// IsValidAccountName reports whether name is allowed as an account name. It
// is used for typed names and for names in the config file; the caller
// decides which error to give.
func IsValidAccountName(name string) bool {
	if !accountNameRE.MatchString(name) {
		return false
	}
	if isWin32DeviceBasename(name) {
		return false
	}
	if reservedNames[name] {
		return false
	}
	if strings.Contains(name, "://") {
		return false
	}
	return true
}

// IsValidProviderName reports whether name is a valid provider name or alias.
func IsValidProviderName(name string) bool {
	return providerNameRE.MatchString(name) && !isWin32DeviceBasename(name)
}

// IsWin32DeviceBasename reports whether name is a device name that Windows
// treats specially as a file name, such as CON or NUL. Only the part before
// the first dot counts.
func IsWin32DeviceBasename(name string) bool {
	return isWin32DeviceBasename(name)
}

// DeriveAuthRef returns the account reference shown in output and previews.
func DeriveAuthRef(provider, name string) string {
	return provider + "/" + name
}

// Find returns the account with this name, as a pointer into r, so the
// caller can change it.
func (r *Registry) Find(name string) (*Account, bool) {
	for i := range r.Accounts {
		if r.Accounts[i].Name == name {
			return &r.Accounts[i], true
		}
	}
	return nil, false
}

// Add adds an account and refuses a name that is already taken.
func (r *Registry) Add(a Account) error {
	if _, ok := r.Find(a.Name); ok {
		return errs.New(errs.CodeAccountAlreadyExists, fmt.Sprintf("account %q already exists", a.Name))
	}
	a.AuthRef = DeriveAuthRef(a.Provider, a.Name)
	r.Accounts = append(r.Accounts, cloneAccount(a))
	return nil
}

// Remove deletes an account. When it was the active account, no account is
// active afterwards.
func (r *Registry) Remove(name string) bool {
	for i := range r.Accounts {
		if r.Accounts[i].Name == name {
			r.Accounts = append(r.Accounts[:i], r.Accounts[i+1:]...)
			if r.ActiveAccount == name {
				r.ActiveAccount = ""
			}
			return true
		}
	}
	return false
}

// Rename changes an account's name and the fields built from it.
func (r *Registry) Rename(oldName, newName string) bool {
	for i := range r.Accounts {
		if r.Accounts[i].Name == oldName {
			r.Accounts[i].Name = newName
			r.Accounts[i].AuthRef = DeriveAuthRef(r.Accounts[i].Provider, newName)
			if r.ActiveAccount == oldName {
				r.ActiveAccount = newName
			}
			return true
		}
	}
	return false
}

// SetActive makes name the active account. The caller checks that it exists.
func (r *Registry) SetActive(name string) {
	r.ActiveAccount = name
}

// ClearActive leaves no account active.
func (r *Registry) ClearActive() {
	r.ActiveAccount = ""
}

// Clone returns a deep copy of the registry.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return nil
	}
	out := *r
	if r.Outlook != nil {
		oc := *r.Outlook
		out.Outlook = &oc
	}
	if r.Gmail != nil {
		gc := *r.Gmail
		out.Gmail = &gc
	}
	out.Accounts = make([]Account, len(r.Accounts))
	for i, account := range r.Accounts {
		out.Accounts[i] = cloneAccount(account)
	}
	return &out
}

// Clone returns a deep copy of the setup.
func (s ProviderSetup) Clone() ProviderSetup {
	var out ProviderSetup
	if s.Outlook != nil {
		v := *s.Outlook
		out.Outlook = &v
	}
	if s.Gmail != nil {
		v := *s.Gmail
		out.Gmail = &v
	}
	if s.MailServer != nil {
		v := *s.MailServer
		out.MailServer = &v
	}
	return out
}

// Equal reports whether two setups are the same.
func (s ProviderSetup) Equal(other ProviderSetup) bool {
	a := s.Clone()
	b := other.Clone()
	switch {
	case (a.Outlook != nil) != (b.Outlook != nil):
		return false
	case (a.Gmail != nil) != (b.Gmail != nil):
		return false
	case (a.MailServer != nil) != (b.MailServer != nil):
		return false
	}
	if a.Outlook != nil && *a.Outlook != *b.Outlook {
		return false
	}
	if a.Gmail != nil && *a.Gmail != *b.Gmail {
		return false
	}
	if a.MailServer != nil && *a.MailServer != *b.MailServer {
		return false
	}
	return true
}

// ProviderHasSetup reports whether the config file has setup fields for
// provider.
func ProviderHasSetup(provider string) bool {
	switch provider {
	case "outlook", "gmail", "imap", "fastmail":
		return true
	default:
		return false
	}
}

// ValidateFor checks that the setup has exactly one field set, the one for
// provider.
func (s ProviderSetup) ValidateFor(provider string) error {
	var count int
	if s.Outlook != nil {
		count++
	}
	if s.Gmail != nil {
		count++
	}
	if s.MailServer != nil {
		count++
	}
	switch provider {
	case "outlook":
		if count != 1 {
			return invalidConfig("config provider setup is invalid")
		}
		if s.Outlook == nil {
			return invalidConfig("config provider setup does not match account provider")
		}
	case "gmail":
		if count != 1 {
			return invalidConfig("config provider setup is invalid")
		}
		if s.Gmail == nil {
			return invalidConfig("config provider setup does not match account provider")
		}
	case "imap", "fastmail":
		if count != 1 {
			return invalidConfig("config provider setup is invalid")
		}
		if s.MailServer == nil {
			return invalidConfig("config provider setup does not match account provider")
		}
		if err := validateMailServerSetup(*s.MailServer); err != nil {
			return err
		}
	default:
		return invalidConfig("config provider setup does not match account provider")
	}
	return nil
}

// SetupFor builds an account's setup from the config. The result shares no
// memory with r.
func SetupFor(r *Registry, account Account) (ProviderSetup, error) {
	if r == nil {
		r = &Registry{Version: 1}
	}
	switch account.Provider {
	case "outlook":
		setup := ProviderSetup{Outlook: &OutlookSetup{}}
		if r.Outlook != nil {
			setup.Outlook.ClientID = r.Outlook.ClientID
		}
		return setup, nil
	case "gmail":
		setup := ProviderSetup{Gmail: &GmailSetup{}}
		if r.Gmail != nil {
			setup.Gmail.ClientID = r.Gmail.ClientID
		}
		return setup, nil
	case "imap", "fastmail":
		setup := ProviderSetup{MailServer: &MailServerSetup{
			Email:    account.Email,
			Username: account.Username,
		}}
		if account.MailServer != nil {
			setup.MailServer.IMAP = account.MailServer.IMAP
			setup.MailServer.SMTP = account.MailServer.SMTP
			setup.MailServer.CAFile = account.MailServer.CAFile
		}
		return setup, setup.ValidateFor(account.Provider)
	default:
		return ProviderSetup{}, invalidConfig("config provider setup does not match account provider")
	}
}

// ApplySetup writes a checked setup into the account's config fields.
func ApplySetup(r *Registry, account *Account, setup ProviderSetup) error {
	if r == nil || account == nil {
		return invalidConfig("config provider setup is invalid")
	}
	setup = setup.Clone()
	if err := setup.ValidateFor(account.Provider); err != nil {
		return err
	}
	switch account.Provider {
	case "outlook":
		if setup.Outlook.ClientID == "" {
			r.Outlook = nil
		} else {
			r.Outlook = &OutlookConfig{ClientID: setup.Outlook.ClientID}
		}
	case "gmail":
		if setup.Gmail.ClientID == "" {
			r.Gmail = nil
		} else {
			r.Gmail = &GmailConfig{ClientID: setup.Gmail.ClientID}
		}
	case "imap", "fastmail":
		account.Email = setup.MailServer.Email
		account.Username = setup.MailServer.Username
		account.MailServer = &MailServerConfig{IMAP: setup.MailServer.IMAP, SMTP: setup.MailServer.SMTP, CAFile: setup.MailServer.CAFile}
	default:
		return nil
	}
	return nil
}

// validate checks the whole config. Its errors are config file errors, also
// for names that would be input errors when typed.
func (r *Registry) validate(recognizedProviders []string) error {
	providers, err := recognizedProviderSet(recognizedProviders)
	if err != nil {
		return err
	}
	if len(r.Accounts) > MaxRegistryAccounts {
		return invalidConfig("config contains too many accounts")
	}
	names := map[string]bool{}
	for _, account := range r.Accounts {
		if !providers[account.Provider] {
			return invalidConfig(fmt.Sprintf("config references unknown provider %q", account.Provider))
		}
		if !IsValidAccountName(account.Name) {
			return invalidConfig(fmt.Sprintf("config contains invalid account name %q", account.Name))
		}
		if names[account.Name] {
			return invalidConfig(fmt.Sprintf("config contains duplicate account %q", account.Name))
		}
		names[account.Name] = true
		if account.AuthRef != "" && account.AuthRef != DeriveAuthRef(account.Provider, account.Name) {
			return invalidConfig(fmt.Sprintf("config account %q has invalid auth_ref", account.Name))
		}
		if err := validateAccountProviderShape(account); err != nil {
			return err
		}
	}
	if r.ActiveAccount != "" && !slices.ContainsFunc(r.Accounts, func(a Account) bool { return a.Name == r.ActiveAccount }) {
		return invalidConfig(fmt.Sprintf("active account %q is not configured", r.ActiveAccount))
	}
	return nil
}

func decodeOneDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, invalidConfig("the config file is not valid YAML")
	}
	var extra yaml.Node
	err := dec.Decode(&extra)
	if err == nil {
		return nil, invalidConfig("the config file must hold exactly one YAML document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, invalidConfig("the config file is not valid YAML")
	}
	return &doc, nil
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func readVersion(root *yaml.Node) (int, error) {
	if root == nil || root.Kind != yaml.MappingNode {
		return 0, invalidConfig("the config file needs a version line, version: 1")
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "version" {
			continue
		}
		version, err := strconv.Atoi(root.Content[i+1].Value)
		if err != nil {
			return 0, invalidConfig("the config file needs a version line, version: 1")
		}
		return version, nil
	}
	return 0, invalidConfig("the config file needs a version line, version: 1")
}

func normalizeRegistry(r *Registry) {
	if r == nil {
		return
	}
	if r.Outlook != nil && r.Outlook.ClientID == "" {
		r.Outlook = nil
	}
	if r.Gmail != nil && r.Gmail.ClientID == "" {
		r.Gmail = nil
	}
	for i := range r.Accounts {
		if r.Accounts[i].MailServer != nil && isZeroMailServer(*r.Accounts[i].MailServer) {
			r.Accounts[i].MailServer = nil
		}
	}
}

func cloneAccount(account Account) Account {
	out := account
	if account.MailServer != nil {
		ms := *account.MailServer
		out.MailServer = &ms
	}
	out.Policy.AllowRecipients = append([]string(nil), account.Policy.AllowRecipients...)
	return out
}

func recognizedProviderSet(names []string) (map[string]bool, error) {
	if len(names) == 0 || len(names) > MaxRecognizedProviders {
		return nil, invalidConfig("recognized provider registry is invalid")
	}
	providers := map[string]bool{}
	for _, name := range names {
		if !IsValidProviderName(name) {
			return nil, invalidConfig("recognized provider registry is invalid")
		}
		if providers[name] {
			return nil, invalidConfig("recognized provider registry is invalid")
		}
		providers[name] = true
	}
	return providers, nil
}

func isProviderName(name string) bool {
	return IsValidProviderName(name)
}

func validateAccountProviderShape(account Account) error {
	switch account.Provider {
	case "outlook":
		if account.Username != "" || account.MailServer != nil {
			return invalidConfig(fmt.Sprintf("config account %q has invalid provider setup", account.Name))
		}
	case "gmail":
		if account.Username != "" || account.MailServer != nil {
			return invalidConfig(fmt.Sprintf("config account %q has invalid provider setup", account.Name))
		}
	case "imap", "fastmail":
		if account.Email == "" || account.Username == "" || account.MailServer == nil {
			return invalidConfig(fmt.Sprintf("config account %q has incomplete mail server setup", account.Name))
		}
		if account.ProviderAccountID != "" {
			return invalidConfig(fmt.Sprintf("config account %q has invalid provider identity", account.Name))
		}
		setup := MailServerSetup{Email: account.Email, Username: account.Username, IMAP: account.MailServer.IMAP, SMTP: account.MailServer.SMTP, CAFile: account.MailServer.CAFile}
		if err := validateMailServerSetup(setup); err != nil {
			return err
		}
		if account.Provider == "fastmail" {
			if account.Email != account.Username {
				return invalidConfig(fmt.Sprintf("config account %q has invalid Fastmail setup", account.Name))
			}
			if account.MailServer.IMAP != FastmailIMAPEndpoint() || account.MailServer.SMTP != FastmailSMTPEndpoint() {
				return invalidConfig(fmt.Sprintf("config account %q has invalid Fastmail setup", account.Name))
			}
		}
	}
	return nil
}

func validateMailServerSetup(setup MailServerSetup) error {
	if setup.Email == "" || setup.Username == "" {
		return invalidConfig("config mail server setup is incomplete")
	}
	if canonical, err := NormalizeConfiguredEmail(setup.Email); err != nil || canonical != setup.Email {
		return invalidConfig("config mail server setup is invalid")
	}
	if canonical, err := NormalizeMailServerUsername(setup.Username); err != nil || canonical != setup.Username {
		return invalidConfig("config mail server setup is invalid")
	}
	if err := validateEndpoint(setup.IMAP); err != nil {
		return err
	}
	if err := validateEndpoint(setup.SMTP); err != nil {
		return err
	}
	if strings.ContainsAny(setup.CAFile, "\x00\r\n") {
		return invalidConfig("config mail server CA file path is invalid")
	}
	return nil
}

func validateEndpoint(ep ServerEndpoint) error {
	if ep.Host == "" || ep.Port == 0 {
		return invalidConfig("config mail server endpoint is invalid")
	}
	if canonical, err := NormalizeMailServerHost(ep.Host); err != nil || canonical != ep.Host {
		return invalidConfig("config mail server endpoint is invalid")
	}
	switch ep.TLS {
	case TLSImplicit, TLSStartTLS:
		return nil
	default:
		return invalidConfig("config mail server endpoint is invalid")
	}
}

// FastmailIMAPEndpoint returns Fastmail's IMAP server, which every Fastmail
// account uses.
func FastmailIMAPEndpoint() ServerEndpoint {
	return ServerEndpoint{Host: "imap.fastmail.com", Port: 993, TLS: TLSImplicit}
}

// FastmailSMTPEndpoint returns Fastmail's SMTP server, which every Fastmail
// account uses.
func FastmailSMTPEndpoint() ServerEndpoint {
	return ServerEndpoint{Host: "smtp.fastmail.com", Port: 465, TLS: TLSImplicit}
}

// NormalizeMailServerHost checks a mail server's host name or IP address
// (without brackets) and returns it in the form maildo stores.
func NormalizeMailServerHost(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", invalidConfig("mail server host is invalid")
	}
	if strings.ContainsAny(value, "\x00\r\n\t ") || strings.Contains(value, "://") ||
		strings.ContainsAny(value, "/\\?#@") || strings.HasPrefix(value, "[") ||
		strings.Contains(value, "]") || strings.Contains(value, "%") {
		return "", invalidConfig("mail server host is invalid")
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return "", invalidConfig("mail server host is invalid")
		}
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.String(), nil
	}
	if strings.Contains(value, ":") || strings.HasSuffix(value, ".") {
		return "", invalidConfig("mail server host is invalid")
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", invalidConfig("mail server host is invalid")
	}
	ascii = strings.ToLower(ascii)
	if !validDNSName(ascii) {
		return "", invalidConfig("mail server host is invalid")
	}
	return ascii, nil
}

// NormalizeConfiguredEmail checks an IMAP account's address and returns it
// in the form maildo stores. The part before the @ may hold non-ASCII
// letters (SMTPUTF8) and is kept as typed.
func NormalizeConfiguredEmail(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return "", invalidConfig("configured email is invalid")
	}
	if strings.Count(value, "@") != 1 || strings.ContainsAny(value, "<>(),;[]\"\\") {
		return "", invalidConfig("configured email is invalid")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) || unicode.Is(unicode.Zs, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return "", invalidConfig("configured email is invalid")
		}
	}
	local, domain, _ := strings.Cut(value, "@")
	if len(local) == 0 || len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", invalidConfig("configured email is invalid")
	}
	for _, atom := range strings.Split(local, ".") {
		if atom == "" {
			return "", invalidConfig("configured email is invalid")
		}
		for _, r := range atom {
			if r < utf8.RuneSelf {
				if !isEmailAtext(byte(r)) {
					return "", invalidConfig("configured email is invalid")
				}
				continue
			}
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) || unicode.Is(unicode.Zs, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				return "", invalidConfig("configured email is invalid")
			}
		}
	}
	canonicalDomain, err := NormalizeMailServerHost(domain)
	if err != nil {
		return "", invalidConfig("configured email is invalid")
	}
	out := local + "@" + canonicalDomain
	if len(out) > 254 {
		return "", invalidConfig("configured email is invalid")
	}
	return out, nil
}

// NormalizeMailServerUsername checks a login name for the IMAP and SMTP
// servers and returns it unchanged.
func NormalizeMailServerUsername(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || len(value) > 320 {
		return "", invalidConfig("mail server username is invalid")
	}
	for i := 0; i < len(value); i++ {
		if value[i] == 0 || value[i] == '\r' || value[i] == '\n' || value[i] < 0x20 || value[i] == 0x7f {
			return "", invalidConfig("mail server username is invalid")
		}
	}
	return value, nil
}

func validDNSName(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	labels := strings.Split(value, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if !isASCIILetterDigit(label[0]) || !isASCIILetterDigit(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !isASCIILetterDigit(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isASCIILetterDigit(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isEmailAtext(b byte) bool {
	if isASCIILetterDigit(b) {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '/', '=', '?', '^', '_', '`', '{', '|', '}', '~':
		return true
	default:
		return false
	}
}

func isZeroMailServer(ms MailServerConfig) bool {
	return ms.IMAP == (ServerEndpoint{}) && ms.SMTP == (ServerEndpoint{})
}

func invalidConfig(message string) error {
	return errs.New(errs.CodeInvalidConfig, message)
}

func readLimited(path string, limit int64) ([]byte, error) {
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
		return nil, fmt.Errorf("config exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("config exceeds limit")
	}
	return data, nil
}

func isWin32DeviceBasename(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	default:
		return false
	}
}
