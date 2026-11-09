package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/search"
	"github.com/vincentsch/rungrad"
)

type messagesModule struct {
	h    *handlers
	defs []cmdDef
}

// newMessagesModule declares the commands that only read mail. search looks
// in the whole mailbox, while list always shows one folder.
func newMessagesModule(h *handlers) messagesModule {
	modes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	threadModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	defs := []cmdDef{
		{
			path: "search", use: "search <query>", short: "Search your mail", groupID: "messages",
			examples: []string{
				`maildo search "from:client@example.com contract"`,
				`maildo search invoice --account work --account personal --json`,
				`maildo search "has:attachment after:2026-01-01" --all-accounts --limit 50`,
				`maildo search --folder Archive "from:vendor@example.com subject:invoice"`,
				`maildo search report --plain`,
			},
			related:      []string{"list", "folders list"},
			outputModes:  modes,
			supportsMeta: true,
			args:         cobra.MinimumNArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
				cmd.Flags().String("folder", "", "Search only this folder; without it, every folder except Trash and Junk")
				cmd.Flags().Int("limit", 25, "Messages per page (max 1000)")
				cmd.Flags().Int("page", 1, "Page of results to show, starting at 1")
			},
			run: h.searchMessages,
		},
		{
			path: "list", use: "list [folder]", short: "List the newest messages in a folder", groupID: "messages",
			examples: []string{
				"maildo list",
				"maildo list inbox --unread",
				"maildo list --account work --json",
				"maildo list Archive --all-accounts",
				"maildo list inbox --plain",
			},
			related:      []string{"search", "folders list"},
			outputModes:  modes,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
				cmd.Flags().Bool("unread", false, "Show only unread messages")
				cmd.Flags().Int("limit", 25, "Messages per page (max 1000)")
				cmd.Flags().Int("page", 1, "Page of results to show, starting at 1")
			},
			run: h.listMessages,
		},
		{
			path: "read", use: "read [id]", short: "Read a message", groupID: "messages",
			examples: []string{
				"maildo read 4d1a2f3",
				"maildo read --id 3c1f2a7",
				`maildo read --internet-id "<message-id@example.com>" --account work`,
				`maildo read --query "from:client@example.com subject:quote" --latest`,
				`maildo read --query "invoice has:attachment" --latest --all-accounts --json`,
			},
			related:      []string{"thread", "search", "list"},
			outputModes:  modes,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
				cmd.Flags().String("id", "", "Message id, or a short id from list or search")
				cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
				cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
				cmd.Flags().Bool("latest", false, "With --query, use the newest match")
				cmd.Flags().Bool("first", false, "With --query, use the first match; matches come newest first, so this is the same as --latest")
				cmd.Flags().String("body-mode", string(mail.BodyModeReadable), "How to show the body: readable, text, html or raw")
			},
			run: h.readMessage,
		},
		{
			path: "thread", use: "thread [id]", short: "Show the messages of a conversation, with the start of each", groupID: "messages",
			examples: []string{
				"maildo thread 4d1a2f3",
				"maildo thread --id 3c1f2a7",
				"maildo thread --thread-id AAQk...",
				`maildo thread --query "subject:contract" --first --account work --json`,
			},
			related:      []string{"read", "search", "list"},
			outputModes:  threadModes,
			supportsMeta: true,
			args:         cobra.MaximumNArgs(1),
			idArg:        true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
				cmd.Flags().String("id", "", "Message id, or a short id from list or search")
				cmd.Flags().String("internet-id", "", "Message-ID header of the message, such as <id@example.com>")
				cmd.Flags().String("thread-id", "", "Thread or conversation id from the provider, as in --json output")
				cmd.Flags().String("query", "", "Search for the message; use with --latest or --first")
				cmd.Flags().Bool("latest", false, "With --query, use the newest match")
				cmd.Flags().Bool("first", false, "With --query, use the first match; matches come newest first, so this is the same as --latest")
			},
			run: h.readThread,
		},
	}
	defs = markMailboxAuth(defs,
		"search",
		"list",
		"read",
		"thread",
	)
	return messagesModule{h: h, defs: defs}
}

func (m messagesModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "messages", Title: "Search and read"}}
}

func (m messagesModule) Commands() []*rungrad.Command {
	out := make([]*rungrad.Command, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.command())
	}
	return out
}

