package imap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"sort"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// Listing and searching messages.
//
// Both work the same way. The server does the matching: one UID SEARCH per
// folder returns the matching UIDs. One cheap UID FETCH (INTERNALDATE) per
// folder tells when each match arrived. maildo sorts all matches newest first
// across folders, cuts out the requested page, and fetches details only for
// that page, in one UID FETCH per folder. A folder with 100,000 messages
// costs a few round trips, not one per message.

const (
	// fetchBatch is the most UIDs one detail FETCH asks for.
	fetchBatch = 250
	// dateBatch is the most UIDs one INTERNALDATE FETCH asks for. Those
	// answers are small, so the batch can be large.
	dateBatch = 5000
	// previewBytes is how much of the text part is fetched for the preview.
	previewBytes = 2048
	previewRunes = 200
	// wholePreviewPartBytes is the largest text part fetched whole for a
	// preview.
	wholePreviewPartBytes = 16 * 1024
)

// maxSortCandidates is how many of a folder's newest matches (by UID) are
// sorted by date. Larger result sets are cut with a warning. Tests lower it.
var maxSortCandidates = 20000

type imapFetchedMessage struct {
	account           string
	provider          string
	folder            mail.FolderSummary
	mailbox           string
	uidValidity       uint32
	uid               uint32
	id                string
	internetMessageID string
	threadID          string
	envelope          *goimap.Envelope
	flags             []goimap.Flag
	internalDate      *time.Time
	rfc822Size        int64
	bodyStructure     goimap.BodyStructure
	header            imapHeaderBlock
	preview           string
}

type imapHeaderBlock struct {
	raw          []byte
	values       textproto.MIMEHeader
	visible      []mail.Header
	messageID    string
	references   []string
	inReplyTo    []string
	completeRefs bool
}

// threadHeaderFields are fetched with every message row so the row can carry
// its thread id.
var threadHeaderFields = []string{"Message-ID", "References", "In-Reply-To"}

func (c *accountClient) ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	folder, err := c.recordFromFolder(req.Folder, provider.CapMessagesList)
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	var criteria goimap.SearchCriteria
	if req.Filter.Unread {
		criteria.NotFlag = []goimap.Flag{goimap.FlagSeen}
	}
	var out mail.MessageSummaryList
	err = c.withIMAPSession(ctx, func(session *imapSession) error {
		var err error
		out, _, err = c.collectPage(ctx, session, []imapFolderRecord{folder}, criteria, matchFilter{}, req.Offset, req.Limit, errs.ProviderOperationIMAPMessagesList)
		return err
	})
	return out, err
}

func (c *accountClient) SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	criteria, filter := searchCriteria(req.Query)
	var out mail.MessageSummaryList
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		var folders []imapFolderRecord
		if req.Folder != nil {
			record, err := c.recordFromFolder(*req.Folder, provider.CapMessagesSearch)
			if err != nil {
				return err
			}
			folders = append(folders, record)
		} else {
			snapshot, err := c.folderSnapshot(ctx, session, false)
			if err != nil {
				return err
			}
			var allMail bool
			folders, allMail = scopedFolders(snapshot, req.SkipRoles, req.OnlyRoles)
			if allMail {
				c.skipRolesInAllMail(&criteria, req.SkipRoles)
			}
		}
		var err error
		var leftOut bool
		out, leftOut, err = c.collectPage(ctx, session, folders, criteria, filter, req.Offset, req.Limit, errs.ProviderOperationIMAPMessagesSearch)
		if err == nil && leftOut && req.Complete {
			err = c.incompleteSearch(out.Warnings)
		}
		return err
	})
	return out, err
}

// searchableFolders picks the folders a mailbox-wide search looks in. Trash
// and Junk are left out, as in most mail clients; search them with --folder.
// When the server has an "All Mail" folder (Gmail), that folder alone holds
// every message, so searching it avoids duplicates.
func searchableFolders(snapshot imapFolderSnapshot) []imapFolderRecord {
	folders, _ := scopedFolders(snapshot, nil, nil)
	return folders
}

