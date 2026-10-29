package outlook

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"

	"github.com/vincentsch/maildo/internal/mail"
)

// graphWellKnownNames maps maildo's folder roles to Graph's well-known
// folder names.
var graphWellKnownNames = map[string]string{
	mail.WellKnownInbox:   "inbox",
	mail.WellKnownDrafts:  "drafts",
	mail.WellKnownSent:    "sentitems",
	mail.WellKnownDeleted: "deleteditems",
	mail.WellKnownArchive: "archive",
	mail.WellKnownJunk:    "junkemail",
}

// searchScope is what a search without a folder needs to know about the
// mailbox's standard folders: the folder ids whose messages it leaves out,
// the folders it searches alone, and the names of folders rows point to.
type searchScope struct {
	leaveOut map[string]bool
	only     []string
	names    map[string]mail.FolderSummary
}

func (c *Client) newSearchScope(ctx context.Context, skip, only []string) (*searchScope, error) {
	known, err := c.wellKnownFolderRows(ctx)
	if err != nil {
		return nil, err
	}
	scope := &searchScope{leaveOut: map[string]bool{}, names: map[string]mail.FolderSummary{}}
	for role, f := range known {
		scope.names[f.ID] = mail.FolderSummary{ID: f.ID, DisplayName: f.DisplayName, WellKnownName: role}
	}
	if len(only) > 0 {
		for _, role := range only {
			// A mailbox without the folder (many have no Archive) has
			// nothing to search there.
			if f, ok := known[role]; ok {
				scope.only = append(scope.only, f.ID)
			}
		}
		return scope, nil
	}
	for _, role := range append([]string{mail.WellKnownDeleted, mail.WellKnownJunk}, skip...) {
		if f, ok := known[role]; ok {
			scope.leaveOut[f.ID] = true
		}
	}
	return scope, nil
}

// nameFolders fills in the folder name of rows that carry only a folder id.
// The standard folders are known already; other folders are looked up once
// each.
func (s *searchScope) nameFolders(ctx context.Context, c *Client, rows []mail.MessageSummary) {
	for i := range rows {
		id := rows[i].Folder.ID
		if id == "" {
			continue
		}
		if name, ok := s.names[id]; ok {
			rows[i].Folder = name
			continue
		}
		rows[i].Folder = c.folderName(ctx, id)
	}
}

// folderName looks up the name of the folder with this id. Graph sends only
// the folder id with a message. When the folder is gone or the lookup fails,
// the summary keeps only the id; the command still shows the message.
func (c *Client) folderName(ctx context.Context, id string) mail.FolderSummary {
	if id == "" {
		return mail.FolderSummary{}
	}
	c.folderMu.Lock()
	defer c.folderMu.Unlock()
	if f, ok := c.folderNames[id]; ok {
		return f
	}
	out := mail.FolderSummary{ID: id}
	var f graphFolder
	target := fmt.Sprintf("%s/me/mailFolders/%s?$select=id,displayName", c.graphBaseURL, url.PathEscape(id))
	if err := c.graphGet(ctx, target, &f); err == nil {
		out.DisplayName = f.DisplayName
	} else if !errors.Is(err, errGraphNotFound) {
		// Try again next time; a 404 is final.
		return out
	}
	if c.folderNames == nil {
		c.folderNames = map[string]mail.FolderSummary{}
	}
	c.folderNames[id] = out
	return out
}

// wellKnownLookups is how many standard folders maildo looks up at the same
// time. Graph answers 429 when one app sends more than four requests to the
// same mailbox at once.
const wellKnownLookups = 3

// wellKnownFolderRows asks Graph for the standard folders by their
// well-known names, a few at a time, and keys them by maildo role. A mailbox
// without one of them (Graph answers 404) gets no entry for it.
func (c *Client) wellKnownFolderRows(ctx context.Context) (map[string]graphFolder, error) {
	type answer struct {
		role   string
		folder graphFolder
		err    error
	}
	answers := make(chan answer, len(graphWellKnownNames))
	slots := make(chan struct{}, wellKnownLookups)
	var wg sync.WaitGroup
	for role, name := range graphWellKnownNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			var f graphFolder
			err := c.graphGet(ctx, fmt.Sprintf("%s/me/mailFolders/%s", c.graphBaseURL, name), &f)
			answers <- answer{role: role, folder: f, err: err}
		}()
	}
	wg.Wait()
	close(answers)
	out := make(map[string]graphFolder, len(graphWellKnownNames))
	var firstErr error
	for a := range answers {
		switch {
		case errors.Is(a.err, errGraphNotFound):
		case a.err != nil:
			if firstErr == nil {
				firstErr = a.err
			}
		case a.folder.ID != "":
			out[a.role] = a.folder
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
