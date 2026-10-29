package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

const messagePageSize = 100

// messageSelect lists the fields Graph sends for each row of list and search.
// The body is not among them; read and thread ask for it.
var messageSelect = strings.Join([]string{
	"id",
	"internetMessageId",
	"conversationId",
	"parentFolderId",
	"subject",
	"from",
	"toRecipients",
	"ccRecipients",
	"receivedDateTime",
	"sentDateTime",
	"isRead",
	"importance",
	"hasAttachments",
	"bodyPreview",
	"flag",
}, ",")

type graphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphMessage struct {
	ID                string           `json:"id"`
	InternetMessageID string           `json:"internetMessageId"`
	ConversationID    string           `json:"conversationId"`
	ParentFolderID    string           `json:"parentFolderId"`
	Subject           string           `json:"subject"`
	From              *graphRecipient  `json:"from"`
	ToRecipients      []graphRecipient `json:"toRecipients"`
	CcRecipients      []graphRecipient `json:"ccRecipients"`
	ReceivedDateTime  string           `json:"receivedDateTime"`
	SentDateTime      string           `json:"sentDateTime"`
	IsRead            bool             `json:"isRead"`
	Importance        string           `json:"importance"`
	HasAttachments    bool             `json:"hasAttachments"`
	BodyPreview       string           `json:"bodyPreview"`
	Flag              *struct {
		FlagStatus string `json:"flagStatus"`
	} `json:"flag"`
}

type graphMessagePage struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

// ListMessages returns the newest messages of one folder. The command has
// already turned the folder name into a Graph folder id.
func (c *Client) ListMessages(ctx context.Context, req mail.ListMessagesRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	if req.Offset > 0 {
		// Also fetch the rows before the page and drop them, so list and
		// search page the same way.
		inner := req
		inner.Limit, inner.Offset = req.Offset+req.Limit, 0
		out, err := c.ListMessages(ctx, inner)
		out.Messages = mail.DropRows(out.Messages, req.Offset)
		return out, err
	}
	base := c.folderMessagesURL(req.Folder.ID)
	values := baseMessageValues(req.Limit)
	values.Set("$orderby", "receivedDateTime desc")
	if req.Filter.Unread {
		values.Set("$filter", buildListFilter(req.Filter))
	}
	rows, err := c.drainFilter(ctx, withQuery(base, values), req.Limit, nil)
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	out := make([]mail.MessageSummary, 0, len(rows))
	folder := mail.SummaryFrom(req.Folder)
	for _, g := range rows {
		m := c.toSummary(g)
		m.Folder = folder
		out = append(out, m)
	}
	return mail.MessageSummaryList{Messages: out}, nil
}

// SearchMessages searches the whole mailbox or one folder. Graph cannot
// combine $search with $filter or $orderby. A query that looks for text
// (words, phrases or the from:, to:, cc: and subject: operators) goes to
// $search, and maildo applies the other conditions itself after it has read
// all result pages. Any other query goes to $filter alone.
//
// Without a folder, the search covers /me/messages, which holds every
// folder, and leaves out rows from Deleted Items, Junk Email and the roles
// in SkipRoles by their parent folder. OnlyRoles searches each of those
// folders on its own instead.
func (c *Client) SearchMessages(ctx context.Context, req mail.SearchRequest) (mail.MessageSummaryList, error) {
	if req.Limit <= 0 {
		return mail.MessageSummaryList{Messages: []mail.MessageSummary{}}, nil
	}
	if req.Offset > 0 {
		inner := req
		inner.Limit, inner.Offset = req.Offset+req.Limit, 0
		out, err := c.SearchMessages(ctx, inner)
		out.Messages = mail.DropRows(out.Messages, req.Offset)
		return out, err
	}
	if req.Folder != nil {
		rows, warnings, err := c.searchIn(ctx, c.folderMessagesURL(req.Folder.ID), req, nil)
		if err != nil {
			return mail.MessageSummaryList{}, err
		}
		for i := range rows {
			rows[i].Folder = mail.SummaryFrom(*req.Folder)
		}
		return mail.MessageSummaryList{Messages: rows, Warnings: warnings}, nil
	}
	scope, err := c.newSearchScope(ctx, req.SkipRoles, req.OnlyRoles)
	if err != nil {
		return mail.MessageSummaryList{}, err
	}
	var out mail.MessageSummaryList
	if len(req.OnlyRoles) > 0 {
		for _, id := range scope.only {
			rows, warnings, err := c.searchIn(ctx, c.folderMessagesURL(id), req, nil)
			if err != nil {
				return mail.MessageSummaryList{}, err
			}
			out.Messages = append(out.Messages, rows...)
			out.Warnings = append(out.Warnings, warnings...)
		}
		sortMessagesLocal(out.Messages)
		if len(out.Messages) > req.Limit {
			out.Messages = out.Messages[:req.Limit]
		}
	} else {
		out.Messages, out.Warnings, err = c.searchIn(ctx, c.graphBaseURL+"/me/messages", req, scope.leaveOut)
		if err != nil {
			return mail.MessageSummaryList{}, err
		}
	}
	if out.Messages == nil {
		out.Messages = []mail.MessageSummary{}
	}
	scope.nameFolders(ctx, c, out.Messages)
	return out, nil
}

