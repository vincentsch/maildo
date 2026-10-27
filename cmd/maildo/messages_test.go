package main

import (
	"errors"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessagesSearchAndListFakeProvider(t *testing.T) {
	h := seededMessagesHarness(t, "work")

	var active searchResult
	decodeOK(t, h.run("search", "invoice", "--json"), &active)
	assertSelection(t, active.AccountSelection, "single", "work")
	if active.Count != len(testkit.SampleMessages()["work"]) || len(active.Messages) != active.Count {
		t.Fatalf("active search count=%d rows=%d", active.Count, len(active.Messages))
	}
	assertMessagesLabelled(t, active.Messages, "work", "fake")
	if active.Query.Terms[0] != "invoice" {
		t.Fatalf("query = %#v", active.Query)
	}

	var explicit listMessagesResult
	decodeOK(t, h.run("list", "--account", "personal", "--json"), &explicit)
	assertSelection(t, explicit.AccountSelection, "single", "personal")
	assertMessagesLabelled(t, explicit.Messages, "personal", "fake")
	if explicit.Messages[0].Folder.ID != "inbox-id" || explicit.Messages[0].Folder.WellKnownName != mail.WellKnownInbox {
		t.Fatalf("default list folder = %#v", explicit.Messages[0].Folder)
	}

	var many searchResult
	decodeOK(t, h.run("search", "anything", "--account", "work", "--account", "personal", "--limit", "4", "--json"), &many)
	assertSelection(t, many.AccountSelection, "read_many", "work", "personal")
	if got, want := messageIDs(many.Messages), []string{"work-new", "personal-new", "work-sent-fallback", "personal-mid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged ids = %#v, want %#v", got, want)
	}

	var all searchResult
	decodeOK(t, h.run("search", "anything", "--all-accounts", "--limit", "2", "--json"), &all)
	assertSelection(t, all.AccountSelection, "all", "work", "personal")
	if got, want := messageIDs(all.Messages), []string{"work-new", "personal-new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("limited ids = %#v, want %#v", got, want)
	}

	var unread listMessagesResult
	decodeOK(t, h.run("list", "inbox", "--all-accounts", "--unread", "--json"), &unread)
	if got, want := messageIDs(unread.Messages), []string{"work-new", "personal-new", "personal-tie"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unread ids = %#v, want %#v", got, want)
	}
	for _, row := range unread.Messages {
		if row.IsRead {
			t.Fatalf("read row survived --unread: %#v", row)
		}
		if row.Account == "" || row.Provider == "" || row.ID == "" || row.ProviderMeta == nil || row.ProviderMeta.Source != mail.MetadataSourceFakeForTest {
			t.Fatalf("row missing stable fields: %#v", row)
		}
	}
}

