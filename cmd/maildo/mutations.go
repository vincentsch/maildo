package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/search"
	"github.com/vincentsch/rungrad"
)

const (
	maxExplicitMutationTargets = 1000
	defaultBulkMaxTargets      = 100
	maxIDsFileBytes            = 1 << 20
	maxIDsFileLineBytes        = 8 << 10
)

// mutationsModule holds the commands that change messages: move, archive,
// mark, flag, categories and the rest.
type mutationsModule struct {
	h    *handlers
	defs []cmdDef
}

// mutationSpec is what differs between the message-changing commands that
// otherwise work the same way.
type mutationSpec struct {
	operation    mail.MessageMutationOperation
	capability   provider.Capability
	targetFolder bool
}

// mutationTarget is one message a command will change. A message given by id
// may have only MessageID. One found by a search or by --internet-id also
// carries its subject and sender for the output, without its body.
type mutationTarget struct {
	Selection         provider.AccountSelection
	Searched          *provider.AccountSelection
	Account           provider.ResolvedAccount
	Selector          mail.MessageSelector
	SelectorKind      string
	MessageID         string
	InternetMessageID string
	Summary           *mail.MessageSummary
	// Described is true when Summary came from a metadata read rather than
	// a search, so its read and flag state is not known.
	Described bool
	// Problem is why a preview could not find the message.
	Problem string
}

func newMutationsModule(h *handlers) mutationsModule {
	resultModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	selectorFlags := func(cmd *cobra.Command, many bool) {
		provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
		if many {
			cmd.Flags().StringArray("id", nil, "Message id, or a short id from list or search (repeatable)")
			cmd.Flags().String("query", "", "Search for messages; use with --all, --latest or --first")
		} else {
			cmd.Flags().StringArray("id", nil, "Message id, or a short id from list or search")
			cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
		}
		cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
		cmd.Flags().Bool("latest", false, "With --query, use the newest match")
		cmd.Flags().Bool("first", false, "With --query, the same as --latest")
		cmd.Flags().String("folder", "", "With --query, search only this folder; without it, Sent, Drafts, Trash and Junk are left out (restore looks only in Trash and Junk)")
	}
	singleSelectorFlags := func(cmd *cobra.Command) { selectorFlags(cmd, false) }
	bulkSelectorFlags := func(cmd *cobra.Command) {
		selectorFlags(cmd, true)
		cmd.Flags().String("ids-file", "", "Read message ids from a file, one per line, or from stdin with -")
		cmd.Flags().Bool("all", false, "With --query, use every match, up to --max-targets")
		cmd.Flags().Int("max-targets", defaultBulkMaxTargets, "Most messages --all may pick, at most 1000")
		cmd.Flags().Bool("apply", false, "Make the change; without it, a change to more than one message only shows a preview")
	}
	withTo := func(cmd *cobra.Command) {
		bulkSelectorFlags(cmd)
		cmd.Flags().String("to", "", "Folder to move or copy the messages to")
	}
	withCategory := func(cmd *cobra.Command) {
		bulkSelectorFlags(cmd)
		cmd.Flags().StringArray("category", nil, "Category name (repeatable)")
	}
	defs := []cmdDef{
		{
			path: "move", use: "move", short: "Move messages to another folder", groupID: "mutations",
			examples: []string{
				`maildo move --id 3c1f2a7 --account work --to archive`,
				`maildo move 4d1a2f3 9be01c4 --to Clients`,
				`maildo move --id 3c1f2a7 --id 9be01c4 --to "Clients/ACME"`,
				`maildo move --ids-file ids.txt --to archive --apply --json`,
				`maildo move --query "from:client@example.com" --all --max-targets 25 --to archive --apply`,
			},
			related:      []string{"copy", "restore", "folders list"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    withTo,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationMove, capability: provider.CapMessagesMove, targetFolder: true})
			},
		},
		{
			path: "copy", use: "copy", short: "Copy messages to another folder (on Gmail, this adds a label)", groupID: "mutations",
			examples: []string{
				`maildo copy --id 3c1f2a7 --account work --to archive`,
				`maildo copy 4d1a2f3 9be01c4 --to Clients`,
				`maildo copy --id 3c1f2a7 --id 9be01c4 --to "Clients/ACME"`,
				`maildo copy --internet-id "<message-id@example.com>" --to archive --apply --json`,
				`maildo copy --query "invoice" --all --to archive --apply`,
			},
			related:      []string{"move", "restore", "folders list"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    withTo,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationCopy, capability: provider.CapMessagesCopy, targetFolder: true})
			},
		},
		{
			path: "archive", use: "archive", short: "Archive messages", groupID: "mutations",
			examples: []string{
				`maildo archive --id 3c1f2a7 --account work`,
				`maildo archive 4d1a2f3 9be01c4`,
				`maildo archive --id 3c1f2a7 --id 9be01c4`,
				`maildo archive --ids-file ids.txt --apply --json`,
				`maildo archive --query "invoice" --all --max-targets 50 --apply`,
			},
			related:      []string{"move", "delete", "junk"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationArchive, capability: provider.CapMessagesArchive})
			},
		},
		{
			path: "delete", use: "delete", short: "Move messages to Trash", groupID: "mutations",
			examples: []string{
				`maildo delete --id 3c1f2a7 --account work`,
				`maildo delete 4d1a2f3 9be01c4`,
				`maildo delete --id 3c1f2a7 --id 9be01c4`,
				`maildo delete --internet-id "<message-id@example.com>" --apply --json`,
				`maildo delete --query "spam" --all --apply`,
			},
			related:      []string{"archive", "restore", "junk", "draft delete"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationDelete, capability: provider.CapMessagesDelete})
			},
		},
		{
			path: "restore", use: "restore", short: "Move messages back from Trash or Junk (to the inbox, or to --to)", groupID: "mutations",
			examples: []string{
				`maildo restore 4d1a2f3`,
				`maildo restore --id 3c1f2a7 --account work --to Clients`,
				`maildo restore 4d1a2f3 9be01c4 --to inbox`,
				`maildo restore --id 3c1f2a7 --id 9be01c4 --to inbox`,
				`maildo restore --ids-file ids.txt --to inbox --apply --json`,
				`maildo restore --query "invoice" --all --to inbox --apply`,
			},
			related:      []string{"delete", "move", "folders list"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure: func(cmd *cobra.Command) {
				bulkSelectorFlags(cmd)
				cmd.Flags().String("to", "", "Folder to put the messages back in (default: the inbox)")
			},
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationRestore, capability: provider.CapMessagesRestore, targetFolder: true})
			},
		},
		{
			path: "junk", use: "junk", short: "Move messages to the Junk folder", groupID: "mutations",
			examples: []string{
				`maildo junk --id 3c1f2a7 --account work`,
				`maildo junk 4d1a2f3 9be01c4`,
				`maildo junk --id 3c1f2a7 --id 9be01c4`,
				`maildo junk --ids-file ids.txt --apply --json`,
				`maildo junk --query "spam" --all --apply`,
			},
			related:      []string{"archive", "delete", "move"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationJunk, capability: provider.CapMessagesJunk})
			},
		},
		{
			path: "mark", use: "mark", short: "Mark messages as read or unread", groupID: "mutations",
			examples: []string{`maildo mark read --id 3c1f2a7`, `maildo mark unread --id 3c1f2a7 --dry-run`},
			related:  []string{"flag set", "flag clear"},
		},
		{
			path: "mark read", use: "read", short: "Mark messages as read",
			examples:     []string{`maildo mark read 4d1a2f3`, `maildo mark read --id 3c1f2a7 --account work`, `maildo mark read --id 3c1f2a7 --id 9be01c4`, `maildo mark read --query "invoice" --all --apply --json`},
			related:      []string{"mark unread"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationMarkRead, capability: provider.CapMessagesMarkRead})
			},
		},
		{
			path: "mark unread", use: "unread", short: "Mark messages as unread",
			examples:     []string{`maildo mark unread --id 3c1f2a7 --account work`, `maildo mark unread --id 3c1f2a7 --id 9be01c4`, `maildo mark unread --ids-file ids.txt --apply --json`, `maildo mark unread --query "invoice" --all --dry-run --json`},
			related:      []string{"mark read"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationMarkUnread, capability: provider.CapMessagesMarkRead})
			},
		},
		{
			path: "flag", use: "flag", short: "Flag messages or remove the flag", groupID: "mutations",
			examples: []string{`maildo flag set --id 3c1f2a7`, `maildo flag clear --id 3c1f2a7 --dry-run`},
			related:  []string{"mark read", "mark unread"},
		},
		{
			path: "flag set", use: "set", short: "Flag messages",
			examples:     []string{`maildo flag set 4d1a2f3`, `maildo flag set --id 3c1f2a7 --account work`, `maildo flag set --id 3c1f2a7 --id 9be01c4`, `maildo flag set --query "invoice" --all --apply --json`},
			related:      []string{"flag clear"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationFlagSet, capability: provider.CapMessagesFlag})
			},
		},
		{
			path: "flag clear", use: "clear", short: "Remove the flag from messages",
			examples:     []string{`maildo flag clear --id 3c1f2a7 --account work`, `maildo flag clear --id 3c1f2a7 --id 9be01c4`, `maildo flag clear --ids-file ids.txt --apply --json`, `maildo flag clear --query "invoice" --all --dry-run --json`},
			related:      []string{"flag set"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    bulkSelectorFlags,
			run: func(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
				return h.runMessageMutation(f, cmd, mutationSpec{operation: mail.MutationFlagClear, capability: provider.CapMessagesFlag})
			},
		},
		{
			path: "categories", use: "categories", short: "Show or replace message categories (on Gmail, your own labels)", groupID: "mutations",
			examples: []string{`maildo categories list --id 3c1f2a7`, `maildo categories set --id 3c1f2a7 --category Client`},
			related:  []string{"read", "search"},
		},
		{
			path: "categories list", use: "list", short: "List the categories of a message (on Gmail, your own labels)",
			examples:     []string{`maildo categories list --id 3c1f2a7`, `maildo categories list --internet-id "<message-id@example.com>" --account work --json`, `maildo categories list --query "invoice" --latest --json`},
			related:      []string{"categories set", "categories clear"},
			outputModes:  resultModes,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    singleSelectorFlags,
			run:          h.categoriesList,
		},
		{
			path: "categories set", use: "set", short: "Replace the categories of messages (on Gmail, your own labels)",
			examples:     []string{`maildo categories set --id 3c1f2a7 --account work --category Client`, `maildo categories set --id 3c1f2a7 --id 9be01c4 --category Client`, `maildo categories set --ids-file ids.txt --category Bills --apply --json`, `maildo categories set --query "invoice" --all --category Client --dry-run --json`},
			related:      []string{"categories list", "categories clear"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    withCategory,
			run:          h.categoriesSet,
		},
		{
			path: "categories clear", use: "clear", short: "Remove categories from messages (on Gmail, your own labels)",
			examples:     []string{`maildo categories clear --id 3c1f2a7 --account work`, `maildo categories clear --id 3c1f2a7 --id 9be01c4 --category Client`, `maildo categories clear --query "invoice" --all --category Bills --apply --json`},
			related:      []string{"categories list", "categories set"},
			outputModes:  resultModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.NoArgs,
			configure:    withCategory,
			run:          h.categoriesClear,
		},
	}
	// Message ids can also come as arguments: maildo archive 4d1a2f3 9be01c4.
	for i := range defs {
		if defs[i].run == nil {
			continue
		}
		defs[i].idArg = true
		defs[i].use += " [id...]"
		if defs[i].path == "categories list" {
			defs[i].use = strings.TrimSuffix(defs[i].use, " [id...]") + " [id]"
			defs[i].args = cobra.MaximumNArgs(1)
		} else {
			defs[i].args = cobra.ArbitraryArgs
		}
	}
	defs = markMailboxAuth(defs,
		"move",
		"copy",
		"archive",
		"delete",
		"restore",
		"junk",
		"mark read",
		"mark unread",
		"flag set",
		"flag clear",
		"categories list",
		"categories set",
		"categories clear",
	)
	return mutationsModule{h: h, defs: defs}
}

