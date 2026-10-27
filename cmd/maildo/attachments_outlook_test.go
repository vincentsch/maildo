package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

func TestOutlookMessageAttachmentsListMetadataRequest(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var listed messageAttachmentsResult
	decodeOK(t, h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json"), &listed)
	if listed.Count != 4 || listed.Attachments[0].ProviderType != "#microsoft.graph.fileAttachment" ||
		listed.Attachments[3].Kind != mail.AttachmentKindItem || listed.ProviderMeta == nil || listed.ProviderMeta.Source != mail.MetadataSourceMicrosoftGraph {
		t.Fatalf("listed = %#v", listed)
	}
	req := h.server.lastMessageRequest()
	if req.Method != http.MethodGet || req.Path != "/me/messages/download-source/attachments" {
		t.Fatalf("metadata request = %#v", req)
	}
	if got := req.Query.Get("$select"); got != "id,name,contentType,size,isInline,contentId" ||
		strings.Contains(got, "contentBytes") || strings.Contains(got, "@odata.type") {
		t.Fatalf("metadata select = %q", got)
	}
	plain := h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--plain")
	decodeOK(t, plain, nil)
	if len(strings.Split(strings.TrimSpace(plain.Stdout), "\n")) != 4 ||
		len(strings.Split(strings.Split(strings.TrimSpace(plain.Stdout), "\n")[0], "\t")) != 8 {
		t.Fatalf("plain output = %q", plain.Stdout)
	}
}

func TestOutlookMessageAttachmentsInternetIDResolutionIsMetadataOnly(t *testing.T) {
	h := seededOutlookMessagesHarness(t)

	var listed messageAttachmentsResult
	decodeOK(t, h.runNoBrowser("attachments", "list", "--internet-id", "<single@example.test>", "--account", "work", "--json"), &listed)
	if listed.SourceID != "internet-single" || listed.SourceInternetID == "" {
		t.Fatalf("internet-id list = %#v", listed)
	}
	reqs := h.server.messageRequestsSnapshot()
	if len(reqs) < 2 {
		t.Fatalf("requests = %#v", reqs)
	}
	resolve := reqs[0]
	if resolve.Path != "/me/messages" || !strings.Contains(resolve.Query.Get("$filter"), "internetMessageId eq ") ||
		resolve.Query.Get("$select") != "id,internetMessageId" || resolve.Prefer != "" {
		t.Fatalf("internet-id resolution request = %#v", resolve)
	}
	if strings.Contains(resolve.Query.Get("$select"), "body") || strings.Contains(resolve.Query.Get("$select"), "internetMessageHeaders") {
		t.Fatalf("metadata-only select leaked body/header fields: %q", resolve.Query.Get("$select"))
	}
	metadata := reqs[len(reqs)-1]
	if metadata.Path != "/me/messages/internet-single/attachments" {
		t.Fatalf("attachment metadata request = %#v", metadata)
	}
}

func TestOutlookMessageAttachmentsDownloadStreamsAndEscapes(t *testing.T) {
	h := seededOutlookMessagesHarness(t)
	outDir := filepath.Join(t.TempDir(), "out")

	var result messageAttachmentDownloadResult
	decodeOK(t, h.runNoBrowser("attachments", "download", "--id", "download-source", "--all", "--to", outDir, "--account", "work", "--json"), &result)
	if result.DownloadedCount != 3 || result.SkippedCount != 1 || result.FailedCount != 0 {
		t.Fatalf("download result = %#v", result)
	}
	for name, want := range map[string]string{
		"download.txt": "bytes for download-source/download-file",
		"slash.txt":    "bytes for download-source/slash/id",
	} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q err=%v", name, got, err)
		}
	}
	zero, err := os.ReadFile(filepath.Join(outDir, "zero.bin"))
	if err != nil || len(zero) != 0 {
		t.Fatalf("zero attachment = %d err=%v", len(zero), err)
	}
	reqs := h.server.messageRequestsSnapshot()
	foundEscaped := false
	for _, req := range reqs {
		if strings.Contains(req.EscapedPath, "/attachments/slash%2Fid/$value") {
			foundEscaped = true
		}
	}
	if !foundEscaped {
		t.Fatalf("escaped slash attachment download not recorded: %#v", reqs)
	}
}

