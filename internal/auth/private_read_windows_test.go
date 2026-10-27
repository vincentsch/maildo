//go:build windows

package auth

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This test needs native Windows execution; cross-compilation is not evidence
// that NtCreateFile or DACL evaluation behaved correctly at runtime.
func TestManagedCredentialReadWindowsDACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "public"}[public], func(t *testing.T) {
			root := t.TempDir()
			store := NewStore(root)
			if err := store.SaveBytes("outlook", "work", []byte("SYNTHETIC WINDOWS CACHE")); err != nil {
				t.Fatal(err)
			}
			entry := func(sid *windows.SID, access windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
				return windows.EXPLICIT_ACCESS{AccessPermissions: access, AccessMode: windows.GRANT_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(sid)}}
			}
			entries := []windows.EXPLICIT_ACCESS{entry(user.User.Sid, windows.GENERIC_ALL)}
			if public {
				entries = append(entries, entry(world, windows.FILE_GENERIC_READ))
			}
			acl, err := windows.ACLFromEntries(entries, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(store.cachePath("outlook", "work"), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			data, present, err := store.LoadBytes("outlook", "work")
			defer zero(data)
			if public {
				if err == nil || present || len(data) != 0 {
					t.Fatal("public DACL consumed")
				}
			} else if err != nil || !present {
				t.Fatalf("private DACL rejected: %v", err)
			}
		})
	}
}

func TestManagedCredentialReadWindowsReparsePoints(t *testing.T) {
	for _, component := range []string{"file", "provider"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			store := NewStore(root)
			if err := store.SaveBytes("outlook", "work", []byte("SYNTHETIC WINDOWS CACHE")); err != nil {
				t.Fatal(err)
			}
			path := store.cachePath("outlook", "work")
			if component == "provider" {
				path = filepath.Dir(path)
			}
			outside := filepath.Join(t.TempDir(), "synthetic")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("synthetic symlink unavailable: %v", err)
			}
			data, present, err := store.LoadBytes("outlook", "work")
			zero(data)
			if err == nil || present || len(data) != 0 {
				t.Fatal("reparse point consumed")
			}
		})
	}
}
