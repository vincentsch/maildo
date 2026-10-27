package outlook

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

func TestArchivePolicyNarrowDecodersPreservePresenceAndOpenPurposeValues(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "missing"},
		{name: "null", raw: `null`},
		{name: "blank string", raw: `"  "`},
		{name: "direct", raw: `"user"`, want: "user"},
		{name: "unknown direct", raw: `"future-purpose"`, want: "future-purpose"},
		{name: "value envelope", raw: `{"value":"shared","ignored":"sentinel"}`, want: "shared"},
		{name: "blank envelope", raw: `{"value":"\t"}`},
		{name: "missing value", raw: `{}`, wantErr: true},
		{name: "null value", raw: `{"value":null}`, wantErr: true},
		{name: "non-string value", raw: `{"value":3}`, wantErr: true},
		{name: "number", raw: `3`, wantErr: true},
		{name: "boolean", raw: `true`, wantErr: true},
		{name: "array", raw: `[]`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeMailboxPurpose(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("decodeMailboxPurpose(%s) = %q, %v", tt.raw, got, err)
			}
		})
	}

	for _, tt := range []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{}, {raw: `null`}, {raw: `""`}, {raw: `"  "`},
		{raw: `" archive/id "`, want: " archive/id "},
		{raw: `3`, wantErr: true}, {raw: `false`, wantErr: true}, {raw: `[]`, wantErr: true}, {raw: `{}`, wantErr: true},
	} {
		got, err := decodeArchiveFolderID(json.RawMessage(tt.raw))
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Fatalf("decodeArchiveFolderID(%s) = %q, %v", tt.raw, got, err)
		}
	}
}

