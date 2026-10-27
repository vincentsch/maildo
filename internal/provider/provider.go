// Package provider defines what a mail provider such as IMAP or Gmail
// implements, so the CLI can log in and work with the mailbox.
//
// The real providers and the test fakes implement the same interfaces.
package provider

import (
	"context"
	"errors"
	"reflect"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/rungrad"
)

type InteractiveAuthOperation string

const (
	InteractiveLogin  InteractiveAuthOperation = "login"
	InteractiveRepair InteractiveAuthOperation = "repair"
)

type InteractiveAuthFlow string

const (
	InteractiveAuthFlowDefault    InteractiveAuthFlow = ""
	InteractiveAuthFlowDeviceCode InteractiveAuthFlow = "device_code"
)

// SecretRegistrar lets a provider register a secret so maildo hides it in
// all output. It is all a provider gets outside login and repair.
type SecretRegistrar interface {
	RegisterSecret(string) error
}

// ProviderRuntime is what a provider may use for a normal command with a
// saved login. It cannot read environment variables, open a browser, ask
// questions or print notices.
type ProviderRuntime interface {
	SecretRegistrar
}

// InteractiveProviderRuntime is only handed out during login and repair.
type InteractiveProviderRuntime interface {
	ProviderRuntime
	LookupSecretEnv(string) (string, bool, error)
	OpenBrowser(context.Context, string) error
	WriteNotice(string) error
}

// VisibleInput reads non-secret setup values.
type VisibleInput interface {
	ReadLine(context.Context, string, string) (string, error)
}

// SecretReader reads a password from the terminal without echoing it.
type SecretReader interface {
	ReadHidden(context.Context, *rungrad.Factory, string) ([]byte, error)
}

// InteractiveSecretSource gives a provider a password typed during login,
// and overwrites it with zeros when the callback returns.
type InteractiveSecretSource interface {
	WithSecret(context.Context, string, func([]byte) error) error
}

var (
	ErrProviderRuntimeClosed      = errors.New("provider_runtime_closed")
	ErrProviderRuntimeDenied      = errors.New("provider_runtime_denied")
	ErrCredentialCapabilityClosed = errors.New("credential_capability_closed")
	ErrCredentialDestroyed        = errors.New("credential_destroyed")
	ErrAccountClientInvalid       = errors.New("account_client_invalid")
)

// Credential is a decoded login (password or tokens). Destroy wipes it.
type Credential interface {
	Destroy()
	Destroyed() bool
}

type CredentialPresence string

const (
	CredentialAbsent  CredentialPresence = "absent"
	CredentialPresent CredentialPresence = "present"
)

// CredentialSource lets repair read the saved login once, if there is one.
type CredentialSource interface {
	WithBytes(context.Context, func([]byte) error) (CredentialPresence, error)
}

// CredentialLease gives access to the saved login during one callback only.
type CredentialLease interface {
	Current() Credential
	Replace(Credential) error
}

// CredentialSession runs one callback with the saved login and wipes the
// login afterwards.
type CredentialSession interface {
	WithCurrent(context.Context, func(CredentialLease) error) error
}

type IdentityAssurance string

const (
	IdentityProviderVerified IdentityAssurance = "provider_verified"
	IdentityConfigured       IdentityAssurance = "configured"
)

// Identity is who the login belongs to. Login returns it and maildo saves
// it with the account. It holds no secrets.
type Identity struct {
	Email             string
	DisplayName       string
	ProviderAccountID string
	Assurance         IdentityAssurance
}

type InteractiveAuthContext struct {
	Runtime   InteractiveProviderRuntime
	Operation InteractiveAuthOperation
	Flow      InteractiveAuthFlow
	Provider  string
	Account   string
	Setup     maildoconfig.ProviderSetup
	Existing  CredentialSource
	Secrets   InteractiveSecretSource
}

type InteractiveAuthResult struct {
	Setup      maildoconfig.ProviderSetup
	Credential Credential
	Identity   Identity
}

type StoredAuthContext struct {
	Runtime                   ProviderRuntime
	Provider                  string
	Account                   string
	Setup                     maildoconfig.ProviderSetup
	ExpectedEmail             string
	ExpectedDisplayName       string
	ExpectedProviderAccountID string
	// SenderName is the display name for From on mail this account writes.
	SenderName string
	Credential CredentialSession
}

// RefreshResult says what a refresh without user input changed.
type RefreshResult struct {
	RefreshChecked      bool
	CredentialChanged   bool
	RotatedRefreshToken bool
}

// LocalStatus says whether the saved login looks usable, judged without
// asking the server.
type LocalStatus int

const (
	LocalAbsent LocalStatus = iota
	LocalUsable
	LocalRepairRequired
)

// AccountClient is a logged-in connection to one account. The mailbox
// interfaces (reading, sending and so on) are implemented by the concrete
// clients.
type AccountClient interface {
	ProviderName() string
	AccountName() string
	Destroy()
}

