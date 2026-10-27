package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
)

func TestFolderTreeRejectsCyclesAndMalformedIdentities(t *testing.T) {
	for _, kind := range []string{"self_cycle", "two_folder_cycle", "repeated_leaf", "duplicate_root", "duplicate_across_pages", "empty_leaf_id", "empty_parent_id", "negative_child_count"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				// A regression may not run forever. Acceptance still requires the
				// typed error at the exact request count, never this safety response.
				if n > 8 {
					w.WriteHeader(500)
					return
				}
				row := graphFolder{ID: "first", DisplayName: "PRIVATE FOLDER METADATA", ChildFolderCount: 1}
				page := graphFolderPage{Value: []graphFolder{row}}
				switch kind {
				case "two_folder_cycle":
					if n == 2 {
						page.Value[0].ID = "second"
					}
				case "repeated_leaf":
					if n == 2 {
						page.Value[0].ChildFolderCount = 0
					}
				case "duplicate_root":
					page.Value = append(page.Value, row)
				case "duplicate_across_pages":
					if n == 1 {
						page.NextLink = "http://" + r.Host + r.URL.Path + "?page=2"
					}
				case "empty_leaf_id":
					page.Value[0].ID = ""
					page.Value[0].ChildFolderCount = 0
				case "empty_parent_id":
					page.Value[0].ID = ""
				case "negative_child_count":
					page.Value[0].ChildFolderCount = -1
				}
				json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			c := dispatchTestClient(server.URL+"/v1.0", server.Client())
			defer c.Destroy()
			rows, err := c.fetchFolderTree(context.Background())
			assertFolderTreeFailure(t, c, rows, err, errs.CodeProviderDataError)
			want := int32(1)
			switch kind {
			case "self_cycle", "repeated_leaf", "duplicate_across_pages":
				want = 2
			case "two_folder_cycle":
				want = 3
			}
			if calls.Load() != want {
				t.Fatalf("requests=%d want=%d (duplicate collection must not dispatch)", calls.Load(), want)
			}
		})
	}
}

func TestFolderTreeFinitePagedOpaqueControl(t *testing.T) {
	const base = "https://graph.example.test/v1.0"
	const parent = "parent /+==&?"
	const other = "PARENT /+==&?" // IDs are opaque and case-sensitive; names may repeat.
	const token = "opaque /+==&? cursor"
	var calls atomic.Int32
	c := dispatchTestClient(base, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		var page graphFolderPage
		switch req.URL.EscapedPath() {
		case "/v1.0/me/mailFolders":
			if req.URL.Query().Get("$skiptoken") == "" {
				page.Value = []graphFolder{{ID: parent, DisplayName: "Shared", ChildFolderCount: 2}, {ID: other, DisplayName: "Shared"}}
				page.NextLink = base + "/me/mailFolders?%24skiptoken=" + url.QueryEscape(token)
			} else {
				if req.URL.RawQuery != "%24skiptoken="+url.QueryEscape(token) {
					t.Error("opaque page cursor changed")
				}
				page.Value = []graphFolder{{ID: "root-leaf", DisplayName: "Leaf"}}
			}
		case "/v1.0/me/mailFolders/" + url.PathEscape(parent) + "/childFolders":
			page.Value = []graphFolder{{ID: "child", DisplayName: "Child", ParentFolderID: parent, ChildFolderCount: 1}, {ID: "child-leaf", DisplayName: "Leaf", ParentFolderID: parent}}
		case "/v1.0/me/mailFolders/child/childFolders":
			page.Value = []graphFolder{{ID: "grandchild", DisplayName: "Leaf", ParentFolderID: "child"}}
		default:
			t.Errorf("unexpected child identity/path: %s", req.URL.EscapedPath())
		}
		body, _ := json.Marshal(page)
		return testRoundTripResponse(req, 200, string(body)), nil
	})})
	defer c.Destroy()
	rows, err := c.fetchFolderTree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]graphFolder{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		byID[r.ID] = r
	}
	if want := []string{parent, other, "root-leaf", "child", "child-leaf", "grandchild"}; !reflect.DeepEqual(ids, want) || calls.Load() != 4 {
		t.Fatalf("finite traversal ids=%v calls=%d", ids, calls.Load())
	}
	paths, _ := deriveFolderPaths(rows, byID)
	if paths["grandchild"] != "Shared/Child/Leaf" {
		t.Fatalf("finite hierarchy path: %q", paths["grandchild"])
	}
}

