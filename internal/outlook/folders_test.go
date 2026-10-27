package outlook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

func TestFolderGraphURLValidation(t *testing.T) {
	c := &Client{graphBaseURL: "https://graph.microsoft.com/v1.0"}
	for _, tt := range []struct {
		name   string
		target string
		want   bool
	}{
		{name: "same base", target: "https://graph.microsoft.com/v1.0/me/mailFolders?$top=100", want: true},
		{name: "same host child path", target: "https://graph.microsoft.com/v1.0/me/mailFolders/inbox", want: true},
		{name: "cross host", target: "https://example.invalid/v1.0/me/mailFolders", want: false},
		{name: "outside base path", target: "https://graph.microsoft.com/beta/me/mailFolders", want: false},
		{name: "base prefix confusion", target: "https://graph.microsoft.com/v1.0-other/me/mailFolders", want: false},
		{name: "different scheme", target: "http://graph.microsoft.com/v1.0/me/mailFolders", want: false},
		{name: "different port", target: "https://graph.microsoft.com:8443/v1.0/me/mailFolders", want: false},
		{name: "user info", target: "https://user@graph.microsoft.com/v1.0/me/mailFolders", want: false},
		{name: "fragment", target: "https://graph.microsoft.com/v1.0/me/mailFolders#hidden", want: false},
		{name: "literal dot segment", target: "https://graph.microsoft.com/v1.0/../beta/me/mailFolders", want: false},
		{name: "escaped dot segment", target: "https://graph.microsoft.com/v1.0/%2e%2e/beta/me/mailFolders", want: false},
		{name: "malformed escape", target: "https://graph.microsoft.com/v1.0/%zz", want: false},
		{name: "relative", target: "/v1.0/me/mailFolders", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.validGraphURL(tt.target); got != tt.want {
				t.Fatalf("validGraphURL(%q) = %t, want %t", tt.target, got, tt.want)
			}
		})
	}
}

func TestGraphFolderMutationDoStatusMapping(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body       string
		wantExit   int
		wantCode   string
		wantNoBody bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`, wantExit: 3, wantCode: errs.CodeAuthRepairRequired},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, wantExit: 3, wantCode: errs.CodeAuthRepairRequired},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{}`, wantExit: rungrad.ExitRateLimited},
		{name: "server", status: http.StatusInternalServerError, body: `{}`, wantExit: rungrad.ExitAPI},
		{name: "oversized unauthorized", status: http.StatusUnauthorized, body: strings.Repeat("{", maxBodyBytes+1), wantExit: 3, wantCode: errs.CodeAuthRepairRequired, wantNoBody: true},
		{name: "oversized forbidden", status: http.StatusForbidden, body: strings.Repeat("{", maxBodyBytes+1), wantExit: 3, wantCode: errs.CodeAuthRepairRequired, wantNoBody: true},
		{name: "oversized rate limited", status: http.StatusTooManyRequests, body: strings.Repeat("{", maxBodyBytes+1), wantExit: rungrad.ExitRateLimited, wantNoBody: true},
		{name: "oversized server", status: http.StatusInternalServerError, body: strings.Repeat("{", maxBodyBytes+1), wantExit: rungrad.ExitAPI, wantNoBody: true},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("{", maxBodyBytes+1), wantExit: rungrad.ExitAPI, wantNoBody: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			c := &Client{graphBaseURL: srv.URL, http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("secret-token"), account: "work"}
			_, body, err := c.graphFolderMutationDo(context.Background(), http.MethodGet, srv.URL+"/me/mailFolders/inbox", nil)
			if err == nil {
				t.Fatal("graphFolderMutationDo succeeded")
			}
			if tt.wantNoBody && body != nil {
				t.Fatalf("body = %d bytes, want nil", len(body))
			}
			if got := exitCode(err); got != tt.wantExit {
				t.Fatalf("exit = %d, want %d (err=%v)", got, tt.wantExit, err)
			}
			if tt.wantCode != "" {
				product, ok := err.(*errs.Error)
				if !ok || product.Code() != tt.wantCode {
					t.Fatalf("product err = %#v, want code %q", err, tt.wantCode)
				}
			}
		})
	}
}

func TestGraphFolderMutationDoPinsURLBeforeBearer(t *testing.T) {
	var hostileHits atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileHits.Add(1)
		t.Errorf("hostile server received request with Authorization=%q", r.Header.Get("Authorization"))
	}))
	t.Cleanup(hostile.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("good server should not be called for hostile target")
	}))
	t.Cleanup(good.Close)

	c := &Client{graphBaseURL: good.URL, http: good.Client(), tokenType: "Bearer", accessToken: secretFromString("secret-token"), account: "work"}
	_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodGet, hostile.URL+"/me/mailFolders/inbox", nil)
	if err == nil {
		t.Fatal("hostile target succeeded")
	}
	if hostileHits.Load() != 0 {
		t.Fatalf("hostile server hits = %d, want 0", hostileHits.Load())
	}
}

