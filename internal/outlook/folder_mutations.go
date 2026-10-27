package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

// graphFolderMutation is the part of Graph's mailFolder answer that folder
// changes and the check before a delete need. The counts are pointers, so a
// count of zero and a missing count can be told apart.
type graphFolderMutation struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	TotalItemCount   *int   `json:"totalItemCount"`
	UnreadItemCount  *int   `json:"unreadItemCount"`
	ChildFolderCount *int   `json:"childFolderCount"`
}

// protectedFolderAliases are Graph's well-known folders. maildo never renames
// or deletes them, even those that maildo does not list.
var protectedFolderAliases = []string{
	"msgfolderroot",
	"inbox",
	"drafts",
	"sentitems",
	"deleteditems",
	"archive",
	"junkemail",
	"outbox",
	"conversationhistory",
	"searchfolders",
	"recoverableitemsdeletions",
	"clutter",
	"scheduled",
	"syncissues",
	"conflicts",
	"localfailures",
	"serverfailures",
}

// PrepareFolderRename checks, before a rename, whether the folder is one of
// Outlook's own folders.
func (c *Client) PrepareFolderRename(ctx context.Context, req mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error) {
	return c.prepareFolderProtection(ctx, mail.FolderOperationRename, req.Target)
}

// PrepareFolderDelete checks whether the folder is one of Outlook's own
// folders. It also reads fresh message and subfolder counts. The command
// deletes only a folder whose counts are both known and zero.
func (c *Client) PrepareFolderDelete(ctx context.Context, req mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error) {
	preflight, err := c.prepareFolderProtection(ctx, mail.FolderOperationDelete, req.Target)
	if err != nil || preflight.Protected {
		return preflight, err
	}
	status, body, err := c.graphFolderMutationDo(ctx, http.MethodGet, c.graphBaseURL+"/me/mailFolders/"+url.PathEscape(req.Target.ID), nil)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.FolderMutationPreflight{}, c.folderMutationNotFound(req.Target.ID, "folder_id")
	}
	if status != http.StatusOK {
		return mail.FolderMutationPreflight{}, c.folderMutationError()
	}
	g, err := c.decodeFolderMutation(body)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if g.ID != req.Target.ID {
		return mail.FolderMutationPreflight{}, c.folderMutationError()
	}
	target := mail.SummaryFrom(req.Target)
	if g.DisplayName != "" {
		target.DisplayName = g.DisplayName
	}
	preflight.Target = target
	if g.TotalItemCount != nil {
		preflight.TotalItemCount = *g.TotalItemCount
		preflight.TotalItemCountKnown = true
	}
	if g.ChildFolderCount != nil {
		preflight.ChildFolderCount = *g.ChildFolderCount
		preflight.ChildFolderKnown = true
	}
	preflight.CountsSource = mail.FolderCountsSourceRefreshed
	return preflight, nil
}

// CreateFolder creates a folder at the top or inside a parent folder. It
// counts as done when Graph returns the new folder's id.
func (c *Client) CreateFolder(ctx context.Context, req mail.FolderCreateRequest) (mail.FolderMutationResult, error) {
	target := c.graphBaseURL + "/me/mailFolders"
	if req.Parent != nil {
		target = c.graphBaseURL + "/me/mailFolders/" + url.PathEscape(req.Parent.ID) + "/childFolders"
	}
	status, body, err := c.graphFolderMutationDo(ctx, http.MethodPost, target, map[string]any{"displayName": req.DisplayName})
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		parentID := ""
		if req.Parent != nil {
			parentID = req.Parent.ID
		}
		return mail.FolderMutationResult{}, c.folderMutationNotFound(parentID, "parent_folder_id")
	}
	if err := c.folderNameStatusError(status, body, req.DisplayName, req.Parent); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status != http.StatusCreated {
		return mail.FolderMutationResult{}, c.folderMutationError()
	}
	g, err := c.decodeFolderMutation(body)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	display := firstNonEmpty(g.DisplayName, req.DisplayName)
	created := mail.FolderSummary{ID: g.ID, DisplayName: display, Path: display}
	if req.Parent != nil {
		created.Path = folderMutationPath(req.Parent.Path, display)
	}
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationCreate,
		ParentFolder:       cloneOutlookFolderSummary(req.Parent),
		CreatedFolder:      &created,
		NewDisplayName:     display,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		ProviderMeta:       graphFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

