package main

import (
	"reflect"
	"strings"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestReadFakeProviderSelectorsBodyModesAndMeta(t *testing.T) {
	h := seededReadHarness(t, "work")

	var byID readResult
	decodeOK(t, h.run("read", "--id", "work-new", "--json"), &byID)
	assertSelection(t, byID.AccountSelection, "single", "work")
	msg := byID.Message
	if msg.Account != "work" || msg.Provider != "fake" || msg.ID != "work-new" || msg.InternetMessageID != "<work-new@example.test>" ||
		msg.ThreadID != "thread-work-new" || msg.Folder.ID != "inbox-id" || msg.From.Address != "client@example.test" ||
		len(msg.To) != 2 || len(msg.Headers) != 2 || msg.CreatedAt == nil || msg.LastModifiedAt == nil ||
		msg.IsRead || msg.Importance != "high" || !sameStrings(msg.Categories, []string{"Contracts", "Client"}) ||
		!msg.HasAttachments || len(msg.Attachments) != 2 || msg.Body.Mode != mail.BodyModeReadable || msg.Body.Content == "" {
		t.Fatalf("read message shape = %#v", msg)
	}

	var byInternet readResult
	decodeOK(t, h.run("read", "--internet-id", "<work-new@example.test>", "--account", "work", "--json"), &byInternet)
	if byInternet.Message.ID != "work-new" {
		t.Fatalf("internet selector message = %#v", byInternet.Message)
	}

	var query readResult
	decodeOK(t, h.run("read", "--query", "anything", "--latest", "--all-accounts", "--json"), &query)
	assertSelection(t, query.AccountSelection, "single", "work")
	if query.Message.ID != "work-new" {
		t.Fatalf("query selected = %#v", query.Message)
	}
	decodeOK(t, h.run("read", "--query", "anything", "--latest", "--account", "work", "--account", "personal", "--json"), &query)
	assertSelection(t, query.AccountSelection, "single", "work")
	if query.Message.ID != "work-new" {
		t.Fatalf("read-many query selected = %#v", query.Message)
	}
	var envelope struct {
		Data readResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("read", "--query", "anything", "--latest", "--all-accounts", "--json", "--include-meta"), &envelope)
	assertSelection(t, envelope.Data.AccountSelection, "single", "work")
	if envelope.Meta.Extra["searched_scope"] != "all" || envelope.Meta.Extra["selector_kind"] != string(mail.SelectorQuery) {
		t.Fatalf("query meta = %#v", envelope.Meta.Extra)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["searched_accounts"]); !sameStrings(got, []string{"work", "personal"}) {
		t.Fatalf("searched accounts = %#v", got)
	}

	var first readResult
	decodeOK(t, h.run("read", "--query", "anything", "--first", "--account", "personal", "--json"), &first)
	if first.Message.ID != "personal-new" || first.Message.Account != "personal" {
		t.Fatalf("first selected = %#v", first.Message)
	}

	for _, mode := range []string{"readable", "text", "html", "raw"} {
		var got readResult
		decodeOK(t, h.run("read", "--id", "work-new", "--body-mode", mode, "--json"), &got)
		if string(got.Message.Body.Mode) != mode {
			t.Fatalf("body mode %s result = %#v", mode, got.Message.Body)
		}
		if mode == "raw" && got.Message.Body.Encoding != "utf8" {
			t.Fatalf("raw encoding = %#v", got.Message.Body)
		}
	}

	plain := h.run("read", "--id", "work-new", "--plain")
	decodeOK(t, plain, nil)
	if strings.TrimSpace(plain.Stdout) != "Full body for work-new." || strings.Contains(plain.Stdout, "Subject:") {
		t.Fatalf("plain stdout = %q", plain.Stdout)
	}
	human := h.run("read", "--id", "work-new")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "Subject:  Contract update") || !strings.Contains(human.Stdout, "Attachments:") ||
		!strings.Contains(human.Stdout, "ID:       "+shortID("work", "work-new")) || !strings.HasSuffix(human.Stdout, "\n\nFull body for work-new.\n") {
		t.Fatalf("human stdout = %q", human.Stdout)
	}
	if got := strings.TrimSpace(h.run("read", "--id", "work-new", "--json", "--jq", ".message.id").Stdout); got != "work-new" {
		t.Fatalf("jq stdout = %q", got)
	}
	if got := strings.TrimSpace(h.run("read", "--id", "work-new", "--template", "{{.message.id}}").Stdout); got != "work-new" {
		t.Fatalf("template stdout = %q", got)
	}
	decodeOK(t, h.run("read", "--id", "work-new", "--dry-run", "--json"), &readResult{})
	badDryMeta := h.run("read", "--id", "work-new", "--dry-run", "--json", "--include-meta")
	assertFrameworkUsage(t, badDryMeta)
	if !strings.Contains(badDryMeta.Stderr, "--include-meta cannot be combined with --dry-run") {
		t.Fatalf("dry/meta stderr = %s", badDryMeta.Stderr)
	}
	decodeOK(t, h.run("read", "--id", "work-new", "--no-color", "--json"), &readResult{})
	decodeOK(t, h.run("read", "--id", "work-new", "--no-ansi", "--json"), &readResult{})
}

