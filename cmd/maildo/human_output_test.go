package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// A short id printed by list works in every command that takes a message.
func TestShortIDsFromListWorkInLaterCommands(t *testing.T) {
	h, fake := seededMutationHarness(t)
	list := h.run("list")
	decodeOK(t, list, nil)
	short := shortID("work", "work-new")
	if !strings.Contains(list.Stdout, short) {
		t.Fatalf("list does not show the short id %s:\n%s", short, list.Stdout)
	}

	var read readResult
	decodeOK(t, h.run("read", short, "--json"), &read)
	if read.Message.ID != "work-new" {
		t.Fatalf("read by short id = %q", read.Message.ID)
	}

	human := h.run("mark", "read", short)
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, `Marked "Contract update" from Client as read.`) {
		t.Fatalf("mark read output = %q", human.Stdout)
	}
	if got := strings.Join(fake.MutationCallLog, ","); got != "work:work-new" {
		t.Fatalf("mutation calls = %s", got)
	}

	// The short id knows its account; naming another one is an error.
	assertErrorCode(t, h.run("archive", short, "--account", "personal", "--json"), errs.CodeInvalidSelector)
	// A short id maildo never printed is explained.
	res := h.run("read", "abcdef0")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "unknown short id abcdef0") || !strings.Contains(res.Stderr, "maildo list") {
		t.Fatalf("unknown short id: exit %d stderr %s", res.Exit, res.Stderr)
	}
	// The cache holds only ids and account names, readable by you alone.
	cache := filepath.Join(h.root, "short-ids.json")
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatalf("short id cache missing: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("short id cache mode = %v", info.Mode().Perm())
	}
}

func TestBulkPreviewPrintsTheApplyCommand(t *testing.T) {
	h, fake := seededMutationHarness(t)
	res := h.run("archive", "--query", "anything", "--all")
	decodeOK(t, res, nil)
	for _, want := range []string{
		"Would archive 4 messages in work:",
		"ID       FROM",
		"FOLDER",
		"Nothing was changed. To archive these messages, run:",
		"--account work --id work-new --id work-dupe-b --id work-dupe-a --id work-old --apply",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("preview lacks %q:\n%s", want, res.Stdout)
		}
	}
	if fake.MessageMutationCalls.Load() != 0 {
		t.Fatal("preview changed messages")
	}
}