// scopedFolders is searchableFolders with roles to leave out (the skip
// argument) or to look at alone (only). allMail is true when the result is Gmail's All Mail
// folder. That folder holds Sent and Drafts too, so those roles are left
// out with search criteria instead.
func scopedFolders(snapshot imapFolderSnapshot, skip, only []string) (folders []imapFolderRecord, allMail bool) {
	if len(only) > 0 {
		for _, record := range snapshot.selectable {
			if hasRoleName(only, record.folder.WellKnownName) {
				folders = append(folders, record)
			}
		}
		return folders, false
	}
	for _, record := range snapshot.selectable {
		if record.hasRole(mail.SpecialUseAll) {
			return []imapFolderRecord{record}, true
		}
	}
	for _, record := range snapshot.selectable {
		if record.hasRole(mail.SpecialUseTrash) || record.hasRole(mail.SpecialUseJunk) || hasRoleName(skip, record.folder.WellKnownName) {
			continue
		}
		folders = append(folders, record)
	}
	return folders, false
}

// skipRolesInAllMail leaves Drafts and Sent out of a search in Gmail's All
// Mail folder. Drafts carry the \Draft flag. Sent mail is recognized by the
// account's own address in From, which also leaves out mail you sent to
// yourself.
func (c *accountClient) skipRolesInAllMail(criteria *goimap.SearchCriteria, skip []string) {
	if hasRoleName(skip, mail.WellKnownDrafts) {
		criteria.NotFlag = append(criteria.NotFlag, goimap.FlagDraft)
	}
	if hasRoleName(skip, mail.WellKnownSent) && c.setup.Email != "" {
		criteria.Not = append(criteria.Not, goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: "From", Value: c.setup.Email}}})
	}
}

func hasRoleName(roles []string, role string) bool {
	if role == "" {
		return false
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// incompleteSearch is the error for a search that left out matches while
// the caller needs all of them, as commands that change mail do.
func (c *accountClient) incompleteSearch(warnings []string) error {
	reason := "some matches were left out"
	if len(warnings) > 0 {
		reason = warnings[0]
	}
	return errs.NewWithDetails(errs.CodeReadLimitExceeded,
		"the search could not return every match ("+reason+"), so maildo will not pick messages from it",
		map[string]any{
			"account":     c.account,
			"provider":    c.provider,
			"reason":      "search_incomplete",
			"remediation": "narrow the search, for example with newer:30d or from:, or name a folder with --folder",
		})
}

// matchFilter holds the parts of a query that IMAP SEARCH cannot answer
// exactly. They are checked on the INTERNALDATE pass.
type matchFilter struct {
	after          *time.Time
	before         *time.Time
	needsStructure bool
	hasAttachment  *bool
}

func (f matchFilter) match(date time.Time, structure goimap.BodyStructure) bool {
	if f.after != nil && date.Before(*f.after) {
		return false
	}
	if f.before != nil && !date.Before(*f.before) {
		return false
	}
	if f.hasAttachment != nil {
		parts, _ := attachmentParts(structure)
		if (len(parts) > 0) != *f.hasAttachment {
			return false
		}
	}
	return true
}

// searchCriteria translates maildo's query into IMAP SEARCH keys. Words and
// different operators must all match (AND); repeated values of one operator,
// like from:a from:b, match any of them (OR). Dates widen by a day on the
// server, because IMAP compares dates in the server's time zone, and are then
// checked exactly against the arrival time.
func searchCriteria(q mail.Query) (goimap.SearchCriteria, matchFilter) {
	var criteria goimap.SearchCriteria
	var filter matchFilter
	criteria.Text = append(criteria.Text, q.Terms...)
	criteria.Text = append(criteria.Text, q.Phrases...)
	for _, field := range []struct {
		header string
		values []string
	}{{"From", q.From}, {"To", q.To}, {"Cc", q.CC}, {"Subject", q.Subject}} {
		if len(field.values) == 0 {
			continue
		}
		var alternatives []goimap.SearchCriteria
		for _, v := range field.values {
			alternatives = append(alternatives, goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: field.header, Value: v}}})
		}
		criteria.And(ptr(anyOf(alternatives)))
	}
	if q.After != nil {
		start := q.After.UTCStart()
		filter.after = &start
		criteria.Since = start.AddDate(0, 0, -1)
	}
	if q.Before != nil {
		end := q.Before.UTCStart()
		filter.before = &end
		criteria.Before = end.AddDate(0, 0, 1)
	}
	if q.Unread != nil {
		if *q.Unread {
			criteria.NotFlag = append(criteria.NotFlag, goimap.FlagSeen)
		} else {
			criteria.Flag = append(criteria.Flag, goimap.FlagSeen)
		}
	}
	if q.Flagged != nil {
		if *q.Flagged {
			criteria.Flag = append(criteria.Flag, goimap.FlagFlagged)
		} else {
			criteria.NotFlag = append(criteria.NotFlag, goimap.FlagFlagged)
		}
	}
	if q.HasAttachment != nil {
		filter.needsStructure = true
		filter.hasAttachment = q.HasAttachment
		if *q.HasAttachment {
			// Mail with attachments is always multipart, which the server can
			// check cheaply. The exact check uses BODYSTRUCTURE.
			criteria.Header = append(criteria.Header, goimap.SearchCriteriaHeaderField{Key: "Content-Type", Value: "multipart"})
		}
	}
	return criteria, filter
}