func TestGraphFolderMutationDoPinsRedirectsBeforeBearer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		method  string
		payload any
	}{
		{name: "moved permanently", status: http.StatusMovedPermanently, method: http.MethodGet},
		{name: "found", status: http.StatusFound, method: http.MethodPost, payload: map[string]any{"displayName": "Projects"}},
		{name: "temporary redirect", status: http.StatusTemporaryRedirect, method: http.MethodPatch, payload: map[string]any{"displayName": "Projects"}},
		{name: "permanent redirect", status: http.StatusPermanentRedirect, method: http.MethodDelete},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var hostileHits atomic.Int64
			hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hostileHits.Add(1)
				t.Errorf("redirect target received request with Authorization=%q", r.Header.Get("Authorization"))
			}))
			t.Cleanup(hostile.Close)
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", hostile.URL+"/redirected")
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(primary.Close)

			c := &Client{graphBaseURL: primary.URL, http: primary.Client(), tokenType: "Bearer", accessToken: secretFromString("secret-token"), account: "work"}
			_, _, err := c.graphFolderMutationDo(context.Background(), tt.method, primary.URL+"/me/mailFolders/inbox", tt.payload)
			if err == nil {
				t.Fatal("unpinned redirect succeeded")
			}
			if hostileHits.Load() != 0 {
				t.Fatalf("redirect target hits = %d, want 0", hostileHits.Load())
			}
		})
	}
}

func TestFolderProtectionAliasesArePinned(t *testing.T) {
	want := []string{
		"msgfolderroot", "inbox", "drafts", "sentitems", "deleteditems", "archive", "junkemail", "outbox",
		"conversationhistory", "searchfolders", "recoverableitemsdeletions", "clutter", "scheduled", "syncissues",
		"conflicts", "localfailures", "serverfailures",
	}
	if strings.Join(protectedFolderAliases, "\n") != strings.Join(want, "\n") {
		t.Fatalf("protected folder aliases = %#v, want %#v", protectedFolderAliases, want)
	}
	seen := map[string]bool{}
	for _, alias := range protectedFolderAliases {
		if seen[alias] {
			t.Fatalf("duplicate protected folder alias %q", alias)
		}
		seen[alias] = true
	}
}

