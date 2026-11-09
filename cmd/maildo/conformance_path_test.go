//go:build conformance

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// On macOS the temp folder is reached through a link (/var to /private/var),
// so $HOME and the resolved home differ. Paths under $HOME must still count
// as inside the home.
func TestConformancePathUnderLinkedHome(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "private")
	if err := os.MkdirAll(filepath.Join(real, "rungrad-conformance-1", "maildo"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "var")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	t.Setenv("HOME", filepath.Join(link, "rungrad-conformance-1"))
	home, err := filepath.EvalSymlinks(filepath.Join(link, "rungrad-conformance-1"))
	if err != nil {
		t.Fatal(err)
	}
	path, rel, err := conformanceContainedPath(home, filepath.Join(link, "rungrad-conformance-1", "maildo", "config.yaml"))
	if err != nil {
		t.Fatalf("path under the linked home was refused: %v", err)
	}
	if rel != filepath.Join("maildo", "config.yaml") || path != filepath.Join(home, "maildo", "config.yaml") {
		t.Fatalf("path=%q rel=%q", path, rel)
	}
	if _, _, err := conformanceContainedPath(home, filepath.Join(base, "elsewhere", "config.yaml")); err == nil {
		t.Fatal("a path outside the home was accepted")
	}
}
