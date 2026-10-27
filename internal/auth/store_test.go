package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestStoreOpaqueBytesPermissionsAndDelete(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	data := []byte("{\"access_token\":\"access-secret-12345\",\"refresh_token\":\"refresh-secret-12345\"}\n")
	if err := store.SaveBytes("fake", "work", data); err != nil {
		t.Fatalf("SaveBytes: %v", err)
	}
	path := store.cachePath("fake", "work")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %o, want 0600", got)
	}
	dirSt, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if got := dirSt.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir mode = %o, want 0700", got)
	}
	loaded, present, err := store.LoadBytes("fake", "work")
	if err != nil || !present {
		t.Fatalf("LoadBytes = (%q, %t, %v), want present nil", string(loaded), present, err)
	}
	if !bytes.Equal(loaded, data) {
		t.Fatalf("loaded bytes = %q, want %q", string(loaded), string(data))
	}
	loaded[0] = '['
	loadedAgain, present, err := store.LoadBytes("fake", "work")
	if err != nil || !present || !bytes.Equal(loadedAgain, data) {
		t.Fatalf("LoadBytes after caller mutation = (%q, %t, %v)", string(loadedAgain), present, err)
	}
	exists, err := store.Exists("fake", "work")
	if err != nil || !exists {
		t.Fatalf("Exists = (%t, %v), want true nil", exists, err)
	}
	deleted, err := store.Delete("fake", "work")
	if err != nil || !deleted {
		t.Fatalf("Delete = (%t, %v), want true nil", deleted, err)
	}
	deleted, err = store.Delete("fake", "work")
	if err != nil || deleted {
		t.Fatalf("Delete absent = (%t, %v), want false nil", deleted, err)
	}
	_, present, err = store.LoadBytes("fake", "work")
	if err != nil || present {
		t.Fatalf("LoadBytes absent present=%t err=%v, want false nil", present, err)
	}
}

func TestCleanupStaleRemovesOnlyOldMaildoStagingFiles(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	store := NewStore(root)
	authDir := filepath.Join(root, "auth", "fake")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	normal := filepath.Join(authDir, "normal.json")
	if err := os.WriteFile(normal, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write normal: %v", err)
	}
	stale := filepath.Join(authDir, ".normal.json.tmp-123456")
	if err := os.WriteFile(stale, []byte("secret should not be read"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	broad := filepath.Join(authDir, ".normal.json.tmp-leftover")
	if err := os.WriteFile(broad, []byte("not an owned staging basename"), 0o600); err != nil {
		t.Fatalf("write broad: %v", err)
	}
	configStale := filepath.Join(root, ".config.yaml.tmp-leftover")
	if err := os.WriteFile(configStale, []byte("config tmp"), 0o600); err != nil {
		t.Fatalf("write config stale: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age stale: %v", err)
	}
	if err := os.Chtimes(broad, old, old); err != nil {
		t.Fatalf("age broad: %v", err)
	}
	if err := os.Chtimes(configStale, old, old); err != nil {
		t.Fatalf("age config stale: %v", err)
	}
	freshStage := filepath.Join(authDir, ".fresh.json.tmp-stage-live")
	if err := os.WriteFile(freshStage, []byte("live stage"), 0o600); err != nil {
		t.Fatalf("write fresh stage: %v", err)
	}
	unrelated := filepath.Join(root, ".unrelated.tmp-keep")
	if err := os.WriteFile(unrelated, []byte("not maildo"), 0o600); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	if err := store.CleanupStale(configPath); err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale stat = %v, want not exist", err)
	}
	if _, err := os.Stat(configStale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config stale stat = %v, want not exist", err)
	}
	for _, path := range []string{normal, freshStage, broad, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(path), err)
		}
	}
}

func TestCleanupStaleFailsClosedOnAuthSymlink(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	store := NewStore(root)
	authDir := filepath.Join(root, "auth", "fake")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	target := filepath.Join(authDir, "target.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(authDir, ".target.json.tmp-123456")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := store.CleanupStale(configPath)
	var productErr *errs.Error
	if !errors.As(err, &productErr) || productErr.Code() != errs.CodeInvalidConfig {
		t.Fatalf("CleanupStale symlink err = %v, want invalid_config", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
}

func TestStoreErrorsArePathFreeProductErrors(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	path := store.cachePath("fake", "work")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir cache-as-dir: %v", err)
	}
	_, _, err := store.LoadBytes("fake", "work")
	var productErr *errs.Error
	if !errors.As(err, &productErr) {
		t.Fatalf("error = %v, want product error", err)
	}
	if productErr.Code() != errs.CodeInvalidConfig {
		t.Fatalf("code = %q, want %q", productErr.Code(), errs.CodeInvalidConfig)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), root) {
		t.Fatalf("error leaked path: %v", err)
	}
}

func TestStoreRejectsInvalidNamesBeforePathDerivation(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveBytes("../provider", "work", []byte("{}")); err == nil {
		t.Fatal("SaveBytes accepted invalid provider")
	}
	if _, _, err := store.LoadBytes("fake", "../work"); err == nil {
		t.Fatal("LoadBytes accepted invalid account")
	}
}
