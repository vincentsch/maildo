package outlook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

func TestInteractiveAuthCodeReturnsVerifiedCredential(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var gotChallenge atomic.Value
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			verifier := r.Form.Get("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != gotChallenge.Load().(string) {
				t.Errorf("PKCE challenge = %q, want %q", got, gotChallenge.Load())
			}
			if r.Form.Get("code") != "auth-code-secret-12345" {
				t.Errorf("code = %q", r.Form.Get("code"))
			}
			writeJSON(w, map[string]any{
				"access_token":  "access-secret-12345",
				"refresh_token": "refresh-secret-12345",
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(DefaultScopes, " "),
				"id_token":      testIDToken("tenant-123"),
			})
		case "/me":
			if got := r.Header.Get("Authorization"); got != "Bearer access-secret-12345" {
				t.Errorf("Authorization = %q", got)
			}
			writeJSON(w, map[string]any{
				"id":                "graph-id-123",
				"displayName":       "Work User",
				"mail":              "work@example.com",
				"userPrincipalName": "ignored@example.com",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := New(Options{
		AuthorizeURL:    srv.URL + "/authorize",
		TokenURL:        srv.URL + "/token",
		GraphBaseURL:    srv.URL,
		Now:             func() time.Time { return now },
		LoopbackTimeout: 2 * time.Second,
	})
	f := testFactory()
	f.BrowserOpener = func(_ context.Context, authorize string) error {
		u, err := url.Parse(authorize)
		if err != nil {
			return err
		}
		q := u.Query()
		gotChallenge.Store(q.Get("code_challenge"))
		callback := q.Get("redirect_uri") + "?code=" + url.QueryEscape("auth-code-secret-12345") + "&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(callback)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id-123"))
	if err != nil {
		t.Fatalf("InteractiveAuth: %v", err)
	}
	defer result.Credential.Destroy()
	if result.Identity.Email != "work@example.com" ||
		result.Identity.DisplayName != "Work User" ||
		result.Identity.ProviderAccountID != "graph-id-123" ||
		result.Identity.Assurance != provider.IdentityProviderVerified {
		t.Fatalf("identity = %#v", result.Identity)
	}
	cache, ok := result.Credential.(*TokenCache)
	if !ok {
		t.Fatalf("credential = %T, want *TokenCache", result.Credential)
	}
	if cache.Version != 1 || cache.Provider != "outlook" || cache.AccountName != "work" || cache.ClientID != "client-id-123" {
		t.Fatalf("cache shape = %#v", cache)
	}
	if cache.VerifiedEmail != "work@example.com" || cache.ProviderAccountID != "graph-id-123" || cache.TenantID != "tenant-123" {
		t.Fatalf("cache identity = %#v", cache)
	}
	if !cache.ExpiresAt.Equal(now.Add(time.Hour)) || !scopesSatisfy(cache.Scopes, requiredResourceScopes) {
		t.Fatalf("cache expiry/scopes = %#v", cache)
	}
	if err := storeCredential(root, "work", result.Credential); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	cacheFile := cachePath(root, "work")
	st, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %o, want 0600", got)
	}
	dirSt, err := os.Stat(filepath.Dir(cacheFile))
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if got := dirSt.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir mode = %o, want 0700", got)
	}
	client, _, err := clientFromStore(t, p, root, "work", testSetup("client-id-123"), "work@example.com", "graph-id-123")
	if err != nil {
		t.Fatalf("Client after process restart analogue: %v", err)
	}
	client.Destroy()
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls.Load())
	}
}

func TestDestroyedClientFailsBeforeGraphIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "should not be reached", http.StatusTeapot)
	}))
	defer srv.Close()
	client := &Client{graphBaseURL: srv.URL, http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("destroyed-access-secret-12345"), account: "work"}
	client.Destroy()
	err := client.graphGet(context.Background(), srv.URL+"/me/mailFolders", &struct{}{})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeProviderClientClosed {
		t.Fatalf("graphGet err = %v, want provider client closed", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("server calls = %d, want 0", calls.Load())
	}
}

