package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/outlook"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const archivePolicyProcessHelperEnv = "MAILDO_ARCHIVE_POLICY_HELPER_PROCESS"

func TestArchivePolicyConcurrentSubprocessReadsAreDeterministic(t *testing.T) {
	h := seededOutlookArchiveProcessHarness(t)
	before := hashTree(t, h.root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type process struct {
		cmd    *exec.Cmd
		stdout bytes.Buffer
		stderr bytes.Buffer
	}
	processes := make([]*process, 0, 10)
	for i := 0; i < 10; i++ {
		item := &process{}
		item.cmd = archivePolicyHelperCommand(exe, h.configPath, h.server.srv.URL, "-")
		item.cmd.Stdout = &item.stdout
		item.cmd.Stderr = &item.stderr
		if err := item.cmd.Start(); err != nil {
			t.Fatalf("start subprocess %d: %v", i, err)
		}
		processes = append(processes, item)
	}

	var first []byte
	for i, item := range processes {
		if err := item.cmd.Wait(); err != nil {
			t.Fatalf("subprocess %d failed: %v stdout=%q stderr=%q", i, err, item.stdout.String(), item.stderr.String())
		}
		if item.stdout.Len() == 0 || item.stderr.Len() != 0 {
			t.Fatalf("subprocess %d output stdout=%q stderr=%q", i, item.stdout.String(), item.stderr.String())
		}
		data := item.stdout.Bytes()
		var report archivePolicyCheckResult
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatalf("decode subprocess %d: %v\n%s", i, err, data)
		}
		if report.Account != "work" || report.Provider != "outlook" || report.Status != mail.ArchivePolicyReportPartial || report.ArchiveFolder == nil || report.ArchiveFolder.ID != "archive-id" || report.Warnings == nil {
			t.Fatalf("subprocess %d report=%#v", i, report)
		}
		if first == nil {
			first = append([]byte(nil), data...)
		} else if !bytes.Equal(first, data) {
			t.Fatalf("subprocess %d output differs\nfirst=%s\ngot=%s", i, first, data)
		}
	}

	if h.server.archiveSettingsCalls.Load() != 10 || h.server.archiveFolderCalls.Load() != 10 || h.server.archivePolicyCalls.Load() != 20 || h.server.tokenCalls.Load() != 0 {
		t.Fatalf("requests settings=%d folder=%d total=%d token=%d", h.server.archiveSettingsCalls.Load(), h.server.archiveFolderCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load())
	}
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("concurrent archive subprocesses mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
	if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("concurrent archive subprocesses wrote action log: %v", err)
	}
}

