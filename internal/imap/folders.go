package imap

import (
	"context"
	"errors"
	"sort"
	"strings"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

var (
	_ provider.FolderReader            = (*accountClient)(nil)
	_ provider.MessageSearcher         = (*accountClient)(nil)
	_ provider.MessageLister           = (*accountClient)(nil)
	_ provider.MessageReader           = (*accountClient)(nil)
	_ provider.ThreadReader            = (*accountClient)(nil)
	_ provider.MessageAttachmentReader = (*accountClient)(nil)
)

// Where folder roles (Inbox, Sent, Trash...) come from. Servers with the
// SPECIAL-USE extension say it; for the others maildo guesses from common
// folder names, as every mail client does.
const (
	specialUseUnavailable = "unavailable"
	specialUseIMAP4rev1   = "imap4rev1"
	specialUseIMAP4rev2   = "imap4rev2"
	specialUseNames       = "names"
)

type imapFolderSnapshot struct {
	folders             []mail.Folder
	byID                map[string]imapFolderRecord
	byMailbox           map[string]imapFolderRecord
	selectable          []imapFolderRecord
	specialUseAuthority string
	activeRevision      imapActiveRevision
}

type imapFolderRecord struct {
	folder      mail.Folder
	mailbox     string
	delimiter   rune
	selectable  bool
	uidValidity uint32
	uidNext     uint32
	specialUse  []mail.ProviderSpecialUse
}

func (r imapFolderRecord) hasRole(role mail.ProviderSpecialUse) bool {
	for _, special := range r.specialUse {
		if special == role {
			return true
		}
	}
	return false
}

type imapFolderBuild struct {
	mailbox     string
	delimiter   rune
	attrs       map[goimap.MailboxAttr]bool
	specialUse  []mail.ProviderSpecialUse
	selectable  bool
	displayName string
	path        string
	parentPath  string
	parentName  string
	parentID    string
	orphan      bool
	status      imapMailboxStatus
}

type imapMailboxStatus struct {
	total               uint32
	unread              uint32
	uidValidity         uint32
	uidNext             uint32
	known               bool
	uidNotSticky        bool
	readWriteKnown      bool
	readWrite           bool
	permanentFlagsKnown bool
	permanentFlags      []goimap.Flag
	highestModSeqKnown  bool
	highestModSeq       uint64
	noModSeq            bool
}

func (c *accountClient) ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error) {
	normalized, err := mail.NewFolderListRequest(req.Detail, req.DetailIDs)
	if err != nil {
		return mail.FolderList{}, err
	}
	var out mail.FolderList
	err = c.withIMAPSession(ctx, func(session *imapSession) error {
		// Finding a folder by name needs no message counts, so maildo does not
		// ask for STATUS here.
		withCounts := normalized.Detail != mail.FolderListDetailIdentity
		snapshot, err := c.folderSnapshot(ctx, session, withCounts)
		if err != nil {
			return err
		}
		folders := snapshot.folders
		if normalized.Detail == mail.FolderListDetailSelected {
			selected := make([]mail.Folder, 0, len(normalized.DetailIDs))
			for _, id := range normalized.DetailIDs {
				record, ok := snapshot.byID[id]
				if !ok {
					return errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
						"account":   c.account,
						"provider":  c.provider,
						"folder_id": id,
					})
				}
				selected = append(selected, record.folder)
			}
			folders = selected
		}
		out = mail.FolderList{Folders: folders}
		return nil
	})
	return out, err
}

// fetchFolderSnapshot lists every folder with message counts. Commands that
// change mail use it to check the folders they write to.
func (c *accountClient) fetchFolderSnapshot(ctx context.Context, session *imapSession) (imapFolderSnapshot, error) {
	return c.folderSnapshot(ctx, session, true)
}

