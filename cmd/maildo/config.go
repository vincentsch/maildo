package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad"
)

// configKey is one setting `maildo config` can read and change. Account
// settings need an account; the others apply to the whole config.
type configKey struct {
	name    string
	account bool
	help    string
	get     func(reg *maildoconfig.Registry, acct *maildoconfig.Account) string
	set     func(reg *maildoconfig.Registry, acct *maildoconfig.Account, value string) error
	// loosens reports whether going from old to new removes a limit.
	loosens func(old, new string) bool
}

func boolKey(get func(p *maildoconfig.Policy) *bool) (func(*maildoconfig.Registry, *maildoconfig.Account) string, func(*maildoconfig.Registry, *maildoconfig.Account, string) error) {
	return func(_ *maildoconfig.Registry, a *maildoconfig.Account) string {
			return strconv.FormatBool(*get(&a.Policy))
		}, func(_ *maildoconfig.Registry, a *maildoconfig.Account, value string) error {
			v, err := strconv.ParseBool(value)
			if err != nil {
				return rungrad.NewError(rungrad.ExitUsage, "use true or false")
			}
			*get(&a.Policy) = v
			return nil
		}
}

var configKeys = func() []configKey {
	readOnlyGet, readOnlySet := boolKey(func(p *maildoconfig.Policy) *bool { return &p.ReadOnly })
	draftsGet, draftsSet := boolKey(func(p *maildoconfig.Policy) *bool { return &p.DraftsOnly })
	unattendedGet, unattendedSet := boolKey(func(p *maildoconfig.Policy) *bool { return &p.UnattendedSend })
	turnsOff := func(old, new string) bool { return old == "true" && new != "true" }
	return []configKey{
		{
			name: "outlook.client_id", help: "Application (client) id of your Microsoft app registration",
			get: func(r *maildoconfig.Registry, _ *maildoconfig.Account) string {
				if r.Outlook == nil {
					return ""
				}
				return r.Outlook.ClientID
			},
			set: func(r *maildoconfig.Registry, _ *maildoconfig.Account, v string) error {
				r.Outlook = nil
				if v != "" {
					r.Outlook = &maildoconfig.OutlookConfig{ClientID: v}
				}
				return nil
			},
		},
		{
			name: "gmail.client_id", help: "OAuth client id of your Google Cloud app (ends in .apps.googleusercontent.com)",
			get: func(r *maildoconfig.Registry, _ *maildoconfig.Account) string {
				if r.Gmail == nil {
					return ""
				}
				return r.Gmail.ClientID
			},
			set: func(r *maildoconfig.Registry, _ *maildoconfig.Account, v string) error {
				if v != "" && !strings.HasSuffix(v, ".apps.googleusercontent.com") {
					return rungrad.NewError(rungrad.ExitUsage, "a Google OAuth client id ends in .apps.googleusercontent.com")
				}
				r.Gmail = nil
				if v != "" {
					r.Gmail = &maildoconfig.GmailConfig{ClientID: v}
				}
				return nil
			},
		},
		{
			name: "sender_name", account: true, help: "Your name as it appears in From when you send",
			get: func(_ *maildoconfig.Registry, a *maildoconfig.Account) string { return a.SenderName },
			set: func(_ *maildoconfig.Registry, a *maildoconfig.Account, v string) error {
				if strings.ContainsAny(v, "\r\n<>\"") {
					return rungrad.NewError(rungrad.ExitUsage, "a sender name cannot contain line breaks, quotes or angle brackets")
				}
				a.SenderName = strings.TrimSpace(v)
				return nil
			},
		},
		{name: "policy.read_only", account: true, help: "true: maildo may only read; it sends, drafts and changes nothing", get: readOnlyGet, set: readOnlySet, loosens: turnsOff},
		{name: "policy.drafts_only", account: true, help: "true: maildo may read and write drafts, but not send or change mail", get: draftsGet, set: draftsSet, loosens: turnsOff},
		{
			name: "policy.allow_recipients", account: true, help: "Comma-separated addresses and @domains that sending is limited to; empty allows all",
			get: func(_ *maildoconfig.Registry, a *maildoconfig.Account) string {
				return strings.Join(a.Policy.AllowRecipients, ",")
			},
			set: func(_ *maildoconfig.Registry, a *maildoconfig.Account, v string) error {
				a.Policy.AllowRecipients = nil
				for _, entry := range strings.Split(v, ",") {
					entry = strings.ToLower(strings.TrimSpace(entry))
					if entry == "" {
						continue
					}
					if !strings.Contains(entry, "@") || strings.ContainsAny(entry, " <>\"") {
						return rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("%q is not an address or an @domain", entry))
					}
					a.Policy.AllowRecipients = append(a.Policy.AllowRecipients, entry)
				}
				return nil
			},
			loosens: func(old, new string) bool {
				if old == "" {
					return false
				}
				if new == "" {
					return true
				}
				kept := map[string]bool{}
				for _, e := range strings.Split(old, ",") {
					kept[e] = true
				}
				for _, e := range strings.Split(new, ",") {
					if !kept[strings.ToLower(strings.TrimSpace(e))] {
						return true
					}
				}
				return false
			},
		},
		{
			name: "policy.max_bulk", account: true, help: "The most messages one command may change; 0 means no extra limit",
			get: func(_ *maildoconfig.Registry, a *maildoconfig.Account) string { return strconv.Itoa(a.Policy.MaxBulk) },
			set: func(_ *maildoconfig.Registry, a *maildoconfig.Account, v string) error {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return rungrad.NewError(rungrad.ExitUsage, "use a number of messages, or 0 for no extra limit")
				}
				a.Policy.MaxBulk = n
				return nil
			},
			loosens: func(old, new string) bool {
				o, _ := strconv.Atoi(old)
				n, _ := strconv.Atoi(new)
				return o > 0 && (n == 0 || n > o)
			},
		},
		{name: "policy.unattended_send", account: true, help: "true: maildo sends from this account without an approval code and without asking; a given --approve must still match", get: unattendedGet, set: unattendedSet,
			loosens: func(old, new string) bool { return old != "true" && new == "true" }},
	}
}()

