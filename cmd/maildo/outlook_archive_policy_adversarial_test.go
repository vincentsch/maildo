package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestArchivePolicyHumanOutputNeutralizesUnicodeControls(t *testing.T) {
	var unsafe strings.Builder
	unsafe.WriteString("prefix")
	for r := rune(0); r <= 0x1f; r++ {
		unsafe.WriteRune(r)
	}
	for r := rune(0x7f); r <= 0x9f; r++ {
		unsafe.WriteRune(r)
	}
	for _, r := range []rune{
		0x00ad, 0x061c, 0x200b, 0x200c, 0x200d, 0x200e, 0x200f,
		0x2028, 0x2029, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x2060, 0x2061, 0x2062, 0x2063, 0x2064, 0x2066, 0x2067,
		0x2068, 0x2069, 0xfeff,
	} {
		unsafe.WriteRune(r)
	}
	unsafe.WriteString("\x1b[31m\x1b]0;title\x07 combining:e\u0301 non-bmp:\U0001f4ec suffix")
	hostile := unsafe.String()

	report := resolvedArchivePolicyReport()
	report.MailboxPurpose = hostile
	report.ArchiveFolderID = hostile
	report.ArchiveFolder.ID = hostile
	report.ArchiveFolder.DisplayName = hostile
	fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": report})
	h := seededArchivePolicyHarness(t, fake, "work", "work")

	baselineFake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
	baseline := seededArchivePolicyHarness(t, baselineFake, "work", "work").run("outlook", "archive-policy", "check")
	decodeOK(t, baseline, nil)
	human := h.run("outlook", "archive-policy", "check")
	decodeOK(t, human, nil)
	if strings.Count(human.Stdout, "\n") != strings.Count(baseline.Stdout, "\n") {
		t.Fatalf("hostile values changed the human line count: %q", human.Stdout)
	}
	for _, r := range human.Stdout {
		if r != '\n' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)) {
			t.Fatalf("human output retained unsafe rune U+%04X: %q", r, human.Stdout)
		}
	}
	if strings.Contains(human.Stdout, "\x1b") {
		t.Fatalf("human output retained an ANSI escape: %q", human.Stdout)
	}

	machine := h.run("outlook", "archive-policy", "check", "--json")
	decodeOK(t, machine, nil)
	var got archivePolicyCheckResult
	if err := json.Unmarshal([]byte(machine.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.MailboxPurpose != hostile || got.ArchiveFolderID != hostile || got.ArchiveFolder == nil || got.ArchiveFolder.ID != hostile || got.ArchiveFolder.DisplayName != hostile {
		t.Fatalf("machine values changed: %#v", got)
	}

	jq := h.run("outlook", "archive-policy", "check", "--json", "--jq", ".archive_folder.display_name")
	decodeOK(t, jq, nil)
	// --jq prints strings raw, like gh, with terminal escapes removed. The
	// exact value is in --json.
	if !strings.HasPrefix(jq.Stdout, "prefix") || !strings.HasSuffix(jq.Stdout, "suffix\n") || strings.Contains(jq.Stdout, "\x1b") {
		t.Fatalf("jq output=%q", jq.Stdout)
	}
	template := h.run("outlook", "archive-policy", "check", "--template", "{{if eq .archive_folder.display_name .mailbox_purpose}}exact{{else}}changed{{end}}")
	decodeOK(t, template, nil)
	if template.Stdout != "exact\n" {
		t.Fatalf("template did not receive identical machine values: %q", template.Stdout)
	}
}

func TestArchivePolicyOutputModesAcrossReportShapes(t *testing.T) {
	resolved := resolvedArchivePolicyReport()
	resolved.ArchiveFolder.TotalItemCount = 0
	resolved.ArchiveFolder.TotalItemCountKnown = true
	resolved.ArchiveFolder.UnreadItemCount = 0
	resolved.ArchiveFolder.UnreadItemCountKnown = false
	resolved.ArchiveFolder.ChildFolderCount = 2
	resolved.ArchiveFolder.ChildFolderCountKnown = true
	missing := resolvedArchivePolicyReport()
	missing.ArchiveFolderID = ""
	missing.ArchiveFolder = nil
	missing.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}
	unresolved := missing
	unresolved.ArchiveFolderID = "archive-id"
	unresolved.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}

	for _, tt := range []struct {
		name   string
		report mail.ArchivePolicyReport
	}{
		{name: "resolved", report: resolved},
		{name: "settings unavailable", report: unavailableArchivePolicyReport()},
		{name: "folder id missing", report: missing},
		{name: "folder unresolved", report: unresolved},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": tt.report})
			h := seededArchivePolicyHarness(t, fake, "work", "work")

			human := h.run("outlook", "archive-policy", "check")
			decodeOK(t, human, nil)
			if !strings.Contains(human.Stdout, "Archive folder\n") || !strings.Contains(human.Stdout, "Exchange Online Archive") {
				t.Fatalf("human report is incomplete: %q", human.Stdout)
			}
			if tt.name == "resolved" && (!strings.Contains(human.Stdout, "Messages:    0\n") || !strings.Contains(human.Stdout, "Unread:      unknown") || !strings.Contains(human.Stdout, "Subfolders:  2\n")) {
				t.Fatalf("presence-aware counts collapsed: %q", human.Stdout)
			}

			normal := h.run("outlook", "archive-policy", "check", "--json")
			dry := h.run("outlook", "archive-policy", "check", "--dry-run", "--json")
			identity := h.run("outlook", "archive-policy", "check", "--json", "--jq=")
			decodeOK(t, normal, nil)
			decodeOK(t, dry, nil)
			decodeOK(t, identity, nil)
			if normal.Stdout != dry.Stdout {
				t.Fatalf("normal/dry differ\nnormal=%q\ndry=%q", normal.Stdout, dry.Stdout)
			}
			var normalValue, identityValue any
			if err := json.Unmarshal([]byte(normal.Stdout), &normalValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(identity.Stdout), &identityValue); err != nil || !reflect.DeepEqual(normalValue, identityValue) {
				t.Fatalf("empty jq changed the machine model: err=%v normal=%#v identity=%#v", err, normalValue, identityValue)
			}

			jq := h.run("outlook", "archive-policy", "check", "--json", "--jq", ".warnings")
			decodeOK(t, jq, nil)
			var warnings []mail.ArchivePolicyWarning
			if err := json.Unmarshal([]byte(jq.Stdout), &warnings); err != nil || !reflect.DeepEqual(warnings, tt.report.Warnings) {
				t.Fatalf("jq warnings=%#v err=%v output=%q", warnings, err, jq.Stdout)
			}
			template := h.run("outlook", "archive-policy", "check", "--template", "{{.account}}:{{.status}}:{{len .warnings}}")
			decodeOK(t, template, nil)
			if strings.TrimSpace(template.Stdout) != "work:"+string(tt.report.Status)+":"+strconv.Itoa(len(tt.report.Warnings)) {
				t.Fatalf("template output=%q", template.Stdout)
			}

			meta := h.run("outlook", "archive-policy", "check", "--include-meta", "--json")
			decodeOK(t, meta, nil)
			var envelope struct {
				Meta struct {
					Extra map[string]any `json:"extra"`
				} `json:"meta"`
			}
			if err := json.Unmarshal([]byte(meta.Stdout), &envelope); err != nil {
				t.Fatal(err)
			}
			codes, ok := envelope.Meta.Extra["warning_codes"].([]any)
			if !ok || len(codes) != len(tt.report.Warnings) {
				t.Fatalf("metadata warning codes=%#v", envelope.Meta.Extra["warning_codes"])
			}

			for _, flag := range []string{"--no-color", "--no-ansi", "--no-pager", "--quiet"} {
				controlled := h.run("outlook", "archive-policy", "check", "--json", flag)
				decodeOK(t, controlled, nil)
				if controlled.Stdout != normal.Stdout {
					t.Fatalf("%s changed JSON output", flag)
				}
			}
			if fake.CapabilityCalls.Load() != fake.AccountClientCalls.Load() || fake.AccountClientCalls.Load() != fake.ArchiveReportCalls.Load() || fake.ArchiveReportCalls.Load() == 0 {
				t.Fatalf("provider calls capability=%d client=%d report=%d", fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.ArchiveReportCalls.Load())
			}
		})
	}
}

