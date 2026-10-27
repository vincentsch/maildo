package main

import (
	"strings"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestForwardFakeProviderDraftSendAndAttachments(t *testing.T) {
	h, fake := seededForwardHarness(t)

	var draft forwardResult
	decodeOK(t, h.run("forward", "--id", "src1", "--to", "client@example.test", "--body", "FYI", "--include-attachment", "contract.pdf", "--json"), &draft)
	if draft.ActionMode != "draft" || draft.DraftID == "" || draft.SentID != "" || len(draft.SelectedSourceAttachments) != 1 || draft.AttachmentCount != 2 {
		t.Fatalf("draft forward = %#v", draft)
	}
	if fake.Drafts.ForwardCreateCalls.Load() != 1 || fake.Drafts.UpdateCalls.Load() != 0 || fake.Drafts.SendCalls.Load() != 0 || fake.Drafts.MetadataOnlyReadCalls.Load() != 0 {
		t.Fatalf("counters forward=%d update=%d send=%d metadata=%d", fake.Drafts.ForwardCreateCalls.Load(), fake.Drafts.UpdateCalls.Load(), fake.Drafts.SendCalls.Load(), fake.Drafts.MetadataOnlyReadCalls.Load())
	}
	stored := fake.Drafts.Snapshot()[draft.DraftID]
	if len(stored.To) != 1 || stored.To[0].Address != "client@example.test" || !strings.Contains(stored.Body.Content, "FYI") || !strings.Contains(stored.Body.Content, "Forwarded message") {
		t.Fatalf("stored forward draft = %#v", stored)
	}

	var sent forwardResult
	decodeOK(t, approvedRun(t, h.run, "forward", "--id", "src1", "--to", "client@example.test", "--body", "Send", "--send", "--include-attachments", "all", "--json"), &sent)
	if sent.ActionMode != "send" || sent.SentID == "" || fake.Drafts.SendCalls.Load() != 1 || len(sent.UnsupportedSourceExclusion) != 1 {
		t.Fatalf("sent forward = %#v sendCalls=%d", sent, fake.Drafts.SendCalls.Load())
	}
}

func TestForwardFakeProviderDryRunValidationAndStaleInterfaces(t *testing.T) {
	h, fake := seededForwardHarness(t)
	missing := h.run("forward", "--id", "src1", "--body", "dry secret", "--include-attachment", "missing.pdf", "--dry-run", "--json")
	assertErrorCodeExit(t, missing, errs.CodeAttachmentNotFound, 5)
	if fake.Drafts.ForwardCreateCalls.Load() != 0 || fake.Drafts.ReadCalls.Load() != 0 || strings.Contains(missing.Stdout+missing.Stderr, "dry secret") {
		t.Fatalf("dry-run counters forward=%d read=%d stdout=%q stderr=%s", fake.Drafts.ForwardCreateCalls.Load(), fake.Drafts.ReadCalls.Load(), missing.Stdout, missing.Stderr)
	}
	preview := decodePreview(t, h.run("forward", "--id", "src1", "--body", "dry secret", "--include-attachment", "contract.pdf", "--dry-run", "--json"))
	if fake.Drafts.ForwardCreateCalls.Load() != 0 || fake.Drafts.ReadCalls.Load() != 0 {
		t.Fatalf("dry-run preview=%#v counters forward=%d read=%d", preview, fake.Drafts.ForwardCreateCalls.Load(), fake.Drafts.ReadCalls.Load())
	}
	msg := preview["message"].(map[string]any)
	if attachments, _ := msg["attachments"].([]any); len(attachments) != 1 || attachments[0].(map[string]any)["name"] != "contract.pdf" || msg["subject"] != "FW: fake" || preview["send"] != false {
		t.Fatalf("preview = %#v", preview)
	}
	assertErrorCode(t, h.run("forward", "--id", "src1", "--body", "x", "--draft", "--send", "--yes", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("forward", "--id", "src1", "--body", "x", "--include-attachments", "all", "--include-attachment", "a.pdf", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("forward", "--id", "src1", "--body", "x", "--include-attachment-id", "a", "--include-attachment-id", "a", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("forward", "--id", "src1", "--body", "x", "--send", "--yes", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("forward", "--id", "src1", "--body", "x", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidAccountScope)
	assertFrameworkUsage(t, h.run("forward", "--id", "src1", "--body", "x", "--all-accounts", "--json"))

	queryStale := testkit.NewFakeForwardProvider("fake")
	queryStale.MailboxImplemented = true
	queryHarness := seedForwardHarness(t, queryStale)
	assertErrorCode(t, queryHarness.run("forward", "--query", "subject:x", "--latest", "--body", "x", "--dry-run", "--json"), errs.CodeCapabilityNotImplemented)
	if queryStale.Drafts.ForwardCreateCalls.Load() != 0 {
		t.Fatalf("query stale searcher created forwards: %d", queryStale.Drafts.ForwardCreateCalls.Load())
	}

	for _, tt := range []struct {
		missing string
		args    []string
	}{
		{missing: "forward", args: []string{"forward", "--id", "src1", "--body", "x", "--json"}},
	} {
		t.Run("stale "+tt.missing, func(t *testing.T) {
			stale := testkit.NewFakeStaleForwardProvider("fake", tt.missing)
			staleHarness := seedForwardHarness(t, stale)
			assertErrorCode(t, staleHarness.run(tt.args...), errs.CodeCapabilityNotImplemented)
			if stale.Drafts.ForwardCreateCalls.Load() != 0 {
				t.Fatalf("stale %s created forwards: %d", tt.missing, stale.Drafts.ForwardCreateCalls.Load())
			}
		})
	}
}

func TestForwardFakeProviderOutputModesDoNotLeakBodies(t *testing.T) {
	h, _ := seededForwardHarness(t)

	human := h.run("forward", "--id", "src1", "--body", "human secret")
	if human.Exit != 0 || !strings.Contains(human.Stdout, "Saved the draft \"FW: fake\" in work.") ||
		strings.Contains(human.Stdout+human.Stderr, "human secret") {
		t.Fatalf("human stdout=%q stderr=%s", human.Stdout, human.Stderr)
	}

	jq := h.run("forward", "--id", "src1", "--body", "jq secret", "--json", "--jq", ".draft_id")
	if jq.Exit != 0 || !strings.Contains(jq.Stdout, "fake-forward-") || strings.Contains(jq.Stdout+jq.Stderr, "jq secret") {
		t.Fatalf("jq result stdout=%s stderr=%s", jq.Stdout, jq.Stderr)
	}

	tmpl := h.run("forward", "--id", "src1", "--body", "template secret", "--template", "{{.action_mode}}")
	if strings.TrimSpace(tmpl.Stdout) != "draft" || strings.Contains(tmpl.Stdout+tmpl.Stderr, "template secret") {
		t.Fatalf("template stdout=%q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}

	var envelope struct {
		Data forwardResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("forward", "--id", "src1", "--body", "meta secret", "--json", "--include-meta"), &envelope)
	if envelope.Data.DraftID == "" || envelope.Meta.Extra["command_path"] != "maildo forward" ||
		strings.Contains(previewString(map[string]any{"data": envelope}), "meta secret") {
		t.Fatalf("meta envelope = %#v", envelope)
	}
}

func TestForwardFakeProviderPostCreateFailuresSurfaceDraftID(t *testing.T) {
	fake := testkit.NewFakeForwardProvider("fake")
	fake.Drafts.FailForwardAfterDraft.Store(true)
	h := seedForwardHarness(t, fake)
	res := h.run("forward", "--id", "src1", "--body", "secret", "--json")
	if res.Exit == 0 || fake.Drafts.ForwardCreateCalls.Load() != 1 || strings.Contains(res.Stdout+res.Stderr, "secret") {
		t.Fatalf("failure stdout=%s stderr=%s calls=%d", res.Stdout, res.Stderr, fake.Drafts.ForwardCreateCalls.Load())
	}
	body := errorJSON(t, res)
	if body["draft_id"] != "fake-forward-1" || body["draft_created"] != true {
		t.Fatalf("error details = %#v", body)
	}
}

func seededForwardHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	fake := testkit.NewFakeForwardProvider("fake")
	return seedForwardHarness(t, fake), fake
}

func seedForwardHarness(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
	t.Helper()
	fake.Drafts.SourceAttachments["src1"] = []mail.Attachment{
		{ID: "src-file", Name: "contract.pdf", ContentType: "application/pdf", Size: 1200, Kind: mail.AttachmentKindFile},
		{ID: "src-inline", Name: "inline.png", ContentType: "image/png", Size: 320, IsInline: true, ContentID: "cid-inline", Kind: mail.AttachmentKindFile},
		{ID: "src-item", Name: "embedded.eml", ContentType: "message/rfc822", Size: 50, Kind: mail.AttachmentKindItem, ProviderType: "#fake.itemAttachment"},
	}
	h := newFakeHarness(t, fake)
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(h.root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return h
}