// folderSnapshot runs LIST "" "*" and, when withCounts is set, asks for
// message counts in the same LIST (LIST-STATUS) or with pipelined STATUS
// commands. Odd LIST rows, such as duplicates or names that do not decode,
// are merged or left out, so one bad row does not fail the whole listing.
func (c *accountClient) folderSnapshot(ctx context.Context, session *imapSession, withCounts bool) (imapFolderSnapshot, error) {
	state, err := session.activeState(ctx)
	if err != nil {
		return imapFolderSnapshot{}, err
	}
	caps := state.capabilities
	authority := specialUseNames
	opts := &goimap.ListOptions{}
	if state.revision == imapActiveRev2 {
		authority = specialUseIMAP4rev2
		opts.ReturnSpecialUse = true
	} else if advertisedCapability(caps, goimap.CapSpecialUse) {
		authority = specialUseIMAP4rev1
		opts.ReturnSpecialUse = true
	}
	listStatus := withCounts && (state.revision == imapActiveRev2 || advertisedCapability(caps, goimap.CapListStatus))
	if listStatus {
		opts.ReturnStatus = statusOptions()
	}
	if !opts.ReturnSpecialUse && opts.ReturnStatus == nil {
		opts = nil
	}

	var listed []*goimap.ListData
	if err := session.withReadCommand(ctx, errs.ProviderOperationIMAPFoldersRead, func(dep *imapclient.Client) error {
		var err error
		listed, err = dep.List("", "*", opts).Collect()
		return err
	}); err != nil {
		return imapFolderSnapshot{}, err
	}
	if len(listed) > limits.IMAPMaxMailboxes {
		return imapFolderSnapshot{}, errs.NewReadLimitExceeded(c.account, imapErrProvider(c.provider), errs.ReadLimitMailboxes, uint64(len(listed)))
	}

	builds := make(map[string]*imapFolderBuild, len(listed))
	for _, item := range listed {
		if item == nil || item.Mailbox == "" {
			continue
		}
		mailbox, err := canonicalMailboxName(item.Mailbox)
		if err != nil {
			continue
		}
		attrs := mailboxAttrSet(item.Attrs)
		selectable := !attrs[attrKey(goimap.MailboxAttrNoSelect)] && !attrs[attrKey(goimap.MailboxAttrNonExistent)]
		if build, exists := builds[mailbox]; exists {
			for attr := range attrs {
				build.attrs[attr] = true
			}
			build.selectable = build.selectable && selectable
			continue
		}
		build := &imapFolderBuild{mailbox: mailbox, delimiter: item.Delim, attrs: attrs, selectable: selectable}
		if item.Status != nil {
			build.status = statusFromData(item.Status)
		}
		builds[mailbox] = build
	}

	names := make([]string, 0, len(builds))
	for name := range builds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		build := builds[name]
		if authority == specialUseNames {
			build.specialUse = specialUseFromName(build.mailbox, build.delimiter)
		} else {
			build.specialUse = specialUseFromAttrs(build.attrs)
		}
		if err := deriveFolderBuildNames(build, builds); err != nil {
			delete(builds, name)
		}
	}
	names = names[:0]
	for name := range builds {
		names = append(names, name)
	}
	sort.Strings(names)

	if withCounts && !listStatus {
		var need []*imapFolderBuild
		for _, name := range names {
			if builds[name].selectable {
				need = append(need, builds[name])
			}
		}
		if err := c.pipelinedStatus(ctx, session, need); err != nil {
			return imapFolderSnapshot{}, err
		}
	}

	children := map[string]int{}
	for _, build := range builds {
		if build.parentID != "" {
			children[build.parentID]++
		}
	}
	snapshot := imapFolderSnapshot{
		folders:             make([]mail.Folder, 0, len(names)),
		byID:                make(map[string]imapFolderRecord, len(names)),
		byMailbox:           make(map[string]imapFolderRecord, len(names)),
		specialUseAuthority: authority,
		activeRevision:      state.revision,
	}
	for _, name := range names {
		build := builds[name]
		id, err := formatFolderID(build.mailbox)
		if err != nil {
			continue
		}
		meta := mail.FolderProviderMetadata{
			Source:       mail.MetadataSourceIMAP,
			Selectable:   boolPtr(build.selectable),
			SpecialUse:   append([]mail.ProviderSpecialUse(nil), build.specialUse...),
			CountsSource: mail.CountsSourceUnknown,
		}
		if build.status.known {
			meta.CountsSource = mail.CountsSourceStatus
		}
		if build.orphan {
			meta.OrphanParent = boolPtr(true)
		}
		providerMeta, err := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataFolder, meta)
		if err != nil {
			return imapFolderSnapshot{}, err
		}
		folder := mail.Folder{
			Account:          c.account,
			Provider:         c.provider,
			ID:               id,
			DisplayName:      build.displayName,
			WellKnownName:    wellKnownFolderName(build.mailbox, build.specialUse),
			ParentID:         build.parentID,
			ParentPath:       build.parentPath,
			Path:             build.path,
			TotalCount:       int(build.status.total),
			TotalCountKnown:  build.status.known,
			UnreadCount:      int(build.status.unread),
			UnreadCountKnown: build.status.known,
			ChildCount:       children[id],
			ProviderMeta:     providerMeta,
		}
		record := imapFolderRecord{
			folder:      folder,
			mailbox:     build.mailbox,
			delimiter:   build.delimiter,
			selectable:  build.selectable,
			uidValidity: build.status.uidValidity,
			uidNext:     build.status.uidNext,
			specialUse:  append([]mail.ProviderSpecialUse(nil), build.specialUse...),
		}
		snapshot.folders = append(snapshot.folders, folder)
		snapshot.byID[id] = record
		snapshot.byMailbox[build.mailbox] = record
		if build.selectable {
			snapshot.selectable = append(snapshot.selectable, record)
		}
	}
	sort.SliceStable(snapshot.folders, func(i, j int) bool {
		a, b := snapshot.folders[i], snapshot.folders[j]
		if ap, bp := sortableFolderPath(a), sortableFolderPath(b); ap != bp {
			return ap < bp
		}
		return a.ID < b.ID
	})
	sort.SliceStable(snapshot.selectable, func(i, j int) bool {
		return sortableFolderPath(snapshot.selectable[i].folder) < sortableFolderPath(snapshot.selectable[j].folder)
	})
	return snapshot, nil
}

