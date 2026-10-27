//go:build windows

package fsutil

import (
	"os"

	"golang.org/x/sys/windows"
)

// TryLockFile takes an exclusive lock on file without waiting. It reports
// false when another process holds the lock.
func TryLockFile(file *os.File) (bool, error) {
	var ol windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ol,
	)
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_SHARING_VIOLATION {
		return false, nil
	}
	return false, err
}

// UnlockFile releases a lock taken with TryLockFile.
func UnlockFile(file *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &ol)
}
