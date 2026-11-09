package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/mimecompose"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestCapabilities(t *testing.T) {
	caps, err := New(Options{}).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != len(provider.AllCapabilities()) {
		t.Fatalf("capabilities = %d, want %d", len(caps), len(provider.AllCapabilities()))
	}
	implemented := map[provider.Capability]bool{
		provider.CapFoldersRead:        true,
		provider.CapMessagesSearch:     true,
		provider.CapMessagesList:       true,
		provider.CapMessagesRead:       true,
		provider.CapThreadsRead:        true,
		provider.CapDraftCreate:        true,
		provider.CapDraftRead:          true,
		provider.CapDraftUpdate:        true,
		provider.CapDraftSend:          true,
		provider.CapSendDirect:         true,
		provider.CapReplyDraftCreate:   true,
		provider.CapForwardDraftCreate: true,
		provider.CapAttachmentsRead:    true,
		provider.CapAttachmentsMutate:  true,
		provider.CapFoldersMutate:      true,
		provider.CapMessagesMove:       true,
		provider.CapMessagesCopy:       true,
		provider.CapMessagesArchive:    true,
		provider.CapMessagesDelete:     true,
		provider.CapMessagesRestore:    true,
		provider.CapMessagesJunk:       true,
		provider.CapMessagesMarkRead:   true,
		provider.CapMessagesFlag:       true,
		provider.CapCategoriesRead:     true,
		provider.CapCategoriesMutate:   true,
	}
	unsupported := map[provider.Capability]bool{
		provider.CapRulesRead:           true,
		provider.CapRulesMutate:         true,
		provider.CapArchivePolicyReport: true,
	}
	for _, cap := range provider.AllCapabilities() {
		switch {
		case implemented[cap]:
			if caps.State(cap) != provider.CapImplemented {
				t.Fatalf("%s = %s, want implemented", cap, caps.State(cap))
			}
		case unsupported[cap]:
			if caps.State(cap) != provider.CapUnsupported {
				t.Fatalf("%s = %s, want unsupported", cap, caps.State(cap))
			}
		default:
			if caps.State(cap) != provider.CapSupportedNotImpl {
				t.Fatalf("%s = %s, want supported-not-implemented", cap, caps.State(cap))
			}
		}
	}
}