// RenameFolder changes the folder's name. It counts as done when Graph
// returns the folder's id.
func (c *Client) RenameFolder(ctx context.Context, req mail.FolderRenameRequest) (mail.FolderMutationResult, error) {
	targetURL := c.graphBaseURL + "/me/mailFolders/" + url.PathEscape(req.Target.ID)
	status, body, err := c.graphFolderMutationDo(ctx, http.MethodPatch, targetURL, map[string]any{"displayName": req.NewDisplayName})
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.FolderMutationResult{}, c.folderMutationNotFound(req.Target.ID, "folder_id")
	}
	if err := c.folderNameStatusError(status, body, req.NewDisplayName, nil); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status != http.StatusOK {
		return mail.FolderMutationResult{}, c.folderMutationError()
	}
	g, err := c.decodeFolderMutation(body)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	display := firstNonEmpty(g.DisplayName, req.NewDisplayName)
	target := mail.SummaryFrom(req.Target)
	renamed := mail.FolderSummary{ID: g.ID, DisplayName: display, Path: folderMutationPath(req.Target.ParentPath, display)}
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationRename,
		TargetFolder:       &target,
		RenamedFolder:      &renamed,
		OldDisplayName:     req.Target.DisplayName,
		NewDisplayName:     display,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		ProviderMeta:       graphFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

// DeleteFolder deletes the folder. The command asks for confirmation before;
// this only checks that Graph answered 204.
func (c *Client) DeleteFolder(ctx context.Context, req mail.FolderDeleteRequest) (mail.FolderMutationResult, error) {
	status, _, err := c.graphFolderMutationDo(ctx, http.MethodDelete, c.graphBaseURL+"/me/mailFolders/"+url.PathEscape(req.Target.ID), nil)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.FolderMutationResult{}, c.folderMutationNotFound(req.Target.ID, "folder_id")
	}
	if status != http.StatusNoContent {
		return mail.FolderMutationResult{}, c.folderMutationError()
	}
	target := mail.SummaryFrom(req.Target)
	return mail.FolderMutationResult{
		Operation:           mail.FolderOperationDelete,
		TargetFolder:        &target,
		DeletedFolder:       &target,
		TotalItemCount:      req.Preflight.TotalItemCount,
		TotalItemCountKnown: req.Preflight.TotalItemCountKnown,
		ChildFolderCount:    req.Preflight.ChildFolderCount,
		ChildFolderKnown:    req.Preflight.ChildFolderKnown,
		CountsSource:        req.Preflight.CountsSource,
		VerificationStatus:  mail.FolderVerificationProviderResponse,
		ProviderMeta:        graphFolderMeta(mail.MetadataFolderMutationResult),
	}, nil
}

// prepareFolderProtection marks the folder as protected when it is one of
// Outlook's well-known folders. If the check fails for any reason other than
// a missing well-known folder, nothing is changed.
func (c *Client) prepareFolderProtection(ctx context.Context, op mail.FolderMutationOperation, target mail.Folder) (mail.FolderMutationPreflight, error) {
	summary := mail.SummaryFrom(target)
	preflight := mail.FolderMutationPreflight{
		Operation:    op,
		Target:       summary,
		ProviderMeta: graphFolderMeta(mail.MetadataFolderMutationPreflight),
	}
	protected, err := c.protectedFolderIDs(ctx)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if alias, ok := protected[target.ID]; ok {
		preflight.Protected = true
		preflight.ProtectionReason = "provider_managed_folder"
		preflight.ProviderMeta = graphProtectedFolderMeta(alias)
	}
	return preflight, nil
}

// protectedFolderIDs returns the ids of Outlook's well-known folders, each
// with its well-known name. Not every mailbox has all of them.
func (c *Client) protectedFolderIDs(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(protectedFolderAliases))
	for _, alias := range protectedFolderAliases {
		status, body, err := c.graphFolderMutationDo(ctx, http.MethodGet, c.graphBaseURL+"/me/mailFolders/"+url.PathEscape(alias), nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			continue
		}
		if status != http.StatusOK {
			return nil, c.folderMutationError()
		}
		g, err := c.decodeFolderMutation(body)
		if err != nil {
			return nil, err
		}
		out[g.ID] = alias
	}
	return out, nil
}

