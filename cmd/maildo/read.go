package main

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/search"
	"github.com/vincentsch/rungrad"
)

// selectorScope is the account the read uses and, for --query, all the
// accounts the search looked in.
type selectorScope struct {
	Selection provider.AccountSelection
	Searched  *provider.AccountSelection
	Owner     provider.ResolvedAccount
	Selected  *search.SelectedMessage
}

// readMessage finds the account and the message id first, and then reads the
// message. A --query is run here, so the provider only ever reads a message
// by its id.
func (h *handlers) readMessage(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		modeFlag, _ := cmd.Flags().GetString("body-mode")
		bodyMode, err := mail.ParseBodyMode(modeFlag)
		if err != nil {
			return err
		}
		selector, err := parseCommandSelector(cmd, false)
		if err != nil {
			return err
		}
		scope, err := h.resolveSelectorScope(f, cmd, clientScope, selector)
		if err != nil {
			return err
		}
		var resolved mail.ResolvedMessageSelector
		if selector.Kind == mail.SelectorQuery {
			// The search already found the account and the message id, so the
			// read does not search again.
			resolved = mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: scope.Selected.Message.ID}
		} else {
			resolved = selector.Resolved()
		}
		var msg mail.Message
		if err := provider.WithAccountClient(cmd.Context(), clientScope, scope.Owner, provider.ClientAccessMessagesRead, func(reader provider.MessageReader) error {
			var err error
			msg, err = reader.ReadMessage(cmd.Context(), mail.ReadMessageRequest{Selector: resolved, BodyMode: bodyMode})
			return err
		}); err != nil {
			return err
		}
		result := readResult{AccountSelection: scope.Selection, Message: msg}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  readMeta(cmd.CommandPath(), scope.Selection, selector, bodyMode, scope.Searched),
			Human: humanReadMessage(f, result),
			Plain: plainReadBody(msg),
		})
	})
}

// readThread finds the conversation of a message. With --query it uses the
// thread id of the message the search found. When the search result has no
// thread id, the provider finds the conversation from the message id.
func (h *handlers) readThread(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		selector, err := parseCommandSelector(cmd, true)
		if err != nil {
			return err
		}
		scope, err := h.resolveSelectorScope(f, cmd, clientScope, selector)
		if err != nil {
			return err
		}
		var resolved mail.ResolvedMessageSelector
		if selector.Kind == mail.SelectorQuery {
			if scope.Selected.Message.ThreadID != "" {
				resolved = mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorThreadID, ThreadID: scope.Selected.Message.ThreadID}
			} else {
				resolved = mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: scope.Selected.Message.ID}
			}
		} else {
			resolved = selector.Resolved()
		}
		var thread mail.Thread
		if err := provider.WithAccountClient(cmd.Context(), clientScope, scope.Owner, provider.ClientAccessThreadsRead, func(reader provider.ThreadReader) error {
			var err error
			thread, err = reader.ReadThread(cmd.Context(), mail.ReadThreadRequest{Selector: resolved})
			return err
		}); err != nil {
			return err
		}
		result := threadResult{AccountSelection: scope.Selection, Thread: thread}
		return f.WriteResultWithMeta(result, threadMeta(cmd.CommandPath(), scope.Selection, selector, len(thread.Messages), scope.Searched), humanThread(f, result))
	})
}

// parseCommandSelector reads how the user picked the message from the flags.
// It checks which flags were given, because --id "" gets a different error
// than no --id at all.
func parseCommandSelector(cmd *cobra.Command, allowThreadID bool) (mail.MessageSelector, error) {
	flags := cmd.Flags()
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
	id, _ := flags.GetString("id")
	internetID, _ := flags.GetString("internet-id")
	threadID := ""
	hasThreadID := false
	if flags.Lookup("thread-id") != nil {
		threadID, _ = flags.GetString("thread-id")
		hasThreadID = flags.Changed("thread-id")
	}
	latest, _ := flags.GetBool("latest")
	first, _ := flags.GetBool("first")
	return mail.ParseMessageSelector(mail.SelectorFlags{
		ID: id, HasID: flags.Changed("id"),
		InternetID: internetID, HasInternetID: flags.Changed("internet-id"),
		ThreadID: threadID, HasThreadID: hasThreadID,
		Query: query, HasQuery: hasQuery,
		Latest: latest, First: first, AllowThreadID: allowThreadID,
	})
}

// resolveSelectorScope returns the account for the read. --query may search
// several accounts and then uses the account of the message it found. An id
// always belongs to one account, because the same id could mean different
// messages in two accounts.
func (h *handlers) resolveSelectorScope(f *rungrad.Factory, cmd *cobra.Command, clientScope provider.AccountClientScope, selector mail.MessageSelector) (selectorScope, error) {
	if selector.Kind == mail.SelectorQuery {
		searched, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return selectorScope{}, err
		}
		run := func(ctx context.Context, acct provider.ResolvedAccount, limit int) (mail.MessageSummaryList, error) {
			var out mail.MessageSummaryList
			err := provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessMessagesSearch, func(searcher provider.MessageSearcher) error {
				var err error
				out, err = searcher.SearchMessages(ctx, mail.SearchRequest{Query: *selector.Query, Limit: limit})
				return err
			})
			return out, err
		}
		selected, err := search.ResolveSelected(cmd.Context(), searched, run)
		if err != nil {
			return selectorScope{}, err
		}
		// Selection is the account the message is in. Searched is every account
		// the search looked at, which --include-meta shows.
		narrowed := provider.AccountSelection{Scope: "single", Accounts: []provider.ResolvedAccount{selected.Owner}}
		searchedCopy := selected.Searched
		return selectorScope{Selection: narrowed, Searched: &searchedCopy, Owner: selected.Owner, Selected: &selected}, nil
	}

	accounts, _ := cmd.Flags().GetStringArray("account")
	if len(accounts) > 1 {
		return selectorScope{}, errs.New(errs.CodeInvalidSelector, "a message id belongs to one account, so give at most one --account")
	}
	allAccounts, _ := cmd.Flags().GetBool("all-accounts")
	if allAccounts {
		return selectorScope{}, errs.New(errs.CodeInvalidSelector, "--all-accounts does not work with a message id; use --account to name its account")
	}
	sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
	if err != nil {
		return selectorScope{}, err
	}
	return selectorScope{Selection: sel, Owner: sel.Accounts[0]}, nil
}

func readSelectorLimit() int {
	return 1
}
