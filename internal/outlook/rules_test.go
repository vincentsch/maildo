package outlook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

func TestListRulesPaginationAndFieldMapping(t *testing.T) {
	var srv *httptest.Server
	var requests []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path+" top="+r.URL.Query().Get("$top")+" skip="+r.URL.Query().Get("$skiptoken"))
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer rule-access-token-12345" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/me/mailFolders/inbox/messageRules" && r.URL.Query().Get("$skiptoken") == "":
			writeJSON(w, map[string]any{
				"value":           []any{richGraphRule("rule-1")},
				"@odata.nextLink": srv.URL + "/me/mailFolders/inbox/messageRules?$skiptoken=2",
			})
		case r.URL.Path == "/me/mailFolders/inbox/messageRules" && r.URL.Query().Get("$skiptoken") == "2":
			writeJSON(w, map[string]any{"value": []any{map[string]any{
				"id": "rule-2", "displayName": "Second", "sequence": 2,
				"isEnabled": true, "isReadOnly": false, "hasError": false,
				"conditions": map[string]any{"subjectContains": []string{"two"}},
				"exceptions": map[string]any{},
				"actions":    map[string]any{"markAsRead": true},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testRulesClient(srv)

	got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2: %#v", len(got.Rules), got.Rules)
	}
	if len(requests) != 2 ||
		requests[0] != "/me/mailFolders/inbox/messageRules top=100 skip=" ||
		requests[1] != "/me/mailFolders/inbox/messageRules top= skip=2" {
		t.Fatalf("requests = %#v", requests)
	}
	rule := got.Rules[0]
	if rule.Account != "work" || rule.Provider != "outlook" || rule.Scope != mail.RuleScopeInbox || rule.ProviderMeta["source"] != "microsoft_graph" {
		t.Fatalf("rule identity = %#v", rule)
	}
	if rule.ProviderState == nil {
		t.Fatal("provider state missing")
	}
	conditionState, ok := rule.ProviderState["conditions"].(map[string]any)
	if !ok || conditionState == nil || conditionState["providerPredicate"] == nil {
		t.Fatalf("condition provider state = %#v", rule.ProviderState["conditions"])
	}
	actionState, ok := rule.ProviderState["actions"].(map[string]any)
	if !ok || actionState == nil || actionState["providerAction"] == nil {
		t.Fatalf("action provider state = %#v", rule.ProviderState["actions"])
	}
	if rule.Conditions.Semantics.Families != "and" || rule.Conditions.Semantics.RepeatedValues != "or" {
		t.Fatalf("semantics = %#v", rule.Conditions.Semantics)
	}
	if len(rule.Conditions.FromAddresses) != 1 || rule.Conditions.FromAddresses[0].Address != "sender@example.test" ||
		len(rule.Conditions.SubjectContains) != 2 || rule.Conditions.HasAttachments == nil || !*rule.Conditions.HasAttachments {
		t.Fatalf("conditions = %#v", rule.Conditions)
	}
	if rule.Conditions.WithinSizeRange == nil || *rule.Conditions.WithinSizeRange.Minimum != 10 || *rule.Conditions.WithinSizeRange.Maximum != 20 {
		t.Fatalf("size range = %#v", rule.Conditions.WithinSizeRange)
	}
	if rule.Conditions.MessageActionFlag != "any" || len(rule.Conditions.SentToAddresses) != 1 {
		t.Fatalf("preserved predicate fields = %#v", rule.Conditions)
	}
	for _, tt := range []struct {
		name  string
		value *bool
		want  bool
	}{
		{"isApprovalRequest", rule.Conditions.IsApprovalRequest, false},
		{"isAutomaticForward", rule.Conditions.IsAutomaticForward, true},
		{"isAutomaticReply", rule.Conditions.IsAutomaticReply, false},
		{"isEncrypted", rule.Conditions.IsEncrypted, true},
		{"isMeetingRequest", rule.Conditions.IsMeetingRequest, true},
		{"isMeetingResponse", rule.Conditions.IsMeetingResponse, false},
		{"isNonDeliveryReport", rule.Conditions.IsNonDeliveryReport, true},
		{"isPermissionControlled", rule.Conditions.IsPermissionControlled, false},
		{"isReadReceipt", rule.Conditions.IsReadReceipt, true},
		{"isSigned", rule.Conditions.IsSigned, false},
		{"isVoicemail", rule.Conditions.IsVoicemail, true},
		{"notSentToMe", rule.Conditions.NotSentToMe, false},
		{"sentCcMe", rule.Conditions.SentCCMe, true},
		{"sentOnlyToMe", rule.Conditions.SentOnlyToMe, false},
		{"sentToMe", rule.Conditions.SentToMe, true},
		{"sentToOrCcMe", rule.Conditions.SentToOrCCMe, false},
	} {
		if tt.value == nil || *tt.value != tt.want {
			t.Fatalf("%s = %v, want %t; conditions=%#v", tt.name, tt.value, tt.want, rule.Conditions)
		}
	}
	if rule.Actions.MoveToFolder == nil || rule.Actions.MoveToFolder.ProviderFolderID != "archive-id" ||
		rule.Actions.CopyToFolder == nil || rule.Actions.CopyToFolder.ProviderFolderID != "copy-id" ||
		rule.Actions.MarkAsRead == nil || !*rule.Actions.MarkAsRead || !rule.Actions.StopProcessingRules ||
		len(rule.Actions.ForwardTo) != 1 || rule.Actions.ForwardTo[0].Address != "outside@example.net" ||
		len(rule.Actions.RedirectTo) != 1 || len(rule.Actions.ForwardAsAttachment) != 1 {
		t.Fatalf("actions = %#v", rule.Actions)
	}
	for _, field := range []string{
		"rule.providerFuture",
		"conditions.bodyContains",
		"conditions.providerPredicate",
		"exceptions.exceptionFuture",
		"actions.copyToFolder",
		"actions.forwardTo",
		"actions.providerAction",
	} {
		if !hasUnsupported(rule.UnsupportedFields, field) {
			t.Fatalf("missing unsupported %s in %#v", field, rule.UnsupportedFields)
		}
	}
	for _, field := range []string{"rule.@odata.context", "conditions.@odata.type"} {
		if hasUnsupported(rule.UnsupportedFields, field) {
			t.Fatalf("OData metadata surfaced as unsupported %s in %#v", field, rule.UnsupportedFields)
		}
	}
}

func TestListRulesPageFailureReturnsNoPartialResult(t *testing.T) {
	var srv *httptest.Server
	var calls atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("$skiptoken") == "failed-page" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"page failed"}`))
			return
		}
		writeJSON(w, map[string]any{
			"value":           []any{richGraphRule("first-page-rule")},
			"@odata.nextLink": srv.URL + "/me/mailFolders/inbox/messageRules?$skiptoken=failed-page",
		})
	}))
	defer srv.Close()

	got, err := testRulesClient(srv).ListRules(context.Background(), mail.RuleListRequest{})
	if err == nil {
		t.Fatal("pagination failure succeeded")
	}
	if len(got.Rules) != 0 {
		t.Fatalf("partial rules returned after page failure: %#v", got.Rules)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
}

func TestRulePaginationTargetsStayInsideNormalizedGraphBase(t *testing.T) {
	tests := []string{
		"different host",
		"same host different port",
		"different scheme",
		"relative URL",
		"outside base path",
		"dot segment escapes base",
		"encoded dot segment escapes base",
		"userinfo is rejected",
		"valid base child",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			var primaryCalls atomic.Int32
			var outsideCalls atomic.Int32
			var outsideAuth atomic.Int32
			var secondaryCalls atomic.Int32
			var secondaryAuth atomic.Int32

			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				if r.Header.Get("Authorization") != "" {
					secondaryAuth.Add(1)
				}
				writeJSON(w, map[string]any{"value": []any{}})
			}))
			defer secondary.Close()

			var primary *httptest.Server
			primary = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				if r.URL.Path == "/v1.0/me/mailFolders/inbox/messageRules" {
					var next string
					switch name {
					case "different host":
						next = strings.Replace(secondary.URL, "127.0.0.1", "localhost", 1) + "/v1.0/next"
					case "same host different port":
						next = secondary.URL + "/v1.0/next"
					case "different scheme":
						next = strings.Replace(primary.URL, "http://", "https://", 1) + "/v1.0/next"
					case "relative URL":
						next = "/v1.0/next"
					case "outside base path":
						next = primary.URL + "/outside"
					case "dot segment escapes base":
						next = primary.URL + "/v1.0/../outside"
					case "encoded dot segment escapes base":
						next = primary.URL + "/v1.0/%2e%2e/outside"
					case "userinfo is rejected":
						next = strings.Replace(primary.URL, "http://", "http://maildo.invalid@", 1) + "/v1.0/next"
					case "valid base child":
						next = primary.URL + "/v1.0/next"
					}
					writeJSON(w, map[string]any{"value": []any{}, "@odata.nextLink": next})
					return
				}
				if r.URL.Path == "/v1.0/next" {
					if r.Header.Get("Authorization") != "Bearer rule-access-token-12345" {
						t.Errorf("positive-control authorization = %q", r.Header.Get("Authorization"))
					}
					writeJSON(w, map[string]any{"value": []any{richGraphRule("second-page")}})
					return
				}
				outsideCalls.Add(1)
				if r.Header.Get("Authorization") != "" {
					outsideAuth.Add(1)
				}
				writeJSON(w, map[string]any{"value": []any{}})
			}))
			defer primary.Close()

			client := &Client{
				accessToken:  secretFromString("rule-access-token-12345"),
				tokenType:    "Bearer",
				graphBaseURL: primary.URL + "/v1.0",
				http:         primary.Client(),
				account:      "work",
			}
			got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
			if name == "valid base child" {
				if err != nil || len(got.Rules) != 1 || got.Rules[0].ID != "second-page" {
					t.Fatalf("positive control = %#v, err=%v", got, err)
				}
				if primaryCalls.Load() != 2 {
					t.Fatalf("positive control requests = %d, want 2", primaryCalls.Load())
				}
				return
			}
			if err == nil || len(got.Rules) != 0 {
				t.Fatalf("hostile next link returned %#v, err=%v", got, err)
			}
			if primaryCalls.Load() != 1 || outsideCalls.Load() != 0 || secondaryCalls.Load() != 0 {
				t.Fatalf("requests primary=%d outside=%d secondary=%d", primaryCalls.Load(), outsideCalls.Load(), secondaryCalls.Load())
			}
			if outsideAuth.Load() != 0 || secondaryAuth.Load() != 0 {
				t.Fatalf("bearer reached rejected target: outside=%d secondary=%d", outsideAuth.Load(), secondaryAuth.Load())
			}
		})
	}
}

func TestRulePaginationCyclesTerminateWithoutPartialResults(t *testing.T) {
	for _, name := range []string{"self cycle", "two page cycle", "query reordering cycle"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				next := ""
				switch name {
				case "self cycle":
					next = srv.URL + r.URL.RequestURI()
				case "two page cycle":
					if r.URL.Query().Get("page") == "two" {
						next = srv.URL + "/me/mailFolders/inbox/messageRules?$top=100"
					} else {
						next = srv.URL + "/me/mailFolders/inbox/messageRules?page=two"
					}
				case "query reordering cycle":
					if r.URL.Query().Get("a") == "1" {
						next = srv.URL + "/me/mailFolders/inbox/messageRules?b=2&a=1"
					} else {
						next = srv.URL + "/me/mailFolders/inbox/messageRules?a=1&b=2"
					}
				}
				writeJSON(w, map[string]any{"value": []any{richGraphRule("partial")}, "@odata.nextLink": next})
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			got, err := testRulesClient(srv).ListRules(ctx, mail.RuleListRequest{})
			if err == nil || len(got.Rules) != 0 {
				t.Fatalf("cycle returned %#v, err=%v", got, err)
			}
			wantCalls := int32(1)
			if name != "self cycle" {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatalf("cycle requests = %d, want %d", calls.Load(), wantCalls)
			}
			if ctx.Err() != nil {
				t.Fatalf("cycle relied on context timeout: %v", ctx.Err())
			}
		})
	}
}

func TestRulePaginationLongFiniteChainPreservesOrderAndClosesBodies(t *testing.T) {
	const pageCount = 257
	var calls atomic.Int32
	var closes atomic.Int32
	client := &Client{
		accessToken:  secretFromString("rule-access-token-12345"),
		tokenType:    "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			page := 0
			if raw := r.URL.Query().Get("page"); raw != "" {
				parsed, err := strconv.Atoi(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid synthetic page %q: %w", raw, err)
				}
				page = parsed
			}
			if page < 0 || page >= pageCount {
				return nil, fmt.Errorf("synthetic page out of range: %d", page)
			}
			if page == 0 && r.URL.Query().Get("$top") != "100" {
				t.Errorf("initial page $top = %q, want 100", r.URL.Query().Get("$top"))
			}
			if page > 0 && r.URL.Query().Get("$top") != "" {
				t.Errorf("provider next link unexpectedly changed: %s", r.URL.RawQuery)
			}
			id := fmt.Sprintf("rule-%03d", page)
			sequence := pageCount - page
			payload := map[string]any{
				"value": []any{map[string]any{
					"id": id, "displayName": id, "sequence": sequence,
					"isEnabled": true, "conditions": map[string]any{},
					"exceptions": map[string]any{}, "actions": map[string]any{},
				}},
			}
			if page+1 < pageCount {
				payload["@odata.nextLink"] = fmt.Sprintf("https://graph.example.test/v1.0/me/mailFolders/inbox/messageRules?page=%d", page+1)
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       &trackedRuleBody{Reader: strings.NewReader(string(body)), closes: &closes},
				Request:    r,
			}, nil
		})},
		account: "work",
	}

	got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
	if err != nil {
		t.Fatalf("long finite pagination: %v", err)
	}
	if len(got.Rules) != pageCount || calls.Load() != pageCount || closes.Load() != pageCount {
		t.Fatalf("rules=%d requests=%d closes=%d, want %d each", len(got.Rules), calls.Load(), closes.Load(), pageCount)
	}
	for i, rule := range got.Rules {
		wantID := fmt.Sprintf("rule-%03d", i)
		wantSequence := pageCount - i
		if rule.ID != wantID || rule.Sequence != wantSequence {
			t.Fatalf("rule %d = id %q sequence %d, want %q/%d", i, rule.ID, rule.Sequence, wantID, wantSequence)
		}
	}
}

func TestRuleReadsAreSafeAcrossConcurrentCallers(t *testing.T) {
	const (
		callerCount  = 32
		readsPerCall = 25
	)
	var calls atomic.Int32
	var closes atomic.Int32
	client := &Client{
		accessToken:  secretFromString("rule-access-token-12345"),
		tokenType:    "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &trackedRuleBody{
					Reader: strings.NewReader(`{"value":[{"id":"shared-rule","displayName":"Shared","sequence":7,"isEnabled":true,"conditions":{},"exceptions":{},"actions":{}}]}`),
					closes: &closes,
				},
				Request: r,
			}, nil
		})},
		account: "work",
	}

	errsSeen := make(chan error, callerCount*readsPerCall)
	var wg sync.WaitGroup
	for caller := 0; caller < callerCount; caller++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for read := 0; read < readsPerCall; read++ {
				got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
				if err != nil {
					errsSeen <- err
					continue
				}
				if len(got.Rules) != 1 || got.Rules[0].ID != "shared-rule" || got.Rules[0].Sequence != 7 {
					errsSeen <- fmt.Errorf("unexpected concurrent result: %#v", got)
				}
			}
		}()
	}
	wg.Wait()
	close(errsSeen)
	for err := range errsSeen {
		t.Errorf("concurrent rule read: %v", err)
	}
	want := int32(callerCount * readsPerCall)
	if calls.Load() != want || closes.Load() != want {
		t.Fatalf("requests=%d closes=%d, want %d each", calls.Load(), closes.Load(), want)
	}
}

func TestRulesAlwaysUseInboxScope(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		switch {
		case r.URL.Path == "/me/mailFolders/inbox/messageRules":
			writeJSON(w, map[string]any{"value": []any{}})
		case r.URL.EscapedPath() == "/me/mailFolders/inbox/messageRules/"+url.PathEscape("rule/id"):
			writeJSON(w, map[string]any{
				"id": "rule/id", "displayName": "Scoped", "sequence": 1,
				"isEnabled": true, "isReadOnly": false, "hasError": false,
				"conditions": map[string]any{},
				"exceptions": map[string]any{},
				"actions":    map[string]any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testRulesClient(srv)
	if _, err := client.ListRules(context.Background(), mail.RuleListRequest{Scope: mail.RuleScope("archive")}); err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if _, err := client.GetRule(context.Background(), mail.RuleGetRequest{Scope: mail.RuleScope("archive"), RuleID: "rule/id"}); err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	want := []string{"/me/mailFolders/inbox/messageRules", "/me/mailFolders/inbox/messageRules/" + url.PathEscape("rule/id")}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestGetRuleEscapesIDAndMapsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/me/mailFolders/inbox/messageRules/"+url.PathEscape("id/with space") {
			t.Fatalf("escaped path = %s", r.URL.EscapedPath())
		}
		writeJSON(w, map[string]any{
			"id": "id/with space", "displayName": "Escaped", "sequence": 1,
			"isEnabled": true, "isReadOnly": false, "hasError": false,
			"conditions": map[string]any{},
			"exceptions": map[string]any{},
			"actions":    map[string]any{},
		})
	}))
	defer srv.Close()
	got, err := testRulesClient(srv).GetRule(context.Background(), mail.RuleGetRequest{RuleID: "id/with space"})
	if err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	if got.ID != "id/with space" {
		t.Fatalf("id = %q", got.ID)
	}

	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"missing"}`))
			}))
			defer srv.Close()
			_, err := testRulesClient(srv).GetRule(context.Background(), mail.RuleGetRequest{RuleID: "missing"})
			assertErrCode(t, err, errs.CodeRuleNotFound, 5)
			var productErr *errs.Error
			details := map[string]any(nil)
			if errors.As(err, &productErr) {
				details = productErr.Details()
			}
			if productErr == nil || details["rule_id"] != "missing" || details["scope"] != string(mail.RuleScopeInbox) {
				t.Fatalf("details = %#v", productErr)
			}
		})
	}
}

