//go:build linux || darwin

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedCredentialReadRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"final_link", "dangling_link", "provider_link", "auth_link", "file_0644", "provider_0755", "auth_0755", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "auth", "outlook")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "work.json")
			if err := os.WriteFile(path, []byte("SYNTHETIC PRIVATE CACHE"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "final_link", "dangling_link":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(outside, "sentinel")
				if kind == "final_link" {
					if err := os.WriteFile(target, []byte("SYNTHETIC OUTSIDE"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "provider_link", "auth_link":
				linked := dir
				if kind == "auth_link" {
					linked = filepath.Dir(dir)
				}
				if err := os.Rename(linked, filepath.Join(outside, "original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "original"), linked); err != nil {
					t.Fatal(err)
				}
			case "file_0644":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "provider_0755":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "auth_0755":
				if err := os.Chmod(filepath.Dir(dir), 0755); err != nil {
					t.Fatal(err)
				}
			case "directory", "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			store := NewStore(root)
			if present, err := store.Exists("outlook", "work"); err == nil || present {
				t.Fatalf("Exists accepted %s: %t %v", kind, present, err)
			}
			data, present, err := store.LoadBytes("outlook", "work")
			if err == nil || present || len(data) != 0 {
				t.Fatalf("Store consumed unsafe path: present=%t bytes=%d err=%v", present, len(data), err)
			}
			coord, err := NewCoordinator(filepath.Join(root, "config.yaml"), []string{"outlook"})
			if err != nil {
				t.Fatal(err)
			}
			data, present, err = coord.LoadCredentialBytes(context.Background(), "outlook", "work")
			if err == nil || present || len(data) != 0 {
				t.Fatalf("Coordinator consumed unsafe path: present=%t bytes=%d err=%v", present, len(data), err)
			}
			if kind == "file_0644" {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0644 {
					t.Fatal("unsafe credential silently repaired")
				}
			}
		})
	}
}

func TestManagedCredentialReadDetectsSubstitution(t *testing.T) {
	for _, component := range []string{"file", "provider", "auth"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			store := NewStore(root)
			if err := store.SaveBytes("outlook", "work", []byte("ORIGINAL SYNTHETIC")); err != nil {
				t.Fatal(err)
			}
			r, err := openManagedRead(root, []string{"auth", "outlook"}, "work.json")
			if err != nil {
				t.Fatal(err)
			}
			defer r.close()
			path := store.cachePath("outlook", "work")
			if component == "provider" {
				path = filepath.Dir(path)
			}
			if component == "auth" {
				path = filepath.Dir(filepath.Dir(path))
			}
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if component == "provider" || component == "auth" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("SUBSTITUTE SYNTHETIC"), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := r.read(maxCredentialBytes)
			if err == nil || len(data) != 0 {
				t.Fatalf("substitution accepted: bytes=%d err=%v", len(data), err)
			}
		})
	}
}

func TestPrivateTransactionMaterialPinsEveryManagedComponent(t *testing.T) {
	for _, component := range []string{"auth", ".transactions", "transaction", "material"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			dirs := []string{"auth", ".transactions", "transaction"}
			dir := filepath.Join(root, dirs[0], dirs[1], dirs[2])
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "material")
			if err := os.WriteFile(path, []byte("SYNTHETIC RECOVERY MATERIAL"), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := readLimited(root, dirs, "material", maxCredentialBytes)
			zero(data)
			if err != nil {
				t.Fatal(err)
			}
			switch component {
			case "auth":
				path = filepath.Join(root, "auth")
			case ".transactions":
				path = filepath.Dir(dir)
			case "transaction":
				path = dir
			}
			outside := filepath.Join(t.TempDir(), "synthetic")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			data, err = readLimited(root, dirs, "material", maxCredentialBytes)
			if err == nil || len(data) != 0 {
				t.Fatalf("recovery material followed %s link: bytes=%d err=%v", component, len(data), err)
			}
		})
	}
}

func TestCredentialRootAliasAndNonsecretInputRemainAllowed(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "user-root")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	store := NewStore(alias)
	if err := store.SaveBytes("outlook", "work", []byte("PRIVATE SYNTHETIC")); err != nil {
		t.Fatal(err)
	}
	data, present, err := store.LoadBytes("outlook", "work")
	defer zero(data)
	if err != nil || !present {
		t.Fatalf("private root alias rejected: %v", err)
	}
	input := filepath.Join(root, "nonsecret.yaml")
	if err := os.WriteFile(input, []byte("nonsecret registry input"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(input, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLimited(root, nil, filepath.Base(input), maxCredentialBytes); err == nil {
		t.Fatal("private reader accepted public input")
	}
	if _, err := readConfigLimited(input, maxCredentialBytes); err != nil {
		t.Fatal(err)
	}
}