func TestAuthCodeStateMismatchDoesNotExchange(t *testing.T) {
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := New(Options{AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", GraphBaseURL: srv.URL, LoopbackTimeout: time.Second})
	f := testFactory()
	f.BrowserOpener = callbackBrowser("auth-code-secret-12345", true)

	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id"))
	if err == nil || !strings.Contains(err.Error(), "did not belong to this sign-in") {
		t.Fatalf("err = %v, want state verification failure", err)
	}
	if result.Credential != nil {
		result.Credential.Destroy()
		t.Fatal("credential returned after state mismatch")
	}
	if tokenCalls.Load() != 0 {
		t.Fatalf("token endpoint was called")
	}
	assertStringNotContains(t, err.Error(), "auth-code-secret-12345")
}

func TestAuthCodeTokenEndpointErrorDoesNotReturnCredential(t *testing.T) {
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "invalid_grant", "error_description": "raw auth-code-secret-12345"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := New(Options{AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", GraphBaseURL: srv.URL, LoopbackTimeout: time.Second})
	f := testFactory()
	f.BrowserOpener = callbackBrowser("auth-code-secret-12345", false)

	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id"))
	if err == nil || !strings.Contains(err.Error(), "sign-in failed") {
		t.Fatalf("err = %v, want normalized sign-in failure", err)
	}
	if result.Credential != nil {
		result.Credential.Destroy()
		t.Fatal("credential returned after token endpoint error")
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls.Load())
	}
	assertStringNotContains(t, err.Error(), "auth-code-secret-12345", "raw")
}

func TestAuthCodeLoopbackTimeoutAndCancel(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		timeout time.Duration
		open    func(context.Context, string) error
	}{
		{
			name:    "timeout",
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			timeout: 10 * time.Millisecond,
			open:    func(context.Context, string) error { return nil },
		},
		{
			name: "cancel",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			timeout: time.Second,
			open:    func(context.Context, string) error { return nil },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var tokenCalls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenCalls.Add(1)
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			p := New(Options{AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", GraphBaseURL: srv.URL, LoopbackTimeout: tt.timeout})
			f := testFactory()
			f.BrowserOpener = tt.open
			ctx, cancel := tt.ctx()
			defer cancel()

			result, err := p.InteractiveAuth(ctx, interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id"))
			if err == nil || !strings.Contains(err.Error(), "sign-in was not completed") {
				t.Fatalf("err = %v, want incomplete sign-in", err)
			}
			if result.Credential != nil {
				result.Credential.Destroy()
				t.Fatal("credential returned after incomplete sign-in")
			}
			if tokenCalls.Load() != 0 {
				t.Fatalf("token endpoint was called")
			}
		})
	}
}

func TestMissingClientIDDoesNotOpenBrowser(t *testing.T) {
	p := New(Options{})
	f := testFactory()
	var browserCalls atomic.Int64
	f.BrowserOpener = func(context.Context, string) error {
		browserCalls.Add(1)
		return nil
	}

	_, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, ""))
	assertErrCode(t, err, errs.CodeAuthAppConfigMissing, 1)
	if browserCalls.Load() != 0 {
		t.Fatalf("browser calls = %d, want 0", browserCalls.Load())
	}
}

func TestGraphIdentityProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile map[string]any
		want    string
		code    string
	}{
		{
			name:    "mail",
			profile: map[string]any{"id": "id-1", "displayName": "Mail User", "mail": "mail@example.com", "userPrincipalName": "upn@example.com"},
			want:    "mail@example.com",
		},
		{
			name:    "upn fallback",
			profile: map[string]any{"id": "id-2", "displayName": "UPN User", "mail": "", "userPrincipalName": "personal@example.com"},
			want:    "personal@example.com",
		},
		{
			name:    "unusable",
			profile: map[string]any{"id": "id-3", "displayName": "No Mail", "mail": "", "userPrincipalName": "not-email"},
			code:    errs.CodeProviderIdentityUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, f := providerWithStaticGraph(t, tt.profile)
			result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id"))
			if result.Credential != nil {
				defer result.Credential.Destroy()
			}
			if tt.code != "" {
				assertErrCode(t, err, tt.code, 3)
				if result.Credential != nil {
					t.Fatal("credential returned after unusable identity")
				}
				return
			}
			if err != nil {
				t.Fatalf("InteractiveAuth: %v", err)
			}
			if result.Identity.Email != tt.want {
				t.Fatalf("email = %q, want %q", result.Identity.Email, tt.want)
			}
		})
	}
}

func TestDeviceCodeLoginPollsWithoutBrowserAndRedactsInstructions(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	var tokenCalls atomic.Int64
	var browserCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devicecode":
			writeJSON(w, map[string]any{
				"device_code":      "device-code-secret-12345",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://microsoft.example/device",
				"expires_in":       600,
				"interval":         1,
			})
		case "/token":
			if tokenCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(w, map[string]any{
				"access_token":  "device-access-secret-12345",
				"refresh_token": "device-refresh-secret-12345",
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(DefaultScopes, " "),
			})
		case "/me":
			writeJSON(w, map[string]any{"id": "device-id", "displayName": "Device User", "mail": "device@example.com"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := New(Options{
		TokenURL:        srv.URL + "/token",
		DeviceCodeURL:   srv.URL + "/devicecode",
		GraphBaseURL:    srv.URL,
		Now:             func() time.Time { return now },
		DevicePollSleep: func(context.Context, time.Duration) error { return nil },
	})
	f := testFactory()
	f.BrowserOpener = func(context.Context, string) error {
		browserCalls.Add(1)
		return nil
	}

	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDeviceCode, "client-id"))
	if err != nil {
		t.Fatalf("device InteractiveAuth: %v", err)
	}
	defer result.Credential.Destroy()
	if browserCalls.Load() != 0 {
		t.Fatalf("browser calls = %d, want 0", browserCalls.Load())
	}
	stderr := f.Stderr.(*bytes.Buffer).String()
	if !strings.Contains(stderr, "https://microsoft.example/device") || !strings.Contains(stderr, "ABCD-EFGH") {
		t.Fatalf("stderr missing device instructions: %q", stderr)
	}
	for _, secret := range []string{"device-code-secret-12345", "device-access-secret-12345", "device-refresh-secret-12345"} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("stderr leaked %q: %s", secret, stderr)
		}
	}
}

