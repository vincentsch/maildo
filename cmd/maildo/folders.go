package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// foldersModule holds the folders commands.
type foldersModule struct {
	h    *handlers
	defs []cmdDef
}

// newFoldersModule declares the folders commands. `folders list` can show
// several accounts; the others work in one.
func newFoldersModule(h *handlers) foldersModule {
	listModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModePlain, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	viewModes := []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate}
	defs := []cmdDef{
		{
			path: "folders", use: "folders", short: "List and change folders", groupID: "folders",
			examples: []string{"maildo folders list", "maildo folders view inbox"},
		},
		{
			path: "folders list", use: "list", short: "List folders with their message counts",
			examples: []string{
				"maildo folders list",
				"maildo folders list --account work",
				"maildo folders list --all-accounts",
				"maildo folders list --json",
				"maildo folders list --plain",
			},
			related:      []string{"folders view"},
			outputModes:  listModes,
			supportsMeta: true,
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
			},
			run: h.foldersList,
		},
		{
			path: "folders view", use: "view <folder>", short: "Show one folder",
			examples: []string{
				"maildo folders view inbox",
				"maildo folders view \"Clients/ACME\" --account work",
				"maildo folders view inbox --json",
			},
			related:      []string{"folders list"},
			outputModes:  viewModes,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.foldersView,
		},
		{
			path: "folders create", use: "create <name>", short: "Create a folder",
			examples: []string{
				`maildo folders create Clients --account work`,
				`maildo folders create ACME --parent Clients`,
				`maildo folders create Receipts --dry-run --json`,
			},
			related:      []string{"folders list", "folders view", "folders rename"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("parent", "", "Create the folder inside this folder")
			},
			run: h.foldersCreate,
		},
		{
			path: "folders rename", use: "rename <folder> <new-name>", short: "Rename a folder",
			examples: []string{
				`maildo folders rename Clients Accounts`,
				`maildo folders rename clients-id Accounts --account work`,
				`maildo folders rename Clients Accounts --dry-run --json`,
			},
			related:      []string{"folders list", "folders view", "folders create"},
			outputModes:  viewModes,
			mutates:      true,
			supportsMeta: true,
			args:         cobra.ExactArgs(2),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
			},
			run: h.foldersRename,
		},
		{
			path: "folders delete", use: "delete <folder>", short: "Delete an empty folder without subfolders (on Gmail, an unused label)",
			examples: []string{
				`maildo folders delete "Old Projects" --account work --dry-run`,
				`maildo folders delete old-projects-id --confirm old-projects-id`,
				`maildo folders delete "Clients/2024" --confirm "Clients/2024" --json`,
			},
			related:      []string{"folders list", "folders view"},
			outputModes:  viewModes,
			mutates:      true,
			destructive:  true,
			supportsMeta: true,
			args:         cobra.ExactArgs(1),
			configure: func(cmd *cobra.Command) {
				provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeSingle})
				cmd.Flags().String("confirm", "", "The folder's id, or its full path, again to confirm the delete; --dry-run prints the command")
			},
			run: h.foldersDelete,
		},
	}
	defs = markMailboxAuth(defs,
		"folders list",
		"folders view",
		"folders create",
		"folders rename",
		"folders delete",
	)
	return foldersModule{h: h, defs: defs}
}

func (m foldersModule) Groups() []rungrad.Group {
	return []rungrad.Group{{ID: "folders", Title: "Folders"}}
}

func (m foldersModule) Commands() []*rungrad.Command {
	parent := m.defs[0].command()
	for _, def := range m.defs[1:] {
		parent.AddCommand(def.command())
	}
	return []*rungrad.Command{parent}
}

func (m foldersModule) Catalog() []rungrad.CommandSpec {
	out := make([]rungrad.CommandSpec, 0, len(m.defs))
	for _, def := range m.defs {
		out = append(out, def.catalog())
	}
	return out
}