func (m mutationsModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "mutations", Title: "Change messages"}}
}

func (m mutationsModule) Commands() []*rungrad.Command {
	byPath := map[string]*rungrad.Command{}
	var top []*rungrad.Command
	for _, def := range m.defs {
		cmd := def.command()
		byPath[def.path] = cmd
		switch {
		case strings.HasPrefix(def.path, "mark "):
			byPath["mark"].AddCommand(cmd)
		case strings.HasPrefix(def.path, "flag "):
			byPath["flag"].AddCommand(cmd)
		case strings.HasPrefix(def.path, "categories "):
			byPath["categories"].AddCommand(cmd)
		default:
			top = append(top, cmd)
		}
	}
	return top
}

func (m mutationsModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

// parseMutationSelector reads how the user picked one message. --id is
// repeatable for the commands that change many messages, so commands that
// work on one message refuse a second --id here.
func parseMutationSelector(cmd *cobra.Command) (mail.MessageSelector, error) {
	flags := cmd.Flags()
	id := ""
	hasID := flags.Changed("id")
	if hasID {
		ids, _ := flags.GetStringArray("id")
		switch len(ids) {
		case 0:
			return mail.MessageSelector{}, errs.New(errs.CodeInvalidSelector, "--id requires a value")
		case 1:
			id = strings.TrimSpace(ids[0])
			if id == "" {
				return mail.MessageSelector{}, errs.New(errs.CodeInvalidSelector, "--id requires a value")
			}
		default:
			return mail.MessageSelector{}, errs.New(errs.CodeInvalidSelector, "this command works on one message, so give only one --id")
		}
	}
	queryRaw, _ := flags.GetString("query")
	hasQuery := flags.Changed("query")
	var query *mail.Query
	if hasQuery {
		parsed, err := search.ParseQuery(queryRaw)
		if err != nil {
			return mail.MessageSelector{}, err
		}
		query = &parsed
	}
	internetID, _ := flags.GetString("internet-id")
	latest, _ := flags.GetBool("latest")
	first, _ := flags.GetBool("first")
	return mail.ParseMessageSelector(mail.SelectorFlags{
		ID: id, HasID: hasID,
		InternetID: strings.TrimSpace(internetID), HasInternetID: flags.Changed("internet-id"),
		Query: query, HasQuery: hasQuery,
		Latest: latest, First: first,
	})
}

// parsedMutationSelection is how the user picked the messages, checked but
// not yet looked up in any account.
type parsedMutationSelection struct {
	Kind        string
	Selector    mail.MessageSelector
	QueryRaw    string
	IDs         []string
	Qualified   []provider.AccountQualifiedTarget
	IDsFileBase string
	MaxTargets  int
	Bulk        bool
	// Scope says where a --query looks.
	Scope searchScope
}

// searchScope is where --query looks: the folder named with --folder, or
// Trash and Junk for restore.
type searchScope struct {
	folder  string
	restore bool
}

// mutationTargetSet is the list of messages a command will change, used by
// both the preview and the change itself. The order is the order in which
// the user gave them, or the search order.
type mutationTargetSet struct {
	Selection provider.AccountSelection
	Summary   mail.BulkSelectorSummary
	Targets   []mutationTarget
	Accounts  map[string]provider.ResolvedAccount
	Bulk      bool
	// SkipsOwnMail is set when a --query search left out Sent and Drafts.
	SkipsOwnMail bool
}

// mutationPlan is what a command does to each message, such as the folder to
// move it to or the categories to set.
type mutationPlan struct {
	Operation      mail.MessageMutationOperation
	Capability     provider.Capability
	TargetFolder   *mail.FolderRef
	CategoryMode   string
	Categories     []string
	CategoryChange *mail.CategoryChange
	NamedClear     bool
}

const (
	categoryModeNone     = ""
	categoryModeSet      = "set"
	categoryModeClearAll = "clear_all"
	categoryModeClear    = "clear"
)

// parseBulkMutationSelection checks how the user picked the messages before
// maildo connects to any account, so a mistyped command never reaches the
// server.
func parseBulkMutationSelection(cmd *cobra.Command, dryRun bool) (parsedMutationSelection, error) {
	flags := cmd.Flags()
	apply, _ := flags.GetBool("apply")
	if dryRun && apply {
		return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--dry-run and --apply cannot be combined")
	}
	all, _ := flags.GetBool("all")
	latest, _ := flags.GetBool("latest")
	first, _ := flags.GetBool("first")
	maxTargets, _ := flags.GetInt("max-targets")
	maxChanged := flags.Changed("max-targets")
	if maxChanged && !(flags.Changed("query") && all) {
		return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--max-targets works only with --query and --all")
	}
	if maxTargets < 1 || maxTargets > maxExplicitMutationTargets {
		return parsedMutationSelection{}, errs.NewWithDetails(errs.CodeInvalidSelector, "--max-targets must be between 1 and 1000", map[string]any{
			"max_targets": maxTargets,
			"maximum":     maxExplicitMutationTargets,
		})
	}

	idsFile, _ := flags.GetString("ids-file")
	hasIDsFile := flags.Changed("ids-file")
	hasIDs := flags.Changed("id")
	hasInternet := flags.Changed("internet-id")
	hasQuery := flags.Changed("query")
	if all && !hasQuery {
		return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--all works only with --query")
	}
	var scope searchScope
	if flags.Lookup("folder") != nil && flags.Changed("folder") {
		if !hasQuery {
			return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--folder works only with --query")
		}
		scope.folder, _ = flags.GetString("folder")
		if strings.TrimSpace(scope.folder) == "" {
			return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--folder requires a folder name")
		}
	}
	families := 0
	for _, present := range []bool{hasIDs, hasIDsFile, hasInternet, hasQuery} {
		if present {
			families++
		}
	}
	if families == 0 {
		return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "say which messages to change: give their ids, or use --ids-file, --internet-id or --query")
	}
	if families > 1 {
		return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "pick the messages in one way only: ids, --ids-file, --internet-id or --query")
	}
	if hasIDsFile {
		if all || latest || first {
			return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--ids-file cannot be used with --all, --latest or --first")
		}
		fileTargets, err := readMutationIDsFile(idsFile, cmd.InOrStdin())
		if err != nil {
			return parsedMutationSelection{}, err
		}
		if err := checkPreviewIDsFile(cmd, idsFile, fileTargets.data); err != nil {
			return parsedMutationSelection{}, err
		}
		out := parsedMutationSelection{IDsFileBase: fileTargets.basename, Bulk: true}
		if len(fileTargets.qualified) > 0 {
			out.Kind = "ids_file_qualified"
			out.Qualified = fileTargets.qualified
		} else {
			out.Kind = "ids_file"
			out.IDs = fileTargets.ids
		}
		return out, nil
	}
	if hasIDs {
		ids, _ := flags.GetStringArray("id")
		if len(ids) == 0 {
			return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--id requires a value")
		}
		out := make([]string, 0, len(ids))
		for _, raw := range ids {
			id := strings.TrimSpace(raw)
			if id == "" {
				return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--id requires a value")
			}
			out = append(out, id)
		}
		if len(out) > maxExplicitMutationTargets {
			return parsedMutationSelection{}, errs.NewWithDetails(errs.CodeInvalidSelector, fmt.Sprintf("too many message ids; one command takes at most %d", maxExplicitMutationTargets), map[string]any{
				"target_count": len(out),
				"maximum":      maxExplicitMutationTargets,
			})
		}
		if len(out) == 1 {
			selector := mail.MessageSelector{Kind: mail.SelectorID, ID: out[0]}
			return parsedMutationSelection{Kind: string(mail.SelectorID), Selector: selector}, nil
		}
		return parsedMutationSelection{Kind: "ids", IDs: out, Bulk: true}, nil
	}
	if hasQuery {
		queryRaw, _ := flags.GetString("query")
		parsed, err := search.ParseQuery(queryRaw)
		if err != nil {
			return parsedMutationSelection{}, err
		}
		if all {
			if latest || first {
				return parsedMutationSelection{}, errs.New(errs.CodeInvalidSelector, "--all cannot be combined with --latest or --first")
			}
			return parsedMutationSelection{Kind: "query_all", QueryRaw: strings.TrimSpace(queryRaw), Selector: mail.MessageSelector{Kind: mail.SelectorQuery, Query: &parsed}, MaxTargets: maxTargets, Bulk: true, Scope: scope}, nil
		}
		selector, err := mail.ParseMessageSelector(mail.SelectorFlags{
			Query: &parsed, HasQuery: true,
			Latest: latest, First: first,
		})
		if err != nil {
			return parsedMutationSelection{}, err
		}
		return parsedMutationSelection{Kind: mutationSelectorKind(selector), Selector: selector, QueryRaw: strings.TrimSpace(queryRaw), Scope: scope}, nil
	}
	internetID, _ := flags.GetString("internet-id")
	selector, err := mail.ParseMessageSelector(mail.SelectorFlags{
		InternetID: strings.TrimSpace(internetID), HasInternetID: hasInternet,
		Latest: latest, First: first,
	})
	if err != nil {
		return parsedMutationSelection{}, err
	}
	return parsedMutationSelection{Kind: mutationSelectorKind(selector), Selector: selector}, nil
}