// searchIn runs one search against a messages collection and leaves out
// rows whose parent folder is in leaveOut. Rows carry only their folder id.
func (c *Client) searchIn(ctx context.Context, base string, req mail.SearchRequest, leaveOut map[string]bool) ([]mail.MessageSummary, []string, error) {
	keep := func(g graphMessage) bool { return !leaveOut[g.ParentFolderID] }
	if req.Query.IsSearchStyle() {
		kql, err := buildSearchKQL(req.Query)
		if err != nil {
			return nil, nil, c.unsupportedQuery(err.Error())
		}
		values := baseMessageValues(req.Limit)
		values.Set("$search", kql)
		// maildo filters and sorts the results itself afterwards, so it reads
		// all of them. Stopping at the limit could drop rows that belong in the
		// result.
		rows, capped, err := c.drainSearch(ctx, withQuery(base, values))
		if err != nil {
			return nil, nil, err
		}
		var warnings []string
		if capped {
			if req.Complete {
				return nil, nil, c.searchCapError()
			}
			warnings = append(warnings, fmt.Sprintf("Outlook returns at most %d search results, so older matches may be missing. Narrow the search, for example with newer:30d or --folder", c.searchResultCap))
		}
		out := c.summaries(keepRows(rows, keep))
		out = applyLocalPostFilter(out, req.Query)
		sortMessagesLocal(out)
		if len(out) > req.Limit {
			out = out[:req.Limit]
		}
		return out, warnings, nil
	}

	// Graph runs a query without text exactly and sorts it, so maildo stops
	// reading as soon as it has enough rows.
	values := baseMessageValues(req.Limit)
	values.Set("$filter", buildQueryFilter(req.Query))
	values.Set("$orderby", "receivedDateTime desc")
	rows, err := c.drainFilter(ctx, withQuery(base, values), req.Limit, keep)
	if err != nil {
		return nil, nil, err
	}
	return c.summaries(rows), nil, nil
}

func keepRows(rows []graphMessage, keep func(graphMessage) bool) []graphMessage {
	out := rows[:0]
	for _, g := range rows {
		if keep(g) {
			out = append(out, g)
		}
	}
	return out
}

// searchCapError is returned when a command needs every match and Outlook
// may have cut the results at its search cap.
func (c *Client) searchCapError() error {
	return errs.NewWithDetails(errs.CodeReadLimitExceeded,
		fmt.Sprintf("Outlook returns at most %d search results, so maildo cannot be sure it found every match", c.searchResultCap),
		map[string]any{
			"account":     c.account,
			"provider":    "outlook",
			"reason":      "search_cap",
			"limit":       c.searchResultCap,
			"remediation": "narrow the search, for example with newer:30d or from:, or name a folder with --folder",
		})
}

func (c *Client) folderMessagesURL(folderID string) string {
	return c.graphBaseURL + "/me/mailFolders/" + url.PathEscape(folderID) + "/messages"
}

func baseMessageValues(limit int) url.Values {
	values := url.Values{}
	values.Set("$select", messageSelect)
	values.Set("$top", fmt.Sprintf("%d", min(limit, messagePageSize)))
	return values
}

func withQuery(base string, values url.Values) string {
	return base + "?" + values.Encode()
}

func buildListFilter(filter mail.ListFilter) string {
	if !filter.Unread {
		return ""
	}
	return strings.Join([]string{dateAnchorFilter(), "isRead eq false"}, " and ")
}