func TestArchivePolicyRendererValidationPrecedesProviderWork(t *testing.T) {
	for _, args := range [][]string{
		{"outlook", "archive-policy", "check", "--json", "--jq", ".broken["},
		{"outlook", "archive-policy", "check", "--template", "{{"},
	} {
		fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
		h := seededArchivePolicyHarness(t, fake, "work", "work")
		res := h.run(args...)
		if res.Exit == 0 || res.Stdout != "" || fake.CapabilityCalls.Load() != 0 || fake.AccountClientCalls.Load() != 0 || fake.ArchiveReportCalls.Load() != 0 {
			t.Fatalf("invalid renderer %v result=%#v calls=%d/%d/%d", args, res, fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.ArchiveReportCalls.Load())
		}
	}
}

type failAfterOutputWriter struct {
	limit int
	buf   bytes.Buffer
	calls int
}

func (w *failAfterOutputWriter) Write(p []byte) (int, error) {
	w.calls++
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return 0, errors.New("synthetic output failure")
	}
	if remaining > len(p) {
		remaining = len(p)
	}
	n, _ := w.buf.Write(p[:remaining])
	if n != len(p) {
		return n, errors.New("synthetic output failure")
	}
	return n, nil
}

func TestArchivePolicyOutputWriteFailuresDoNotRetryOrMutateState(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "human"},
		{name: "json", args: []string{"--json"}},
		{name: "jq", args: []string{"--json", "--jq", ".status"}},
		{name: "template", args: []string{"--template", "{{.status}}"}},
	} {
		for _, limit := range []int{0, 7} {
			t.Run(mode.name+"/limit-"+strconv.Itoa(limit), func(t *testing.T) {
				fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": resolvedArchivePolicyReport()})
				h := seededArchivePolicyHarness(t, fake, "work", "work")
				before := hashTree(t, h.root)
				writer := &failAfterOutputWriter{limit: limit}
				var stderr bytes.Buffer
				args := []string{"--config", h.configPath, "outlook", "archive-policy", "check"}
				args = append(args, mode.args...)
				exit := h.app().RunIO(args, strings.NewReader(""), writer, &stderr)
				if exit == 0 || writer.calls == 0 || fake.CapabilityCalls.Load() != 1 || fake.AccountClientCalls.Load() != 1 || fake.ArchiveReportCalls.Load() != 1 {
					t.Fatalf("exit=%d calls=%d provider=%d/%d/%d stderr=%q", exit, writer.calls, fake.CapabilityCalls.Load(), fake.AccountClientCalls.Load(), fake.ArchiveReportCalls.Load(), stderr.String())
				}
				if limit == 0 && writer.buf.Len() != 0 {
					t.Fatalf("byte-zero failure wrote %q", writer.buf.String())
				}
				if limit > 0 && writer.buf.Len() != limit {
					t.Fatalf("mid-write accepted %d bytes, want %d", writer.buf.Len(), limit)
				}
				if strings.Contains(stderr.String(), "seed-") || strings.Contains(stderr.String(), h.root) {
					t.Fatalf("write error leaked secret/path: %q", stderr.String())
				}
				if after := hashTree(t, h.root); !sameStrings(before, after) {
					t.Fatalf("output failure mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
				}
				if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
					t.Fatalf("output failure wrote action log: %v", err)
				}
			})
		}
	}
}

