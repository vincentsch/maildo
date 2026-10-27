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

	"github.com/vincentsch/maildo/internal/provider"
)

type graphReadRoundTripFunc func(*http.Request) (*http.Response, error)

func (f graphReadRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// observedReadCloser lets tests assert both response ownership and whether a
// status branch consumed a body that should have remained unread.
type observedReadCloser struct {
	body      io.Reader
	readCalls atomic.Int64
	closed    atomic.Bool
}

func (b *observedReadCloser) Read(p []byte) (int, error) {
	b.readCalls.Add(1)
	return b.body.Read(p)
}

func (b *observedReadCloser) Close() error {
	b.closed.Store(true)
	return nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestGraphReadRequestClassifiesBeforeReadingAndClosesEveryResponse(t *testing.T) {
	surfaces := []struct {
		name    string
		surface graphReadSurface
	}{
		{"message", graphReadMessage},
		{"draft", graphReadDraft},
		{"rule", graphReadRule},
		{"archive", graphReadArchivePolicy},
	}
	statuses := []struct {
		status       int
		wantErr      bool
		bodyReadable bool
		ruleReason   provider.RuleReadFailureReason
	}{
		{http.StatusOK, false, true, ""},
		{http.StatusUnauthorized, true, false, provider.RuleReadFailureAuthRepairRequired},
		{http.StatusForbidden, true, false, provider.RuleReadFailureAuthRepairRequired},
		{http.StatusNotFound, false, false, ""},
		{http.StatusGone, false, false, ""},
		{http.StatusBadRequest, false, false, ""},
		{http.StatusTooManyRequests, true, false, provider.RuleReadFailureRateLimited},
		{http.StatusFound, true, false, provider.RuleReadFailureProviderAPI},
		{http.StatusSwitchingProtocols, true, false, provider.RuleReadFailureProviderAPI},
		{http.StatusCreated, true, false, provider.RuleReadFailureProviderAPI},
		{http.StatusInternalServerError, true, false, provider.RuleReadFailureProviderAPI},
	}
	for _, surface := range surfaces {
		for _, status := range statuses {
			t.Run(surface.name+"/"+http.StatusText(status.status), func(t *testing.T) {
				body := &observedReadCloser{body: strings.NewReader("ok")}
				client := &Client{
					graphBaseURL: "https://graph.example.test/v1.0", account: "work",
					tokenType: "Bearer", accessToken: secretFromString("secret"),
					http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						if req.Header.Get("Authorization") != "Bearer secret" || req.Method != http.MethodGet {
							t.Fatalf("request = %#v", req)
						}
						return &http.Response{StatusCode: status.status, Body: body, Header: make(http.Header), Request: req}, nil
					})},
				}
				gotStatus, gotBody, err := client.graphReadRequest(context.Background(), surface.surface, client.graphBaseURL+"/me/messages/id", "prefer-value")
				if gotStatus != status.status || (err != nil) != status.wantErr {
					t.Fatalf("status/body/error = %d %q %v", gotStatus, gotBody, err)
				}
				if status.bodyReadable && string(gotBody) != "ok" {
					t.Fatalf("body = %q", gotBody)
				}
				if !status.bodyReadable && (len(gotBody) != 0 || body.readCalls.Load() != 0) {
					t.Fatalf("non-200 body was consumed: body=%q reads=%d", gotBody, body.readCalls.Load())
				}
				if !body.closed.Load() {
					t.Fatal("response body was not closed")
				}
				if surface.surface == graphReadRule && err != nil {
					var typed *provider.RuleReadFailureError
					if !errors.As(err, &typed) || typed.Reason() != status.ruleReason {
						t.Fatalf("rule error = %T %v", err, err)
					}
				}
			})
		}
	}
}