func TestCreateDraftUploadsMIMEAndVerifiesRaw(t *testing.T) {
	var mu sync.Mutex
	var uploaded []byte
	var uploadMeta map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		switch r.URL.Path {
		case "/upload/gmail/v1/users/me/drafts":
			if r.Method != http.MethodPost || r.URL.Query().Get("uploadType") != "multipart" {
				t.Fatalf("draft upload request = %s %s", r.Method, r.URL.String())
			}
			meta, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			uploadMeta = meta
			uploaded = raw
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			writeJSON(w, map[string]any{
				"id": "draft-1",
				"message": map[string]any{
					"id":       "msg-draft-1",
					"threadId": "thread-draft-1",
					"labelIds": []string{"DRAFT"},
				},
			})
		case "/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodGet || r.URL.Query().Get("format") != "raw" {
				t.Fatalf("draft verification request = %s %s", r.Method, r.URL.String())
			}
			mu.Lock()
			raw := append([]byte(nil), uploaded...)
			mu.Unlock()
			writeJSON(w, map[string]any{
				"id": "draft-1",
				"message": map[string]any{
					"id":       "msg-draft-1",
					"threadId": "thread-draft-1",
					"labelIds": []string{"DRAFT"},
					"raw":      base64.RawURLEncoding.EncodeToString(raw),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ref, err := testClient(server).CreateDraft(context.Background(), mail.CreateDraftRequest{Message: outboundTextMessage()})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "draft-1" || ref.WriteOutcomeFacts == nil || ref.ProviderMeta == nil {
		t.Fatalf("draft ref = %#v", ref)
	}
	mu.Lock()
	meta := uploadMeta
	raw := string(uploaded)
	mu.Unlock()
	if _, ok := meta["message"].(map[string]any); !ok || !strings.Contains(raw, "Subject: Test Subject") || !strings.Contains(raw, "Hello from Gmail") {
		t.Fatalf("upload meta=%#v raw=%q", meta, raw)
	}
}

func TestDirectSendUploadsPreparedMIMEAndVerifiesSentRaw(t *testing.T) {
	var mu sync.Mutex
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		switch r.URL.Path {
		case "/upload/gmail/v1/users/me/messages/send":
			if r.Method != http.MethodPost || r.URL.Query().Get("uploadType") != "multipart" {
				t.Fatalf("send upload request = %s %s", r.Method, r.URL.String())
			}
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "sent-1", "threadId": "thread-sent-1", "labelIds": []string{"SENT"}})
		case "/gmail/v1/users/me/messages/sent-1":
			if r.Method != http.MethodGet || r.URL.Query().Get("format") != "raw" {
				t.Fatalf("sent verification request = %s %s", r.Method, r.URL.String())
			}
			mu.Lock()
			raw := append([]byte(nil), uploaded...)
			mu.Unlock()
			writeJSON(w, map[string]any{
				"id":       "sent-1",
				"threadId": "thread-sent-1",
				"labelIds": []string{"SENT"},
				"raw":      base64.RawURLEncoding.EncodeToString(raw),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	prepared, err := DirectSendPreparer{}.PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:     "work",
		Provider:    "gmail",
		Email:       "work@example.test",
		DisplayName: "Work",
	}, outboundTextMessage())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	outcome, err := testClient(server).SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.MessageID != "sent-1" || outcome.WriteOutcomeFacts == nil {
		t.Fatalf("direct send outcome = %#v", outcome)
	}
	mu.Lock()
	raw := string(uploaded)
	mu.Unlock()
	if !strings.Contains(raw, `From: "Work" <work@example.test>`) || !strings.Contains(raw, "Subject: Test Subject") || !strings.Contains(raw, "Hello from Gmail") {
		t.Fatalf("sent raw MIME = %q", raw)
	}
}

func TestGmailWriteStatusClassification(t *testing.T) {
	client := &Client{account: "work"}
	tests := []struct {
		status      int
		body        []byte
		wantCode    string
		wantOutcome errs.WriteOutcome
	}{
		{status: http.StatusBadRequest, wantCode: errs.CodeInvalidComposeInput, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusUnauthorized, wantCode: errs.CodeAuthRepairRequired, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusForbidden, body: []byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`), wantCode: errs.CodeProviderRateLimited, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusNotFound, wantCode: errs.CodeDraftNotFound, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusConflict, wantCode: errs.CodeProviderConflict, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusRequestEntityTooLarge, wantCode: errs.CodeMessageTooLarge, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusTooManyRequests, wantCode: errs.CodeProviderRateLimited, wantOutcome: errs.WriteOutcomeRejected},
		{status: http.StatusInternalServerError, wantCode: errs.CodeVerificationFailed, wantOutcome: errs.WriteOutcomeUnknown},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			attempt, err := errs.NewProviderWriteAttempt("work", errs.ProviderGmail, errs.WriteGmailDraftUpdate)
			if err != nil {
				t.Fatal(err)
			}
			if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
				t.Fatal(err)
			}
			err = client.writeStatusError(attempt, errs.WriteGmailDraftUpdate, "/drafts/draft-1", "draft-1", tt.status, tt.body)
			assertProductCode(t, err, tt.wantCode)
			facts, ok := errs.WriteOutcomeFactsFromError(err)
			if !ok || facts.Outcome != tt.wantOutcome {
				t.Fatalf("write facts = %#v ok=%v", facts, ok)
			}
			if tt.wantOutcome == errs.WriteOutcomeUnknown && facts.AcceptedByProvider {
				t.Fatalf("unknown status accepted facts = %#v", facts)
			}
		})
	}
}

func TestReplySendReturnsKnownPartialWhenPhaseTwoIsRejected(t *testing.T) {
	var mu sync.Mutex
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/gmail/v1/users/me/drafts":
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "reply-draft", "message": map[string]any{"id": "reply-msg", "threadId": "source-thread", "labelIds": []string{"DRAFT"}}})
		case "/gmail/v1/users/me/drafts/reply-draft":
			mu.Lock()
			raw := append([]byte(nil), uploaded...)
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "reply-draft", "message": map[string]any{"id": "reply-msg", "threadId": "source-thread", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw)}})
		case "/upload/gmail/v1/users/me/drafts/send":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": "notFound"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source, err := provider.NewComposeSourceSnapshotForTest("work", "gmail", provider.ComposeSourceMaterial{
		SourceID:          "src1",
		InternetMessageID: "<src1@example.test>",
		ThreadID:          "source-thread",
		From:              mail.Recipient{Address: "sender@example.test"},
		To:                []mail.Recipient{{Address: "work@example.test"}},
		Subject:           "Source",
		Text:              []byte("source body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Destroy()
	body := mail.OutboundBody{Format: mail.BodyFormatText, ContentType: "text", Text: "reply body", Bytes: len("reply body"), Present: true}
	result, err := testClient(server).CreateReplyDraft(context.Background(), provider.ReplyDraftRequest{
		Mode:     mail.ReplyModeReply,
		Quote:    mail.QuoteModeNone,
		Language: mail.QuoteLanguageEnglish,
		Action:   mail.ComposeExecutionSend,
		Body:     body,
		Source:   source,
		Check:    func(mail.SendContent) error { return nil },
	})
	if err == nil {
		t.Fatal("reply send succeeded; want known partial")
	}
	if result.DraftID != "reply-draft" {
		t.Fatalf("draft id = %q", result.DraftID)
	}
	assertProductCode(t, err, errs.CodeDraftNotFound)
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeKnownPartial || !facts.WriteMayHaveLanded || !facts.AcceptedByProvider || facts.Succeeded {
		t.Fatalf("known partial facts = %#v ok=%v", facts, ok)
	}
}

func TestUpdateDraftPreservesThreadWhenSubjectUnchanged(t *testing.T) {
	existing := []byte("From: Work <work@example.test>\r\nTo: Client <client@example.test>\r\nSubject: Stable\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nold body")
	var mu sync.Mutex
	var uploaded []byte
	var uploadMeta map[string]any
	var getCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected method %s", r.Method)
			}
			mu.Lock()
			getCount++
			count := getCount
			raw := append([]byte(nil), uploaded...)
			mu.Unlock()
			messageID := "old-msg"
			if count == 1 {
				raw = existing
			} else {
				messageID = "new-msg"
			}
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": messageID, "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw)}})
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodPut {
				t.Fatalf("unexpected upload method %s", r.Method)
			}
			meta, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			uploadMeta = meta
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "new-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ref, err := testClient(server).UpdateDraft(context.Background(), mail.UpdateDraftRequest{
		DraftID: "draft-1",
		Patch: mail.DraftPatch{Body: mail.BodyPatch{Set: true, Body: mail.OutboundBody{
			Format: mail.BodyFormatText, ContentType: "text", Text: "new body", Bytes: len("new body"), Present: true,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "draft-1" || ref.WriteOutcomeFacts == nil {
		t.Fatalf("update ref = %#v", ref)
	}
	mu.Lock()
	meta := uploadMeta
	raw := string(uploaded)
	mu.Unlock()
	message, ok := meta["message"].(map[string]any)
	if !ok || message["threadId"] != "thread-1" {
		t.Fatalf("upload metadata = %#v", meta)
	}
	if !strings.Contains(raw, "Subject: Stable") || !strings.Contains(raw, "new body") {
		t.Fatalf("uploaded raw = %q", raw)
	}
}

func TestDraftAttachmentAddUsesStructuralReadAndSingleReplacement(t *testing.T) {
	oldBytes := []byte("old")
	newBytes := []byte("new")
	oldRaw := draftAttachmentRawMIME(t, []mimecompose.BinaryPart{{
		Role:        mimecompose.PartRoleAttachment,
		ContentType: "text/plain",
		Disposition: "attachment",
		Filename:    "old.txt",
		Bytes:       oldBytes,
	}})
	var mu sync.Mutex
	var uploaded []byte
	var fullReads int
	var putUploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			switch r.URL.Query().Get("format") {
			case "full":
				if r.URL.Query().Get("fields") != "id,message(id,threadId,labelIds,payload)" {
					t.Fatalf("full draft fields = %q", r.URL.Query().Get("fields"))
				}
				mu.Lock()
				fullReads++
				count := fullReads
				mu.Unlock()
				if count == 1 {
					writeJSON(w, draftStructure("draft-1", "old-msg", "thread-1", []testAttachmentPart{{
						PartID: "1", Name: "old.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: oldBytes,
					}}))
					return
				}
				writeJSON(w, draftStructure("draft-1", "new-msg", "thread-1", []testAttachmentPart{
					{PartID: "1", Name: "old.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: oldBytes},
					{PartID: "2", Name: "new.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: newBytes},
				}))
			case "raw":
				mu.Lock()
				raw := append([]byte(nil), uploaded...)
				mu.Unlock()
				messageID := "new-msg"
				if len(raw) == 0 {
					raw = oldRaw
					messageID = "old-msg"
				}
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
					"id": messageID, "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw),
				}})
			default:
				t.Fatalf("draft format = %q", r.URL.Query().Get("format"))
			}
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodPut || r.URL.Query().Get("uploadType") != "multipart" {
				t.Fatalf("upload request = %s %s", r.Method, r.URL.String())
			}
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			putUploads++
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "new-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := testClient(server).AddDraftAttachments(context.Background(), mail.AddDraftAttachmentsRequest{
		DraftID: "draft-1",
		Uploads: []mail.AttachmentUpload{{
			Name: "new.txt", ContentType: "text/plain", Size: int64(len(newBytes)), Bytes: append([]byte(nil), newBytes...),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WriteOutcomeFacts == nil || len(result.Added) != 1 || result.Added[0].ID != "2" || result.Added[0].ExecutionToken != nil {
		t.Fatalf("add result = %#v", result)
	}
	if len(result.FinalAttachments) != 2 || result.FinalAttachments[1].Name != "new.txt" || result.FinalAttachments[1].ExecutionToken != nil {
		t.Fatalf("final attachments = %#v", result.FinalAttachments)
	}
	mu.Lock()
	raw := string(uploaded)
	puts := putUploads
	reads := fullReads
	mu.Unlock()
	if puts != 1 || reads != 2 {
		t.Fatalf("put uploads = %d full reads = %d", puts, reads)
	}
	if !strings.Contains(raw, "old.txt") || !strings.Contains(raw, "new.txt") {
		t.Fatalf("uploaded raw = %q", raw)
	}
}

func TestDraftAttachmentAddRejectsUnchangedContainedMessageID(t *testing.T) {
	oldRaw := draftAttachmentRawMIME(t, nil)
	newBytes := []byte("new")
	var mu sync.Mutex
	var fullReads int
	var rawReads int
	var putUploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			switch r.URL.Query().Get("format") {
			case "full":
				mu.Lock()
				fullReads++
				count := fullReads
				mu.Unlock()
				if count != 1 {
					t.Fatalf("unexpected post-write full read %d", count)
				}
				writeJSON(w, draftStructure("draft-1", "old-msg", "thread-1", nil))
			case "raw":
				mu.Lock()
				rawReads++
				count := rawReads
				mu.Unlock()
				if count != 1 {
					t.Fatalf("unexpected post-write raw read %d", count)
				}
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
					"id": "old-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(oldRaw),
				}})
			default:
				t.Fatalf("draft format = %q", r.URL.Query().Get("format"))
			}
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodPut {
				t.Fatalf("upload method = %s", r.Method)
			}
			_, _ = readGmailMultipartUpload(t, r)
			mu.Lock()
			putUploads++
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "old-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testClient(server).AddDraftAttachments(context.Background(), mail.AddDraftAttachmentsRequest{
		DraftID: "draft-1",
		Uploads: []mail.AttachmentUpload{{
			Name: "new.txt", ContentType: "text/plain", Size: int64(len(newBytes)), Bytes: append([]byte(nil), newBytes...),
		}},
	})
	assertProductCode(t, err, errs.CodeVerificationFailed)
	assertProductReason(t, err, string(errs.OutcomeProviderResultMalformed))
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeUnknown || !facts.WriteMayHaveLanded || !facts.AcceptedByProvider {
		t.Fatalf("write outcome facts = %#v, %v", facts, ok)
	}
	mu.Lock()
	reads, raws, puts := fullReads, rawReads, putUploads
	mu.Unlock()
	if reads != 1 || raws != 1 || puts != 1 {
		t.Fatalf("full reads = %d raw reads = %d uploads = %d", reads, raws, puts)
	}
}

func TestDraftAttachmentAddProofMismatchIsPostWriteMismatch(t *testing.T) {
	oldRaw := draftAttachmentRawMIME(t, nil)
	newBytes := []byte("new")
	var mu sync.Mutex
	var uploaded []byte
	var fullReads int
	var rawReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			switch r.URL.Query().Get("format") {
			case "full":
				mu.Lock()
				fullReads++
				count := fullReads
				mu.Unlock()
				if count == 1 {
					writeJSON(w, draftStructure("draft-1", "old-msg", "thread-1", nil))
					return
				}
				writeJSON(w, draftStructure("draft-1", "new-msg", "thread-1", []testAttachmentPart{{
					PartID: "1", Name: "new.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: []byte("bad"),
				}}))
			case "raw":
				mu.Lock()
				rawReads++
				count := rawReads
				raw := append([]byte(nil), uploaded...)
				mu.Unlock()
				messageID := "new-msg"
				if count == 1 {
					raw = oldRaw
					messageID = "old-msg"
				}
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
					"id": messageID, "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw),
				}})
			default:
				t.Fatalf("draft format = %q", r.URL.Query().Get("format"))
			}
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodPut {
				t.Fatalf("upload method = %s", r.Method)
			}
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "new-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testClient(server).AddDraftAttachments(context.Background(), mail.AddDraftAttachmentsRequest{
		DraftID: "draft-1",
		Uploads: []mail.AttachmentUpload{{
			Name: "new.txt", ContentType: "text/plain", Size: int64(len(newBytes)), Bytes: append([]byte(nil), newBytes...),
		}},
	})
	assertProductCode(t, err, errs.CodeVerificationFailed)
	assertProductReason(t, err, string(errs.OutcomePostWriteMismatch))
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeUnknown || !facts.WriteMayHaveLanded || !facts.AcceptedByProvider {
		t.Fatalf("write outcome facts = %#v, %v", facts, ok)
	}
}

func TestDraftAttachmentRemoveConsumesTokenAndUsesSingleReplacement(t *testing.T) {
	oldBytes := []byte("old")
	oldRaw := draftAttachmentRawMIME(t, []mimecompose.BinaryPart{{
		Role:        mimecompose.PartRoleAttachment,
		ContentType: "text/plain",
		Disposition: "attachment",
		Filename:    "old.txt",
		Bytes:       oldBytes,
	}})
	var mu sync.Mutex
	var uploaded []byte
	var fullReads int
	var putUploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			switch r.URL.Query().Get("format") {
			case "full":
				if r.URL.Query().Get("fields") != "id,message(id,threadId,labelIds,payload)" {
					t.Fatalf("full draft fields = %q", r.URL.Query().Get("fields"))
				}
				mu.Lock()
				fullReads++
				count := fullReads
				mu.Unlock()
				if count <= 2 {
					writeJSON(w, draftStructure("draft-1", "old-msg", "thread-1", []testAttachmentPart{{
						PartID: "1", Name: "old.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: oldBytes,
					}}))
					return
				}
				writeJSON(w, draftStructure("draft-1", "new-msg", "thread-1", nil))
			case "raw":
				mu.Lock()
				raw := append([]byte(nil), uploaded...)
				mu.Unlock()
				messageID := "new-msg"
				if len(raw) == 0 {
					raw = oldRaw
					messageID = "old-msg"
				}
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
					"id": messageID, "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw),
				}})
			default:
				t.Fatalf("draft format = %q", r.URL.Query().Get("format"))
			}
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			if r.Method != http.MethodPut {
				t.Fatalf("upload method = %s", r.Method)
			}
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			putUploads++
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "new-msg", "threadId": "thread-1", "labelIds": []string{"DRAFT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	draft, err := client.ReadDraft(context.Background(), mail.ReadDraftRequest{DraftID: "draft-1", MetadataOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Attachments) != 1 || draft.Attachments[0].ExecutionToken == nil {
		t.Fatalf("draft attachments = %#v", draft.Attachments)
	}
	requestToken := draft.Attachments[0].ExecutionToken.Clone()
	defer draft.Attachments[0].ExecutionToken.Destroy()
	defer requestToken.Destroy()
	result, err := client.RemoveDraftAttachment(context.Background(), mail.RemoveDraftAttachmentRequest{
		DraftID: "draft-1", AttachmentID: "1", ExecutionToken: requestToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WriteOutcomeFacts == nil || result.Removed.ID != "1" || result.Removed.ExecutionToken != nil || len(result.FinalAttachments) != 0 {
		t.Fatalf("remove result = %#v", result)
	}
	mu.Lock()
	raw := string(uploaded)
	puts := putUploads
	reads := fullReads
	mu.Unlock()
	if puts != 1 || reads != 3 {
		t.Fatalf("put uploads = %d full reads = %d", puts, reads)
	}
	if strings.Contains(raw, "old.txt") {
		t.Fatalf("uploaded raw still contains removed attachment: %q", raw)
	}
	_, err = client.RemoveDraftAttachment(context.Background(), mail.RemoveDraftAttachmentRequest{
		DraftID: "draft-1", AttachmentID: "1", ExecutionToken: requestToken,
	})
	assertProductCode(t, err, errs.CodeSharedProviderContractInvalid)
}

func TestDraftAttachmentRemoveRejectsTokenFromDifferentClient(t *testing.T) {
	oldBytes := []byte("old")
	var mu sync.Mutex
	var fullReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			if r.URL.Query().Get("format") != "full" {
				t.Fatalf("unexpected draft format = %q", r.URL.Query().Get("format"))
			}
			mu.Lock()
			fullReads++
			count := fullReads
			mu.Unlock()
			if count != 1 {
				t.Fatalf("unexpected draft read after cross-client token use: %d", count)
			}
			writeJSON(w, draftStructure("draft-1", "old-msg", "thread-1", []testAttachmentPart{{
				PartID: "1", Name: "old.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: oldBytes,
			}}))
		case "/upload/gmail/v1/users/me/drafts/draft-1":
			t.Fatal("cross-client token use reached upload")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	first := testClient(server)
	second := testClient(server)
	draft, err := first.ReadDraft(context.Background(), mail.ReadDraftRequest{DraftID: "draft-1", MetadataOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Attachments) != 1 || draft.Attachments[0].ExecutionToken == nil {
		t.Fatalf("draft attachments = %#v", draft.Attachments)
	}
	requestToken := draft.Attachments[0].ExecutionToken.Clone()
	if requestToken == nil {
		t.Fatal("cloned execution token is nil")
	}
	defer draft.Attachments[0].ExecutionToken.Destroy()
	defer requestToken.Destroy()

	_, err = second.RemoveDraftAttachment(context.Background(), mail.RemoveDraftAttachmentRequest{
		DraftID: "draft-1", AttachmentID: "1", ExecutionToken: requestToken,
	})
	assertProductCode(t, err, errs.CodeSharedProviderContractInvalid)
	mu.Lock()
	reads := fullReads
	mu.Unlock()
	if reads != 1 {
		t.Fatalf("full reads = %d", reads)
	}
}

func TestForwardSelectedAttachmentsRetainsCIDDependenciesInSingleDraftUpload(t *testing.T) {
	fileBytes := []byte("file")
	logoBytes := []byte("logo")
	var mu sync.Mutex
	var uploaded []byte
	var sourceReads int
	var draftUploads int
	var draftFullReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/source-1":
			if r.URL.Query().Get("format") != "full" || r.URL.Query().Get("fields") != "id,threadId,payload" {
				t.Fatalf("source structure query = %s", r.URL.RawQuery)
			}
			mu.Lock()
			sourceReads++
			mu.Unlock()
			writeJSON(w, messageStructure("source-1", "source-thread", []testAttachmentPart{
				{PartID: "1", Name: "file.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: fileBytes},
				{PartID: "2", ContentType: "image/png", Disposition: "inline", ContentID: "logo@example.test", Bytes: logoBytes},
			}))
		case "/upload/gmail/v1/users/me/drafts":
			if r.Method != http.MethodPost {
				t.Fatalf("upload method = %s", r.Method)
			}
			_, raw := readGmailMultipartUpload(t, r)
			mu.Lock()
			draftUploads++
			uploaded = raw
			mu.Unlock()
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "draft-msg", "threadId": "draft-thread", "labelIds": []string{"DRAFT"}}})
		case "/gmail/v1/users/me/drafts/draft-1":
			switch r.URL.Query().Get("format") {
			case "raw":
				mu.Lock()
				raw := append([]byte(nil), uploaded...)
				mu.Unlock()
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
					"id": "draft-msg", "threadId": "draft-thread", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(raw),
				}})
			case "full":
				mu.Lock()
				draftFullReads++
				mu.Unlock()
				writeJSON(w, draftStructure("draft-1", "draft-msg", "draft-thread", []testAttachmentPart{
					{PartID: "1", ContentType: "image/png", Disposition: "inline", ContentID: "logo@example.test", Bytes: logoBytes},
					{PartID: "2", Name: "file.txt", ContentType: "text/plain", Disposition: "attachment", Bytes: fileBytes},
				}))
			default:
				t.Fatalf("draft format = %q", r.URL.Query().Get("format"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source, err := provider.NewComposeSourceSnapshotForTest("work", "gmail", provider.ComposeSourceMaterial{
		SourceID: "source-1", ThreadID: "source-thread", From: mail.Recipient{Address: "sender@example.test"}, Subject: "Source",
		Text: []byte("source text"), HTML: []byte(`<p><img src="cid:logo@example.test"></p>`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Destroy()

	result, err := testClient(server).CreateForwardDraft(context.Background(), provider.ForwardDraftRequest{
		To: []mail.Recipient{{Address: "client@example.test"}},
		Body: mail.OutboundBody{
			Format: mail.BodyFormatHTML, ContentType: "html", Text: "note", HTML: `<p>note</p><img src="cid:logo@example.test">`, Bytes: 1, Present: true,
		},
		Action:      mail.ComposeExecutionDraft,
		Attachments: mail.ForwardAttachmentSelection{Mode: mail.ForwardAttachmentsSelected, Names: []string{"file.txt"}},
		Source:      source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WriteOutcomeFacts == nil || result.DraftID != "draft-1" || result.AttachmentCount != 2 {
		t.Fatalf("forward result = %#v", result)
	}
	if len(result.SelectedSourceAttachments) != 1 || result.SelectedSourceAttachments[0].Name != "file.txt" || result.SelectedSourceAttachments[0].IsInline {
		t.Fatalf("selected source attachments = %#v", result.SelectedSourceAttachments)
	}
	if len(result.FinalAttachments) != 2 || !result.FinalAttachments[0].IsInline || result.FinalAttachments[1].Name != "file.txt" {
		t.Fatalf("final attachments = %#v", result.FinalAttachments)
	}
	mu.Lock()
	raw := string(uploaded)
	sourceCount := sourceReads
	uploadCount := draftUploads
	fullCount := draftFullReads
	mu.Unlock()
	if sourceCount != 1 || uploadCount != 1 || fullCount != 1 {
		t.Fatalf("source reads = %d uploads = %d draft full reads = %d", sourceCount, uploadCount, fullCount)
	}
	if !strings.Contains(raw, "file.txt") || !strings.Contains(raw, "Content-ID: <logo@example.test>") {
		t.Fatalf("uploaded raw = %q", raw)
	}
}

func TestForwardDefaultNoneRejectsRequiredCIDAttachments(t *testing.T) {
	source, err := provider.NewComposeSourceSnapshotForTest("work", "gmail", provider.ComposeSourceMaterial{
		SourceID: "source-1", ThreadID: "source-thread", From: mail.Recipient{Address: "sender@example.test"}, Subject: "Source",
		Text: []byte("source text"), HTML: []byte(`<p><img src="cid:logo@example.test"></p>`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Destroy()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	_, err = testClient(server).ValidateForwardDraft(context.Background(), provider.ForwardDraftRequest{
		Body: mail.OutboundBody{
			Format: mail.BodyFormatHTML, ContentType: "html", Text: "note", HTML: `<p>note</p><img src="cid:logo@example.test">`, Bytes: 1, Present: true,
		},
		Action:      mail.ComposeExecutionDraft,
		Attachments: mail.ForwardAttachmentSelection{Mode: mail.ForwardAttachmentsNone},
		Source:      source,
	})
	assertProductCode(t, err, errs.CodeInvalidComposeInput)
	assertProductReason(t, err, "required_cid_attachment")
}

func TestForwardDefaultNoneIgnoresUserAuthoredCIDWithoutSourceReference(t *testing.T) {
	source, err := provider.NewComposeSourceSnapshotForTest("work", "gmail", provider.ComposeSourceMaterial{
		SourceID: "source-1", ThreadID: "source-thread", From: mail.Recipient{Address: "sender@example.test"}, Subject: "Source",
		Text: []byte("source text"), HTML: []byte(`<p>forwarded html</p>`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Destroy()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, err := testClient(server).ValidateForwardDraft(context.Background(), provider.ForwardDraftRequest{
		To: []mail.Recipient{{Address: "client@example.test"}},
		Body: mail.OutboundBody{
			Format: mail.BodyFormatHTML, ContentType: "html", Text: "note", HTML: `<p>note</p><img src="cid:user@example.test">`, Bytes: 1, Present: true,
		},
		Action:      mail.ComposeExecutionDraft,
		Attachments: mail.ForwardAttachmentSelection{Mode: mail.ForwardAttachmentsNone},
		Source:      source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AttachmentCount != 0 || len(result.SelectedSourceAttachments) != 0 {
		t.Fatalf("forward validation result = %#v", result)
	}
	if calls != 0 {
		t.Fatalf("unexpected provider calls = %d", calls)
	}
}

func TestForwardRejectsKnownAggregateAttachmentSizeBeforeFetch(t *testing.T) {
	var mu sync.Mutex
	var structureReads int
	var attachmentReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/source-1":
			if r.URL.Query().Get("format") != "full" || r.URL.Query().Get("fields") != "id,threadId,payload" {
				t.Fatalf("source structure query = %s", r.URL.RawQuery)
			}
			mu.Lock()
			structureReads++
			mu.Unlock()
			writeJSON(w, messageExternalAttachmentStructure("source-1", "source-thread", []testExternalAttachmentPart{
				{PartID: "1", Name: "large.bin", ContentType: "application/octet-stream", Disposition: "attachment", AttachmentID: "ext-1", Size: mail.MaxOutboundEncodedMessageBytes},
				{PartID: "2", Name: "extra.bin", ContentType: "application/octet-stream", Disposition: "attachment", AttachmentID: "ext-2", Size: 1},
			}))
		case "/gmail/v1/users/me/messages/source-1/attachments/ext-1", "/gmail/v1/users/me/messages/source-1/attachments/ext-2":
			mu.Lock()
			attachmentReads++
			mu.Unlock()
			http.Error(w, "attachment content should not be fetched", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source, err := provider.NewComposeSourceSnapshotForTest("work", "gmail", provider.ComposeSourceMaterial{
		SourceID: "source-1", ThreadID: "source-thread", From: mail.Recipient{Address: "sender@example.test"}, Subject: "Source",
		Text: []byte("source text"), HTML: []byte(`<p>forwarded html</p>`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Destroy()

	_, err = testClient(server).ValidateForwardDraft(context.Background(), provider.ForwardDraftRequest{
		To: []mail.Recipient{{Address: "client@example.test"}},
		Body: mail.OutboundBody{
			Format: mail.BodyFormatText, ContentType: "text", Text: "note", Bytes: 4, Present: true,
		},
		Action:      mail.ComposeExecutionDraft,
		Attachments: mail.ForwardAttachmentSelection{Mode: mail.ForwardAttachmentsAll},
		Source:      source,
	})
	assertProductCode(t, err, errs.CodeMessageTooLarge)
	var product *errs.Error
	if !errors.As(err, &product) || product.Details()["observed_at_least"] != uint64(mail.MaxOutboundEncodedMessageBytes+1) {
		t.Fatalf("message size details = %#v", product)
	}
	mu.Lock()
	structures, attachments := structureReads, attachmentReads
	mu.Unlock()
	if structures != 1 || attachments != 0 {
		t.Fatalf("structure reads = %d attachment reads = %d", structures, attachments)
	}
}

func TestReadDraftRawUsesPublicBodyLimit(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), int(mail.MaxMessageBodyBytes)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/drafts/draft-1":
			if r.URL.Query().Get("format") == "raw" {
				writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{"id": "msg-1", "threadId": "thread-1", "labelIds": []string{"DRAFT"}, "raw": base64.RawURLEncoding.EncodeToString(oversized)}})
				return
			}
			writeJSON(w, map[string]any{"id": "draft-1", "message": map[string]any{
				"id":           "msg-1",
				"threadId":     "thread-1",
				"labelIds":     []string{"DRAFT"},
				"internalDate": "1777046400000",
				"payload": map[string]any{
					"mimeType": "text/plain",
					"headers": []map[string]string{
						{"name": "Message-ID", "value": "<draft@example.test>"},
						{"name": "From", "value": "Work <work@example.test>"},
						{"name": "To", "value": "Client <client@example.test>"},
						{"name": "Subject", "value": "Draft"},
					},
					"body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("small")), "size": 5},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testClient(server).ReadDraft(context.Background(), mail.ReadDraftRequest{DraftID: "draft-1", BodyMode: mail.BodyModeRaw})
	assertProductCode(t, err, errs.CodeReadLimitExceeded)
}

func TestSetupParserResolve(t *testing.T) {
	parser := SetupParser{}
	ctx := context.Background()

	res, err := parser.Resolve(ctx, provider.SetupResolveContext{
		Existing: maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{ClientID: "stored-client"}},
		LookupEnv: func(name string) (string, bool, error) {
			if name == envClientID {
				return "env-client", true, nil
			}
			return "", false, nil
		},
		Flags: setupFlags{"gmail-client-id": {Changed: true, Value: "flag-client"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Setup.Gmail == nil || res.Setup.Gmail.ClientID != "flag-client" {
		t.Fatalf("client id = %#v", res.Setup.Gmail)
	}
	if previewValue(res.Preview, "client_id_source") != "flag" || previewValue(res.Preview, "client_id_configured") != "true" {
		t.Fatalf("preview = %#v", res.Preview)
	}

	res, err = parser.Resolve(ctx, provider.SetupResolveContext{
		Existing: maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{ClientID: "stored-client"}},
		LookupEnv: func(name string) (string, bool, error) {
			if name == envClientID {
				return "env-client", true, nil
			}
			return "", false, nil
		},
		Flags: setupFlags{"gmail-client-id": {Changed: false, Value: ""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Setup.Gmail == nil || res.Setup.Gmail.ClientID != "env-client" || previewValue(res.Preview, "client_id_source") != "environment" {
		t.Fatalf("env resolution = %#v preview=%#v", res.Setup.Gmail, res.Preview)
	}

	_, err = parser.Resolve(ctx, provider.SetupResolveContext{Flags: setupFlags{}})
	assertProductCode(t, err, errs.CodeAuthAppConfigMissing)

	_, err = parser.Resolve(ctx, provider.SetupResolveContext{
		Flags: setupFlags{"gmail-client-id": {Changed: true, Value: " bad"}},
	})
	if err == nil {
		t.Fatal("whitespace-padded client id accepted")
	}
}

func TestCredentialCodecClosedShapeAndRotation(t *testing.T) {
	codec := CredentialCodec{}
	binding := credentialBinding("work")
	cache := validCache("work", "work@example.test")

	raw, err := codec.Encode(binding, cache)
	if err != nil {
		t.Fatal(err)
	}
	reg := &secretRecorder{}
	decoded, err := codec.Decode(reg, binding, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Destroy()
	if !sameStrings(reg.values, []string{
		"gmail-client-secret",
		"gmail-access-token",
		"gmail-refresh-token",
		"Bearer gmail-access-token",
	}) {
		t.Fatalf("registered secrets = %#v", reg.values)
	}

	if _, err := codec.Decode(nil, binding, []byte(`{"version":1,"version":1}`)); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("duplicate top-level key error = %v", err)
	}
	unknown := strings.Replace(string(raw), "\n}", ",\n  \"future\": true\n}", 1)
	if _, err := codec.Decode(nil, binding, []byte(unknown)); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("unknown field error = %v", err)
	}

	rotated := validCache("work", "work@example.test")
	rotated.SetRefreshToken("rotated-refresh-token")
	ok, err := codec.RefreshTokenRotated(binding, cache, rotated)
	if err != nil || !ok {
		t.Fatalf("rotated = %t, %v", ok, err)
	}
	status, err := codec.LocalHealth(maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{ClientID: "other-client"}}, binding, cache)
	if err != nil || status != provider.LocalRepairRequired {
		t.Fatalf("local health = %v, %v", status, err)
	}
}

func TestOAuthEndpointInjectionUsesLoopbackOnly(t *testing.T) {
	var tokenCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls++
		if r.Method != http.MethodPost {
			t.Fatalf("token method = %s", r.Method)
		}
		writeJSON(w, map[string]any{
			"access_token":  "oauth-access-token",
			"refresh_token": "oauth-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
			"scope":         requiredScope,
		})
	}))
	defer server.Close()

	p := New(Options{
		AuthorizeURL: server.URL + "/authorize",
		TokenURL:     server.URL + "/token",
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
	})
	authURL, err := p.authorizeURL("work", "client-id", "http://127.0.0.1:4567/", "state-value", "verifier-value")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "http" || parsed.Host != strings.TrimPrefix(server.URL, "http://") || parsed.Path != "/authorize" ||
		parsed.Query().Get("client_id") != "client-id" || parsed.Query().Get("scope") != requiredScope {
		t.Fatalf("authorize URL = %s", authURL)
	}
	token, err := p.exchangeToken(context.Background(), "work", url.Values{"grant_type": {"authorization_code"}}, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "oauth-access-token" || token.RefreshToken != "oauth-refresh-token" || token.TokenType != "Bearer" ||
		!sameStrings(token.Scopes, []string{requiredScope}) || tokenCalls != 1 {
		t.Fatalf("token = %#v calls=%d", token, tokenCalls)
	}

	bad := New(Options{AuthorizeURL: "https://example.com/authorize"})
	if _, err := bad.authorizeURL("work", "client-id", "http://127.0.0.1:4567/", "state-value", "verifier-value"); err == nil {
		t.Fatal("non-loopback custom authorize endpoint accepted")
	}
}

func TestHTTPNotFoundMappingIsResourceSpecific(t *testing.T) {
	assertProductCode(t, classifyHTTPStatus("work", "/labels/Label_1", http.StatusNotFound, nil, false), errs.CodeFolderNotFound)
	assertProductCode(t, classifyHTTPStatus("work", "/threads/t1", http.StatusGone, nil, false), errs.CodeThreadNotFound)
	assertProductCode(t, classifyHTTPStatus("work", "/messages/m1", http.StatusNotFound, nil, false), errs.CodeMessageNotFound)
}

func TestListSearchReadAndThreadAgainstAPI(t *testing.T) {
	api := newGmailAPIFixture(t)
	defer api.server.Close()
	client := &Client{
		account:     "work",
		email:       "work@example.test",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  api.server.URL + "/gmail/v1/users/me",
		http:        api.server.Client(),
	}

	folders, err := client.ListFolders(context.Background(), mail.FolderListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := folderIDs(folders.Folders); !reflect.DeepEqual(got, []string{"INBOX", "Label_1", "Label_parent", "STARRED", "UNREAD"}) {
		t.Fatalf("folder ids = %#v", got)
	}
	var project mail.Folder
	for _, f := range folders.Folders {
		if f.ID == "Label_1" {
			project = f
		}
	}
	if project.DisplayName != "Project" || project.Path != "Work/Project" || project.ParentID != "Label_parent" ||
		project.ProviderMeta == nil || project.ProviderMeta.LabelType != mail.LabelTypeUser {
		t.Fatalf("project folder = %#v", project)
	}

	search, err := client.SearchMessages(context.Background(), mail.SearchRequest{
		Query: mail.Query{From: []string{"sender@example.test"}, Terms: []string{"invoice"}},
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := messageSummaryIDs(search.Messages); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("search ids = %#v", got)
	}
	if !strings.Contains(api.lastMessageListQuery(), `from:"sender@example.test" "invoice"`) {
		t.Fatalf("gmail query = %q", api.lastMessageListQuery())
	}

	msg, err := client.ReadMessage(context.Background(), mail.ReadMessageRequest{
		Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: "m1"},
		BodyMode: mail.BodyModeReadable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != "m1" || msg.ThreadID != "t1" || msg.Subject != "Quarterly invoice" ||
		msg.Body.Content != "Plain body" || msg.ProviderMeta == nil || msg.ProviderMeta.Starred == nil || !*msg.ProviderMeta.Starred ||
		len(msg.Attachments) != 1 || msg.Attachments[0].ProviderMeta == nil || msg.Attachments[0].ProviderMeta.ExternalAttachmentIDPresent == nil {
		t.Fatalf("message = %#v", msg)
	}

	thread, err := client.ReadThread(context.Background(), mail.ReadThreadRequest{
		Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorThreadID, ThreadID: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if thread.ThreadID != "t1" || len(thread.Messages) != 1 || thread.ProviderMeta == nil ||
		thread.ProviderMeta.Basis != mail.ThreadBasisGmailNative || thread.ProviderMeta.Complete == nil || !*thread.ProviderMeta.Complete {
		t.Fatalf("thread = %#v", thread)
	}
}

func TestCopyMessageAddsUserLabelAndReportsChanged(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "Label_42", Name: "Project Alpha", Type: "user"},
	}, map[string][]string{"g-2": {"INBOX"}})
	defer api.server.Close()

	result, err := testClient(api.server).CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: "g-2",
		Folder: mail.Folder{
			Account:     "work",
			Provider:    "gmail",
			ID:          "Label_42",
			DisplayName: "Project Alpha",
			Path:        "Project Alpha",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != "g-2" || result.TargetFolder == nil || result.TargetFolder.ID != "Label_42" || result.Changed == nil || !*result.Changed {
		t.Fatalf("copy result = %#v", result)
	}
	if result.WriteOutcomeFacts == nil || result.WriteOutcomeFacts.Outcome != errs.WriteOutcomeSucceeded || !result.WriteOutcomeFacts.ConfirmedAfterWrite || !result.WriteOutcomeFacts.Succeeded {
		t.Fatalf("write facts = %#v", result.WriteOutcomeFacts)
	}
	if result.ProviderMeta == nil || result.ProviderMeta.FinalLabelIDs == nil || !sameStrings(*result.ProviderMeta.FinalLabelIDs, []string{"INBOX", "Label_42"}) {
		t.Fatalf("provider metadata = %#v", result.ProviderMeta)
	}
	if len(api.modifyRequests) != 1 {
		t.Fatalf("modify requests = %#v", api.modifyRequests)
	}
	if got := api.modifyRequests[0]; !reflect.DeepEqual(got.Add, []string{"Label_42"}) || len(got.Remove) != 0 || got.Fields != gmailMessageWriteFields {
		t.Fatalf("modify request = %#v", got)
	}
	if labels := api.messageLabels("g-2"); !sameStrings(labels, []string{"INBOX", "Label_42"}) {
		t.Fatalf("message labels = %#v", labels)
	}
}

func TestCopyMessageNoOpSkipsWriteAndReportsUnchanged(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "Label_42", Name: "Project Alpha", Type: "user"},
	}, map[string][]string{"g-2": {"INBOX", "Label_42"}})
	defer api.server.Close()

	result, err := testClient(api.server).CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: "g-2",
		Folder: mail.Folder{
			Account:     "work",
			Provider:    "gmail",
			ID:          "Label_42",
			DisplayName: "Project Alpha",
			Path:        "Project Alpha",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed == nil || *result.Changed {
		t.Fatalf("changed = %#v", result.Changed)
	}
	if result.WriteOutcomeFacts == nil || result.WriteOutcomeFacts.Outcome != errs.WriteOutcomeNoOp || !result.WriteOutcomeFacts.RetrySafe || !result.WriteOutcomeFacts.Succeeded {
		t.Fatalf("write facts = %#v", result.WriteOutcomeFacts)
	}
	if len(api.modifyRequests) != 0 {
		t.Fatalf("modify requests = %#v", api.modifyRequests)
	}
}

func TestCopyMessageFromTrashAddsLabelWithoutUntrash(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "TRASH", Name: "TRASH", Type: "system"},
		{ID: "Label_42", Name: "Project Alpha", Type: "user"},
	}, map[string][]string{"g-2": {"TRASH"}})
	defer api.server.Close()

	result, err := testClient(api.server).CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: "g-2",
		Folder: mail.Folder{
			Account:     "work",
			Provider:    "gmail",
			ID:          "Label_42",
			DisplayName: "Project Alpha",
			Path:        "Project Alpha",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed == nil || !*result.Changed {
		t.Fatalf("changed = %#v", result.Changed)
	}
	if len(api.modifyRequests) != 1 {
		t.Fatalf("modify requests = %#v", api.modifyRequests)
	}
	if got := api.modifyRequests[0]; !reflect.DeepEqual(got.Add, []string{"Label_42"}) || len(got.Remove) != 0 {
		t.Fatalf("modify request = %#v", got)
	}
	if labels := api.messageLabels("g-2"); !sameStrings(labels, []string{"Label_42", "TRASH"}) {
		t.Fatalf("message labels = %#v", labels)
	}
}

func TestDeleteAndMoveToTrashUseTrashEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(*Client) (mail.MessageMutationResult, error)
	}{
		{
			name: "delete",
			run: func(client *Client) (mail.MessageMutationResult, error) {
				return client.DeleteMessage(context.Background(), mail.MessageMutationRequest{MessageID: "g-trash"})
			},
		},
		{
			name: "move to trash",
			run: func(client *Client) (mail.MessageMutationResult, error) {
				return client.MoveMessage(context.Background(), mail.MessageMoveRequest{
					MessageID: "g-trash",
					Folder:    mail.Folder{Account: "work", Provider: "gmail", ID: "TRASH", DisplayName: "Trash", Path: "TRASH"},
				})
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newGmailMutationFixture(t, []gmailLabel{
				{ID: "INBOX", Name: "INBOX", Type: "system"},
				{ID: "TRASH", Name: "TRASH", Type: "system"},
			}, map[string][]string{"g-trash": {"INBOX"}})
			defer api.server.Close()

			result, err := tt.run(testClient(api.server))
			if err != nil {
				t.Fatal(err)
			}
			if result.Changed == nil || !*result.Changed || len(api.trashRequests) != 1 || len(api.modifyRequests) != 0 {
				t.Fatalf("result=%#v trash=%#v modify=%#v", result, api.trashRequests, api.modifyRequests)
			}
			if labels := api.messageLabels("g-trash"); !sameStrings(labels, []string{"TRASH"}) {
				t.Fatalf("message labels = %#v", labels)
			}
			// The message left the inbox, so an undo puts it back there.
			if result.SourceFolder == nil || result.SourceFolder.ID != "INBOX" {
				t.Fatalf("source folder = %#v", result.SourceFolder)
			}
		})
	}
}

func TestRestoreMessageUsesUntrashEndpoint(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "TRASH", Name: "TRASH", Type: "system"},
	}, map[string][]string{"g-restore": {"TRASH"}})
	defer api.server.Close()

	result, err := testClient(api.server).RestoreMessage(context.Background(), mail.MessageRestoreRequest{
		MessageID: "g-restore",
		Folder:    mail.Folder{Account: "work", Provider: "gmail", ID: "INBOX", DisplayName: "Inbox", Path: "INBOX"},
	})
	if err != nil {
		var product *errs.Error
		if errors.As(err, &product) {
			t.Fatalf("%v details=%#v untrash=%#v modify=%#v labels=%#v", err, product.ErrorDetails(), api.untrashRequests, api.modifyRequests, api.messageLabels("g-restore"))
		}
		t.Fatal(err)
	}
	if result.Changed == nil || !*result.Changed || len(api.untrashRequests) != 1 || len(api.modifyRequests) != 1 {
		t.Fatalf("result=%#v untrash=%#v modify=%#v", result, api.untrashRequests, api.modifyRequests)
	}
	if got := api.modifyRequests[0]; !reflect.DeepEqual(got.Add, []string{"INBOX"}) || len(got.Remove) != 0 {
		t.Fatalf("modify request = %#v", got)
	}
	if labels := api.messageLabels("g-restore"); !sameStrings(labels, []string{"INBOX"}) {
		t.Fatalf("message labels = %#v", labels)
	}
}

func TestClearNamedCategoriesPreservesFreshUnrelatedLabel(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "Label_A", Name: "Project/A", Type: "user"},
		{ID: "Label_B", Name: "Project/B", Type: "user"},
	}, map[string][]string{"g-3": {"INBOX", "Label_A", "Label_B"}})
	defer api.server.Close()

	req, err := mail.NewMessageCategorySetRequest("g-3", mail.CategoryMutationClearNamed, []string{}, []string{"Project/A", "Missing"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := testClient(api.server).SetMessageCategories(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Categories == nil {
		t.Fatal("categories change missing")
	}
	if !reflect.DeepEqual(result.Categories.Before, []string{"Project/A", "Project/B"}) ||
		!reflect.DeepEqual(result.Categories.After, []string{"Project/B"}) ||
		!reflect.DeepEqual(result.Categories.Removed, []string{"Project/A"}) ||
		!reflect.DeepEqual(result.Categories.Skipped, []string{"Missing"}) ||
		result.Categories.Count != 1 {
		t.Fatalf("categories = %#v", result.Categories)
	}
	if result.Changed == nil || !*result.Changed {
		t.Fatalf("changed = %#v", result.Changed)
	}
	if len(api.modifyRequests) != 1 {
		t.Fatalf("modify requests = %#v", api.modifyRequests)
	}
	if got := api.modifyRequests[0]; len(got.Add) != 0 || !reflect.DeepEqual(got.Remove, []string{"Label_A"}) {
		t.Fatalf("modify request = %#v", got)
	}
	if labels := api.messageLabels("g-3"); !sameStrings(labels, []string{"INBOX", "Label_B"}) {
		t.Fatalf("message labels = %#v", labels)
	}
	if result.ProviderMeta == nil || result.ProviderMeta.FinalLabelIDs == nil || !sameStrings(*result.ProviderMeta.FinalLabelIDs, []string{"INBOX", "Label_B"}) {
		t.Fatalf("provider metadata = %#v", result.ProviderMeta)
	}
}

func TestCategoriesSetSplitsModifyChunks(t *testing.T) {
	labels := []gmailLabel{{ID: "INBOX", Name: "INBOX", Type: "system"}}
	categories := make([]string, 101)
	for i := range categories {
		categories[i] = fmt.Sprintf("Project/%03d", i)
		labels = append(labels, gmailLabel{ID: fmt.Sprintf("Label_%03d", i), Name: categories[i], Type: "user"})
	}
	api := newGmailMutationFixture(t, labels, map[string][]string{"g-chunk": {"INBOX"}})
	defer api.server.Close()

	req, err := mail.NewMessageCategorySetRequest("g-chunk", mail.CategoryMutationSet, categories, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := testClient(api.server).SetMessageCategories(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed == nil || !*result.Changed || len(api.modifyRequests) != 2 {
		t.Fatalf("result=%#v modify=%#v", result, api.modifyRequests)
	}
	if len(api.modifyRequests[0].Add) != gmailModifyChunkSize || len(api.modifyRequests[1].Add) != 1 {
		t.Fatalf("modify chunks = %#v", api.modifyRequests)
	}
}

func TestCategoriesSetKnownPartialOnLaterModifyChunk(t *testing.T) {
	labels := []gmailLabel{{ID: "INBOX", Name: "INBOX", Type: "system"}}
	categories := make([]string, 101)
	for i := range categories {
		categories[i] = fmt.Sprintf("Project/%03d", i)
		labels = append(labels, gmailLabel{ID: fmt.Sprintf("Label_%03d", i), Name: categories[i], Type: "user"})
	}
	api := newGmailMutationFixture(t, labels, map[string][]string{"g-partial": {"INBOX"}})
	api.failModifyOrdinal = 2
	api.failModifyStatus = http.StatusTooManyRequests
	defer api.server.Close()

	req, err := mail.NewMessageCategorySetRequest("g-partial", mail.CategoryMutationSet, categories, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = testClient(api.server).SetMessageCategories(context.Background(), req)
	assertProductCode(t, err, errs.CodeProviderRateLimited)
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeKnownPartial || !facts.WriteMayHaveLanded || facts.RetrySafe || !facts.AcceptedByProvider {
		t.Fatalf("known partial facts = %#v, %v", facts, ok)
	}
	var product *errs.Error
	if !errors.As(err, &product) || product.Details()["compound_result"] != "known_partial" {
		t.Fatalf("details = %#v", product)
	}
	if len(api.modifyRequests) != 2 || len(api.modifyRequests[0].Add) != gmailModifyChunkSize || len(api.modifyRequests[1].Add) != 1 {
		t.Fatalf("modify requests = %#v", api.modifyRequests)
	}
}

func TestUnsupportedGmailSystemDestinationReturnsInvalidSelector(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "CATEGORY_PROMOTIONS", Name: "CATEGORY_PROMOTIONS", Type: "system"},
	}, map[string][]string{"g-system": {"INBOX"}})
	defer api.server.Close()

	_, err := testClient(api.server).CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: "g-system",
		Folder:    mail.Folder{Account: "work", Provider: "gmail", ID: "CATEGORY_PROMOTIONS", DisplayName: "Promotions", Path: "CATEGORY_PROMOTIONS"},
	})
	assertProductCode(t, err, errs.CodeInvalidSelector)
	assertProductReason(t, err, gmailSystemUnsupportedReason)
}

func TestLabelVerificationIdentityMismatchIsPostWriteMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/labels":
			writeJSON(w, map[string]any{"labels": []map[string]string{}})
		case r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/labels":
			writeJSON(w, labelDetail("Label_created", "Project Alpha", "user", 0, 0, nil))
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/labels/Label_created":
			writeJSON(w, labelDetail("Label_created", "Different", "user", 0, 0, nil))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testClient(server).CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Project Alpha"})
	assertProductCode(t, err, errs.CodeVerificationFailed)
	assertProductReason(t, err, string(errs.OutcomePostWriteMismatch))
}

func TestSuccessfulMessageWriteRequiresJSONContentType(t *testing.T) {
	labels := []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "Label_42", Name: "Project Alpha", Type: "user"},
	}
	current := []string{"INBOX"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/labels":
			rows := make([]map[string]string, len(labels))
			for i, label := range labels {
				rows[i] = map[string]string{"id": label.ID, "name": label.Name, "type": label.Type}
			}
			writeJSON(w, map[string]any{"labels": rows})
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/messages/g-media":
			writeJSON(w, map[string]any{"id": "g-media", "labelIds": current})
		case r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/messages/g-media/modify":
			current = []string{"INBOX", "Label_42"}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(`{"id":"g-media","labelIds":["INBOX","Label_42"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testClient(server).CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: "g-media",
		Folder:    mail.Folder{Account: "work", Provider: "gmail", ID: "Label_42", DisplayName: "Project Alpha", Path: "Project Alpha"},
	})
	assertProductCode(t, err, errs.CodeVerificationFailed)
	assertProductReason(t, err, string(errs.OutcomeProviderResultMalformed))
}

func TestCreateFolderRejectsGmailLabelLimitBeforeWrite(t *testing.T) {
	labels := make([]gmailLabel, 0, limits.GmailMaxLabels)
	for i := 0; i < limits.GmailMaxLabels; i++ {
		labels = append(labels, gmailLabel{ID: fmt.Sprintf("Label_%d", i), Name: fmt.Sprintf("Label %d", i), Type: "user"})
	}
	client := &Client{
		account:     "work",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  "http://127.0.0.1/gmail/v1/users/me",
		labelBase:   labels,
	}

	_, err := client.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Overflow"})
	assertProductCode(t, err, errs.CodeResourceLimitExceeded)
}

func TestFolderLabelCreateRenameDeleteRoundTrip(t *testing.T) {
	api := newGmailMutationFixture(t, []gmailLabel{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
	}, nil)
	defer api.server.Close()
	client := testClient(api.server)

	created, err := client.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Project Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if created.CreatedFolder == nil || created.CreatedFolder.ID != "Label_created" || created.CreatedFolder.DisplayName != "Project Alpha" ||
		created.WriteOutcomeFacts == nil || created.Outcome != mail.WriteOutcomeSucceeded || created.ProviderMeta == nil || created.ProviderMeta.LabelType != mail.LabelTypeUser {
		t.Fatalf("created = %#v", created)
	}
	if len(api.createRequests) != 1 || api.createRequests[0].Name != "Project Alpha" || api.createRequests[0].Fields != gmailLabelResultFields {
		t.Fatalf("create requests = %#v", api.createRequests)
	}

	renamed, err := client.RenameFolder(context.Background(), mail.FolderRenameRequest{
		Target: mail.Folder{
			Account:     "work",
			Provider:    "gmail",
			ID:          "Label_created",
			DisplayName: "Project Alpha",
			Path:        "Project Alpha",
		},
		NewDisplayName: "Project Beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.RenamedFolder == nil || renamed.RenamedFolder.DisplayName != "Project Beta" || renamed.OldDisplayName != "Project Alpha" ||
		renamed.NewDisplayName != "Project Beta" || renamed.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("renamed = %#v", renamed)
	}
	if len(api.patchRequests) != 1 || api.patchRequests[0].ID != "Label_created" || api.patchRequests[0].Name != "Project Beta" || api.patchRequests[0].Fields != gmailLabelResultFields {
		t.Fatalf("patch requests = %#v", api.patchRequests)
	}

	deleted, err := client.DeleteFolder(context.Background(), mail.FolderDeleteRequest{
		Target: mail.Folder{
			Account:     "work",
			Provider:    "gmail",
			ID:          "Label_created",
			DisplayName: "Project Beta",
			Path:        "Project Beta",
		},
		Preflight: mail.FolderMutationPreflight{TotalItemCount: 3, TotalItemCountKnown: true, ChildFolderCount: 1, ChildFolderKnown: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedFolder == nil || deleted.DeletedFolder.DisplayName != "Project Beta" || deleted.TotalItemCount != 3 ||
		!deleted.TotalItemCountKnown || deleted.ChildFolderCount != 1 || !deleted.ChildFolderKnown ||
		deleted.Outcome != mail.WriteOutcomeSucceeded || deleted.ProviderMeta == nil || deleted.ProviderMeta.DeleteSemantics != mail.DeleteSemanticsLabelOnly {
		t.Fatalf("deleted = %#v", deleted)
	}
	if len(api.deleteRequests) != 1 || api.deleteRequests[0].ID != "Label_created" {
		t.Fatalf("delete requests = %#v", api.deleteRequests)
	}
}

func TestTokenResponseValidationFailsClosed(t *testing.T) {
	p := New(Options{Now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }})

	_, err := p.parseTokenResponse("work", http.StatusOK, []byte(`{"error":"invalid_grant"}`), false, nil, nil)
	assertProductCode(t, err, errs.CodeProviderDataError)

	_, err = p.parseTokenResponse("work", http.StatusBadRequest, []byte(`{"error":"invalid_grant"}`), false, nil, nil)
	assertProductCode(t, err, errs.CodeAuthRepairRequired)

	_, err = p.parseTokenResponse("work", http.StatusOK, []byte(`{"access_token":"token","expires_in":9223372037,"token_type":"Bearer"}`), false, []string{requiredScope}, nil)
	assertProductCode(t, err, errs.CodeProviderDataError)

	_, err = p.parseTokenResponse("work", http.StatusOK, []byte(`{"access_token":"first","access_token":"second","expires_in":3600,"token_type":"Bearer"}`), false, []string{requiredScope}, nil)
	assertProductCode(t, err, errs.CodeProviderDataError)
}

func TestForcedRefreshPropagatesTokenServerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}))
	defer server.Close()

	session := &testCredentialSession{current: validCache("work", "work@example.test")}
	p := New(Options{
		TokenURL:   server.URL + "/token",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) },
	})
	_, err := p.ForcedRefresh(context.Background(), provider.StoredAuthContext{
		Runtime:       &secretRecorder{},
		Provider:      "gmail",
		Account:       "work",
		Setup:         maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{ClientID: "gmail-client"}},
		ExpectedEmail: "work@example.test",
		Credential:    session,
	})
	if err == nil {
		t.Fatal("token server failure returned nil")
	}
	assertNotProductCode(t, err, errs.CodeAuthRepairRequired)
	if session.replaced != nil {
		t.Fatalf("credential was replaced after token failure: %#v", session.replaced)
	}
}

