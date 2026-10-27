package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/mail"
)

func TestArchivePolicyRejectsNonObjectProviderDocuments(t *testing.T) {
	validSettings := []byte(`{"archiveFolder":"archive-id","userPurpose":"user"}`)
	for _, tt := range []struct {
		name        string
		settings    []byte
		folder      []byte
		wantCalls   int64
		wantErr     bool
		wantWarning mail.ArchivePolicyWarning
	}{
		{name: "settings string", settings: []byte(`"value"`), wantCalls: 1, wantErr: true},
		{name: "settings number", settings: []byte(`1`), wantCalls: 1, wantErr: true},
		{name: "settings boolean", settings: []byte(`true`), wantCalls: 1, wantErr: true},
		{name: "settings array", settings: []byte(`[]`), wantCalls: 1, wantErr: true},
		{name: "settings null", settings: []byte(`null`), wantCalls: 1, wantErr: true},
		{name: "settings empty", settings: []byte{}, wantCalls: 1, wantErr: true},
		{name: "settings truncated", settings: []byte(`{"archiveFolder":`), wantCalls: 1, wantErr: true},
		{name: "settings trailing document", settings: []byte(`{} {}`), wantCalls: 1, wantErr: true},
		{name: "settings unrelated object", settings: []byte(`{"ignored":{"deep":[1,2,3]}}`), wantCalls: 1, wantWarning: mail.ArchivePolicyWarningFolderIDMissing},
		{name: "folder string", settings: validSettings, folder: []byte(`"value"`), wantCalls: 2, wantErr: true},
		{name: "folder number", settings: validSettings, folder: []byte(`1`), wantCalls: 2, wantErr: true},
		{name: "folder boolean", settings: validSettings, folder: []byte(`true`), wantCalls: 2, wantErr: true},
		{name: "folder array", settings: validSettings, folder: []byte(`[]`), wantCalls: 2, wantErr: true},
		{name: "folder null", settings: validSettings, folder: []byte(`null`), wantCalls: 2, wantErr: true},
		{name: "folder empty", settings: validSettings, folder: []byte{}, wantCalls: 2, wantErr: true},
		{name: "folder truncated", settings: validSettings, folder: []byte(`{"id":`), wantCalls: 2, wantErr: true},
		{name: "folder trailing document", settings: validSettings, folder: []byte(`{"id":"archive-id","displayName":"Archive"} {}`), wantCalls: 2, wantErr: true},
		{name: "folder nested extras", settings: validSettings, folder: []byte(`{"id":"archive-id","displayName":"Archive","ignored":{"deep":[1,2,3]}}`), wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			client := archivePolicyBodyClient(&calls, tt.settings, tt.folder)
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if (err != nil) != tt.wantErr || calls.Load() != tt.wantCalls {
				t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
			}
			if tt.wantErr {
				if !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
					t.Fatalf("invalid document returned partial report %#v", report)
				}
				return
			}
			if tt.wantWarning != "" && !reflect.DeepEqual(report.Warnings, []mail.ArchivePolicyWarning{tt.wantWarning}) {
				t.Fatalf("warnings = %#v", report.Warnings)
			}
		})
	}
}

func TestArchivePolicyCountDecoderRejectsEveryNonIntegerShape(t *testing.T) {
	for _, field := range []string{"totalItemCount", "unreadItemCount", "childFolderCount"} {
		for _, tt := range []struct {
			name      string
			raw       string
			omit      bool
			want      int
			wantKnown bool
			wantErr   bool
		}{
			{name: "missing", omit: true},
			{name: "null", raw: "null"},
			{name: "zero", raw: "0", wantKnown: true},
			{name: "negative zero", raw: "-0", wantKnown: true},
			{name: "one", raw: "1", want: 1, wantKnown: true},
			{name: "maximum", raw: "2147483647", want: math.MaxInt32, wantKnown: true},
			{name: "negative", raw: "-1", wantErr: true},
			{name: "over maximum", raw: "2147483648", wantErr: true},
			{name: "fraction", raw: "1.5", wantErr: true},
			{name: "exponent", raw: "1e0", wantErr: true},
			{name: "string", raw: `"1"`, wantErr: true},
			{name: "boolean", raw: "true", wantErr: true},
			{name: "array", raw: "[]", wantErr: true},
			{name: "object", raw: "{}", wantErr: true},
			{name: "over int64", raw: "9223372036854775808", wantErr: true},
		} {
			t.Run(field+"/"+tt.name, func(t *testing.T) {
				body := `{"id":"archive-id","displayName":"Archive"}`
				if !tt.omit {
					body = fmt.Sprintf(`{"id":"archive-id","displayName":"Archive",%q:%s}`, field, tt.raw)
				}
				folder, err := decodeArchivePolicyFolder([]byte(body), "archive-id")
				if (err != nil) != tt.wantErr {
					t.Fatalf("folder=%#v err=%v", folder, err)
				}
				if tt.wantErr {
					return
				}
				var got int
				var known bool
				switch field {
				case "totalItemCount":
					got, known = folder.TotalItemCount, folder.TotalItemCountKnown
				case "unreadItemCount":
					got, known = folder.UnreadItemCount, folder.UnreadItemCountKnown
				case "childFolderCount":
					got, known = folder.ChildFolderCount, folder.ChildFolderCountKnown
				}
				if got != tt.want || known != tt.wantKnown || (!known && got != 0) {
					t.Fatalf("count=%d known=%t", got, known)
				}
			})
		}
	}
}

