package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/fsutil"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

// Approval codes work once
//
// Right before a send, maildo writes the message's approval code to
// sent-approvals.json next to the config file. After the send it writes how
// the send went. A later command with the same --approve code is refused,
// so a printed command that runs twice (an agent that retries, a command
// pasted again) does not send the message twice. When maildo cannot tell
// whether a message went out, it counts the code as used.
//
// The code is a hash of the message, so a new preview of the same message
// shows the same code. That preview says the message went out before, and
// it lets the code send the message once more.
//
// A send without --approve (a person who confirms in the terminal, or an
// account with policy.unattended_send) is written down too but never
// refused: the person saw the message, and a script may send the same
// report every week.

const (
	sentApprovalsFile     = "sent-approvals.json"
	sentApprovalsLockFile = "sent-approvals.lock"
	sentApprovalsVersion  = 1
	// maxSentApprovals is how many sends the file keeps, newest first.
	maxSentApprovals = 10000
)

type sentApproval struct {
	Code    string `json:"code"`
	Account string `json:"account"`
	// At is when the send started.
	At time.Time `json:"at"`
	// Outcome is "sent", "unknown" when the message may have gone out, or
	// "sending" when maildo stopped before it knew.
	Outcome string `json:"outcome"`
	// Previewed is true when a preview showed the message again after this
	// send. The code then works for one more send.
	Previewed bool `json:"previewed,omitempty"`
}

type sentApprovalFile struct {
	Version int            `json:"version"`
	Sent    []sentApproval `json:"sent"`
}

func (file *sentApprovalFile) find(code string) int {
	for i, s := range file.Sent {
		if s.Code == code {
			return i
		}
	}
	return -1
}

func (file *sentApprovalFile) remove(i int) {
	file.Sent = append(file.Sent[:i:i], file.Sent[i+1:]...)
}

// approval is a send that passed every check. approveSend returns it, the
// provider sends with its code, and logSend writes down how the send went.
type approval struct {
	code string
	// path is the sent-approvals.json the send was written to.
	path string
	// before is the earlier send of the same message, if any. It goes back
	// into the file when this send fails before anything went out.
	before *sentApproval
}

func sentApprovalsPath(f *rungrad.Factory) string {
	return filepath.Join(filepath.Dir(f.ConfigPath()), sentApprovalsFile)
}

func loadSentApprovals(path string) (sentApprovalFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sentApprovalFile{Version: sentApprovalsVersion}, nil
	}
	if err != nil {
		return sentApprovalFile{}, err
	}
	var file sentApprovalFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != sentApprovalsVersion {
		return sentApprovalFile{}, fmt.Errorf("%s is damaged", sentApprovalsFile)
	}
	return file, nil
}