func TestGetRuleKeepsOpaqueIDsInsideInboxCollection(t *testing.T) {
	ids := []string{
		"slash/segment",
		"space value",
		"percent%2Fencoded",
		"query?value=1",
		"fragment#value",
		"leading...",
		"...trailing",
		"ünicode-规则",
		"tab\tcarriage\rline\n",
		strings.Repeat("long-id-", 128),
	}
	for _, id := range ids {
		t.Run(url.PathEscape(id), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				want := "/me/mailFolders/inbox/messageRules/" + url.PathEscape(id)
				if r.URL.EscapedPath() != want || r.URL.RawQuery != "" || r.URL.Fragment != "" {
					t.Errorf("target path=%q query=%q fragment=%q, want path=%q", r.URL.EscapedPath(), r.URL.RawQuery, r.URL.Fragment, want)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()
			_, err := testRulesClient(srv).GetRule(context.Background(), mail.RuleGetRequest{RuleID: id})
			assertErrCode(t, err, errs.CodeRuleNotFound, 5)
			if calls.Load() != 1 {
				t.Fatalf("requests = %d, want 1", calls.Load())
			}
		})
	}

	for _, id := range []string{".", ".."} {
		t.Run("dot segment "+id, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				want := "/me/mailFolders/inbox/messageRules/%2E"
				if id == ".." {
					want = "/me/mailFolders/inbox/messageRules/%2E%2E"
				}
				if r.URL.EscapedPath() != want {
					t.Errorf("escaped path = %q, want %q", r.URL.EscapedPath(), want)
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			_, err := testRulesClient(srv).GetRule(context.Background(), mail.RuleGetRequest{RuleID: id})
			assertErrCode(t, err, errs.CodeRuleNotFound, 5)
			if calls.Load() != 1 {
				t.Fatalf("dot-segment id sent %d request(s)", calls.Load())
			}
			if strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("safe rejection leaked secret or URL: %v", err)
			}
		})
	}
}