func TestGraphReadRequestSurfaceWordingReadFailuresAndSizeLimits(t *testing.T) {
	for _, tt := range []struct {
		name       string
		surface    graphReadSurface
		broad      string
		size       string
		readReason provider.RuleReadFailureReason
		sizeReason provider.RuleReadFailureReason
	}{
		{"message", graphReadMessage, `could not search or list messages for account "work" (outlook)`, "message body exceeds the read size limit", "", ""},
		{"draft", graphReadDraft, `could not search or list messages for account "work" (outlook)`, "message body exceeds the read size limit", "", ""},
		{"rule", graphReadRule, `could not read rules for account "work" (outlook)`, "Outlook rule response exceeds the read size limit", provider.RuleReadFailureTransport, provider.RuleReadFailureOversizedResponse},
		{"archive", graphReadArchivePolicy, `could not read archive policy for account "work" (outlook)`, "Outlook archive-policy response exceeds the read size limit", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &observedReadCloser{body: failingReader{}}
			client := graphReadTestClient(body)
			_, _, err := client.graphReadRequest(context.Background(), tt.surface, client.graphBaseURL+"/me/messages/id", "")
			if err == nil || err.Error() != tt.broad || !body.closed.Load() {
				t.Fatalf("read failure = %v closed=%t", err, body.closed.Load())
			}
			assertRuleReadReason(t, err, tt.readReason)

			body = &observedReadCloser{body: strings.NewReader(strings.Repeat("x", maxBodyBytes+1))}
			client = graphReadTestClient(body)
			_, _, err = client.graphReadRequest(context.Background(), tt.surface, client.graphBaseURL+"/me/messages/id", "")
			if err == nil || err.Error() != tt.size || !body.closed.Load() {
				t.Fatalf("oversized failure = %v closed=%t", err, body.closed.Load())
			}
			assertRuleReadReason(t, err, tt.sizeReason)
		})
	}
}

func TestGraphReadRequestExactSizeBoundaryAcrossSurfaces(t *testing.T) {
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		for _, size := range []int{0, maxBodyBytes - 1, maxBodyBytes, maxBodyBytes + 1} {
			t.Run(fmt.Sprintf("surface-%d/size-%d", surface, size), func(t *testing.T) {
				body := &observedReadCloser{body: strings.NewReader(strings.Repeat("x", size))}
				client := graphReadTestClient(body)
				status, got, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/me/messages/id", "")
				if size <= maxBodyBytes {
					if err != nil || status != http.StatusOK || len(got) != size {
						t.Fatalf("status=%d len=%d err=%v", status, len(got), err)
					}
				} else if err == nil || len(got) != 0 {
					t.Fatalf("oversized status=%d len=%d err=%v", status, len(got), err)
				}
				if !body.closed.Load() {
					t.Fatal("response body was not closed")
				}
			})
		}

		t.Run(fmt.Sprintf("surface-%d/non-200-oversized", surface), func(t *testing.T) {
			body := &observedReadCloser{body: strings.NewReader(strings.Repeat("x", maxBodyBytes+1))}
			client := &Client{
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				tokenType: "Bearer", accessToken: secretFromString("secret"),
				http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusNotFound, Body: body, Header: make(http.Header), Request: req}, nil
				})},
			}
			status, got, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/me/messages/id", "")
			if err != nil || status != http.StatusNotFound || len(got) != 0 || body.readCalls.Load() != 0 || !body.closed.Load() {
				t.Fatalf("status=%d len=%d err=%v reads=%d closed=%t", status, len(got), err, body.readCalls.Load(), body.closed.Load())
			}
		})
	}
}

func TestGraphReadSurfaceErrorVocabularyIsExact(t *testing.T) {
	client := &Client{account: "work"}
	for _, tt := range []struct {
		name    string
		surface graphReadSurface
		broad   string
		prepare string
		rate    string
		size    string
	}{
		{name: "message", surface: graphReadMessage, broad: `could not search or list messages for account "work" (outlook)`, prepare: "could not prepare an Outlook message request", rate: "Outlook message request was rate limited", size: "message body exceeds the read size limit"},
		{name: "draft", surface: graphReadDraft, broad: `could not search or list messages for account "work" (outlook)`, prepare: "could not prepare an Outlook message request", rate: "Outlook message request was rate limited", size: "message body exceeds the read size limit"},
		{name: "rule", surface: graphReadRule, broad: `could not read rules for account "work" (outlook)`, prepare: "could not prepare an Outlook rule request", rate: "Outlook rule request was rate limited", size: "Outlook rule response exceeds the read size limit"},
		{name: "archive", surface: graphReadArchivePolicy, broad: `could not read archive policy for account "work" (outlook)`, prepare: "could not prepare an Outlook archive-policy request", rate: "Outlook archive-policy request was rate limited", size: "Outlook archive-policy response exceeds the read size limit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := client.graphReadErrors(tt.surface)
			if !ok || got.broad.Error() != tt.broad || got.prepare.Error() != tt.prepare || got.rate.Error() != tt.rate || got.size.Error() != tt.size {
				t.Fatalf("surface vocabulary = %#v, ok=%t", got, ok)
			}
		})
	}
	if _, ok := client.graphReadErrors(graphReadSurface(255)); ok {
		t.Fatal("invalid surface has an error vocabulary")
	}
}