// parsedIDsFile holds the ids from an ids file and the file's base name. The
// full path stays out of the output and the action log.
type parsedIDsFile struct {
	basename  string
	ids       []string
	qualified []provider.AccountQualifiedTarget
	data      []byte
}

// readMutationIDsFile reads one id per line, or "account<TAB>id" lines for
// messages in several accounts. "-" reads stdin, so you can pipe in the
// output of search --jq.
func readMutationIDsFile(path string, stdin io.Reader) (parsedIDsFile, error) {
	base := filepath.Base(path)
	if strings.TrimSpace(path) == "" {
		return parsedIDsFile{}, errs.New(errs.CodeInvalidSelector, "--ids-file requires a value")
	}
	var src io.Reader
	if path == "-" {
		base, src = "stdin", stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "maildo could not read the ids file", map[string]any{"ids_file": base})
		}
		defer file.Close()
		src = file
	}
	data, err := io.ReadAll(io.LimitReader(src, maxIDsFileBytes+1))
	if err != nil {
		return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "maildo could not read the ids file", map[string]any{"ids_file": base})
	}
	if len(data) > maxIDsFileBytes {
		return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "the ids file is too large", map[string]any{
			"ids_file": base,
			"maximum":  maxIDsFileBytes,
		})
	}
	if !utf8.Valid(data) {
		return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "the ids file must be UTF-8 text", map[string]any{"ids_file": base})
	}
	lines := strings.Split(string(data), "\n")
	var ids []string
	var qualified []provider.AccountQualifiedTarget
	mode := ""
	for lineNo, line := range lines {
		if len(line) > maxIDsFileLineBytes {
			return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "a line in the ids file is too long", map[string]any{
				"ids_file": base,
				"line":     lineNo + 1,
				"maximum":  maxIDsFileLineBytes,
			})
		}
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "\t") {
			if mode == "bare" {
				return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "some lines in the ids file name an account and some do not; use one form for all lines", map[string]any{"ids_file": base})
			}
			cols := strings.Split(line, "\t")
			if len(cols) != 2 {
				return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "a line in the ids file has too many columns; use the account, a tab and the id", map[string]any{"ids_file": base, "line": lineNo + 1})
			}
			account := unquoteID(cols[0])
			id := unquoteID(cols[1])
			if account == "" {
				return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "a line in the ids file has no account before the tab", map[string]any{"ids_file": base, "line": lineNo + 1})
			}
			if id == "" {
				return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "a line in the ids file has no id after the tab", map[string]any{"ids_file": base, "line": lineNo + 1})
			}
			mode = "qualified"
			qualified = append(qualified, provider.AccountQualifiedTarget{Account: account, ID: id})
			continue
		}
		if mode == "qualified" {
			return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "some lines in the ids file name an account and some do not; use one form for all lines", map[string]any{"ids_file": base})
		}
		id := unquoteID(line)
		if id == "" {
			continue
		}
		mode = "bare"
		ids = append(ids, id)
	}
	count := len(ids) + len(qualified)
	if count == 0 {
		return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, "the ids file has no message ids", map[string]any{"ids_file": base})
	}
	if count > maxExplicitMutationTargets {
		return parsedIDsFile{}, errs.NewWithDetails(errs.CodeInvalidSelector, fmt.Sprintf("too many message ids; one command takes at most %d", maxExplicitMutationTargets), map[string]any{
			"ids_file":     base,
			"target_count": count,
			"maximum":      maxExplicitMutationTargets,
		})
	}
	return parsedIDsFile{basename: base, ids: ids, qualified: qualified, data: data}, nil
}

// previewIDsFileName is the name writePreviewIDs gives an ids file: the
// command, the time and the start of a SHA-256 of the content.
var previewIDsFileName = regexp.MustCompile(`^([a-z-]+)-[0-9]{8}-[0-9]{6}-([0-9a-f]{8})\.txt$`)

// checkPreviewIDsFile refuses an ids file from maildo's previews folder
// that changed after the preview wrote it, or that a preview of another
// command wrote. The printed apply command then changes exactly the
// messages the preview showed. Other ids files are the user's own.
func checkPreviewIDsFile(cmd *cobra.Command, path string, data []byte) error {
	if path == "-" || filepath.Base(filepath.Dir(path)) != previewsDir {
		return nil
	}
	m := previewIDsFileName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return nil
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:])[:8] != m[2] {
		return errs.NewWithDetails(errs.CodeInvalidSelector, "the ids file changed after the preview that wrote it, so nothing was changed", map[string]any{
			"ids_file": filepath.Base(path),
			"hint":     "run the command with --dry-run again and use the command it prints",
		})
	}
	if m[1] != previewIDsFilePrefix(cmd) {
		return errs.NewWithDetails(errs.CodeInvalidSelector, fmt.Sprintf("this ids file is from a preview of maildo %s, so nothing was changed", strings.ReplaceAll(m[1], "-", " ")), map[string]any{
			"ids_file": filepath.Base(path),
			"hint":     "run this command with --dry-run and use the command it prints",
		})
	}
	return nil
}