// foldersList reads the folders of every account before it prints anything.
// When one account fails, the command fails and prints no half list.
func (h *handlers) foldersList(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		folders := make([]mail.Folder, 0)
		req := folderListRequest(mail.FolderListDetailFull)
		for _, acct := range sel.Accounts {
			var list mail.FolderList
			if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
				var err error
				list, err = reader.ListFolders(cmd.Context(), req)
				return err
			}); err != nil {
				return err
			}
			folders = append(folders, list.Folders...)
		}
		sortFoldersForList(sel, folders)
		result := folderListResult{AccountSelection: sel, Folders: folders}
		return f.WriteOutput(rungrad.Output{
			Model: result,
			Meta:  foldersListMeta(cmd.CommandPath(), sel, len(folders)),
			Human: humanFolderList(f, result),
			Plain: plainFolderList(result),
		})
	})
}

func sortFoldersForList(sel provider.AccountSelection, folders []mail.Folder) {
	accountOrder := make(map[string]int, len(sel.Accounts))
	for i, acct := range sel.Accounts {
		accountOrder[acct.Name] = i
	}
	sort.SliceStable(folders, func(i, j int) bool {
		ai, aj := accountOrder[folders[i].Account], accountOrder[folders[j].Account]
		if ai != aj {
			return ai < aj
		}
		if folders[i].Path != folders[j].Path {
			return folders[i].Path < folders[j].Path
		}
		if folders[i].DisplayName != folders[j].DisplayName {
			return folders[i].DisplayName < folders[j].DisplayName
		}
		return folders[i].ID < folders[j].ID
	})
}

// foldersView finds the folder in one read of the account's folders. The
// folder can be named by id, path, display name or a standard name like
// "inbox"; an unknown or ambiguous name is an error.
func (h *handlers) foldersView(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		ref, err := mail.ParseFolderRef(args[0])
		if err != nil {
			return err
		}
		var res mail.FolderResolution
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
			identityList, err := reader.ListFolders(cmd.Context(), folderListRequest(mail.FolderListDetailIdentity))
			if err != nil {
				return err
			}
			res, err = mail.ResolveFolderRef(mail.FolderResolveScope{Account: acct.Name, Provider: acct.Provider}, ref, identityList.Folders)
			if err != nil {
				return err
			}
			selectedReq := folderListSelectedRequest(res.Folder.ID)
			selectedList, err := reader.ListFolders(cmd.Context(), selectedReq)
			if err != nil {
				return err
			}
			selected, err := commandSelectedFolder(acct, selectedList.Folders, res.Folder.ID)
			if err != nil {
				return err
			}
			res.Folder = selected
			return nil
		}); err != nil {
			return err
		}
		result := folderViewResult{
			AccountSelection: sel,
			Account:          acct.Name,
			Provider:         acct.Provider,
			Ref:              res.Ref,
			MatchedBy:        res.MatchedBy,
			Folder:           res.Folder,
		}
		return f.WriteResultWithMeta(result, foldersViewMeta(cmd.CommandPath(), acct, res), humanFolderView(result))
	})
}

