package outlook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func dispatchTestClient(base string, httpClient *http.Client) *Client {
	return &Client{accessToken: secretFromString("SYNTHETIC DISPATCH TOKEN"), account: "work", email: "work@example.test", graphBaseURL: base, http: httpClient}
}

func dispatchTestSend(t *testing.T, ctx context.Context, c *Client) (mail.DirectSendOutcome, error) {
	t.Helper()
	p, err := (DirectSendPreparer{}).PrepareDirectSend(context.Background(), provider.ComposeIdentity{Provider: "outlook", Account: "work", Email: "work@example.test"}, mail.OutboundMessage{To: []mail.Recipient{{Address: "to@example.test"}}, Body: mail.OutboundBody{Present: true, Text: "SYNTHETIC BODY"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Destroy()
	out, err := c.SendMessage(ctx, mail.SendMessageRequest{Prepared: p})
	if err != nil {
		_, again := c.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: p})
		if again == nil {
			t.Fatal("consumed send carrier replayed")
		}
	}
	return out, err
}

func TestGraphDispatchRefusesRedirectsWithoutMutatingClient(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, targetKind := range []string{"port", "host", "scheme", "outside_api", "inside_api"} {
			for _, operation := range []string{"folder", "message", "compose", "download", "direct_send", "identity", "oauth"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, targetKind, operation), func(t *testing.T) {
					var destinations, callbacks, sources atomic.Int32
					var forwardedBytes atomic.Int64
					var authorized atomic.Bool
					capture := func(w http.ResponseWriter, r *http.Request) {
						destinations.Add(1)
						authorized.Store(r.Header.Get("Authorization") != "")
						b, _ := io.ReadAll(r.Body)
						forwardedBytes.Add(int64(len(b)))
						w.WriteHeader(202)
					}
					dst := httptest.NewServer(http.HandlerFunc(capture))
					defer dst.Close()
					var target string
					src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/outside" || r.URL.Path == "/v1.0/redirected" {
							capture(w, r)
							return
						}
						sources.Add(1)
						if operation != "oauth" && r.Header.Get("Authorization") == "" {
							t.Error("initial synthetic request lacks authorization")
						}
						w.Header().Set("Location", target)
						w.WriteHeader(status)
					}))
					defer src.Close()
					switch targetKind {
					case "port":
						target = dst.URL + "/outside"
					case "host":
						target = strings.Replace(dst.URL, "127.0.0.1", "localhost", 1) + "/outside"
					case "scheme":
						target = strings.Replace(dst.URL, "http:", "https:", 1) + "/outside"
					case "outside_api":
						target = src.URL + "/outside"
					case "inside_api":
						target = src.URL + "/v1.0/redirected"
					}
					jar, _ := cookiejar.New(nil)
					transport := http.DefaultTransport.(*http.Transport).Clone()
					defer transport.CloseIdleConnections()
					client := &http.Client{Transport: transport, Timeout: time.Second, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { callbacks.Add(1); return nil }}
					callbackPtr := reflect.ValueOf(client.CheckRedirect).Pointer()
					c := dispatchTestClient(src.URL+"/v1.0", client)
					defer c.Destroy()
					ctx := context.Background()
					var err error
					switch operation {
					case "folder":
						err = c.graphGet(ctx, c.graphBaseURL+"/me/mailFolders", &graphFolderPage{})
					case "message":
						err = c.graphMessageGet(ctx, c.graphBaseURL+"/me/messages", &graphMessagePage{})
					case "compose":
						var code int
						code, _, err = c.graphDoWithMessages(ctx, http.MethodPost, c.graphBaseURL+"/me/messages", map[string]string{"body": "SYNTHETIC BODY"}, "", c.messagesError, "prepare", "rate", "size")
						if err == nil && code >= 300 {
							err = errors.New("redirect rejected")
						}
					case "download":
						_, err = c.DownloadMessageAttachment(ctx, mail.MessageAttachmentDownloadRequest{MessageID: "msg", AttachmentID: "part"})
					case "direct_send":
						_, err = dispatchTestSend(t, ctx, c)
					case "identity":
						p := &Provider{opts: Options{GraphBaseURL: c.graphBaseURL, HTTPClient: client}}
						_, err = p.verifyAndBuildCache(ctx, nil, "synthetic-client", &tokenResponse{AccessToken: "SYNTHETIC TOKEN"})
					case "oauth":
						var resp *http.Response
						resp, err = formPost(ctx, client, src.URL+"/token", nil)
						if resp != nil {
							resp.Body.Close()
							if resp.StatusCode >= 300 {
								err = errors.New("redirect rejected")
							}
						}
					}
					if err == nil {
						t.Fatal("redirect claimed success")
					}
					if sources.Load() != 1 {
						t.Fatalf("initial requests=%d want=1", sources.Load())
					}
					if destinations.Load() != 0 || authorized.Load() || forwardedBytes.Load() != 0 {
						t.Fatalf("redirect dispatched: hits=%d authorization=%t bytes=%d", destinations.Load(), authorized.Load(), forwardedBytes.Load())
					}
					if callbacks.Load() != 0 || client.Transport != transport || client.Jar != jar || client.Timeout != time.Second || reflect.ValueOf(client.CheckRedirect).Pointer() != callbackPtr {
						t.Fatal("shared caller client changed")
					}
				})
			}
		}
	}
}