// previewIDsFilePrefix names a command in ids file names: "mark-read".
func previewIDsFilePrefix(cmd *cobra.Command) string {
	return strings.ReplaceAll(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "), " ", "-")
}

// unquoteID trims a line of an ids file and removes JSON quotes, so the
// output of --jq '.messages[].id' works whether or not it printed raw
// strings.
func unquoteID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if unquoted, err := strconv.Unquote(s); err == nil {
			return strings.TrimSpace(unquoted)
		}
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// resolveMutationTargetSet turns the user's selection into the list of
// messages to change. Ids are used as they are. --query --all searches once,
// here, so the preview and the change work on the same list.
func (h *handlers) resolveMutationTargetSet(ctx context.Context, f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, parsed parsedMutationSelection, resolveInternet bool) (mutationTargetSet, error) {
	switch parsed.Kind {
	case string(mail.SelectorID), string(mail.SelectorInternetID), "query_latest", "query_first":
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return mutationTargetSet{}, err
		}
		acct := sel.Accounts[0]
		target, err := h.resolveMutationTarget(ctx, clientScope, sel, acct, parsed.Selector, parsed.Scope, resolveInternet)
		if err != nil {
			return mutationTargetSet{}, err
		}
		return mutationTargetSet{
			Selection: sel,
			Summary: mail.BulkSelectorSummary{
				Kind:        parsed.Kind,
				Query:       parsed.QueryRaw,
				TargetCount: 1,
			},
			Targets:  []mutationTarget{target},
			Accounts: map[string]provider.ResolvedAccount{accountKey(acct.Name, acct.Provider): acct},
		}, nil
	case "ids", "ids_file":
		ids, err := expandShortIDList(f, cmd, parsed.IDs)
		if err != nil {
			return mutationTargetSet{}, err
		}
		parsed.IDs = ids
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return mutationTargetSet{}, err
		}
		acct := sel.Accounts[0]
		targets := make([]mutationTarget, 0, len(parsed.IDs))
		for _, id := range parsed.IDs {
			targets = append(targets, mutationTarget{
				Selection:    sel,
				Account:      acct,
				Selector:     mail.MessageSelector{Kind: mail.SelectorID, ID: id},
				SelectorKind: parsed.Kind,
				MessageID:    id,
			})
		}
		if err := rejectDuplicateTargets(targets); err != nil {
			return mutationTargetSet{}, err
		}
		return mutationTargetSet{
			Selection: sel,
			Summary: mail.BulkSelectorSummary{
				Kind:        parsed.Kind,
				IDsFile:     parsed.IDsFileBase,
				TargetCount: len(targets),
			},
			Targets:  targets,
			Accounts: map[string]provider.ResolvedAccount{accountKey(acct.Name, acct.Provider): acct},
			Bulk:     true,
		}, nil
	case "ids_file_qualified":
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeBulkQualified, AccountQualifiedTargets: parsed.Qualified})
		if err != nil {
			return mutationTargetSet{}, err
		}
		accounts := accountsByName(sel.Accounts)
		targets := make([]mutationTarget, 0, len(parsed.Qualified))
		for _, q := range parsed.Qualified {
			acct, ok := accounts[q.Account]
			if !ok {
				return mutationTargetSet{}, errs.New(errs.CodeUnknownAccount, fmt.Sprintf("unknown account %q", q.Account))
			}
			targets = append(targets, mutationTarget{
				Selection:    sel,
				Account:      acct,
				Selector:     mail.MessageSelector{Kind: mail.SelectorID, ID: q.ID},
				SelectorKind: parsed.Kind,
				MessageID:    q.ID,
			})
		}
		if err := rejectDuplicateTargets(targets); err != nil {
			return mutationTargetSet{}, err
		}
		return mutationTargetSet{
			Selection: sel,
			Summary: mail.BulkSelectorSummary{
				Kind:        parsed.Kind,
				IDsFile:     parsed.IDsFileBase,
				TargetCount: len(targets),
			},
			Targets:  targets,
			Accounts: accountsByKey(sel.Accounts),
			Bulk:     true,
		}, nil
	case "query_all":
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return mutationTargetSet{}, err
		}
		query := parsed.Selector.Query
		if query == nil {
			return mutationTargetSet{}, errs.New(errs.CodeInvalidSelector, "--query requires a parsed query")
		}
		run := h.mutationSearchRunner(clientScope, parsed.Scope, *query)
		// Ask for one more than --max-targets, so a search with too many matches
		// fails instead of changing only the first ones.
		limit := parsed.MaxTargets + 1
		rows, err := search.ExecuteMessages(ctx, sel, run, search.ExecuteOptions{Limit: limit})
		if err != nil {
			return mutationTargetSet{}, err
		}
		if len(rows) > parsed.MaxTargets {
			return mutationTargetSet{}, errs.NewWithDetails(errs.CodeBulkTargetLimitExceeded,
				fmt.Sprintf("the search matches more than %d messages, which is the --max-targets limit, so nothing was changed", parsed.MaxTargets), map[string]any{
					"selector_kind":    parsed.Kind,
					"account_scope":    sel.Scope,
					"limit":            parsed.MaxTargets,
					"maximum":          maxExplicitMutationTargets,
					"matched_at_least": len(rows),
					"hint":             fmt.Sprintf("narrow the search, for example with older:30d or --folder, or raise --max-targets (at most %d)", maxExplicitMutationTargets),
				})
		}
		acct := sel.Accounts[0]
		targets := make([]mutationTarget, 0, len(rows))
		for _, row := range rows {
			targets = append(targets, mutationTarget{
				Selection:         sel,
				Account:           acct,
				Selector:          parsed.Selector,
				SelectorKind:      parsed.Kind,
				MessageID:         row.ID,
				InternetMessageID: row.InternetMessageID,
				Summary:           &row,
			})
		}
		return mutationTargetSet{
			Selection: sel,
			Summary: mail.BulkSelectorSummary{
				Kind:           parsed.Kind,
				Query:          parsed.QueryRaw,
				TargetCount:    len(targets),
				MaxTargets:     parsed.MaxTargets,
				MatchedAtLeast: len(rows),
			},
			Targets:      targets,
			Accounts:     map[string]provider.ResolvedAccount{accountKey(acct.Name, acct.Provider): acct},
			Bulk:         true,
			SkipsOwnMail: parsed.Scope.folder == "" && !parsed.Scope.restore,
		}, nil
	default:
		return mutationTargetSet{}, errs.New(errs.CodeInvalidSelector, "unsupported mutation selector")
	}
}

func rejectDuplicateTargets(targets []mutationTarget) error {
	seen := map[string]struct{}{}
	for _, target := range targets {
		key := accountKey(target.Account.Name, target.Account.Provider) + "\x00" + target.MessageID
		if _, ok := seen[key]; ok {
			return errs.NewWithDetails(errs.CodeInvalidSelector, "the same message is named twice", map[string]any{
				"account":    target.Account.Name,
				"provider":   target.Account.Provider,
				"message_id": target.MessageID,
			})
		}
		seen[key] = struct{}{}
	}
	return nil
}

func accountsByName(accounts []provider.ResolvedAccount) map[string]provider.ResolvedAccount {
	out := make(map[string]provider.ResolvedAccount, len(accounts))
	for _, acct := range accounts {
		out[acct.Name] = acct
	}
	return out
}

func accountsByKey(accounts []provider.ResolvedAccount) map[string]provider.ResolvedAccount {
	out := make(map[string]provider.ResolvedAccount, len(accounts))
	for _, acct := range accounts {
		out[accountKey(acct.Name, acct.Provider)] = acct
	}
	return out
}

func accountKey(account, providerName string) string {
	return account + "\x00" + providerName
}

// runMessageMutation runs a command that changes messages: parse the
// selection, find the messages, then preview or change them.
func (h *handlers) runMessageMutation(f *rungrad.Factory, cmd *cobra.Command, spec mutationSpec) error {
	parsed, err := parseBulkMutationSelection(cmd, f.DryRun())
	if err != nil {
		return err
	}
	parsed.Scope.restore = spec.operation == mail.MutationRestore
	var folderRef mail.FolderRef
	var folderPtr *mail.FolderRef
	if spec.targetFolder {
		if spec.operation == mail.MutationRestore && !cmd.Flags().Changed("to") {
			// Restore puts mail back in the inbox unless told otherwise.
			folderRef, err = mail.ParseFolderRef(mail.WellKnownInbox)
		} else {
			folderRef, err = parseMutationFolderRef(cmd)
		}
		if err != nil {
			return err
		}
		folderPtr = &folderRef
	}
	return h.executeMutationPlan(f, cmd, nil, parsed, mutationPlan{
		Operation:    spec.operation,
		Capability:   spec.capability,
		TargetFolder: folderPtr,
	})
}

// invokeMutation calls the provider method for the operation. The folder and
// the message id are already looked up.
func invokeMutation(ctx context.Context, mutator provider.MessageMutator, op mail.MessageMutationOperation, messageID string, folder *mail.Folder) (mail.MessageMutationResult, error) {
	switch op {
	case mail.MutationMove:
		return mutator.MoveMessage(ctx, mail.MessageMoveRequest{MessageID: messageID, Folder: *folder})
	case mail.MutationCopy:
		return mutator.CopyMessage(ctx, mail.MessageCopyRequest{MessageID: messageID, Folder: *folder})
	case mail.MutationArchive:
		return mutator.ArchiveMessage(ctx, mail.MessageMutationRequest{MessageID: messageID})
	case mail.MutationDelete:
		return mutator.DeleteMessage(ctx, mail.MessageMutationRequest{MessageID: messageID})
	case mail.MutationRestore:
		return mutator.RestoreMessage(ctx, mail.MessageRestoreRequest{MessageID: messageID, Folder: *folder})
	case mail.MutationJunk:
		return mutator.JunkMessage(ctx, mail.MessageMutationRequest{MessageID: messageID})
	case mail.MutationMarkRead:
		return mutator.SetMessageReadState(ctx, mail.MessageReadStateRequest{MessageID: messageID, IsRead: true})
	case mail.MutationMarkUnread:
		return mutator.SetMessageReadState(ctx, mail.MessageReadStateRequest{MessageID: messageID, IsRead: false})
	case mail.MutationFlagSet:
		return mutator.SetMessageFlag(ctx, mail.MessageFlagRequest{MessageID: messageID, Flagged: true})
	case mail.MutationFlagClear:
		return mutator.SetMessageFlag(ctx, mail.MessageFlagRequest{MessageID: messageID, Flagged: false})
	default:
		return mail.MessageMutationResult{}, errs.New(errs.CodeInvalidSelector, "unsupported mutation operation")
	}
}

