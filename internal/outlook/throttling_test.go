package outlook

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentsch/rungrad"
)

// mailboxConcurrencyGraph answers like Graph did for a Microsoft 365 mailbox
// on 2026-10-29: a fifth request that runs while four others are open gets
// 429 ApplicationThrottled with Retry-After.
type mailboxConcurrencyGraph struct {
	throttle string
	mu       sync.Mutex
	open     int
	most     int
}

func (g *mailboxConcurrencyGraph) serve(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.open++
	if g.open > g.most {
		g.most = g.open
	}
	over := g.open > 4
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.open--; g.mu.Unlock() }()
	// Hold the request a moment so that requests sent at the same time
	// overlap, as they do over a real network.
	time.Sleep(30 * time.Millisecond)
	if over {
		resp := testRoundTripResponse(req, http.StatusTooManyRequests, g.throttle)
		resp.Header.Set("Retry-After", "6")
		return resp, nil
	}
	name := strings.TrimPrefix(req.URL.Path, "/v1.0/me/mailFolders/")
	return testRoundTripResponse(req, http.StatusOK, `{"id":"`+name+`-id","displayName":"`+name+`"}`), nil
}

func TestStandardFolderLookupsStayUnderGraphsMailboxLimit(t *testing.T) {
	body, err := os.ReadFile("testdata/graph-429-mailbox-concurrency.json")
	if err != nil {
		t.Fatal(err)
	}
	g := &mailboxConcurrencyGraph{throttle: string(body)}
	c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(g.serve)})
	rows, err := c.wellKnownFolderRows(context.Background())
	if err != nil {
		t.Fatalf("standard folders: %v", err)
	}
	if len(rows) != len(graphWellKnownNames) {
		t.Fatalf("got %d standard folders, want %d", len(rows), len(graphWellKnownNames))
	}
	if g.most > 4 {
		t.Fatalf("%d requests ran at once; Graph throttles more than 4", g.most)
	}
}

func TestThrottledFolderReadSaysToSlowDown(t *testing.T) {
	body, err := os.ReadFile("testdata/graph-429-mailbox-concurrency.json")
	if err != nil {
		t.Fatal(err)
	}
	c := dispatchTestClient("https://graph.example.test/v1.0", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := testRoundTripResponse(req, http.StatusTooManyRequests, string(body))
		resp.Header.Set("Retry-After", "6")
		return resp, nil
	})})
	var f graphFolder
	err = c.graphGet(context.Background(), "https://graph.example.test/v1.0/me/mailFolders/inbox", &f)
	if exitCode(err) != rungrad.ExitRateLimited || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("throttled read: exit %d, %v", exitCode(err), err)
	}
}