func TestOutlookMessageAttachmentsErrorsAndHostileLinks(t *testing.T) {
	t.Run("metadata not found", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		assertErrorCodeExit(t, h.runNoBrowser("attachments", "list", "--id", "attachments-missing", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)
		assertErrorCodeExit(t, h.runNoBrowser("attachments", "list", "--id", "attachments-gone", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)
	})

	t.Run("metadata malformed and oversized success", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.malformedMessages.Store(true)
		malformed := h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json")
		if malformed.Exit == 0 || malformed.Stdout != "" || strings.Contains(malformed.Stderr, `{"id":`) {
			t.Fatalf("malformed metadata result exit=%d stdout=%s stderr=%s", malformed.Exit, malformed.Stdout, malformed.Stderr)
		}

		h = seededOutlookMessagesHarness(t)
		h.server.oversizedMessages.Store(true)
		oversized := h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json")
		if oversized.Exit == 0 || oversized.Stdout != "" || !strings.Contains(oversized.Stderr, "message body exceeds the read size limit") ||
			strings.Contains(oversized.Stderr, strings.Repeat("{", 128)) {
			t.Fatalf("oversized metadata result exit=%d stdout=%s stderr=%s", oversized.Exit, oversized.Stdout, oversized.Stderr)
		}
	})

	t.Run("metadata oversized status bodies keep status class", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.messageStatus.Store(http.StatusNotFound)
		h.server.oversizedStatusBodies.Store(true)
		assertErrorCodeExit(t, h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json"), errs.CodeMessageNotFound, 5)

		h = seededOutlookMessagesHarness(t)
		h.server.messageStatus.Store(http.StatusTooManyRequests)
		h.server.oversizedStatusBodies.Store(true)
		rate := h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json")
		if rate.Exit != rungrad.ExitRateLimited || rate.Stdout != "" || !strings.Contains(rate.Stderr, "rate limited") ||
			strings.Contains(rate.Stderr, "message body exceeds the read size limit") || strings.Contains(rate.Stderr, strings.Repeat("{", 128)) {
			t.Fatalf("rate metadata result exit=%d stdout=%s stderr=%s", rate.Exit, rate.Stdout, rate.Stderr)
		}

		h = seededOutlookMessagesHarness(t)
		h.server.messageStatus.Store(http.StatusInternalServerError)
		h.server.oversizedStatusBodies.Store(true)
		server := h.runNoBrowser("attachments", "list", "--id", "download-source", "--account", "work", "--json")
		if server.Exit == 0 || server.Stdout != "" || !strings.Contains(server.Stderr, "could not search or list messages") ||
			strings.Contains(server.Stderr, "message body exceeds the read size limit") || strings.Contains(server.Stderr, strings.Repeat("{", 128)) {
			t.Fatalf("server metadata result exit=%d stdout=%s stderr=%s", server.Exit, server.Stdout, server.Stderr)
		}
	})

	for _, tt := range []struct {
		name string
		id   string
		code string
		exit int
	}{
		{name: "download missing", id: "missing-download", code: errs.CodeAttachmentNotFound, exit: 5},
		{name: "download gone", id: "gone-download", code: errs.CodeAttachmentNotFound, exit: 5},
		{name: "download auth 401", id: "auth-401", code: errs.CodeAuthRepairRequired, exit: 3},
		{name: "download auth 403", id: "auth-403", code: errs.CodeAuthRepairRequired, exit: 3},
		{name: "oversized", id: "oversized-download", code: errs.CodeAttachmentTooLarge, exit: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := seededOutlookMessagesHarness(t)
			res := h.runNoBrowser("attachments", "download", "--id", "download-status", "--attachment-id", tt.id, "--to", filepath.Join(t.TempDir(), "out"), "--account", "work", "--json")
			assertErrorCodeExit(t, res, tt.code, tt.exit)
			if res.Stdout != "" {
				t.Fatalf("provider failure wrote stdout: %s", res.Stdout)
			}
		})
	}

	t.Run("short download body", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		outDir := filepath.Join(t.TempDir(), "out")
		res := h.runNoBrowser("attachments", "download", "--id", "download-status", "--attachment-id", "short-download", "--to", outDir, "--account", "work", "--json")
		if res.Exit == 0 || res.Stdout != "" {
			t.Fatalf("short body result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
		if _, err := os.Stat(filepath.Join(outDir, "short.txt")); !os.IsNotExist(err) {
			t.Fatalf("short body left final file: %v", err)
		}
		if matches, _ := filepath.Glob(filepath.Join(outDir, ".*.tmp-*")); len(matches) != 0 {
			t.Fatalf("short body left temp files: %#v", matches)
		}
	})

	t.Run("rate and server", func(t *testing.T) {
		for _, id := range []string{"rate-download", "server-download"} {
			h := seededOutlookMessagesHarness(t)
			if id == "server-download" {
				h.server.oversizedStatusBodies.Store(true)
			}
			res := h.runNoBrowser("attachments", "download", "--id", "download-status", "--attachment-id", id, "--to", filepath.Join(t.TempDir(), "out"), "--account", "work", "--json")
			if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stderr, "message body exceeds the read size limit") ||
				strings.Contains(res.Stderr, strings.Repeat("{", 128)) {
				t.Fatalf("%s exit=%d stdout=%s stderr=%s", id, res.Exit, res.Stdout, res.Stderr)
			}
		}
	})

	t.Run("hostile attachment page", func(t *testing.T) {
		h := seededOutlookMessagesHarness(t)
		h.server.hostileAttachmentLink.Store(true)
		res := h.runNoBrowser("attachments", "list", "--id", "read-id", "--account", "work", "--json")
		if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stderr, "example.invalid") {
			t.Fatalf("hostile attachment link result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
		}
	})
}
