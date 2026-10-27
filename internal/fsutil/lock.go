package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrBusy means another process held a lock for longer than Lock waits.
var ErrBusy = errors.New("another maildo command is using it right now")

// Lock takes an exclusive lock on the file at path and creates the file
// if needed. It waits up to wait for another process to let go, and
// returns a func that releases the lock.
func Lock(path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := TryLockFile(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if ok {
			return func() {
				_ = UnlockFile(file)
				_ = file.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, ErrBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}
