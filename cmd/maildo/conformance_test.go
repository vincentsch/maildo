package main

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/vincentsch/rungrad/conformance"
)

const (
	testConformanceSecretEnv   = "MAILDO_CONFORMANCE_TOKEN"
	testConformanceStateDir    = ".maildo-conformance"
	testConformanceStateFile   = "mailbox-state.json"
	testConformanceAuditFile   = "write-audit.ndjson"
	conformanceReportPrefix    = "maildo-provider-conformance"
	selectorContractReportName = "maildo-selector-contracts.json"
)

func TestConformance(t *testing.T) {
	defaultBin := buildMaildoBinary(t, nil)
	taggedBin := buildMaildoBinary(t, []string{"conformance"})
	defaultManifest := runManifestBinary(t, defaultBin, t.TempDir())
	taggedManifest := runManifestBinary(t, taggedBin, t.TempDir())
	if defaultManifest != taggedManifest {
		t.Fatal("default and tagged manifest bytes differ")
	}
	assertManifestDoesNotLeak(t, taggedManifest, "fixture-message", "logged-out", testConformanceSecretEnv, testConformanceStateDir)
	assertDefaultBinaryExcludesFixtures(t, defaultBin)

	ruleset, err := conformance.DefaultRuleset()
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	oracles := loadProviderConformanceOracles(t)
	rungradIdentity := conformanceRungradIdentity(t, taggedBin)
	candidateVersion := conformanceCandidateVersion()
	for _, providerName := range []string{"outlook", "gmail", "imap", "fastmail"} {
		t.Run(providerName, func(t *testing.T) {
			oracle := oracles[providerName]
			runner, err := conformance.NewRunner(conformance.Target{
				Path:         taggedBin,
				Read:         []string{"folders", "list", "--account", providerName},
				Mutate:       oracle.MutateCommand(providerName),
				Auth:         []string{"folders", "list", "--account", providerName + "-logged-out"},
				Ambiguous:    []string{"folders", "view", "Dup", "--account", providerName + "-ambiguous"},
				NotFound:     []string{"read", "--account", providerName, "--id", oracle.AbsentID},
				APIError:     []string{"folders", "list", "--account", providerName + "-api-error"},
				Forbidden:    []string{"folders", "list", "--account", providerName + "-forbidden"},
				RateLimited:  []string{"folders", "list", "--account", providerName + "-rate-limited"},
				Destructive:  []string{"folders", "delete", "Disposable", "--account", providerName},
				HasUpdate:    true,
				Secret:       []string{"folders", "list", "--account", providerName},
				SecretEnv:    testConformanceSecretEnv,
				ManifestMode: conformance.ManifestModeRequired,
			})
			if err != nil {
				t.Fatalf("new conformance runner: %v", err)
			}
			defer runner.Close()
			if err := runner.DiscoverManifest(); err != nil {
				t.Fatalf("discover manifest: %v", err)
			}
			score := runner.Score(ruleset)
			assertConformanceScore(t, score)
			if !strings.HasPrefix(score.Manifest.ToolVersion, candidateVersion+" (commit ") {
				t.Fatalf("manifest tool version = %q, want %q", score.Manifest.ToolVersion, candidateVersion)
			}
			writeConformanceWrapperReport(t, conformanceWrapper{
				Schema:           "maildo-conformance/v1",
				Provider:         providerName,
				CandidateVersion: candidateVersion,
				RunGrad:          rungradIdentity,
				Score:            score,
			})
		})
	}

	assertConformanceFolderOrderAndNoPreviewWrites(t, taggedBin)
	writeSelectorContractReport(t, taggedBin)
}

type providerConformanceOracle struct {
	Schema               string   `json:"schema"`
	Provider             string   `json:"provider"`
	PresentID            string   `json:"present_id"`
	AbsentID             string   `json:"absent_id"`
	MutationDestination  string   `json:"mutation_destination"`
	SharedFieldAllowlist []string `json:"shared_field_allowlist"`
}

func (o providerConformanceOracle) MutateCommand(providerName string) []string {
	return []string{"move", "--account", providerName, "--id", o.PresentID, "--to", o.MutationDestination}
}

type conformanceRungradModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum"`
}

type conformanceWrapper struct {
	Schema           string                   `json:"schema"`
	Provider         string                   `json:"provider"`
	CandidateVersion string                   `json:"candidate_version"`
	RunGrad          conformanceRungradModule `json:"rungrad"`
	Score            conformance.Score        `json:"score"`
}