func statusOptions() *goimap.StatusOptions {
	return &goimap.StatusOptions{NumMessages: true, NumUnseen: true, UIDValidity: true, UIDNext: true}
}

func statusFromData(data *goimap.StatusData) imapMailboxStatus {
	if data == nil || data.NumMessages == nil || data.UIDValidity == 0 {
		return imapMailboxStatus{}
	}
	status := imapMailboxStatus{total: *data.NumMessages, uidValidity: data.UIDValidity, uidNext: uint32(data.UIDNext), known: true}
	if data.NumUnseen != nil {
		status.unread = *data.NumUnseen
	}
	return status
}

// pipelinedStatus sends one STATUS per folder without waiting for each
// answer, then collects the answers. On a distant server this costs about one
// round trip instead of one per folder. A folder whose STATUS fails is
// listed without counts.
func (c *accountClient) pipelinedStatus(ctx context.Context, session *imapSession, builds []*imapFolderBuild) error {
	if len(builds) == 0 {
		return nil
	}
	return session.withReadCommand(ctx, errs.ProviderOperationIMAPFoldersRead, func(dep *imapclient.Client) error {
		cmds := make([]*imapclient.StatusCommand, len(builds))
		for i, build := range builds {
			cmds[i] = dep.Status(build.mailbox, statusOptions())
		}
		for i, cmd := range cmds {
			data, err := cmd.Wait()
			if err != nil {
				var imapErr *goimap.Error
				if errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeNo {
					continue
				}
				return err
			}
			builds[i].status = statusFromData(data)
		}
		return nil
	})
}