// categoriesList only reads, so it shows the categories even with
// --dry-run.
func (h *handlers) categoriesList(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		selector, err := parseMutationSelector(cmd)
		if err != nil {
			return err
		}
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		if err := provider.CheckClientAccess(acct, provider.ClientAccessCategoriesRead); err != nil {
			return err
		}
		var scope searchScope
		if cmd.Flags().Changed("folder") {
			if selector.Kind != mail.SelectorQuery {
				return errs.New(errs.CodeInvalidSelector, "--folder works only with --query")
			}
			scope.folder, _ = cmd.Flags().GetString("folder")
		}
		target, err := h.resolveMutationTarget(cmd.Context(), clientScope, sel, acct, selector, scope, true)
		if err != nil {
			return err
		}
		var list mail.MessageCategoryListResult
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessCategoriesRead, func(manager provider.CategoryManager) error {
			var err error
			list, err = manager.ListMessageCategories(cmd.Context(), mail.MessageCategoryListRequest{MessageID: target.MessageID})
			return err
		}); err != nil {
			return err
		}
		if target.InternetMessageID == "" {
			target.InternetMessageID = list.InternetMessageID
		}
		out := categoryListResultFromProvider(target, list)
		return f.WriteResultWithMeta(out, categoryListMeta(cmd.CommandPath(), out), humanCategoryList(out, targetLabel(target)))
	})
}

// categoriesSet checks the new categories before it looks up any message,
// so a mistake in them never reaches the server.
func (h *handlers) categoriesSet(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	categories, err := requiredCategoryFlags(cmd)
	if err != nil {
		return err
	}
	parsed, err := parseBulkMutationSelection(cmd, f.DryRun())
	if err != nil {
		return err
	}
	change := &mail.CategoryChange{After: append([]string(nil), categories...), Count: len(categories)}
	return h.executeMutationPlan(f, cmd, nil, parsed, mutationPlan{
		Operation:      mail.MutationCategoriesSet,
		Capability:     provider.CapCategoriesMutate,
		CategoryMode:   categoryModeSet,
		Categories:     categories,
		CategoryChange: change,
	})
}

// categoriesClear without --category removes all categories at once. With
// --category it first reads each message's categories, to work out the new
// list and which of the named ones the message did not have.
func (h *handlers) categoriesClear(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	requested, err := optionalCategoryFlags(cmd)
	if err != nil {
		return err
	}
	parsed, err := parseBulkMutationSelection(cmd, f.DryRun())
	if err != nil {
		return err
	}
	named := len(requested) > 0
	if !named {
		change := &mail.CategoryChange{After: []string{}, Count: 0}
		return h.executeMutationPlan(f, cmd, nil, parsed, mutationPlan{
			Operation:      mail.MutationCategoriesClear,
			Capability:     provider.CapCategoriesMutate,
			CategoryMode:   categoryModeClearAll,
			Categories:     []string{},
			CategoryChange: change,
		})
	}
	return h.executeMutationPlan(f, cmd, nil, parsed, mutationPlan{
		Operation:    mail.MutationCategoriesClear,
		Capability:   provider.CapCategoriesMutate,
		CategoryMode: categoryModeClear,
		Categories:   requested,
		NamedClear:   true,
	})
}

// executeMutationPlan decides between a preview and the change. A change to
// one message happens at once; a change to several messages only previews
// unless --apply is given.
func (h *handlers) executeMutationPlan(f *rungrad.Factory, cmd *cobra.Command, args []string, parsed parsedMutationSelection, plan mutationPlan) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		applyFlag, _ := cmd.Flags().GetBool("apply")
		apply := !f.DryRun() && (!parsed.Bulk || applyFlag)
		resolveInternet := apply || plan.NamedClear
		// Say "not supported" before searching, not after.
		if parsed.Kind != "ids_file_qualified" {
			sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
			if err != nil {
				return err
			}
			if err := h.checkMutationPlanCapabilities([]mutationTarget{{Account: sel.Accounts[0]}}, plan); err != nil {
				return err
			}
		}
		targetSet, err := h.resolveMutationTargetSet(cmd.Context(), f, cmd, clientScope, parsed, resolveInternet)
		if err != nil {
			return err
		}
		folders, err := h.resolveMutationFolders(cmd.Context(), clientScope, targetSet.Targets, plan.TargetFolder)
		if err != nil {
			return err
		}
		if targetSet.Bulk {
			if err := checkBulkPolicy(targetSet.Targets); err != nil {
				return err
			}
		}
		if !apply {
			if err := h.checkMutationPlanCapabilities(targetSet.Targets, plan); err != nil {
				return err
			}
			categoryChanges, err := h.previewCategoryChanges(cmd.Context(), clientScope, targetSet.Targets, plan)
			if err != nil {
				return err
			}
			h.describeTargets(cmd.Context(), clientScope, targetSet.Targets, maxDescribedTargets)
			applyCommand, err := applyCommandFor(f, cmd, targetSet)
			if err != nil {
				return err
			}
			preview := buildMutationPreview(targetSet, plan, folders, categoryChanges, applyCommand)
			return f.WriteResult(preview, humanMutationPreview(f, preview, targetSet, plan))
		}
		if undoable(plan.Operation) {
			// Outlook does not say which folder a moved message came from,
			// so maildo looks it up first. The undo command needs it.
			for i := range targetSet.Targets {
				if targetSet.Targets[i].Account.Provider == "outlook" {
					h.describeTargets(cmd.Context(), clientScope, targetSet.Targets[i:i+1], 1)
				}
			}
		}
		if !f.Flags.JSON && f.Flags.JQ == "" && f.Flags.Template == "" {
			// People see the subject and sender of what changed.
			limit := 1
			if targetSet.Bulk {
				limit = maxListedResults
			}
			h.describeTargets(cmd.Context(), clientScope, targetSet.Targets, limit)
		}
		if targetSet.Bulk {
			return h.applyBulkMutation(f, cmd, clientScope, targetSet, plan, folders)
		}
		if len(targetSet.Targets) != 1 {
			return errs.New(errs.CodeInvalidSelector, "single-target mutation resolved an invalid target count")
		}
		return h.applySingleMutation(f, cmd, clientScope, targetSet, targetSet.Targets[0], plan, folders)
	})
}

func (h *handlers) checkMutationPlanCapabilities(targets []mutationTarget, plan mutationPlan) error {
	for _, acct := range distinctTargetAccounts(targets) {
		if plan.NamedClear {
			if err := provider.CheckClientAccess(acct, provider.ClientAccessCategoriesRead); err != nil {
				return err
			}
			if err := provider.CheckClientAccess(acct, provider.ClientAccessCategoriesMutate); err != nil {
				return err
			}
			continue
		}
		switch plan.CategoryMode {
		case categoryModeSet, categoryModeClearAll:
			if err := provider.CheckClientAccess(acct, provider.ClientAccessCategoriesMutate); err != nil {
				return err
			}
		default:
			if err := provider.CheckClientAccess(acct, messageMutationAccess(plan.Operation)); err != nil {
				return err
			}
		}
	}
	return nil
}

func messageMutationAccess(op mail.MessageMutationOperation) provider.ClientAccessKind {
	switch op {
	case mail.MutationMove:
		return provider.ClientAccessMessageMove
	case mail.MutationCopy:
		return provider.ClientAccessMessageCopy
	case mail.MutationArchive:
		return provider.ClientAccessMessageArchive
	case mail.MutationDelete:
		return provider.ClientAccessMessageDelete
	case mail.MutationRestore:
		return provider.ClientAccessMessageRestore
	case mail.MutationJunk:
		return provider.ClientAccessMessageJunk
	case mail.MutationMarkRead, mail.MutationMarkUnread:
		return provider.ClientAccessMessageMarkRead
	case mail.MutationFlagSet, mail.MutationFlagClear:
		return provider.ClientAccessMessageFlag
	default:
		return ""
	}
}

func categorySetRequest(messageID string, plan mutationPlan) (mail.MessageCategorySetRequest, error) {
	mode := mail.CategoryMutationSet
	var requested []string
	if plan.CategoryMode == categoryModeClearAll {
		mode = mail.CategoryMutationClearAll
		requested = []string{}
	}
	return mail.NewMessageCategorySetRequest(messageID, mode, plan.Categories, requested)
}

func distinctTargetAccounts(targets []mutationTarget) []provider.ResolvedAccount {
	seen := map[string]struct{}{}
	out := make([]provider.ResolvedAccount, 0)
	for _, target := range targets {
		key := accountKey(target.Account.Name, target.Account.Provider)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, target.Account)
	}
	return out
}

func (h *handlers) resolveMutationFolders(ctx context.Context, clientScope provider.AccountClientScope, targets []mutationTarget, ref *mail.FolderRef) (map[string]mail.Folder, error) {
	if ref == nil {
		return nil, nil
	}
	out := map[string]mail.Folder{}
	for _, acct := range distinctTargetAccounts(targets) {
		folder, err := h.resolveMutationFolder(ctx, clientScope, acct, *ref)
		if err != nil {
			return nil, err
		}
		out[accountKey(acct.Name, acct.Provider)] = folder
	}
	return out, nil
}