func assertConformanceScore(t *testing.T, score conformance.Score) {
	t.Helper()
	if score.SpecVersion != "rungrad-spec/1" || score.Manifest.Status != conformance.ManifestPresent {
		t.Fatalf("score manifest/spec = %#v", score)
	}
	if score.Total != 22 || score.Applicable != 22 || score.Passed != 22 || score.Overall != 1 {
		t.Fatalf("conformance score differs:\n%s", score.Report())
	}
	var notApplicable, failed []string
	for _, rule := range score.Rules {
		switch rule.Result {
		case conformance.ResultNotApplicable:
			notApplicable = append(notApplicable, rule.ID)
		case conformance.ResultFail:
			failed = append(failed, rule.ID)
		}
	}
	if len(notApplicable) != 0 || len(failed) != 0 {
		t.Fatalf("not applicable = %v failed = %v\n%s", notApplicable, failed, score.Report())
	}
}

func loadProviderConformanceOracles(t *testing.T) map[string]providerConformanceOracle {
	t.Helper()
	out := map[string]providerConformanceOracle{}
	for _, providerName := range []string{"outlook", "gmail", "imap", "fastmail"} {
		path := filepath.Join("testdata", "conformance", providerName, "oracle.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		var oracle providerConformanceOracle
		if err := dec.Decode(&oracle); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		var extra any
		if err := dec.Decode(&extra); err == nil {
			t.Fatalf("oracle %s has trailing JSON", path)
		}
		if oracle.Schema != "maildo-conformance-oracle/v1" || oracle.Provider != providerName ||
			oracle.PresentID == "" || oracle.AbsentID == "" || oracle.MutationDestination == "" ||
			len(oracle.SharedFieldAllowlist) == 0 {
			t.Fatalf("invalid oracle %s: %#v", path, oracle)
		}
		out[providerName] = oracle
	}
	return out
}

func conformanceRungradIdentity(t *testing.T, bin string) conformanceRungradModule {
	t.Helper()
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		t.Fatalf("build info: %v", err)
	}
	for _, dep := range info.Deps {
		if dep != nil && dep.Path == "github.com/vincentsch/rungrad" {
			sum := goSumEntry(t, dep.Path, dep.Version)
			if dep.Sum != "" && dep.Sum != sum {
				t.Fatalf("embedded rungrad sum = %q, committed sum = %q", dep.Sum, sum)
			}
			return conformanceRungradModule{Path: dep.Path, Version: dep.Version, Sum: sum}
		}
	}
	t.Fatal("rungrad module missing from conformance binary")
	return conformanceRungradModule{}
}

func goSumEntry(t *testing.T, path, version string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRootFromConformanceTest(), "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	needle := path + " " + version + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, needle) {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				return fields[2]
			}
		}
	}
	t.Fatalf("go.sum entry missing for %s %s", path, version)
	return ""
}

func writeConformanceWrapperReport(t *testing.T, wrapper conformanceWrapper) {
	t.Helper()
	if wrapper.Schema != "maildo-conformance/v1" || wrapper.Provider == "" || wrapper.CandidateVersion == "" || wrapper.RunGrad.Path == "" {
		t.Fatalf("invalid wrapper: %#v", wrapper)
	}
	writeCanonicalJSONReport(t, conformanceReportPath(t, conformanceReportPrefix+"-"+wrapper.Provider+".json"), wrapper)
}

type selectorContractReport struct {
	Schema           string                `json:"schema"`
	CandidateVersion string                `json:"candidate_version"`
	Rows             []selectorContractRow `json:"rows"`
}

type selectorContractRow struct {
	Provider string   `json:"provider"`
	Args     []string `json:"args"`
	Exit     int      `json:"exit"`
	Code     string   `json:"code"`
}

func writeSelectorContractReport(t *testing.T, bin string) {
	t.Helper()
	var rows []selectorContractRow
	for _, tt := range []struct {
		providerName string
		args         []string
	}{
		{"outlook", []string{"read", "--account", "outlook", "--internet-id", "not-a-message-id"}},
		{"gmail", []string{"read", "--account", "gmail", "--internet-id", "not-a-message-id"}},
		{"imap", []string{"read", "--account", "imap", "--id", "imap-message/v1/not-base64/0/0"}},
		{"fastmail", []string{"read", "--account", "fastmail", "--id", "imap-message/v1/not-base64/0/0"}},
	} {
		home := newConformanceHome(t)
		res := runConformanceProcess(t, bin, home, tt.args...)
		if res.exit != 1 || res.stdout != "" || !strings.Contains(res.stderr, "invalid message selector") {
			t.Fatalf("%s selector result = exit %d stdout=%q stderr=%q", tt.providerName, res.exit, res.stdout, res.stderr)
		}
		assertAuditAbsent(t, home)
		rows = append(rows, selectorContractRow{Provider: tt.providerName, Args: tt.args, Exit: res.exit, Code: "invalid_selector"})
	}
	writeCanonicalJSONReport(t, conformanceReportPath(t, selectorContractReportName), selectorContractReport{
		Schema:           "maildo-selector-contracts/v1",
		CandidateVersion: conformanceCandidateVersion(),
		Rows:             rows,
	})
}