func TestArchivePolicyRejectsAmbiguousConsumedProviderFields(t *testing.T) {
	validSettings := `{"archiveFolder":"archive-id","userPurpose":"user"}`
	validFolder := `{"id":"archive-id","displayName":"Archive","totalItemCount":0,"unreadItemCount":0,"childFolderCount":0}`
	for _, tt := range []struct {
		name         string
		settingsBody string
		folderBody   string
		wantCalls    int64
		wantErr      bool
	}{
		{name: "duplicate archive folder", settingsBody: `{"archiveFolder":"archive-id","archiveFolder":"archive-id"}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "case variant archive folder", settingsBody: `{"archiveFolder":"archive-id","ArchiveFolder":"archive-id"}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "duplicate purpose", settingsBody: `{"archiveFolder":"archive-id","userPurpose":"user","userPurpose":"user"}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "case variant purpose", settingsBody: `{"archiveFolder":"archive-id","userPurpose":"user","UserPurpose":"user"}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "duplicate purpose envelope value", settingsBody: `{"archiveFolder":"archive-id","userPurpose":{"value":"user","value":"user"}}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "case variant purpose envelope value", settingsBody: `{"archiveFolder":"archive-id","userPurpose":{"value":"user","Value":"user"}}`, folderBody: validFolder, wantCalls: 1, wantErr: true},
		{name: "duplicate folder id", settingsBody: validSettings, folderBody: `{"id":"archive-id","id":"archive-id","displayName":"Archive"}`, wantCalls: 2, wantErr: true},
		{name: "case variant folder id", settingsBody: validSettings, folderBody: `{"id":"archive-id","ID":"archive-id","displayName":"Archive"}`, wantCalls: 2, wantErr: true},
		{name: "duplicate display name", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"Archive","displayName":"Archive"}`, wantCalls: 2, wantErr: true},
		{name: "duplicate total count", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"Archive","totalItemCount":0,"totalItemCount":0}`, wantCalls: 2, wantErr: true},
		{name: "duplicate unread count", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"Archive","unreadItemCount":0,"unreadItemCount":0}`, wantCalls: 2, wantErr: true},
		{name: "duplicate child count", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"Archive","childFolderCount":0,"childFolderCount":0}`, wantCalls: 2, wantErr: true},
		{name: "duplicate unknown fields stay ignored", settingsBody: `{"archiveFolder":"archive-id","ignored":1,"ignored":2}`, folderBody: `{"id":"archive-id","displayName":"Archive","ignored":1,"ignored":2}`, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			client := &Client{
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				tokenType: "Bearer", accessToken: secretFromString("secret"),
				http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					body := tt.settingsBody
					if call == 2 {
						body = tt.folderBody
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
				})},
			}
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if (err != nil) != tt.wantErr || calls.Load() != tt.wantCalls {
				t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
			}
			if tt.wantErr && !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
				t.Fatalf("ambiguous provider data returned partial report %#v", report)
			}
		})
	}
}

func TestArchivePolicyRejectsMalformedUnicodeInConsumedStrings(t *testing.T) {
	invalidString := func(prefix, suffix string) string {
		return string(append(append([]byte(prefix), 0xff), []byte(suffix)...))
	}
	validSettings := `{"archiveFolder":"archive-id","userPurpose":"user"}`
	validFolder := `{"id":"archive-id","displayName":"Archive"}`
	for _, tt := range []struct {
		name         string
		settingsBody string
		folderBody   string
		wantCalls    int64
	}{
		{name: "archive folder id", settingsBody: invalidString(`{"archiveFolder":"`, `"}`), folderBody: validFolder, wantCalls: 1},
		{name: "direct purpose", settingsBody: invalidString(`{"archiveFolder":"archive-id","userPurpose":"`, `"}`), folderBody: validFolder, wantCalls: 1},
		{name: "purpose envelope", settingsBody: invalidString(`{"archiveFolder":"archive-id","userPurpose":{"value":"`, `"}}`), folderBody: validFolder, wantCalls: 1},
		{name: "folder id", settingsBody: validSettings, folderBody: invalidString(`{"id":"`, `","displayName":"Archive"}`), wantCalls: 2},
		{name: "folder display name", settingsBody: validSettings, folderBody: invalidString(`{"id":"archive-id","displayName":"`, `"}`), wantCalls: 2},
		{name: "archive folder id unpaired high surrogate", settingsBody: `{"archiveFolder":"\ud800"}`, folderBody: validFolder, wantCalls: 1},
		{name: "archive folder id unpaired low surrogate", settingsBody: `{"archiveFolder":"\udc00"}`, folderBody: validFolder, wantCalls: 1},
		{name: "direct purpose unpaired high surrogate", settingsBody: `{"archiveFolder":"archive-id","userPurpose":"\ud800"}`, folderBody: validFolder, wantCalls: 1},
		{name: "direct purpose unpaired low surrogate", settingsBody: `{"archiveFolder":"archive-id","userPurpose":"\udc00"}`, folderBody: validFolder, wantCalls: 1},
		{name: "purpose envelope unpaired high surrogate", settingsBody: `{"archiveFolder":"archive-id","userPurpose":{"value":"\ud800"}}`, folderBody: validFolder, wantCalls: 1},
		{name: "purpose envelope unpaired low surrogate", settingsBody: `{"archiveFolder":"archive-id","userPurpose":{"value":"\udc00"}}`, folderBody: validFolder, wantCalls: 1},
		{name: "folder id unpaired high surrogate", settingsBody: validSettings, folderBody: `{"id":"\ud800","displayName":"Archive"}`, wantCalls: 2},
		{name: "folder id unpaired low surrogate", settingsBody: validSettings, folderBody: `{"id":"\udc00","displayName":"Archive"}`, wantCalls: 2},
		{name: "folder display name unpaired high surrogate", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"\ud800"}`, wantCalls: 2},
		{name: "folder display name unpaired low surrogate", settingsBody: validSettings, folderBody: `{"id":"archive-id","displayName":"\udc00"}`, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			client := &Client{
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				tokenType: "Bearer", accessToken: secretFromString("secret"),
				http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					body := tt.settingsBody
					if calls.Add(1) == 2 {
						body = tt.folderBody
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
				})},
			}
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if err == nil || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) || calls.Load() != tt.wantCalls {
				t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
			}
		})
	}
}