// Provider is what every provider implements. Only InteractiveAuth may open
// a browser, print notices, read the environment or ask for a password.
type Provider interface {
	Name() string
	Capabilities(context.Context) (Capabilities, error)
	InteractiveAuth(context.Context, InteractiveAuthContext) (InteractiveAuthResult, error)
	LocalHealth(context.Context, StoredAuthContext) (LocalStatus, error)
	ForcedRefresh(context.Context, StoredAuthContext) (RefreshResult, error)
	Logout(context.Context, StoredAuthContext) error
	NewAccountClient(context.Context, StoredAuthContext) (AccountClient, error)
}

// CredentialCodec turns a provider's saved login into bytes and back.
type CredentialCodec interface {
	ProviderName() string
	Decode(SecretRegistrar, auth.CredentialBinding, []byte) (Credential, error)
	Encode(auth.CredentialBinding, Credential) ([]byte, error)
	RegisterSecrets(SecretRegistrar, Credential) error
	LocalHealth(maildoconfig.ProviderSetup, auth.CredentialBinding, Credential) (LocalStatus, error)
	RefreshTokenRotated(auth.CredentialBinding, Credential, Credential) (bool, error)
	RebindCredential(SecretRegistrar, []byte, auth.CredentialBinding, auth.CredentialBinding) (auth.CredentialRebindResult, error)
}

type ProviderRegistration struct {
	Name      string
	Lifecycle Provider
	Codec     CredentialCodec
}

type providerEntry struct {
	name      string
	lifecycle Provider
	codec     CredentialCodec
}

// Registry maps provider names to their implementation and codec. Tests
// register fakes here; the real program registers the real providers.
type Registry struct {
	byName map[string]providerEntry
	order  []string
	frozen bool
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]providerEntry{}}
}

// Register adds a provider. It refuses a name that is already registered.
func (r *Registry) Register(reg ProviderRegistration) error {
	if r == nil {
		return errInvalidWiring
	}
	if r.frozen {
		return errInvalidWiring
	}
	if !IsProviderName(reg.Name) || isNil(reg.Lifecycle) || isNil(reg.Codec) {
		return errInvalidWiring
	}
	lifecycleName, err := safeProviderName(reg.Lifecycle)
	if err != nil || lifecycleName != reg.Name {
		return errInvalidWiring
	}
	codecName, err := safeCodecName(reg.Codec)
	if err != nil || codecName != reg.Name {
		return errInvalidWiring
	}
	if r.byName == nil {
		r.byName = map[string]providerEntry{}
	}
	if _, exists := r.byName[reg.Name]; exists {
		return errInvalidWiring
	}
	r.byName[reg.Name] = providerEntry{name: reg.Name, lifecycle: reg.Lifecycle, codec: reg.Codec}
	r.order = append(r.order, reg.Name)
	return nil
}

func (r *Registry) Get(name string) (Provider, CredentialCodec, bool) {
	if r == nil {
		return nil, nil, false
	}
	entry, ok := r.byName[name]
	return entry.lifecycle, entry.codec, ok
}

// Names returns the provider names in the order they were registered.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

// Validate checks every entry again and, if all is well, locks the registry
// against further changes.
func (r *Registry) Validate() error {
	if r == nil {
		return errInvalidWiring
	}
	seen := map[string]bool{}
	for _, name := range r.order {
		entry, ok := r.byName[name]
		if !ok || seen[name] || !IsProviderName(name) || isNil(entry.lifecycle) || isNil(entry.codec) {
			return errInvalidWiring
		}
		seen[name] = true
		lifecycleName, err := safeProviderName(entry.lifecycle)
		if err != nil || lifecycleName != name {
			return errInvalidWiring
		}
		codecName, err := safeCodecName(entry.codec)
		if err != nil || codecName != name {
			return errInvalidWiring
		}
	}
	if len(seen) != len(r.byName) {
		return errInvalidWiring
	}
	r.frozen = true
	return nil
}

var errInvalidWiring = errors.New("invalid application wiring")

func safeProviderName(p Provider) (name string, err error) {
	defer func() {
		if recover() != nil {
			name = ""
			err = errInvalidWiring
		}
	}()
	name = p.Name()
	if !IsProviderName(name) {
		return "", errInvalidWiring
	}
	return name, nil
}

func safeCodecName(c CredentialCodec) (name string, err error) {
	defer func() {
		if recover() != nil {
			name = ""
			err = errInvalidWiring
		}
	}()
	name = c.ProviderName()
	if !IsProviderName(name) {
		return "", errInvalidWiring
	}
	return name, nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// IsProviderName reports whether name is a valid provider name or alias.
func IsProviderName(name string) bool {
	return maildoconfig.IsValidProviderName(name)
}