func TestReadRawMessageDoesNotRequireReadableBody(t *testing.T) {
	rawMIME := "Content-Type: application/pdf\r\n\r\n%PDF-1.7"
	rawPayload := base64.RawURLEncoding.EncodeToString([]byte(rawMIME))
	var attachmentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/labels":
			writeJSON(w, map[string]any{"labels": []map[string]any{{"id": "INBOX", "name": "INBOX", "type": "system"}}})
		case "/gmail/v1/users/me/messages/raw-only":
			switch r.URL.Query().Get("format") {
			case "full":
				writeJSON(w, rawOnlyFixtureMessage())
			case "raw":
				writeJSON(w, map[string]any{"id": "raw-only", "threadId": "thread-raw", "raw": rawPayload})
			default:
				t.Fatalf("unexpected message format %q", r.URL.Query().Get("format"))
			}
		case "/gmail/v1/users/me/messages/raw-only/attachments/att-raw":
			attachmentCalls++
			http.Error(w, "attachment content must not be fetched", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	msg, err := client.ReadMessage(context.Background(), mail.ReadMessageRequest{
		Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: "raw-only"},
		BodyMode: mail.BodyModeRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != "raw-only" || msg.Body.Mode != mail.BodyModeRaw || msg.Body.Content != rawMIME ||
		len(msg.Attachments) != 1 || !msg.HasAttachments || attachmentCalls != 0 {
		t.Fatalf("raw message = %#v attachmentCalls=%d", msg, attachmentCalls)
	}
}

func TestFetchMessageRejectsMismatchedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/messages/m1" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": "m2", "threadId": "t1", "payload": map[string]any{"headers": []map[string]string{}}})
	}))
	defer server.Close()

	_, err := testClient(server).fetchMessage(context.Background(), "m1", "metadata", "Message-ID", "id,threadId,payload/headers")
	assertProductCode(t, err, errs.CodeProviderDataError)
}