// buildQueryFilter writes the conditions in the order Graph expects when it
// sorts by receivedDateTime desc: the date comes first. Without a date in the
// query, a date far in the past takes its place, so attachment and unread
// filters can still be sorted.
func buildQueryFilter(q mail.Query) string {
	parts := make([]string, 0, 4)
	if q.After != nil {
		parts = append(parts, "receivedDateTime ge "+odataDate(q.After.UTCStart()))
	}
	if q.Before != nil {
		parts = append(parts, "receivedDateTime lt "+odataDate(q.Before.UTCStart()))
	}
	if q.After == nil && q.Before == nil {
		parts = append(parts, dateAnchorFilter())
	}
	if q.HasAttachment != nil {
		value := "false"
		if *q.HasAttachment {
			value = "true"
		}
		parts = append(parts, "hasAttachments eq "+value)
	}
	if q.Unread != nil {
		parts = append(parts, fmt.Sprintf("isRead eq %t", !*q.Unread))
	}
	if q.Flagged != nil {
		if *q.Flagged {
			parts = append(parts, "flag/flagStatus eq 'flagged'")
		} else {
			parts = append(parts, "flag/flagStatus ne 'flagged'")
		}
	}
	return strings.Join(parts, " and ")
}

func dateAnchorFilter() string {
	return "receivedDateTime ge 1900-01-01T00:00:00Z"
}

func odataDate(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// buildSearchKQL turns maildo's search into Graph's KQL. Several values of
// the same operator are joined with OR, different operators with AND, which
// is what maildo's search language means on every provider.
func buildSearchKQL(q mail.Query) (string, error) {
	clauses := make([]string, 0)
	addField := func(field string, values []string) error {
		if len(values) == 0 {
			return nil
		}
		ors := make([]string, 0, len(values))
		for _, value := range values {
			quoted, err := kqlQuoted(value)
			if err != nil {
				return err
			}
			ors = append(ors, field+":"+quoted)
		}
		if len(ors) == 1 {
			clauses = append(clauses, ors[0])
			return nil
		}
		clauses = append(clauses, "("+strings.Join(ors, " OR ")+")")
		return nil
	}
	for _, spec := range []struct {
		field  string
		values []string
	}{
		{field: "from", values: q.From},
		{field: "to", values: q.To},
		{field: "cc", values: q.CC},
		{field: "subject", values: q.Subject},
	} {
		if err := addField(spec.field, spec.values); err != nil {
			return "", err
		}
	}
	for _, term := range q.Terms {
		quoted, err := kqlQuoted(term)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, quoted)
	}
	for _, phrase := range q.Phrases {
		quoted, err := kqlQuoted(phrase)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, quoted)
	}
	if len(clauses) == 0 {
		return "", errs.New(errs.CodeInvalidQuery, "search query has no search predicate")
	}
	return `"` + strings.Join(clauses, " AND ") + `"`, nil
}

func kqlQuoted(value string) (string, error) {
	if strings.ContainsAny(value, "\"\\") {
		return "", errs.New(errs.CodeUnsupportedQueryOperator, "query value cannot be preserved exactly for Outlook search")
	}
	// Graph wants the whole $search value in double quotes, so the quotes
	// around each KQL value are escaped.
	return `\"` + value + `\"`, nil
}

// graphMessageGet reads one page of messages, with a size limit. Its errors
// name only the account. They never show the request URL, the token or what
// Graph sent.
func (c *Client) graphMessageGet(ctx context.Context, target string, out *graphMessagePage) error {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return err
	}
	if !c.validGraphURL(target) {
		return c.messagesError()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return rungrad.NewError(rungrad.ExitAPI, "could not prepare an Outlook message request")
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := c.doGraphRequestNoRedirect(req)
	if err != nil {
		return c.messagesError()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return repairRequired(c.account)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.messagesError()
	}
	if resp.StatusCode == http.StatusBadRequest {
		// InefficientFilter means Graph will not run this query as maildo
		// wrote it. Report that as a search Outlook cannot do, not as a
		// general error.
		if graphErrorCode(body) == "InefficientFilter" {
			return c.unsupportedQuery("Outlook rejected this filter as inefficient")
		}
		return c.messagesError()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.messagesError()
	}
	if err := json.Unmarshal(body, out); err != nil {
		return c.messagesError()
	}
	return nil
}

// graphErrorCode reads Graph's error code from an error answer. The rest of
// the answer is never shown, because it can contain request details.
func graphErrorCode(body []byte) string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Error.Code
}

