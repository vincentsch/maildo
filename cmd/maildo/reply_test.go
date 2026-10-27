package main

import (
	"bytes"
	"encoding/json"
	"github.com/vincentsch/rungrad"
	"strings"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestReplyFakeProviderDraftSendAndReplyAll(t *testing.T) {
	h, fake := seededReplyHarness(t)

	var draft replyResult
	decodeOK(t, h.run("reply", "--id", "src1", "--body", "Thanks", "--json"), &draft)
	if draft.ActionMode != "draft" || draft.DraftID == "" || draft.SentID != "" || draft.ReplyMode != mail.ReplyModeReply {
		t.Fatalf("draft reply = %#v", draft)
	}
	if fake.Drafts.ReplyCreateCalls.Load() != 1 || fake.Drafts.UpdateCalls.Load() != 0 || fake.Drafts.SendCalls.Load() != 0 {
		t.Fatalf("counters create=%d update=%d send=%d", fake.Drafts.ReplyCreateCalls.Load(), fake.Drafts.UpdateCalls.Load(), fake.Drafts.SendCalls.Load())
	}

	var sent replyResult
	decodeOK(t, approvedRun(t, h.run, "reply", "--id", "src1", "--body", "Thanks", "--send", "--json"), &sent)
	if sent.ActionMode != "send" || sent.SentID == "" || fake.Drafts.SendCalls.Load() != 1 {
		t.Fatalf("sent reply = %#v sendCalls=%d", sent, fake.Drafts.SendCalls.Load())
	}

	var replyAll replyResult
	decodeOK(t, h.run("reply", "--id", "src1", "--reply-all", "--body", "x", "--json"), &replyAll)
	if replyAll.ReplyMode != mail.ReplyModeReplyAll || len(replyAll.CC) != 1 || replyAll.CC[0].Address != "other@example.test" {
		t.Fatalf("reply-all = %#v", replyAll)
	}
}

func TestReplyFakeProviderQuoteModes(t *testing.T) {
	h, fake := seededReplyHarness(t)

	var full replyResult
	decodeOK(t, h.run("reply", "--id", "src-full", "--body", "Full body", "--json"), &full)
	fullDraft := fake.Drafts.Snapshot()[full.DraftID]
	if fullDraft.Body.ContentType != "text" || !strings.Contains(fullDraft.Body.Content, "Full body") || !strings.Contains(fullDraft.Body.Content, "> original source line one") {
		t.Fatalf("full patched body = %#v", fullDraft.Body)
	}

	var none replyResult
	decodeOK(t, h.run("reply", "--id", "src-none", "--quote", "none", "--body", "None body", "--json"), &none)
	noneDraft := fake.Drafts.Snapshot()[none.DraftID]
	if noneDraft.Body.ContentType != "text" || strings.TrimSpace(noneDraft.Body.Content) != "None body" || strings.Contains(noneDraft.Body.Content, "original source line") {
		t.Fatalf("none patched body = %#v", noneDraft.Body)
	}

	var short replyResult
	decodeOK(t, h.run("reply", "--id", "src-short", "--quote", "short", "--quote-language", "en", "--body", "Short body", "--json"), &short)
	shortDraft := fake.Drafts.Snapshot()[short.DraftID]
	for _, want := range []string{"Short body", "On 2026-06-28 10:00 UTC, Source Sender <source@example.test> wrote:", "> original source line one"} {
		if !strings.Contains(shortDraft.Body.Content, want) {
			t.Fatalf("short body missing %q: %#v", want, shortDraft.Body)
		}
	}
}

func TestReplyFakeProviderValidationScopeAndStaleInterfaces(t *testing.T) {
	h, fake := seededReplyHarness(t)
	assertErrorCode(t, h.run("reply", "--id", "src1", "--draft", "--send", "--yes", "--body", "x", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("reply", "--id", "src1", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("reply", "--id", "src1", "--body", "  \n\t", "--json"), errs.CodeInvalidComposeInput)
	assertErrorCode(t, h.run("reply", "--id", "src1", "--account", "work", "--account", "personal", "--body", "x", "--json"), errs.CodeInvalidAccountScope)
	assertFrameworkUsage(t, h.run("reply", "--id", "src1", "--all-accounts", "--body", "x", "--json"))
	assertFrameworkUsage(t, h.run("reply", "--id", "src1", "--all", "--body", "x", "--json"))
	if fake.Drafts.ReplyCreateCalls.Load() != 0 {
		t.Fatalf("validation created replies: %d", fake.Drafts.ReplyCreateCalls.Load())
	}

	active := h.run("reply", "--id", "src1", "--body", "x", "--json")
	decodeOK(t, active, &replyResult{})
	explicit := h.run("reply", "--id", "src1", "--account", "work", "--body", "x", "--json")
	decodeOK(t, explicit, &replyResult{})

	for _, tt := range []struct {
		missing string
		args    []string
	}{
		{missing: "reply", args: []string{"reply", "--id", "src1", "--body", "x", "--json"}},
	} {
		t.Run("stale "+tt.missing, func(t *testing.T) {
			stale := testkit.NewFakeStaleReplyProvider("fake", tt.missing)
			staleHarness := seedReplyHarness(t, stale)
			assertErrorCode(t, staleHarness.run(tt.args...), errs.CodeCapabilityNotImplemented)
			if stale.Drafts.ReplyCreateCalls.Load() != 0 {
				t.Fatalf("stale %s created replies: %d", tt.missing, stale.Drafts.ReplyCreateCalls.Load())
			}
		})
	}

	queryFake := testkit.NewFakeReplyProvider("fake")
	queryHarness := seedReplyHarness(t, queryFake)
	assertErrorCode(t, queryHarness.run("reply", "--query", "subject:x", "--latest", "--body", "x", "--json"), errs.CodeCapabilityNotImplemented)
	if queryFake.Drafts.ReplyCreateCalls.Load() != 0 {
		t.Fatalf("query without search created replies: %d", queryFake.Drafts.ReplyCreateCalls.Load())
	}
}

func TestReplyFakeProviderPostCreateFailuresSurfaceDraftID(t *testing.T) {
	fake := testkit.NewFakeReplyProvider("fake")
	fake.Drafts.FailReplyAfterDraft.Store(true)
	h := seedReplyHarness(t, fake)
	res := h.run("reply", "--id", "src1", "--body", "x", "--json")
	if res.Exit == 0 {
		t.Fatalf("failure succeeded: stdout=%s", res.Stdout)
	}
	if fake.Drafts.ReplyCreateCalls.Load() != 1 {
		t.Fatalf("reply create calls = %d", fake.Drafts.ReplyCreateCalls.Load())
	}
	if !strings.Contains(res.Stderr, "fake-reply-1") {
		t.Fatalf("draft id missing from error: %s", res.Stderr)
	}
	body := errorJSON(t, res)
	if body["draft_id"] != "fake-reply-1" || body["draft_created"] != true {
		t.Fatalf("error details = %#v", body)
	}
}

func TestReplyFakeProviderDryRunAndOutputModes(t *testing.T) {
	h, fake := seededReplyHarness(t)
	preview := decodePreview(t, h.run("reply", "--id", "src1", "--body", "reply secret body", "--dry-run", "--json"))
	if fake.Drafts.ReplyCreateCalls.Load() != 0 {
		t.Fatalf("dry-run created reply: %d", fake.Drafts.ReplyCreateCalls.Load())
	}
	if preview["send"] != false || !strings.HasPrefix(preview["message"].(map[string]any)["body"].(string), "reply secret body") || preview["approval_code"] != nil {
		t.Fatalf("preview = %#v", preview)
	}
	if bad := h.run("reply", "--id", "src1", "--body", "x", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("reply --include-meta --dry-run succeeded")
	}

	human := h.run("reply", "--id", "src1", "--body", "human secret")
	decodeOK(t, human, nil)
	if strings.Contains(human.Stdout+human.Stderr, "human secret") {
		t.Fatalf("human leaked body: stdout=%s stderr=%s", human.Stdout, human.Stderr)
	}
	var jsonResult replyResult
	decodeOK(t, h.run("reply", "--id", "src1", "--body", "json secret", "--json"), &jsonResult)
	jq := h.run("reply", "--id", "src1", "--body", "jq secret", "--json", "--jq", ".draft_id")
	if jq.Exit != 0 || !strings.Contains(jq.Stdout, "fake-reply-") || strings.Contains(jq.Stdout+jq.Stderr, "jq secret") {
		t.Fatalf("jq result stdout=%s stderr=%s", jq.Stdout, jq.Stderr)
	}
	tmpl := h.run("reply", "--id", "src1", "--body", "template secret", "--template", "{{.reply_mode}}")
	if strings.TrimSpace(tmpl.Stdout) != "reply" || strings.Contains(tmpl.Stdout+tmpl.Stderr, "template secret") {
		t.Fatalf("template stdout=%q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
}

func TestReplyResultAwkwardProviderDataShape(t *testing.T) {
	result := replyResult{
		AccountSelection: provider.AccountSelection{
			Scope: "single",
			Accounts: []provider.ResolvedAccount{{
				Name:     "work\taccount\r\nnext",
				Provider: "fake\tprovider\r\nnext",
				Active:   true,
			}},
		},
		SourceID:         strings.Repeat("source-id-", 20) + "\r\nsuffix",
		SourceInternetID: "<awkward@example.test>",
		ReplyMode:        mail.ReplyModeReplyAll,
		ActionMode:       "draft",
		QuoteMode:        mail.QuoteModeShort,
		QuoteLanguage:    mail.QuoteLanguageGerman,
		DraftID:          "draft\tid\r\nnext",
		To: []mail.Recipient{{
			Name:    "Client\tName\r\nComma, \"Quote\" 日本語",
			Address: "client@example.test",
		}},
		CC:              []mail.Recipient{{Address: "copy@example.test"}},
		Subject:         "Subject\tline\r\nnext",
		AttachmentCount: 2,
		ProviderMeta:    fakeDraftProviderMetadataForTest(t, mail.MetadataDraftWrite),
	}

	var human bytes.Buffer
	humanReply(&rungrad.Factory{Flags: &rungrad.GlobalFlags{JSON: true}}, result)(&human)
	humanOut := human.String()
	// Two lines: what was saved, and the draft id.
	if strings.ContainsAny(humanOut, "\t\r") || strings.Count(humanOut, "\n") != 2 {
		t.Fatalf("human output kept row-breaking characters: %q", humanOut)
	}
	for _, forbidden := range []string{"Subject\tline", "Client\tName", "source body", "provider quote"} {
		if strings.Contains(humanOut, forbidden) {
			t.Fatalf("human output leaked %q: %q", forbidden, humanOut)
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal reply result: %v", err)
	}
	var roundTrip replyResult
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal reply result: %v", err)
	}
	if roundTrip.Subject != result.Subject ||
		roundTrip.To[0].Name != result.To[0].Name ||
		roundTrip.AttachmentCount != 2 ||
		roundTrip.ProviderMeta == nil || roundTrip.ProviderMeta.Source != mail.MetadataSourceFakeForTest {
		t.Fatalf("json round trip lost structured data: %#v", roundTrip)
	}
	if strings.Contains(string(data), "source body") || strings.Contains(string(data), "provider quote") {
		t.Fatalf("json output leaked body or quote content: %s", data)
	}
}

func seededReplyHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	fake := testkit.NewFakeReplyProvider("fake")
	return seedReplyHarness(t, fake), fake
}

func seedReplyHarness(t *testing.T, fake *testkit.FakeProvider) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(h.root, account, nil); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return h
}