func TestDeviceCodeMissingRefreshTokenDoesNotReturnCredential(t *testing.T) {
	var browserCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devicecode":
			writeJSON(w, map[string]any{
				"device_code":      "device-code-secret-12345",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://microsoft.example/device",
				"expires_in":       600,
				"interval":         1,
				"message":          "raw message should not be printed",
			})
		case "/token":
			writeJSON(w, map[string]any{
				"access_token": "device-access-secret-12345",
				"expires_in":   3600,
				"token_type":   "Bearer",
				"scope":        strings.Join(DefaultScopes, " "),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := New(Options{
		TokenURL:        srv.URL + "/token",
		DeviceCodeURL:   srv.URL + "/devicecode",
		GraphBaseURL:    srv.URL,
		DevicePollSleep: func(context.Context, time.Duration) error { return nil },
	})
	f := testFactory()
	f.BrowserOpener = func(context.Context, string) error {
		browserCalls.Add(1)
		return nil
	}

	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDeviceCode, "client-id"))
	if err == nil || !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("err = %v, want missing refresh token", err)
	}
	if result.Credential != nil {
		result.Credential.Destroy()
		t.Fatal("credential returned after missing device refresh token")
	}
	if browserCalls.Load() != 0 {
		t.Fatalf("browser calls = %d, want 0", browserCalls.Load())
	}
	testutilOut := f.Stdout.(*bytes.Buffer).String() + f.Stderr.(*bytes.Buffer).String()
	assertStringNotContains(t, testutilOut, "device-code-secret-12345", "device-access-secret-12345", "raw message")
}

