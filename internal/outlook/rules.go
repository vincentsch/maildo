package outlook

import (
	"bytes"
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
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

const rulePageSize = 100

type graphRulePage struct {
	Value    []graphRule `json:"value"`
	NextLink string      `json:"@odata.nextLink"`
}

// graphRule is Graph's messageRule. Fields maildo does not know are noted,
// so they can be reported, but their raw JSON is not kept.
type graphRule struct {
	ID          string                      `json:"id"`
	DisplayName string                      `json:"displayName"`
	Sequence    int                         `json:"sequence"`
	IsEnabled   bool                        `json:"isEnabled"`
	IsReadOnly  bool                        `json:"isReadOnly"`
	HasError    bool                        `json:"hasError"`
	Conditions  graphRulePredicates         `json:"conditions"`
	Exceptions  graphRulePredicates         `json:"exceptions"`
	Actions     graphRuleActions            `json:"actions"`
	Unknown     []mail.RuleUnsupportedField `json:"-"`
	RawState    map[string]any              `json:"-"`
}

// graphRulePredicates has the conditions maildo can show and edit, and the
// other documented Graph conditions. maildo keeps those unchanged and lists
// them as unsupported.
type graphRulePredicates struct {
	BodyContains           []string                    `json:"bodyContains"`
	BodyOrSubjectContains  []string                    `json:"bodyOrSubjectContains"`
	Categories             []string                    `json:"categories"`
	FromAddresses          []graphRecipient            `json:"fromAddresses"`
	HasAttachments         *bool                       `json:"hasAttachments"`
	HeaderContains         []string                    `json:"headerContains"`
	Importance             string                      `json:"importance"`
	IsApprovalRequest      *bool                       `json:"isApprovalRequest"`
	IsAutomaticForward     *bool                       `json:"isAutomaticForward"`
	IsAutomaticReply       *bool                       `json:"isAutomaticReply"`
	IsEncrypted            *bool                       `json:"isEncrypted"`
	IsMeetingRequest       *bool                       `json:"isMeetingRequest"`
	IsMeetingResponse      *bool                       `json:"isMeetingResponse"`
	IsNonDeliveryReport    *bool                       `json:"isNonDeliveryReport"`
	IsPermissionControlled *bool                       `json:"isPermissionControlled"`
	IsReadReceipt          *bool                       `json:"isReadReceipt"`
	IsSigned               *bool                       `json:"isSigned"`
	IsVoicemail            *bool                       `json:"isVoicemail"`
	MessageActionFlag      string                      `json:"messageActionFlag"`
	NotSentToMe            *bool                       `json:"notSentToMe"`
	RecipientContains      []string                    `json:"recipientContains"`
	SenderContains         []string                    `json:"senderContains"`
	Sensitivity            string                      `json:"sensitivity"`
	SentCCMe               *bool                       `json:"sentCcMe"`
	SentOnlyToMe           *bool                       `json:"sentOnlyToMe"`
	SentToAddresses        []graphRecipient            `json:"sentToAddresses"`
	SentToMe               *bool                       `json:"sentToMe"`
	SentToOrCCMe           *bool                       `json:"sentToOrCcMe"`
	SubjectContains        []string                    `json:"subjectContains"`
	WithinSizeRange        *graphSizeRange             `json:"withinSizeRange"`
	Unknown                []mail.RuleUnsupportedField `json:"-"`
	HasAttachmentsPresent  bool                        `json:"-"`
}

// graphRuleActions works like graphRulePredicates: actions maildo can edit
// become maildo actions, and the rest are kept and listed as unsupported.
type graphRuleActions struct {
	AssignCategories      []string                    `json:"assignCategories"`
	CopyToFolder          string                      `json:"copyToFolder"`
	Delete                *bool                       `json:"delete"`
	ForwardAsAttachment   []graphRecipient            `json:"forwardAsAttachmentTo"`
	ForwardTo             []graphRecipient            `json:"forwardTo"`
	MarkAsRead            *bool                       `json:"markAsRead"`
	MarkImportance        string                      `json:"markImportance"`
	MoveToFolder          string                      `json:"moveToFolder"`
	PermanentDelete       *bool                       `json:"permanentDelete"`
	RedirectTo            []graphRecipient            `json:"redirectTo"`
	StopProcessingRules   *bool                       `json:"stopProcessingRules"`
	Unknown               []mail.RuleUnsupportedField `json:"-"`
	MarkAsReadPresent     bool                        `json:"-"`
	StopProcessingPresent bool                        `json:"-"`
}

type graphSizeRange struct {
	Minimum *int `json:"minimumSize"`
	Maximum *int `json:"maximumSize"`
}

// The decoders below note the fields Graph sent that the structs do not
// know, before encoding/json drops them. Known fields maildo cannot edit yet
// are handled further down.
func (g *graphRule) UnmarshalJSON(data []byte) error {
	type alias graphRule
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*g = graphRule(a)
	g.RawState = map[string]any{
		"conditions": rawObjectState(data, "conditions"),
		"actions":    rawObjectState(data, "actions"),
	}
	g.Unknown = unknownGraphFields(data, "", map[string]bool{
		"id": true, "displayName": true, "sequence": true, "isEnabled": true,
		"isReadOnly": true, "hasError": true, "conditions": true,
		"exceptions": true, "actions": true,
	})
	return nil
}

// UnmarshalJSON also notes whether hasAttachments was in the answer at all,
// since a nil pointer cannot tell a missing field from null.
func (g *graphRulePredicates) UnmarshalJSON(data []byte) error {
	type alias graphRulePredicates
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*g = graphRulePredicates(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, g.HasAttachmentsPresent = raw["hasAttachments"]
	g.Unknown = unknownGraphFields(data, "", map[string]bool{
		"bodyContains": true, "bodyOrSubjectContains": true, "categories": true,
		"fromAddresses": true, "hasAttachments": true, "headerContains": true,
		"importance": true, "isApprovalRequest": true, "isAutomaticForward": true,
		"isAutomaticReply": true, "isEncrypted": true, "isMeetingRequest": true,
		"isMeetingResponse": true, "isNonDeliveryReport": true,
		"isPermissionControlled": true, "isReadReceipt": true, "isSigned": true,
		"isVoicemail": true, "messageActionFlag": true, "notSentToMe": true,
		"recipientContains": true, "senderContains": true, "sensitivity": true,
		"sentCcMe": true, "sentOnlyToMe": true, "sentToAddresses": true,
		"sentToMe": true, "sentToOrCcMe": true, "subjectContains": true,
		"withinSizeRange": true,
	})
	return nil
}

// UnmarshalJSON also notes which action fields were in the answer, so later
// writes and checks can tell false, null and a missing field apart.
func (g *graphRuleActions) UnmarshalJSON(data []byte) error {
	type alias graphRuleActions
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*g = graphRuleActions(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, g.MarkAsReadPresent = raw["markAsRead"]
	_, g.StopProcessingPresent = raw["stopProcessingRules"]
	g.Unknown = unknownGraphFields(data, "", map[string]bool{
		"assignCategories": true, "copyToFolder": true, "delete": true,
		"forwardAsAttachmentTo": true, "forwardTo": true, "markAsRead": true,
		"markImportance": true, "moveToFolder": true, "permanentDelete": true,
		"redirectTo": true, "stopProcessingRules": true,
	})
	return nil
}

// appendOpaqueGraphPathSegment adds an id from Outlook to the URL path as it
// is, so no path cleaning can change its meaning. Only Path and RawPath
// change.
func appendOpaqueGraphPathSegment(target *url.URL, segment string) error {
	if target == nil || strings.TrimSpace(segment) == "" {
		return fmt.Errorf("invalid opaque Graph path segment")
	}
	escaped := url.PathEscape(segment)
	if segment == "." {
		escaped = "%2E"
	} else if segment == ".." {
		escaped = "%2E%2E"
	}
	pathPrefix := target.Path
	rawPrefix := target.EscapedPath()
	if !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}
	if !strings.HasSuffix(rawPrefix, "/") {
		rawPrefix += "/"
	}
	target.Path = pathPrefix + segment
	target.RawPath = rawPrefix + escaped
	return nil
}

// validGraphRuleRequestURL applies the same checks as validGraphURL, with one
// exception: the last path segment may be a rule or archive folder id that
// is "." or "..", which maildo encoded itself. The URL must still point to
// Graph.
func (c *Client) validGraphRuleRequestURL(target string) bool {
	if c.validGraphURL(target) {
		return true
	}
	base, err := url.Parse(c.graphBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.Fragment != "" {
		return false
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		!strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return false
	}
	escaped := u.EscapedPath()
	cut := strings.LastIndex(escaped, "/")
	if cut < 0 || !safeGraphPath(escaped[:cut]) {
		return false
	}
	tail, err := url.PathUnescape(escaped[cut+1:])
	if err != nil || (tail != "." && tail != "..") {
		return false
	}
	basePath := strings.TrimRight(base.EscapedPath(), "/")
	return basePath == "" || escaped[:cut] == basePath || strings.HasPrefix(escaped[:cut], basePath+"/")
}

// doGraphRequestNoRedirect sends one request to Graph and follows no
// redirects. The caller sets the headers, reads and closes the response and
// decides what the status means.
func (c *Client) doGraphRequestNoRedirect(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !c.validGraphRuleRequestURL(req.URL.String()) {
		return nil, c.ruleMutationError()
	}
	return noRedirectHTTPClient(c.http).Do(req)
}

// noRedirectHTTPClient copies the client, with its transport, timeout and
// cookie jar, and turns off redirects for the copy. A redirect can then never
// send the token or a write to another address.
func noRedirectHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

// ruleCollectionTarget builds the URL of the Inbox rules and checks it before
// any request carries the token.
func (c *Client) ruleCollectionTarget() (*url.URL, error) {
	target, err := url.Parse(c.graphBaseURL + "/me/mailFolders/" + url.PathEscape(string(mail.RuleScopeInbox)) + "/messageRules")
	if err != nil || !c.validGraphURL(target.String()) {
		return nil, c.ruleMutationError()
	}
	return target, nil
}

// ruleTarget adds the rule id to the URL as one segment, unchanged, and checks
// the finished URL.
func (c *Client) ruleTarget(ruleID string) (*url.URL, error) {
	target, err := c.ruleCollectionTarget()
	if err != nil {
		return nil, err
	}
	if err := appendOpaqueGraphPathSegment(target, ruleID); err != nil {
		return nil, c.ruleMutationError()
	}
	if !c.validGraphRuleRequestURL(target.String()) {
		return nil, c.ruleMutationError()
	}
	return target, nil
}

// graphRuleRead reads rules through graphReadRequest and turns a 404 into
// "rule not found". Its errors keep their reason, which the checks after a
// rule change need.
func (c *Client) graphRuleRead(ctx context.Context, target *url.URL) ([]byte, error) {
	targetString := ""
	if target != nil {
		targetString = target.String()
	}
	status, body, err := c.graphReadRequest(ctx, graphReadRule, targetString, "")
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return nil, provider.NewRuleReadFailureError(provider.RuleReadFailureNotFound, c.ruleNotFound(""))
	case http.StatusOK:
	default:
		return nil, provider.NewRuleReadFailureError(provider.RuleReadFailureProviderAPI, c.rulesError())
	}
	return body, nil
}

// ListRules returns the Outlook Inbox rules. maildo treats all Outlook rules
// as Inbox rules, so the scope in the request is not used.
func (c *Client) ListRules(ctx context.Context, _ mail.RuleListRequest) (mail.RuleListResult, error) {
	scope := mail.RuleScopeInbox
	values := url.Values{}
	values.Set("$top", fmt.Sprintf("%d", rulePageSize))
	next := withQuery(c.graphBaseURL+"/me/mailFolders/"+url.PathEscape(string(scope))+"/messageRules", values)
	var out []mail.Rule
	var pages graphPageGuard
	for next != "" {
		if err := pages.visit(ctx, c, next); err != nil {
			return mail.RuleListResult{}, provider.NewRuleReadFailureError(provider.RuleReadFailureProviderAPI, err)
		}
		// graphReadRequest checks each next link from Graph before it sends
		// the token.
		target, parseErr := url.Parse(next)
		if parseErr != nil {
			return mail.RuleListResult{}, provider.NewRuleReadFailureError(provider.RuleReadFailureProviderAPI, c.rulesError())
		}
		body, err := c.graphRuleRead(ctx, target)
		if err != nil {
			return mail.RuleListResult{}, err
		}
		var page graphRulePage
		if err := json.Unmarshal(body, &page); err != nil {
			return mail.RuleListResult{}, provider.NewRuleReadFailureError(provider.RuleReadFailureMalformedResponse, c.rulesError())
		}
		for _, row := range page.Value {
			out = append(out, c.toRule(row, scope))
		}
		next = page.NextLink
	}
	return mail.RuleListResult{Rules: out}, nil
}

// rulePageKey checks a page address and sorts its query parameters, so a
// next link that only reorders them is caught as a repeat before maildo sends
// the token again.
func (c *Client) rulePageKey(target string) (string, bool) {
	if !c.validGraphURL(target) {
		return "", false
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return u.String(), true
}

// GetRule returns one Outlook Inbox rule by its id.
func (c *Client) GetRule(ctx context.Context, req mail.RuleGetRequest) (mail.Rule, error) {
	scope := mail.RuleScopeInbox
	target, err := c.ruleTarget(req.RuleID)
	if err != nil {
		return mail.Rule{}, provider.NewRuleReadFailureError(provider.RuleReadFailureProviderAPI, c.rulesError())
	}
	body, err := c.graphRuleRead(ctx, target)
	if err != nil {
		var readErr *provider.RuleReadFailureError
		if errors.As(err, &readErr) && readErr.Reason() == provider.RuleReadFailureNotFound {
			return mail.Rule{}, provider.NewRuleReadFailureError(provider.RuleReadFailureNotFound, c.ruleNotFound(req.RuleID))
		}
		return mail.Rule{}, err
	}
	var row graphRule
	if err := json.Unmarshal(body, &row); err != nil {
		return mail.Rule{}, provider.NewRuleReadFailureError(provider.RuleReadFailureMalformedResponse, c.rulesError())
	}
	if row.ID != req.RuleID {
		return mail.Rule{}, provider.NewRuleReadFailureError(provider.RuleReadFailureProviderAPI, c.rulesError())
	}
	return c.toRule(row, scope), nil
}

// toRule turns a Graph rule into maildo's rule. It keeps Graph's raw
// conditions and actions apart, so a later change can send back the fields
// maildo does not edit.
func (c *Client) toRule(g graphRule, scope mail.RuleScope) mail.Rule {
	conditions := toPredicates(g.Conditions)
	exceptions := toPredicates(g.Exceptions)
	rule := mail.Rule{
		Account:      c.account,
		Provider:     "outlook",
		Scope:        scope,
		ID:           g.ID,
		DisplayName:  g.DisplayName,
		Sequence:     g.Sequence,
		IsEnabled:    g.IsEnabled,
		IsReadOnly:   g.IsReadOnly,
		HasError:     g.HasError,
		Conditions:   conditions,
		Exceptions:   exceptions,
		Actions:      toActions(g.Actions),
		ProviderMeta: map[string]any{"source": "microsoft_graph"},
		ProviderFieldPresence: mail.RuleProviderFieldPresence{
			ConditionsHasAttachments:                 g.Conditions.HasAttachmentsPresent,
			ActionsMarkAsRead:                        g.Actions.MarkAsReadPresent,
			ActionsStopProcessingRules:               g.Actions.StopProcessingPresent,
			ActionsStopProcessingRulesValueAvailable: g.Actions.StopProcessingRules != nil,
		},
		ProviderState: map[string]any{
			"conditions": cloneRuleStateMap(stateMapValue(g.RawState["conditions"])),
			"actions":    cloneRuleStateMap(stateMapValue(g.RawState["actions"])),
		},
	}
	rule.UnsupportedFields = append(rule.UnsupportedFields, withArea(g.Unknown, "rule")...)
	rule.UnsupportedFields = append(rule.UnsupportedFields, predicateUnsupported("conditions", g.Conditions)...)
	rule.UnsupportedFields = append(rule.UnsupportedFields, predicateUnsupported("exceptions", g.Exceptions)...)
	rule.UnsupportedFields = append(rule.UnsupportedFields, actionUnsupported(g.Actions)...)
	rule.UnsupportedFields = sortedRuleUnsupported(rule.UnsupportedFields)
	return rule
}

// toPredicates turns Graph's conditions into maildo's, also the ones maildo
// cannot edit yet.
func toPredicates(g graphRulePredicates) mail.RulePredicates {
	out := mail.RulePredicates{
		Semantics:              mail.DefaultRulePredicateSemantics(),
		FromAddresses:          recipients(g.FromAddresses),
		SenderContains:         append([]string(nil), g.SenderContains...),
		RecipientContains:      append([]string(nil), g.RecipientContains...),
		SubjectContains:        append([]string(nil), g.SubjectContains...),
		HeaderContains:         append([]string(nil), g.HeaderContains...),
		HasAttachments:         cloneBoolPtr(g.HasAttachments),
		BodyContains:           append([]string(nil), g.BodyContains...),
		BodyOrSubjectContains:  append([]string(nil), g.BodyOrSubjectContains...),
		Categories:             append([]string(nil), g.Categories...),
		Importance:             g.Importance,
		Sensitivity:            g.Sensitivity,
		MessageActionFlag:      g.MessageActionFlag,
		SentToAddresses:        recipients(g.SentToAddresses),
		IsApprovalRequest:      cloneBoolPtr(g.IsApprovalRequest),
		IsAutomaticForward:     cloneBoolPtr(g.IsAutomaticForward),
		IsAutomaticReply:       cloneBoolPtr(g.IsAutomaticReply),
		IsEncrypted:            cloneBoolPtr(g.IsEncrypted),
		IsMeetingRequest:       cloneBoolPtr(g.IsMeetingRequest),
		IsMeetingResponse:      cloneBoolPtr(g.IsMeetingResponse),
		IsNonDeliveryReport:    cloneBoolPtr(g.IsNonDeliveryReport),
		IsPermissionControlled: cloneBoolPtr(g.IsPermissionControlled),
		IsReadReceipt:          cloneBoolPtr(g.IsReadReceipt),
		IsSigned:               cloneBoolPtr(g.IsSigned),
		IsVoicemail:            cloneBoolPtr(g.IsVoicemail),
		NotSentToMe:            cloneBoolPtr(g.NotSentToMe),
		SentCCMe:               cloneBoolPtr(g.SentCCMe),
		SentOnlyToMe:           cloneBoolPtr(g.SentOnlyToMe),
		SentToMe:               cloneBoolPtr(g.SentToMe),
		SentToOrCCMe:           cloneBoolPtr(g.SentToOrCCMe),
	}
	if g.WithinSizeRange != nil {
		out.WithinSizeRange = &mail.RuleSizeRange{
			Minimum: cloneIntPtr(g.WithinSizeRange.Minimum),
			Maximum: cloneIntPtr(g.WithinSizeRange.Maximum),
		}
	}
	return out
}

// toActions turns Graph's actions into maildo's. Folder ids stay ids here;
// the command looks up the folder names later.
func toActions(g graphRuleActions) mail.RuleActions {
	out := mail.RuleActions{
		MarkAsRead:          cloneBoolPtr(g.MarkAsRead),
		AssignCategories:    append([]string(nil), g.AssignCategories...),
		MarkImportance:      g.MarkImportance,
		ForwardTo:           recipients(g.ForwardTo),
		ForwardAsAttachment: recipients(g.ForwardAsAttachment),
		RedirectTo:          recipients(g.RedirectTo),
	}
	if g.MoveToFolder != "" {
		out.MoveToFolder = &mail.RuleFolderTarget{ProviderFolderID: g.MoveToFolder}
	}
	if g.CopyToFolder != "" {
		out.CopyToFolder = &mail.RuleFolderTarget{ProviderFolderID: g.CopyToFolder}
	}
	if g.StopProcessingRules != nil {
		out.StopProcessingRules = *g.StopProcessingRules
	}
	if g.Delete != nil {
		out.Delete = *g.Delete
	}
	if g.PermanentDelete != nil {
		out.PermanentDelete = *g.PermanentDelete
	}
	return out
}

// predicateUnsupported lists the conditions maildo keeps but cannot edit,
// and the unknown fields the decoder noted.
func predicateUnsupported(area string, g graphRulePredicates) []mail.RuleUnsupportedField {
	var out []mail.RuleUnsupportedField
	out = append(out, preservedArray(area, "bodyContains", len(g.BodyContains))...)
	out = append(out, preservedArray(area, "bodyOrSubjectContains", len(g.BodyOrSubjectContains))...)
	out = append(out, preservedArray(area, "categories", len(g.Categories))...)
	out = append(out, preservedString(area, "importance", g.Importance)...)
	out = append(out, preservedString(area, "sensitivity", g.Sensitivity)...)
	out = append(out, preservedString(area, "messageActionFlag", g.MessageActionFlag)...)
	out = append(out, preservedArray(area, "sentToAddresses", len(g.SentToAddresses))...)
	if g.WithinSizeRange != nil {
		out = append(out, preserved(area, "withinSizeRange", "object", 1))
	}
	for _, item := range []struct {
		field string
		value *bool
	}{
		{"isApprovalRequest", g.IsApprovalRequest},
		{"isAutomaticForward", g.IsAutomaticForward},
		{"isAutomaticReply", g.IsAutomaticReply},
		{"isEncrypted", g.IsEncrypted},
		{"isMeetingRequest", g.IsMeetingRequest},
		{"isMeetingResponse", g.IsMeetingResponse},
		{"isNonDeliveryReport", g.IsNonDeliveryReport},
		{"isPermissionControlled", g.IsPermissionControlled},
		{"isReadReceipt", g.IsReadReceipt},
		{"isSigned", g.IsSigned},
		{"isVoicemail", g.IsVoicemail},
		{"notSentToMe", g.NotSentToMe},
		{"sentCcMe", g.SentCCMe},
		{"sentOnlyToMe", g.SentOnlyToMe},
		{"sentToMe", g.SentToMe},
		{"sentToOrCcMe", g.SentToOrCCMe},
	} {
		if item.value != nil {
			out = append(out, preserved(area, item.field, "bool", 1))
		}
	}
	out = append(out, withArea(g.Unknown, area)...)
	return out
}

// actionUnsupported lists the actions maildo keeps but cannot edit, and the
// unknown fields the decoder noted.
func actionUnsupported(g graphRuleActions) []mail.RuleUnsupportedField {
	const area = "actions"
	var out []mail.RuleUnsupportedField
	out = append(out, preservedArray(area, "assignCategories", len(g.AssignCategories))...)
	out = append(out, preservedString(area, "copyToFolder", g.CopyToFolder)...)
	if g.Delete != nil {
		out = append(out, preserved(area, "delete", "bool", 1))
	}
	if len(g.ForwardAsAttachment) > 0 {
		out = append(out, preserved(area, "forwardAsAttachmentTo", "array", len(g.ForwardAsAttachment)))
	}
	if len(g.ForwardTo) > 0 {
		out = append(out, preserved(area, "forwardTo", "array", len(g.ForwardTo)))
	}
	out = append(out, preservedString(area, "markImportance", g.MarkImportance)...)
	if g.PermanentDelete != nil {
		out = append(out, preserved(area, "permanentDelete", "bool", 1))
	}
	if len(g.RedirectTo) > 0 {
		out = append(out, preserved(area, "redirectTo", "array", len(g.RedirectTo)))
	}
	out = append(out, withArea(g.Unknown, area)...)
	return out
}

func preservedArray(area, field string, count int) []mail.RuleUnsupportedField {
	if count == 0 {
		return nil
	}
	return []mail.RuleUnsupportedField{preserved(area, field, "array", count)}
}

func preservedString(area, field, value string) []mail.RuleUnsupportedField {
	if value == "" {
		return nil
	}
	return []mail.RuleUnsupportedField{preserved(area, field, "string", 1)}
}

func preserved(area, field, kind string, count int) mail.RuleUnsupportedField {
	return mail.RuleUnsupportedField{
		Area:      area,
		Field:     field,
		ValueKind: kind,
		Count:     count,
		Summary:   "preserved documented provider field",
	}
}

func withArea(in []mail.RuleUnsupportedField, area string) []mail.RuleUnsupportedField {
	out := make([]mail.RuleUnsupportedField, len(in))
	for i, item := range in {
		item.Area = area
		out[i] = item
	}
	return out
}

// unknownGraphFields describes the fields maildo does not know, without
// their raw JSON. @odata fields belong to the response, not to the rule, so
// they are left out.
func unknownGraphFields(data []byte, area string, known map[string]bool) []mail.RuleUnsupportedField {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	out := make([]mail.RuleUnsupportedField, 0)
	for field, raw := range fields {
		if known[field] || strings.HasPrefix(field, "@odata.") {
			continue
		}
		kind, count := rawKind(raw)
		out = append(out, mail.RuleUnsupportedField{
			Area:      area,
			Field:     field,
			ValueKind: kind,
			Count:     count,
			Summary:   rawSummary(kind, count),
		})
	}
	return sortedRuleUnsupported(out)
}

func rawKind(raw json.RawMessage) (string, int) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "invalid_json", 0
	}
	switch x := v.(type) {
	case nil:
		return "null", 0
	case bool:
		return "bool", 1
	case string:
		return "string", 1
	case float64:
		return "number", 1
	case []any:
		return "array", len(x)
	case map[string]any:
		return "object", len(x)
	default:
		return "unknown", 0
	}
}

func rawSummary(kind string, count int) string {
	switch kind {
	case "array":
		return fmt.Sprintf("unknown provider array with %d item(s)", count)
	case "object":
		return fmt.Sprintf("unknown provider object with %d field(s)", count)
	case "invalid_json":
		return "unknown provider field with invalid JSON"
	default:
		return "unknown provider " + kind
	}
}

func sortedRuleUnsupported(in []mail.RuleUnsupportedField) []mail.RuleUnsupportedField {
	out := append([]mail.RuleUnsupportedField(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Area == out[j].Area {
			return out[i].Field < out[j].Field
		}
		return out[i].Area < out[j].Area
	})
	return out
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// CreateRule creates an Outlook Inbox rule and returns the id Graph gave it.
// The command reads the rule back to check it.
func (c *Client) CreateRule(ctx context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error) {
	payload := map[string]any{
		"displayName": req.Rule.DisplayName,
		"sequence":    req.Rule.Sequence,
		"isEnabled":   true,
		"conditions":  graphRuleConditionsFromRule(req.Rule.Conditions),
		"actions":     graphRuleActionsFromRule(req.Rule.Actions, req.Rule.ProviderFieldPresence),
	}
	target, err := c.ruleCollectionTarget()
	if err != nil {
		return mail.RuleMutationResult{}, err
	}
	_, body, err := c.graphRuleMutationDo(ctx, http.MethodPost, target.String(), payload)
	if err != nil {
		return mail.RuleMutationResult{}, err
	}
	ruleID, err := c.decodeCreatedRuleID(body)
	if err != nil {
		return mail.RuleMutationResult{}, err
	}
	return mail.RuleMutationResult{
		Operation:        mail.RuleOperationCreate,
		RuleID:           ruleID,
		ProviderMetadata: graphRuleMutationMeta(),
	}, nil
}

// UpdateRule sends only the fields that change. Graph replaces conditions
// and actions as a whole on PATCH, so those are rebuilt from the rule as it
// was read (ProviderState), which keeps the fields maildo cannot edit.
func (c *Client) UpdateRule(ctx context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error) {
	payload, err := graphRuleUpdatePayload(req)
	if err != nil {
		return mail.RuleMutationResult{}, c.ruleMutationError()
	}
	target, err := c.ruleTarget(req.RuleID)
	if err != nil {
		return mail.RuleMutationResult{}, err
	}
	status, _, err := c.graphRuleMutationDo(ctx, http.MethodPatch, target.String(), payload)
	if err != nil {
		return mail.RuleMutationResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.RuleMutationResult{}, c.ruleNotFound(req.RuleID)
	}
	if status != http.StatusOK {
		return mail.RuleMutationResult{}, c.ruleMutationError()
	}
	return mail.RuleMutationResult{
		Operation:        req.ChangeSet.Operation,
		RuleID:           req.RuleID,
		ProviderMetadata: graphRuleMutationMeta(),
	}, nil
}

// DeleteRule deletes one Outlook Inbox rule by its id.
func (c *Client) DeleteRule(ctx context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error) {
	target, err := c.ruleTarget(req.RuleID)
	if err != nil {
		return mail.RuleDeleteResult{}, err
	}
	status, _, err := c.graphRuleMutationDo(ctx, http.MethodDelete, target.String(), nil)
	if err != nil {
		return mail.RuleDeleteResult{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return mail.RuleDeleteResult{}, c.ruleNotFound(req.RuleID)
	}
	if status != http.StatusNoContent {
		return mail.RuleDeleteResult{}, c.ruleMutationError()
	}
	return mail.RuleDeleteResult{Operation: mail.RuleOperationDelete, RuleID: req.RuleID, ProviderMetadata: graphRuleMutationMeta()}, nil
}

// graphRuleUpdatePayload builds the smallest PATCH. When a condition or an
// action changes, it sends the whole conditions or actions object, with the
// fields maildo cannot edit unchanged.
func graphRuleUpdatePayload(req mail.RuleUpdateRequest) (map[string]any, error) {
	change := req.ChangeSet
	payload := map[string]any{}
	// Simple fields can change on their own, so unchanged ones are left out.
	if change.DisplayName != nil && change.DisplayName.Set {
		payload["displayName"] = change.DisplayName.After
	}
	if change.Sequence != nil && change.Sequence.Set {
		payload["sequence"] = change.Sequence.After
	}
	if change.IsEnabled != nil && change.IsEnabled.Set && change.IsEnabled.After != nil {
		payload["isEnabled"] = *change.IsEnabled.After
	}
	if ruleChangeTouchesConditions(change) {
		// Start from the conditions as they were read. Starting from an empty
		// object would delete the conditions maildo cannot edit.
		conditions, err := providerRuleState(req.Existing.ProviderState, "conditions")
		if err != nil {
			return nil, err
		}
		mergeRuleConditionChanges(conditions, change)
		payload["conditions"] = conditions
	}
	if ruleChangeTouchesActions(change) {
		// The same goes for the actions.
		actions, err := providerRuleState(req.Existing.ProviderState, "actions")
		if err != nil {
			return nil, err
		}
		mergeRuleActionChanges(actions, change)
		payload["actions"] = actions
	}
	return payload, nil
}

// mergeRuleConditionChanges applies the changes to the conditions object. A
// cleared condition is removed. If Graph kept a removed field anyway, the
// check after the change notices it.
func mergeRuleConditionChanges(dst map[string]any, change mail.RuleChangeSet) {
	if change.FromAddresses != nil && change.FromAddresses.Set {
		dst["fromAddresses"] = graphRecipientsPayload(change.FromAddresses.After)
	}
	if change.SenderContains != nil && change.SenderContains.Set {
		dst["senderContains"] = append([]string(nil), change.SenderContains.After...)
	}
	if change.RecipientContains != nil && change.RecipientContains.Set {
		dst["recipientContains"] = append([]string(nil), change.RecipientContains.After...)
	}
	if change.SubjectContains != nil && change.SubjectContains.Set {
		dst["subjectContains"] = append([]string(nil), change.SubjectContains.After...)
	}
	if change.HeaderContains != nil && change.HeaderContains.Set {
		dst["headerContains"] = append([]string(nil), change.HeaderContains.After...)
	}
	if change.HasAttachments != nil && change.HasAttachments.Set {
		if change.HasAttachments.Cleared {
			delete(dst, "hasAttachments")
		} else if change.HasAttachments.After != nil {
			dst["hasAttachments"] = *change.HasAttachments.After
		}
	}
}

// mergeRuleActionChanges applies the changes to the actions object. A
// cleared action is removed. If Graph kept a removed field anyway, the check
// after the change notices it.
func mergeRuleActionChanges(dst map[string]any, change mail.RuleChangeSet) {
	if change.MoveToFolder != nil && change.MoveToFolder.Set {
		id := ""
		if change.MoveToFolder.After != nil {
			id = change.MoveToFolder.After.ProviderFolderID
		}
		dst["moveToFolder"] = id
	}
	if change.MarkAsRead != nil && change.MarkAsRead.Set {
		if change.MarkAsRead.Cleared {
			delete(dst, "markAsRead")
		} else if change.MarkAsRead.After != nil {
			dst["markAsRead"] = *change.MarkAsRead.After
		}
	}
	if change.StopProcessingRules != nil && change.StopProcessingRules.Set {
		if change.StopProcessingRules.Cleared {
			delete(dst, "stopProcessingRules")
		} else if change.StopProcessingRules.After != nil {
			dst["stopProcessingRules"] = *change.StopProcessingRules.After
		}
	}
}

func graphRuleConditionsFromRule(p mail.RulePredicates) map[string]any {
	out := map[string]any{}
	if len(p.FromAddresses) > 0 {
		out["fromAddresses"] = graphRecipientsPayload(p.FromAddresses)
	}
	if len(p.SenderContains) > 0 {
		out["senderContains"] = append([]string(nil), p.SenderContains...)
	}
	if len(p.RecipientContains) > 0 {
		out["recipientContains"] = append([]string(nil), p.RecipientContains...)
	}
	if len(p.SubjectContains) > 0 {
		out["subjectContains"] = append([]string(nil), p.SubjectContains...)
	}
	if len(p.HeaderContains) > 0 {
		out["headerContains"] = append([]string(nil), p.HeaderContains...)
	}
	if p.HasAttachments != nil {
		out["hasAttachments"] = *p.HasAttachments
	}
	return out
}

// graphRuleActionsFromRule sends stopProcessingRules only when it was set,
// because its value false cannot show whether the field was there.
func graphRuleActionsFromRule(a mail.RuleActions, presence mail.RuleProviderFieldPresence) map[string]any {
	out := map[string]any{}
	if a.MoveToFolder != nil {
		out["moveToFolder"] = a.MoveToFolder.ProviderFolderID
	}
	if a.MarkAsRead != nil {
		out["markAsRead"] = *a.MarkAsRead
	}
	if presence.ActionsStopProcessingRules {
		out["stopProcessingRules"] = a.StopProcessingRules
	}
	return out
}

func ruleChangeTouchesConditions(change mail.RuleChangeSet) bool {
	return change.FromAddresses != nil ||
		change.SenderContains != nil ||
		change.RecipientContains != nil ||
		change.SubjectContains != nil ||
		change.HeaderContains != nil ||
		change.HasAttachments != nil
}

func ruleChangeTouchesActions(change mail.RuleChangeSet) bool {
	return change.MoveToFolder != nil ||
		change.MarkAsRead != nil ||
		change.StopProcessingRules != nil
}

// providerRuleState returns a copy of the raw conditions or actions read with
// the rule. An empty object is fine; a missing one is an error.
func providerRuleState(state map[string]any, key string) (map[string]any, error) {
	if state == nil {
		return nil, fmt.Errorf("missing provider state")
	}
	value, ok := state[key]
	if !ok {
		return nil, fmt.Errorf("missing provider state")
	}
	return cloneRuleStateMap(stateMapValue(value)), nil
}

// graphRuleMutationDo sends one rule change and decides what the status means
// for that kind of change. It never retries. When the outcome is unclear (a
// broken connection or a server error), the command reads the rule back.
func (c *Client) graphRuleMutationDo(ctx context.Context, method, target string, payload any) (int, []byte, error) {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, err
	}
	if !c.validGraphRuleRequestURL(target) {
		return 0, nil, c.ruleMutationError()
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, c.ruleMutationError()
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, rungrad.NewError(rungrad.ExitAPI, "could not prepare the rule request to Outlook")
	}
	// Check the finished URL before the request carries the token.
	if !c.validGraphRuleRequestURL(req.URL.String()) {
		return 0, nil, c.ruleMutationError()
	}
	req.Header.Set("Authorization", authHeader)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, dispatchErr := c.doGraphRequestNoRedirect(req)
	if resp == nil || resp.StatusCode == 0 {
		return 0, nil, provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeTransportUnknown)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	_ = dispatchErr // When there is a status, the status counts.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return status, nil, repairRequired(c.account)
	}
	if status == http.StatusTooManyRequests {
		return status, nil, rungrad.NewError(rungrad.ExitRateLimited, "the rule request to Outlook was rate limited")
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		if method == http.MethodPost {
			return status, nil, c.ruleMutationError()
		}
		return status, nil, nil
	}
	expected := (method == http.MethodPost && status == http.StatusCreated) ||
		(method == http.MethodPatch && status == http.StatusOK) ||
		(method == http.MethodDelete && status == http.StatusNoContent)
	if expected {
		switch method {
		case http.MethodPost:
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
			if readErr != nil {
				return status, nil, provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeUnreadable)
			}
			if len(responseBody) > maxBodyBytes {
				return status, nil, provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeOversized)
			}
			return status, responseBody, nil
		case http.MethodPatch:
			// The body of a PATCH answer is never trusted. Read and drop it,
			// up to the usual limit; the command checks the rule with a GET.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes+1))
			return status, nil, nil
		case http.MethodDelete:
			return status, nil, nil
		}
	}
	if status >= 200 && status < 300 {
		return status, nil, provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeUnexpected2xx)
	}
	if status == http.StatusRequestTimeout || status >= 500 {
		return status, nil, provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeServerUnknown)
	}
	// 1xx, redirects and other 4xx statuses mean Outlook refused the change.
	// Nothing is checked or tried again.
	return status, nil, c.ruleMutationError()
}