type pageCandidate struct {
	folder int
	uid    uint32
	date   time.Time
}

// collectPage runs the search in each folder, orders all matches newest first
// and returns rows offset..offset+limit with the total count. leftOut is true
// when matches were left out: a folder the server would not open, or one
// with more matches than maildo sorts.
func (c *accountClient) collectPage(ctx context.Context, session *imapSession, folders []imapFolderRecord, criteria goimap.SearchCriteria, filter matchFilter, offset, limit int, op errs.ProviderOperation) (out mail.MessageSummaryList, leftOut bool, err error) {
	var warnings []string
	var candidates []pageCandidate
	for i := range folders {
		folder := &folders[i]
		status, refused, err := c.examineForSearch(ctx, session, folder.mailbox, op)
		if err != nil {
			return mail.MessageSummaryList{}, false, err
		}
		if refused {
			leftOut = true
			warnings = append(warnings, fmt.Sprintf("left out folder %s, because the server would not open it", folderLabel(*folder)))
			continue
		}
		folder.uidValidity = status.uidValidity
		folder.uidNext = status.uidNext
		if status.total == 0 {
			continue
		}
		uids, err := c.uidSearch(ctx, session, criteria, op)
		if err != nil {
			return mail.MessageSummaryList{}, false, err
		}
		if len(uids) == 0 {
			continue
		}
		sort.Slice(uids, func(a, b int) bool { return uids[a] > uids[b] })
		if len(uids) > maxSortCandidates {
			leftOut = true
			warnings = append(warnings, fmt.Sprintf("%s has %d matches; maildo looked only at the newest %d", folderLabel(*folder), len(uids), maxSortCandidates))
			uids = uids[:maxSortCandidates]
		}
		dates, err := c.fetchDates(ctx, session, uids, filter.needsStructure, op)
		if err != nil {
			return mail.MessageSummaryList{}, false, err
		}
		for _, uid := range uids {
			d, ok := dates[uid]
			if !ok || !filter.match(d.date, d.structure) {
				continue
			}
			candidates = append(candidates, pageCandidate{folder: i, uid: uid, date: d.date})
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		if !x.date.Equal(y.date) {
			return x.date.After(y.date)
		}
		if x.folder != y.folder {
			return folders[x.folder].mailbox < folders[y.folder].mailbox
		}
		return x.uid > y.uid
	})
	total := len(candidates)
	out = mail.MessageSummaryList{Messages: []mail.MessageSummary{}, Total: total, TotalKnown: true, Warnings: warnings}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return out, leftOut, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := candidates[offset:end]

	// Fetch details folder by folder, then put rows back in page order.
	byFolder := map[int][]uint32{}
	var order []int
	for _, cand := range page {
		if _, seen := byFolder[cand.folder]; !seen {
			order = append(order, cand.folder)
		}
		byFolder[cand.folder] = append(byFolder[cand.folder], cand.uid)
	}
	type rowKey struct {
		folder int
		uid    uint32
	}
	fetched := map[int][]imapFetchedMessage{}
	for _, fi := range order {
		folder := folders[fi]
		if _, err := c.examine(ctx, session, folder.mailbox, op); err != nil {
			return mail.MessageSummaryList{}, false, err
		}
		msgs, skipped, err := c.fetchMessages(ctx, session, folder, byFolder[fi], false, op)
		if err != nil {
			return mail.MessageSummaryList{}, false, err
		}
		out.Warnings = append(out.Warnings, skipped...)
		fetched[fi] = msgs
	}
	// Previews come last, because they are optional. When a server answers
	// them badly, only the previews are missing.
	for _, fi := range order {
		_, err := c.examine(ctx, session, folders[fi].mailbox, op)
		if err == nil {
			err = c.fetchPreviews(ctx, session, fetched[fi], op)
		}
		if err != nil {
			out.Warnings = append(out.Warnings, "message previews are not available from this server")
			break
		}
	}
	rows := map[rowKey]mail.MessageSummary{}
	for fi, msgs := range fetched {
		for _, msg := range msgs {
			rows[rowKey{fi, msg.uid}] = c.toSummary(msg)
		}
	}
	for _, cand := range page {
		if row, ok := rows[rowKey{cand.folder, cand.uid}]; ok {
			out.Messages = append(out.Messages, row)
		}
	}
	return out, leftOut, nil
}