func TestMessageAttachmentsListAndDownload(t *testing.T) {
	inlineBytes := []byte("inline bytes")
	externalBytes := []byte("external bytes")
	inlineEncoded := base64.RawURLEncoding.EncodeToString(inlineBytes)
	externalEncoded := base64.RawURLEncoding.EncodeToString(externalBytes)
	var structureCalls, attachmentCalls int
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
			t.Fatalf("authorization header = %q", got)
		}
		paths = append(paths, r.URL.String())
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/m1":
			structureCalls++
			if r.URL.Query().Get("format") != "full" || r.URL.Query().Get("fields") != "id,threadId,payload" {
				t.Fatalf("structure query = %s", r.URL.RawQuery)
			}
			writeJSON(w, gmailAttachmentMessageJSON(inlineEncoded, "private-secret", int64(len(inlineBytes)), int64(len(externalBytes))))
		case "/gmail/v1/users/me/messages/m1/attachments/private-secret":
			attachmentCalls++
			if r.URL.Query().Get("fields") != "size,data" {
				t.Fatalf("attachment query = %s", r.URL.RawQuery)
			}
			writeJSON(w, map[string]any{"size": len(externalBytes), "data": externalEncoded})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	list, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if structureCalls != 1 || attachmentCalls != 0 {
		t.Fatalf("list calls structure=%d attachment=%d paths=%v", structureCalls, attachmentCalls, paths)
	}
	if list.MessageID != "m1" || list.InternetMessageID != "<m1@example.test>" || len(list.Attachments) != 3 {
		t.Fatalf("list = %#v", list)
	}
	if list.ProviderMeta == nil || list.ProviderMeta.Source != mail.MetadataSourceGmailAPI || list.ProviderMeta.SnapshotMessageID != "m1" {
		t.Fatalf("list provider metadata = %#v", list.ProviderMeta)
	}
	if got := []string{list.Attachments[0].ID, list.Attachments[1].ID, list.Attachments[2].ID}; !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Fatalf("public attachment ids = %#v", got)
	}
	inline := list.Attachments[0]
	if !inline.SizeFactsPresent || inline.SizeFacts.EncodedSizeKnown || inline.SizeFacts.EncodedSize != 0 ||
		!inline.SizeFacts.DecodedSizeKnown || inline.SizeFacts.DecodedSize != uint64(len(inlineBytes)) {
		t.Fatalf("inline size facts = %#v present=%v", inline.SizeFacts, inline.SizeFactsPresent)
	}
	external := list.Attachments[1]
	if external.ProviderMeta == nil || external.ProviderMeta.ExternalAttachmentIDPresent == nil || !*external.ProviderMeta.ExternalAttachmentIDPresent {
		t.Fatalf("external provider metadata = %#v", external.ProviderMeta)
	}
	if inline.ProviderMeta == nil || inline.ProviderMeta.ExternalAttachmentIDPresent != nil || strings.Contains(fmt.Sprintf("%#v", list), "private-secret") {
		t.Fatalf("private attachment locator leaked in list: %#v", list)
	}

	inlineContent, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	gotInline, err := io.ReadAll(inlineContent.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := inlineContent.Content.Close(); err != nil {
		t.Fatal(err)
	}
	if string(gotInline) != string(inlineBytes) || inlineContent.AttachmentID != "1" || inlineContent.DeclaredSize != int64(len(inlineBytes)) {
		t.Fatalf("inline content = %q result=%#v", gotInline, inlineContent)
	}
	if structureCalls != 2 || attachmentCalls != 0 {
		t.Fatalf("inline calls structure=%d attachment=%d paths=%v", structureCalls, attachmentCalls, paths)
	}

	externalContent, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "2"})
	if err != nil {
		t.Fatal(err)
	}
	gotExternal, err := io.ReadAll(externalContent.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := externalContent.Content.Close(); err != nil {
		t.Fatal(err)
	}
	if string(gotExternal) != string(externalBytes) || externalContent.AttachmentID != "2" || externalContent.DeclaredSize != int64(len(externalBytes)) {
		t.Fatalf("external content = %q result=%#v", gotExternal, externalContent)
	}
	if structureCalls != 3 || attachmentCalls != 1 {
		t.Fatalf("external calls structure=%d attachment=%d paths=%v", structureCalls, attachmentCalls, paths)
	}
}