func TestAppendOpaqueGraphPathSegment(t *testing.T) {
	if err := appendOpaqueGraphPathSegment(nil, "id"); err == nil {
		t.Fatal("nil target succeeded")
	}
	for _, segment := range []string{"", " ", "\t\r\n"} {
		target, _ := url.Parse("https://graph.example.test/v1.0/rules?keep=yes#fragment")
		before := *target
		if err := appendOpaqueGraphPathSegment(target, segment); err == nil || *target != before {
			t.Fatalf("segment %q: target=%#v err=%v", segment, target, err)
		}
	}
	for _, segment := range []string{"/", "%", "%2F", "?", "#", ".", "..", "ünicode", " leading", "trailing "} {
		t.Run(url.PathEscape(segment), func(t *testing.T) {
			target, _ := url.Parse("https://user@example.test/v1.0/rules?keep=yes#fragment")
			if err := appendOpaqueGraphPathSegment(target, segment); err != nil {
				t.Fatal(err)
			}
			escaped := url.PathEscape(segment)
			if segment == "." {
				escaped = "%2E"
			}
			if segment == ".." {
				escaped = "%2E%2E"
			}
			if target.Path != "/v1.0/rules/"+segment || target.RawPath != "/v1.0/rules/"+escaped || target.EscapedPath() != "/v1.0/rules/"+escaped || target.RawQuery != "keep=yes" || target.Fragment != "fragment" || target.User.String() != "user" {
				t.Fatalf("segment %q produced %#v", segment, target)
			}
			req, _ := http.NewRequest(http.MethodGet, target.String(), nil)
			if req.URL.RequestURI() != "/v1.0/rules/"+escaped+"?keep=yes" {
				t.Fatalf("RequestURI = %q", req.URL.RequestURI())
			}
		})
	}
}

func FuzzAppendOpaqueGraphPathSegmentPreservesExactSegment(f *testing.F) {
	for _, segment := range []string{
		"rule-id",
		"",
		" \t\r\n",
		"/",
		"%",
		"%2F",
		"?query=value",
		"#fragment",
		".",
		"..",
		" leading and trailing ",
		"ünicode/规则",
		"path\\separator",
	} {
		f.Add(segment)
	}

	f.Fuzz(func(t *testing.T, segment string) {
		if len(segment) > 512 {
			t.Skip()
		}
		target, err := url.Parse("https://user@example.test/v1.0/me/mailFolders/inbox/messageRules?keep=yes#fragment")
		if err != nil {
			t.Fatal(err)
		}
		before := *target
		err = appendOpaqueGraphPathSegment(target, segment)
		if strings.TrimSpace(segment) == "" {
			if err == nil || !reflect.DeepEqual(*target, before) {
				t.Fatalf("blank segment changed target: segment=%q target=%#v err=%v", segment, target, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("opaque segment %q: %v", segment, err)
		}

		escaped := url.PathEscape(segment)
		if segment == "." {
			escaped = "%2E"
		} else if segment == ".." {
			escaped = "%2E%2E"
		}
		wantPath := before.Path + "/" + segment
		wantRawPath := before.EscapedPath() + "/" + escaped
		want := before
		want.Path = wantPath
		want.RawPath = wantRawPath
		if !reflect.DeepEqual(*target, want) || target.EscapedPath() != wantRawPath {
			t.Fatalf("segment %q produced %#v, want %#v", segment, target, want)
		}

		roundTripped, err := url.Parse(target.String())
		if err != nil {
			t.Fatalf("parse serialized target for %q: %v", segment, err)
		}
		if roundTripped.EscapedPath() != wantRawPath || roundTripped.RawQuery != before.RawQuery || roundTripped.Fragment != before.Fragment || roundTripped.User.String() != before.User.String() {
			t.Fatalf("segment %q round trip produced %#v", segment, roundTripped)
		}
	})
}

func TestGraphRequestNoRedirectContract(t *testing.T) {
	var hostileCalls atomic.Int32
	hostile := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hostileCalls.Add(1) }))
	defer hostile.Close()
	var initialCalls atomic.Int32
	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initialCalls.Add(1)
		http.Redirect(w, r, hostile.URL+"/stolen", http.StatusFound)
	}))
	defer initial.Close()
	var inheritedCalls atomic.Int32
	shared := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { inheritedCalls.Add(1); return nil }}
	client := testRulesClient(initial)
	client.http = shared
	req, _ := http.NewRequest(http.MethodGet, initial.URL+"/me/mailFolders/inbox/messageRules", nil)
	resp, err := client.doGraphRequestNoRedirect(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	if initialCalls.Load() != 1 || hostileCalls.Load() != 0 || inheritedCalls.Load() != 0 || shared.CheckRedirect == nil || shared.Timeout != time.Second {
		t.Fatalf("calls initial=%d hostile=%d inherited=%d shared=%#v", initialCalls.Load(), hostileCalls.Load(), inheritedCalls.Load(), shared)
	}
	resp.Body.Close()
	unpinned, _ := http.NewRequest(http.MethodGet, hostile.URL+"/outside", nil)
	if response, err := client.doGraphRequestNoRedirect(unpinned); err == nil || response != nil || hostileCalls.Load() != 0 {
		t.Fatalf("unpinned response=%#v err=%v hostile=%d", response, err, hostileCalls.Load())
	}

	var closes atomic.Int32
	owned := &Client{
		accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &trackedRuleBody{Reader: strings.NewReader("owned"), closes: &closes}, Request: r}, nil
		})},
	}
	ownedReq, _ := http.NewRequest(http.MethodGet, owned.graphBaseURL+"/me/mailFolders/inbox/messageRules", nil)
	ownedResp, err := owned.doGraphRequestNoRedirect(ownedReq)
	if err != nil || ownedResp == nil || closes.Load() != 0 {
		t.Fatalf("owned response=%#v closes=%d err=%v", ownedResp, closes.Load(), err)
	}
	ownedResp.Body.Close()
	if closes.Load() != 1 {
		t.Fatalf("caller close count=%d, want 1", closes.Load())
	}
}

func TestGetRuleRejectsMismatchedIdentity(t *testing.T) {
	var calls atomic.Int32
	client := &Client{
		accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := `{"id":"different-id","displayName":"Rule","sequence":1,"isEnabled":true,"conditions":{},"exceptions":{},"actions":{}}`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})},
	}
	_, err := client.GetRule(context.Background(), mail.RuleGetRequest{Scope: mail.RuleScopeInbox, RuleID: "requested-id"})
	var readErr *provider.RuleReadFailureError
	if !errors.As(err, &readErr) || readErr.Reason() != provider.RuleReadFailureProviderAPI || calls.Load() != 1 || strings.Contains(err.Error(), "different-id") {
		t.Fatalf("calls=%d err=%#v", calls.Load(), err)
	}
}

func TestOutlookRuleMutationOutcomeUnknownReasonMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   io.Reader
		method string
		want   provider.RuleMutationOutcomeUnknownReason
	}{
		{"missing id", http.StatusCreated, strings.NewReader(`{"id":" "}`), http.MethodPost, provider.RuleMutationOutcomeMissingID},
		{"malformed", http.StatusCreated, strings.NewReader(`{"id":`), http.MethodPost, provider.RuleMutationOutcomeMalformed},
		{"empty", http.StatusCreated, strings.NewReader(``), http.MethodPost, provider.RuleMutationOutcomeMalformed},
		{"wrong shaped", http.StatusCreated, strings.NewReader(`[]`), http.MethodPost, provider.RuleMutationOutcomeMalformed},
		{"wrong id type", http.StatusCreated, strings.NewReader(`{"id":3}`), http.MethodPost, provider.RuleMutationOutcomeMalformed},
		{"oversized", http.StatusCreated, strings.NewReader(strings.Repeat("x", maxBodyBytes+1)), http.MethodPost, provider.RuleMutationOutcomeOversized},
		{"unreadable", http.StatusCreated, io.MultiReader(strings.NewReader(`{"id":"x"}`), ruleReadError{}), http.MethodPost, provider.RuleMutationOutcomeUnreadable},
		{"unexpected success", http.StatusAccepted, strings.NewReader(`{}`), http.MethodPost, provider.RuleMutationOutcomeUnexpected2xx},
		{"timeout", http.StatusRequestTimeout, strings.NewReader(`{}`), http.MethodPatch, provider.RuleMutationOutcomeServerUnknown},
		{"server", http.StatusInternalServerError, strings.NewReader(`{}`), http.MethodDelete, provider.RuleMutationOutcomeServerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work"}
			client.http = &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(tt.body), Request: r}, nil
			})}
			target := client.graphBaseURL + "/me/mailFolders/inbox/messageRules"
			_, body, err := client.graphRuleMutationDo(context.Background(), tt.method, target, map[string]any{"safe": true})
			if tt.method == http.MethodPost && tt.status == http.StatusCreated && tt.want != provider.RuleMutationOutcomeOversized && tt.want != provider.RuleMutationOutcomeUnreadable {
				_, err = client.decodeCreatedRuleID(body)
			}
			var outcome *provider.RuleMutationOutcomeUnknownError
			if !errors.As(err, &outcome) || outcome.Reason() != tt.want {
				t.Fatalf("err=%#v, want reason %s", err, tt.want)
			}
		})
	}
}

