//go:build windows

package auth

import (
	"os"

	"github.com/vincentsch/maildo/internal/errs"
)

// maildo keeps saved logins in files that only you may read. On Linux and
// macOS the file mode does that. On Windows the mode means nothing, and
// maildo does not yet set the access control list (ACL) that would, so it
// refuses to save or read logins there instead of keeping them readable to
// other users of the computer.
func platformSupported() error {
	return errs.New(errs.CodeUnsupportedPlatform, "maildo does not support Windows yet, because it cannot keep saved logins private there. Use it on Linux or macOS, or under WSL (Windows Subsystem for Linux)")
}

func lockFileModeInvalid(os.FileMode) bool { return false }

func managedDirectoryModeInvalid(os.FileMode) bool { return false }