// foldersCreate checks the new name before it reads any folders. With
// --parent, it looks up the parent first, so the server gets its id.
func (h *handlers) foldersCreate(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		name, err := mail.ValidateFolderDisplayName(args[0])
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		var parent *mail.Folder
		parentRaw, _ := cmd.Flags().GetString("parent")
		if strings.TrimSpace(parentRaw) != "" {
			res, _, err := h.resolveFolderWithSnapshot(cmd.Context(), clientScope, acct, parentRaw)
			if err != nil {
				return err
			}
			parent = &res.Folder
		}
		req := mail.FolderCreateRequest{DisplayName: name}
		if parent != nil {
			summary := mail.SummaryFrom(*parent)
			req.Parent = &summary
		}
		if f.DryRun() {
			if err := provider.CheckClientAccess(acct, provider.ClientAccessFoldersMutate); err != nil {
				return err
			}
			return writeFolderPreview(f, folderMutationPreviewPlan{
				Account:     acct,
				Operation:   mail.FolderOperationCreate,
				DisplayName: name,
				Parent:      req.Parent,
				Command:     folderCommand(cmd, args, acct, ""),
			})
		}
		var result mail.FolderMutationResult
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersMutate, func(manager provider.FolderManager) error {
			var err error
			result, err = manager.CreateFolder(cmd.Context(), req)
			return err
		})
		path := name
		if req.Parent != nil {
			path = folderSummaryText(*req.Parent) + "/" + name
		}
		change := audit.Change{Kind: "folder", Name: path}
		if result.CreatedFolder != nil {
			change.ID, change.Name = result.CreatedFolder.ID, folderSummaryText(*result.CreatedFolder)
		}
		logStatus := h.logChange(f, cmd, acct, string(mail.FolderOperationCreate), change, err)
		if err != nil {
			return err
		}
		out := folderMutationResultFromProvider(sel, result, true)
		out.ActionLogStatus = logStatus
		return f.WriteResultWithMeta(out, folderMutationMeta(cmd.CommandPath(), out), humanFolderMutation(out))
	})
}

// foldersRename refuses standard folders such as the Inbox, then asks
// the provider whether the folder may be renamed, before it changes anything.
func (h *handlers) foldersRename(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		newName, err := mail.ValidateFolderDisplayName(args[1])
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		res, _, err := h.resolveFolderWithSnapshot(cmd.Context(), clientScope, acct, args[0])
		if err != nil {
			return err
		}
		if res.Folder.WellKnownName != "" {
			return protectedFolderError(acct, res.Folder, "well_known_folder")
		}
		var preflight mail.FolderMutationPreflight
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersMutate, func(manager provider.FolderManager) error {
			var err error
			preflight, err = manager.PrepareFolderRename(cmd.Context(), mail.FolderRenamePreflightRequest{Target: res.Folder})
			return err
		}); err != nil {
			return err
		}
		if preflight.Protected {
			return protectedFolderError(acct, res.Folder, preflight.ProtectionReason)
		}
		if f.DryRun() {
			target := mail.SummaryFrom(res.Folder)
			return writeFolderPreview(f, folderMutationPreviewPlan{
				Account:        acct,
				Operation:      mail.FolderOperationRename,
				Target:         &target,
				NewDisplayName: newName,
				Preflight:      preflight,
				Command:        folderCommand(cmd, args, acct, ""),
			})
		}
		var result mail.FolderMutationResult
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersMutate, func(manager provider.FolderManager) error {
			var err error
			result, err = manager.RenameFolder(cmd.Context(), mail.FolderRenameRequest{
				Target:         res.Folder,
				NewDisplayName: newName,
				Preflight:      preflight,
			})
			return err
		})
		change := audit.Change{Kind: "folder", ID: res.Folder.ID, Name: folderSummaryText(mail.SummaryFrom(res.Folder)), NewName: newName}
		if result.RenamedFolder != nil {
			change.NewName = folderSummaryText(*result.RenamedFolder)
			if result.RenamedFolder.ID != res.Folder.ID {
				change.NewID = result.RenamedFolder.ID
			}
		}
		logStatus := h.logChange(f, cmd, acct, string(mail.FolderOperationRename), change, err)
		if err != nil {
			return err
		}
		out := folderMutationResultFromProvider(sel, result, true)
		out.ActionLogStatus = logStatus
		return f.WriteResultWithMeta(out, folderMutationMeta(cmd.CommandPath(), out), humanFolderMutation(out))
	})
}