func TestArchivePolicyPreservesValidEscapedUnicodeStrings(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "surrogate pair", raw: `"\ud83d\ude00"`, want: "😀"},
		{name: "replacement character", raw: `"\ufffd"`, want: "�"},
		{name: "literal escape text", raw: `"\\ud800"`, want: `\ud800`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeArchivePolicyString([]byte(tt.raw))
			if err != nil || got != tt.want {
				t.Fatalf("decoded=%q err=%v", got, err)
			}
		})
	}
}

func TestArchivePolicyFolderCountsDistinguishUnknownZeroAndInt32Bounds(t *testing.T) {
	for _, tt := range []struct {
		raw       string
		want      int
		wantKnown bool
		wantErr   bool
	}{
		{}, {raw: `null`},
		{raw: `0`, wantKnown: true},
		{raw: `2147483647`, want: math.MaxInt32, wantKnown: true},
		{raw: `-1`, wantErr: true},
		{raw: `2147483648`, wantErr: true},
		{raw: `1.0`, wantErr: true},
		{raw: `"1"`, wantErr: true},
	} {
		got, known, err := decodeGraphCount(json.RawMessage(tt.raw))
		if (err != nil) != tt.wantErr || got != tt.want || known != tt.wantKnown {
			t.Fatalf("decodeGraphCount(%s) = %d, %t, %v", tt.raw, got, known, err)
		}
	}

	folder, err := decodeArchivePolicyFolder([]byte(`{"id":"archive","displayName":"Archive","totalItemCount":0,"childFolderCount":2}`), "archive")
	if err != nil {
		t.Fatal(err)
	}
	if !folder.TotalItemCountKnown || folder.UnreadItemCountKnown || folder.UnreadItemCount != 0 || !folder.ChildFolderCountKnown {
		t.Fatalf("presence-aware folder = %#v", folder)
	}
	for _, body := range []string{
		`{"displayName":"Archive"}`,
		`{"id":"Archive","displayName":"Archive"}`,
		`{"id":"different","displayName":"Archive"}`,
		`{"id":"archive","displayName":" "}`,
		`{"id":"archive","displayName":"Archive","totalItemCount":-1}`,
		`{"id":"archive","displayName":"Archive","unreadItemCount":2147483648}`,
	} {
		if _, err := decodeArchivePolicyFolder([]byte(body), "archive"); err == nil {
			t.Fatalf("decodeArchivePolicyFolder accepted %s", body)
		}
	}
}

func TestArchivePolicyReportUsesOneRoundTripPerLogicalRead(t *testing.T) {
	var calls atomic.Int64
	client := &Client{
		graphBaseURL: "https://graph.example.test/v1.0", account: "work",
		tokenType: "Bearer", accessToken: secretFromString("secret"),
		http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			call := calls.Add(1)
			var payload string
			switch call {
			case 1:
				payload = `{"archiveFolder":"archive-id","userPurpose":"user"}`
			case 2:
				payload = `{"id":"archive-id","displayName":"Archive","totalItemCount":0,"unreadItemCount":0,"childFolderCount":0}`
			default:
				t.Fatalf("unexpected RoundTrip %d", call)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header), Request: req}, nil
		})},
	}
	report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
	if err != nil || calls.Load() != 2 || report.ArchiveFolder == nil || report.ArchiveFolder.ID != "archive-id" {
		t.Fatalf("report=%#v err=%v RoundTrips=%d", report, err, calls.Load())
	}
}

