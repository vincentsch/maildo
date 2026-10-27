package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/audit"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestOutlookFolderTreeCLIHasTypedErrorsAndNoPartialOutput(t *testing.T) {
	for _, kind := range []string{"self_cycle", "two_folder_cycle", "missing_identity", "folder_budget"} {
		for _, flags := range [][]string{{}, {"--json"}, {"--plain"}, {"--jq", "."}, {"--template", "{{.}}"}, {"--json", "--include-meta"}} {
			t.Run(kind+"/"+strings.Join(flags, "_"), func(t *testing.T) {
				h := seededOutlookFoldersHarness(t)
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := int(calls.Add(1))
					// Bound the fixture independently if traversal protection regresses.
					if n > limits.OutlookMaxFolders/100+2 {
						w.WriteHeader(500)
						return
					}
					rows := []any{map[string]any{"id": "first", "displayName": "PRIVATE FOLDER METADATA", "childFolderCount": 1}}
					page := map[string]any{"value": rows}
					switch kind {
					case "two_folder_cycle":
						if n == 2 {
							rows[0].(map[string]any)["id"] = "second"
						}
					case "missing_identity":
						rows[0].(map[string]any)["id"] = ""
					case "folder_budget":
						rows = nil
						start := (n - 1) * 100
						end := start + 100
						if end > limits.OutlookMaxFolders {
							end = limits.OutlookMaxFolders + 1
						}
						for i := start; i < end; i++ {
							rows = append(rows, map[string]any{"id": fmt.Sprintf("leaf-%d", i), "displayName": "PRIVATE FOLDER METADATA"})
						}
						page["value"] = rows
						if end <= limits.OutlookMaxFolders {
							page["@odata.nextLink"] = "http://" + r.Host + "/v1.0/me/mailFolders?page=" + fmt.Sprint(n+1)
						}
					}
					json.NewEncoder(w).Encode(page)
				}))
				defer server.Close()
				h.providers = testkit.NewOutlookRegistry(outlook.Options{
					GraphBaseURL: server.URL + "/v1.0", TokenURL: server.URL + "/token", AuthorizeURL: server.URL + "/authorize", DeviceCodeURL: server.URL + "/devicecode",
					HTTPClient: server.Client(), Now: func() time.Time { return h.now },
				})
				args := append([]string{"folders", "list", "--account", "work"}, flags...)
				res := h.runNoBrowser(args...)
				if res.Exit != 1 || res.Stdout != "" {
					t.Fatalf("tree failure: exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
				}
				if strings.Contains(res.Stderr, "PRIVATE FOLDER METADATA") || strings.Contains(res.Stderr, server.URL) {
					t.Fatal("partial folder data/target leaked")
				}
				wantCalls, wantCode := 2, errs.CodeProviderDataError
				switch kind {
				case "two_folder_cycle":
					wantCalls = 3
				case "missing_identity":
					wantCalls = 1
				case "folder_budget":
					wantCalls = limits.OutlookMaxFolders/100 + 1
					wantCode = errs.CodeReadLimitExceeded
				}
				if int(calls.Load()) != wantCalls {
					t.Fatalf("requests=%d want=%d", calls.Load(), wantCalls)
				}
				if len(flags) > 0 && flags[0] != "--plain" {
					var machine map[string]any
					if err := json.Unmarshal([]byte(res.Stderr), &machine); err != nil {
						t.Fatal(err)
					}
					if machine["code"] != wantCode || machine["provider"] != "outlook" || machine["account"] != "work" {
						t.Fatalf("wrong typed/account error: %v", machine)
					}
					if kind == "folder_budget" && (machine["dimension"] != "folders" || machine["limit"] != float64(limits.OutlookMaxFolders)) {
						t.Fatalf("wrong CLI budget: %v", machine)
					}
				} else if !strings.Contains(res.Stderr, "Outlook sent ") && !strings.Contains(res.Stderr, "more than maildo reads in one command") {
					t.Fatalf("missing human error: %s", res.Stderr)
				}
				if _, err := os.Stat(filepath.Join(h.root, audit.RelativePath)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("folder read failure recorded an action: %v", err)
				}
			})
		}
	}
}

func TestOutlookFolderTreeCLIPageBudgetDiscardsEarlierRows(t *testing.T) {
	h := seededOutlookFoldersHarness(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n > limits.OutlookMaxPages {
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": fmt.Sprintf("folder-%d", n), "displayName": "PRIVATE FOLDER METADATA", "childFolderCount": 1}}})
	}))
	defer server.Close()
	h.providers = testkit.NewOutlookRegistry(outlook.Options{
		GraphBaseURL: server.URL, TokenURL: server.URL + "/token", AuthorizeURL: server.URL + "/authorize", DeviceCodeURL: server.URL + "/devicecode",
		HTTPClient: server.Client(), Now: func() time.Time { return h.now },
	})
	res := h.runNoBrowser("folders", "list", "--account", "work", "--json")
	assertErrorCodeExit(t, res, errs.CodeReadLimitExceeded, 1)
	if res.Stdout != "" || calls.Load() != limits.OutlookMaxPages || strings.Contains(res.Stderr, "PRIVATE FOLDER METADATA") {
		t.Fatalf("whole-walk limit: calls=%d stdout=%s stderr=%s", calls.Load(), res.Stdout, res.Stderr)
	}
	var machine map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &machine); err != nil {
		t.Fatal(err)
	}
	if machine["dimension"] != "pages" || machine["limit"] != float64(limits.OutlookMaxPages) || machine["observed_at_least"] != float64(limits.OutlookMaxPages+1) {
		t.Fatalf("page limit facts: %v", machine)
	}
}