// graphFolderMutationDo sends a folder request with its own redirect rules:
// the first URL and every redirect must stay on Graph before the request
// carries the token.
func (c *Client) graphFolderMutationDo(ctx context.Context, method, target string, payload any) (int, []byte, error) {
	if _, err := c.authorizationHeader(); err != nil {
		return 0, nil, err
	}
	// This HTTP client belongs to this request only. Other requests on the
	// same account may run at the same time with other redirect rules.
	return c.graphDoWithMessagesClient(ctx, c.folderMutationHTTPClient(), method, target, payload, "", c.folderMutationError, "could not prepare the folder request to Outlook", "the folder request to Outlook was rate limited", "folder mutation response exceeds the read size limit")
}

// folderMutationHTTPClient returns a copy of the account's HTTP client that
// also checks every redirect stays on Graph. It is a copy, so the shared
// client's settings never change.
func (c *Client) folderMutationHTTPClient() *http.Client {
	guarded := &http.Client{}
	var inherited func(*http.Request, []*http.Request) error
	if c.http != nil {
		guarded.Transport = c.http.Transport
		guarded.Jar = c.http.Jar
		guarded.Timeout = c.http.Timeout
		inherited = c.http.CheckRedirect
	}
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		expectedMethod := req.Method
		if len(via) > 0 {
			expectedMethod = via[len(via)-1].Method
		}
		validate := func() error {
			if req.URL == nil || !c.validGraphURL(req.URL.String()) {
				return fmt.Errorf("Outlook folder mutation redirect was rejected")
			}
			// net/http or a custom callback can change the request method on a
			// redirect. That is refused, so the answer to a read is never taken
			// for a successful create, rename or delete.
			if req.Method != expectedMethod {
				return fmt.Errorf("Outlook folder mutation redirect changed the request method")
			}
			return nil
		}
		if err := validate(); err != nil {
			return err
		}
		if inherited != nil {
			// The caller's own redirect rule runs only after the target passed
			// the Graph URL check, and the URL is checked again afterwards in
			// case that rule changed the request.
			if err := inherited(req, via); err != nil {
				return err
			}
			return validate()
		}
		// Match net/http's default limit when no custom policy was supplied.
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return guarded
}

// decodeFolderMutation requires an id in the answer, so a folder change
// counts as done only when Graph named the folder it changed.
func (c *Client) decodeFolderMutation(body []byte) (graphFolderMutation, error) {
	var out graphFolderMutation
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return graphFolderMutation{}, c.folderMutationError()
	}
	return out, nil
}

// folderNameStatusError turns only the Graph error codes it knows into
// invalid_folder_name. Any other 400 stays a general error; maildo does not
// guess from Graph's error text.
func (c *Client) folderNameStatusError(status int, body []byte, name string, parent *mail.FolderSummary) error {
	switch {
	case status == http.StatusConflict:
		return c.invalidFolderName(name, parent, "duplicate_name", "folder name is already taken in that parent")
	case status == http.StatusBadRequest:
		switch graphErrorCode(body) {
		case "ErrorInvalidPropertyValue", "ErrorInvalidParameter":
			return c.invalidFolderName(name, parent, "provider_rejected_name", "folder name is not valid for Outlook")
		}
	}
	return nil
}

// invalidFolderName reports a name Outlook refused with the same error and
// details as a name maildo itself refuses.
func (c *Client) invalidFolderName(name string, parent *mail.FolderSummary, reason, message string) error {
	details := map[string]any{
		"account":      c.account,
		"provider":     "outlook",
		"display_name": name,
		"reason":       reason,
	}
	if parent != nil {
		details["parent_folder_id"] = parent.ID
		details["parent_path"] = parent.Path
	}
	return errs.NewWithDetails(errs.CodeInvalidFolderName, message, details)
}

// folderMutationNotFound adds the id field only when there is a folder or
// parent id to report.
func (c *Client) folderMutationNotFound(id, field string) error {
	details := map[string]any{
		"account":  c.account,
		"provider": "outlook",
	}
	if id != "" && field != "" {
		details[field] = id
	}
	return errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", details)
}

func (c *Client) folderMutationError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("the folder change in Outlook account %q failed", c.account))
}

func cloneOutlookFolderSummary(in *mail.FolderSummary) *mail.FolderSummary {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// folderMutationPath builds the folder's path again, because Graph's answer
// does not include it.
func folderMutationPath(parentPath, display string) string {
	if parentPath == "" {
		return display
	}
	return parentPath + "/" + display
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