func ptr[T any](v T) *T { return &v }

func folderLabel(folder imapFolderRecord) string {
	if folder.folder.Path != "" {
		return folder.folder.Path
	}
	return folder.mailbox
}

// examineForSearch is examine for a folder search walks through. A folder
// the server refuses to open (it may have been deleted a moment ago) is
// left out instead of failing the whole search.
func (c *accountClient) examineForSearch(ctx context.Context, session *imapSession, mailbox string, op errs.ProviderOperation) (imapMailboxStatus, bool, error) {
	if current := session.currentSelected(); current.name == mailbox && current.status.known {
		return current.status, false, nil
	}
	session.forgetSelected()
	var data *goimap.SelectData
	refused := false
	obs, err := session.withObservedReadCommand(ctx, op, "EXAMINE", func(dep *imapclient.Client) error {
		var err error
		data, err = dep.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait()
		if isIMAPStatusNo(err) {
			refused = true
			return nil
		}
		return err
	})
	if err != nil {
		return imapMailboxStatus{}, false, err
	}
	if refused {
		return imapMailboxStatus{}, true, nil
	}
	status, err := c.mailboxStatusFromSelectData(data, obs, op)
	if err != nil {
		return imapMailboxStatus{}, false, err
	}
	session.rememberSelected(mailbox, true, status)
	return status, false, nil
}