// changeSentApprovals reads the file, lets change edit it and writes it
// back, all under the lock. When change returns an error, nothing is
// written.
func changeSentApprovals(path string, change func(*sentApprovalFile) error) error {
	unlock, err := fsutil.Lock(filepath.Join(filepath.Dir(path), sentApprovalsLockFile), shortIDLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	file, err := loadSentApprovals(path)
	if err != nil {
		return err
	}
	if err := change(&file); err != nil {
		return err
	}
	if len(file.Sent) > maxSentApprovals {
		file.Sent = file.Sent[:maxSentApprovals]
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return writeStateFile(path, data)
}

// sentApprovalsUnavailable is the error for a send that maildo could not
// write down. Without that record a repeated command could send the
// message twice, so maildo does not send.
func sentApprovalsUnavailable(acct provider.ResolvedAccount, cause error) error {
	return errs.NewWithDetails(errs.CodeInvalidConfig,
		fmt.Sprintf("maildo could not write down this send in its config folder (%s), so nothing was sent", withoutPath(cause)),
		map[string]any{
			"account": acct.Name,
			"hint":    "check that maildo's config folder is writable. If " + sentApprovalsFile + " in it is damaged, move it away; maildo then starts a new one",
		})
}

// withoutPath is an I/O error without the file path, which error messages
// never show.
func withoutPath(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error()
	}
	return err.Error()
}

// sentAtText is "on Tue, 27 Oct 2026 at 14:03" in local time.
func sentAtText(t time.Time) string {
	return t.Local().Format("on Mon, 2 Jan 2006 at 15:04")
}

func approvalUsed(acct provider.ResolvedAccount, s sentApproval) error {
	message := "this approved message was already sent " + sentAtText(s.At) + ", so maildo did not send it again"
	if s.Outcome != "sent" {
		message = "this approved message may already have been sent " + sentAtText(s.At) + ", so maildo did not send it again"
	}
	return errs.NewWithDetails(errs.CodeApprovalUsed, message, map[string]any{
		"account": acct.Name,
		"sent_at": s.At.UTC().Format(time.RFC3339),
		// Not "outcome", which names the result of this command's own
		// write; this command wrote nothing.
		"sent_outcome": s.Outcome,
		"hint":         "check Sent before you send it again. To send it again on purpose, run the command with --dry-run for a new preview and run the command it prints",
	})
}

// checkApprovalUnused refuses a code that already sent its message. It
// runs before maildo asks or sends; claimApproval checks again under the
// lock.
func checkApprovalUnused(path string, acct provider.ResolvedAccount, code string) error {
	file, err := loadSentApprovals(path)
	if err != nil {
		return sentApprovalsUnavailable(acct, err)
	}
	if i := file.find(code); i >= 0 && !file.Sent[i].Previewed {
		return approvalUsed(acct, file.Sent[i])
	}
	return nil
}

// claimApproval writes the code down right before the send. approved says
// whether the send came with --approve, which makes a used code an error.
func claimApproval(path string, now time.Time, acct provider.ResolvedAccount, code string, approved bool) (approval, error) {
	claimed := approval{code: code, path: path}
	var used error
	err := changeSentApprovals(path, func(file *sentApprovalFile) error {
		if i := file.find(code); i >= 0 {
			before := file.Sent[i]
			if approved && !before.Previewed {
				used = approvalUsed(acct, before)
				return used
			}
			claimed.before = &before
			file.remove(i)
		}
		file.Sent = append([]sentApproval{{Code: code, Account: acct.Name, At: now.UTC(), Outcome: "sending"}}, file.Sent...)
		return nil
	})
	if used != nil {
		return approval{}, used
	}
	if err != nil {
		return approval{}, sentApprovalsUnavailable(acct, err)
	}
	return claimed, nil
}

// finishApproval writes down how a send went. A send that surely did not
// go out gives the code back, so the same command can try again.
func finishApproval(f *rungrad.Factory, a approval, sendErr error) {
	if a.code == "" {
		return
	}
	outcome := "sent"
	if sendErr != nil {
		outcome = ""
		if facts, ok := errs.WriteOutcomeFactsFromError(sendErr); ok && facts.WriteMayHaveLanded {
			outcome = "unknown"
		}
	}
	err := changeSentApprovals(a.path, func(file *sentApprovalFile) error {
		i := file.find(a.code)
		switch {
		case i < 0:
			return nil
		case outcome != "":
			file.Sent[i].Outcome = outcome
		case a.before != nil:
			file.Sent[i] = *a.before
		default:
			file.remove(i)
		}
		return nil
	})
	if err != nil && outcome != "" {
		f.Infof("Warning: maildo could not write down that this message was sent (%s). Running the same command again would send it again.", withoutPath(err))
	}
}

// previewApproval looks up an earlier send of a message. With rearm, as
// for a preview that prints the code, it also lets the code send the
// message once more. It returns nil when the message was never sent.
func previewApproval(path string, code string, rearm bool) *sentApproval {
	file, err := loadSentApprovals(path)
	if err != nil {
		return nil
	}
	i := file.find(code)
	if i < 0 {
		return nil
	}
	before := file.Sent[i]
	if rearm && !before.Previewed {
		_ = changeSentApprovals(path, func(file *sentApprovalFile) error {
			if i := file.find(code); i >= 0 {
				file.Sent[i].Previewed = true
			}
			return nil
		})
	}
	return &before
}

// sentBefore is how a preview reports an earlier send of the same message.
type sentBefore struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
}

func newSentBefore(s *sentApproval) *sentBefore {
	if s == nil {
		return nil
	}
	return &sentBefore{At: s.At, Outcome: s.Outcome}
}

// writeSentBefore tells a person that this message went out before.
func writeSentBefore(w io.Writer, account string, s *sentBefore) {
	if s == nil {
		return
	}
	if s.Outcome == "sent" {
		fmt.Fprintf(w, "maildo already sent this message from %s %s. Check Sent before you send it again.\n", plainField(account), sentAtText(s.At))
		return
	}
	fmt.Fprintf(w, "maildo tried to send this message from %s %s, and it may have gone out. Check Sent before you send it again.\n", plainField(account), sentAtText(s.At))
}