func TestOutlookDirectSendTransmissionOutcomes(t *testing.T) {
	for _, stage := range []string{"lost_response", "cancel_after_body", "cancel_after_headers", "cancel_at_connection", "cancel_before_headers", "cancel_before_dispatch", "dial_failure", "opaque_transport_error", "unreadable_202", "unclosable_202", "nonempty_202", "unexpected_200", "server_500", "request_timeout_408", "accepted_202", "rejected_403"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			var closed atomic.Int32
			var client *http.Client
			base := "http://synthetic.invalid/v1.0"
			if stage == "lost_response" || stage == "cancel_after_body" || stage == "cancel_after_headers" || stage == "cancel_at_connection" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					calls.Add(1)
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if stage == "cancel_after_body" {
						cancel()
					}
					conn.Close()
				}))
				defer srv.Close()
				base = srv.URL + "/v1.0"
				client = srv.Client()
				if stage == "cancel_after_headers" {
					ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: cancel})
				}
				if stage == "cancel_at_connection" {
					ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { cancel() }})
				}
			} else if stage == "dial_failure" || stage == "cancel_before_dispatch" || stage == "cancel_before_headers" {
				client = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
					calls.Add(1)
					if stage == "cancel_before_headers" {
						cancel()
						return nil, ctx.Err()
					}
					return nil, errors.New("SYNTHETIC PRIVATE TRANSPORT DETAIL")
				}}}
				if stage == "cancel_before_dispatch" {
					cancel()
				}
			} else {
				client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					io.Copy(io.Discard, req.Body)
					req.Body.Close()
					if stage == "opaque_transport_error" {
						return nil, errors.New("SYNTHETIC PRIVATE TRANSPORT DETAIL")
					}
					status, body := 202, ""
					switch stage {
					case "nonempty_202":
						body = "SYNTHETIC PRIVATE RESPONSE"
					case "unexpected_200":
						status = 200
					case "server_500":
						status = 500
					case "request_timeout_408":
						status = 408
					case "rejected_403":
						status = 403
					}
					resp := testRoundTripResponse(req, status, body)
					resp.Body = &sendAcceptanceBody{Reader: strings.NewReader(body), closes: &closed, readFails: stage == "unreadable_202", closeFails: stage == "unclosable_202"}
					return resp, nil
				})}
			}
			c := dispatchTestClient(base, client)
			defer c.Destroy()
			out, err := dispatchTestSend(t, ctx, c)
			if stage == "accepted_202" {
				if err != nil || out.WriteOutcomeFacts == nil || !out.Succeeded || !out.AcceptedByProvider {
					t.Fatalf("acceptance: %#v %v", out, err)
				}
				return
			}
			facts, ok := errs.WriteOutcomeFactsFromError(err)
			if !ok {
				t.Fatalf("missing write facts: %v", err)
			}
			if stage == "dial_failure" || stage == "cancel_before_dispatch" || stage == "cancel_before_headers" {
				if facts.Outcome != errs.WriteOutcomeNotApplied || facts.WriteMayHaveLanded || !facts.RetrySafe {
					t.Fatalf("predispatch facts: %+v", facts)
				}
			} else if stage == "rejected_403" {
				if facts.Outcome != errs.WriteOutcomeRejected || facts.WriteMayHaveLanded {
					t.Fatalf("rejection facts: %+v", facts)
				}
			} else if facts.Outcome != errs.WriteOutcomeUnknown || !facts.WriteMayHaveLanded || facts.RetrySafe || facts.AcceptedByProvider || facts.ConfirmedAfterWrite || facts.Succeeded {
				t.Fatalf("uncertain facts: %+v", facts)
			}
			if calls.Load() > 1 {
				t.Fatalf("submission replayed: %d", calls.Load())
			}
			if closed.Load() > 1 {
				t.Fatal("acceptance body closed twice")
			}
			if strings.Contains(fmt.Sprint(err), "SYNTHETIC PRIVATE") {
				t.Fatal("transport/response detail leaked")
			}
		})
	}
}

type sendAcceptanceBody struct {
	io.Reader
	closes                *atomic.Int32
	readFails, closeFails bool
}

func (b *sendAcceptanceBody) Read(p []byte) (int, error) {
	if b.readFails {
		return 0, errors.New("SYNTHETIC PRIVATE BODY ERROR")
	}
	return b.Reader.Read(p)
}
func (b *sendAcceptanceBody) Close() error {
	b.closes.Add(1)
	if b.closeFails {
		return errors.New("SYNTHETIC PRIVATE CLOSE ERROR")
	}
	return nil
}