func TestOutlookRuleMutationTransportOutcomeUnknown(t *testing.T) {
	var calls atomic.Int32
	client := &Client{accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work", http: &http.Client{Transport: ruleRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport sentinel")
	})}}
	_, _, err := client.graphRuleMutationDo(context.Background(), http.MethodPatch, client.graphBaseURL+"/me/mailFolders/inbox/messageRules/rule", map[string]any{"isEnabled": false})
	var outcome *provider.RuleMutationOutcomeUnknownError
	if !errors.As(err, &outcome) || outcome.Reason() != provider.RuleMutationOutcomeTransportUnknown || calls.Load() != 1 || strings.Contains(err.Error(), "transport sentinel") {
		t.Fatalf("calls=%d err=%#v", calls.Load(), err)
	}
}

func TestOutlookRuleMutationStatusAndRedirectMatrix(t *testing.T) {
	for _, tt := range []struct {
		name         string
		method       string
		status       int
		body         func() io.Reader
		wantReason   provider.RuleMutationOutcomeUnknownReason
		wantCode     string
		wantExit     int
		wantRejected bool
	}{
		{name: "post accepted", method: http.MethodPost, status: http.StatusCreated, body: func() io.Reader { return strings.NewReader(`{"id":"created"}`) }},
		{name: "patch accepted ignores body", method: http.MethodPatch, status: http.StatusOK, body: func() io.Reader {
			return io.MultiReader(strings.NewReader(strings.Repeat("x", maxBodyBytes+1)), ruleReadError{})
		}},
		{name: "delete accepted ignores body", method: http.MethodDelete, status: http.StatusNoContent, body: func() io.Reader { return strings.NewReader("ignored") }},
		{name: "unauthorized", method: http.MethodPost, status: http.StatusUnauthorized, wantCode: errs.CodeAuthRepairRequired, wantExit: rungrad.ExitAuth},
		{name: "forbidden", method: http.MethodPatch, status: http.StatusForbidden, wantCode: errs.CodeAuthRepairRequired, wantExit: rungrad.ExitAuth},
		{name: "rate limited", method: http.MethodDelete, status: http.StatusTooManyRequests, wantExit: rungrad.ExitRateLimited},
		{name: "post not found is rejected", method: http.MethodPost, status: http.StatusNotFound, wantRejected: true},
		{name: "post gone is rejected", method: http.MethodPost, status: http.StatusGone, wantRejected: true},
		{name: "patch not found is definitive", method: http.MethodPatch, status: http.StatusNotFound},
		{name: "patch gone is definitive", method: http.MethodPatch, status: http.StatusGone},
		{name: "delete not found is definitive", method: http.MethodDelete, status: http.StatusNotFound},
		{name: "delete gone is definitive", method: http.MethodDelete, status: http.StatusGone},
		{name: "switching protocols is rejected", method: http.MethodPatch, status: http.StatusSwitchingProtocols, wantRejected: true},
		{name: "redirect is rejected", method: http.MethodDelete, status: http.StatusTemporaryRedirect, wantRejected: true},
		{name: "other client error is rejected", method: http.MethodPost, status: http.StatusBadRequest, wantRejected: true},
		{name: "unexpected success is uncertain", method: http.MethodPost, status: http.StatusAccepted, wantReason: provider.RuleMutationOutcomeUnexpected2xx},
		{name: "timeout is uncertain", method: http.MethodPatch, status: http.StatusRequestTimeout, wantReason: provider.RuleMutationOutcomeServerUnknown},
		{name: "server error is uncertain", method: http.MethodDelete, status: http.StatusInternalServerError, wantReason: provider.RuleMutationOutcomeServerUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls, closes atomic.Int32
			reader := io.Reader(strings.NewReader(`{"error":"safe"}`))
			if tt.body != nil {
				reader = tt.body()
			}
			client := &Client{accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work", http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: &trackedRuleBody{Reader: reader, closes: &closes}, Request: r}, nil
			})}}
			_, _, err := client.graphRuleMutationDo(context.Background(), tt.method, client.graphBaseURL+"/me/mailFolders/inbox/messageRules", map[string]any{"safe": true})
			if calls.Load() != 1 || closes.Load() != 1 {
				t.Fatalf("calls=%d closes=%d err=%v", calls.Load(), closes.Load(), err)
			}
			switch {
			case tt.wantReason != "":
				var outcome *provider.RuleMutationOutcomeUnknownError
				if !errors.As(err, &outcome) || outcome.Reason() != tt.wantReason {
					t.Fatalf("err=%#v, want reason %s", err, tt.wantReason)
				}
			case tt.wantCode != "":
				assertErrCode(t, err, tt.wantCode, tt.wantExit)
			case tt.wantExit != 0:
				var exitErr interface{ ExitCode() int }
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.wantExit {
					t.Fatalf("err=%#v, want exit %d", err, tt.wantExit)
				}
			case tt.wantRejected:
				var outcome *provider.RuleMutationOutcomeUnknownError
				if err == nil || errors.As(err, &outcome) {
					t.Fatalf("err=%#v, want definitive rejection", err)
				}
			default:
				if err != nil {
					t.Fatalf("err=%v, want accepted/definitive known-id status", err)
				}
			}
		})
	}
	var redirected, redirectedAuth atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		if r.Header.Get("Authorization") != "" {
			redirectedAuth.Add(1)
		}
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := testRulesClient(source)
	_, _, err := client.graphRuleMutationDo(context.Background(), http.MethodPost, source.URL+"/me/mailFolders/inbox/messageRules", map[string]any{"safe": true})
	if err == nil || redirected.Load() != 0 || redirectedAuth.Load() != 0 {
		t.Fatalf("redirect err=%v destination calls=%d auth=%d", err, redirected.Load(), redirectedAuth.Load())
	}
}

func TestOutlookRuleMutationHTTPFailuresAndPinning(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		wantReason provider.RuleMutationOutcomeUnknownReason
		wantCode   string
	}{
		{name: "forbidden ignores oversized body", status: http.StatusForbidden, wantCode: errs.CodeAuthRepairRequired},
		{name: "server ignores oversized body", status: http.StatusInternalServerError, wantReason: provider.RuleMutationOutcomeServerUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &Client{accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work"}
			client.http = &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxBodyBytes+1))), Request: r}, nil
			})}
			_, _, err := client.graphRuleMutationDo(context.Background(), http.MethodPatch, client.graphBaseURL+"/me/mailFolders/inbox/messageRules/rule", map[string]any{"isEnabled": false})
			if calls.Load() != 1 || err == nil {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
			if tt.wantCode != "" {
				assertErrCode(t, err, tt.wantCode, 3)
			} else {
				var outcome *provider.RuleMutationOutcomeUnknownError
				if !errors.As(err, &outcome) || outcome.Reason() != tt.wantReason {
					t.Fatalf("outcome = %#v", err)
				}
			}
		})
	}

	var hostileCalls atomic.Int32
	client := &Client{accessToken: secretFromString("secret"), tokenType: "Bearer", graphBaseURL: "https://graph.example.test/v1.0", account: "work"}
	client.http = &http.Client{Transport: ruleRoundTripFunc(func(*http.Request) (*http.Response, error) {
		hostileCalls.Add(1)
		return nil, errors.New("must not dispatch")
	})}
	_, _, err := client.graphRuleMutationDo(context.Background(), http.MethodDelete, "https://hostile.example.test/stolen", nil)
	if err == nil || hostileCalls.Load() != 0 || strings.Contains(err.Error(), "hostile.example.test") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("pinning calls=%d err=%v", hostileCalls.Load(), err)
	}
}

func TestOutlookRulePresenceDecoding(t *testing.T) {
	row := graphRule{}
	data := []byte(`{"id":"rule","displayName":"Rule","sequence":1,"isEnabled":true,"conditions":{"hasAttachments":null},"exceptions":{},"actions":{"markAsRead":false,"stopProcessingRules":null}}`)
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	rule := (&Client{account: "work"}).toRule(row, mail.RuleScopeInbox)
	p := rule.ProviderFieldPresence
	if !p.ConditionsHasAttachments || !p.ActionsMarkAsRead || !p.ActionsStopProcessingRules || p.ActionsStopProcessingRulesValueAvailable || rule.Conditions.HasAttachments != nil || rule.Actions.MarkAsRead == nil || *rule.Actions.MarkAsRead {
		t.Fatalf("decoded rule=%#v presence=%#v", rule, p)
	}
}

func TestRuleReadResponsesCloseAndRejectUnsafeBodies(t *testing.T) {
	tests := []struct {
		name   string
		status int
		reader func() io.Reader
		wantOK bool
	}{
		{name: "success", status: http.StatusOK, reader: func() io.Reader { return strings.NewReader(`{"value":[]}`) }, wantOK: true},
		{name: "non success", status: http.StatusBadRequest, reader: func() io.Reader { return strings.NewReader(`{"error":"bad"}`) }},
		{name: "malformed", status: http.StatusOK, reader: func() io.Reader { return strings.NewReader(`{"value":`) }},
		{name: "trailing garbage", status: http.StatusOK, reader: func() io.Reader { return strings.NewReader(`{"value":[]} trailing`) }},
		{name: "empty no content", status: http.StatusNoContent, reader: func() io.Reader { return strings.NewReader("") }},
		{name: "oversized", status: http.StatusOK, reader: func() io.Reader { return strings.NewReader(strings.Repeat(" ", maxBodyBytes+1)) }},
		{name: "read failure", status: http.StatusOK, reader: func() io.Reader {
			return io.MultiReader(strings.NewReader(`{"value":[]}`), ruleReadError{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var closes atomic.Int32
			client := &Client{
				accessToken:  secretFromString("rule-access-token-12345"),
				tokenType:    "Bearer",
				graphBaseURL: "https://graph.example.test/v1.0",
				http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: tt.status,
						Header:     make(http.Header),
						Body:       &trackedRuleBody{Reader: tt.reader(), closes: &closes},
						Request:    r,
					}, nil
				})},
				account: "work",
			}
			got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
			if tt.wantOK {
				if err != nil || len(got.Rules) != 0 {
					t.Fatalf("success result=%#v err=%v", got, err)
				}
			} else if err == nil || len(got.Rules) != 0 {
				t.Fatalf("unsafe response returned %#v, err=%v", got, err)
			}
			if closes.Load() != 1 {
				t.Fatalf("body close calls = %d, want 1", closes.Load())
			}
			if err != nil && (strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), "graph.example.test")) {
				t.Fatalf("error leaked secret or target: %v", err)
			}
		})
	}
}