// Ids from --jq, quoted or not, work in --ids-file, and a failed
// apply names the messages that did not change.
func TestIDsFileAcceptsJQOutputAndNamesFailures(t *testing.T) {
	h, _ := seededMutationHarness(t)
	path := filepath.Join(h.root, "ids.txt")
	if err := os.WriteFile(path, []byte("\"work-new\"\nwork-old\n\"missing-id\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview := decodePreview(t, h.run("mark", "read", "--ids-file", path, "--json"))
	targets := previewTargets(t, preview)
	if targets[0]["id"] != "work-new" || targets[2]["problem"] == nil {
		t.Fatalf("preview targets = %#v", targets)
	}
	res := h.run("mark", "read", "--ids-file", path, "--apply")
	if res.Exit == 0 {
		t.Fatal("apply with a missing id succeeded")
	}
	if !strings.Contains(res.Stderr, "1 of 3 messages could not be changed") || !strings.Contains(res.Stderr, "Failed: message "+shortID("work", "missing-id")) ||
		!strings.Contains(res.Stderr, "The other 2 messages were changed.") {
		t.Fatalf("partial failure stderr:\n%s", res.Stderr)
	}
}

func TestJQPrintsRawStrings(t *testing.T) {
	h, _ := seededMutationHarness(t)
	res := h.run("list", "--jq", ".messages[].id")
	decodeOK(t, res, nil)
	if !strings.HasPrefix(res.Stdout, "work-new\n") {
		t.Fatalf("jq output = %q", res.Stdout)
	}
	count := h.run("list", "--jq", ".count")
	if strings.TrimSpace(count.Stdout) != "4" {
		t.Fatalf("jq number = %q", count.Stdout)
	}
}

func TestRelativeTime(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 0, 0, 0, time.Local)
	now = func() time.Time { return base }
	t.Cleanup(func() { now = time.Now })
	for _, tt := range []struct {
		at   time.Time
		want string
	}{
		{base.Add(-20 * time.Second), "just now"},
		{base.Add(-1 * time.Minute), "1 minute ago"},
		{base.Add(-45 * time.Minute), "45 minutes ago"},
		{base.Add(-3 * time.Hour), "3 hours ago"},
		{base.Add(-26 * time.Hour), "yesterday"},
		{base.Add(-4 * 24 * time.Hour), "4 days ago"},
		{base.Add(-20 * 24 * time.Hour), "Oct 6"},
		{time.Date(2025, 3, 4, 9, 0, 0, 0, time.Local), "Mar 4, 2025"},
		{time.Time{}, "-"},
	} {
		if got := relativeTime(tt.at); got != tt.want {
			t.Errorf("relativeTime(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestTableTruncatesTheFlexibleColumn(t *testing.T) {
	tbl := newTable("ID", "SUBJECT", "DATE")
	tbl.flex = 1
	tbl.add("abc1234", strings.Repeat("long subject ", 10), "yesterday")
	tbl.add("def5678", "日本語の件名", "Oct 6")
	var b strings.Builder
	tbl.render(&b, 50, nil)
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if textWidth(line) > 50 {
			t.Fatalf("line wider than 50 columns: %q", line)
		}
	}
	if !strings.Contains(b.String(), "...") {
		t.Fatalf("long subject was not cut short:\n%s", b.String())
	}
}

func TestOperationPhrases(t *testing.T) {
	for _, tt := range []struct {
		op   mail.MessageMutationOperation
		past bool
		want string
	}{
		{mail.MutationArchive, true, `Archived "X" from Y`},
		{mail.MutationMove, false, `move "X" from Y to Clients`},
		{mail.MutationFlagClear, true, `Removed the flag from "X" from Y`},
		{mail.MutationMarkUnread, false, `mark "X" from Y as unread`},
	} {
		if got := opPhrase(tt.op, `"X" from Y`, "Clients", nil, tt.past); got != tt.want {
			t.Errorf("opPhrase(%s) = %q, want %q", tt.op, got, tt.want)
		}
	}
}

func TestAuthCommandsSpeakPlainly(t *testing.T) {
	h, _ := seededMutationHarness(t)
	list := h.run("auth", "list")
	decodeOK(t, list, nil)
	for _, want := range []string{"ACCOUNT", "PROVIDER", "STATUS", "* active account", "logged in"} {
		if !strings.Contains(list.Stdout, want) {
			t.Fatalf("auth list lacks %q:\n%s", want, list.Stdout)
		}
	}
	status := h.run("auth", "status")
	decodeOK(t, status, nil)
	if !strings.Contains(status.Stdout, "logged in, the active account.") || strings.Contains(status.Stdout, "credential cache") {
		t.Fatalf("auth status:\n%s", status.Stdout)
	}
	logout := h.run("auth", "logout", "personal")
	decodeOK(t, logout, nil)
	if !strings.Contains(logout.Stdout, "deleted its saved login") || strings.Contains(logout.Stdout, "Credential cache") {
		t.Fatalf("auth logout:\n%s", logout.Stdout)
	}
}

func TestLoginDryRunSpeaksPlainly(t *testing.T) {
	h := newMailServerHarness(t, "me@example.test")
	res := h.run("login", "--provider", "imap", "--name", "extra", "--email", "me@example.test",
		"--imap-host", "imap.example.test", "--smtp-host", "smtp.example.test", "--dry-run")
	decodeOK(t, res, nil)
	for _, want := range []string{`Would log in and save the login as account "extra" (imap)`, "IMAP:    imap.example.test:993, implicit TLS", "Nothing was changed."} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("login --dry-run lacks %q:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, " = ") || strings.Contains(res.Stdout, "DRY RUN") {
		t.Fatalf("login --dry-run still prints fields:\n%s", res.Stdout)
	}
}

func TestDraftReadLooksLikeRead(t *testing.T) {
	h := newSendHarness(t)
	var created struct {
		DraftID string `json:"draft_id"`
	}
	decodeOK(t, h.run("draft", "create", "--to", "Clara <clara@example.test>", "--subject", "Quote", "--body", "Here it is.", "--json"), &created)
	res := h.run("draft", "read", created.DraftID)
	decodeOK(t, res, nil)
	for _, want := range []string{"Subject:  Quote", "To:       Clara <clara@example.test>", "Here it is."} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("draft read lacks %q:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "Is Draft") || strings.Contains(res.Stdout, "Has Attachments") {
		t.Fatalf("draft read prints internal fields:\n%s", res.Stdout)
	}
}
