package main

import (
	"go/build"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportBoundaries(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	assertNoProductionImport(t, filepath.Join(root, "internal", "mail"),
		"github.com/vincentsch/maildo/internal/provider",
		"github.com/vincentsch/maildo/internal/outlook",
	)
	assertNoProductionImport(t, filepath.Join(root, "internal", "provider"),
		"github.com/vincentsch/maildo/internal/outlook",
	)
	assertNoProductionImport(t, filepath.Join(root, "internal", "compose"),
		"github.com/vincentsch/maildo/internal/provider",
		"github.com/vincentsch/maildo/internal/outlook",
	)
	assertNoProductionImport(t, filepath.Join(root, "internal", "rules"),
		"github.com/vincentsch/maildo/internal/provider",
		"github.com/vincentsch/maildo/internal/outlook",
	)
	assertNoProductionImport(t, filepath.Join(root, "internal", "audit"),
		"github.com/vincentsch/maildo/internal/provider",
		"github.com/vincentsch/maildo/internal/outlook",
		"github.com/vincentsch/maildo/internal/testkit",
	)
	assertNoProductionImport(t, filepath.Join(root, "internal", "htmltext"),
		"github.com/vincentsch/maildo/internal/mail",
		"github.com/vincentsch/maildo/internal/provider",
		"github.com/vincentsch/maildo/internal/outlook",
		"github.com/vincentsch/maildo/internal/errs",
		"github.com/vincentsch/maildo/internal/search",
		"github.com/vincentsch/maildo/internal/config",
		"github.com/vincentsch/maildo/internal/testkit",
		"github.com/vincentsch/maildo/internal/fsutil",
	)
	assertNoProductionImport(t, filepath.Join(root, "cmd", "maildo"),
		"github.com/vincentsch/maildo/internal/testkit",
		"github.com/vincentsch/rungrad/conformance",
		"github.com/yuin/goldmark",
		"github.com/microcosm-cc/bluemonday",
	)
}

func TestDefaultCommandDependencyGraphExcludesFixtures(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/maildo")
	cmd.Dir = filepath.Clean(filepath.Join("..", ".."))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/maildo: %v\n%s", err, out)
	}
	graph := string(out)
	for _, forbidden := range []string{
		"github.com/vincentsch/maildo/internal/testkit",
		"github.com/vincentsch/rungrad/conformance",
	} {
		if strings.Contains(graph, forbidden) {
			t.Fatalf("default dependency graph contains %s", forbidden)
		}
	}
}

func assertNoProductionImport(t *testing.T, dir string, forbidden ...string) {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("ImportDir(%s): %v", dir, err)
	}
	for _, imp := range pkg.Imports {
		for _, bad := range forbidden {
			if imp == bad {
				t.Fatalf("%s imports forbidden package %s", dir, bad)
			}
		}
	}
}
