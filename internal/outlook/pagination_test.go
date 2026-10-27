package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestGraphCollectionsBoundPaginationAndDiscardPartialRows(t *testing.T) {
	for _, collection := range []string{"attachments", "folders", "filter", "search", "conversation", "rules"} {
		for _, kind := range []string{"self_cycle", "two_page_cycle", "empty_budget", "malformed", "outside_origin", "cancelled", "valid"} {
			t.Run(collection+"/"+kind, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int32
				const base = "https://graph.example.test/v1.0"
				c := dispatchTestClient(base, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					n := int(calls.Add(1))
					page, _ := strconv.Atoi(req.URL.Query().Get("page"))
					next := req.URL.String()
					rows := []any{map[string]any{"id": fmt.Sprint(n), "name": "metadata-only", "subject": "metadata-only", "displayName": "metadata-only", "conditions": map[string]any{}, "actions": map[string]any{}}}
					switch kind {
					case "self_cycle": // Equivalent query ordering must also be detected.
						q := req.URL.Query()
						next = base + req.URL.Path[len("/v1.0"):] + "?" + q.Encode()
					case "two_page_cycle":
						next = base + "/me/messages?page=" + fmt.Sprint((page+1)%2)
					case "empty_budget":
						rows = []any{}
						next = base + "/me/messages?page=" + fmt.Sprint(n)
					case "malformed":
						next = base + "/%zz?PRIVATE_CURSOR"
					case "outside_origin":
						next = "https://outside.invalid/v1.0/me/messages?PRIVATE_CURSOR"
					case "cancelled":
						cancel()
						next = base + "/me/messages?page=1"
					case "valid":
						if n == 1 {
							next = base + "/me/messages?%24skiptoken=" + url.QueryEscape("opaque /+==& cursor")
						} else {
							next = ""
							if req.URL.Query().Get("$skiptoken") != "opaque /+==& cursor" {
								t.Fatal("opaque cursor changed")
							}
						}
					}
					data, _ := json.Marshal(map[string]any{"value": rows, "@odata.nextLink": next})
					return testRoundTripResponse(req, 200, string(data)), nil
				})})
				c.searchResultCap = 10000
				defer c.Destroy()
				var count int
				var err error
				switch collection {
				case "attachments":
					var v []mail.Attachment
					v, err = c.fetchAttachments(ctx, "msg", nil)
					count = len(v)
				case "folders":
					var v []graphFolder
					v, err = c.fetchFolderPages(ctx, base+"/me/mailFolders")
					count = len(v)
				case "filter":
					var v []graphMessage
					v, err = c.drainFilter(ctx, base+"/me/messages", limits.OutlookMaxPages+10, nil)
					count = len(v)
				case "search":
					var v []graphMessage
					v, _, err = c.drainSearch(ctx, base+"/me/messages")
					count = len(v)
				case "conversation":
					var v []graphMessage
					v, err = c.fetchConversationMessages(ctx, "thread")
					count = len(v)
				case "rules":
					var v mail.RuleListResult
					v, err = c.ListRules(ctx, mail.RuleListRequest{})
					count = len(v.Rules)
				}
				if kind == "valid" {
					if err != nil || count != 2 || calls.Load() != 2 {
						t.Fatalf("genuine pages rows=%d calls=%d err=%v", count, calls.Load(), err)
					}
					return
				}
				if err == nil || count != 0 {
					t.Fatalf("partial collection escaped: rows=%d calls=%d err=%v", count, calls.Load(), err)
				}
				max := int32(3)
				if kind == "empty_budget" {
					max = limits.OutlookMaxPages
					if calls.Load() != max {
						t.Fatalf("budget stopped at %d not %d", calls.Load(), max)
					}
					var product *errs.Error
					if !errors.As(err, &product) || product.Code() != errs.CodeReadLimitExceeded {
						t.Fatalf("wrong budget error: %v", err)
					}
				}
				if calls.Load() > max {
					t.Fatalf("unbounded traversal: %d", calls.Load())
				}
			})
		}
	}
}