func TestMessagesOutputModesMetaDryRunAndEmpty(t *testing.T) {
	h := seededMessagesHarness(t, "work")

	human := h.run("search", "invoice")
	decodeOK(t, human, nil)
	row := "*+  " + shortID("work", "work-new") + "  Client"
	if !strings.Contains(human.Stdout, "3 messages matching \"invoice\" (work)") || !strings.Contains(human.Stdout, row) ||
		!strings.Contains(human.Stdout, "ID       FROM") || !strings.Contains(human.Stdout, "* unread  ! flagged  + attachment") {
		t.Fatalf("human stdout = %q", human.Stdout)
	}

	jq := h.run("search", "invoice", "--json", "--jq", ".scope")
	if strings.TrimSpace(jq.Stdout) != "single" {
		t.Fatalf("jq stdout = %q stderr=%s", jq.Stdout, jq.Stderr)
	}
	count := h.run("search", "invoice", "--json", "--jq", ".count")
	if strings.TrimSpace(count.Stdout) != "3" {
		t.Fatalf("count stdout = %q stderr=%s", count.Stdout, count.Stderr)
	}
	tmpl := h.run("list", "--template", "{{.scope}}:{{.count}}")
	if strings.TrimSpace(tmpl.Stdout) != "single:3" {
		t.Fatalf("template stdout = %q stderr=%s", tmpl.Stdout, tmpl.Stderr)
	}
	plain := h.run("search", "invoice", "--limit", "2", "--plain")
	decodeOK(t, plain, nil)
	wantPlain := strings.Join([]string{
		"work\tfake\twork-new\t<work-new@example.test>\tContract update",
		"work\tfake\twork-sent-fallback\t<work-sent@example.test>\tSent fallback row",
		"",
	}, "\n")
	if plain.Stdout != wantPlain {
		t.Fatalf("plain stdout = %q, want %q", plain.Stdout, wantPlain)
	}

	var envelope struct {
		Data searchResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("search", "invoice", "--json", "--include-meta"), &envelope)
	assertSelection(t, envelope.Data.AccountSelection, "single", "work")
	if envelope.Meta.Extra["command_path"] != "maildo search" || envelope.Meta.Extra["message_count"] != float64(3) {
		t.Fatalf("meta = %#v", envelope.Meta.Extra)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["accounts"]); !sameStrings(got, []string{"work"}) {
		t.Fatalf("meta accounts = %#v", got)
	}
	if got := metaStringSlice(t, envelope.Meta.Extra["providers"]); !sameStrings(got, []string{"fake"}) {
		t.Fatalf("meta providers = %#v", got)
	}

	var dry searchResult
	decodeOK(t, h.run("search", "invoice", "--dry-run", "--json"), &dry)
	if dry.Count != 3 {
		t.Fatalf("dry run result = %#v", dry)
	}
	bad := h.run("search", "invoice", "--include-meta", "--dry-run", "--json")
	assertFrameworkUsage(t, bad)
	if !strings.Contains(bad.Stderr, "--include-meta cannot be combined with --dry-run") {
		t.Fatalf("dry-run meta stderr = %s", bad.Stderr)
	}
	decodeOK(t, h.run("list", "--no-color", "--json"), &listMessagesResult{})
	decodeOK(t, h.run("list", "--no-ansi", "--json"), &listMessagesResult{})

	empty := seededMessagesHarnessWithProvider(t, testkit.NewFakeMailboxProvider("fake", map[string][]mail.MessageSummary{"work": {}, "personal": {}}), "work")
	noMessages := empty.run("search", "anything")
	decodeOK(t, noMessages, nil)
	if !strings.HasPrefix(noMessages.Stdout, "No messages match \"anything\" (work).") {
		t.Fatalf("empty human stdout = %q", noMessages.Stdout)
	}
	var emptyJSON searchResult
	decodeOK(t, empty.run("search", "anything", "--json"), &emptyJSON)
	if emptyJSON.Count != 0 || emptyJSON.Messages == nil || len(emptyJSON.Messages) != 0 {
		t.Fatalf("empty json = %#v", emptyJSON)
	}
}