func findConfigKey(name string) (configKey, error) {
	for _, key := range configKeys {
		if key.name == name {
			return key, nil
		}
	}
	names := make([]string, 0, len(configKeys))
	for _, key := range configKeys {
		names = append(names, key.name)
	}
	return configKey{}, rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("unknown setting %q; settings are: %s", name, strings.Join(names, ", ")))
}

type configModule struct{ defs []cmdDef }

func newConfigModule(h *handlers) configModule {
	modes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	accountFlag := func(cmd *cobra.Command) {
		cmd.Flags().String("account", "", "Account the setting is for (default: the active account)")
	}
	defs := []cmdDef{
		{path: "config", use: "config", short: "Read and change maildo settings", groupID: "auth",
			examples: []string{"maildo config list", "maildo config set policy.drafts_only true --account work"}},
		{
			path: "config list", use: "list", short: "Show all settings",
			examples:    []string{"maildo config list", "maildo config list --json"},
			related:     []string{"config get", "config set"},
			outputModes: modes, args: cobra.NoArgs, run: h.configList,
		},
		{
			path: "config get", use: "get <setting>", short: "Show one setting",
			examples:    []string{"maildo config get outlook.client_id", "maildo config get policy.read_only --account work"},
			related:     []string{"config set", "config list"},
			outputModes: modes, args: cobra.ExactArgs(1), configure: accountFlag, run: h.configGet,
		},
		{
			path: "config set", use: "set <setting> <value>", short: "Change one setting",
			examples: []string{
				"maildo config set outlook.client_id 00000000-0000-0000-0000-000000000000",
				"maildo config set gmail.client_id 1234-abc.apps.googleusercontent.com",
				"maildo config set sender_name \"Ann Example\" --account work",
				"maildo config set policy.drafts_only true --account work",
				"maildo config set policy.allow_recipients \"@example.com,boss@example.org\" --account work",
				"maildo config set policy.max_bulk 50 --account work",
			},
			related:     []string{"config get", "config list"},
			outputModes: modes, mutates: true, args: cobra.ExactArgs(2), configure: accountFlag, run: h.configSet,
		},
	}
	return configModule{defs: defs}
}

func (m configModule) Groups() []rungrad.Group { return nil }

func (m configModule) Commands() []*rungrad.Command {
	parent := m.defs[0].command()
	for _, def := range m.defs[1:] {
		parent.AddCommand(def.command())
	}
	return []*rungrad.Command{parent}
}

func (m configModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

type configSetting struct {
	Setting string `json:"setting"`
	Account string `json:"account,omitempty"`
	Value   string `json:"value"`
	Help    string `json:"help,omitempty"`
}

type configListResult struct {
	Settings []configSetting `json:"settings"`
}

// configTarget picks the account an account setting applies to.
func configTarget(cmd *cobra.Command, reg *maildoconfig.Registry, key configKey) (*maildoconfig.Account, error) {
	name, _ := cmd.Flags().GetString("account")
	if !key.account {
		if name != "" {
			return nil, rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("%s is the same for all accounts, so leave out --account", key.name))
		}
		return nil, nil
	}
	if name == "" {
		name = reg.ActiveAccount
	}
	if name == "" {
		return nil, errs.New(errs.CodeMissingActiveAccount, "there is no active account; name one with --account <name>")
	}
	return lookupAccount(reg, name)
}