func (h *handlers) previewCategoryChanges(ctx context.Context, clientScope provider.AccountClientScope, targets []mutationTarget, plan mutationPlan) (map[int]*mail.CategoryChange, error) {
	if !plan.NamedClear {
		return nil, nil
	}
	out := make(map[int]*mail.CategoryChange, len(targets))
	for i := range targets {
		change, internetID, err := h.categoryClearChange(ctx, clientScope, targets[i], plan.Categories)
		if err != nil {
			return nil, err
		}
		if targets[i].InternetMessageID == "" {
			targets[i].InternetMessageID = internetID
		}
		out[i] = change
	}
	return out, nil
}

func (h *handlers) applySingleMutation(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, set mutationTargetSet, target mutationTarget, plan mutationPlan, folders map[string]mail.Folder) error {
	var folder *mail.Folder
	if plan.TargetFolder != nil {
		resolved := folders[accountKey(target.Account.Name, target.Account.Provider)]
		folder = &resolved
	}
	result, categoryChange, attempted, err := h.invokePlannedMutation(cmd.Context(), clientScope, target, plan, folder)
	if err != nil && !attempted {
		return err
	}
	row := appliedTargetFromResult(target, plan.Operation, result, err)
	if folder != nil && row.TargetFolder == nil {
		summary := mail.SummaryFrom(*folder)
		row.TargetFolder = &summary
	}
	if categoryChange != nil {
		result.Categories = mergeCategoryChange(result.Categories, categoryChange)
		row.Categories = result.Categories
	}
	counts := countAppliedTargets([]mail.AppliedMutationTarget{row})
	status, logErr := h.writeMutationActionLog(f, cmd, set.Summary, counts, []mail.AppliedMutationTarget{row}, plan)
	if logErr != nil {
		return actionLogWriteFailedError{
			operation:         string(plan.Operation),
			selector:          set.Summary,
			counts:            counts,
			targets:           []mail.AppliedMutationTarget{row},
			status:            status,
			mutationAttempted: attempted,
			mutationApplied:   counts.Succeeded > 0,
		}
	}
	if err != nil {
		return err
	}
	out := mutationResultFromProvider(target, plan.Operation, true, result)
	out.ActionLogStatus = status
	if folder != nil && out.TargetFolder == nil {
		summary := mail.SummaryFrom(*folder)
		out.TargetFolder = &summary
	}
	out.ResultMessageID, out.SourceFolder = row.ResultMessageID, row.SourceFolder
	out.UndoCommands = undoCommands(f, cmd, plan.Operation, []mail.AppliedMutationTarget{row})
	return f.WriteResultWithMeta(out, mutationMeta(cmd.CommandPath(), out), humanMutationResult(f, out, target, plan))
}

// applyBulkMutation changes several messages and prints the outcome. The
// whole command gets one line in the action log.
func (h *handlers) applyBulkMutation(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, set mutationTargetSet, plan mutationPlan, folders map[string]mail.Folder) error {
	outcome := h.applyMutationTargets(cmd.Context(), clientScope, set.Targets, plan, folders, bulkProgress(f))
	status := mail.ActionLogStatus{Written: false, EntryCount: 0, RelativePath: audit.RelativePath}
	if len(set.Targets) == 0 {
		status.Reason = "no_targets"
		result := bulkMutationResult{
			AccountSelection: set.Selection,
			Operation:        string(plan.Operation),
			Applied:          true,
			Selector:         set.Summary,
			Counts:           outcome.Counts,
			Targets:          outcome.Rows,
			ActionLogStatus:  status,
		}
		return f.WriteResultWithMeta(result, bulkMutationMeta(cmd.CommandPath(), result), humanBulkResult(f, result, set, plan))
	}
	if outcome.MutationAttempted {
		var logErr error
		status, logErr = h.writeMutationActionLog(f, cmd, set.Summary, outcome.Counts, outcome.Rows, plan)
		if logErr != nil {
			return actionLogWriteFailedError{
				operation:         string(plan.Operation),
				selector:          set.Summary,
				counts:            outcome.Counts,
				targets:           outcome.Rows,
				status:            status,
				mutationAttempted: true,
				mutationApplied:   outcome.MutationApplied,
			}
		}
	} else if outcome.Counts.Failed > 0 || outcome.Counts.Skipped > 0 {
		status.Reason = "not_attempted"
	}
	result := bulkMutationResult{
		AccountSelection: set.Selection,
		Operation:        string(plan.Operation),
		Applied:          true,
		Selector:         set.Summary,
		Counts:           outcome.Counts,
		Targets:          outcome.Rows,
		ActionLogStatus:  status,
		UndoCommands:     undoCommands(f, cmd, plan.Operation, outcome.Rows),
	}
	if outcome.Counts.Failed > 0 || outcome.Counts.Skipped > 0 {
		return bulkPartialFailureError{
			undo:              result.UndoCommands,
			operation:         string(plan.Operation),
			selector:          set.Summary,
			counts:            outcome.Counts,
			targets:           outcome.Rows,
			status:            status,
			mutationAttempted: outcome.MutationAttempted,
			mutationApplied:   outcome.MutationApplied,
			exit:              outcome.ExitCode,
			labels:            targetLabels(set.Targets),
		}
	}
	return f.WriteResultWithMeta(result, bulkMutationMeta(cmd.CommandPath(), result), humanBulkResult(f, result, set, plan))
}

// bulkProgressEvery is how often a change to many messages says how far
// it got.
var bulkProgressEvery = 2 * time.Second

// bulkProgress prints how far a change to many messages has got, so a
// long change does not look stuck. It prints nothing when the output goes
// to a file or a program, or with --quiet.
func bulkProgress(f *rungrad.Factory) func(done, total int) {
	if !f.TerminalMode().ANSI {
		return nil
	}
	last := time.Now()
	return func(done, total int) {
		if done == 0 || time.Since(last) < bulkProgressEvery {
			return
		}
		last = time.Now()
		f.Infof("%s of %s messages done...", thousands(done), thousands(total))
	}
}

// mutationApplyOutcome is what happened to each message, and the exit code
// for the whole command.
type mutationApplyOutcome struct {
	Rows              []mail.AppliedMutationTarget
	Counts            mail.BulkMutationCounts
	MutationAttempted bool
	MutationApplied   bool
	ExitCode          int
}

// stoppedAccount remembers an error that stopped an account, so maildo does
// not try the account's other messages and fail the same way again.
type stoppedAccount struct {
	reason string
	code   string
	detail map[string]any
	exit   int
}

// applyMutationTargets changes the messages in order. A message that is not
// found fails alone; any other error stops the work in that account.
func (h *handlers) applyMutationTargets(ctx context.Context, clientScope provider.AccountClientScope, targets []mutationTarget, plan mutationPlan, folders map[string]mail.Folder, progress func(done, total int)) mutationApplyOutcome {
	rows := make([]mail.AppliedMutationTarget, 0, len(targets))
	stopped := map[string]stoppedAccount{}
	out := mutationApplyOutcome{}
	for i, target := range targets {
		if progress != nil {
			progress(i, len(targets))
		}
		key := accountKey(target.Account.Name, target.Account.Provider)
		if stop, ok := stopped[key]; ok {
			// Keep the message in the list, but don't ask the server again after an
			// error stopped this account.
			rows = append(rows, skippedTarget(target, plan.Operation, stop.reason, stop.code, stop.detail))
			out.ExitCode = preferredBulkExit(out.ExitCode, stop.exit, stop.code)
			continue
		}
		var folder *mail.Folder
		if plan.TargetFolder != nil {
			resolved := folders[key]
			folder = &resolved
		}
		result, categoryChange, attempted, err := h.invokeBulkTarget(ctx, clientScope, target, plan, folder)
		if attempted {
			out.MutationAttempted = true
		}
		if err != nil {
			code := productErrorCode(err)
			details, _ := mutationErrorDetailsAndFacts(err)
			exit := errorExitCode(err)
			out.ExitCode = preferredBulkExit(out.ExitCode, exit, code)
			if code == errs.CodeMessageNotFound || code == errs.CodeInvalidSelector {
				// Only a message that is not found fails alone. Other not-found
				// errors, such as a missing folder, stop the account.
				rows = append(rows, failedTarget(target, plan.Operation, err, details))
				continue
			}
			if attempted {
				rows = append(rows, failedTarget(target, plan.Operation, err, details))
			} else {
				rows = append(rows, skippedTarget(target, plan.Operation, err.Error(), code, details))
			}
			stopped[key] = stoppedAccount{reason: err.Error(), code: code, detail: details, exit: exit}
			continue
		}
		if categoryChange != nil {
			result.Categories = mergeCategoryChange(result.Categories, categoryChange)
		}
		row := appliedTargetFromResult(target, plan.Operation, result, nil)
		if folder != nil && row.TargetFolder == nil {
			summary := mail.SummaryFrom(*folder)
			row.TargetFolder = &summary
		}
		rows = append(rows, row)
		out.MutationApplied = true
	}
	out.Rows = rows
	out.Counts = countAppliedTargets(rows)
	if out.ExitCode == 0 {
		out.ExitCode = 1
	}
	return out
}

