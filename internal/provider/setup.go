package provider

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad"
)

type SetupPreviewField struct {
	Name  string
	Value string
}

type SetupResolution struct {
	Setup   maildoconfig.ProviderSetup
	Flow    InteractiveAuthFlow
	Missing []string
	Preview []SetupPreviewField
	// Notes are lines for the person logging in, such as where server
	// settings came from.
	Notes []string
}

type SetupFlagValue struct {
	Changed bool
	Value   string
}

type SetupFlagValues interface {
	Lookup(name string) (SetupFlagValue, bool)
}

type SetupResolveContext struct {
	Flags     SetupFlagValues
	Operation InteractiveAuthOperation
	Provider  string
	Account   string
	Existing  maildoconfig.ProviderSetup
	LookupEnv func(string) (string, bool, error)
}

type SetupPromptContext struct {
	Operation InteractiveAuthOperation
	Provider  string
	Account   string
	Visible   VisibleInput
}

type SetupParser interface {
	ConfigureFlags(*cobra.Command) error
	Resolve(context.Context, SetupResolveContext) (SetupResolution, error)
	PromptMissing(context.Context, SetupPromptContext, SetupResolution) (SetupResolution, error)
}

type SetupParserRegistration struct {
	ID      string
	Aliases []string
	Parser  SetupParser
}

type parserEntry struct {
	id      string
	aliases []string
	parser  SetupParser
}

type configuredCommand struct {
	owners map[string]string
}

// SetupParserRegistry holds the setup parsers of the providers, in order,
// and the flags each one adds to the login and repair commands.
type SetupParserRegistry struct {
	entries     []parserEntry
	byID        map[string]parserEntry
	aliasToID   map[string]string
	configured  map[*cobra.Command]configuredCommand
	frozen      bool
	configuring bool
}

func NewSetupParserRegistry() *SetupParserRegistry {
	return &SetupParserRegistry{
		byID:       map[string]parserEntry{},
		aliasToID:  map[string]string{},
		configured: map[*cobra.Command]configuredCommand{},
	}
}

func (r *SetupParserRegistry) Register(reg SetupParserRegistration) error {
	if r == nil || r.frozen || !IsProviderName(reg.ID) || isNil(reg.Parser) || len(reg.Aliases) == 0 {
		return errInvalidWiring
	}
	if _, exists := r.byID[reg.ID]; exists {
		return errInvalidWiring
	}
	seenAliases := map[string]bool{}
	for _, alias := range reg.Aliases {
		if !IsProviderName(alias) || seenAliases[alias] {
			return errInvalidWiring
		}
		seenAliases[alias] = true
		if owner, exists := r.aliasToID[alias]; exists && owner != reg.ID {
			return errInvalidWiring
		}
	}
	entry := parserEntry{id: reg.ID, aliases: append([]string(nil), reg.Aliases...), parser: reg.Parser}
	if r.byID == nil {
		r.byID = map[string]parserEntry{}
	}
	if r.aliasToID == nil {
		r.aliasToID = map[string]string{}
	}
	r.byID[reg.ID] = entry
	r.entries = append(r.entries, entry)
	for _, alias := range entry.aliases {
		r.aliasToID[alias] = reg.ID
	}
	return nil
}

func (r *SetupParserRegistry) ConfigureFlags(cmd *cobra.Command) error {
	if r == nil || cmd == nil {
		return errInvalidWiring
	}
	if r.configured == nil {
		r.configured = map[*cobra.Command]configuredCommand{}
	}
	if _, exists := r.configured[cmd]; exists {
		return errInvalidWiring
	}
	owners := map[string]string{}
	for _, entry := range r.entries {
		before := localFlags(cmd)
		if err := entry.parser.ConfigureFlags(cmd); err != nil {
			return errInvalidWiring
		}
		after := localFlags(cmd)
		for name, flag := range before {
			if after[name] != flag {
				return errInvalidWiring
			}
		}
		for name := range after {
			if _, existed := before[name]; existed {
				continue
			}
			if _, taken := owners[name]; taken {
				return errInvalidWiring
			}
			owners[name] = entry.id
		}
	}
	r.configured[cmd] = configuredCommand{owners: owners}
	return nil
}