func (h *handlers) configList(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	reg, _, err := h.prepare(cmd.Context(), f, false)
	if err != nil {
		return err
	}
	var result configListResult
	for _, key := range configKeys {
		if !key.account {
			result.Settings = append(result.Settings, configSetting{Setting: key.name, Value: key.get(reg, nil), Help: key.help})
		}
	}
	for i := range reg.Accounts {
		for _, key := range configKeys {
			if key.account {
				result.Settings = append(result.Settings, configSetting{Setting: key.name, Account: reg.Accounts[i].Name, Value: key.get(reg, &reg.Accounts[i]), Help: key.help})
			}
		}
	}
	return f.WriteResult(result, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SETTING\tACCOUNT\tVALUE")
		for _, s := range result.Settings {
			value := s.Value
			switch {
			case value == "":
				value = "(not set)"
			case s.Setting == "policy.max_bulk" && value == "0":
				value = "0 (no limit)"
			}
			account := s.Account
			if account == "" {
				account = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Setting, account, value)
		}
		_ = tw.Flush()
	})
}

func (h *handlers) configGet(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	key, err := findConfigKey(args[0])
	if err != nil {
		return err
	}
	reg, _, err := h.prepare(cmd.Context(), f, false)
	if err != nil {
		return err
	}
	acct, err := configTarget(cmd, reg, key)
	if err != nil {
		return err
	}
	out := configSetting{Setting: key.name, Value: key.get(reg, acct), Help: key.help}
	if acct != nil {
		out.Account = acct.Name
	}
	return f.WriteResult(out, func(w io.Writer) { fmt.Fprintln(w, out.Value) })
}

// configSet changes one setting. Loosening a policy needs a person at a
// terminal, so an agent running maildo without one cannot widen its own
// limits.
func (h *handlers) configSet(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	key, err := findConfigKey(args[0])
	if err != nil {
		return err
	}
	value := args[1]
	reg, coord, err := h.prepare(cmd.Context(), f, false)
	if err != nil {
		return err
	}
	acct, err := configTarget(cmd, reg, key)
	if err != nil {
		return err
	}
	old := key.get(reg, acct)
	probe := *reg
	var probeAcct *maildoconfig.Account
	if acct != nil {
		copied := *acct
		copied.Policy.AllowRecipients = append([]string(nil), acct.Policy.AllowRecipients...)
		probeAcct = &copied
	}
	if err := key.set(&probe, probeAcct, value); err != nil {
		return err
	}
	value = key.get(&probe, probeAcct)
	out := configSetting{Setting: key.name, Value: value, Help: key.help}
	if acct != nil {
		out.Account = acct.Name
	}
	if f.DryRun() {
		preview := struct {
			DryRun bool `json:"dry_run"`
			configSetting
			OldValue string `json:"old_value"`
		}{true, out, old}
		return f.WriteResult(preview, func(w io.Writer) {
			fmt.Fprintf(w, "Would set %s from %q to %q. Nothing was changed.\n", key.name, old, value)
		})
	}
	if key.loosens != nil && key.loosens(old, value) {
		if !canPrompt(f, cmd) {
			// A policy refusal, exit 4, like every other policy error.
			return errs.NewWithDetails(errs.CodePolicyDenied,
				fmt.Sprintf("changing %s from %q to %q removes a limit, which needs a person at a terminal; nothing was changed", key.name, old, value),
				map[string]any{"setting": key.name, "hint": "only the account's owner can loosen a policy, by running this command in their own terminal"})
		}
		ok, err := f.Chooser().Confirm(fmt.Sprintf("Loosen the policy of %q? %s changes from %q to %q.", out.Account, key.name, old, value), "Yes, change it", "No, leave it")
		if err != nil || !ok {
			return rungrad.NewError(rungrad.ExitUsage, "setting left unchanged")
		}
	}
	if _, err := coord.UpdateRegistry(cmd.Context(), func(locked *maildoconfig.Registry) error {
		var target *maildoconfig.Account
		if acct != nil {
			found, ok := locked.Find(acct.Name)
			if !ok {
				return errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", acct.Name))
			}
			target = found
		}
		return key.set(locked, target, value)
	}); err != nil {
		return err
	}
	return f.WriteResult(out, func(w io.Writer) {
		shown := value
		if shown == "" {
			shown = "(not set)"
		}
		if out.Account != "" {
			fmt.Fprintf(w, "Set %s to %s for %s.\n", key.name, shown, out.Account)
		} else {
			fmt.Fprintf(w, "Set %s to %s.\n", key.name, shown)
		}
	})
}