func TestRefreshAndClientLifecycle(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var tokenCalls atomic.Int64
	var browserCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			call := tokenCalls.Add(1)
			refreshToken := "new-refresh-secret-12345"
			if call > 1 {
				refreshToken = "newer-refresh-secret-12345"
			}
			writeJSON(w, map[string]any{
				"access_token":  "new-access-secret-12345",
				"refresh_token": refreshToken,
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(DefaultScopes, " "),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cache := healthyCache("work", now.Add(-time.Minute))
	cache.SetRefreshToken("old-refresh-secret-12345")
	if err := storeCache(root, "work", cache); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	p := New(Options{TokenURL: srv.URL + "/token", Now: func() time.Time { return now }})
	f := testFactory()
	f.BrowserOpener = func(context.Context, string) error {
		browserCalls.Add(1)
		return nil
	}

	client, changed, err := clientFromStoreWithFactory(t, p, f, root, "work", testSetup("client-id"), cache.VerifiedEmail, cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("Client refresh: %v", err)
	}
	defer client.Destroy()
	if !changed {
		t.Fatal("expired credential was not replaced")
	}
	if client.VerifiedEmail() != cache.VerifiedEmail {
		t.Fatalf("client email = %q", client.VerifiedEmail())
	}
	if tokenCalls.Load() != 1 || browserCalls.Load() != 0 {
		t.Fatalf("calls token=%d browser=%d", tokenCalls.Load(), browserCalls.Load())
	}
	loaded := loadCache(t, root, "work")
	if loaded.AccessTokenValue() != "new-access-secret-12345" || loaded.RefreshTokenValue() != "new-refresh-secret-12345" {
		t.Fatalf("refreshed cache = %#v", loaded)
	}
	data, err := os.ReadFile(cachePath(root, "work"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if bytes.Contains(data, []byte("old-refresh-secret-12345")) {
		t.Fatalf("old refresh token remained in file: %s", data)
	}
	healthy, changed, err := clientFromStoreWithFactory(t, p, f, root, "work", testSetup("client-id"), cache.VerifiedEmail, cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("Client healthy: %v", err)
	}
	healthy.Destroy()
	if changed {
		t.Fatal("healthy credential was replaced")
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("healthy client refreshed again")
	}
	result, changed, err := forcedRefreshFromStore(t, p, f, root, "work", testSetup("client-id"), cache.VerifiedEmail, cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("ForcedRefresh: %v", err)
	}
	if !result.RefreshChecked || !result.CredentialChanged || !changed || !result.RotatedRefreshToken {
		t.Fatalf("refresh result = %#v changed=%t", result, changed)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("forced refresh calls = %d, want 2", tokenCalls.Load())
	}
}

func TestClientRefreshesWithinSkewAndKeepsOmittedRefreshToken(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		writeJSON(w, map[string]any{
			"access_token": "skew-access-secret-12345",
			"expires_in":   3600,
			"token_type":   "Bearer",
			"scope":        strings.Join(DefaultScopes, " "),
		})
	}))
	defer srv.Close()
	cache := healthyCache("work", now.Add(100*time.Second))
	cache.SetRefreshToken("stable-refresh-secret-12345")
	if err := storeCache(root, "work", cache); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	p := New(Options{TokenURL: srv.URL + "/token", Now: func() time.Time { return now }})

	client, _, err := clientFromStore(t, p, root, "work", testSetup("client-id"), cache.VerifiedEmail, cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	client.Destroy()
	loaded := loadCache(t, root, "work")
	if tokenCalls.Load() != 1 || loaded.AccessTokenValue() != "skew-access-secret-12345" || loaded.RefreshTokenValue() != "stable-refresh-secret-12345" {
		t.Fatalf("refresh token handling: calls=%d cache=%#v", tokenCalls.Load(), loaded)
	}
}

func TestLocalHealthAndRepairRequired(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*TokenCache)
		want   provider.LocalStatus
	}{
		{name: "usable", want: provider.LocalUsable},
		{name: "wrong version", mutate: func(c *TokenCache) { c.Version = 2 }, want: provider.LocalRepairRequired},
		{name: "missing refresh", mutate: func(c *TokenCache) { c.SetRefreshToken("") }, want: provider.LocalRepairRequired},
		{name: "under scoped", mutate: func(c *TokenCache) { c.Scopes = []string{"User.Read"} }, want: provider.LocalRepairRequired},
		{name: "identity mismatch", mutate: func(c *TokenCache) { c.ProviderAccountID = "other-id" }, want: provider.LocalRepairRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache := healthyCache("work", now.Add(time.Hour))
			if tt.mutate != nil {
				tt.mutate(cache)
			}
			if err := storeCache(root, "work", cache); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			got, err := localHealthFromStore(t, New(Options{}), root, "work", testSetup("client-id"), "work@example.com", "provider-work")
			if err != nil {
				t.Fatalf("LocalHealth: %v", err)
			}
			if got != tt.want {
				t.Fatalf("status = %v, want %v", got, tt.want)
			}
		})
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(cachePath(root, "work")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath(root, "work"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := localHealthFromStore(t, New(Options{}), root, "work", testSetup("client-id"), "", "")
	if err != nil || got != provider.LocalRepairRequired {
		t.Fatalf("corrupt LocalHealth = (%v, %v)", got, err)
	}
}

func TestClientBrokenCachesAreRepairRequired(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		mutate             func(*TokenCache)
		expectedEmail      string
		expectedProviderID string
	}{
		{name: "missing access", mutate: func(c *TokenCache) { c.SetAccessToken("") }},
		{name: "missing refresh", mutate: func(c *TokenCache) { c.SetRefreshToken("") }},
		{name: "wrong version", mutate: func(c *TokenCache) { c.Version = 2 }},
		{name: "under scoped", mutate: func(c *TokenCache) { c.Scopes = []string{"User.Read"} }},
		{name: "provider id mismatch", mutate: func(c *TokenCache) { c.ProviderAccountID = "other-id" }, expectedProviderID: "provider-work"},
		{name: "email mismatch fallback", mutate: func(c *TokenCache) { c.ProviderAccountID = ""; c.VerifiedEmail = "other@example.com" }, expectedEmail: "work@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache := healthyCache("work", now.Add(time.Hour))
			tt.mutate(cache)
			if err := storeCache(root, "work", cache); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			_, _, err := clientFromStore(t, New(Options{Now: func() time.Time { return now }}), root, "work", testSetup("client-id"), tt.expectedEmail, tt.expectedProviderID)
			assertErrCode(t, err, errs.CodeAuthRepairRequired, 3)
			if !strings.Contains(err.Error(), `maildo auth repair work`) {
				t.Fatalf("repair command missing from error: %v", err)
			}
		})
	}
}

func TestCodecRejectsCrossProviderCredentialBytes(t *testing.T) {
	cache := healthyCache("work", time.Now().Add(time.Hour))
	cache.Provider = "gmail"
	data, err := encodeTokenCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := CredentialCodec{}.Decode(testRuntime{f: testFactory()}, auth.CredentialBinding{Provider: "outlook", Account: "work"}, data)
	if err == nil {
		cred.Destroy()
		t.Fatal("cross-provider credential decoded successfully")
	}
	if !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("err = %v, want credential capability closed", err)
	}
}

