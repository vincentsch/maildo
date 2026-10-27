package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/rungrad"
)

// cmdDef declares one command: its help, its flags and its handler. The
// command, the generated docs and rungrad's command list all come from it, so
// they cannot drift apart.
type cmdDef struct {
	path           string
	use            string
	short          string
	examples       []string
	related        []string
	outputModes    []string
	mutates        bool
	destructive    bool
	requiresAuth   bool
	authResolution rungrad.AuthResolution
	supportsMeta   bool
	groupID        string
	args           cobra.PositionalArgs
	configure      func(*cobra.Command)
	run            func(*rungrad.Factory, *cobra.Command, []string) error
	// idArg lets the command take the message id as its argument, like
	// "maildo read 4d1a2f3", as well as with --id.
	idArg bool
}

func markMailboxAuth(defs []cmdDef, paths ...string) []cmdDef {
	marked := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		marked[path] = struct{}{}
	}
	for i := range defs {
		if _, ok := marked[defs[i].path]; !ok {
			continue
		}
		defs[i].requiresAuth = true
		defs[i].authResolution = rungrad.AuthResolutionHandler
		delete(marked, defs[i].path)
	}
	if len(marked) > 0 {
		missing := make([]string, 0, len(marked))
		for path := range marked {
			missing = append(missing, path)
		}
		panic("maildo: mailbox auth marker references missing command(s): " + strings.Join(missing, ", "))
	}
	return defs
}

// command builds the rungrad command from the declaration.
func (d cmdDef) command() *rungrad.Command {
	return &rungrad.Command{
		Use:            d.use,
		Short:          d.short,
		Examples:       append([]string(nil), d.examples...),
		Related:        append([]string(nil), d.related...),
		OutputModes:    append([]string(nil), d.outputModes...),
		Mutates:        d.mutates,
		Destructive:    d.destructive,
		RequiresAuth:   d.requiresAuth,
		AuthResolution: d.authResolution,
		SupportsMeta:   d.supportsMeta,
		GroupID:        d.groupID,
		Args:           d.args,
		Configure:      d.configure,
		Run:            d.wrapRun(),
	}
}

// wrapRun adds what every command shares: an id argument where the command
// takes one, short ids in --id, and raw strings from --jq.
func (d cmdDef) wrapRun() func(*rungrad.Factory, *cobra.Command, []string) error {
	if d.run == nil {
		// Parent commands only group their subcommands.
		return nil
	}
	return func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
		clearShortIDAnnotations(cmd)
		if d.idArg && len(args) > 0 {
			if cmd.Flags().Changed("id") {
				return rungrad.NewError(rungrad.ExitUsage, "give message ids either as arguments or with --id, not both")
			}
			for _, id := range args {
				// A repeatable --id collects every argument.
				if err := cmd.Flags().Set("id", id); err != nil {
					return err
				}
			}
			args = nil
		}
		if err := expandShortIDs(f, cmd); err != nil {
			return err
		}
		if f.Flags != nil && f.Flags.JQ != "" {
			stdout := f.Stdout
			f.Stdout = &rawJQWriter{w: stdout}
			defer func() { f.Stdout = stdout }()
		}
		return d.run(f, cmd, args)
	}
}

// catalog builds the row for rungrad's command list, which rungrad checks
// against the real command.
func (d cmdDef) catalog() rungrad.CommandSpec {
	return rungrad.CommandSpec{
		Path:         d.path,
		Summary:      d.short,
		GroupID:      d.groupID,
		OutputModes:  append([]string(nil), d.outputModes...),
		Examples:     append([]string(nil), d.examples...),
		Related:      append([]string(nil), d.related...),
		RequiresAuth: d.requiresAuth,
		Mutates:      d.mutates,
		Destructive:  d.destructive,
		SupportsMeta: d.supportsMeta,
	}
}

// authModule holds maildo login and the auth commands.
type authModule struct {
	h    *handlers
	defs []cmdDef
}