func TestFolderMutationRejectsWrongSuccessStatuses(t *testing.T) {
	operations := []struct {
		name     string
		method   string
		statuses []int
		run      func(*Client) error
	}{
		{
			name: "create", method: http.MethodPost, statuses: []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent},
			run: func(c *Client) error {
				_, err := c.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Projects"})
				return err
			},
		},
		{
			name: "rename", method: http.MethodPatch, statuses: []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent},
			run: func(c *Client) error {
				_, err := c.RenameFolder(context.Background(), mail.FolderRenameRequest{Target: mail.Folder{ID: "folder-id", DisplayName: "Old"}, NewDisplayName: "New"})
				return err
			},
		},
		{
			name: "delete", method: http.MethodDelete, statuses: []int{http.StatusOK, http.StatusCreated, http.StatusAccepted},
			run: func(c *Client) error {
				_, err := c.DeleteFolder(context.Background(), mail.FolderDeleteRequest{Target: mail.Folder{ID: "folder-id"}})
				return err
			},
		},
	}
	for _, operation := range operations {
		for _, status := range operation.statuses {
			t.Run(fmt.Sprintf("%s/%d", operation.name, status), func(t *testing.T) {
				var hits atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if r.Method != operation.method {
						t.Errorf("method = %s, want %s", r.Method, operation.method)
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"id":"returned-id","displayName":"Result"}`))
				}))
				t.Cleanup(srv.Close)
				c := testFolderMutationClient(srv)
				if err := operation.run(c); err == nil {
					t.Fatalf("status %d was accepted", status)
				}
				if hits.Load() != 1 {
					t.Fatalf("request attempts = %d, want 1", hits.Load())
				}
			})
		}
	}
}

func TestFolderMutationResponseShapesStayNarrow(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write([]byte(`{"id":"provider-id","displayName":"","unknown":{"secret":"hidden"}}`))
	}))
	t.Cleanup(srv.Close)
	c := testFolderMutationClient(srv)
	created, err := c.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Requested Create"})
	if err != nil || method != http.MethodPost || created.CreatedFolder == nil || created.CreatedFolder.ID != "provider-id" || created.CreatedFolder.DisplayName != "Requested Create" {
		t.Fatalf("create fallback result=%#v method=%s err=%v", created, method, err)
	}
	renamed, err := c.RenameFolder(context.Background(), mail.FolderRenameRequest{Target: mail.Folder{ID: "requested-id", DisplayName: "Old"}, NewDisplayName: "Requested Rename"})
	if err != nil || method != http.MethodPatch || renamed.RenamedFolder == nil || renamed.RenamedFolder.ID != "provider-id" || renamed.RenamedFolder.DisplayName != "Requested Rename" {
		t.Fatalf("rename fallback result=%#v method=%s err=%v", renamed, method, err)
	}
	if created.ProviderMeta == nil || created.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph || created.ProviderMeta.ProtectedAlias != "" ||
		renamed.ProviderMeta == nil || renamed.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph || renamed.ProviderMeta.ProtectedAlias != "" {
		t.Fatalf("provider metadata escaped response fields: create=%#v rename=%#v", created.ProviderMeta, renamed.ProviderMeta)
	}

	for name, body := range map[string]string{
		"missing":   `{"displayName":"Folder"}`,
		"null":      `{"id":null,"displayName":"Folder"}`,
		"numeric":   `{"id":7,"displayName":"Folder"}`,
		"malformed": `{"id":`,
	} {
		t.Run(name, func(t *testing.T) {
			shapeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(shapeServer.Close)
			shapeClient := testFolderMutationClient(shapeServer)
			if _, err := shapeClient.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Folder"}); err == nil {
				t.Fatalf("response %s was accepted", body)
			}
		})
	}
}

func TestFolderProtectionAliasFailuresStopPreflight(t *testing.T) {
	for name, response := range map[string]string{
		"malformed":  `{"id":`,
		"missing id": `{"displayName":"Managed"}`,
		"numeric id": `{"id":9}`,
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.EscapedPath() != "/me/mailFolders/msgfolderroot" {
					t.Errorf("request continued after failed first alias: %s", r.URL.EscapedPath())
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(response))
			}))
			t.Cleanup(srv.Close)
			c := testFolderMutationClient(srv)
			if _, err := c.PrepareFolderRename(context.Background(), mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "ordinary-id"}}); err == nil {
				t.Fatal("malformed protection alias succeeded")
			}
			if requests.Load() != 1 {
				t.Fatalf("protection requests = %d, want 1", requests.Load())
			}
		})
	}

	for _, tt := range []struct {
		name      string
		status    int
		oversized bool
		wantExit  int
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantExit: 3},
		{name: "forbidden", status: http.StatusForbidden, wantExit: 3},
		{name: "rate limited", status: http.StatusTooManyRequests, wantExit: rungrad.ExitRateLimited},
		{name: "server error", status: http.StatusInternalServerError, wantExit: rungrad.ExitAPI},
		{name: "oversized success", status: http.StatusOK, oversized: true, wantExit: rungrad.ExitAPI},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tt.status)
				body := `{}`
				if tt.oversized {
					body = strings.Repeat("{", maxBodyBytes+1)
				}
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			c := testFolderMutationClient(srv)
			_, err := c.PrepareFolderRename(context.Background(), mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "ordinary-id"}})
			if err == nil || exitCode(err) != tt.wantExit {
				t.Fatalf("preflight error=%v exit=%d want=%d", err, exitCode(err), tt.wantExit)
			}
			if requests.Load() != 1 {
				t.Fatalf("protection requests = %d, want 1", requests.Load())
			}
		})
	}

	t.Run("transport failure", func(t *testing.T) {
		var requests atomic.Int64
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("protection transport sentinel")
		})
		c := &Client{
			graphBaseURL: "https://graph.microsoft.com/v1.0",
			http:         &http.Client{Transport: transport},
			tokenType:    "Bearer",
			accessToken:  secretFromString("synthetic-token"),
			account:      "work",
		}
		if _, err := c.PrepareFolderRename(context.Background(), mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "ordinary-id"}}); err == nil {
			t.Fatal("transport failure succeeded")
		}
		if requests.Load() != 1 {
			t.Fatalf("protection requests = %d, want 1", requests.Load())
		}
	})
}

func TestFolderProtectionMissingAliasesContinueAndDuplicateIDsRemainProtected(t *testing.T) {
	for _, missingStatus := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(missingStatus), func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				alias := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
				switch alias {
				case "msgfolderroot", "outbox":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"id":"managed-id","displayName":"Localized"}`))
				default:
					w.WriteHeader(missingStatus)
				}
			}))
			t.Cleanup(srv.Close)
			c := testFolderMutationClient(srv)
			preflight, err := c.PrepareFolderRename(context.Background(), mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "managed-id", DisplayName: "Different Name"}})
			if err != nil || !preflight.Protected || preflight.ProtectionReason != "provider_managed_folder" {
				t.Fatalf("preflight=%#v err=%v", preflight, err)
			}
			if requests.Load() != int64(len(protectedFolderAliases)) {
				t.Fatalf("protection requests = %d, want %d", requests.Load(), len(protectedFolderAliases))
			}
			if preflight.ProviderMeta == nil || preflight.ProviderMeta.ProtectedAlias != mail.ProtectedAliasOutbox {
				t.Fatalf("duplicate alias metadata = %#v", preflight.ProviderMeta)
			}
		})
	}
}