func TestArchivePolicyInterruptedProcessEmitsNoPartialOutput(t *testing.T) {
	h := seededOutlookArchiveProcessHarness(t)
	before := hashTree(t, h.root)
	arrived := make(chan struct{})
	release := make(chan struct{})
	h.server.mu.Lock()
	h.server.archiveFolderArrived = arrived
	h.server.archiveFolderRelease = release
	h.server.mu.Unlock()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd := archivePolicyHelperCommand(exe, h.configPath, h.server.srv.URL, "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for blocked archive folder request")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("interrupted archive helper exited successfully")
	}
	close(release)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("interrupted process emitted output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("interrupted archive process mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}

	recoveredPath := filepath.Join(root, "recovered.json")
	recovery := archivePolicyHelperCommand(exe, h.configPath, h.server.srv.URL, recoveredPath)
	if output, err := recovery.CombinedOutput(); err != nil {
		t.Fatalf("fresh archive process failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(recoveredPath)
	if err != nil {
		t.Fatal(err)
	}
	var recovered archivePolicyCheckResult
	if err := json.Unmarshal(data, &recovered); err != nil || recovered.ArchiveFolder == nil || recovered.ArchiveFolder.ID != "archive-id" {
		t.Fatalf("recovered report=%#v err=%v output=%s", recovered, err, data)
	}
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("fresh archive recovery read mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
	if h.server.archiveSettingsCalls.Load() != 2 || h.server.archiveFolderCalls.Load() != 2 || h.server.tokenCalls.Load() != 0 {
		t.Fatalf("interruption/recovery requests settings=%d folder=%d token=%d", h.server.archiveSettingsCalls.Load(), h.server.archiveFolderCalls.Load(), h.server.tokenCalls.Load())
	}
}

func TestArchivePolicyClosedStdoutPipeDoesNotRetryOrMutateState(t *testing.T) {
	h := seededOutlookArchiveProcessHarness(t)
	before := hashTree(t, h.root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := archivePolicyHelperCommand(exe, h.configPath, h.server.srv.URL, "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("archive helper with closed stdout pipe exited successfully")
	}
	if strings.Contains(stderr.String(), "seed-outlook-") || strings.Contains(stderr.String(), h.root) || strings.Contains(stderr.String(), h.server.srv.URL) {
		t.Fatalf("closed-pipe error leaked secret or path: %q", stderr.String())
	}
	if h.server.archiveSettingsCalls.Load() != 1 || h.server.archiveFolderCalls.Load() != 1 || h.server.archivePolicyCalls.Load() != 2 || h.server.tokenCalls.Load() != 0 {
		t.Fatalf("closed-pipe requests settings=%d folder=%d total=%d token=%d", h.server.archiveSettingsCalls.Load(), h.server.archiveFolderCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load())
	}
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("closed stdout pipe mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
	if _, err := os.Stat(filepath.Join(h.root, "audit", "actions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("closed stdout pipe wrote action log: %v", err)
	}

	recoveredPath := filepath.Join(t.TempDir(), "recovered.json")
	recovery := archivePolicyHelperCommand(exe, h.configPath, h.server.srv.URL, recoveredPath)
	if output, err := recovery.CombinedOutput(); err != nil {
		t.Fatalf("fresh archive process after closed pipe failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(recoveredPath)
	if err != nil {
		t.Fatal(err)
	}
	var recovered archivePolicyCheckResult
	if err := json.Unmarshal(data, &recovered); err != nil || recovered.ArchiveFolder == nil || recovered.ArchiveFolder.ID != "archive-id" {
		t.Fatalf("recovered report=%#v err=%v output=%s", recovered, err, data)
	}
	if after := hashTree(t, h.root); !sameStrings(before, after) {
		t.Fatalf("closed-pipe recovery mutated runtime tree\nbefore=%#v\nafter=%#v", before, after)
	}
	if h.server.archiveSettingsCalls.Load() != 2 || h.server.archiveFolderCalls.Load() != 2 || h.server.archivePolicyCalls.Load() != 4 || h.server.tokenCalls.Load() != 0 {
		t.Fatalf("closed-pipe recovery requests settings=%d folder=%d total=%d token=%d", h.server.archiveSettingsCalls.Load(), h.server.archiveFolderCalls.Load(), h.server.archivePolicyCalls.Load(), h.server.tokenCalls.Load())
	}
}

func TestArchivePolicyProcessHelper(t *testing.T) {
	if os.Getenv(archivePolicyProcessHelperEnv) != "1" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(os.Args) < sep+4 {
		fmt.Fprintln(os.Stderr, "missing archive helper arguments")
		os.Exit(2)
	}
	configPath := os.Args[sep+1]
	serverURL := os.Args[sep+2]
	outPath := os.Args[sep+3]
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	registry := testkit.NewOutlookRegistry(outlook.Options{
		AuthorizeURL:    serverURL + "/authorize",
		TokenURL:        serverURL + "/token",
		DeviceCodeURL:   serverURL + "/devicecode",
		GraphBaseURL:    serverURL,
		Now:             func() time.Time { return now },
		LoopbackTimeout: time.Second,
		DevicePollSleep: func(context.Context, time.Duration) error { return nil },
	})
	app := newApp(Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())})
	args := []string{"--config", configPath, "outlook", "archive-policy", "check", "--json"}
	if outPath == "-" {
		exit := app.RunIO(args, bytes.NewReader(nil), os.Stdout, os.Stderr)
		os.Exit(exit)
	}
	result := testutil.RunWith(app, testutil.Options{LookupEnv: func(name string) (string, bool) {
		if name == "MAILDO_OUTLOOK_CLIENT_ID" {
			return "cli-client-id", true
		}
		return "", false
	}}, args...)
	if result.Exit != 0 {
		fmt.Fprintf(os.Stderr, "archive helper exit=%d stderr=%s", result.Exit, result.Stderr)
		os.Exit(result.Exit)
	}
	if err := os.WriteFile(outPath, []byte(result.Stdout), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func seededOutlookArchiveProcessHarness(t *testing.T) *outlookHarness {
	t.Helper()
	h := newOutlookHarness(t)
	account := outlookFoldersAccount()
	if err := testkit.SeedRegistry(h.configPath, "work", account); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SeedOutlookCache(h.root, "work", func(cache *testkit.TokenCache) {
		cache.VerifiedEmail = account.Email
		cache.ProviderAccountID = account.ProviderAccountID
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func archivePolicyHelperCommand(exe, configPath, serverURL, outPath string) *exec.Cmd {
	cmd := exec.Command(exe, "-test.run=^TestArchivePolicyProcessHelper$", "--", configPath, serverURL, outPath)
	cmd.Env = append(os.Environ(), archivePolicyProcessHelperEnv+"=1", "MAILDO_OUTLOOK_CLIENT_ID=cli-client-id")
	return cmd
}
