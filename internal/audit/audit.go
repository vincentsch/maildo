// Package audit writes maildo's action log, audit/actions.jsonl in the config
// folder: one line for each command that changed or sent mail.
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/vincentsch/maildo/internal/fsutil"
	"github.com/vincentsch/maildo/internal/mail"
)

const RelativePath = "audit/actions.jsonl"

// Entry is one line of the action log. maildo only adds lines; no command
// reads the log back.
type Entry struct {
	Version      int                           `json:"version"`
	CreatedAt    time.Time                     `json:"created_at"`
	Command      string                        `json:"command"`
	Operation    mail.MessageMutationOperation `json:"operation"`
	Selector     mail.BulkSelectorSummary      `json:"selector"`
	Counts       mail.BulkMutationCounts       `json:"counts"`
	Targets      []mail.AppliedMutationTarget  `json:"targets"`
	TargetFolder *mail.FolderSummary           `json:"target_folder,omitempty"`
	Categories   *mail.CategoryChange          `json:"categories,omitempty"`
	// Sent is set for sends instead of the target fields.
	Sent *SentMessage `json:"sent,omitempty"`
	// Change is set for changes to drafts, folders and rules instead of
	// the target fields.
	Change *Change `json:"change,omitempty"`
}

// Change records a change to a draft, a draft's attachments, a folder or a
// rule. Message text is not logged.
type Change struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	// Kind is draft, attachment, folder or rule.
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	// NewID is set when the change gave the item a new id, as IMAP drafts
	// get on every change.
	NewID string `json:"new_id,omitempty"`
	// Name is the draft subject, attachment file name, folder path or rule
	// name.
	Name    string `json:"name,omitempty"`
	NewName string `json:"new_name,omitempty"`
	// Outcome is "done" or "failed"; a failed change may still have
	// reached the server.
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code,omitempty"`
}

// SentMessage records who a message went to. The body is not logged.
type SentMessage struct {
	Account   string   `json:"account"`
	Provider  string   `json:"provider"`
	MessageID string   `json:"message_id,omitempty"`
	DraftID   string   `json:"draft_id,omitempty"`
	To        []string `json:"to"`
	CC        []string `json:"cc,omitempty"`
	BCC       []string `json:"bcc,omitempty"`
	Subject   string   `json:"subject"`
	// Outcome is "sent" or "failed"; a failed send may still have reached
	// the server.
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code,omitempty"`
}

// Append adds one line to the action log under root.
func Append(root string, entry Entry) error {
	return appendWith(root, entry, osOpenFile)
}

type appendFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

type openFileFunc func(string, int, os.FileMode) (appendFile, error)

func osOpenFile(name string, flag int, perm os.FileMode) (appendFile, error) {
	return os.OpenFile(name, flag, perm)
}

// appendWith does the work of Append; tests call it to inject failures. It
// encodes the entry before it opens the file, so a JSON error never leaves a
// half-made log behind.
func appendWith(root string, entry Entry, open openFileFunc) error {
	path := filepath.Join(root, RelativePath)
	dir := filepath.Dir(path)
	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return err
	}
	if err := validateExistingLog(path); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(entry); err != nil {
		return err
	}
	f, err := open(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closeNeeded := true
	defer func() {
		if closeNeeded {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// Check the open file again after Lstat, so a file swapped in between
	// cannot make maildo write to a directory or a device.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the action log %s is not a normal file", RelativePath)
	}
	n, err := f.Write(buf.Bytes())
	if err != nil {
		return err
	}
	if n != buf.Len() {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	closeNeeded = false
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// validateExistingLog refuses a log that is a link or not a normal file, and
// makes a normal file private, before maildo opens it.
func validateExistingLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the action log %s is a link; maildo writes only to a normal file", RelativePath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the action log %s is not a normal file", RelativePath)
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}