func TestArchiveFolderTargetKeepsProviderIDAsOneOpaqueSegment(t *testing.T) {
	client := &Client{graphBaseURL: "https://graph.example.test/v1.0", account: "work"}
	values := url.Values{}
	values.Set("$select", "id,displayName,totalItemCount,unreadItemCount,childFolderCount")
	for _, id := range []string{"a/b", "%", "%2F", "?", "#", ".", "..", "Grüße", " archive "} {
		t.Run(id, func(t *testing.T) {
			target, err := client.archiveFolderTarget(id, values)
			if err != nil {
				t.Fatal(err)
			}
			escapedID := url.PathEscape(id)
			if id == "." {
				escapedID = "%2E"
			} else if id == ".." {
				escapedID = "%2E%2E"
			}
			if target.Path != "/v1.0/me/mailFolders/"+id || target.RawPath != "/v1.0/me/mailFolders/"+escapedID || target.EscapedPath() != "/v1.0/me/mailFolders/"+escapedID || target.RawQuery != values.Encode() {
				t.Fatalf("target for %q = Path:%q RawPath:%q Escaped:%q Query:%q", id, target.Path, target.RawPath, target.EscapedPath(), target.RawQuery)
			}
			requestURI := target.RequestURI()
			if !strings.HasPrefix(requestURI, "/v1.0/me/mailFolders/"+escapedID+"?") || strings.Count(strings.Split(requestURI, "?")[0], "/") != 4 {
				t.Fatalf("RequestURI for %q = %q", id, requestURI)
			}
		})
	}
	if _, err := client.archiveFolderTarget(" \t ", values); err == nil {
		t.Fatal("blank archive folder id was accepted")
	}
}

func TestFinalizeArchivePolicyReportEnforcesBranchesAndFixedAvailability(t *testing.T) {
	client := &Client{account: "work"}
	folder := &mail.ArchivePolicyFolder{ID: "archive", DisplayName: "Archive", TotalItemCountKnown: true}
	valid := []struct {
		status   mail.ArchivePolicyReportStatus
		purpose  string
		id       string
		folder   *mail.ArchivePolicyFolder
		warnings []mail.ArchivePolicyWarning
	}{
		{mail.ArchivePolicyReportUnavailable, "", "", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningMailboxSettingsUnavailable}},
		{mail.ArchivePolicyReportPartial, "user", "", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}},
		{mail.ArchivePolicyReportPartial, "user", "archive", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}},
		{mail.ArchivePolicyReportPartial, "future", "archive", folder, nil},
	}
	for _, tt := range valid {
		report, err := client.finalizeArchivePolicyReport(tt.status, tt.purpose, tt.id, tt.folder, tt.warnings)
		if err != nil {
			t.Fatalf("valid branch failed: %v", err)
		}
		if report.Account != "work" || report.Provider != "outlook" || report.OnlineArchive.Reason != mail.ArchivePolicyReasonNotExposedByGraphMailAPI || report.RetentionPolicy.Reason != mail.ArchivePolicyReasonRequiresExchangeAdminContext || report.ProviderMetadata.Source != mail.ArchivePolicySourceMicrosoftGraph || report.Warnings == nil {
			t.Fatalf("finalized report = %#v", report)
		}
	}

	invalid := []struct {
		status   mail.ArchivePolicyReportStatus
		id       string
		folder   *mail.ArchivePolicyFolder
		warnings []mail.ArchivePolicyWarning
	}{
		{"", "", nil, nil},
		{mail.ArchivePolicyReportUnavailable, "archive", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningMailboxSettingsUnavailable}},
		{mail.ArchivePolicyReportPartial, "", nil, nil},
		{mail.ArchivePolicyReportPartial, "archive", nil, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}},
		{mail.ArchivePolicyReportPartial, "archive", folder, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}},
		{mail.ArchivePolicyReportPartial, "archive", folder, []mail.ArchivePolicyWarning{"future_warning"}},
		{mail.ArchivePolicyReportPartial, "archive", folder, []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved, mail.ArchivePolicyWarningFolderUnresolved}},
		{mail.ArchivePolicyReportPartial, "archive", &mail.ArchivePolicyFolder{ID: "archive", DisplayName: "Archive", TotalItemCount: 1}, nil},
	}
	for _, tt := range invalid {
		if _, err := client.finalizeArchivePolicyReport(tt.status, "", tt.id, tt.folder, tt.warnings); err == nil {
			t.Fatalf("invalid branch accepted: %#v", tt)
		}
	}
}

