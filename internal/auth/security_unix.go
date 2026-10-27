//go:build linux || darwin

package auth

import "os"

func platformSupported() error { return nil }

func lockFileModeInvalid(mode os.FileMode) bool {
	return mode.Perm()&0o177 != 0
}

func managedDirectoryModeInvalid(mode os.FileMode) bool { return mode.Perm()&0o077 != 0 }