func graphReadTestClient(body io.ReadCloser) *Client {
	return &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: req}, nil
		})},
	}
}

func assertRuleReadReason(t *testing.T, err error, want provider.RuleReadFailureReason) {
	t.Helper()
	var typed *provider.RuleReadFailureError
	if want == "" {
		if errors.As(err, &typed) {
			t.Fatalf("unexpected typed rule failure %s", typed.Reason())
		}
		return
	}
	if !errors.As(err, &typed) || typed.Reason() != want {
		t.Fatalf("rule failure = %T %v, want %s", err, err, want)
	}
}

func TestGraphReadRequestRejectsInvalidSurfaceAndTargetBeforeDispatch(t *testing.T) {
	var calls atomic.Int64
	client := &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work", tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("must not dispatch")
		})},
	}
	if _, _, err := client.graphReadRequest(context.Background(), graphReadSurface(255), client.graphBaseURL+"/me", ""); err == nil || err.Error() != "invalid Outlook Graph read surface" {
		t.Fatalf("invalid surface error = %v", err)
	}
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		_, _, err := client.graphReadRequest(context.Background(), surface, "https://hostile.example.test/me", "")
		vocabulary, _ := client.graphReadErrors(surface)
		if err == nil || err.Error() != vocabulary.broad.Error() {
			t.Fatalf("surface %d invalid target error = %v", surface, err)
		}
		var typed *provider.RuleReadFailureError
		if surface == graphReadRule && (!errors.As(err, &typed) || typed.Reason() != provider.RuleReadFailureProviderAPI) {
			t.Fatalf("invalid rule target error = %T %v", err, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid reads dispatched %d requests", calls.Load())
	}
}

func TestGraphReadRequestRejectsOffOriginTargetBeforeAuthorizationExposure(t *testing.T) {
	var calls, authorized atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer hostile.Close()
	client := &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "" {
				authorized.Add(1)
			}
			return http.DefaultTransport.RoundTrip(req)
		})},
	}
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		if _, _, err := client.graphReadRequest(context.Background(), surface, hostile.URL+"/stolen", ""); err == nil {
			t.Fatalf("surface %d accepted off-origin target", surface)
		}
	}
	if calls.Load() != 0 || authorized.Load() != 0 {
		t.Fatalf("hostile target calls=%d authorized=%d", calls.Load(), authorized.Load())
	}
}

func TestGraphReadRequestDirectTargetPinningMatrix(t *testing.T) {
	invalidTargets := []string{
		"http://graph.example.test/v1.0/me",
		"https://graph.example.test:444/v1.0/me",
		"https://sub.graph.example.test/v1.0/me",
		"https://unrelated.example.test/v1.0/me",
		"https://user@graph.example.test/v1.0/me",
		"https://graph.example.test/v1.0/me#fragment",
		"/v1.0/me",
		"https://graph.example.test/%zz",
		"https://graph.example.test/beta/me",
		"https://graph.example.test/v1.0-other/me",
		"https://graph.example.test/v1.0/../me",
		"https://graph.example.test/v1.0/%2e%2e/me",
	}
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		for _, target := range invalidTargets {
			t.Run(fmt.Sprintf("surface-%d/%s", surface, url.PathEscape(target)), func(t *testing.T) {
				var calls atomic.Int64
				client := &Client{
					graphBaseURL: "https://graph.example.test/v1.0", account: "work",
					tokenType: "Bearer", accessToken: secretFromString("secret"),
					http: &http.Client{Transport: graphReadRoundTripFunc(func(*http.Request) (*http.Response, error) {
						calls.Add(1)
						return nil, errors.New("must not dispatch")
					})},
				}
				if _, _, err := client.graphReadRequest(context.Background(), surface, target, ""); err == nil || calls.Load() != 0 {
					t.Fatalf("target=%q err=%v calls=%d", target, err, calls.Load())
				}
			})
		}
	}

	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		var calls atomic.Int64
		body := &observedReadCloser{body: strings.NewReader("ignored")}
		client := &Client{
			graphBaseURL: "https://graph.example.test/v1.0", account: "work",
			tokenType: "Bearer", accessToken: secretFromString("secret"),
			http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusNotFound, Body: body, Header: make(http.Header), Request: req}, nil
			})},
		}
		status, _, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/me", "")
		if err != nil || status != http.StatusNotFound || calls.Load() != 1 || !body.closed.Load() {
			t.Fatalf("surface=%d status=%d err=%v calls=%d closed=%t", surface, status, err, calls.Load(), body.closed.Load())
		}
	}

	encodedFinalDot := "https://graph.example.test/v1.0/me/mailFolders/%2E"
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		var calls atomic.Int64
		client := &Client{
			graphBaseURL: "https://graph.example.test/v1.0", account: "work",
			tokenType: "Bearer", accessToken: secretFromString("secret"),
			http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
			})},
		}
		_, _, err := client.graphReadRequest(context.Background(), surface, encodedFinalDot, "")
		allowed := surface == graphReadRule || surface == graphReadArchivePolicy
		if allowed && (err != nil || calls.Load() != 1) {
			t.Fatalf("surface=%d rejected local encoded final dot: err=%v calls=%d", surface, err, calls.Load())
		}
		if !allowed && (err == nil || calls.Load() != 0) {
			t.Fatalf("surface=%d accepted encoded final dot: err=%v calls=%d", surface, err, calls.Load())
		}
	}
}