func TestFolderTreeWholeWalkPageBudget(t *testing.T) {
	for _, shape := range []string{"deep", "wide", "empty_pages"} {
		t.Run(shape, func(t *testing.T) {
			var calls atomic.Int32
			c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				n := int(calls.Add(1))
				if n > limits.OutlookMaxPages {
					return testRoundTripResponse(req, 500, ""), nil
				}
				page := graphFolderPage{}
				if shape == "wide" && n == 1 {
					for i := 0; i < limits.OutlookMaxPages; i++ {
						page.Value = append(page.Value, graphFolder{ID: fmt.Sprintf("parent-%d", i), ChildFolderCount: 1})
					}
				} else if shape == "empty_pages" && n > 1 && req.URL.Query().Get("page") == "" {
					page.NextLink = "https://graph.example.test" + req.URL.EscapedPath() + "?page=2"
				} else {
					row := graphFolder{ID: fmt.Sprintf("folder-%d", n), ChildFolderCount: 1}
					if shape == "wide" {
						row.ChildFolderCount = 0
					}
					page.Value = []graphFolder{row}
				}
				body, _ := json.Marshal(page)
				return testRoundTripResponse(req, 200, string(body)), nil
			})})
			defer c.Destroy()
			rows, err := c.fetchFolderTree(context.Background())
			assertFolderTreeFailure(t, c, rows, err, errs.CodeReadLimitExceeded)
			assertFolderTreeLimit(t, err, errs.ReadLimitPages, limits.OutlookMaxPages)
			if calls.Load() != limits.OutlookMaxPages {
				t.Fatalf("whole-walk requests=%d want=%d", calls.Load(), limits.OutlookMaxPages)
			}
		})
	}
}