func TestCodecDecodePropagatesSecretRegistrationError(t *testing.T) {
	cache := healthyCache("work", time.Now().Add(time.Hour))
	data, err := encodeTokenCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CredentialCodec{}.Decode(failingRegistrar{}, auth.CredentialBinding{Provider: "outlook", Account: "work"}, data)
	if !errors.Is(err, provider.ErrProviderRuntimeClosed) {
		t.Fatalf("Decode err = %v, want provider runtime closed", err)
	}
}

func TestCodecRebindsUnknownFieldCredentialAsRepairRequired(t *testing.T) {
	raw := []byte(`{
  "version": 1,
  "provider": "outlook",
  "account_name": "work",
  "scopes": ["User.Read", "Mail.ReadWrite", "Mail.Send", "MailboxSettings.ReadWrite"],
  "token_type": "Bearer",
  "access_token": "unknown-access-secret-12345",
  "refresh_token": "unknown-refresh-secret-12345",
  "expires_at": "2030-01-01T00:00:00Z",
  "future_cache_field": {"preserve": true}
}
`)
	result, err := CredentialCodec{}.RebindCredential(testRuntime{f: testFactory()}, raw, auth.CredentialBinding{Provider: "outlook", Account: "work"}, auth.CredentialBinding{Provider: "outlook", Account: "renamed"})
	if err != nil {
		t.Fatalf("RebindCredential: %v", err)
	}
	if result.Disposition != auth.CredentialReboundNeedsRepair {
		t.Fatalf("disposition = %q, want %q", result.Disposition, auth.CredentialReboundNeedsRepair)
	}
	var rebound map[string]json.RawMessage
	if err := json.Unmarshal(result.Bytes, &rebound); err != nil {
		t.Fatalf("decode rebound bytes: %v", err)
	}
	if string(rebound["provider"]) != `"outlook"` || string(rebound["account_name"]) != `"renamed"` {
		t.Fatalf("binding not rewritten: %s", string(result.Bytes))
	}
	if _, ok := rebound["future_cache_field"]; !ok {
		t.Fatalf("unknown field was not preserved: %s", string(result.Bytes))
	}
}

func TestCodecRebindPreservesUnsupportedVersion(t *testing.T) {
	cache := healthyCache("work", time.Now().Add(time.Hour))
	cache.Version = 99
	data, err := encodeTokenCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	result, err := CredentialCodec{}.RebindCredential(testRuntime{f: testFactory()}, data, auth.CredentialBinding{Provider: "outlook", Account: "work"}, auth.CredentialBinding{Provider: "outlook", Account: "renamed"})
	if err != nil {
		t.Fatalf("RebindCredential: %v", err)
	}
	if result.Disposition != auth.CredentialPreservedNeedsRepair {
		t.Fatalf("disposition = %q, want %q", result.Disposition, auth.CredentialPreservedNeedsRepair)
	}
	if !bytes.Equal(result.Bytes, data) {
		t.Fatal("unsupported-version cache was rewritten")
	}
}

func TestRefreshTokenAuthFailuresAreRepairRequired(t *testing.T) {
	for _, tokenError := range []string{"invalid_grant", "consent_required", "interaction_required", "invalid_scope"} {
		t.Run(tokenError, func(t *testing.T) {
			root := t.TempDir()
			if err := storeCache(root, "work", healthyCache("work", time.Now().Add(time.Hour))); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			var tokenCalls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{
					"error":             tokenError,
					"error_description": "raw provider response with refresh-secret-work",
				})
			}))
			defer srv.Close()
			_, _, err := forcedRefreshFromStore(t, New(Options{TokenURL: srv.URL}), testFactory(), root, "work", testSetup("client-id"), "", "")
			assertErrCode(t, err, errs.CodeAuthRepairRequired, 3)
			if tokenCalls.Load() != 1 {
				t.Fatalf("token calls = %d, want 1", tokenCalls.Load())
			}
			assertStringNotContains(t, err.Error(), "refresh-secret", "raw provider response", tokenError)
		})
	}
}

func TestEndpointDerivation(t *testing.T) {
	p := New(Options{Authority: "https://login.example.test/common/"})
	if got := p.endpointForAuthority("https://login.example.test/common", "authorize"); got != "https://login.example.test/common/oauth2/v2.0/authorize" {
		t.Fatalf("authorize = %q", got)
	}
	if got := p.endpointForAuthority("https://login.example.test/common", "token"); got != "https://login.example.test/common/oauth2/v2.0/token" {
		t.Fatalf("token = %q", got)
	}
	if got := p.endpointForAuthority("https://login.example.test/common", "devicecode"); got != "https://login.example.test/common/oauth2/v2.0/devicecode" {
		t.Fatalf("devicecode = %q", got)
	}
	override := New(Options{Authority: "https://login.example.test/common", TokenURL: "https://override.example/token"})
	if got := override.endpointForAuthority("https://cache.example/tenant", "token"); got != "https://override.example/token" {
		t.Fatalf("override token = %q", got)
	}
}