func TestArchivePolicyFinalizerCopiesWarnings(t *testing.T) {
	client := &Client{account: "work"}
	warnings := []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}
	report, err := client.finalizeArchivePolicyReport(mail.ArchivePolicyReportPartial, "user", "archive-id", nil, warnings)
	if err != nil {
		t.Fatal(err)
	}
	warnings[0] = mail.ArchivePolicyWarningMailboxSettingsUnavailable
	if !reflect.DeepEqual(report.Warnings, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}) {
		t.Fatalf("finalized warnings aliased caller memory: %#v", report.Warnings)
	}
}

func TestArchivePolicyValidDocumentsRespectExactReadLimit(t *testing.T) {
	settingsDocument := func(size int, archiveID string) []byte {
		prefix := fmt.Sprintf(`{"archiveFolder":%q,"padding":"`, archiveID)
		suffix := `"}`
		if archiveID == "" {
			prefix = `{"archiveFolder":null,"padding":"`
		}
		if size < len(prefix)+len(suffix) {
			t.Fatalf("settings document size %d is too small", size)
		}
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}
	folderDocument := func(size int) []byte {
		prefix := `{"id":"archive-id","displayName":"Archive","padding":"`
		suffix := `"}`
		if size < len(prefix)+len(suffix) {
			t.Fatalf("folder document size %d is too small", size)
		}
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}

	for _, size := range []int{maxBodyBytes - 1, maxBodyBytes, maxBodyBytes + 1} {
		t.Run(fmt.Sprintf("settings-%d", size), func(t *testing.T) {
			var calls atomic.Int64
			client := archivePolicyBodyClient(&calls, settingsDocument(size, ""), nil)
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if size <= maxBodyBytes {
				if err != nil || calls.Load() != 1 || !reflect.DeepEqual(report.Warnings, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}) {
					t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
				}
			} else if err == nil || calls.Load() != 1 || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
				t.Fatalf("oversized report=%#v err=%v calls=%d", report, err, calls.Load())
			}
		})

		t.Run(fmt.Sprintf("folder-%d", size), func(t *testing.T) {
			var calls atomic.Int64
			client := archivePolicyBodyClient(&calls, settingsDocument(128, "archive-id"), folderDocument(size))
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if size <= maxBodyBytes {
				if err != nil || calls.Load() != 2 || report.ArchiveFolder == nil || report.ArchiveFolder.ID != "archive-id" {
					t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
				}
			} else if err == nil || calls.Load() != 2 || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
				t.Fatalf("oversized report=%#v err=%v calls=%d", report, err, calls.Load())
			}
		})
	}
}

type blockingArchiveBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (b *blockingArchiveBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *blockingArchiveBody) Close() error {
	b.closed.Store(true)
	return nil
}

type cancelAfterArchiveBody struct {
	body   []byte
	cancel context.CancelFunc
	sent   bool
	closed atomic.Bool
}

func (b *cancelAfterArchiveBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.body), nil
	}
	b.cancel()
	return 0, io.EOF
}