// uidSearch runs UID SEARCH on the selected mailbox. With ESEARCH the server
// answers with compact UID ranges instead of one number per message.
func (c *accountClient) uidSearch(ctx context.Context, session *imapSession, criteria goimap.SearchCriteria, op errs.ProviderOperation) ([]uint32, error) {
	var data *goimap.SearchData
	err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
		var options *goimap.SearchOptions
		if caps := dep.Caps(); caps.Has(goimap.CapESearch) || caps.Has(goimap.CapIMAP4rev2) {
			options = &goimap.SearchOptions{ReturnAll: true}
		}
		var err error
		data, err = dep.UIDSearch(&criteria, options).Wait()
		var refused *goimap.Error
		if errors.As(err, &refused) && refused.Type == goimap.StatusResponseTypeBad {
			if simpler, changed := splitAddressWords(criteria); changed {
				data, err = dep.UIDSearch(&simpler, options).Wait()
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if data == nil || data.All == nil {
		return nil, nil
	}
	set, ok := data.All.(goimap.UIDSet)
	if !ok {
		return nil, c.providerDataError(op, errs.ProviderDataReasonInvalidProtocol)
	}
	var out []uint32
	for _, r := range set {
		if r.Start == 0 || r.Stop == 0 {
			continue
		}
		start, stop := uint32(r.Start), uint32(r.Stop)
		if start > stop {
			start, stop = stop, start
		}
		for uid := start; ; uid++ {
			out = append(out, uid)
			if uid == stop {
				break
			}
		}
	}
	return out, nil
}

// splitAddressWords rewrites FROM, TO, CC and BCC keys whose value has
// spaces into one key per word. Some servers (GreenMail among them) refuse
// FROM "Clara Client" with BAD but accept FROM Clara FROM Client, which
// matches the same messages in nearly every case.
func splitAddressWords(c goimap.SearchCriteria) (goimap.SearchCriteria, bool) {
	changed := false
	var header []goimap.SearchCriteriaHeaderField
	for _, kv := range c.Header {
		switch strings.ToUpper(kv.Key) {
		case "FROM", "TO", "CC", "BCC":
			if words := strings.Fields(kv.Value); len(words) > 1 {
				for _, w := range words {
					header = append(header, goimap.SearchCriteriaHeaderField{Key: kv.Key, Value: w})
				}
				changed = true
				continue
			}
		}
		header = append(header, kv)
	}
	c.Header = header
	if len(c.Not) > 0 {
		not := make([]goimap.SearchCriteria, len(c.Not))
		for i, n := range c.Not {
			var ch bool
			not[i], ch = splitAddressWords(n)
			changed = changed || ch
		}
		c.Not = not
	}
	if len(c.Or) > 0 {
		or := make([][2]goimap.SearchCriteria, len(c.Or))
		for i, pair := range c.Or {
			var a, b bool
			or[i][0], a = splitAddressWords(pair[0])
			or[i][1], b = splitAddressWords(pair[1])
			changed = changed || a || b
		}
		c.Or = or
	}
	return c, changed
}

type uidDate struct {
	date      time.Time
	structure goimap.BodyStructure
}

// fetchDates learns when each UID arrived, in large batches. Messages that
// vanished in the meantime are missing from the result.
func (c *accountClient) fetchDates(ctx context.Context, session *imapSession, uids []uint32, withStructure bool, op errs.ProviderOperation) (map[uint32]uidDate, error) {
	out := make(map[uint32]uidDate, len(uids))
	batch := dateBatch
	if withStructure {
		batch = fetchBatch
	}
	for start := 0; start < len(uids); start += batch {
		end := start + batch
		if end > len(uids) {
			end = len(uids)
		}
		options := &goimap.FetchOptions{UID: true, InternalDate: true}
		if withStructure {
			options.BodyStructure = &goimap.FetchItemBodyStructure{Extended: true}
		}
		var msgs []*imapclient.FetchMessageBuffer
		err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
			var err error
			msgs, err = dep.Fetch(uidSetOf(uids[start:end]), options).Collect()
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if msg == nil || msg.UID == 0 {
				continue
			}
			out[uint32(msg.UID)] = uidDate{date: msg.InternalDate, structure: msg.BodyStructure}
		}
	}
	return out, nil
}

func uidSetOf(uids []uint32) goimap.UIDSet {
	var set goimap.UIDSet
	for _, uid := range uids {
		set.AddNum(goimap.UID(uid))
	}
	return set
}

// fetchMessages fetches the row details for uids in the selected folder: flags,
// dates, size, envelope, structure and the thread headers. With preview set it
// also fetches the start of each message's text for the preview line. A
// message whose headers or structure cannot be understood still becomes a row;
// skipped lists the ones the server returned nothing usable for.
func (c *accountClient) fetchMessages(ctx context.Context, session *imapSession, folder imapFolderRecord, uids []uint32, preview bool, op errs.ProviderOperation) ([]imapFetchedMessage, []string, error) {
	header := &goimap.FetchItemBodySection{Specifier: goimap.PartSpecifierHeader, HeaderFields: threadHeaderFields, Peek: true}
	options := &goimap.FetchOptions{
		UID:           true,
		Flags:         true,
		InternalDate:  true,
		RFC822Size:    true,
		Envelope:      true,
		BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*goimap.FetchItemBodySection{header},
	}
	byUID := map[uint32]*imapclient.FetchMessageBuffer{}
	for start := 0; start < len(uids); start += fetchBatch {
		end := start + fetchBatch
		if end > len(uids) {
			end = len(uids)
		}
		var msgs []*imapclient.FetchMessageBuffer
		err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
			var err error
			msgs, err = dep.Fetch(uidSetOf(uids[start:end]), options).Collect()
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		for _, msg := range msgs {
			if msg != nil && msg.UID != 0 {
				byUID[uint32(msg.UID)] = msg
			}
		}
	}
	whole, err := c.refetchHeaders(ctx, session, byUID, header, op)
	if err != nil {
		return nil, nil, err
	}
	var out []imapFetchedMessage
	var skipped []string
	for _, uid := range uids {
		buf, ok := byUID[uid]
		if !ok {
			continue
		}
		raw := buf.FindBodySection(header)
		if fields, ok := whole[uid]; ok {
			raw = fields
		}
		msg, err := c.fetchedMessage(folder, uid, buf, raw)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("left out message %d in %s: %v", uid, folderLabel(folder), err))
			continue
		}
		out = append(out, msg)
	}
	if preview && len(out) > 0 {
		if err := c.fetchPreviews(ctx, session, out, op); err != nil {
			return nil, nil, err
		}
	}
	return out, skipped, nil
}

