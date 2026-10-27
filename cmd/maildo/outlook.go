package main

import (
	"reflect"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// outlookModule holds the commands that only work with Outlook accounts.
type outlookModule struct {
	h    *handlers
	defs []cmdDef
}

// newOutlookModule builds "outlook archive-policy check" and the two parent
// commands above it, which only show help.
func newOutlookModule(h *handlers) outlookModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	defs := []cmdDef{
		{
			path: "outlook", use: "outlook", short: "Commands that only work with Outlook accounts", groupID: "outlook",
			examples: []string{"maildo outlook archive-policy check"},
			related:  []string{"rules health", "folders list"},
			args:     cobra.NoArgs,
			run:      showOutlookHelp,
		},
		{
			path: "outlook archive-policy", use: "archive-policy", short: "Check Outlook archive and retention settings",
			examples: []string{"maildo outlook archive-policy check"},
			related:  []string{"folders list", "rules health"},
			args:     cobra.NoArgs,
			run:      showOutlookHelp,
		},
		{
			path: "outlook archive-policy check", use: "check", short: "Show which archive and retention settings maildo can see",
			examples: []string{
				"maildo outlook archive-policy check",
				"maildo outlook archive-policy check --account work",
				"maildo outlook archive-policy check --json",
			},
			related:      []string{"folders list", "rules health"},
			outputModes:  resultModes,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.outlookArchivePolicyCheck,
		},
	}
	defs = markMailboxAuth(defs, "outlook archive-policy check")
	return outlookModule{h: h, defs: defs}
}

// showOutlookHelp prints help for the parent commands. Their NoArgs check
// still rejects a mistyped subcommand.
func showOutlookHelp(_ *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

func (m outlookModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "outlook", Title: "Outlook"}}
}

// Commands nests the commands by hand, so "outlook archive-policy check"
// works and a shortcut like "outlook check" does not.
func (m outlookModule) Commands() []*rungrad.Command {
	parent := m.defs[0].command()
	archivePolicy := m.defs[1].command()
	archivePolicy.AddCommand(m.defs[2].command())
	parent.AddCommand(archivePolicy)
	return []*rungrad.Command{parent}
}

func (m outlookModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

// outlookArchivePolicyCheck refuses non-Outlook accounts before it opens a
// saved login, and checks that the report is for the selected account
// before it prints anything.
func (h *handlers) outlookArchivePolicyCheck(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		if acct.Provider != "outlook" {
			return errs.NewWithDetails(
				errs.CodeUnsupportedCapability,
				"outlook archive-policy check works only with Outlook accounts",
				map[string]any{
					"account": acct.Name, "provider": acct.Provider,
					"required_provider": "outlook", "capability": string(provider.CapArchivePolicyReport),
				},
			)
		}
		var report mail.ArchivePolicyReport
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessArchivePolicyReport, func(reporter provider.ArchivePolicyReporter) error {
			var err error
			report, err = reporter.ArchivePolicyReport(cmd.Context(), mail.ArchivePolicyReportRequest{})
			return err
		}); err != nil {
			return err
		}
		if report.Account != acct.Name || report.Provider != acct.Provider {
			return archivePolicySelectionError(acct)
		}
		result := archivePolicyCheckResult{AccountSelection: sel, ArchivePolicyReport: report}
		return f.WriteResultWithMeta(result, archivePolicyMeta(cmd.CommandPath(), result), humanArchivePolicy(result))
	})
}

// nilAccountClient is true for a nil client, also when it is a nil pointer
// inside a non-nil interface. Calling methods on either would panic.
func nilAccountClient(client provider.AccountClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// archivePolicySelectionError names only the account the user selected, so a
// report for some other account cannot put its text into the error.
func archivePolicySelectionError(acct provider.ResolvedAccount) error {
	return rungrad.NewError(rungrad.ExitAPI, "could not read archive policy for account "+strconvQuote(acct.Name)+" (outlook)")
}
