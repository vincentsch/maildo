package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// outlookHarness combines the CLI harness with a local Microsoft identity and
// Graph server plus a fixed clock for deterministic credential tests.
type outlookHarness struct {
	*cliHarness
	server *cliOutlookServer
	now    time.Time
}

// cliOutlookServer is a stateful HTTP fixture for Outlook authentication and
// Graph operations. It records request structure while keeping credential
// values out of stored test evidence.
type cliOutlookServer struct {
	t                          *testing.T
	srv                        *httptest.Server
	mu                         sync.Mutex
	profileID                  string
	email                      string
	userPrincipal              string
	displayName                string
	invalidGrant               atomic.Bool
	missingRefresh             atomic.Bool
	refreshMalformed           atomic.Bool
	refreshMalformedStatus     atomic.Int64
	refreshMissingAccess       atomic.Bool
	refreshMissingReplacement  atomic.Bool
	refreshUnderScoped         atomic.Bool
	tokenCalls                 atomic.Int64
	graphCalls                 atomic.Int64
	deviceCodeCalls            atomic.Int64
	devicePolls                atomic.Int64
	mailFolderCalls            atomic.Int64
	archivePolicyCalls         atomic.Int64
	archiveSettingsCalls       atomic.Int64
	archiveFolderCalls         atomic.Int64
	archiveSettingsStatus      atomic.Int64
	archiveFolderStatus        atomic.Int64
	archiveSettingsBody        atomic.Value
	archiveFolderBody          atomic.Value
	archiveFolderArrived       chan struct{}
	archiveFolderRelease       <-chan struct{}
	archiveFolderSignalOnce    sync.Once
	messageCalls               atomic.Int64
	ruleCalls                  atomic.Int64
	ruleWriteStatus            atomic.Int64
	ruleCollectionStatus       atomic.Int64
	malformedRules             atomic.Bool
	oversizedRules             atomic.Bool
	missingRuleMutationID      atomic.Bool
	skipRuleStateUpdate        atomic.Bool
	unreadableRuleMutationBody atomic.Bool
	conflictingRuleResponse    atomic.Bool
	ruleWriteDisconnect        atomic.Bool
	retainClearedRuleKeys      atomic.Bool
	malformedRuleRead          atomic.Bool
	oversizedRuleRead          atomic.Bool
	ruleReadDisconnect         atomic.Bool
	conflictingRuleReadID      atomic.Bool
	rulePostWriteHasError      atomic.Bool
	rulePostWriteReadOnly      atomic.Bool
	rulePostWriteDrop          atomic.Value
	archiveAlias404            atomic.Bool
	hostileNextLink            atomic.Bool
	mailFolderStatus           atomic.Int64
	mailFolderWriteStatus      atomic.Int64
	mismatchedFolderRefreshID  atomic.Bool
	// mailFolderWriteCode lets folder-write tests prove 400 handling branches
	// on Graph error code, not only HTTP status.
	mailFolderWriteCode   atomic.Value
	messageStatus         atomic.Int64
	searchCapHit          atomic.Bool
	inefficientFilter     atomic.Bool
	hostileMessageLink    atomic.Bool
	hostileAttachmentLink atomic.Bool
	failAttachmentPage2   atomic.Bool
	failThreadPage2       atomic.Bool
	malformedMessages     atomic.Bool
	missingMutationID     atomic.Bool
	oversizedMessages     atomic.Bool
	oversizedStatusBodies atomic.Bool
	messageReplyStatus    atomic.Int64
	// These two statuses affect only the write request after the preflight GET,
	// which lets tests model a draft disappearing between check and write.
	messagePatchStatus     atomic.Int64
	messageSendStatus      atomic.Int64
	messageStatusByID      map[string]int
	mailFolderStatusByPath map[string]int
	ruleReadStatusByID     map[string]int
	messageRequests        []messageRequest
	mailFolderRequests     []mailFolderRequest
	ruleRequests           []ruleRequest
	archivePolicyRequests  []archivePolicyRequest
	ruleState              map[string]map[string]any
	ruleBeforePatch        func(*cliOutlookServer, string, map[string]any)
	archiveDisplayName     string
	attachmentState        map[string][]map[string]any
	// draftPatches holds the fields maildo patched into a draft, so later
	// reads of that draft return them, as Graph does.
	draftPatches     map[string]map[string]any
	nextAttachmentID atomic.Int64
	nextRuleID       atomic.Int64
}

func newOutlookHarness(t *testing.T) *outlookHarness {
	return newOutlookHarnessWithSearchCap(t, 0)
}

func newOutlookHarnessWithSearchCap(t *testing.T, searchCap int) *outlookHarness {
	t.Helper()
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	s := &cliOutlookServer{
		t:                      t,
		profileID:              "outlook-work",
		email:                  "work@example.com",
		userPrincipal:          "work@example.com",
		displayName:            "Work User",
		attachmentState:        map[string][]map[string]any{},
		draftPatches:           map[string]map[string]any{},
		messageStatusByID:      map[string]int{},
		mailFolderStatusByPath: map[string]int{},
		ruleReadStatusByID:     map[string]int{},
		ruleState:              defaultGraphRuleState(),
		archiveDisplayName:     "Archive",
	}
	s.archiveSettingsBody.Store([]byte(`{"archiveFolder":"archive-id","userPurpose":{"value":"user"},"automaticRepliesSetting":{"internalReplyMessage":"ignored-reply-body-sentinel"}}`))
	s.archiveFolderBody.Store([]byte(`{"id":"archive-id","displayName":"Archive","totalItemCount":0,"unreadItemCount":0,"childFolderCount":0,"ignored":"folder-sentinel"}`))
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	root := t.TempDir()
	h := &outlookHarness{
		cliHarness: &cliHarness{
			t:          t,
			root:       root,
			configPath: filepath.Join(root, "config.yaml"),
			providers: testkit.NewOutlookRegistry(outlook.Options{
				AuthorizeURL:    s.srv.URL + "/authorize",
				TokenURL:        s.srv.URL + "/token",
				DeviceCodeURL:   s.srv.URL + "/devicecode",
				GraphBaseURL:    s.srv.URL,
				Now:             func() time.Time { return now },
				LoopbackTimeout: time.Second,
				SearchResultCap: searchCap,
				DevicePollSleep: func(context.Context, time.Duration) error { return nil },
			}),
		},
		server: s,
		now:    now,
	}
	return h
}

type messageRequest struct {
	Method      string
	Path        string
	EscapedPath string
	Query       url.Values
	Prefer      string
	Body        []byte
}

type ruleRequest struct {
	Method      string
	Path        string
	EscapedPath string
	Query       url.Values
	Body        []byte
}

// archivePolicyRequest records request structure for assertions without keeping
// bearer-token values or provider response data.
type archivePolicyRequest struct {
	Method               string
	Path                 string
	EscapedPath          string
	RequestURI           string
	Query                url.Values
	Prefer               string
	AuthorizationPresent bool
}

// mailFolderRequest keeps enough request detail for mutation tests to assert
// method, escaped path, query, and write payload while preserving the older
// path-only snapshot helper.
type mailFolderRequest struct {
	Method      string
	Path        string
	EscapedPath string
	Query       url.Values
	Body        []byte
}

func (h *outlookHarness) runOutlook(args ...string) testutil.Result {
	return h.runWith(h.options(h.browserOpener()), args...)
}

func (h *outlookHarness) runNoBrowser(args ...string) testutil.Result {
	return h.runWith(h.options(func(context.Context, string) error {
		h.browserCalls.Add(1)
		return nil
	}), args...)
}

func (h *outlookHarness) runDevice(args ...string) testutil.Result {
	return h.runWith(h.options(func(context.Context, string) error {
		h.browserCalls.Add(1)
		return nil
	}), args...)
}

func (h *outlookHarness) options(opener func(context.Context, string) error) testutil.Options {
	return testutil.Options{
		LookupEnv: func(name string) (string, bool) {
			if name == "MAILDO_OUTLOOK_CLIENT_ID" {
				return "cli-client-id", true
			}
			return "", false
		},
		BrowserOpener: opener,
	}
}