// refetchHeaders works around servers, GreenMail among them, that send
// nothing when maildo asks for the thread header fields by name. A message
// with a Message-ID in its envelope but no header fields gets its whole
// header fetched, and maildo keeps those fields from it. Without them, a
// thread would miss the replies to a message.
func (c *accountClient) refetchHeaders(ctx context.Context, session *imapSession, byUID map[uint32]*imapclient.FetchMessageBuffer, fields *goimap.FetchItemBodySection, op errs.ProviderOperation) (map[uint32][]byte, error) {
	var uids []uint32
	for uid, buf := range byUID {
		if len(bytes.TrimSpace(buf.FindBodySection(fields))) == 0 && buf.Envelope != nil && strings.TrimSpace(buf.Envelope.MessageID) != "" {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	whole := &goimap.FetchItemBodySection{Specifier: goimap.PartSpecifierHeader, Peek: true}
	out := map[uint32][]byte{}
	for start := 0; start < len(uids); start += fetchBatch {
		end := min(start+fetchBatch, len(uids))
		var msgs []*imapclient.FetchMessageBuffer
		err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
			var err error
			msgs, err = dep.Fetch(uidSetOf(uids[start:end]), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{whole}}).Collect()
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if msg != nil && msg.UID != 0 {
				out[uint32(msg.UID)] = headerFields(msg.FindBodySection(whole), fields.HeaderFields)
			}
		}
	}
	return out, nil
}

// headerFields keeps only the named fields of a header block, with their
// folded continuation lines.
func headerFields(raw []byte, names []string) []byte {
	var out bytes.Buffer
	keep := false
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		if line[0] != ' ' && line[0] != '\t' {
			name, _, _ := strings.Cut(line, ":")
			keep = false
			for _, want := range names {
				if strings.EqualFold(strings.TrimSpace(name), want) {
					keep = true
				}
			}
		}
		if keep {
			out.WriteString(line)
		}
	}
	return out.Bytes()
}

// fetchedMessage turns one FETCH answer into maildo's message record.
func (c *accountClient) fetchedMessage(folder imapFolderRecord, uid uint32, buf *imapclient.FetchMessageBuffer, rawHeader []byte) (imapFetchedMessage, error) {
	id, err := formatMessageID(folder.mailbox, folder.uidValidity, uid)
	if err != nil {
		return imapFetchedMessage{}, err
	}
	header := parseHeaderBlockLenient(rawHeader)
	internetID := header.messageID
	if internetID == "" && buf.Envelope != nil && strings.TrimSpace(buf.Envelope.MessageID) != "" {
		if normalized, err := mail.NormalizeMessageIDScalar(buf.Envelope.MessageID); err == nil {
			internetID = normalized
		}
	}
	threadID := ""
	if internetID != "" {
		threadID, _ = formatThreadID(rootMessageID(header, internetID))
	} else {
		threadID, _ = formatSingletonThreadID(id)
	}
	msg := imapFetchedMessage{
		account:           c.account,
		provider:          c.provider,
		folder:            mail.SummaryFrom(folder.folder),
		mailbox:           folder.mailbox,
		uidValidity:       folder.uidValidity,
		uid:               uid,
		id:                id,
		internetMessageID: internetID,
		threadID:          threadID,
		envelope:          buf.Envelope,
		flags:             buf.Flags,
		rfc822Size:        buf.RFC822Size,
		bodyStructure:     buf.BodyStructure,
		header:            header,
	}
	if !buf.InternalDate.IsZero() {
		t := buf.InternalDate
		msg.internalDate = &t
	}
	return msg, nil
}

