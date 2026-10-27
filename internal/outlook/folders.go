package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

const folderPageSize = 100

var errGraphNotFound = errors.New("graph: not found")

// graphFolder is the part of Graph's mailFolder that maildo needs to list
// folders and build their paths.
type graphFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	TotalItemCount   int    `json:"totalItemCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	ChildFolderCount int    `json:"childFolderCount"`
}

// graphFolderPage is one page of Graph's folder list. The paging details
// stay inside this package.
type graphFolderPage struct {
	Value    []graphFolder `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

// ListFolders returns all folders of the mailbox. After reading the tree it
// marks the standard folders, matched by id.
func (c *Client) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	normalized, err := mail.NewFolderListRequest(req.Detail, req.DetailIDs)
	if err != nil {
		return mail.FolderList{}, err
	}
	fetched, err := c.fetchFolderTree(ctx)
	if err != nil {
		if errors.Is(err, errGraphNotFound) {
			return mail.FolderList{}, c.folderError()
		}
		return mail.FolderList{}, err
	}

	byID := make(map[string]graphFolder, len(fetched))
	for _, f := range fetched {
		if f.ID != "" {
			byID[f.ID] = f
		}
	}
	paths, parentPaths := deriveFolderPaths(fetched, byID)
	wellKnown, err := c.fetchWellKnownFolders(ctx)
	if err != nil {
		return mail.FolderList{}, err
	}

	out := make([]mail.Folder, 0, len(fetched))
	for _, f := range fetched {
		row := mail.Folder{
			Account:          c.account,
			Provider:         "outlook",
			ID:               f.ID,
			DisplayName:      f.DisplayName,
			WellKnownName:    wellKnown[f.ID],
			ParentID:         f.ParentFolderID,
			ParentPath:       parentPaths[f.ID],
			Path:             paths[f.ID],
			TotalCount:       f.TotalItemCount,
			TotalCountKnown:  true,
			UnreadCount:      f.UnreadItemCount,
			UnreadCountKnown: true,
			ChildCount:       f.ChildFolderCount,
			ProviderMeta:     graphFolderMeta(mail.MetadataFolder),
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].ID < out[j].ID
		}
		return out[i].Path < out[j].Path
	})
	out = selectFolderDetailRows(out, normalized)
	return mail.FolderList{Folders: out}, nil
}

func selectFolderDetailRows(folders []mail.Folder, req mail.FolderListRequest) []mail.Folder {
	if req.Detail != mail.FolderListDetailSelected {
		return folders
	}
	ids := make(map[string]struct{}, len(req.DetailIDs))
	for _, id := range req.DetailIDs {
		ids[id] = struct{}{}
	}
	out := make([]mail.Folder, 0, len(req.DetailIDs))
	for _, folder := range folders {
		if _, ok := ids[folder.ID]; ok {
			out = append(out, folder)
		}
	}
	return out
}