// A search without matches comes back as 204 No Content, as seen live on
// 2026-11-08. That is an empty result, not an error.
func TestSearchWithoutMatchesAcceptsNoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/messages" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	search, err := testClient(server).SearchMessages(context.Background(), mail.SearchRequest{Query: mail.Query{Terms: []string{"nothing"}}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Messages) != 0 {
		t.Fatalf("search = %#v", search)
	}
}

// Gmail returns a new attachmentId every time it sends the message, as seen
// in the live test on 2026-11-08. The download must use the current one.
func TestMessageAttachmentDownloadAcceptsANewGmailAttachmentID(t *testing.T) {
	externalBytes := []byte("%PDF fresh")
	structureCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/m1":
			structureCalls++
			writeJSON(w, gmailAttachmentMessageJSON("", fmt.Sprintf("att-fetch-%d", structureCalls), 0, int64(len(externalBytes))))
		case "/gmail/v1/users/me/messages/m1/attachments/att-fetch-2":
			writeJSON(w, map[string]any{"size": len(externalBytes), "data": base64.RawURLEncoding.EncodeToString(externalBytes)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	if _, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	content, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "2"})
	if err != nil {
		t.Fatal(err)
	}
	defer content.Content.Close()
	got, err := io.ReadAll(content.Content)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(externalBytes) {
		t.Fatalf("content = %q", got)
	}
}

func TestMessageAttachmentDownloadRequiresFreshSnapshot(t *testing.T) {
	inlineBytes := []byte("first")
	mutatedBytes := []byte("second")
	inlineEncoded := base64.RawURLEncoding.EncodeToString(inlineBytes)
	mutatedEncoded := base64.RawURLEncoding.EncodeToString(mutatedBytes)
	useMutated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/messages/m1" {
			http.NotFound(w, r)
			return
		}
		encoded := inlineEncoded
		size := int64(len(inlineBytes))
		if useMutated {
			encoded = mutatedEncoded
			size = int64(len(mutatedBytes))
		}
		writeJSON(w, gmailAttachmentMessageJSON(encoded, "private-secret", size, 10))
	}))
	defer server.Close()

	client := testClient(server)
	if _, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	useMutated = true
	_, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "1"})
	assertProductCode(t, err, errs.CodeAttachmentNotFound)
	assertProductReason(t, err, "stale_snapshot_part")
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v", err)
	}
	if product.Details()["snapshot_message_id"] != "m1" || product.Details()["attachment_id"] != "1" || strings.Contains(fmt.Sprintf("%#v", product.Details()), "private-secret") {
		t.Fatalf("stale details = %#v", product.Details())
	}
}