func TestArchivePolicyWarningPrecedenceOrderIsStable(t *testing.T) {
	want := []mail.ArchivePolicyWarning{
		mail.ArchivePolicyWarningMailboxSettingsUnavailable,
		mail.ArchivePolicyWarningFolderIDMissing,
		mail.ArchivePolicyWarningFolderUnresolved,
	}
	if !reflect.DeepEqual(archivePolicyWarnings, want) {
		t.Fatalf("warning precedence=%#v, want %#v", archivePolicyWarnings, want)
	}
}

func TestArchivePolicyReportConditionalReadsAndOutcomePrecedence(t *testing.T) {
	type fixture struct {
		settingsStatus int
		settingsBody   string
		folderStatus   int
		folderBody     string
	}
	for _, tt := range []struct {
		name          string
		fixture       fixture
		wantStatus    mail.ArchivePolicyReportStatus
		wantWarning   mail.ArchivePolicyWarning
		wantFolder    bool
		wantFolderGET int64
		wantErr       bool
	}{
		{name: "resolved", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id","userPurpose":{"value":"user","ignored":"sentinel"},"automaticRepliesSetting":{"internalReplyMessage":"ignored-secret"}}`, folderBody: `{"id":"archive-id","displayName":"Archive","totalItemCount":0,"unreadItemCount":0,"childFolderCount":0,"ignored":"sentinel"}`}, wantStatus: mail.ArchivePolicyReportPartial, wantFolder: true, wantFolderGET: 1},
		{name: "settings unavailable", fixture: fixture{settingsStatus: http.StatusNotFound, settingsBody: strings.Repeat("x", maxBodyBytes+10)}, wantStatus: mail.ArchivePolicyReportUnavailable, wantWarning: mail.ArchivePolicyWarningMailboxSettingsUnavailable},
		{name: "settings gone", fixture: fixture{settingsStatus: http.StatusGone, settingsBody: strings.Repeat("x", maxBodyBytes+10)}, wantStatus: mail.ArchivePolicyReportUnavailable, wantWarning: mail.ArchivePolicyWarningMailboxSettingsUnavailable},
		{name: "folder id missing", fixture: fixture{settingsBody: `{"archiveFolder":null,"userPurpose":"user"}`}, wantStatus: mail.ArchivePolicyReportPartial, wantWarning: mail.ArchivePolicyWarningFolderIDMissing},
		{name: "blank folder id", fixture: fixture{settingsBody: `{"archiveFolder":"  ","userPurpose":"user"}`}, wantStatus: mail.ArchivePolicyReportPartial, wantWarning: mail.ArchivePolicyWarningFolderIDMissing},
		{name: "folder not found", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id"}`, folderStatus: http.StatusNotFound, folderBody: strings.Repeat("x", maxBodyBytes+10)}, wantStatus: mail.ArchivePolicyReportPartial, wantWarning: mail.ArchivePolicyWarningFolderUnresolved, wantFolderGET: 1},
		{name: "folder unresolved", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id"}`, folderStatus: http.StatusGone, folderBody: strings.Repeat("x", maxBodyBytes+10)}, wantStatus: mail.ArchivePolicyReportPartial, wantWarning: mail.ArchivePolicyWarningFolderUnresolved, wantFolderGET: 1},
		{name: "settings ordinary 4xx", fixture: fixture{settingsStatus: http.StatusBadRequest}, wantErr: true},
		{name: "malformed settings", fixture: fixture{settingsBody: `{"archiveFolder":`}, wantErr: true},
		{name: "null settings object", fixture: fixture{settingsBody: `null`}, wantErr: true},
		{name: "invalid archive folder shape", fixture: fixture{settingsBody: `{"archiveFolder":false}`}, wantErr: true},
		{name: "invalid mailbox purpose", fixture: fixture{settingsBody: `{"archiveFolder":null,"userPurpose":{"value":null}}`}, wantErr: true},
		{name: "second read fails", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id","userPurpose":"user"}`, folderStatus: http.StatusInternalServerError}, wantFolderGET: 1, wantErr: true},
		{name: "malformed folder", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id"}`, folderBody: `{"id":`}, wantFolderGET: 1, wantErr: true},
		{name: "folder id differs only by case", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id"}`, folderBody: `{"id":"ARCHIVE-ID","displayName":"Archive"}`}, wantFolderGET: 1, wantErr: true},
		{name: "invalid folder data", fixture: fixture{settingsBody: `{"archiveFolder":"archive-id"}`, folderBody: `{"id":"archive-id","displayName":"Archive","totalItemCount":2147483648}`}, wantFolderGET: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var settingsCalls, folderCalls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" || r.Method != http.MethodGet || r.Header.Get("Prefer") != "" {
					t.Errorf("request headers/method = %#v %s", r.Header, r.Method)
				}
				switch {
				case r.URL.Path == "/me/mailboxSettings":
					settingsCalls.Add(1)
					if got := r.URL.Query().Get("$select"); got != "archiveFolder,userPurpose" {
						t.Errorf("settings select = %q", got)
					}
					status := tt.fixture.settingsStatus
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(tt.fixture.settingsBody))
				case r.URL.Path == "/me/mailFolders/archive-id":
					folderCalls.Add(1)
					if got := r.URL.Query().Get("$select"); got != "id,displayName,totalItemCount,unreadItemCount,childFolderCount" {
						t.Errorf("folder select = %q", got)
					}
					status := tt.fixture.folderStatus
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(tt.fixture.folderBody))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := &Client{graphBaseURL: srv.URL, http: srv.Client(), tokenType: "Bearer", accessToken: secretFromString("secret"), account: "work"}
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ArchivePolicyReport error = %v", err)
			}
			if settingsCalls.Load() != 1 || folderCalls.Load() != tt.wantFolderGET {
				t.Fatalf("calls settings=%d folder=%d", settingsCalls.Load(), folderCalls.Load())
			}
			if tt.wantErr {
				if !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) {
					t.Fatalf("failure returned partial report %#v", report)
				}
				return
			}
			if report.Status != tt.wantStatus || (len(report.Warnings) == 1 && report.Warnings[0] != tt.wantWarning) || (report.ArchiveFolder != nil) != tt.wantFolder {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestArchivePolicyReportTransportAndReadFailuresReturnNoPartialResult(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failFolder bool
		readError  bool
		wantCalls  int64
	}{
		{name: "settings transport", wantCalls: 1},
		{name: "settings body read", readError: true, wantCalls: 1},
		{name: "folder transport", failFolder: true, wantCalls: 2},
		{name: "folder body read", failFolder: true, readError: true, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			var failedBody *observedReadCloser
			client := &Client{
				graphBaseURL: "https://graph.example.test/v1.0", account: "work",
				tokenType: "Bearer", accessToken: secretFromString("secret"),
				http: &http.Client{Transport: graphReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					isFailureCall := (!tt.failFolder && call == 1) || (tt.failFolder && call == 2)
					if isFailureCall {
						if !tt.readError {
							return nil, context.Canceled
						}
						failedBody = &observedReadCloser{body: failingReader{}}
						return &http.Response{StatusCode: http.StatusOK, Body: failedBody, Header: make(http.Header), Request: req}, nil
					}
					body := io.NopCloser(strings.NewReader(`{"archiveFolder":"archive-id","userPurpose":"user"}`))
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: req}, nil
				})},
			}
			report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
			if err == nil || !reflect.DeepEqual(report, mail.ArchivePolicyReport{}) || calls.Load() != tt.wantCalls {
				t.Fatalf("report=%#v err=%v calls=%d", report, err, calls.Load())
			}
			if failedBody != nil && !failedBody.closed.Load() {
				t.Fatal("failed response body was not closed")
			}
		})
	}
}
