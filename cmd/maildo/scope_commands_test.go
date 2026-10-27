package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

type scopeHandlers struct {
	deps Deps
}

func (h *scopeHandlers) registryLoader(f *rungrad.Factory) provider.RegistrySnapshotLoader {
	return func(ctx context.Context) (*maildoconfig.Registry, error) {
		coord, err := auth.NewCoordinator(f.ConfigPath(), h.deps.Providers.Names())
		if err != nil {
			return nil, err
		}
		return coord.LoadRegistry(ctx, f.DryRun())
	}
}

// openResult is the tiny probe shape for proving a resolved account can build
// an authenticated account client without exposing provider internals.
type openResult struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	Ready    bool   `json:"ready"`
}

// capCheckResult is rendered only after CheckCapability succeeds. Failure paths
// return the product error directly so machine output keeps the normal error
// shape.
type capCheckResult struct {
	Account    string `json:"account"`
	Provider   string `json:"provider"`
	Capability string `json:"capability"`
	State      string `json:"state"`
}

// scopeModule is a test-only feature module. It reuses cmdDef so RunGrad help,
// docs, manifest, and catalog validation exercise the same path production
// commands use, while _test.go keeps the surface out of the built binary.
type scopeModule struct {
	h    *scopeHandlers
	defs []cmdDef
}

// newScopeModule declares the test-only commands that prove provider scope and
// capability contracts without shipping test mailbox commands.
func newScopeModule(h *scopeHandlers) scopeModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	// Keep account flag setup centralized even in tests, so manifest/help checks
	// verify the exact helper future production commands will call.
	accountFlags := func(req provider.ScopeRequest, extra func(*cobra.Command)) func(*cobra.Command) {
		return func(cmd *cobra.Command) {
			provider.ConfigureAccountFlags(cmd, req)
			if extra != nil {
				extra(cmd)
			}
		}
	}
	bulkFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeBulkQualified})
		cmd.Flags().StringArray("target", nil, "Account-qualified target in account:id form")
	}
	capCheckFlags := func(cmd *cobra.Command) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		cmd.Flags().String("capability", "", "Capability name to check")
		_ = cmd.MarkFlagRequired("capability")
	}
	defs := []cmdDef{
		{
			path: "scopekit", use: "scopekit", short: "Exercise account scope contracts", groupID: "scopekit",
			examples: []string{"maildo scopekit read-one", "maildo scopekit read-many --account work --account personal"},
		},
		{
			path: "scopekit read-one", use: "read-one", short: "Resolve one account",
			examples:     []string{"maildo scopekit read-one", "maildo scopekit read-one --account work", "maildo scopekit read-one --json"},
			related:      []string{"scopekit read-many", "scopekit read-all"},
			outputModes:  resultModes,
			supportsMeta: true,
			configure:    accountFlags(provider.ScopeRequest{Mode: provider.ModeSingle}, nil),
			run:          h.readOne,
		},
		{
			path: "scopekit read-many", use: "read-many", short: "Resolve one or more explicit accounts",
			examples:     []string{"maildo scopekit read-many", "maildo scopekit read-many --account work --account personal", "maildo scopekit read-many --account work --account personal --json"},
			related:      []string{"scopekit read-one", "scopekit read-all"},
			outputModes:  resultModes,
			supportsMeta: true,
			configure:    accountFlags(provider.ScopeRequest{Mode: provider.ModeMulti}, nil),
			run:          h.readMany,
		},
		{
			path: "scopekit read-all", use: "read-all", short: "Resolve all configured accounts",
			examples:     []string{"maildo scopekit read-all", "maildo scopekit read-all --all-accounts", "maildo scopekit read-all --all-accounts --json"},
			related:      []string{"scopekit read-one", "scopekit read-many"},
			outputModes:  resultModes,
			supportsMeta: true,
			configure:    accountFlags(provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true}, nil),
			run:          h.readAll,
		},
		{
			path: "scopekit mutate-one", use: "mutate-one", short: "Resolve one account for mutation",
			examples:     []string{"maildo scopekit mutate-one", "maildo scopekit mutate-one --account work", "maildo scopekit mutate-one --account work --json"},
			related:      []string{"scopekit bulk"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			configure:    accountFlags(provider.ScopeRequest{Mode: provider.ModeSingle}, nil),
			run:          h.mutateOne,
		},
		{
			path: "scopekit bulk", use: "bulk", short: "Resolve account-qualified mutation targets",
			examples:     []string{"maildo scopekit bulk --target work:AAA", "maildo scopekit bulk --target work:AAA --target personal:BBB", "maildo scopekit bulk --target work:AAA --json"},
			related:      []string{"scopekit mutate-one"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			configure:    bulkFlags,
			run:          h.bulk,
		},
		{
			path: "scopekit capabilities", use: "capabilities", short: "Report provider capabilities",
			examples:     []string{"maildo scopekit capabilities", "maildo scopekit capabilities --account work", "maildo scopekit capabilities --account work --json"},
			related:      []string{"scopekit cap-check"},
			outputModes:  resultModes,
			supportsMeta: true,
			configure:    accountFlags(provider.ScopeRequest{Mode: provider.ModeSingle}, nil),
			run:          h.capabilities,
		},
		{
			path: "scopekit cap-check", use: "cap-check", short: "Check one provider capability",
			examples:     []string{"maildo scopekit cap-check --capability messages_list", "maildo scopekit cap-check --account work --capability messages_list", "maildo scopekit cap-check --account work --capability messages_list --json"},
			related:      []string{"scopekit capabilities"},
			outputModes:  resultModes,
			supportsMeta: true,
			configure:    capCheckFlags,
			run:          h.capCheck,
		},
		{
			path: "scopekit open", use: "open", short: "Open an account-scoped provider client",
			examples:       []string{"maildo scopekit open", "maildo scopekit open --account work", "maildo scopekit open --account work --json"},
			related:        []string{"scopekit read-one"},
			outputModes:    resultModes,
			requiresAuth:   true,
			authResolution: rungrad.AuthResolutionHandler,
			supportsMeta:   true,
			configure:      accountFlags(provider.ScopeRequest{Mode: provider.ModeSingle}, nil),
			run:            h.open,
		},
	}
	return scopeModule{h: h, defs: defs}
}