// invokeBulkTarget returns attempted=false when it failed before it asked the
// server for the change. The caller then reports the message as not tried
// instead of failed.
func (h *handlers) invokeBulkTarget(ctx context.Context, clientScope provider.AccountClientScope, target mutationTarget, plan mutationPlan, folder *mail.Folder) (mail.MessageMutationResult, *mail.CategoryChange, bool, error) {
	if plan.NamedClear {
		change, internetID, err := h.categoryClearChange(ctx, clientScope, target, plan.Categories)
		if err != nil {
			return mail.MessageMutationResult{}, nil, false, err
		}
		if target.InternetMessageID == "" {
			target.InternetMessageID = internetID
		}
		var result mail.MessageMutationResult
		called := false
		err = provider.WithAccountClient(ctx, clientScope, target.Account, provider.ClientAccessCategoriesMutate, func(manager provider.CategoryManager) error {
			var err error
			req, err := mail.NewMessageCategorySetRequest(target.MessageID, mail.CategoryMutationClearNamed, change.After, plan.Categories)
			if err != nil {
				return err
			}
			called = true
			result, err = manager.SetMessageCategories(ctx, req)
			return err
		})
		return result, change, called, err
	}
	switch plan.CategoryMode {
	case categoryModeSet, categoryModeClearAll:
		var result mail.MessageMutationResult
		called := false
		err := provider.WithAccountClient(ctx, clientScope, target.Account, provider.ClientAccessCategoriesMutate, func(manager provider.CategoryManager) error {
			var err error
			req, err := categorySetRequest(target.MessageID, plan)
			if err != nil {
				return err
			}
			called = true
			result, err = manager.SetMessageCategories(ctx, req)
			return err
		})
		return result, plan.CategoryChange, called, err
	default:
		var result mail.MessageMutationResult
		called := false
		err := provider.WithAccountClient(ctx, clientScope, target.Account, messageMutationAccess(plan.Operation), func(mutator provider.MessageMutator) error {
			var err error
			called = true
			result, err = invokeMutation(ctx, mutator, plan.Operation, target.MessageID, folder)
			return err
		})
		return result, nil, called, err
	}
}

func (h *handlers) invokePlannedMutation(ctx context.Context, clientScope provider.AccountClientScope, target mutationTarget, plan mutationPlan, folder *mail.Folder) (mail.MessageMutationResult, *mail.CategoryChange, bool, error) {
	if plan.NamedClear {
		change, _, err := h.categoryClearChange(ctx, clientScope, target, plan.Categories)
		if err != nil {
			return mail.MessageMutationResult{}, nil, false, err
		}
		var result mail.MessageMutationResult
		called := false
		err = provider.WithAccountClient(ctx, clientScope, target.Account, provider.ClientAccessCategoriesMutate, func(manager provider.CategoryManager) error {
			var err error
			req, err := mail.NewMessageCategorySetRequest(target.MessageID, mail.CategoryMutationClearNamed, change.After, plan.Categories)
			if err != nil {
				return err
			}
			called = true
			result, err = manager.SetMessageCategories(ctx, req)
			return err
		})
		return result, change, called, err
	}
	switch plan.CategoryMode {
	case categoryModeSet, categoryModeClearAll:
		var result mail.MessageMutationResult
		called := false
		err := provider.WithAccountClient(ctx, clientScope, target.Account, provider.ClientAccessCategoriesMutate, func(manager provider.CategoryManager) error {
			var err error
			req, err := categorySetRequest(target.MessageID, plan)
			if err != nil {
				return err
			}
			called = true
			result, err = manager.SetMessageCategories(ctx, req)
			return err
		})
		return result, plan.CategoryChange, called, err
	default:
		var result mail.MessageMutationResult
		called := false
		err := provider.WithAccountClient(ctx, clientScope, target.Account, messageMutationAccess(plan.Operation), func(mutator provider.MessageMutator) error {
			var err error
			called = true
			result, err = invokeMutation(ctx, mutator, plan.Operation, target.MessageID, folder)
			return err
		})
		return result, nil, called, err
	}
}

func (h *handlers) categoryClearChange(ctx context.Context, clientScope provider.AccountClientScope, target mutationTarget, requested []string) (*mail.CategoryChange, string, error) {
	var current mail.MessageCategoryListResult
	err := provider.WithAccountClient(ctx, clientScope, target.Account, provider.ClientAccessCategoriesRead, func(manager provider.CategoryManager) error {
		var err error
		current, err = manager.ListMessageCategories(ctx, mail.MessageCategoryListRequest{MessageID: target.MessageID})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return categoryClearChangeFromCurrent(current.Categories, requested), current.InternetMessageID, nil
}

func categoryClearChangeFromCurrent(current, requested []string) *mail.CategoryChange {
	remaining, removed, skipped := diffCategories(current, requested)
	return &mail.CategoryChange{
		Before:  append([]string(nil), current...),
		After:   append([]string(nil), remaining...),
		Removed: removed,
		Skipped: skipped,
		Count:   len(remaining),
	}
}

func appliedTargetFromResult(target mutationTarget, op mail.MessageMutationOperation, result mail.MessageMutationResult, err error) mail.AppliedMutationTarget {
	row := plannedTargetBase(target, op)
	if err != nil {
		row.Status = "failed"
		row.Reason = err.Error()
		row.ErrorCode = productErrorCode(err)
		row.ErrorDetails, row.WriteOutcomeFacts = mutationErrorDetailsAndFacts(err)
		return row
	}
	messageID := result.MessageID
	if messageID == "" {
		messageID = target.MessageID
	}
	internetID := result.InternetMessageID
	if internetID == "" {
		internetID = target.InternetMessageID
	}
	row.ID = messageID
	row.InternetMessageID = internetID
	row.Status = "succeeded"
	row.ResultMessageID = result.ResultMessageID
	if row.ResultMessageID == "" {
		// The id stayed the same, as for a flag, or for a move on Gmail.
		// new_id is still set, so a script finds the current id in one
		// place.
		row.ResultMessageID = messageID
	}
	row.SourceFolder = result.SourceFolder
	if row.SourceFolder == nil && target.Summary != nil && target.Summary.Folder.ID != "" {
		folder := target.Summary.Folder
		row.SourceFolder = &folder
	}
	row.TargetFolder = result.TargetFolder
	row.ReadState = result.ReadState
	row.FlagState = result.FlagState
	row.Categories = result.Categories
	row.Changed = cloneBoolPtr(result.Changed)
	row.WriteOutcomeFacts = mail.CloneWriteOutcomeFacts(result.WriteOutcomeFacts)
	row.ProviderMeta = cloneProviderMetadataForResult(result.ProviderMeta)
	return row
}

func plannedTargetBase(target mutationTarget, op mail.MessageMutationOperation) mail.AppliedMutationTarget {
	return mail.AppliedMutationTarget{
		Account:           target.Account.Name,
		Provider:          target.Account.Provider,
		Operation:         op,
		ID:                target.MessageID,
		InternetMessageID: target.InternetMessageID,
	}
}

func failedTarget(target mutationTarget, op mail.MessageMutationOperation, err error, details map[string]any) mail.AppliedMutationTarget {
	row := plannedTargetBase(target, op)
	row.Status = "failed"
	row.Reason = err.Error()
	row.ErrorCode = productErrorCode(err)
	cleanDetails, facts := mutationErrorDetailsAndFacts(err)
	if cleanDetails == nil {
		cleanDetails = stripWriteOutcomeDetailKeys(details)
	}
	row.ErrorDetails = cleanDetails
	row.WriteOutcomeFacts = facts
	return row
}

func skippedTarget(target mutationTarget, op mail.MessageMutationOperation, reason, code string, details map[string]any) mail.AppliedMutationTarget {
	row := plannedTargetBase(target, op)
	row.Status = "skipped"
	// Nothing changed, so the message keeps its id.
	row.ResultMessageID = row.ID
	row.Reason = reason
	row.ErrorCode = code
	row.ErrorDetails = details
	return row
}

func countAppliedTargets(rows []mail.AppliedMutationTarget) mail.BulkMutationCounts {
	counts := mail.BulkMutationCounts{Total: len(rows)}
	for _, row := range rows {
		switch row.Status {
		case "succeeded":
			counts.Succeeded++
		case "failed":
			counts.Failed++
		case "skipped":
			counts.Skipped++
		}
	}
	return counts
}

func productErrorCode(err error) string {
	details := safeErrorDetails(err)
	if code, ok := details["code"].(string); ok {
		return code
	}
	return ""
}

func mutationErrorDetailsAndFacts(err error) (map[string]any, *mail.WriteOutcomeFacts) {
	details := safeErrorDetails(err)
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok {
		return details, nil
	}
	return stripWriteOutcomeDetailKeys(details), mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts))
}

