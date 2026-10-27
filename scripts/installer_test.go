package scripts

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var releaseAssets = []string{
	"maildo-linux-amd64", "maildo-linux-arm64",
	"maildo-darwin-amd64", "maildo-darwin-arm64",
}

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerLocalRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	script, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	dist, root := t.TempDir(), t.TempDir()
	var manifest strings.Builder
	for _, asset := range releaseAssets {
		body := "#!/bin/sh\nprintf 'fixture 0.1.0\\n'\n# " + asset + "\n"
		put(t, filepath.Join(dist, asset), body)
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256([]byte(body)), asset)
	}
	put(t, filepath.Join(dist, "checksums.txt"), manifest.String())
	cmd := exec.Command("sh", "fake-release.sh", dist, root, "v0.1.0")
	absoluteRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != absoluteRoot {
		t.Fatalf("fixture: %v: %s", err, output)
	}
	for _, layout := range []string{"releases/latest/download", "releases/download/v0.1.0"} {
		for _, asset := range append(append([]string{}, releaseAssets...), "checksums.txt") {
			got, err := os.ReadFile(filepath.Join(root, layout, asset))
			want, _ := os.ReadFile(filepath.Join(dist, asset))
			if err != nil || string(got) != string(want) {
				t.Fatalf("fixture copy differs: %s/%s: %v", layout, asset, err)
			}
		}
	}
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	defer server.Close()
	for _, tc := range []struct {
		name, os, arch, version, asset, failure, hash string
	}{
		{name: "latest", os: "Linux", arch: "x86_64", asset: releaseAssets[0]},
		{name: "pinned", os: "Linux", arch: "amd64", version: "v0.1.0", asset: releaseAssets[0]},
		{name: "linux arm", os: "Linux", arch: "aarch64", asset: releaseAssets[1]},
		{name: "mac intel shasum", os: "Darwin", arch: "x86_64", asset: releaseAssets[2], hash: "shasum"},
		{name: "mac arm", os: "Darwin", arch: "arm64", asset: releaseAssets[3]},
		{name: "pinned without v", os: "Linux", arch: "amd64", version: "0.1.0", asset: releaseAssets[0]},
		{name: "git bash", os: "MINGW64_NT-10.0", arch: "x86_64", failure: "does not support Windows yet"},
		{name: "msys arm", os: "MSYS_NT-10.0", arch: "aarch64", failure: "does not support Windows yet"},
		{name: "cygwin", os: "CYGWIN_NT-10.0", arch: "AMD64", failure: "does not support Windows yet"},
		{name: "wrong checksum", os: "Linux", arch: "x86_64", failure: "does not match its checksum"},
		{name: "missing entry", os: "Linux", arch: "x86_64", failure: "has no checksum for"},
		{name: "missing asset", os: "Linux", arch: "x86_64", failure: "could not download"},
		{name: "missing manifest", os: "Linux", arch: "x86_64", failure: "/checksums.txt"},
		{name: "unsupported arch", os: "Linux", arch: "riscv64", failure: "no maildo build for riscv64"},
		{name: "unsupported os", os: "FreeBSD", arch: "x86_64", failure: "no maildo build for FreeBSD"},
		{name: "no hash tool", os: "Linux", arch: "x86_64", hash: "none", failure: "needs sha256sum or shasum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix, shim, tmp := t.TempDir(), t.TempDir(), t.TempDir()
			// A restricted PATH also proves shasum works when sha256sum is absent.
			programs := []string{"curl", "awk", "mktemp", "rm", "cp", "chmod", "mv"}
			if tc.hash == "shasum" {
				programs = append(programs, "shasum")
			} else if tc.hash != "none" {
				if _, err := exec.LookPath("sha256sum"); err == nil {
					programs = append(programs, "sha256sum")
				} else {
					programs = append(programs, "shasum")
				}
			}
			for _, name := range programs {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(shim, name)); err != nil {
					t.Fatal(err)
				}
			}
			// mkdir is needed only after verification succeeds.
			path, _ := exec.LookPath("mkdir")
			if err := os.Symlink(path, filepath.Join(shim, "mkdir")); err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(shim, "uname"), fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo '%s';; -m) echo '%s';; esac\n", tc.os, tc.arch))
			layout := filepath.Join(root, "releases/latest/download")
			checksums := filepath.Join(layout, "checksums.txt")
			native := filepath.Join(layout, releaseAssets[0])
			switch tc.name {
			case "wrong checksum":
				put(t, checksums, strings.Repeat("0", 64)+"  "+releaseAssets[0]+"\n")
			case "missing entry":
				put(t, checksums, "")
			case "missing asset":
				if err := os.Remove(native); err != nil {
					t.Fatal(err)
				}
			case "missing manifest":
				if err := os.Remove(checksums); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				put(t, checksums, manifest.String())
				body, _ := os.ReadFile(filepath.Join(dist, releaseAssets[0]))
				put(t, native, string(body))
			})
			destination := filepath.Join(prefix, "maildo")
			put(t, destination, "existing install sentinel")
			cmd := exec.Command("/bin/sh", script)
			cmd.Env = append(os.Environ(), "PATH="+shim, "TMPDIR="+tmp, "INSTALL_DIR="+prefix, "MAILDO_DOWNLOAD_BASE="+server.URL, "MAILDO_VERSION="+tc.version)
			output, err := cmd.CombinedOutput()
			got, readErr := os.ReadFile(destination)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.failure != "" {
				if err == nil || !strings.Contains(string(output), tc.failure) || string(got) != "existing install sentinel" {
					t.Fatalf("failure must preserve install: %v: %s", err, output)
				}
			} else {
				want, _ := os.ReadFile(filepath.Join(dist, tc.asset))
				if err != nil || string(got) != string(want) || !strings.Contains(string(output), "Next: maildo login") || !strings.Contains(string(output), "export PATH=") {
					t.Fatalf("installation failed: %v: %s", err, output)
				}
				if tc.name == "latest" || strings.HasPrefix(tc.name, "pinned") {
					if out, err := exec.Command(destination).CombinedOutput(); err != nil || string(out) != "fixture 0.1.0\n" {
						t.Fatalf("installed executable: %v: %s", err, out)
					}
				}
			}
			if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
				t.Fatal("download temporary files were not cleaned")
			}
			if entries, err := os.ReadDir(prefix); err != nil || len(entries) != 1 {
				t.Fatal("staged install files were not cleaned")
			}
		})
	}
}