// conformanceReportPath is where a conformance report goes: the folder in
// MAILDO_CONFORMANCE_REPORT_DIR when set, else a temporary folder.
func conformanceReportPath(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("MAILDO_CONFORMANCE_REPORT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func conformanceCandidateVersion() string {
	if v := os.Getenv("MAILDO_CONFORMANCE_VERSION"); v != "" {
		return v
	}
	return version
}

func writeCanonicalJSONReport(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	switch value.(type) {
	case conformanceWrapper:
		var decoded conformanceWrapper
		if err := dec.Decode(&decoded); err != nil {
			t.Fatal(err)
		}
	case selectorContractReport:
		var decoded selectorContractReport
		if err := dec.Decode(&decoded); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported report type %T", value)
	}
}

func repoRootFromConformanceTest() string {
	return filepath.Clean("../..")
}

func TestConformanceFixtureRejectsUnsafeState(t *testing.T) {
	taggedBin := buildMaildoBinary(t, []string{"conformance"})
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{
			name: "invalid version",
			setup: func(t *testing.T, home string) {
				writeConformanceStateBytes(t, home, []byte(`{"version":2,"folders":[],"messages":[]}`+"\n"), 0o600)
			},
		},
		{
			name: "duplicate folder",
			setup: func(t *testing.T, home string) {
				writeConformanceStateBytes(t, home, []byte(`{"version":1,"folders":[{"account":"fixture","id":"one","display_name":"A","path":"A","total_item_count":0,"unread_item_count":0,"child_folder_count":0},{"account":"fixture","id":"one","display_name":"B","path":"B","total_item_count":0,"unread_item_count":0,"child_folder_count":0}],"messages":[]}`+"\n"), 0o600)
			},
		},
		{
			name: "dangling message",
			setup: func(t *testing.T, home string) {
				writeConformanceStateBytes(t, home, []byte(`{"version":1,"folders":[],"messages":[{"account":"fixture","id":"message","folder_id":"missing","subject":"Broken","is_read":false}]}`+"\n"), 0o600)
			},
		},
		{
			name: "state symlink",
			setup: func(t *testing.T, home string) {
				dir := ensureConformanceTestDir(t, home)
				target := filepath.Join(home, "target.json")
				if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, testConformanceStateFile)); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
		{
			name: "state wrong mode",
			setup: func(t *testing.T, home string) {
				writeConformanceStateBytes(t, home, []byte(`{"version":1,"folders":[],"messages":[]}`+"\n"), 0o644)
			},
		},
		{
			name: "audit wrong mode",
			setup: func(t *testing.T, home string) {
				seedConformanceProcess(t, taggedBin, home)
				audit := filepath.Join(home, testConformanceStateDir, testConformanceAuditFile)
				if err := os.WriteFile(audit, []byte{}, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(audit, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "audit symlink",
			setup: func(t *testing.T, home string) {
				seedConformanceProcess(t, taggedBin, home)
				target := filepath.Join(home, "audit-outside.ndjson")
				if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
					t.Fatal(err)
				}
				audit := filepath.Join(home, testConformanceStateDir, testConformanceAuditFile)
				if err := os.Symlink(target, audit); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := newConformanceHome(t)
			tt.setup(t, home)
			res := runConformanceProcess(t, taggedBin, home, "folders", "list", "--account", "fixture")
			if res.exit != 2 || res.stdout != "" || !strings.Contains(res.stderr, "maildo: conformance fixture state unavailable") {
				t.Fatalf("result = exit %d stdout=%q stderr=%q", res.exit, res.stdout, res.stderr)
			}
		})
	}
}

func TestConformanceFixtureRejectsConfigSymlinkEscapes(t *testing.T) {
	taggedBin := buildMaildoBinary(t, []string{"conformance"})
	for _, tt := range []struct {
		name       string
		configPath func(t *testing.T, home, outside string) string
	}{
		{
			name: "parent symlink",
			configPath: func(t *testing.T, home, outside string) string {
				link := filepath.Join(home, "link")
				if err := os.Symlink(outside, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return filepath.Join(link, "config.yaml")
			},
		},
		{
			name: "leaf symlink",
			configPath: func(t *testing.T, home, outside string) string {
				target := filepath.Join(outside, "config.yaml")
				if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, "config.yaml")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return link
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := newConformanceHome(t)
			outside := t.TempDir()
			configPath := tt.configPath(t, home, outside)
			res := runConformanceProcess(t, taggedBin, home, "--config", configPath, "folders", "list", "--account", "fixture")
			if res.exit != 2 || res.stdout != "" || !strings.Contains(res.stderr, "maildo: conformance fixture state unavailable") {
				t.Fatalf("result = exit %d stdout=%q stderr=%q", res.exit, res.stdout, res.stderr)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "parent symlink" && len(entries) != 0 {
				t.Fatalf("outside directory was written: %#v", entries)
			}
		})
	}
}

func TestConformanceFixtureConsumesEnvironmentCredential(t *testing.T) {
	taggedBin := buildMaildoBinary(t, []string{"conformance"})
	home := newConformanceHome(t)
	seedConformanceProcess(t, taggedBin, home)
	// rungrad keeps its files in the user config folder, which macOS puts
	// under Library even when XDG_CONFIG_HOME is set.
	configDir := home
	if runtime.GOOS == "darwin" {
		configDir = filepath.Join(home, "Library", "Application Support")
	}
	cache := filepath.Join(configDir, "maildo", "auth", "conformance", "fixture.json")
	corrupt := []byte("not-json\n")
	if err := os.WriteFile(cache, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	withoutEnv := runConformanceProcess(t, taggedBin, home, "folders", "list", "--account", "fixture", "--json")
	if withoutEnv.exit != 3 {
		t.Fatalf("without env exit = %d stdout=%q stderr=%q", withoutEnv.exit, withoutEnv.stdout, withoutEnv.stderr)
	}
	secret := "score-sentinel-secret-12345"
	withEnv := runConformanceProcessWithEnv(t, taggedBin, home, map[string]string{testConformanceSecretEnv: secret}, "folders", "list", "--account", "fixture", "--json")
	if withEnv.exit != 0 {
		t.Fatalf("with env exit = %d stdout=%q stderr=%q", withEnv.exit, withEnv.stdout, withEnv.stderr)
	}
	if strings.Contains(withEnv.stdout+withEnv.stderr, secret) {
		t.Fatalf("credential sentinel leaked: stdout=%q stderr=%q", withEnv.stdout, withEnv.stderr)
	}
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("environment credential rewrote cache: %q", string(after))
	}
}

func TestConformanceFixtureAuditWrites(t *testing.T) {
	taggedBin := buildMaildoBinary(t, []string{"conformance"})
	t.Run("delete records provider id", func(t *testing.T) {
		home := newConformanceHome(t)
		seedConformanceProcess(t, taggedBin, home)
		res := runConformanceProcess(t, taggedBin, home, "folders", "delete", "Disposable", "--account", "fixture", "--confirm", "disposable-id", "--json")
		if res.exit != 0 {
			t.Fatalf("delete exit = %d stdout=%q stderr=%q", res.exit, res.stdout, res.stderr)
		}
		got := readConformanceAudit(t, home)
		want := `{"version":1,"operation":"folder_delete","account":"fixture","target":"disposable-id"}` + "\n"
		if got != want {
			t.Fatalf("audit = %q, want %q", got, want)
		}
	})

	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"folders", "create", "Created", "--account", "fixture", "--json"}},
		{name: "rename", args: []string{"folders", "rename", "zeta-id", "Renamed", "--account", "fixture", "--json"}},
	} {
		t.Run(tt.name+" unsupported write", func(t *testing.T) {
			home := newConformanceHome(t)
			seedConformanceProcess(t, taggedBin, home)
			res := runConformanceProcess(t, taggedBin, home, tt.args...)
			if res.exit != 2 || res.stdout != "" {
				t.Fatalf("%s exit = %d stdout=%q stderr=%q", tt.name, res.exit, res.stdout, res.stderr)
			}
			assertAuditAbsent(t, home)
		})
	}
}