func (b *cancelAfterArchiveBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestArchivePolicyCancellationStopsConditionalReads(t *testing.T) {
	t.Run("before settings dispatch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int64
		client := archivePolicyBodyClient(&calls, []byte(`{}`), nil)
		report, err := client.ArchivePolicyReport(ctx, mail.ArchivePolicyReportRequest{})
		if err == nil || calls.Load() != 0 || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
			t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
		}
	})

	t.Run("while reading settings", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		body := &blockingArchiveBody{ctx: ctx, started: make(chan struct{})}
		var calls atomic.Int64
		client := archivePolicyReadCloserClient(&calls, body, nil)
		result := make(chan error, 1)
		go func() {
			report, err := client.ArchivePolicyReport(ctx, mail.ArchivePolicyReportRequest{})
			if !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
				result <- fmt.Errorf("partial report %#v", report)
				return
			}
			result <- err
		}()
		awaitArchiveSignal(t, body.started)
		cancel()
		if err := awaitArchiveResult(t, result); err == nil || calls.Load() != 1 || !body.closed.Load() {
			t.Fatalf("err=%v calls=%d closed=%t", err, calls.Load(), body.closed.Load())
		}
	})

	t.Run("after settings before folder dispatch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		body := &cancelAfterArchiveBody{body: []byte(`{"archiveFolder":"archive-id"}`), cancel: cancel}
		var calls atomic.Int64
		client := archivePolicyReadCloserClient(&calls, body, io.NopCloser(strings.NewReader(`{"id":"archive-id","displayName":"Archive"}`)))
		report, err := client.ArchivePolicyReport(ctx, mail.ArchivePolicyReportRequest{})
		if err == nil || calls.Load() != 1 || !body.closed.Load() || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
			t.Fatalf("report=%#v err=%v calls=%d closed=%t", report, err, calls.Load(), body.closed.Load())
		}
	})

	t.Run("while reading folder", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		body := &blockingArchiveBody{ctx: ctx, started: make(chan struct{})}
		var calls atomic.Int64
		client := archivePolicyReadCloserClient(&calls, io.NopCloser(strings.NewReader(`{"archiveFolder":"archive-id"}`)), body)
		result := make(chan error, 1)
		go func() {
			report, err := client.ArchivePolicyReport(ctx, mail.ArchivePolicyReportRequest{})
			if !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
				result <- fmt.Errorf("partial report %#v", report)
				return
			}
			result <- err
		}()
		awaitArchiveSignal(t, body.started)
		cancel()
		if err := awaitArchiveResult(t, result); err == nil || calls.Load() != 2 || !body.closed.Load() {
			t.Fatalf("err=%v calls=%d closed=%t", err, calls.Load(), body.closed.Load())
		}
	})
}

type countedArchiveBody struct {
	io.Reader
	closed *atomic.Int64
}

func (b *countedArchiveBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestArchivePolicyRepeatedReadsCloseEveryResponse(t *testing.T) {
	var created, closed atomic.Int64
	for i := 0; i < 1000; i++ {
		mode := i % 7
		var calls atomic.Int64
		ctx := context.Background()
		if mode == 6 {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = canceled
		}
		client := &Client{
			graphBaseURL: "https://graph.example.test/v1.0", account: "work",
			tokenType: "Bearer", accessToken: secretFromString("secret"),
			http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				status := http.StatusOK
				payload := `{"archiveFolder":"archive-id","userPurpose":"user"}`
				switch mode {
				case 1:
					status, payload = http.StatusNotFound, "ignored"
				case 2:
					payload = `{"archiveFolder":null}`
				case 3:
					if call == 2 {
						status, payload = http.StatusGone, "ignored"
					}
				case 4:
					status, payload = http.StatusInternalServerError, "ignored"
				case 5:
					payload = `{"archiveFolder":`
				default:
					if call == 2 {
						payload = `{"id":"archive-id","displayName":"Archive"}`
					}
				}
				created.Add(1)
				return &http.Response{StatusCode: status, Body: &countedArchiveBody{Reader: strings.NewReader(payload), closed: &closed}, Header: make(http.Header), Request: req}, nil
			})},
		}
		_, _ = client.ArchivePolicyReport(ctx, mail.ArchivePolicyReportRequest{})
		if created.Load() != closed.Load() {
			t.Fatalf("iteration %d created=%d closed=%d", i, created.Load(), closed.Load())
		}
	}

	shared := &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			payload := `{"archiveFolder":"archive-id"}`
			if strings.Contains(req.URL.Path, "/mailFolders/") {
				payload = `{"id":"archive-id","displayName":"Archive"}`
			}
			created.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Body: &countedArchiveBody{Reader: strings.NewReader(payload), closed: &closed}, Header: make(http.Header), Request: req}, nil
		})},
	}
	var wait sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := shared.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != closed.Load() {
		t.Fatalf("after concurrent reads created=%d closed=%d", created.Load(), closed.Load())
	}
}