// fetchFolderTree reads the folder tree level by level. Graph sends each
// folder's number of subfolders, so only folders that have some are opened.
// A folder id or page address that comes back twice anywhere in the walk is
// an error. The number of folders and of page requests is limited.
func (c *Client) fetchFolderTree(ctx context.Context) ([]graphFolder, error) {
	var pages graphPageGuard
	seen := make(map[string]struct{})
	accept := func(f graphFolder) error {
		if f.ID == "" {
			return c.folderTreeDataError("Outlook sent a folder without an id")
		}
		if f.ChildFolderCount < 0 {
			return c.folderTreeDataError("Outlook sent a folder with an invalid number of subfolders")
		}
		if _, repeated := seen[f.ID]; repeated {
			return c.folderTreeDataError("Outlook sent the same folder twice")
		}
		if len(seen) >= limits.OutlookMaxFolders {
			return errs.NewReadLimitExceeded(c.account, errs.ProviderOutlook, errs.ReadLimitFolders, uint64(limits.OutlookMaxFolders+1))
		}
		seen[f.ID] = struct{}{}
		return nil
	}
	rootURL := fmt.Sprintf("%s/me/mailFolders?$top=%d", c.graphBaseURL, folderPageSize)
	roots, err := c.fetchFolderPagesWithGuard(ctx, rootURL, &pages, accept)
	if err != nil {
		return nil, err
	}
	out := append([]graphFolder(nil), roots...)
	queue := make([]string, 0, len(roots))
	for _, f := range roots {
		if f.ChildFolderCount > 0 {
			queue = append(queue, f.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		childURL := fmt.Sprintf("%s/me/mailFolders/%s/childFolders?$top=%d", c.graphBaseURL, url.PathEscape(id), folderPageSize)
		// accept refuses a folder id it has seen, so no folder is queued
		// twice. The page guard refuses a page address it has already
		// fetched, also when it came from another folder's next link.
		children, err := c.fetchFolderPagesWithGuard(ctx, childURL, &pages, accept)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
		for _, f := range children {
			if f.ChildFolderCount > 0 {
				queue = append(queue, f.ID)
			}
		}
	}
	return out, nil
}

func (c *Client) folderTreeDataError(message string) error {
	return errs.NewWithDetails(errs.CodeProviderDataError, message, map[string]any{"account": c.account, "provider": "outlook"})
}

// fetchFolderPages reads all pages of one folder list. Each next link goes
// through graphGet, which checks it points to Graph before sending the token.
func (c *Client) fetchFolderPages(ctx context.Context, firstURL string) ([]graphFolder, error) {
	var pages graphPageGuard
	return c.fetchFolderPagesWithGuard(ctx, firstURL, &pages, nil)
}

func (c *Client) fetchFolderPagesWithGuard(ctx context.Context, firstURL string, pages *graphPageGuard, accept func(graphFolder) error) ([]graphFolder, error) {
	var out []graphFolder
	next := firstURL
	for next != "" {
		if err := pages.visit(ctx, c, next); err != nil {
			return nil, err
		}
		var page graphFolderPage
		if err := c.graphGet(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, folder := range page.Value {
			if accept != nil {
				if err := accept(folder); err != nil {
					return nil, err
				}
			}
			out = append(out, folder)
		}
		next = page.NextLink
	}
	return out, nil
}

// deriveFolderPaths builds each folder's path from the parent ids, in
// whatever order Graph sent the folders. A folder whose parent is not in
// the list gets a top-level path. That is what Graph's top-level folders
// look like: their parent is the mailbox root, which Graph does not list.
func deriveFolderPaths(folders []graphFolder, byID map[string]graphFolder) (map[string]string, map[string]string) {
	paths := make(map[string]string, len(folders))
	parentPaths := make(map[string]string, len(folders))
	visiting := map[string]bool{}
	var pathFor func(id string) string
	pathFor = func(id string) string {
		if p, ok := paths[id]; ok {
			return p
		}
		f, ok := byID[id]
		if !ok {
			return ""
		}
		// A broken answer with a parent loop must not recurse forever. The
		// folder then gets a top-level path and can still be picked by id.
		if visiting[id] {
			paths[id] = f.DisplayName
			parentPaths[id] = ""
			return paths[id]
		}
		visiting[id] = true
		defer delete(visiting, id)
		parent, hasParent := byID[f.ParentFolderID]
		if !hasParent || parent.ID == id {
			paths[id] = f.DisplayName
			parentPaths[id] = ""
			return paths[id]
		}
		parentPath := pathFor(parent.ID)
		if parentPath == "" {
			paths[id] = f.DisplayName
			parentPaths[id] = ""
			return paths[id]
		}
		parentPaths[id] = parentPath
		paths[id] = parentPath + "/" + f.DisplayName
		return paths[id]
	}
	for _, f := range folders {
		if f.ID != "" {
			pathFor(f.ID)
		}
	}
	return paths, parentPaths
}

// fetchWellKnownFolders asks Graph for the standard folders by their
// well-known names and maps each id to its role. This works however the
// folders are named in the user's language.
func (c *Client) fetchWellKnownFolders(ctx context.Context) (map[string]string, error) {
	rows, err := c.wellKnownFolderRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for role, f := range rows {
		out[f.ID] = role
	}
	return out, nil
}

// graphGet sends a folder read only to Graph and follows no redirects, also
// none that net/http would follow on its own.
func (c *Client) graphGet(ctx context.Context, target string, v any) error {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return err
	}
	if !c.validGraphURL(target) {
		return c.folderError()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "could not prepare the folder request to Outlook")
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := c.doGraphRequestNoRedirect(req)
	if err != nil {
		return c.folderError()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errGraphNotFound
	}
	// Microsoft can revoke a saved token. Say that the login needs repair,
	// as for any other broken login, rather than give a general error.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return repairRequired(c.account)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.folderError()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.folderError()
	}
	if err := json.Unmarshal(body, v); err != nil {
		return c.folderError()
	}
	return nil
}

// validGraphURL accepts only full URLs on the Graph host maildo is set up
// for, and, when that base URL has a path, only below that path.
func (c *Client) validGraphURL(target string) bool {
	base, err := url.Parse(c.graphBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.Fragment != "" || !safeGraphPath(base.EscapedPath()) {
		return false
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" || !safeGraphPath(u.EscapedPath()) {
		return false
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return false
	}
	basePath := strings.TrimRight(base.EscapedPath(), "/")
	if basePath == "" {
		return true
	}
	path := u.EscapedPath()
	return path == basePath || strings.HasPrefix(path, basePath+"/")
}

// safeGraphPath rejects "." and ".." path segments, also percent-encoded
// ones. An HTTP client or proxy could resolve them to a path outside Graph.
// Other escaped characters in ids are left alone.
func safeGraphPath(escapedPath string) bool {
	for _, segment := range strings.Split(escapedPath, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "." || decoded == ".." {
			return false
		}
	}
	return true
}

// folderError is broad on purpose. It must not show request URLs, tokens or
// the path of the saved login.
func (c *Client) folderError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not list folders for account %q (outlook)", c.account))
}