func buildMaildoBinary(t *testing.T, tags []string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "maildo")
	args := []string{"build"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "-ldflags", "-X main.version="+conformanceCandidateVersion())
	args = append(args, "-o", bin, ".")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return bin
}

func assertDefaultBinaryExcludesFixtures(t *testing.T, bin string) {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, blocked := range [][]byte{
		[]byte("fixture-message"),
		[]byte(testConformanceSecretEnv),
		[]byte(testConformanceStateDir),
		[]byte("conformance fixture"),
		[]byte("internal/testkit"),
		[]byte("scopekit"),
	} {
		if bytes.Contains(data, blocked) {
			t.Fatalf("default binary contains fixture marker %q", blocked)
		}
	}
}

func assertConformanceFolderOrderAndNoPreviewWrites(t *testing.T, bin string) {
	t.Helper()
	home := newConformanceHome(t)
	seed := runConformanceProcess(t, bin, home, "folders", "list", "--account", "fixture", "--json")
	if seed.exit != 0 {
		t.Fatalf("seed list: exit %d stdout=%q stderr=%q", seed.exit, seed.stdout, seed.stderr)
	}
	var body struct {
		Folders []struct {
			Path string `json:"path"`
			ID   string `json:"id"`
		} `json:"folders"`
	}
	if err := json.Unmarshal([]byte(seed.stdout), &body); err != nil {
		t.Fatalf("folder list JSON: %v\n%s", err, seed.stdout)
	}
	var got []string
	for _, f := range body.Folders {
		got = append(got, f.Path+"/"+f.ID)
	}
	want := []string{"Archive/archive-id", "Disposable/disposable-id", "Inbox/inbox-id", "Zeta/zeta-id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("folder order = %#v, want %#v", got, want)
	}
	assertAuditAbsent(t, home)
	before := snapshotConformanceTree(t, home)
	for _, tt := range []struct {
		name     string
		args     []string
		wantExit int
	}{
		{name: "move dry run", args: []string{"move", "--account", "fixture", "--id", "fixture-message", "--to", "Archive", "--dry-run"}, wantExit: 0},
		{name: "delete dry run", args: []string{"folders", "delete", "Disposable", "--account", "fixture", "--dry-run"}, wantExit: 0},
		{name: "delete refusal", args: []string{"folders", "delete", "Disposable", "--account", "fixture", "--no-prompt"}, wantExit: 1},
	} {
		res := runConformanceProcess(t, bin, home, tt.args...)
		if res.exit != tt.wantExit {
			t.Fatalf("%s exit %d, want %d; stdout=%q stderr=%q", tt.name, res.exit, tt.wantExit, res.stdout, res.stderr)
		}
		assertAuditAbsent(t, home)
		after := snapshotConformanceTree(t, home)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s changed conformance tree\nbefore=%v\nafter=%v", tt.name, sortedSnapshotKeys(before), sortedSnapshotKeys(after))
		}
	}
}

