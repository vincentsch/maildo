package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestDraftAttachmentsListPlainAndMetadataOnly(t *testing.T) {
	h, fake := seededAttachmentHarness(t)

	var listed draftAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--draft-id", "seed-draft", "--json"), &listed)
	if listed.Count != 9 || fake.Drafts.MetadataOnlyReadCalls.Load() != 1 || listed.Attachments[0].Kind != mail.AttachmentKindFile {
		t.Fatalf("listed = %#v metadata=%d", listed, fake.Drafts.MetadataOnlyReadCalls.Load())
	}
	plain := h.run("attachments", "list", "--draft-id", "seed-draft", "--plain")
	decodeOK(t, plain, nil)
	lines := strings.Split(strings.TrimSpace(plain.Stdout), "\n")
	if len(lines) != 9 || len(strings.Split(lines[0], "\t")) != 6 || strings.Contains(plain.Stdout, "unsafe\tname") || strings.Contains(plain.Stdout, "line\nbreak") {
		t.Fatalf("plain output = %q", plain.Stdout)
	}
	human := h.run("attachments", "list", "--draft-id", "seed-draft")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "9 attachments on the draft (work):") ||
		strings.Contains(human.Stdout+human.Stderr, "body must not appear") ||
		strings.Contains(human.Stdout, "unsafe\tname") ||
		strings.Contains(human.Stdout, "line\nbreak") {
		t.Fatalf("human list output = %q stderr=%s", human.Stdout, human.Stderr)
	}
	jq := h.run("attachments", "list", "--draft-id", "seed-draft", "--json", "--jq", ".count")
	if strings.TrimSpace(jq.Stdout) != "9" {
		t.Fatalf("jq stdout=%q stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("attachments", "list", "--draft-id", "seed-draft", "--template", "{{.count}}")
	if strings.TrimSpace(tmpl.Stdout) != "9" {
		t.Fatalf("template stdout=%q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	var envelope struct {
		Data draftAttachmentsResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("attachments", "list", "--draft-id", "seed-draft", "--json", "--include-meta"), &envelope)
	if envelope.Data.Count != 9 || envelope.Meta.Extra["command_path"] != "maildo attachments list" {
		t.Fatalf("meta envelope = %#v", envelope)
	}
	dry := h.run("attachments", "list", "--draft-id", "seed-draft", "--dry-run", "--json")
	decodeOK(t, dry, &draftAttachmentsResult{})
}

func TestDraftAttachmentsAddValidationDryRunAndLive(t *testing.T) {
	h, fake := seededAttachmentHarness(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "quote.pdf")
	if err := os.WriteFile(file, []byte("PDF DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "unknown.weirdext")
	if err := os.WriteFile(unknown, []byte("fallback"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview := decodePreview(t, h.run("attachments", "add", "--draft-id", "seed-draft", file, "--dry-run", "--json"))
	if fake.Drafts.AttachmentAddCalls.Load() != 0 || !strings.Contains(previewString(preview), "quote.pdf") ||
		strings.Contains(previewString(preview), file) || strings.Contains(previewString(preview), "PDF DATA") {
		t.Fatalf("add preview=%#v addCalls=%d", preview, fake.Drafts.AttachmentAddCalls.Load())
	}

	var added draftAttachmentAddResult
	decodeOK(t, h.run("attachments", "add", "--draft-id", "seed-draft", file, empty, unknown, "--json"), &added)
	if len(added.Added) != 3 || added.AttachmentCount != 12 || fake.Drafts.AttachmentAddCalls.Load() != 1 || strings.Contains(previewString(map[string]any{"data": added}), file) {
		t.Fatalf("added = %#v calls=%d", added, fake.Drafts.AttachmentAddCalls.Load())
	}
	uploadsByName := map[string]mail.AttachmentUpload{}
	for _, upload := range added.Uploads {
		if len(upload.Bytes) != 0 {
			t.Fatalf("upload bytes leaked in result: %#v", added.Uploads)
		}
		uploadsByName[upload.Name] = upload
	}
	if uploadsByName["quote.pdf"].ContentType != "application/pdf" ||
		uploadsByName["empty.bin"].Size != 0 ||
		uploadsByName["unknown.weirdext"].ContentType != "application/octet-stream" ||
		uploadsByName["unknown.weirdext"].Size != int64(len("fallback")) {
		t.Fatalf("safe upload summaries = %#v", added.Uploads)
	}
	humanAddHarness, _ := seededAttachmentHarness(t)
	humanAdd := humanAddHarness.run("attachments", "add", "--draft-id", "seed-draft", file)
	if humanAdd.Exit != 0 || !strings.HasPrefix(humanAdd.Stdout, "Added quote.pdf to the draft \"Seed\" in work. It now has ") ||
		strings.Contains(humanAdd.Stdout+humanAdd.Stderr, file) ||
		strings.Contains(humanAdd.Stdout+humanAdd.Stderr, "PDF DATA") {
		t.Fatalf("human add stdout=%q stderr=%s", humanAdd.Stdout, humanAdd.Stderr)
	}

	dupDir := t.TempDir()
	dupA := filepath.Join(dupDir, "same.txt")
	dupBDir := filepath.Join(dupDir, "nested")
	if err := os.Mkdir(dupBDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dupB := filepath.Join(dupBDir, "same.txt")
	if err := os.WriteFile(dupA, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dupB, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, h.run("attachments", "add", "--draft-id", "seed-draft", dupA, dupB, "--json"), errs.CodeInvalidComposeInput)
	link := filepath.Join(dupDir, "alias.txt")
	if err := os.Symlink(dupA, link); err == nil {
		assertErrorCode(t, h.run("attachments", "add", "--draft-id", "seed-draft", dupA, link, "--json"), errs.CodeInvalidComposeInput)
	}
	assertErrorCode(t, h.run("attachments", "add", "--draft-id", "seed-draft", dir, "--json"), errs.CodeInvalidComposeInput)
	large := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(large, make([]byte, mail.MaxSimpleAttachmentBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, h.run("attachments", "add", "--draft-id", "seed-draft", large, "--json"), errs.CodeAttachmentTooLarge)
}

func TestDraftAttachmentsRemoveValidationDryRunAndLive(t *testing.T) {
	h, fake := seededAttachmentHarness(t)
	preview := decodePreview(t, h.run("attachments", "remove", "--draft-id", "seed-draft", "--name", "contract.pdf", "--dry-run", "--json"))
	if fake.Drafts.AttachmentRemoveCalls.Load() != 0 || preview["body"].(map[string]any)["attachment_name"] != "contract.pdf" {
		t.Fatalf("remove preview=%#v calls=%d", preview, fake.Drafts.AttachmentRemoveCalls.Load())
	}

	var removed draftAttachmentRemoveResult
	decodeOK(t, h.run("attachments", "remove", "--draft-id", "seed-draft", "--name", "contract.pdf", "--json"), &removed)
	if removed.Removed.ID != "att-file" || removed.AttachmentCount != 8 || fake.Drafts.AttachmentRemoveCalls.Load() != 1 {
		t.Fatalf("removed = %#v calls=%d", removed, fake.Drafts.AttachmentRemoveCalls.Load())
	}
	idHarness, _ := seededAttachmentHarness(t)
	var removedByID draftAttachmentRemoveResult
	decodeOK(t, idHarness.run("attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-dup-a", "--json"), &removedByID)
	if removedByID.Removed.ID != "att-dup-a" || removedByID.AttachmentCount != 8 {
		t.Fatalf("removed by id with duplicate names = %#v", removedByID)
	}
	humanHarness, _ := seededAttachmentHarness(t)
	humanRemove := humanHarness.run("attachments", "remove", "--draft-id", "seed-draft", "--name", "contract.pdf")
	if humanRemove.Exit != 0 || !strings.HasPrefix(humanRemove.Stdout, "Removed contract.pdf from the draft \"Seed\" in work. It now has ") ||
		strings.Contains(humanRemove.Stdout+humanRemove.Stderr, "body must not appear") {
		t.Fatalf("human remove stdout=%q stderr=%s", humanRemove.Stdout, humanRemove.Stderr)
	}

	for _, tt := range []struct {
		name string
		args []string
		code string
		exit int
	}{
		{name: "ambiguous", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--name", "duplicate.txt", "--json"}, code: errs.CodeAmbiguousAttachment, exit: 1},
		{name: "missing", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--name", "missing.txt", "--json"}, code: errs.CodeAttachmentNotFound, exit: 5},
		{name: "inline", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-inline", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
		{name: "item", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-item", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
		{name: "reference", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-reference", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
		{name: "unknown", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-unknown", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
		{name: "undecoded", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--attachment-id", "att-undecoded", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
		{name: "target conflict", args: []string{"attachments", "remove", "--draft-id", "seed-draft", "--name", "x", "--attachment-id", "y", "--json"}, code: errs.CodeInvalidComposeInput, exit: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := seededAttachmentHarness(t)
			assertErrorCodeExit(t, h.run(tt.args...), tt.code, tt.exit)
		})
	}

	stale := testkit.NewFakeStaleDraftAttachmentProvider("fake")
	stale.Drafts = testkit.NewDraftState(seededAttachmentDraft(t))
	staleHarness := seedAttachmentHarness(t, stale)
	dir := t.TempDir()
	file := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(file, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, staleHarness.run("attachments", "add", "--draft-id", "seed-draft", file, "--json"), errs.CodeCapabilityNotImplemented)
	assertErrorCode(t, staleHarness.run("attachments", "remove", "--draft-id", "seed-draft", "--name", "contract.pdf", "--json"), errs.CodeCapabilityNotImplemented)
	if stale.Drafts.AttachmentAddCalls.Load() != 0 || stale.Drafts.AttachmentRemoveCalls.Load() != 0 {
		t.Fatalf("stale attachment manager mutated add=%d remove=%d", stale.Drafts.AttachmentAddCalls.Load(), stale.Drafts.AttachmentRemoveCalls.Load())
	}
}

func TestDraftAttachmentsPartialUploadFailureDetails(t *testing.T) {
	h, fake := seededAttachmentHarness(t)
	fake.Drafts.FailAddAfter.Store(1)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(first, []byte("first secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.run("attachments", "add", "--draft-id", "seed-draft", first, second, "--json")
	if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stdout+res.Stderr, first) || strings.Contains(res.Stdout+res.Stderr, "second secret") {
		t.Fatalf("partial failure stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	body := errorJSON(t, res)
	if body["draft_id"] != "seed-draft" || body["partial"] != true || body["failed_file_name"] != "second.txt" {
		t.Fatalf("partial details = %#v", body)
	}
}

func TestDraftAttachmentsAddFinalMetadataFailureIsNotPartial(t *testing.T) {
	h, fake := seededAttachmentHarness(t)
	fake.Drafts.FailAddAfterAll.Store(true)
	dir := t.TempDir()
	file := filepath.Join(dir, "done.txt")
	if err := os.WriteFile(file, []byte("done secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := h.run("attachments", "add", "--draft-id", "seed-draft", file, "--json")
	if res.Exit == 0 || res.Stdout != "" || strings.Contains(res.Stdout+res.Stderr, file) || strings.Contains(res.Stdout+res.Stderr, "done secret") {
		t.Fatalf("final metadata failure stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	body := errorJSON(t, res)
	if _, ok := body["partial"]; ok || strings.Contains(res.Stderr, "before the failure") {
		t.Fatalf("final metadata failure mislabeled as partial: %#v", body)
	}
}

func TestMessageAttachmentsListSelectorsPlainAndMeta(t *testing.T) {
	h, fake := seededMessageAttachmentHarness(t)

	var byID messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--id", "work-new", "--json"), &byID)
	assertSelection(t, byID.AccountSelection, "single", "work")
	if byID.SourceID != "work-new" || byID.SourceInternetID == "" || byID.Count != len(messageAttachmentRows()) ||
		byID.Attachments[0].Kind != mail.AttachmentKindFile {
		t.Fatalf("message attachment list = %#v", byID)
	}
	plain := h.run("attachments", "list", "--id", "work-new", "--plain")
	decodeOK(t, plain, nil)
	lines := strings.Split(strings.TrimSpace(plain.Stdout), "\n")
	if len(lines) != len(messageAttachmentRows()) || len(strings.Split(lines[0], "\t")) != 8 {
		t.Fatalf("plain message attachment output = %q", plain.Stdout)
	}

	var byInternet messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--internet-id", "<work-new@example.test>", "--account", "work", "--json"), &byInternet)
	if byInternet.SourceID != "work-new" || fake.MessageMetadataOnlyReadCalls.Load() != 1 {
		t.Fatalf("internet-id result=%#v metadata-only=%d", byInternet, fake.MessageMetadataOnlyReadCalls.Load())
	}

	var query messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--query", "anything", "--latest", "--all-accounts", "--json"), &query)
	assertSelection(t, query.AccountSelection, "single", "work")
	if query.SourceID != "work-new" {
		t.Fatalf("query selected = %#v", query)
	}
	var envelope struct {
		Data messageAttachmentsResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("attachments", "list", "--query", "anything", "--latest", "--all-accounts", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["searched_scope"] != "all" || envelope.Meta.Extra["selector_kind"] != string(mail.SelectorQuery) {
		t.Fatalf("message attachment meta = %#v", envelope.Meta.Extra)
	}

	var first messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--query", "anything", "--first", "--account", "personal", "--json"), &first)
	if first.SourceID != "personal-new" || first.Count != 1 {
		t.Fatalf("query first selected = %#v", first)
	}
}

func TestMessageAttachmentsListEscapesProviderFields(t *testing.T) {
	rows := []mail.Attachment{{
		ID:           "id\tone",
		Name:         "line\nbreak\rname-" + strings.Repeat("x", 80),
		ContentType:  "text/plain\todd",
		Size:         3,
		ContentID:    "cid\none",
		Kind:         mail.AttachmentKindFile,
		ProviderType: "#fake\rfile",
	}}
	h, _ := seededMessageAttachmentHarnessWithRows(t, rows, map[string]string{"id\tone": "abc"})

	plain := h.run("attachments", "list", "--id", "work-new", "--plain")
	decodeOK(t, plain, nil)
	lines := strings.Split(strings.TrimSpace(plain.Stdout), "\n")
	if len(lines) != 1 || strings.Contains(lines[0], "\r") || len(strings.Split(lines[0], "\t")) != 8 {
		t.Fatalf("plain provider fields were not copy-safe: %q", plain.Stdout)
	}
	if strings.Contains(plain.Stdout, "line\nbreak") || strings.Contains(plain.Stdout, "cid\none") {
		t.Fatalf("plain output leaked raw multiline fields: %q", plain.Stdout)
	}

	human := h.run("attachments", "list", "--id", "work-new")
	decodeOK(t, human, nil)
	if strings.Contains(human.Stdout, "line\nbreak") || strings.Contains(human.Stdout, "cid\none") || strings.Contains(human.Stdout, "\r") {
		t.Fatalf("human output leaked raw provider control characters: %q", human.Stdout)
	}

	if strings.TrimSpace(h.run("attachments", "list", "--id", "work-new", "--json", "--jq", ".count").Stdout) != "1" {
		t.Fatal("jq list count mismatch with hostile provider metadata")
	}
	if strings.TrimSpace(h.run("attachments", "list", "--id", "work-new", "--template", "{{.count}}").Stdout) != "1" {
		t.Fatal("template list count mismatch with hostile provider metadata")
	}
}

func TestMessageAttachmentsJSONPreservesSizeFacts(t *testing.T) {
	externalPresent := true
	gmailMeta, err := mail.NewProviderMetadata(errs.ProviderGmail, mail.MetadataAttachment, mail.AttachmentProviderMetadata{
		Source:                      mail.MetadataSourceGmailAPI,
		SnapshotMessageID:           "work-new",
		ExternalAttachmentIDPresent: &externalPresent,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []mail.Attachment{
		{
			ID:          "att-empty",
			Name:        "empty.txt",
			ContentType: "text/plain",
			Kind:        mail.AttachmentKindFile,
			SizeFacts: mail.AttachmentSizeFacts{
				EncodedSize:      0,
				EncodedSizeKnown: false,
				DecodedSize:      0,
				DecodedSizeKnown: true,
			},
			SizeFactsPresent: true,
		},
		{
			ID:           "att-inline",
			Name:         "inline.txt",
			ContentType:  "text/plain",
			Kind:         mail.AttachmentKindFile,
			ProviderMeta: gmailMeta,
			SizeFacts: mail.AttachmentSizeFacts{
				EncodedSize:      4,
				EncodedSizeKnown: true,
				DecodedSize:      3,
				DecodedSizeKnown: true,
			},
			SizeFactsPresent: true,
		},
	}
	h, _ := seededMessageAttachmentHarnessWithRows(t, rows, map[string]string{"att-empty": "", "att-inline": "abc"})

	list := h.run("attachments", "list", "--id", "work-new", "--json")
	decodeOK(t, list, nil)
	listRows := attachmentRowsFromJSON(t, list.Stdout)
	if len(listRows) != 2 {
		t.Fatalf("list rows = %#v", listRows)
	}
	assertJSONField(t, listRows[0], "encoded_size", float64(0))
	assertJSONField(t, listRows[0], "encoded_size_known", false)
	assertJSONField(t, listRows[0], "decoded_size", float64(0))
	assertJSONField(t, listRows[0], "decoded_size_known", true)
	assertJSONField(t, listRows[1], "encoded_size", float64(4))
	assertJSONField(t, listRows[1], "encoded_size_known", true)
	assertGmailAttachmentJSONMetadata(t, listRows[1])

	outDir := filepath.Join(t.TempDir(), "downloads")
	download := h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-inline", "--to", outDir, "--json")
	decodeOK(t, download, nil)
	downloadRows := attachmentRowsFromJSON(t, download.Stdout)
	if len(downloadRows) != 1 {
		t.Fatalf("download rows = %#v", downloadRows)
	}
	assertJSONField(t, downloadRows[0], "encoded_size", float64(4))
	assertJSONField(t, downloadRows[0], "encoded_size_known", true)
	assertJSONField(t, downloadRows[0], "decoded_size", float64(3))
	assertJSONField(t, downloadRows[0], "decoded_size_known", true)
	assertGmailAttachmentJSONMetadata(t, downloadRows[0])
}

func TestMessageAttachmentsQuerySelectorsAreFresh(t *testing.T) {
	h, fake := seededMessageAttachmentHarness(t)

	var first messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--query", "anything", "--latest", "--all-accounts", "--json"), &first)
	if first.SourceID != "work-new" {
		t.Fatalf("initial query selected = %#v", first)
	}

	newestAt := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	newest := fake.MessagesByAccount["work"][0]
	newest.ID = "work-newer"
	newest.InternetMessageID = "<work-newer@example.test>"
	newest.ReceivedAt = &newestAt
	fake.MessagesByAccount["work"] = append([]mail.MessageSummary{newest}, fake.MessagesByAccount["work"]...)
	fake.MessageAttachmentsByAccount["work"]["work-newer"] = []mail.Attachment{
		{ID: "fresh-file", Name: "fresh.txt", ContentType: "text/plain", Size: 5, Kind: mail.AttachmentKindFile},
	}
	fake.MessageAttachmentContentByAccount["work"]["work-newer"] = map[string]testkit.FakeAttachmentContent{
		"fresh-file": {Bytes: []byte("fresh"), ContentType: "text/plain"},
	}

	var second messageAttachmentsResult
	decodeOK(t, h.run("attachments", "list", "--query", "anything", "--latest", "--all-accounts", "--json"), &second)
	if second.SourceID != "work-newer" || second.Count != 1 || second.Attachments[0].ID != "fresh-file" {
		t.Fatalf("fresh query selected = %#v", second)
	}

	outDir := filepath.Join(t.TempDir(), "fresh")
	var downloaded messageAttachmentDownloadResult
	decodeOK(t, h.run("attachments", "download", "--query", "anything", "--latest", "--attachment-id", "fresh-file", "--to", outDir, "--json"), &downloaded)
	if downloaded.SourceID != "work-newer" || downloaded.DownloadedCount != 1 {
		t.Fatalf("fresh download = %#v", downloaded)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "fresh.txt"))
	if err != nil || string(got) != "fresh" {
		t.Fatalf("fresh file = %q err=%v", got, err)
	}
}

func TestMessageAttachmentsSelectorValidationAndCapabilities(t *testing.T) {
	h, _ := seededMessageAttachmentHarness(t)
	assertErrorCode(t, h.run("attachments", "list", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "list", "--id", "work-new", "--internet-id", "<work-new@example.test>", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "list", "--draft-id", "seed-draft", "--id", "work-new", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "list", "--id", "work-new", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "list", "--id", "work-new", "--all-accounts", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "download", "--query", "anything", "--all", "--to", t.TempDir(), "--json"), errs.CodeInvalidSelector)

	draftHarness, _ := seededAttachmentHarness(t)
	assertErrorCode(t, draftHarness.run("attachments", "list", "--draft-id", "seed-draft", "--all-accounts", "--json"), errs.CodeInvalidSelector)

	stale := testkit.NewFakeStaleMessageAttachmentProvider("fake")
	staleHarness := seedMessageAttachmentHarness(t, stale)
	assertErrorCode(t, staleHarness.run("attachments", "list", "--id", "work-new", "--json"), errs.CodeCapabilityNotImplemented)
}

func TestMessageAttachmentsDownloadSelectionDryRunAndFiles(t *testing.T) {
	h, fake := seededMessageAttachmentHarness(t)
	outDir := filepath.Join(t.TempDir(), "downloads")

	preview := decodePreview(t, h.run("attachments", "download", "--id", "work-new", "--all", "--to", outDir, "--dry-run", "--json"))
	if fake.MessageAttachmentListCalls.Load() != 1 || fake.MessageAttachmentDownloadCalls.Load() != 0 {
		t.Fatalf("dry-run calls list=%d download=%d", fake.MessageAttachmentListCalls.Load(), fake.MessageAttachmentDownloadCalls.Load())
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created output dir: %v", err)
	}
	if !strings.Contains(previewString(preview), "report.pdf") || strings.Contains(previewString(preview), "report bytes") {
		t.Fatalf("download preview leaked or omitted data: %#v", preview)
	}

	var result messageAttachmentDownloadResult
	decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--all", "--to", outDir, "--json"), &result)
	if result.SelectedCount != len(messageAttachmentRows()) || result.DownloadedCount != 7 || result.SkippedCount != 1 || result.FailedCount != 0 {
		t.Fatalf("download result = %#v", result)
	}
	for name, content := range map[string]string{
		"report.pdf":      "report bytes",
		"inline.png":      "inline bytes",
		"duplicate.txt":   "duplicate a",
		"duplicate-2.txt": "duplicate b",
		"path.txt":        "path bytes",
		"back.txt":        "back bytes",
		"attachment-008":  "empty name bytes",
	} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil || string(got) != content {
			t.Fatalf("downloaded %s = %q err=%v", name, got, err)
		}
	}
	if strings.Contains(previewString(map[string]any{"data": result}), "report bytes") {
		t.Fatalf("download result leaked bytes: %#v", result)
	}

	nameDir := filepath.Join(t.TempDir(), "by-name")
	var byName messageAttachmentDownloadResult
	decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--name", "inline.png", "--to", nameDir, "--json"), &byName)
	if byName.DownloadedCount != 1 || byName.Attachments[0].ID != "att-inline" {
		t.Fatalf("download by name = %#v", byName)
	}

	humanDir := filepath.Join(t.TempDir(), "human")
	human := h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", humanDir)
	decodeOK(t, human, nil)
	if !strings.HasPrefix(human.Stdout, "Saved 1 attachment to "+humanDir+".") ||
		!strings.Contains(human.Stdout, "report.pdf  12 bytes  saved") ||
		strings.Contains(human.Stdout, "report bytes") ||
		strings.Contains(human.Stdout, "\x00") {
		t.Fatalf("human download output exposed unsafe details: %q", human.Stdout)
	}

	if strings.TrimSpace(h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", filepath.Join(t.TempDir(), "tmpl"), "--template", "{{.downloaded_count}}").Stdout) != "1" {
		t.Fatal("template download count mismatch")
	}
	if strings.TrimSpace(h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", filepath.Join(t.TempDir(), "jq"), "--json", "--jq", ".downloaded_count").Stdout) != "1" {
		t.Fatal("jq download count mismatch")
	}
	var envelope struct {
		Data messageAttachmentDownloadResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", filepath.Join(t.TempDir(), "meta"), "--json", "--include-meta"), &envelope)
	if envelope.Data.DownloadedCount != 1 ||
		envelope.Meta.Extra["operation"] != "attachments_download" ||
		envelope.Meta.Extra["downloaded_count"] != float64(1) {
		t.Fatalf("download include-meta = %#v meta=%#v", envelope.Data, envelope.Meta.Extra)
	}
}

func TestMessageAttachmentsDownloadValidationAndFilenamePlanning(t *testing.T) {
	h, fake := seededMessageAttachmentHarness(t)
	assertFrameworkUsage(t, h.run("attachments", "download", "--id", "work-new", "--all", "--json"))
	assertFrameworkUsage(t, h.run("attachments", "download", "--id", "work-new", "--all", "--to", "   ", "--json"))
	if fake.MessageAttachmentListCalls.Load() != 0 || fake.MessageAttachmentDownloadCalls.Load() != 0 {
		t.Fatalf("missing --to touched provider list=%d download=%d", fake.MessageAttachmentListCalls.Load(), fake.MessageAttachmentDownloadCalls.Load())
	}

	outDir := t.TempDir()
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--name", "duplicate.txt", "--to", filepath.Join(outDir, "dup"), "--json"), errs.CodeAmbiguousAttachment)
	assertErrorCodeExit(t, h.run("attachments", "download", "--id", "work-new", "--name", "missing.txt", "--to", filepath.Join(outDir, "missing"), "--json"), errs.CodeAttachmentNotFound, 5)
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-item", "--to", filepath.Join(outDir, "item"), "--json"), errs.CodeUnsupportedAttachment)
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--name", "report.pdf", "--name", "report.pdf", "--to", filepath.Join(outDir, "dupreq"), "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--attachment-id", "att-file", "--to", filepath.Join(outDir, "dupid"), "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "   ", "--to", filepath.Join(outDir, "emptyid"), "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("attachments", "download", "--id", "work-new", "--name", "   ", "--to", filepath.Join(outDir, "emptyname"), "--json"), errs.CodeInvalidSelector)

	existingDir := filepath.Join(outDir, "existing")
	if err := os.Mkdir(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existingDir, "report.pdf"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The error names the file that is in the way and how to replace it.
	exists := h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", existingDir, "--json")
	assertErrorCodeExit(t, exists, errs.CodeOutputFileExists, 1)
	if !strings.Contains(exists.Stderr, `"file": "report.pdf"`) || !strings.Contains(exists.Stderr, "--overwrite") {
		t.Fatalf("existing file error = %s", exists.Stderr)
	}
	decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", existingDir, "--overwrite", "--json"), &messageAttachmentDownloadResult{})
	replaced, err := os.ReadFile(filepath.Join(existingDir, "report.pdf"))
	if err != nil || string(replaced) != "report bytes" {
		t.Fatalf("overwrite result = %q err=%v", replaced, err)
	}
	dirConflict := filepath.Join(outDir, "dir-conflict")
	if err := os.Mkdir(dirConflict, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dirConflict, "report.pdf"), 0o700); err != nil {
		t.Fatal(err)
	}
	assertFrameworkUsage(t, h.run("attachments", "download", "--id", "work-new", "--attachment-id", "att-file", "--to", dirConflict, "--overwrite", "--json"))

	collisionRows := []mail.Attachment{
		{ID: "collision-a", Name: "report.txt", ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile},
		{ID: "collision-b", Name: "report.txt", ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile},
		{ID: "collision-c", Name: "report-2.txt", ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile},
	}
	collisionHarness, _ := seededMessageAttachmentHarnessWithRows(t, collisionRows, map[string]string{
		"collision-a": "a",
		"collision-b": "b",
		"collision-c": "c",
	})
	collisionDir := filepath.Join(outDir, "generated-collision")
	var collision messageAttachmentDownloadResult
	decodeOK(t, collisionHarness.run("attachments", "download", "--id", "work-new", "--all", "--to", collisionDir, "--overwrite", "--json"), &collision)
	if collision.DownloadedCount != 3 || len(collision.Attachments) != 3 {
		t.Fatalf("collision result = %#v", collision)
	}
	for name, want := range map[string]string{
		"report.txt":     "a",
		"report-2.txt":   "b",
		"report-2-2.txt": "c",
	} {
		got, err := os.ReadFile(filepath.Join(collisionDir, name))
		if err != nil || string(got) != want {
			t.Fatalf("collision file %s = %q err=%v", name, got, err)
		}
	}

	// An unsafe name is made safe, and the file is saved inside
	// the download folder.
	for name, want := range map[string]string{
		"../secret.txt":    "secret.txt",
		"../../etc/passwd": "passwd",
		"/abs.txt":         "abs.txt",
		`C:\abs.txt`:       "abs.txt",
		"bad\nname.txt":    "bad_name.txt",
		"..hidden":         "hidden",
		`what?<now>|.txt`:  "what__now__.txt",
	} {
		t.Run(fmt.Sprintf("unsafe_%q", name), func(t *testing.T) {
			h, _ := seededMessageAttachmentHarnessWithRows(t, []mail.Attachment{{ID: "unsafe", Name: name, ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile}}, map[string]string{"unsafe": "x"})
			out := filepath.Join(t.TempDir(), "out")
			var result messageAttachmentDownloadResult
			decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--all", "--to", out, "--json"), &result)
			if result.DownloadedCount != 1 || result.Attachments[0].SafeBasename != want {
				t.Fatalf("download of %q = %#v", name, result)
			}
			if got, err := os.ReadFile(filepath.Join(out, want)); err != nil || string(got) != "x" {
				t.Fatalf("saved file %s: %q %v", want, got, err)
			}
		})
	}

	for _, name := range []string{"CON", "nul.txt", "COM1.log"} {
		t.Run(fmt.Sprintf("reserved_%q", name), func(t *testing.T) {
			h, _ := seededMessageAttachmentHarnessWithRows(t, []mail.Attachment{{ID: "unsafe", Name: name, ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile}}, map[string]string{"unsafe": "x"})
			out := filepath.Join(t.TempDir(), "out")
			var result messageAttachmentDownloadResult
			decodeOK(t, h.run("attachments", "download", "--id", "work-new", "--all", "--to", out, "--json"), &result)
			if result.DownloadedCount != 1 || result.Attachments[0].SafeBasename != "attachment-001" {
				t.Fatalf("unsafe filename result = %#v", result)
			}
			if _, err := os.Stat(filepath.Join(out, "attachment-001")); err != nil {
				t.Fatalf("safe fallback file missing for %q: %v", name, err)
			}
			if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
				t.Fatalf("unsafe filename was created for %q: %v", name, err)
			}
		})
	}
}

func TestMessageAttachmentsPartialDownloadFailure(t *testing.T) {
	rows := []mail.Attachment{
		{ID: "first", Name: "first.txt", ContentType: "text/plain", Size: 5, Kind: mail.AttachmentKindFile},
		{ID: "second", Name: "second.txt", ContentType: "text/plain", Size: 6, Kind: mail.AttachmentKindFile},
		{ID: "third", Name: "third.txt", ContentType: "text/plain", Size: 5, Kind: mail.AttachmentKindFile},
	}
	h, fake := seededMessageAttachmentHarnessWithRows(t, rows, map[string]string{"first": "first", "third": "third"})
	fake.MessageAttachmentDownloadErrByAccount = map[string]map[string]map[string]error{
		"work": {
			"work-new": {
				"second": errs.NewWithDetails(errs.CodeAttachmentNotFound, "attachment not found", map[string]any{"attachment_id": "second"}),
			},
		},
	}
	outDir := filepath.Join(t.TempDir(), "partial")
	res := h.run("attachments", "download", "--id", "work-new", "--attachment-id", "first", "--attachment-id", "second", "--attachment-id", "third", "--to", outDir, "--json")
	if res.Exit == 0 || res.Stdout != "" {
		t.Fatalf("partial result exit=%d stdout=%s stderr=%s", res.Exit, res.Stdout, res.Stderr)
	}
	body := errorJSON(t, res)
	if body["code"] != errs.CodeAttachmentDownloadPartialFailure || body["underlying_error_code"] != errs.CodeAttachmentNotFound ||
		body["downloaded_count"] != float64(1) || body["failed_count"] != float64(1) {
		t.Fatalf("partial details = %#v", body)
	}
	attachments, ok := body["attachments"].([]any)
	if !ok || len(attachments) != 3 {
		t.Fatalf("partial attachments = %#v", body["attachments"])
	}
	for _, idx := range []int{1, 2} {
		row, ok := attachments[idx].(map[string]any)
		if !ok {
			t.Fatalf("partial row %d = %#v", idx, attachments[idx])
		}
		if _, ok := row["local_path"]; ok {
			t.Fatalf("partial row %d unexpectedly exposed local_path: %#v", idx, row)
		}
	}
	if got, err := os.ReadFile(filepath.Join(outDir, "first.txt")); err != nil || string(got) != "first" {
		t.Fatalf("first file = %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "second.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed final exists err=%v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(outDir, ".*.tmp-*")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %#v", matches)
	}
}

func TestMessageAttachmentWriteStreamFaultsCleanTempFiles(t *testing.T) {
	t.Run("declared size over cap", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large.bin")
		err := writeDownloadedAttachment(path, "large.bin", false, mail.MessageAttachmentContent{
			AttachmentID: "large",
			Content:      io.NopCloser(strings.NewReader("x")),
			DeclaredSize: mail.MaxMessageAttachmentDownloadBytes + 1,
		})
		assertProductErrorCode(t, err, errs.CodeAttachmentTooLarge)
		assertNoAttachmentFile(t, path)
		assertNoAttachmentTemps(t, dir)
	})

	t.Run("actual stream over cap", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "stream.bin")
		err := writeDownloadedAttachment(path, "stream.bin", false, mail.MessageAttachmentContent{
			AttachmentID: "stream",
			Content:      io.NopCloser(repeatedByteReader{remaining: mail.MaxMessageAttachmentDownloadBytes + 1}),
		})
		assertProductErrorCode(t, err, errs.CodeAttachmentTooLarge)
		assertNoAttachmentFile(t, path)
		assertNoAttachmentTemps(t, dir)
	})

	t.Run("read error after partial body", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "partial.bin")
		err := writeDownloadedAttachment(path, "partial.bin", false, mail.MessageAttachmentContent{
			AttachmentID: "partial",
			Content:      readCloser{Reader: partialErrorReader{sent: false}},
		})
		if err == nil {
			t.Fatal("expected stream write error")
		}
		assertNoAttachmentFile(t, path)
		assertNoAttachmentTemps(t, dir)
	})

	t.Run("close error after body", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "close.bin")
		err := writeDownloadedAttachment(path, "close.bin", false, mail.MessageAttachmentContent{
			AttachmentID: "close",
			Content:      readCloser{Reader: strings.NewReader("body"), closeErr: errors.New("close failed")},
		})
		if err == nil {
			t.Fatal("expected close error")
		}
		assertNoAttachmentFile(t, path)
		assertNoAttachmentTemps(t, dir)
	})

	t.Run("short clean body", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "short.bin")
		err := writeDownloadedAttachment(path, "short.bin", false, mail.MessageAttachmentContent{
			AttachmentID: "short",
			Content:      io.NopCloser(strings.NewReader("ne")),
			DeclaredSize: 100,
		})
		if err == nil {
			t.Fatal("expected short body error")
		}
		assertNoAttachmentFile(t, path)
		assertNoAttachmentTemps(t, dir)
	})
}

func TestMessageAttachmentOverwriteReplacesSymlinkPathOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior and permissions are platform-specific on Windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "report.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := writeDownloadedAttachment(link, "report.txt", false, mail.MessageAttachmentContent{
		AttachmentID: "report",
		Content:      io.NopCloser(strings.NewReader("new")),
		DeclaredSize: 3,
	})
	if err == nil {
		t.Fatal("expected existing symlink to fail without overwrite")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("outside target changed without overwrite: %q err=%v", got, err)
	}

	err = writeDownloadedAttachment(link, "report.txt", true, mail.MessageAttachmentContent{
		AttachmentID: "report",
		Content:      io.NopCloser(strings.NewReader("new")),
		DeclaredSize: 3,
	})
	if err != nil {
		t.Fatalf("overwrite symlink: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("final path is still symlink info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "new" {
		t.Fatalf("final file = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("outside target changed after overwrite: %q err=%v", got, err)
	}
	assertNoAttachmentTemps(t, dir)
}

func TestMessageAttachmentDanglingSymlinkRequiresOverwrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges on Windows")
	}
	for _, overwrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(overwrite), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "absent.txt")
			path := filepath.Join(dir, "report.txt")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if err := checkFinalPath(path, overwrite); (err == nil) != overwrite {
				t.Errorf("planning overwrite=%t: %v", overwrite, err)
			}
			err := writeDownloadedAttachment(path, "report.txt", overwrite, mail.MessageAttachmentContent{
				Content: io.NopCloser(strings.NewReader("new")), DeclaredSize: 3,
			})
			if (err == nil) != overwrite {
				t.Errorf("download overwrite=%t: %v", overwrite, err)
			}
			info, statErr := os.Lstat(path)
			if statErr != nil || (info.Mode()&os.ModeSymlink == 0) != overwrite {
				t.Fatalf("destination overwrite=%t: %v, %v", overwrite, info, statErr)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("symlink target was written: %v", err)
			}
			assertNoAttachmentTemps(t, dir)
		})
	}
}

func TestMessageAttachmentConcurrentDownloadsDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	const writers = 16
	start := make(chan struct{})
	type result struct {
		body string
		err  error
	}
	results := make(chan result, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat(strconv.Itoa(i)+" ", 1024)
			<-start
			err := writeDownloadedAttachment(path, "report.txt", false, mail.MessageAttachmentContent{
				Content: io.NopCloser(strings.NewReader(body)), DeclaredSize: int64(len(body)),
			})
			results <- result{body, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	winner := ""
	for res := range results {
		if res.err == nil {
			successes++
			winner = res.body
		}
	}
	if successes != 1 {
		t.Fatalf("successful downloads = %d, want exactly one", successes)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != winner {
		t.Fatalf("published bytes differ from successful download: %v", err)
	}
	assertNoAttachmentTemps(t, dir)
}

func assertProductErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var productErr *errs.Error
	if !errors.As(err, &productErr) {
		t.Fatalf("error %T %[1]v is not product error", err)
	}
	if productErr.Code() != code {
		t.Fatalf("error code = %s, want %s", productErr.Code(), code)
	}
}

func assertNoAttachmentFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("attachment file exists or stat failed for %s: %v", path, err)
	}
}

func assertNoAttachmentTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %#v", matches)
	}
}

type repeatedByteReader struct {
	remaining int64
}

func (r repeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type partialErrorReader struct {
	sent bool
}

func (r partialErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("stream failed")
	}
	r.sent = true
	copy(p, "partial")
	return len("partial"), errors.New("stream failed")
}

type readCloser struct {
	io.Reader
	closeErr error
}

func (r readCloser) Close() error {
	return r.closeErr
}

func seededAttachmentHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	fake := testkit.NewFakeDraftAttachmentProvider("fake", seededAttachmentDraft(t))
	return seedAttachmentHarness(t, fake), fake
}

func seedAttachmentHarness(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
	t.Helper()
	if fake.Drafts == nil {
		fake.Drafts = testkit.NewDraftState(seededAttachmentDraft(t))
	}
	h := newFakeHarness(t, fake)
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(h.root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return h
}

func seededMessageAttachmentHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	content := map[string]string{
		"att-file":   "report bytes",
		"att-inline": "inline bytes",
		"att-dup-a":  "duplicate a",
		"att-dup-b":  "duplicate b",
		"att-path":   "path bytes",
		"att-back":   "back bytes",
		"att-empty":  "empty name bytes",
	}
	return seededMessageAttachmentHarnessWithRows(t, messageAttachmentRows(), content)
}

func seededMessageAttachmentHarnessWithRows(t *testing.T, rows []mail.Attachment, content map[string]string) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	attachments := map[string]map[string][]mail.Attachment{
		"work": {
			"work-new": append([]mail.Attachment(nil), rows...),
			"work-old": {},
		},
		"personal": {
			"personal-new": {{ID: "personal-file", Name: "personal.pdf", ContentType: "application/pdf", Size: 9, Kind: mail.AttachmentKindFile}},
		},
	}
	contentMap := map[string]map[string]map[string]testkit.FakeAttachmentContent{
		"work": {
			"work-new": {},
		},
		"personal": {
			"personal-new": {
				"personal-file": {Bytes: []byte("personal"), ContentType: "application/pdf"},
			},
		},
	}
	for id, body := range content {
		contentMap["work"]["work-new"][id] = testkit.FakeAttachmentContent{Bytes: []byte(body), ContentType: "application/octet-stream"}
	}
	fake := testkit.NewFakeMessageAttachmentProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), attachments, contentMap)
	return seedMessageAttachmentHarness(t, fake), fake
}

func seedMessageAttachmentHarness(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	work := workAccount()
	personal := work
	personal.Name = "personal"
	personal.Email = "personal@example.test"
	personal.AuthRef = "fake/personal"
	personal.DefaultFrom = "personal@example.test"
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{work, personal})
	if err := testkit.SeedCredential(h.root, work, nil); err != nil {
		t.Fatalf("seed work credential: %v", err)
	}
	if err := testkit.SeedCredential(h.root, personal, nil); err != nil {
		t.Fatalf("seed personal credential: %v", err)
	}
	return h
}

func messageAttachmentRows() []mail.Attachment {
	return []mail.Attachment{
		{ID: "att-file", Name: "report.pdf", ContentType: "application/pdf", Size: 12, Kind: mail.AttachmentKindFile, ProviderType: "#fake.fileAttachment"},
		{ID: "att-inline", Name: "inline.png", ContentType: "image/png", Size: 11, IsInline: true, ContentID: "cid-inline", Kind: mail.AttachmentKindFile, ProviderType: "#fake.fileAttachment"},
		{ID: "att-item", Name: "embedded.eml", ContentType: "message/rfc822", Size: 9, Kind: mail.AttachmentKindItem, ProviderType: "#fake.itemAttachment"},
		{ID: "att-dup-a", Name: "duplicate.txt", ContentType: "text/plain", Size: 11, Kind: mail.AttachmentKindFile},
		{ID: "att-dup-b", Name: "duplicate.txt", ContentType: "text/plain", Size: 11, Kind: mail.AttachmentKindFile},
		{ID: "att-path", Name: "nested/path.txt", ContentType: "text/plain", Size: 10, Kind: mail.AttachmentKindFile},
		{ID: "att-back", Name: `nested\back.txt`, ContentType: "text/plain", Size: 10, Kind: mail.AttachmentKindFile},
		{ID: "att-empty", Name: "", ContentType: "text/plain", Size: 16, Kind: mail.AttachmentKindFile},
	}
}

func attachmentRowsFromJSON(t *testing.T, body string) []any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, body)
	}
	rows, ok := decoded["attachments"].([]any)
	if !ok {
		t.Fatalf("attachments field = %#v", decoded["attachments"])
	}
	return rows
}

