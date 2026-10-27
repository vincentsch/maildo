package auth

import (
	"fmt"
	"io"
	"os"
)

// managedRead is a file in maildo's private folders, opened so that no folder
// on the way can be swapped for a link. It checks the file before and after
// reading, including that the open file is still the one at that path.
type managedRead struct {
	file         *os.File
	validatePath func() error
	closeParents func()
}

func (r *managedRead) close() {
	_ = r.file.Close()
	r.closeParents()
}

func (r *managedRead) validate() error {
	info, err := r.file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("credential is not regular")
	}
	if err := privateReadPermissions(r.file, info); err != nil {
		return err
	}
	return r.validatePath()
}

func (r *managedRead) read(limit int64) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	info, err := r.file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("credential exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(r.file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("credential exceeds limit")
	}
	if err == nil {
		err = r.validate()
	}
	if err != nil {
		zero(data)
		return nil, err
	}
	return data, nil
}
