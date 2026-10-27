package main

import (
	"encoding/json"
	"errors"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestComposeDraftLifecycleFakeProvider(t *testing.T) {
	h, fake := seededDraftHarness(t)

	preview := decodePreview(t, h.run("draft", "create", "--to", "client@example.test", "--subject", "Secret", "--body", "body text must not leak", "--dry-run", "--json"))
	if fake.Drafts.CreateCalls.Load() != 0 {
		t.Fatalf("dry-run create mutated: %d", fake.Drafts.CreateCalls.Load())
	}
	body := preview["body"].(map[string]any)
	if body["body_source"] != "inline" || body["body_bytes"] != "23" || strings.Contains(previewString(preview), "body text must not leak") {
		t.Fatalf("unsafe create preview = %#v", preview)
	}

	var created draftWriteResult
	decodeOK(t, h.run("draft", "create", "--to", "Client <client@example.test>", "--cc", "copy@example.test", "--subject", "Draft subject", "--body", "Draft body", "--from", "Fake fake <fake@example.test>", "--json"), &created)
	if created.DraftID == "" || created.Operation != "draft_create" || created.RequestedFrom == "" || created.BodyFormat != "text" {
		t.Fatalf("created = %#v", created)
	}

	var read draftReadResult
	decodeOK(t, h.run("draft", "read", "--id", created.DraftID, "--json"), &read)
	if read.Draft.ID != created.DraftID || read.Draft.Body.Content != "Draft body" {
		t.Fatalf("read = %#v", read)
	}
	plain := h.run("draft", "read", "--id", created.DraftID, "--plain")
	decodeOK(t, plain, nil)
	if strings.TrimSpace(plain.Stdout) != "Draft body" {
		t.Fatalf("plain = %q", plain.Stdout)
	}

	beforeUpdate := fake.Drafts.UpdateCalls.Load()
	updatePreview := decodePreview(t, h.runWith(testutil.Options{Stdin: strings.NewReader("# Updated")}, "draft", "update", "--id", created.DraftID, "--clear-cc", "--body-file", "-", "--markdown", "--dry-run", "--json"))
	if fake.Drafts.ReadCalls.Load() == 0 || fake.Drafts.UpdateCalls.Load() != beforeUpdate {
		t.Fatalf("update dry-run counters read=%d update=%d before=%d", fake.Drafts.ReadCalls.Load(), fake.Drafts.UpdateCalls.Load(), beforeUpdate)
	}
	updateBody := updatePreview["body"].(map[string]any)
	if updateBody["body_source"] != "stdin" || updateBody["cc"] != "clear" || strings.Contains(previewString(updatePreview), "# Updated") {
		t.Fatalf("unsafe update preview = %#v", updatePreview)
	}

	var updated draftWriteResult
	decodeOK(t, h.run("draft", "update", "--id", created.DraftID, "--to", "new@example.test", "--subject", "Updated", "--body", "<b>safe</b><script>x()</script>", "--html", "--json"), &updated)
	if updated.Operation != "draft_update" || updated.DraftID != created.DraftID || updated.BodyFormat != "html" {
		t.Fatalf("updated = %#v", updated)
	}
	var updatedRead draftReadResult
	decodeOK(t, h.run("draft", "read", "--id", created.DraftID, "--body-mode", "html", "--json"), &updatedRead)
	if strings.Contains(updatedRead.Draft.Body.Content, "script") || !strings.Contains(updatedRead.Draft.Body.Content, "<b>safe</b>") {
		t.Fatalf("updated body = %q", updatedRead.Draft.Body.Content)
	}

	beforeSend := fake.Drafts.SendCalls.Load()
	decodePreview(t, h.run("draft", "send", "--yes", "--id", created.DraftID, "--dry-run", "--json"))
	if fake.Drafts.SendCalls.Load() != beforeSend {
		t.Fatalf("draft-send dry-run sent: before=%d after=%d", beforeSend, fake.Drafts.SendCalls.Load())
	}
	var sent draftWriteResult
	decodeOK(t, approvedRun(t, h.run, "draft", "send", "--id", created.DraftID, "--json"), &sent)
	if sent.Operation != "draft_send" || sent.DraftID != created.DraftID {
		t.Fatalf("sent = %#v", sent)
	}
	assertErrorCodeExit(t, h.run("draft", "read", "--id", created.DraftID, "--json"), errs.CodeDraftNotFound, 5)
}

func TestComposeDirectSendAndValidationFakeProvider(t *testing.T) {
	h, fake := seededDraftHarness(t)
	preview := decodePreview(t, h.runWith(testutil.Options{Stdin: strings.NewReader("stdin secret body")}, "send", "--to", "client@example.test", "--subject", "Preview", "--body-file", "-", "--dry-run", "--json"))
	if fake.Drafts.DirectCalls.Load() != 0 {
		t.Fatalf("send dry-run called provider: %d", fake.Drafts.DirectCalls.Load())
	}
	// The preview shows the recipients, the subject, the whole body and
	// the approval code.
	msg, _ := preview["message"].(map[string]any)
	to, _ := msg["to"].([]any)
	if preview["operation"] != "send" || preview["dry_run"] != true || len(to) != 1 ||
		msg["subject"] != "Preview" || msg["body"] != "stdin secret body" || preview["approval_code"] == "" {
		t.Fatalf("send preview = %#v", preview)
	}

	var sent directSendResult
	decodeOK(t, approvedRun(t, h.run, "send", "--to", "client@example.test", "--subject", "Send subject", "--body", "Send body", "--json"), &sent)
	if sent.Operation != "send" || sent.MessageID == "" || sent.Summary.ToCount != 1 || !sent.Summary.SubjectPresent || !sent.Summary.TextBodyPresent || fake.Drafts.DirectCalls.Load() != 1 {
		t.Fatalf("sent = %#v directCalls=%d", sent, fake.Drafts.DirectCalls.Load())
	}
	jq := approvedRun(t, h.run, "send", "--to", "client@example.test", "--subject", "JQ", "--body", "Body", "--json", "--jq", ".operation")
	if strings.TrimSpace(jq.Stdout) != "send" {
		t.Fatalf("jq stdout=%q stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("draft", "read", "--id", "seed-draft", "--template", "{{.draft.id}}")
	if strings.TrimSpace(tmpl.Stdout) != "seed-draft" {
		t.Fatalf("template stdout=%q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}

	for _, tt := range [][]string{
		{"send", "--yes", "--subject", "No recipient", "--json"},
		{"send", "--yes", "--to", "bad", "--body", "x", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--body", "x", "--body-file", "-", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--body", "x", "--text", "--html", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--from", "other@example.test", "--json"},
		{"draft", "update", "--id", "seed-draft", "--json"},
		{"draft", "update", "--id", "seed-draft", "--to", "client@example.test", "--clear-to", "--json"},
		{"draft", "read", "--id", "", "--json"},
	} {
		assertErrorCode(t, h.run(tt...), errs.CodeInvalidComposeInput)
	}
	assertErrorCodeExit(t, h.run("draft", "read", "--id", "missing", "--json"), errs.CodeDraftNotFound, 5)
	assertErrorCodeExit(t, h.run("draft", "send", "--yes", "--id", "not-draft", "--json"), errs.CodeDraftNotFound, 5)

	repeated := h.run("send", "--yes", "--account", "work", "--account", "personal", "--to", "client@example.test", "--subject", "x", "--json")
	assertErrorCode(t, repeated, errs.CodeInvalidAccountScope)
	if bad := h.run("send", "--yes", "--to", "client@example.test", "--subject", "x", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("--include-meta --dry-run succeeded")
	}
}

func TestDirectSendLocalValidationPrecedesAccountResolution(t *testing.T) {
	fake := testkit.NewFakeDraftProvider("fake")
	h := newFakeHarness(t, fake)
	for _, args := range [][]string{
		{"send", "--yes", "--to", "bad", "--subject", "x", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--body", "x", "--body-file", "-", "--json"},
		{"send", "--yes", "--to", "client@example.test", "--subject", "x", "--from", "bad sender", "--json"},
	} {
		assertErrorCode(t, h.run(args...), errs.CodeInvalidComposeInput)
	}
	if fake.CapabilityCalls.Load() != 0 || fake.AccountClientCalls.Load() != 0 || fake.Drafts.DirectCalls.Load() != 0 {
		t.Fatalf("provider work occurred before local validation: caps=%d clients=%d direct=%d",
			fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.Drafts.DirectCalls.Load())
	}
}

func TestMIMEBoundedBodyIngressRejectsOneOverLimit(t *testing.T) {
	cmd := &cobra.Command{Use: "send"}
	addReplyBodyFlags(cmd)
	if err := cmd.ParseFlags([]string{"--body-file", "-"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	f := &rungrad.Factory{Stdin: &boundedReader{remaining: mail.MaxOutboundEncodedMessageBytes + 1}}
	_, err := readBodyInput(f, cmd, provider.ResolvedAccount{Name: "work", Provider: "gmail"}, provider.ComposeIngressMIMEBounded)
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeMessageTooLarge {
		t.Fatalf("error = %#v", err)
	}
	if product.Details()["limit_source"] != string(errs.MessageSizeComposeSource) || product.Details()["observed_at_least"] != uint64(mail.MaxOutboundEncodedMessageBytes+1) {
		t.Fatalf("details = %#v", product.Details())
	}
}

func TestComposeStaleCapabilityAndSecretRedaction(t *testing.T) {
	stale := testkit.NewFakeStaleDraftProvider("fake")
	h := newFakeHarness(t, stale)
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	access := "compose-access-secret-12345"
	refresh := "compose-refresh-secret-12345"
	if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	for _, args := range [][]string{
		{"draft", "create", "--to", "client@example.test", "--subject", "x", "--json"},
		{"draft", "read", "--id", "seed-draft", "--json"},
		{"draft", "update", "--id", "seed-draft", "--subject", "x", "--json"},
		{"draft", "send", "--id", "seed-draft", "--json"},
		{"send", "--to", "client@example.test", "--subject", "x", "--json"},
	} {
		run := h.run
		if args[0] == "send" || args[1] == "send" {
			run = func(a ...string) testutil.Result { return approvedRun(t, h.run, a...) }
		}
		res := run(args...)
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		testkit.AssertNoSecretLeak(t, res, access, refresh)
	}
}

func seededDraftHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	seed := mail.Draft{
		ID:        "seed-draft",
		IsDraft:   true,
		From:      mail.Recipient{Address: "fake@example.test"},
		To:        []mail.Recipient{{Address: "client@example.test"}},
		Subject:   "Seed",
		Body:      mail.MessageBody{ContentType: "text", Mode: mail.BodyModeReadable, Content: "Seed body"},
		Folder:    mail.FolderSummary{ID: "drafts-id", DisplayName: "Drafts", WellKnownName: mail.WellKnownDrafts, Path: "Drafts"},
		CreatedAt: ptrTime(time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)),
	}
	notDraft := seed
	notDraft.ID = "not-draft"
	notDraft.IsDraft = false
	fake := testkit.NewFakeDraftProvider("fake", seed, notDraft)
	h := newFakeHarness(t, fake)
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(h.root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return h, fake
}

func ptrTime(t time.Time) *time.Time { return &t }

func previewString(preview map[string]any) string {
	b, _ := json.Marshal(preview)
	return string(b)
}

type boundedReader struct{ remaining int64 }

func (r *boundedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}