type concurrentGraphReadTransport struct {
	total    int64
	arrived  atomic.Int64
	released chan struct{}
	once     sync.Once
	closed   atomic.Int64
}

func (t *concurrentGraphReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.arrived.Add(1) == t.total {
		t.once.Do(func() { close(t.released) })
	}
	<-t.released
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://unrelated.example.test/stolen"}},
		Body:       &countedGraphReadBody{Reader: strings.NewReader("redirect-body"), closed: &t.closed},
		Request:    req,
	}, nil
}

type countedGraphReadBody struct {
	io.Reader
	closed *atomic.Int64
}

func (b *countedGraphReadBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestGraphReadConcurrentCallsPreserveSharedClient(t *testing.T) {
	const workers = 64
	transport := &concurrentGraphReadTransport{total: workers, released: make(chan struct{})}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var inheritedRedirectCalls atomic.Int64
	inherited := func(*http.Request, []*http.Request) error {
		inheritedRedirectCalls.Add(1)
		return nil
	}
	sharedHTTP := &http.Client{Transport: transport, Timeout: 3 * time.Second, Jar: jar, CheckRedirect: inherited}
	client := &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"), http: sharedHTTP,
	}

	results := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(surface graphReadSurface) {
			defer wait.Done()
			status, body, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/me", "")
			if err == nil || status != http.StatusFound || len(body) != 0 {
				results <- fmt.Errorf("surface=%d status=%d body=%q err=%v", surface, status, body, err)
				return
			}
			results <- nil
		}(graphReadSurface(i % 4))
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if transport.arrived.Load() != workers || transport.closed.Load() != workers || inheritedRedirectCalls.Load() != 0 {
		t.Fatalf("arrived=%d closed=%d inherited_redirects=%d", transport.arrived.Load(), transport.closed.Load(), inheritedRedirectCalls.Load())
	}
	if sharedHTTP.Transport != transport || sharedHTTP.Jar != jar || sharedHTTP.Timeout != 3*time.Second || sharedHTTP.CheckRedirect == nil {
		t.Fatal("shared HTTP client fields changed")
	}
	if err := sharedHTTP.CheckRedirect(&http.Request{}, nil); err != nil || inheritedRedirectCalls.Load() != 1 {
		t.Fatalf("inherited redirect callback changed: err=%v calls=%d", err, inheritedRedirectCalls.Load())
	}
}

func TestGraphReadRequestDispatchFailureUsesSurfaceTransportClassification(t *testing.T) {
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		client := &Client{
			graphBaseURL: "https://graph.example.test/v1.0", account: "work", tokenType: "Bearer", accessToken: secretFromString("secret"),
			http: &http.Client{Transport: graphReadRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("synthetic dispatch failure")
			})},
		}
		_, body, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/me/messages/id", "")
		if err == nil || len(body) != 0 || strings.Contains(err.Error(), "synthetic") {
			t.Fatalf("surface %d dispatch error = %v body=%q", surface, err, body)
		}
		if surface == graphReadRule {
			var typed *provider.RuleReadFailureError
			if !errors.As(err, &typed) || typed.Reason() != provider.RuleReadFailureTransport {
				t.Fatalf("rule dispatch error = %T %v", err, err)
			}
		}
	}
}