func TestRuleResponseSizeBoundaries(t *testing.T) {
	boundaries := []struct {
		name      string
		size      int
		oversized bool
	}{
		{name: "limit minus one", size: maxBodyBytes - 1},
		{name: "exact limit", size: maxBodyBytes},
		{name: "limit plus one", size: maxBodyBytes + 1, oversized: true},
	}

	for _, boundary := range boundaries {
		t.Run("list/"+boundary.name, func(t *testing.T) {
			body := paddedRuleJSONObject(t, `{"value":[]}`, boundary.size)
			var closes atomic.Int32
			client := exactRuleResponseClient(http.StatusOK, body, &closes)
			got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
			if boundary.oversized {
				assertRuleReadFailureReason(t, err, provider.RuleReadFailureOversizedResponse)
				if len(got.Rules) != 0 {
					t.Fatalf("oversized list returned %d rows", len(got.Rules))
				}
			} else if err != nil || len(got.Rules) != 0 {
				t.Fatalf("list size %d result=%#v err=%v", boundary.size, got, err)
			}
			if closes.Load() != 1 {
				t.Fatalf("list size %d close count=%d", boundary.size, closes.Load())
			}
		})

		t.Run("direct get/"+boundary.name, func(t *testing.T) {
			body := paddedRuleJSONObject(t, `{"id":"target","displayName":"Rule","sequence":1,"isEnabled":true,"conditions":{},"exceptions":{},"actions":{}}`, boundary.size)
			var closes atomic.Int32
			client := exactRuleResponseClient(http.StatusOK, body, &closes)
			got, err := client.GetRule(context.Background(), mail.RuleGetRequest{Scope: mail.RuleScopeInbox, RuleID: "target"})
			if boundary.oversized {
				assertRuleReadFailureReason(t, err, provider.RuleReadFailureOversizedResponse)
				if got.ID != "" {
					t.Fatalf("oversized direct get returned id %q", got.ID)
				}
			} else if err != nil || got.ID != "target" {
				t.Fatalf("direct get size %d result=%#v err=%v", boundary.size, got, err)
			}
			if closes.Load() != 1 {
				t.Fatalf("direct get size %d close count=%d", boundary.size, closes.Load())
			}
		})

		t.Run("accepted create/"+boundary.name, func(t *testing.T) {
			body := paddedRuleJSONObject(t, `{"id":"created"}`, boundary.size)
			var closes atomic.Int32
			client := exactRuleResponseClient(http.StatusCreated, body, &closes)
			_, responseBody, err := client.graphRuleMutationDo(context.Background(), http.MethodPost, client.graphBaseURL+"/me/mailFolders/inbox/messageRules", map[string]any{"safe": true})
			if boundary.oversized {
				var outcome *provider.RuleMutationOutcomeUnknownError
				if !errors.As(err, &outcome) || outcome.Reason() != provider.RuleMutationOutcomeOversized || responseBody != nil {
					t.Fatalf("oversized create body=%d response=%d err=%#v", boundary.size, len(responseBody), err)
				}
			} else {
				if err != nil || len(responseBody) != boundary.size {
					t.Fatalf("create size %d response=%d err=%v", boundary.size, len(responseBody), err)
				}
				id, decodeErr := client.decodeCreatedRuleID(responseBody)
				if decodeErr != nil || id != "created" {
					t.Fatalf("create size %d id=%q err=%v", boundary.size, id, decodeErr)
				}
			}
			if closes.Load() != 1 {
				t.Fatalf("create size %d close count=%d", boundary.size, closes.Load())
			}
		})
	}
}

func TestRuleListLaterPageSizeBoundariesReturnAllOrNothing(t *testing.T) {
	boundaries := []struct {
		name      string
		size      int
		oversized bool
	}{
		{name: "limit minus one", size: maxBodyBytes - 1},
		{name: "exact limit", size: maxBodyBytes},
		{name: "limit plus one", size: maxBodyBytes + 1, oversized: true},
	}
	const firstPage = `{"value":[{"id":"first-page-rule","displayName":"First","sequence":1,"isEnabled":true,"conditions":{},"exceptions":{},"actions":{}}],"@odata.nextLink":"https://graph.example.test/v1.0/me/mailFolders/inbox/messageRules?$skiptoken=second"}`

	for _, boundary := range boundaries {
		t.Run(boundary.name, func(t *testing.T) {
			secondPage := paddedRuleJSONObject(t, `{"value":[]}`, boundary.size)
			var calls atomic.Int32
			var firstCloses atomic.Int32
			var secondCloses atomic.Int32
			client := &Client{
				accessToken: secretFromString("synthetic-rule-token"), tokenType: "Bearer",
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					switch calls.Add(1) {
					case 1:
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &trackedRuleBody{Reader: strings.NewReader(firstPage), closes: &firstCloses}, Request: r}, nil
					case 2:
						if r.URL.Query().Get("$skiptoken") != "second" {
							t.Errorf("second-page query = %q", r.URL.RawQuery)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &trackedRuleBody{Reader: strings.NewReader(secondPage), closes: &secondCloses}, Request: r}, nil
					default:
						return nil, errors.New("unexpected extra page request")
					}
				})},
			}

			got, err := client.ListRules(context.Background(), mail.RuleListRequest{})
			if boundary.oversized {
				assertRuleReadFailureReason(t, err, provider.RuleReadFailureOversizedResponse)
				if len(got.Rules) != 0 {
					t.Fatalf("oversized later page returned partial rules: %#v", got.Rules)
				}
			} else if err != nil || len(got.Rules) != 1 || got.Rules[0].ID != "first-page-rule" {
				t.Fatalf("later page size %d result=%#v err=%v", boundary.size, got, err)
			}
			if calls.Load() != 2 || firstCloses.Load() != 1 || secondCloses.Load() != 1 {
				t.Fatalf("later page size %d calls/closes=%d/%d/%d", boundary.size, calls.Load(), firstCloses.Load(), secondCloses.Load())
			}
		})
	}
}

func TestGraphRuleMutationConcurrentCallsPreserveSharedClient(t *testing.T) {
	const callCount = 64
	transport := &concurrentRuleMutationTransport{records: map[string]concurrentRuleMutationRecord{}}
	var inheritedRedirectCalls atomic.Int32
	shared := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			inheritedRedirectCalls.Add(1)
			return nil
		},
	}
	redirectPointer := reflect.ValueOf(shared.CheckRedirect).Pointer()
	client := &Client{
		accessToken: secretFromString("synthetic-rule-token"), tokenType: "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0", account: "work", http: shared,
	}

	errsSeen := make(chan error, callCount)
	var wg sync.WaitGroup
	for i := 0; i < callCount; i++ {
		wg.Add(1)
		go func(call int) {
			defer wg.Done()
			id := fmt.Sprintf("rule-%02d/opaque", call)
			target, err := client.ruleTarget(id)
			if err != nil {
				errsSeen <- err
				return
			}
			status, _, err := client.graphRuleMutationDo(context.Background(), http.MethodPatch, target.String(), map[string]any{"call": call})
			if err != nil || status != http.StatusOK {
				errsSeen <- fmt.Errorf("call %d status=%d err=%w", call, status, err)
			}
		}(i)
	}
	wg.Wait()
	close(errsSeen)
	for err := range errsSeen {
		t.Errorf("concurrent mutation: %v", err)
	}

	if shared.Transport != transport || shared.Timeout != 3*time.Second || shared.CheckRedirect == nil ||
		reflect.ValueOf(shared.CheckRedirect).Pointer() != redirectPointer || inheritedRedirectCalls.Load() != 0 {
		t.Fatalf("shared client changed transport=%t timeout=%s redirect=%p calls=%d",
			shared.Transport == transport, shared.Timeout, shared.CheckRedirect, inheritedRedirectCalls.Load())
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.records) != callCount || transport.closes.Load() != callCount || transport.duplicates != 0 {
		t.Fatalf("records=%d closes=%d duplicates=%d", len(transport.records), transport.closes.Load(), transport.duplicates)
	}
	for call := 0; call < callCount; call++ {
		path := fmt.Sprintf("/v1.0/me/mailFolders/inbox/messageRules/rule-%02d%%2Fopaque", call)
		record, ok := transport.records[path]
		if !ok || record.method != http.MethodPatch || record.authorization != "Bearer synthetic-rule-token" || record.call != call {
			t.Fatalf("record %q = %#v, present=%t", path, record, ok)
		}
	}
}

func TestRuleReadCancellationReturnsPromptlyWithoutPartialResult(t *testing.T) {
	started := make(chan struct{})
	client := &Client{
		accessToken:  secretFromString("rule-access-token-12345"),
		tokenType:    "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
		account: "work",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var got mail.RuleListResult
	var err error
	go func() {
		got, err = client.ListRules(ctx, mail.RuleListRequest{})
		close(done)
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not terminate the rule read")
	}
	if err == nil || len(got.Rules) != 0 {
		t.Fatalf("canceled read returned %#v, err=%v", got, err)
	}
	if strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), "graph.example.test") {
		t.Fatalf("cancellation error leaked secret or URL: %v", err)
	}
}