func TestMessageAttachmentExternalDownloadHonorsDeclaredSizeLimit(t *testing.T) {
	var attachmentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/m1":
			writeJSON(w, gmailAttachmentMessageJSON("", "private-secret", 0, mail.MaxMessageAttachmentDownloadBytes+1))
		case "/gmail/v1/users/me/messages/m1/attachments/private-secret":
			attachmentCalls++
			http.Error(w, "unexpected attachment content fetch", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	if _, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	_, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "2"})
	assertProductCode(t, err, errs.CodeAttachmentTooLarge)
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v", err)
	}
	if product.Details()["dimension"] != string(errs.GmailAttachmentSizeDecoded) ||
		product.Details()["snapshot_message_id"] != "m1" ||
		product.Details()["attachment_id"] != "2" ||
		attachmentCalls != 0 {
		t.Fatalf("too large details=%#v attachmentCalls=%d", product.Details(), attachmentCalls)
	}
}

func TestMessageAttachmentSelectedEncodedLimitWinsOverAggregateLimit(t *testing.T) {
	limit := uint64(limits.GmailMaxAttachmentEncodedBytes)
	stats := attachmentJSONStats{
		totalBytes:         limit + 1,
		aggregateDataBytes: limit + 1,
		selectedDataBytes:  limit + 1,
	}
	err := attachmentStructureBudgetError("work", stats, "m1", "1")
	assertProductCode(t, err, errs.CodeAttachmentTooLarge)
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v", err)
	}
	if product.Details()["snapshot_message_id"] != "m1" ||
		product.Details()["attachment_id"] != "1" ||
		product.Details()["dimension"] != string(errs.GmailAttachmentSizeEncoded) {
		t.Fatalf("selected limit details = %#v", product.Details())
	}
}

func TestMessageAttachmentParserTracksSelectedDataAfterBodyBeforePartID(t *testing.T) {
	body := `{"id":"m1","threadId":"t1","payload":{"mimeType":"multipart/mixed","parts":[{"body":{"data":"YQ","size":1},"partId":"1","mimeType":"text/plain","filename":"a.txt","headers":[{"name":"Content-Disposition","value":"attachment; filename=\"a.txt\""}]}]}}`
	msg, stats, err := parseAttachmentStructureJSON("work", strings.NewReader(body), "1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != "m1" || msg.Payload == nil || stats.selectedDataBytes != 2 || stats.aggregateDataBytes != 2 {
		t.Fatalf("msg=%#v stats=%#v", msg, stats)
	}
}

func TestMessageAttachmentStructuralNonDataLimitPrecedesLaterMalformedData(t *testing.T) {
	body := `{"id":"m1","threadId":"t1","payload":{"mimeType":"text/plain","padding":"` +
		strings.Repeat("x", int(limits.GmailMaxJSONResponseBytes)+1) +
		`","body":{"data":"+/"}}}`

	_, _, err := parseAttachmentStructureJSON("work", strings.NewReader(body), "", "")
	assertProductCode(t, err, errs.CodeReadLimitExceeded)
}

func TestMessageAttachmentExternalSelectedLimitWinsOverCompleteResponseLimit(t *testing.T) {
	encodedLimit := int(limits.GmailMaxAttachmentEncodedBytes)
	contentLimit := int(limits.GmailMaxAttachmentContentResponseBytes)
	data := strings.Repeat("A", encodedLimit+2)
	pad := contentLimit + 1 - len(data) - len(`{"data":"`) - len(`","size":0}`)
	if pad < 0 {
		t.Fatalf("test fixture cannot fit content limit: pad=%d", pad)
	}
	body := `{` + strings.Repeat(" ", pad) + `"data":"` + data + `","size":0}`
	limited := newAttachmentLimitReader(strings.NewReader(body), limits.GmailMaxAttachmentContentResponseBytes)

	_, _, err := parseExternalAttachmentJSON("work", limited, "m1", "1")
	assertProductCode(t, err, errs.CodeAttachmentTooLarge)
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v", err)
	}
	if product.Details()["snapshot_message_id"] != "m1" ||
		product.Details()["attachment_id"] != "1" ||
		product.Details()["observed_at_least"] != uint64(limits.GmailMaxAttachmentEncodedBytes+1) ||
		!limited.over() {
		t.Fatalf("selected/content tie details=%#v observed=%d", product.Details(), limited.observed())
	}
}

func TestMessageAttachmentExternalErrorsRedactPrivateLocator(t *testing.T) {
	part := gmailAttachmentSnapshotPart{
		attachment: mail.Attachment{ContentType: "application/pdf"},
		externalID: "private-secret",
	}

	t.Run("server status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server failed", http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := testClient(server).externalAttachmentContent(context.Background(), "m1", "2", part)
		if err == nil {
			t.Fatal("expected external status error")
		}
		if strings.Contains(fmt.Sprintf("%v %#v", err, err), "private-secret") {
			t.Fatalf("private attachment locator leaked in status error: %v", err)
		}
		if !strings.Contains(err.Error(), "/attachments/{attachment}") {
			t.Fatalf("status error did not use redacted operation: %v", err)
		}
	})

	t.Run("connection", func(t *testing.T) {
		client := &Client{
			account:     "work",
			accessToken: secretFromString("gmail-access-token"),
			tokenType:   "Bearer",
			apiBaseURL:  "http://127.0.0.1/gmail/v1/users/me",
			http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("dial private-secret failed")
			})},
		}

		_, err := client.externalAttachmentContent(context.Background(), "m1", "2", part)
		assertProductCode(t, err, errs.CodeProviderConnectionError)
		var product *errs.Error
		if !errors.As(err, &product) {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(fmt.Sprintf("%#v", product.Details()), "private-secret") ||
			product.Details()["operation"] != "/messages/m1/attachments/{attachment}" {
			t.Fatalf("connection details = %#v", product.Details())
		}
	})
}

func TestMessageAttachmentExternalMissingContentIsProviderData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/m1":
			writeJSON(w, gmailAttachmentMessageJSON("YQ", "private-secret", 1, 1))
		case "/gmail/v1/users/me/messages/m1/attachments/private-secret":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	if _, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	_, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "2"})
	assertProductCode(t, err, errs.CodeProviderDataError)
	assertProductReason(t, err, "invalid_protocol")
	if strings.Contains(fmt.Sprintf("%#v", err), "private-secret") {
		t.Fatalf("private attachment locator leaked in error: %v", err)
	}
}

func TestMessageAttachmentsListValidatesUnselectedBodyData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/messages/m1" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"id":       "m1",
			"threadId": "t1",
			"payload": map[string]any{
				"mimeType": "multipart/mixed",
				"parts": []map[string]any{{
					"partId":   "0",
					"mimeType": "text/plain",
					"body":     map[string]any{"size": 1, "data": "+/"},
				}},
			},
		})
	}))
	defer server.Close()

	_, err := testClient(server).ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"})
	assertProductCode(t, err, errs.CodeProviderDataError)
	assertProductReason(t, err, "invalid_base64")
}

func TestMessageAttachmentStreamDestroyedWithClient(t *testing.T) {
	externalBytes := []byte("external bytes")
	externalEncoded := base64.RawURLEncoding.EncodeToString(externalBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/m1":
			writeJSON(w, gmailAttachmentMessageJSON("YQ", "private-secret", 1, int64(len(externalBytes))))
		case "/gmail/v1/users/me/messages/m1/attachments/private-secret":
			writeJSON(w, map[string]any{"size": len(externalBytes), "data": externalEncoded})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server)
	if _, err := client.ListMessageAttachments(context.Background(), mail.MessageAttachmentListRequest{MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	content, err := client.DownloadMessageAttachment(context.Background(), mail.MessageAttachmentDownloadRequest{MessageID: "m1", AttachmentID: "2"})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := content.Content.(*encodedAttachmentStream)
	if !ok {
		t.Fatalf("content stream = %T", content.Content)
	}
	stream.mu.Lock()
	path := stream.path
	stream.mu.Unlock()
	if path == "" {
		t.Fatal("stream did not record a private spool path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spool before destroy: %v", err)
	}
	client.Destroy()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool after destroy exists or stat failed: %v", err)
	}
}

func TestMessageAttachmentsRejectMalformedAttachmentStructures(t *testing.T) {
	client := &Client{account: "work"}
	for _, tc := range []struct {
		name       string
		parts      []gmailPart
		wantReason string
	}{
		{
			name: "duplicate part ids",
			parts: []gmailPart{
				gmailAttachmentPart("1", "a.txt", "YQ", nil, 1),
				gmailAttachmentPart("1", "b.txt", "Yg", nil, 1),
			},
			wantReason: "invalid_protocol",
		},
		{
			name:       "empty part id",
			parts:      []gmailPart{gmailAttachmentPart("", "a.txt", "YQ", nil, 1)},
			wantReason: "invalid_protocol",
		},
		{
			name:       "missing content representation",
			parts:      []gmailPart{gmailAttachmentPart("1", "a.txt", "", nil, 1)},
			wantReason: "invalid_protocol",
		},
		{
			name:       "both inline data and external locator",
			parts:      []gmailPart{gmailAttachmentPart("1", "a.txt", "YQ", stringPtr("private-secret"), 1)},
			wantReason: "invalid_protocol",
		},
		{
			name:       "negative declared size",
			parts:      []gmailPart{gmailAttachmentPart("1", "a.txt", "YQ", nil, -1)},
			wantReason: "invalid_json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.normalizePayload(&gmailMessage{
				ID:       "m1",
				ThreadID: "t1",
				Payload: &gmailPart{
					MimeType: "multipart/mixed",
					Parts:    tc.parts,
				},
			}, false)
			assertProductCode(t, err, errs.CodeProviderDataError)
			assertProductReason(t, err, tc.wantReason)
		})
	}
}

func gmailAttachmentMessageJSON(inlineEncoded, privateAttachmentID string, inlineSize, externalSize int64) map[string]any {
	return map[string]any{
		"id":       "m1",
		"threadId": "t1",
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers":  []map[string]any{{"name": "Message-ID", "value": "<m1@example.test>"}},
			"parts": []map[string]any{
				{
					"partId":   "0",
					"mimeType": "text/plain",
					"body":     map[string]any{"size": 4, "data": "Ym9keQ"},
				},
				{
					"partId":   "1",
					"mimeType": "text/plain",
					"filename": "inline.txt",
					"headers":  []map[string]any{{"name": "Content-Disposition", "value": `attachment; filename="inline.txt"`}},
					"body":     map[string]any{"size": inlineSize, "data": inlineEncoded},
				},
				{
					"partId":   "2",
					"mimeType": "application/pdf",
					"filename": "external.pdf",
					"headers":  []map[string]any{{"name": "Content-Disposition", "value": `attachment; filename="external.pdf"`}},
					"body":     map[string]any{"size": externalSize, "attachmentId": privateAttachmentID},
				},
				{
					"partId":   "3",
					"mimeType": "application/octet-stream",
					"filename": "empty.bin",
					"headers":  []map[string]any{{"name": "Content-Disposition", "value": `attachment; filename="empty.bin"`}},
					"body":     map[string]any{"size": 0},
				},
			},
		},
	}
}

func gmailAttachmentPart(partID, name, data string, attachmentID *string, size int64) gmailPart {
	part := gmailPart{
		PartID:   partID,
		MimeType: "text/plain",
		Filename: name,
		Headers:  []gmailHeader{{Name: "Content-Disposition", Value: `attachment; filename="` + name + `"`}},
		Body:     gmailBody{Size: &size},
	}
	if data != "" {
		part.Body.Data = &data
	}
	part.Body.AttachmentID = attachmentID
	return part
}

func TestAttachmentBodyFailureClassesArePreserved(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantReason string
		noCode     string
	}{
		{name: "missing external body", status: http.StatusNotFound, body: `{}`, wantCode: errs.CodeProviderDataError, wantReason: "invalid_protocol"},
		{name: "authentication rejected", status: http.StatusUnauthorized, body: `{}`, wantCode: errs.CodeAuthRepairRequired},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{}`, wantCode: errs.CodeProviderRateLimited},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"errors":[{"reason":"domainPolicy"}]}}`, wantCode: errs.CodeProviderForbidden},
		{name: "malformed json", status: http.StatusOK, body: `{`, wantCode: errs.CodeProviderDataError},
		{name: "negative declared size", status: http.StatusOK, body: `{"size":-1,"data":"YQ"}`, wantCode: errs.CodeProviderDataError, wantReason: "invalid_json"},
		{name: "server failure", status: http.StatusInternalServerError, body: `{}`, noCode: errs.CodeProviderDataError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/gmail/v1/users/me/messages/m1/attachments/att-1" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			attID := "att-1"
			_, err := testClient(server).leafBodyBytes(context.Background(), "m1", &mimeLeaf{attachmentID: &attID})
			if tc.wantCode != "" {
				assertProductCode(t, err, tc.wantCode)
				if tc.wantReason != "" {
					assertProductReason(t, err, tc.wantReason)
				}
				return
			}
			if err == nil {
				t.Fatal("server failure returned nil")
			}
			assertNotProductCode(t, err, tc.noCode)
		})
	}
}

func TestAttachmentBodyConnectionFailureIsPreserved(t *testing.T) {
	attID := "att-1"
	client := &Client{
		account:     "work",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  "http://127.0.0.1/gmail/v1/users/me",
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed")
		})},
	}
	_, err := client.leafBodyBytes(context.Background(), "m1", &mimeLeaf{attachmentID: &attID})
	assertProductCode(t, err, errs.CodeProviderConnectionError)
}