func TestThreadFakeProviderSelectorsOrderingAndOutput(t *testing.T) {
	h := seededReadHarness(t, "work")
	var byID threadResult
	decodeOK(t, h.run("thread", "--id", "work-new", "--json"), &byID)
	assertSelection(t, byID.AccountSelection, "single", "work")
	if got, want := threadMessageIDs(byID.Thread.Messages), []string{"work-new", "work-thread-a", "work-thread-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread order = %#v, want %#v", got, want)
	}
	for _, row := range byID.Thread.Messages {
		if row.Account != "work" || row.Provider != "fake" {
			t.Fatalf("thread row labels = %#v", row)
		}
	}

	var byThreadID threadResult
	decodeOK(t, h.run("thread", "--thread-id", "thread-work-new", "--json"), &byThreadID)
	if len(byThreadID.Thread.Messages) != 3 {
		t.Fatalf("thread-id result = %#v", byThreadID.Thread)
	}
	var byInternetID threadResult
	decodeOK(t, h.run("thread", "--internet-id", "<work-new@example.test>", "--json"), &byInternetID)
	if byInternetID.Thread.ThreadID != "thread-work-new" || len(byInternetID.Thread.Messages) != 3 {
		t.Fatalf("thread internet-id result = %#v", byInternetID.Thread)
	}

	var query threadResult
	decodeOK(t, h.run("thread", "--query", "anything", "--first", "--account", "personal", "--json"), &query)
	assertSelection(t, query.AccountSelection, "single", "personal")
	if query.Thread.ThreadID != "thread-personal-new" || len(query.Thread.Messages) != 1 {
		t.Fatalf("query thread = %#v", query.Thread)
	}
	decodeOK(t, h.run("thread", "--query", "anything", "--latest", "--account", "work", "--account", "personal", "--json"), &query)
	assertSelection(t, query.AccountSelection, "single", "work")
	if query.Thread.ThreadID != "thread-work-new" {
		t.Fatalf("read-many query thread = %#v", query.Thread)
	}

	var raw map[string]any
	decodeOK(t, h.run("thread", "--id", "work-new", "--json"), &raw)
	thread := raw["thread"].(map[string]any)
	row := thread["messages"].([]any)[0].(map[string]any)
	if _, ok := row["body"]; ok {
		t.Fatalf("thread row has body: %#v", row)
	}

	human := h.run("thread", "--id", "work-new")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "Contract update, 3 messages") || !strings.Contains(human.Stdout, shortID("work", "work-new")+"  client@example.test") {
		t.Fatalf("thread human stdout = %q", human.Stdout)
	}
	plain := h.run("thread", "--id", "work-new", "--plain")
	assertFrameworkUsage(t, plain)
	assertNoCode(t, plain)

	assertErrorCodeExit(t, h.run("thread", "--thread-id", "missing-thread", "--json"), errs.CodeThreadNotFound, 5)
	assertErrorCodeExit(t, h.run("thread", "--id", "missing-message", "--json"), errs.CodeMessageNotFound, 5)
}