func (h *outlookHarness) browserOpener() func(context.Context, string) error {
	return func(_ context.Context, authorize string) error {
		h.browserCalls.Add(1)
		u, err := url.Parse(authorize)
		if err != nil {
			return err
		}
		q := u.Query()
		callback := q.Get("redirect_uri") + "?code=cli-auth-code-secret-12345&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(callback)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
}

func (s *cliOutlookServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		s.tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			s.t.Errorf("parse form: %v", err)
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			body := map[string]any{
				"access_token": "cli-access-secret-12345",
				"expires_in":   3600,
				"token_type":   "Bearer",
				"scope":        strings.Join(outlook.DefaultScopes, " "),
			}
			if !s.missingRefresh.Load() {
				body["refresh_token"] = "cli-refresh-secret-12345"
			}
			writeCLIJSON(w, body)
		case "refresh_token":
			if s.invalidGrant.Load() {
				w.WriteHeader(http.StatusBadRequest)
				writeCLIJSON(w, map[string]any{"error": "invalid_grant"})
				return
			}
			if s.refreshMalformed.Load() {
				if status := s.refreshMalformedStatus.Load(); status != 0 {
					w.WriteHeader(int(status))
				}
				_, _ = w.Write([]byte(`{"access_token":`))
				return
			}
			body := map[string]any{
				"access_token":  "cli-refreshed-access-secret-12345",
				"refresh_token": "cli-rotated-refresh-secret-12345",
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(outlook.DefaultScopes, " "),
			}
			if s.refreshMissingAccess.Load() {
				delete(body, "access_token")
			}
			if s.refreshMissingReplacement.Load() {
				delete(body, "refresh_token")
			}
			if s.refreshUnderScoped.Load() {
				body["scope"] = "User.Read"
			}
			writeCLIJSON(w, body)
		case "urn:ietf:params:oauth:grant-type:device_code":
			if s.devicePolls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				writeCLIJSON(w, map[string]any{"error": "authorization_pending"})
				return
			}
			writeCLIJSON(w, map[string]any{
				"access_token":  "cli-device-access-secret-12345",
				"refresh_token": "cli-device-refresh-secret-12345",
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(outlook.DefaultScopes, " "),
			})
		default:
			http.Error(w, "bad grant", http.StatusBadRequest)
		}
	case "/devicecode":
		s.deviceCodeCalls.Add(1)
		writeCLIJSON(w, map[string]any{
			"device_code":      "cli-device-code-secret-12345",
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://microsoft.example/device",
			"expires_in":       600,
			"interval":         1,
		})
	case "/me":
		s.graphCalls.Add(1)
		s.mu.Lock()
		profile := map[string]any{"id": s.profileID, "displayName": s.displayName, "mail": s.email, "userPrincipalName": s.userPrincipal}
		s.mu.Unlock()
		writeCLIJSON(w, profile)
	case "/me/mailboxSettings":
		s.handleArchivePolicySettings(w, r)
	default:
		if r.URL.Path == "/me/sendMail" {
			s.handleSendMail(w, r)
			return
		}
		if r.URL.Path == "/me/messages" || strings.HasSuffix(r.URL.Path, "/messages") {
			s.handleMessages(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/me/messages/") {
			s.handleMessageItem(w, r)
			return
		}
		if isMessageRulesPath(r) {
			s.handleRules(w, r)
			return
		}
		if isArchivePolicyFolderRequest(r) {
			s.handleArchivePolicyFolder(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/me/mailFolders") {
			s.handleMailFolders(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

// isArchivePolicyFolderRequest separates the report's minimized exact-id read
// from the broader folder fixture, even though both share the same URL prefix.
func isArchivePolicyFolderRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/me/mailFolders/") &&
		r.URL.Query().Get("$select") == "id,displayName,totalItemCount,unreadItemCount,childFolderCount"
}

// handleArchivePolicySettings serves the configurable first report read and
// records it separately from other Graph traffic.
func (s *cliOutlookServer) handleArchivePolicySettings(w http.ResponseWriter, r *http.Request) {
	s.archivePolicyCalls.Add(1)
	s.archiveSettingsCalls.Add(1)
	s.recordArchivePolicyRequest(r)
	if r.Method != http.MethodGet {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	status := int(s.archiveSettingsStatus.Load())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	body, _ := s.archiveSettingsBody.Load().([]byte)
	_, _ = w.Write(append([]byte(nil), body...))
}

// handleArchivePolicyFolder serves the conditional exact-folder read without
// routing it through the general folder-list fixture.
func (s *cliOutlookServer) handleArchivePolicyFolder(w http.ResponseWriter, r *http.Request) {
	s.archivePolicyCalls.Add(1)
	s.archiveFolderCalls.Add(1)
	s.recordArchivePolicyRequest(r)
	s.mu.Lock()
	arrived := s.archiveFolderArrived
	release := s.archiveFolderRelease
	s.mu.Unlock()
	if arrived != nil {
		s.archiveFolderSignalOnce.Do(func() { close(arrived) })
	}
	if release != nil {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}
	if r.Method != http.MethodGet {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	status := int(s.archiveFolderStatus.Load())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	body, _ := s.archiveFolderBody.Load().([]byte)
	_, _ = w.Write(append([]byte(nil), body...))
}

// recordArchivePolicyRequest records only whether authorization was present;
// the credential value must never enter test diagnostics or snapshots.
func (s *cliOutlookServer) recordArchivePolicyRequest(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archivePolicyRequests = append(s.archivePolicyRequests, archivePolicyRequest{
		Method: r.Method, Path: r.URL.Path, EscapedPath: r.URL.EscapedPath(), RequestURI: r.RequestURI,
		Query: cloneValues(r.URL.Query()), Prefer: r.Header.Get("Prefer"), AuthorizationPresent: r.Header.Get("Authorization") != "",
	})
}

// archivePolicyRequestRecordsSnapshot deep-copies query values so later fixture
// changes cannot alter previously recorded requests.
func (s *cliOutlookServer) archivePolicyRequestRecordsSnapshot() []archivePolicyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]archivePolicyRequest, len(s.archivePolicyRequests))
	for i, request := range s.archivePolicyRequests {
		request.Query = cloneValues(request.Query)
		out[i] = request
	}
	return out
}

func (s *cliOutlookServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/me/messages" {
		s.messageCalls.Add(1)
		body := readRequestBody(s.t, r)
		s.recordMessageRequest(r, body)
		if status := s.messageStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message fixture status")
			return
		}
		w.WriteHeader(http.StatusCreated)
		if s.malformedMessages.Load() {
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		if s.oversizedMessages.Load() {
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		writeCLIJSON(w, graphDraftFixture("draft-created", true, r.Header.Get("Prefer")))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	s.messageCalls.Add(1)
	s.recordMessageRequest(r, nil)
	if status := s.messageStatus.Load(); status != 0 {
		s.writeMessageStatus(w, status, "message fixture status")
		return
	}
	if s.malformedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":[`))
		return
	}
	if s.oversizedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	if s.inefficientFilter.Load() && r.URL.Query().Get("$filter") != "" {
		w.WriteHeader(http.StatusBadRequest)
		writeCLIJSON(w, map[string]any{"error": map[string]any{"code": "InefficientFilter", "message": "bad filter"}})
		return
	}
	if r.URL.Query().Get("page") == "thread2" {
		if s.failThreadPage2.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			writeCLIJSON(w, map[string]any{"error": "thread page failed"})
			return
		}
		writeCLIJSON(w, map[string]any{"value": []map[string]any{
			graphThreadMessageFixture("thread-c", "thread-read-id", "2026-06-28T10:04:00Z"),
		}})
		return
	}
	if r.URL.Query().Get("kind") == "search" && r.URL.Query().Get("page") == "2" {
		writeCLIJSON(w, map[string]any{"value": []map[string]any{
			graphMessageFixture("search-mid", "archive-id", "2026-06-27T08:00:00Z", "Search mid", true, false),
		}})
		return
	}
	if r.URL.Query().Get("$search") != "" {
		if s.searchCapHit.Load() {
			writeCLIJSON(w, map[string]any{
				"value": []map[string]any{
					graphMessageFixture("cap-1", "inbox-id", "2026-06-28T10:00:00Z", "Cap 1", false, true),
					graphMessageFixture("cap-2", "inbox-id", "2026-06-28T09:00:00Z", "Cap 2", false, false),
				},
				"@odata.nextLink": s.nextMessagesLink(r, "?page=cap"),
			})
			return
		}
		writeCLIJSON(w, map[string]any{
			"value": []map[string]any{
				graphMessageFixture("search-old", "inbox-id", "2026-06-20T08:00:00Z", "Search old", true, false),
				graphMessageFixture("search-new", "inbox-id", "2026-06-28T11:00:00Z", "Search new", false, true),
			},
			"@odata.nextLink": s.nextMessagesLink(r, "?page=2&kind=search"),
		})
		return
	}
	filter := r.URL.Query().Get("$filter")
	if strings.HasPrefix(filter, "internetMessageId eq ") {
		switch {
		case strings.Contains(filter, "missing@example.test"):
			writeCLIJSON(w, map[string]any{"value": []map[string]any{}})
		case strings.Contains(filter, "duplicate-next@example.test"):
			writeCLIJSON(w, map[string]any{
				"value":           []map[string]any{graphFullMessageFixture("internet-dupe-a", true, r.Header.Get("Prefer"))},
				"@odata.nextLink": s.nextMessagesLink(r, "?page=internet-dupe"),
			})
		case strings.Contains(filter, "duplicate@example.test"):
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphFullMessageFixture("internet-dupe-a", true, r.Header.Get("Prefer")),
				graphFullMessageFixture("internet-dupe-b", true, r.Header.Get("Prefer")),
			}})
		default:
			out := graphFullMessageFixture("internet-single", true, r.Header.Get("Prefer"))
			if internetID, ok := internetMessageIDFromFilter(filter); ok {
				out["internetMessageId"] = internetID
				out["internetMessageHeaders"] = []map[string]any{
					{"name": "Message-ID", "value": internetID},
					{"name": "X-Fixture", "value": "outlook"},
				}
			}
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				out,
			}})
		}
		return
	}
	if strings.HasPrefix(filter, "conversationId eq ") {
		if strings.Contains(filter, "missing-thread") {
			writeCLIJSON(w, map[string]any{"value": []map[string]any{}})
			return
		}
		conv := "thread-read-id"
		if strings.Contains(filter, "quoted''thread") {
			conv = "quoted'thread"
		}
		if r.URL.Query().Get("page") == "thread2" {
			if s.failThreadPage2.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				writeCLIJSON(w, map[string]any{"error": "thread page failed"})
				return
			}
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphThreadMessageFixture("thread-c", conv, "2026-06-28T10:04:00Z"),
			}})
			return
		}
		writeCLIJSON(w, map[string]any{
			"value": []map[string]any{
				graphThreadMessageFixture("thread-b", conv, "2026-06-28T10:03:00Z"),
				graphThreadMessageFixture("thread-a", conv, "2026-06-28T10:00:00Z"),
			},
			"@odata.nextLink": s.nextMessagesLink(r, "?page=thread2"),
		})
		return
	}
	if r.URL.Query().Get("page") == "2" {
		writeCLIJSON(w, map[string]any{"value": []map[string]any{
			graphMessageFixture("list-old", "inbox-id", "2026-06-20T08:00:00Z", "List old", true, false),
		}})
		return
	}
	rows := []map[string]any{
		graphMessageFixture("list-new", "inbox-id", "2026-06-28T10:00:00Z", "List new", false, true),
		graphMessageFixture("list-mid", "inbox-id", "2026-06-27T10:00:00Z", "List mid", true, false),
		graphMessageFixtureMissingOptional("list-missing", "archive-id"),
	}
	writeCLIJSON(w, map[string]any{
		"value":           rows,
		"@odata.nextLink": s.nextMessagesLink(r, "?page=2"),
	})
}

func (s *cliOutlookServer) nextMessagesLink(r *http.Request, query string) string {
	if s.hostileMessageLink.Load() {
		return "https://example.invalid" + r.URL.Path + query
	}
	return s.srv.URL + r.URL.Path + query
}

func internetMessageIDFromFilter(filter string) (string, bool) {
	const prefix = "internetMessageId eq "
	value, ok := strings.CutPrefix(filter, prefix)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), true
}

func (s *cliOutlookServer) lastMessageRequest() messageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messageRequests) == 0 {
		s.t.Fatal("no message request recorded")
	}
	return s.messageRequests[len(s.messageRequests)-1]
}

func (s *cliOutlookServer) firstMessageRequest() messageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messageRequests) == 0 {
		s.t.Fatal("no message request recorded")
	}
	return s.messageRequests[0]
}

func (s *cliOutlookServer) resetMessageRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageRequests = nil
	s.messageCalls.Store(0)
}

func (s *cliOutlookServer) messageRequestsSnapshot() []messageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]messageRequest, len(s.messageRequests))
	copy(out, s.messageRequests)
	return out
}

func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func isMessageRulesPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/me/mailFolders/") && strings.Contains(r.URL.Path, "/messageRules")
}