// foldersDelete never asks a question; it needs --confirm instead. It counts
// the messages and checks whether the folder may be deleted before it looks
// at --confirm. The --dry-run preview shows the folder, the counts and the
// command with --confirm, but the error for a missing --confirm does not.
func (h *handlers) foldersDelete(f *rungrad.Factory, cmd *cobra.Command, args []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(clientScope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(clientScope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeSingle})
		if err != nil {
			return err
		}
		acct := sel.Accounts[0]
		res, snapshot, err := h.resolveFolderWithSnapshot(cmd.Context(), clientScope, acct, args[0])
		if err != nil {
			return err
		}
		if res.Folder.WellKnownName != "" {
			return protectedFolderError(acct, res.Folder, "well_known_folder")
		}
		var preflight mail.FolderMutationPreflight
		if err := provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersMutate, func(manager provider.FolderManager) error {
			var err error
			preflight, err = manager.PrepareFolderDelete(cmd.Context(), mail.FolderDeletePreflightRequest{Target: res.Folder})
			return err
		}); err != nil {
			return err
		}
		// A provider that marks a folder as full or as having subfolders
		// gets the same answer as the count check below.
		if preflight.Protected && preflight.ProtectionReason != "not_empty" && preflight.ProtectionReason != "has_children" {
			return protectedFolderError(acct, res.Folder, preflight.ProtectionReason)
		}
		if err := requireEmptyFolder(acct, res.Folder, preflight); err != nil {
			return err
		}
		pathAccepted := folderPathIsUnique(res.Folder.Path, snapshot.Folders)
		if f.DryRun() {
			target := mail.SummaryFrom(res.Folder)
			token := res.Folder.ID
			if pathAccepted {
				token = res.Folder.Path
			}
			return writeFolderPreview(f, folderMutationPreviewPlan{
				Account:                   acct,
				Operation:                 mail.FolderOperationDelete,
				Target:                    &target,
				Preflight:                 preflight,
				ConfirmationRequired:      true,
				PathConfirmationAvailable: pathAccepted,
				Command:                   folderCommand(cmd, args, acct, token),
			})
		}
		confirm, _ := cmd.Flags().GetString("confirm")
		if !confirmFolderDelete(confirm, res.Folder, snapshot.Folders) {
			return confirmationRequiredError(acct, res.Ref.Kind, pathAccepted)
		}
		var result mail.FolderMutationResult
		err = provider.WithAccountClient(cmd.Context(), clientScope, acct, provider.ClientAccessFoldersMutate, func(manager provider.FolderManager) error {
			var err error
			result, err = manager.DeleteFolder(cmd.Context(), mail.FolderDeleteRequest{Target: res.Folder, Preflight: preflight})
			return err
		})
		logStatus := h.logChange(f, cmd, acct, string(mail.FolderOperationDelete), audit.Change{Kind: "folder", ID: res.Folder.ID, Name: folderSummaryText(mail.SummaryFrom(res.Folder))}, err)
		if err != nil {
			return err
		}
		out := folderMutationResultFromProvider(sel, result, true)
		out.ConfirmationRequired = true
		out.ConfirmationMatched = true
		out.ActionLogStatus = logStatus
		return f.WriteResultWithMeta(out, folderMutationMeta(cmd.CommandPath(), out), humanFolderMutation(out))
	})
}

// resolveFolderWithSnapshot returns the folder and the folder list it was
// found in. A delete uses that list to check that no other folder has the
// same path before it accepts the path in --confirm.
func (h *handlers) resolveFolderWithSnapshot(ctx context.Context, clientScope provider.AccountClientScope, acct provider.ResolvedAccount, raw string) (mail.FolderResolution, mail.FolderList, error) {
	ref, err := mail.ParseFolderRef(raw)
	if err != nil {
		return mail.FolderResolution{}, mail.FolderList{}, err
	}
	var list mail.FolderList
	var res mail.FolderResolution
	err = provider.WithAccountClient(ctx, clientScope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		var err error
		list, err = reader.ListFolders(ctx, folderListRequest(mail.FolderListDetailIdentity))
		if err != nil {
			return err
		}
		res, err = mail.ResolveFolderRef(mail.FolderResolveScope{Account: acct.Name, Provider: acct.Provider}, ref, list.Folders)
		return err
	})
	if err != nil {
		return mail.FolderResolution{}, mail.FolderList{}, err
	}
	return res, list, nil
}