func stripWriteOutcomeDetailKeys(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		switch k {
		case "outcome", "write_may_have_landed", "retry_safe", "accepted_by_provider", "confirmed_after_write", "succeeded":
			continue
		default:
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func errorExitCode(err error) int {
	if e, ok := err.(interface{ ExitCode() int }); ok {
		return e.ExitCode()
	}
	return 1
}

// preferredBulkExit picks one exit code when messages fail for different
// reasons. A login problem wins, then rate limits, then refusals and server
// errors, then not found, then everything else.
func preferredBulkExit(current, next int, code string) int {
	if next == 0 {
		if code == errs.CodeMessageNotFound {
			next = rungrad.ExitNotFound
		} else {
			next = rungrad.ExitUsage
		}
	}
	if current == 0 {
		return next
	}
	if current == rungrad.ExitAuth || next == rungrad.ExitAuth {
		return rungrad.ExitAuth
	}
	if current == rungrad.ExitRateLimited || next == rungrad.ExitRateLimited {
		return rungrad.ExitRateLimited
	}
	if current == rungrad.ExitAPI || next == rungrad.ExitAPI || current == rungrad.ExitForbidden || next == rungrad.ExitForbidden {
		if current == rungrad.ExitForbidden || next == rungrad.ExitForbidden {
			return rungrad.ExitForbidden
		}
		return rungrad.ExitAPI
	}
	if current == rungrad.ExitNotFound || next == rungrad.ExitNotFound {
		return rungrad.ExitNotFound
	}
	return rungrad.ExitUsage
}

// writeMutationActionLog adds the command to the action log after maildo
// asked the server for the changes. The status it returns names the log by
// its relative path, so errors never show where the config folder is.
func (h *handlers) writeMutationActionLog(f *rungrad.Factory, cmd *cobra.Command, selector mail.BulkSelectorSummary, counts mail.BulkMutationCounts, rows []mail.AppliedMutationTarget, plan mutationPlan) (mail.ActionLogStatus, error) {
	status := mail.ActionLogStatus{Written: false, EntryCount: 0, RelativePath: audit.RelativePath}
	entry := audit.Entry{
		Version:   1,
		CreatedAt: h.deps.Now().UTC(),
		Command:   actionLogCommandPath(cmd),
		Operation: plan.Operation,
		Selector:  selector,
		Counts:    counts,
		Targets:   rows,
	}
	if plan.CategoryChange != nil {
		entry.Categories = plan.CategoryChange
	}
	if err := audit.Append(filepath.Dir(f.ConfigPath()), entry); err != nil {
		status.Reason = "append_failed"
		status.ErrorCode = errs.CodeActionLogWriteFailed
		return status, err
	}
	status.Written = true
	status.EntryCount = 1
	return status, nil
}

// actionLogCommandPath is the command without "maildo", such as "mark read"
// or "categories clear".
func actionLogCommandPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if root := cmd.Root(); root != nil {
		rootPath := root.CommandPath()
		if strings.HasPrefix(path, rootPath+" ") {
			return strings.TrimPrefix(path, rootPath+" ")
		}
	}
	return path
}

// resolveMutationTarget finds the message id for one picked message. With
// resolveInternet false, a --dry-run of --internet-id does not read the
// message, unless the command needs its current state.
func (h *handlers) resolveMutationTarget(ctx context.Context, clientScope provider.AccountClientScope, sel provider.AccountSelection, acct provider.ResolvedAccount, selector mail.MessageSelector, scope searchScope, resolveInternet bool) (mutationTarget, error) {
	target := mutationTarget{Selection: sel, Account: acct, Selector: selector, SelectorKind: mutationSelectorKind(selector)}
	switch selector.Kind {
	case mail.SelectorID:
		target.MessageID = selector.ID
	case mail.SelectorInternetID:
		target.InternetMessageID = selector.InternetID
		if resolveInternet {
			msg, err := h.resolveInternetMutationTarget(ctx, clientScope, acct, selector.InternetID)
			if err != nil {
				return mutationTarget{}, err
			}
			target.MessageID = msg.ID
			target.InternetMessageID = msg.InternetMessageID
		}
	case mail.SelectorQuery:
		selected, err := search.ResolveSelected(ctx, sel, h.mutationSearchRunner(clientScope, scope, *selector.Query))
		if err != nil {
			return mutationTarget{}, err
		}
		target.MessageID = selected.Message.ID
		target.InternetMessageID = selected.Message.InternetMessageID
		target.Searched = &selected.Searched
		summary := selected.Message
		target.Summary = &summary
	default:
		return mutationTarget{}, errs.New(errs.CodeInvalidSelector, "say which message to change: give its id, or use --internet-id or --query")
	}
	return target, nil
}

// mutationSearchRunner runs the search behind a --query selector of a
// command that changes mail. Like every search it leaves out Trash and
// Junk; it also leaves out Sent and Drafts, so a broad query cannot reach
// your own mail. --folder names one folder instead, and restore looks only
// in Trash and Junk. Complete makes a provider that cannot return every
// match fail, so maildo never picks from a partial list.
func (h *handlers) mutationSearchRunner(clientScope provider.AccountClientScope, scope searchScope, query mail.Query) search.AccountSearchFunc {
	return func(ctx context.Context, acct provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error) {
		req := mail.SearchRequest{Query: query, Limit: limit, Complete: true}
		switch {
		case scope.folder != "":
			ref, err := mail.ParseFolderRef(scope.folder)
			if err != nil {
				return mail.MessageSummaryList{}, err
			}
			folder, err := h.resolveFolder(ctx, clientScope, acct, ref)
			if err != nil {
				return mail.MessageSummaryList{}, err
			}
			req.Folder = &folder
		case scope.restore:
			req.OnlyRoles = []string{mail.WellKnownDeleted, mail.WellKnownJunk}
		default:
			req.SkipRoles = []string{mail.WellKnownSent, mail.WellKnownDrafts}
		}
		var out mail.MessageSummaryList
		err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesSearch, func(searcher provider.MessageSearcher) error {
			var err error
			out, err = searcher.SearchMessages(ctx, req)
			return err
		})
		return out, err
	}
}

func (h *handlers) resolveInternetMutationTarget(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, internetID string) (mail.Message, error) {
	var msg mail.Message
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesRead, func(reader provider.MessageReader) error {
		var err error
		msg, err = reader.ReadMessage(ctx, mail.ReadMessageRequest{
			Selector:     mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorInternetID, InternetID: internetID},
			MetadataOnly: true,
		})
		return err
	})
	return msg, err
}

// parseMutationFolderRef checks --to before maildo connects to any account.
func parseMutationFolderRef(cmd *cobra.Command) (mail.FolderRef, error) {
	if !cmd.Flags().Changed("to") {
		return mail.FolderRef{}, rungrad.NewError(rungrad.ExitUsage, "name the folder with --to, for example --to Archive")
	}
	raw, _ := cmd.Flags().GetString("to")
	if strings.TrimSpace(raw) == "" {
		return mail.FolderRef{}, rungrad.NewError(rungrad.ExitUsage, "name the folder with --to, for example --to Archive")
	}
	return mail.ParseFolderRef(raw)
}

func (h *handlers) resolveMutationFolder(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, ref mail.FolderRef) (mail.Folder, error) {
	return h.resolveFolder(ctx, clientScope, acct, ref)
}

func mutationSelectorKind(selector mail.MessageSelector) string {
	if selector.Kind == mail.SelectorQuery {
		if selector.Pick == mail.PickFirst {
			return "query_first"
		}
		return "query_latest"
	}
	return string(selector.Kind)
}

func requiredCategoryFlags(cmd *cobra.Command) ([]string, error) {
	values, err := optionalCategoryFlags(cmd)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errs.NewWithDetails(errs.CodeInvalidSelector, "name at least one category with --category", map[string]any{"field": "category"})
	}
	return values, nil
}

func optionalCategoryFlags(cmd *cobra.Command) ([]string, error) {
	values, _ := cmd.Flags().GetStringArray("category")
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, errs.NewWithDetails(errs.CodeInvalidSelector, "--category requires a value", map[string]any{"field": "category"})
		}
		if _, ok := seen[value]; ok {
			return nil, errs.NewWithDetails(errs.CodeInvalidSelector, "the same category is given twice", map[string]any{"field": "category", "category": value})
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

// diffCategories removes the named categories (exact, case-sensitive
// matches) and keeps the others in their order. skipped are the named ones
// the message did not have.
func diffCategories(current, requested []string) (remaining, removed, skipped []string) {
	requestedSet := map[string]struct{}{}
	for _, name := range requested {
		requestedSet[name] = struct{}{}
	}
	removedSet := map[string]struct{}{}
	for _, name := range current {
		if _, ok := requestedSet[name]; ok {
			removed = append(removed, name)
			removedSet[name] = struct{}{}
			continue
		}
		remaining = append(remaining, name)
	}
	for _, name := range requested {
		if _, ok := removedSet[name]; !ok {
			skipped = append(skipped, name)
		}
	}
	return remaining, removed, skipped
}

// mergeCategoryChange fills in what maildo worked out itself (the old list,
// removed and missing names) when the provider returns only the new list.
func mergeCategoryChange(got, fallback *mail.CategoryChange) *mail.CategoryChange {
	if got == nil {
		return fallback
	}
	if got.After == nil && fallback != nil {
		got.After = append([]string(nil), fallback.After...)
	}
	if got.Count == 0 && got.After != nil {
		got.Count = len(got.After)
	}
	if fallback != nil {
		if got.Before == nil {
			got.Before = append([]string(nil), fallback.Before...)
		}
		if got.Removed == nil {
			got.Removed = append([]string(nil), fallback.Removed...)
		}
		if got.Skipped == nil {
			got.Skipped = append([]string(nil), fallback.Skipped...)
		}
	}
	return got
}

// checkBulkPolicy applies each account's policy.max_bulk to its share of
// the targets.
func checkBulkPolicy(targets []mutationTarget) error {
	counts := map[string]int{}
	accounts := map[string]provider.ResolvedAccount{}
	for _, t := range targets {
		key := accountKey(t.Account.Name, t.Account.Provider)
		counts[key]++
		accounts[key] = t.Account
	}
	for key, n := range counts {
		if err := provider.CheckBulkSize(accounts[key], n); err != nil {
			return err
		}
	}
	return nil
}