// decodeCreatedRuleID reads the new rule's id, which the check after a
// create needs. Without a usable id the outcome is unknown.
func (c *Client) decodeCreatedRuleID(body []byte) (string, error) {
	var response struct {
		ID string `json:"id"`
	}
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &response) != nil {
		return "", provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeMalformed)
	}
	if strings.TrimSpace(response.ID) == "" {
		return "", provider.NewRuleMutationOutcomeUnknownError(provider.RuleMutationOutcomeMissingID)
	}
	return response.ID, nil
}

func graphRuleMutationMeta() mail.RuleMutationProviderMetadata {
	metadata, err := mail.NewRuleMutationProviderMetadata(mail.RuleMutationSourceMicrosoftGraph)
	if err != nil {
		panic(err)
	}
	return metadata
}

// rawObjectState decodes the raw conditions or actions with json.Number, so
// numbers are sent back exactly as Graph wrote them.
func rawObjectState(data []byte, key string) map[string]any {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return map[string]any{}
	}
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return map[string]any{}
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func stateMapValue(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

// cloneRuleStateMap makes a deep copy, so a change to the request cannot
// change the rule as it was read.
func cloneRuleStateMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneRuleStateValue(v)
	}
	return out
}

func cloneRuleStateValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneRuleStateMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneRuleStateValue(item)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	default:
		return x
	}
}

func (c *Client) rulesError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not read rules for account %q (outlook)", c.account))
}

func (c *Client) ruleMutationError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not change the Outlook rule for account %q", c.account))
}

func (c *Client) ruleNotFound(ruleID string) error {
	return errs.NewWithDetails(errs.CodeRuleNotFound, "rule not found", map[string]any{
		"account":  c.account,
		"provider": "outlook",
		"scope":    string(mail.RuleScopeInbox),
		"rule_id":  ruleID,
	})
}