func folderListRequest(detail mail.FolderListDetail) mail.FolderListRequest {
	req, err := mail.NewFolderListRequest(detail, nil)
	if err != nil {
		panic(err)
	}
	return req
}

func folderListSelectedRequest(id string) mail.FolderListRequest {
	req, err := mail.NewFolderListRequest(mail.FolderListDetailSelected, []string{id})
	if err != nil {
		panic(err)
	}
	return req
}

func commandSelectedFolder(acct provider.ResolvedAccount, folders []mail.Folder, id string) (mail.Folder, error) {
	if len(folders) != 1 {
		return mail.Folder{}, invalidSelectedFolderResponse(acct, id)
	}
	folder := folders[0]
	if folder.ID != id || folder.Account != acct.Name || folder.Provider != acct.Provider {
		return mail.Folder{}, invalidSelectedFolderResponse(acct, id)
	}
	return folder, nil
}

func invalidSelectedFolderResponse(acct provider.ResolvedAccount, id string) error {
	return errs.NewWithDetails(errs.CodeProviderDataError, "the server sent folder details for the wrong folder", map[string]any{
		"account":   acct.Name,
		"provider":  acct.Provider,
		"operation": "folders_view",
		"reason":    "invalid_protocol",
		"folder_id": id,
	})
}

func protectedFolderError(acct provider.ResolvedAccount, folder mail.Folder, reason string) error {
	if reason == "" {
		reason = "protected_folder"
	}
	return errs.NewWithDetails(errs.CodeProtectedFolder,
		fmt.Sprintf("folder %q is protected and cannot be renamed or deleted", folder.DisplayName),
		map[string]any{
			"account":         acct.Name,
			"provider":        acct.Provider,
			"folder_id":       folder.ID,
			"display_name":    folder.DisplayName,
			"path":            folder.Path,
			"well_known_name": folder.WellKnownName,
			"reason":          reason,
		})
}

// confirmationRequiredError says what --confirm needs without printing the
// folder's id or path, which would let a script paste it back. The --dry-run
// preview shows the folder and its counts.
func confirmationRequiredError(acct provider.ResolvedAccount, refKind mail.FolderRefKind, pathAccepted bool) error {
	shape := "provider_folder_id"
	if pathAccepted {
		shape = "provider_folder_id_or_unique_resolved_path"
	}
	return errs.NewWithDetails(errs.CodeConfirmationRequired,
		"deleting a folder needs --confirm with the folder's id, or with its full path when no other folder has the same path; run the command with --dry-run to see the folder and the full command",
		map[string]any{
			"account":                     acct.Name,
			"provider":                    acct.Provider,
			"ref_kind":                    string(refKind),
			"accepted_token_shape":        shape,
			"path_confirmation_available": pathAccepted,
		})
}