func TestReadSelectorErrorsScopeCapabilitiesAndReadOnly(t *testing.T) {
	h := seededReadHarness(t, "work")
	assertErrorCode(t, h.run("read", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("read", "--id", "work-new", "--internet-id", "<work-new@example.test>", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("read", "--query", "anything", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("read", "--query", "anything", "--latest", "--first", "--json"), errs.CodeInvalidSelector)
	assertFrameworkUsage(t, h.run("read", "--id", "work-new", "--all", "--json"))
	assertErrorCode(t, h.run("read", "--internet-id", "<duplicate@example.test>", "--json"), errs.CodeAmbiguousMessageSelector)
	assertErrorCode(t, h.run("read", "--id", "work-new", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("read", "--id", "work-new", "--all-accounts", "--json"), errs.CodeInvalidSelector)
	assertFrameworkUsage(t, h.run("read", "--id", "work-new", "--account", "work", "--all-accounts", "--json"))
	assertErrorCode(t, h.run("read", "--id", "work-new", "--account", "missing", "--json"), errs.CodeUnknownAccount)
	assertErrorCode(t, seededReadHarness(t, "").run("read", "--id", "work-new", "--json"), errs.CodeMissingActiveAccount)

	empty := seededReadHarnessWithProvider(t, testkit.NewFakeReadProvider("fake", map[string][]mail.MessageSummary{"work": {}, "personal": {}}, testkit.SampleFullMessages(), testkit.SampleThreads()), "work")
	assertErrorCodeExit(t, empty.run("read", "--query", "none", "--latest", "--json"), errs.CodeMessageNotFound, 5)

	noCreds := newFakeHarness(t, testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads()))
	seedRegistry(t, noCreds.configPath, "work", []maildoconfig.Account{folderWorkAccount()})
	assertErrorCode(t, noCreds.run("read", "--id", "work-new", "--body-mode", "txt", "--json"), errs.CodeInvalidSelector)

	staleRead := seededReadHarnessWithProvider(t, testkit.NewFakeProvider("fake"), "work")
	res := staleRead.run("read", "--id", "work-new", "--json")
	assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
	body := errorJSON(t, res)
	if body["capability"] != string(provider.CapMessagesRead) {
		t.Fatalf("stale read body = %#v", body)
	}

	staleThread := seededReadHarnessWithProvider(t, testkit.NewFakeStaleThreadProvider("fake"), "work")
	res = staleThread.run("thread", "--id", "work-new", "--json")
	assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
	body = errorJSON(t, res)
	if body["capability"] != string(provider.CapThreadsRead) {
		t.Fatalf("stale thread body = %#v", body)
	}

	var raw map[string]any
	decodeOK(t, h.run("read", "--id", "work-new", "--json"), &raw)
	if _, ok := raw["row"]; ok {
		t.Fatalf("row field leaked: %#v", raw)
	}
	if _, ok := raw["index"]; ok {
		t.Fatalf("index field leaked: %#v", raw)
	}
	// A row number is not a message id.
	if res := h.run("read", "1", "--json"); res.Exit == 0 {
		t.Fatalf("read 1 succeeded: %s", res.Stdout)
	}

	before := hashTree(t, h.root)
	decodeOK(t, h.run("read", "--id", "work-new", "--json"), &readResult{})
	decodeOK(t, h.run("thread", "--id", "work-new", "--json"), &threadResult{})
	decodeOK(t, h.run("read", "--id", "work-new", "--dry-run", "--json"), &readResult{})
	decodeOK(t, h.run("thread", "--id", "work-new", "--dry-run", "--json"), &threadResult{})
	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("read/thread mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func seededReadHarness(t *testing.T, active string) *cliHarness {
	t.Helper()
	return seededReadHarnessWithProvider(t, testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads()), active)
}

func seededReadHarnessWithProvider(t *testing.T, fake *testkit.FakeProvider, active string) *cliHarness {
	t.Helper()
	h := newFakeHarness(t, fake)
	accounts := []maildoconfig.Account{folderWorkAccount(), folderPersonalAccount()}
	seedRegistry(t, h.configPath, active, accounts)
	for _, account := range accounts {
		if err := testkit.SeedCredential(h.root, account, nil); err != nil {
			t.Fatalf("seed credential %s: %v", account.Name, err)
		}
	}
	return h
}

func threadMessageIDs(rows []mail.ThreadMessage) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}