// fetchPreviews fetches the start of each message's main text part for the
// preview line. Messages are grouped by part number so one FETCH serves many.
// Small parts are fetched whole; for large ones only the first previewBytes
// are asked for, unless the server has shown it gets such partial fetches
// wrong (GreenMail answers BODY[1]<0>{n} without the required space).
func (c *accountClient) fetchPreviews(ctx context.Context, session *imapSession, msgs []imapFetchedMessage, op errs.ProviderOperation) error {
	type group struct {
		path    []uint32
		partial bool
		members []int
	}
	groups := map[string]*group{}
	var keys []string
	parts := map[int]*imapBodyPart{}
	for i := range msgs {
		part, err := preferredBodyPart(msgs[i].bodyStructure, mail.BodyModeReadable)
		if err != nil || part == nil {
			continue
		}
		partial := part.encodedSize > wholePreviewPartBytes
		if partial && c.partialFetchBroken.Load() {
			continue
		}
		parts[i] = part
		key := fmt.Sprintf("%s/%t", pathString(part.path), partial)
		g, ok := groups[key]
		if !ok {
			g = &group{path: part.path, partial: partial}
			groups[key] = g
			keys = append(keys, key)
		}
		g.members = append(g.members, i)
	}
	for _, key := range keys {
		g := groups[key]
		section := &goimap.FetchItemBodySection{Part: uintPathToInt(g.path), Peek: true}
		if g.partial {
			section.Partial = &goimap.SectionPartial{Offset: 0, Size: previewBytes}
		}
		uids := make([]uint32, 0, len(g.members))
		for _, i := range g.members {
			uids = append(uids, msgs[i].uid)
		}
		var bufs []*imapclient.FetchMessageBuffer
		err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
			var err error
			bufs, err = dep.Fetch(uidSetOf(uids), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
			return err
		})
		if err != nil {
			if g.partial && errorCode(err) == errs.CodeProviderDataError {
				c.partialFetchBroken.Store(true)
			}
			return err
		}
		byUID := map[uint32][]byte{}
		for _, buf := range bufs {
			if buf != nil {
				byUID[uint32(buf.UID)] = buf.FindBodySection(section)
			}
		}
		for _, i := range g.members {
			if raw, ok := byUID[msgs[i].uid]; ok {
				if len(raw) > previewBytes {
					raw = raw[:previewBytes]
				}
				msgs[i].preview = previewText(raw, parts[i])
			}
		}
	}
	return nil
}

// parseHeaderBlockLenient reads a header block and keeps going past broken
// lines. It never fails; a header it cannot parse gives no ids.
func parseHeaderBlockLenient(raw []byte) imapHeaderBlock {
	header := imapHeaderBlock{raw: append([]byte(nil), raw...), completeRefs: true}
	values, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if err != nil {
		values = lenientMIMEHeader(raw)
	}
	header.values = values
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	decoder := wordDecoder()
	for _, name := range names {
		for _, value := range values[name] {
			decoded, err := decoder.DecodeHeader(value)
			if err != nil {
				decoded = value
			}
			header.visible = append(header.visible, mail.Header{Name: name, Value: strings.ToValidUTF8(decoded, "�")})
		}
	}
	if vals := values.Values("Message-Id"); len(vals) >= 1 {
		if id, err := mail.NormalizeMessageIDScalar(vals[0]); err == nil {
			header.messageID = id
		}
		if len(vals) > 1 {
			header.completeRefs = false
		}
	}
	if vals := values.Values("References"); len(vals) >= 1 {
		if ids, err := mail.ParseMessageIDTokens(strings.Join(vals, " ")); err == nil {
			header.references = ids
		} else {
			header.completeRefs = false
		}
	}
	if vals := values.Values("In-Reply-To"); len(vals) >= 1 {
		if ids, err := mail.ParseMessageIDTokens(vals[0]); err == nil && len(ids) >= 1 {
			header.inReplyTo = ids[:1]
		} else {
			header.completeRefs = false
		}
	}
	return header
}

// lenientMIMEHeader parses "Name: value" lines one by one, joins folded
// continuation lines, and drops lines that are not headers.
func lenientMIMEHeader(raw []byte) textproto.MIMEHeader {
	out := textproto.MIMEHeader{}
	lastKey := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && lastKey != "" {
			vals := out[lastKey]
			vals[len(vals)-1] += " " + strings.TrimSpace(line)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			lastKey = ""
			continue
		}
		lastKey = textproto.CanonicalMIMEHeaderKey(name)
		out[lastKey] = append(out[lastKey], strings.TrimSpace(value))
	}
	return out
}

func rootMessageID(header imapHeaderBlock, own string) string {
	if len(header.references) > 0 {
		return header.references[0]
	}
	if len(header.inReplyTo) == 1 {
		return header.inReplyTo[0]
	}
	return own
}

