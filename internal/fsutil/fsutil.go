// Package fsutil holds small file helpers for the config, the saved logins
// and the short id cache.
//
// They return plain I/O errors. The callers turn them into errors without
// file paths before anything is shown.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// staleTempAge is how old a temporary file must be before cleanup deletes
// it, so cleanup never removes one that another maildo is still using
// during a login or repair.
const staleTempAge = time.Hour

// EnsureDir creates dir and its parents, sets dir's mode to perm, and checks
// that dir is a real folder, not a link.
func EnsureDir(dir string, perm os.FileMode) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed directory is not a directory")
	}
	if err := os.Chmod(dir, perm); err != nil {
		return err
	}
	info, err = os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&^perm != 0 {
		return fmt.Errorf("managed directory security is invalid")
	}
	return nil
}

// WriteFileAtomic writes a temporary file next to path and renames it into
// place, so a reader sees either the old file or the new one. The temporary
// name ends in .next, which makes a cut-off write easy to recognize.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Base(path)
	tmpPath := filepath.Join(dir, "."+base+".next")
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTmp = false
	return SyncDir(dir)
}

// SyncDir flushes a folder to disk after a file in it was renamed or removed.
func SyncDir(dir string) error {
	return syncDir(dir)
}

// StagingGlobs returns the name patterns of maildo's temporary files, the
// only files cleanup looks at. The pattern for the config folder uses the
// config file's name, so another program's .name.tmp-* file there is left
// alone.
func StagingGlobs(root, configPath string) []string {
	var globs []string
	if configPath != "" && filepath.Clean(filepath.Dir(configPath)) == filepath.Clean(root) {
		globs = append(globs, filepath.Join(root, "."+filepath.Base(configPath)+".tmp-*"))
	}
	globs = append(globs, filepath.Join(root, "auth", "*", ".*.json.tmp-*"))
	return globs
}

// RemoveStale deletes old temporary files that maildo left behind, without
// reading them. Newer ones may belong to another maildo command that is
// still running, so they stay.
func RemoveStale(root, configPath string) error {
	for _, pattern := range StagingGlobs(root, configPath) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, match := range matches {
			if err := removeKnownStagingFile(root, configPath, match); err != nil {
				return err
			}
		}
	}
	authRoot := filepath.Join(root, "auth")
	err := filepath.WalkDir(authRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if isAuthStagingPath(root, path) && isAuthStagingBase(filepath.Base(path)) {
			return removeStaleFile(path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removeKnownStagingFile(root, configPath, path string) error {
	base := filepath.Base(path)
	switch {
	case isConfigStagingBase(root, configPath, path):
		return removeStaleFile(path)
	case isAuthStagingPath(root, path) && isAuthStagingBase(base):
		return removeStaleFile(path)
	default:
		return nil
	}
}

func removeStaleFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unexpected auth staging file type")
	}
	if time.Since(info.ModTime()) < staleTempAge {
		return nil
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func isConfigStagingBase(root, configPath, path string) bool {
	if configPath == "" || filepath.Clean(filepath.Dir(configPath)) != filepath.Clean(root) {
		return false
	}
	wantPrefix := "." + filepath.Base(configPath) + ".tmp-"
	return filepath.Dir(path) == filepath.Clean(root) && strings.HasPrefix(filepath.Base(path), wantPrefix)
}

func isAuthStagingBase(base string) bool {
	return authStagingBaseRE.MatchString(base)
}

var pathNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var authStagingBaseRE = regexp.MustCompile(`^\.[a-z][a-z0-9_-]{0,63}\.json\.tmp-(?:[0-9]+|stage-[A-Za-z0-9_-]+|migrate-[A-Za-z0-9_-]+)$`)

func isAuthStagingPath(root, path string) bool {
	authRoot := filepath.Join(root, "auth")
	if !isUnder(authRoot, path) {
		return false
	}
	providerName := filepath.Base(filepath.Dir(path))
	return pathNameRE.MatchString(providerName)
}

func isUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