func TestGraphReadRequestResponseAssociatedDispatchErrorClosesBody(t *testing.T) {
	for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
		body := &observedReadCloser{body: strings.NewReader("provider-body-must-not-be-read")}
		client := &Client{
			graphBaseURL: "https://graph.example.test/v1.0", account: "work", tokenType: "Bearer", accessToken: secretFromString("secret"),
			http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusFound,
					Body:       body,
					Header:     http.Header{"Location": []string{"http://[::1"}},
					Request:    req,
				}, nil
			})},
		}
		_, responseBody, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/start", "")
		if err == nil || len(responseBody) != 0 || strings.Contains(err.Error(), "provider-body") {
			t.Fatalf("surface %d dispatch error=%v body=%q", surface, err, responseBody)
		}
		if !body.closed.Load() || body.readCalls.Load() != 0 {
			t.Fatalf("surface %d response body closed=%t reads=%d", surface, body.closed.Load(), body.readCalls.Load())
		}
	}
}

func TestGraphReadRequestNeverFollowsRedirectsOrForwardsAuthorization(t *testing.T) {
	var alternateCalls, alternateAuth atomic.Int64
	alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alternateCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			alternateAuth.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer alternate.Close()

	var sameOriginDestinationCalls, sameOriginAuth atomic.Int64
	var location atomic.Value
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			sameOriginDestinationCalls.Add(1)
			if r.Header.Get("Authorization") != "" {
				sameOriginAuth.Add(1)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, location.Load().(string), http.StatusFound)
	}))
	defer primary.Close()

	client := &Client{graphBaseURL: primary.URL, http: primary.Client(), account: "work", tokenType: "Bearer", accessToken: secretFromString("secret")}
	for _, redirect := range []string{
		primary.URL + "/destination",
		alternate.URL + "/destination",
		"https://subdomain.example.test/destination",
		"https://unrelated.example.invalid/destination",
	} {
		location.Store(redirect)
		for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
			if _, _, err := client.graphReadRequest(context.Background(), surface, primary.URL+"/start", ""); err == nil {
				t.Fatalf("surface %d followed or accepted redirect %q", surface, redirect)
			}
		}
	}
	if sameOriginDestinationCalls.Load() != 0 || sameOriginAuth.Load() != 0 || alternateCalls.Load() != 0 || alternateAuth.Load() != 0 {
		t.Fatalf("redirect destinations reached: same=%d/%d alternate=%d/%d", sameOriginDestinationCalls.Load(), sameOriginAuth.Load(), alternateCalls.Load(), alternateAuth.Load())
	}
}

func TestGraphReadRequestRejectsEveryRedirectClassBeforeDestinationDispatch(t *testing.T) {
	for _, location := range []string{
		"https://graph.example.test/v1.0/destination",
		"https://graph.example.test:444/v1.0/destination",
		"https://sub.graph.example.test/v1.0/destination",
		"https://unrelated.example.test/destination",
	} {
		for _, surface := range []graphReadSurface{graphReadMessage, graphReadDraft, graphReadRule, graphReadArchivePolicy} {
			var roundTrips, destinationAuth atomic.Int64
			body := &observedReadCloser{body: strings.NewReader("redirect-body")}
			client := &Client{
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				tokenType: "Bearer", accessToken: secretFromString("secret"),
				http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if roundTrips.Add(1) > 1 && req.Header.Get("Authorization") != "" {
						destinationAuth.Add(1)
					}
					return &http.Response{
						StatusCode: http.StatusFound,
						Body:       body,
						Header:     http.Header{"Location": []string{location}},
						Request:    req,
					}, nil
				})},
			}
			if _, _, err := client.graphReadRequest(context.Background(), surface, client.graphBaseURL+"/start", ""); err == nil {
				t.Fatalf("surface %d accepted redirect to %q", surface, location)
			}
			if roundTrips.Load() != 1 || destinationAuth.Load() != 0 || !body.closed.Load() || body.readCalls.Load() != 0 {
				t.Fatalf("surface %d redirect %q roundTrips=%d auth=%d closed=%t reads=%d", surface, location, roundTrips.Load(), destinationAuth.Load(), body.closed.Load(), body.readCalls.Load())
			}
		}
	}
}