// mailboxAttrSet stores attributes lower-cased: servers differ in how they
// spell \Noselect or \HasChildren.
func mailboxAttrSet(attrs []goimap.MailboxAttr) map[goimap.MailboxAttr]bool {
	out := make(map[goimap.MailboxAttr]bool, len(attrs))
	for _, attr := range attrs {
		out[attrKey(attr)] = true
	}
	return out
}

func attrKey(attr goimap.MailboxAttr) goimap.MailboxAttr {
	return goimap.MailboxAttr(strings.ToLower(string(attr)))
}

var specialUseAttrs = map[goimap.MailboxAttr]mail.ProviderSpecialUse{
	goimap.MailboxAttrAll:     mail.SpecialUseAll,
	goimap.MailboxAttrArchive: mail.SpecialUseArchive,
	goimap.MailboxAttrDrafts:  mail.SpecialUseDrafts,
	goimap.MailboxAttrFlagged: mail.SpecialUseFlagged,
	goimap.MailboxAttrJunk:    mail.SpecialUseJunk,
	goimap.MailboxAttrSent:    mail.SpecialUseSent,
	goimap.MailboxAttrTrash:   mail.SpecialUseTrash,
}

func specialUseFromAttrs(attrs map[goimap.MailboxAttr]bool) []mail.ProviderSpecialUse {
	var values []mail.ProviderSpecialUse
	for attr, special := range specialUseAttrs {
		if attrs[attrKey(attr)] {
			values = append(values, special)
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}

// Folder names that mail clients treat as a role on servers without
// SPECIAL-USE. Only top-level folders (or folders directly under INBOX, as
// Courier and Cyrus name them) count, so "Clients/Archive" stays a normal
// folder.
var specialUseNamesByRole = map[string]mail.ProviderSpecialUse{
	"archive": mail.SpecialUseArchive, "archives": mail.SpecialUseArchive, "archiv": mail.SpecialUseArchive,
	"drafts": mail.SpecialUseDrafts, "draft": mail.SpecialUseDrafts, "entwürfe": mail.SpecialUseDrafts,
	"junk": mail.SpecialUseJunk, "spam": mail.SpecialUseJunk, "junk e-mail": mail.SpecialUseJunk, "junk email": mail.SpecialUseJunk, "bulk mail": mail.SpecialUseJunk,
	"sent": mail.SpecialUseSent, "sent items": mail.SpecialUseSent, "sent mail": mail.SpecialUseSent, "sent messages": mail.SpecialUseSent, "gesendet": mail.SpecialUseSent, "gesendete objekte": mail.SpecialUseSent, "gesendete elemente": mail.SpecialUseSent,
	"trash": mail.SpecialUseTrash, "deleted": mail.SpecialUseTrash, "deleted items": mail.SpecialUseTrash, "deleted messages": mail.SpecialUseTrash, "papierkorb": mail.SpecialUseTrash, "gelöscht": mail.SpecialUseTrash, "gelöschte elemente": mail.SpecialUseTrash,
}

func specialUseFromName(mailbox string, delimiter rune) []mail.ProviderSpecialUse {
	name := mailbox
	if delimiter != 0 {
		parts := strings.Split(mailbox, string(delimiter))
		switch {
		case len(parts) == 1:
		case len(parts) == 2 && asciiEqualFold(parts[0], "INBOX"):
			name = parts[1]
		default:
			return nil
		}
	}
	if role, ok := specialUseNamesByRole[strings.ToLower(strings.TrimSpace(name))]; ok {
		return []mail.ProviderSpecialUse{role}
	}
	return nil
}

func deriveFolderBuildNames(build *imapFolderBuild, byName map[string]*imapFolderBuild) error {
	if asciiEqualFold(build.mailbox, "INBOX") {
		build.mailbox = "INBOX"
		build.displayName = "INBOX"
		build.path = "INBOX"
		return nil
	}
	delimiter := build.delimiter
	segments := []string{build.mailbox}
	if delimiter != 0 {
		segments = strings.Split(build.mailbox, string(delimiter))
	}
	if len(segments) == 0 || len(segments) > limits.IMAPMaxHierarchyDepth {
		return errBadOpaqueID
	}
	for _, segment := range segments {
		if segment == "" {
			return errBadOpaqueID
		}
	}
	build.displayName = segments[len(segments)-1]
	// A folder name that itself contains "/" cannot be shown as a slash path
	// without becoming ambiguous, so it gets no path.
	slashInName := false
	for _, segment := range segments {
		if strings.Contains(segment, "/") && delimiter != '/' {
			slashInName = true
		}
	}
	if !slashInName {
		build.path = strings.Join(segments, "/")
	}
	if len(segments) <= 1 || delimiter == 0 {
		return nil
	}
	parentName := strings.Join(segments[:len(segments)-1], string(delimiter))
	build.parentName = parentName
	if !slashInName {
		build.parentPath = strings.Join(segments[:len(segments)-1], "/")
	}
	parent, ok := byName[parentName]
	if !ok {
		build.orphan = true
		return nil
	}
	id, err := formatFolderID(parent.mailbox)
	if err != nil {
		return err
	}
	build.parentID = id
	return nil
}

func sortableFolderPath(folder mail.Folder) string {
	if folder.Path != "" {
		return folder.Path
	}
	if folder.DisplayName != "" {
		return folder.DisplayName
	}
	return folder.ID
}

func wellKnownFolderName(mailbox string, special []mail.ProviderSpecialUse) string {
	if asciiEqualFold(mailbox, "INBOX") {
		return mail.WellKnownInbox
	}
	for _, value := range special {
		switch value {
		case mail.SpecialUseArchive:
			return mail.WellKnownArchive
		case mail.SpecialUseDrafts:
			return mail.WellKnownDrafts
		case mail.SpecialUseJunk:
			return mail.WellKnownJunk
		case mail.SpecialUseSent:
			return mail.WellKnownSent
		case mail.SpecialUseTrash:
			return mail.WellKnownDeleted
		}
	}
	return ""
}

// recordFromFolder turns a resolved folder back into its IMAP mailbox name.
func (c *accountClient) recordFromFolder(folder mail.Folder, capability provider.Capability) (imapFolderRecord, error) {
	mailbox, err := parseFolderID(folder.ID)
	if err != nil {
		return imapFolderRecord{}, errs.NewWithDetails(errs.CodeInvalidSelector, "this is not an IMAP folder id", map[string]any{
			"account":   c.account,
			"provider":  c.provider,
			"folder_id": folder.ID,
		})
	}
	if folder.ProviderMeta != nil && folder.ProviderMeta.Selectable != nil && !*folder.ProviderMeta.Selectable {
		return imapFolderRecord{}, errs.NewIMAPSourceMailboxNotSelectable(c.account, imapErrProvider(c.provider), string(capability), folder.ID)
	}
	var special []mail.ProviderSpecialUse
	if folder.ProviderMeta != nil {
		special = append(special, folder.ProviderMeta.SpecialUse...)
	}
	return imapFolderRecord{folder: folder, mailbox: mailbox, selectable: true, specialUse: special}, nil
}

func boolPtr(v bool) *bool { return &v }

func imapErrProvider(providerName string) errs.ProviderName {
	if providerName == "fastmail" {
		return errs.ProviderFastmail
	}
	return errs.ProviderIMAP
}

func (c *accountClient) providerDataError(operation errs.ProviderOperation, reason errs.ProviderDataReason) error {
	failure, err := errs.NewProviderFailureContext(c.account, imapErrProvider(c.provider), errs.ProviderProtocolIMAP, operation)
	if err != nil {
		return err
	}
	return errs.NewProviderDataFailure(failure, reason)
}