func (m scopeModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "scopekit", Title: "Scope Kit"}}
}

func (m scopeModule) Commands() []*rungrad.Command {
	scopekit := m.defs[0].command()
	for _, def := range m.defs[1:] {
		scopekit.AddCommand(def.command())
	}
	return []*rungrad.Command{scopekit}
}

func (m scopeModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

func (h *scopeHandlers) readOne(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return h.renderSelection(f, cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
}

func (h *scopeHandlers) readMany(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return h.renderSelection(f, cmd, provider.ScopeRequest{Mode: provider.ModeMulti})
}

func (h *scopeHandlers) readAll(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return h.renderSelection(f, cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
}

func (h *scopeHandlers) mutateOne(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return h.renderSelection(f, cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
}

func (h *scopeHandlers) bulk(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	values, _ := cmd.Flags().GetStringArray("target")
	targets := make([]provider.AccountQualifiedTarget, 0, len(values))
	for _, value := range values {
		// The resolver owns validation. A token without "account:" is passed
		// through with an empty account so ResolveAccounts returns the product
		// invalid-scope error.
		account, id, ok := strings.Cut(value, ":")
		if !ok {
			account, id = "", value
		}
		targets = append(targets, provider.AccountQualifiedTarget{Account: account, ID: id})
	}
	return h.renderSelection(f, cmd, provider.ScopeRequest{
		Mode:                    provider.ModeBulkQualified,
		AccountQualifiedTargets: targets,
	})
}

func (h *scopeHandlers) capabilities(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		caps, err := h.deps.Providers.Capabilities(cmd.Context(), acct.Provider)
		if err != nil {
			return err
		}
		report := provider.NewCapabilityReport(acct, caps)
		return f.WriteResultWithMeta(report, scopeMeta(cmd.CommandPath(), sel), humanCapabilities(report))
	})
}

func (h *scopeHandlers) capCheck(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		capName, _ := cmd.Flags().GetString("capability")
		acct := sel.Accounts[0]
		caps, err := h.deps.Providers.Capabilities(cmd.Context(), acct.Provider)
		if err != nil {
			return err
		}
		capability := provider.Capability(capName)
		if err := provider.CheckCapability(acct, caps, capability); err != nil {
			return err
		}
		result := capCheckResult{
			Account:    acct.Name,
			Provider:   acct.Provider,
			Capability: capName,
			State:      string(caps.State(capability)),
		}
		return f.WriteResultWithMeta(result, scopeMeta(cmd.CommandPath(), sel), humanCapCheck(result))
	})
}

func (h *scopeHandlers) open(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := provider.AllowAccountIdentityAccessForTest(sel.Accounts[0])
		var result openResult
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessAccountIdentityForTest, func(client provider.AccountClient) error {
			result = openResult{Account: client.AccountName(), Provider: client.ProviderName(), Ready: true}
			return nil
		}); err != nil {
			return err
		}
		return f.WriteResultWithMeta(result, scopeMeta(cmd.CommandPath(), sel), humanOpen(result))
	})
}

func (h *scopeHandlers) renderSelection(f *rungrad.Factory, cmd *cobra.Command, req provider.ScopeRequest) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), req)
		if err != nil {
			return err
		}
		return f.WriteResultWithMeta(sel, scopeMeta(cmd.CommandPath(), sel), humanSelection(sel))
	})
}

func (h *scopeHandlers) clientBinder(f *rungrad.Factory) provider.AccountClientBinder {
	return (&handlers{deps: h.deps}).clientBinder(f)
}

// scopeMeta deliberately carries only account/provider names and scope shape;
// credential paths, auth refs, tokens, and provider payloads never belong in
// response metadata.
func scopeMeta(commandPath string, sel provider.AccountSelection) output.Meta {
	accounts := make([]string, 0, len(sel.Accounts))
	providers := make([]string, 0, len(sel.Accounts))
	for _, account := range sel.Accounts {
		accounts = append(accounts, account.Name)
		providers = append(providers, account.Provider)
	}
	return output.Meta{Extra: map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
	}}
}

func humanSelection(sel provider.AccountSelection) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "%s:", sel.Scope)
		for _, account := range sel.Accounts {
			fmt.Fprintf(w, " %s(%s)", account.Name, account.Provider)
		}
		fmt.Fprintln(w)
	}
}

func humanCapabilities(report provider.CapabilityReport) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "%s (%s)\n", report.Account, report.Provider)
		for _, c := range report.Capabilities {
			fmt.Fprintf(w, "%s\t%s\n", c.Name, c.State)
		}
	}
}

func humanCapCheck(result capCheckResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "%s (%s): %s %s\n", result.Account, result.Provider, result.Capability, result.State)
	}
}

func humanOpen(result openResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "%s (%s): ready=%t\n", result.Account, result.Provider, result.Ready)
	}
}