func (s *cliOutlookServer) handleRules(w http.ResponseWriter, r *http.Request) {
	s.ruleCalls.Add(1)
	var reqBody []byte
	if r.Method != http.MethodGet {
		reqBody = readRequestBody(s.t, r)
	}
	s.recordRuleRequest(r, reqBody)
	folder, ruleID, collection, ok := messageRulePathParts(r)
	if !ok || folder != string(mail.RuleScopeInbox) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		if status := s.ruleWriteStatus.Load(); status != 0 {
			s.writeRuleStatus(w, status, "rule write fixture status")
			return
		}
	}
	if collection {
		switch r.Method {
		case http.MethodGet:
			if status := s.ruleCollectionStatus.Load(); status != 0 {
				s.writeRuleStatus(w, status, "rule collection fixture status")
				return
			}
			writeCLIJSON(w, map[string]any{"value": s.graphRuleRows()})
		case http.MethodPost:
			s.writeCreatedRule(w, reqBody)
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		if status := s.ruleReadStatusForID(ruleID); status != 0 {
			s.writeRuleStatus(w, int64(status), "rule read fixture status")
			return
		}
		rule, ok := s.graphRuleByID(ruleID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s.ruleReadDisconnect.Load() {
			s.disconnectRuleResponse(w)
			return
		}
		if s.malformedRuleRead.Load() {
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		if s.oversizedRuleRead.Load() {
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		if s.conflictingRuleReadID.Load() {
			rule["id"] = "conflicting-rule-read-id"
		}
		writeCLIJSON(w, rule)
	case http.MethodPatch:
		s.writePatchedRule(w, ruleID, reqBody)
	case http.MethodDelete:
		if !s.deleteGraphRule(ruleID) {
			http.NotFound(w, r)
			return
		}
		if s.ruleWriteDisconnect.Load() {
			s.disconnectRuleResponse(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}

func messageRulePathParts(r *http.Request) (folder, ruleID string, collection, ok bool) {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/me/mailFolders/")
	if escaped == r.URL.EscapedPath() {
		return "", "", false, false
	}
	parts := strings.SplitN(escaped, "/messageRules", 2)
	if len(parts) != 2 {
		return "", "", false, false
	}
	folder, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", false, false
	}
	switch tail := parts[1]; {
	case tail == "":
		return folder, "", true, true
	case strings.HasPrefix(tail, "/"):
		id, err := url.PathUnescape(strings.TrimPrefix(tail, "/"))
		if err != nil {
			return "", "", false, false
		}
		return folder, id, false, true
	default:
		return "", "", false, false
	}
}

func (s *cliOutlookServer) writeCreatedRule(w http.ResponseWriter, body []byte) {
	if s.malformedRules.Load() {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":`))
		return
	}
	if s.oversizedRules.Load() {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	payload := decodeRulePayload(s.t, body)
	id := fmt.Sprintf("created-rule-%d", s.nextRuleID.Add(1))
	rule := graphRuleFromPayload(id, payload)
	s.mu.Lock()
	s.ruleState[id] = cloneRuleJSONMap(rule)
	s.mu.Unlock()
	if s.ruleWriteDisconnect.Load() {
		s.disconnectRuleResponse(w)
		return
	}
	if s.unreadableRuleMutationBody.Load() {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + id + `"`))
		return
	}
	w.WriteHeader(http.StatusCreated)
	if s.missingRuleMutationID.Load() {
		writeCLIJSON(w, map[string]any{"displayName": rule["displayName"]})
		return
	}
	if s.conflictingRuleResponse.Load() {
		writeCLIJSON(w, map[string]any{
			"id": id, "displayName": "response-only-name", "sequence": 2147483647,
			"source": "response-secret-source", "provider_metadata": map[string]any{"source": "response-secret-source", "extra": true},
		})
		return
	}
	writeCLIJSON(w, rule)
}

func (s *cliOutlookServer) writePatchedRule(w http.ResponseWriter, id string, body []byte) {
	payload := decodeRulePayload(s.t, body)
	s.mu.Lock()
	rule, ok := s.ruleState[id]
	if !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if s.ruleBeforePatch != nil {
		s.ruleBeforePatch(s, id, rule)
		s.ruleBeforePatch = nil
	}
	updated := cloneRuleJSONMap(rule)
	for _, key := range []string{"displayName", "sequence", "isEnabled"} {
		if value, ok := payload[key]; ok {
			updated[key] = cloneRuleJSONValue(value)
		}
	}
	for _, key := range []string{"conditions", "actions"} {
		if value, ok := payload[key]; ok {
			replacement := cloneRuleJSONValue(value)
			if s.retainClearedRuleKeys.Load() {
				before, _ := rule[key].(map[string]any)
				after, _ := replacement.(map[string]any)
				for oldKey, oldValue := range before {
					if _, exists := after[oldKey]; !exists {
						after[oldKey] = cloneRuleJSONValue(oldValue)
					}
				}
			}
			updated[key] = replacement
		}
	}
	if s.rulePostWriteHasError.Load() {
		updated["hasError"] = true
	}
	if s.rulePostWriteReadOnly.Load() {
		updated["isReadOnly"] = true
	}
	if path, _ := s.rulePostWriteDrop.Load().(string); path != "" {
		dropGraphRuleField(updated, path)
	}
	if !s.skipRuleStateUpdate.Load() {
		s.ruleState[id] = cloneRuleJSONMap(updated)
	}
	s.mu.Unlock()
	if s.ruleWriteDisconnect.Load() {
		s.disconnectRuleResponse(w)
		return
	}
	if s.malformedRules.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":`))
		return
	}
	if s.oversizedRules.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	if s.missingRuleMutationID.Load() {
		writeCLIJSON(w, map[string]any{"displayName": updated["displayName"]})
		return
	}
	if s.conflictingRuleResponse.Load() {
		writeCLIJSON(w, map[string]any{
			"id": "response-only-id", "displayName": "response-only-name", "sequence": 2147483647,
			"source": "response-secret-source", "provider_metadata": map[string]any{"source": "response-secret-source", "extra": true},
		})
		return
	}
	writeCLIJSON(w, updated)
}

func dropGraphRuleField(rule map[string]any, path string) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		delete(rule, path)
		return
	}
	current := rule
	for _, part := range parts[:len(parts)-1] {
		next, _ := current[part].(map[string]any)
		if next == nil {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func (s *cliOutlookServer) disconnectRuleResponse(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		s.t.Error("rule fixture response writer cannot hijack")
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		s.t.Errorf("hijack rule fixture connection: %v", err)
		return
	}
	_ = conn.Close()
}

func (s *cliOutlookServer) writeRuleStatus(w http.ResponseWriter, status int64, msg string) {
	w.WriteHeader(int(status))
	if s.oversizedStatusBodies.Load() {
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	writeCLIJSON(w, map[string]any{"error": map[string]any{"code": "ErrorFixture", "message": msg}})
}

func decodeRulePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if len(body) == 0 {
		return payload
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Errorf("decode rule payload: %v", err)
	}
	return payload
}

func graphRuleFromPayload(id string, payload map[string]any) map[string]any {
	displayName, _ := payload["displayName"].(string)
	if displayName == "" {
		displayName = "Created rule"
	}
	sequence := payload["sequence"]
	if sequence == nil {
		sequence = float64(1)
	}
	isEnabled, _ := payload["isEnabled"].(bool)
	conditions, _ := payload["conditions"].(map[string]any)
	actions, _ := payload["actions"].(map[string]any)
	return graphRuleFixture(id, displayName, sequence, isEnabled, conditions, actions)
}

func (s *cliOutlookServer) graphRuleRows() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]map[string]any, 0, len(s.ruleState))
	for _, rule := range s.ruleState {
		rows = append(rows, cloneRuleJSONMap(rule))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := ruleSequenceValue(rows[i]), ruleSequenceValue(rows[j])
		if left == right {
			return fmt.Sprint(rows[i]["id"]) < fmt.Sprint(rows[j]["id"])
		}
		return left < right
	})
	return rows
}

func (s *cliOutlookServer) graphRuleByID(id string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.ruleState[id]
	if !ok {
		return nil, false
	}
	return cloneRuleJSONMap(rule), true
}

func (s *cliOutlookServer) deleteGraphRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ruleState[id]; !ok {
		return false
	}
	delete(s.ruleState, id)
	return true
}

func ruleSequenceValue(rule map[string]any) int {
	switch v := rule["sequence"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func (s *cliOutlookServer) recordRuleRequest(r *http.Request, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleRequests = append(s.ruleRequests, ruleRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		EscapedPath: r.URL.EscapedPath(),
		Query:       cloneValues(r.URL.Query()),
		Body:        append([]byte(nil), body...),
	})
}

func (s *cliOutlookServer) resetRuleRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleRequests = nil
	s.ruleCalls.Store(0)
}

func (s *cliOutlookServer) ruleRequestRecordsSnapshot() []ruleRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ruleRequest, len(s.ruleRequests))
	for i, req := range s.ruleRequests {
		req.Query = cloneValues(req.Query)
		req.Body = append([]byte(nil), req.Body...)
		out[i] = req
	}
	return out
}

func (s *cliOutlookServer) setRuleReadStatus(id string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleReadStatusByID[id] = status
}

func (s *cliOutlookServer) setRuleBeforePatch(hook func(*cliOutlookServer, string, map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleBeforePatch = hook
}

func (s *cliOutlookServer) archiveFolderName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.archiveDisplayName
}

func (s *cliOutlookServer) ruleReadStatusForID(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ruleReadStatusByID[id]
}

func defaultGraphRuleState() map[string]map[string]any {
	return map[string]map[string]any{
		"rule-move-client": graphRuleFixtureWithExceptions("rule-move-client", "Move client mail", 1, true,
			map[string]any{
				"fromAddresses": []any{
					map[string]any{"emailAddress": map[string]any{"name": "Client", "address": "client@example.test"}},
				},
				"bodyContains":      []any{"body"},
				"subjectContains":   []any{"invoice", "contract"},
				"providerPredicate": []any{"preserved"},
			},
			map[string]any{
				"subjectContains": []any{"skip"},
				"exceptionFuture": "preserved",
			},
			map[string]any{
				"moveToFolder":        "acme-id",
				"copyToFolder":        "archive-id",
				"markAsRead":          true,
				"markImportance":      "low",
				"stopProcessingRules": true,
				"assignCategories":    []any{"Review"},
				"forwardTo": []any{
					map[string]any{"emailAddress": map[string]any{"address": "ops@external.test"}},
				},
				"providerAction": map[string]any{"preserved": true},
			}),
		"rule-forward-alert": graphRuleFixture("rule-forward-alert", "Forward alerts", 2, false,
			map[string]any{
				"senderContains": []any{"alerts"},
				"hasAttachments": true,
			},
			map[string]any{
				"forwardTo": []any{
					map[string]any{"emailAddress": map[string]any{"address": "ops@external.test"}},
				},
			}),
	}
}

func graphRuleFixture(id, display string, sequence any, enabled bool, conditions, actions map[string]any) map[string]any {
	return graphRuleFixtureWithExceptions(id, display, sequence, enabled, conditions, nil, actions)
}

func graphRuleFixtureWithExceptions(id, display string, sequence any, enabled bool, conditions, exceptions, actions map[string]any) map[string]any {
	if conditions == nil {
		conditions = map[string]any{}
	}
	if exceptions == nil {
		exceptions = map[string]any{}
	}
	if actions == nil {
		actions = map[string]any{}
	}
	return map[string]any{
		"id":          id,
		"displayName": display,
		"sequence":    sequence,
		"isEnabled":   enabled,
		"isReadOnly":  false,
		"hasError":    false,
		"conditions":  cloneRuleJSONMap(conditions),
		"exceptions":  cloneRuleJSONMap(exceptions),
		"actions":     cloneRuleJSONMap(actions),
	}
}

func cloneRuleJSONMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneRuleJSONValue(v)
	}
	return out
}

func cloneRuleJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneRuleJSONMap(x)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, v := range x {
			out[k] = v
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneRuleJSONValue(item)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, item := range x {
			out[i] = cloneRuleJSONMap(item)
		}
		return out
	default:
		return x
	}
}