func TestRuleReadSecondPageCancellationClosesBodiesAndReturnsNoPartialResult(t *testing.T) {
	var calls atomic.Int32
	var firstPageCloses atomic.Int32
	var secondPageCloses atomic.Int32
	secondPageStarted := make(chan struct{})
	client := &Client{
		accessToken:  secretFromString("rule-access-token-12345"),
		tokenType:    "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0",
		http: &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch calls.Add(1) {
			case 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: &trackedRuleBody{
						Reader: strings.NewReader(`{"value":[{"id":"first-page-rule","displayName":"First","sequence":1,"isEnabled":true,"conditions":{},"exceptions":{},"actions":{}}],"@odata.nextLink":"https://graph.example.test/v1.0/me/mailFolders/inbox/messageRules?page=2"}`),
						closes: &firstPageCloses,
					},
					Request: r,
				}, nil
			case 2:
				close(secondPageStarted)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       &cancelingRuleBody{ctx: r.Context(), closes: &secondPageCloses},
					Request:    r,
				}, nil
			default:
				return nil, errors.New("unexpected third page request")
			}
		})},
		account: "work",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var got mail.RuleListResult
	var err error
	go func() {
		got, err = client.ListRules(ctx, mail.RuleListRequest{})
		close(done)
	}()
	select {
	case <-secondPageStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("second page did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second-page cancellation did not terminate the rule read")
	}
	if err == nil || len(got.Rules) != 0 {
		t.Fatalf("canceled second page returned %#v, err=%v", got, err)
	}
	if calls.Load() != 2 || firstPageCloses.Load() != 1 || secondPageCloses.Load() != 1 {
		t.Fatalf("requests=%d first closes=%d second closes=%d, want 2/1/1", calls.Load(), firstPageCloses.Load(), secondPageCloses.Load())
	}
	if strings.Contains(err.Error(), "first-page-rule") || strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), "graph.example.test") {
		t.Fatalf("cancellation error leaked partial data, secret, or URL: %v", err)
	}
}

func TestRuleReadRedirectCharacterization(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetCalls atomic.Int32
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				writeJSON(w, map[string]any{"value": []any{}})
			}))
			defer secondary.Close()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", secondary.URL+"/redirected")
				w.WriteHeader(status)
			}))
			defer primary.Close()

			got, err := testRulesClient(primary).ListRules(context.Background(), mail.RuleListRequest{})
			if err == nil || len(got.Rules) != 0 {
				t.Fatalf("redirect unexpectedly succeeded: result=%#v err=%v", got, err)
			}
			if targetCalls.Load() != 0 {
				t.Fatalf("redirect target received %d request(s)", targetCalls.Load())
			}
		})
	}
}

func TestRuleUnknownFieldsAreSafeKindSummaries(t *testing.T) {
	payload := richGraphRule("unknown-kinds")
	payload["@odata.future"] = "ignored"
	payload["futureNull"] = nil
	payload["futureBool"] = true
	payload["futureNumber"] = 12.5
	payload["futureString"] = "raw-value-sentinel"
	payload["futureEmptyArray"] = []any{}
	payload["futureArray"] = []any{"raw-value-sentinel", 2}
	payload["futureEmptyObject"] = map[string]any{}
	payload["futureObject"] = map[string]any{"raw-value-sentinel": true, "second": false}
	payload["z-control\x1b字段"] = "raw-value-sentinel"

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var row graphRule
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	rule := (&Client{account: "work"}).toRule(row, mail.RuleScopeInbox)
	wants := map[string]struct {
		kind  string
		count int
	}{
		"futureNull":        {kind: "null", count: 0},
		"futureBool":        {kind: "bool", count: 1},
		"futureNumber":      {kind: "number", count: 1},
		"futureString":      {kind: "string", count: 1},
		"futureEmptyArray":  {kind: "array", count: 0},
		"futureArray":       {kind: "array", count: 2},
		"futureEmptyObject": {kind: "object", count: 0},
		"futureObject":      {kind: "object", count: 2},
		"z-control\x1b字段":   {kind: "string", count: 1},
	}
	for field, want := range wants {
		var found *mail.RuleUnsupportedField
		for i := range rule.UnsupportedFields {
			item := &rule.UnsupportedFields[i]
			if item.Area == "rule" && item.Field == field {
				found = item
				break
			}
		}
		if found == nil || found.ValueKind != want.kind || found.Count != want.count || strings.Contains(found.Summary, "raw-value-sentinel") {
			t.Fatalf("summary for %q = %#v, want kind=%s count=%d", field, found, want.kind, want.count)
		}
	}
	if hasUnsupported(rule.UnsupportedFields, "rule.@odata.future") {
		t.Fatalf("OData field surfaced: %#v", rule.UnsupportedFields)
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "raw-value-sentinel") || strings.Contains(string(encoded), "provider_state") {
		t.Fatalf("normalized JSON exposed raw unknown value/state: %s", encoded)
	}
	if rule.ProviderMeta["source"] != "microsoft_graph" || len(rule.ProviderMeta) != 1 {
		t.Fatalf("provider metadata = %#v", rule.ProviderMeta)
	}
}

func TestGetRuleDecodeAndProviderErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "malformed", status: http.StatusOK, body: `{"id":`},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":"bad"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := testRulesClient(srv).GetRule(context.Background(), mail.RuleGetRequest{RuleID: "bad"})
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("error leaked secret or URL: %v", err)
			}
		})
	}
}

func TestRuleMutationsCreateUpdateDeleteRequests(t *testing.T) {
	var requests []string
	var createBody map[string]any
	var patchBody map[string]any
	var deleteSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("Authorization") != "Bearer rule-access-token-12345" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/me/mailFolders/inbox/messageRules":
			decodeRequestJSON(t, r, &createBody)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{
				"id": "created-rule", "displayName": createBody["displayName"], "sequence": createBody["sequence"],
				"isEnabled": true, "conditions": createBody["conditions"], "actions": createBody["actions"],
			})
		case r.Method == http.MethodPatch && r.URL.EscapedPath() == "/me/mailFolders/inbox/messageRules/"+url.PathEscape("rule/id"):
			decodeRequestJSON(t, r, &patchBody)
			writeJSON(w, richGraphRule("rule/id"))
		case r.Method == http.MethodDelete && r.URL.EscapedPath() == "/me/mailFolders/inbox/messageRules/"+url.PathEscape("rule/id"):
			deleteSeen = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testRulesClient(srv)
	yes := true
	create, err := client.CreateRule(context.Background(), mail.RuleCreateRequest{Rule: mail.Rule{
		DisplayName: "Create rule",
		Sequence:    3,
		Conditions: mail.RulePredicates{
			FromAddresses:   []mail.Recipient{{Name: "Client", Address: "client@example.test"}},
			SubjectContains: []string{"invoice"},
		},
		Actions: mail.RuleActions{
			MoveToFolder:        &mail.RuleFolderTarget{ProviderFolderID: "archive-id"},
			MarkAsRead:          &yes,
			StopProcessingRules: true,
		},
	}})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if create.RuleID != "created-rule" ||
		createBody["displayName"] != "Create rule" ||
		createBody["isEnabled"] != true ||
		createBody["conditions"].(map[string]any)["subjectContains"] == nil ||
		createBody["actions"].(map[string]any)["moveToFolder"] != "archive-id" {
		t.Fatalf("create result=%#v body=%#v", create, createBody)
	}

	existing := ruleFromGraphFixture(t, client, richGraphRule("rule/id"))
	update, err := client.UpdateRule(context.Background(), mail.RuleUpdateRequest{
		RuleID:   "rule/id",
		Existing: existing,
		ChangeSet: mail.RuleChangeSet{
			Operation:       mail.RuleOperationUpdate,
			SubjectContains: &mail.RuleStringCollectionChange{Set: true, After: []string{"updated"}, Count: 1},
			MoveToFolder:    &mail.RuleFolderTargetChange{Set: true, After: &mail.RuleFolderTarget{ProviderFolderID: "updated-folder"}},
			MarkAsRead:      &mail.RuleBoolFieldChange{Set: true, Cleared: true},
		},
	})
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if update.RuleID != "rule/id" {
		t.Fatalf("update result = %#v", update)
	}
	if _, ok := patchBody["exceptions"]; ok {
		t.Fatalf("patch sent exceptions: %#v", patchBody)
	}
	conditions := patchBody["conditions"].(map[string]any)
	actions := patchBody["actions"].(map[string]any)
	if conditions["providerPredicate"] == nil || conditions["bodyContains"] == nil ||
		conditions["subjectContains"].([]any)[0] != "updated" {
		t.Fatalf("conditions not preserved/replaced: %#v", conditions)
	}
	if actions["providerAction"] == nil || actions["forwardTo"] == nil || actions["assignCategories"] == nil ||
		actions["moveToFolder"] != "updated-folder" {
		t.Fatalf("actions not preserved/replaced: %#v", actions)
	}
	if _, ok := actions["markAsRead"]; ok {
		t.Fatalf("cleared markAsRead still present: %#v", actions)
	}

	deleted, err := client.DeleteRule(context.Background(), mail.RuleDeleteRequest{RuleID: "rule/id"})
	if err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if deleted.RuleID != "rule/id" || !deleteSeen {
		t.Fatalf("delete result=%#v seen=%t", deleted, deleteSeen)
	}
	want := []string{
		"POST /me/mailFolders/inbox/messageRules",
		"PATCH /me/mailFolders/inbox/messageRules/" + url.PathEscape("rule/id"),
		"DELETE /me/mailFolders/inbox/messageRules/" + url.PathEscape("rule/id"),
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("requests = %#v, want %#v", requests, want)
		}
	}
}