func TestFolderMutationReadPreflightsRejectWrongSuccessStatuses(t *testing.T) {
	t.Run("protection lookup", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"managed-id"}`))
		}))
		t.Cleanup(srv.Close)
		c := testFolderMutationClient(srv)
		if _, err := c.PrepareFolderRename(context.Background(), mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "folder-id"}}); err == nil {
			t.Fatal("201 protection response was accepted")
		}
	})

	t.Run("count refresh", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/folder-id") {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"folder-id","totalItemCount":1,"childFolderCount":0}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		c := testFolderMutationClient(srv)
		if _, err := c.PrepareFolderDelete(context.Background(), mail.FolderDeletePreflightRequest{Target: mail.Folder{ID: "folder-id"}}); err == nil {
			t.Fatal("202 count response was accepted")
		}
	})
}

func TestPrepareFolderDeleteRejectsMismatchedRefreshIdentity(t *testing.T) {
	for _, redirected := range []bool{false, true} {
		name := "direct"
		if redirected {
			name = "allowed redirect"
		}
		t.Run(name, func(t *testing.T) {
			var deleteAttempts atomic.Int64
			var refreshResponses atomic.Int64
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete:
					deleteAttempts.Add(1)
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == "/v1.0/me/mailFolders/folder-id" && redirected:
					w.Header().Set("Location", srv.URL+"/v1.0/refreshed-folder")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case r.URL.Path == "/v1.0/me/mailFolders/folder-id" || r.URL.Path == "/v1.0/refreshed-folder":
					refreshResponses.Add(1)
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"id":"different-folder-id","displayName":"Different","totalItemCount":99,"childFolderCount":8}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			c := &Client{
				graphBaseURL: srv.URL + "/v1.0",
				http:         srv.Client(),
				tokenType:    "Bearer",
				accessToken:  secretFromString("synthetic-token"),
				account:      "work",
			}
			_, err := c.PrepareFolderDelete(context.Background(), mail.FolderDeletePreflightRequest{Target: mail.Folder{ID: "folder-id", DisplayName: "Target"}})
			if err == nil {
				t.Fatal("mismatched refresh identity succeeded")
			}
			assertFolderMutationErrorSafe(t, err, srv.URL, "different-folder-id", "Different", "synthetic-token")
			if refreshResponses.Load() != 1 || deleteAttempts.Load() != 0 {
				t.Fatalf("refresh responses=%d delete attempts=%d", refreshResponses.Load(), deleteAttempts.Load())
			}
		})
	}
}

func TestGraphFolderMutationDoTransportAndCancellationFailuresAreBroad(t *testing.T) {
	for _, tt := range []struct {
		name string
		ctx  func() context.Context
		err  error
	}{
		{name: "transport", ctx: context.Background, err: errors.New("transport sentinel with https://hostile.invalid secret-token")},
		{name: "canceled", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int64
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				if tt.err != nil {
					return nil, tt.err
				}
				return nil, errors.New("request should have been canceled")
			})
			client := &http.Client{Transport: transport}
			c := &Client{graphBaseURL: "https://graph.microsoft.com/v1.0", http: client, tokenType: "Bearer", accessToken: secretFromString("secret-token"), account: "work"}
			_, _, err := c.graphFolderMutationDo(tt.ctx(), http.MethodPost, "https://graph.microsoft.com/v1.0/me/mailFolders", map[string]any{"displayName": "Projects"})
			if err == nil {
				t.Fatal("request succeeded")
			}
			if strings.Contains(err.Error(), "hostile.invalid") || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "graph.microsoft.com") {
				t.Fatalf("failure leaked transport detail: %v", err)
			}
			if tt.name == "transport" && attempts.Load() != 1 {
				t.Fatalf("transport attempts = %d, want 1", attempts.Load())
			}
			if attempts.Load() > 1 {
				t.Fatalf("request retried %d times", attempts.Load())
			}
		})
	}
}

func TestFolderMutationCancellationStopsAtEachProviderStage(t *testing.T) {
	t.Run("protection lookup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var attempts atomic.Int64
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempt := attempts.Add(1)
			if attempt < 4 {
				return testRoundTripResponse(req, http.StatusNotFound, ""), nil
			}
			cancel()
			return nil, req.Context().Err()
		})
		c := testFolderMutationTransportClient(transport)
		_, err := c.PrepareFolderRename(ctx, mail.FolderRenamePreflightRequest{Target: mail.Folder{ID: "folder-id"}})
		if err == nil || attempts.Load() != 4 {
			t.Fatalf("protection cancellation attempts=%d err=%v", attempts.Load(), err)
		}
	})

	t.Run("count refresh", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var attempts atomic.Int64
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempt := attempts.Add(1)
			if attempt <= int64(len(protectedFolderAliases)) {
				return testRoundTripResponse(req, http.StatusNotFound, ""), nil
			}
			cancel()
			return nil, req.Context().Err()
		})
		c := testFolderMutationTransportClient(transport)
		_, err := c.PrepareFolderDelete(ctx, mail.FolderDeletePreflightRequest{Target: mail.Folder{ID: "folder-id"}})
		wantAttempts := int64(len(protectedFolderAliases) + 1)
		if err == nil || attempts.Load() != wantAttempts {
			t.Fatalf("count cancellation attempts=%d want=%d err=%v", attempts.Load(), wantAttempts, err)
		}
	})

	for _, tt := range []struct {
		name string
		run  func(context.Context, *Client) error
	}{
		{name: "create write", run: func(ctx context.Context, c *Client) error {
			_, err := c.CreateFolder(ctx, mail.FolderCreateRequest{DisplayName: "Projects"})
			return err
		}},
		{name: "rename write", run: func(ctx context.Context, c *Client) error {
			_, err := c.RenameFolder(ctx, mail.FolderRenameRequest{Target: mail.Folder{ID: "folder-id"}, NewDisplayName: "Accounts"})
			return err
		}},
		{name: "delete write", run: func(ctx context.Context, c *Client) error {
			_, err := c.DeleteFolder(ctx, mail.FolderDeleteRequest{Target: mail.Folder{ID: "folder-id"}})
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var attempts atomic.Int64
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts.Add(1)
				cancel()
				return nil, req.Context().Err()
			})
			c := testFolderMutationTransportClient(transport)
			if err := tt.run(ctx, c); err == nil || attempts.Load() != 1 {
				t.Fatalf("write cancellation attempts=%d err=%v", attempts.Load(), err)
			}
		})
	}
}

func TestFolderMutationRequestsKeepIDsInsideOnePathSegment(t *testing.T) {
	for _, id := range []string{"slash/id", "space id", "query?id", "fragment#id", "percent%id", "東京"} {
		t.Run(id, func(t *testing.T) {
			var gotPath, gotQuery, gotAuth, gotType, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotQuery = r.URL.RawQuery
				gotAuth = r.Header.Get("Authorization")
				gotType = r.Header.Get("Content-Type")
				body := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(body)
				gotBody = string(body)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"returned-id","displayName":"Renamed","unknown":"hidden"}`))
			}))
			t.Cleanup(srv.Close)
			c := testFolderMutationClient(srv)
			result, err := c.RenameFolder(context.Background(), mail.FolderRenameRequest{
				Target: mail.Folder{ID: id, DisplayName: "Old"}, NewDisplayName: "Renamed",
			})
			if err != nil {
				t.Fatalf("RenameFolder: %v", err)
			}
			if gotPath != "/me/mailFolders/"+url.PathEscape(id) || gotQuery != "" {
				t.Fatalf("request path=%q query=%q for id %q", gotPath, gotQuery, id)
			}
			if gotAuth != "Bearer synthetic-token" || gotType != "application/json" {
				t.Fatalf("headers Authorization=%q Content-Type=%q", gotAuth, gotType)
			}
			if gotBody != `{"displayName":"Renamed"}` {
				t.Fatalf("body = %q", gotBody)
			}
			if result.RenamedFolder == nil || result.RenamedFolder.ID != "returned-id" {
				t.Fatalf("result = %#v", result)
			}
			if result.ProviderMeta == nil || result.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph || result.ProviderMeta.ProtectedAlias != "" {
				t.Fatalf("unknown provider field escaped into metadata: %#v", result.ProviderMeta)
			}
		})
	}
}

