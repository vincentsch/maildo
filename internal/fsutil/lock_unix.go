//go:build linux || darwin

package fsutil

import (
	"os"

	"golang.org/x/sys/unix"
)

// TryLockFile takes an exclusive lock on file without waiting. It reports
// false when another process holds the lock.
func TryLockFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
		return false, nil
	}
	return false, err
}

// UnlockFile releases a lock taken with TryLockFile.
func UnlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