func (m messagesModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

// searchMessages parses the search, looks up --folder in each account, and
// lets internal/search merge, sort and page the results.
func (h *handlers) searchMessages(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		query, err := search.ParseQuery(strings.Join(args, " "))
		if err != nil {
			return err
		}
		limit, offset, err := pageFlags(cmd)
		if err != nil {
			return err
		}
		folderArg, _ := cmd.Flags().GetString("folder")
		folderSet := cmd.Flags().Changed("folder")
		var folderRef mail.FolderRef
		if folderSet {
			folderRef, err = mail.ParseFolderRef(folderArg)
			if err != nil {
				return err
			}
		}
		run := func(ctx context.Context, acct provider.ResolvedAccount, offset, limit int) (mail.MessageSummaryList, error) {
			var folder *mail.Folder
			if folderSet {
				resolved, err := h.resolveFolder(ctx, clientScope, acct, folderRef)
				if err != nil {
					return mail.MessageSummaryList{}, err
				}
				folder = &resolved
			}
			var out mail.MessageSummaryList
			err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesSearch, func(searcher provider.MessageSearcher) error {
				var err error
				out, err = searcher.SearchMessages(ctx, mail.SearchRequest{Query: query, Folder: folder, Limit: limit, Offset: offset})
				return err
			})
			return out, err
		}
		page, err := search.ExecutePage(cmd.Context(), sel, run, offset, limit)
		if err != nil {
			return err
		}
		result := searchResult{AccountSelection: sel, Query: query, MessagePage: newMessagePage(page, offset, limit)}
		warnPage(f, page)
		// Quote the search the way a person would type it back.
		raw := "\"" + strings.Join(args, " ") + "\""
		if strings.Contains(raw[1:len(raw)-1], "\"") {
			raw = "'" + strings.Join(args, " ") + "'"
		}
		view := messageView{
			showing:  "messages matching " + raw + accountSuffix(sel),
			empty:    "No messages match " + raw + accountSuffix(sel) + ".",
			folders:  !folderSet,
			nextPage: nextPageCommand(cmd, args, result.MessagePage),
		}
		if !folderSet {
			view.empty += " Trash and Junk are not searched unless you pass --folder."
		}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  messagesMeta(cmd.CommandPath(), sel, len(page.Messages)),
			Human: humanMessageList(f, result.MessagePage, offset, view),
			Plain: plainMessageList(page.Messages),
		})
	})
}

// listMessages looks up the folder in each account and then asks for its
// newest messages.
func (h *handlers) listMessages(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		limit, offset, err := pageFlags(cmd)
		if err != nil {
			return err
		}
		unread, _ := cmd.Flags().GetBool("unread")
		folderArg := mail.WellKnownInbox
		if len(args) == 1 {
			folderArg = args[0]
		}
		ref, err := mail.ParseFolderRef(folderArg)
		if err != nil {
			return err
		}
		// Look up the folder inside each account, because folder names and ids
		// differ between accounts.
		run := func(ctx context.Context, acct provider.ResolvedAccount, offset, limit int) (mail.MessageSummaryList, error) {
			folder, err := h.resolveFolder(ctx, clientScope, acct, ref)
			if err != nil {
				return mail.MessageSummaryList{}, err
			}
			var out mail.MessageSummaryList
			err = provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesList, func(lister provider.MessageLister) error {
				var err error
				out, err = lister.ListMessages(ctx, mail.ListMessagesRequest{Folder: folder, Filter: mail.ListFilter{Unread: unread}, Limit: limit, Offset: offset})
				return err
			})
			return out, err
		}
		page, err := search.ExecutePage(cmd.Context(), sel, run, offset, limit)
		if err != nil {
			return err
		}
		result := listMessagesResult{AccountSelection: sel, Filter: mail.ListFilter{Unread: unread}, MessagePage: newMessagePage(page, offset, limit)}
		warnPage(f, page)
		folderName := folderArg
		if len(page.Messages) > 0 {
			folderName = folderSummaryText(page.Messages[0].Folder)
		}
		kind := "messages"
		if unread {
			kind = "unread messages"
		}
		where := " in " + folderName + accountSuffix(sel)
		view := messageView{
			showing:  kind + where,
			empty:    "No " + kind + where + ".",
			nextPage: nextPageCommand(cmd, args, result.MessagePage),
		}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  messagesMeta(cmd.CommandPath(), sel, len(page.Messages)),
			Human: humanMessageList(f, result.MessagePage, offset, view),
			Plain: plainMessageList(page.Messages),
		})
	})
}

// resolveFolder finds a folder by name, path or id in the account's folder
// list.
func (h *handlers) resolveFolder(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, ref mail.FolderRef) (mail.Folder, error) {
	var folder mail.Folder
	err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		list, err := reader.ListFolders(ctx, folderListRequest(mail.FolderListDetailIdentity))
		if err != nil {
			return err
		}
		res, err := mail.ResolveFolderRef(mail.FolderResolveScope{Account: acct.Name, Provider: acct.Provider}, ref, list.Folders)
		if err != nil {
			return err
		}
		folder = res.Folder
		return nil
	})
	return folder, err
}

// readLimit checks that --limit is between 1 and the maximum.
func readLimit(cmd *cobra.Command) (int, error) {
	limit, _ := cmd.Flags().GetInt("limit")
	if limit < 1 || limit > maxPageLimit {
		return 0, rungrad.NewError(rungrad.ExitUsage, fmt.Sprintf("--limit must be between 1 and %d", maxPageLimit))
	}
	return limit, nil
}

const maxPageLimit = 1000

// pageFlags reads --limit and --page into a limit and an offset.
func pageFlags(cmd *cobra.Command) (limit, offset int, err error) {
	limit, err = readLimit(cmd)
	if err != nil {
		return 0, 0, err
	}
	page, _ := cmd.Flags().GetInt("page")
	if page < 1 {
		return 0, 0, rungrad.NewError(rungrad.ExitUsage, "--page must be 1 or more")
	}
	return limit, (page - 1) * limit, nil
}

// warnPage prints a warning on stderr for each account that failed and each
// message maildo could not read. JSON has them in "warnings".
func warnPage(f *rungrad.Factory, page search.Page) {
	for _, w := range page.Warnings {
		f.Infof("Warning: %s", w)
	}
}