func TestRefreshDerivesTokenEndpointFromCacheAuthority(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/v2.0/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		writeJSON(w, map[string]any{
			"access_token":  "authority-access-secret-12345",
			"refresh_token": "authority-refresh-secret-12345",
			"expires_in":    3600,
			"token_type":    "Bearer",
			"scope":         strings.Join(DefaultScopes, " "),
		})
	}))
	defer srv.Close()
	cache := healthyCache("work", now.Add(-time.Minute))
	cache.Authority = srv.URL
	if err := storeCache(root, "work", cache); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	_, _, err := forcedRefreshFromStore(t, New(Options{Authority: "https://login.default.example/common", Now: func() time.Time { return now }}), testFactory(), root, "work", testSetup("client-id"), "", "")
	if err != nil {
		t.Fatalf("ForcedRefresh: %v", err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want cache authority endpoint", tokenCalls.Load())
	}
}

func TestLocalHealthRequiresCachedOrFallbackClientID(t *testing.T) {
	root := t.TempDir()
	cache := healthyCache("work", time.Now().Add(time.Hour))
	cache.ClientID = ""
	if err := storeCache(root, "work", cache); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	got, err := localHealthFromStore(t, New(Options{}), root, "work", maildoconfig.ProviderSetup{Outlook: &maildoconfig.OutlookSetup{}}, "", cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("LocalHealth: %v", err)
	}
	if got != provider.LocalRepairRequired {
		t.Fatalf("local health without client id = %v, want repair", got)
	}
	got, err = localHealthFromStore(t, New(Options{}), root, "work", testSetup("fallback-client-id"), "", cache.ProviderAccountID)
	if err != nil {
		t.Fatalf("LocalHealth fallback: %v", err)
	}
	if got != provider.LocalUsable {
		t.Fatalf("local health with fallback = %v, want usable", got)
	}
}

func interactiveContext(f *rungrad.Factory, account string, flow provider.InteractiveAuthFlow, clientID string) provider.InteractiveAuthContext {
	return provider.InteractiveAuthContext{
		Runtime:   testRuntime{f: f},
		Operation: provider.InteractiveLogin,
		Flow:      flow,
		Provider:  "outlook",
		Account:   account,
		Setup:     testSetup(clientID),
	}
}

func testSetup(clientID string) maildoconfig.ProviderSetup {
	setup := maildoconfig.ProviderSetup{Outlook: &maildoconfig.OutlookSetup{}}
	if strings.TrimSpace(clientID) != "" {
		setup.Outlook.ClientID = clientID
	}
	return setup
}

type testRuntime struct {
	f *rungrad.Factory
}

type failingRegistrar struct{}

func (failingRegistrar) RegisterSecret(string) error {
	return provider.ErrProviderRuntimeClosed
}

func (r testRuntime) RegisterSecret(value string) error {
	if r.f != nil {
		r.f.RegisterSecret(value)
	}
	return nil
}

func (r testRuntime) LookupSecretEnv(name string) (string, bool, error) {
	if r.f != nil && r.f.LookupEnv != nil {
		value, ok := r.f.LookupEnv(name)
		return value, ok, nil
	}
	return "", false, nil
}

func (r testRuntime) OpenBrowser(ctx context.Context, authorize string) error {
	if r.f == nil {
		return provider.ErrProviderRuntimeDenied
	}
	return r.f.OpenBrowser(ctx, authorize)
}

func (r testRuntime) WriteNotice(text string) error {
	if r.f != nil && r.f.Stderr != nil {
		fmt.Fprintln(r.f.Stderr, text)
	}
	return nil
}

type testCredentialSession struct {
	binding      auth.CredentialBinding
	current      provider.Credential
	allowReplace bool
	admitted     bool
	changed      bool
	rotated      bool
}

func (s *testCredentialSession) WithCurrent(ctx context.Context, fn func(provider.CredentialLease) error) error {
	if s == nil || fn == nil || s.admitted {
		return provider.ErrCredentialCapabilityClosed
	}
	s.admitted = true
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return fn(testCredentialLease{session: s})
}

func (s *testCredentialSession) destroy() {
	if s != nil && s.current != nil {
		s.current.Destroy()
		s.current = nil
	}
}

type testCredentialLease struct {
	session *testCredentialSession
}

func (l testCredentialLease) Current() provider.Credential {
	if l.session == nil {
		return nil
	}
	return l.session.current
}

func (l testCredentialLease) Replace(next provider.Credential) error {
	s := l.session
	if s == nil || !s.allowReplace || next == nil || next.Destroyed() {
		if next != nil {
			next.Destroy()
		}
		return provider.ErrCredentialCapabilityClosed
	}
	rotated, err := CredentialCodec{}.RefreshTokenRotated(s.binding, s.current, next)
	if err != nil {
		next.Destroy()
		return provider.ErrCredentialCapabilityClosed
	}
	if s.current != nil {
		s.current.Destroy()
	}
	s.current = next
	s.changed = true
	s.rotated = rotated
	return nil
}

