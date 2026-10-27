package audit

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
)

func TestAppendWritesSingleJSONLEntryAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	when := time.Date(2026, 7, 14, 7, 0, 0, 0, time.UTC)
	entry := Entry{
		Version:   1,
		CreatedAt: when,
		Command:   "mark read",
		Operation: mail.MutationMarkRead,
		Selector:  mail.BulkSelectorSummary{Kind: "ids", TargetCount: 1},
		Counts:    mail.BulkMutationCounts{Total: 1, Succeeded: 1},
		Targets: []mail.AppliedMutationTarget{{
			Account: "work", Provider: "fake", Operation: mail.MutationMarkRead, ID: "msg-1", Status: "succeeded",
		}},
	}
	if err := Append(root, entry); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := filepath.Join(root, RelativePath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Fatalf("entry was not one JSONL line: %q", data)
	}
	var got Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if got.Version != 1 || !got.CreatedAt.Equal(when) || got.Command != "mark read" || got.Operation != mail.MutationMarkRead {
		t.Fatalf("entry = %#v", got)
	}
	assertPerm(t, filepath.Dir(path), 0o700)
	assertPerm(t, path, 0o600)
}

func TestAppendChmodsExistingRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not stable on windows")
	}
	root := t.TempDir()
	path := filepath.Join(root, RelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, minimalEntry()); err != nil {
		t.Fatalf("append: %v", err)
	}
	assertPerm(t, path, 0o600)
}

func TestAppendRejectsDirectorySymlinkAndNonRegularLogPath(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "directory", setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", setup: func(t *testing.T, path string) {
			t.Helper()
			if runtime.GOOS == "windows" {
				t.Skip("symlink privileges are environment dependent on windows")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", setup: func(t *testing.T, path string) {
			t.Helper()
			if runtime.GOOS == "windows" {
				t.Skip("fifo is unix-specific")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscallMkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, RelativePath)
			tt.setup(t, path)
			if err := Append(root, minimalEntry()); err == nil {
				t.Fatal("append succeeded")
			}
		})
	}
}

func TestAppendReportsShortWriteSyncAndCloseFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		file *faultFile
	}{
		{name: "short write", file: &faultFile{shortWrite: true}},
		{name: "sync", file: &faultFile{syncErr: errors.New("sync failed")}},
		{name: "close", file: &faultFile{closeErr: errors.New("close failed")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			err := appendWith(root, minimalEntry(), func(string, int, os.FileMode) (appendFile, error) {
				return tt.file, nil
			})
			if err == nil {
				t.Fatal("append succeeded")
			}
		})
	}
}

func TestAppendConcurrentWritersProduceCompleteJSONLines(t *testing.T) {
	root := t.TempDir()
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := minimalEntry()
			entry.CreatedAt = entry.CreatedAt.Add(time.Duration(i) * time.Second)
			entry.Command = "archive"
			entry.Targets[0].ID = "msg-" + strconv.Itoa(i)
			errs <- Append(root, entry)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	data, err := os.ReadFile(filepath.Join(root, RelativePath))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != workers {
		t.Fatalf("line count = %d, want %d\n%s", len(lines), workers, data)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode line: %v\n%s", err, line)
		}
		if len(entry.Targets) != 1 || entry.Targets[0].ID == "" {
			t.Fatalf("entry targets = %#v", entry.Targets)
		}
		if seen[entry.Targets[0].ID] {
			t.Fatalf("duplicate target id in log: %s", entry.Targets[0].ID)
		}
		seen[entry.Targets[0].ID] = true
	}
}

func minimalEntry() Entry {
	return Entry{
		Version:   1,
		CreatedAt: time.Date(2026, 7, 14, 7, 0, 0, 0, time.UTC),
		Command:   "archive",
		Operation: mail.MutationArchive,
		Selector:  mail.BulkSelectorSummary{Kind: "id", TargetCount: 1},
		Counts:    mail.BulkMutationCounts{Total: 1, Succeeded: 1},
		Targets:   []mail.AppliedMutationTarget{{Account: "work", Provider: "fake", Operation: mail.MutationArchive, ID: "msg-1", Status: "succeeded"}},
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s perm = %#o, want %#o", path, got, want)
	}
}

type faultFile struct {
	shortWrite bool
	syncErr    error
	closeErr   error
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.shortWrite {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func (f *faultFile) Stat() (os.FileInfo, error) {
	return fakeFileInfo{}, nil
}

func (f *faultFile) Sync() error {
	return f.syncErr
}

func (f *faultFile) Close() error {
	return f.closeErr
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "actions.jsonl" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

var _ io.Writer = (*faultFile)(nil)
