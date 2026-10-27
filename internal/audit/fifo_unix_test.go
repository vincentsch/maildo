//go:build unix

package audit

import "syscall"

func syscallMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