func (s *cliOutlookServer) handleMailFolders(w http.ResponseWriter, r *http.Request) {
	s.mailFolderCalls.Add(1)
	var reqBody []byte
	if r.Method != http.MethodGet {
		reqBody = readRequestBody(s.t, r)
	}
	s.recordMailFolderRequest(r, reqBody)
	if status := s.mailFolderStatus.Load(); status != 0 {
		s.writeMailFolderStatus(w, int(status), "mail folder fixture status", "")
		return
	}
	if r.Method != http.MethodGet {
		if status := s.mailFolderWriteStatus.Load(); status != 0 {
			s.writeMailFolderStatus(w, int(status), "mail folder write fixture status", s.mailFolderWriteErrorCode())
			return
		}
	}
	path := r.URL.Path
	if path == "/me/mailFolders" {
		if r.Method == http.MethodPost {
			s.writeCreatedFolder(w, reqBody, "msgfolderroot")
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("page") {
		case "2":
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphFolderFixture("reports-1", "Reports", "msgfolderroot", 5, 0, 0),
				graphFolderFixture("reports-2", "Reports", "msgfolderroot", 6, 0, 0),
				graphFolderFixture("id/with/slash", "Slash ID", "msgfolderroot", 1, 0, 0),
				graphFolderFixture("opaque-ab-id", "a/b", "msgfolderroot", 1, 0, 0),
			}})
		default:
			writeCLIJSON(w, map[string]any{
				"value": []map[string]any{
					graphFolderFixture("inbox-id", "Inbox", "msgfolderroot", 10, 2, 0),
					graphFolderFixture("drafts-id", "Drafts", "msgfolderroot", 1, 0, 0),
					graphFolderFixture("sent-id", "Gesendete Elemente", "msgfolderroot", 7, 0, 0),
					graphFolderFixture("deleted-id", "Deleted Items", "msgfolderroot", 3, 0, 0),
					graphFolderFixture("archive-id", s.archiveFolderName(), "msgfolderroot", 4, 0, 0),
					graphFolderFixture("junk-id", "Junk Email", "msgfolderroot", 2, 1, 0),
					graphFolderFixture("outbox-id", "Outbox", "msgfolderroot", 0, 0, 0),
					graphFolderFixture("positive-counts-id", "Positive Counts", "msgfolderroot", 9, 0, 2),
					graphFolderFixture("zero-counts-id", "Zero Counts", "msgfolderroot", 0, 0, 0),
					graphFolderFixturePartialCounts("total-only-id", "Total Only", "msgfolderroot", intPointer(4), nil),
					graphFolderFixturePartialCounts("child-only-id", "Child Only", "msgfolderroot", nil, intPointer(3)),
					graphFolderFixtureNoCounts("null-total-id", "Null Total", "msgfolderroot"),
					graphFolderFixtureNoCounts("null-child-id", "Null Child", "msgfolderroot"),
					graphFolderFixtureNoCounts("count-free-id", "Count Free", "msgfolderroot"),
					graphFolderFixture("clients-id", "Clients", "msgfolderroot", 0, 0, 1),
				},
				"@odata.nextLink": s.nextMailFoldersLink(),
			})
		}
		return
	}
	remainder, ok := mailFolderRemainder(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if status := s.mailFolderStatusForPath(remainder); status != 0 {
		s.writeMailFolderStatus(w, status, "mail folder path fixture status", "")
		return
	}
	if strings.HasSuffix(remainder, "/childFolders") {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSuffix(remainder, "/childFolders")
		if r.Method == http.MethodPost {
			s.writeCreatedFolder(w, reqBody, id)
			return
		}
		if id == "positive-counts-id" {
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphFolderFixturePartialCounts("positive-child-a", "Child A", id, nil, intPointer(0)),
				graphFolderFixture("positive-child-b", "Child B", id, 1, 0, 0),
			}})
			return
		}
		if id == "child-only-id" {
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphFolderFixture("child-only-a", "Child One", id, 0, 0, 0),
				graphFolderFixture("child-only-b", "Child Two", id, 0, 0, 0),
				graphFolderFixture("child-only-c", "Child Three", id, 0, 0, 0),
			}})
			return
		}
		if id != "clients-id" {
			http.NotFound(w, r)
			return
		}
		writeCLIJSON(w, map[string]any{"value": []map[string]any{
			graphFolderFixture("acme-id", "ACME", "clients-id", 8, 1, 0),
		}})
		return
	}
	if r.Method == http.MethodPatch {
		s.writeRenamedFolder(w, reqBody, remainder)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	switch remainder {
	case "inbox":
		writeCLIJSON(w, graphFolderFixture("inbox-id", "Inbox", "msgfolderroot", 10, 2, 0))
	case "drafts":
		writeCLIJSON(w, graphFolderFixture("drafts-id", "Drafts", "msgfolderroot", 1, 0, 0))
	case "sentitems":
		writeCLIJSON(w, graphFolderFixture("sent-id", "Gesendete Elemente", "msgfolderroot", 7, 0, 0))
	case "deleteditems":
		writeCLIJSON(w, graphFolderFixture("deleted-id", "Deleted Items", "msgfolderroot", 3, 0, 0))
	case "archive":
		if s.archiveAlias404.Load() {
			http.NotFound(w, r)
			return
		}
		writeCLIJSON(w, graphFolderFixture("archive-id", s.archiveFolderName(), "msgfolderroot", 4, 0, 0))
	case "junkemail":
		writeCLIJSON(w, graphFolderFixture("junk-id", "Junk Email", "msgfolderroot", 2, 1, 0))
	case "outbox":
		writeCLIJSON(w, graphFolderFixture("outbox-id", "Outbox", "msgfolderroot", 0, 0, 0))
	case "conversationhistory":
		writeCLIJSON(w, graphFolderFixture("conversationhistory-id", "Conversation History", "msgfolderroot", 0, 0, 0))
	case "clients-id":
		if s.mismatchedFolderRefreshID.Load() {
			writeCLIJSON(w, graphFolderFixture("different-folder-id", "Different", "msgfolderroot", 99, 0, 8))
			return
		}
		writeCLIJSON(w, graphFolderFixture("clients-id", "Clients", "msgfolderroot", 0, 0, 1))
	case "acme-id":
		writeCLIJSON(w, graphFolderFixture("acme-id", "ACME", "clients-id", 8, 1, 0))
	case "reports-1":
		writeCLIJSON(w, graphFolderFixture("reports-1", "Reports", "msgfolderroot", 5, 0, 0))
	case "reports-2":
		writeCLIJSON(w, graphFolderFixture("reports-2", "Reports", "msgfolderroot", 6, 0, 0))
	case "id/with/slash":
		writeCLIJSON(w, graphFolderFixture("id/with/slash", "Slash ID", "msgfolderroot", 1, 0, 0))
	case "count-free-id":
		writeCLIJSON(w, graphFolderFixtureNoCounts("count-free-id", "Count Free", "msgfolderroot"))
	case "positive-counts-id":
		writeCLIJSON(w, graphFolderFixture("positive-counts-id", "Positive Counts", "msgfolderroot", 9, 0, 2))
	case "zero-counts-id":
		if s.mismatchedFolderRefreshID.Load() {
			writeCLIJSON(w, graphFolderFixture("different-folder-id", "Different", "msgfolderroot", 0, 0, 0))
			return
		}
		writeCLIJSON(w, graphFolderFixture("zero-counts-id", "Zero Counts", "msgfolderroot", 0, 0, 0))
	case "total-only-id":
		writeCLIJSON(w, graphFolderFixturePartialCounts("total-only-id", "Total Only", "msgfolderroot", intPointer(4), nil))
	case "child-only-id":
		writeCLIJSON(w, graphFolderFixturePartialCounts("child-only-id", "Child Only", "msgfolderroot", nil, intPointer(3)))
	case "null-total-id":
		writeCLIJSON(w, map[string]any{"id": "null-total-id", "displayName": "Null Total", "parentFolderId": "msgfolderroot", "totalItemCount": nil, "childFolderCount": 5})
	case "null-child-id":
		writeCLIJSON(w, map[string]any{"id": "null-child-id", "displayName": "Null Child", "parentFolderId": "msgfolderroot", "totalItemCount": 6, "childFolderCount": nil})
	default:
		http.NotFound(w, r)
	}
}

func (s *cliOutlookServer) writeCreatedFolder(w http.ResponseWriter, body []byte, parentID string) {
	if s.malformedMessages.Load() {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":`))
		return
	}
	if s.oversizedMessages.Load() {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	payload := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			s.t.Errorf("decode folder create payload: %v", err)
		}
	}
	display, _ := payload["displayName"].(string)
	if display == "" {
		display = "Created"
	}
	w.WriteHeader(http.StatusCreated)
	if s.missingMutationID.Load() {
		writeCLIJSON(w, map[string]any{"displayName": display})
		return
	}
	writeCLIJSON(w, graphFolderFixture("created-"+folderFixtureSlug(display), display, parentID, 0, 0, 0))
}

func (s *cliOutlookServer) writeRenamedFolder(w http.ResponseWriter, body []byte, id string) {
	if s.malformedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":`))
		return
	}
	if s.oversizedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	payload := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			s.t.Errorf("decode folder rename payload: %v", err)
		}
	}
	display, _ := payload["displayName"].(string)
	if display == "" {
		display = "Renamed"
	}
	w.WriteHeader(http.StatusOK)
	if s.missingMutationID.Load() {
		writeCLIJSON(w, map[string]any{"displayName": display})
		return
	}
	writeCLIJSON(w, graphFolderFixture(id, display, "msgfolderroot", 0, 0, 0))
}

// writeMailFolderStatus emits the narrow Graph error envelope the folder
// mutation adapter inspects, while still allowing oversized status-body tests.
func (s *cliOutlookServer) writeMailFolderStatus(w http.ResponseWriter, status int, msg, graphCode string) {
	w.WriteHeader(status)
	if s.oversizedStatusBodies.Load() {
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	code := graphCode
	if code == "" {
		code = "ErrorFixture"
	}
	if status == http.StatusBadRequest && graphCode == "" {
		code = "ErrorInvalidPropertyValue"
	}
	writeCLIJSON(w, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (s *cliOutlookServer) mailFolderWriteErrorCode() string {
	value := s.mailFolderWriteCode.Load()
	if value == nil {
		return ""
	}
	code, _ := value.(string)
	return code
}

func (s *cliOutlookServer) setMailFolderStatus(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailFolderStatusByPath[path] = status
}

func (s *cliOutlookServer) mailFolderStatusForPath(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mailFolderStatusByPath[path]
}

func mailFolderRemainder(r *http.Request) (string, bool) {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/me/mailFolders/")
	if escaped == r.URL.EscapedPath() {
		return "", false
	}
	out, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false
	}
	return out, true
}

func (s *cliOutlookServer) recordMailFolderRequest(r *http.Request, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailFolderRequests = append(s.mailFolderRequests, mailFolderRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		EscapedPath: r.URL.EscapedPath(),
		Query:       cloneValues(r.URL.Query()),
		Body:        append([]byte(nil), body...),
	})
}

func (s *cliOutlookServer) mailFolderRequestsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.mailFolderRequests))
	for i, req := range s.mailFolderRequests {
		out[i] = req.Path
	}
	return out
}