func TestMIMETextAndSubjectDecoding(t *testing.T) {
	data := base64.RawURLEncoding.EncodeToString([]byte{'c', 'a', 'f', 0xe9})
	client := &Client{
		account:     "work",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		labelBase:   []gmailLabel{{ID: "INBOX", Name: "INBOX", Type: "system"}},
	}
	msg, err := client.fullMessage(context.Background(), gmailMessage{
		ID:           "m1",
		ThreadID:     "t1",
		LabelIDs:     []string{"INBOX"},
		InternalDate: "1777046400000",
		Payload: &gmailPart{
			MimeType: "text/plain; charset=iso-8859-1",
			Headers: []gmailHeader{
				{Name: "Message-ID", Value: "<m1@example.test>"},
				{Name: "Subject", Value: "=?ISO-8859-1?Q?caf=E9?="},
				{Name: "Date", Value: "Tue, 24 Aug 2026 12:00:00 +0000"},
			},
			Body: gmailBody{Data: &data, Size: int64Ptr(4)},
		},
	}, mail.BodyModeText)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != "caf\u00e9" || msg.Body.Content != "caf\u00e9" {
		t.Fatalf("decoded subject/body = %q/%q", msg.Subject, msg.Body.Content)
	}
}

func TestAttachmentClassifierDoesNotFallbackFromUnsafeGmailFilename(t *testing.T) {
	client := &Client{account: "work"}
	normalized, err := client.normalizePayload(&gmailMessage{
		ID:       "m1",
		ThreadID: "t1",
		Payload: &gmailPart{
			MimeType: "multipart/mixed",
			Parts: []gmailPart{{
				PartID:   "1",
				MimeType: "application/pdf",
				Filename: " unsafe.pdf",
				Headers:  []gmailHeader{{Name: "Content-Disposition", Value: `attachment; filename="safe.pdf"`}},
				Body:     gmailBody{AttachmentID: stringPtr("att-1"), Size: int64Ptr(1)},
			}},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Gmail's own name is cleaned, never swapped for a different one from
	// the headers.
	if len(normalized.attachments) != 1 || normalized.attachments[0].Name != "unsafe.pdf" {
		t.Fatalf("attachments = %#v", normalized.attachments)
	}
}

// Gmail leaves "filename" empty for some names it cannot decode; the
// headers then supply the name, in RFC 2231 or RFC 2047 form.
func TestAttachmentNamesFromHeadersWhenGmailLeavesThemOut(t *testing.T) {
	client := &Client{account: "work"}
	parts := []gmailPart{
		{PartID: "1", MimeType: "application/pdf", Headers: []gmailHeader{{Name: "Content-Disposition", Value: "attachment; filename*0*=utf-8''%C3%9Cbersicht%20Q3; filename*1*=%20%E2%80%93%20final.pdf"}}, Body: gmailBody{AttachmentID: stringPtr("att-1"), Size: int64Ptr(1)}},
		{PartID: "2", MimeType: "text/plain", Headers: []gmailHeader{{Name: "Content-Disposition", Value: "attachment"}, {Name: "Content-Type", Value: `text/plain; name="=?iso-8859-1?Q?Gr=FC=DFe.txt?="`}}, Body: gmailBody{AttachmentID: stringPtr("att-2"), Size: int64Ptr(1)}},
		{PartID: "3", MimeType: "text/csv", Filename: "=?UTF-8?B?w5xiZXJzaWNodC5jc3Y=?=", Body: gmailBody{AttachmentID: stringPtr("att-3"), Size: int64Ptr(1)}},
		{PartID: "4", MimeType: "text/plain", Filename: "a\x1b[31mred.txt", Body: gmailBody{AttachmentID: stringPtr("att-4"), Size: int64Ptr(1)}},
	}
	normalized, err := client.normalizePayload(&gmailMessage{ID: "m1", ThreadID: "t1", Payload: &gmailPart{MimeType: "multipart/mixed", Parts: parts}}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Übersicht Q3 – final.pdf", "Grüße.txt", "Übersicht.csv", "a_[31mred.txt"}
	if len(normalized.attachments) != len(want) {
		t.Fatalf("attachments = %#v", normalized.attachments)
	}
	for i, name := range want {
		if got := normalized.attachments[i].Name; got != name {
			t.Fatalf("attachment %d name = %q, want %q", i, got, name)
		}
	}
}

func TestResponseJSONRejectsDuplicateKeys(t *testing.T) {
	var out struct {
		Payload struct {
			MimeType string `json:"mimeType"`
		} `json:"payload"`
	}
	err := decodeJSONResponse("work", []byte(`{"payload":{"mimeType":"text/plain","mimeType":"text/html"}}`), &out)
	assertProductCode(t, err, errs.CodeProviderDataError)
}

func TestAPIBaseURLPinnedAndProfileUsesSharedBuilder(t *testing.T) {
	client := &Client{
		account:     "work",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  "https://example.com/gmail/v1/users/me",
	}
	if _, err := client.buildURL("/labels", nil); err == nil {
		t.Fatal("arbitrary HTTPS API origin accepted")
	}

	var called bool
	p := New(Options{
		APIBaseURL: "https://example.com/gmail/v1/users/me",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("unexpected request")
		})},
	})
	if _, err := p.getProfile(context.Background(), "access-token", "work", false); err == nil {
		t.Fatal("profile accepted arbitrary HTTPS API origin")
	}
	if called {
		t.Fatal("profile made HTTP request before validating API base URL")
	}
}

type setupFlags map[string]provider.SetupFlagValue

func (f setupFlags) Lookup(name string) (provider.SetupFlagValue, bool) {
	v, ok := f[name]
	return v, ok
}

type secretRecorder struct {
	values []string
}

func (r *secretRecorder) RegisterSecret(value string) error {
	r.values = append(r.values, value)
	sort.Strings(r.values)
	return nil
}

type testCredentialSession struct {
	current  provider.Credential
	replaced provider.Credential
}

func (s *testCredentialSession) WithCurrent(_ context.Context, fn func(provider.CredentialLease) error) error {
	return fn(testCredentialLease{session: s})
}

type testCredentialLease struct {
	session *testCredentialSession
}

func (l testCredentialLease) Current() provider.Credential {
	return l.session.current
}

func (l testCredentialLease) Replace(next provider.Credential) error {
	l.session.replaced = next
	l.session.current = next
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func credentialBinding(account string) auth.CredentialBinding {
	return auth.CredentialBinding{Provider: "gmail", Account: account}
}

func validCache(account, email string) *TokenCache {
	return NewTokenCache(TokenCacheData{
		Version:       1,
		Provider:      "gmail",
		AccountName:   account,
		VerifiedEmail: email,
		ClientID:      "gmail-client",
		ClientSecret:  "gmail-client-secret",
		Scopes:        []string{requiredScope},
		TokenType:     "Bearer",
		AccessToken:   "gmail-access-token",
		RefreshToken:  "gmail-refresh-token",
		ExpiresAt:     time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	})
}

func testClient(server *httptest.Server) *Client {
	return &Client{
		account:     "work",
		email:       "work@example.test",
		accessToken: secretFromString("gmail-access-token"),
		tokenType:   "Bearer",
		apiBaseURL:  server.URL + "/gmail/v1/users/me",
		http:        server.Client(),
	}
}

type gmailModifyRequest struct {
	MessageID string
	Add       []string
	Remove    []string
	Fields    string
}

type gmailMessageActionRequest struct {
	MessageID string
	Fields    string
}

type gmailLabelWriteRequest struct {
	ID     string
	Name   string
	Fields string
}

type gmailMutationFixture struct {
	t                 *testing.T
	server            *httptest.Server
	mu                sync.Mutex
	labels            []gmailLabel
	messages          map[string][]string
	modifyRequests    []gmailModifyRequest
	trashRequests     []gmailMessageActionRequest
	untrashRequests   []gmailMessageActionRequest
	createRequests    []gmailLabelWriteRequest
	patchRequests     []gmailLabelWriteRequest
	deleteRequests    []gmailLabelWriteRequest
	nextLabelID       string
	failModifyOrdinal int
	failModifyStatus  int
}

func newGmailMutationFixture(t *testing.T, labels []gmailLabel, messages map[string][]string) *gmailMutationFixture {
	t.Helper()
	f := &gmailMutationFixture{
		t:           t,
		labels:      append([]gmailLabel(nil), labels...),
		messages:    make(map[string][]string, len(messages)),
		nextLabelID: "Label_created",
	}
	for id, labelIDs := range messages {
		f.messages[id] = append([]string(nil), labelIDs...)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *gmailMutationFixture) messageLabels(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages[id]...)
}

func (f *gmailMutationFixture) handle(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
		f.t.Fatalf("authorization header = %q", got)
	}
	switch {
	case r.URL.Path == "/gmail/v1/users/me/labels":
		switch r.Method {
		case http.MethodGet:
			f.writeLabels(w)
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Fatalf("decode label create body: %v", err)
			}
			f.mu.Lock()
			id := f.nextLabelID
			f.labels = append(f.labels, gmailLabel{ID: id, Name: body.Name, Type: "user"})
			f.createRequests = append(f.createRequests, gmailLabelWriteRequest{Name: body.Name, Fields: r.URL.Query().Get("fields")})
			f.mu.Unlock()
			// Like Gmail, the create answer has no type field.
			created := labelDetail(id, body.Name, "", 0, 0, nil)
			delete(created, "type")
			writeJSON(w, created)
		default:
			f.t.Fatalf("labels method = %s", r.Method)
		}
	case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/labels/"):
		f.handleLabel(w, r)
	case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
		f.handleMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *gmailMutationFixture) writeLabels(w http.ResponseWriter) {
	f.mu.Lock()
	rows := make([]map[string]string, len(f.labels))
	for i, label := range f.labels {
		rows[i] = map[string]string{"id": label.ID, "name": label.Name, "type": label.Type}
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"labels": rows})
}

func (f *gmailMutationFixture) handleLabel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/labels/")
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		label, _, ok := f.findLabelLocked(id)
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, labelDetail(label.ID, label.Name, label.Type, 0, 0, nil))
	case http.MethodPatch:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatalf("decode label patch body: %v", err)
		}
		f.mu.Lock()
		label, idx, ok := f.findLabelLocked(id)
		if ok {
			label.Name = body.Name
			label.Type = "user"
			f.labels[idx] = label
			f.patchRequests = append(f.patchRequests, gmailLabelWriteRequest{ID: id, Name: body.Name, Fields: r.URL.Query().Get("fields")})
		}
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, labelDetail(label.ID, label.Name, label.Type, 0, 0, nil))
	case http.MethodDelete:
		f.mu.Lock()
		_, idx, ok := f.findLabelLocked(id)
		if ok {
			f.labels = append(f.labels[:idx], f.labels[idx+1:]...)
			f.deleteRequests = append(f.deleteRequests, gmailLabelWriteRequest{ID: id, Fields: r.URL.Query().Get("fields")})
		}
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Fatalf("label detail method = %s", r.Method)
	}
}

func (f *gmailMutationFixture) findLabelLocked(id string) (gmailLabel, int, bool) {
	for i, label := range f.labels {
		if label.ID == id {
			return label, i, true
		}
	}
	return gmailLabel{}, 0, false
}

func (f *gmailMutationFixture) handleMessage(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1:
		if r.Method != http.MethodGet {
			f.t.Fatalf("message method = %s", r.Method)
		}
		f.writeMessageLabels(w, id)
	case len(parts) == 2 && parts[1] == "modify":
		if r.Method != http.MethodPost {
			f.t.Fatalf("modify method = %s", r.Method)
		}
		var body struct {
			AddLabelIDs    []string `json:"addLabelIds"`
			RemoveLabelIDs []string `json:"removeLabelIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatalf("decode modify body: %v", err)
		}
		f.mu.Lock()
		ordinal := len(f.modifyRequests) + 1
		if f.failModifyOrdinal == ordinal {
			f.modifyRequests = append(f.modifyRequests, gmailModifyRequest{
				MessageID: id,
				Add:       append([]string(nil), body.AddLabelIDs...),
				Remove:    append([]string(nil), body.RemoveLabelIDs...),
				Fields:    r.URL.Query().Get("fields"),
			})
			status := f.failModifyStatus
			if status == 0 {
				status = http.StatusTooManyRequests
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"rate limited"}}`))
			return
		}
		current, ok := f.messages[id]
		if !ok {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		next := addLabels(removeLabels(current, body.RemoveLabelIDs...), body.AddLabelIDs...)
		f.messages[id] = next
		f.modifyRequests = append(f.modifyRequests, gmailModifyRequest{
			MessageID: id,
			Add:       append([]string(nil), body.AddLabelIDs...),
			Remove:    append([]string(nil), body.RemoveLabelIDs...),
			Fields:    r.URL.Query().Get("fields"),
		})
		f.mu.Unlock()
		f.writeMessageLabels(w, id)
	case len(parts) == 2 && parts[1] == "trash":
		if r.Method != http.MethodPost {
			f.t.Fatalf("trash method = %s", r.Method)
		}
		f.mu.Lock()
		current, ok := f.messages[id]
		if !ok {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		f.messages[id] = addLabels(removeLabels(current, "INBOX", "SPAM"), "TRASH")
		f.trashRequests = append(f.trashRequests, gmailMessageActionRequest{MessageID: id, Fields: r.URL.Query().Get("fields")})
		f.mu.Unlock()
		f.writeMessageLabels(w, id)
	case len(parts) == 2 && parts[1] == "untrash":
		if r.Method != http.MethodPost {
			f.t.Fatalf("untrash method = %s", r.Method)
		}
		f.mu.Lock()
		current, ok := f.messages[id]
		if !ok {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		f.messages[id] = removeLabels(current, "TRASH")
		f.untrashRequests = append(f.untrashRequests, gmailMessageActionRequest{MessageID: id, Fields: r.URL.Query().Get("fields")})
		f.mu.Unlock()
		f.writeMessageLabels(w, id)
	default:
		http.NotFound(w, r)
	}
}

func (f *gmailMutationFixture) writeMessageLabels(w http.ResponseWriter, id string) {
	f.mu.Lock()
	labels, ok := f.messages[id]
	labels = append([]string(nil), labels...)
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, nil)
		return
	}
	if labels == nil {
		labels = []string{}
	}
	writeJSON(w, map[string]any{"id": id, "labelIds": labels})
}

func outboundTextMessage() mail.OutboundMessage {
	body := "Hello from Gmail"
	return mail.OutboundMessage{
		To:      []mail.Recipient{{Address: "client@example.test"}},
		Subject: "Test Subject",
		Body: mail.OutboundBody{
			Format:      mail.BodyFormatText,
			ContentType: "text",
			Text:        body,
			Bytes:       len(body),
			Present:     true,
		},
	}
}