func assertJSONField(t *testing.T, row any, key string, want any) {
	t.Helper()
	obj, ok := row.(map[string]any)
	if !ok {
		t.Fatalf("row = %#v", row)
	}
	got, ok := obj[key]
	if !ok {
		t.Fatalf("missing %s in %#v", key, obj)
	}
	if got != want {
		t.Fatalf("%s = %#v, want %#v in %#v", key, got, want, obj)
	}
}

func assertGmailAttachmentJSONMetadata(t *testing.T, row any) {
	t.Helper()
	obj, ok := row.(map[string]any)
	if !ok {
		t.Fatalf("row = %#v", row)
	}
	meta, ok := obj["provider_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("provider metadata = %#v", obj["provider_metadata"])
	}
	if meta["source"] != string(mail.MetadataSourceGmailAPI) ||
		meta["snapshot_message_id"] != "work-new" ||
		meta["external_attachment_id_present"] != true {
		t.Fatalf("provider metadata = %#v", meta)
	}
}

func seededAttachmentDraft(t *testing.T) mail.Draft {
	t.Helper()
	return mail.Draft{
		ID:             "seed-draft",
		IsDraft:        true,
		Subject:        "Seed",
		Folder:         mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"},
		Body:           mail.MessageBody{ContentType: "html", Mode: mail.BodyModeHTML, Content: "<p>body must not appear in attachment output</p>"},
		HasAttachments: true,
		Attachments: []mail.Attachment{
			{ID: "att-file", Name: "contract.pdf", ContentType: "application/pdf", Size: 1200, Kind: mail.AttachmentKindFile},
			{ID: "att-inline", Name: "inline.png", ContentType: "image/png", Size: 320, IsInline: true, ContentID: "cid-inline", Kind: mail.AttachmentKindFile},
			{ID: "att-item", Name: "embedded.eml", ContentType: "message/rfc822", Size: 50, Kind: mail.AttachmentKindItem},
			{ID: "att-reference", Name: "cloud.url", ContentType: "text/uri-list", Size: 50, Kind: mail.AttachmentKindReference},
			{ID: "att-unknown", Name: "unknown.bin", ContentType: "application/octet-stream", Size: 50, Kind: mail.AttachmentKindUnknown},
			{ID: "att-undecoded", Name: "undecoded.bin", ContentType: "application/octet-stream", Size: 50},
			{ID: "att-dup-a", Name: "duplicate.txt", ContentType: "text/plain", Size: 1, Kind: mail.AttachmentKindFile},
			{ID: "att-dup-b", Name: "duplicate.txt", ContentType: "text/plain", Size: 2, Kind: mail.AttachmentKindFile},
			{ID: "att-unsafe", Name: "unsafe\tname\nline\nbreak", ContentType: "text/plain", Size: 3, Kind: mail.AttachmentKindFile},
		},
		ProviderMeta: fakeDraftProviderMetadataForTest(t, mail.MetadataDraft),
	}
}

// The download table shows the size of the saved file, not the server's
// estimate from before the download.
func TestDownloadShowsTheSavedSize(t *testing.T) {
	h := newSendHarness(t)
	// "hello world!" is 12 bytes; the estimate from 16 base64 characters
	// is 11.
	payload := "aGVsbG8gd29ybGQh\r\n"
	h.server.Append("INBOX", "From: Clara <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Notes\r\nMessage-ID: <notes@example.org>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nSee the file.\r\n--b\r\n"+
		"Content-Type: text/plain; name=notes.txt\r\nContent-Disposition: attachment; filename=notes.txt\r\nContent-Transfer-Encoding: base64\r\n\r\n"+payload+"--b--\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "notes", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	res := h.run("attachments", "download", "--id", found.Messages[0].ID, "--all", "--to", t.TempDir())
	if res.Exit != 0 || !strings.Contains(res.Stdout, "notes.txt  12 bytes  saved") {
		t.Fatalf("download: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}