func (s *cliOutlookServer) mailFolderRequestRecordsSnapshot() []mailFolderRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mailFolderRequest, len(s.mailFolderRequests))
	for i, req := range s.mailFolderRequests {
		req.Query = cloneValues(req.Query)
		req.Body = append([]byte(nil), req.Body...)
		out[i] = req
	}
	return out
}

func (s *cliOutlookServer) handleMessageItem(w http.ResponseWriter, r *http.Request) {
	s.messageCalls.Add(1)
	var reqBody []byte
	if r.Method == http.MethodPatch || r.Method == http.MethodPost {
		reqBody = readRequestBody(s.t, r)
	}
	s.recordMessageRequest(r, reqBody)
	path := strings.TrimPrefix(r.URL.Path, "/me/messages/")
	if status := s.messageStatusForPath(path); status != 0 {
		s.writeMessageStatus(w, int64(status), "message fixture status")
		return
	}
	if r.Method == http.MethodPatch {
		if status := s.messagePatchStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message patch fixture status")
			return
		}
	}
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/send") {
		if status := s.messageSendStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message send fixture status")
			return
		}
	}
	if r.Method == http.MethodDelete && !strings.Contains(path, "/attachments/") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && (strings.HasSuffix(path, "/move") || strings.HasSuffix(path, "/copy")) {
		if status := s.messageStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message fixture status")
			return
		}
		if s.oversizedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		if s.malformedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		if s.missingMutationID.Load() {
			w.WriteHeader(http.StatusCreated)
			writeCLIJSON(w, map[string]any{"internetMessageId": "<fixture@example.test>"})
			return
		}
		payload := map[string]any{}
		if len(reqBody) > 0 {
			if err := json.Unmarshal(reqBody, &payload); err != nil {
				s.t.Errorf("decode mutation payload: %v", err)
			}
		}
		destinationID, _ := payload["destinationId"].(string)
		sourcePath := strings.TrimSuffix(strings.TrimSuffix(path, "/move"), "/copy")
		sourceID, _ := url.PathUnescape(sourcePath)
		resultID := sourceID
		if strings.HasSuffix(path, "/copy") {
			resultID = "copy-" + sourceID
		}
		out := graphFullMessageFixture(resultID, false, r.Header.Get("Prefer"))
		out["parentFolderId"] = destinationID
		w.WriteHeader(http.StatusCreated)
		writeCLIJSON(w, out)
		return
	}
	if r.Method == http.MethodPost && (strings.HasSuffix(path, "/createReply") || strings.HasSuffix(path, "/createReplyAll")) {
		if status := s.messageReplyStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message reply fixture status")
			return
		}
		if status := s.messageStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message fixture status")
			return
		}
		if s.oversizedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		if s.malformedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		sourcePath := strings.TrimSuffix(strings.TrimSuffix(path, "/createReply"), "/createReplyAll")
		sourceID, _ := url.PathUnescape(sourcePath)
		w.WriteHeader(http.StatusCreated)
		writeCLIJSON(w, graphReplyDraftFixture("reply-"+sourceID, r.Header.Get("Prefer")))
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/createForward") {
		if status := s.messageReplyStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message forward fixture status")
			return
		}
		if status := s.messageStatus.Load(); status != 0 {
			s.writeMessageStatus(w, status, "message fixture status")
			return
		}
		if s.oversizedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		if s.malformedMessages.Load() {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		sourcePath := strings.TrimSuffix(path, "/createForward")
		sourceID, _ := url.PathUnescape(sourcePath)
		draftID := "forward-" + sourceID
		s.seedForwardDraftAttachments(draftID, sourceID)
		w.WriteHeader(http.StatusCreated)
		writeCLIJSON(w, map[string]any{"id": draftID})
		return
	}
	if status := s.messageStatus.Load(); status != 0 {
		s.writeMessageStatus(w, status, "message fixture status")
		return
	}
	if s.oversizedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	if s.malformedMessages.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":`))
		return
	}
	if strings.HasSuffix(path, "/send") {
		if r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if r.Method == http.MethodPatch {
		id, _ := url.PathUnescape(path)
		if strings.HasPrefix(id, "reply-") || strings.HasPrefix(id, "forward-") || strings.HasPrefix(id, "draft") {
			patch := map[string]any{}
			if json.Unmarshal(reqBody, &patch) == nil {
				s.mu.Lock()
				stored := s.draftPatches[id]
				if stored == nil {
					stored = map[string]any{}
				}
				for k, v := range patch {
					stored[k] = v
				}
				s.draftPatches[id] = stored
				s.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		if s.malformedMessages.Load() {
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		if s.oversizedMessages.Load() {
			_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
			return
		}
		if s.missingMutationID.Load() {
			writeCLIJSON(w, map[string]any{"categories": []string{"Graph"}})
			return
		}
		out := graphFullMessageFixture(id, false, r.Header.Get("Prefer"))
		payload := map[string]any{}
		if len(reqBody) > 0 {
			if err := json.Unmarshal(reqBody, &payload); err != nil {
				s.t.Errorf("decode patch payload: %v", err)
			}
		}
		if value, ok := payload["isRead"].(bool); ok {
			out["isRead"] = value
		}
		if raw, ok := payload["categories"].([]any); ok {
			categories := make([]string, 0, len(raw))
			for _, item := range raw {
				if s, ok := item.(string); ok {
					categories = append(categories, s)
				}
			}
			out["categories"] = categories
		}
		if raw, ok := payload["flag"].(map[string]any); ok {
			status, _ := raw["flagStatus"].(string)
			out["flag"] = map[string]any{"flagStatus": status}
		}
		writeCLIJSON(w, out)
		return
	}
	if r.Method != http.MethodGet && !strings.Contains(path, "/attachments") {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	if strings.Contains(path, "/attachments/") && strings.HasSuffix(path, "/$value") {
		parts := strings.SplitN(path, "/attachments/", 2)
		messageID, _ := url.PathUnescape(parts[0])
		attachmentPath := strings.TrimSuffix(parts[1], "/$value")
		attachmentID, _ := url.PathUnescape(attachmentPath)
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		switch attachmentID {
		case "missing-download":
			http.NotFound(w, r)
		case "gone-download":
			w.WriteHeader(http.StatusGone)
			writeCLIJSON(w, map[string]any{"error": "gone"})
		case "auth-401":
			w.WriteHeader(http.StatusUnauthorized)
			writeCLIJSON(w, map[string]any{"error": "unauthorized"})
		case "auth-403":
			w.WriteHeader(http.StatusForbidden)
			writeCLIJSON(w, map[string]any{"error": "forbidden"})
		case "rate-download":
			w.WriteHeader(http.StatusTooManyRequests)
			writeCLIJSON(w, map[string]any{"error": "rate limited"})
		case "server-download":
			s.writeMessageStatus(w, http.StatusInternalServerError, "attachment download fixture status")
		case "oversized-download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", mail.MaxMessageAttachmentDownloadBytes+1))
			w.WriteHeader(http.StatusOK)
		case "short-download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
		case "zero-download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("bytes for " + messageID + "/" + attachmentID))
		}
		return
	}
	if strings.HasSuffix(path, "/$value") {
		id := strings.TrimSuffix(path, "/$value")
		id, _ = url.PathUnescape(id)
		switch id {
		case "raw-gone":
			w.WriteHeader(http.StatusGone)
			writeCLIJSON(w, map[string]any{"error": "gone"})
		case "raw-auth":
			w.WriteHeader(http.StatusForbidden)
			writeCLIJSON(w, map[string]any{"error": "forbidden"})
		case "raw-unsupported":
			w.WriteHeader(http.StatusNotAcceptable)
			writeCLIJSON(w, map[string]any{"error": "not available"})
		case "raw-rate":
			w.WriteHeader(http.StatusTooManyRequests)
			writeCLIJSON(w, map[string]any{"error": "rate limited"})
		case "raw-500":
			w.WriteHeader(http.StatusInternalServerError)
			writeCLIJSON(w, map[string]any{"error": "server"})
		case "raw-oversized":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("x", 1<<20+1)))
		case "raw-binary":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0xff, 0x00, 0x01})
		default:
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = w.Write([]byte("Subject: Raw\r\n\r\nRaw body for " + id))
		}
		return
	}
	if strings.Contains(path, "/attachments/") {
		parts := strings.Split(path, "/attachments/")
		draftID, _ := url.PathUnescape(parts[0])
		attachmentID, _ := url.PathUnescape(parts[1])
		if r.Method != http.MethodDelete {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if status := s.messageSendStatus.Load(); status != 0 {
			w.WriteHeader(int(status))
			writeCLIJSON(w, map[string]any{"error": "attachment delete fixture status"})
			return
		}
		if !s.deleteAttachment(draftID, attachmentID) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasSuffix(path, "/attachments") {
		idPath := strings.TrimSuffix(path, "/attachments")
		messageID, _ := url.PathUnescape(idPath)
		if r.Method == http.MethodPost {
			payload := map[string]any{}
			if len(reqBody) > 0 {
				if err := json.Unmarshal(reqBody, &payload); err != nil {
					s.t.Errorf("decode upload payload: %v", err)
				}
			}
			name, _ := payload["name"].(string)
			contentType, _ := payload["contentType"].(string)
			contentBytes, _ := payload["contentBytes"].(string)
			att := graphAttachmentFixtureTyped(
				fmt.Sprintf("uploaded-%d", s.nextAttachmentID.Add(1)),
				name,
				contentType,
				int64(len(contentBytes)),
				false,
				"",
				"#microsoft.graph.fileAttachment",
			)
			s.addAttachment(messageID, att)
			w.WriteHeader(http.StatusCreated)
			writeCLIJSON(w, att)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		// Graph answers like this when $select names a property of a
		// derived attachment type without its type (seen on 2026-10-29).
		for _, field := range strings.Split(r.URL.Query().Get("$select"), ",") {
			if field == "contentId" {
				w.WriteHeader(http.StatusBadRequest)
				writeCLIJSON(w, map[string]any{"error": map[string]any{"code": "BadRequest", "message": "Parsing OData Select and Expand failed: Could not find a property named 'contentId' on type 'microsoft.graph.attachment'."}})
				return
			}
		}
		switch messageID {
		case "attachments-missing":
			http.NotFound(w, r)
			return
		case "attachments-gone":
			w.WriteHeader(http.StatusGone)
			writeCLIJSON(w, map[string]any{"error": "gone"})
			return
		}
		if special, ok := s.attachmentsFor(messageID); ok {
			writeCLIJSON(w, map[string]any{"value": special})
			return
		}
		if r.URL.Query().Get("page") == "2" {
			if s.failAttachmentPage2.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				writeCLIJSON(w, map[string]any{"error": "attachment page failed"})
				return
			}
			writeCLIJSON(w, map[string]any{"value": []map[string]any{
				graphAttachmentFixture("att-2", "inline.png", "image/png", 320, true, "cid-inline"),
			}})
			return
		}
		nextLink := s.srv.URL + r.URL.Path + "?page=2"
		if s.hostileAttachmentLink.Load() {
			nextLink = "https://example.invalid" + r.URL.Path + "?page=2"
		}
		writeCLIJSON(w, map[string]any{
			"value": []map[string]any{
				graphAttachmentFixture("att-1", "contract.pdf", "application/pdf", 1200, false, ""),
			},
			"@odata.nextLink": nextLink,
		})
		return
	}
	id, _ := url.PathUnescape(path)
	switch id {
	case "missing", "raw-missing":
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.URL.Query().Get("$select"), "isDraft") {
		var m map[string]any
		switch {
		case strings.HasPrefix(id, "reply-"):
			m = graphReplyDraftFixture(id, r.Header.Get("Prefer"))
		case strings.HasPrefix(id, "forward-"):
			m = graphForwardDraftFixture(id, r.Header.Get("Prefer"))
		default:
			isDraft := strings.HasPrefix(id, "draft") || id == "raw-binary" || id == "raw-gone"
			if id == "not-a-draft" {
				isDraft = false
			}
			m = graphDraftFixture(id, isDraft, r.Header.Get("Prefer"))
		}
		s.mu.Lock()
		for k, v := range s.draftPatches[id] {
			if k != "body" || r.Header.Get("Prefer") == "" {
				m[k] = v
			}
		}
		s.mu.Unlock()
		if strings.Contains(r.URL.Query().Get("$expand"), "attachments") {
			m["attachments"] = s.expandedAttachments(id, m["hasAttachments"] == true)
			m["singleValueExtendedProperties"] = []map[string]any{
				{"id": "String 0x1042", "value": "<source-" + id + "@example.test>"},
			}
		}
		writeCLIJSON(w, m)
		return
	}
	if s.missingMutationID.Load() && strings.Contains(r.URL.Query().Get("$select"), "categories") {
		writeCLIJSON(w, map[string]any{"categories": []string{"Graph"}})
		return
	}
	writeCLIJSON(w, graphFullMessageFixture(id, id != "no-attachments", r.Header.Get("Prefer")))
}

// expandedAttachments is what Graph returns for $expand=attachments: the
// attachment rows with their content in contentBytes.
func (s *cliOutlookServer) expandedAttachments(id string, hasAttachments bool) []map[string]any {
	rows, ok := s.attachmentsFor(id)
	if !ok && hasAttachments {
		rows = []map[string]any{
			graphAttachmentFixtureTyped("att-1", "contract.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("att-2", "inline.png", "image/png", 320, true, "cid-inline", "#microsoft.graph.fileAttachment"),
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		row = cloneAttachmentRow(row)
		if row["@odata.type"] == nil {
			row["@odata.type"] = "#microsoft.graph.fileAttachment"
		}
		if row["@odata.type"] == "#microsoft.graph.fileAttachment" {
			size, _ := row["size"].(int64)
			row["contentBytes"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", int(size))))
		}
		out = append(out, row)
	}
	return out
}

func (s *cliOutlookServer) setMessageStatus(id string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageStatusByID[id] = status
}

func (s *cliOutlookServer) messageStatusForPath(path string) int {
	idPath := path
	for _, suffix := range []string{"/move", "/copy", "/send", "/createReply", "/createReplyAll", "/createForward"} {
		idPath = strings.TrimSuffix(idPath, suffix)
	}
	id, err := url.PathUnescape(idPath)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messageStatusByID[id]
}

func (s *cliOutlookServer) handleSendMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	s.messageCalls.Add(1)
	body := readRequestBody(s.t, r)
	s.recordMessageRequest(r, body)
	if status := s.messageStatus.Load(); status != 0 {
		s.writeMessageStatus(w, status, "message fixture status")
		return
	}
	if status := s.messageSendStatus.Load(); status != 0 {
		s.writeMessageStatus(w, status, "message send fixture status")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *cliOutlookServer) writeMessageStatus(w http.ResponseWriter, status int64, msg string) {
	w.WriteHeader(int(status))
	if s.oversizedStatusBodies.Load() {
		_, _ = w.Write([]byte(strings.Repeat("{", 1<<20+1)))
		return
	}
	writeCLIJSON(w, map[string]any{"error": msg})
}

func (s *cliOutlookServer) recordMessageRequest(r *http.Request, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageRequests = append(s.messageRequests, messageRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		EscapedPath: r.URL.EscapedPath(),
		Query:       cloneValues(r.URL.Query()),
		Prefer:      r.Header.Get("Prefer"),
		Body:        append([]byte(nil), body...),
	})
}

func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}
	return body
}

func graphFolderFixture(id, display, parent string, total, unread, child int) map[string]any {
	return map[string]any{
		"id":               id,
		"displayName":      display,
		"parentFolderId":   parent,
		"totalItemCount":   total,
		"unreadItemCount":  unread,
		"childFolderCount": child,
	}
}

func graphFolderFixtureNoCounts(id, display, parent string) map[string]any {
	return map[string]any{
		"id":              id,
		"displayName":     display,
		"parentFolderId":  parent,
		"unreadItemCount": 0,
	}
}

func graphFolderFixturePartialCounts(id, display, parent string, total, child *int) map[string]any {
	out := map[string]any{
		"id":              id,
		"displayName":     display,
		"parentFolderId":  parent,
		"unreadItemCount": 0,
	}
	if total != nil {
		out["totalItemCount"] = *total
	}
	if child != nil {
		out["childFolderCount"] = *child
	}
	return out
}

func intPointer(value int) *int {
	return &value
}

func folderFixtureSlug(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func graphMessageFixture(id, parent, received, subject string, isRead, attachments bool) map[string]any {
	return map[string]any{
		"id":                id,
		"internetMessageId": "<" + id + "@example.test>",
		"conversationId":    "thread-" + id,
		"parentFolderId":    parent,
		"subject":           subject,
		"from": map[string]any{"emailAddress": map[string]any{
			"name":    "Sender " + id,
			"address": id + "@example.test",
		}},
		"toRecipients": []map[string]any{
			{"emailAddress": map[string]any{"address": "to@example.test"}},
		},
		"ccRecipients":     []map[string]any{},
		"receivedDateTime": received,
		"sentDateTime":     "2026-06-19T08:00:00Z",
		"isRead":           isRead,
		"importance":       "normal",
		"hasAttachments":   attachments,
		"bodyPreview":      "Preview " + id,
	}
}

func graphFullMessageFixture(id string, attachments bool, prefer string) map[string]any {
	bodyType := "html"
	body := "<p>HTML <b>body</b> for " + id + "</p>"
	if strings.Contains(prefer, `"text"`) {
		bodyType = "text"
		body = "Text body for " + id
		if id == "html-fallback" {
			body = ""
		}
	}
	if strings.Contains(prefer, `"html"`) {
		bodyType = "html"
		body = "<p>HTML <b>body</b> for " + id + "</p>"
	}
	out := graphMessageFixture(id, "inbox-id", "2026-06-28T10:00:00Z", "Read "+id, false, attachments)
	out["sender"] = map[string]any{"emailAddress": map[string]any{"name": "Sender " + id, "address": "sender-" + id + "@example.test"}}
	out["replyTo"] = []map[string]any{{"emailAddress": map[string]any{"address": "reply@example.test"}}}
	out["bccRecipients"] = []map[string]any{{"emailAddress": map[string]any{"address": "bcc@example.test"}}}
	out["createdDateTime"] = "2026-06-28T09:58:00Z"
	out["lastModifiedDateTime"] = "2026-06-28T10:02:00Z"
	out["categories"] = []string{"Graph", "Fixture"}
	out["changeKey"] = "ck-" + id
	out["body"] = map[string]any{"contentType": bodyType, "content": body}
	out["internetMessageHeaders"] = []map[string]any{
		{"name": "Message-ID", "value": "<" + id + "@example.test>"},
		{"name": "X-Fixture", "value": "outlook"},
	}
	return out
}

func graphDraftFixture(id string, isDraft bool, prefer string) map[string]any {
	hasAttachments := id != "no-attachments" && id != "draft-inline-only"
	out := graphFullMessageFixture(id, hasAttachments, prefer)
	out["isDraft"] = isDraft
	out["parentFolderId"] = "drafts-id"
	out["subject"] = "Draft " + id
	return out
}

func graphReplyDraftFixture(id, prefer string) map[string]any {
	bodyType := "html"
	body := `<body><div>typed area</div><blockquote id="quote">Original message for ` + id + `</blockquote></body>`
	if strings.Contains(prefer, `"text"`) {
		bodyType = "text"
		body = "Typed area\n\nOriginal message for " + id
	}
	out := graphFullMessageFixture(id, false, prefer)
	out["isDraft"] = true
	out["parentFolderId"] = "drafts-id"
	out["subject"] = "RE: " + id
	out["toRecipients"] = []map[string]any{
		{"emailAddress": map[string]any{"name": "Source Sender", "address": "source@example.test"}},
	}
	out["ccRecipients"] = []map[string]any{
		{"emailAddress": map[string]any{"address": "copy@example.test"}},
	}
	out["bccRecipients"] = []map[string]any{}
	out["body"] = map[string]any{"contentType": bodyType, "content": body}
	return out
}

func graphForwardDraftFixture(id, prefer string) map[string]any {
	bodyType := "html"
	body := `<html><body><div>typed area</div><blockquote id="forwarded">Forwarded message for ` + id + `</blockquote></body></html>`
	if strings.Contains(prefer, `"text"`) {
		bodyType = "text"
		body = "Typed area\n\nForwarded message for " + id
	}
	out := graphFullMessageFixture(id, true, prefer)
	out["isDraft"] = true
	out["parentFolderId"] = "drafts-id"
	out["subject"] = "FW: " + id
	out["toRecipients"] = []map[string]any{}
	out["ccRecipients"] = []map[string]any{}
	out["bccRecipients"] = []map[string]any{}
	out["body"] = map[string]any{"contentType": bodyType, "content": body}
	return out
}

func graphAttachmentFixture(id, name, contentType string, size int64, inline bool, contentID string) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        name,
		"contentType": contentType,
		"size":        size,
		"isInline":    inline,
		"contentId":   contentID,
	}
}