func TestRuleUpdatePreservesUnknownNumericValuesExactly(t *testing.T) {
	const fixture = `{
		"id":"rule-with-numbers","displayName":"Numeric preservation","sequence":7,"isEnabled":true,
		"conditions":{"subjectContains":["old"],"providerPredicate":{"large":9007199254740993,"negative":-9007199254740993,"decimal":1.2300e+40,"nested":[9007199254740995]}},
		"actions":{"markAsRead":true}
	}`
	var graph graphRule
	if err := json.Unmarshal([]byte(fixture), &graph); err != nil {
		t.Fatal(err)
	}
	existing := (&Client{account: "work"}).toRule(graph, mail.RuleScopeInbox)
	providerPredicate := existing.ProviderState["conditions"].(map[string]any)["providerPredicate"].(map[string]any)
	for _, field := range []string{"large", "negative", "decimal"} {
		if _, ok := providerPredicate[field].(json.Number); !ok {
			t.Fatalf("preserved numeric field %s has type %T", field, providerPredicate[field])
		}
	}

	payload, err := graphRuleUpdatePayload(mail.RuleUpdateRequest{
		RuleID: "rule-with-numbers", Existing: existing,
		ChangeSet: mail.RuleChangeSet{
			Operation: mail.RuleOperationUpdate,
			SubjectContains: &mail.RuleStringCollectionChange{
				Set: true, Before: []string{"old"}, After: []string{"new"}, Count: 1,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{"9007199254740993", "-9007199254740993", "1.2300e+40", "9007199254740995"} {
		if !bytes.Contains(encoded, []byte(exact)) {
			t.Fatalf("PATCH lost numeric representation %q: %s", exact, encoded)
		}
	}
	if bytes.Contains(encoded, []byte(`9007199254740992`)) {
		t.Fatalf("PATCH rounded an unknown numeric field: %s", encoded)
	}
}

func TestRuleUpdateRequiresProviderStateBeforePatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := testRulesClient(srv)
	_, err := client.UpdateRule(context.Background(), mail.RuleUpdateRequest{
		RuleID:   "rule-without-state",
		Existing: mail.Rule{ID: "rule-without-state"},
		ChangeSet: mail.RuleChangeSet{
			Operation:    mail.RuleOperationUpdate,
			MoveToFolder: &mail.RuleFolderTargetChange{Set: true, After: &mail.RuleFolderTarget{ProviderFolderID: "archive-id"}},
		},
	})
	if err == nil {
		t.Fatal("UpdateRule succeeded without provider state")
	}
	if calls.Load() != 0 {
		t.Fatalf("patch reached server without provider state: %d", calls.Load())
	}
}

func TestRuleUpdateEmptyProviderStateAndScalarPayloads(t *testing.T) {
	client := &Client{account: "work"}

	omitted := ruleFromGraphFixture(t, client, map[string]any{
		"id": "omitted", "displayName": "Omitted", "sequence": 1,
		"isEnabled": true, "actions": map[string]any{},
	})
	if state, ok := omitted.ProviderState["conditions"].(map[string]any); !ok || state == nil || len(state) != 0 {
		t.Fatalf("omitted conditions state = %#v", omitted.ProviderState["conditions"])
	}
	nulls := ruleFromGraphFixture(t, client, map[string]any{
		"id": "nulls", "displayName": "Nulls", "sequence": 1,
		"isEnabled": true, "conditions": nil, "actions": nil,
	})
	if state, ok := nulls.ProviderState["actions"].(map[string]any); !ok || state == nil || len(state) != 0 {
		t.Fatalf("null actions state = %#v", nulls.ProviderState["actions"])
	}

	var patches []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]any
		decodeRequestJSON(t, r, &patch)
		patches = append(patches, patch)
		out := map[string]any{
			"id": "patched", "displayName": "Patched", "sequence": 1,
			"isEnabled": true, "conditions": map[string]any{}, "actions": map[string]any{},
		}
		if r.URL.Path == "/me/mailFolders/inbox/messageRules/omitted" {
			out["id"] = "omitted"
			out["conditions"] = patch["conditions"]
		}
		if r.URL.Path == "/me/mailFolders/inbox/messageRules/nulls" {
			out["id"] = "nulls"
			out["actions"] = patch["actions"]
		}
		writeJSON(w, out)
	}))
	defer srv.Close()
	client = testRulesClient(srv)

	if _, err := client.UpdateRule(context.Background(), mail.RuleUpdateRequest{
		RuleID:   "omitted",
		Existing: omitted,
		ChangeSet: mail.RuleChangeSet{
			Operation:       mail.RuleOperationUpdate,
			SubjectContains: &mail.RuleStringCollectionChange{Set: true, After: []string{"new"}, Count: 1},
		},
	}); err != nil {
		t.Fatalf("condition update with empty provider state: %v", err)
	}
	if len(patches) != 1 || patches[0]["actions"] != nil || patches[0]["exceptions"] != nil ||
		patches[0]["conditions"].(map[string]any)["subjectContains"].([]any)[0] != "new" {
		t.Fatalf("condition patch = %#v", patches)
	}

	yes := true
	if _, err := client.UpdateRule(context.Background(), mail.RuleUpdateRequest{
		RuleID:   "nulls",
		Existing: nulls,
		ChangeSet: mail.RuleChangeSet{
			Operation:  mail.RuleOperationUpdate,
			MarkAsRead: &mail.RuleBoolFieldChange{Set: true, After: &yes},
		},
	}); err != nil {
		t.Fatalf("action update with empty provider state: %v", err)
	}
	if len(patches) != 2 || patches[1]["conditions"] != nil || patches[1]["exceptions"] != nil ||
		patches[1]["actions"].(map[string]any)["markAsRead"] != true {
		t.Fatalf("action patch = %#v", patches)
	}

	if _, err := client.UpdateRule(context.Background(), mail.RuleUpdateRequest{
		RuleID:   "nulls",
		Existing: nulls,
		ChangeSet: mail.RuleChangeSet{
			Operation: mail.RuleOperationDisable,
			IsEnabled: &mail.RuleBoolFieldChange{Set: true, After: boolPtr(false)},
		},
	}); err != nil {
		t.Fatalf("enable update: %v", err)
	}
	if len(patches) != 3 || len(patches[2]) != 1 || patches[2]["isEnabled"] != false {
		t.Fatalf("isEnabled patch = %#v", patches[2])
	}
}

func TestRuleMutationStatusAndDecodeErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		method   string
		status   int
		body     string
		call     func(*Client) error
		wantCode string
		wantExit int
		wantOK   bool
	}{
		{
			name: "create missing", method: http.MethodPost, status: http.StatusNotFound, body: `{"error":"missing"}`,
			call: func(c *Client) error {
				_, err := c.CreateRule(context.Background(), mail.RuleCreateRequest{Rule: mail.Rule{DisplayName: "x", Sequence: 1, Conditions: mail.RulePredicates{SubjectContains: []string{"x"}}, Actions: mail.RuleActions{MarkAsRead: boolPtr(true)}}})
				return err
			},
		},
		{
			name: "update gone", method: http.MethodPatch, status: http.StatusGone, body: `{"error":"gone"}`,
			call: func(c *Client) error {
				_, err := c.UpdateRule(context.Background(), mail.RuleUpdateRequest{
					RuleID:   "rule-id",
					Existing: ruleFromGraphFixture(t, c, richGraphRule("rule-id")),
					ChangeSet: mail.RuleChangeSet{
						Operation: mail.RuleOperationUpdate,
						IsEnabled: &mail.RuleBoolFieldChange{Set: true, After: boolPtr(false)},
					},
				})
				return err
			},
			wantCode: errs.CodeRuleNotFound, wantExit: 5,
		},
		{
			name: "delete gone", method: http.MethodDelete, status: http.StatusGone, body: `{"error":"gone"}`,
			call: func(c *Client) error {
				_, err := c.DeleteRule(context.Background(), mail.RuleDeleteRequest{RuleID: "rule-id"})
				return err
			},
			wantCode: errs.CodeRuleNotFound, wantExit: 5,
		},
		{
			name: "auth repair", method: http.MethodPost, status: http.StatusUnauthorized, body: `{"error":"auth"}`,
			call: func(c *Client) error {
				_, err := c.CreateRule(context.Background(), mail.RuleCreateRequest{Rule: mail.Rule{DisplayName: "x", Sequence: 1, Conditions: mail.RulePredicates{SubjectContains: []string{"x"}}, Actions: mail.RuleActions{MarkAsRead: boolPtr(true)}}})
				return err
			},
			wantCode: errs.CodeAuthRepairRequired, wantExit: 3,
		},
		{
			name: "rate limit", method: http.MethodPatch, status: http.StatusTooManyRequests, body: `{"error":"slow"}`,
			call: func(c *Client) error {
				_, err := c.UpdateRule(context.Background(), mail.RuleUpdateRequest{
					RuleID:   "rule-id",
					Existing: ruleFromGraphFixture(t, c, richGraphRule("rule-id")),
					ChangeSet: mail.RuleChangeSet{
						Operation: mail.RuleOperationDisable,
						IsEnabled: &mail.RuleBoolFieldChange{Set: true, After: boolPtr(false)},
					},
				})
				return err
			},
		},
		{
			name: "malformed success", method: http.MethodPost, status: http.StatusCreated, body: `{"id":`,
			call: func(c *Client) error {
				_, err := c.CreateRule(context.Background(), mail.RuleCreateRequest{Rule: mail.Rule{DisplayName: "x", Sequence: 1, Conditions: mail.RulePredicates{SubjectContains: []string{"x"}}, Actions: mail.RuleActions{MarkAsRead: boolPtr(true)}}})
				return err
			},
		},
		{
			name: "missing returned id", method: http.MethodPatch, status: http.StatusOK, body: `{"displayName":"no id"}`,
			call: func(c *Client) error {
				_, err := c.UpdateRule(context.Background(), mail.RuleUpdateRequest{
					RuleID:   "rule-id",
					Existing: ruleFromGraphFixture(t, c, richGraphRule("rule-id")),
					ChangeSet: mail.RuleChangeSet{
						Operation: mail.RuleOperationDisable,
						IsEnabled: &mail.RuleBoolFieldChange{Set: true, After: boolPtr(false)},
					},
				})
				return err
			},
			wantOK: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var seen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				if r.Method != tt.method {
					t.Fatalf("method = %s, want %s", r.Method, tt.method)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			err := tt.call(testRulesClient(srv))
			if tt.wantOK {
				if err != nil {
					t.Fatalf("accepted PATCH response failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !seen {
				t.Fatal("server was not called")
			}
			if tt.wantCode != "" {
				assertErrCode(t, err, tt.wantCode, tt.wantExit)
			}
			if tt.status == http.StatusTooManyRequests {
				var rgErr interface{ ExitCode() int }
				if !errors.As(err, &rgErr) || rgErr.ExitCode() != rungrad.ExitRateLimited {
					t.Fatalf("rate limit err = %#v", err)
				}
			}
			if strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("error leaked secret or URL: %v", err)
			}
		})
	}
}