func TestFolderTreeWideDeepAndRowBoundaries(t *testing.T) {
	for _, shape := range []string{"wide", "deep_at_page_limit", "rows_at_limit", "rows_over_limit", "rows_over_collections"} {
		t.Run(shape, func(t *testing.T) {
			var calls atomic.Int32
			c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				n := int(calls.Add(1))
				page := graphFolderPage{}
				if n > limits.OutlookMaxPages {
					return testRoundTripResponse(req, 500, ""), nil
				}
				switch shape {
				case "wide":
					if n == 1 {
						for i := 0; i < 600; i++ {
							page.Value = append(page.Value, graphFolder{ID: fmt.Sprintf("parent-%d", i), DisplayName: "Shared", ChildFolderCount: 1})
						}
					} else {
						page.Value = []graphFolder{{ID: fmt.Sprintf("leaf-%d", n), DisplayName: "Shared"}}
					}
				case "deep_at_page_limit":
					row := graphFolder{ID: fmt.Sprintf("folder-%d", n), DisplayName: "Node", ParentFolderID: fmt.Sprintf("folder-%d", n-1), ChildFolderCount: 1}
					if n == limits.OutlookMaxPages {
						row.ChildFolderCount = 0
					}
					page.Value = []graphFolder{row}
				default:
					start := (n - 1) * folderPageSize
					end := start + folderPageSize
					if end > limits.OutlookMaxFolders {
						end = limits.OutlookMaxFolders + 1
					}
					for i := start; i < end; i++ {
						page.Value = append(page.Value, graphFolder{ID: fmt.Sprintf("leaf-%d", i), DisplayName: "Shared"})
					}
					if shape == "rows_over_collections" && n == 1 {
						page.Value[0].ChildFolderCount = 1
					}
					if end < limits.OutlookMaxFolders || (end == limits.OutlookMaxFolders && shape == "rows_over_limit") {
						page.NextLink = fmt.Sprintf("https://graph.example.test/v1.0/me/mailFolders?page=%d", n+1)
					}
				}
				body, _ := json.Marshal(page)
				return testRoundTripResponse(req, 200, string(body)), nil
			})})
			defer c.Destroy()
			rows, err := c.fetchFolderTree(context.Background())
			if shape == "rows_over_limit" || shape == "rows_over_collections" {
				assertFolderTreeFailure(t, c, rows, err, errs.CodeReadLimitExceeded)
				assertFolderTreeLimit(t, err, errs.ReadLimitFolders, limits.OutlookMaxFolders)
				if int(calls.Load()) != limits.OutlookMaxFolders/folderPageSize+1 {
					t.Fatalf("row budget requests=%d", calls.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantRows, wantCalls := limits.OutlookMaxFolders, limits.OutlookMaxFolders/folderPageSize
			if shape == "wide" {
				wantRows, wantCalls = 1200, 601
			}
			if shape == "deep_at_page_limit" {
				wantRows, wantCalls = limits.OutlookMaxPages, limits.OutlookMaxPages
			}
			if len(rows) != wantRows || int(calls.Load()) != wantCalls {
				t.Fatalf("finite %s rows=%d requests=%d", shape, len(rows), calls.Load())
			}
			if shape == "deep_at_page_limit" {
				byID := make(map[string]graphFolder, len(rows))
				for _, r := range rows {
					byID[r.ID] = r
				}
				paths, _ := deriveFolderPaths(rows, byID)
				if strings.Count(paths[rows[len(rows)-1].ID], "/") != limits.OutlookMaxPages-1 {
					t.Fatal("deep finite path truncated")
				}
			}
		})
	}
}

func TestFolderTreeRejectsPageTargetVisitedInAnotherCollection(t *testing.T) {
	var calls atomic.Int32
	const base = "https://graph.example.test/v1.0"
	c := dispatchTestClient(base, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		page := graphFolderPage{Value: []graphFolder{{ID: "parent", ChildFolderCount: 1}}}
		if n == 1 {
			page.NextLink = base + "/me/mailFolders/parent/childFolders?$top=100"
		} else {
			page.Value = []graphFolder{{ID: "leaf"}}
		}
		body, _ := json.Marshal(page)
		return testRoundTripResponse(req, 200, string(body)), nil
	})})
	defer c.Destroy()
	rows, err := c.fetchFolderTree(context.Background())
	assertFolderTreeFailure(t, c, rows, err, errs.CodeProviderDataError)
	if calls.Load() != 2 {
		t.Fatalf("duplicate collection target dispatched: %d", calls.Load())
	}
}

func assertFolderTreeFailure(t *testing.T, c *Client, rows []graphFolder, err error, code string) {
	t.Helper()
	var product *errs.Error
	if rows != nil || !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("rows=%d nil=%t error=%v want typed %s", len(rows), rows == nil, err, code)
	}
	d := product.Details()
	if d["account"] != c.account || d["provider"] != "outlook" {
		t.Fatalf("wrong error scope: %v", d)
	}
	if strings.Contains(err.Error(), "PRIVATE FOLDER") {
		t.Fatal("folder metadata leaked")
	}
}

func assertFolderTreeLimit(t *testing.T, err error, dimension errs.ReadLimitDimension, limit int) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatal(err)
	}
	d := product.Details()
	if d["dimension"] != string(dimension) || d["limit"] != uint64(limit) || d["observed_at_least"] != uint64(limit+1) {
		t.Fatalf("wrong traversal limit facts: %v", d)
	}
}