func TestMessagesDataShapeAndFieldSanitizing(t *testing.T) {
	received := time.Date(2026, 6, 28, 11, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	rawID := "id-" + strings.Repeat("x", 64) + "\tline\nend\\slash"
	rawSubject := "Hello\tlocalized\r\nback\\slash"
	rawSender := "Jürgen\tMüller\nOps"
	messages := map[string][]mail.MessageSummary{"work": {
		{
			ID: rawID, InternetMessageID: "<long-" + strings.Repeat("m", 64) + "@example.test>",
			Folder:  mail.FolderSummary{ID: "folder\tid", DisplayName: "Földér\r\nName", Path: "Root\tChild"},
			From:    mail.Recipient{Name: rawSender},
			Subject: rawSubject, ReceivedAt: &received, IsRead: false, HasAttachments: true,
		},
		{
			ID: "sent-only", Folder: mail.FolderSummary{ID: "sent-id"},
			From: mail.Recipient{Address: "sent@example.test"}, Subject: "",
			SentAt: &sent, IsRead: true,
		},
		{
			ID: "zero-time", Folder: mail.FolderSummary{ID: "zero-id"},
			From: mail.Recipient{}, Subject: "No timestamps", IsRead: true,
		},
	}}
	h := seededMessagesHarnessWithProvider(t, testkit.NewFakeMailboxProvider("fake", messages), "work")

	var jsonResult searchResult
	decodeOK(t, h.run("search", "anything", "--json"), &jsonResult)
	if got, want := messageIDs(jsonResult.Messages), []string{rawID, "sent-only", "zero-time"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("message order = %#v, want %#v", got, want)
	}
	if jsonResult.Messages[0].Subject != rawSubject || jsonResult.Messages[0].From.Name != rawSender {
		t.Fatalf("json did not preserve raw fields: %#v", jsonResult.Messages[0])
	}

	human := h.run("search", "anything")
	decodeOK(t, human, nil)
	// Summary, blank line, header, three rows, blank line, legend.
	humanLines := strings.Split(strings.TrimSuffix(human.Stdout, "\n"), "\n")
	if len(humanLines) != 8 {
		t.Fatalf("human rows broken by embedded newlines: %q", human.Stdout)
	}
	for _, line := range humanLines {
		if strings.ContainsAny(line, "\r\n") || strings.Contains(line, "\tline") {
			t.Fatalf("human row contains unsanitized control data: %q", line)
		}
		if strings.Contains(line, "null") {
			t.Fatalf("human row rendered null text: %q", line)
		}
	}
	if !strings.Contains(humanLines[3], "Hello localized  back\\slash") || !strings.Contains(humanLines[3], "Jürgen Müller Ops") {
		t.Fatalf("human row missing sanitized localized fields: %q", humanLines[3])
	}

	plain := h.run("search", "anything", "--plain")
	decodeOK(t, plain, nil)
	plainLines := strings.Split(strings.TrimSuffix(plain.Stdout, "\n"), "\n")
	if len(plainLines) != 3 {
		t.Fatalf("plain rows broken by embedded newlines: %q", plain.Stdout)
	}
	fields := strings.Split(plainLines[0], "\t")
	if len(fields) != 5 {
		t.Fatalf("plain field count = %d row=%q", len(fields), plainLines[0])
	}
	if strings.ContainsAny(plainLines[0], "\r\n") || strings.Contains(fields[2], "\t") || strings.Contains(fields[4], "\t") {
		t.Fatalf("plain row contains unsanitized separators: %q", plainLines[0])
	}
	if !strings.Contains(fields[2], "line end\\slash") || !strings.Contains(fields[4], "Hello localized  back\\slash") {
		t.Fatalf("plain row missing sanitized data: fields=%#v", fields)
	}
}

func TestMessagesQueryArgsLimitAndErrors(t *testing.T) {
	h := seededMessagesHarness(t, "work")
	var joined searchResult
	decodeOK(t, h.run("search", "from:a@example.test", "invoice", "--json"), &joined)
	if !reflect.DeepEqual(joined.Query.From, []string{"a@example.test"}) || !reflect.DeepEqual(joined.Query.Terms, []string{"invoice"}) {
		t.Fatalf("joined query = %#v", joined.Query)
	}
	var quoted searchResult
	decodeOK(t, h.run("search", `subject:"quarterly invoice"`, "--json"), &quoted)
	if !reflect.DeepEqual(quoted.Query.Subject, []string{"quarterly invoice"}) {
		t.Fatalf("quoted query = %#v", quoted.Query)
	}
	assertFrameworkUsage(t, h.run("search", "invoice", "--limit", "0", "--json"))
	assertFrameworkUsage(t, h.run("search", "invoice", "--limit", "1001", "--json"))
	assertFrameworkUsage(t, h.run("search", "invoice", "--page", "0", "--json"))
	assertErrorCode(t, h.run("search", "bcc:x", "--json"), errs.CodeUnsupportedQueryOperator)
	assertErrorCode(t, h.run("search", `bad"quote`, "--json"), errs.CodeInvalidQuery)
}

func TestMessagesScopeCapabilityAndProviderErrors(t *testing.T) {
	t.Run("scope errors", func(t *testing.T) {
		h := seededMessagesHarness(t, "")
		assertErrorCode(t, h.run("search", "invoice", "--json"), errs.CodeMissingActiveAccount)
		h = seededMessagesHarness(t, "work")
		assertErrorCode(t, h.run("search", "invoice", "--account", "missing", "--json"), errs.CodeUnknownAccount)
		assertErrorCode(t, h.run("search", "invoice", "--account", "work", "--account", "work", "--json"), errs.CodeInvalidAccountScope)
		allAndAccount := h.run("search", "invoice", "--all-accounts", "--account", "work", "--json")
		assertFrameworkUsage(t, allAndAccount)
		assertNoCode(t, allAndAccount)
	})

	t.Run("capability errors", func(t *testing.T) {
		h := seededMessagesHarnessWithProvider(t, testkit.NewFakeProvider("fake"), "work")
		assertErrorCode(t, h.run("list", "--json"), errs.CodeUnsupportedCapability)
		assertErrorCode(t, h.run("search", "invoice", "--json"), errs.CodeCapabilityNotImplemented)

		staleSearch := seededMessagesHarnessWithProvider(t, testkit.NewFakeStaleMailboxProvider("fake"), "work")
		res := staleSearch.run("search", "invoice", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body := errorJSON(t, res)
		if body["account"] != "work" || body["provider"] != "fake" || body["capability"] != string(provider.CapMessagesSearch) {
			t.Fatalf("stale search body = %#v", body)
		}

		staleList := seededMessagesHarnessWithProvider(t, testkit.NewFakeFolderProvider("fake", testkit.SampleFolders()), "work")
		res = staleList.run("list", "--json")
		assertErrorCode(t, res, errs.CodeCapabilityNotImplemented)
		body = errorJSON(t, res)
		if body["account"] != "work" || body["provider"] != "fake" || body["capability"] != string(provider.CapMessagesList) {
			t.Fatalf("stale list body = %#v", body)
		}
	})

	// One broken account must not hide the others' mail: its rows are left
	// out and a warning names it.
	t.Run("one failing account gives partial results", func(t *testing.T) {
		fake := testkit.NewFakeMailboxProvider("fake", testkit.SampleMessages())
		fake.MailboxErrByAccount = map[string]error{"personal": errors.New(`account "personal" provider failure`)}
		h := seededMessagesHarnessWithProvider(t, fake, "work")
		var out searchResult
		decodeOK(t, h.run("search", "invoice", "--all-accounts", "--json"), &out)
		if len(out.Messages) == 0 || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "personal") || out.Total != nil {
			t.Fatalf("partial result = %d rows, warnings %v, total %v", len(out.Messages), out.Warnings, out.Total)
		}
		for _, row := range out.Messages {
			if row.Account != "work" {
				t.Fatalf("row from failing account: %+v", row)
			}
		}
		human := h.run("search", "invoice", "--all-accounts")
		if human.Exit != 0 || !strings.Contains(human.Stderr, "Warning: account personal is left out") {
			t.Fatalf("human partial result exit %d stderr %q", human.Exit, human.Stderr)
		}
	})

	t.Run("all accounts failing is an error", func(t *testing.T) {
		fake := testkit.NewFakeMailboxProvider("fake", testkit.SampleMessages())
		fake.MailboxErrByAccount = map[string]error{"personal": errors.New("down"), "work": errors.New("down")}
		h := seededMessagesHarnessWithProvider(t, fake, "work")
		if res := h.run("search", "invoice", "--all-accounts", "--json"); res.Exit == 0 {
			t.Fatal("search with every account failing succeeded")
		}
	})
}

func TestMessagesNoRowSelectorsAndReadOnly(t *testing.T) {
	h := seededMessagesHarness(t, "work")
	var raw map[string]any
	decodeOK(t, h.run("search", "invoice", "--json"), &raw)
	if _, ok := raw["row"]; ok {
		t.Fatalf("row field leaked into JSON: %#v", raw)
	}
	if _, ok := raw["index"]; ok {
		t.Fatalf("index field leaked into JSON: %#v", raw)
	}
	// A row number from a table is not a message id.
	if res := h.run("read", "1", "--json"); res.Exit == 0 {
		t.Fatalf("read 1 succeeded: %s", res.Stdout)
	}

	before := hashTree(t, h.root)
	decodeOK(t, h.run("search", "invoice", "--json"), &searchResult{})
	decodeOK(t, h.run("list", "--json"), &listMessagesResult{})
	decodeOK(t, h.run("search", "invoice", "--dry-run", "--json"), &searchResult{})
	decodeOK(t, h.run("list", "--dry-run", "--json"), &listMessagesResult{})
	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("message reads mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestMessagesSecretSweep(t *testing.T) {
	h := seededMessagesHarness(t, "work")
	account := folderWorkAccount()
	secrets := []string{"message-access-token-12345", "message-refresh-token-12345"}
	if err := testkit.SeedCredential(h.root, account, &testkit.TokenCache{
		Version:      1,
		Provider:     account.Provider,
		AccountName:  account.Name,
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		ExpiresAt:    time.Unix(1893456000, 0),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	for _, args := range [][]string{
		{"search", "invoice"},
		{"search", "invoice", "--json"},
		{"search", "invoice", "--json", "--jq", ".scope"},
		{"search", "invoice", "--template", "{{.scope}}"},
		{"search", "invoice", "--plain"},
		{"search", "invoice", "--json", "--include-meta"},
		{"list"},
		{"list", "--json"},
		{"list", "--json", "--jq", ".scope"},
		{"list", "--template", "{{.scope}}"},
		{"list", "--plain"},
		{"list", "--json", "--include-meta"},
	} {
		res := h.run(args...)
		testkit.AssertNoSecretLeak(t, res, secrets...)
		assertNoPathLeak(t, res, h.root, h.configPath)
	}
}

func seededMessagesHarness(t *testing.T, active string) *cliHarness {
	t.Helper()
	return seededMessagesHarnessWithProvider(t, testkit.NewFakeMailboxProvider("fake", testkit.SampleMessages()), active)
}

func seededMessagesHarnessWithProvider(t *testing.T, fake *testkit.FakeProvider, active string) *cliHarness {
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

func assertMessagesLabelled(t *testing.T, rows []mail.MessageSummary, account, providerName string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("no messages returned")
	}
	for i, row := range rows {
		if row.Account != account || row.Provider != providerName {
			t.Fatalf("message[%d] label = %s/%s, want %s/%s; row=%#v", i, row.Account, row.Provider, account, providerName, row)
		}
	}
}

func messageIDs(rows []mail.MessageSummary) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}