func TestRulesGraphErrorMappingAndSecretSafety(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantExit int
	}{
		{name: "malformed", status: http.StatusOK, body: `{"value":`},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":"bad"}`},
		{name: "auth repair", status: http.StatusUnauthorized, body: `{"error":"auth"}`, wantCode: errs.CodeAuthRepairRequired, wantExit: 3},
		{name: "forbidden repair", status: http.StatusForbidden, body: `{"error":"auth"}`, wantCode: errs.CodeAuthRepairRequired, wantExit: 3},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow"}`},
		{name: "server", status: http.StatusInternalServerError, body: `{"error":"server"}`},
		{name: "oversized", status: http.StatusOK, body: `{"value":[],"padding":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := testRulesClient(srv).ListRules(context.Background(), mail.RuleListRequest{})
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.wantCode != "" {
				assertErrCode(t, err, tt.wantCode, tt.wantExit)
			}
			if strings.Contains(err.Error(), "rule-access-token-12345") || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("error leaked secret or URL: %v", err)
			}
		})
	}

	var hostileCalls atomic.Int32
	var hostileAuth atomic.Value
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileCalls.Add(1)
		hostileAuth.Store(r.Header.Get("Authorization"))
		writeJSON(w, map[string]any{"value": []any{}})
	}))
	defer hostile.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"value": []any{}, "@odata.nextLink": hostile.URL + "/rules"})
	}))
	defer srv.Close()
	_, err := testRulesClient(srv).ListRules(context.Background(), mail.RuleListRequest{})
	if err == nil {
		t.Fatal("hostile nextLink succeeded")
	}
	if hostileCalls.Load() != 0 {
		t.Fatalf("hostile nextLink was requested %d times with auth %q", hostileCalls.Load(), hostileAuth.Load())
	}
	if strings.Contains(err.Error(), hostile.URL) || strings.Contains(err.Error(), "rule-access-token-12345") {
		t.Fatalf("hostile nextLink error leaked details: %v", err)
	}

	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rl.Close()
	_, err = testRulesClient(rl).ListRules(context.Background(), mail.RuleListRequest{})
	var rgErr interface{ ExitCode() int }
	if !errors.As(err, &rgErr) || rgErr.ExitCode() != rungrad.ExitRateLimited {
		t.Fatalf("rate limit err = %#v", err)
	}
}

func richGraphRule(id string) map[string]any {
	return map[string]any{
		"@odata.context": "https://graph.microsoft.com/v1.0/$metadata#messageRules/$entity",
		"id":             id,
		"displayName":    "Rich rule",
		"sequence":       1,
		"isEnabled":      true,
		"isReadOnly":     true,
		"hasError":       true,
		"providerFuture": map[string]any{"nested": true},
		"conditions": map[string]any{
			"@odata.type":            "#microsoft.graph.messageRulePredicates",
			"fromAddresses":          []map[string]any{{"emailAddress": map[string]string{"name": "Sender", "address": "sender@example.test"}}},
			"senderContains":         []string{"sender"},
			"recipientContains":      []string{"recipient"},
			"subjectContains":        []string{"invoice", "contract"},
			"headerContains":         []string{"X-Test"},
			"hasAttachments":         true,
			"bodyContains":           []string{"body"},
			"bodyOrSubjectContains":  []string{"either"},
			"categories":             []string{"Blue"},
			"importance":             "high",
			"sensitivity":            "normal",
			"messageActionFlag":      "any",
			"sentToAddresses":        []map[string]any{{"emailAddress": map[string]string{"address": "team@example.test"}}},
			"isApprovalRequest":      false,
			"isAutomaticForward":     true,
			"isAutomaticReply":       false,
			"isEncrypted":            true,
			"isMeetingRequest":       true,
			"isMeetingResponse":      false,
			"isNonDeliveryReport":    true,
			"isPermissionControlled": false,
			"isReadReceipt":          true,
			"isSigned":               false,
			"isVoicemail":            true,
			"notSentToMe":            false,
			"sentCcMe":               true,
			"sentOnlyToMe":           false,
			"sentToMe":               true,
			"sentToOrCcMe":           false,
			"withinSizeRange":        map[string]int{"minimumSize": 10, "maximumSize": 20},
			"providerPredicate":      []any{"x", "y"},
		},
		"exceptions": map[string]any{
			"subjectContains": []string{"except"},
			"exceptionFuture": "x",
		},
		"actions": map[string]any{
			"moveToFolder":          "archive-id",
			"copyToFolder":          "copy-id",
			"markAsRead":            true,
			"stopProcessingRules":   true,
			"assignCategories":      []string{"Review"},
			"delete":                true,
			"permanentDelete":       false,
			"markImportance":        "low",
			"forwardTo":             []map[string]any{{"emailAddress": map[string]string{"address": "outside@example.net"}}},
			"redirectTo":            []map[string]any{{"emailAddress": map[string]string{"address": "redirect@example.net"}}},
			"forwardAsAttachmentTo": []map[string]any{{"emailAddress": map[string]string{"address": "attach@example.net"}}},
			"providerAction":        map[string]any{"future": true},
		},
	}
}

func testRulesClient(srv *httptest.Server) *Client {
	return &Client{
		accessToken:  secretFromString("rule-access-token-12345"),
		tokenType:    "Bearer",
		graphBaseURL: srv.URL,
		http:         srv.Client(),
		account:      "work",
	}
}

func decodeRequestJSON(t *testing.T, r *http.Request, v any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, string(body))
	}
}

func ruleFromGraphFixture(t *testing.T, c *Client, payload map[string]any) mail.Rule {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal rule fixture: %v", err)
	}
	var row graphRule
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("decode rule fixture: %v", err)
	}
	return c.toRule(row, mail.RuleScopeInbox)
}

func hasUnsupported(fields []mail.RuleUnsupportedField, field string) bool {
	for _, item := range fields {
		if item.Area+"."+item.Field == field {
			return true
		}
	}
	return false
}

func boolPtr(v bool) *bool {
	return &v
}

type ruleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ruleRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackedRuleBody struct {
	io.Reader
	closes *atomic.Int32
}

func (b *trackedRuleBody) Close() error {
	b.closes.Add(1)
	return nil
}

type ruleReadError struct{}

func (ruleReadError) Read([]byte) (int, error) {
	return 0, errors.New("synthetic response read failure")
}

func paddedRuleJSONObject(t *testing.T, object string, size int) string {
	t.Helper()
	if !json.Valid([]byte(object)) || !strings.HasSuffix(object, "}") {
		t.Fatalf("invalid base object %q", object)
	}
	prefix := strings.TrimSuffix(object, "}") + `,"padding":"`
	suffix := `"}`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("size %d is too small for padded object", size)
	}
	out := prefix + strings.Repeat("x", padding) + suffix
	if len(out) != size || !json.Valid([]byte(out)) {
		t.Fatalf("padded object size=%d valid=%t, want size %d", len(out), json.Valid([]byte(out)), size)
	}
	return out
}

func exactRuleResponseClient(status int, body string, closes *atomic.Int32) *Client {
	client := &Client{
		accessToken: secretFromString("synthetic-rule-token"), tokenType: "Bearer",
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
	}
	client.http = &http.Client{Transport: ruleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       &trackedRuleBody{Reader: strings.NewReader(body), closes: closes},
			Request:    r,
		}, nil
	})}
	return client
}

func assertRuleReadFailureReason(t *testing.T, err error, want provider.RuleReadFailureReason) {
	t.Helper()
	var readErr *provider.RuleReadFailureError
	if !errors.As(err, &readErr) || readErr.Reason() != want {
		t.Fatalf("read error=%#v, want reason %s", err, want)
	}
}

type concurrentRuleMutationRecord struct {
	method        string
	authorization string
	call          int
}

type concurrentRuleMutationTransport struct {
	mu         sync.Mutex
	records    map[string]concurrentRuleMutationRecord
	duplicates int
	closes     atomic.Int32
}

func (t *concurrentRuleMutationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Call int `json:"call"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	record := concurrentRuleMutationRecord{method: r.Method, authorization: r.Header.Get("Authorization"), call: payload.Call}
	path := r.URL.EscapedPath()
	t.mu.Lock()
	if _, exists := t.records[path]; exists {
		t.duplicates++
	}
	t.records[path] = record
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       &trackedRuleBody{Reader: strings.NewReader("ignored"), closes: &t.closes},
		Request:    r,
	}, nil
}

type cancelingRuleBody struct {
	ctx    context.Context
	closes *atomic.Int32
}

func (b *cancelingRuleBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelingRuleBody) Close() error {
	b.closes.Add(1)
	return nil
}