func TestFolderDeleteRequestHasNoBodyOrJSONContentType(t *testing.T) {
	var gotBody []byte
	var gotType string
	var gotLength int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength = r.ContentLength
		gotType = r.Header.Get("Content-Type")
		if r.Body != nil {
			gotBody = make([]byte, 1)
			n, _ := r.Body.Read(gotBody)
			gotBody = gotBody[:n]
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := testFolderMutationClient(srv)
	_, err := c.DeleteFolder(context.Background(), mail.FolderDeleteRequest{Target: mail.Folder{ID: "folder-id"}})
	if err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if gotLength != 0 || len(gotBody) != 0 || gotType != "" {
		t.Fatalf("DELETE content length=%d body=%q content-type=%q", gotLength, gotBody, gotType)
	}
}

func TestGraphFolderMutationDoAllowsPinnedRedirect(t *testing.T) {
	for _, tt := range []struct {
		name            string
		status          int
		method          string
		payload         any
		wantStatus      int
		wantBody        string
		wantContentType string
	}{
		{name: "post 307", status: http.StatusTemporaryRedirect, method: http.MethodPost, payload: map[string]any{"displayName": "Projects"}, wantStatus: http.StatusCreated, wantBody: `{"displayName":"Projects"}`, wantContentType: "application/json"},
		{name: "post 308", status: http.StatusPermanentRedirect, method: http.MethodPost, payload: map[string]any{"displayName": "Projects"}, wantStatus: http.StatusCreated, wantBody: `{"displayName":"Projects"}`, wantContentType: "application/json"},
		{name: "patch 307", status: http.StatusTemporaryRedirect, method: http.MethodPatch, payload: map[string]any{"displayName": "Accounts"}, wantStatus: http.StatusOK, wantBody: `{"displayName":"Accounts"}`, wantContentType: "application/json"},
		{name: "patch 308", status: http.StatusPermanentRedirect, method: http.MethodPatch, payload: map[string]any{"displayName": "Accounts"}, wantStatus: http.StatusOK, wantBody: `{"displayName":"Accounts"}`, wantContentType: "application/json"},
		{name: "delete 307", status: http.StatusTemporaryRedirect, method: http.MethodDelete, wantStatus: http.StatusNoContent},
		{name: "delete 308", status: http.StatusPermanentRedirect, method: http.MethodDelete, wantStatus: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var srv *httptest.Server
			var destinationHits atomic.Int64
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/start":
					w.Header().Set("Location", srv.URL+"/inside")
					w.WriteHeader(tt.status)
				case "/inside":
					destinationHits.Add(1)
					if r.Method != tt.method || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Content-Type") != tt.wantContentType {
						t.Errorf("redirected request method=%s auth=%q type=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
					}
					body, _ := io.ReadAll(r.Body)
					if string(body) != tt.wantBody {
						t.Errorf("redirected body = %q", body)
					}
					w.WriteHeader(tt.wantStatus)
					if tt.wantStatus != http.StatusNoContent {
						_, _ = w.Write([]byte(`{"id":"folder-id"}`))
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			c := testFolderMutationClient(srv)
			gotStatus, _, err := c.graphFolderMutationDo(context.Background(), tt.method, srv.URL+"/start", tt.payload)
			if err != nil || gotStatus != tt.wantStatus || destinationHits.Load() != 1 {
				t.Fatalf("allowed redirect status=%d hits=%d err=%v", gotStatus, destinationHits.Load(), err)
			}
		})
	}
}

func TestGraphFolderMutationDoRejectsMethodRewritingRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
			t.Run(http.StatusText(status)+"/"+method, func(t *testing.T) {
				var srv *httptest.Server
				var destinationHits atomic.Int64
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/start" {
						w.Header().Set("Location", srv.URL+"/inside")
						w.WriteHeader(status)
						return
					}
					destinationHits.Add(1)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"misleading-success"}`))
				}))
				t.Cleanup(srv.Close)
				c := testFolderMutationClient(srv)
				var payload any
				if method != http.MethodDelete {
					payload = map[string]any{"displayName": "Projects"}
				}
				_, _, err := c.graphFolderMutationDo(context.Background(), method, srv.URL+"/start", payload)
				if err == nil {
					t.Fatal("method-rewriting redirect succeeded")
				}
				assertFolderMutationErrorSafe(t, err, srv.URL, "/inside", "synthetic-token", "misleading-success")
				if destinationHits.Load() != 0 {
					t.Fatalf("method-rewriting destination hits = %d, want 0", destinationHits.Load())
				}
			})
		}
	}
}

func TestFolderMutationHTTPClientPreservesSettingsAndPolicy(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	rejection := errors.New("caller rejected redirect")
	var callbackCalls atomic.Int64
	original := &http.Client{
		Transport: http.DefaultTransport,
		Jar:       jar,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			callbackCalls.Add(1)
			return rejection
		},
	}
	c := &Client{graphBaseURL: "https://graph.microsoft.com/v1.0", http: original}
	guarded := c.folderMutationHTTPClient()
	if guarded == original || guarded.Transport != original.Transport || guarded.Jar != original.Jar || guarded.Timeout != original.Timeout || guarded.CheckRedirect == nil {
		t.Fatalf("guarded client did not preserve settings: original=%#v guarded=%#v", original, guarded)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/inside", nil)
	via, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/start", nil)
	if got := guarded.CheckRedirect(req, []*http.Request{via}); !errors.Is(got, rejection) {
		t.Fatalf("guarded callback error = %v", got)
	}
	if callbackCalls.Load() != 1 {
		t.Fatalf("inherited callback calls = %d", callbackCalls.Load())
	}
	if original.Transport != http.DefaultTransport || original.Jar != jar || original.Timeout != 3*time.Second || original.CheckRedirect == nil {
		t.Fatalf("original client changed: %#v", original)
	}
}

func TestGraphFolderMutationDoHonorsInheritedRedirectPolicy(t *testing.T) {
	rejection := errors.New("caller rejected redirect")
	var callbackCalls atomic.Int64
	var destinationHits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", srv.URL+"/inside")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		destinationHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	original := srv.Client()
	original.CheckRedirect = func(*http.Request, []*http.Request) error {
		callbackCalls.Add(1)
		return rejection
	}
	c := &Client{graphBaseURL: srv.URL, http: original, tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
	_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodPatch, srv.URL+"/start", map[string]any{"displayName": "Accounts"})
	if err == nil || callbackCalls.Load() != 1 || destinationHits.Load() != 0 {
		t.Fatalf("inherited policy err=%v callbacks=%d destination_hits=%d", err, callbackCalls.Load(), destinationHits.Load())
	}
	if original.CheckRedirect == nil {
		t.Fatal("original redirect policy was cleared")
	}
}

func TestGraphFolderMutationDoRejectsInheritedRedirectMutation(t *testing.T) {
	t.Run("destination", func(t *testing.T) {
		var hostileHits atomic.Int64
		hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hostileHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"hostile-success"}`))
		}))
		t.Cleanup(hostile.Close)
		hostileURL, err := url.Parse(hostile.URL + "/capture")
		if err != nil {
			t.Fatal(err)
		}

		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				w.Header().Set("Location", srv.URL+"/inside")
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		original := srv.Client()
		original.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			req.URL = hostileURL
			return nil
		}
		c := &Client{graphBaseURL: srv.URL, http: original, tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
		_, _, err = c.graphFolderMutationDo(context.Background(), http.MethodPatch, srv.URL+"/start", map[string]any{"displayName": "Accounts"})
		if err == nil {
			t.Fatal("inherited redirect destination rewrite succeeded")
		}
		assertFolderMutationErrorSafe(t, err, hostile.URL, "/capture", "synthetic-token", "hostile-success")
		if hostileHits.Load() != 0 {
			t.Fatalf("hostile destination hits = %d, want 0", hostileHits.Load())
		}
	})

	t.Run("method", func(t *testing.T) {
		var destinationHits atomic.Int64
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				w.Header().Set("Location", srv.URL+"/inside")
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			destinationHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"misleading-success"}`))
		}))
		t.Cleanup(srv.Close)
		original := srv.Client()
		original.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			req.Method = http.MethodGet
			return nil
		}
		c := &Client{graphBaseURL: srv.URL, http: original, tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
		_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodPatch, srv.URL+"/start", map[string]any{"displayName": "Accounts"})
		if err == nil {
			t.Fatal("inherited redirect method rewrite succeeded")
		}
		assertFolderMutationErrorSafe(t, err, srv.URL, "/inside", "synthetic-token", "misleading-success")
		if destinationHits.Load() != 0 {
			t.Fatalf("method-rewritten destination hits = %d, want 0", destinationHits.Load())
		}
	})
}

func TestGraphFolderMutationDoRejectsRedirectOutsideConfiguredBasePath(t *testing.T) {
	var destinationHits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/start":
			w.Header().Set("Location", srv.URL+"/beta/inside")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/beta/inside":
			destinationHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{graphBaseURL: srv.URL + "/v1.0", http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
	_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodDelete, srv.URL+"/v1.0/start", nil)
	if err == nil || destinationHits.Load() != 0 {
		t.Fatalf("base-path redirect err=%v destination_hits=%d", err, destinationHits.Load())
	}
	assertFolderMutationErrorSafe(t, err, srv.URL, "/beta/inside", "synthetic-token")
}

func TestGraphFolderMutationDoHandlesRelativeAndMalformedRedirectLocationsSafely(t *testing.T) {
	t.Run("relative inside base", func(t *testing.T) {
		var destinationHits atomic.Int64
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1.0/start":
				w.Header().Set("Location", "inside")
				w.WriteHeader(http.StatusTemporaryRedirect)
			case "/v1.0/inside":
				destinationHits.Add(1)
				if r.Method != http.MethodPatch || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Errorf("relative redirect method=%s auth=%q", r.Method, r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"displayName":"Accounts"}` {
					t.Errorf("relative redirect body=%q", body)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"folder-id"}`))
			}
		}))
		t.Cleanup(srv.Close)
		c := &Client{graphBaseURL: srv.URL + "/v1.0", http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
		status, _, err := c.graphFolderMutationDo(context.Background(), http.MethodPatch, srv.URL+"/v1.0/start", map[string]any{"displayName": "Accounts"})
		if err != nil || status != http.StatusOK || destinationHits.Load() != 1 {
			t.Fatalf("relative redirect status=%d hits=%d err=%v", status, destinationHits.Load(), err)
		}
	})

	t.Run("malformed location", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://[::1")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		t.Cleanup(srv.Close)
		c := testFolderMutationClient(srv)
		_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodDelete, srv.URL+"/start", nil)
		if err == nil {
			t.Fatal("malformed redirect location succeeded")
		}
		assertFolderMutationErrorSafe(t, err, srv.URL, "http://[::1", "synthetic-token")
	})
}

func TestGraphFolderMutationDoChecksEveryRedirectHop(t *testing.T) {
	var hostileHits atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileHits.Add(1)
	}))
	t.Cleanup(hostile.Close)
	var primary *httptest.Server
	primary = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", primary.URL+"/inside")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/inside":
			w.Header().Set("Location", hostile.URL+"/outside")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	}))
	t.Cleanup(primary.Close)
	c := testFolderMutationClient(primary)
	_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodDelete, primary.URL+"/start", nil)
	if err == nil || hostileHits.Load() != 0 {
		t.Fatalf("multi-hop redirect err=%v hostile_hits=%d", err, hostileHits.Load())
	}
}

func TestGraphFolderMutationDoKeepsDefaultRedirectLimit(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var hop int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/hop/"), "%d", &hop)
		w.Header().Set("Location", fmt.Sprintf("%s/hop/%d", srv.URL, hop+1))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c := testFolderMutationClient(srv)
	_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodGet, srv.URL+"/hop/0", nil)
	if err == nil {
		t.Fatal("redirect chain exceeded ten hops")
	}
	if hits.Load() != 10 {
		t.Fatalf("redirect hits = %d, want 10", hits.Load())
	}
}

func TestGraphFolderMutationDoConcurrentCallsKeepSharedClientUnchanged(t *testing.T) {
	var hostileHits atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileHits.Add(1)
	}))
	t.Cleanup(hostile.Close)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", srv.URL+"/inside")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/reject":
			w.Header().Set("Location", hostile.URL+"/outside")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok"}`))
		}
	}))
	t.Cleanup(srv.Close)
	shared := srv.Client()
	transport, jar, timeout, redirect := shared.Transport, shared.Jar, shared.Timeout, shared.CheckRedirect
	c := &Client{graphBaseURL: srv.URL, http: shared, tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}

	errsOut := make(chan error, 60)
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			path := "/direct"
			wantError := false
			switch index % 3 {
			case 1:
				path = "/redirect"
			case 2:
				path = "/reject"
				wantError = true
			}
			_, _, err := c.graphFolderMutationDo(context.Background(), http.MethodGet, srv.URL+path, nil)
			if (err != nil) != wantError {
				errsOut <- fmt.Errorf("request %d path %s err=%v want_error=%t", index, path, err, wantError)
			}
		}(i)
	}
	wg.Wait()
	close(errsOut)
	for err := range errsOut {
		t.Error(err)
	}
	if hostileHits.Load() != 0 {
		t.Fatalf("hostile hits = %d, want 0", hostileHits.Load())
	}
	if shared.Transport != transport || shared.Jar != jar || shared.Timeout != timeout || (shared.CheckRedirect == nil) != (redirect == nil) {
		t.Fatalf("shared client changed: %#v", shared)
	}
}

func testFolderMutationClient(srv *httptest.Server) *Client {
	return &Client{graphBaseURL: srv.URL, http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("synthetic-token"), account: "work"}
}

func testFolderMutationTransportClient(transport http.RoundTripper) *Client {
	return &Client{
		graphBaseURL: "https://graph.microsoft.com/v1.0",
		http:         &http.Client{Transport: transport},
		tokenType:    "Bearer",
		accessToken:  secretFromString("synthetic-token"),
		account:      "work",
	}
}

func testRoundTripResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func assertFolderMutationErrorSafe(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil || exitCode(err) != rungrad.ExitAPI {
		t.Fatalf("folder mutation error=%v exit=%d, want broad API error", err, exitCode(err))
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("folder mutation error leaked %q: %v", value, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	type exitCoder interface{ ExitCode() int }
	if e, ok := err.(exitCoder); ok {
		return e.ExitCode()
	}
	return 1
}
