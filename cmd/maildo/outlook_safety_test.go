package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestOutlookSendCLIReportsUnconfirmedWritesAndKeepsContentPrivate(t *testing.T) {
	for _, stage := range []string{"lost_response", "truncated_202", "malformed_202", "unexpected_200", "server_error", "dial_failure"} {
		for _, flags := range [][]string{{"--json"}, {}, {"--jq", "."}, {"--template", "{{.}}"}, {"--json", "--include-meta"}} {
			t.Run(stage+"/"+strings.Join(flags, "_"), func(t *testing.T) {
				h := seededOutlookMessagesHarness(t)
				var submissions atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/me/sendMail" {
						t.Errorf("unexpected operation: %s", r.URL.Path)
						w.WriteHeader(500)
						return
					}
					io.Copy(io.Discard, r.Body)
					submissions.Add(1)
					switch stage {
					case "lost_response":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					case "truncated_202":
						w.Header().Set("Content-Length", "99")
						w.WriteHeader(202)
						io.WriteString(w, "SYNTHETIC RESPONSE BODY")
					case "malformed_202":
						w.WriteHeader(202)
						io.WriteString(w, "SYNTHETIC RESPONSE BODY")
					case "unexpected_200":
						w.WriteHeader(200)
					default:
						w.WriteHeader(500)
					}
				}))
				defer server.Close()
				client := server.Client()
				if stage == "dial_failure" {
					client = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
						return nil, errors.New("SYNTHETIC TRANSPORT DETAIL")
					}}}
				}
				h.providers = testkit.NewOutlookRegistry(outlook.Options{GraphBaseURL: server.URL, HTTPClient: client, Now: func() time.Time { return h.now }})
				args := append([]string{"send", "--to", "client@example.test", "--body", "SYNTHETIC COMPOSE BODY"}, flags...)
				res := approvedRun(t, h.runNoBrowser, args...)
				if res.Exit != 1 || res.Stdout != "" {
					t.Fatalf("exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
				}
				out := res.Stdout + res.Stderr
				for _, secret := range []string{"SYNTHETIC COMPOSE BODY", "SYNTHETIC RESPONSE BODY", "SYNTHETIC TRANSPORT DETAIL", server.URL, h.root} {
					if strings.Contains(out, secret) {
						t.Fatalf("error leaked %q: %s", secret, out)
					}
				}
				if len(flags) == 0 {
					if !strings.Contains(out, "did not hand the message to Outlook") && !strings.Contains(out, "could not confirm the result") {
						t.Fatalf("misleading plain error: %s", out)
					}
				} else {
					var machine struct {
						Code string `json:"code"`
						errs.WriteOutcomeFacts
					}
					if err := json.Unmarshal([]byte(res.Stderr), &machine); err != nil {
						t.Fatal(err)
					}
					if stage == "dial_failure" {
						if machine.Outcome != errs.WriteOutcomeNotApplied || machine.WriteMayHaveLanded || !machine.RetrySafe {
							t.Fatalf("pre-dispatch: %+v", machine)
						}
					} else if machine.Code != errs.CodeVerificationFailed || machine.Outcome != errs.WriteOutcomeUnknown || !machine.WriteMayHaveLanded || machine.RetrySafe || machine.AcceptedByProvider || machine.ConfirmedAfterWrite || machine.Succeeded {
						t.Fatalf("unconfirmed write: %+v", machine)
					}
				}
				want := int32(1)
				if stage == "dial_failure" {
					want = 0
				}
				if submissions.Load() != want {
					t.Fatalf("submissions=%d want=%d", submissions.Load(), want)
				}
				// A failed send is logged, since it may have reached the
				// server; the body never is.
				logged, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
				if err != nil || !strings.Contains(string(logged), `"outcome":"failed"`) || strings.Contains(string(logged), "SYNTHETIC COMPOSE BODY") {
					t.Fatalf("action log = %s, err %v", logged, err)
				}
			})
		}
	}
}

func TestOutlookAttachmentPaginationCLIDoesNotPublishPartialMetadata(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "part", "name": "PARTIAL METADATA", "size": 1}}, "@odata.nextLink": "http://" + r.Host + r.URL.RequestURI()})
	}))
	defer server.Close()
	h.providers = testkit.NewOutlookRegistry(outlook.Options{GraphBaseURL: server.URL, Now: func() time.Time { return h.now }})
	res := h.runNoBrowser("attachments", "list", "--id", "msg", "--json")
	if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stderr, "PARTIAL METADATA") || calls.Load() != 1 {
		t.Fatalf("cycle: calls=%d exit=%d stdout=%s stderr=%s", calls.Load(), res.Exit, res.Stdout, res.Stderr)
	}
}

func TestUnsafeCredentialCLIStopsBeforeProviderContact(t *testing.T) {
	for _, kind := range []string{"file_0644", "final_link", "provider_link"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && kind == "file_0644" {
				t.Skip("Unix permission case; Windows enforces DACLs")
			}
			h := seededOutlookMessagesHarness(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			h.providers = testkit.NewOutlookRegistry(outlook.Options{GraphBaseURL: server.URL, TokenURL: server.URL + "/token", Now: func() time.Time { return h.now }})
			dir := filepath.Join(h.root, "auth", "outlook")
			path := filepath.Join(dir, "work.json")
			switch kind {
			case "file_0644":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "final_link":
				outside := filepath.Join(t.TempDir(), "synthetic.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "provider_link":
				outside := filepath.Join(t.TempDir(), "synthetic-dir")
				if err := os.Rename(dir, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			for _, args := range [][]string{{"folders", "list", "--json"}, {"auth", "status", "--json"}, {"auth", "refresh", "--json"}} {
				res := h.runNoBrowser(args...)
				if res.Exit == 0 || res.Stdout != "" || calls.Load() != 0 {
					t.Fatalf("unsafe read reached provider: calls=%d exit=%d stdout=%s stderr=%s", calls.Load(), res.Exit, res.Stdout, res.Stderr)
				}
				if strings.Contains(res.Stderr, h.root) || strings.Contains(res.Stderr, "access_token") {
					t.Fatal("credential path/content leaked")
				}
			}
		})
	}
}
