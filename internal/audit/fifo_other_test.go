//go:build !unix

package audit

import "errors"

func syscallMkfifo(string, uint32) error {
	return errors.New("fifos are unix-specific")
}
