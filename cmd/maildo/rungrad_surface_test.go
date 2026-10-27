package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	runconfig "github.com/vincentsch/rungrad/config"
	runmanifest "github.com/vincentsch/rungrad/manifest"
	"github.com/vincentsch/rungrad/testutil"
	"github.com/vincentsch/rungrad/update"
)

type staticUpdateFetcher struct {
	release update.Release
	err     error
	calls   atomic.Int64
}

func (f *staticUpdateFetcher) Latest() (update.Release, error) {
	f.calls.Add(1)
	if f.err != nil {
		return update.Release{}, f.err
	}
	return f.release, nil
}

func TestProductionUpdateFetcherConfiguration(t *testing.T) {
	fetcher, ok := productionDeps().UpdateFetcher.(update.GitHubFetcher)
	if !ok {
		t.Fatalf("production update fetcher = %T, want update.GitHubFetcher", productionDeps().UpdateFetcher)
	}
	if fetcher.Owner != "vincentsch" || fetcher.Repo != "maildo" {
		t.Fatalf("production update fetcher = %#v", fetcher)
	}
}

func TestUpdateCommandUsesFetcherWithoutProductConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher := &staticUpdateFetcher{release: update.Release{Version: "v9.0.0"}}
	deps := productionDeps()
	deps.UpdateFetcher = fetcher

	for _, args := range [][]string{
		{"--config", configPath, "update", "--check"},
		{"--config", configPath, "update", "--check", "--json"},
		{"--config", configPath, "update", "--dry-run"},
	} {
		res := testutil.Run(newApp(deps), args...)
		if res.Exit != 0 {
			t.Fatalf("%v exit %d stdout=%q stderr=%q", args, res.Exit, res.Stdout, res.Stderr)
		}
		if len(args) > 0 && args[len(args)-1] == "--json" && !json.Valid([]byte(res.Stdout)) {
			t.Fatalf("%v stdout is not JSON: %q", args, res.Stdout)
		}
	}
	if got := fetcher.calls.Load(); got != 3 {
		t.Fatalf("fetcher calls = %d, want 3", got)
	}
}

func TestUpdateCommandUnavailableInstallBranch(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldVersion := version
	version = "v0.1.0"
	t.Cleanup(func() { version = oldVersion })

	deps := productionDeps()
	deps.UpdateFetcher = &staticUpdateFetcher{release: update.Release{Version: "v0.2.0"}}
	res := testutil.Run(newApp(deps), "--config", configPath, "update")
	if res.Exit != 2 {
		t.Fatalf("update exit = %d, want 2; stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
	}
	if res.Stdout != "" || !strings.Contains(res.Stderr, "automatic install is unavailable") {
		t.Fatalf("update unavailable output stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestManifestDeterministicAndSideEffectFree(t *testing.T) {
	var loadCalls atomic.Int64
	deps := productionDeps()
	deps.UpdateFetcher = &staticUpdateFetcher{release: update.Release{Version: "v9.0.0"}}
	deps.RunGradLoadConfig = func(string) (runconfig.Config, error) {
		loadCalls.Add(1)
		return runconfig.Config{}, errors.New("manifest must not load config")
	}

	a := testutil.Run(newApp(deps), "__rungrad_manifest")
	b := testutil.Run(newApp(deps), "__rungrad_manifest")
	for _, res := range []testutil.Result{a, b} {
		if res.Exit != 0 || res.Stderr != "" {
			t.Fatalf("manifest result = exit %d stdout=%q stderr=%q", res.Exit, res.Stdout, res.Stderr)
		}
	}
	if a.Stdout != b.Stdout {
		t.Fatal("fresh app manifest bytes differ")
	}
	if got := loadCalls.Load(); got != 0 {
		t.Fatalf("manifest called config loader %d times", got)
	}
	m := decodeManifestBytes(t, []byte(a.Stdout))
	if m.SchemaVersion != runmanifest.SchemaVersion {
		t.Fatalf("schema version = %q, want %q", m.SchemaVersion, runmanifest.SchemaVersion)
	}
	if got, want := manifestGlobalFlagNames(m), []string{"config", "dry-run", "include-meta", "jq", "json", "no-ansi", "no-color", "no-pager", "no-prompt", "plain", "quiet", "template"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global flags = %#v, want %#v", got, want)
	}
	assertManifestDoesNotLeak(t, a.Stdout, "__rungrad_manifest", "scopekit", "fake", "access_token", "refresh_token", "client_secret")
}

func TestBuiltProcessManifestDeterministicAndSideEffectFree(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "maildo")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	var buildOut bytes.Buffer
	build.Stdout = &buildOut
	build.Stderr = &buildOut
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, buildOut.String())
	}

	home := t.TempDir()
	first := runManifestBinary(t, bin, home)
	second := runManifestBinary(t, bin, home)
	if first != second {
		t.Fatal("built process manifest bytes differ")
	}
	decodeManifestBytes(t, []byte(first))
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("manifest created files in isolated home: %v", names)
	}
}

func TestPublicCompletionExcludesHiddenAndFixtureSurface(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			res := testutil.Run(newApp(productionDeps()), "completion", shell)
			if res.Exit != 0 {
				t.Fatalf("completion %s exit %d stdout=%q stderr=%q", shell, res.Exit, res.Stdout, res.Stderr)
			}
			assertManifestDoesNotLeak(t, res.Stdout, "__rungrad_manifest", "scopekit", "fake", "fixture-message", "MAILDO_CONFORMANCE_TOKEN")
		})
	}
}

func decodeManifestBytes(t *testing.T, data []byte) runmanifest.Manifest {
	t.Helper()
	var m runmanifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest JSON: %v\n%s", err, data)
	}
	if err := runmanifest.Validate(&m); err != nil {
		t.Fatalf("manifest validation: %v\n%s", err, data)
	}
	return m
}

func manifestGlobalFlagNames(m runmanifest.Manifest) []string {
	names := make([]string, 0, len(m.GlobalFlags))
	for _, flag := range m.GlobalFlags {
		names = append(names, flag.Name)
	}
	sort.Strings(names)
	return names
}

func assertManifestDoesNotLeak(t *testing.T, raw string, blocked ...string) {
	t.Helper()
	for _, s := range blocked {
		if strings.Contains(raw, s) {
			t.Fatalf("manifest contains %q", s)
		}
	}
}

func runManifestBinary(t *testing.T, bin, home string) string {
	t.Helper()
	cmd := exec.Command(bin, "__rungrad_manifest")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TMPDIR=" + os.TempDir(),
		"TEMP=" + os.TempDir(),
		"TMP=" + os.TempDir(),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("__rungrad_manifest: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("__rungrad_manifest stderr = %q", stderr.String())
	}
	return stdout.String()
}