func (r *SetupParserRegistry) Lookup(alias string) (id string, parser SetupParser, ok bool) {
	if r == nil {
		return "", nil, false
	}
	id, ok = r.aliasToID[alias]
	if !ok {
		return "", nil, false
	}
	entry, ok := r.byID[id]
	if !ok {
		return "", nil, false
	}
	return id, entry.parser, true
}

func (r *SetupParserRegistry) RejectForeignChangedFlags(cmd *cobra.Command, providerAlias string) error {
	if r == nil || cmd == nil {
		return errInvalidWiring
	}
	selectedID, _, ok := r.Lookup(providerAlias)
	if !ok {
		return errs.New(errs.CodeProviderNotAvailable, fmt.Sprintf("provider %q is not available", providerAlias))
	}
	cfg, ok := r.configured[cmd]
	if !ok {
		return errInvalidWiring
	}
	for name, owner := range cfg.owners {
		if owner == selectedID {
			continue
		}
		flag := cmd.Flags().Lookup(name)
		if flag != nil && flag.Changed {
			return rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("--%s is not valid for provider %q", name, providerAlias))
		}
	}
	return nil
}

func (r *SetupParserRegistry) Snapshot(cmd *cobra.Command, providerAlias string) (SetupFlagValues, error) {
	if r == nil || cmd == nil {
		return selectedSetupFlags{}, errInvalidWiring
	}
	selectedID, _, ok := r.Lookup(providerAlias)
	if !ok {
		return selectedSetupFlags{}, errs.New(errs.CodeProviderNotAvailable, fmt.Sprintf("provider %q is not available", providerAlias))
	}
	cfg, ok := r.configured[cmd]
	if !ok {
		return selectedSetupFlags{}, errInvalidWiring
	}
	values := map[string]SetupFlagValue{}
	for name, owner := range cfg.owners {
		if owner != selectedID {
			continue
		}
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			return selectedSetupFlags{}, errInvalidWiring
		}
		values[name] = SetupFlagValue{Changed: flag.Changed, Value: flag.Value.String()}
	}
	return selectedSetupFlags{values: values}, nil
}

func (r *SetupParserRegistry) IDs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, entry.id)
	}
	return out
}

func (r *SetupParserRegistry) ProviderNames() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, entry := range r.entries {
		out = append(out, entry.aliases...)
	}
	return out
}

func (r *SetupParserRegistry) Validate() error {
	if r == nil {
		return errInvalidWiring
	}
	seenIDs := map[string]bool{}
	seenAliases := map[string]bool{}
	for _, entry := range r.entries {
		if !IsProviderName(entry.id) || seenIDs[entry.id] || isNil(entry.parser) || len(entry.aliases) == 0 {
			return errInvalidWiring
		}
		seenIDs[entry.id] = true
		for _, alias := range entry.aliases {
			if !IsProviderName(alias) || seenAliases[alias] || r.aliasToID[alias] != entry.id {
				return errInvalidWiring
			}
			seenAliases[alias] = true
		}
		if r.byID[entry.id].parser != entry.parser {
			return errInvalidWiring
		}
	}
	r.frozen = true
	return nil
}

func ValidateProviderParserPairs(providers *Registry, parsers *SetupParserRegistry) error {
	if providers == nil || parsers == nil {
		return errInvalidWiring
	}
	providerNames := providers.Names()
	parserNames := parsers.ProviderNames()
	if len(providerNames) != len(parserNames) {
		return errInvalidWiring
	}
	seen := map[string]bool{}
	for _, name := range parserNames {
		seen[name] = true
	}
	for _, name := range providerNames {
		if !seen[name] {
			return errInvalidWiring
		}
	}
	return nil
}

func localFlags(cmd *cobra.Command) map[string]*pflag.Flag {
	out := map[string]*pflag.Flag{}
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		out[flag.Name] = flag
	})
	return out
}

type selectedSetupFlags struct {
	values map[string]SetupFlagValue
}

func (s selectedSetupFlags) Lookup(name string) (SetupFlagValue, bool) {
	value, ok := s.values[name]
	return value, ok
}