func (c *accountClient) toSummary(msg imapFetchedMessage) mail.MessageSummary {
	parts, _ := attachmentParts(msg.bodyStructure)
	flagged := hasFlag(msg.flags, goimap.FlagFlagged)
	meta, _ := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataMessageSummary, mail.SummaryProviderMetadata{
		Source:  mail.MetadataSourceIMAP,
		Flagged: &flagged,
	})
	summary := mail.MessageSummary{
		Account:           c.account,
		Provider:          c.provider,
		ID:                msg.id,
		InternetMessageID: msg.internetMessageID,
		ThreadID:          msg.threadID,
		Folder:            msg.folder,
		Subject:           envelopeSubject(msg.envelope),
		IsRead:            hasFlag(msg.flags, goimap.FlagSeen),
		Flagged:           flagged,
		HasAttachments:    len(parts) > 0,
		Preview:           msg.preview,
		ProviderMeta:      meta,
		SortGroup:         msg.mailbox,
		SortOrdinal:       uint64(msg.uid),
	}
	if msg.internalDate != nil {
		t := *msg.internalDate
		summary.ReceivedAt = &t
	}
	if msg.envelope != nil && !msg.envelope.Date.IsZero() {
		t := msg.envelope.Date
		summary.SentAt = &t
	}
	if from := envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.From }); len(from) > 0 {
		summary.From = from[0]
	}
	summary.ToCount = len(envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.To }))
	summary.CCCount = len(envelopeRecipients(msg.envelope, func(e *goimap.Envelope) []goimap.Address { return e.Cc }))
	return summary
}

func envelopeSubject(envelope *goimap.Envelope) string {
	if envelope == nil {
		return ""
	}
	return strings.ToValidUTF8(envelope.Subject, "�")
}

func envelopeRecipients(envelope *goimap.Envelope, selectAddrs func(*goimap.Envelope) []goimap.Address) []mail.Recipient {
	if envelope == nil {
		return nil
	}
	var out []mail.Recipient
	for _, addr := range selectAddrs(envelope) {
		if addr.IsGroupStart() || addr.IsGroupEnd() {
			continue
		}
		email := addr.Addr()
		if email == "" {
			continue
		}
		out = append(out, mail.Recipient{Name: strings.ToValidUTF8(addr.Name, "�"), Address: email})
	}
	return out
}

func hasFlag(flags []goimap.Flag, want goimap.Flag) bool {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(want)) {
			return true
		}
	}
	return false
}

// imapFetchProjection names the items one FETCH asks for. Commands that
// change mail use it to read back what they need to check a write.
type imapFetchProjection struct {
	envelope      bool
	flags         bool
	internalDate  bool
	rfc822Size    bool
	bodyStructure bool
	header        bool
	headerFields  []string
}

// fetchMessageRecord fetches one message from the selected folder. ok is
// false when the UID no longer exists.
func (c *accountClient) fetchMessageRecord(ctx context.Context, session *imapSession, folder imapFolderRecord, uid uint32, op errs.ProviderOperation, projection imapFetchProjection) (imapFetchedMessage, bool, error) {
	options := &goimap.FetchOptions{
		UID:          true,
		Envelope:     projection.envelope,
		Flags:        projection.flags,
		InternalDate: projection.internalDate,
		RFC822Size:   projection.rfc822Size,
	}
	if projection.bodyStructure {
		options.BodyStructure = &goimap.FetchItemBodyStructure{Extended: true}
	}
	var headerSection *goimap.FetchItemBodySection
	if projection.header {
		headerSection = &goimap.FetchItemBodySection{Specifier: goimap.PartSpecifierHeader, HeaderFields: projection.headerFields, Peek: true}
		options.BodySection = append(options.BodySection, headerSection)
	}
	buffer, ok, err := c.fetchOne(ctx, session, uid, options, op)
	if err != nil || !ok {
		return imapFetchedMessage{}, ok, err
	}
	var rawHeader []byte
	if headerSection != nil {
		rawHeader = buffer.FindBodySection(headerSection)
	}
	msg, err := c.fetchedMessage(folder, uid, buffer, rawHeader)
	if err != nil {
		return imapFetchedMessage{}, false, c.providerDataError(op, errs.ProviderDataReasonInvalidMailboxEncoding)
	}
	return msg, true, nil
}

// fetchOne runs one UID FETCH for a single UID in the selected folder.
func (c *accountClient) fetchOne(ctx context.Context, session *imapSession, uid uint32, options *goimap.FetchOptions, op errs.ProviderOperation) (*imapclient.FetchMessageBuffer, bool, error) {
	var messages []*imapclient.FetchMessageBuffer
	err := session.withReadCommand(ctx, op, func(dep *imapclient.Client) error {
		var err error
		messages, err = dep.Fetch(goimap.UIDSetNum(goimap.UID(uid)), options).Collect()
		return err
	})
	if err != nil {
		return nil, false, err
	}
	for _, msg := range messages {
		if msg != nil && uint32(msg.UID) == uid {
			return msg, true, nil
		}
	}
	return nil, false, nil
}
