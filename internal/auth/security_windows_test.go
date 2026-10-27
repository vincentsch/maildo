//go:build windows

package auth

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

// maildo cannot make its files private on Windows yet, so it must refuse to
// keep logins there with one clear error instead of a permission failure.
func TestWindowsIsRefusedBeforeAnyFileIsWritten(t *testing.T) {
	root := t.TempDir()
	_, err := NewCoordinator(filepath.Join(root, "config.yaml"), []string{"imap"})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeUnsupportedPlatform {
		t.Fatalf("NewCoordinator on Windows: %v", err)
	}
}