func clientFromStore(t *testing.T, p *Provider, root, account string, setup maildoconfig.ProviderSetup, expectedEmail, expectedProviderID string) (*Client, bool, error) {
	t.Helper()
	return clientFromStoreWithFactory(t, p, testFactory(), root, account, setup, expectedEmail, expectedProviderID)
}

func clientFromStoreWithFactory(t *testing.T, p *Provider, f *rungrad.Factory, root, account string, setup maildoconfig.ProviderSetup, expectedEmail, expectedProviderID string) (*Client, bool, error) {
	t.Helper()
	ac, session, err := storedContextFromStore(t, f, root, account, setup, expectedEmail, expectedProviderID, true)
	if err != nil {
		return nil, false, err
	}
	defer session.destroy()
	client, err := p.Client(context.Background(), ac)
	if err != nil {
		return nil, false, err
	}
	if err := persistChanged(root, account, session); err != nil {
		client.Destroy()
		return nil, false, err
	}
	return client, session.changed, nil
}

func forcedRefreshFromStore(t *testing.T, p *Provider, f *rungrad.Factory, root, account string, setup maildoconfig.ProviderSetup, expectedEmail, expectedProviderID string) (provider.RefreshResult, bool, error) {
	t.Helper()
	ac, session, err := storedContextFromStore(t, f, root, account, setup, expectedEmail, expectedProviderID, true)
	if err != nil {
		return provider.RefreshResult{}, false, err
	}
	defer session.destroy()
	result, err := p.ForcedRefresh(context.Background(), ac)
	if err != nil {
		return provider.RefreshResult{}, false, err
	}
	if err := persistChanged(root, account, session); err != nil {
		return provider.RefreshResult{}, false, err
	}
	return result, session.changed, nil
}

func localHealthFromStore(t *testing.T, p *Provider, root, account string, setup maildoconfig.ProviderSetup, expectedEmail, expectedProviderID string) (provider.LocalStatus, error) {
	t.Helper()
	ac, session, err := storedContextFromStore(t, testFactory(), root, account, setup, expectedEmail, expectedProviderID, false)
	if err != nil {
		if errors.Is(err, provider.ErrCredentialCapabilityClosed) {
			return provider.LocalRepairRequired, nil
		}
		return provider.LocalAbsent, err
	}
	defer session.destroy()
	return p.LocalHealth(context.Background(), ac)
}

func storedContextFromStore(t *testing.T, f *rungrad.Factory, root, account string, setup maildoconfig.ProviderSetup, expectedEmail, expectedProviderID string, allowReplace bool) (provider.StoredAuthContext, *testCredentialSession, error) {
	t.Helper()
	binding := auth.CredentialBinding{Provider: "outlook", Account: account}
	data, present, err := auth.NewStore(root).LoadBytes(binding.Provider, binding.Account)
	if err != nil {
		return provider.StoredAuthContext{}, nil, err
	}
	if !present {
		return provider.StoredAuthContext{}, nil, repairRequired(account)
	}
	cred, err := CredentialCodec{}.Decode(testRuntime{f: f}, binding, data)
	if err != nil {
		return provider.StoredAuthContext{}, nil, provider.ErrCredentialCapabilityClosed
	}
	session := &testCredentialSession{binding: binding, current: cred, allowReplace: allowReplace}
	ac := provider.StoredAuthContext{
		Runtime:                   testRuntime{f: f},
		Provider:                  binding.Provider,
		Account:                   binding.Account,
		Setup:                     setup.Clone(),
		ExpectedEmail:             expectedEmail,
		ExpectedProviderAccountID: expectedProviderID,
		Credential:                session,
	}
	return ac, session, nil
}

func storeCredential(root, account string, credential provider.Credential) error {
	binding := auth.CredentialBinding{Provider: "outlook", Account: account}
	data, err := CredentialCodec{}.Encode(binding, credential)
	if err != nil {
		return err
	}
	return auth.NewStore(root).SaveBytes(binding.Provider, binding.Account, data)
}

func storeCache(root, account string, cache *TokenCache) error {
	data, err := encodeTokenCache(cache)
	if err != nil {
		return err
	}
	return auth.NewStore(root).SaveBytes("outlook", account, data)
}

func persistChanged(root, account string, session *testCredentialSession) error {
	if session == nil || !session.changed {
		return nil
	}
	binding := auth.CredentialBinding{Provider: "outlook", Account: account}
	data, err := CredentialCodec{}.Encode(binding, session.current)
	if err != nil {
		return err
	}
	return auth.NewStore(root).SaveBytes(binding.Provider, binding.Account, data)
}