type conformanceProcessResult struct {
	stdout string
	stderr string
	exit   int
}

func runConformanceProcess(t *testing.T, bin, home string, args ...string) conformanceProcessResult {
	t.Helper()
	return runConformanceProcessWithEnv(t, bin, home, nil, args...)
}

func runConformanceProcessWithEnv(t *testing.T, bin, home string, extraEnv map[string]string, args ...string) conformanceProcessResult {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TMPDIR=" + os.TempDir(),
		"TEMP=" + os.TempDir(),
		"TMP=" + os.TempDir(),
	}
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("%v: %v", args, err)
		}
	}
	return conformanceProcessResult{stdout: stdout.String(), stderr: stderr.String(), exit: exit}
}

func readConformanceAudit(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, testConformanceStateDir, testConformanceAuditFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newConformanceHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rungrad-conformance-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func seedConformanceProcess(t *testing.T, bin, home string) {
	t.Helper()
	res := runConformanceProcess(t, bin, home, "folders", "list", "--account", "fixture")
	if res.exit != 0 {
		t.Fatalf("seed process exit %d stdout=%q stderr=%q", res.exit, res.stdout, res.stderr)
	}
}

func ensureConformanceTestDir(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, testConformanceStateDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	return dir
}

func writeConformanceStateBytes(t *testing.T, home string, data []byte, mode os.FileMode) {
	t.Helper()
	dir := ensureConformanceTestDir(t, home)
	path := filepath.Join(dir, testConformanceStateFile)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertAuditAbsent(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, testConformanceStateDir, testConformanceAuditFile)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("audit stat = %v, want absent", err)
	}
}

func snapshotConformanceTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in conformance tree")
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedSnapshotKeys(snapshot map[string]string) []string {
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