func graphAttachmentFixtureTyped(id, name, contentType string, size int64, inline bool, contentID, odataType string) map[string]any {
	out := graphAttachmentFixture(id, name, contentType, size, inline, contentID)
	if odataType != "" {
		out["@odata.type"] = odataType
	}
	return out
}

// seedForwardDraftAttachments mirrors Graph createForward behavior: source
// attachments are copied to the new draft with fresh ids before maildo prunes
// unselected copies.
func (s *cliOutlookServer) seedForwardDraftAttachments(draftID, sourceID string) {
	source, _ := s.attachmentsFor(sourceID)
	copied := make([]map[string]any, 0, len(source))
	for _, row := range source {
		clone := cloneAttachmentRow(row)
		if id, _ := clone["id"].(string); id != "" {
			// This fixture forces the reconciliation path where Graph copied fewer
			// duplicate attachments than the source selection requires.
			if sourceID == "forward-copy-missing" && id == "missing-b" {
				continue
			}
			clone["id"] = "copy-" + id
		}
		copied = append(copied, clone)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachmentState[draftID] = copied
}

func (s *cliOutlookServer) addAttachment(draftID string, att map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.attachmentState[draftID]; !ok {
		if defaults, ok := defaultAttachmentRows(draftID); ok {
			s.attachmentState[draftID] = cloneAttachmentRows(defaults)
		}
	}
	s.attachmentState[draftID] = append(s.attachmentState[draftID], cloneAttachmentRow(att))
}

func (s *cliOutlookServer) deleteAttachment(draftID, attachmentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.attachmentState[draftID]; !ok {
		if defaults, ok := defaultAttachmentRows(draftID); ok {
			s.attachmentState[draftID] = cloneAttachmentRows(defaults)
		}
	}
	rows := s.attachmentState[draftID]
	for i, row := range rows {
		if id, _ := row["id"].(string); id == attachmentID {
			s.attachmentState[draftID] = append(rows[:i], rows[i+1:]...)
			return true
		}
	}
	return false
}

func (s *cliOutlookServer) attachmentsFor(messageID string) ([]map[string]any, bool) {
	s.mu.Lock()
	if rows, ok := s.attachmentState[messageID]; ok {
		out := cloneAttachmentRows(rows)
		s.mu.Unlock()
		return out, true
	}
	s.mu.Unlock()
	return defaultAttachmentRows(messageID)
}

func defaultAttachmentRows(messageID string) ([]map[string]any, bool) {
	switch messageID {
	case "download-source":
		return []map[string]any{
			graphAttachmentFixtureTyped("download-file", "download.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("zero-download", "zero.bin", "application/octet-stream", 0, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("slash/id", "slash.txt", "text/plain", 5, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("download-item", "embedded.eml", "message/rfc822", 50, false, "", "#microsoft.graph.itemAttachment"),
		}, true
	case "download-status":
		return []map[string]any{
			graphAttachmentFixtureTyped("missing-download", "missing.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("gone-download", "gone.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("auth-401", "auth401.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("auth-403", "auth403.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("rate-download", "rate.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("server-download", "server.txt", "text/plain", 12, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("oversized-download", "oversized.bin", "application/octet-stream", mail.MaxMessageAttachmentDownloadBytes+1, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("short-download", "short.txt", "text/plain", 100, false, "", "#microsoft.graph.fileAttachment"),
		}, true
	case "forward-source":
		return []map[string]any{
			graphAttachmentFixtureTyped("source-file", "source.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("source-extra", "extra.txt", "text/plain", 20, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("source-inline", "inline.png", "image/png", 320, true, "cid-inline", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("source-item", "embedded.eml", "message/rfc822", 50, false, "", "#microsoft.graph.itemAttachment"),
		}, true
	case "forward-duplicates":
		return []map[string]any{
			graphAttachmentFixtureTyped("dup-a", "duplicate.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("dup-b", "duplicate.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
		}, true
	case "forward-copy-missing":
		return []map[string]any{
			graphAttachmentFixtureTyped("missing-a", "missing.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("missing-b", "missing.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
		}, true
	case "draft-attachments":
		return []map[string]any{
			graphAttachmentFixtureTyped("draft-file", "draft.pdf", "application/pdf", 1200, false, "", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("draft-inline", "inline.png", "image/png", 320, true, "cid-inline", "#microsoft.graph.fileAttachment"),
			graphAttachmentFixtureTyped("draft-item", "embedded.eml", "message/rfc822", 50, false, "", "#microsoft.graph.itemAttachment"),
		}, true
	case "draft-inline-only":
		return []map[string]any{
			graphAttachmentFixtureTyped("inline-only", "inline-only.png", "image/png", 320, true, "cid-inline-only", "#microsoft.graph.fileAttachment"),
		}, true
	default:
		return nil, false
	}
}

func cloneAttachmentRows(in []map[string]any) []map[string]any {
	out := make([]map[string]any, len(in))
	for i, row := range in {
		out[i] = cloneAttachmentRow(row)
	}
	return out
}

func cloneAttachmentRow(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func graphThreadMessageFixture(id, conv, sent string) map[string]any {
	row := graphMessageFixture(id, "inbox-id", "2026-06-28T09:00:00Z", "Thread "+id, id == "thread-b", id == "thread-c")
	row["conversationId"] = conv
	row["sentDateTime"] = sent
	return row
}

func graphMessageFixtureMissingOptional(id, parent string) map[string]any {
	return map[string]any{
		"id":               id,
		"parentFolderId":   parent,
		"subject":          "Missing optional",
		"toRecipients":     []map[string]any{},
		"ccRecipients":     []map[string]any{},
		"receivedDateTime": "",
		"sentDateTime":     "",
		"isRead":           true,
		"hasAttachments":   false,
	}
}

func (s *cliOutlookServer) nextMailFoldersLink() string {
	if s.hostileNextLink.Load() {
		return "https://example.invalid/me/mailFolders?page=2"
	}
	return s.srv.URL + "/me/mailFolders?page=2"
}

func (s *cliOutlookServer) setProfile(id, email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profileID = id
	s.email = email
	s.userPrincipal = email
}

func (s *cliOutlookServer) setDisplay(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displayName = name
}

func (s *cliOutlookServer) setPersonalProfile(id, upn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profileID = id
	s.email = ""
	s.userPrincipal = upn
}

func TestOutlookCLIHappyPathRefreshAndLogout(t *testing.T) {
	h := newOutlookHarness(t)

	var login loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--provider", "outlook", "--name", "work", "--set-active", "--json"), &login)
	if !login.Created || !login.Active || login.Email != "work@example.com" || h.browserCalls.Load() != 1 {
		t.Fatalf("login = %#v browser=%d", login, h.browserCalls.Load())
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	account, _ := reg.Find("work")
	if account.ProviderAccountID != "outlook-work" || account.DefaultFrom != "work@example.com" {
		t.Fatalf("registry account = %#v", account)
	}

	h.browserCalls.Store(0)
	var refresh refreshResult
	decodeOK(t, h.runNoBrowser("auth", "refresh", "work", "--json"), &refresh)
	if refresh.AuthStatus != authStatusRefreshed || !refresh.RotatedRefreshToken || h.browserCalls.Load() != 0 {
		t.Fatalf("refresh = %#v browser=%d", refresh, h.browserCalls.Load())
	}
	cacheBytes, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if strings.Contains(string(cacheBytes), "cli-refresh-secret-12345") {
		t.Fatalf("old refresh token remained in cache: %s", cacheBytes)
	}

	var logout logoutResult
	decodeOK(t, h.runNoBrowser("auth", "logout", "work", "--json"), &logout)
	if !logout.CredentialCacheDeleted || credentialExists(t, h, "outlook", "work") {
		t.Fatalf("logout = %#v", logout)
	}
}

func TestOutlookCLILoginVariantsPromptsAndReauth(t *testing.T) {
	h := newOutlookHarness(t)

	var login loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--provider", "outlook", "--name", "work", "--set-active", "--json"), &login)
	if !login.Created || !login.Active {
		t.Fatalf("initial login = %#v", login)
	}

	h.server.setProfile("outlook-second", "second@example.com")
	var second loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--provider", "outlook", "--name", "second", "--no-prompt", "--no-set-active", "--json"), &second)
	if !second.Created || second.Active || deref(second.ActiveAccount) != "work" {
		t.Fatalf("no-prompt login = %#v", second)
	}

	h.server.setPersonalProfile("outlook-personal", "personal@example.com")
	var personal loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--provider", "outlook", "--name", "personal", "--no-set-active", "--json"), &personal)
	if personal.Email != "personal@example.com" || personal.Active {
		t.Fatalf("personal login = %#v", personal)
	}

	h.server.setProfile("outlook-work", "work@example.com")
	h.server.setDisplay("Work User Updated")
	var reauth loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--name", "work", "--json"), &reauth)
	if !reauth.Reauthenticated || reauth.DisplayName != "Work User Updated" {
		t.Fatalf("reauth = %#v", reauth)
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(reg.Accounts) != 3 {
		t.Fatalf("accounts after reauth = %#v", reg.Accounts)
	}

	prompted := newOutlookHarness(t)
	opts := prompted.options(prompted.browserOpener())
	opts.Stdin = strings.NewReader("prompted\n")
	opts.Terminal = true
	opts.TerminalSet = true
	decodeOK(t, prompted.runWith(opts, "login", "--provider", "outlook"), nil)
	if !credentialExists(t, prompted, "outlook", "prompted") || prompted.browserCalls.Load() != 1 {
		t.Fatalf("prompted login cache=%t browser=%d", credentialExists(t, prompted, "outlook", "prompted"), prompted.browserCalls.Load())
	}

	reader := &failOnRead{}
	machine := newOutlookHarness(t)
	machineOpts := machine.options(machine.browserOpener())
	machineOpts.Stdin = reader
	res := machine.runWith(machineOpts, "--json", "login")
	if res.Exit == 0 {
		t.Fatalf("machine login succeeded, want usage error")
	}
	if reader.reads.Load() != 0 {
		t.Fatal("machine mode attempted to read stdin")
	}
}

func TestOutlookCLIOlderAccountEmailFallbackPersistsProviderID(t *testing.T) {
	h := newOutlookHarness(t)
	account := maildoconfig.Account{
		Name:        "legacy",
		Provider:    "outlook",
		Email:       "work@example.com",
		DisplayName: "Legacy",
		AuthRef:     "outlook/legacy",
		DefaultFrom: "work@example.com",
	}
	if err := testkit.SeedRegistry(h.configPath, "legacy", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	var login loginResult
	decodeOK(t, h.runOutlook("auth", "login", "--name", "legacy", "--json"), &login)
	if !login.Reauthenticated {
		t.Fatalf("legacy reauth = %#v", login)
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	loaded, _ := reg.Find("legacy")
	if loaded.ProviderAccountID != "outlook-work" {
		t.Fatalf("provider id not persisted: %#v", loaded)
	}
}

func TestOutlookCLIRenameKeepsCacheUsable(t *testing.T) {
	h := newOutlookHarness(t)
	decodeOK(t, h.runOutlook("auth", "login", "--provider", "outlook", "--name", "work", "--json"), &loginResult{})
	var renamed renameResult
	decodeOK(t, h.runNoBrowser("auth", "rename", "work", "job", "--json"), &renamed)
	if !renamed.CredentialCacheMigrated || credentialExists(t, h, "outlook", "work") || !credentialExists(t, h, "outlook", "job") {
		t.Fatalf("rename = %#v", renamed)
	}
	cache, err := loadCredentialCache(h, "outlook", "job")
	if err != nil {
		t.Fatalf("load renamed cache: %v", err)
	}
	if cache.AccountName != "job" {
		t.Fatalf("renamed cache account_name = %q", cache.AccountName)
	}
	var refresh refreshResult
	decodeOK(t, h.runNoBrowser("auth", "refresh", "job", "--json"), &refresh)
	if refresh.AuthStatus != authStatusRefreshed {
		t.Fatalf("refresh renamed account = %#v", refresh)
	}
}

func TestOutlookCLIStatusListRepairHealthAndRefreshErrorSplit(t *testing.T) {
	h := newOutlookHarness(t)
	account := maildoconfig.Account{
		Name:              "work",
		Provider:          "outlook",
		Email:             "work@example.test",
		DisplayName:       "Work",
		ProviderAccountID: "outlook-work",
		AuthRef:           "outlook/work",
		DefaultFrom:       "work@example.test",
	}
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var status statusResult
	decodeOK(t, h.runNoBrowser("auth", "status", "work", "--json"), &status)
	if status.AuthStatus != authStatusValid || h.server.tokenCalls.Load() != 0 || h.server.graphCalls.Load() != 0 {
		t.Fatalf("status = %#v token=%d graph=%d", status, h.server.tokenCalls.Load(), h.server.graphCalls.Load())
	}

	if err := os.WriteFile(filepath.Join(h.root, "auth", "outlook", "work.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}
	decodeOK(t, h.runNoBrowser("auth", "status", "work", "--json"), &status)
	if status.AuthStatus != authStatusRepairRequired || status.RepairCommand != "maildo auth repair work" {
		t.Fatalf("repair status = %#v", status)
	}
	var list listResult
	decodeOK(t, h.runNoBrowser("auth", "list", "--json"), &list)
	if len(list.Accounts) != 1 || list.Accounts[0].AuthStatus != authStatusRepairRequired || list.Accounts[0].RepairCommand == "" {
		t.Fatalf("repair list = %#v", list)
	}

	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("restore cache: %v", err)
	}
	h.server.invalidGrant.Store(true)
	decodeOK(t, h.runNoBrowser("auth", "status", "work", "--refresh", "--json"), &status)
	if status.AuthStatus != authStatusRepairRequired || !status.RefreshChecked || status.RepairCommand == "" {
		t.Fatalf("status --refresh repair = %#v", status)
	}
	assertErrorCodeExit(t, h.runNoBrowser("auth", "refresh", "work", "--json"), errs.CodeAuthRepairRequired, 3)
}

func TestOutlookCLILoggedOutStatusListRefreshAndRemove(t *testing.T) {
	h := newOutlookHarness(t)
	account := maildoconfig.Account{
		Name:              "work",
		Provider:          "outlook",
		Email:             "work@example.com",
		DisplayName:       "Work",
		ProviderAccountID: "outlook-work",
		AuthRef:           "outlook/work",
		DefaultFrom:       "work@example.com",
	}
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	var status statusResult
	decodeOK(t, h.runNoBrowser("auth", "status", "work", "--json"), &status)
	if status.AuthStatus != authStatusLoggedOut || status.LoginCommand == "" || status.RefreshChecked {
		t.Fatalf("logged-out status = %#v", status)
	}
	var list listResult
	decodeOK(t, h.runNoBrowser("auth", "list", "--json"), &list)
	if len(list.Accounts) != 1 || list.Accounts[0].AuthStatus != authStatusLoggedOut || list.Accounts[0].LoginCommand == "" {
		t.Fatalf("logged-out list = %#v", list)
	}
	var refresh refreshResult
	decodeOK(t, h.runNoBrowser("auth", "refresh", "work", "--json"), &refresh)
	if refresh.AuthStatus != authStatusLoggedOut || refresh.RefreshChecked || refresh.LoginCommand == "" {
		t.Fatalf("logged-out refresh = %#v", refresh)
	}
	if h.server.tokenCalls.Load() != 0 || h.server.graphCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("logged-out paths called hooks token=%d graph=%d browser=%d", h.server.tokenCalls.Load(), h.server.graphCalls.Load(), h.browserCalls.Load())
	}

	if err := testkit.SeedOutlookCache(h.root, "work", func(c *testkit.TokenCache) {
		c.VerifiedEmail = account.Email
		c.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	var removed removeResult
	decodeOK(t, h.runNoBrowser("auth", "remove", "work", "--force", "--json"), &removed)
	if !removed.Removed || !removed.CredentialCacheDeleted || credentialExists(t, h, "outlook", "work") {
		t.Fatalf("remove = %#v", removed)
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry after remove: %v", err)
	}
	if _, ok := reg.Find("work"); ok {
		t.Fatal("removed account still in registry")
	}
}

func TestOutlookCLIIdentityMismatchLeavesCacheAndRegistryIntact(t *testing.T) {
	h := newOutlookHarness(t)
	var login loginResult
	decodeOK(t, h.runOutlook("login", "--provider", "outlook", "--name", "work", "--json"), &login)
	before, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	h.server.setProfile("different-id", "other@example.com")
	res := h.runOutlook("auth", "login", "--name", "work", "--json")
	assertErrorCodeExit(t, res, errs.CodeAccountIdentityMismatch, 3)
	after, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache after mismatch: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("cache changed after identity mismatch")
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	account, _ := reg.Find("work")
	if account.ProviderAccountID != "outlook-work" || account.Email != "work@example.com" {
		t.Fatalf("registry changed after mismatch: %#v", account)
	}
}

func TestOutlookCLIRepairIdentityAndDevicePaths(t *testing.T) {
	h := newOutlookHarness(t)
	decodeOK(t, h.runOutlook("login", "--provider", "outlook", "--name", "work", "--json"), &loginResult{})
	before, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache before repair: %v", err)
	}

	h.browserCalls.Store(0)
	h.server.setDisplay("Work Repaired")
	var repaired repairResult
	decodeOK(t, h.runOutlook("auth", "repair", "work", "--json"), &repaired)
	if !repaired.Repaired || h.browserCalls.Load() != 1 {
		t.Fatalf("repair = %#v browser=%d", repaired, h.browserCalls.Load())
	}
	reg, err := maildoconfig.Load(h.configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	account, _ := reg.Find("work")
	if account.DisplayName != "Work Repaired" {
		t.Fatalf("registry not updated by repair: %#v", account)
	}

	afterRepair, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache after repair: %v", err)
	}
	h.server.setProfile("different-id", "other@example.com")
	res := h.runOutlook("auth", "repair", "work", "--json")
	assertErrorCodeExit(t, res, errs.CodeAccountIdentityMismatch, 3)
	afterMismatch, err := os.ReadFile(filepath.Join(h.root, "auth", "outlook", "work.json"))
	if err != nil {
		t.Fatalf("read cache after mismatch: %v", err)
	}
	if string(afterMismatch) != string(afterRepair) || string(afterMismatch) == string(before) {
		t.Fatalf("repair mismatch cache rollback failed")
	}

	h.server.setProfile("outlook-work", "work@example.com")
	h.browserCalls.Store(0)
	h.server.deviceCodeCalls.Store(0)
	decodeOK(t, h.runDevice("auth", "repair", "work", "--device-code", "--json"), &repairResult{})
	if h.browserCalls.Load() != 0 || h.server.deviceCodeCalls.Load() == 0 {
		t.Fatalf("device repair hooks browser=%d device=%d", h.browserCalls.Load(), h.server.deviceCodeCalls.Load())
	}
}

func TestOutlookCLIDeviceCodeAndDryRunIsolation(t *testing.T) {
	h := newOutlookHarness(t)
	var login loginResult
	decodeOK(t, h.runDevice("auth", "login", "--provider", "outlook", "--name", "device", "--device-code", "--json"), &login)
	if login.Email != "work@example.com" || h.browserCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != 1 {
		t.Fatalf("device login = %#v browser=%d device=%d", login, h.browserCalls.Load(), h.server.deviceCodeCalls.Load())
	}
	h.browserCalls.Store(0)
	tokenBefore := h.server.tokenCalls.Load()
	graphBefore := h.server.graphCalls.Load()
	deviceBefore := h.server.deviceCodeCalls.Load()
	deviceRes := h.runDevice("auth", "login", "--provider", "outlook", "--name", "dry", "--device-code", "--dry-run", "--json")
	decodePreview(t, deviceRes)
	for _, args := range [][]string{
		{"auth", "repair", "device", "--device-code", "--dry-run", "--json"},
		{"auth", "refresh", "device", "--dry-run", "--json"},
		{"auth", "status", "device", "--refresh", "--dry-run", "--json"},
		{"auth", "logout", "device", "--dry-run", "--json"},
		{"auth", "rename", "device", "renamed", "--dry-run", "--json"},
		{"auth", "remove", "device", "--force", "--dry-run", "--json"},
	} {
		decodePreview(t, h.runDevice(args...))
	}
	if h.browserCalls.Load() != 0 || h.server.deviceCodeCalls.Load() != deviceBefore || h.server.tokenCalls.Load() != tokenBefore || h.server.graphCalls.Load() != graphBefore {
		t.Fatalf("dry-run hit hooks browser=%d device=%d/%d token=%d/%d graph=%d/%d",
			h.browserCalls.Load(), h.server.deviceCodeCalls.Load(), deviceBefore, h.server.tokenCalls.Load(), tokenBefore, h.server.graphCalls.Load(), graphBefore)
	}
	if strings.Contains(deviceRes.Stderr, "cli-device-code-secret-12345") || strings.Contains(deviceRes.Stdout, "cli-device-code-secret-12345") {
		t.Fatalf("device secret leaked in dry-run")
	}
}

func TestOutlookCLIMachineErrorsForProviderIdentityAndMissingConfig(t *testing.T) {
	h := newOutlookHarness(t)
	h.server.setProfile("bad-id", "")
	res := h.runOutlook("auth", "login", "--provider", "outlook", "--name", "bad", "--json")
	assertErrorCodeExit(t, res, errs.CodeProviderIdentityUnavailable, 3)
	testkit.AssertNoSecretLeak(t, res, "cli-access-secret-12345", "cli-refresh-secret-12345")

	noConfig := newOutlookHarness(t)
	opts := noConfig.options(noConfig.browserOpener())
	opts.LookupEnv = func(string) (string, bool) { return "", false }
	missing := noConfig.runWith(opts, "auth", "login", "--provider", "outlook", "--name", "work", "--json")
	assertErrorCodeExit(t, missing, errs.CodeAuthAppConfigMissing, 1)
	if noConfig.browserCalls.Load() != 0 {
		t.Fatalf("missing config opened browser")
	}
}

func TestOutlookCLIMissingLoginRefreshTokenFailsBeforeSave(t *testing.T) {
	h := newOutlookHarness(t)
	h.server.missingRefresh.Store(true)
	res := h.runOutlook("auth", "login", "--provider", "outlook", "--name", "work", "--json")
	if res.Exit != 3 {
		t.Fatalf("exit = %d, want 3\nstdout=%s\nstderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	if credentialExists(t, h, "outlook", "work") {
		t.Fatal("cache saved when refresh token was missing")
	}
	if _, err := maildoconfig.Load(h.configPath, []string{"outlook"}); err != nil {
		t.Fatalf("missing-refresh left invalid registry: %v", err)
	}
	testkit.AssertNoSecretLeak(t, res, "cli-access-secret-12345")
}

func writeCLIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// A person waiting for the browser sees where to sign in and that maildo
// waits, also when the browser never opens.
func TestOutlookCLILoginShowsTheSignInAddress(t *testing.T) {
	h := newOutlookHarness(t)
	res := h.runOutlook("login", "--provider", "outlook", "--name", "work")
	if res.Exit != 0 {
		t.Fatalf("login exit %d: %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	for _, want := range []string{"If it does not open, visit this address:\nhttp://127.0.0.1:", "/authorize?client_id=cli-client-id", "Waiting for you to finish signing in"} {
		if !strings.Contains(res.Stderr, want) {
			t.Fatalf("stderr is missing %q:\n%s", want, res.Stderr)
		}
	}
}