func loadCache(t *testing.T, root, account string) *TokenCache {
	t.Helper()
	data, present, err := auth.NewStore(root).LoadBytes("outlook", account)
	if err != nil {
		t.Fatalf("load cache bytes: %v", err)
	}
	if !present {
		t.Fatalf("cache for %s is absent", account)
	}
	cred, err := CredentialCodec{}.Decode(testRuntime{f: testFactory()}, auth.CredentialBinding{Provider: "outlook", Account: account}, data)
	if err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	cache, ok := cred.(*TokenCache)
	if !ok {
		cred.Destroy()
		t.Fatalf("credential = %T, want *TokenCache", cred)
	}
	t.Cleanup(cache.Destroy)
	return cache
}

func callbackBrowser(code string, wrongState bool) func(context.Context, string) error {
	return func(_ context.Context, authorize string) error {
		u, err := url.Parse(authorize)
		if err != nil {
			return err
		}
		state := u.Query().Get("state")
		if wrongState {
			state = "wrong-state"
		}
		callback := u.Query().Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
		resp, err := http.Get(callback)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
}

func providerWithStaticGraph(t *testing.T, profile map[string]any) (*Provider, *rungrad.Factory) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, map[string]any{
				"access_token":  "access-secret-12345",
				"refresh_token": "refresh-secret-12345",
				"expires_in":    3600,
				"token_type":    "Bearer",
				"scope":         strings.Join(DefaultScopes, " "),
			})
		case "/me":
			writeJSON(w, profile)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p := New(Options{AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", GraphBaseURL: srv.URL, LoopbackTimeout: time.Second})
	f := testFactory()
	f.BrowserOpener = func(_ context.Context, authorize string) error {
		u, _ := url.Parse(authorize)
		callback := u.Query().Get("redirect_uri") + "?code=auth-code-secret-12345&state=" + url.QueryEscape(u.Query().Get("state"))
		resp, err := http.Get(callback)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	return p, f
}

func healthyCache(account string, expires time.Time) *TokenCache {
	return NewTokenCache(TokenCacheData{
		Version:           1,
		Provider:          "outlook",
		AccountName:       account,
		Authority:         "https://login.microsoftonline.com/common",
		ClientID:          "client-id",
		Scopes:            append([]string(nil), DefaultScopes...),
		TokenType:         "Bearer",
		AccessToken:       "access-secret-" + account,
		RefreshToken:      "refresh-secret-" + account,
		ExpiresAt:         expires,
		VerifiedEmail:     account + "@example.com",
		DisplayName:       "Account " + account,
		ProviderAccountID: "provider-" + account,
	})
}

func testFactory() *rungrad.Factory {
	return &rungrad.Factory{
		Flags:  &rungrad.GlobalFlags{JSON: true},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func testIDToken(tenant string) string {
	payload, _ := json.Marshal(map[string]string{"tid": tenant})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func assertErrCode(t *testing.T, err error, code string, exit int) {
	t.Helper()
	var productErr *errs.Error
	if !errors.As(err, &productErr) {
		t.Fatalf("err = %T %v, want product error %s", err, err, code)
	}
	if productErr.Code() != code || productErr.ExitCode() != exit {
		t.Fatalf("error = %#v, want code=%s exit=%d", productErr, code, exit)
	}
}

func assertStringNotContains(t *testing.T, s string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(s, value) {
			t.Fatalf("%q unexpectedly contains %q", s, value)
		}
	}
}

func cachePath(root, account string) string {
	return filepath.Join(root, "auth", "outlook", account+".json")
}

// Over SSH or in a coding agent's shell no browser opens, so sign-in shows
// the address and keeps waiting for the user to open it elsewhere.
func TestAuthCodeSignInShowsTheAddressAndWaitsWhenNoBrowserOpens(t *testing.T) {
	p, f := providerWithStaticGraph(t, map[string]any{"id": "graph-id", "mail": "work@example.com"})
	var shown string
	f.BrowserOpener = func(_ context.Context, authorize string) error {
		shown = authorize
		u, _ := url.Parse(authorize)
		callback := u.Query().Get("redirect_uri") + "?code=auth-code-secret-12345&state=" + url.QueryEscape(u.Query().Get("state"))
		// The user opens the printed address in another browser.
		go func() {
			if resp, err := http.Get(callback); err == nil {
				_ = resp.Body.Close()
			}
		}()
		return errors.New("no browser here")
	}
	result, err := p.InteractiveAuth(context.Background(), interactiveContext(f, "work", provider.InteractiveAuthFlowDefault, "client-id"))
	if err != nil {
		t.Fatalf("InteractiveAuth: %v", err)
	}
	result.Credential.Destroy()
	stderr := f.Stderr.(*bytes.Buffer).String()
	want := []string{
		"Opening your browser to sign in. If it does not open, visit this address:\n" + shown + "\n",
		"The browser did not open. Open the address above in a browser on this computer.\n",
		"Waiting for you to finish signing in (up to 1s)...\n",
	}
	last := -1
	for _, w := range want {
		at := strings.Index(stderr, w)
		if at <= last {
			t.Fatalf("stderr is missing %q or has it out of order:\n%s", w, stderr)
		}
		last = at
	}
}