// requireEmptyFolder lets a delete go ahead only for a folder that holds
// no messages and has no subfolders. In this beta maildo deletes nothing
// else: a delete can take messages and subfolders along, and some servers
// cannot say for sure what that would reach. A folder whose counts are
// unknown is refused too.
func requireEmptyFolder(acct provider.ResolvedAccount, target mail.Folder, preflight mail.FolderMutationPreflight) error {
	name := firstNonEmpty(target.Path, target.DisplayName)
	details := map[string]any{
		"account":      acct.Name,
		"provider":     acct.Provider,
		"folder_id":    target.ID,
		"display_name": target.DisplayName,
		"path":         target.Path,
	}
	refuse := func(reason, message, hint string) error {
		details["reason"] = reason
		details["hint"] = hint
		return errs.NewWithDetails(errs.CodeProtectedFolder, message, details)
	}
	messages := preflight.TotalItemCountKnown && preflight.TotalItemCount > 0
	subfolders := preflight.ChildFolderKnown && preflight.ChildFolderCount > 0
	switch {
	case messages || preflight.ProtectionReason == "not_empty":
		holds := "still holds messages"
		if messages {
			details["total_item_count"] = preflight.TotalItemCount
			holds = "holds " + pluralize(preflight.TotalItemCount, "message", "messages")
		}
		return refuse("not_empty",
			fmt.Sprintf("folder %q %s, and maildo deletes only empty folders without subfolders", name, holds),
			"move or delete its messages first, then delete the folder")
	case subfolders || preflight.ProtectionReason == "has_children":
		has := "has subfolders"
		if subfolders {
			details["child_folder_count"] = preflight.ChildFolderCount
			has = "has " + pluralize(preflight.ChildFolderCount, "subfolder", "subfolders")
		}
		return refuse("has_children",
			fmt.Sprintf("folder %q %s, and maildo deletes only empty folders without subfolders", name, has),
			"delete its subfolders first, then delete the folder")
	case preflight.CountsSource != mail.FolderCountsSourceRefreshed || !preflight.TotalItemCountKnown || !preflight.ChildFolderKnown:
		return refuse("counts_unknown",
			fmt.Sprintf("maildo could not count the messages and subfolders in %q, so it did not delete it", name),
			"maildo deletes a folder only when it can see that the folder is empty; try again later")
	}
	return nil
}

// folderCommand is the command that makes a previewed folder change: the
// same command for the same account, without --dry-run. A delete also
// carries its --confirm value.
func folderCommand(cmd *cobra.Command, args []string, acct provider.ResolvedAccount, confirm string) string {
	set := [][2]string{{"account", acct.Name}}
	if confirm != "" {
		set = append(set, [2]string{"confirm", confirm})
	}
	return changeCommand(cmd, args, set)
}

// changeCommand is a previewed command ready to run: without --dry-run,
// without flags that only change how output looks, and with set added.
func changeCommand(cmd *cobra.Command, args []string, set [][2]string) string {
	return commandLine(cmd, args, []string{"dry-run", "no-color", "no-ansi", "no-pager", "quiet", "no-prompt"}, set)
}

func folderPathIsUnique(path string, folders []mail.Folder) bool {
	if path == "" {
		return false
	}
	count := 0
	for _, folder := range folders {
		if folder.Path == path {
			count++
		}
	}
	return count == 1
}

func confirmFolderDelete(value string, target mail.Folder, folders []mail.Folder) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value == target.ID {
		return true
	}
	return target.Path != "" && value == target.Path && folderPathIsUnique(target.Path, folders)
}

func foldersListMeta(commandPath string, sel provider.AccountSelection, count int) output.Meta {
	accounts, providers := accountProviderNames(sel)
	return output.Meta{Extra: map[string]any{
		"command_path": commandPath,
		"scope":        sel.Scope,
		"accounts":     accounts,
		"providers":    providers,
		"folder_count": count,
	}}
}

func foldersViewMeta(commandPath string, acct provider.ResolvedAccount, res mail.FolderResolution) output.Meta {
	return output.Meta{Extra: map[string]any{
		"command_path": commandPath,
		"scope":        "single",
		"account":      acct.Name,
		"provider":     acct.Provider,
		"ref":          res.Ref.Raw,
		"ref_kind":     string(res.Ref.Kind),
		"matched_by":   res.MatchedBy,
	}}
}

func accountProviderNames(sel provider.AccountSelection) ([]string, []string) {
	accounts := make([]string, 0, len(sel.Accounts))
	providers := make([]string, 0, len(sel.Accounts))
	for _, acct := range sel.Accounts {
		accounts = append(accounts, acct.Name)
		providers = append(providers, acct.Provider)
	}
	return accounts, providers
}
