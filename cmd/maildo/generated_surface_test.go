package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/docsgen"
	"github.com/vincentsch/rungrad/testutil"
)

const updateGeneratedSurfaceEnv = "MAILDO_UPDATE_GOLDENS"

func TestGeneratedDocsAndHelpGoldens(t *testing.T) {
	newProductionApp := func() *rungrad.App { return newApp(productionDeps()) }
	docsDir := filepath.Join("..", "..", "docs", "commands")
	helpDir := filepath.Join("testdata", "help")

	docs := docsgen.Generate(newProductionApp())
	if len(docs) < 60 {
		t.Fatalf("generated doc count = %d, want a page per command", len(docs))
	}
	assertGeneratedContentExcludes(t, docs, "docs", "__rungrad_manifest", "scopekit", "fake")

	help := testutil.CaptureAllHelp(newProductionApp())
	if len(help) != len(docs)-1 {
		t.Fatalf("help golden count = %d, want one per command (%d)", len(help), len(docs)-1)
	}
	assertGeneratedContentExcludes(t, help, "help", "__rungrad_manifest", "scopekit", "fake")

	if os.Getenv(updateGeneratedSurfaceEnv) == "1" {
		if err := writeGeneratedFiles(docsDir, ".md", docs); err != nil {
			t.Fatalf("write generated docs: %v", err)
		}
		if err := writeGeneratedFiles(helpDir, ".txt", helpGoldenFiles(newProductionApp(), help)); err != nil {
			t.Fatalf("write help goldens: %v", err)
		}
	}

	if err := auditGeneratedTree(docsDir); err != nil {
		t.Fatalf("generated docs tree is unsafe: %v", err)
	}
	if err := auditGeneratedTree(helpDir); err != nil {
		t.Fatalf("help golden tree is unsafe: %v", err)
	}
	testutil.AssertDocsInSync(t, newProductionApp, false, docsDir)
	testutil.AssertHelpGoldens(t, newProductionApp, false, helpDir)
}

func TestGeneratedFileUpdaterRejectsUnsafeTrees(t *testing.T) {
	t.Run("bad wanted paths", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			path string
			ext  string
		}{
			{name: "absolute", path: filepath.Join(t.TempDir(), "page.md"), ext: ".md"},
			{name: "unclean", path: "a/../page.md", ext: ".md"},
			{name: "parent", path: "../page.md", ext: ".md"},
			{name: "wrong extension", path: "page.txt", ext: ".md"},
			{name: "empty extension", path: "page.md", ext: ""},
		} {
			t.Run(tt.name, func(t *testing.T) {
				err := writeGeneratedFiles(t.TempDir(), tt.ext, map[string]string{tt.path: "content\n"})
				if err == nil {
					t.Fatal("writeGeneratedFiles succeeded, want rejection")
				}
			})
		}
	})

	t.Run("root symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		root := filepath.Join(dir, "root")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, root); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := writeGeneratedFiles(root, ".md", map[string]string{"index.md": "content\n"}); err == nil {
			t.Fatal("writeGeneratedFiles accepted symlink root")
		}
	})

	t.Run("descendant symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := writeGeneratedFiles(root, ".md", map[string]string{"index.md": "content\n"}); err == nil {
			t.Fatal("writeGeneratedFiles accepted descendant symlink")
		}
	})

	t.Run("non regular target", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "index.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeGeneratedFiles(root, ".md", map[string]string{"index.md": "content\n"}); err == nil {
			t.Fatal("writeGeneratedFiles accepted directory target")
		}
	})

	t.Run("prunes only chosen extension", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "old.md"), []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("manual\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeGeneratedFiles(root, ".md", map[string]string{"index.md": "content\n"}); err != nil {
			t.Fatalf("writeGeneratedFiles: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "old.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old.md stat = %v, want removed", err)
		}
		if got, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(got) != "manual\n" {
			t.Fatalf("keep.txt = %q, %v", got, err)
		}
	})
}

func helpGoldenFiles(app *rungrad.App, help map[string]string) map[string]string {
	tool := app.Root().Name()
	want := make(map[string]string, len(help))
	for path, content := range help {
		name := tool + ".txt"
		if path != "" {
			name = tool + "_" + strings.ReplaceAll(path, " ", "_") + ".txt"
		}
		want[name] = content
	}
	return want
}

func assertGeneratedContentExcludes(t *testing.T, files map[string]string, label string, blocked ...string) {
	t.Helper()
	for path, content := range files {
		for _, s := range blocked {
			if strings.Contains(path, s) || strings.Contains(content, s) {
				t.Fatalf("%s %s contains %q", label, path, s)
			}
		}
	}
}

func writeGeneratedFiles(root, ext string, want map[string]string) error {
	if ext == "" || !strings.HasPrefix(ext, ".") {
		return fmt.Errorf("extension must start with dot")
	}
	if err := auditGeneratedTree(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	paths := make([]string, 0, len(want))
	for path := range want {
		clean, err := cleanGeneratedPath(path, ext)
		if err != nil {
			return err
		}
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	for _, path := range paths {
		full, err := containedGeneratedPath(root, path)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(full); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("target %s is not a regular file", path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(want[path]), 0o644); err != nil {
			return err
		}
	}

	keep := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		keep[filepath.ToSlash(path)] = struct{}{}
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed under generated tree: %s", path)
		}
		if d.IsDir() || !d.Type().IsRegular() || filepath.Ext(path) != ext {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := keep[filepath.ToSlash(rel)]; !ok {
			return os.Remove(path)
		}
		return nil
	})
}

func auditGeneratedTree(root string) error {
	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("generated tree root must not be a symlink")
	case !info.IsDir():
		return fmt.Errorf("generated tree root must be a directory")
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed under generated tree: %s", path)
		}
		return nil
	})
}

func cleanGeneratedPath(path, ext string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("generated path %q must be relative", path)
	}
	if filepath.Clean(path) != path || path == "." {
		return "", fmt.Errorf("generated path %q must be clean", path)
	}
	if filepath.Ext(path) != ext {
		return "", fmt.Errorf("generated path %q must use %s", path, ext)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "" || part == ".." {
			return "", fmt.Errorf("generated path %q escapes the generated tree", path)
		}
	}
	return path, nil
}

func containedGeneratedPath(root, rel string) (string, error) {
	full := filepath.Join(root, rel)
	back, err := filepath.Rel(root, full)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(filepath.ToSlash(back), "../") || back == ".." {
		return "", fmt.Errorf("generated path %q escapes the generated tree", rel)
	}
	return full, nil
}