// newAuthModule declares the auth commands. maildo login and maildo auth
// login run the same handler, but each has its own examples.
func newAuthModule(h *handlers) authModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	loginFlags := func(cmd *cobra.Command) {
		cmd.Flags().String("provider", "", "Provider: imap, fastmail, gmail or outlook (default: picked from --email)")
		cmd.Flags().String("name", "", "Account name (default: suggested from the address)")
		cmd.Flags().Bool("set-active", false, "Make this the active account")
		cmd.Flags().Bool("no-set-active", false, "Leave the active account as it is")
		if err := h.deps.SetupParsers.ConfigureFlags(cmd); err != nil {
			panic(err)
		}
	}
	repairFlags := func(cmd *cobra.Command) {
		if err := h.deps.SetupParsers.ConfigureFlags(cmd); err != nil {
			panic(err)
		}
	}
	removeFlags := func(cmd *cobra.Command) {
		cmd.Flags().Bool("force", false, "Remove the active account, and leave no account active")
		cmd.Flags().String("set-active", "", "Account to make active after the removal")
	}
	statusFlags := func(cmd *cobra.Command) {
		cmd.Flags().Bool("refresh", false, "Also renew the login, to check that it still works")
	}
	defs := []cmdDef{
		{
			path: "login", use: "login", short: "Log in to a mail account", groupID: "auth",
			examples: []string{
				"maildo login",
				"maildo login --email you@example.com",
				"MAILDO_IMAP_PASSWORD=app-password maildo login --email you@posteo.de",
				"maildo login --email you@example.net --imap-host imap.example.net --smtp-host smtp.example.net",
				"maildo login --provider fastmail --name personal --email you@fastmail.com",
				"maildo login --provider outlook --name work",
				"maildo login --provider outlook --name work --device-code",
				"maildo login --provider gmail --name personal --gmail-client-id CLIENT_ID",
			},
			related:      []string{"auth login", "auth list", "auth status"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			configure:    loginFlags,
			run:          h.login,
		},
		{
			path: "auth", use: "auth", short: "Manage your accounts and their logins", groupID: "auth",
			examples: []string{"maildo auth list", "maildo auth status", "maildo auth refresh work --dry-run"},
			related:  []string{"login"},
		},
		{
			path: "auth login", use: "login", short: "Log in to a mail account",
			examples: []string{
				"maildo auth login",
				"maildo auth login --email you@example.com",
				"MAILDO_IMAP_PASSWORD=app-password maildo auth login --email you@posteo.de",
				"maildo auth login --email you@example.net --imap-host imap.example.net --smtp-host smtp.example.net",
				"maildo auth login --provider fastmail --name personal --email you@fastmail.com",
				"maildo auth login --provider outlook --name work",
				"maildo auth login --provider outlook --name work --device-code",
				"maildo auth login --provider gmail --name personal --gmail-client-id CLIENT_ID",
			},
			related:      []string{"login", "auth list", "auth status"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			configure:    loginFlags,
			run:          h.login,
		},
		{
			path: "auth list", use: "list", short: "List your accounts",
			examples:     []string{"maildo auth list", "maildo auth list --json"},
			related:      []string{"auth status", "auth login"},
			outputModes:  resultModes,
			supportsMeta: true,
			run:          h.list,
		},
		{
			path: "auth status", use: "status [name]", short: "Show whether an account has a working login",
			examples:     []string{"maildo auth status", "maildo auth status work", "maildo auth status work --refresh --dry-run", "maildo auth status work --json"},
			related:      []string{"auth refresh", "auth repair", "auth list"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.RangeArgs(0, 1),
			configure:    statusFlags,
			run:          h.status,
		},
		{
			path: "auth refresh", use: "refresh <name>", short: "Renew an account's saved login without asking you",
			examples:     []string{"maildo auth refresh work", "maildo auth refresh work --dry-run", "maildo auth refresh work --json"},
			related:      []string{"auth status", "auth repair"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			run:          h.refresh,
		},
		{
			path: "auth repair", use: "repair <name>", short: "Log in again when a saved login stopped working",
			examples:     []string{"maildo auth repair work", "maildo auth repair personal --gmail-client-id CLIENT_ID", "maildo auth repair hosted --imap-host imap.example.net --imap-port 993 --imap-tls implicit --smtp-host smtp.example.net --smtp-port 465 --smtp-tls implicit", "maildo auth repair work --device-code", "maildo auth repair work --dry-run", "maildo auth repair work --json"},
			related:      []string{"auth refresh", "auth status"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure:    repairFlags,
			run:          h.repair,
		},
		{
			path: "auth switch", use: "switch <name>", short: "Change the active account",
			examples:     []string{"maildo auth switch work", "maildo auth switch work --dry-run", "maildo auth switch work --json"},
			related:      []string{"auth list", "auth status"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			run:          h.switchAccount,
		},
		{
			path: "auth logout", use: "logout <name>", short: "Delete the saved login of an account",
			examples:     []string{"maildo auth logout work", "maildo auth logout work --dry-run", "maildo auth logout work --json"},
			related:      []string{"auth login", "auth status"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			run:          h.logout,
		},
		{
			path: "auth rename", use: "rename <old> <new>", short: "Rename an account",
			examples:     []string{"maildo auth rename work office", "maildo auth rename work office --dry-run", "maildo auth rename work office --json"},
			related:      []string{"auth list", "auth switch"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(2),
			run:          h.rename,
		},
		{
			path: "auth remove", use: "remove <name>", short: "Remove an account from maildo",
			examples:     []string{"maildo auth remove old", "maildo auth remove work --set-active personal", "maildo auth remove work --force --dry-run", "maildo auth remove old --json"},
			related:      []string{"auth logout", "auth list"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure:    removeFlags,
			run:          h.remove,
		},
	}
	return authModule{h: h, defs: defs}
}

func (m authModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "auth", Title: "Accounts and setup"}}
}

// Commands puts the auth subcommands under maildo auth, and returns maildo
// login as a command of its own.
func (m authModule) Commands() []*rungrad.Command {
	login := m.defs[0].command()
	auth := m.defs[1].command()
	for _, def := range m.defs[2:] {
		auth.AddCommand(def.command())
	}
	return []*rungrad.Command{login, auth}
}

// Catalog lists the same commands for rungrad.
func (m authModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}