func FuzzArchivePolicyNarrowDecoders(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte(`null`), []byte(`"archive-id"`), []byte(`{"value":"user"}`), []byte(`"\ud800"`), []byte(`{"value":"\udc00"}`), []byte(`"\ud83d\ude00"`), []byte(`0`), []byte(`2147483647`), []byte(`2147483648`)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		folderID, folderErr := decodeArchiveFolderID(raw)
		if folderErr == nil {
			if !utf8.ValidString(folderID) {
				t.Fatalf("accepted folder id with invalid UTF-8: %q", raw)
			}
			var decoded string
			if err := json.Unmarshal(raw, &decoded); err == nil {
				if !validJSONUnicodeEscapes(raw) {
					t.Fatalf("accepted folder id with an unpaired surrogate: %q", raw)
				}
				if strings.TrimSpace(decoded) == "" {
					decoded = ""
				}
				if folderID != decoded {
					t.Fatalf("folder id changed: got=%q want=%q raw=%q", folderID, decoded, raw)
				}
			}
		}

		purpose, purposeErr := decodeMailboxPurpose(raw)
		if purposeErr == nil {
			if !utf8.ValidString(purpose) {
				t.Fatalf("accepted mailbox purpose with invalid UTF-8: %q", raw)
			}
			var consumed json.RawMessage
			var direct string
			if err := json.Unmarshal(raw, &direct); err == nil {
				consumed = raw
			} else {
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(raw, &envelope); err == nil {
					consumed = envelope["value"]
				}
			}
			if len(consumed) != 0 {
				if !validJSONUnicodeEscapes(consumed) {
					t.Fatalf("accepted mailbox purpose with an unpaired surrogate: %q", raw)
				}
				var decoded string
				if err := json.Unmarshal(consumed, &decoded); err == nil {
					if strings.TrimSpace(decoded) == "" {
						decoded = ""
					}
					if purpose != decoded {
						t.Fatalf("mailbox purpose changed: got=%q want=%q raw=%q", purpose, decoded, raw)
					}
				}
			}
		}
		count, known, err := decodeGraphCount(raw)
		if err == nil && (count < 0 || int64(count) > math.MaxInt32 || (!known && count != 0)) {
			t.Fatalf("accepted invalid count=%d known=%t raw=%q", count, known, raw)
		}
	})
}

func FuzzArchiveFolderTargetStaysPinned(f *testing.F) {
	for _, seed := range []string{"archive-id", "a/b", "%2F", ".", "..", "Grüße", " archive "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		if strings.TrimSpace(id) == "" {
			return
		}
		client := &Client{graphBaseURL: "https://graph.example.test/v1.0", account: "work"}
		values := url.Values{"$select": []string{"id,displayName,totalItemCount,unreadItemCount,childFolderCount"}}
		target, err := client.archiveFolderTarget(id, values)
		if err != nil {
			return
		}
		if target.Scheme != "https" || target.Host != "graph.example.test" || target.RawQuery != values.Encode() || !strings.HasPrefix(target.EscapedPath(), "/v1.0/me/mailFolders/") {
			t.Fatalf("target escaped boundary: %#v", target)
		}
		tail := strings.TrimPrefix(target.EscapedPath(), "/v1.0/me/mailFolders/")
		decoded, err := url.PathUnescape(tail)
		if err != nil || decoded != id || strings.Contains(tail, "/") {
			t.Fatalf("id=%q tail=%q decoded=%q err=%v", id, tail, decoded, err)
		}
	})
}

func FuzzArchivePolicyFinalizerInvariants(f *testing.F) {
	f.Add(uint8(0), "user")
	f.Add(uint8(1), "future-purpose")
	f.Fuzz(func(t *testing.T, branch uint8, purpose string) {
		client := &Client{account: "work"}
		var report mail.ArchivePolicyReport
		var err error
		switch branch % 4 {
		case 0:
			report, err = client.finalizeArchivePolicyReport(mail.ArchivePolicyReportUnavailable, "", "", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningMailboxSettingsUnavailable})
		case 1:
			report, err = client.finalizeArchivePolicyReport(mail.ArchivePolicyReportPartial, purpose, "", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing})
		case 2:
			report, err = client.finalizeArchivePolicyReport(mail.ArchivePolicyReportPartial, purpose, "archive-id", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved})
		case 3:
			folder := &mail.ArchivePolicyFolder{ID: "archive-id", DisplayName: "Archive"}
			report, err = client.finalizeArchivePolicyReport(mail.ArchivePolicyReportPartial, purpose, "archive-id", folder, nil)
		}
		if err != nil || report.Account != "work" || report.Provider != "outlook" || report.Warnings == nil || report.OnlineArchive.Reason != mail.ArchivePolicyReasonNotExposedByGraphMailAPI || report.RetentionPolicy.Reason != mail.ArchivePolicyReasonRequiresExchangeAdminContext {
			t.Fatalf("report=%#v err=%v", report, err)
		}
	})
}

func archivePolicyBodyClient(calls *atomic.Int64, settings, folder []byte) *Client {
	return archivePolicyReadCloserClient(calls, io.NopCloser(strings.NewReader(string(settings))), io.NopCloser(strings.NewReader(string(folder))))
}

func archivePolicyReadCloserClient(calls *atomic.Int64, settings, folder io.ReadCloser) *Client {
	return &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := settings
			if calls.Add(1) == 2 {
				body = folder
			}
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: req}, nil
		})},
	}
}

func awaitArchiveSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for archive read stage")
	}
}

func awaitArchiveResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for archive report cancellation")
		return nil
	}
}