type testAttachmentPart struct {
	PartID      string
	Name        string
	ContentType string
	Disposition string
	ContentID   string
	Bytes       []byte
}

type testExternalAttachmentPart struct {
	PartID       string
	Name         string
	ContentType  string
	Disposition  string
	ContentID    string
	AttachmentID string
	Size         int64
}

func draftAttachmentRawMIME(t *testing.T, attachments []mimecompose.BinaryPart) []byte {
	t.Helper()
	doc, err := mimecompose.NewDocument(mimecompose.Message{
		From:        mail.Recipient{Address: "work@example.test"},
		To:          []mail.Recipient{{Address: "client@example.test"}},
		Subject:     "Attachment draft",
		TextBody:    []byte("body"),
		Attachments: attachments,
	}, mimecompose.DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	defer doc.Destroy()
	encoded, err := doc.Encode(mimecompose.EncodeOptions{BccPolicy: mimecompose.BccHeaderPolicyRetain, AddressMode: mimecompose.AddressHeaderASCII})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	return append([]byte(nil), encoded.Bytes...)
}

func draftStructure(draftID, messageID, threadID string, attachments []testAttachmentPart) map[string]any {
	return map[string]any{
		"id":      draftID,
		"message": messageStructure(messageID, threadID, attachments),
	}
}

func messageStructure(messageID, threadID string, attachments []testAttachmentPart) map[string]any {
	parts := []map[string]any{{
		"partId":   "0",
		"mimeType": "text/plain",
		"body":     map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("body")), "size": 4},
	}}
	for _, attachment := range attachments {
		headers := []map[string]string{{
			"name":  "Content-Disposition",
			"value": attachmentDispositionHeader(attachment.Disposition, attachment.Name),
		}}
		if attachment.ContentID != "" {
			headers = append(headers, map[string]string{"name": "Content-ID", "value": "<" + attachment.ContentID + ">"})
		}
		parts = append(parts, map[string]any{
			"partId":   attachment.PartID,
			"mimeType": attachment.ContentType,
			"filename": attachment.Name,
			"headers":  headers,
			"body": map[string]any{
				"data": base64.RawURLEncoding.EncodeToString(attachment.Bytes),
				"size": len(attachment.Bytes),
			},
		})
	}
	return map[string]any{
		"id":       messageID,
		"threadId": threadID,
		"labelIds": []string{"DRAFT"},
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []map[string]string{
				{"name": "Message-ID", "value": "<draft@example.test>"},
				{"name": "From", "value": "Work <work@example.test>"},
				{"name": "To", "value": "Client <client@example.test>"},
				{"name": "Subject", "value": "Attachment draft"},
			},
			"parts": parts,
		},
	}
}

func messageExternalAttachmentStructure(messageID, threadID string, attachments []testExternalAttachmentPart) map[string]any {
	parts := []map[string]any{{
		"partId":   "0",
		"mimeType": "text/plain",
		"body":     map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("body")), "size": 4},
	}}
	for _, attachment := range attachments {
		headers := []map[string]string{{
			"name":  "Content-Disposition",
			"value": attachmentDispositionHeader(attachment.Disposition, attachment.Name),
		}}
		if attachment.ContentID != "" {
			headers = append(headers, map[string]string{"name": "Content-ID", "value": "<" + attachment.ContentID + ">"})
		}
		attachmentID := attachment.AttachmentID
		if attachmentID == "" {
			attachmentID = "ext-" + attachment.PartID
		}
		parts = append(parts, map[string]any{
			"partId":   attachment.PartID,
			"mimeType": attachment.ContentType,
			"filename": attachment.Name,
			"headers":  headers,
			"body": map[string]any{
				"attachmentId": attachmentID,
				"size":         attachment.Size,
			},
		})
	}
	return map[string]any{
		"id":       messageID,
		"threadId": threadID,
		"labelIds": []string{"INBOX"},
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []map[string]string{
				{"name": "Message-ID", "value": "<source@example.test>"},
				{"name": "From", "value": "Sender <sender@example.test>"},
				{"name": "To", "value": "Client <client@example.test>"},
				{"name": "Subject", "value": "Source"},
			},
			"parts": parts,
		},
	}
}

func attachmentDispositionHeader(disposition, name string) string {
	if disposition == "" {
		disposition = "attachment"
	}
	if name == "" {
		return disposition
	}
	return disposition + `; filename="` + name + `"`
}

func readGmailMultipartUpload(t *testing.T, r *http.Request) (map[string]any, []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse upload content type: %v", err)
	}
	if mediaType != "multipart/related" {
		t.Fatalf("upload content type = %q, want multipart/related", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("upload boundary missing")
	}
	reader := multipart.NewReader(r.Body, boundary)
	var meta map[string]any
	var raw []byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read upload part: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read upload part body: %v", err)
		}
		contentType := part.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(contentType, "application/json"):
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatalf("decode upload metadata: %v", err)
			}
		case strings.HasPrefix(contentType, "message/rfc822"):
			raw = data
		default:
			t.Fatalf("unexpected upload part content type %q", contentType)
		}
	}
	if meta == nil || len(raw) == 0 {
		t.Fatalf("incomplete upload meta=%#v raw_bytes=%d", meta, len(raw))
	}
	return meta, raw
}

func rawOnlyFixtureMessage() map[string]any {
	return map[string]any{
		"id":           "raw-only",
		"threadId":     "thread-raw",
		"labelIds":     []string{"INBOX"},
		"snippet":      "attachment only",
		"internalDate": "1777046400000",
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []map[string]string{
				{"name": "Message-ID", "value": "<raw-only@example.test>"},
				{"name": "Subject", "value": "Attachment only"},
				{"name": "Date", "value": "Tue, 24 Aug 2026 12:00:00 +0000"},
			},
			"parts": []map[string]any{
				{
					"partId":   "1",
					"mimeType": "application/pdf",
					"filename": "document.pdf",
					"headers":  []map[string]string{{"name": "Content-Disposition", "value": `attachment; filename="document.pdf"`}},
					"body":     map[string]any{"attachmentId": "att-raw", "size": 128},
				},
			},
		},
	}
}

func previewValue(fields []provider.SetupPreviewField, name string) string {
	for _, field := range fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

type gmailAPIFixture struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	messageList []url.Values
}

func newGmailAPIFixture(t *testing.T) *gmailAPIFixture {
	t.Helper()
	f := &gmailAPIFixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *gmailAPIFixture) lastMessageListQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageList) == 0 {
		return ""
	}
	return f.messageList[len(f.messageList)-1].Get("q")
}

func (f *gmailAPIFixture) handle(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer gmail-access-token" {
		f.t.Fatalf("authorization header = %q", got)
	}
	if r.Method != http.MethodGet {
		f.t.Fatalf("method = %s", r.Method)
	}
	switch r.URL.Path {
	case "/gmail/v1/users/me/labels":
		writeJSON(w, map[string]any{"labels": []map[string]any{
			{"id": "INBOX", "name": "INBOX", "type": "system"},
			{"id": "UNREAD", "name": "UNREAD", "type": "system"},
			{"id": "STARRED", "name": "STARRED", "type": "system"},
			{"id": "Label_parent", "name": "Work", "type": "user"},
			{"id": "Label_1", "name": "Work/Project", "type": "user"},
		}})
	case "/gmail/v1/users/me/labels/INBOX":
		writeJSON(w, labelDetail("INBOX", "INBOX", "system", 12, 2, nil))
	case "/gmail/v1/users/me/labels/UNREAD":
		writeJSON(w, labelDetail("UNREAD", "UNREAD", "system", 2, 2, nil))
	case "/gmail/v1/users/me/labels/STARRED":
		writeJSON(w, labelDetail("STARRED", "STARRED", "system", 1, 0, nil))
	case "/gmail/v1/users/me/labels/Label_parent":
		writeJSON(w, labelDetail("Label_parent", "Work", "user", 3, 0, map[string]string{"textColor": "#000000", "backgroundColor": "#ffffff"}))
	case "/gmail/v1/users/me/labels/Label_1":
		writeJSON(w, labelDetail("Label_1", "Work/Project", "user", 1, 0, map[string]string{"textColor": "#123456", "backgroundColor": "#abcdef"}))
	case "/gmail/v1/users/me/messages":
		f.mu.Lock()
		f.messageList = append(f.messageList, r.URL.Query())
		f.mu.Unlock()
		writeJSON(w, map[string]any{"messages": []map[string]string{{"id": "m1", "threadId": "t1"}}})
	case "/gmail/v1/users/me/messages/m1":
		if r.URL.Query().Get("format") == "metadata" {
			writeJSON(w, map[string]any{"id": "m1", "threadId": "t1", "payload": map[string]any{"headers": []map[string]string{{"name": "Message-ID", "value": "<m1@example.test>"}}}})
			return
		}
		writeJSON(w, gmailFixtureMessage("m1", "t1", "Plain body", "Quarterly invoice", 1777046400000))
	case "/gmail/v1/users/me/threads/t1":
		writeJSON(w, map[string]any{"id": "t1", "messages": []map[string]any{
			gmailFixtureMessage("m1", "t1", "Plain body", "Quarterly invoice", 1777046400000),
		}})
	default:
		http.NotFound(w, r)
	}
}

func labelDetail(id, name, typ string, total, unread int, color map[string]string) map[string]any {
	out := map[string]any{
		"id":                    id,
		"name":                  name,
		"type":                  typ,
		"messageListVisibility": "show",
		"labelListVisibility":   "labelShow",
		"messagesTotal":         total,
		"messagesUnread":        unread,
	}
	if color != nil {
		out["color"] = color
	}
	return out
}

func gmailFixtureMessage(id, threadID, body, subject string, internalDate int64) map[string]any {
	data := base64.RawURLEncoding.EncodeToString([]byte(body))
	return map[string]any{
		"id":           id,
		"threadId":     threadID,
		"labelIds":     []string{"INBOX", "UNREAD", "STARRED", "Label_1"},
		"snippet":      "preview",
		"internalDate": strconv.FormatInt(internalDate, 10),
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []map[string]string{
				{"name": "Message-ID", "value": "<m1@example.test>"},
				{"name": "From", "value": "Sender <sender@example.test>"},
				{"name": "To", "value": "Recipient <recipient@example.test>"},
				{"name": "Subject", "value": subject},
				{"name": "Date", "value": "Tue, 24 Aug 2026 12:00:00 +0000"},
			},
			"parts": []map[string]any{
				{"partId": "0", "mimeType": "text/plain", "body": map[string]any{"data": data, "size": len(body)}},
				{"partId": "1", "mimeType": "application/pdf", "filename": "invoice.pdf", "headers": []map[string]string{{"name": "Content-Disposition", "value": `attachment; filename="invoice.pdf"`}}, "body": map[string]any{"attachmentId": "att-1", "size": 128}},
			},
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func assertProductCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("error code = %v, want %s", err, code)
	}
}

func assertNotProductCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if errors.As(err, &product) && product.Code() == code {
		t.Fatalf("error code = %s, did not want %s", product.Code(), code)
	}
}

func assertProductReason(t *testing.T, err error, reason string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %v, want product reason %s", err, reason)
	}
	if got := product.Details()["reason"]; got != reason {
		t.Fatalf("error reason = %#v, want %s", got, reason)
	}
}

func stringPtr(value string) *string {
	return &value
}

func int64Ptr(value int64) *int64 {
	return &value
}

func folderIDs(folders []mail.Folder) []string {
	out := make([]string, len(folders))
	for i, folder := range folders {
		out[i] = folder.ID
	}
	sort.Strings(out)
	return out
}

func messageSummaryIDs(messages []mail.MessageSummary) []string {
	out := make([]string, len(messages))
	for i, message := range messages {
		out[i] = message.ID
	}
	return out
}

func sameStrings(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

type signInRuntime struct {
	env     map[string]string
	notices []string
	opened  int
}

func (r *signInRuntime) RegisterSecret(string) error { return nil }
func (r *signInRuntime) LookupSecretEnv(name string) (string, bool, error) {
	v, ok := r.env[name]
	return v, ok, nil
}
func (r *signInRuntime) OpenBrowser(context.Context, string) error {
	r.opened++
	return nil
}
func (r *signInRuntime) WriteNotice(text string) error {
	r.notices = append(r.notices, text)
	return nil
}

func gmailSignIn(p *Provider, r *signInRuntime) error {
	_, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   r,
		Operation: provider.InteractiveLogin,
		Provider:  "gmail",
		Account:   "personal",
		Setup:     maildoconfig.ProviderSetup{Gmail: &maildoconfig.GmailSetup{ClientID: "client-id.apps.googleusercontent.com"}},
	})
	return err
}

// Google refuses a Desktop app's sign-in without its secret, but only after
// the consent screen, so maildo checks before it opens the browser.
func TestGmailSignInWithoutClientSecretStopsBeforeTheBrowser(t *testing.T) {
	r := &signInRuntime{}
	err := gmailSignIn(New(Options{}), r)
	assertProductCode(t, err, errs.CodeAuthAppConfigMissing)
	if !strings.Contains(err.Error(), "MAILDO_GMAIL_CLIENT_SECRET") || r.opened != 0 || len(r.notices) != 0 {
		t.Fatalf("err = %v, browser opened %d times, notices %q", err, r.opened, r.notices)
	}
}

func TestGmailSignInShowsTheAddressWhileItWaits(t *testing.T) {
	r := &signInRuntime{env: map[string]string{"MAILDO_GMAIL_CLIENT_SECRET": "client-secret"}}
	err := gmailSignIn(New(Options{LoopbackTimeout: 10 * time.Millisecond}), r)
	if err == nil {
		t.Fatal("sign-in without a callback succeeded")
	}
	got := strings.Join(r.notices, "\n")
	if r.opened != 1 || !strings.Contains(got, "visit this address:\nhttps://accounts.google.com/") || !strings.Contains(got, "Waiting for you to finish signing in") {
		t.Fatalf("browser opened %d times, notices:\n%s", r.opened, got)
	}
}