// drainFilter reads Graph pages until it has limit rows that keep accepts.
// It can stop early because Graph sorts these results itself. A nil keep
// accepts every row.
func (c *Client) drainFilter(ctx context.Context, target string, limit int, keep func(graphMessage) bool) ([]graphMessage, error) {
	out := make([]graphMessage, 0)
	next := target
	var pages graphPageGuard
	for next != "" && len(out) < limit {
		if err := pages.visit(ctx, c, next); err != nil {
			return nil, err
		}
		var page graphMessagePage
		if err := c.graphMessageGet(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, g := range page.Value {
			if keep == nil || keep(g) {
				out = append(out, g)
			}
		}
		next = page.NextLink
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// drainSearch reads every page of a search, so maildo's own date and
// attachment filters and its sorting see all results. Graph stops a $search
// at searchResultCap results, often without saying so. Reaching the cap sets
// capped, because matches past it may exist.
func (c *Client) drainSearch(ctx context.Context, target string) (rows []graphMessage, capped bool, err error) {
	out := make([]graphMessage, 0)
	next := target
	var pages graphPageGuard
	for next != "" {
		if err := pages.visit(ctx, c, next); err != nil {
			return nil, false, err
		}
		var page graphMessagePage
		if err := c.graphMessageGet(ctx, next, &page); err != nil {
			return nil, false, err
		}
		out = append(out, page.Value...)
		next = page.NextLink
		if len(out) >= c.searchResultCap {
			return out, true, nil
		}
	}
	return out, false, nil
}

// summaries turns Graph rows into maildo's message rows. Each row has only
// the folder id; SearchMessages fills in the folder names.
func (c *Client) summaries(rows []graphMessage) []mail.MessageSummary {
	out := make([]mail.MessageSummary, 0, len(rows))
	for _, g := range rows {
		m := c.toSummary(g)
		m.Folder = mail.FolderSummary{ID: g.ParentFolderID}
		out = append(out, m)
	}
	return out
}

// toSummary copies the fields a list or search row shows. The body and the
// headers are left to read and thread.
func (c *Client) toSummary(g graphMessage) mail.MessageSummary {
	m := mail.MessageSummary{
		Account:           c.account,
		Provider:          "outlook",
		ID:                g.ID,
		InternetMessageID: g.InternetMessageID,
		ThreadID:          g.ConversationID,
		ToCount:           len(g.ToRecipients),
		CCCount:           len(g.CcRecipients),
		Subject:           g.Subject,
		IsRead:            g.IsRead,
		Importance:        g.Importance,
		HasAttachments:    g.HasAttachments,
		Preview:           g.BodyPreview,
		Flagged:           g.Flag != nil && g.Flag.FlagStatus == "flagged",
		ProviderMeta:      graphSummaryMeta(),
	}
	if g.From != nil {
		m.From = mail.Recipient{Name: g.From.EmailAddress.Name, Address: g.From.EmailAddress.Address}
	}
	if t, ok := parseGraphTime(g.ReceivedDateTime); ok {
		m.ReceivedAt = &t
	}
	if t, ok := parseGraphTime(g.SentDateTime); ok {
		m.SentAt = &t
	}
	return m
}

func parseGraphTime(raw string) (time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// applyLocalPostFilter applies the conditions Graph cannot combine with
// $search, such as dates. It runs after drainSearch has read all pages.
func applyLocalPostFilter(rows []mail.MessageSummary, q mail.Query) []mail.MessageSummary {
	out := make([]mail.MessageSummary, 0, len(rows))
	for _, row := range rows {
		if q.After != nil || q.Before != nil {
			if row.ReceivedAt == nil {
				continue
			}
			if q.After != nil && row.ReceivedAt.Before(q.After.UTCStart()) {
				continue
			}
			if q.Before != nil && !row.ReceivedAt.Before(q.Before.UTCStart()) {
				continue
			}
		}
		if q.HasAttachment != nil && row.HasAttachments != *q.HasAttachment {
			continue
		}
		if q.Unread != nil && row.IsRead == *q.Unread {
			continue
		}
		if q.Flagged != nil && row.Flagged != *q.Flagged {
			continue
		}
		out = append(out, row)
	}
	return out
}

// sortMessagesLocal sorts search results the way maildo sorts them on every
// provider.
func sortMessagesLocal(rows []mail.MessageSummary) {
	sort.SliceStable(rows, func(i, j int) bool {
		return mail.MessageSummaryLess(rows[i], rows[j])
	})
}

func (c *Client) messagesError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not search or list messages for account %q (outlook)", c.account))
}

func (c *Client) unsupportedQuery(detail string) error {
	return errs.NewWithDetails(errs.CodeUnsupportedQueryOperator,
		fmt.Sprintf("Outlook cannot run this search for account %q", c.account),
		map[string]any{
			"account":  c.account,
			"provider": "outlook",
			"reason":   detail,
		})
}