func TestArchivePolicyRuntimeTreeChangesAreAllowlisted(t *testing.T) {
	resolved := resolvedArchivePolicyReport()
	missing := resolved
	missing.ArchiveFolderID = ""
	missing.ArchiveFolder = nil
	missing.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing}
	unresolved := missing
	unresolved.ArchiveFolderID = "archive-id"
	unresolved.Warnings = []mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved}
	for _, tt := range []struct {
		name      string
		report    mail.ArchivePolicyReport
		reportErr error
		args      []string
	}{
		{name: "resolved", report: resolved, args: []string{"--json"}},
		{name: "resolved dry run", report: resolved, args: []string{"--dry-run", "--json"}},
		{name: "settings unavailable", report: unavailableArchivePolicyReport(), args: []string{"--json"}},
		{name: "folder id missing", report: missing, args: []string{"--json"}},
		{name: "folder unresolved", report: unresolved, args: []string{"--json"}},
		{name: "provider failure", reportErr: errors.New("safe fixture failure"), args: []string{"--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := testkit.NewFakeArchivePolicyProvider("outlook", map[string]mail.ArchivePolicyReport{"work": tt.report})
			if tt.reportErr != nil {
				fake.ArchiveReportErrByAccount = map[string]error{"work": tt.reportErr}
			}
			h := seededArchivePolicyHarness(t, fake, "work", "work")
			before := hashTree(t, h.root)
			args := []string{"outlook", "archive-policy", "check"}
			res := h.run(append(args, tt.args...)...)
			if tt.reportErr == nil {
				decodeOK(t, res, nil)
			} else if res.Exit == 0 || res.Stdout != "" {
				t.Fatalf("provider failure result=%#v", res)
			}
			if after := hashTree(t, h.root); !sameStrings(before, after) {
				t.Fatalf("archive report mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestArchivePolicyRefreshWithoutReplacementRetainsRefreshToken(t *testing.T) {
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(cache *testkit.TokenCache) {
		cache.VerifiedEmail = account.Email
		cache.ProviderAccountID = account.ProviderAccountID
		cache.ExpiresAt = time.Unix(1, 0)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := loadCredentialCache(h, "outlook", "work")
	if err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoBefore, err := os.Stat(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	treeBefore := hashTree(t, h.root)
	h.server.refreshMissingReplacement.Store(true)
	res := h.runNoBrowser("outlook", "archive-policy", "check", "--json")
	decodeOK(t, res, nil)
	after, err := loadCredentialCache(h, "outlook", "work")
	if err != nil {
		t.Fatal(err)
	}
	if after.RefreshToken != before.RefreshToken || after.AccessToken != "cli-refreshed-access-secret-12345" || h.server.tokenCalls.Load() != 1 || h.server.archivePolicyCalls.Load() != 2 || h.server.deviceCodeCalls.Load() != 0 || h.browserCalls.Load() != 0 {
		t.Fatalf("cache refresh=%t access=%q calls token=%d archive=%d device=%d browser=%d", after.RefreshToken == before.RefreshToken, after.AccessToken, h.server.tokenCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.deviceCodeCalls.Load(), h.browserCalls.Load())
	}
	configAfter, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoAfter, err := os.Stat(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) || configInfoBefore.Mode() != configInfoAfter.Mode() || configInfoBefore.Size() != configInfoAfter.Size() || !configInfoBefore.ModTime().Equal(configInfoAfter.ModTime()) || !os.SameFile(configInfoBefore, configInfoAfter) {
		t.Fatal("refresh changed or replaced the account registry")
	}
	credentialFile := filepath.Join(h.root, "auth", "outlook", "work.json")
	credentialInfo, err := os.Stat(credentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if credentialInfo.Mode().Perm() != 0o600 {
		t.Fatalf("refreshed credential mode = %v", credentialInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(credentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "work.json" || entries[0].IsDir() {
		t.Fatalf("refresh left credential remnants: %#v", entries)
	}
	credentialPath := filepath.Join("auth", "outlook", "work.json")
	lockPath := filepath.Join("auth", ".transaction.lock")
	if treeAfter := hashTree(t, h.root); !sameStrings(treeWithoutFiles(treeBefore, credentialPath, lockPath), treeWithoutFiles(treeAfter, credentialPath, lockPath)) {
		t.Fatalf("refresh changed files outside %q\nbefore=%#v\nafter=%#v", credentialPath, treeBefore, treeAfter)
	}
}

func treeWithoutFile(tree []string, path string) []string {
	return treeWithoutFiles(tree, path)
}

func treeWithoutFiles(tree []string, paths ...string) []string {
	filtered := make([]string, 0, len(tree))
	prefixes := make([]string, 0, len(paths))
	for _, path := range paths {
		prefixes = append(prefixes, path+" ")
	}
	for _, entry := range tree {
		skip := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

var _ io.Writer = (*failAfterOutputWriter)(nil)
