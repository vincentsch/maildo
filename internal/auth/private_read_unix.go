//go:build linux || darwin

package auth

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func privateReadPermissions(_ *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0o177 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("credential permissions are not private")
	}
	return nil
}

func openManagedRead(root string, dirs []string, name string) (_ *managedRead, err error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parents := []int{rootFD}
	closeParents := func() {
		for i := len(parents) - 1; i >= 0; i-- {
			_ = unix.Close(parents[i])
		}
	}
	defer func() {
		if err != nil {
			closeParents()
		}
	}()
	for _, dir := range dirs {
		fd, openErr := unix.Openat(parents[len(parents)-1], dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, openErr
		}
		parents = append(parents, fd)
		var info unix.Stat_t
		if err = unix.Fstat(fd, &info); err != nil {
			return nil, err
		}
		if info.Mode&0o077 != 0 {
			return nil, fmt.Errorf("managed directory permissions are not private")
		}
	}
	fd, err := unix.Openat(parents[len(parents)-1], name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	validate := func() error {
		for i, component := range append(append([]string(nil), dirs...), name) {
			var entry, opened unix.Stat_t
			if err := unix.Fstatat(parents[i], component, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			openedFD := fd
			if i < len(dirs) {
				openedFD = parents[i+1]
			}
			if err := unix.Fstat(openedFD, &opened); err != nil {
				return err
			}
			if entry.Dev != opened.Dev || entry.Ino != opened.Ino || entry.Mode != opened.Mode {
				return fmt.Errorf("managed path changed")
			}
			if i < len(dirs) && opened.Mode&0o077 != 0 {
				return fmt.Errorf("managed directory permissions are not private")
			}
		}
		return nil
	}
	r := &managedRead{file: file, validatePath: validate, closeParents: closeParents}
	if err = r.validate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return r, nil
}
